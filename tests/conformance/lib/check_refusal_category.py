#!/usr/bin/env python3
"""A receiver cap answers LimitExceeded; a schema bound answers InvalidMessage (generator#416).

Usage:
  check_refusal_category.py --emit-schema [--shapes LIST]
  check_refusal_category.py <label> [--message NAME] [--cwd DIR] [--verb VERB]
                            [--shapes LIST]
                            [--max-dyn-array-count N] [--max-dyn-string-len N]
                            [--max-dyn-blob-len N]
                            [--status-verb VERB] [--status-limit NAME]
                            [--status-invalid NAME] [--status-complete NAME]
                            [--limit-pattern REGEX] [--invalid-pattern REGEX]
                            [--no-values] -- <harness argv...>

CORELIB_PLAN §6.3 keeps three refusals apart, and the first two are the pair a
decode can confuse:

  * over a bound the SCHEMA declares (`count`, `maxlen`, a declared width) ->
    **InvalidMessage**. The sender wrote something this schema cannot hold.
  * over a configured RECEIVER CAP on a field the schema leaves unbounded
    (§6.2.1) -> **LimitExceeded**. The bytes are well formed; the same message
    decodes under a looser cap. It is a policy verdict, and §6.3 says it "MUST
    NOT be reported as InvalidMessage" -- reporting it that way tells the caller
    the sender is broken when the receiver's own configuration is what refused.

§6.3 adds the direction that is easy to lose in the other half: LimitExceeded is
"never raised for a field the schema bounds". So the two claims are symmetric and
this driver asserts both, on ONE schema, against ONE harness, with the cap
strictly below the schema bound so that every row is unambiguous.

## Why a driver rather than eleven blocks

Every suite already generates a capped project for the `generator#102` receiver-
limit block, and the rule under test is the same schema, the same fixtures and
the same two verdicts in all of them. Only the RENDERING of a verdict varies --
a status verb that prints the category name, or an error text that names an
exception class -- which is the variation `check_fixlen_array_subtype.py` beside
it already absorbs. So the table lives here once (ARCHITECTURE §12) and a suite
contributes only the capped project and its harness argv.

The driver prints its OWN schema (`--emit-schema`, the `check_vectors_decode.py`
idiom), so the ids it writes into fixtures and the bounds it asserts against
cannot drift from what the harness was built with. `--shapes` must be passed
identically to `--emit-schema` and to the run, as must the cap numbers, or the
schema and the expectations stop describing the same project.

## The four shapes

A receiver cap reaches four different pieces of machinery, and a decoder can get
the category right in one and wrong in another, so `refusal` declares an
unbounded and a bounded field of each -- the bounded one is what pins the mirror
rule -- plus the count-less WRAPPER array, which has no schema-bounded twin
because the whole point of it is that the schema declares no count:

    dynarr   id 0  array<u32>              max_dyn_array_count applies
    bndarr   id 1  array<u32> count 8      bounded -> the cap must NOT apply
    dynstr   id 2  string                  max_dyn_string_len applies
    bndstr   id 3  string maxlen 32        bounded -> the cap must NOT apply
    dynblob  id 4  blob                    max_dyn_blob_len applies
    bndblob  id 5  blob maxlen 32          bounded -> the cap must NOT apply
    wraparr  id 6  array<string>           the element INDEX takes the count cap

The wrapper row is the one shape where the cap is typically NOT compared by
generated code at all: a count-less array's elements are keyed by an unbounded
varint index and the list grows to fit, so the index IS the length, and every
backend hands the number to a corelib collector (corelib-dart's `StringSeq.rcap`,
and the same shape elsewhere) which decides the category itself. A suite that
tested only the count-headed array would leave that decision unmeasured.

`--shapes` selects them (default: all four). A suite whose harness cannot build
one -- a target with no wrapper arrays, a JSON rendering this driver cannot read
a blob back out of -- declines it by name, so the omission is a decision in the
suite rather than a silence.

## The table

Generated with `max_dyn_array_count: 4, max_dyn_string_len: 8,
max_dyn_blob_len: 8`, the rows are:

    dynarr_over_cap     count 5  > cap 4                      LimitExceeded
    dynarr_at_cap       count 4  = cap 4                      COMPLETE
    bndarr_over_bound   count 9  > schema count 8             InvalidMessage
    bndarr_over_cap     count 6  > cap 4, < schema count 8    COMPLETE
    dynstr_over_cap     len 9    > cap 8                      LimitExceeded
    dynstr_at_cap       len 8    = cap 8                      COMPLETE
    bndstr_over_bound   len 40   > schema maxlen 32           InvalidMessage
    bndstr_over_cap     len 20   > cap 8, < schema maxlen 32  COMPLETE
    dynblob_over_cap    len 9    > cap 8                      LimitExceeded
    dynblob_at_cap      len 8    = cap 8                      COMPLETE
    bndblob_over_bound  len 40   > schema maxlen 32           InvalidMessage
    bndblob_over_cap    len 20   > cap 8, < schema maxlen 32  COMPLETE
    wraparr_over_cap    index 4  -> 5 slots > cap 4           LimitExceeded
    wraparr_at_cap      index 3  -> 4 slots = cap 4           COMPLETE

Every payload is COMPLETE, so truncation can never explain a rejection, and the
accepted rows carry their decoded value so a "refuse everything" decoder cannot
pass by rejecting less.

The rows that matter to §6.3 are the `_over_cap` refusals against the
`_over_bound` ones: they differ only in WHICH number the wire breached, so a
decoder that collapses the categories -- in either direction -- fails one of each
pair, whichever way it collapses them. The `_at_cap` and `_over_cap` accepting
rows pin the cap's own value: they fail loudly if a suite configures a cap this
driver was not told about, which is what keeps the assertions honest without the
driver reading the suite's config file. `wraparr` pins it from both sides at the
exact boundary -- index 3 fills the cap and decodes, index 4 is one slot past it
and is refused.

## Categories are asserted, never exit status

A bare non-zero exit cannot see the collapse this issue is about -- both
categories exit non-zero -- so the driver REFUSES to run without a category
channel. Two shapes are supported, matching what a suite's harness offers:

  * `--status-verb` -- a verb printing the verdict name on line 1 (`trydecode`,
    `status`), compared against `--status-limit` / `--status-invalid` /
    `--status-complete`. Exact-match, so exclusivity is free.
  * `--limit-pattern` / `--invalid-pattern` -- regexes over stdout+stderr for a
    harness that names the category in its error text (an exception class, say).
    Both must be given: each refusing row asserts its own pattern MATCHES and the
    other one does NOT, which is the assertion a single "does it mention a
    limit?" grep cannot make.

That the same over-cap bytes decode under a looser cap -- the property that makes
the verdict policy rather than format -- is pinned by each suite's neighbouring
`generator#102` block against its uncapped build; it needs a second project and
so is not this driver's to own.
"""

