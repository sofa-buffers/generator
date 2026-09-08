#!/usr/bin/env python3
"""An `enum` and a `bitfield` are CLOSED: only what the schema declares is valid (generator#516).

Usage:
  check_closed_kinds.py --emit-schema
  check_closed_kinds.py <label> [--message NAME] [--cwd DIR] [--verb VERB]
                        [--status-verb VERB] [--status-invalid NAME]
                        [--status-complete NAME] [--no-values]
                        [--skip-positions LIST] [--hull-only LIST]
                        -- <harness argv...>

MESSAGE_SPEC §1 (doc `a50db95`) closes both leaf types by what the schema
declares rather than by a width:

  * an `enum`'s bound is the **set of constants** it declares. A wire value that
    is not one of them is malformed input and MUST be reported INVALID (§7.1).
    The signed 32-bit range of the wire type is that type's ceiling, not the
    field's bound.
  * a `bitfield`'s bound is the **mask of the positions** it declares: `v` is
    valid exactly when `v & ~mask == 0`. Every combination of declared flags is
    valid, the zero value included, and any other bit is INVALID.

## Why the definitions here are GAPPED

The enum declares {0, 1, 2, 10} and the bitfield positions 0, 1 and 3
(mask 0b1011). Both gaps are the entire point. A contiguous definition makes a
closed set look like an interval, so a decoder that bounds at min..max, or at the
width of the integer it stores the field in, passes every row a contiguous
fixture can produce. With the gaps, `5` is inside the enum's hull and not a
constant, and `4` fits the bitfield's byte and sets a bit no flag declares --
neither is expressible as an interval, and both MUST be refused.

## Storage is never the bound

§1 lets a receiver hold the field in the smallest integer covering the declared
constants/positions, and the footprint profiles do. That is a MAY about storage
and says nothing about validity: a field whose declared positions are 0..3 does
not become 0..255 valid because the target holds it in a byte. So each kind is
probed with THREE values -- a declared one (accept), an undeclared one that fits
the likely storage (reject), and one past that storage (reject) -- and it is the
middle row that separates the closed rule from a width check.

## Six positions, because a value lands in all of them

    scalar          cen  id 0    cbf  id 1
    array element   cena id 2    cbfa id 3
    struct member   cst  id 4 -> st_en id 0, st_bf id 1
    struct-array    csa  id 5 -> element seq -> sa_en id 0, sa_bf id 1
    union member    cun  id 6 -> un_en id 0, un_bf id 1
    matrix row      cmat id 7 (enum rows)   cmbf id 8 (bitfield rows)

Several backends emit ONE store arm per kind serving the four scalar-family
positions and a second pair for the two array positions, so a fix that covers
only the scalar is a third of the job and a suite that probes only the scalar
cannot see the difference. `--skip-positions` lets a suite decline a position its
harness genuinely cannot express, by name, so the omission is a decision rather
than a silence; the declined position is left out of `--emit-schema` too, since a
target that cannot express the shape cannot build a harness that declares it.
`--hull-only` is the weaker declension, for a position whose bound has to travel
through a corelib hook that carries an INTERVAL and nothing else: the accepting
rows and the beyond-the-hull reject still run, and only the gap row -- the one no
interval can express -- is dropped.

## Verdicts

Every payload is COMPLETE, so truncation can never explain a rejection. A
rejected row is asserted by exit status (and by `--status-verb`, when the
harness has one); an accepted row additionally asserts the decoded VALUE, so a
decoder that passes by refusing everything fails the accepting half and one that
passes by keeping everything fails the refusing half.

The accepted rows include the ZERO value of both kinds and, for the bitfield,
every declared combination -- §1 is explicit that all of them are valid, and a
mask check written as `v == 0 or v == one_declared_flag` would pass a
single-flag probe.
"""

import argparse
import base64
import json
import re
import subprocess
import sys

MESSAGE = "closed"

