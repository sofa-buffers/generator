#!/usr/bin/env python3
"""A fixlen ARRAY of string/blob/reserved elements is INVALID, not a skip (generator#411).

Usage:
  check_fixlen_array_subtype.py <label> [--schema PATH] [--field NAME]
                                [--message NAME] [--unknown-id N] [--cwd DIR]
                                [--invalid-pattern REGEX] [--status-verb VERB]
                                -- <harness argv...>

CORELIB_PLAN §4.8.1 fixes the fixlen-array decode order in five steps, and the
order of the middle three is **normative**:

  1. read `element_count`, format ceiling only, commit no memory;
  2. read the `fixlen_word` -- subtype plus per-element length;
  3. if the subtype is anything but `fp32`/`fp64` -- a `string`, a `blob`, or a
     reserved `0x4`-`0x7` -- the field is **INVALID**, *before any schema is
     consulted* (§4.8, §5.2.2);
  4. a fixed-width subtype that merely CONTRADICTS the declared element type is a
     MESSAGE_SPEC §7.3 mismatch -- **skip**, and the schema `count` MUST NOT be
     applied;
  5. otherwise apply the schema `count` (over it -> INVALID).

Steps 3 and 4 are different verdicts on different questions. Step 3 is a
**format** violation: §4.8 admits no fixlen array of `string` or `blob`, so no
schema could have declared one and the bytes are malformed whatever follows.
Step 4 is a **schema** mismatch: a perfectly well-formed field, merely not this
one. Routing step 3 into the skip would make a decoder accept a construct the
format does not have -- and it would do so silently, because generated code
never sees the `fixlen_word`: every backend's array arm reduces to "is this the
element kind I declared? no -> return", so a corelib that forwards such a header
instead of rejecting it at the word is accepted without a murmur.

## Why this needs a driver rather than eleven blocks

Seven suites already carry a `generator#259` block covering steps 4 and 5, and
all seven hard-code the same four bytes against the same field. Step 3 is the
same bytes again, the same field again and the same verdict again in all eleven
languages -- exactly the shape ARCHITECTURE §12 wants written once. What varies
is only how a suite spawns its harness and how that harness renders a float, and
both are absorbed here: the harness argv is passed through, and the decoded value
is compared as JSON **numbers**, so `[0,-1.5,3.25]` (C, C++, C#, Go, TypeScript),
`[0.0,-1.5,3.25]` (Rust, Java, Kotlin, Dart) and `[0, -1.5, 3.25]` (Python) are
one expectation rather than eleven greps that rot apart.

## The schema it reads, and why it does not print one

The other lib drivers print their own schema so the ids they assert cannot drift
from the ids they declare. This one reads the suite's schema instead and derives
every fixture byte from it -- the field's id becomes the header varint, its
element count and default become the controls' expectations -- which buys the
same anti-drift property without the generate-and-build a twelfth message would
cost every suite (four extra Cargo builds in `rust`, four in `cpp`, a full Maven
and Gradle cycle in `java` and `kotlin`). It also fits the rule under test better
than an own schema would: step 3 is decided *before any schema is consulted*, so
the sharpest fixture here -- the same bad subtype on an id the schema does not
declare at all -- has no schema to print.

## The table

Against a declared fixlen array field (`somefloatarray`: `fp32`, `count: 3`,
id 17 -> header `\215\001`), with `fixlen_word = (length << 3) | subtype` and
subtype `0`=fp32, `1`=fp64, `2`=string, `3`=blob, `4`-`7` reserved (§4.6):

    subtype_string          count 2, word 0x22  4-byte STRING     INVALID
    subtype_blob            count 2, word 0x23  4-byte BLOB       INVALID
    subtype_reserved_4      count 2, word 0x24  reserved 0x4      INVALID
    subtype_reserved_7      count 2, word 0x27  reserved 0x7      INVALID
    subtype_string_over     count 5, word 0x22                    INVALID  (step 3 precedes step 5)
    subtype_string_unknown  count 2, word 0x22, UNDECLARED id     INVALID  (step 3 precedes any schema)
    fp64_skip               count 2, word 0x41  8-byte fp64       accepted, field keeps its DEFAULT
    fp64_skip_unknown       count 2, word 0x41, UNDECLARED id     accepted, field keeps its DEFAULT
    fp32_read               count 3, word 0x20  4-byte fp32       accepted, field READS BACK

Every count is **inside** the declared `count: 3` except the one that says
otherwise, and every payload is **complete** -- so neither the schema bound nor
truncation can explain a rejection, which is what makes these step-3 rows and
not step-5 or §5.2.3 ones.

The table straddles the accept/reject boundary on purpose, and in both
directions on the *same* subtype question: a decoder that rejects every fixlen
array fails `fp64_skip`, one that accepts every subtype fails the four INVALID
rows, and one that skips the field it should have read fails `fp32_read`. The
`_unknown` pair is the row no schema can explain -- the bad subtype and its good
twin sit on the very same undeclared id, so the only thing separating them is
the `fixlen_word`.

## Categories

Not every harness has a category channel: `c` and the `corelib: c-cpp` C++ legs
report exit status only. So the category is asserted where a suite HAS one, via
whichever of the two shapes it has -- `--status-verb` for a verb that prints
`INVALID`/`COMPLETE` on line 1 (`status`, `trydecode`), `--invalid-pattern` for a
harness that names the category in its error text -- while the exit-status leg
runs everywhere. Without one of those a wrongly-INCOMPLETE verdict would pass,
which is why the suites that can pin it, do.

§5.2.3's neighbouring question -- the same bad word with the payload *cut off*,
which must still be INVALID rather than INCOMPLETE -- is deliberately not here:
that is a different rule and belongs with the verdict-precedence coverage.
"""