import argparse
import json
import re
import subprocess
import sys

MESSAGE = "refusal"
SHAPES = ("array", "string", "blob", "wrapper")

# The schema this driver prints. The bounds live here, not in a suite's YAML, so
# a fixture can never breach a number the harness was not built with.
DYNARR_ID, BNDARR_ID, DYNSTR_ID, BNDSTR_ID = 0, 1, 2, 3
DYNBLOB_ID, BNDBLOB_ID, WRAPARR_ID = 4, 5, 6
BNDARR_COUNT = 8
BNDSTR_MAXLEN = 32
BNDBLOB_MAXLEN = 32

# Wire types (MESSAGE_SPEC §4.2): a field header is (id << 3) | wire_type.
WT_FIXLEN = 2
WT_ARRAY_UNSIGNED = 3
WT_SEQ_BEGIN = 6
WT_SEQ_END = 7
SUBTYPE_STRING = 2
SUBTYPE_BLOB = 3

# The byte a blob fixture is filled with, and the character a string is.
BLOB_FILL = 0x62  # 'b'


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


def header(fid, wire_type):
    return varint((fid << 3) | wire_type)


def fixlen_word(length, subtype):
    """§4.6: fixlen_word = (length << 3) | subtype."""
    return varint((length << 3) | subtype)


def parse_shapes(spec):
    shapes = [s.strip() for s in spec.split(",") if s.strip()]
    unknown = [s for s in shapes if s not in SHAPES]
    if unknown:
        die("--shapes: %s is not one of %s"
            % (", ".join(unknown), ", ".join(SHAPES)))
    if not shapes:
        die("--shapes selected nothing; the driver has no table to run")
    return shapes


