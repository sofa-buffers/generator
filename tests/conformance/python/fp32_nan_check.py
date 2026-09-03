#!/usr/bin/env python3
"""An fp32 payload survives decode BIT-FOR-BIT -- into the value, and back out
onto the wire (CORELIB_PLAN §6.5, MESSAGE_SPEC §4.6, generator#414).

The rule is universal -- "for every implementation, decode -> re-encode of any
fp32 payload (signaling NaN included) MUST reproduce the exact 4 wire bytes, at
every fp32 position" -- but the HAZARD is specific to the three targets whose
only float type is a double: Python, TypeScript and Dart. Widening an fp32
signaling NaN into a double is not bit-preserving: IEEE widening SETS the quiet
bit and keeps the payload (§6.5's diagram: 0x7F800001 -> 0x7FC00001), so the
signaling-ness is gone the instant the value passes through the wider float and
no later code can recover it. TypeScript (generator#235) and Dart (generator#226)
each pin this after a real defect; Python had no NaN case of any kind, which is
the gap this closes.

WHAT THIS DRIVER ASSERTS

§6.5's testing clause names TWO oracles -- "across decode -> re-encode AND any
materialized walk" -- and this driver runs both on every fixture:

  * the ROUND-TRIP path: re-encode the decoded message and diff the bytes,
    in == out;
  * the VALUE path: the materialized `msg.somefp32` / `msg.somefloatarray[i]`
    must be the fp64 image of the wire word, sign, quiet bit and 23-bit payload
    intact (`check_values`). §6.5 calls the omission of this leg out by name --
    "a port that guards its round-trip path but not its value path is the defect
    class this section exists to prevent" -- and it is the shape of both sibling
    defects.

Today the two coincide in python: the dataclass value IS what the re-encode
narrows, so a value that lost the payload could not re-encode bit-exactly. They
stop coinciding the moment python opts into corelib-py's raw channel
(`_wants_f32_bits` / `on_float32_bits` / `write_float32_bits`), which is what
§6.5 says a double-only target MUST provide: a message stashing raw bits beside
a canonical quiet NaN would pass the round-trip leg and fail this one. The value
leg is therefore also the assertion to REVISIT deliberately if python ever takes
that route -- §6.5 MAY-permits a convenience value that only knows it is `NaN`
-- rather than one to discover by watching it go red.

WHAT IT DOES NOT ASSERT: A MECHANISM

  * Python's generated code has no raw-bits path. `backend.go` emits
    `e.write_float32(...)` and `visitor.go` routes decode through `on_float32` /
    `on_float32_array` -- the ordinary widened-double channel. corelib-py's
    §6.5 raw channel is never overridden here, so this leg gives it no coverage;
    that is corelib-py's own pytest's job. What holds the bits together is
    corelib-py doing the widening BY HAND on the ordinary path
    (`_core.py::_unpack_f32_bits` / `_pack_f32_bits`, and the accelerator's
    `_speedups.pyx::_unpack_f32` / `_pack_f32`), which special-case NaN and move
    the 23-bit fp32 mantissa to and from the top of the 52-bit fp64 one so sign,
    quiet bit and payload all survive the trip through a Python float.

  * On x86-64 the safety net is invisible. Measured on CPython 3.14 / x86-64:
    bare `struct.pack('<f', struct.unpack('<f', b'\\x01\\x00\\x80\\x7f')[0])`
    already returns `0100807f`, because SSE moves do not quiet. So a corelib-py
    regression that DELETED those manual paths would still leave this green
    here, and would only go red on a platform that normalizes (x87 / 32-bit,
    some ARM configurations). Read a pass as "the outcome holds on this
    platform", not as "the widening path was exercised".

WHY A DRIVER RATHER THAN A HARNESS VERB. The check is wire -> object -> wire with
no JSON detour: the generated harness renders a NaN as JSON `NaN`, and feeding
that back yields the canonical payload-less quiet NaN 0x7FC00000 -- measured on
`decode | encode` -- so a block built on the existing `encode`/`decode` verbs
would be vacuous or falsely red. ts and dart run their blocks over a harness
`recode` verb (wire -> wire) that `generators/python/project.go` does not emit;
generator#468 tracks emitting it and folding the three one-shot legs onto one
`tests/conformance/lib/` driver, leaving only the python-specific engine and
streaming assertions here.

WHY IT ASSERTS ITS OWN ENGINE. run.sh runs it once per engine, but its own
`require_engine` checked a DIFFERENT process; corelib-py falls back to pure
Python whenever `sofab._speedups` cannot be imported, so an accelerator missing
here would make the native pass a silent duplicate of the pure one, both
printing success (generator#451). The two engines carry independent fp32 paths
-- `_core.py` and `_speedups.pyx` narrow and widen with separate code -- which
is exactly why this runs twice.

Usage: fp32_nan_check.py <generated-project-dir> <native|python>
       (asserts the engine it actually loaded; exits non-zero on failure)
"""

