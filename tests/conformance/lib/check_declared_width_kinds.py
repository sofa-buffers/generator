#!/usr/bin/env python3
"""An `enum` and a `bitfield` are bound by the WIDTH their declaration implies (generator#516).

Usage:
  check_declared_width_kinds.py --emit-schema
  check_declared_width_kinds.py <label> [--message NAME] [--cwd DIR] [--verb VERB]
                        [--status-verb VERB] [--status-invalid NAME]
                        [--status-complete NAME] [--invalid-pattern REGEX]
                        [--no-values] [--skip-positions LIST]
                        [--storage-masked LIST]
                        (--stream-verb VERB [--stream-sizes LIST]
                         [--stream-invalid-pattern REGEX] | --no-stream REASON)
                        -- <harness argv...>

MESSAGE_SPEC §1 (doc `382159e`, PR #95) binds both leaf types to the WIDTH their
declaration implies:

  * an `enum` is bounded by the smallest **signed** type holding every declared
    constant -- `i8`..`i64`. A value inside that width is valid even when the
    schema names no constant for it; only a value outside it is malformed input
    and MUST be reported INVALID (§7.1).
  * a `bitfield` is bounded by the smallest **unsigned** type holding its highest
    declared `pos` -- `u8`..`u64`. A value inside that width is valid whichever
    bits it carries, undeclared ones included; undeclared bits are NOT masked
    away, because masking would turn malformed-looking input into a DECLARED
    combination and report it Ok.

This REPLACED the closed-set reading MESSAGE_SPEC carried for six days (doc PR
#89, `a50db95`), under which an enum was bound by its set of constants and a
bitfield by the mask of its declared bits. Every backend in the family now emits
the width bound, so only the width tables below remain.

## Why the definitions here are GAPPED

The enum declares {0, 1, 2, 10} -- implying an i8, −128..127 -- and the bitfield
positions 0, 1 and 3, implying a u8, 0..255. Both gaps are the entire point, and
under the width rule they are what the ACCEPTING rows are made of: `5` sits
between the constants and `4` sets a bit no flag declares, and both MUST decode.
A decoder still carrying the withdrawn set/mask bound refuses exactly those two
rows and passes everything else here.

## Storage is never the bound

§1's fourth consequence: a receiver that cannot hold the field at exactly the
declared width holds it wider and MUST then enforce the width as an explicit
check, because nothing about its storage will. The bound is never satisfied by
the storage type happening to be narrow enough. So each kind is probed with
values on both sides of the implied width -- including both of its EDGES, which
is what separates a real width check from one taken from the constants' hull
(0..10 here, far narrower than the i8 the declaration implies).

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
cannot see the difference.

## The two declensions, weaker last

Each is spelled as a comma-separated list whose items are a POSITION (both kinds)
or a `position:kind` pair, so a suite declines exactly the cell it cannot reach
and no more. Every one of them prints itself in the final line: a declined cell is
a decision on the record, never a silence.

  * `--skip-positions` -- the harness cannot express the shape at all. The cell is
    left out of `--emit-schema` too, since a target that cannot build a harness
    for the shape cannot declare it either. No suite uses it at the moment: the
    C++ one did, for `matrix:enum`, until generator#531 gave an enum matrix row a
    generated collector that compiles and carries the bound.
  * `--storage-masked` -- the corelib narrows the element to the receiver's
    storage BEFORE generated code can see it, so a value past that storage has
    already become a declared one by the time anything may test it (corelib-cpp's
    sofab::MessageSeq reads a matrix row with an unbounded static_cast, and 256
    into a one-byte bitfield row is 0). Only the beyond-storage row is dropped;
    the gap row -- which is what tells a real check from a width check -- still
    runs.

## Verdicts

Every payload is COMPLETE, so truncation can never explain a rejection. A
rejected row is asserted by exit status AND by a category channel -- either
`--status-verb`, where the harness has a verdict verb, or `--invalid-pattern`,
a regex the harness's own output must match. One of the two is REQUIRED: a bare
"exit status is not zero" scores a panic, an assertion failure or a
safety-checked process abort as a correct rejection, and that is precisely the
failure mode the zig half of this rule existed to remove (generator#517: an
out-of-range `@intCast` aborting the process with rc=134 rather than answering
INVALID). An accepted row additionally asserts the decoded VALUE, so a decoder
that passes by refusing everything fails the accepting half and one that passes
by keeping everything fails the refusing half.

## Every row again, cut into chunks

The one-shot verb hands the whole message over at once, so it can only ever
exercise a bound that is reached in one pass. That is not where every bound
lives: once an array element is stored at the width it declares, the bound is
the corelib narrowing into that destination, reached from its BULK element
offer -- and a bulk fill RESUMES, with a half-arrived element sitting in the
decoder's accumulator until its last byte turns up. An accumulator that is not
carried, a narrowing taken on a partial value, a bulk cursor rewound by a
suspend: none of it is visible while the message arrives in one piece.

CORELIB_PLAN §5.2 makes the decode outcome computable at ANY byte boundary and
§5.2.3 fixes the verdict precedence, so the property is:

    the verdict AND the decoded value do not depend on where the chunks were cut

`--stream-verb` replays EVERY row of the table through the harness's streaming
surface and asserts exactly that -- the same verdict, through the same category
channel, and for an accepted row the same leaf value and the same decoded
object as the one-shot leg produced. The rejecting rows are the half this is
really for: an over-width element split across a feed boundary must still be
INVALID, and a driver that only replays well-formed fixtures cannot see it.

`--stream-sizes` sweeps several chunk widths, passed to the harness as the
argument after the message name (`0` = the whole buffer in one feed, the
degenerate split, which separates "the streaming path is wrong" from "it is
wrong WHEN IT SUSPENDS"). Give it only where the harness actually reads that
argument: a harness that ignores it drives its own fixed split -- one byte per
feed throughout this family -- and sweeping widths it never honours would
report coverage it did not have. Omit the flag there and the streaming verb is
invoked bare, once per row, on whatever split the harness itself uses.

A suite whose harness has no streaming surface at all declines with
`--no-stream REASON`, and the reason prints in the final line. One of the two
flags is REQUIRED: a suite must say which, so the leg can never go missing in
silence.

The accepted rows include the ZERO value of both kinds and, for the bitfield,
every declared combination -- §1 is explicit that all of them are valid, and a
mask check written as `v == 0 or v == one_declared_flag` would pass a
single-flag probe.
"""

