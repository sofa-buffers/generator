#!/usr/bin/env python3
"""UTF-8 at every string position, in both directions (generator#652).

Usage:
  check_utf8_positions.py <label> [--schema PATH] [--message NAME] [--cwd DIR]
                          [--verb VERB] [--invalid-pattern REGEX]
                          [--status-verb VERB] [--no-declared-leg]
                          [--tail-field NAME] [--unknown-id N]
                          [--encode [--encode-verb VERB] [--surrogates
                           [--refusal-pattern REGEX]]]
                          -- <harness argv...>

`check_skipped_string_utf8.py` asks the "declared string with invalid bytes is
INVALID" question at one position, the top-level `somestring`. Generated code
renders the validation call once per scope (go: six `UTF8Valid` sites for
`example.yaml`, zig: seven `_takeStr` sites), so a scope that forgets it is
invisible to a root-only table. This driver asks the same question at every
position the suite schema has a string:

    somestringarray[0]          wrapper-array element, first
    somestringarray[2]          wrapper-array element, after a gap
    somestruct.nestedstring     string in a nested struct
    somestructwitharray.label   string in a struct that also holds an array
    someunion.option2           union string option
    someunionarray[1].asstring  union string option inside a wrapper array
    somemap[1].key              string in a struct inside a wrapper array

## Decode half

Each position gets INVALID rows (`ff ff`, a byte no sequence may contain, and
`e2 82`, a sequence that runs out at payload completion, §6.4.4) and a
`control_reads_back` row at the SAME position with valid bytes, which must
decode, carry the string, and keep the trailing `someu8 = 42`. Without the
control a harness pointed at the wrong id would pass every INVALID row for the
wrong reason. Callers run it on `decode` and `streamdecode`.

`--no-declared-leg` drops the INVALID rows for a footprint build with the strict
check compiled out (the control rows stay).

## Encode half (`--encode`)

  * Multi-byte round trip. A string of a 2-, 3- and two 4-byte characters
    (U+00E9, U+20AC, U+1F600 and U+10FFFF) is encoded at every position above,
    the payload is searched for the exact UTF-8 bytes with the matching
    fixlen_word (U+10FFFF is `F4 8F BF BF`), and the message is decoded back to
    the same string. A UTF-16 target that encodes each code unit separately
    (CESU-8) emits two 3-byte surrogates where one 4-byte sequence belongs, so
    the word and the bytes both differ.
  * `--surrogates` (Unicode-string targets: java, kotlin, csharp, typescript,
    dart, python). CORELIB_PLAN §6.4.1: an unpaired surrogate must be refused
    with InvalidArgument, never replaced by U+FFFD. A lone high surrogate, a
    lone low surrogate, a high surrogate at the end of the string and a reversed
    pair are encoded at the top level, and a lone high surrogate at every other
    position. A refusal is a failing exit status AND no output bytes (so no
    `EF BF BD` was written), and `--refusal-pattern` names the category. The
    round trip above is the control: a harness that cannot spell the string, or
    refuses every string, fails there first.

    Byte-container targets (c, cpp, go, zig) cannot hold a surrogate; their
    refusal of non-UTF-8 bytes is `check_invalid_utf8.py --encode`. Rust's
    `String` cannot hold invalid UTF-8 either, so rust has no refusal to test.
"""

import argparse
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import check_skipped_string_utf8 as ssu  # noqa: E402
import check_header_limits as chl  # noqa: E402

BAD_FF = ssu.BAD_FF
BAD_TRUNC = ssu.BAD_TRUNC
WT_SEQ_BEGIN = ssu.WT_SEQ_BEGIN
WT_FIXLEN = ssu.WT_FIXLEN
SEQ_END = ssu.SEQ_END
varint = ssu.varint

# 2-, 3-, 4-byte (supplementary plane) and the last code point.
MULTI = "\u00e9\u20ac\U0001F600\U0010FFFF"


def seq(fid):
    return varint((fid << 3) | WT_SEQ_BEGIN)


def word(payload):
    return varint((len(payload) << 3) | ssu.SUB_STRING)


class Position:
    """One string position: where it sits and how to wrap bytes for it."""

    def __init__(self, name, outer, parent_id, leaf_id, elem=None, field=None,
                 inner=None, json_for=None):
        self.name = name
        self.outer = outer          # top-level field name, e.g. somestruct
        self.parent_id = parent_id  # its id
        self.leaf_id = leaf_id      # the string's id in its own scope
        self.elem = elem            # wrapper-array element index, or None
        self.field = field
        self.inner = inner          # True when the element is itself a scope
        self.json_for = json_for

    def wire(self, leaf_payload):
        """Header + word + payload at this position, framed for its scope."""
        leaf = varint((self.leaf_id << 3) | WT_FIXLEN) + word(leaf_payload) + leaf_payload
        if self.elem is None and not self.inner:
            if self.outer == "somestring":
                return leaf
            return seq(self.parent_id) + leaf + SEQ_END
        if self.inner:
            # wrapper array of scopes: sequence (array) { sequence (element) { leaf } }
            return (seq(self.parent_id) + seq(self.elem) + leaf + SEQ_END
                    + SEQ_END)
        # wrapper array of plain strings: the element IS the string, id = index
        e = varint((self.elem << 3) | WT_FIXLEN) + word(leaf_payload) + leaf_payload
        return seq(self.parent_id) + e + SEQ_END

    def value_json(self, s):
        return self.json_for(s)


