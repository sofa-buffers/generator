#!/usr/bin/env python3
"""An fp32 payload survives decode -> re-encode BIT-FOR-BIT (generator#468).

Usage:
  check_fp32_nan.py <label> --schema PATH
                    --scalar-message NAME --scalar-field NAME
                    --array-message NAME --array-field NAME
                    [--array-default-message NAME --array-default-field NAME]
                    [--cwd DIR] [--verb recode]
                    [--expect N] [--expect-normalize N]
                    -- <harness argv...>

CORELIB_PLAN §6.5 states one rule: for every implementation, decode ->
re-encode of any fp32 payload -- a signaling NaN included -- MUST reproduce the
exact 4 wire bytes, at every fp32 position. The rule is universal; the HAZARD is
not. It belongs to the three targets whose only float type is a double:
TypeScript, Python and Dart. Widening an fp32 signaling NaN into a double is not
bit-preserving -- IEEE widening SETS the quiet bit and keeps the payload
(§6.5's diagram: 0x7F800001 -> 0x7FC00001) -- so the signaling-ness is gone the
instant the value passes through the wider float, and no later code can recover
it. The fix, in all three, is a raw-bits channel beside the numeric one.

## Why this is a driver and not three shell blocks

Each of the three pinned this after a real defect and each wrote its own block:
TypeScript (generator#235) and Dart (generator#226) as shell loops over the
harness `recode` verb, Python (generator#414) as an in-process driver, because
`generators/python/project.go` emitted no `recode` verb at all. No two tables
agreed -- dart had no control value and no §2 row, ts had no quiet NaN at an
array position, only python had both §2 rows -- so each suite's coverage was an
accident of which defect its author had just fixed. ARCHITECTURE §12 asks for
one driver per CONCERN; the concern here is one sentence of §6.5, and the only
thing that differed between the three was how a suite spawns its harness. That
is absorbed after `--`, the way `check_chunk_invariance.py` absorbs it.

The verb has to be wire -> wire. A block over `encode`/`decode` would be
vacuous or falsely red: JSON has no rendering for an fp32 NaN's payload bits
(python's `json.dumps` writes the bare token `NaN`, ts writes `null`), and
`decode | encode` of a signaling NaN measurably comes back as the canonical
payload-less quiet NaN 0x7FC00000.

## The table, and why each row is in it

    scalar sNaN     0x7F800001    exact
    scalar qNaN     0x7FC00001    exact
    scalar -qNaN    0xFFC00001    exact
    scalar -sNaN    0xFF800001    exact
    scalar 2.5      (control)     exact
    array  sNaN | 1.0 | -sNaN     exact
    array  sNaN | qNaN | -qNaN    exact
    scalar declared default       normalizes away (§2)
    array  all-zero fp32[3]       exact, where no array default is declared
    array  declared default       normalizes away (§2)

§6.5 asks for a signaling, a quiet and a negative NaN "at both a scalar and an
array position". Sign bit, quiet bit and 23-bit payload are three separate
things a lossy path loses separately, so the negatives are their own rows rather
than a variation: a target that copies the raw bits but rebuilds the sign from
the number would pass the first two rows and fail the third. `2.5` is the
control that says nothing regressed for the ordinary values a double carries
exactly, and the `1.0` wedged BETWEEN two array NaNs pins that the element loop
does not stop at the first NaN -- neither is decoration, both are rows a
raw-bits path has broken in this family before.

The two §2 rows are the mistake that is easiest to make the moment a target
starts carrying raw wire bytes beside a value: bytes on the wire must NOT be
read as "the field was present". MESSAGE_SPEC §2 decides presence from the
VALUE, so an explicitly-encoded default must still normalize away to the empty
message. A backend that flips a `has_` flag when it stashes raw bits passes
every exact row above and fails only here.

Both halves must be present or the table proves nothing: a harness that echoed
its input would pass every exact row, and one that emitted nothing would
pass both §2 rows. The driver refuses a table that is one-sided.

## Derived from the schema, not hard-coded

Every fixture byte comes from the suite's own schema: the field's id becomes the
header varint (§4.3 `header = (id << 3) | wire_type`), the element width and
fp32 subtype the fixlen word (§4.6 `fixlen_word = (len << 3) | subtype`), and
the array field's DECLARED default becomes the §2 row's payload. The scan is a
narrow regex over one named scope rather than a YAML parse -- no lib driver may
depend on PyYAML -- and it handles both the block style of `example.yaml` and
the one-line flow style the ts and dart suites write their `conf.yaml` in.
Anything it cannot find is a hard failure: a driver that quietly fell back to a
hard-coded id would keep passing after the schema moved out from under it.

That also bounds how vacuous a §2 row can be. A wrong id makes the field
unknown, so it is skipped and re-encodes to nothing -- which passes a §2 row and
FAILS every exact row, because they carry the very same header.

The array §2 row is derived from a DECLARED array default, so it needs a field
that has one. Where the suite's array subject does not -- ts and dart point at a
`vecf32a` whose whole job is to be unbounded by a default -- the suite names a
second fp32-array field with `--array-default-message/--array-default-field`,
and the row is built from that one instead. Both suites do; the two targets that
keep a per-ARRAY raw-bits companion (a `Uint8Array | null` in ts, a bit-exact
`Float32List` copy in dart) are exactly where a presence flag is most likely to
be flipped by "raw bytes were captured" rather than by "value != default".

Beside it, and independent of it, the subject array carries the INVERSE row.
Measured: where no default is declared the default is the empty array, so an
all-zero `fp32[3]` is a value and re-encodes verbatim, count word included --
the same mistake §2 forbids, seen from the other side, and it is the count word
rather than §2 that this one pins.

Because those two rows can substitute for each other, a suite pins the number of
each KIND (`--expect` and `--expect-normalize`) and not just the total: a
`default:` the schema stopped declaring -- or wrote in a shape the scan could not
read -- would otherwise swap the §2 row for the exact one and stay green. The
scan reads a list default in both spellings for the same reason, and hard-fails
on a `default:` key whose value it cannot read.

## What this driver does NOT assert

The second oracle §6.5's testing clause names -- "across decode -> re-encode AND
any materialized walk" -- is not reachable from a wire -> wire verb: it needs
in-process field access. §6.5 calls dropping it out by name ("a port that guards
its round-trip path but not its value path is the defect class this section
exists to prevent"), so it stays with the suite that can express it (python's
`tests/conformance/python/fp32_nan_check.py`, which imports THIS table so the
fixtures have one definition). The streaming decode surface is likewise
suite-local: `recode` is one-shot in all three harnesses.
"""

