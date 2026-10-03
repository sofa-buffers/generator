#!/usr/bin/env python3
"""An explicit empty value is not an absent one (MESSAGE_SPEC §2), for every kind.

Usage:
  check_empty_vs_absent.py --emit-schema [--skip-kinds LIST] [--bounded-only]
  check_empty_vs_absent.py <label> [--cwd DIR] [--verb VERB] [--message NAME]
                           [--skip-kinds LIST] [--bounded-only]
                           -- <harness argv...>
  (--emit-schema takes --skip-kinds and --bounded-only too, and they must match
  the run's.)

## The gap this closes

MESSAGE_SPEC §2: a field is written iff its value differs from its default, and
an ABSENT field reads as the declared default. For a field whose default is
non-empty that makes "empty" and "absent" two different messages:

  * `[]` for an array whose default is `[1, 2]` differs from the default, so it
    MUST be written (as a count-0 array) and MUST decode back as `[]`;
  * `""` for a string and an empty blob whose default is non-empty are values
    too, and are written as a zero-length value;
  * `{}` writes nothing and reads back as the defaults.

A backend that omits an empty array because "empty looks like nothing", or a
decoder that refills the default after a count-0 array, passes every other suite:
their schemas declare no array or blob default and none of them sends `[]` for a
field that has one. The only runtime coverage of an explicit empty value was
union held options (`check_union.py`), where the explicit form is canonical for a
different reason -- it selects the option.

generator#139 is the regression on the other side of the same boundary: Go wrote
an explicit empty array where the field EQUALLED its default and had to omit it.
Both directions are asserted here, at the byte level.

## What is asserted -- nothing here is taken from the backend under test

For each field, one message at a time (every other field untouched):

  1. `{field: empty}` encodes to EXACTLY the bytes this file computes from the
     wire format: the field header, a zero count (or length), and for a float
     array the fixlen word that an empty float array still carries. A struct is
     framed because its child differs from the child's default.
  2. Those bytes decode to a message whose `field` is empty and whose every other
     field is its declared default -- the defaults THIS file's schema declares,
     printed below beside the bytes.
  3. That decoded message re-encodes to the same bytes.

and, once for the whole message:

  4. `{}` encodes to ZERO bytes (absent -> default, generator#139) and the empty
     byte string decodes to every declared default.

## The fields

One array per native element kind that can carry a default,

    u8 u16 u32 u64  i8 i16 i32 i64  fp32 fp64  boolean  enum  bitfield  bitfield64

each with a non-empty default, plus a string and a blob with non-empty defaults
and a struct whose child is such an array. An array of string, blob, struct, union
or array takes no default at all (validator), so "empty != absent" is not
observable for them on a plain field and they are not here.

Every field is declared twice: bounded (`count:` / `maxlen:`) and, unless
`--bounded-only`, with its bound left open -- the unbounded form is a different
container in most backends. `--bounded-only` is for a profile with no heap to hold
one (C, C++ over the C corelib, Rust no_std without `allow_dynamic`).

The way a harness spells an empty value (`[]`, `null`, `""`, a base64 string for a
byte array) and a 64-bit default (number or quoted) is rendering, not a wire fact;
`harness_dialect.py` carries those spellings for every driver.

## Loud, never quiet

Every case must run; a harness failure is a failure; the case count is printed and
a declined kind is printed with it.
"""

import argparse
import json
import subprocess
import sys

import harness_dialect as hd

MESSAGE = "emptyabs"

# Element kind -> (items declaration, non-empty default as JSON, wire kind).
# Wire kind: "u" unsigned varint array, "s" signed varint array, "f32"/"f64" fixlen.
KINDS = {
    "u8":    ("{ type: u8 }",   [1, 2], "u"),
    "u16":   ("{ type: u16 }",  [300], "u"),
    "u32":   ("{ type: u32 }",  [1, 2], "u"),
    "u64":   ("{ type: u64 }",  [4294967296], "u"),
    "i8":    ("{ type: i8 }",   [-1, 2], "s"),
    "i16":   ("{ type: i16 }",  [-1], "s"),
    "i32":   ("{ type: i32 }",  [-70000], "s"),
    "i64":   ("{ type: i64 }",  [-4294967296], "s"),
    "fp32":  ("{ type: fp32 }", [1.5], "f32"),
    "fp64":  ("{ type: fp64 }", [2.5], "f64"),
    "boolean": ("{ type: boolean }", [True], "u"),
    "enum":  ("{ type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }", [10], "s"),
    "bitfield": ("{ type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }",
                 [8], "u"),
    "bitfield64": ("{ type: bitfield, bits: { A: { pos: 0 }, B: { pos: 40 }, D: { pos: 63 } } }",
                   [1099511627776], "u"),
}

# Kinds a backend may carry one byte per element, and so print as base64 (Go's
# `[]byte`): u8, and the gapped enum and bitfield whose carriers are uint8.
BYTE_KINDS = ("u8", "enum", "bitfield")