# The declared sets. Gapped on purpose; see the module docstring.
ENUM_CONSTS = {"A": 0, "B": 1, "C": 2, "Z": 10}
BIT_POS = {"A": 0, "B": 1, "D": 3}
MASK = 0b1011

# Values probed at every position.
ENUM_OK = (0, 2, 10)          # declared
ENUM_GAP = 5                  # inside the hull 0..10, not a constant
ENUM_WIDE = 1000              # past the i8 a narrow target stores it in
BIT_OK = (0, 3, 8, 11)        # every declared combination, zero included
BIT_GAP = 4                   # bit 2, undeclared, fits the u8 storage
BIT_WIDE = 256                # past that storage

# Field ids.
CEN, CBF, CENA, CBFA, CST, CSA, CUN, CMAT, CMBF = range(9)

# Wire types (MESSAGE_SPEC §4.2): a field header is (id << 3) | wire_type.
WT_UNSIGNED = 0
WT_SIGNED = 1
WT_ARRAY_UNSIGNED = 3
WT_ARRAY_SIGNED = 4
WT_SEQ_BEGIN = 6
WT_SEQ_END = 7

POSITIONS = ("scalar", "array", "struct", "structarray", "union", "matrix")


def die(msg):
    print("FAIL: " + msg)
    sys.exit(1)


def varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def zigzag(n):
    return varint((n << 1) ^ (n >> 63))


def header(fid, wire_type):
    return varint((fid << 3) | wire_type)


SEQ_END = header(0, WT_SEQ_END)


def enum_yaml():
    return "{ " + ", ".join("%s: %d" % kv for kv in ENUM_CONSTS.items()) + " }"


def bits_yaml():
    return "{ " + ", ".join("%s: { pos: %d }" % kv for kv in BIT_POS.items()) + " }"


def emit_schema(positions) -> int:
    """Print the `closed` message, for appending to a conformance schema.

    A position a suite declined with --skip-positions is not DECLARED either: a
    target that cannot express the shape at all -- corelib-cpp's row collector
    carries no element bound, and `array<array<enum>>` does not compile there --
    could not build a harness for a schema that names it.
    """
    e, b = enum_yaml(), bits_yaml()
    print("# closed -- the closed-enum / closed-bitfield message (MESSAGE_SPEC §1,")
    print("# generator#516), printed by tests/conformance/lib/check_closed_kinds.py so")
    print("# the ids, the declared constants and the declared positions the fixtures")
    print("# breach have exactly one definition between them. The sets are GAPPED: an")
    print("# interval bound passes every row a contiguous definition can produce.")
    print("  %s:" % MESSAGE)
    print("    payload:")
    if "scalar" in positions:
        print("      cen:  { id: %d, type: enum, enum: %s }" % (CEN, e))
        print("      cbf:  { id: %d, type: bitfield, bits: %s }" % (CBF, b))
    if "array" in positions:
        print("      cena: { id: %d, type: array, items: { type: enum, count: 4, enum: %s } }" % (CENA, e))
        print("      cbfa: { id: %d, type: array, items: { type: bitfield, count: 4, bits: %s } }" % (CBFA, b))
    if "struct" in positions:
        print("      cst:")
        print("        id: %d" % CST)
        print("        type: struct")
        print("        fields:")
        print("          st_en: { id: 0, type: enum, enum: %s }" % e)
        print("          st_bf: { id: 1, type: bitfield, bits: %s }" % b)
    if "structarray" in positions:
        print("      csa:")
        print("        id: %d" % CSA)
        print("        type: array")
        print("        items:")
        print("          type: struct")
        print("          count: 2")
        print("          fields:")
        print("            sa_en: { id: 0, type: enum, enum: %s }" % e)
        print("            sa_bf: { id: 1, type: bitfield, bits: %s }" % b)
    if "union" in positions:
        print("      cun:")
        print("        id: %d" % CUN)
        print("        type: union")
        print("        default_id: 0")
        print("        oneof:")
        print("          un_en: { id: 0, type: enum, enum: %s }" % e)
        print("          un_bf: { id: 1, type: bitfield, bits: %s }" % b)
    if "matrix" in positions:
        print("      cmat: { id: %d, type: array, items: { type: array, count: 2, items: { type: enum, count: 3, enum: %s } } }" % (CMAT, e))
        print("      cmbf: { id: %d, type: array, items: { type: array, count: 2, items: { type: bitfield, count: 3, bits: %s } } }" % (CMBF, b))
    return 0


