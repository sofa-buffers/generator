#!/usr/bin/env python3
"""A skipped string is never UTF-8-validated; a read one always is (generator#417).

Usage:
  check_skipped_string_utf8.py <label> [--schema PATH] [--field NAME]
                               [--scalar-field NAME] [--tail-field NAME]
                               [--nested-field NAME] [--nested-scalar-field NAME]
                               [--message NAME] [--unknown-id N] [--cwd DIR]
                               [--verb VERB] [--invalid-pattern REGEX]
                               [--status-verb VERB] [--no-declared-leg]
                               [--skip-row NAME]... -- <harness argv...>

`--message` is the harness's own message ARGUMENT (empty for a harness that
takes none), not a scope for the schema scan -- see parse_field.

CORELIB_PLAN §6.4.5 is normative and one sentence long: validation runs only
where a `string` is **materialized** -- read into a destination -- never on a
skip, in any mode. §6.4.4 says where it runs when it does run: at **payload
completion**, so a chunk boundary can never change the verdict. Together they
draw a line that has two sides, and a test that only ever walks one of them is
worse than no test at all:

  * a **declared** string whose bytes are not valid UTF-8 is `INVALID`;
  * the **same bytes** at a position the decoder steps over decode `COMPLETE`,
    with every declared field left at its default.

Both halves have to be asserted on the same byte sequence, because they pull in
opposite directions. A backend that validates too eagerly passes the first and
fails the second; one that never validates passes the second and fails the
first. Neither failure is visible from the other side.

## Why this is a driver and not eleven blocks

The bytes are the same in every language (three headers, one fixlen word, two
bad bytes), the verdicts are the same, and the shape of the assertion -- "the
declared field kept its default" -- is the same. What varies is only how a suite
spawns its harness and how that harness renders a value, and both are absorbed
here: the harness argv is passed through after `--`, and the decoded fields are
compared as JSON **values**, so `"somefp64":3.1415926535897931` (C, C++),
`"somefp64": 3.141592653589793` (Python) and `3.141592653589793` (everyone else)
are one expectation rather than eleven greps that rot apart. That is what
ARCHITECTURE §12 asks for, and the sibling `check_fixlen_array_subtype.py`
beside it is the same shape one rule over.

Three suites hand-rolled this pair before the driver existed (`go`, `dart`,
`kotlin`) and no two of them agreed: three different undeclared ids, three
different assertion styles, the decoded object piped to /dev/null in all three,
and none of them pinning the continuation control below. All eleven suites call
this driver now; the shape those blocks had is what the paragraph above is
about.

## The schema it reads, and why it does not print one

Like its sibling it reads the suite's own schema rather than printing a twelfth
message, and derives every fixture byte from it -- the field's id becomes the
header varint, its declared default becomes the skip rows' expectation. That
buys the same anti-drift property without a second generate-and-build in every
suite (four extra Cargo builds in `rust`, four in `cpp`, a full Maven cycle in
`java`). The rule under test is also *about* what the schema declares, so a
schema the suite did not actually build against would be the wrong one to read.

Five declarations are read: a `string` field (`somestring`), a non-string
scalar (`somefp64`), an unsigned scalar for the continuation control (`someu8`),
and -- for the nested row -- a `struct` field (`somestruct`) with a non-string
scalar inside it (`nestedint`).

## The table

With `header = (id << 3) | wire_type` (§4.3), wire type `2` = fixlen,
`fixlen_word = (length << 3) | subtype` (§4.6) and subtype `2` = string,
`3` = blob:

    declared_ff              id 11, string word, ff ff          INVALID
    declared_bad_cont        id 11, string word, c3 28          INVALID
    declared_truncated_seq   id 11, string word, e2 82          INVALID
    declared_overlong        id 11, string word, c0 af          INVALID
    skipped_unknown_id       UNDECLARED id, string word, ff ff  DEFAULTS
    skipped_blob_subtype     id 11, BLOB word, ff ff            DEFAULTS
    skipped_string_at_scalar id 9 (fp64), string word, ff ff    DEFAULTS
    skipped_string_nested    id 20 { id 0 (u8), string, ff ff } DEFAULTS
    control_reads_back       id 11, string word, "hi"           READS BACK

The four INVALID rows are four different *kinds* of invalidity, not four
spellings of one: `ff` is a byte no sequence may contain in any position,
`c3 28` is a legal lead byte with an illegal continuation, `e2 82` is a sequence
that simply runs out at payload completion (the §6.4.4 row -- a validator that
only looks at bytes as they arrive calls it fine), and `c0 af` is an overlong
encoding of `/`, which is well-formed byte by byte and still forbidden. Every
payload is **complete**, so truncation can never explain a rejection.

The four skip rows are four different reasons to skip, in increasing sharpness.
`skipped_unknown_id` is the easy one: an id the schema never heard of, where a
decoder has nothing to validate into. `skipped_blob_subtype` is not a UTF-8 row
at all and is not claimed as one -- no decoder validates a BLOB payload; it is a
§7.3 skip at a **live destination**, the id that *does* declare a string, kept
because that is where a backend can plausibly materialize first and dispatch
second. `skipped_string_at_scalar` puts a well-formed STRING field on an id
declared as a scalar, and this one is a UTF-8 row: the payload IS a string, so
whoever accepts the header owns a verdict on it, and it is generated code's
decline arm that has to refuse the header -- not the string dispatcher. It is
the row that found generator#417's Python defect.

`skipped_string_nested` is that same shape one scope down: the identical bytes
at a scalar id inside a sequence-framed struct. It is a separate row because it
is a separate generated dispatch arm -- every backend renders one arm per scope,
and the two defects this family has already had (generator#297, generator#300)
were both a skip that was right at the message root and wrong inside a nested
scope. A per-scope guard written once and emitted N times is exactly the thing a
root-only table cannot measure.

`control_reads_back` is what stops the whole skip half from passing vacuously. A
backend that skipped *everything* -- or a harness pointed at the wrong field --
satisfies all four DEFAULTS rows and only fails here.

## The continuation control

Every skip row and the control carry a trailing `someu8 = 42` (a value that is
not its declared default). A skip that swallowed the rest of the message, or
consumed one byte too few, still leaves the declared string at its default and
would pass a test that asserted only that; it cannot leave `someu8` at 42. This
is the assertion none of the three hand-rolled blocks had.

## Categories

A skipped field decoding is asserted by exit status plus the decoded values, but
a *rejection* needs the category named, or a wrongly-INCOMPLETE verdict passes
on exit status alone. So the INVALID rows go through whichever of two shapes a
harness has: `--status-verb` for a verb printing `INVALID`/`COMPLETE` on line 1
(`status`, `trydecode`), `--invalid-pattern` for a harness that names the
category in its error text. Suites with neither may still run the skip half --
that half does not need a category -- but they are asked to say so.

## Both decode surfaces

Callers run the table through `--verb decode` and `--verb streamdecode`. §6.4.4
makes this mandatory rather than tidy: validity is a property of the complete
payload, so a chunk boundary must not change the verdict, and several corelibs
reach the validator twice -- once for a whole-buffer decode and once for the
chunked path. A table run only on the one-shot verb passes with the streaming
copy mutated, which is precisely the shape of two defects this family has
already had in skip handling (generator#297, generator#300).

## The two escape hatches

`--no-declared-leg` drops the four INVALID rows. It is for a *footprint* build
that compiles the strict check out entirely (CORELIB_PLAN §6.4.2 lets the `c`
and `c-cpp` profiles default it off): there the declared rows would assert
nothing. The skip rows stay -- §6.4.5 holds "in any mode", and a build with the
validator compiled out is exactly where a decoder could start validating skipped
bytes without anything noticing. A suite that uses this flag is expected to run
the full table against a second, strict-built harness as well.

`--skip-row NAME` quarantines one named row. It exists for a *known, filed*
backend defect and nothing else; the caller must name the issue in a comment
beside it. An unknown row name is a hard failure, so a quarantine cannot outlive
the row it names.
"""

