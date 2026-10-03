#!/usr/bin/env python3
"""Drive a generated harness's ENCODE path against every shared wire vector.

Usage:
  check_vectors_encode.py --emit-schema <test_vectors.json> [--max-id N]
  check_vectors_encode.py <test_vectors.json> <label>
                          [--cwd DIR] [--verb VERB] [--max-id N]
                          [--int64-json number|string]
                          [--inf-json inf|infinity|literal|overflow]
                          [--int64-safe] [--min-checked N]
                          -- <harness argv...>

One driver for all eleven suites, as `check_vectors_decode.py` is for the other
direction. It replaces the eight per-language `check_vectors.py` copies and the
three Go-test copies, each of which carried its own filter and each of which
dropped, silently, every vector it could not map (generator#650): 34 to 44 of
131 reached an encoder, and boolean, blob, every native array, every sequence,
`id_max` and `full_scale_example` reached none.

## What is asserted

Each vector's `fields` are the dense ops of one message. The driver

  1. derives the message that op list is an instance of -- one field per id,
     typed from the op (`unsigned` -> u64, `signed` -> i64, `array` -> a native
     array of its `element_type`, a `sequence` -> a struct, or a wrapper array
     when its last element carries the `element: true` marker);
  2. prints those messages with `--emit-schema`, so the schema and the values
     fed to it have one definition and cannot drift;
  3. feeds `fields` as JSON to `<harness argv...> encode <message>` and compares
     the output byte for byte with the vector's `serialized_sparse.hex` -- the
     sparse-canonical bytes a generated encoder must produce (MESSAGE_SPEC §2).

Messages are shared between vectors of the same shape, so the schema stays far
smaller than the file.

## Loud, never quiet

A driver that quietly narrows what it selects passes while testing less than it
claims. So every vector is either CHECKED or EXCLUDED, and an exclusion is a
named entry with a reason, never a `continue`:

  * `EXCLUDED_GROUPS` -- by design, with the reason beside each;
  * a vector whose ops are not in ascending id order within a message -- the
    C vectorgen writes ops in call order, and a generated encoder writes a
    message's fields by ascending id, so no schema-driven encoder reproduces
    those bytes (`nested_sequence_with_array`, `nested_sequence_multilevel`);
  * `--max-id N` -- a target whose descriptor profile cannot declare an id above
    N (corelib-c-cpp's default 16-bit profile: C tops out at 65535);
  * `--int64-safe` -- a build whose 64-bit scalar is a double (TypeScript
    `int64: number`) cannot carry a value above 2^53-1.

The run prints `checked`, `excluded` and a per-group breakdown and fails when

  * `checked + excluded != len(vectors)`,
  * `checked` is below `--min-checked` (default `MIN_CHECKED`), or
  * a group in `REQUIRED_GROUPS` has no checked vector (under `--int64-safe`,
    `LOSSY_GROUPS` are not required: their one vector cannot be carried).

The last two are what stops a narrowed mapping from passing with a lower count.

## Id vectors

`id_max` and `id_max_32bit` carry the value 0, so their sparse column is empty
and byte-checking them as they stand would prove only that nothing is written.
They are encoded from a field declared with `default: 1`, which makes 0 a value
worth writing, and compared against the DENSE column `serialized.hex`.

## Spellings

Per-harness JSON spelling lives in `harness_dialect.py` and in the options
above: a 64-bit integer is a bare number or a decimal string
(`--int64-json`), and +/-infinity goes in as the string `inf` / `-inf`, the
vector file's own spelling of it, as `Infinity` (a string, or the bare token) or
as the literal `1e999`, whichever the harness's JSON reader takes (`--inf-json`).
"""
import concurrent.futures
import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import harness_dialect as hd  # noqa: E402

PREFIX = "venc"

# The vector groups that are not encode checks, and why. Every other group is
# byte-compared.
EXCLUDED_GROUPS = {
    "skip": "decode-only by design: the message carries fields a decoder must "
            "skip (unknown ids), which no schema-driven encoder emits",
    "skip/matrix": "decode-only by design: the wire-type cross product laid out "
                   "as [read][skipped][anchor] triples to test a decoder's skip",
}

