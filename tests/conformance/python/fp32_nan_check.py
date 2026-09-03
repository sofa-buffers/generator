#!/usr/bin/env python3
"""The two fp32 oracles a `recode` verb cannot reach (CORELIB_PLAN §6.5).

The concern itself -- "decode -> re-encode of any fp32 payload, a signaling NaN
included, reproduces the exact 4 wire bytes" -- lives in one shared driver,
`tests/conformance/lib/check_fp32_nan.py`, which all three double-only targets
(TypeScript, Python, Dart) run over their harness `recode` verb (generator#468).
This file is what is LEFT of python's copy: the two surfaces a wire -> wire
subprocess cannot express. It imports the shared FIXTURE TABLE rather than
carrying one, so the rows here and the rows there cannot drift apart again.

## Leg 1: the materialized value (§6.5's second oracle)

§6.5's testing clause names two oracles -- "across decode -> re-encode AND any
materialized walk" -- and calls the omission of the second one out by name: "a
port that guards its round-trip path but not its value path is the defect class
this section exists to prevent". `check_values` is that walk: the value the
caller reads (`msg.somefp32`, `msg.somefloatarray[i]`) must be the fp64 image of
the wire word, sign, quiet bit and 23-bit payload intact.

Today the two oracles coincide in python: the dataclass value IS what the
re-encode narrows, so a value that lost the payload could not re-encode
bit-exactly. They stop coinciding the moment python opts into corelib-py's raw
channel (`_wants_f32_bits` / `on_float32_bits` / `write_float32_bits`), which is
what §6.5 says a double-only target MUST provide: a message stashing raw bits
beside a canonical quiet NaN would pass the round-trip leg and fail this one.
The value leg is therefore also the assertion to REVISIT deliberately if python
ever takes that route -- §6.5 MAY-permits a convenience value that only knows it
is `NaN` -- rather than one to discover by watching it go red.

## Leg 2: the streaming decode surface

§6.5 asks for the outcome on every decode surface, and `recode` is one-shot in
all three harnesses. Here the same fixtures are fed through `cls.decoder()` at
three chunk widths, and both oracles run on the result of each. Chunk 3 is the
point of the sweep: it lands INSIDE the 4-byte fp32 word rather than beside it,
so a reassembly path that drops or re-orders payload bytes shows up there and
not at 1. (This cannot be folded into `check_chunk_invariance.py` either: that
driver compares parsed JSON VALUES, and two separately parsed NaNs are never
equal in python, so every NaN fixture would fail there spuriously.)

The one-shot leg is kept beside them as the reference the chunked ones must
match; it is not a second decoder (generated `decode(data)` is literally one
`Decoder(...).feed(data)`), but it is the public entry point and the
`MAX_FIELD_SPAN` reassembly sizing the backend derives for it.

## Why it asserts its own engine

run.sh runs it once per engine, but its own `require_engine` checked a DIFFERENT
process; corelib-py falls back to pure Python whenever `sofab._speedups` cannot
be imported, so an accelerator missing here would make the native pass a silent
duplicate of the pure one, both printing success (generator#451). The two
engines carry independent fp32 paths -- `_core.py::_unpack_f32_bits` /
`_pack_f32_bits` and the accelerator's `_speedups.pyx::_unpack_f32` /
`_pack_f32` narrow and widen with separate code -- which is exactly why this
runs twice. The shared driver spawns a subprocess and inherits `SOFAB_PUREPYTHON`,
so it selects an engine but cannot assert one; that is this file's job.

## What neither leg asserts: a mechanism

  * Python's generated code has no raw-bits path. `backend.go` emits
    `e.write_float32(...)` and `visitor.go` routes decode through `on_float32` /
    `on_float32_array` -- the ordinary widened-double channel. corelib-py's
    §6.5 raw channel is never overridden here, so this gives it no coverage;
    that is corelib-py's own pytest's job. What holds the bits together is
    corelib-py doing the widening BY HAND on the ordinary path, special-casing
    NaN and moving the 23-bit fp32 mantissa to and from the top of the 52-bit
    fp64 one so sign, quiet bit and payload all survive the trip through a
    Python float.

  * On x86-64 the safety net is invisible. Measured on CPython 3.14 / x86-64:
    bare `struct.pack('<f', struct.unpack('<f', b'\\x01\\x00\\x80\\x7f')[0])`
    already returns `0100807f`, because SSE moves do not quiet. So a corelib-py
    regression that DELETED those manual paths would still leave this green
    here, and would only go red on a platform that normalizes (x87 / 32-bit,
    some ARM configurations). Read a pass as "the outcome holds on this
    platform", not as "the widening path was exercised".

Usage: fp32_nan_check.py <generated-project-dir> <native|python> <schema.yaml>
       (asserts the engine it actually loaded; exits non-zero on failure)
"""

import os
import struct
import sys

import sofab
from sofab import Status

# The engine oracle is shared with the other per-engine drivers in this
# directory (see engine.py); sys.path[0] is this directory.
from engine import require_engine