import struct
import sys

import sofab
from sofab import Status

# The engine oracle is shared with the other per-engine drivers in this
# directory (see engine.py); sys.path[0] is this directory.
from engine import require_engine

# Chunk widths for the streaming surface. 1 puts a boundary between every pair
# of bytes; 3 lands inside the 4-byte fp32 word rather than beside it (a
# reassembly path that drops or re-orders payload bytes shows up here and not at
# 1); 4096 is the whole message in one feed, the reference the other two must
# match.
#
# The one-shot leg is not a second decoder: generated `decode(data)` is literally
# one `Decoder(...).feed(data)` over the same corelib path. What it adds is the
# public entry point and the `MAX_FIELD_SPAN` reassembly sizing the backend
# derives for it -- not an independent implementation to compare against.
CHUNK_SIZES = (1, 3, 4096)

# example.yaml's two fp32 positions, and the headers that reach them.
#   somefp32       id  8, fp32,             default 0
#     header (8 << 3) | 2 (fixlen)        = 0x42, then fixlen word 0x20
#                                           (4-byte elements, fp32 subtype)
#   somefloatarray id 17, array<fp32> x 3, default [0.0, -1.5, 3.25]
#     header (17 << 3) | 5 (array-fixlen) = 0x8d 0x01 (varint), then the wire
#     count, then the same fixlen word 0x20
S = "4220"            # scalar somefp32 header + fixlen word
A = "8d010320"        # array somefloatarray header + count 3 + fixlen word

SCALAR = "somefp32"
ARRAY = "somefloatarray"

# (field, label, header hex, element words as hex, must re-encode to the same
#  bytes?)
#
# The four NaNs are the point: a signaling NaN, a quiet one carrying a payload,
# and both of their negatives -- sign bit, quiet bit and mantissa payload are
# three separate things a lossy path loses separately. §6.5 asks for a
# signaling, a quiet and a negative NaN "at both a scalar and an array
# position", so the array carries its own three; 2.5, and the 1.0 wedged between
# two array NaNs, are the controls that say nothing regressed for ordinary
# values.
#
# The two `False` rows are the mistake that is easiest to make once a target
# starts carrying raw wire bytes: bytes on the wire must NOT be read as "the
# field was present". MESSAGE_SPEC §2 decides presence from the VALUE, so an
# explicitly-encoded default must still normalize away to the empty message.
FIXTURES = (
    (SCALAR, "scalar sNaN      0x7F800001", S, ("0100807f",), True),
    (SCALAR, "scalar qNaN      0x7FC00001", S, ("0100c07f",), True),
    (SCALAR, "scalar -qNaN     0xFFC00001", S, ("0100c0ff",), True),
    (SCALAR, "scalar -sNaN     0xFF800001", S, ("010080ff",), True),
    (SCALAR, "scalar 2.5       (control)", S, ("00002040",), True),
    # An ordinary 1.0 sits between the two NaNs: it must survive beside them,
    # and its position pins that the element loop does not stop at the first NaN.
    (ARRAY, "array  sNaN|1.0|-sNaN", A, ("0100807f", "0000803f", "010080ff"), True),
    # The quiet half of §6.5's "signaling, quiet and negative" at an array
    # position, which neither the ts nor the dart block carries.
    (ARRAY, "array  sNaN|qNaN|-qNaN", A, ("0100807f", "0100c07f", "0100c0ff"), True),
    # Presence, not payload (§2): an explicit +0.0 / the declared array default.
    (SCALAR, "scalar +0.0      (default)", S, ("00000000",), False),
    (ARRAY, "array  default   [0.0,-1.5,3.25]", A,
     ("00000000", "0000c0bf", "00005040"), False),
)

failures = 0


def fail(what, detail):
    global failures
    print("FAIL: %s: %s" % (what, detail))
    failures += 1