import argparse
import json
import os
import re
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


# §4.3: header = (id << 3) | wire_type. §4.6: fixlen_word = (len << 3) | subtype.
WT_UNSIGNED = 0
WT_FIXLEN = 2
WT_SEQ_BEGIN = 6
SEQ_END = b"\x07"            # §4.9: exactly one byte, (id 0) << 3 | 0b111
SUB_STRING, SUB_BLOB = 2, 3

# Four kinds of invalidity, not four spellings of one -- see the docstring.
BAD_FF = b"\xff\xff"          # a byte no UTF-8 sequence may contain, anywhere
BAD_CONT = b"\xc3\x28"        # legal 2-byte lead, illegal continuation
BAD_TRUNC = b"\xe2\x82"       # 3-byte sequence that runs out at completion
BAD_OVERLONG = b"\xc0\xaf"    # overlong encoding of "/"

TAIL_VALUE = 42               # the continuation control's value


def field_block(lines, field, path):
    """The indented body of one field declaration."""
    hits = [i for i, line in enumerate(lines)
            if re.match(r"^\s+%s:\s*$" % re.escape(field), line)]
    if len(hits) > 1:
        die("schema %s declares %d fields named %r (lines %s) -- this driver "
            "derives its fixtures from one declaration and cannot choose"
            % (path, len(hits), field, ", ".join(str(i + 1) for i in hits)))
    if not hits:
        die("schema %s declares no field %r -- this driver's fixtures are "
            "derived from it and cannot be built" % (path, field))
    start = hits[0]
    indent = len(lines[start]) - len(lines[start].lstrip())
    block = []
    for line in lines[start + 1:]:
        if line.strip() and (len(line) - len(line.lstrip())) <= indent:
            break
        block.append(line)
    return "\n".join(block) + "\n"


