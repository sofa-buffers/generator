#!/usr/bin/env python3
"""Footprint feature subsets, RUN (generator#659) -- C and C++ `corelib: c-cpp`.

Usage:
  check_feature_subset.py --list LANG            names of the rows LANG runs (c | cpp)
  check_feature_subset.py --schema NAME          the row's schema, one message `m`
  check_feature_subset.py --flags NAME           the row's -DSOFAB_DISABLE_* flags
  check_feature_subset.py <label> NAME [--status-pattern RE] --full <harness...> --
                          <stripped harness argv...>

## What a stripped build is

corelib-c-cpp can be built with `SOFAB_DISABLE_{FIXLEN,ARRAY,SEQUENCE,FP64,INT64}_
SUPPORT` to drop whole wire types for a smaller footprint. CORELIB_PLAN §6.2.2
calls that a type loss: the build is a format SUBSET by design, an unknown id that
carries a wire type it does not have is INVALID rather than skipped (§5.2.2), and
conformance is measured on the full build, so the shared vectors are NOT expected
to pass here. What a test owes such a build is two things, and this is both:

  1. the features left ENABLED work end to end: a message of the row's schema
     encodes on the stripped build to the same bytes the full build writes, and
     the stripped build decodes the full build's bytes back to the same value;
  2. a feature that is DISABLED is refused as specified: a wire type that needs
     it, at an id the schema does not declare, decodes as INVALID -- on the
     one-shot and on the streaming surface -- while the same bytes decode as
     COMPLETE (skipped) on the full build.

Until this existed the rows were only COMPILED (`gcc -c`, `g++ -fsyntax-only`):
nothing linked the stripped corelib, so a stripped branch that failed at run time,
or one that skipped a payload it had compiled out, stayed green.

## The probes

One wire fragment per feature, all at an unknown id (30) so the schema never
declares it. A probe is refused when ANY feature it needs is disabled, and must
be SKIPPED (COMPLETE) when every one of them is enabled -- the second half is what
stops a stripped build from passing by refusing everything:

    fixlen    a string fixlen field                        needs FIXLEN
    fp64      a fixlen field with the fp64 subtype         needs FIXLEN + FP64
    array     an unsigned varint array                     needs ARRAY
    sequence  an empty sequence                            needs SEQUENCE
    int64     a varint wider than 32 bits                  needs INT64

Each row's disabled set is read from the flags it is built with, so the table
below is the single place a row is defined.

## Loud, never quiet

A row that runs no probe, a harness that prints something other than a verdict and
a round trip that is empty are failures.
"""
import argparse
import json
import os
import struct
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import json_equal  # noqa: E402

MSG = "m"
UNKNOWN_ID = 30

ALL = ("FIXLEN", "ARRAY", "SEQUENCE", "FP64", "INT64")