# ---- wire builders, one per position -------------------------------------

def w_scalar_enum(v):
    return header(CEN, WT_SIGNED) + zigzag(v)


def w_scalar_bit(v):
    return header(CBF, WT_UNSIGNED) + varint(v)


def w_array_enum(v):
    return header(CENA, WT_ARRAY_SIGNED) + varint(1) + zigzag(v)


def w_array_bit(v):
    return header(CBFA, WT_ARRAY_UNSIGNED) + varint(1) + varint(v)


def w_struct_enum(v):
    return header(CST, WT_SEQ_BEGIN) + header(0, WT_SIGNED) + zigzag(v) + SEQ_END


def w_struct_bit(v):
    return header(CST, WT_SEQ_BEGIN) + header(1, WT_UNSIGNED) + varint(v) + SEQ_END


def w_structarray_enum(v):
    inner = header(0, WT_SEQ_BEGIN) + header(0, WT_SIGNED) + zigzag(v) + SEQ_END
    return header(CSA, WT_SEQ_BEGIN) + inner + SEQ_END


def w_structarray_bit(v):
    inner = header(0, WT_SEQ_BEGIN) + header(1, WT_UNSIGNED) + varint(v) + SEQ_END
    return header(CSA, WT_SEQ_BEGIN) + inner + SEQ_END


def w_union_enum(v):
    return header(CUN, WT_SEQ_BEGIN) + header(0, WT_SIGNED) + zigzag(v) + SEQ_END


def w_union_bit(v):
    return header(CUN, WT_SEQ_BEGIN) + header(1, WT_UNSIGNED) + varint(v) + SEQ_END


def w_matrix_enum(v):
    row = header(0, WT_ARRAY_SIGNED) + varint(1) + zigzag(v)
    return header(CMAT, WT_SEQ_BEGIN) + row + SEQ_END


def w_matrix_bit(v):
    row = header(0, WT_ARRAY_UNSIGNED) + varint(1) + varint(v)
    return header(CMBF, WT_SEQ_BEGIN) + row + SEQ_END


# (position, kind) -> (builder, leaf key the accepted value is read back under,
#                      how deeply the value is wrapped: 0 scalar, 1 list, 2 matrix)
SHAPES = {
    ("scalar", "enum"): (w_scalar_enum, "cen", 0),
    ("scalar", "bitfield"): (w_scalar_bit, "cbf", 0),
    ("array", "enum"): (w_array_enum, "cena", 1),
    ("array", "bitfield"): (w_array_bit, "cbfa", 1),
    ("struct", "enum"): (w_struct_enum, "st_en", 0),
    ("struct", "bitfield"): (w_struct_bit, "st_bf", 0),
    ("structarray", "enum"): (w_structarray_enum, "sa_en", 0),
    ("structarray", "bitfield"): (w_structarray_bit, "sa_bf", 0),
    ("union", "enum"): (w_union_enum, "un_en", 0),
    ("union", "bitfield"): (w_union_bit, "un_bf", 0),
    ("matrix", "enum"): (w_matrix_enum, "cmat", 2),
    ("matrix", "bitfield"): (w_matrix_bit, "cmbf", 2),
}

WHY = {
    "scalar": "a scalar field",
    "array": "a native array element",
    "struct": "a struct member",
    "structarray": "a member of a struct-array element",
    "union": "a union member",
    "matrix": "a matrix row element",
}


