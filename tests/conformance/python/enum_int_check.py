#!/usr/bin/env python3
"""An enum or bitfield field is an ``int`` in generated Python, at every position.

MESSAGE_SPEC §1 bounds an ``enum`` and a ``bitfield`` by the WIDTH their
declaration implies, not by the constants or flags they declare: ``5`` in an enum
declaring {0, 1, 2, 10} is valid, decodes as received and re-encodes unchanged.
The generated ``IntEnum`` / ``IntFlag`` class only holds the named constants
(``m.cen == Closed_Cen.Z`` works because an ``IntEnum`` member is an ``int``); it
is not the field's type. Naming it as the annotation would claim a closed set
that ``Closed_Cen(m.cen)`` refuses with ValueError for such a value, and a fresh
message would hold an enum MEMBER where a decoded one holds an ``int``.

So this driver asserts, over the ``closed`` message the shared declared-width
driver prints (tests/conformance/lib/check_declared_width_kinds.py: GAPPED
definitions, an enum and a bitfield at all six positions):

  * every field, struct member, union option and array element is annotated
    ``int`` (``list[int]``, ``list[list[int]]``);
  * a fresh message holds a plain ``int`` there (``type(x) is int``), a union
    option's getter included while another option is held, so a fresh and a
    decoded message hold the same type -- and are equal, with the same repr and
    the same JSON;
  * a message carrying in-width values no constant names decodes -- one-shot and
    fed one byte at a time -- into plain ints equal to the wire values, and
    re-encodes to the same bytes;
  * one forged scalar (``01 0a``: field 0, signed varint, zig-zag 5) ties the
    stored value to the wire independently of the generated encoder.

Usage: enum_int_check.py <generated-project-dir> <native|python>
       (asserts the engine it actually loaded; exits non-zero on failure)
"""
import json
import sys
import typing

from engine import require_engine

failures = 0


def check(what, ok):
    global failures
    if not ok:
        failures += 1
        print("FAIL: " + what)


def is_int(v):
    return type(v) is int


def all_ints(v):
    if isinstance(v, list):
        return all(all_ints(x) for x in v)
    return is_int(v)


