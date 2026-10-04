#!/usr/bin/env python3
"""Encode a multi-KB message through the drain path (generator#653).

Usage:
  check_stream_encode.py --emit-schema
  check_stream_encode.py --emit-bounded-schema
  check_stream_encode.py <label> [--windows 0,1,7,511,512,513] [--cwd DIR]
                         [--int-strings] [--base64-bytes]
                         -- <harness argv...>

ARCHITECTURE §9.2 / §9.6: encoding is STREAMING. A message is written through
a fixed buffer that a flush sink drains each time it fills, so the message can
exceed RAM; for an unbounded message the generated one-shot `encode()` is that
same shape (a fixed scratch plus a sink appending into caller-owned storage).
CORELIB_PLAN §5.1 adds the contract any such buffer has to meet: a buffer at or
above MIN_OUTPUT_BUFFER produces the SAME bytes as a one-shot encode.

Every other fixture the suites encode is a few hundred bytes, which is below a
512-byte scratch, so the drain ran at most once and a sink bug -- wrong slice
bounds, a buffer reused before it was copied, a lost tail at `flush` -- passed
all of them. This driver builds a message of tens of KB so the scratch fills
many times, and with a window of 1..513 bytes the drain also lands on every
kind of boundary (mid-varint, mid-length-word, mid-payload, mid-frame).

## What it checks, per backend

  1. `encode drain`: the bytes equal the wire this driver builds itself. The
     wire for these shapes is simple (MESSAGE_SPEC §4), so it is computed here
     instead of frozen; the driver also asserts the message is at least
     8 KiB, so a fixture edit cannot quietly shrink it under the scratch.
  2. `streamencode drain <window>` at each window: the bytes are identical to
     the one-shot encode. The harness verb encodes through the generated
     `encodeTo`/`serialize` surface into a sink with a buffer of that size; a
     window below the corelib's MIN_OUTPUT_BUFFER is raised to it by the harness.
     Window 0 is the generated streaming method itself (`encodeTo`/`EncodeTo`),
     with whatever scratch it owns; every other window drives the same generated
     serializer through a buffer of that size.
  3. every result decodes back (`decode drain`) to the fixture, compared as data
     with json_equal.

## The schema

`--emit-schema` prints it: an unbounded string and blob, an unbounded u32 native
array (mixed varint widths) and fp64 native array, an unbounded wrapper array of
strings, and a struct holding a large blob, so a sequence frame spans drains.
`--emit-bounded-schema` is the same message with large `maxlen`/`count` bounds,
for the targets that cannot hold an unbounded field (c, cpp `corelib: c-cpp`,
rust `no_std`): the drain path is what is under test, not the allocator.
"""
import json
import os
import struct
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import json_equal  # noqa: E402

MESSAGE = "drain"
DEFAULT_WINDOWS = "0,1,7,511,512,513"
MIN_BYTES = 8192

STR_LEN = 3000
BLOB_LEN = 20000
U32_COUNT = 3000
FP64_COUNT = 400
WRAP_COUNT = 6
WRAP_LEN = 300
SUB_BLOB_LEN = 5000

SCHEMA_HEAD = """\
# The multi-drain encode message (generator#653), printed by
# tests/conformance/lib/check_stream_encode.py so the schema and the driver that
# builds its wire share one definition. The fixture encodes to tens of KB: far
# above any scratch a backend's one-shot encode() or encodeTo() drains through.
"""


def emit_schema() -> int:
    sys.stdout.write(SCHEMA_HEAD)
    print("  drain:")
    print("    payload:")
    print("      s: { id: 0, type: string }")
    print("      b: { id: 1, type: blob }")
    print("      u: { id: 2, type: array, items: { type: u32 } }")
    print("      f: { id: 3, type: array, items: { type: fp64 } }")
    print("      w: { id: 4, type: array, items: { type: string } }")
    print("      st:")
    print("        id: 5")
    print("        type: struct")
    print("        fields:")
    print("          sb: { id: 0, type: blob }")
    print("          n:  { id: 1, type: u32 }")
    return 0


def emit_bounded_schema() -> int:
    sys.stdout.write(SCHEMA_HEAD)
    print("# Bounded variant: every bound sits above the fixture, so a fixed-storage")
    print("# target holds the same message.")
    print("  drain:")
    print("    payload:")
    print(f"      s: {{ id: 0, type: string, maxlen: {STR_LEN + 100} }}")
    print(f"      b: {{ id: 1, type: blob, maxlen: {BLOB_LEN + 100} }}")
    print(f"      u: {{ id: 2, type: array, items: {{ type: u32, count: {U32_COUNT + 100} }} }}")
    print(f"      f: {{ id: 3, type: array, items: {{ type: fp64, count: {FP64_COUNT + 20} }} }}")
    print(f"      w: {{ id: 4, type: array, items: {{ type: string, count: {WRAP_COUNT + 2}, maxlen: {WRAP_LEN + 20} }} }}")
    print("      st:")
    print("        id: 5")
    print("        type: struct")
    print("        fields:")
    print(f"          sb: {{ id: 0, type: blob, maxlen: {SUB_BLOB_LEN + 100} }}")
    print("          n:  { id: 1, type: u32 }")
    return 0


def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def hdr(field_id: int, wire_type: int) -> bytes:
    return varint((field_id << 3) | wire_type)