# name -> (languages, disabled features, schema payload, sample value).
# The sample sets every field to a non-default value, so a field the stripped
# build loses cannot hide behind its default.
ROWS = {
    # Everything stripped: scalars and booleans only.
    "min": (
        ("c",), ALL,
        "{ a: {id: 0, type: u8}, b: {id: 1, type: i16}, c: {id: 2, type: i32}, d: {id: 3, type: boolean} }",
        {"a": 200, "b": -300, "c": -70000, "d": True},
    ),
    "array": (
        ("c",), ("FIXLEN", "SEQUENCE", "FP64", "INT64"),
        "{ a: {id: 0, type: i32}, arr: {id: 1, type: array, items: {type: u8, count: 4}} }",
        {"a": -5, "arr": [1, 2, 3, 4]},
    ),
    "fixlen": (
        ("c",), ("ARRAY", "SEQUENCE", "INT64"),
        "{ a: {id: 0, type: i32}, s: {id: 1, type: string, maxlen: 16}, b: {id: 2, type: blob, maxlen: 8},"
        " f: {id: 3, type: fp32}, g: {id: 4, type: fp64} }",
        {"a": 7, "s": "hello", "b": [1, 2, 3], "f": 2.5, "g": -1.25},
    ),
    "sequence": (
        ("c",), ("ARRAY", "FP64", "INT64"),
        "{ a: {id: 0, type: i32}, st: {id: 1, type: struct, fields: { x: {id: 0, type: i32} }},"
        " sa: {id: 2, type: array, items: {type: string, count: 3, maxlen: 8}} }",
        {"a": 9, "st": {"x": 4}, "sa": ["p", "q", "r"]},
    ),
    "nofp64": (
        ("c", "cpp"), ("FP64",),
        "{ a: {id: 0, type: u64}, f: {id: 1, type: fp32}, s: {id: 2, type: string, maxlen: 16},"
        " arr: {id: 3, type: array, items: {type: u8, count: 4}} }",
        {"a": 5000000000, "f": 1.5, "s": "hi", "arr": [9, 8, 7, 6]},
    ),
    "noint64": (
        ("c", "cpp"), ("INT64",),
        "{ a: {id: 0, type: u32}, b: {id: 1, type: i32}, f: {id: 2, type: fp32}, s: {id: 3, type: string, maxlen: 16},"
        " st: {id: 4, type: struct, fields: {x: {id: 0, type: i32}}} }",
        {"a": 4000000000, "b": -5, "f": 0.5, "s": "yo", "st": {"x": 3}},
    ),
    # The C++ wrapper requires FIXLEN and SEQUENCE (it #errors without them), so
    # these two are the only cpp rows that drop ARRAY.
    "noarray": (
        ("cpp",), ("ARRAY",),
        "{ a: {id: 0, type: i32}, s: {id: 1, type: string, maxlen: 16},"
        " st: {id: 2, type: struct, fields: {x: {id: 0, type: i32}}},"
        " sa: {id: 3, type: array, items: {type: string, count: 3, maxlen: 16}} }",
        {"a": 3, "s": "hey", "st": {"x": 8}, "sa": ["u", "v", "w"]},
    ),
    "stripped": (
        ("cpp",), ("ARRAY", "FP64", "INT64"),
        "{ a: {id: 0, type: u8}, b: {id: 1, type: i16}, c: {id: 2, type: i32}, s: {id: 3, type: string, maxlen: 16},"
        " bl: {id: 4, type: blob, maxlen: 8}, st: {id: 5, type: struct, fields: {x: {id: 0, type: i32}}},"
        " sa: {id: 6, type: array, items: {type: string, count: 3, maxlen: 16}} }",
        {"a": 200, "b": -300, "c": -70000, "s": "s", "bl": [1, 2], "st": {"x": 5}, "sa": ["u", "v", "w"]},
    ),
}


def varint(v: int) -> bytes:
    out = bytearray()
    while v >= 0x80:
        out.append((v & 0x7F) | 0x80)
        v >>= 7
    out.append(v)
    return bytes(out)


def hdr(wire: int) -> bytes:
    return varint((UNKNOWN_ID << 3) | wire)


# probe -> (features it needs, wire bytes). Wire types: 0 unsigned, 2 fixlen,
# 3 unsigned array, 6 sequence start, 7 sequence end (§4).
PROBES = {
    "fixlen": (("FIXLEN",), hdr(2) + varint((1 << 3) | 2) + b"x"),
    "fp64": (("FIXLEN", "FP64"), hdr(2) + varint((8 << 3) | 1) + struct.pack("<d", 2.5)),
    "array": (("ARRAY",), hdr(3) + varint(2) + b"\x01\x02"),
    "sequence": (("SEQUENCE",), hdr(6) + hdr(7)),
    "int64": (("INT64",), hdr(0) + b"\xff" * 9 + b"\x01"),
}


def flags_of(disabled) -> str:
    return " ".join(f"-DSOFAB_DISABLE_{f}_SUPPORT" for f in disabled)