import argparse
import base64
import concurrent.futures
import json
import os
import re
import subprocess
import sys

MESSAGE = "closed"

# Handed to a harness as its chunk size to find out whether it reads the argument
# at all. Deliberately unparseable as an integer in every target language.
NOT_A_SIZE = "notasize"

# The declared sets. Gapped on purpose; see the module docstring.
ENUM_CONSTS = {"A": 0, "B": 1, "C": 2, "Z": 10}
BIT_POS = {"A": 0, "B": 1, "D": 3}
MASK = 0b1011

# The widths those declarations IMPLY (MESSAGE_SPEC §1). {0,1,2,10} fits an i8
# and needs nothing wider; the highest declared `pos` is 3, so the bitfield is a
# u8. These two intervals are the whole bound under the width rule.
ENUM_WIDTH_LO, ENUM_WIDTH_HI = -128, 127
BIT_WIDTH_HI = 255

# Values probed at every position.
ENUM_GAP = 5                  # inside the width, not a constant
ENUM_LOW = -1                 # below every constant, inside the width
ENUM_WIDE = 1000              # past the i8 the declaration implies
BIT_GAP = 4                   # bit 2, undeclared, inside the u8 width
BIT_WIDE = 256                # past that width

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
KINDS = ("enum", "bitfield")

