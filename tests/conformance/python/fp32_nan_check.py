#!/usr/bin/env python3
"""An fp32 payload survives decode + re-encode BIT-FOR-BIT (CORELIB_PLAN §6.5,
MESSAGE_SPEC §4.6, generator#414).

The rule is universal -- "for every implementation, decode -> re-encode of any
fp32 payload (signaling NaN included) MUST reproduce the exact 4 wire bytes, at
every fp32 position" -- but the HAZARD is specific to the three targets whose
only float type is a double: Python, TypeScript and Dart. Widening an fp32
signaling NaN into a double and narrowing it back is where the payload dies,
because the usual answer is to quiet it (0x7F800001 -> 0x7FC00000). TypeScript
(generator#235) and Dart (generator#226) each pin this after a real defect;
Python had no NaN case of any kind, which is the gap this closes.

WHAT THIS DRIVER ASSERTS, AND WHAT IT DOES NOT

It asserts the §6.5 OUTCOME through generated code: in == out, byte for byte.
It does NOT assert a mechanism, and the distinction matters twice over.

  * Python's generated code has no raw-bits path. `backend.go` emits
    `e.write_float32(...)` and `visitor.go` routes decode through `on_float32` /
    `on_float32_array` -- the ordinary widened-double channel. corelib-py's
    §6.5 raw channel (`on_float32_bits`, `write_float32_bits`, opt-in via
    `_wants_f32_bits`) is never overridden here, so this leg gives it no
    coverage; that is corelib-py's own pytest's job. What holds the bits
    together is corelib-py doing the widening BY HAND on the ordinary path
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

WHY A DRIVER RATHER THAN A HARNESS VERB. §6.5 wants the outcome on *every*
decode surface, and the check is wire -> object -> wire with no JSON detour:
the generated harness renders a NaN as JSON `NaN`, and feeding that back yields
the canonical quiet 0x7FC00000 -- measured -- so a block built on the existing
`encode`/`decode` verbs would be vacuous or falsely red. The python harness has
no `recode` verb (only TypeScript and Dart emit one), so the round-trip happens
in-process here, which also lets the streaming surface be driven with the same
fixtures instead of a second set of verbs.

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

import sys

import sofab
from sofab import Status

# Chunk widths for the streaming surface. 1 puts a boundary between every pair
# of bytes; 3 lands inside the 4-byte fp32 word rather than beside it (a
# reassembly path that drops or re-orders payload bytes shows up here and not at
# 1); 4096 is the whole message in one feed, the reference the other two must
# match.
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

# label -> (wire hex, must re-encode to the same bytes?)
#
# The four NaNs are the point: a signaling NaN, a quiet one carrying a payload,
# and both of their negatives -- sign bit, quiet bit and mantissa payload are
# three separate things a lossy path loses separately. 2.5 is the control that
# says nothing regressed for ordinary values.
#
# The two `False` rows are the mistake that is easiest to make once a target
# starts carrying raw wire bytes: bytes on the wire must NOT be read as "the
# field was present". MESSAGE_SPEC §2 decides presence from the VALUE, so an
# explicitly-encoded default must still normalize away to the empty message.
FIXTURES = (
    ("scalar sNaN      0x7F800001", S + "0100807f", True),
    ("scalar qNaN      0x7FC00001", S + "0100c07f", True),
    ("scalar -qNaN     0xFFC00001", S + "0100c0ff", True),
    ("scalar -sNaN     0xFF800001", S + "010080ff", True),
    ("scalar 2.5       (control)", S + "00002040", True),
    # An ordinary 1.0 sits between the two NaNs: it must survive beside them,
    # and its position pins that the element loop does not stop at the first NaN.
    ("array  sNaN|1.0|-sNaN", A + "0100807f" + "0000803f" + "010080ff", True),
    # Presence, not payload (§2): an explicit +0.0 / the declared array default.
    ("scalar +0.0      (default)", S + "00000000", False),
    ("array  default   [0.0,-1.5,3.25]", A + "00000000" + "0000c0bf" + "00005040", False),
)

failures = 0


def fail(what, detail):
    global failures
    print("FAIL: %s: %s" % (what, detail))
    failures += 1


def require_engine(want):
    """Assert IN THIS PROCESS which corelib-py engine is loaded (see the header).

    `IMPL` alone is not enough for `native`: the exported Encoder/Decoder must
    BE the accelerator's, which is what a native leg actually exercises.
    """
    if sofab.IMPL != want:
        print("FAIL: this leg must run on the '%s' engine, but sofab.IMPL is '%s'"
              % (want, sofab.IMPL))
        return False
    if want == "native":
        from sofab import _speedups
        if sofab.Encoder is not _speedups.Encoder or sofab.Decoder is not _speedups.Decoder:
            print("FAIL: sofab.IMPL says 'native' but the exported Encoder/Decoder "
                  "are not the accelerator's")
            return False
    return True


def check(what, msg, wire, exact):
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
    for label, hexwire, exact in FIXTURES:
        wire = bytes.fromhex(hexwire)

        # ---- surface 1: the one-shot path ---------------------------------
        try:
            msg = cls.decode(wire)
        except Exception as exc:  # noqa: BLE001
            fail("one-shot %s" % label, "must decode, raised %r" % (exc,))
            continue
        check("one-shot %s" % label, msg, wire, exact)

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
            check("streaming(chunk=%d) %s" % (size, label), dec.message, wire, exact)

    if failures:
        return 1
    print("   fp32 bit-exactness: %d fixtures x (1 one-shot + %d chunk sizes)"
          % (len(FIXTURES), len(CHUNK_SIZES)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
