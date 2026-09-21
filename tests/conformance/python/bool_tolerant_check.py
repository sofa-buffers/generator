#!/usr/bin/env python3
"""CORELIB_PLAN §4.4 through generated Python, in all three boolean routes
(generator#590).

§4.4 is CANONICAL on encode and TOLERANT on decode: an encoder writes ``true``
as ``1``; a decoder reads **every** value other than ``0`` as ``true``, keeps no
trace of the raw value (a re-encode emits ``1``), and bounds a boolean by no
width at all -- ``256`` and ``2^64-1`` are ``true``, never INVALID and never
truncated to ``false``. Unlike an ``enum`` or a ``bitfield``, a boolean carries
no declared width for the rule to be applied against.

Generated Python reaches that rule by three different routes, and they are
implemented in three different places, so a check on one says nothing about the
others:

  * ``flag`` -- a bound SCALAR, through the table's ``.boolean`` entry: the
    corelib stores 0/1 in the slot and the scatter reads ``U[at] != 0``.
  * ``few``  -- a bound ARRAY, through ``.boolean_array`` (corelib-py#158).
    This is the route #590 moved off ``unsigned_array``. The corelib normalizes
    each element; the scatter converts int -> bool.
  * ``many`` -- an UNBOUND array (its count is past the backend's binding
    ceiling), through the visitor's ``on_unsigned_array``. The corelib hands the
    visitor RAW values BY DESIGN -- without a table it cannot know the field is a
    boolean -- so here the generated comprehension is what applies §4.4, and it
    has to stay.

WHY THE RAW VALUE IS INVISIBLE TO AN ORDINARY ROUND TRIP. A non-canonical
``true`` decodes to something truthy either way, and Python's ``bool`` IS an
``int``, so a slot that leaked a raw ``1`` compares equal to ``True`` and a JSON
dump prints ``true`` for both. This driver therefore asserts the STORED TYPE
(``type(v) is bool``) beside the value, and asserts the RE-ENCODED BYTES, which
is where a kept ``2`` becomes visible as ``02``.

THE FIXTURES ARE THE SHARED ONES. The ``boolean_tolerant`` block of the
corelibs' ``assets/test_vectors.json`` (crucible#189) is the authority on what
each raw value means, and every backend that implements §4.4 answers the same
table. Its vectors sit on field id 0; each one is re-tagged onto the id of the
route under test, which changes exactly the tag byte -- the payload, the count
and the element bytes are the shared ones, unedited.

A vector's ``requires`` tag is read for one thing only -- ``"array"``, which says
which routes the vector belongs on. The other tag in this block is ``"int64"``,
which excuses a build whose varint accumulator cannot hold ``2^64-1``; Python's
``int`` is unbounded and both engines accumulate into a 64-bit path that carries
it, so no vector here is skipped and none may be. Filtering on a tag a target
does satisfy is how a driver quietly stops testing what it claims to.

Sparse-canonical encoding (MESSAGE_SPEC §2) is the one place the expected
re-encode is NOT the block's ``reencoded_hex``: a value equal to its schema
default is omitted, so a scalar that decoded to ``false`` re-encodes to nothing
at all. That case is derived, never special-cased away -- see ``want_reencode``.

BOTH SURFACES. Every case runs one-shot and then fed ONE BYTE AT A TIME. The
array route needs it: ``boolean_array`` normalizes the slots when the array
COMPLETES, not per element, so a decode suspended inside the array resumes with
raw values already in the destination. A completed chunked decode must still
land canonical, and only a split feed reaches that state.

Usage: bool_tolerant_check.py <generated-project-dir> <native|python> <test_vectors.json>
       (asserts the engine it actually loaded; exits non-zero on failure)
"""
import json
import sys
from pathlib import Path

# The three routes: (field name, field id, "scalar" or "array", schema default).
ROUTES = [
    ("flag", 0, "scalar", False),
    ("few", 1, "array", []),
    ("many", 2, "array", []),
]

# The `sofab` package, bound by main() once sys.path carries the project.
SOFAB = None

# Values a CALLER may put in a boolean field that are not already 0/1. `2` and
# `256` are the wire values §4.4 is about; the rest are the objects `write_bool`
# documents it accepts, truth-tested exactly as `if` would test them.
NONCANONICAL = [2, 256, 2**64 - 1, 0, [], "x"]

failures = []


def fail(surface, what):
    print(f"FAIL: [{surface}] {what}")
    failures.append(what)