import collections
import concurrent.futures
import json
import os
import re
import struct
import subprocess
import sys

# §4.3 wire types, §4.6 fixlen subtypes.
WT_FIXLEN = 2
WT_ARRAY_FIXLEN = 5
SUB_FP32 = 0
FP32_WIDTH = 4

# The NaNs, as fp32 bit patterns. Three things a lossy path loses separately:
# the quiet bit (sNaN vs qNaN), the payload (the trailing 1), the sign.
SNAN = 0x7F800001
QNAN = 0x7FC00001
NEG_QNAN = 0xFFC00001
NEG_SNAN = 0xFF800001
F_2_5 = 0x40200000            # 2.5, the ordinary-value control
F_1_0 = 0x3F800000            # 1.0, wedged between two array NaNs

Row = collections.namedtuple("Row", "kind label message field wire words exact")
Field = collections.namedtuple("Field", "message name id default count")


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


def bits_of(value):
    """The fp32 bit pattern of a schema-declared default value."""
    try:
        return struct.unpack("<I", struct.pack("<f", float(value)))[0]
    except (TypeError, ValueError, OverflowError) as exc:
        die("declared default %r is not an fp32 value: %s" % (value, exc))


def words_bytes(words):
    return b"".join(struct.pack("<I", w) for w in words)


# ---------------------------------------------------------------------------
# the schema scan
# ---------------------------------------------------------------------------