CAP = 4
MAXLEN = 8
STR_DEFAULT = "hi"
BLOB_DEFAULT = b"Hi"          # "SGk="
BLOB_DEFAULT_B64 = "SGk="
STRUCT_CHILD = "u8"           # the struct's child array: a u8 array default [9]
STRUCT_CHILD_DEFAULT = [9]

# Wire types (MESSAGE_SPEC §3): the low three bits of a field header.
WT_VARINT_ARRAY_U = 3
WT_VARINT_ARRAY_S = 4
WT_FIXLEN_ARRAY = 5
WT_LENGTH = 2
WT_SEQ_BEGIN = 6
SEQ_END = 0x07
# Length-delimited subtype in the length byte of a zero-length value.
LEN_STRING = 0x02
LEN_BLOB = 0x03
# The fixlen word an (even empty) float array carries: element width and type.
FIXLEN_FP32 = 0x20
FIXLEN_FP64 = 0x41


def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def header(fid: int, wt: int) -> bytes:
    return varint((fid << 3) | wt)


def empty_array_bytes(fid: int, wire: str) -> bytes:
    if wire == "u":
        return header(fid, WT_VARINT_ARRAY_U) + b"\x00"
    if wire == "s":
        return header(fid, WT_VARINT_ARRAY_S) + b"\x00"
    word = FIXLEN_FP32 if wire == "f32" else FIXLEN_FP64
    return header(fid, WT_FIXLEN_ARRAY) + b"\x00" + bytes([word])


def fields(kinds, bounded_only=False):
    """The message's fields in id order, each with its schema text and its facts."""
    out = []
    fid = 0

    def add(name, yaml, default, empty, wire_bytes, byte_kind=False):
        nonlocal fid
        out.append(dict(name=name, id=fid, yaml=yaml, default=default, empty=empty,
                        wire=wire_bytes(fid), byte_kind=byte_kind))
        fid += 1

    variants = ((True, ""),) if bounded_only else ((True, ""), (False, "d_"))
    for bounded, pre in variants:
        for k in kinds:
            items, default, wire = KINDS[k]
            if bounded:
                items = items[:-1].rstrip() + f", count: {CAP} }}"
            add(pre + "a_" + k,
                f"type: array, items: {items}, default: {json.dumps(default)}",
                default, [], lambda i, w=wire: empty_array_bytes(i, w), k in BYTE_KINDS)
        cap = f", maxlen: {MAXLEN}" if bounded else ""
        add(pre + "s", f'type: string{cap}, default: "{STR_DEFAULT}"',
            STR_DEFAULT, "", lambda i: header(i, WT_LENGTH) + bytes([LEN_STRING]))
        add(pre + "bl", f'type: blob{cap}, default: "{BLOB_DEFAULT_B64}"',
            BLOB_DEFAULT, b"", lambda i: header(i, WT_LENGTH) + bytes([LEN_BLOB]), True)
        child = KINDS[STRUCT_CHILD][0]
        if bounded:
            child = child[:-1].rstrip() + f", count: {CAP} }}"
        add(pre + "st",
            "type: struct, fields: { v: { id: 0, type: array, items: " + child
            + f", default: {json.dumps(STRUCT_CHILD_DEFAULT)} }} }}",
            {"v": STRUCT_CHILD_DEFAULT}, {"v": []},
            lambda i: header(i, WT_SEQ_BEGIN) + empty_array_bytes(0, "u") + bytes([SEQ_END]))
    return out


def emit_schema(kinds, bounded_only=False) -> int:
    print("# Explicit empty vs absent (MESSAGE_SPEC §2), printed by")
    print("# tests/conformance/lib/check_empty_vs_absent.py so the defaults the schema")
    print("# declares and the bytes the driver expects have one definition.")
    print("#")
    print("# One field per native element kind with a NON-EMPTY default, a string and a")
    print("# blob with non-empty defaults, and a struct whose child is such an array.")
    print("# Each is declared bounded and, unless --bounded-only, left open.")
    print(f"  {MESSAGE}:")
    print("    payload:")
    for f in fields(kinds, bounded_only):
        print(f"      {f['name']}: {{ id: {f['id']}, {f['yaml']} }}")
    return 0


def is_empty(v) -> bool:
    return v is None or v == [] or v == "" or v == {}


def same(exp, act, byte_kind=False) -> bool:
    """Compare by value across the harness's JSON spelling (harness_dialect.py)."""
    if isinstance(exp, dict):
        return (isinstance(act, dict)
                and all(same(e, act.get(k), True if k == "v" else False)
                        for k, e in exp.items()))
    if isinstance(exp, bytes):
        return hd.bytes_of(act) == exp
    if isinstance(exp, str):
        return (act or "") == exp
    if isinstance(exp, list):
        if not exp:
            return is_empty(act)
        if byte_kind and isinstance(act, str):
            got = hd.bytes_of(act)
            return got is not None and got == bytes(hd.as_int(x) for x in exp)
        return (isinstance(act, list) and len(exp) == len(act)
                and all(same(e, a) for e, a in zip(exp, act)))
    if isinstance(exp, bool) or isinstance(act, bool):
        return bool(exp) == bool(act)
    for norm in (hd.as_int, hd.as_float):
        e, a = norm(exp), norm(act)
        if e is not None and a is not None:
            return e == a
    return exp == act


