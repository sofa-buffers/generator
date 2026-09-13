#!/usr/bin/env python3
"""Compare two JSON documents as DATA, not as text.

Usage:
  json_equal.py <expected> <actual> [--label TEXT] [--allow-extra]

Both arguments are JSON documents, passed as strings. Exit 0 when they carry
the same data, 1 otherwise, with every differing path named on stderr.

## Why a round trip may not be compared as a string

A harness renders its own JSON, and the rendering is the backend's, not the
format's. The same message decoded by two backends:

    cpp : {"zz":7,"aa":"hi","mm":{"a":5,"b":""}}
    go  : {"aa":"hi","mm":{"b":"","a":5},"zz":7}

Three differences, none of them semantic: cpp orders members by schema id and go
alphabetically, the union arms come in either order, and both print every arm
rather than the selected one. A string comparison therefore pins the rendering
convention, not the values -- it cannot be shared across suites, and it turns a
cosmetic change into a red build.

## Why grepping single fields is not the answer either

The alternative in use pins two to six fields of a thirty-field message:

    echo "$OUT" | grep -q '"someu64":18446744073709551615' || FAIL

That is robust against rendering and blind to everything else. The remaining
twenty-four fields are decoded and never looked at -- and when a schema GROWS,
nothing notices: the grepped fields are still there. generator#531 is the
receipt. A field was added to a corpus message, three suites passed locally, and
CI went red only in TypeScript, the one suite comparing whole strings.

## What this does instead

Parse both sides and compare structurally:

  * **order never matters** -- object members are compared by name;
  * **a missing field** is an error: the round trip lost it;
  * **an extra field** is an error too, and that is the point. A field the actual
    output carries and the expected fixture does not means the fixture stopped
    covering the message -- usually because the schema grew. Pass `--allow-extra`
    to downgrade that to a warning where a fixture is deliberately partial.

Numbers compare by value, so `2.0` and `2` agree; that is one rendering
difference too many to be worth failing on, and no wire fact rides on it.
"""
from __future__ import annotations

import json
import sys


def diff(exp, act, path="$"):
    """Yield a human-readable line per difference, depth-first."""
    if isinstance(exp, dict) and isinstance(act, dict):
        for k in exp:
            if k not in act:
                yield f"  {path}.{k}: missing from the actual output"
            else:
                yield from diff(exp[k], act[k], f"{path}.{k}")
        for k in act:
            if k not in exp:
                yield f"  {path}.{k}: EXTRA in the actual output (the fixture does not cover it)"
        return
    if isinstance(exp, list) and isinstance(act, list):
        if len(exp) != len(act):
            yield f"  {path}: length {len(exp)} expected, {len(act)} actual"
        for i, (e, a) in enumerate(zip(exp, act)):
            yield from diff(e, a, f"{path}[{i}]")
        return
    if isinstance(exp, bool) != isinstance(act, bool):
        yield f"  {path}: {exp!r} expected, {act!r} actual"
        return
    if isinstance(exp, (int, float)) and isinstance(act, (int, float)):
        if exp != act:
            yield f"  {path}: {exp!r} expected, {act!r} actual"
        return
    if exp != act:
        yield f"  {path}: {exp!r} expected, {act!r} actual"


def main(argv):
    args, allow_extra, label = [], False, ""
    it = iter(range(len(argv)))
    skip = -1
    for i, a in enumerate(argv):
        if i == skip:
            continue
        if a == "--allow-extra":
            allow_extra = True
        elif a == "--label":
            label = argv[i + 1] if i + 1 < len(argv) else ""
            skip = i + 1
        elif a.startswith("--label="):
            label = a.split("=", 1)[1]
        elif a.startswith("--"):
            pass
        else:
            args.append(a)
    if len(args) != 2:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2

    parsed = []
    for which, text in (("expected", args[0]), ("actual", args[1])):
        try:
            parsed.append(json.loads(text))
        except json.JSONDecodeError as e:
            print(f"FAIL: {label or 'json_equal'}: the {which} side is not JSON ({e})", file=sys.stderr)
            print(f"  {text[:400]}", file=sys.stderr)
            return 1

    lines = list(diff(*parsed))
    if allow_extra:
        lines = [ln for ln in lines if "EXTRA" not in ln]
    if not lines:
        return 0
    print(f"FAIL: {label or 'round trip'}: the decoded message does not match the fixture", file=sys.stderr)
    for ln in lines:
        print(ln, file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