def _scope_span(text, name, what, path, required=True):
    """The span of one named YAML declaration's body, in either style.

    `vecf32: { payload: { a: { id: 0, type: fp32 } } }` (the flow style the ts
    and dart suites write) and an indented block (`example.yaml`) are both one
    scope; which one it is decides where the body ends. An AMBIGUOUS name is
    refused rather than resolved by position -- the fixtures are derived from
    one declaration and the driver cannot choose between two.

    Returned as a span rather than a string so a nested scope can be EXCISED
    from its parent (`_drop_scope`) and not merely read.
    """
    hits = list(re.finditer(r"(?:^|[{,])([ \t]*)%s:[ \t]*(.*)$" % re.escape(name),
                            text, re.M))
    if not hits:
        if not required:
            return None
        die("schema %s declares no %s %r -- this driver's fixtures are derived "
            "from it and cannot be built" % (path, what, name))
    if len(hits) > 1:
        die("schema %s declares %d %ss named %r -- this driver derives its "
            "fixtures from one declaration and cannot choose"
            % (path, len(hits), what, name))
    m = hits[0]
    if m.group(2).strip().startswith("{"):
        # Flow style: the balanced brace region.
        opened = text.index("{", m.start(2))
        depth = 0
        for i in range(opened, len(text)):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    return (opened, i + 1)
        die("schema %s: %s %r has an unbalanced flow mapping" % (path, what, name))
    if m.group(0)[0] in "{,":
        die("schema %s: %s %r is written inline but carries no mapping"
            % (path, what, name))
    # Block style: every following line indented deeper than the key.
    indent = len(m.group(1))
    lines = text[m.end():].splitlines(True)
    begin = stop = m.end() + (len(lines[0]) if lines else 0)
    for line in lines[1:]:
        if line.strip() and (len(line) - len(line.lstrip())) <= indent:
            break
        stop += len(line)
    return (begin, stop)


def _scope(text, name, what, path):
    begin, stop = _scope_span(text, name, what, path)
    return text[begin:stop]


def _drop_scope(text, name):
    """`text` with a nested scope's body excised, if it declares one.

    A field scope contains its `items:` block, so reading a key off the field
    reads the ELEMENT's key too. Every read here is meant to be the field's own.
    """
    span = _scope_span(text, name, name, "<enclosing scope>", required=False)
    return text if span is None else text[:span[0]] + text[span[1]:]


# A key sits either at the start of a line (block style) or just inside a `{` /
# after a `,` (the one-line flow style the ts and dart suites use), and nowhere
# else -- so `default:` inside a quoted `description:` is never read as one.
_KEY = r"(?:^[ \t]*|[{,][ \t]*)"


def _scalar(text, key):
    m = re.search(_KEY + r"%s:[ \t]*([^,}\n]+)" % re.escape(key), text, re.M)
    return m.group(1).split("#")[0].strip() if m else None


def _default(scope):
    """The `default:` of a field scope, as a python value.

    Read from the field's OWN keys -- the nested `items:` block is excised
    first, so an element-level key can never be mistaken for the field's -- and
    in BOTH spellings a list default has: the inline `[0.0, -1.5, 3.25]` of the
    flow style, and the block

        default:
          - 0.0
          - -1.5

    that the same schema becomes after an innocuous reformat. Reading only the
    inline form would downgrade the §2 array row into the weaker inverse row
    with the suite still green, which is the one outcome the "derived from the
    schema" contract exists to prevent: where the KEY is present, a value this
    cannot read is a hard failure, never a silently dropped row. YAML flow lists
    are JSON lists here, which is why `json.loads` is enough.
    """
    body = _drop_scope(scope, "items")
    key = re.search(_KEY + r"default:[ \t]*", body, re.M)
    if not key:
        return None
    inline = re.match(r"(\[[^\]]*\]|[^,}\n]+)", body[key.end():])
    raw = inline.group(1).split("#")[0].strip() if inline else ""
    if raw:
        try:
            return json.loads(raw)
        except ValueError:
            die("cannot read the declared default %r" % raw)
    # Nothing on the key's own line: a block list follows it.
    values = []
    for line in body[key.end():].splitlines()[1:]:
        if not line.strip():
            continue
        element = re.match(r"^[ \t]*-[ \t]*(.+?)[ \t]*(?:#.*)?$", line)
        if not element:
            break
        values.append(element.group(1))
    if not values:
        die("a `default:` is declared here but carries no value this driver can "
            "read -- the §2 fixture is derived from that default and must not "
            "be built from a guess")
    try:
        return [json.loads(v) for v in values]
    except ValueError:
        die("cannot read the declared block-list default %r" % values)


def parse_fp32_field(path, message, field, want_array):
    """Read one fp32 (or fp32-array) field's id, count and declared default."""
    with open(path, "r", encoding="utf-8") as fh:
        text = fh.read()
    scope = _scope(_scope(text, message, "message", path), field, "field", path)
    raw_id = _scalar(scope, "id")
    if raw_id is None:
        die("field %r in %s declares no id" % (field, path))
    ftype = _scalar(scope, "type")
    count = None
    if want_array:
        if ftype != "array":
            die("field %r is declared %r, but this driver needs an array of "
                "fp32 there" % (field, ftype))
        items = _scope(scope, "items", "items block", path)
        if _scalar(items, "type") != "fp32":
            die("field %r is an array of %r, not of fp32"
                % (field, _scalar(items, "type")))
        raw_count = _scalar(items, "count")
        if raw_count is None:
            die("array field %r in %s declares no count" % (field, path))
        count = int(raw_count)
    elif ftype != "fp32":
        die("field %r is declared %r, but this driver needs an fp32 there"
            % (field, ftype))
    return Field(message, field, int(raw_id), _default(scope), count)


