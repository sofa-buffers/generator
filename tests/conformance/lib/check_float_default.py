#!/usr/bin/env python3
"""A float is compared with its default by BIT PATTERN: -0.0 is not the default 0.

Usage:
  check_float_default.py --emit-schema
  check_float_default.py <label> --backend KEY [--cwd DIR] [--message NAME]
                         -- <harness argv...>

## The rule (generator#636)

CORELIB_PLAN §4.6: floats round-trip bit-for-bit, so `-0.0` survives. A field is
omitted when it equals its default, and "equals" for a float is the bit pattern:
`-0.0` (`00 00 00 80`) is a value next to a default of `+0.0` and is WRITTEN,
`+0.0` at a zero default is omitted, and a non-zero default is omitted only for
its exact bits. C already does this (corelib-c-cpp `object.c`); an IEEE `!=` in a
generated encoder drops the field (`-0.0 == 0.0`) and the decoder returns `+0.0`.

## What is asserted

Every case is a WIRE BYTE LITERAL written below from MESSAGE_SPEC §4, never a
decoded JSON value: JavaScript prints `-0` as `0`, so a JSON comparison cannot see
the sign. `{"f":-0.0,"d":-0.0}` is the 16 bytes
`02 20 00 00 00 80 0a 41 00 00 00 00 00 00 00 80` on every backend.

The schema is printed by `--emit-schema`: fp32 `f` and fp64 `d` at default 0,
fp32 `g` and fp64 `h` at default 1.5, and `a`, an fp32 array whose default is
`[0.0, 1.5]`.

## KNOWN_GAP

The cells that fail TODAY, per backend (`--backend KEY`), so the driver can land
before the backend fixes. A listed case that fails is reported as a known gap and
the run stays green; a listed case that PASSES fails the run, so the entry has to
be removed together with the fix. Each backend fix removes its entry; the list is
empty when the last one lands. The case count and the gap list are printed.
"""
import json
import struct
import subprocess
import sys

MSG = "fdef"

# Cases that fail today, per backend: the generated backends compare a float with
# an IEEE `!=` (Java and Kotlin compare an array's elements by bits already, so
# their array case passes). C compares bytes and has no entry.
KNOWN_GAP = {
    "c": (),
    "cpp": ("a[0] -0.0",),
    "c-cpp": ("a[0] -0.0",),
    "rust": ("a[0] -0.0",),
    "rs-no-std": ("a[0] -0.0",),
    "go": ("a[0] -0.0",),
    "java": (),
    "kotlin": (),
    "csharp": (),
    "typescript": ("a[0] -0.0",),
    "python": ("a[0] -0.0",),
    "zig": ("a[0] -0.0",),
    "dart": ("a[0] -0.0",),
}


def hdr(fid, wtype):
    """One-byte field header (id < 16): id << 3 | wire type."""
    return bytes([fid << 3 | wtype])


def f32(v):
    return struct.pack("<f", v)


def f64(v):
    return struct.pack("<d", v)


def f32_wire(fid, v):
    return hdr(fid, 2) + b"\x20" + f32(v)


def f64_wire(fid, v):
    return hdr(fid, 2) + b"\x41" + f64(v)


def f32_array_wire(fid, vs):
    """Wire type 5, element count, fixlen word (4 bytes, fp), raw elements."""
    return hdr(fid, 5) + bytes([len(vs)]) + b"\x20" + b"".join(f32(v) for v in vs)


