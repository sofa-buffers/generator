#!/usr/bin/env python3
"""A decode into a reused destination reads exactly what a fresh decode reads.

Usage:
  check_reused_destination.py --emit-schema
  check_reused_destination.py --self-test
  check_reused_destination.py <label> [--msg NAME --fixture NAME=JSON ...]
                              [--invalid-tail HEX] [--verb VERB] [--cwd DIR]
                              -- <harness argv...>

## The rule

MESSAGE_SPEC §2 omits every field equal to its default, and an ABSENT field reads
as its declared default. Absence fires nothing on the decode side -- no callback,
no store -- so a decoder that is handed a destination which still holds an OLDER
message must put it back to the declared defaults before the first byte is
applied. Otherwise every field the new message omits keeps the older message's
value, and the result differs from a fresh decode of the very same bytes.

Every surface that decodes into a caller-owned destination owes that contract
(the TypeScript Decoder's `out`, C# `TryDecode(data, msg)`, C++ `try_decode`,
Java/Kotlin/Dart `tryDecode(data, out)`): the destination reads as if it had been
constructed for this message alone. That covers a field absent from the second
message, an array that is now shorter, a union that now holds another option or
its default one, a blob that falls back to its default, and the empty payload,
where every field is absent at once. On a REFUSED second message the destination
holds the fields decoded before the error, and never a value of the message it
held before.

A fresh decode cannot get this wrong: every field starts at its initializer. A
reused destination can, one field kind at a time -- a reset that forgets a nested
struct, a list, a union or an array with a declared default leaves that field
alone, and a sparse second message never overwrites it.

## The harness verb

    <harness> reusedecode <message> <hex of the message it held>   (stdin: the next one)

decodes the hex into a destination with the plain decode, then decodes stdin
into that SAME destination through the surface that takes one. It prints three
lines and exits 0, a refusal included:

    1  the verdict: COMPLETE, INCOMPLETE, or the refusal's code
    2  the destination as JSON, the same rendering `decode` prints
    3  the destination re-encoded, as lowercase hex ("-" if the encode refused)

A harness whose language has a second such surface can name it with `--verb`.
Besides it the driver uses only `decode` (a fresh destination) and, for fixtures,
`encode`.

## The inputs

Without `--fixture` the driver runs against its OWN schema (`--emit-schema`,
message `reuse`) and forges every input here from the wire format, so nothing it
compares against comes from the backend under test. The schema has a field of
each shape a reset has to put back: a scalar with a non-zero default, a 64-bit
integer, a boolean defaulting to true, an fp32 (held as a signaling NaN with
payload bits, whose wire word some targets keep beside the value), a string with
a default, a blob without and one with a default, a native array with a default
and an fp32 array, a string array, a struct, a union whose default option is a
scalar, a union whose default option is a struct and which has an array option,
an array of structs without and one with a member default, a deprecated field
with a default, a struct nested in a struct, a matrix, an array of unions, an
enum and a bitfield with a flag above bit 31. Every array and string is bounded,
so the statically sized profiles build it too. The images:

    full      every field away from its default
    sparse    scalars, string, blobs, enum and bitfield absent; every array
              shorter; a struct with one field absent; both unions on another
              option
    defaults  one field; everything else, the unions included, at its default
    held_default_option
              both unions on their DEFAULT option at a non-default value (written,
              §4.2), and the defaulted blob present and empty
    other     the second union on its third option and both struct arrays
              LONGER than in full, with elements at their member default
    empty     the empty message: zero bytes

With `--fixture NAME=JSON` (repeatable; `NAME=@file` reads the JSON from a file)
the inputs are the harness's own `encode` of each fixture, plus the empty
message, against the message `--msg` names -- the backend's own schema.

## The table

    control   every input decoded into a destination that held the empty message
              re-encodes to itself and reads as a fresh `decode` of it. For the
              forged images that proves each one canonical for this backend; until
              it holds, the rows below cannot speak and are not run.
    distinct  every input decodes to a different message. Otherwise a pair
              compares a message with itself, and a destination that kept the
              older message would pass.
    pair      for every ordered pair (held, next), held == next included: the
              verdict is COMPLETE, the JSON equals a fresh `decode` of next
              (compared as data), and the re-encode is byte-identical to next
    refused   the same pairs with each malformed tail appended to next: the
              verdict is a refusal, and the destination holds exactly what was
              decoded before the error -- the JSON of a fresh decode of next
              WITHOUT the tail, and next's bytes on re-encode

The tails are malformed in every schema, by MESSAGE_SPEC §4.1, a varint longer
than ten bytes being INVALID whatever it encodes: an 11-byte field header (ten
0x80 bytes, then 0x01), and a header of an undeclared id followed by an 11-byte
value. `--invalid-tail` replaces both with one of its own.

`--self-test` checks the hand-written images against known bytes and runs the
table against a model codec, once correct and once with a reset that forgets one
field (or all of them); the table must pass the first and fail every other.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import struct
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import harness_dialect  # noqa: E402
import json_equal  # noqa: E402

MESSAGE = "reuse"
VERB = "reusedecode"
OK = ("COMPLETE", "INCOMPLETE")

# Wire types (CORELIB_PLAN §4.3).
WT_UNSIGNED, WT_SIGNED, WT_FIXLEN, WT_ARR_U, WT_ARR_S, WT_ARR_F, WT_SEQ = 0, 1, 2, 3, 4, 5, 6
END = b"\x07"                                  # sequence end marker (§4.9)
SUB_FP32, SUB_STRING, SUB_BLOB = 0, 2, 3       # fixlen subtypes (§4.6)
FIXLEN_FP32 = 0x20                             # fixlen word of an fp32 array (§4.8)

# Field ids of `reuse`.
(N, I, OKF, F, S, B, BD, A, FA, SA, ST, U, RA, OLD, NS, MX, UA, E, BF,
 UD, RD) = range(21)
# An id the schema does not declare, for the second malformed tail.
UNKNOWN = 31

SCHEMA = f"""\
# Decode into a reused destination (MESSAGE_SPEC §2), printed by
# tests/conformance/lib/check_reused_destination.py so the schema and the bytes
# it forges against it have one definition between them.
  {MESSAGE}:
    payload:
      n:  {{ id: {N}, type: u32, default: 5 }}
      i:  {{ id: {I}, type: i64 }}
      ok: {{ id: {OKF}, type: boolean, default: true }}
      f:  {{ id: {F}, type: fp32 }}
      s:  {{ id: {S}, type: string, maxlen: 16, default: "dflt" }}
      b:  {{ id: {B}, type: blob, maxlen: 8 }}
      bd: {{ id: {BD}, type: blob, maxlen: 8, default: "AQI=" }}
      a:  {{ id: {A}, type: array, items: {{ type: u16, count: 8 }}, default: [1, 2] }}
      fa: {{ id: {FA}, type: array, items: {{ type: fp32, count: 4 }} }}
      sa: {{ id: {SA}, type: array, items: {{ type: string, count: 4, maxlen: 8 }} }}
      st:
        id: {ST}
        type: struct
        fields:
          x: {{ id: 0, type: i32, default: -3 }}
          t: {{ id: 1, type: string, maxlen: 8 }}
      u:
        id: {U}
        type: union
        default_id: 0
        oneof:
          p: {{ id: 0, type: u8 }}
          q: {{ id: 1, type: string, maxlen: 8 }}
          r: {{ id: 2, type: struct, fields: {{ y: {{ id: 0, type: u8 }} }} }}
      ra: {{ id: {RA}, type: array, items: {{ type: struct, count: 4, fields: {{ z: {{ id: 0, type: u8 }} }} }} }}
      old: {{ id: {OLD}, type: u32, default: 9, deprecated: true }}
      ns:
        id: {NS}
        type: struct
        fields:
          w: {{ id: 0, type: u8, default: 4 }}
          in:
            id: 1
            type: struct
            fields:
              v: {{ id: 0, type: string, maxlen: 8, default: "iv" }}
              k: {{ id: 1, type: u16 }}
      mx: {{ id: {MX}, type: array, items: {{ type: array, count: 3, items: {{ type: u8, count: 4 }} }} }}
      ua:
        id: {UA}
        type: array
        items:
          type: union
          count: 3
          default_id: 0
          oneof:
            m: {{ id: 0, type: i32 }}
            t: {{ id: 1, type: string, maxlen: 4 }}
      e:  {{ id: {E}, type: enum, enum: {{ A: 0, B: 1, C: 2 }}, default: 1 }}
      bf: {{ id: {BF}, type: bitfield, bits: {{ X: {{ pos: 0, default: true }}, Y: {{ pos: 1 }}, H: {{ pos: 40 }} }} }}
      ud:
        id: {UD}
        type: union
        default_id: 0
        oneof:
          so: {{ id: 0, type: struct, fields: {{ x: {{ id: 0, type: u8, default: 3 }}, y: {{ id: 1, type: string, maxlen: 8 }} }} }}
          xs: {{ id: 1, type: array, items: {{ type: u16, count: 4 }} }}
          no: {{ id: 2, type: i32 }}
      rd: {{ id: {RD}, type: array, items: {{ type: struct, count: 3, fields: {{ x: {{ id: 0, type: u8, default: 3 }} }} }} }}
