#!/usr/bin/env python3
"""A native array round-trips at every length, for every element kind.

Usage:
  check_array_lengths.py --emit-schema
  check_array_lengths.py <label> [--cwd DIR] [--verb VERB] [--message NAME]
                         [--skip-kinds LIST] [--max-dyn N]
                         -- <harness argv...>

## The gap this closes

`examples/messages/example.yaml` -- the schema every conformance suite builds --
declares its native arrays at `count: 2`..`count: 8`, and `tests/bench`'s
`vehicle_telemetry` at 4 and 8. Nothing in the repo declared a native array of
capacity 16 or more, and nothing declared an unbounded one carrying real
elements. generator#550 is what that cost: the TypeScript backend emitted a
decode path reachable only from `count >= 16`, it did not compile, and eleven
green suites could not see it because not one of them ever generated the arm.

A length is not a cosmetic parameter of an array. It decides which arm of a
codec runs -- a drain loop against a resumable tail, an inline buffer against a
heap one, a bulk hand-off against a per-element callback -- and those arms are
where the family's array defects have actually lived. So the lengths here are
chosen to straddle the boundaries codecs put thresholds on: empty, one, the
handful a fixed-capacity profile inlines, and lengths either side of 16.

## What is asserted

One message per ROUND, with **every** array field carried at once, encoded from
JSON and decoded back. The comparison is the input against the output, as data:

  * an element LOST or GAINED is a length mismatch at that field;
  * an element CHANGED is a value mismatch at that index;
  * a field whose arrays share a scratch buffer with the field before it shows
    up here and nowhere else -- which is the point of filling every kind in one
    message rather than one kind per message. A shared fill buffer, a shared
    target object or a shared count register that is not reset between two
    arrays of one message is invisible to a suite that decodes them apart.

Numbers are compared BY VALUE across the string/number spelling split: a 64-bit
field is quoted on the way in (JSON has no unsigned 64-bit number, and a JS
number is a double) and several backends quote it on the way out too, while
others do not. That is rendering, not a wire fact -- `json_equal.py` makes the
same argument for member order.

## The kinds

Every native (non-wrapper) element kind the format has, because each is a
different destination in a codec that fills arrays in bulk, and a backend can
get one right and the next one wrong:

    u8 u16 u32 u64  i8 i16 i32 i64  fp32 fp64  boolean  enum  bitfield  bitfield64

`enum` and `bitfield` are declared GAPPED -- constants {0,1,2,10}, positions
{0,1,3} -- for the reason `check_closed_kinds.py` gives at length 1: a
contiguous declaration makes a closed set look like an interval. Here the gap
also has to survive a BULK fill, where a codec that carries an interval per
destination is at its most tempted to widen the bound to the hull.

`bitfield64` declares position 63, so its value does not fit a signed 64-bit
carrier and does not fit a double either; it is the one kind whose JSON is
quoted for a reason other than convention.

fp32 values are chosen to be exactly representable in 32 bits, so a round trip
through the narrower type is value-preserving and a mismatch is a real defect
rather than the rounding the type is entitled to.

## The declensions

`--skip-kinds` drops element kinds a target cannot express (each one prints
itself in the final line, so a declined kind is a decision on the record).
`--max-dyn N` states the receiver cap the project was generated with, so the
unbounded rounds stay under it; without it the unbounded field is exercised at
the same lengths as the bounded one.
"""

import argparse
import json
import subprocess
import sys

MESSAGE = "arrlen"