import argparse
import json
import os
import re
import struct
import subprocess
import sys


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


def fixlen_word(length, subtype):
    """§4.6: fixlen_word = (length << 3) | subtype."""
    return varint((length << 3) | subtype)


# Wire type 5 = array-fixlen; a field header is (id << 3) | wire_type (§4.2).
WT_ARRAY_FIXLEN = 5
FP32, FP64, STRING, BLOB = 0, 1, 2, 3


def parse_schema_field(path, field):
    """Read the declared id / element type / count / default of one array field.

    Deliberately a narrow scan rather than a YAML parse: no lib driver may depend
    on PyYAML, and everything needed here is one indented block. Anything it
    cannot find is a hard failure -- a driver that quietly fell back to a
    hard-coded id would keep passing after the schema moved out from under it.
    """
    with open(path, "r", encoding="utf-8") as fh:
        lines = fh.read().splitlines()
    start = None
    indent = 0
    for i, line in enumerate(lines):
        if re.match(r"^\s+%s:\s*$" % re.escape(field), line):
            start = i
            indent = len(line) - len(line.lstrip())
            break
    if start is None:
        die("schema %s declares no field %r -- this driver's fixtures are "
            "derived from it and cannot be built" % (path, field))
    block = []
    for line in lines[start + 1:]:
        if line.strip() and (len(line) - len(line.lstrip())) <= indent:
            break
        block.append(line)
    text = "\n".join(block) + "\n"

    def need(pattern, what):
        m = re.search(pattern, text, re.M)
        if not m:
            die("field %r in %s declares no %s" % (field, path, what))
        return m.group(1)

    fid = int(need(r"^\s*id:\s*(\d+)\s*$", "id"))
    ftype = need(r"^\s*type:\s*(\w+)\s*$", "type")
    if ftype != "array":
        die("field %r is %r, not an array -- this driver needs a fixlen array"
            % (field, ftype))
    etype = need(r"^\s*items:\s*\n(?:.*\n)*?\s+type:\s*(\w+)\s*$",
                 "element type")
    if etype != "fp32":
        die("field %r declares %r elements; this driver's controls assume fp32"
            % (field, etype))
    count = int(need(r"^\s*items:\s*\n(?:.*\n)*?\s+count:\s*(\d+)\s*$",
                     "element count"))
    default = json.loads(need(r"^\s*default:\s*(\[[^\]]*\])\s*$", "default"))
    if len(default) != count:
        die("field %r has count %d but a default of %d elements -- the skip "
            "controls assert the default and would be ambiguous under padding"
            % (field, count, len(default)))
    return fid, count, default


def f32(x):
    return struct.pack("<f", x)


def build_table(fid, count, unknown_id):
    """The fixture table, derived from the schema so the two cannot drift."""
    hdr = varint((fid << 3) | WT_ARRAY_FIXLEN)
    unk = varint((unknown_id << 3) | WT_ARRAY_FIXLEN)
    pay8 = b"abcdefgh"                    # 2 x 4-byte elements, COMPLETE
    over = count + 2                      # over the declared bound

    rows = [
        ("subtype_string", hdr + varint(2) + fixlen_word(4, STRING) + pay8,
         "invalid", None,
         "a 4-byte STRING fixlen-array subtype"),
        ("subtype_blob", hdr + varint(2) + fixlen_word(4, BLOB) + pay8,
         "invalid", None,
         "a 4-byte BLOB fixlen-array subtype"),
        ("subtype_reserved_4", hdr + varint(2) + fixlen_word(4, 4) + pay8,
         "invalid", None,
         "the reserved fixlen subtype 0x4"),
        ("subtype_reserved_7", hdr + varint(2) + fixlen_word(4, 7) + pay8,
         "invalid", None,
         "the reserved fixlen subtype 0x7, the top of the reserved range"),
        ("subtype_string_over",
         hdr + varint(over) + fixlen_word(4, STRING) + b"x" * (4 * over),
         "invalid", None,
         "a STRING subtype whose count is over the schema bound "
         "(step 3 must precede step 5)"),
        ("subtype_string_unknown",
         unk + varint(2) + fixlen_word(4, STRING) + pay8,
         "invalid", None,
         "a STRING subtype on an id the schema does not declare "
         "(step 3 must precede any schema)"),
        ("fp64_skip", hdr + varint(2) + fixlen_word(8, FP64) + b"\0" * 16,
         "accept", "default",
         "an fp64 array at the fp32-declared id -- a §7.3 skip, not a reject"),
        ("fp64_skip_unknown",
         unk + varint(2) + fixlen_word(8, FP64) + b"\0" * 16,
         "accept", "default",
         "an fp64 array on an undeclared id -- the good twin of the row above"),
    ]
    read_back = [1.5, 2.5, 3.5, 4.5, 5.5][:count]
    while len(read_back) < count:
        read_back.append(6.5)
    payload = b"".join(f32(v) for v in read_back)
    rows.append(
        ("fp32_read", hdr + varint(count) + fixlen_word(4, FP32) + payload,
         "accept", read_back,
         "a well-formed fp32 array at its own id -- must be READ, not skipped"))
    return rows