def parse_field(path, field, want_type):
    """Read one scalar field's declared id, default and maxlen.

    Deliberately a narrow scan rather than a YAML parse: no lib driver may
    depend on PyYAML, and everything needed here is one indented block. Anything
    it cannot find is a hard failure -- a driver that quietly fell back to a
    hard-coded id would keep passing after the schema moved out from under it.

    The scan is over the whole FILE, not one message: `--message` names the
    harness's message argument, and one suite (`c`) passes it empty because its
    harness takes none, so it is no scope to narrow to. An AMBIGUOUS name is
    therefore refused rather than resolved by position.
    """
    with open(path, "r", encoding="utf-8") as fh:
        lines = fh.read().splitlines()
    text = field_block(lines, field, path)

    def need(pattern, what):
        m = re.search(pattern, text, re.M)
        if not m:
            die("field %r in %s declares no %s" % (field, path, what))
        return m.group(1)

    fid = int(need(r"^\s*id:\s*(\d+)\s*$", "id"))
    ftype = need(r"^\s*type:\s*(\w+)\s*$", "type")
    if ftype != want_type:
        die("field %r is declared %r, but this driver needs a %r there"
            % (field, ftype, want_type))
    m = re.search(r"^\s*default:\s*(.+?)\s*(?:#.*)?$", text, re.M)
    default = None
    if m:
        raw = m.group(1)
        try:
            default = json.loads(raw)
        except ValueError:
            default = raw.strip('"\'')
    m = re.search(r"^\s*maxlen:\s*(\d+)\s*$", text, re.M)
    maxlen = int(m.group(1)) if m else None
    return fid, default, maxlen