# ...and the fixtures are shared with the driver the other two suites run, so
# "the rows python checks" and "the rows the family checks" are one list.
sys.path.insert(1, os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                "..", "lib"))
from check_fp32_nan import build_table, parse_fp32_field  # noqa: E402

# Chunk widths for the streaming surface. 1 puts a boundary between every pair
# of bytes; 3 lands inside the 4-byte fp32 word rather than beside it (a
# reassembly path that drops or re-orders payload bytes shows up here and not at
# 1); 4096 is the whole message in one feed, the reference the other two must
# match.
CHUNK_SIZES = (1, 3, 4096)

# The message the shared table is built from -- example.yaml's two fp32
# positions, `somefp32` (id 8, default 0) and `somefloatarray` (id 17, an
# fp32[3] with a DECLARED default, which is what makes the §2 array row
# expressible here at all).
MESSAGE = "myfirstmessage"
SCALAR = "somefp32"
ARRAY = "somefloatarray"

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


def check_values(what, msg, row):
    """The MATERIALIZED walk (§6.5): the value the caller reads must carry the
    payload, not merely report `NaN`.

    Compared as fp64 bit patterns rather than with `==`, because no NaN compares
    equal to itself and `0.0 == -0.0`.
    """
    got = getattr(msg, row.field)
    values = [got] if row.kind == "scalar" else list(got)
    if len(values) != len(row.words):
        fail(what, "%s materialized %d element(s), expected %d"
                   % (row.field, len(values), len(row.words)))
        return
    for i, (value, word) in enumerate(zip(values, row.words)):
        try:
            bits = struct.unpack("<Q", struct.pack("<d", value))[0]
        except (struct.error, TypeError) as exc:
            fail(what, "%s[%d] is not a float (%r): %r" % (row.field, i, value, exc))
            continue
        want = widened(word)
        if bits != want:
            fail(what, "the materialized value at %s[%d] lost fp32 payload bits"
                       % (row.field, i))
            print("  wire word 0x%08X" % word)
            print("  want fp64 0x%016X" % want)
            print("  got  fp64 0x%016X" % bits)


def check_wire(what, msg, row):
    """Re-encode a decoded message and diff the bytes.

    The shared driver makes this assertion on the one-shot `recode` verb; it is
    repeated here because the STREAMING surface has no such verb, and running
    the same oracle on both surfaces is what makes them comparable.

    A row that is not `exact` carried a value equal to the field's default, so
    §2 normalization must drop the field and the re-encode is the EMPTY message.
    Anything else -- including a re-encode that raises -- is a failure.
    """
    try:
        out = msg.encode()
    except Exception as exc:  # noqa: BLE001 -- any failure here is this check's
        fail(what, "re-encoding the decoded message raised %r" % (exc,))
        return
    if row.exact:
        if out != row.wire:
            fail(what, "not bit-exact -- an fp32 payload was altered")
            print("  want %s" % row.wire.hex())
            print("  got  %s" % out.hex())
    elif out:
        fail(what, "a value equal to the field's default must re-encode as the "
                   "empty message (MESSAGE_SPEC §2), got %s" % out.hex())


def check(what, msg, row):
    check_values(what, msg, row)
    check_wire(what, msg, row)


def main(argv):
    if len(argv) != 4:
        print("\n".join(__doc__.strip().splitlines()[-2:]))
        return 2
    if not require_engine(argv[2]):
        return 2
    print("   engine: sofab.IMPL = %s" % sofab.IMPL)
    rows = build_table(
        parse_fp32_field(argv[3], MESSAGE, SCALAR, want_array=False),
        parse_fp32_field(argv[3], MESSAGE, ARRAY, want_array=True),
    )
    sys.path.insert(0, argv[1])
    # The harness's own message map, so this driver and the generated project
    # agree on which class a message name means without a second copy of the
    # backend's name-exporting rule.
    import harness  # noqa: E402 -- the generated project is only on the path now

    for row in rows:
        cls = harness.MESSAGES[row.message]

        # ---- surface 1: the one-shot path ---------------------------------
        try:
            msg = cls.decode(row.wire)
        except Exception as exc:  # noqa: BLE001
            fail("one-shot %s" % row.label, "must decode, raised %r" % (exc,))
            continue
        check("one-shot %s" % row.label, msg, row)

        # ---- surface 2: the streaming path, at several splits --------------
        # A narrowing that only runs on the whole-payload delivery path would
        # pass leg 1 while a split fp32 word came back quieted.
        for size in CHUNK_SIZES:
            dec = cls.decoder()
            st = None
            for i in range(0, len(row.wire), size):
                st = dec.feed(row.wire[i:i + size])
            if st is not Status.COMPLETE:
                fail("streaming(chunk=%d) %s" % (size, row.label),
                     "reported %s, expected COMPLETE" % (st,))
                continue
            check("streaming(chunk=%d) %s" % (size, row.label), dec.message, row)

    if failures:
        return 1
    print("   fp32 value + streaming oracles: %d shared fixtures x (1 one-shot "
          "+ %d chunk sizes) x (value + re-encode)"
          % (len(rows), len(CHUNK_SIZES)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
