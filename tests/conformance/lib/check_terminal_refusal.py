#!/usr/bin/env python3
"""A refusal is terminal: asking the decoder again repeats it (generator#647).

Usage:
  check_terminal_refusal.py --emit-schema [--bounded]
  check_terminal_refusal.py <label> [--bounded] [--message NAME] [--cwd DIR] [--verb VERB]
                            [--marker NAME[:INVALID:LIMIT]]...
                            --invalid-name NAME --limit-name NAME
                            [--max-dyn-array-count N] [--max-dyn-string-len N]
                            -- <harness argv...>

CORELIB_PLAN §5.2: an `INVALID` outcome is terminal. §6.3: `LimitExceeded` is "a
terminal, receiver-local policy rejection". After either verdict the decoder must
refuse AGAIN, under the same category, and never hand back a message -- whether the
caller re-feeds an empty chunk or asks for the result. A decoder that forgets it
refused is a decoder that can answer COMPLETE for bytes it rejected.

## Why a shared driver

Every suite needs the same table: a refusal the CORELIB raises, a refusal a schema
bound raises, and a refusal a receiver cap raises. Which layer holds the latch
differs per backend (the corelib in most, sticky flags in generated code in some),
which is exactly why the property is asserted black-box, on the harness, and why the
table is not re-typed per language. Only the RENDERING varies: what the verdict is
called (`INVALID_MSG`, `InvalidMessage`, `invalid`, ...) and which marker carries the
second answer, so both are arguments.

The driver prints its OWN schema (`--emit-schema`, the check_refusal_category.py
idiom) so the ids and bounds the fixtures breach have one definition. The cap numbers
must be the ones the project was generated with (`max_dyn_array_count`,
`max_dyn_string_len`); the two over-cap rows breach exactly one past them.

## Profiles with no receiver caps

A footprint profile (c, cpp over corelib-c-cpp, rust over rs-no-std) has no receiver
cap -- every field is statically bounded and `max_dyn_*` is not accepted -- so it has
no LimitExceeded to repeat. `--bounded` (identically on `--emit-schema` and on the
run) drops the two unbounded fields and the two over-cap rows and keeps the three
INVALID ones. A suite uses it only where the profile cannot take a cap.

## The harness contract

`<harness> <verb> <message>` reads the bytes on stdin and, when the decode is
refused, exits non-zero and writes a second-verdict marker to stdout or stderr after
asking again:

    [finish=X]   finish() (or feeding an empty chunk, where there is no finish)
    [refeed=X]   an empty feed

X is the category the second call answered, or `none` / `null` when it returned a
message from a decoder that had refused one. The driver requires the refusal, then
requires every named marker to read the category the fixture breached. A harness
that continues past its first error and reports a later, different answer, or one
that forgets and says `none`, fails here.

## The table

    corelib_varint_overflow  a varint past the 64-bit bound     INVALID (corelib)
    schema_overcount         9 elements, schema count 8         INVALID (schema)
    schema_overmaxlen        40 bytes, schema maxlen 32         INVALID (schema)
    schema_overmaxlen_eof    the same length word, input ends   INVALID (schema)
    cap_array_over           cap + 1 elements, no schema count  LimitExceeded
    cap_string_over          cap + 1 bytes, no schema maxlen    LimitExceeded
"""

import argparse
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import check_refusal_category as rc  # noqa: E402  (shared wire builders)

MESSAGE = "terminal"

SCALAR_ID, BNDARR_ID, DYNARR_ID, DYNSTR_ID, BNDSTR_ID = 0, 1, 2, 3, 4
BNDARR_COUNT = 8
BNDSTR_MAXLEN = 32


def die(msg):
    print("FAIL: " + msg)
    sys.exit(1)


def emit_schema(bounded) -> int:
    print("# terminal -- the 'a refusal is terminal' message (generator#647), printed by")
    print("# tests/conformance/lib/check_terminal_refusal.py so the ids and bounds its")
    print("# fixtures breach have one definition. Generate it with the cap numbers the run")
    print("# is given (max_dyn_array_count / max_dyn_string_len), or with --bounded for a")
    print("# profile that takes no cap.")
    print("  %s:" % MESSAGE)
    print("    payload:")
    print("      scalar: { id: %d, type: u32 }" % SCALAR_ID)
    print("      bndarr: { id: %d, type: array, items: { type: u32, count: %d } }"
          % (BNDARR_ID, BNDARR_COUNT))
    if not bounded:
        print("      dynarr: { id: %d, type: array, items: { type: u32 } }" % DYNARR_ID)
        print("      dynstr: { id: %d, type: string }" % DYNSTR_ID)
    print("      bndstr: { id: %d, type: string, maxlen: %d }" % (BNDSTR_ID, BNDSTR_MAXLEN))
    return 0