def positions(schema):
    def p(name, want):
        return ssu.parse_field(schema, name, want)[0]

    top = p("somestring", "string")
    arr = p("somestringarray", "array")
    st = p("somestruct", "struct")
    nested = p("nestedstring", "string")
    swa = p("somestructwitharray", "struct")
    label = p("label", "string")
    un = p("someunion", "union")
    opt2 = p("option2", "string")
    ua = p("someunionarray", "array")
    asstr = p("asstring", "string")
    mp = p("somemap", "array")
    key = p("key", "string")
    return [
        Position("somestring", "somestring", top, top,
                 json_for=lambda s: {"somestring": s}),
        Position("somestringarray[0]", "somestringarray", arr, 0, elem=0,
                 json_for=lambda s: {"somestringarray": [s]}),
        Position("somestringarray[2]", "somestringarray", arr, 0, elem=2,
                 json_for=lambda s: {"somestringarray": ["a", "b", s]}),
        Position("somestruct.nestedstring", "somestruct", st, nested,
                 json_for=lambda s: {"somestruct": {"nestedstring": s}}),
        Position("somestructwitharray.label", "somestructwitharray", swa, label,
                 json_for=lambda s: {"somestructwitharray": {"label": s}}),
        Position("someunion.option2", "someunion", un, opt2,
                 json_for=lambda s: {"someunion": {"option2": s}}),
        Position("someunionarray[1].asstring", "someunionarray", ua, asstr,
                 elem=1, inner=True,
                 json_for=lambda s: {"someunionarray": [{"asint": 1}, {"asstring": s}]}),
        Position("somemap[1].key", "somemap", mp, key, elem=1, inner=True,
                 json_for=lambda s: {"somemap": [{"key": "a", "value": 1},
                                                {"key": s, "value": 2}]}),
    ]


