#!/usr/bin/env python3
r"""Drive the shared `invalid_utf8` table (generator#651).

Usage:
  check_invalid_utf8.py --emit-schema
  check_invalid_utf8.py <test_vectors.json> <label> [--cwd DIR] [--verb VERB]
                        (--status-verb VERB [--status-invalid NAME] ...
                         | --invalid-pattern REGEX --limit-pattern REGEX)
                        [--encode [--encode-verb VERB] [--refusal-pattern REGEX]]
                        -- <harness argv...>

CORELIB_PLAN §6.4: a `string` whose bytes are not valid UTF-8 is INVALID where it is
read, and a byte-container target must refuse to WRITE one. Every row of the vector
file's `invalid_utf8` block carries both halves:

  * `serialized_hex` is decoded into a message with one `string` at id 0 and the
    answer must be INVALID -- not INCOMPLETE, not a decoded message;
  * with `--encode`, `string_hex` is encoded and the harness must refuse, by a
    failing exit status or by writing no bytes (a message holding a non-empty
    string cannot encode to nothing). Only the
    targets whose string is a byte container can be handed such a string (C, C++,
    Go, Zig); the others hold text, so the same bytes cannot reach their encoder.

The rows are overlong forms, surrogates, out-of-range lead bytes, a bare
continuation byte and truncated sequences. All of them are `requires: fixlen`.

## The strict build

`SOFAB_STRICT_UTF8` is the corelib's switch for the check. corelib-c-cpp (C, C++
over c-cpp) defaults it OFF as a footprint profile (§6.4.2), and the target's CI
must still build and test the check-ON configuration, so those suites run this
driver against a strict-built harness. Every other corelib defaults it ON.

## The message

`utf8str` has `s: string maxlen 16` at id 0, bounded so the footprint profile can
declare it; the longest row is four bytes.

## Spelling an invalid string

The encode input is JSON whose string holds the row's raw bytes, which no JSON
text can spell as characters. The harness's reader must pass them through; a
harness whose JSON front door cannot (Go's decoder substitutes U+FFFD) takes the
`\xNN` escape instead (`--encode-spelling escape`).

The category channel is `check_header_limits.py`'s: `--status-verb` or the two
patterns.
"""

import argparse
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import check_header_limits as chl  # noqa: E402

MESSAGE = "utf8str"
MAXLEN = 16


def emit_schema():
    print("# utf8str -- the invalid_utf8 message (generator#651), printed by")
    print("# tests/conformance/lib/check_invalid_utf8.py.")
    print(f"  {MESSAGE}: " + json.dumps(
        {"payload": {"s": {"id": 0, "type": "string", "maxlen": MAXLEN}}},
        separators=(", ", ": ")))
    return 0


def spell(raw, how):
    if how == "escape":
        body = b"".join(b"\\x%02x" % c for c in raw)
    else:
        body = raw
    return b'{"s":"' + body + b'"}'


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("vectors", nargs="?")
    ap.add_argument("label", nargs="?")
    ap.add_argument("--emit-schema", action="store_true")
    chl.add_channel_args(ap)
    ap.add_argument("--encode", action="store_true")
    ap.add_argument("--encode-verb", default="encode")
    ap.add_argument("--encode-spelling", choices=("raw", "escape"), default="raw")
    ap.add_argument("--refusal-pattern", default=None)
    argv = sys.argv[1:]
    harness = []
    if "--" in argv:
        sep = argv.index("--")
        argv, harness = argv[:sep], argv[sep + 1:]
    args = ap.parse_args(argv)
    if args.emit_schema:
        return emit_schema()
    if not args.vectors or not args.label or not harness:
        chl.die("need <test_vectors.json> <label> and a harness argv after `--`")
    chl.need_channel(args)

    with open(args.vectors) as fh:
        rows = json.load(fh).get("invalid_utf8")
    if not rows:
        chl.die(f"{args.vectors} has no `invalid_utf8` block")

    for r in rows:
        if r["decode_outcome"] != "invalid" or r["encode_outcome"] != "invalid_argument":
            chl.die(f"row {r['name']}: outcomes {r['decode_outcome']}/{r['encode_outcome']} "
                    f"are not the ones this driver asserts")
        if r["id"] != 0:
            chl.die(f"row {r['name']}: field id {r['id']}; utf8str declares id 0")
        got, text = chl.verdict(args, harness, MESSAGE, bytes.fromhex(r["serialized_hex"]))
        if got != "invalid":
            chl.die(f"[{args.label}] {r['name']}: {r['serialized_hex']} must decode INVALID "
                    f"under a strict build, got no INVALID verdict ({got})\n{text.strip()}")

    refused = by_status = 0
    if args.encode:
        # THE CONTROL: a valid string in the same spelling must encode, to exactly
        # header + fixlen_word + bytes, so a harness that refuses every string it is
        # handed (or cannot read the spelling) does not pass the rows below.
        ok = "h\u00e9llo \u20ac \U0001F600".encode()
        rc, out, err = chl.spawn_raw(harness + [args.encode_verb, MESSAGE], args.cwd,
                                 spell(ok, args.encode_spelling))
        want = b"\x02" + chl.rc_varint((len(ok) << 3) | 2) + ok
        if rc != 0 or out != want:
            chl.die(f"[{args.label}] control: a valid UTF-8 string must encode to "
                    f"{want.hex()}, got rc {rc} {out.hex()}\n{err.decode(errors='replace').strip()}")
        for r in rows:
            raw = bytes.fromhex(r["string_hex"])
            rc, out, err = chl.spawn_raw(harness + [args.encode_verb, MESSAGE], args.cwd,
                                     spell(raw, args.encode_spelling))
            text = (out + err).decode('utf-8', 'replace')
            # A refusal is a failing exit status, or no bytes at all: a message
            # holding a non-empty string cannot encode to nothing, and the C++
            # `encode()` has no error channel beyond the empty vector it returns.
            if rc == 0 and out:
                chl.die(f"[{args.label}] {r['name']}: encoding {r['string_hex']} must be "
                        f"refused (invalid argument), the harness exited 0 with "
                        f"{len(out)} bytes of output")
            if rc != 0:
                by_status += 1
            if args.refusal_pattern and not re.search(args.refusal_pattern, text):
                chl.die(f"[{args.label}] {r['name']}: refused, but not as an invalid "
                        f"argument ({args.refusal_pattern!r}):\n{text.strip()}")
            refused += 1

    print(f"   [{args.label}] invalid_utf8: {len(rows)} rows INVALID (decode [{args.status_verb or args.verb}])"
          + (f", {refused} refused (encode; {by_status} by exit status, {refused - by_status} by empty output)" if args.encode else ", encode not run (target's strings are not byte containers)"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