def run(argv, cwd, data):
    proc = subprocess.run(argv, cwd=cwd, input=data,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return (proc.returncode,
            proc.stdout.decode("utf-8", "replace"),
            proc.stderr.decode("utf-8", "replace"))


def decoded_field(out, field):
    """Pull `field` out of the harness's JSON, whatever the float rendering."""
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if field in obj:
            return obj[field]
    return None


def same_numbers(got, want):
    if not isinstance(got, list) or len(got) != len(want):
        return False
    return all(isinstance(g, (int, float)) and not isinstance(g, bool)
               and float(g) == float(w) for g, w in zip(got, want))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("label")
    ap.add_argument("--schema", default=None)
    ap.add_argument("--field", default="somefloatarray")
    ap.add_argument("--message", default="myfirstmessage")
    ap.add_argument("--unknown-id", type=int, default=99)
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--invalid-pattern", default=None)
    ap.add_argument("--status-verb", default=None)
    argv = sys.argv[1:]
    if "--" not in argv:
        die("no harness argv given (put it after `--`)")
    sep = argv.index("--")
    args = ap.parse_args(argv[:sep])
    harness = argv[sep + 1:]
    if not harness:
        die("no harness argv given (put it after `--`)")

    root = os.path.dirname(os.path.dirname(os.path.dirname(
        os.path.dirname(os.path.abspath(__file__)))))
    schema = args.schema or os.path.join(root, "examples", "messages",
                                         "example.yaml")
    fid, count, default = parse_schema_field(schema, args.field)
    if fid == args.unknown_id:
        die("--unknown-id %d is the declared id of %r" % (fid, args.field))
    with open(schema, "r", encoding="utf-8") as fh:
        if re.search(r"^\s*id:\s*%d\s*$" % args.unknown_id, fh.read(), re.M):
            die("--unknown-id %d IS declared by %s -- pick an id the schema "
                "does not use, or the pre-schema rows prove nothing"
                % (args.unknown_id, schema))

    table = build_table(fid, count, args.unknown_id)
    msg = [args.message] if args.message else []

    checked = 0
    for name, wire, expect, value, why in table:
        rc, out, err = run(harness + [args.verb] + msg, args.cwd, wire)

        if expect == "invalid":
            if rc == 0:
                die("[%s] %s must be INVALID (CORELIB_PLAN §4.8.1 step 3) but "
                    "the decode SUCCEEDED -- %s; bytes: %s"
                    % (args.label, name, why, wire.hex()))
            if args.invalid_pattern and not re.search(args.invalid_pattern,
                                                      out + err):
                die("[%s] %s -- rejected, but not as InvalidMessage: the harness "
                    "must name %r and printed:\n%s"
                    % (args.label, name, args.invalid_pattern,
                       (out + err).strip()))
            if args.status_verb:
                _, sout, serr = run(harness + [args.status_verb] + msg,
                                    args.cwd, wire)
                got = (sout.strip().splitlines() or [""])[0]
                if got != "INVALID":
                    die("[%s] %s -- category must be INVALID, got %r%s"
                        % (args.label, name, got,
                           ("\n" + serr.strip()) if serr.strip() else ""))
        else:
            if rc != 0:
                die("[%s] %s must DECODE -- %s (step 4 is untouched by "
                    "step 3); rc=%d:\n%s"
                    % (args.label, name, why, rc, (out + err).strip()))
            want = default if value == "default" else value
            got = decoded_field(out, args.field)
            if got is None:
                die("[%s] %s -- the harness printed no %r; got:\n%s"
                    % (args.label, name, args.field, out.strip()))
            if not same_numbers(got, want):
                die("[%s] %s decoded, but %r is %s -- want %s (%s); bytes: %s"
                    % (args.label, name, args.field, json.dumps(got),
                       json.dumps(want), why, wire.hex()))
        checked += 1

    if checked != len(table):
        die("[%s] ran %d of %d rows" % (args.label, checked, len(table)))
    print("   [%s] fixlen-array subtype: %d rows (%d INVALID, %d accepted) "
          "on %s id %d"
          % (args.label, checked,
             sum(1 for r in table if r[2] == "invalid"),
             sum(1 for r in table if r[2] == "accept"),
             args.field, fid))


if __name__ == "__main__":
    main()