# Vectors encoded against a field whose default is 1 and compared with the
# dense column (see "Id vectors" above).
DENSE_ANCHORED = ("id_max", "id_max_32bit")

# Groups that must have at least one byte-exact vector on every backend.
REQUIRED_GROUPS = (
    "scalar/unsigned", "scalar/signed", "scalar/float", "scalar/string",
    "scalar/boolean", "scalar/blob", "scalar/id",
    "array/integer", "array/float", "array/string", "array/struct", "array/nested",
    "sequence", "composite",
)

# Required groups whose every vector carries a 64-bit value above 2^53-1, so a
# `--int64-safe` run cannot check them.
LOSSY_GROUPS = ("composite",)

# The floor. 77 of the file's 131 vectors are checked (75 on a 16-bit descriptor
# profile, 67 on `int64: number`); the rest are in EXCLUDED_GROUPS or excluded by
# the rules above. A mapping that narrows below this fails instead of passing,
# and the slack leaves room for a vector-file change that adds an excluded shape.
MIN_CHECKED = 60

INT_OPS = ("unsigned", "signed")
SAFE = 2**53 - 1


class Gap(Exception):
    """The driver cannot express a vector. Always a driver bug, never a skip."""


# ---- op list -> tree --------------------------------------------------------

def load_vectors(path):
    """The vector list, with a `-0` literal kept as negative zero.

    The file writes an fp element of -0.0 as the bare integer `-0`, which
    `json.load` reads as the int 0 -- a different fp32, one sign bit apart. Only
    that literal is read differently; every other integer stays an int.
    """
    with open(path) as fh:
        return json.load(
            fh, parse_int=lambda s: -0.0 if s == "-0" else int(s))["vectors"]


def parse(fields):
    """Fold the flat op list into nodes.

    A node is a dict with `id`, `kind` ("leaf", "narray" or "seq"), and for a
    sequence its `children` and `elem`: whether the sequence closes on an op
    carrying `element: true`. For a leaf that flag is the op's own.
    """
    root = {"kind": "seq", "children": [], "id": None}
    stack = [root]
    for f in fields:
        op = f["op"]
        if op == "sequence_begin":
            node = {"kind": "seq", "id": f["id"], "children": [], "elem": False}
            stack[-1]["children"].append(node)
            stack.append(node)
        elif op == "sequence_end":
            stack.pop()["elem"] = bool(f.get("element"))
        elif op == "array":
            stack[-1]["children"].append(
                {"kind": "narray", "id": f["id"], "etype": f["element_type"],
                 "values": f["values"], "elem": bool(f.get("element"))})
        else:
            stack[-1]["children"].append(
                {"kind": "leaf", "id": f["id"], "op": op, "f": f,
                 "elem": bool(f.get("element"))})
    if len(stack) != 1:
        raise Gap("unbalanced sequence_begin/sequence_end")
    return root


def is_wrapper_array(node):
    """A wrapper array is the sequence whose LAST element is marked
    `element: true` (MESSAGE_SPEC §2/§5.1); any other sequence is a struct."""
    return bool(node["children"]) and node["children"][-1]["elem"]


# ---- types ------------------------------------------------------------------
# A type is a tuple, so equal shapes compare equal and share one message. A
# struct lists its fields by id, the order a generated encoder writes them in:
#   ("u64", default) ("i64",) ("fp32",) ("fp64",) ("boolean",)
#   ("string", maxlen) ("blob", maxlen) ("narray", etype, count)
#   ("warray", item, count) ("struct", ((id, type), ...))
# None is a frame that carried nothing, whose kind its siblings decide.

def blob_len(f):
    return len(f["value_hex"]) // 2


