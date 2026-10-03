#!/usr/bin/env python3
"""Drive a generated harness's DECODE path against the shared wire vectors.

Usage:
  check_vectors_decode.py --emit-schema [--max-id N]
  check_vectors_decode.py <test_vectors.json> <label>
                          [--cwd DIR] [--mode MODE] [--max-id N] [--int64-safe]
                          -- <harness argv...>
  check_vectors_decode.py --typed <test_vectors.json> <label>
                          [--cwd DIR] [--mode MODE] [--max-id N] [--int64-safe]
                          [--min-checked N] [--nonfinite-null] -- <harness argv...>

The companion `check_vectors_encode.py` drives the *encode* direction and compares
`serialized_sparse`. This one drives the other half, which no tier covered
before (generator#444): it feeds each vector's `serialized.hex` -- the dense
column, which is what a decoder actually receives -- into

    <harness argv...> <mode> vecskip        (MODE defaults to `decode`)

on stdin, and asserts the fields the schema knows come back with their exact
values while everything else is skipped.

## What makes this a skip test

`vecskip` -- printed by `--emit-schema` and appended to each language's
conformance schema, so all of them declare it identically -- declares `u64`, and
only `u64`, on the ids the shared vectors put their unsigned *anchors* on. So for
any vector, every field on the wire is one of three things:

  * a declared id carrying the unsigned wire type -- read, value asserted;
  * an id the schema does not declare -- an unknown field, skipped
    (ARCHITECTURE §5/§9.1);
  * a declared id carrying some *other* wire type -- a MESSAGE_SPEC §7.3
    mismatch, skipped exactly as an unknown id is, the field keeping its
    default (ARCHITECTURE §12).

The third case is why the schema does not need to match the vector, and it is
the reason this driver reaches further than the vectors alone do: a `signed`,
`string`, `blob`, array or sequence sitting on a declared id is a §7.3 case the
generated file cannot express (its encoder only produces well-typed messages),
and it is checked here on every backend, from real bytes, without hand-building
a fixture.

The detector is the anchor behind each skip. Group `skip/matrix` lays the whole
wire-type cross product out as `[read P] [skipped S] [anchor]` triples, so a
skip that consumes one byte too few or too many leaves the decoder inside the
next field and the anchor's value comparison fails.

`skip_ids` is not consulted: the schema *is* the declining mechanism here, and
it declines strictly more than `skip_ids` names.

## `--typed`: the values, not just the skips

`vecskip` reads `u64` and nothing else, so on its own it never asserts a signed,
float, string, blob, array or sequence value (generator#651). `--typed` is the
other half: it feeds the same dense bytes into the message that
`check_vectors_encode.py --emit-schema` derives from the vector's own op list
(`venc<N>`, so one schema serves both directions) and compares what the harness
prints with the vector's `fields`, as data and by the vector's types:

  * 64-bit integers in either spelling (`harness_dialect.as_int`);
  * floats by value -- an fp32 by its IEEE-754 single bits, so a harness that
    prints the shortest decimal for the float still matches -- with NaN equal to
    NaN, signed zero kept, and infinities read through whichever spelling the
    harness uses (`inf`, `Infinity`, a bare token, `1e999`);
  * with `--nonfinite-null`, a harness whose JSON writer prints null for an
    infinity or a NaN (serde_json): a non-finite expectation is met by null, the
    count is printed, and the sign of an infinity is not asserted there;
  * blobs and `u8` arrays as a JSON array or base64 (`harness_dialect.bytes_of`);
  * a struct by the keys the vector sets, a wrapper array element by element.

A vector whose ops are not in ascending id order has no byte-exact ENCODE (a
generated encoder writes ascending ids) but is a valid DECODE input, so it is
checked here. Every vector with a message is either compared or named in the
excluded list with its reason; the run prints both counts and the per-group
breakdown, and fails when a required group has no compared vector.

## `--mode`

The same vectors and the same expectations against a different decode surface.
`--mode streamdecode` drives a harness mode that feeds the decoder **one byte at
a time**, matching the corelibs' chunked scenario: every position inside every
skipped payload becomes a suspend/resume boundary, which is where a resync bug
the single-buffer path hides shows up. Only harnesses that emit such a mode can
be driven this way; the rest run the one-shot `decode` surface only.

## `--max-id`

The ceiling on the ids `vecskip` may declare, for targets whose descriptor
profile cannot hold the largest of them (corelib-c-cpp's default
`SOFAB_OBJECT_DESCR_PROFILE` is 16-bit, so C tops out at 65535). It must be
passed identically to `--emit-schema` and to the run, or the expectations stop
matching the schema. Dropping an id does not drop a vector: the ids above the
ceiling simply stop being *read*, so the fields on them are skipped like any
other unknown id and the vector still has to decode cleanly to its end.

## `--int64-safe`

For a harness whose 64-bit scalar is a JS `number` (TypeScript `int64: number`),
documented lossy above 2^53. An anchor above 2^53-1 cannot come back exact, so it
is compared to the precision of a double (relative error <= 2^-52) instead of
exactly. The decode is still driven over every vector and every other anchor,
and every skipped field, is still asserted exactly; the number of anchors held
to the weaker comparison is printed.

## Loud, never quiet

A driver that silently narrows what it selects passes while testing less than it
claims -- the failure mode `check_vectors_encode.py`'s checked-plus-excluded balance exists for,
and the one the upstream C harness hit with a fixed `MAXSKIP` that truncated an
over-long `skip_ids` list. So: every vector in the file is run, a harness that
exits non-zero (a decoder that rejects rather than skips) is a failure and not a
skip, the vector count is printed, and a run that did not reach every vector
fails.
"""
import concurrent.futures
import json
import math
import os
import struct
import subprocess
import sys

