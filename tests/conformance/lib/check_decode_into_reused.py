#!/usr/bin/env python3
"""Decoding into a destination that already holds a message equals a fresh decode.

Usage:
  check_decode_into_reused.py --emit-schema
  check_decode_into_reused.py --self-test
  check_decode_into_reused.py <label> [--cwd DIR] [--verb VERB] -- <harness argv...>

## The rule

MESSAGE_SPEC §2 omits every field equal to its default, and an ABSENT field reads
as its declared default. Absence fires nothing on the decode side -- no callback,
no store -- so a decoder that is handed a destination which still holds an OLDER
message must put it back to the declared defaults before the first byte is
applied. Otherwise every field the new message omits keeps the older message's
value, and the result differs from a fresh decode of the very same bytes.

Every surface that decodes into a caller-owned destination owes the same
contract: the value it leaves there equals a fresh decode of the input. On a
rejected input the destination holds the fields decoded before the error, and
never a value of the message it held before.

## The harness verb

    <harness> recodeinto <message> <hex of the message it held>   < bytes

decodes the hex into a destination (the plain decode), then decodes stdin into
that SAME destination through the surface that takes one, and writes the
destination re-encoded to stdout. It exits 0 when the input was accepted and 1
when it was refused -- writing the re-encode in both cases, since what a refused
destination holds is part of the contract. A harness whose language has a
second such surface can take another verb name (`--verb`).

The re-encode is compared byte for byte, and the bytes are computed HERE from the
wire format, not taken from the backend under test: the destination equals a
fresh decode exactly when it re-encodes to the canonical image of the input
(every image below is canonical, which the `control` rows check against the
backend's own decode -> encode). A field left at an older value is a field that
differs from its default, so it shows up as bytes the input never carried.

## The schema (`--emit-schema`)

One message, `reuse`, with a field of each shape a reset has to put back: a
scalar with a non-zero default, a 64-bit integer, a boolean defaulting to true,
an fp32 (whose NaN bytes some targets keep beside the value), a string with a
default, a blob without and one with a default, a native array with a default and
an fp32 array, a string array, a struct, a union and an array of structs. Every
array and string is bounded, so the statically sized profiles build it too.

## The rows

    control_*        each image below decodes and re-encodes to itself (recode)
    full_to_sparse   FULL held, SPARSE decoded into it: absent scalars, string and
                     blobs, shorter arrays, a struct with one field absent, the
                     union on a different option
    full_to_defaults FULL held, a message with ONE field decoded into it: every
                     other field back to its default, the union to its default
                     option
    full_to_empty    FULL held, the empty message (zero bytes) decoded into it:
                     the destination re-encodes to zero bytes
    full_to_rejected FULL held, SPARSE followed by a refused field: the verb exits
                     1 and the destination holds SPARSE -- what was decoded before
                     the error, nothing of FULL
    sparse_to_full   SPARSE held, FULL decoded into it (the plain case, so a
                     harness that ignores the held message cannot pass by luck)
"""

import argparse
import struct
import subprocess
import sys

MESSAGE = "reuse"

# Wire types (CORELIB_PLAN §4.3).
WT_UNSIGNED, WT_SIGNED, WT_FIXLEN, WT_ARR_U, WT_ARR_S, WT_ARR_F, WT_SEQ = 0, 1, 2, 3, 4, 5, 6
END = b"\x07"                                  # sequence end marker (§4.9)
SUB_FP32, SUB_STRING, SUB_BLOB = 0, 2, 3       # fixlen subtypes (§4.6)
FIXLEN_FP32 = 0x20                             # fixlen word of an fp32 array (§4.8)

# Field ids of `reuse`.
N, I, OK, F, S, B, BD, A, FA, SA, ST, U, RA = range(13)
# An id the schema does not declare, for the refused field.
UNKNOWN = 31

