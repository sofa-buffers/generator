#!/usr/bin/env python3
"""The generated union API (MESSAGE_SPEC §4.2, generator#608), driven directly.

check_union.py proves the WIRE: what a union encodes to and what a forged frame
decodes to. It reaches the API only through ``from_jsonable``/``to_jsonable``, so
nothing there would notice a getter that stores the default it returns, a
``mutable_<opt>()`` that resets an option already held, a setter that copies, or a
``clear()`` that forgets the default option's own default. This drives those
members on the driver's own schema (``check_union.py --emit-schema``):

  * a fresh union holds default_id at its own default (``UniU``: ``pt`` = x 7);
  * a getter of an option NOT held returns that option's default -- a fresh
    object for a struct/union/array option -- and stores nothing;
  * a setter selects and stores the reference it is given (no copy);
  * ``mutable_<opt>()`` is select-if-not-held: it builds the option at its
    default when another is held and returns the held object untouched when not;
  * ``clear()`` goes back to default_id at its default;
  * ``has_<opt>()``, ``which`` and the ``<OPT>_ID`` constants agree;
  * a union compares as a value (the dataclass ``__eq__``);
  * a $defs union split per default_id: ``UnionPickDefaultT`` and
    ``UnionPickDefaultN`` start at different options;
  * a gap in an array of unions is the element type's default_id at its default.

Usage: union_api_check.py <generated-project-dir> <native|python>
"""

import sys

import sofab

from engine import require_engine

failures = 0


def check(what, cond):
    global failures
    if not cond:
        print("FAIL: %s" % what)
        failures += 1


def main(argv):
    if len(argv) != 3:
        print(__doc__.strip().splitlines()[-1])
        return 2
    if not require_engine(argv[2]):
        return 2
    sys.path.insert(0, argv[1])
    import message as m  # noqa: E402 -- the generated project is only on the path now

    # ---- a fresh union: default_id at its own default -----------------------
    u = m.UniU()
    check("fresh UniU holds pt", u.which == m.UniU.PT_ID == 2 and u.has_pt())
    check("fresh UniU.pt is at its default (x 7)", u.pt.x == 7 and u.pt.y == 0)
    check("fresh UniU is default", u._is_default())
    check("fresh UniU encodes to nothing", m.Uni().encode() == b"")

    # ---- a getter of another option: its default, stored nowhere ------------
    check("num not held reads its default 5", u.num == 5 and not u.has_num())
    check("inner not held reads its own default (b -2)", u.inner.b == -2)
    detached = u.box
    detached.z = 99
    check("writing a detached default is lost", u.box.z == 3 and u.has_pt())
    arr = u.arr
    arr.append(1)
    check("appending to a detached array default is lost", u.arr == [] and u.has_pt())
    check("reading other options left the held one alone", u.which == m.UniU.PT_ID)

    # ---- a setter selects and keeps the reference ---------------------------
    box = m.UniUBox()
    box.z = 4
    u.box = box
    check("setter selects box", u.which == m.UniU.BOX_ID and u.has_box() and not u.has_pt())
    check("setter stores the reference, not a copy", u.box is box)
    box.z = 5
    check("an edit through the caller's reference is seen", u.box.z == 5)
    u.num = 5
    check("a scalar at its own default is held, not default",
          u.has_num() and not u._is_default())
    check("... and is written (forced)", u.encode() == bytes([0x00, 0x05]))

    # ---- mutable_<opt>(): select if not held --------------------------------
    pt = u.mutable_pt()
    check("mutable_pt selects pt at its default", u.has_pt() and pt.x == 7)
    pt.y = 3
    again = u.mutable_pt()
    check("mutable_pt of the held option returns it untouched", again is pt and again.y == 3)
    strs = u.mutable_strs()
    strs.append("ab")
    check("mutable_strs edits in place", u.has_strs() and u.strs == ["ab"])
    check("mutable_strs of the held option keeps its elements", u.mutable_strs() == ["ab"])
    inner = u.mutable_inner()
    check("mutable_inner selects inner at ITS default (b -2)", inner.has_b() and inner.b == -2)
    inner.a = 0
    check("inner's own switch", u.inner.has_a() and u.inner.a == 0)

    # ---- clear() -------------------------------------------------------------
    u.clear()
    check("clear() holds default_id at its default",
          u.has_pt() and u.pt.x == 7 and u.pt.y == 0 and u._is_default())
    check("clear() builds a fresh default, not the old pt", u.pt is not pt)

    # ---- value equality -----------------------------------------------------
    a, b = m.UniU(), m.UniU()
    a.s, b.s = "x", "x"
    check("two unions holding the same option and value are equal", a == b)
    b.s = "y"
    check("... and differ when the value does", a != b)
    b.num = 0
    check("... and when the option does", a != b)

    # ---- a $defs union split per default_id --------------------------------
    t, n = m.UnionPickDefaultT(), m.UnionPickDefaultN()
    check("Pick_default_t starts at t (k 2)", t.has_t() and t.t.k == 2)
    check("Pick_default_n starts at n (6)", n.has_n() and n.n == 6)
    msg = m.Uni()
    check("the omitted-default_id site is Pick_default_n", type(msg.po) is m.UnionPickDefaultN)
    check("the default_id: 1 site is Pick_default_t", type(msg.pf) is m.UnionPickDefaultT)

    # ---- an array of unions fills a gap with default_id ---------------------
    # v: element 1 holds i = 3, element 0 is a gap -> default_id s at "".
    v1 = m.UniVElem()
    v1.i = 3
    src = m.Uni()
    src.v = [m.UniVElem(), v1]
    got = m.Uni.decode(src.encode())
    check("a gap in v is s at its default", got.v[0].has_s() and got.v[0].s == "")
    check("the element after the gap holds i = 3", got.v[1].has_i() and got.v[1].i == 3)
    check("v2's gap is p at its NON-zero default (q 9)",
          # seq(3, seq(1, unsigned(0, 2))): element 0 is a gap
          m.Uni.decode(bytes.fromhex("1e" "0e" "00" "02" "07" "07")).v2[0].p.q == 9)

    if failures:
        print("union API: %d check(s) failed (engine %s)" % (failures, sofab.IMPL))
        return 1
    print("   [%s] union API OK" % sofab.IMPL)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