# ---------------------------------------------------------------------------
# the table
# ---------------------------------------------------------------------------

def _scalar_row(field, label, word, exact=True):
    hdr = varint((field.id << 3) | WT_FIXLEN) + \
        varint((FP32_WIDTH << 3) | SUB_FP32)
    return Row("scalar", label, field.message, field.name,
               hdr + struct.pack("<I", word), (word,), exact)


def _array_row(field, label, words, exact=True):
    # §3: the wire count IS the array's length; the declared count is only its
    # capacity, so a 3-element fixture is legal in any array declaring room for
    # 3 or more.
    words = tuple(words)
    hdr = varint((field.id << 3) | WT_ARRAY_FIXLEN) + varint(len(words)) + \
        varint((FP32_WIDTH << 3) | SUB_FP32)
    return Row("array", label, field.message, field.name,
               hdr + words_bytes(words), words, exact)


def build_table(scalar, array, array_default=None):
    """The fixture table, derived from the parsed field declarations.

    Shared with `tests/conformance/python/fp32_nan_check.py`, which runs the
    same rows through the surfaces a `recode` verb cannot reach, so the two
    cannot drift apart into two tables again.

    `array_default` is an optional SECOND fp32-array field, one that declares a
    default, for a suite whose main array subject declares none: it carries the
    §2 array row those suites could not otherwise express.
    """
    if array.count < 3:
        die("array field %r declares count %d; this table needs room for 3 "
            "elements" % (array.name, array.count))

    rows = [
        _scalar_row(scalar, "scalar sNaN      0x%08X" % SNAN, SNAN),
        _scalar_row(scalar, "scalar qNaN      0x%08X" % QNAN, QNAN),
        _scalar_row(scalar, "scalar -qNaN     0x%08X" % NEG_QNAN, NEG_QNAN),
        _scalar_row(scalar, "scalar -sNaN     0x%08X" % NEG_SNAN, NEG_SNAN),
        _scalar_row(scalar, "scalar 2.5       (control)", F_2_5),
        _array_row(array, "array  sNaN|1.0|-sNaN", (SNAN, F_1_0, NEG_SNAN)),
        _array_row(array, "array  sNaN|qNaN|-qNaN", (SNAN, QNAN, NEG_QNAN)),
        # §2: an explicitly-encoded default is still an ABSENT field.
        _scalar_row(scalar, "scalar default   (§2 normalizes away)",
                    bits_of(scalar.default if scalar.default is not None else 0.0),
                    exact=False),
    ]
    if array.default is not None:
        if array_default is not None:
            die("array field %r already declares a default, so the §2 array row "
                "is built from it; --array-default-* names a second subject and "
                "the driver cannot choose" % array.name)
        rows.append(_array_row(array, "array  default   (§2 normalizes away)",
                               [bits_of(v) for v in array.default], exact=False))
    else:
        # No declared default: the default is the EMPTY array, so an all-zero
        # one is a value and must survive verbatim -- count word included. This
        # is the row that catches a backend reading "all elements zero" as
        # "absent", which is the same mistake §2 forbids, seen from the other
        # side.
        rows.append(_array_row(array, "array  0,0,0     (not the default: no "
                               "default declared)", (0, 0, 0)))
    if array_default is not None:
        if array_default.default is None:
            die("field %r declares no default, but it is named as the array §2 "
                "subject and that row IS its default" % array_default.name)
        if len(array_default.default) > array_default.count:
            die("field %r declares a %d-element default in an array of count %d"
                % (array_default.name, len(array_default.default),
                   array_default.count))
        rows.append(_array_row(array_default,
                               "array  default   (§2 normalizes away)",
                               [bits_of(v) for v in array_default.default],
                               exact=False))
    return rows


# ---------------------------------------------------------------------------
# the harness legs
# ---------------------------------------------------------------------------