# Every native element kind, with the JSON values probed at each length. The
# values cycle, so a round of length N takes the first N of the cycle -- which
# keeps the extremes of each declared range inside even the shortest rounds.
KINDS = {
    "u8":    ("{ type: u8 }",   [0, 255, 1, 128, 7]),
    "u16":   ("{ type: u16 }",  [0, 65535, 1, 32768, 513]),
    "u32":   ("{ type: u32 }",  [0, 4294967295, 1, 2147483648, 70000]),
    "u64":   ("{ type: u64 }",  ["0", "18446744073709551615", "1", "9223372036854775808", "4294967296"]),
    "i8":    ("{ type: i8 }",   [0, -128, 127, -1, 42]),
    "i16":   ("{ type: i16 }",  [0, -32768, 32767, -1, 300]),
    "i32":   ("{ type: i32 }",  [0, -2147483648, 2147483647, -1, 70000]),
    "i64":   ("{ type: i64 }",  ["0", "-9223372036854775808", "9223372036854775807", "-1", "-4294967296"]),
    # Exactly representable in 32 bits, so the narrowing is value-preserving.
    "fp32":  ("{ type: fp32 }", [0.0, -1.5, 3.25, 0.125, -0.0625]),
    "fp64":  ("{ type: fp64 }", [0.0, -1.5, 1e300, 0.1, -2.25]),
    "boolean": ("{ type: boolean }", [False, True, True, False, True]),
    "enum":  ("{ type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }", [0, 10, 1, 2, 10]),
    "bitfield": ("{ type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }",
                 [0, 11, 1, 8, 3]),
    "bitfield64": ("{ type: bitfield, bits: { A: { pos: 0 }, B: { pos: 40 }, D: { pos: 63 } } }",
                   ["0", "9223373136366403585", "1", "9223372036854775808", "1099511627776"]),
}

# The capacity the bounded field declares, and the lengths each round carries.
#
# 15/16/17 straddle the one threshold this family has actually shipped a defect
# on; 0 is the empty array, which is a delivery event of its own for a codec
# that hands a destination over (it is the only place an array's emptiness can
# show up in a container reused across fields); 1 and 2 are the lengths a
# fixed-capacity profile keeps inline.
CAP = 64
BOUNDED_LENGTHS = (0, 1, 2, 15, 16, 17, CAP)
# The unbounded field's own rounds, run alongside. The long one is what makes an
# array cross several chunk boundaries under a drip-fed decoder.
DYN_LENGTHS = (0, 1, 3, 257)


def field(kind: str) -> str:
    return "a_" + kind


def dyn_field(kind: str) -> str:
    return "d_" + kind


def emit_schema(kinds) -> int:
    print("# The native-array length message (generator#550), printed by")
    print("# tests/conformance/lib/check_array_lengths.py so the capacity the driver")
    print("# fills to and the capacity the schema declares have one definition.")
    print("#")
    print("# Every native element kind, twice: once bounded at a capacity no other")
    print("# schema in the repo reaches, and once left open. The enum and the bitfield")
    print("# are GAPPED -- a contiguous declaration makes a closed set look like an")
    print("# interval, which is exactly what a bulk fill is tempted to widen it to.")
    print(f"  {MESSAGE}:")
    print("    payload:")
    fid = 0
    for k in kinds:
        items = KINDS[k][0]
        bounded = items[:-1].rstrip() + f", count: {CAP} }}"
        print(f"      {field(k)}: {{ id: {fid}, type: array, items: {bounded} }}")
        fid += 1
        print(f"      {dyn_field(k)}: {{ id: {fid}, type: array, items: {items} }}")
        fid += 1
    return 0


def cycle(values, n):
    return [values[i % len(values)] for i in range(n)]


def build(kinds, blen: int, dlen: int):
    """One round's input: every field filled to its round's length."""
    out = {}
    for k in kinds:
        vals = KINDS[k][1]
        out[field(k)] = cycle(vals, blen)
        out[dyn_field(k)] = cycle(vals, dlen)
    return out


def same(exp, act) -> bool:
    """Compare by value across the string/number spelling of an integer.

    A 64-bit field is quoted on the way in whatever the target does on the way
    out; JSON has no unsigned 64-bit number and a JS number is a double, so the
    quoting is the only portable spelling of the input and cannot also be an
    assertion about the output.
    """
    if isinstance(exp, list) and isinstance(act, list):
        return len(exp) == len(act) and all(same(e, a) for e, a in zip(exp, act))
    if isinstance(exp, bool) or isinstance(act, bool):
        return bool(exp) == bool(act)
    for norm in (_as_int, _as_float):
        e, a = norm(exp), norm(act)
        if e is not None and a is not None:
            return e == a
    return exp == act


def _as_int(v):
    if isinstance(v, int):
        return v
    if isinstance(v, str):
        try:
            return int(v)
        except ValueError:
            return None
    return None


def _as_float(v):
    if isinstance(v, (int, float)):
        return float(v)
    if isinstance(v, str):
        try:
            return float(v)
        except ValueError:
            return None
    return None


def die(msg: str) -> None:
    sys.stderr.write(f"check_array_lengths: {msg}\n")
    raise SystemExit(2)


