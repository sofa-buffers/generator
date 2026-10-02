#!/usr/bin/env python3
"""CORELIB_PLAN §4.4 -- a boolean is read tolerantly and written canonically (generator#644).

Usage:
  check_boolean_tolerant.py --emit-schema
  check_boolean_tolerant.py <label> <test_vectors.json> [--cwd DIR] [--verb VERB]
                            [--skip-requires TAG]... [--message NAME]
                            -- <harness argv...>

## The rule

A decoder reads EVERY value other than `0` as `true` -- `2`, `256` and `2^64-1`
included. Such a value is not INVALID, it is normalised away, and a re-encode
emits `1`. A boolean carries no width bound at all (CORELIB_PLAN §4.4,
MESSAGE_SPEC §1 type table).

## Why a shared driver

The rule is applied in different places on different targets: in the corelib's
read kind, in a typed destination, or in generated code (Go emits `v != 0`).
Whichever it is, a wrong answer is silent -- 0 and 1 are right on every target.
The main `vectors` list carries only canonical booleans, so no other check
reaches it. The fixtures are the shared ones: the `boolean_tolerant` block of
the corelibs' `assets/test_vectors.json` (crucible#189), the authority on what
each raw value means. This driver reads nothing else.

## What runs

Each vector sits on field id 0. It is moved onto every position of its kind
(scalar vectors on scalar positions, `requires: array` vectors on array
positions) by changing the tag byte only -- the payload, the count and the
elements stay the shared ones -- and wrapped in the enclosing sequences where
the position is nested. Then

  1. `<harness> <verb> <message>` decodes the wire; the position must hold
     `expect.values`;
  2. the decoded JSON goes to `<harness> encode <message>`; the bytes must be
     the block's `reencoded_hex`, re-tagged and wrapped the same way, with the
     sparse-default rule applied (MESSAGE_SPEC §2: a value equal to its default
     is not written, so a scalar `false` re-encodes to nothing). A union arm
     other than the default option is written even at its default.

The positions are the ones that take different generated code: a scalar, a
bounded array, a scalar and an array inside a struct, a row of an array of
arrays, a depth-3 row, and a scalar and an array arm of a union.

`--verb` names the decode surface (`decode`, the default, or `streamdecode`).
`--skip-requires TAG` skips the vectors that list TAG in `requires` and prints
the skip on the final line. The only legitimate use is `int64`, for a build that
cannot hold a 64-bit varint; every call site must comment why.

## Loud, never quiet

A missing block, a position that receives no vector and a case count that is not
the expected one are failures. The final line names the vector and position
counts and any skip.
"""
import concurrent.futures
import json
import os
import subprocess
import sys

MSG = "bt"

# Field ids, shared by the emitted schema and the wire builders.
FLAG, FLAGS, ROWS, NESTED, CHOICE, CUBE = 0, 1, 2, 3, 4, 5
NEST_INNER, NEST_FLAG = 0, 1          # inside `nested`
OPT_OTHER, OPT_BITS, OPT_FLAG = 0, 1, 2   # inside `choice`; `other` is default_id
COUNT = 5     # the longest vector array holds 5 elements

SEQ = 6
END = b"\x07"


def emit_schema() -> int:
    print("# CORELIB_PLAN §4.4 in every position a boolean reaches generated code")
    print("# (generator#644), printed by tests/conformance/lib/check_boolean_tolerant.py so")
    print("# the schema and the driver that re-tags the shared vectors have one definition.")
    print("version: 1")
    print("messages:")
    print(f"  {MSG}:")
    print("    payload:")
    print(f"      flag:   {{ id: {FLAG}, type: boolean }}")
    print(f"      flags:  {{ id: {FLAGS}, type: array, items: {{ type: boolean, count: {COUNT} }} }}")
    print(f"      rows:   {{ id: {ROWS}, type: array, items: {{ type: array, count: 2,"
          f" items: {{ type: boolean, count: {COUNT} }} }} }}")
    print(f"      nested: {{ id: {NESTED}, type: struct, fields: {{"
          f" inner: {{ id: {NEST_INNER}, type: array, items: {{ type: boolean, count: {COUNT} }} }},"
          f" flag: {{ id: {NEST_FLAG}, type: boolean }} }} }}")
    print(f"      choice: {{ id: {CHOICE}, type: union, oneof: {{ other: {{ id: {OPT_OTHER}, type: u8 }},"
          f" bits: {{ id: {OPT_BITS}, type: array, items: {{ type: boolean, count: {COUNT} }} }},"
          f" flag: {{ id: {OPT_FLAG}, type: boolean }} }} }}")
    print(f"      cube:   {{ id: {CUBE}, type: array, items: {{ type: array, count: 2,"
          f" items: {{ type: array, count: 2, items: {{ type: boolean, count: {COUNT} }} }} }} }}")
    return 0