def fixture():
    """(json input, expected wire bytes)."""
    s = "".join(chr(ord("a") + (i * 7) % 26) for i in range(STR_LEN))
    b = [(i * 7 + 3) % 256 for i in range(BLOB_LEN)]
    # Mixed varint widths (1..5 bytes), never the default 0.
    u = [((i * 2654435761) >> (i % 29)) % 4000000000 + 1 for i in range(U32_COUNT)]
    f = [i * 0.5 + 0.25 for i in range(FP64_COUNT)]
    w = ["".join(chr(ord("A") + (i * 5 + j) % 26) for j in range(WRAP_LEN - 3 * i))
         for i in range(WRAP_COUNT)]
    sb = [(i * 13 + 5) % 256 for i in range(SUB_BLOB_LEN)]
    n = 123456789

    wire = bytearray()
    # string / blob: header (fixlen), length word (n<<3 | 2 string, 3 blob), payload.
    sb_ = s.encode()
    wire += hdr(0, 2) + varint((len(sb_) << 3) | 2) + sb_
    wire += hdr(1, 2) + varint((len(b) << 3) | 3) + bytes(b)
    # native unsigned array: header (type 3), element count, then the varints.
    wire += hdr(2, 3) + varint(len(u)) + b"".join(varint(v) for v in u)
    # fixlen array: header (type 5), count, ONE fixlen word (8 bytes, float = 1), data.
    wire += hdr(3, 5) + varint(len(f)) + varint((8 << 3) | 1)
    wire += b"".join(struct.pack("<d", v) for v in f)
    # wrapper array: a sequence whose child ids are the element indices, closed by 0x07.
    wire += hdr(4, 6)
    for i, text in enumerate(w):
        t = text.encode()
        wire += hdr(i, 2) + varint((len(t) << 3) | 2) + t
    wire += bytes([0x07])
    # struct: a sequence frame holding a large blob and a scalar.
    wire += hdr(5, 6)
    wire += hdr(0, 2) + varint((len(sb) << 3) | 3) + bytes(sb)
    wire += hdr(1, 0) + varint(n)
    wire += bytes([0x07])

    obj = {"s": s, "b": b, "u": u, "f": f, "w": w, "st": {"sb": sb, "n": n}}
    return obj, bytes(wire)


def run(cmd, data, cwd):
    return subprocess.run(cmd, input=data, cwd=cwd, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE)


def tail(stderr: bytes) -> str:
    lines = [l.strip() for l in stderr.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:]) if lines else "<no output>"


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def first_diff(a: bytes, b: bytes) -> int:
    for i, (x, y) in enumerate(zip(a, b)):
        if x != y:
            return i
    return min(len(a), len(b))


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()
    if "--emit-bounded-schema" in argv:
        return emit_bounded_schema()
    if "--" not in argv or not argv or argv[0].startswith("--"):
        print(__doc__)
        return 2
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    label = head[0]
    cwd = opt(head, "--cwd")
    windows = [int(w) for w in opt(head, "--windows", DEFAULT_WINDOWS).split(",")]
    int_strings = "--int-strings" in head
    base64_bytes = "--base64-bytes" in head

    obj, want = fixture()
    if len(want) < MIN_BYTES:
        print(f"FAIL: [{label}] the fixture encodes to {len(want)} bytes, below the "
              f"{MIN_BYTES} the drain check needs")
        return 1
    # `1` or the corelib's floor, whichever is larger, is the point of the first
    # window; a driver that stopped asking for the smallest one checks less.
    if 1 not in windows or len([w for w in windows if w]) < 4:
        print(f"FAIL: [{label}] --windows must include 1 and at least four sizes in all")
        return 1
    body = json.dumps(obj).encode()

    def decoded_matches(what, data):
        dec = run(cmd + ["decode", MESSAGE], data, cwd)
        if dec.returncode != 0:
            print(f"FAIL: [{label}] {what}: decode of its own bytes failed: {tail(dec.stderr)}")
            return False
        try:
            got = json.loads(dec.stdout.decode())
        except ValueError:
            print(f"FAIL: [{label}] {what}: decode printed no JSON: "
                  f"{dec.stdout.decode(errors='replace')[:200]!r}")
            return False
        lines = list(json_equal.diff(obj, got, int_strings=int_strings, base64_bytes=base64_bytes))
        if lines:
            print(f"FAIL: [{label}] {what}: decoded back to a different message")
            for ln in lines[:8]:
                print(ln)
            return False
        return True

    one = run(cmd + ["encode", MESSAGE], body, cwd)
    if one.returncode != 0:
        print(f"FAIL: [{label}] one-shot encode of {len(want)} bytes failed: {tail(one.stderr)}")
        return 1
    if one.stdout != want:
        print(f"FAIL: [{label}] one-shot encode: {len(one.stdout)} bytes, want {len(want)}; "
              f"first difference at byte {first_diff(one.stdout, want)}")
        return 1
    if not decoded_matches("one-shot encode", one.stdout):
        return 1

    for w in windows:
        enc = run(cmd + ["streamencode", MESSAGE, str(w)], body, cwd)
        if enc.returncode != 0:
            print(f"FAIL: [{label}] streamencode with a {w}-byte window failed: {tail(enc.stderr)}")
            return 1
        if enc.stdout != one.stdout:
            print(f"FAIL: [{label}] streamencode with a {w}-byte window: {len(enc.stdout)} bytes, "
                  f"the one-shot encode is {len(one.stdout)}; first difference at byte "
                  f"{first_diff(enc.stdout, one.stdout)}")
            return 1
        if not decoded_matches(f"streamencode at window {w}", enc.stdout):
            return 1

    size = f"{len(want):,}".replace(",", " ")
    print(f"   [{label}] stream encode: {size} bytes one-shot byte-exact; windows "
          f"{','.join(str(w) for w in windows)} identical; all decode back")
    return 0


if __name__ == "__main__":
    sys.exit(main())