def type_of(node):
    k = node["kind"]
    if k == "leaf":
        op, f = node["op"], node["f"]
        if op == "unsigned":
            return ("u64", 0)
        if op == "signed":
            return ("i64",)
        if op in ("fp32", "fp64", "boolean"):
            return (op,)
        if op == "string":
            return ("string", max(len(f["value"].encode()), 1))
        if op == "blob":
            return ("blob", max(blob_len(f), 1))
        raise Gap(f"unknown op {op!r}")
    if k == "narray":
        return ("narray", node["etype"], max(len(node["values"]), 8))
    kids = node["children"]
    if not kids:
        return None
    if is_wrapper_array(node):
        item = None
        for c in kids:
            item = unify(item, type_of(c))
        return ("warray", item, len(kids))
    fields = {}
    for c in kids:
        fields[c["id"]] = unify(fields.get(c["id"]), type_of(c))
    return ("struct", tuple(sorted(fields.items(), key=lambda kv: kv[0])))


def unify(a, b):
    if a is None:
        return b
    if b is None or a == b:
        return a
    if a[0] != b[0]:
        raise Gap(f"cannot unify {a[0]} with {b[0]}")
    t = a[0]
    if t in ("string", "blob"):
        return (t, max(a[1], b[1]))
    if t == "narray" and a[1] == b[1]:
        return (t, a[1], max(a[2], b[2]))
    if t == "warray":
        return (t, unify(a[1], b[1]), max(a[2], b[2]))
    if t == "struct":
        merged = dict(a[1])
        for i, ty in b[1]:
            merged[i] = unify(merged.get(i), ty)
        return (t, tuple(sorted(merged.items(), key=lambda kv: kv[0])))
    raise Gap(f"cannot unify {a} with {b}")


def resolve(t):
    """An unresolved frame is an all-default struct. A struct needs one field to
    be valid, so it declares a placeholder `f0` the vector never sets."""
    if t is None:
        return ("struct", ((0, ("u64", 0)),))
    if t[0] == "warray":
        return ("warray", resolve(t[1]), t[2])
    if t[0] == "struct":
        return ("struct", tuple((i, resolve(ty)) for i, ty in t[1]))
    return t


def schema_of(t):
    """The schema field body for resolved type `t`, minus its id."""
    k = t[0]
    if k == "u64":
        return {"type": "u64", **({"default": t[1]} if t[1] else {})}
    if k in ("i64", "fp32", "fp64", "boolean"):
        return {"type": k}
    if k in ("string", "blob"):
        return {"type": k, "maxlen": t[1]}
    if k == "narray":
        return {"type": "array", "items": {"type": t[1], "count": t[2]}}
    if k == "warray":
        item = schema_of(t[1])
        item["count"] = t[2]
        return {"type": "array", "items": item}
    return {"type": "struct",
            "fields": {f"f{i}": {"id": i, **schema_of(ty)} for i, ty in t[1]}}


# ---- values -----------------------------------------------------------------

def int_in(v, dialect):
    return hd.int64_in(int(v), dialect)


def render(node, t, dialect, inf):
    """The harness JSON for `node`, given its resolved type `t`."""
    k = t[0]
    if node["kind"] == "leaf":
        f = node["f"]
        if k == "u64" or k == "i64":
            return int_in(f["value"], dialect)
        if k == "boolean":
            return bool(f["value"])
        if k in ("fp32", "fp64"):
            return hd.float_in(f["value"], inf)
        if k == "string":
            return f["value"]
        if k == "blob":
            return list(bytes.fromhex(f["value_hex"]))
    if k == "narray":
        wide = t[1] in ("u64", "i64")
        return [int_in(x, dialect) if wide else hd.float_in(x, inf) for x in node["values"]]
    if k == "warray":
        return [render(c, t[1], dialect, inf) for c in node["children"]]
    if k == "struct":
        tys = dict(t[1])
        return {f"f{c['id']}": render(c, tys[c["id"]], dialect, inf)
                for c in node["children"]}
    raise Gap(f"cannot render {k}")