# --- the width rule (MESSAGE_SPEC §1 @ doc `382159e`, PR #95) ----------------
#
# Accepted: every declared value, AND the undeclared ones that fit the implied
# width, AND both edges of that width. The two undeclared rows are the ones that
# tell this rule from the withdrawn closed-set one -- under that rule they were
# refused.
ENUM_OK = (0, 2, 10, ENUM_GAP, ENUM_LOW, ENUM_WIDTH_HI, ENUM_WIDTH_LO)
BIT_OK = (0, 3, 8, 11, BIT_GAP, BIT_WIDTH_HI)

# The rejecting probes, as (row suffix, value, the declension that may drop it,
# why it MUST be refused). A row whose declension is None is never declined:
# every target must refuse it however its bound is carried.
#
# Every rejecting value now sits OUTSIDE the implied width, so each is a
# candidate for `--storage-masked`: a corelib that narrows the element to the
# receiver's storage before generated code sees it has already turned the value
# into an in-width one by the time anything may test it.
ENUM_REJECTS = (
    ("above_width", ENUM_WIDTH_HI + 1, "masked",
     "one past the i8 the declaration implies -- the smallest value that must "
     "still be refused, and the one a bound taken from the constants' hull "
     "(0..10) would refuse for the wrong reason"),
    ("below_width", ENUM_WIDTH_LO - 1, "masked",
     "one BELOW that i8, and expressible because an enum array travels as a "
     "SIGNED array -- a bound that lost its lower half, or a hook handed only an "
     "elem_max, keeps it while every other row here still passes"),
    ("beyond_storage", ENUM_WIDE, "masked",
     "far past the implied i8; keeping it masked is the older defect, keeping it "
     "at all is this one"),
)
BIT_REJECTS = (
    ("above_width", BIT_WIDTH_HI + 1, "masked",
     "one past the u8 the highest declared `pos` implies -- the smallest value "
     "that must still be refused"),
    ("beyond_storage", BIT_WIDE * 16, "masked",
     "far past that u8; a mask on the raw carrier refuses it with the same "
     "clause, so no separate width term is needed"),
)

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


def emit_schema(live) -> int:
    """Print the `closed` message, for appending to a conformance schema.

    The message keeps the name `closed`: it is what every harness in the family
    names, and the rule it probes changed, not the shape.

    A cell a suite declined with --skip-positions is not DECLARED either: a
    target that cannot express the shape at all could not build a harness for a
    schema that names it. The declension is per (position, kind), so a target
    that can carry one kind of matrix row but not the other declares the half it
    can build. No suite declines anything this way today.

    `live` is the set of (position, kind) pairs still in play.
    """
    e, b = enum_yaml(), bits_yaml()

    def on(pos, kind):
        return (pos, kind) in live
    print("# closed -- the enum / bitfield declared-width message (MESSAGE_SPEC §1,")
    print("# generator#516), printed by")
    print("# tests/conformance/lib/check_declared_width_kinds.py so the ids, the declared")
    print("# constants and the declared positions the fixtures probe have exactly one")
    print("# definition between them. The declarations are GAPPED, which under the width")
    print("# rule is what the ACCEPTING rows are made of: the value between the")
    print("# constants, and the bit no flag declares, both MUST decode.")
    print("  %s:" % MESSAGE)
    print("    payload:")
    if on("scalar", "enum"):
        print("      cen:  { id: %d, type: enum, enum: %s }" % (CEN, e))
    if on("scalar", "bitfield"):
        print("      cbf:  { id: %d, type: bitfield, bits: %s }" % (CBF, b))
    if on("array", "enum"):
        print("      cena: { id: %d, type: array, items: { type: enum, count: 4, enum: %s } }" % (CENA, e))
    if on("array", "bitfield"):
        print("      cbfa: { id: %d, type: array, items: { type: bitfield, count: 4, bits: %s } }" % (CBFA, b))
    if on("struct", "enum") or on("struct", "bitfield"):
        print("      cst:")
        print("        id: %d" % CST)
        print("        type: struct")
        print("        fields:")
        if on("struct", "enum"):
            print("          st_en: { id: 0, type: enum, enum: %s }" % e)
        if on("struct", "bitfield"):
            print("          st_bf: { id: 1, type: bitfield, bits: %s }" % b)
    if on("structarray", "enum") or on("structarray", "bitfield"):
        print("      csa:")
        print("        id: %d" % CSA)
        print("        type: array")
        print("        items:")
        print("          type: struct")
        print("          count: 2")
        print("          fields:")
        if on("structarray", "enum"):
            print("            sa_en: { id: 0, type: enum, enum: %s }" % e)
        if on("structarray", "bitfield"):
            print("            sa_bf: { id: 1, type: bitfield, bits: %s }" % b)
    if on("union", "enum") or on("union", "bitfield"):
        print("      cun:")
        print("        id: %d" % CUN)
        print("        type: union")
        print("        default_id: 0")
        print("        oneof:")
        if on("union", "enum"):
            print("          un_en: { id: 0, type: enum, enum: %s }" % e)
        if on("union", "bitfield"):
            print("          un_bf: { id: 1, type: bitfield, bits: %s }" % b)
    if on("matrix", "enum"):
        print("      cmat: { id: %d, type: array, items: { type: array, count: 2, items: { type: enum, count: 3, enum: %s } } }" % (CMAT, e))
    if on("matrix", "bitfield"):
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