def build_table(cap_count, cap_len, bounded):
    # Header 0x00 = id 0, varint wire type; ten continuation bytes and an eleventh
    # run past the 64-bit bound (§4.1), which the corelib refuses by itself.
    overflow = rc.header(SCALAR_ID, 0) + b"\xff" * 10 + b"\x01"
    rows = [
        ("corelib_varint_overflow", overflow, "invalid"),
        ("schema_overcount", rc.uarray(BNDARR_ID, BNDARR_COUNT + 1), "invalid"),
        ("schema_overmaxlen", rc.string(BNDSTR_ID, BNDSTR_MAXLEN + 8), "invalid"),
        # The refusal is taken at the length word, before a payload byte exists, so
        # the stream ends right behind it: INVALID, not INCOMPLETE, and still terminal.
        ("schema_overmaxlen_eof",
         rc.header(BNDSTR_ID, rc.WT_FIXLEN)
         + rc.fixlen_word(BNDSTR_MAXLEN + 8, rc.SUBTYPE_STRING), "invalid"),
    ]
    if not bounded:
        rows += [
            ("cap_array_over", rc.uarray(DYNARR_ID, cap_count + 1), "limit"),
            ("cap_string_over", rc.string(DYNSTR_ID, cap_len + 1), "limit"),
        ]
    return rows


def parse_marker(spec, invalid, limit):
    parts = spec.split(":")
    if len(parts) == 1:
        return parts[0], invalid, limit
    if len(parts) == 3:
        return parts[0], parts[1], parts[2]
    die("--marker %r: want NAME or NAME:INVALID:LIMIT" % spec)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("label", nargs="?")
    ap.add_argument("--emit-schema", action="store_true")
    ap.add_argument("--bounded", action="store_true")
    ap.add_argument("--message", default=MESSAGE)
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="streamdecode")
    ap.add_argument("--marker", action="append", default=[])
    ap.add_argument("--invalid-name", default=None)
    ap.add_argument("--limit-name", default=None)
    ap.add_argument("--max-dyn-array-count", type=int, default=4)
    ap.add_argument("--max-dyn-string-len", type=int, default=8)

    argv = sys.argv[1:]
    if "--" in argv:
        sep = argv.index("--")
        head, harness = argv[:sep], argv[sep + 1:]
    else:
        head, harness = argv, []
    args = ap.parse_args(head)

    if args.emit_schema:
        return emit_schema(args.bounded)
    if not args.label:
        die("no label given (the suite name this run is reported under)")
    if not harness:
        die("no harness argv given (put it after `--`)")
    if not args.marker:
        die("no --marker: the driver reads the second verdict from the harness, and "
            "an exit status alone cannot say whether the decoder refused AGAIN")
    markers = []
    for spec in args.marker:
        name, inv, lim = parse_marker(spec, args.invalid_name, args.limit_name)
        if inv is None or (lim is None and not args.bounded):
            die("--marker %s needs --invalid-name and --limit-name (or the "
                "NAME:INVALID:LIMIT form)" % name)
        markers.append((name, inv, lim))
    if args.max_dyn_array_count < 1 or args.max_dyn_string_len < 1:
        die("a cap below 1 leaves no over-cap fixture")

    table = build_table(args.max_dyn_array_count, args.max_dyn_string_len,
                        args.bounded)
    for name, wire, kind in table:
        proc = subprocess.run(harness + [args.verb, args.message], cwd=args.cwd,
                              input=wire, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE)
        text = (proc.stdout.decode("utf-8", "replace")
                + proc.stderr.decode("utf-8", "replace"))
        if proc.returncode == 0:
            die("[%s] %s must be refused by the streaming decoder; bytes: %s"
                % (args.label, name, wire.hex()))
        for marker, inv, lim in markers:
            want = inv if kind == "invalid" else lim
            m = re.search(r"\[%s=([^\]]*)\]" % re.escape(marker), text)
            if not m:
                die("[%s] %s -- the harness printed no [%s=...] marker after the "
                    "refusal; it cannot report a second verdict:\n%s"
                    % (args.label, name, marker, text.strip()))
            if m.group(1) != want:
                die("[%s] %s -- a refusal is terminal (CORELIB_PLAN §5.2/§6.3): "
                    "after it, [%s] must name %r, got %r%s\n%s"
                    % (args.label, name, marker, want, m.group(1),
                       " (a message was handed back after a refusal)"
                       if m.group(1) in ("none", "RETURNED") else "",
                       text.strip()))

    print("   [%s] a refusal is terminal [%s]: %d fixtures (%d INVALID, %d "
          "LimitExceeded), markers %s"
          % (args.label, args.verb, len(table),
             sum(1 for r in table if r[2] == "invalid"),
             sum(1 for r in table if r[2] == "limit"),
             ", ".join(m[0] for m in markers)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