def message_type(vector):
    root = parse(vector["fields"])
    t = type_of(root)
    if t is None or t[0] != "struct":
        raise Gap(f"{vector['name']}: not a message")
    return root, t


# ---- selection --------------------------------------------------------------

def all_ids(fields):
    return [f["id"] for f in fields if "id" in f]


def wide_values(vector):
    for f in vector["fields"]:
        if f["op"] in INT_OPS:
            yield f["value"]
        elif f["op"] == "array" and f["element_type"] in ("u64", "i64"):
            yield from f["values"]


def ids_ascend(fields):
    """True when every message in the op list writes its ids in ascending order.
    A wrapper array's element ids ascend too, so the one rule covers both."""
    last = [-1]
    for f in fields:
        if f["op"] == "sequence_end":
            last.pop()
            continue
        if f["id"] <= last[-1]:
            return False
        last[-1] = f["id"]
        if f["op"] == "sequence_begin":
            last.append(-1)
    return True


def lossy_int64(vector):
    """The reason an `int64: number` build cannot carry this vector, else None."""
    if any(abs(int(v)) > SAFE for v in wide_values(vector)):
        return "64-bit value above 2^53-1 on an `int64: number` build"
    return None


def classify(vector, max_id):
    """Return None when the vector is checked, else the reason it is excluded.

    Only what the SCHEMA depends on is decided here, so `--emit-schema` and the
    run agree on which messages exist. `--int64-safe` changes what is fed to a
    message, never which messages there are, and is applied by `main`."""
    g = vector["group"]
    if g in EXCLUDED_GROUPS:
        return EXCLUDED_GROUPS[g]
    if not ids_ascend(vector["fields"]):
        return ("ops written out of ascending id order; a generated encoder "
                "writes a message's fields by ascending id")
    if max_id is not None and any(i > max_id for i in all_ids(vector["fields"])):
        return f"id above --max-id {max_id} (descriptor profile cannot declare it)"
    return None


def prepare(vectors, max_id):
    """(checked, excluded, messages): checked is [(vector, msg_index, root, type)],
    excluded is [(vector, reason)], messages the ordered unique resolved types."""
    checked, excluded, messages = [], [], []
    for v in vectors:
        why = classify(v, max_id)
        if why:
            excluded.append((v, why))
            continue
        root, t = message_type(v)
        if v["name"] in DENSE_ANCHORED:
            # one anchor field whose 0 is not the default, so it is written
            t = ("struct", tuple((i, ("u64", 1)) for i, _ in t[1]))
        t = resolve(t)
        if t not in messages:
            messages.append(t)
        checked.append((v, messages.index(t), root, t))
    return checked, excluded, messages


def emit_schema(vectors, max_id) -> int:
    _, _, messages = prepare(vectors, max_id)
    print("# The encode-side shared-vector messages (generator#650), printed by")
    print("# tests/conformance/lib/check_vectors_encode.py from the vector file itself,")
    print("# so the schema and the values fed to it have exactly one definition.")
    for n, t in enumerate(messages):
        body = schema_of(t)["fields"]
        print(f"  {PREFIX}{n}: {json.dumps({'payload': body}, separators=(', ', ': '))}")
    return 0


# ---- run --------------------------------------------------------------------

def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def workers():
    return min(8, (os.cpu_count() or 2))


