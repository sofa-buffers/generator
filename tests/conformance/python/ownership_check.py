#!/usr/bin/env python3
"""A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412).

The rule: no value the codec delivers may outlive the callback it arrived in.
§6.0 fixes that for ``feed`` -- a chunk is borrowed only for the duration of the
call, so once it returns the caller may reuse, overwrite or free that memory and
the decoded message must not be affected -- and §6.7.1 gives the one-shot path no
exemption: ``decode(buffer)`` copies too, because a message whose lifetime
depends on which entry point produced it is the divergence §6.7 ends. The
generated ``decode`` docstring states the rule in prose; this is the test.

Nothing else in this suite reaches it. Every other decode here hands the harness
a buffer that stays alive and unmodified for the whole run -- the chunk-invariance
row (generator#413) included, which compares two readers of the SAME live bytes
and would see identical values out of an aliased destination.

WHY THIS PORT NEEDS TWO ORACLES, NOT ONE. The scrub-the-input oracle the other
suites use is PARTLY VACUOUS in Python, and a translated copy of it would be a
false pass. ``Decoder.feed`` puts the chunk where the walk can reach it through
``_reassemble``, and with nothing carried that is::

    buf = data if isinstance(data, bytes) else bytes(data)

-- so a ``bytearray`` or ``memoryview`` handed to a one-shot decode is COPIED at
the front door (the native engine's ``_rebind`` does the same), and scrubbing the
caller's object afterwards can never reach what the codec walked. Measured: with
corelib-py mutated to hand the visitor ``memoryview(buf)[pos:end]`` -- a real
§6.7 violation -- both one-shot scrub legs still passed while
``type(msg.someblob)`` was literally ``memoryview``.

Only a real ``bytes`` input is ADOPTED uncopied, and a ``bytes`` cannot be
scrubbed. So the deterministic half of this check is a leg that decodes from
``bytes`` and asserts the DESTINATION TYPES: a slice of a ``bytes`` is itself a
``bytes`` (the language copies), so anything that is not ``str``/``bytes``/``list``
in a destination is a window the codec handed out. That is the leg that caught
the mutation above.

KNOWN REACH -- do not read a pass as "every field is copied":

  * Leg 1 (scrub a mutable one-shot input) is blunted by the front-door copy
    described above. It is kept because it is what a caller actually does and it
    costs nothing, not because it can fail.
  * Leg 2 (destination types out of a ``bytes`` input) is the one with teeth on
    the one-shot path, and its reach is exactly "the corelib stopped copying" --
    the generated visitor stores the delivered value unchanged
    (``self._o.someblob = value``), so the copy is entirely the corelib's.
  * Leg 3 (streaming out of a reusable scratch) does bite, but its sensitivity
    comes from the corelib's reassembly buffer being REUSED across feeds, not
    from the caller's scrub, so it is order-dependent. It sweeps chunk sizes for
    the family reason: a payload SPLIT across chunks is reassembled and copied
    out whether or not anything wanted a view, so a small-chunk-only feed is
    structurally unable to fail.
  * A native array (someuintarray, somefloatarray, ...) is built as a ``list`` of
    decoded scalars and never passes through a payload callback at all.
  * The scribble byte is 0x41 ('A'), not 0xff, for the reason the family settled
    on: an aliased string destination must still RE-ENCODE, so the oracle stays a
    byte diff and never becomes a UTF-8 error that unrelated causes could produce.

Usage: ownership_check.py <generated-project-dir>   (exits non-zero on failure)
"""

import sys

from sofab import Status

# See the header note: an aliased string destination must still encode.
SCRIBBLE = 0x41

# The sweep ends at a size larger than the whole message: only a chunk at least
# as long as a payload reaches the corelib's whole-payload delivery path.
CHUNK_SIZES = (1, 7, 16, 32, 64, 4096)

failures = 0


def fail(what, detail):
    global failures
    print("FAIL: %s: %s" % (what, detail))
    failures += 1


def sample(message):
    """Fill every aliasing-capable field kind: string, blob, array<string>,
    array<blob>, a string nested in a struct, a string in a union and the string
    key of a dynamic wrapper-array row -- plus the native arrays, which are here
    so the wire carries them, not because they can alias."""
    m = message.Myfirstmessage()
    m.somestring = "héllo wörld payload"
    m.someblob = b"\x01\x02\x03\x04\x05"
    m.someuintarray = [9, 8, 7, 6]
    m.somefloatarray = [1.5, -2.5, 3.5]
    m.somestringarray = ["a", "bb", "ccc"]
    m.someblobarray = [b"\x09\x09", b"\x08"]
    m.somestruct.nestedstring = "nested payload"
    m.someunion.option2 = "union payload"
    m.somemap = [
        message.MyfirstmessageSomemapElem(key="first key", value=1),
        message.MyfirstmessageSomemapElem(key="second key", value=2),
    ]
    return m