SCHEMA = f"""\
# Decode into a reused destination (MESSAGE_SPEC §2), printed by
# tests/conformance/lib/check_decode_into_reused.py so the schema and the bytes
# it forges against it have one definition between them.
  {MESSAGE}:
    payload:
      n:  {{ id: {N}, type: u32, default: 5 }}
      i:  {{ id: {I}, type: i64 }}
      ok: {{ id: {OK}, type: boolean, default: true }}
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


def fp32(fid: int, x: float) -> bytes:
    return fixlen(fid, SUB_FP32, struct.pack("<f", x))


def uarray(fid: int, values) -> bytes:
    return header(fid, WT_ARR_U) + varint(len(values)) + b"".join(varint(v) for v in values)


def f32array(fid: int, values) -> bytes:
    return (header(fid, WT_ARR_F) + varint(len(values)) + varint(FIXLEN_FP32)
            + b"".join(struct.pack("<f", v) for v in values))


def seq(fid: int, *parts: bytes) -> bytes:
    return header(fid, WT_SEQ) + b"".join(parts) + END


# Every field away from its default.
FULL = b"".join([
    unsigned(N, 77),
    signed(I, -1234567890123),
    unsigned(OK, 0),
    fp32(F, 1.5),
    string(S, "full"),
    blob(B, b"\x09\x08\x07"),
    blob(BD, b"\x05"),
    uarray(A, [10, 11, 12, 13]),
    f32array(FA, [1.0, 2.0, 3.0]),
    seq(SA, string(0, "aa"), string(1, "bb"), string(2, "cc")),
    seq(ST, signed(0, 42), string(1, "in")),
    seq(U, seq(2, unsigned(0, 9))),
    seq(RA, seq(0, unsigned(0, 1)), seq(1, unsigned(0, 2)), seq(2, unsigned(0, 3))),
])

# Scalars, string and blobs absent; every array shorter; the struct with x
# absent; the union on another option (held, so written: §4.2).
SPARSE = b"".join([
    uarray(A, [3]),
    f32array(FA, [4.0]),
    seq(SA, string(0, "z")),
    seq(ST, string(1, "sp")),
    seq(U, string(1, "q")),
    seq(RA, seq(0, unsigned(0, 5))),
])

# One field; everything else, the union included, at its default.
DEFAULTS = unsigned(N, 6)

EMPTY = b""

# SPARSE, then a field of an undeclared id whose varint runs past the 10-byte /
# 64-bit bound (§4.1): INVALID, and only after every SPARSE field was applied.
REJECTED = SPARSE + header(UNKNOWN, WT_UNSIGNED) + b"\xff" * 10 + b"\x01"

IMAGES = (("full", FULL), ("sparse", SPARSE), ("defaults", DEFAULTS))

# row, held, decoded into it, expected re-encode, expected exit code
ROWS = (
    ("full_to_sparse", FULL, SPARSE, SPARSE, 0),
    ("full_to_defaults", FULL, DEFAULTS, DEFAULTS, 0),
    ("full_to_empty", FULL, EMPTY, EMPTY, 0),
    ("full_to_rejected", FULL, REJECTED, SPARSE, 1),
    ("sparse_to_full", SPARSE, FULL, FULL, 0),
)


def self_test() -> int:
    want = {
        "defaults": "00 06",
        "union q": "5e 0a 0a 71 07",
    }
    got = {"defaults": DEFAULTS.hex(" "), "union q": seq(U, string(1, "q")).hex(" ")}
    bad = [k for k in want if want[k] != got[k]]
    for k in bad:
        print(f"FAIL self-test: {k} is {got[k]}, want {want[k]}")
    if not bad:
        print("self-test: the hand-written images match")
    return 1 if bad else 0


def tail(b: bytes) -> str:
    lines = [l.strip() for l in b.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:])[:300] if lines else "<no output>"


def check(cmd, cwd, verb):
    """Yields a problem string per failing row; nothing when all hold."""
    def run(argv, data):
        return subprocess.run(cmd + argv, input=data, cwd=cwd,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE)

    for name, img in IMAGES:
        p = run(["recode", MESSAGE], img)
        if p.returncode != 0:
            yield f"control_{name}: recode failed, harness exited {p.returncode}: {tail(p.stderr)}"
        elif p.stdout != img:
            yield (f"control_{name}: the image is not canonical for this backend, so the rows "
                   f"below cannot speak: sent {img.hex(' ')}, got {p.stdout.hex(' ')}")

    for row, held, data, want, rc in ROWS:
        p = run([verb, MESSAGE, held.hex()], data)
        if p.returncode != rc:
            yield (f"{row}: expected exit {rc} ({'refused' if rc else 'accepted'}), "
                   f"harness exited {p.returncode}: {tail(p.stderr)}")
            continue
        if p.stdout != want:
            yield (f"{row}: the destination must equal a fresh decode of the input "
                   f"(re-encode {want.hex(' ') or '<empty>'}), got {p.stdout.hex(' ') or '<empty>'}")


def main() -> int:
    if len(sys.argv) > 1 and sys.argv[1] == "--emit-schema":
        sys.stdout.write(SCHEMA)
        return 0
    if len(sys.argv) > 1 and sys.argv[1] == "--self-test":
        return self_test()
    if "--" not in sys.argv:
        print(__doc__)
        return 2
    i = sys.argv.index("--")
    ap = argparse.ArgumentParser()
    ap.add_argument("label")
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="recodeinto")
    args = ap.parse_args(sys.argv[1:i])
    cmd = sys.argv[i + 1:]
    if not cmd:
        print("FAIL: no harness command after --")
        return 2
    problems = list(check(cmd, args.cwd, args.verb))
    total = len(IMAGES) + len(ROWS)
    if problems:
        for p in problems:
            print(f"    FAIL [{args.label}] {p}")
        print(f"FAIL [{args.label}] decode into a reused destination: "
              f"{len(problems)} of {total} rows failed")
        return 1
    print(f"==> [{args.label}] decode into a reused destination: {total} rows ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