def retag(payload: bytes, field_id: int) -> bytes:
    """The block's bytes, moved from field id 0 onto ``field_id``.

    A field header is the varint ``(id << 3) | wire_type``. Every vector in the
    block sits on id 0, so its first byte IS the bare wire type; re-tagging is
    that byte with the id shifted in, and the ids here are small enough to keep
    the header one byte. Nothing after the header is touched.
    """
    wt = payload[0]
    if wt > 7:
        raise SystemExit(f"vector header {wt:#04x} is not a bare id-0 tag byte")
    tag = (field_id << 3) | wt
    if tag > 0x7F:
        raise SystemExit(f"re-tagged header {tag} would need a second varint byte")
    return bytes([tag]) + payload[1:]


def want_reencode(vec, field_id, default) -> bytes:
    """The bytes generated code must re-emit for this vector.

    The block's own ``reencoded_hex``, re-tagged -- except when the decoded value
    equals the field's schema default, which MESSAGE_SPEC §2 says is omitted from
    the wire entirely. That is derived from the expectation rather than listed,
    so a new vector in the block needs no edit here.
    """
    values = vec["expect"]["values"]
    got = values[0] if len(values) == 1 and not isinstance(default, list) else values
    if got == default:
        return b""
    return retag(bytes.fromhex(vec["expect"]["reencoded_hex"]), field_id)


def want_value(vec, kind):
    values = vec["expect"]["values"]
    return values[0] if kind == "scalar" else list(values)


def check(mod, surface, name, field_id, kind, default, vec, stream):
    """One vector, on one route, over one decode surface."""
    wire = retag(bytes.fromhex(vec["serialized_hex"]), field_id)
    label = f"{name}/{vec['name']}"

    try:
        if stream:
            d = mod.Bools.decoder()
            st = None
            for i in range(len(wire)):
                st = d.feed(wire[i : i + 1])
            if st is not SOFAB.Status.COMPLETE:
                fail(surface, f"{label}: chunked decode ended {st}, want COMPLETE")
                return
            msg = d.message
        else:
            msg = mod.Bools.decode(wire)
    except Exception as exc:
        # §4.4 gives a boolean no width, so no value on this table may be
        # REFUSED -- on either surface. Reported rather than raised, so one
        # rejected vector does not hide the verdict on the rest.
        fail(surface, f"{label}: decode raised {type(exc).__name__}: {exc}")
        return

    got = getattr(msg, name)
    want = want_value(vec, kind)
    if got != want:
        fail(surface, f"{label}: decoded {got!r}, want {want!r}")
        return

    # A raw slot that leaked through compares EQUAL to the bool it stands for
    # (Python's bool is an int), so the type is the oracle with teeth here.
    seen = [got] if kind == "scalar" else got
    for v in seen:
        if type(v) is not bool:
            fail(surface, f"{label}: element is {type(v).__name__} {v!r}, not bool")
            return

    out = msg.encode()
    exp = want_reencode(vec, field_id, default)
    if out != exp:
        fail(surface, f"{label}: re-encoded {out.hex()}, want {exp.hex()} (§4.4 is canonical on encode)")