def must_match(what, want, got):
    """Re-encode and diff. Comparing bytes rather than fields is the stronger
    statement anyway: two messages that encode identically ARE the same message
    on the wire. A re-encode that RAISES counts as a failure of this check too --
    the encoder validates its input, so a scribbled destination can surface as an
    exception rather than as different bytes."""
    try:
        re = got.encode()
    except Exception as exc:  # noqa: BLE001 -- any failure here is this check's
        fail(what, "re-encoding the decoded message raised %r" % (exc,))
        return
    if re != want:
        fail(what, "a decoded field aliased the buffer it was decoded from")
        print("  want %s" % want.hex())
        print("  got  %s" % re.hex())
        print("  somestring = %r  someblob = %s" % (got.somestring, bytes(got.someblob).hex()))
        for i, b in enumerate(got.someblobarray):
            print("  someblobarray[%d] = %s" % (i, bytes(b).hex()))


def check_types(what, got):
    """The leg the front-door copy cannot blunt. A slice of a ``bytes`` is a
    ``bytes``; a window into one is a ``memoryview``. So the destination's TYPE
    is what distinguishes a copy from a borrow when the input is immutable."""
    want = [
        ("somestring", got.somestring, str),
        ("someblob", got.someblob, bytes),
        ("somestruct.nestedstring", got.somestruct.nestedstring, str),
        ("someunion.option2", got.someunion.option2, str),
        ("someuintarray", got.someuintarray, list),
    ]
    for i, s in enumerate(got.somestringarray):
        want.append(("somestringarray[%d]" % i, s, str))
    for i, b in enumerate(got.someblobarray):
        want.append(("someblobarray[%d]" % i, b, bytes))
    for i, e in enumerate(got.somemap):
        want.append(("somemap[%d].key" % i, e.key, str))
    for name, value, kind in want:
        if type(value) is not kind:
            fail(what, "%s is a %s, not a %s -- the codec handed out a window "
                       "into the input buffer" % (name, type(value).__name__, kind.__name__))


def main(argv):
    if len(argv) != 2:
        print(__doc__.strip().splitlines()[-1])
        return 2
    sys.path.insert(0, argv[1])
    import message  # noqa: E402 -- the generated project is only on the path now

    want = sample(message).encode()
    if not want:
        print("FAIL: the sample encoded to nothing")
        return 2

    # ---- 1. one-shot, out of MUTABLE storage scrubbed on return ------------
    # §6.7.1: `data` may be reused or overwritten the moment decode returns.
    # See the header: the front door copies a non-`bytes` chunk, so this leg is
    # what a caller does rather than what a regression trips.
    wire = bytearray(want)
    got = message.Myfirstmessage.decode(wire)
    wire[:] = bytes([SCRIBBLE]) * len(wire)
    must_match("one-shot decode(bytearray)", want, got)

    wire2 = bytearray(want)
    got2 = message.Myfirstmessage.decode(memoryview(wire2))
    wire2[:] = bytes([SCRIBBLE]) * len(wire2)
    must_match("one-shot decode(memoryview)", want, got2)

    # ---- 2. one-shot out of `bytes`, which the decoder ADOPTS uncopied -----
    # The only input the codec walks in place, so the only one whose destinations
    # can be windows -- and being immutable, the only thing that can tell is the
    # destination's type.
    got3 = message.Myfirstmessage.decode(want)
    must_match("one-shot decode(bytes)", want, got3)
    check_types("one-shot decode(bytes)", got3)

    # ---- 3. streaming, every chunk out of ONE reusable scratch -------------
    # §6.0: the borrow ends when feed returns, so the scratch is destroyed there
    # and reused for the next chunk -- which is what a caller reading a socket
    # into a fixed buffer actually does.
    for size in CHUNK_SIZES:
        scratch = bytearray(size)
        dec = message.Myfirstmessage.decoder()
        st = None
        for i in range(0, len(want), size):
            n = min(size, len(want) - i)
            scratch[:n] = want[i:i + n]
            st = dec.feed(memoryview(scratch)[:n])
            scratch[:] = bytes([SCRIBBLE]) * size
        if st is not Status.COMPLETE:
            fail("streaming feed(chunk=%d)" % size,
                 "reported %s, expected COMPLETE" % (st,))
            continue
        # There is no finish(): the corelib publishes the outcome on feed's
        # return and the half-built message is on .message throughout.
        must_match("streaming feed(chunk=%d)" % size, want, dec.message)
        check_types("streaming feed(chunk=%d)" % size, dec.message)

    if failures:
        return 1
    print("   ownership: %d bytes, decoded message owns them -- 3 one-shot legs "
          "+ %d chunk sizes" % (len(want), len(CHUNK_SIZES)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