# --- the wire image -----------------------------------------------------------

def retag(payload: bytes, field_id: int) -> bytes:
    """The block's bytes, moved from field id 0 onto `field_id`.

    A field header is the varint `(id << 3) | wire_type`. Every vector sits on
    id 0, so its first byte IS the bare wire type; re-tagging is that byte with
    the id shifted in. Nothing after the header is touched.
    """
    wt = payload[0]
    if wt > 7:
        raise SystemExit(f"vector header {wt:#04x} is not a bare id-0 tag byte")
    tag = (field_id << 3) | wt
    if tag > 0x7F:
        raise SystemExit(f"re-tagged header {tag} would need a second varint byte")
    return bytes([tag]) + payload[1:]


def seq(fid: int, *parts: bytes) -> bytes:
    return bytes([(fid << 3) | SEQ]) + b"".join(parts) + END


# A position: name, kind ("scalar"/"array"), the JSON path to its value, the
# schema default, whether a default is still written (a non-default union arm),
# and how the retagged vector is wrapped on the wire.
POSITIONS = [
    ("flag", "scalar", ["flag"], False, False,
     lambda v: retag(v, FLAG)),
    ("flags", "array", ["flags"], [], False,
     lambda v: retag(v, FLAGS)),
    ("rows", "array", ["rows"], [], False,
     lambda v: seq(ROWS, retag(v, 0))),
    ("nested.flag", "scalar", ["nested", "flag"], False, False,
     lambda v: seq(NESTED, retag(v, NEST_FLAG))),
    ("nested.inner", "array", ["nested", "inner"], [], False,
     lambda v: seq(NESTED, retag(v, NEST_INNER))),
    ("choice.flag", "scalar", ["choice", "flag"], False, True,
     lambda v: seq(CHOICE, retag(v, OPT_FLAG))),
    ("choice.bits", "array", ["choice", "bits"], [], True,
     lambda v: seq(CHOICE, retag(v, OPT_BITS))),
    ("cube", "array", ["cube"], [], False,
     lambda v: seq(CUBE, seq(0, retag(v, 0)))),
]


def expected_value(kind, path, vec):
    values = vec["expect"]["values"]
    v = values[0] if kind == "scalar" else list(values)
    if path == ["rows"]:
        return [v]
    if path == ["cube"]:
        return [[v]]
    return v


def get(obj, path):
    for k in path:
        if not isinstance(obj, dict) or k not in obj:
            return None, f"{'.'.join(path)}: missing from the decoded object"
        obj = obj[k]
    return obj, None


def same(want, got) -> bool:
    """A boolean is `true`/`false`, or 0/1; anything else (a leaked 2) differs."""
    if isinstance(want, list):
        return isinstance(got, list) and len(want) == len(got) \
            and all(same(w, g) for w, g in zip(want, got))
    if isinstance(got, bool):
        return got is want
    return type(got) is int and got in (0, 1) and bool(got) is want


def want_reencode(vec, kind, path, default, written, wrap) -> bytes:
    """The bytes generated code must re-emit for this vector at this position."""
    values = vec["expect"]["values"]
    got = values[0] if kind == "scalar" else values
    if got == default and not written:
        return b""
    return wrap(bytes.fromhex(vec["expect"]["reencoded_hex"]))


# --- running a harness --------------------------------------------------------

def diagnostic(stderr: bytes) -> str:
    lines = [l.strip() for l in stderr.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:]) if lines else "<no output>"