# (name, JSON sent, exact wire expected). Ids: f 0, d 1, g 2, h 3, a 4.
CASES = [
    ("fresh message", {}, b""),
    ("f,d +0.0", {"f": 0.0, "d": 0.0}, b""),
    ("f -0.0", {"f": -0.0}, f32_wire(0, -0.0)),
    ("d -0.0", {"d": -0.0}, f64_wire(1, -0.0)),
    ("f,d -0.0", {"f": -0.0, "d": -0.0}, f32_wire(0, -0.0) + f64_wire(1, -0.0)),
    ("g,h at default 1.5", {"g": 1.5, "h": 1.5}, b""),
    ("g,h +0.0 next to 1.5", {"g": 0.0, "h": 0.0}, f32_wire(2, 0.0) + f64_wire(3, 0.0)),
    ("g,h -0.0 next to 1.5", {"g": -0.0, "h": -0.0}, f32_wire(2, -0.0) + f64_wire(3, -0.0)),
    ("a at default [0.0, 1.5]", {"a": [0.0, 1.5]}, b""),
    ("a[0] -0.0", {"a": [-0.0, 1.5]}, f32_array_wire(4, [-0.0, 1.5])),
    ("a[1] 2.5", {"a": [0.0, 2.5]}, f32_array_wire(4, [0.0, 2.5])),
]
_NAMES = {c[0] for c in CASES}
for _b, _gap in KNOWN_GAP.items():
    for _n in _gap:
        assert _n in _NAMES, f"KNOWN_GAP[{_b!r}] names an unknown case {_n!r}"


def emit_schema() -> int:
    print("# Float defaults compared by bit pattern (generator#636), printed by")
    print("# tests/conformance/lib/check_float_default.py so the schema and the")
    print("# wire bytes the driver expects have one definition between them.")
    print(f"  {MSG}:")
    print("    payload:")
    print("      f: { id: 0, type: fp32, default: 0 }")
    print("      d: { id: 1, type: fp64, default: 0 }")
    print("      g: { id: 2, type: fp32, default: 1.5 }")
    print("      h: { id: 3, type: fp64, default: 1.5 }")
    print("      a:")
    print("        id: 4")
    print("        type: array")
    print("        items: { type: fp32, count: 3 }")
    print("        default: [0.0, 1.5]")
    return 0


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()
    if "--" not in argv:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    if not head or not cmd:
        print("FAIL: need a label and a harness argv after `--`", file=sys.stderr)
        return 2
    label = head[0]
    cwd = None
    msg = MSG
    backend = None
    i = 1
    while i < len(head):
        if head[i] == "--cwd" and i + 1 < len(head):
            cwd = head[i + 1]
        elif head[i] == "--message" and i + 1 < len(head):
            msg = head[i + 1]
        elif head[i] == "--backend" and i + 1 < len(head):
            backend = head[i + 1]
        else:
            print(f"FAIL: unknown option {head[i]!r}", file=sys.stderr)
            return 2
        i += 2
    if backend not in KNOWN_GAP:
        print(f"FAIL: --backend must be one of {sorted(KNOWN_GAP)}, got {backend!r}",
              file=sys.stderr)
        return 2
    gap = set(KNOWN_GAP[backend])

    failures, open_gaps, stale = [], [], []
    for name, sent, want in CASES:
        p = subprocess.run(cmd + ["encode", msg], input=json.dumps(sent).encode(),
                           cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if p.returncode != 0:
            problem = f"{name}: encode failed: {p.stderr.decode(errors='replace').strip()[:600]}"
        elif p.stdout != want:
            problem = (f"{name}: sent {json.dumps(sent)}, expected {want.hex(' ') or '(no bytes)'},"
                       f" got {p.stdout.hex(' ') or '(no bytes)'}")
        else:
            problem = None
        if name in gap:
            if problem is None:
                stale.append(name)
            else:
                open_gaps.append(name)
        elif problem is not None:
            failures.append(problem)
    for name in stale:
        failures.append(f"{name}: passes now but is listed in KNOWN_GAP[{backend!r}] --"
                        " remove the entry")

    if failures:
        print(f"FAIL: [{label}] float default by bit pattern (generator#636):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1
    print(f"==> [{label}] float default by bit pattern: {len(CASES)} case(s),"
          f" {len(CASES) - len(open_gaps)} pass, KNOWN_GAP[{backend}] = {sorted(open_gaps)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