"""


# --- the wire image ---------------------------------------------------------

def varint(n: int) -> bytes:
    assert n >= 0
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def zigzag(n: int) -> int:
    return (-n << 1) - 1 if n < 0 else n << 1


def header(fid: int, wt: int) -> bytes:
    return varint((fid << 3) | wt)


def unsigned(fid: int, n: int) -> bytes:
    return header(fid, WT_UNSIGNED) + varint(n)


def signed(fid: int, n: int) -> bytes:
    return header(fid, WT_SIGNED) + varint(zigzag(n))


def fixlen(fid: int, sub: int, payload: bytes) -> bytes:
    return header(fid, WT_FIXLEN) + varint((len(payload) << 3) | sub) + payload


def string(fid: int, s: str) -> bytes:
    return fixlen(fid, SUB_STRING, s.encode())


def blob(fid: int, b: bytes) -> bytes:
    return fixlen(fid, SUB_BLOB, b)


# A signaling fp32 NaN with payload bits (0x7fa00001: quiet bit clear), which a
# double-only target can only re-encode from the wire word it kept (§4.6).
SNAN32 = bytes.fromhex("0100a07f")


def uarray(fid: int, values) -> bytes:
    return header(fid, WT_ARR_U) + varint(len(values)) + b"".join(varint(v) for v in values)


def f32array(fid: int, values) -> bytes:
    return (header(fid, WT_ARR_F) + varint(len(values)) + varint(FIXLEN_FP32)
            + b"".join(struct.pack("<f", v) for v in values))


def seq(fid: int, *parts: bytes) -> bytes:
    return header(fid, WT_SEQ) + b"".join(parts) + END


FULL = b"".join([
    unsigned(N, 77),
    signed(I, -1234567890123),
    unsigned(OKF, 0),
    fixlen(F, SUB_FP32, SNAN32),
    string(S, "full"),
    blob(B, b"\x09\x08\x07"),
    blob(BD, b"\x05"),
    uarray(A, [10, 11, 12, 13]),
    f32array(FA, [1.0, 2.0, 3.0]),
    seq(SA, string(0, "aa"), string(1, "bb"), string(2, "cc")),
    seq(ST, signed(0, 42), string(1, "in")),
    seq(U, seq(2, unsigned(0, 9))),
    seq(RA, seq(0, unsigned(0, 1)), seq(1, unsigned(0, 2)), seq(2, unsigned(0, 3))),
    unsigned(OLD, 99),
    seq(NS, unsigned(0, 8), seq(1, string(0, "deep"), unsigned(1, 300))),
    seq(MX, uarray(0, [1, 2, 3]), uarray(1, [4]), uarray(2, [5, 6])),
    seq(UA, seq(0, signed(0, -7)), seq(1, string(1, "ut")), seq(2, signed(0, 8))),
    signed(E, 2),
    unsigned(BF, (1 << 40) | 2),
    seq(UD, seq(0, unsigned(0, 77), string(1, "hey"))),
    seq(RD, seq(0, unsigned(0, 5)), seq(1, unsigned(0, 6))),
])

# The union held on another option is held, so written (§4.2).
SPARSE = b"".join([
    uarray(A, [3]),
    f32array(FA, [4.0]),
    seq(SA, string(0, "z")),
    seq(ST, string(1, "sp")),
    seq(U, string(1, "q")),
    seq(RA, seq(0, unsigned(0, 5))),
    seq(NS, seq(1, unsigned(1, 3))),
    seq(MX, uarray(0, [7])),
    seq(UA, seq(0, string(1, "v"))),
    seq(UD, uarray(1, [4, 5, 6])),
    seq(RD, seq(0, unsigned(0, 9))),
])

DEFAULTS = unsigned(N, 6)

# A reset that put a union back on its default option but kept that option's old
# value would still differ from a fresh decode of a message that omits it.
HELD_DEFAULT_OPTION = b"".join([
    unsigned(N, 77),
    blob(BD, b""),
    seq(U, unsigned(0, 42)),
    seq(UD, seq(0, string(1, "z"))),
])

OTHER = b"".join([
    seq(RA, seq(0, unsigned(0, 4)), seq(1, unsigned(0, 4)), seq(2, unsigned(0, 4)),
        seq(3, unsigned(0, 4))),
    seq(UD, signed(2, -2)),
    seq(RD, seq(0, unsigned(0, 0)), seq(1, unsigned(0, 0)), seq(2, unsigned(0, 1))),
])

EMPTY = b""

IMAGES = (("empty", EMPTY), ("full", FULL), ("sparse", SPARSE), ("defaults", DEFAULTS),
          ("held_default_option", HELD_DEFAULT_OPTION), ("other", OTHER))

# Both malformed by §4.1, in any schema: an 11-byte varint is INVALID.
TAILS = (("overlong header", bytes([0x80] * 10 + [0x01])),
         ("overlong value", header(UNKNOWN, WT_UNSIGNED) + b"\xff" * 10 + b"\x01"))


# --- comparing two decoded messages -----------------------------------------

def _nan_free(v):
    """NaN never equals itself; both sides come from one harness, so any NaN is
    the same token here (the bytes compare its bits)."""
    if isinstance(v, float) and math.isnan(v):
        return "NaN"
    if isinstance(v, dict):
        return {k: _nan_free(x) for k, x in v.items()}
    if isinstance(v, list):
        return [_nan_free(x) for x in v]
    return v


def differences(a, b):
    a, b = _nan_free(a), _nan_free(b)
    kw = dict(int_strings=True, base64_bytes=True)
    return list(json_equal.diff(a, b, **kw)) + list(json_equal.diff(b, a, **kw))


class Harness:
    """The generated harness, driven through its decode, encode and reuse verbs."""

    def __init__(self, argv, msg, verb, cwd):
        self.argv, self.msg, self.verb, self.cwd = argv, msg, verb, cwd

    def _run(self, args, data):
        p = subprocess.run(self.argv + args, input=data, cwd=self.cwd,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if p.returncode != 0:
            raise RuntimeError(f"{args[0]} exited {p.returncode}: "
                               f"{p.stderr.decode(errors='replace').strip()[-400:]}")
        return p.stdout.decode(errors="replace")

    def encode(self, doc):
        p = subprocess.run(self.argv + ["encode", self.msg], input=json.dumps(doc).encode(),
                           cwd=self.cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if p.returncode != 0:
            raise RuntimeError(f"encode exited {p.returncode}: "
                               f"{p.stderr.decode(errors='replace').strip()[-400:]}")
        return p.stdout

    def decode(self, data):
        return harness_dialect.loads_out(self._run(["decode", self.msg], data).splitlines()[0])

    def reuse(self, prev, nxt):
        lines = self._run([self.verb, self.msg, prev.hex()], nxt).splitlines()
        if len(lines) < 3:
            raise RuntimeError(f"{self.verb} printed {len(lines)} lines, want 3: {lines!r}")
        return lines[0].strip(), harness_dialect.loads_out(lines[1]), lines[2].strip()


def check(h, inputs, tails, jobs=8):
    """Run the table against harness h. Returns (cases run, failure lines)."""
    fails = []
    names = [n for n, _ in inputs]
    data = dict(inputs)
    try:
        with ThreadPoolExecutor(jobs) as ex:
            fresh = dict(zip(names, ex.map(lambda n: h.decode(data[n]), names)))
    except RuntimeError as e:
        return 0, [f"a fresh decode of an input failed: {e}"]

    def one(case):
        prev, nxt, tail = case
        try:
            return h.reuse(data[prev], data[nxt] + (tail[1] if tail else b""))
        except RuntimeError as e:
            return e

    def judge(case, got, cname):
        prev, nxt, tail = case
        if isinstance(got, Exception):
            return [f"{cname}: {got}"]
        st, doc, hexed = got
        out = []
        if tail and st in OK:
            out.append(f"{cname}: verdict {st}, want a refusal")
        if not tail and st != "COMPLETE":
            out.append(f"{cname}: verdict {st}, want COMPLETE")
        lines = differences(fresh[nxt], doc)
        if lines:
            out.append(f"{cname}: the destination differs from a fresh decode of {nxt}:\n"
                       + "\n".join(lines[:12]))
        if hexed != data[nxt].hex():
            out.append(f"{cname}: re-encodes to {hexed or '(empty)'}, "
                       f"want {data[nxt].hex() or '(empty)'}")
        return out

    controls = [("empty", n, None) for n in names]
    with ThreadPoolExecutor(jobs) as ex:
        got = list(ex.map(one, controls))
    for case, g in zip(controls, got):
        for f in judge(case, g, f"control {case[1]}"):
            fails.append(f + "\n(an input that does not survive a decode into a fresh "
                         "destination is not canonical for this backend)")
    for i, a in enumerate(names):
        for b in names[i + 1:]:
            if not differences(fresh[a], fresh[b]):
                fails.append(f"distinct: {a} and {b} decode to the same message, so a "
                             "pair of them cannot tell a reset from none")
    if fails:
        return len(controls), fails + ["the control or distinct rows failed: the table was not run"]

    cases = [(p, n, t) for p in names for n in names for t in (None,) + tuple(tails)]
    with ThreadPoolExecutor(jobs) as ex:
        got = list(ex.map(one, cases))
    for case, g in zip(cases, got):
        p, n, t = case
        fails += judge(case, g, f"{p} -> {n}" + (f" + {t[0]} tail" if t else ""))
    return len(controls) + len(cases), fails


def parse_fixtures(specs):
    out = []
    for spec in specs:
        name, sep, text = spec.partition("=")
        if not sep:
            raise SystemExit(f"--fixture wants NAME=JSON, got {spec!r}")
        if text.startswith("@"):
            with open(text[1:], encoding="utf-8") as fh:
                text = fh.read()
        out.append((name, json.loads(text)))
    return out


# ---- --self-test -------------------------------------------------------------
#
# A model message of three fields with defaults. Its "wire" is the JSON of the
# fields that differ from their default, so the empty message is the empty
# payload, as in the real format. The correct model resets before it decodes.
# Each broken model forgets one thing, and the table must catch every one.

MODEL_DEFAULTS = {"n": 7, "arr": [1, 2], "u": {"a": 0}}


class Model:
    def __init__(self, keep=None):
        self.keep = keep  # the field a broken reset forgets, or "all"

    @staticmethod
    def _wire(doc):
        sparse = {k: v for k, v in doc.items() if MODEL_DEFAULTS[k] != v}
        return json.dumps(sparse, sort_keys=True).encode() if sparse else b""

    @staticmethod
    def _parse(data):
        for _, tail in TAILS:
            if data.endswith(tail):
                ok = data[:-len(tail)]
                return (json.loads(ok) if ok else {}), "InvalidMessage"
        return (json.loads(data) if data else {}), "COMPLETE"

    def encode(self, doc):
        return self._wire({**MODEL_DEFAULTS, **doc})

    def decode(self, data):
        return {**MODEL_DEFAULTS, **self._parse(data)[0]}

    def reuse(self, prev, nxt):
        dest = self.decode(prev)
        if self.keep != "all":
            kept = {self.keep: dest[self.keep]} if self.keep else {}
            dest = {**MODEL_DEFAULTS, **kept}
        fields, st = self._parse(nxt)
        dest.update(fields)
        return st, dest, self._wire(dest).hex()


def self_test():
    want = {"defaults": "00 06", "union q": "5e 0a 0a 71 07",
            "ud xs": "9e 01 0b 03 04 05 06 07"}
    got = {"defaults": DEFAULTS.hex(" "), "union q": seq(U, string(1, "q")).hex(" "),
           "ud xs": seq(UD, uarray(1, [4, 5, 6])).hex(" ")}
    bad = [k for k in want if want[k] != got[k]]
    for k in bad:
        print(f"FAIL self-test: the image {k} is {got[k]}, want {want[k]}")
    if bad:
        return 1
    m = Model()
    fixtures = [("full", {"n": 1, "arr": [5, 6, 7], "u": {"b": "x"}}),
                ("short", {"arr": [9]}), ("other", {"n": 3, "u": {"a": 4}})]
    inputs = [("empty", b"")] + [(n, m.encode(d)) for n, d in fixtures]
    cases, fails = check(m, inputs, TAILS, jobs=1)
    if fails:
        print("FAIL self-test: the correct model fails:\n" + "\n".join(fails))
        return 1
    for keep in ("all", "n", "arr", "u"):
        if not check(Model(keep), inputs, TAILS, jobs=1)[1]:
            print(f"FAIL self-test: a reset that keeps {keep!r} passes")
            return 1
    print(f"self-test: the hand-written images match; the correct model passes {cases} "
          "cases; a reset that forgets any one field, or all of them, fails")
    return 0


def main(argv):
    if argv[:1] == ["--emit-schema"]:
        sys.stdout.write(SCHEMA)
        return 0
    if argv[:1] == ["--self-test"]:
        return self_test()
    if "--" not in argv:
        print(__doc__)
        return 2
    cut = argv.index("--")
    ap = argparse.ArgumentParser()
    ap.add_argument("label")
    ap.add_argument("--msg", default=None)
    ap.add_argument("--fixture", action="append", default=[])
    ap.add_argument("--invalid-tail", default=None)
    ap.add_argument("--verb", default=VERB)
    ap.add_argument("--cwd", default=None)
    args = ap.parse_args(argv[:cut])
    cmd = argv[cut + 1:]
    if not cmd:
        print("FAIL: no harness command after --")
        return 2
    if args.fixture and not args.msg:
        print("--fixture needs --msg: the message of the backend's own schema")
        return 2
    h = Harness(cmd, args.msg or MESSAGE, args.verb, args.cwd)
    if args.fixture:
        try:
            inputs = [("empty", b"")] + [(n, h.encode(d)) for n, d in parse_fixtures(args.fixture)]
        except RuntimeError as e:
            print(f"FAIL [{args.label}] reused destination: a fixture did not encode: {e}")
            return 1
    else:
        inputs = list(IMAGES)
    tails = TAILS if args.invalid_tail is None else (("given", bytes.fromhex(args.invalid_tail)),)
    cases, fails = check(h, inputs, tails)
    if fails:
        failed = len({f.split(":", 1)[0] for f in fails})
        print(f"FAIL [{args.label}] reused destination ({failed} of {cases} cases failed):")
        for f in fails:
            print("  " + f.replace("\n", "\n  "))
        return 1
    print(f"==> [{args.label}] a reused destination reads as a fresh decode ({cases} cases)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