def contains(v, s):
    if isinstance(v, str):
        return v == s
    if isinstance(v, list):
        return any(contains(x, s) for x in v)
    if isinstance(v, dict):
        return any(contains(x, s) for x in v.values())
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("label")
    ap.add_argument("--schema", default=None)
    ap.add_argument("--message", default="myfirstmessage")
    ap.add_argument("--tail-field", default="someu8")
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--invalid-pattern", default=None)
    ap.add_argument("--status-verb", default=None)
    ap.add_argument("--no-declared-leg", action="store_true")
    ap.add_argument("--encode", action="store_true")
    ap.add_argument("--encode-verb", default="encode")
    ap.add_argument("--surrogates", action="store_true")
    ap.add_argument("--refusal-pattern", default=None)
    argv = sys.argv[1:]
    if "--" not in argv:
        ssu.die("no harness argv given (put it after `--`)")
    sep = argv.index("--")
    args = ap.parse_args(argv[:sep])
    harness = argv[sep + 1:]
    if not harness:
        ssu.die("no harness argv given (put it after `--`)")
    if args.surrogates and not args.encode:
        ssu.die("--surrogates is part of the encode half; pass --encode")

    root = os.path.dirname(os.path.dirname(os.path.dirname(
        os.path.dirname(os.path.abspath(__file__)))))
    schema = args.schema or os.path.join(root, "examples", "messages",
                                         "example.yaml")
    pos = positions(schema)
    tail_id, tail_default, _ = ssu.parse_field(schema, args.tail_field, "u8")
    if tail_default == ssu.TAIL_VALUE:
        ssu.die("the continuation control value is the declared default of %s"
                % args.tail_field)
    tail = varint((tail_id << 3) | ssu.WT_UNSIGNED) + varint(ssu.TAIL_VALUE)
    msg = [args.message] if args.message else []

    n_inv = n_ok = 0
    for ps in pos:
        rows = [("declared_ff", BAD_FF, "invalid"),
                ("declared_truncated_seq", BAD_TRUNC, "invalid"),
                ("control_reads_back", b"hi", "read")]
        for rname, payload, expect in rows:
            if expect == "invalid" and args.no_declared_leg:
                continue
            wire = ps.wire(payload) + tail
            rc, out, err = ssu.run(harness + [args.verb] + msg, args.cwd, wire)
            tag = "%s@%s" % (rname, ps.name)
            if expect == "invalid":
                if rc == 0:
                    ssu.die("[%s] %s [%s] must be INVALID (CORELIB_PLAN §6.4.4) "
                            "but the decode SUCCEEDED; bytes: %s"
                            % (args.label, tag, args.verb, wire.hex()))
                if args.invalid_pattern and not re.search(args.invalid_pattern,
                                                          out + err):
                    ssu.die("[%s] %s -- rejected, but not as InvalidMessage: "
                            "the harness must name %r and printed:\n%s"
                            % (args.label, tag, args.invalid_pattern,
                               (out + err).strip()))
                if args.status_verb:
                    _, sout, serr = ssu.run(harness + [args.status_verb] + msg,
                                            args.cwd, wire)
                    got = (sout.strip().splitlines() or [""])[0]
                    if got != "INVALID":
                        ssu.die("[%s] %s -- category must be INVALID, got %r%s"
                                % (args.label, tag, got,
                                   ("\n" + serr.strip()) if serr.strip() else ""))
                n_inv += 1
                continue
            if rc != 0:
                ssu.die("[%s] %s [%s] must decode (valid bytes at the same "
                        "position, so the INVALID rows prove something); rc=%d:\n%s"
                        % (args.label, tag, args.verb, rc, (out + err).strip()))
            obj = ssu.decoded(out)
            if obj is None or ssu.TAIL_VALUE != obj.get(args.tail_field):
                ssu.die("[%s] %s -- the trailing %s = %d was not read back; got:\n%s"
                        % (args.label, tag, args.tail_field, ssu.TAIL_VALUE,
                           out.strip()))
            if not contains(obj.get(ps.outer), "hi"):
                ssu.die("[%s] %s -- decoded, but %s does not carry \"hi\"; got:\n%s"
                        % (args.label, tag, ps.outer, out.strip()))
            n_ok += 1

    n_rt = n_sur = 0
    if args.encode:
        utf8 = MULTI.encode("utf-8")
        assert utf8.endswith(b"\xf4\x8f\xbf\xbf")
        want = word(utf8) + utf8
        for ps in pos:
            doc = json.dumps(ps.value_json(MULTI), ensure_ascii=False).encode("utf-8")
            rc, out, err = chl.spawn_raw(harness + [args.encode_verb] + msg, args.cwd, doc)
            if rc != 0 or want not in out:
                ssu.die("[%s] %s: %r must encode with fixlen_word %s and the "
                        "UTF-8 bytes %s (a CESU-8 or replacing encoder differs); "
                        "rc=%d, output %s\n%s"
                        % (args.label, ps.name, MULTI, word(utf8).hex(),
                           utf8.hex(), rc, out.hex(),
                           err.decode("utf-8", "replace").strip()))
            rc, dout, derr = ssu.run(harness + ["decode"] + msg, args.cwd, out)
            obj = ssu.decoded(dout)
            if rc != 0 or obj is None or not contains(obj.get(ps.outer), MULTI):
                ssu.die("[%s] %s: the encoded %r did not decode back to the "
                        "same string; rc=%d:\n%s" % (args.label, ps.name, MULTI,
                                                    rc, (dout + derr).strip()))
            n_rt += 1

        if args.surrogates:
            top = pos[0]
            for sname, text in (("lone_high", "a\\ud800b"),
                                ("lone_low", "a\\udc00b"),
                                ("trailing_high", "ab\\ud800"),
                                ("reversed_pair", "a\\udc00\\ud800b")):
                doc = ('{"somestring":"%s"}' % text).encode("ascii")
                n_sur += refuse(args, harness, msg, "somestring/" + sname, doc)
            for ps in pos[1:]:
                js = json.dumps(ps.value_json("@"), ensure_ascii=True)
                doc = js.replace("@", "a\\ud800b").encode("ascii")
                n_sur += refuse(args, harness, msg, ps.name + "/lone_high", doc)

    print("   [%s] UTF-8 positions [%s]: %d positions, %d INVALID + %d read%s"
          % (args.label, args.verb, len(pos), n_inv, n_ok,
             (", %d multi-byte round trips, %d surrogate refusals" % (n_rt, n_sur))
             if args.encode else ""))


def refuse(args, harness, msg, what, doc):
    rc, out, err = chl.spawn_raw(harness + [args.encode_verb] + msg, args.cwd, doc)
    text = (out + err).decode("utf-8", "replace")
    if rc == 0 or out:
        ssu.die("[%s] %s: an unpaired surrogate must be refused (CORELIB_PLAN "
                "§6.4.1, InvalidArgument) with no output; rc=%d, %d bytes "
                "written (%s)" % (args.label, what, rc, len(out), out.hex()))
    if b"\xef\xbf\xbd" in out:
        ssu.die("[%s] %s: U+FFFD written" % (args.label, what))
    if args.refusal_pattern and not re.search(args.refusal_pattern, text):
        ssu.die("[%s] %s: refused, but not as an invalid argument (%r):\n%s"
                % (args.label, what, args.refusal_pattern, text.strip()))
    return 1


if __name__ == "__main__":
    main()