def run(argv, stdin_bytes, cwd):
    return subprocess.run(argv, input=stdin_bytes, cwd=cwd,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def main() -> int:
    ap = argparse.ArgumentParser(add_help=False)
    ap.add_argument("label", nargs="?")
    ap.add_argument("--emit-schema", action="store_true")
    ap.add_argument("--cwd")
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--message", default=MESSAGE)
    ap.add_argument("--skip-kinds", default="")
    ap.add_argument("--max-dyn", type=int, default=0)
    # The harness argv is split off BY HAND at `--`, as every other driver in
    # this directory does. Leaving it to argparse's own `--` handling is what
    # broke CI: whether the separator survives into a trailing nargs="*" differs
    # between Python versions, so the same command line parsed here and failed
    # on the runner.
    argv = sys.argv[1:]
    emit_only = "--emit-schema" in argv and "--" not in argv
    if "--" in argv:
        sep = argv.index("--")
        args = ap.parse_args(argv[:sep])
        harness = argv[sep + 1:]
    else:
        args = ap.parse_args(argv)
        harness = []
    if not emit_only and not harness:
        die("no harness argv given (put it after `--`)")
    args.harness = harness

    skipped = [k.strip() for k in args.skip_kinds.split(",") if k.strip()]
    for k in skipped:
        if k not in KINDS:
            print(f"FAIL: --skip-kinds names {k!r}, which is not an element kind"
                  f" ({', '.join(KINDS)})", file=sys.stderr)
            return 2
    kinds = [k for k in KINDS if k not in skipped]

    if args.emit_schema:
        return emit_schema(kinds)
    if not args.label or not args.harness:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2

    dyn_lengths = DYN_LENGTHS
    if args.max_dyn:
        dyn_lengths = tuple(n for n in DYN_LENGTHS if n <= args.max_dyn)

    rounds = []
    for i, blen in enumerate(BOUNDED_LENGTHS):
        rounds.append((blen, dyn_lengths[i % len(dyn_lengths)]))
    # ...and one round carrying the longest of both at once, so the longest
    # arrays in the file are exercised side by side rather than only apart.
    if dyn_lengths and (CAP, max(dyn_lengths)) not in rounds:
        rounds.append((CAP, max(dyn_lengths)))

    for blen, dlen in rounds:
        want = build(kinds, blen, dlen)
        text = json.dumps(want)
        enc = run(args.harness + ["encode", args.message], text.encode(), args.cwd)
        if enc.returncode != 0:
            print(f"FAIL: [{args.label}] encode failed at bounded={blen} dynamic={dlen}:\n"
                  f"{enc.stderr.decode(errors='replace')[:800]}", file=sys.stderr)
            return 1
        dec = run(args.harness + [args.verb, args.message], enc.stdout, args.cwd)
        if dec.returncode != 0:
            print(f"FAIL: [{args.label}] {args.verb} failed at bounded={blen} dynamic={dlen}"
                  f" ({len(enc.stdout)} bytes):\n"
                  f"{dec.stderr.decode(errors='replace')[:800]}", file=sys.stderr)
            return 1
        try:
            got = json.loads(dec.stdout.decode())
        except ValueError:
            print(f"FAIL: [{args.label}] {args.verb} printed no JSON at bounded={blen}:\n"
                  f"{dec.stdout.decode(errors='replace')[:400]}", file=sys.stderr)
            return 1
        for name, exp in want.items():
            act = got.get(name)
            if act is None and not exp:
                act = []
            if act is None:
                print(f"FAIL: [{args.label}] bounded={blen} dynamic={dlen}:"
                      f" field {name!r} missing from the decoded message", file=sys.stderr)
                return 1
            if not same(exp, act):
                print(f"FAIL: [{args.label}] bounded={blen} dynamic={dlen}: {name}"
                      f" round-tripped as {len(act) if isinstance(act, list) else act}"
                      f" element(s), expected {len(exp)}", file=sys.stderr)
                print(f"  in : {json.dumps(exp)[:300]}", file=sys.stderr)
                print(f"  out: {json.dumps(act)[:300]}", file=sys.stderr)
                return 1

    note = f"; skipped {', '.join(skipped)}" if skipped else ""
    print(f"==> [{args.label}] native arrays round-trip via `{args.verb}`:"
          f" {len(rounds)} length round(s) x {len(kinds)} element kind(s),"
          f" bounded {BOUNDED_LENGTHS} / unbounded {dyn_lengths}{note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