# The ids `vecskip` declares as u64. Ids 0..26 are every top-level `unsigned` id
# in the shared suite; 100001 is the anchor behind `skip_large_id`'s three-byte
# header varint (MESSAGE_SPEC §4.3). An id no vector uses would only ever assert
# its default, so this list is exactly the readable-anchor set and nothing more.
DECLARED = list(range(27)) + [100001]

# Wire types a `u64` field accepts. §4.4 puts `boolean` on the wire as the
# unsigned integer 0/1, so it is NOT a §7.3 mismatch against `u64`; every other
# op is.
UNSIGNED_OPS = ("unsigned", "boolean")


def declared(max_id):
    return [i for i in DECLARED if max_id is None or i <= max_id]


def emit_schema(max_id) -> int:
    """Print the `vecskip` message, for appending to a conformance schema."""
    print("# vecskip -- the decode-side shared-vector message (generator#444), printed by")
    print("# tests/conformance/lib/check_vectors_decode.py so the schema and the")
    print("# expectations asserted against it have exactly one definition between them.")
    print("  vecskip:")
    print("    payload:")
    for fid in declared(max_id):
        print(f"      f{fid}: {{ id: {fid}, type: u64 }}")
    return 0


def expected(vector, max_id) -> dict:
    """The value each declared field must hold after decoding `serialized.hex`.

    A declared id is read only when the *top-level* op on it carries the
    unsigned-varint wire type. Anything else there is a §7.3 mismatch and leaves
    the field at its default, and so does an id that never appears. Ops nested
    inside a sequence are not top-level and cannot reach a top-level id, so the
    walk tracks depth. On a repeated id the last occurrence wins (MESSAGE_SPEC
    §7.4); no shared vector currently exercises that at top level.
    """
    out = {fid: 0 for fid in declared(max_id)}
    depth = 0
    for f in vector["fields"]:
        op = f["op"]
        if op == "sequence_end":
            depth -= 1
            continue
        if depth == 0 and op in UNSIGNED_OPS and f["id"] in out:
            out[f["id"]] = int(f["value"]) if op == "boolean" else f["value"]
        if op == "sequence_begin":
            depth += 1
    return out


def as_int(v):
    """Harness JSON carries u64 as a number or, where the language's own integer
    is too narrow to survive JSON (Dart, TypeScript), as an unsigned-decimal
    string. Both normalize to the same Python int."""
    if isinstance(v, bool):
        raise ValueError("boolean in a u64 field")
    if isinstance(v, str):
        return int(v, 10)
    if isinstance(v, float):
        if v != int(v):
            raise ValueError(f"non-integral u64 field: {v}")
        return int(v)
    return int(v)


def workers():
    return min(8, (os.cpu_count() or 2))