def widened(word):
    """The fp64 bit pattern a bit-preserving widening of this fp32 word gives.

    A NaN is mapped by hand -- sign, exponent all ones, and the 23-bit fp32
    payload sitting at the top of the 52-bit fp64 mantissa (`<< 29`), so the
    quiet bit stays exactly where it was -- because that is the mapping §6.5
    demands and the one corelib-py implements. Everything else is ordinary IEEE
    widening, exact for every finite fp32, which `struct` does.
    """
    if (word & 0x7F800000) == 0x7F800000 and (word & 0x007FFFFF):
        return ((word >> 31) << 63) | (0x7FF << 52) | ((word & 0x007FFFFF) << 29)
    value = struct.unpack("<f", struct.pack("<I", word))[0]
    return struct.unpack("<Q", struct.pack("<d", value))[0]


def check_values(what, msg, field, words):
    """The MATERIALIZED walk (§6.5): the value the caller reads must carry the
    payload, not merely report `NaN`.

    Compared as fp64 bit patterns rather than with `==`, because no NaN compares
    equal to itself and `0.0 == -0.0`.
    """
    got = getattr(msg, field)
    values = [got] if field == SCALAR else list(got)
    if len(values) != len(words):
        fail(what, "%s materialized %d element(s), expected %d"
                   % (field, len(values), len(words)))
        return
    for i, (value, word) in enumerate(zip(values, words)):
        try:
            bits = struct.unpack("<Q", struct.pack("<d", value))[0]
        except (struct.error, TypeError) as exc:
            fail(what, "%s[%d] is not a float (%r): %r" % (field, i, value, exc))
            continue
        want = widened(word)
        if bits != want:
            fail(what, "the materialized value at %s[%d] lost fp32 payload bits"
                       % (field, i))
            print("  wire word 0x%08X" % word)
            print("  want fp64 0x%016X" % want)
            print("  got  fp64 0x%016X" % bits)


def check_wire(what, msg, wire, exact):
    """Re-encode a decoded message and diff the bytes.

    `exact` False means the fixture carried a value equal to the field's default,
    so §2 normalization must drop the field and the re-encode is the EMPTY
    message. Anything else -- including a re-encode that raises -- is a failure
    of this check.
    """
    try:
        out = msg.encode()
    except Exception as exc:  # noqa: BLE001 -- any failure here is this check's
        fail(what, "re-encoding the decoded message raised %r" % (exc,))
        return
    if exact:
        if out != wire:
            fail(what, "not bit-exact -- an fp32 payload was altered")
            print("  want %s" % wire.hex())
            print("  got  %s" % out.hex())
    elif out:
        fail(what, "a value equal to the field's default must re-encode as the "
                   "empty message (MESSAGE_SPEC §2), got %s" % out.hex())


def check(what, msg, field, words, wire, exact):
    check_values(what, msg, field, words)
    check_wire(what, msg, wire, exact)


def main(argv):
    if len(argv) != 3:
        print("\n".join(__doc__.strip().splitlines()[-2:]))
        return 2
    if not require_engine(argv[2]):
        return 2
    print("   engine: sofab.IMPL = %s" % sofab.IMPL)
    sys.path.insert(0, argv[1])
    import message  # noqa: E402 -- the generated project is only on the path now

    cls = message.Myfirstmessage
    for field, label, header, hexwords, exact in FIXTURES:
        wire = bytes.fromhex(header + "".join(hexwords))
        words = [struct.unpack("<I", bytes.fromhex(w))[0] for w in hexwords]

        # ---- surface 1: the one-shot path ---------------------------------
        try:
            msg = cls.decode(wire)
        except Exception as exc:  # noqa: BLE001
            fail("one-shot %s" % label, "must decode, raised %r" % (exc,))
            continue
        check("one-shot %s" % label, msg, field, words, wire, exact)

        # ---- surface 2: the streaming path, at several splits --------------
        # §6.5 asks for the outcome on every decode surface, and a narrowing
        # that only runs on the whole-payload delivery path would pass leg 1
        # while a split fp32 word came back quieted.
        for size in CHUNK_SIZES:
            dec = cls.decoder()
            st = None
            for i in range(0, len(wire), size):
                st = dec.feed(wire[i:i + size])
            if st is not Status.COMPLETE:
                fail("streaming(chunk=%d) %s" % (size, label),
                     "reported %s, expected COMPLETE" % (st,))
                continue
            check("streaming(chunk=%d) %s" % (size, label),
                  dec.message, field, words, wire, exact)

    if failures:
        return 1
    print("   fp32 bit-exactness: %d fixtures x (1 one-shot + %d chunk sizes) "
          "x (value + re-encode)" % (len(FIXTURES), len(CHUNK_SIZES)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