def render(v) -> str:
    return json.dumps(v if not isinstance(v, bytes) else list(v))[:120]


def to_json(v):
    """A default as the JSON an encode input carries (bytes as a number array)."""
    return list(v) if isinstance(v, bytes) else v


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
    ap.add_argument("--bounded-only", action="store_true")
    # The harness argv is split off BY HAND at `--`, as every driver here does.
    argv = sys.argv[1:]
    if "--" in argv:
        sep = argv.index("--")
        args = ap.parse_args(argv[:sep])
        harness = argv[sep + 1:]
    else:
        args = ap.parse_args(argv)
        harness = []

    skipped = [k.strip() for k in args.skip_kinds.split(",") if k.strip()]
    for k in skipped:
        if k not in KINDS:
            print(f"FAIL: --skip-kinds names {k!r}, which is not an element kind"
                  f" ({', '.join(KINDS)})", file=sys.stderr)
            return 2
    kinds = [k for k in KINDS if k not in skipped]

    if args.emit_schema:
        return emit_schema(kinds, args.bounded_only)
    if not args.label or not harness:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2

    flds = fields(kinds, args.bounded_only)
    failures = []
    cases = 0

    def encode(obj, what):
        p = run(harness + ["encode", args.message], json.dumps(obj).encode(), args.cwd)
        if p.returncode != 0:
            failures.append(f"{what}: encode failed: {p.stderr.decode(errors='replace')[:400]}")
            return None
        return p.stdout

    def decode(wire, what):
        p = run(harness + [args.verb, args.message], wire, args.cwd)
        if p.returncode != 0:
            failures.append(f"{what}: {args.verb} failed ({len(wire)} bytes): "
                            f"{p.stderr.decode(errors='replace')[:400]}")
            return None
        try:
            return json.loads(p.stdout.decode())
        except ValueError:
            failures.append(f"{what}: {args.verb} printed no JSON: {p.stdout[:200]!r}")
            return None

    def check_defaults(got, what, except_name=None):
        for f in flds:
            if f["name"] == except_name:
                continue
            if f["name"] not in got:
                failures.append(f"{what}: {f['name']} missing from the decoded message")
            elif not same(f["default"], got[f["name"]], f["byte_kind"]):
                failures.append(f"{what}: {f['name']} decoded as {render(got[f['name']])},"
                                f" expected its default {render(f['default'])}")

    # 4. absent -> default, the generator#139 direction
    cases += 1
    wire = encode({}, "{}")
    if wire is not None and wire != b"":
        failures.append(f"{{}} encoded to {len(wire)} byte(s) {wire.hex(' ')}, expected 0:"
                        " a message holding only its defaults writes nothing (§2)")
    got = decode(b"", "empty input")
    if got is not None:
        check_defaults(got, "empty input")

    for f in flds:
        name = f["name"]
        what = f"explicit empty {name}"
        cases += 1
        want = f["wire"]
        wire = encode({name: to_json(f["empty"])}, what)
        if wire is None:
            continue
        if wire != want:
            failures.append(f"{what} written as {len(wire)} byte(s) {wire.hex(' ') or '(none)'},"
                            f" expected {want.hex(' ')}")
            continue
        got = decode(wire, what)
        if got is None:
            continue
        have = got.get(name)
        if not same(f["empty"], have, f["byte_kind"]):
            failures.append(f"{name} decoded as {render(have)}, expected {render(f['empty'])}"
                            f" (not the default {render(f['default'])})")
        check_defaults(got, what, name)
        again = encode(got, what + " (re-encode)")
        if again is not None and again != want:
            failures.append(f"{what}: decoded then re-encoded as {again.hex(' ') or '(none)'},"
                            f" expected {want.hex(' ')}")

    note = f"; skipped {', '.join(skipped)}" if skipped else ""
    if args.bounded_only:
        note += "; bounded only (unbounded d_* fields omitted)"
    if failures:
        print(f"FAIL: [{args.label}] empty vs absent (MESSAGE_SPEC §2) via `{args.verb}`:",
              file=sys.stderr)
        for m in failures:
            print(f"  - {m}", file=sys.stderr)
        return 1
    print(f"==> [{args.label}] empty vs absent via `{args.verb}`: {cases} case(s),"
          f" {len(flds)} field(s) over {len(kinds)} element kind(s) -- explicit empty"
          f" written and read back, absent reads as default{note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