def emit_schema(shapes) -> int:
    """Print the `refusal` message, for appending to a conformance schema."""
    print("# refusal -- the LimitExceeded-vs-InvalidMessage message (generator#416),")
    print("# printed by tests/conformance/lib/check_refusal_category.py so the ids and")
    print("# bounds the fixtures breach have exactly one definition between them. It")
    print("# must be generated with the same cap numbers and the same --shapes the run")
    print("# is given (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len).")
    print("  %s:" % MESSAGE)
    print("    payload:")
    if "array" in shapes:
        print("      dynarr: { id: %d, type: array, items: { type: u32 } }" % DYNARR_ID)
        print("      bndarr: { id: %d, type: array, items: { type: u32, count: %d } }"
              % (BNDARR_ID, BNDARR_COUNT))
    if "string" in shapes:
        print("      dynstr: { id: %d, type: string }" % DYNSTR_ID)
        print("      bndstr: { id: %d, type: string, maxlen: %d }"
              % (BNDSTR_ID, BNDSTR_MAXLEN))
    if "blob" in shapes:
        print("      dynblob: { id: %d, type: blob }" % DYNBLOB_ID)
        print("      bndblob: { id: %d, type: blob, maxlen: %d }"
              % (BNDBLOB_ID, BNDBLOB_MAXLEN))
    if "wrapper" in shapes:
        print("      wraparr: { id: %d, type: array, items: { type: string } }"
              % WRAPARR_ID)
    return 0


def uarray(fid, count):
    """A complete unsigned array of `count` ones at `fid`."""
    return header(fid, WT_ARRAY_UNSIGNED) + varint(count) + b"\x01" * count


def fixlen(fid, length, subtype, fill):
    """A complete `length`-byte fixlen payload at `fid`."""
    return header(fid, WT_FIXLEN) + fixlen_word(length, subtype) + fill * length


def string(fid, length):
    return fixlen(fid, length, SUBTYPE_STRING, b"x")


def blob(fid, length):
    return fixlen(fid, length, SUBTYPE_BLOB, bytes([BLOB_FILL]))


def wrapper_element(fid, index):
    """A count-less array at `fid` carrying one 1-byte string at `index`.

    The index is the length: the list is grown to `index + 1` slots, and that is
    the number the receiver cap has to bound (§6.2.1).
    """
    return (header(fid, WT_SEQ_BEGIN) + string(index, 1)
            + header(0, WT_SEQ_END))


def midpoint(cap, bound, flag):
    """A value strictly over the cap and strictly inside the schema bound."""
    if cap >= bound:
        die("%s %d is not below the schema's %d -- the over-cap-but-inside-the-"
            "bound row could not exist" % (flag, cap, bound))
    mid = (cap + bound) // 2
    if mid <= cap:
        die("%s %d leaves no value strictly between the cap and the schema's "
            "%d, so the over-cap-but-inside-the-bound row would sit AT the cap "
            "and assert nothing (§6.2.1)" % (flag, cap, bound))
    return mid