def main(argv):
    if len(argv) != 3:
        print(__doc__)
        return 2
    if not require_engine(argv[2]):
        return 1
    sys.path.insert(0, argv[1])
    import message as m  # noqa: E402 -- the generated project is only on the path now
    import sofab  # noqa: E402

    # 1. The annotations.
    hints = typing.get_type_hints(m.Closed)
    for name, want in [("cen", int), ("cbf", int), ("cena", list[int]), ("cbfa", list[int]),
                       ("cmat", list[list[int]]), ("cmbf", list[list[int]])]:
        check("Closed.%s is annotated %r, want %r" % (name, hints.get(name), want), hints.get(name) == want)
    for cls, names in [(m.Closed_Cst, ("st_en", "st_bf")), (m.Closed_Csa, ("sa_en", "sa_bf"))]:
        h = typing.get_type_hints(cls)
        for name in names:
            check("%s.%s is annotated %r, want int" % (cls.__name__, name, h.get(name)), h.get(name) is int)
    for opt in ("un_en", "un_bf"):
        prop = getattr(m.Closed_Cun, opt)
        got = typing.get_type_hints(prop.fget).get("return")
        check("Closed_Cun.%s returns %r, want int" % (opt, got), got is int)
        got = typing.get_type_hints(prop.fset).get("v")
        check("Closed_Cun.%s's setter takes %r, want int" % (opt, got), got is int)

    # 2. A fresh message holds plain ints.
    fresh = m.Closed()
    for name in ("cen", "cbf"):
        v = getattr(fresh, name)
        check("fresh Closed.%s is %r (%s), want a plain int" % (name, v, type(v).__name__), is_int(v))
    for obj, name in [(m.Closed_Cst(), "st_en"), (m.Closed_Cst(), "st_bf"),
                      (m.Closed_Csa(), "sa_en"), (m.Closed_Csa(), "sa_bf"),
                      (m.Closed_Cun(), "un_en"), (m.Closed_Cun(), "un_bf")]:
        v = getattr(obj, name)
        check("fresh %s.%s is %r (%s), want a plain int" % (type(obj).__name__, name, v, type(v).__name__), is_int(v))

    # A union getter's NOT-HELD fallback is a plain int too: un_en is the
    # default option, so only another option being held reaches its fallback.
    c = m.Closed_Cun()
    c.un_bf = 4
    v = c.un_en
    check("Closed_Cun.un_en while un_bf is held is %r (%s), want a plain int" % (v, type(v).__name__),
          is_int(v) and v == 0)
    c = m.Closed_Cun()
    v = c.un_bf
    check("Closed_Cun.un_bf while un_en is held is %r (%s), want a plain int" % (v, type(v).__name__),
          is_int(v) and v == 0)

    # A fresh message and one decoded from its own encoding are the same value:
    # equal, the same repr, the same JSON.
    back = m.Closed.decode(fresh.encode())
    check("a fresh message != its decoded encoding: %r vs %r" % (fresh, back), fresh == back)
    check("a fresh message's repr differs from its decoded encoding's: %r vs %r" % (fresh, back),
          repr(fresh) == repr(back))
    jf = json.dumps(fresh.to_jsonable(), sort_keys=True)
    jb = json.dumps(back.to_jsonable(), sort_keys=True)
    check("a fresh message's JSON differs from its decoded encoding's: %s vs %s" % (jf, jb), jf == jb)

    # The holders still hold the constants, and compare with the int field.
    check("Closed_Cen.Z is 10", m.Closed_Cen.Z == 10)
    check("Closed_Cbf.D is 1 << 3", m.Closed_Cbf.D == 8)

    # 3. In-width values no constant names, at every position.
    src = m.Closed()
    src.cen = 5
    src.cbf = 4
    src.cena = [5, -128, 127]
    src.cbfa = [4, 255]
    src.cst.st_en = 5
    src.cst.st_bf = 4
    src.csa = [m.Closed_Csa(sa_en=5, sa_bf=4)]
    src.cmat = [[5, 127]]
    src.cmbf = [[4, 255]]
    for opt, val in (("un_en", 5), ("un_bf", 4)):
        setattr(src.cun, opt, val)
        wire = src.encode()
        decoded = [("one-shot", m.Closed.decode(wire))]
        d = m.Closed.decoder()
        st = None
        for i in range(len(wire)):
            st = d.feed(wire[i:i + 1])
        check("[%s] chunked decode ended %s, want COMPLETE" % (opt, st), st is sofab.Status.COMPLETE)
        decoded.append(("streamed", d.message))
        for surface, got in decoded:
            tag = "%s/%s" % (opt, surface)
            for name, want in [("cen", 5), ("cbf", 4), ("cena", [5, -128, 127]), ("cbfa", [4, 255]),
                               ("cmat", [[5, 127]]), ("cmbf", [[4, 255]])]:
                v = getattr(got, name)
                check("[%s] Closed.%s decoded %r, want %r as plain ints" % (tag, name, v, want),
                      v == want and all_ints(v))
            for obj, name, want in [(got.cst, "st_en", 5), (got.cst, "st_bf", 4),
                                    (got.csa[0] if got.csa else None, "sa_en", 5),
                                    (got.csa[0] if got.csa else None, "sa_bf", 4)]:
                v = getattr(obj, name, None)
                check("[%s] %s decoded %r, want the plain int %d" % (tag, name, v, want), is_int(v) and v == want)
            v = getattr(got.cun, opt)
            check("[%s] Closed_Cun.%s decoded %r, want the plain int %d" % (tag, opt, v, val),
                  got.cun.which == getattr(m.Closed_Cun, opt.upper() + "_ID") and is_int(v) and v == val)
            check("[%s] re-encodes unchanged" % tag, got.encode() == wire)

    # 4. One forged scalar: field 0 (cen), signed varint, zig-zag(5) = 10.
    wire = bytes.fromhex("010a")
    got = m.Closed.decode(wire)
    check("forged 01 0a decoded cen=%r (%s), want the plain int 5" % (got.cen, type(got.cen).__name__),
          is_int(got.cen) and got.cen == 5)
    check("forged 01 0a re-encodes unchanged", got.encode() == wire)
    named = m.Closed.decode(bytes.fromhex("0114"))
    check("decoded cen=10 compares equal to Closed_Cen.Z", named.cen == m.Closed_Cen.Z and is_int(named.cen))

    if failures:
        print("enum/bitfield as int: %d check(s) failed (engine %s)" % (failures, sofab.IMPL))
        return 1
    print("enum/bitfield as int: OK -- annotations, fresh defaults, decode and re-encode at all six positions (engine %s)"
          % sofab.IMPL)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