def build_table(positions, hull=()):
    """The rows to run, one per (position, kind, value).

    A position named in `hull` keeps every row except the undeclared-inside-the-
    hull one. That is not a relaxation of the rule -- it is the statement that at
    THIS position the bound travels through a corelib hook carrying an INTERVAL
    and nothing else (corelib-cpp's sofab::ElemBound, corelib-py's
    on_array_begin), so the hull is the most of the declared set that fits and
    the gap stays unenforced. The suite that passes it says so out loud, and the
    beyond-the-hull reject and every accepting row still run: a target that
    stopped bounding the position at all still fails here.
    """
    rows = []
    for pos in positions:
        for kind, oks, gap, wide, gapwhy, widewhy in (
            ("enum", ENUM_OK, ENUM_GAP, ENUM_WIDE,
             "inside the declared hull 0..10 and not one of the constants -- an "
             "interval bound keeps it, and MUST NOT",
             "past the narrow signed integer a footprint target stores the enum in; "
             "keeping it masked is the older defect, keeping it at all is this one"),
            ("bitfield", BIT_OK, BIT_GAP, BIT_WIDE,
             "bit 2, which no flag declares, inside the byte the field is stored in "
             "-- the mask is NOT every bit up to the highest declared one",
             "past that byte; a mask test on the raw carrier refuses it with the "
             "same clause, so no separate width term is needed"),
        ):
            build, leaf, depth = SHAPES[(pos, kind)]
            for v in oks:
                rows.append(("%s_%s_ok_%d" % (pos, kind, v), build(v), "accept",
                             (leaf, v, depth),
                             "%s carrying the DECLARED %s value %d" % (WHY[pos], kind, v)))
            if pos not in hull:
                rows.append(("%s_%s_undeclared" % (pos, kind), build(gap), "invalid",
                             None, "%s carrying %d: %s" % (WHY[pos], gap, gapwhy)))
            rows.append(("%s_%s_beyond_storage" % (pos, kind), build(wide), "invalid",
                         None, "%s carrying %d: %s" % (WHY[pos], wide, widewhy)))
    return rows


