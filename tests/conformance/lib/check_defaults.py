#!/usr/bin/env python3
"""A default declared inside a struct is what absence means -- at every depth.

Usage:
  check_defaults.py --emit-schema
  check_defaults.py <label> [--cwd DIR] [--message NAME] -- <harness argv...>

## The gap this closes (generator#609)

MESSAGE_SPEC §2: a field equal to its default is not written, and an absent
field IS its default. A default declared inside a struct -- one level down, two
levels down, or inside a struct array's element -- has to hold on both sides of
that rule, or they disagree:

* the encoder omits a member only when it equals the schema default, so a fresh
  message whose nested members sit at the language's zero value instead is NOT
  empty on the wire;
* the decoder reconstructs an absent member from whatever the fresh object
  holds, so a zero there reads a nested default back as 0;
* an interior struct-array element equal to its default is omitted (§5.1) and
  refilled from the element default -- zero-filled, `[9, 1]` arrives as `[0, 1]`.

The Go backend did all three, while examples/messages/example.yaml carried
non-zero nested defaults (`nestedint: 255`, `deepint: -123456`) the whole time.
No suite saw it, because every check compared a backend's output with that same
backend's output: the round-trip baseline is `{}` encoded and decoded BY THE
BACKEND UNDER TEST, and a baseline wrong in the same way as the decoder passes.

## What is asserted -- nothing here is taken from the backend under test

1. `{}` encodes to the EMPTY byte string (a wire fact, §2).
2. The empty byte string decodes to the defaults THIS FILE's schema declares:
   the expectations are written below, next to the schema that declares them.
3. That decoded message re-encodes to the empty byte string again.
4. A struct array `[default, x]` round-trips as `[default, x]`: the interior
   default element is omitted on the wire and must come back as the element
   default, not as zero.
5. An explicit zero where the default is not zero is a VALUE: it is written and
   round-trips as 0, so seeding defaults must not paper over it.

## Loud, never quiet

Every case must run; a harness failure is a failure; the case count is printed.
"""
import json
import subprocess
import sys

MSG = "dflt"

# The declared defaults, in the shape a harness renders them. Kept beside the
# schema text so the two cannot drift apart.
DEFAULTS = {
    "top": 7,
    "s": {"a": 5, "t": "hi", "inner": {"b": -3}},
}
ELEM_DEFAULT_C = 9


def emit_schema() -> int:
    print("# Non-zero defaults below the top level (generator#609), printed by")
    print("# tests/conformance/lib/check_defaults.py so the schema and the")
    print("# expectations the driver checks against have one definition between them.")
    print(f"  {MSG}:")
    print("    payload:")
    print("      top: { id: 0, type: u16, default: 7 }")
    print("      s:")
    print("        id: 1")
    print("        type: struct")
    print("        fields:")
    print("          a: { id: 0, type: u8, default: 5 }")
    print('          t: { id: 1, type: string, maxlen: 8, default: "hi" }')
    print("          inner:")
    print("            id: 2")
    print("            type: struct")
    print("            fields:")
    print("              b: { id: 0, type: i32, default: -3 }")
    print("      arr:")
    print("        id: 2")
    print("        type: array")
    print("        items:")
    print("          type: struct")
    print("          count: 3")
    print("          fields:")
    print(f"            c: {{ id: 0, type: u8, default: {ELEM_DEFAULT_C} }}")
    return 0