def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def run_row(cmd, cwd, verb, row):
    p = subprocess.run(
        cmd + [verb, row.message], input=row.wire, cwd=cwd,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return p.returncode, p.stdout, p.stderr


def main():
    argv = sys.argv[1:]
    if "--" not in argv:
        print("\n".join(__doc__.strip().splitlines()[2:10]))
        return 2
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]

    label = head[0] if head else ""
    schema = opt(head, "--schema")
    cwd = opt(head, "--cwd")
    verb = opt(head, "--verb", "recode")
    expect = opt(head, "--expect")
    expect_norm = opt(head, "--expect-normalize")
    s_msg, s_fld = opt(head, "--scalar-message"), opt(head, "--scalar-field")
    a_msg, a_fld = opt(head, "--array-message"), opt(head, "--array-field")
    ad_msg = opt(head, "--array-default-message")
    ad_fld = opt(head, "--array-default-field")
    # Every failure in this file is meant to be a FAIL line, so the four
    # selectors are checked here rather than reaching `re.escape(None)` as a
    # traceback out of the schema scan.
    if not (label and schema and cmd and s_msg and s_fld and a_msg and a_fld):
        print("FAIL: usage: check_fp32_nan.py <label> --schema PATH "
              "--scalar-message NAME --scalar-field NAME "
              "--array-message NAME --array-field NAME -- <harness argv...>")
        return 1
    if bool(ad_msg) != bool(ad_fld):
        print("FAIL: --array-default-message and --array-default-field name one "
              "field together; give both or neither")
        return 1
    if not os.path.isfile(schema):
        die("schema %s does not exist" % schema)

    scalar = parse_fp32_field(schema, s_msg, s_fld, want_array=False)
    array = parse_fp32_field(schema, a_msg, a_fld, want_array=True)
    array_default = (parse_fp32_field(schema, ad_msg, ad_fld, want_array=True)
                     if ad_msg else None)
    rows = build_table(scalar, array, array_default)

    exact_rows = sum(1 for r in rows if r.exact)
    empty_rows = len(rows) - exact_rows
    if expect is not None and len(rows) != int(expect):
        print("FAIL: expected %s fixtures, built %d" % (expect, len(rows)))
        return 1
    # The count alone cannot see a row that changed KIND: where the array §2 row
    # is not expressible the driver carries an exact inverse row in its place,
    # so a schema that stopped declaring the default it is derived from would
    # swap one for the other and keep the count. Suites pin both numbers.
    if expect_norm is not None and empty_rows != int(expect_norm):
        print("FAIL: expected %s §2 normalize-away fixtures, built %d -- a row "
              "changed kind, most likely a `default:` the schema no longer "
              "declares in a shape this driver reads"
              % (expect_norm, empty_rows))
        return 1
    # One-sided tables prove nothing: a harness that echoed stdin passes every
    # exact row, one that printed nothing passes every §2 row.
    if not (exact_rows and empty_rows):
        print("FAIL: table is one-sided (%d exact / %d normalize-away)"
              % (exact_rows, empty_rows))
        return 1

    # One process per row; startup dominates (an `npx tsx` costs far more than
    # re-encoding six bytes) and the rows are independent. Judged in table
    # order, so which row is reported never depends on which finished first.
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=min(8, (os.cpu_count() or 2))) as pool:
        results = list(pool.map(lambda r: run_row(cmd, cwd, verb, r), rows))

    failures = 0
    for row, (rc, out, err) in zip(rows, results):
        detail = err.decode("utf-8", errors="replace").strip()
        if rc != 0:
            print("FAIL %s: must decode, exit %d%s"
                  % (row.label, rc, ": " + detail if detail else ""))
            failures += 1
            continue
        if row.exact:
            if out != row.wire:
                print("FAIL %s: not bit-exact -- an fp32 payload was altered"
                      % row.label)
                print("  want %s" % row.wire.hex())
                print("  got  %s" % out.hex())
                failures += 1
        elif out:
            print("FAIL %s: a value equal to the field's default must "
                  "re-encode as the empty message (MESSAGE_SPEC §2), got %s"
                  % (row.label, out.hex()))
            failures += 1

    if failures:
        return 1
    print("%s fp32 bit-exactness: %d fixtures through `%s` "
          "(%d bit-exact, %d §2 normalize-away); scalar id %d, array id %d"
          % (label, len(rows), verb, exact_rows, empty_rows, scalar.id, array.id))
    return 0


if __name__ == "__main__":
    sys.exit(main())
