#!/usr/bin/env python3
"""Drive a generated harness's DECODE path against the shared wire vectors.

Usage:
  check_vectors_decode.py --emit-schema [--max-id N]
  check_vectors_decode.py <test_vectors.json> <label>
                          [--cwd DIR] [--mode MODE] [--max-id N] -- <harness argv...>

The companion `check_vectors.py` drives the *encode* direction and compares
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

## Loud, never quiet

A driver that silently narrows what it selects passes while testing less than it
claims -- the failure mode `check_vectors.py`'s `checked == 0` guard exists for,
and the one the upstream C harness hit with a fixed `MAXSKIP` that truncated an
over-long `skip_ids` list. So: every vector in the file is run, a harness that
exits non-zero (a decoder that rejects rather than skips) is a failure and not a
skip, the vector count is printed, and a run that did not reach every vector
fails.
"""
import concurrent.futures
import json
import os
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


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        mx = opt(argv, "--max-id")
        return emit_schema(int(mx) if mx else None)

    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    vectors_path, label = head[0], head[1]
    cwd = opt(head, "--cwd")
    mode = opt(head, "--mode", "decode")
    mx = opt(head, "--max-id")
    max_id = int(mx) if mx else None

    vectors = json.load(open(vectors_path))["vectors"]

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
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
