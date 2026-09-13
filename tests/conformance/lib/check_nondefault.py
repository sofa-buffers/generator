#!/usr/bin/env python3
"""Report round-trip fixture fields that sit ON their schema default.

Usage:
  check_nondefault.py <baseline> <decoded-fixture> [--label TEXT]
                      [--except PATH[,PATH...]] [--warn-only]

`--except` names paths that are legitimately at their default: a union carries ONE
selected arm, so the arms beside it must read as their defaults and saying so is
part of the test rather than a hole in it.

`baseline` is what the harness decodes from an EMPTY message -- every field at
its schema default, spelled the way this backend spells it. `fixture` is the
round trip's input. Exit 0 when every field the baseline has is present in the
fixture AND differs from it, 1 otherwise.

## Why a fixture at the default proves nothing

A round trip asks one question: does what I sent arrive? A field the fixture
leaves out is reconstructed from its default, so the pass says "the default
survived" -- which it would even if the field's decode were broken for every
other value. example.yaml declares non-zero defaults on purpose (`someu8: 7`,
`somefp64: 3.14159...`), so "left out" and "set to something real" look alike in
the output and only the input tells them apart.

    someu8 default 7, fixture omits it
      -> the round trip compares 7 against 7
      -> a backend masking someu8 to 0 above 127 still passes

## Why the OMISSION tests want the opposite

The sparse-encoding rule (MESSAGE_SPEC S2) is the mirror image: a field equal to
its default MUST NOT reach the wire, and the receiver reconstructs it. That test
therefore needs every field ON its default -- and it has its own schema and its
own fixture for exactly that reason. The two must not share an input: one proves
values travel, the other proves they do not.

This check guards the first kind. Point it at the round-trip fixture only.
"""
from __future__ import annotations

import json
import sys


def walk(base, fix, skip, path="$"):
    if isinstance(base, dict):
        if not isinstance(fix, dict):
            yield f"  {path}: the fixture does not carry an object here"
            return
        for k, v in base.items():
            if f"{path}.{k}" in skip:
                continue
            if k not in fix:
                yield f"  {path}.{k}: NOT SET by the fixture -- the round trip compares its default with itself"
            else:
                yield from walk(v, fix[k], skip, f"{path}.{k}")
        return
    if base == fix:
        yield f"  {path}: set to the schema default ({json.dumps(base)[:60]}) -- the round trip cannot tell a working decode from a broken one"


def main(argv):
    args = [a for a in argv if not a.startswith("--")]
    warn = "--warn-only" in argv
    skip = set()
    for i, a in enumerate(argv):
        if a == "--except" and i + 1 < len(argv):
            skip.update(argv[i + 1].split(","))
        elif a.startswith("--except="):
            skip.update(a.split("=", 1)[1].split(","))
    label = ""
    for i, a in enumerate(argv):
        if a == "--label" and i + 1 < len(argv):
            label = argv[i + 1]
        elif a.startswith("--label="):
            label = a.split("=", 1)[1]
    args = [a for a in args if a != label and a not in {",".join(sorted(skip))} and a not in skip]
    for i, a in enumerate(argv):
        if a == "--except" and i + 1 < len(argv) and argv[i + 1] in args:
            args.remove(argv[i + 1])
    if len(args) != 2:
        print("usage: check_nondefault.py <baseline> <fixture> [--label TEXT] [--warn-only]", file=sys.stderr)
        return 2
    try:
        base, fix = json.loads(args[0]), json.loads(args[1])
    except json.JSONDecodeError as e:
        print(f"FAIL: {label or 'nondefault'}: not JSON ({e})", file=sys.stderr)
        return 1

    lines = list(walk(base, fix, skip))
    if not lines:
        return 0
    head = "WARN" if warn else "FAIL"
    print(f"{head}: {label or 'round-trip fixture'}: {len(lines)} field(s) prove nothing", file=sys.stderr)
    for ln in lines:
        print(ln, file=sys.stderr)
    return 0 if warn else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