def build_table(shapes, cap_count, cap_len, cap_blob):
    rows = []
    if "array" in shapes or "wrapper" in shapes:
        if cap_count < 1:
            die("--max-dyn-array-count %d: a cap below 1 leaves no accepting row"
                % cap_count)
    if "array" in shapes:
        mid = midpoint(cap_count, BNDARR_COUNT, "--max-dyn-array-count")
        rows += [
            ("dynarr_over_cap", uarray(DYNARR_ID, cap_count + 1), "limit", None,
             "an unbounded array %d elements long, over the configured cap %d -- a "
             "receiver POLICY refusal, never InvalidMessage (§6.3)"
             % (cap_count + 1, cap_count)),
            ("dynarr_at_cap", uarray(DYNARR_ID, cap_count), "accept",
             ("dynarr", [1] * cap_count),
             "the same array exactly AT the cap -- pins the cap's own value"),
            ("bndarr_over_bound", uarray(BNDARR_ID, BNDARR_COUNT + 1), "invalid",
             None,
             "a schema-bounded array past its own count %d -- the schema is what "
             "refused, so InvalidMessage, and §6.3 forbids LimitExceeded for a field "
             "the schema bounds" % BNDARR_COUNT),
            ("bndarr_over_cap", uarray(BNDARR_ID, mid), "accept",
             ("bndarr", [1] * mid),
             "%d elements: over the cap %d, inside the schema's count %d -- the cap "
             "must not reach a bounded field at all (§6.2.1)"
             % (mid, cap_count, BNDARR_COUNT)),
        ]
    if "string" in shapes:
        smid = midpoint(cap_len, BNDSTR_MAXLEN, "--max-dyn-string-len")
        rows += [
            ("dynstr_over_cap", string(DYNSTR_ID, cap_len + 1), "limit", None,
             "an unbounded string %d bytes long, over the configured cap %d"
             % (cap_len + 1, cap_len)),
            ("dynstr_at_cap", string(DYNSTR_ID, cap_len), "accept",
             ("dynstr", "x" * cap_len),
             "the same string exactly AT the cap -- pins the cap's own value"),
            ("bndstr_over_bound", string(BNDSTR_ID, BNDSTR_MAXLEN + 8), "invalid",
             None,
             "a schema-bounded string past its own maxlen %d -- InvalidMessage, the "
             "declared-bound half of the pair" % BNDSTR_MAXLEN),
            ("bndstr_over_cap", string(BNDSTR_ID, smid), "accept",
             ("bndstr", "x" * smid),
             "%d bytes: over the cap %d, inside the schema's maxlen %d"
             % (smid, cap_len, BNDSTR_MAXLEN)),
        ]
    if "blob" in shapes:
        bmid = midpoint(cap_blob, BNDBLOB_MAXLEN, "--max-dyn-blob-len")
        rows += [
            ("dynblob_over_cap", blob(DYNBLOB_ID, cap_blob + 1), "limit", None,
             "an unbounded blob %d bytes long, over the configured cap %d -- the "
             "blob cap is a THIRD number, and a backend that routes it through the "
             "InvalidMessage path is invisible to the string rows"
             % (cap_blob + 1, cap_blob)),
            ("dynblob_at_cap", blob(DYNBLOB_ID, cap_blob), "accept",
             ("dynblob", [BLOB_FILL] * cap_blob),
             "the same blob exactly AT the cap -- pins the cap's own value"),
            ("bndblob_over_bound", blob(BNDBLOB_ID, BNDBLOB_MAXLEN + 8), "invalid",
             None,
             "a schema-bounded blob past its own maxlen %d -- InvalidMessage"
             % BNDBLOB_MAXLEN),
            ("bndblob_over_cap", blob(BNDBLOB_ID, bmid), "accept",
             ("bndblob", [BLOB_FILL] * bmid),
             "%d bytes: over the cap %d, inside the schema's maxlen %d"
             % (bmid, cap_blob, BNDBLOB_MAXLEN)),
        ]
    if "wrapper" in shapes:
        rows += [
            ("wraparr_over_cap", wrapper_element(WRAPARR_ID, cap_count), "limit",
             None,
             "a count-less array whose element index %d makes it %d slots, one past "
             "the cap %d. The schema declares no count here, so nothing about these "
             "bytes is invalid -- and on this shape the category is usually decided "
             "inside the corelib's collector, not by generated code (§6.2.1/§6.3)"
             % (cap_count, cap_count + 1, cap_count)),
            ("wraparr_at_cap", wrapper_element(WRAPARR_ID, cap_count - 1),
             "accept",
             ("wraparr", [""] * (cap_count - 1) + ["x"]),
             "the element one index lower: exactly %d slots, filling the cap -- the "
             "other side of the same boundary" % cap_count),
        ]
    return rows