def build_table(live, masked=frozenset()):
    """The rows to run, one per (position, kind, value).

    `live` is the set of (position, kind) cells still in play; `masked` is the
    weaker declension, and it drops exactly ONE rejecting row from the cell it
    names -- see the module docstring for what it means and why the rest of the
    cell still runs.
    """
    kinds = (("enum", ENUM_OK, ENUM_REJECTS),
             ("bitfield", BIT_OK, BIT_REJECTS))
    rows = []
    for pos in POSITIONS:
        for kind, oks, rejects in kinds:
            if (pos, kind) not in live:
                continue
            build, leaf, depth = SHAPES[(pos, kind)]
            for v in oks:
                rows.append(("%s_%s_ok_%d" % (pos, kind, v), build(v), "accept",
                             (leaf, v, depth),
                             "%s carrying the DECLARED %s value %d" % (WHY[pos], kind, v)))
            for name, v, declension, why in rejects:
                if declension == "masked" and (pos, kind) in masked:
                    continue
                rows.append(("%s_%s_%s" % (pos, kind, name), build(v), "invalid",
                             None, "%s carrying %d: %s" % (WHY[pos], v, why)))
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
    ap.add_argument("--invalid-pattern", default=None)
    ap.add_argument("--no-values", action="store_true")
    ap.add_argument("--skip-positions", default="")
    ap.add_argument("--storage-masked", default="")
    ap.add_argument("--stream-verb", default=None)
    ap.add_argument("--stream-sizes", default="")
    ap.add_argument("--stream-invalid-pattern", default=None)
    ap.add_argument("--no-stream", default=None)

    argv = sys.argv[1:]
    if "--" in argv:
        sep = argv.index("--")
        head, harness = argv[:sep], argv[sep + 1:]
    else:
        head, harness = argv, []
    args = ap.parse_args(head)

    def named(flag, raw):
        """Parse a declension list into a set of (position, kind) CELLS.

        An item is a position (both kinds) or `position:kind` (one of them), so a
        suite declines exactly the cell its corelib cannot reach -- corelib-cpp
        carries an enum matrix row with a bound and reads a bitfield one
        unbounded, so only the latter is declined, and before this granularity
        existed that cost the whole position.
        """
        cells = set()
        for item in (x.strip() for x in raw.split(",")):
            if not item:
                continue
            pos, sep, kind = item.partition(":")
            if pos not in POSITIONS:
                die("%s: %r is not one of %s" % (flag, pos, ", ".join(POSITIONS)))
            if sep and kind not in KINDS:
                die("%s: %r is not one of %s" % (flag, kind, ", ".join(KINDS)))
            for k in ((kind,) if sep else KINDS):
                cells.add((pos, k))
        return cells

    def spell(cells):
        return ", ".join("%s:%s" % c for c in sorted(cells))

    skip = named("--skip-positions", args.skip_positions)
    live = {(p, k) for p in POSITIONS for k in KINDS} - skip

    if args.emit_schema:
        return emit_schema(live)

    if not live:
        die("--skip-positions declined every position; there is nothing left to check")
    masked = named("--storage-masked", args.storage_masked)
    both = masked & skip
    if both:
        die("--storage-masked names %s, which --skip-positions already declined"
            % spell(both))
    # One of the two category channels is REQUIRED on a rejecting row: exit
    # status alone scores a panic or a process abort as a correct INVALID, which
    # is the failure this rule's zig half existed to remove (generator#517).
    if not args.status_verb and not args.invalid_pattern:
        die("neither --status-verb nor --invalid-pattern was given; a rejecting row "
            "would then be asserted by exit status alone, and a panic or a "
            "safety-checked abort would score as a correct INVALID")
    invalid_re = re.compile(args.invalid_pattern) if args.invalid_pattern else None

    # The chunked replay is declared the same way: a suite either names its
    # streaming verb or says why it has none, and the declension prints. Leaving
    # both out would let the leg go missing in silence, which is the failure mode
    # every declension in this driver exists to rule out.
    if args.stream_verb and args.no_stream:
        die("--stream-verb and --no-stream are mutually exclusive: a suite either "
            "replays the table through its streaming surface or says why it cannot")
    if not args.stream_verb and not args.no_stream:
        die("neither --stream-verb nor --no-stream was given; the chunked replay "
            "would then be missing in silence. Name the harness's streaming verb, "
            "or decline it with --no-stream '<reason>'.")
    splits = []
    stream_re = None
    if args.stream_verb:
        # A rejecting row needs a CATEGORY on this leg too, and --status-verb
        # cannot serve it: that verb runs the ONE-SHOT decoder, so using it here
        # would assert the one-shot verdict twice and the chunked one never.
        pattern = args.stream_invalid_pattern or args.invalid_pattern
        if not pattern:
            die("--stream-verb needs a category channel for its rejecting rows: "
                "give --stream-invalid-pattern, or --invalid-pattern, which it "
                "falls back to. --status-verb cannot serve this leg -- it runs the "
                "one-shot decoder, and would assert that verdict twice while "
                "asserting the chunked one never")
        stream_re = re.compile(pattern)
        raw = [x.strip() for x in args.stream_sizes.split(",") if x.strip()]
        for item in raw:
            if not item.isdigit():
                die("--stream-sizes: %r is not a chunk size (0 = the whole buffer "
                    "in one feed)" % item)
        if raw and not args.message:
            die("--stream-sizes passes the size as the argument AFTER the message "
                "name, so --message may not be empty")
        # No sizes given: the harness reads no size argument and drives its own
        # fixed split -- one byte per feed throughout this family. Passing widths
        # it would ignore is worse than not sweeping at all, because the summary
        # would then claim a sweep that never happened. That is enforced below,
        # once the table exists to probe with, rather than left to the caller.
        splits = [int(x) for x in raw] or [None]

    if not args.label:
        die("no label given (the suite name this run is reported under)")
    if not harness:
        die("no harness argv given (put it after `--`)")

    msg = [args.message] if args.message else []
    table = build_table(live, masked)

    # The rule above -- pass sizes only to a harness that reads them -- was stated
    # and not enforced, which leaves exactly the hole it exists to close: a harness
    # that IGNORES the argument decodes identically at every rung, and the summary
    # line then reports a sweep that never happened. Read as coverage, it is worse
    # than the single split it replaced.
    #
    # Probe it once, with a size no harness can parse. One that reads the argument
    # must fail on it; one that never looks at it decodes as usual. A harness that
    # swallows the parse error and falls back to a default reads as "ignores it"
    # here, which is the safe direction: the sweep is refused rather than claimed.
    if splits != [None]:
        probe_wire = next((w for _, w, e, _, _ in table if e == "accept"), None)
        if probe_wire is not None:
            prc, _, _ = run(harness + [args.stream_verb] + msg + [NOT_A_SIZE],
                            args.cwd, probe_wire)
            if prc == 0:
                die("--stream-sizes was given, but the %r verb ignores the chunk "
                    "size: handed %r as the size it decoded normally, so every "
                    "rung of the sweep feeds the same bytes the same way and the "
                    "summary would claim a sweep that never happened. Either make "
                    "the harness read the argument after the message name (see "
                    "the java and kotlin harnesses), or drop --stream-sizes and "
                    "let it drive its own split."
                    % (args.stream_verb, NOT_A_SIZE))

    def where(size):
        if size is None:
            return "the harness's own split"
        return "the whole buffer in one feed" if size == 0 else "%d-byte chunks" % size

    # `None` is a legitimate split (the harness's own), so the one-shot leg needs
    # a marker of its own rather than sharing it.
    one_shot_leg = object()

    def tag(size):
        if size is one_shot_leg:
            return ""
        return " through %s at %s" % (args.stream_verb, where(size))

    # Every harness invocation this run makes, collected BEFORE any of them runs.
    # Process startup dominates -- a JVM, a `dotnet` host or an `npx tsx` costs
    # far more than decoding nine bytes -- and the runs are independent, so a
    # pool turns minutes into seconds. Results are keyed and judged in TABLE
    # order afterwards, so which failure is reported never depends on which
    # process happened to finish first.
    jobs = []
    for name, wire, expect, value, why in table:
        jobs.append(((name, "verb", None), harness + [args.verb] + msg, wire))
        if args.status_verb:
            jobs.append(((name, "status", None),
                         harness + [args.status_verb] + msg, wire))
        for size in splits:
            jobs.append(((name, "stream", size),
                         harness + [args.stream_verb] + msg
                         + ([] if size is None else [str(size)]), wire))
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=min(8, os.cpu_count() or 2)) as pool:
        done = dict(zip([j[0] for j in jobs],
                        pool.map(lambda j: run(j[1], args.cwd, j[2]), jobs)))

    def check_value(name, out, value, why, wire, size):
        """Assert the row's leaf in a harness's printed JSON; return the object."""
        obj = decoded_json(out)
        if obj is None:
            die("[%s] %s%s -- the harness printed no JSON object; got:\n%s"
                % (args.label, name, tag(size), out.strip()))
        leaf, want, depth = value
        got = find_leaf(obj, leaf)
        if got is None:
            die("[%s] %s%s -- the harness printed no %r; got:\n%s"
                % (args.label, name, tag(size), leaf, out.strip()))
        got = unwrap(got, depth, "[%s] %s%s" % (args.label, name, tag(size)))
        if isinstance(got, str):
            try:
                got = int(got, 0)
            except ValueError:
                pass
        if isinstance(got, bool) or not isinstance(got, (int, float)) or int(got) != want:
            die("[%s] %s%s decoded, but %s is %r -- want %d (%s); bytes: %s"
                % (args.label, name, tag(size), leaf, got, want, why, wire.hex()))
        return obj

    for name, wire, expect, value, why in table:
        rc, out, err = done[(name, "verb", None)]
        text = (out + err).strip()
        one_shot = None

        if expect == "invalid":
            if rc == 0:
                die("[%s] %s must be INVALID (MESSAGE_SPEC §1/§7.1) -- %s; the "
                    "decode succeeded instead:\n%s"
                    % (args.label, name, why, text))
            if invalid_re and not invalid_re.search(text):
                die("[%s] %s was refused, but not as INVALID: nothing in the "
                    "harness's output matches %r, so a panic, an assertion failure "
                    "or a safety-checked abort would read the same. rc=%d, output:\n%s"
                    % (args.label, name, args.invalid_pattern, rc, text))
            if args.status_verb:
                _, sout, serr = done[(name, "status", None)]
                got = (sout.strip().splitlines() or [""])[0]
                if got != args.status_invalid:
                    die("[%s] %s -- must be %s, got %r%s"
                        % (args.label, name, args.status_invalid, got,
                           ("\n" + serr.strip()) if serr.strip() else ""))
        else:
            if rc != 0:
                die("[%s] %s must DECODE -- %s; rc=%d:\n%s"
                    % (args.label, name, why, rc, text))
            if args.status_verb:
                _, sout, serr = done[(name, "status", None)]
                got = (sout.strip().splitlines() or [""])[0]
                if got != args.status_complete:
                    die("[%s] %s -- must decode %s, got %r%s"
                        % (args.label, name, args.status_complete, got,
                           ("\n" + serr.strip()) if serr.strip() else ""))
            if not args.no_values:
                one_shot = check_value(name, out, value, why, wire, one_shot_leg)

        # ...and the same bytes again, cut into chunks. The verdict and the value
        # must be the ones above at EVERY split: where an array element is stored
        # at its declared width the bound is the corelib narrowing into that
        # destination, reached from a bulk fill that suspends and resumes, and a
        # message that arrives in one piece never makes it do either.
        for size in splits:
            src, sout, serr = done[(name, "stream", size)]
            stext = (sout + serr).strip()
            if expect == "invalid":
                if src == 0:
                    die("[%s] %s must be INVALID%s as well -- %s; the one-shot "
                        "`%s` refused it and the chunked decode accepted it, so "
                        "the verdict depends on where the bytes were cut "
                        "(CORELIB_PLAN §5.2). bytes: %s\n%s"
                        % (args.label, name, tag(size), why, args.verb,
                           wire.hex(), stext))
                if not stream_re.search(stext):
                    die("[%s] %s was refused%s, but not as INVALID: nothing in "
                        "the harness's output matches %r, so a panic, an assertion "
                        "failure or a safety-checked abort would read the same. "
                        "rc=%d, output:\n%s"
                        % (args.label, name, tag(size),
                           args.stream_invalid_pattern or args.invalid_pattern,
                           src, stext))
                continue
            if src != 0:
                die("[%s] %s must DECODE%s as well -- %s; the one-shot `%s` "
                    "accepted it. rc=%d, bytes: %s\n%s"
                    % (args.label, name, tag(size), why, args.verb, src,
                       wire.hex(), stext))
            if args.no_values:
                continue
            chunked = check_value(name, sout, value, why, wire, size)
            if one_shot is not None and chunked != one_shot:
                # The verdict agreeing while the MESSAGE does not is the resume
                # bug a leaf-only assertion cannot see: a decoder that comes back
                # from a suspend in the wrong field keeps the row's own value and
                # loses something beside it.
                die("[%s] %s decoded%s and its %s is right, but the decoded "
                    "MESSAGE is not the one `%s` produced from the same bytes "
                    "(CORELIB_PLAN §5.2/§6.0).\n  one-shot: %s\n  chunked : %s"
                    % (args.label, name, tag(size), value[0], args.verb,
                       json.dumps(one_shot, sort_keys=True),
                       json.dumps(chunked, sort_keys=True)))

    note = ""
    if skip:
        note += "; declined: " + spell(skip)
    if masked:
        note += "; storage-masked (the beyond-storage value never reaches "
        note += "generated code): " + spell(masked)
    if args.no_stream:
        note += "; chunked replay DECLINED: " + args.no_stream
    else:
        note += "; %d chunked replays through %s [%s]" % (
            len(table) * len(splits), args.stream_verb,
            ", ".join("the harness's own split (one byte per feed)" if s is None
                      else "whole" if s == 0 else str(s) for s in splits))
    print("==> [%s] enum/bitfield declared width: %d rows over %d cell(s) OK%s"
          % (args.label, len(table), len(live), note))
    return 0

if __name__ == "__main__":
    sys.exit(main())