def check_contract(mod, engine):
    """The three properties the generator's change RESTS on.

    Everything above is a round trip, and a round trip was already green before
    #590 -- generated code normalized booleans itself, so the table above passes
    either way. It pins the CONTRACT, not the change. These legs pin the
    change's own premises, which nothing else would notice breaking:

      1. `boolean_array` takes NO width. That is the whole safety argument for
         routing booleans through it: `unsigned_array` HAS an `elem_max`, and
         binding a boolean with a ceiling of 1 makes 256 INVALID, which §4.4
         forbids -- #581, in the C++ backend. If corelib-py ever gave
         `boolean_array` a width argument, the mistake would be back in reach and
         every test above would stay green.

      2. `write_bool_array` is BYTE-IDENTICAL to the 0/1 list the generated
         encoder used to build. #590 is explicitly a no-wire-change issue, on
         both engines, and this is that claim stated as bytes rather than
         asserted.

      3. A boolean array the CALLER filled with non-canonical values still goes
         out as 1/0 -- see the comment on that leg for why no re-encode above
         can see this.
    """
    import sofab

    b = sofab.Binding()
    for kw in ("elem_max", "max_value", "elem_min", "min_value"):
        try:
            b.boolean_array(0, at=0, cap=4, **{kw: 1})
        except TypeError:
            continue
        fail(f"corelib/{engine}", f"Binding.boolean_array accepted {kw}=: §4.4 gives a "
                                  f"boolean no width, and a ceiling of 1 makes 256 INVALID (#581)")

    # The ENCODE half of §4.4, from the OBJECT side. Everything above re-encodes
    # a message the decoder built, whose fields therefore already hold real
    # `bool`s -- and `write_unsigned_array` of a list of `bool` emits 1/0 too,
    # Python's bool being an int. So a boolean array wired to the plain unsigned
    # writer is indistinguishable there, and only a value the CALLER put in the
    # field can tell them apart: `2` and `256` go out raw through the unsigned
    # writer and as `1` through the boolean one. That is the rule -- "an encoder
    # MUST write true as 1" -- and this is where it is actually visible.
    obj = mod.Bools(few=NONCANONICAL, many=NONCANONICAL)
    try:
        wire = obj.encode()
    except Exception as exc:
        # The plain unsigned writer REFUSES a non-integer element, where
        # write_bool_array truth-tests it; so this is the same finding as a
        # non-canonical byte below, reported rather than raised.
        fail(f"encode/{engine}", f"encoding {NONCANONICAL!r} raised "
                                 f"{type(exc).__name__}: {exc} -- §4.4 tests an element "
                                 f"for truth exactly as `if` would")
        return
    for fid, n in ((1, "few"), (2, "many")):
        want = bytes([(fid << 3) | 3, len(NONCANONICAL)]) + bytes(
            1 if v else 0 for v in NONCANONICAL)
        if want not in wire:
            fail(f"encode/{engine}", f"{n}: encoding {NONCANONICAL!r} produced {wire.hex()}, "
                                     f"which does not carry the canonical {want.hex()} (§4.4)")

    # Both writers, same message, compared as bytes. The values include the
    # non-canonical truths §4.4 is about and the objects `write_bool` documents
    # it accepts (truth-tested exactly as `if` would test them).
    vals = [False, True, 2, 256, 2**64 - 1, 0, [], [0], None, "x"]
    canon = [1 if v else 0 for v in vals]

    def emit(write):
        out = []
        e = sofab.Encoder.over_buffer(bytearray(512), 0, lambda v: out.append(bytes(v)))
        write(e)
        e.flush()
        return b"".join(out)

    got = emit(lambda e: e.write_bool_array(7, vals))
    want = emit(lambda e: e.write_unsigned_array(7, canon))
    if got != want:
        fail(f"corelib/{engine}", f"write_bool_array emitted {got.hex()}, "
                                  f"write_unsigned_array of the same 0/1 list emitted {want.hex()}")


def main(argv):
    if len(argv) != 4:
        print(__doc__)
        return 2
    proj, engine, vectors = argv[1], argv[2], argv[3]

    sys.path.insert(0, proj)
    global SOFAB
    import sofab

    SOFAB = sofab

    # Asserted IN THIS PROCESS: the runner proved the engine for a different one,
    # and an accelerator that failed to import here would fall back to pure
    # Python and report a second pure leg as a native one (generator#451).
    if sofab.IMPL != engine:
        print(f"FAIL: this leg must run on the '{engine}' engine, but sofab.IMPL is '{sofab.IMPL}'")
        return 1
    import message as mod

    block = json.loads(Path(vectors).read_text()).get("boolean_tolerant")
    if not block:
        print(f"FAIL: {vectors} carries no 'boolean_tolerant' block (crucible#189)")
        return 1

    # Loud, never quiet: a driver that silently selects nothing passes while
    # testing nothing. Each route states how many vectors it must see.
    scalars = [v for v in block if "array" not in v.get("requires", [])]
    arrays = [v for v in block if "array" in v.get("requires", [])]
    if not scalars or not arrays:
        print(f"FAIL: the block has {len(scalars)} scalar and {len(arrays)} array vectors; both halves are required")
        return 1

    checked = 0
    for name, fid, kind, default in ROUTES:
        vecs = scalars if kind == "scalar" else arrays
        for stream in (False, True):
            surface = f"python/{engine} {'streamed' if stream else 'one-shot'}"
            for vec in vecs:
                check(mod, surface, name, fid, kind, default, vec, stream)
                checked += 1

    check_contract(mod, engine)

    want = (len(scalars) + 2 * len(arrays)) * 2
    if checked != want:
        print(f"FAIL: ran {checked} cases, expected {want}")
        return 1
    if failures:
        # Not "n of checked": the contract legs below are not vector cases, so a
        # ratio against `checked` would misreport which half of the run failed.
        print(f"FAIL: {len(failures)} §4.4 check(s) failed on the {engine} engine "
              f"({checked} vector cases were run)")
        return 1
    print(f"   [{engine}] {checked} §4.4 cases OK "
          f"({len(scalars)} scalar + {len(arrays)} array vectors x 3 routes x 2 surfaces), "
          f"plus the no-width binder and the byte-identical array writer")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