def run(argv, cwd, data):
    proc = subprocess.run(argv, cwd=cwd, input=data,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return (proc.returncode,
            proc.stdout.decode("utf-8", "replace"),
            proc.stderr.decode("utf-8", "replace"))


def decoded_field(out, field):
    """Pull `field` out of the harness's JSON, whatever the rendering."""
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


def same_value(got, want):
    if isinstance(want, str):
        return got == want
    if not isinstance(got, list) or len(got) != len(want):
        return False
    for g, w in zip(got, want):
        if isinstance(w, str):
            if g != w:
                return False
            continue
        if (not isinstance(g, (int, float)) or isinstance(g, bool)
                or float(g) != float(w)):
            return False
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("label", nargs="?")
    ap.add_argument("--emit-schema", action="store_true")
    ap.add_argument("--message", default=MESSAGE)
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--shapes", default=",".join(SHAPES))
    ap.add_argument("--max-dyn-array-count", type=int, default=4)
    ap.add_argument("--max-dyn-string-len", type=int, default=8)
    ap.add_argument("--max-dyn-blob-len", type=int, default=8)
    ap.add_argument("--status-verb", default=None)
    ap.add_argument("--status-limit", default="LIMITEXCEEDED")
    ap.add_argument("--status-invalid", default="INVALID")
    ap.add_argument("--status-complete", default="COMPLETE")
    ap.add_argument("--limit-pattern", default=None)
    ap.add_argument("--invalid-pattern", default=None)
    ap.add_argument("--no-values", action="store_true")

    argv = sys.argv[1:]
    if "--" in argv:
        sep = argv.index("--")
        head, harness = argv[:sep], argv[sep + 1:]
    else:
        head, harness = argv, []
    args = ap.parse_args(head)
    shapes = parse_shapes(args.shapes)

    if args.emit_schema:
        return emit_schema(shapes)

    if not args.label:
        die("no label given (the suite name this run is reported under)")
    if not harness:
        die("no harness argv given (put it after `--`)")

    patterns = bool(args.limit_pattern) or bool(args.invalid_pattern)
    if patterns and not (args.limit_pattern and args.invalid_pattern):
        die("--limit-pattern and --invalid-pattern come as a pair: each refusing "
            "row asserts its own matches AND the other does not, which is the "
            "collapse §6.3 forbids")
    if not args.status_verb and not patterns:
        die("no category channel: pass --status-verb, or the "
            "--limit-pattern/--invalid-pattern pair. Exit status alone cannot "
            "tell LimitExceeded from InvalidMessage, which is the whole of "
            "CORELIB_PLAN §6.3")

    table = build_table(shapes, args.max_dyn_array_count,
                        args.max_dyn_string_len, args.max_dyn_blob_len)
    msg = [args.message] if args.message else []

    for name, wire, expect, value, why in table:
        rc, out, err = run(harness + [args.verb] + msg, args.cwd, wire)
        text = out + err

        if expect == "accept":
            if rc != 0:
                die("[%s] %s [%s] must DECODE -- %s; rc=%d:\n%s"
                    % (args.label, name, args.verb, why, rc, text.strip()))
            if args.status_verb:
                _, sout, serr = run(harness + [args.status_verb] + msg,
                                    args.cwd, wire)
                got = (sout.strip().splitlines() or [""])[0]
                if got != args.status_complete:
                    die("[%s] %s -- must decode COMPLETE, got %r%s"
                        % (args.label, name, got,
                           ("\n" + serr.strip()) if serr.strip() else ""))
            if not args.no_values:
                field, want = value
                got = decoded_field(out, field)
                if got is None:
                    die("[%s] %s -- the harness printed no %r; got:\n%s"
                        % (args.label, name, field, out.strip()))
                if not same_value(got, want):
                    die("[%s] %s decoded, but %r is %s -- want %s (%s); bytes: %s"
                        % (args.label, name, field, json.dumps(got),
                           json.dumps(want), why, wire.hex()))
            continue

        want_status = (args.status_limit if expect == "limit"
                       else args.status_invalid)
        other_status = (args.status_invalid if expect == "limit"
                        else args.status_limit)
        want_pat = args.limit_pattern if expect == "limit" else args.invalid_pattern
        other_pat = args.invalid_pattern if expect == "limit" else args.limit_pattern
        wanted = "LimitExceeded" if expect == "limit" else "InvalidMessage"
        refused = "LimitExceeded" if expect != "limit" else "InvalidMessage"

        if rc == 0:
            die("[%s] %s [%s] must be refused as %s (CORELIB_PLAN §6.3) but the "
                "decode SUCCEEDED -- %s; bytes: %s"
                % (args.label, name, args.verb, wanted, why, wire.hex()))
        if patterns:
            if not re.search(want_pat, text):
                die("[%s] %s -- refused, but not as %s: the harness must name "
                    "%r (%s) and printed:\n%s"
                    % (args.label, name, wanted, want_pat, why, text.strip()))
            if re.search(other_pat, text):
                die("[%s] %s -- reported as %s (%r matched). §6.3 keeps the two "
                    "apart: %s. Harness printed:\n%s"
                    % (args.label, name, refused, other_pat, why, text.strip()))
        if args.status_verb:
            _, sout, serr = run(harness + [args.status_verb] + msg, args.cwd,
                                wire)
            got = (sout.strip().splitlines() or [""])[0]
            if got == other_status:
                die("[%s] %s -- reported as %s. §6.3 keeps the two apart: %s"
                    % (args.label, name, refused, why))
            if got != want_status:
                die("[%s] %s -- category must be %s (%r), got %r%s"
                    % (args.label, name, wanted, want_status, got,
                       ("\n" + serr.strip()) if serr.strip() else ""))

    channel = ("--status-verb %s" % args.status_verb if args.status_verb
               else "%s/%s" % (args.limit_pattern, args.invalid_pattern))
    print("   [%s] refusal category [%s]: %d rows over %s (%d LimitExceeded, %d "
          "InvalidMessage, %d accepted) via %s"
          % (args.label, args.verb, len(table), "+".join(shapes),
             sum(1 for r in table if r[2] == "limit"),
             sum(1 for r in table if r[2] == "invalid"),
             sum(1 for r in table if r[2] == "accept"), channel))
    return 0


if __name__ == "__main__":
    sys.exit(main())