def run_one(cmd, mode, cwd, vector):
    p = subprocess.run(
        cmd + [mode, "vecskip"],
        input=bytes.fromhex(vector["serialized"]["hex"]),
        cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return p.returncode, p.stdout, p.stderr


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


# ---- --typed ----------------------------------------------------------------

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import harness_dialect as hd  # noqa: E402
import check_vectors_encode as enc  # noqa: E402

INT_TYPES = ("u8", "u16", "u32", "u64", "i8", "i16", "i32", "i64")
# The floor of vectors compared by value: 79 of the file's 131 (75 on a 16-bit
# descriptor profile, 69 on `int64: number`); the rest are the skip-only groups. A
# mapping that narrows below it fails instead of passing.
TYPED_MIN_CHECKED = 60


def f32(x):
    return struct.pack("<f", x)


# --nonfinite-null: the harness's JSON writer has no spelling for an infinity or a
# NaN and prints null (serde_json), so a non-finite expectation is met by null and
# the sign of an infinity is not asserted. Counted and printed, never silent.
NONFINITE_NULL = {"on": False, "n": 0}


def float_same(got, want, single):
    w = float(want)          # the vector's "inf" / "-inf" / number
    if got is None and NONFINITE_NULL["on"] and not math.isfinite(w):
        NONFINITE_NULL["n"] += 1
        return True
    g = hd.as_float(got)
    if g is None:
        return False
    if math.isnan(w) or math.isnan(g):
        return math.isnan(w) and math.isnan(g)
    if single:
        try:
            return f32(g) == f32(w)
        except OverflowError:   # a double outside float32 range is an infinity there
            return math.isinf(w) and math.copysign(1, g) == math.copysign(1, w)
    return g == w and math.copysign(1, g) == math.copysign(1, w)


def int_same(got, want):
    g = hd.as_int(got)
    return g is not None and g == int(want)


def same(t, want, got):
    """True when the harness value `got` is the rendering of `want` under type `t`."""
    k = t[0]
    if got is None and k in ("narray", "warray"):
        got = []     # a nil slice (Go) is an empty array
    if k in ("u64", "i64"):
        return int_same(got, want)
    if k == "boolean":
        return got in (True, False, 0, 1) and bool(got) == bool(want)
    if k in ("fp32", "fp64"):
        return float_same(got, want, k == "fp32")
    if k == "string":
        return got == want
    if k == "blob":
        return hd.bytes_of(got) == bytes(want)
    if k == "narray":
        if t[1] == "u8" and not isinstance(got, list):
            return hd.bytes_of(got) == bytes(want)
        if not isinstance(got, list) or len(got) != len(want):
            return False
        if t[1] in INT_TYPES:
            return all(int_same(g, w) for g, w in zip(got, want))
        return all(float_same(g, w, t[1] == "fp32") for g, w in zip(got, want))
    if k == "warray":
        return (isinstance(got, list) and len(got) == len(want)
                and all(same(t[1], w, g) for g, w in zip(got, want)))
    if k == "struct":
        tys = dict(t[1])
        if not isinstance(got, dict):
            return False
        for name, w in want.items():
            if name not in got or not same(tys[int(name[1:])], w, got[name]):
                return False
        return True
    return False


def run_typed(head, cmd) -> int:
    vectors_path, label = head[0], head[1]
    cwd = opt(head, "--cwd")
    mode = opt(head, "--mode", "decode")
    mx = opt(head, "--max-id")
    max_id = int(mx) if mx else None
    safe = "--int64-safe" in head
    NONFINITE_NULL["on"] = "--nonfinite-null" in head
    floor = int(opt(head, "--min-checked", str(TYPED_MIN_CHECKED)))

    vectors = enc.load_vectors(vectors_path)
    _, excluded, _, decodable = enc.prepare(vectors, max_id)
    # Only a vector with no message is excluded from DECODE; the encode-only
    # exclusion (ids out of order) is not one.
    dropped = [(v, r) for v, r in excluded if enc.schema_reason(v, max_id)]
    jobs = []
    for v, n, root, t in decodable:
        lossy = enc.lossy_int64(v) if safe else None
        if lossy:
            dropped.append((v, lossy))
            continue
        jobs.append((v, f"{enc.PREFIX}{n}", root, t))

    def run_job(j):
        p = subprocess.run(cmd + [mode, j[1]], input=bytes.fromhex(j[0]["serialized"]["hex"]),
                           cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        return p.returncode, p.stdout, p.stderr

    # First alone: a harness that builds itself on first use must not start eight times.
    outcomes = [run_job(jobs[0])] if jobs else []
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers()) as pool:
        outcomes += list(pool.map(run_job, jobs[1:]))

    failed, per_group = [], {}
    for (v, msg, root, t), (rc, out, err) in zip(jobs, outcomes):
        if rc != 0:
            failed.append(f"FAIL vector {v['name']}: {mode} {msg} exited {rc}: "
                          f"{err.decode(errors='replace').strip()[:300]}")
            continue
        try:
            # Non-finite floats and `-0` in every spelling a harness uses.
            got = hd.loads_out(out.decode())
        except ValueError:
            failed.append(f"FAIL vector {v['name']}: harness printed no JSON: "
                          f"{out.decode(errors='replace')[:200]!r}")
            continue
        want = enc.render(root, t, "number", "inf")
        if not same(t, want, got):
            failed.append(f"FAIL vector {v['name']} ({v['group']}): decoded "
                          f"{json.dumps(got)[:300]}\n     want {json.dumps(want)[:300]}\n"
                          f"     wire {v['serialized']['hex']}")
            continue
        per_group[v["group"]] = per_group.get(v["group"], 0) + 1

    ran, total = sum(per_group.values()), len(vectors)
    if failed or ran + len(dropped) != total:
        for line in failed[:10]:
            print(line)
        if len(failed) > 10:
            print(f"... and {len(failed) - 10} more")
        print(f"FAIL: compared + excluded != total ({ran} + {len(dropped)} != {total}): "
              f"{len(failed)} vector(s) did not decode to their values")
        return 1
    if ran < floor:
        print(f"FAIL: compared {ran} vectors, below the floor of {floor}")
        return 1
    required = [g for g in enc.REQUIRED_GROUPS if not (safe and g in enc.LOSSY_GROUPS)]
    absent = [g for g in required if not per_group.get(g)]
    if absent:
        print(f"FAIL: no value-compared vector in group(s) {', '.join(absent)}")
        return 1
    groups = ", ".join(f"{g} {n}" for g, n in sorted(per_group.items()))
    print(f"{label} shared-vector typed decode [{mode}]: {ran} vectors decoded to "
          f"their values, {len(dropped)} excluded, {total} total")
    print(f"  compared by group: {groups}")
    why = {}
    for v, reason in dropped:
        why.setdefault(reason, []).append(v["group"])
    for reason, gs in why.items():
        names = ", ".join(f"{g} x{gs.count(g)}" for g in sorted(set(gs)))
        print(f"  excluded ({names}): {reason}")
    if NONFINITE_NULL["on"]:
        print(f"  --nonfinite-null: {NONFINITE_NULL['n']} non-finite floats met by null "
              f"(the harness's JSON writer cannot spell them; the sign of an infinity is "
              f"not asserted)")
    return 0


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        mx = opt(argv, "--max-id")
        return emit_schema(int(mx) if mx else None)

    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    if "--typed" in head:
        return run_typed([a for a in head if a != "--typed"], cmd)
    vectors_path, label = head[0], head[1]
    cwd = opt(head, "--cwd")
    mode = opt(head, "--mode", "decode")
    mx = opt(head, "--max-id")
    max_id = int(mx) if mx else None
    safe = "--int64-safe" in head
    lossy = 0

    with open(vectors_path) as fh:
        vectors = json.load(fh)["vectors"]

    # One harness process per vector, run concurrently. Process startup dominates
    # everything else here -- a JVM, a `dotnet` host or an `npx tsx` transpile is
    # far more expensive than decoding 36 bytes -- and the runs are independent,
    # so the pool turns a serial minute into a few seconds. Results are collected
    # by index and judged in file order, so which vector is reported does not
    # depend on which process happened to finish first.
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers()) as pool:
        outcomes = list(pool.map(lambda v: run_one(cmd, mode, cwd, v), vectors))

    checked = skips = 0
    for v, (rc, out, err) in zip(vectors, outcomes):
        if rc != 0:
            # Not a skip: a vector this driver cannot decode is a decoder that
            # rejected a well-formed message instead of skipping past what it
            # does not declare.
            print(f"FAIL vector {v['name']}: {mode} exited {rc}: "
                  f"{err.decode(errors='replace').strip()}")
            return 1
        try:
            got = json.loads(out.decode())
        except ValueError:
            print(f"FAIL vector {v['name']}: harness printed no JSON: "
                  f"{out.decode(errors='replace')[:200]!r}")
            return 1
        # A field the harness never printed reads as its default below, which is
        # the right reading for a skipped field but indistinguishable from a
        # harness that renamed or dropped the whole set -- and 51 of the vectors
        # expect nothing but defaults, so such a harness would sail through them.
        # Demand the names once, against the first vector.
        missing = [f"f{fid}" for fid in declared(max_id) if f"f{fid}" not in got]
        if missing and checked == 0:
            print(f"FAIL vector {v['name']}: harness JSON is missing "
                  f"{len(missing)} of the declared fields ({', '.join(missing[:5])}"
                  f"{', ...' if len(missing) > 5 else ''}) -- it is not rendering "
                  f"the vecskip message this driver asserts against")
            return 1
        for fid, wv in expected(v, max_id).items():
            gv = as_int(got.get(f"f{fid}", 0))
            if safe and wv > 2**53 - 1:
                lossy += 1
                if abs(gv - wv) <= wv * 2**-52:
                    continue
            if gv != wv:
                print(f"FAIL vector {v['name']}: field f{fid} decoded {gv}, want {wv}")
                print(f"     wire {v['serialized']['hex']}")
                return 1
        skips += 1 if v.get("skip_ids") else 0
        checked += 1

    if checked != len(vectors):
        print(f"FAIL: decoded {checked} of {len(vectors)} vectors")
        return 1
    print(f"{label} shared-vector decode conformance [{mode}]: {checked} vectors "
          f"decoded ({skips} carrying skip_ids), every undeclared or "
          f"§7.3-mismatched field skipped")
    if safe:
        print(f"  --int64-safe: {lossy} anchors above 2^53 compared to double precision")
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