def run(argv, cwd, data):
    proc = subprocess.run(argv, cwd=cwd, input=data,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return (proc.returncode,
            proc.stdout.decode("utf-8", "replace"),
            proc.stderr.decode("utf-8", "replace"))


def norm(key):
    """Compare JSON keys across the family's naming conventions."""
    return re.sub(r"[^a-z0-9]", "", key.lower())


def find_leaf(node, key):
    """First value under `key`, anywhere in the decoded JSON.

    The eleven harnesses wrap a struct, a struct-array element and a union
    differently -- a nested object, a list of objects, a tagged object -- and the
    leaf names here are unique across the whole message, so searching for the
    leaf is portable where a fixed path is not.
    """
    want = norm(key)
    if isinstance(node, dict):
        for k, v in node.items():
            if norm(k) == want:
                return v
        for v in node.values():
            got = find_leaf(v, key)
            if got is not None:
                return got
    elif isinstance(node, list):
        for v in node:
            got = find_leaf(v, key)
            if got is not None:
                return got
    return None


def decoded_json(out):
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            return json.loads(line)
        except ValueError:
            continue
    return None


def as_container(got):
    """A byte container some languages render as base64, back to element values.

    Nothing to do with the closed rule: an array whose element is stored in an
    unsigned byte is a byte sequence to Go's encoding/json and to Java's Jackson,
    and both spell it base64 rather than as a list. The elements are still the
    values the decoder kept, so the row's assertion is made against them rather
    than dropped -- a harness that renders them differently must not cost the
    check its accepting half.
    """
    if not isinstance(got, str):
        return got
    try:
        return list(base64.b64decode(got, validate=True))
    except Exception:
        return got


def unwrap(got, depth, name):
    """Peel `depth` list levels off a decoded value, checking each is length 1."""
    for _ in range(depth):
        got = as_container(got)
        if not isinstance(got, list) or len(got) != 1:
            die("%s -- expected a 1-element container, got %r" % (name, got))
        got = got[0]
    return got


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("label", nargs="?")
    ap.add_argument("--emit-schema", action="store_true")
    ap.add_argument("--message", default=MESSAGE)
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--status-verb", default=None)
    ap.add_argument("--status-invalid", default="INVALID")
    ap.add_argument("--status-complete", default="COMPLETE")
    ap.add_argument("--no-values", action="store_true")
    ap.add_argument("--skip-positions", default="")
    ap.add_argument("--hull-only", default="")

    argv = sys.argv[1:]
    if "--" in argv:
        sep = argv.index("--")
        head, harness = argv[:sep], argv[sep + 1:]
    else:
        head, harness = argv, []
    args = ap.parse_args(head)

    def named(flag, raw):
        got = [x.strip() for x in raw.split(",") if x.strip()]
        unknown = [x for x in got if x not in POSITIONS]
        if unknown:
            die("%s: %s is not one of %s"
                % (flag, ", ".join(unknown), ", ".join(POSITIONS)))
        return got

    skip = named("--skip-positions", args.skip_positions)
    positions = [p for p in POSITIONS if p not in skip]

    if args.emit_schema:
        return emit_schema(positions)

    if not positions:
        die("--skip-positions declined every position; there is nothing left to check")
    hull = named("--hull-only", args.hull_only)
    both = [p for p in hull if p in skip]
    if both:
        die("--hull-only names %s, which --skip-positions already declined"
            % ", ".join(both))

    if not args.label:
        die("no label given (the suite name this run is reported under)")
    if not harness:
        die("no harness argv given (put it after `--`)")

    msg = [args.message] if args.message else []
    table = build_table(positions, hull)

    for name, wire, expect, value, why in table:
        rc, out, err = run(harness + [args.verb] + msg, args.cwd, wire)
        text = (out + err).strip()

        if expect == "invalid":
            if rc == 0:
                die("[%s] %s must be INVALID (MESSAGE_SPEC §1/§7.1) -- %s; the "
                    "decode succeeded instead:\n%s"
                    % (args.label, name, why, text))
            if args.status_verb:
                _, sout, serr = run(harness + [args.status_verb] + msg,
                                    args.cwd, wire)
                got = (sout.strip().splitlines() or [""])[0]
                if got != args.status_invalid:
                    die("[%s] %s -- must be %s, got %r%s"
                        % (args.label, name, args.status_invalid, got,
                           ("\n" + serr.strip()) if serr.strip() else ""))
            continue

        if rc != 0:
            die("[%s] %s must DECODE -- %s; rc=%d:\n%s"
                % (args.label, name, why, rc, text))
        if args.status_verb:
            _, sout, serr = run(harness + [args.status_verb] + msg, args.cwd, wire)
            got = (sout.strip().splitlines() or [""])[0]
            if got != args.status_complete:
                die("[%s] %s -- must decode %s, got %r%s"
                    % (args.label, name, args.status_complete, got,
                       ("\n" + serr.strip()) if serr.strip() else ""))
        if args.no_values:
            continue
        obj = decoded_json(out)
        if obj is None:
            die("[%s] %s -- the harness printed no JSON object; got:\n%s"
                % (args.label, name, out.strip()))
        leaf, want, depth = value
        got = find_leaf(obj, leaf)
        if got is None:
            die("[%s] %s -- the harness printed no %r; got:\n%s"
                % (args.label, name, leaf, out.strip()))
        got = unwrap(got, depth, "[%s] %s" % (args.label, name))
        if isinstance(got, str):
            try:
                got = int(got, 0)
            except ValueError:
                pass
        if isinstance(got, bool) or not isinstance(got, (int, float)) or int(got) != want:
            die("[%s] %s decoded, but %s is %r -- want %d (%s); bytes: %s"
                % (args.label, name, leaf, got, want, why, wire.hex()))

    note = ""
    if skip:
        note += "; declined: " + ", ".join(skip)
    if hull:
        note += "; hull-only (the gap stays unenforced): " + ", ".join(hull)
    print("==> [%s] closed enum/bitfield: %d rows over %d position(s) OK%s"
          % (args.label, len(table), len(positions), note))
    return 0


if __name__ == "__main__":
    sys.exit(main())
