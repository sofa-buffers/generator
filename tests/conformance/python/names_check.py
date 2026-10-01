"""Round-trip every message of the shared name-collision schema.

Usage: names_check.py <generated-project-dir>

tests/conformance/lib/names.yaml spells its messages like the names the
generated module already has -- sofab's imports (`status`, `visitor`,
`binding`), the module constants (`reassembly`, `unbounded`), keywords (`none`,
`self`), roles and paths (`m_a` beside `m.a`). A class that rebinds one of
those breaks the module at IMPORT or at the first decode, while the generator
exits 0, so every message here is built, encoded, decoded one-shot and
streamed back. Message `m` carries the paths and is checked against
names.json by run.sh; every other message not in FIXTURES has the one field
`x`.
"""

import sys

sys.path.insert(0, sys.argv[1])

import harness  # noqa: E402  (after the path it is found on)
import message  # noqa: E402
from sofab import Status  # noqa: E402

# The messages whose one field is not `x`: each holds a path clash.
FIXTURES = {
    "a": {"b_c": {"x": 7}},
    "a_b": {"c": {"y": 7}},
    "struct_point": {"p": {"x": 7, "y": -3}},
    # every $defs type a second time, each away from its default
    "m_a": {
        "x": 7,
        "pa": [{"x": 1, "y": -2}],
        "s1": {"pt": {"x": 3}},
        "s2": {"num": 4},
        "sd": {"other": 5},
        "col": 2,
        "fl": 3,
    },
}

checked = 0
for name, cls in harness.MESSAGES.items():
    if name == "m":
        continue
    want = FIXTURES.get(name, {"x": 7})
    obj = cls.from_jsonable(want)
    wire = obj.encode()
    got = cls.decode(wire).to_jsonable()
    assert got == want, (name, got)
    d = cls.decoder()
    st = Status.INCOMPLETE
    for i in range(len(wire)):
        st = d.feed(wire[i : i + 1])
    assert st is Status.COMPLETE, (name, st)
    assert d.message.to_jsonable() == want, (name, d.message.to_jsonable())
    checked += 1

# The module's own public names are still the module's: no class rebound them.
assert message.REASSEMBLY > message.MAX_FIELD_SPAN > 0
assert message.Status is Status
print("names: %d messages round-trip one-shot and streamed" % checked)