def build_table(sid, scalar_id, tail_id, unknown_id, nest_id, nest_scalar_id):
    """The fixture table, derived from the schema so the two cannot drift."""
    s_hdr = varint((sid << 3) | WT_FIXLEN)
    u_hdr = varint((unknown_id << 3) | WT_FIXLEN)
    n_hdr = varint((scalar_id << 3) | WT_FIXLEN)
    # §4.9 framing for the nested row: sequence start, the child scope's own
    # header, sequence end.
    seq_hdr = varint((nest_id << 3) | WT_SEQ_BEGIN)
    in_hdr = varint((nest_scalar_id << 3) | WT_FIXLEN)
    # A trailing field a skip must not eat: someu8 = 42, not its default.
    tail = varint((tail_id << 3) | WT_UNSIGNED) + varint(TAIL_VALUE)

    def word(payload, subtype):
        return varint((len(payload) << 3) | subtype)

    def declared(payload):
        return s_hdr + word(payload, SUB_STRING) + payload

    return [
        ("declared_ff", declared(BAD_FF), "invalid",
         "0xff, a byte no UTF-8 sequence may contain in any position, in a "
         "DECLARED string"),
        ("declared_bad_cont", declared(BAD_CONT), "invalid",
         "a legal 2-byte lead followed by a byte that is not a continuation"),
        ("declared_truncated_seq", declared(BAD_TRUNC), "invalid",
         "a 3-byte sequence that runs out at payload completion (§6.4.4: the "
         "verdict is taken on the COMPLETE payload, not byte by byte)"),
        ("declared_overlong", declared(BAD_OVERLONG), "invalid",
         "an overlong encoding of '/' -- well-formed byte by byte, still "
         "forbidden"),
        ("skipped_unknown_id",
         u_hdr + word(BAD_FF, SUB_STRING) + BAD_FF + tail, "accept",
         "invalid UTF-8 in a string on an id the schema does not declare"),
        ("skipped_blob_subtype",
         s_hdr + word(BAD_FF, SUB_BLOB) + BAD_FF + tail, "accept",
         "a BLOB subtype on the id that DOES declare a string -- a §7.3 skip "
         "at a live string destination, which is where a backend can "
         "materialize first and dispatch second"),
        ("skipped_string_at_scalar",
         n_hdr + word(BAD_FF, SUB_STRING) + BAD_FF + tail, "accept",
         "a well-formed STRING field on an id declared as a non-string scalar "
         "-- generated code's decline arm has to refuse it, not the string "
         "dispatcher"),
        ("skipped_string_nested",
         seq_hdr + in_hdr + word(BAD_FF, SUB_STRING) + BAD_FF + SEQ_END + tail,
         "accept",
         "the same bytes one scope down, at a scalar id inside a "
         "sequence-framed struct -- a separate generated dispatch arm, and the "
         "scope where this family's earlier skip defects lived "
         "(generator#297, generator#300)"),
        ("control_reads_back",
         s_hdr + word(b"hi", SUB_STRING) + b"hi" + tail, "read",
         "the same field, the same header, valid bytes -- must be READ, or "
         "every row above passes vacuously"),
    ]