def run(cmd, argv, data, cwd):
    p = subprocess.run(cmd + argv, input=data, cwd=cwd,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return p.returncode, p.stdout, p.stderr


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def opts(args, name):
    return [args[i + 1] for i, a in enumerate(args) if a == name]


def one_case(cmd, cwd, verb, msg, pos, vec):
    """Returns a list of failure strings for one vector at one position."""
    name, kind, path, default, written, wrap = pos
    tag = f"{name}/{vec['name']} [{verb}]"
    wire = wrap(bytes.fromhex(vec["serialized_hex"]))
    rc, out, err = run(cmd, [verb, msg], wire, cwd)
    if rc != 0:
        return [f"{tag}: decode exited {rc}: {diagnostic(err)}; bytes {wire.hex()} "
                f"(§4.4: no value may be refused)"]
    try:
        obj = json.loads(out.decode())
    except ValueError:
        return [f"{tag}: harness printed no JSON: {out[:200]!r}"]
    got, problem = get(obj, path)
    if problem:
        return [f"{tag}: {problem}"]
    want = expected_value(kind, path, vec)
    if path[0] == "choice" and (not isinstance(obj["choice"], dict) or list(obj["choice"]) != [path[1]]):
        return [f"{tag}: a union holds exactly ONE option, {path[1]!r}; got {json.dumps(obj['choice'])}"]
    if not same(want, got):
        return [f"{tag}: decoded {json.dumps(got)}, want {json.dumps(want)}; bytes {wire.hex()}"]

    rc, out, err = run(cmd, ["encode", msg], json.dumps(obj).encode(), cwd)
    if rc != 0:
        return [f"{tag}: re-encode exited {rc}: {diagnostic(err)}"]
    exp = want_reencode(vec, kind, path, default, written, wrap)
    if out != exp:
        return [f"{tag}: re-encoded {out.hex() or '<empty>'}, want {exp.hex() or '<empty>'} "
                f"(§4.4 is canonical on encode); decoded wire {wire.hex()}"]
    return []


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    label, vectors_path = head[0], head[1]
    cwd = opt(head, "--cwd")
    verb = opt(head, "--verb", "decode")
    msg = opt(head, "--message", MSG)
    skip = opts(head, "--skip-requires")

    with open(vectors_path) as fh:
        block = json.load(fh).get("boolean_tolerant")
    if not block:
        print(f"FAIL {label}: {vectors_path} carries no 'boolean_tolerant' block (crucible#189)")
        return 1

    skipped = [v for v in block if set(v.get("requires", [])) & set(skip)]
    live = [v for v in block if v not in skipped]
    scalars = [v for v in live if "array" not in v.get("requires", [])]
    arrays = [v for v in live if "array" in v.get("requires", [])]
    if not scalars or not arrays:
        print(f"FAIL {label}: {len(scalars)} scalar and {len(arrays)} array vectors left; both halves are required")
        return 1

    jobs = []
    per_pos = {}
    for pos in POSITIONS:
        vecs = scalars if pos[1] == "scalar" else arrays
        per_pos[pos[0]] = len(vecs)
        jobs += [(pos, v) for v in vecs]
    empty = [n for n, c in per_pos.items() if c == 0]
    if empty:
        print(f"FAIL {label}: no vector reached position(s) {', '.join(empty)}")
        return 1

    workers = min(8, os.cpu_count() or 2)
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        results = list(pool.map(lambda j: one_case(cmd, cwd, verb, msg, j[0], j[1]), jobs))
    failed = [f for r in results for f in r]
    want_n = len(scalars) * sum(1 for p in POSITIONS if p[1] == "scalar") \
        + len(arrays) * sum(1 for p in POSITIONS if p[1] == "array")
    if len(results) != want_n:
        failed.append(f"ran {len(results)} cases, expected {want_n}")

    if failed:
        print(f"FAIL {label} §4.4 boolean tolerant decode [{verb}]: {len(failed)} failures")
        for f in failed:
            print("  " + f)
        return 1
    note = (f"; SKIPPED {len(skipped)} vector(s) requiring {','.join(skip)}: "
            f"{', '.join(v['name'] for v in skipped)}") if skipped else "; no skips"
    print(f"{label} §4.4 boolean tolerant decode [{verb}]: {len(live)} vectors "
          f"({len(scalars)} scalar, {len(arrays)} array) x {len(POSITIONS)} positions = {len(results)} "
          f"cases, each decoded and re-encoded{note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