def run(cmd, argv, stdin, cwd):
    return subprocess.run(cmd + argv, input=stdin, cwd=cwd,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def diag(p) -> str:
    return p.stderr.decode(errors="replace").strip()[:600]


def num(v):
    """A harness may render an integer as a JSON number or as a string."""
    if isinstance(v, str):
        try:
            return int(v)
        except ValueError:
            return v
    return v


def lookup(obj, path):
    for key in path:
        if not isinstance(obj, dict) or key not in obj:
            return None, False
        obj = obj[key]
    return obj, True


def leaves(d, prefix=()):
    for k, v in d.items():
        if isinstance(v, dict):
            yield from leaves(v, prefix + (k,))
        else:
            yield prefix + (k,), v


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()
    if "--" not in argv:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    if not head or not cmd:
        print("FAIL: need a label and a harness argv after `--`", file=sys.stderr)
        return 2
    label = head[0]
    cwd = None
    msg = MSG
    i = 1
    while i < len(head):
        if head[i] == "--cwd":
            cwd = head[i + 1]
            i += 2
        elif head[i] == "--message":
            msg = head[i + 1]
            i += 2
        else:
            print(f"FAIL: unknown option {head[i]!r}", file=sys.stderr)
            return 2

    failures = []
    cases = 0

    def encode(obj, what):
        p = run(cmd, ["encode", msg], json.dumps(obj).encode(), cwd)
        if p.returncode != 0:
            failures.append(f"{what}: encode failed: {diag(p)}")
            return None
        return p.stdout

    def decode(wire, what):
        p = run(cmd, ["decode", msg], wire, cwd)
        if p.returncode != 0:
            failures.append(f"{what}: decode failed ({len(wire)} bytes): {diag(p)}")
            return None
        try:
            return json.loads(p.stdout.decode())
        except ValueError:
            failures.append(f"{what}: decode printed no JSON: {p.stdout[:200]!r}")
            return None

    # 1. a fresh message is the empty message
    cases += 1
    wire = encode({}, "fresh message")
    if wire is not None and wire != b"":
        failures.append("a message holding only its defaults must encode to ZERO bytes"
                        f" (MESSAGE_SPEC §2), got {len(wire)}: {wire.hex(' ')}")

    # 2. absence decodes to the declared defaults -- the schema's, not the backend's
    cases += 1
    got = decode(b"", "empty input")
    if got is not None:
        for path, want in leaves(DEFAULTS):
            have, ok = lookup(got, path)
            if not ok:
                failures.append(f"empty input: {'.'.join(path)} missing from the decoded message")
            elif num(have) != want:
                failures.append(f"empty input: {'.'.join(path)} decoded as {have!r},"
                                f" the schema declares {want!r}")
        arr, ok = lookup(got, ("arr",))
        if ok and arr not in (None, []):
            failures.append(f"empty input: arr decoded as {arr!r}, the declared default is []")

        # 3. ...and re-encodes to nothing again
        cases += 1
        again = encode(got, "decoded empty input")
        if again is not None and again != b"":
            failures.append("the decoded empty message must re-encode to ZERO bytes,"
                            f" got {len(again)}: {again.hex(' ')}")

    # 4. an interior default element comes back as the element default
    cases += 1
    sent = [{"c": ELEM_DEFAULT_C}, {"c": 1}]
    wire = encode({"arr": sent}, "struct array [default, x]")
    if wire is not None:
        back = decode(wire, "struct array [default, x]")
        if back is not None:
            arr, _ = lookup(back, ("arr",))
            cs = [num(e.get("c")) if isinstance(e, dict) else e for e in (arr or [])]
            if cs != [ELEM_DEFAULT_C, 1]:
                failures.append(f"struct array: sent c={[ELEM_DEFAULT_C, 1]}, got c={cs}"
                                " -- an interior element omitted for equalling its default"
                                " must be refilled with the element default (§5.1)")

    # 5. an explicit zero is a value, not a default
    cases += 1
    wire = encode({"top": 0, "s": {"a": 0, "inner": {"b": 0}}}, "explicit zeros")
    if wire is not None:
        if wire == b"":
            failures.append("explicit zeros where the defaults are 7/5/-3 encoded to nothing:"
                            " a value differing from its default must be written")
        back = decode(wire, "explicit zeros")
        if back is not None:
            for path in (("top",), ("s", "a"), ("s", "inner", "b")):
                have, ok = lookup(back, path)
                if not ok or num(have) != 0:
                    failures.append(f"explicit zeros: {'.'.join(path)} came back as"
                                    f" {have!r}, sent 0")

    if failures:
        print(f"FAIL: [{label}] nested defaults (generator#609):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1
    print(f"==> [{label}] nested defaults: {cases} case(s) -- fresh message and decoded"
          " empty input are the empty message, absence reads as the schema's defaults,"
          " interior default elements and explicit zeros round-trip")
    return 0


if __name__ == "__main__":
    sys.exit(main())