def run(argv, cwd, data):
    proc = subprocess.run(argv, cwd=cwd, input=data,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return (proc.returncode,
            proc.stdout.decode("utf-8", "replace"),
            proc.stderr.decode("utf-8", "replace"))


def decoded(out):
    """The harness's JSON object, whatever else it printed around it."""
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            return json.loads(line)
        except ValueError:
            continue
    return None


def same(got, want):
    if isinstance(want, bool) or isinstance(got, bool):
        return got is want
    if isinstance(want, (int, float)) and isinstance(got, (int, float)):
        return float(got) == float(want)
    return got == want


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("label")
    ap.add_argument("--schema", default=None)
    ap.add_argument("--field", default="somestring")
    ap.add_argument("--scalar-field", default="somefp64")
    ap.add_argument("--tail-field", default="someu8")
    ap.add_argument("--nested-field", default="somestruct")
    ap.add_argument("--nested-scalar-field", default="nestedint")
    ap.add_argument("--message", default="myfirstmessage")
    ap.add_argument("--unknown-id", type=int, default=99)
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--invalid-pattern", default=None)
    ap.add_argument("--status-verb", default=None)
    ap.add_argument("--no-declared-leg", action="store_true")
    ap.add_argument("--skip-row", action="append", default=[])
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
    sid, sdefault, smaxlen = parse_field(schema, args.field, "string")
    if sdefault is None:
        sdefault = ""
    if smaxlen is not None and smaxlen < 2:
        die("field %r declares maxlen %d; this driver's payloads are 2 bytes"
            % (args.field, smaxlen))
    scalar_id, scalar_default, _ = parse_field(schema, args.scalar_field,
                                               "fp64")
    tail_id, tail_default, _ = parse_field(schema, args.tail_field, "u8")
    nest_id, _, _ = parse_field(schema, args.nested_field, "struct")
    nest_scalar_id, _, _ = parse_field(schema, args.nested_scalar_field, "u8")
    if tail_default == TAIL_VALUE:
        die("the continuation control writes %s = %d, which is also its "
            "declared default -- it would prove nothing"
            % (args.tail_field, TAIL_VALUE))
    for name, fid in (("--field", sid), ("--scalar-field", scalar_id),
                      ("--tail-field", tail_id), ("--nested-field", nest_id)):
        if fid == args.unknown_id:
            die("--unknown-id %d is the declared id of %s" % (fid, name))
    with open(schema, "r", encoding="utf-8") as fh:
        if re.search(r"^\s*id:\s*%d\s*$" % args.unknown_id, fh.read(), re.M):
            die("--unknown-id %d IS declared by %s -- pick an id the schema "
                "does not use, or the skip rows prove nothing"
                % (args.unknown_id, schema))

    table = build_table(sid, scalar_id, tail_id, args.unknown_id, nest_id,
                        nest_scalar_id)
    known = set(name for name, _, _, _ in table)
    for name in args.skip_row:
        if name not in known:
            die("--skip-row %r names no row in this table (%s) -- a quarantine "
                "must not outlive the row it names"
                % (name, ", ".join(sorted(known))))
    msg = [args.message] if args.message else []

    n_invalid = n_accept = 0
    for name, wire, expect, why in table:
        if name in args.skip_row:
            continue
        if expect == "invalid" and args.no_declared_leg:
            continue
        rc, out, err = run(harness + [args.verb] + msg, args.cwd, wire)

        if expect == "invalid":
            if rc == 0:
                die("[%s] %s [%s] must be INVALID (CORELIB_PLAN §6.4.4) but the "
                    "decode SUCCEEDED -- %s; bytes: %s"
                    % (args.label, name, args.verb, why, wire.hex()))
            if args.invalid_pattern and not re.search(args.invalid_pattern,
                                                      out + err):
                die("[%s] %s -- rejected, but not as InvalidMessage: the "
                    "harness must name %r and printed:\n%s"
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
            n_invalid += 1
            continue

        # Accept rows: the decode must succeed AND leave the declared fields
        # exactly as the schema declared them, with the trailing field read.
        if rc != 0:
            die("[%s] %s [%s] must decode COMPLETE (CORELIB_PLAN §6.4.5: a "
                "skipped string is never validated, in any mode) -- %s; "
                "rc=%d:\n%s"
                % (args.label, name, args.verb, why, rc, (out + err).strip()))
        obj = decoded(out)
        if obj is None:
            die("[%s] %s -- the harness printed no JSON object; got:\n%s"
                % (args.label, name, out.strip()))
        want = {args.tail_field: TAIL_VALUE}
        if expect == "read":
            want[args.field] = "hi"
        else:
            want[args.field] = sdefault
            want[args.scalar_field] = scalar_default
        for fname, wanted in sorted(want.items()):
            if fname not in obj:
                die("[%s] %s -- the harness printed no %r; got:\n%s"
                    % (args.label, name, fname, out.strip()))
            if not same(obj[fname], wanted):
                die("[%s] %s decoded, but %r is %s -- want %s (%s); bytes: %s"
                    % (args.label, name, fname, json.dumps(obj[fname]),
                       json.dumps(wanted), why, wire.hex()))
        n_accept += 1

    if n_invalid + n_accept == 0:
        die("[%s] ran no rows at all" % args.label)
    print("   [%s] skipped-string UTF-8 [%s]: %d rows (%d INVALID, %d skipped "
          "+ read) on %s id %d"
          % (args.label, args.verb, n_invalid + n_accept, n_invalid, n_accept,
             args.field, sid))


if __name__ == "__main__":
    main()
