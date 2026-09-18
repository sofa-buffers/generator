#!/usr/bin/env python3
"""The destination table decodes to the same message the visitor would have built.

Part of a generated class is decoded through a corelib-py ``Binding`` handed over
by ``Visitor.destinations()``: those fields are written straight into a slot, with
no callback, and one ``scatter()`` moves them onto the dataclass when the decode
completes (ARCHITECTURE §9.5.1, generator#561). Everything the table cannot carry
stays on the flat visitor, and the two run in ONE decoder.

That split is invisible in a round-trip, which is why this file exists: every case
below is one where a table could plausibly disagree with the hook it replaced, and
four of them are cases the round-trip suites cannot reach at all.

  A. An UNKNOWN id inside a scope the table entered. The decoder descends into a
     bound sequence by itself and tells the visitor nothing (corelib-py#146), so
     the visitor's idea of "where am I" still names the parent -- and an unknown
     id inside the child is offered to it under the PARENT's location. The schema
     below is built so that the id used inside the struct is also a declared id of
     the message (3), which is exactly the collision that would put the nested
     value in the root's field. The backend's rule is that a scope may be entered
     only when its parent binds everything it declares, so the parent has no arm
     to misfire; this asserts the outcome that rule exists for.

  B. An EMPTY array replaces a non-empty default. A count slot holds the wire's
     element count, so zero is a real answer -- which is why the storage starts
     filled with a sentinel no arrival can write, rather than with zeroes.

  C. An ABSENT field keeps the dataclass default, for both a scalar and an array.

  D. §7.4, for the three shapes the table carries: a repeated scalar, string and
     array each take the LAST occurrence, and the array REPLACES.

  E. §7.3: a header whose wire type contradicts a bound id is skipped by the
     corelib's own tag test on the entry, ahead of the bound the entry declares,
     and the field keeps its default. Nothing is materialized and the decode stays
     COMPLETE.

  F. A decode short of COMPLETE publishes NO table field: the scatter runs after
     the verdict, so a refused message cannot leave half a value on the object.

  G. Two scopes whose paths spell the same name (``dup.inner`` beside the sibling
     ``dup_inner``) each decode into their own object. A scope is named after its
     path, both the dispatch location and the table are named from it, and a
     duplicate name means the later module-level assignment wins -- so one scope
     would decode into the other's destination, silently.

  H. A field inside a WRAPPER-ARRAY element whose id the root's table also names
     stays in the element. The table is suppressed for the duration of a scope the
     visitor entered, and the element declares a nested struct before that field,
     so this is the shape that catches the suppression being lifted one scope too
     early -- which it was, in corelib-py's pure engine (corelib-py#152): the
     element's value landed in the message's own field and the element kept the
     default.

  I. The same message fed one byte at a time lands identically -- the table's
     resume path crosses chunk boundaries inside a bound value.

Usage: destination_table_check.py <project-dir> <native|python>
"""

import sys
import os