def run_one(cmd, verb, cwd, msg, payload):
    p = subprocess.run(cmd + [verb, msg], input=payload, cwd=cwd,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return p.returncode, p.stdout, p.stderr


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        rest = [a for a in argv if a != "--emit-schema"]
        mx = opt(rest, "--max-id")
        path = next(a for i, a in enumerate(rest)
                    if not a.startswith("--") and (i == 0 or rest[i - 1] != "--max-id"))
        return emit_schema(load_vectors(path), int(mx) if mx else None)

    if "--" not in argv:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    vectors_path, label = head[0], head[1]
    cwd = opt(head, "--cwd")
    verb = opt(head, "--verb", "encode")
    mx = opt(head, "--max-id")
    max_id = int(mx) if mx else None
    dialect = hd.check_dialect(opt(head, "--int64-json", "number"))
    inf = hd.check_inf_dialect(opt(head, "--inf-json", "inf"))
    safe = "--int64-safe" in head
    floor = int(opt(head, "--min-checked", str(MIN_CHECKED)))

    vectors = load_vectors(vectors_path)
    checked, excluded, _ = prepare(vectors, max_id)
    if safe:
        # Not a schema matter (see classify): the message stays, the vector goes.
        kept = []
        for entry in checked:
            why = lossy_int64(entry[0])
            if why:
                excluded.append((entry[0], why))
            else:
                kept.append(entry)
        checked = kept

    jobs = []
    for v, n, root, t in checked:
        payload = hd.dumps_in(render(root, t, dialect, inf), inf).encode()
        want = (v["serialized"] if v["name"] in DENSE_ANCHORED
                else v["serialized_sparse"])["hex"]
        jobs.append((v, f"{PREFIX}{n}", payload, want))

    # One harness process per vector, run concurrently, judged in file order. The
    # first runs alone: a harness that builds itself on first use (the TypeScript
    # bundle) must not be started eight times over before it exists.
    def run_job(j):
        return run_one(cmd, verb, cwd, j[1], j[2])

    outcomes = [run_job(jobs[0])] if jobs else []
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers()) as pool:
        outcomes += list(pool.map(run_job, jobs[1:]))

    ran, per_group, failed = 0, {}, []
    for (v, msg, payload, want), (rc, out, err) in zip(jobs, outcomes):
        if rc != 0:
            failed.append(f"FAIL vector {v['name']}: {verb} {msg} exited {rc}: "
                          f"{err.decode(errors='replace').strip()[:400]}")
            continue
        got = out.hex()
        if got != want:
            failed.append(f"FAIL vector {v['name']} ({v['group']}): got {got} want {want}\n"
                          f"     input {payload.decode()[:300]}")
            continue
        ran += 1
        per_group[v["group"]] = per_group.get(v["group"], 0) + 1

    # Only a vector that reached a byte-exact match counts as checked, so a
    # vector the harness could not run (a message missing from the schema, say)
    # leaves the books unbalanced instead of lowering the count quietly.
    total = len(vectors)
    if failed or ran + len(excluded) != total:
        for line in failed[:10]:
            print(line)
        if len(failed) > 10:
            print(f"... and {len(failed) - 10} more")
        print(f"FAIL: checked + excluded != total ({ran} + {len(excluded)} != {total}):"
              f" {len(failed)} vector(s) did not match byte for byte")
        return 1
    if ran < floor:
        print(f"FAIL: checked {ran} vectors, below the floor of {floor}")
        return 1
    # `composite` is one vector, and it carries u64/i64 arrays out to +-2^63..2^64:
    # an `int64: number` build cannot hold them, so there the group is not
    # required (the bigint and long builds of the same target are).
    required = [g for g in REQUIRED_GROUPS if not (safe and g in LOSSY_GROUPS)]
    absent = [g for g in required if not per_group.get(g)]
    if absent:
        print(f"FAIL: no byte-exact vector in group(s) {', '.join(absent)}")
        return 1

    why = {}
    for v, reason in excluded:
        why.setdefault(reason, []).append(v["group"])
    groups = ", ".join(f"{g} {n}" for g, n in sorted(per_group.items()))
    print(f"{label} shared-vector encode conformance: {ran} byte-exact, "
          f"{len(excluded)} excluded, {total} total")
    print(f"  checked by group: {groups}")
    for reason, gs in why.items():
        names = ", ".join(f"{g} x{gs.count(g)}" for g in sorted(set(gs)))
        print(f"  excluded ({names}): {reason}")
        if reason not in EXCLUDED_GROUPS.values():
            print("    " + ", ".join(v["name"] for v, r in excluded if r == reason))
    return 0


if __name__ == "__main__":
    sys.exit(main())