def run(argv, data: bytes):
    p = subprocess.run(argv, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
    return p.returncode, p.stdout.decode(errors="replace"), p.stderr.decode(errors="replace")


def status(harness, data: bytes) -> str:
    rc, out, err = run(harness + ["status", MSG], data)
    if rc != 0 or not out.strip():
        raise RuntimeError(f"`status` failed rc={rc}: {out!r} {err!r}")
    return out.split()[0]


def stream_verdict(harness, data: bytes):
    """(ok, stderr) of the streaming surface: one byte per feed in this family."""
    rc, _, err = run(harness + ["streamdecode", MSG], data)
    return rc == 0, err


def check(label: str, name: str, full, stripped, pattern: str) -> int:
    langs, disabled, _, sample = ROWS[name]
    fails = []

    # 1. the enabled features: same bytes out, same value back.
    text = json.dumps(sample)
    p = subprocess.run(stripped + ["encode", MSG], input=text.encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    wire_s = p.stdout
    q = subprocess.run(full + ["encode", MSG], input=text.encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    wire_f = q.stdout
    if p.returncode != 0 or not wire_s:
        fails.append(f"stripped encode failed rc={p.returncode}: {p.stderr.decode()!r}")
    elif wire_s != wire_f:
        fails.append(f"stripped encode wrote {wire_s.hex()}, the full build writes {wire_f.hex()}")
    else:
        for who, h in (("stripped", stripped), ("full", full)):
            rc, out, err = run(h + ["decode", MSG], wire_f)
            if rc != 0:
                fails.append(f"{who} decode of the full build's bytes failed rc={rc}: {err!r}")
                continue
            got = json.loads(out)
            d = list(json_equal.diff(sample, got)) + list(json_equal.diff(got, sample))
            if d:
                fails.append(f"{who} round trip differs: {d[:3]}")
    print(f"    [{label}:{name}] round trip: {len(wire_s or b'')} bytes, stripped == full"
          if not fails else f"    [{label}:{name}] round trip FAILED")

    # 2. the disabled features: refused; the enabled ones: skipped.
    refused = skipped = 0
    for probe, (needs, wire) in PROBES.items():
        gone = [f for f in needs if f in disabled]
        want = "INVALID" if gone else "COMPLETE"
        got = status(stripped, wire)
        sgot_ok, serr = stream_verdict(stripped, wire)
        if got != want:
            fails.append(f"probe {probe}: stripped build answered {got}, want {want}")
        if (not gone) != sgot_ok:
            fails.append(f"probe {probe}: streaming decode {'accepted' if sgot_ok else 'refused'}, want "
                         f"{'refusal' if gone else 'acceptance'}")
        elif gone and pattern and pattern not in serr:
            fails.append(f"probe {probe}: streaming refusal is not {pattern!r}: {serr!r}")
        control = status(full, wire)
        if control != "COMPLETE":
            fails.append(f"probe {probe}: the full build answered {control}, want COMPLETE (skipped)")
        if gone:
            refused += 1
            print(f"    [{label}:{name}] {probe:<8} -> {got} (needs {'+'.join(gone)} disabled); full build {control}")
        else:
            skipped += 1
            print(f"    [{label}:{name}] {probe:<8} -> {got} (enabled, skipped)")
    if not refused and not skipped:
        fails.append("no probe ran")
    for f in fails:
        print(f"FAIL: [{label}:{name}] {f}")
    return 1 if fails else 0


def main() -> int:
    argv = sys.argv[1:]
    if argv[:1] == ["--list"]:
        print(" ".join(n for n, r in ROWS.items() if argv[1] in r[0]))
        return 0
    if argv[:1] == ["--schema"]:
        print(f"version: 1\nmessages:\n  {MSG}: {{ payload: {ROWS[argv[1]][2]} }}")
        return 0
    if argv[:1] == ["--flags"]:
        print(flags_of(ROWS[argv[1]][1]))
        return 0
    ap = argparse.ArgumentParser()
    ap.add_argument("label")
    ap.add_argument("name", choices=sorted(ROWS))
    ap.add_argument("--status-pattern", default="INVALID")
    ap.add_argument("--full", nargs="+", required=True)
    if "--" not in argv:
        ap.error("the stripped harness argv must follow --")
    i = argv.index("--")
    args = ap.parse_args(argv[:i])
    return check(args.label, args.name, args.full, argv[i + 1:], args.status_pattern)


if __name__ == "__main__":
    sys.exit(main())