def main(argv):
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    proj, want_engine = argv[1], argv[2]
    sys.path.insert(0, proj)

    import sofab
    from sofab import Encoder

    # Asserted in THIS process: the caller proved it for a different one, and an
    # accelerator that failed to import here would silently repeat the pure leg.
    if sofab.IMPL != want_engine:
        print("FAIL: expected the %s engine, got %s" % (want_engine, sofab.IMPL),
              file=sys.stderr)
        return 1

    import message

    failures = []

    def check(name, got, expected):
        if got != expected:
            failures.append("%s: got %r, expected %r" % (name, got, expected))

    def wire(build):
        buf = bytearray(512)
        e = Encoder.over_buffer(buf)
        build(e)
        return bytes(buf[:e.bytes_used()])

    # --- A: an unknown id inside a bound scope, colliding with a root id ------
    def nested(e):
        e.write_unsigned(3, 111)            # the message's own `tag`
        e.write_sequence_begin_lazy(6)      # `inner`, a scope the table enters
        e.write_unsigned(3, 222)            #   inner.a -- id 3 INSIDE the struct
        e.write_unsigned(9, 999)            #   an id the schema does not declare
        e.write_sequence_end()
    m = message.E.decode(wire(nested))
    check("A/root field untouched by a nested id", m.tag, 111)
    check("A/nested field lands in the struct", m.inner.a, 222)

    # --- B: an empty array replaces a non-empty default ----------------------
    def empty(e):
        e.write_unsigned(3, 1)
        e.write_unsigned_array(7, [])
    check("B/empty array replaces the default", message.E.decode(wire(empty)).nums, [])

    # --- C: absence keeps the default ----------------------------------------
    def only_tag(e):
        e.write_unsigned(3, 1)
    m = message.E.decode(wire(only_tag))
    check("C/absent array keeps the default", m.nums, [9, 9, 9])
    check("C/absent string keeps the default", m.name, "")

    # --- D: §7.4, last occurrence wins ---------------------------------------
    def repeated(e):
        e.write_unsigned(3, 1)
        e.write_unsigned(3, 2)
        e.write_string(5, "first")
        e.write_string(5, "second")
        e.write_unsigned_array(7, [1, 2, 3])
        e.write_unsigned_array(7, [4])
    m = message.E.decode(wire(repeated))
    check("D/repeated scalar", m.tag, 2)
    check("D/repeated string", m.name, "second")
    check("D/repeated array replaces", m.nums, [4])

    # --- E: §7.3, a contradicting tag on a bound id is skipped ---------------
    def mistyped(e):
        e.write_string(3, "not an unsigned")   # id 3 is declared u64
        e.write_unsigned(4, 7)                 # id 4 is declared fp64
        e.write_unsigned(5, 5)                 # id 5 is declared string
    m = message.E.decode(wire(mistyped))
    check("E/mismatched tags skipped, defaults kept", (m.tag, m.ratio, m.name), (0, 0.0, ""))

    # --- F: a refused decode publishes nothing -------------------------------
    def cut_inside_a_payload(e):
        e.write_unsigned(3, 5)
        e.write_string(5, "a longer string")
    truncated = wire(cut_inside_a_payload)[:-4]
    try:
        message.E.decode(truncated)
        failures.append("F/truncation must raise SofaIncompleteError")
    except sofab.SofaIncompleteError:
        pass
    dec = message.E.decoder()
    st = dec.feed(truncated)
    check("F/refused decode publishes no table field",
          (st is message.Status.INCOMPLETE, dec.message.tag), (True, 0))

    # --- G: two scopes whose paths spell one name ----------------------------
    o = message.E()
    o.dup.inner.k = 11
    o.dup_inner.k = 22
    m = message.E.decode(o.encode())
    check("G/colliding scope names stay apart", (m.dup.inner.k, m.dup_inner.k), (11, 22))

    # --- H: an element field whose id the root table also names ---------------
    o = message.E()
    o.ratio = 1.5
    row = message.ERowsElem()
    row.when.k = 99
    row.ratio = 42.25
    o.rows = [row]
    m = message.E.decode(o.encode())
    check("H/element field stays in the element", (m.ratio, m.rows[0].ratio), (1.5, 42.25))

    # --- I: the same message, one byte per feed ------------------------------
    full = wire(repeated)
    dec = message.E.decoder()
    st = None
    for i in range(len(full)):
        st = dec.feed(full[i:i + 1])
    check("I/streamed one byte at a time",
          (st is message.Status.COMPLETE, dec.message.tag, dec.message.name, dec.message.nums),
          (True, 2, "second", [4]))

    if failures:
        for f in failures:
            print("FAIL: [%s] %s" % (want_engine, f), file=sys.stderr)
        return 1
    print("   [%s] destination table: 9 cases (nested unknown id, empty vs absent, "
          "§7.4 x3, §7.3 skip, refused decode, colliding scope names, element id vs "
          "root id, byte-at-a-time)" % want_engine)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
