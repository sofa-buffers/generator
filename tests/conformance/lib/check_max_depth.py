#!/usr/bin/env python3
"""MAX_DEPTH = 255: a generated decoder follows nesting to the ceiling, not past it (generator#646).

Usage:
  check_max_depth.py --emit-schema
  check_max_depth.py --self-test
  check_max_depth.py <label> [--cwd DIR] [--sizes 1,0] [--no-stream]
                     [--max-depth N] [--message NAME]
                     [--invalid-pattern REGEX] [--stream-invalid-pattern REGEX]
                     [--limit-pattern REGEX] [--stream-limit-pattern REGEX]
                     [--status-verb VERB] [--status-invalid NAME]
                     -- <harness argv...>

## The rule

CORELIB_PLAN §4.9/§6.2: `MAX_DEPTH` is 255, a format-wide ceiling. An encoder must not
open more than 255 nested sequences and a decoder rejects deeper nesting as `INVALID`
(§5.2.2) rather than risk unbounded recursion. The boundary as the corelibs test it:
255 nested sequences, closed, decode; 256 are INVALID, closed or not.

## Why a driver on the generated harness

Most backends track the current decode scope in GENERATED code -- a location stack
(Kotlin, Java, C#, Zig, Rust) or a current-scope-plus-stack pair (Python, TypeScript)
-- beside the corelib's own depth counter. A generated stack that drops a push, grows
wrongly, or disagrees with the corelib by one loses a field after the unwind or
accepts a malformed message (generator#283 was that defect in Rust). The corelibs'
unit tests cannot reach it and the shared vectors stop at three levels, so the
property is asserted black-box on the harness, for every backend. Backends that lean
on corelib-driven nesting (C, C++, Go, Dart) run the same table: the boundary is a
property of the whole decode path.

The generated scope tracking is a SECOND counter of BOUND levels, so the table mixes
bound and skipped levels (the corelib's F-0050 off-by-one let one bound level plus 255
skipped ones through).

## The schema

The driver prints its own (`--emit-schema`), so the ids it forges have one definition:

    deep { s: struct { t: struct { x: u32 }, y: u32 }, after: u32 }

Id 9 is unknown in every scope, so it gives arbitrary skipped depth.

## The table (N = --max-depth, 255)

    A_bound_and_skipped   open s, t, then N-2 unknown levels, closed; then x, y, after
                          -> decodes, x/y/after hold the sent values
    B_skipped_only        N unknown levels at top level, closed; then after   -> decodes
    C_over_closed         A with one more unknown level (N+1 in all), closed  -> INVALID
    C_over_unclosed       the same, the closing headers never sent            -> INVALID
    D_over_closed         B with N+1 levels, closed                            -> INVALID
    D_over_unclosed       the same, the closing headers never sent            -> INVALID
    E_many_shallow        300 sibling unknown sequences, each 20 deep, inside t,
                          then x, y, after -> decodes (a stack that never shrinks, or a
                          counter that drifts per unwind, fails here)

Every accepted row is decoded through `decode` and, unless `--no-stream`, through
`streamdecode` at each of `--sizes`.

## Categories

A refused row must be refused as INVALID: not a crash and not a receiver-cap verdict.
`--invalid-pattern` (regex over stdout+stderr of the refused run; the streamdecode
surface uses `--stream-invalid-pattern` when given) must match, and `--limit-pattern`,
when given, must NOT. `--status-verb VERB` names a verb printing the verdict on line 1,
compared with `--status-invalid` (default INVALID); a suite whose `decode` error echoes
a stack trace reads the category that way. One of the two channels is required: an exit
status alone cannot tell INVALID from a crash.

## Profiles with a lower ceiling

`--max-depth N` takes the ceiling the profile is registered with (CORELIB_PLAN profile
registry); no suite passes one today, every profile ships 255. Never skip a row instead.
"""

import json
import re
import subprocess
import sys

MSG = "deep"

# Field ids, shared by the emitted schema and the forged headers.
S, AFTER = 0, 1
T, Y = 0, 1  # inside s
X = 0        # inside t
UNKNOWN = 9

VALUES = {"x": 1000, "y": 2000, "after": 3000}

# Wire types (CORELIB_PLAN §4.3). Normative; do not renumber.
WT_UNSIGNED, WT_SEQ = 0, 6
END = b"\x07"

SHALLOW_SIBLINGS, SHALLOW_DEPTH = 300, 20


def emit_schema() -> int:
    print("# The MAX_DEPTH message (generator#646), printed by")
    print("# tests/conformance/lib/check_max_depth.py so the schema and the driver that")
    print("# forges its bytes have one definition between them.")
    print("version: 1")
    print("messages:")
    print(f"  {MSG}:")
    print("    payload:")
    print(f"      s: {{ id: {S}, type: struct, fields: {{")
    print(f"        t: {{ id: {T}, type: struct, fields: {{ x: {{ id: {X}, type: u32 }} }} }},")
    print(f"        y: {{ id: {Y}, type: u32 }} }} }}")
    print(f"      after: {{ id: {AFTER}, type: u32 }}")
    return 0


# --- the wire image ---------------------------------------------------------

def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def header(fid: int, wt: int) -> bytes:
    """`(id << 3) | wire_type`, MESSAGE_SPEC §4.3."""
    return varint((fid << 3) | wt)


def unsigned(fid: int, n: int) -> bytes:
    return header(fid, WT_UNSIGNED) + varint(n)


def opens(n: int) -> bytes:
    return header(UNKNOWN, WT_SEQ) * n


def nest(n: int) -> bytes:
    """`n` unknown sequences, one inside the next, all closed."""
    return opens(n) + END * n


def seq(fid: int, *parts: bytes) -> bytes:
    return header(fid, WT_SEQ) + b"".join(parts) + END


def mixed(skipped: int, closed: bool = True, inner: bytes = b"") -> bytes:
    """s { t { <skipped levels> x }, y }, after -- or, unclosed, its open headers."""
    if closed:
        t = seq(T, nest(skipped), inner or unsigned(X, VALUES["x"]))
        return seq(S, t, unsigned(Y, VALUES["y"])) + unsigned(AFTER, VALUES["after"])
    return header(S, WT_SEQ) + header(T, WT_SEQ) + opens(skipped)


def top(levels: int, closed: bool = True) -> bytes:
    if closed:
        return nest(levels) + unsigned(AFTER, VALUES["after"])
    return opens(levels)


def cases(depth: int):
    """(name, wire, expected fields or None for INVALID, why)."""
    full = {"s": {"t": {"x": VALUES["x"]}, "y": VALUES["y"]}, "after": VALUES["after"]}
    sibling = nest(SHALLOW_DEPTH) * SHALLOW_SIBLINGS
    return [
        ("A_bound_and_skipped", mixed(depth - 2), full,
         f"s and t plus {depth - 2} unknown levels is exactly {depth}: accepted, and "
         "every field after the unwind is intact"),
        ("B_skipped_only", top(depth), {"after": VALUES["after"]},
         f"{depth} unknown levels at top level are accepted and `after` survives"),
        ("C_over_closed", mixed(depth - 1), None,
         f"{depth + 1} nested sequences (bound + skipped) are INVALID (§4.9)"),
        ("C_over_unclosed", mixed(depth - 1, closed=False), None,
         f"the {depth + 1}th open is refused before any close arrives"),
        ("D_over_closed", top(depth + 1), None,
         f"{depth + 1} skipped levels are INVALID (§4.9)"),
        ("D_over_unclosed", top(depth + 1, closed=False), None,
         f"the {depth + 1}th open is refused before any close arrives"),
        ("E_many_shallow", mixed(0, inner=sibling + unsigned(X, VALUES["x"])), full,
         f"{SHALLOW_SIBLINGS} sibling unknown sequences {SHALLOW_DEPTH} deep must not "
         "accumulate depth: a stack that never shrinks, or a counter that drifts per "
         "unwind, loses a field"),
    ]


# Hand-written images for --self-test.
SELF = {
    # depth 3: s, t, one unknown level, x, y, after
    "A@3": (3, "A_bound_and_skipped", "06 06 4e 07 00 e8 07 07 08 d0 0f 07 08 b8 17"),
    "B@2": (2, "B_skipped_only", "4e 4e 07 07 08 b8 17"),
    "D_over_unclosed@2": (2, "D_over_unclosed", "4e 4e 4e"),
}


def self_test() -> int:
    bad = []
    for key, (depth, name, want) in SELF.items():
        got = {n: w for n, w, _, _ in cases(depth)}[name].hex(" ")
        if got != want:
            bad.append(f"{key}: built {got}, want {want}")
    for b in bad:
        print("FAIL self-test " + b)
    if not bad:
        print(f"self-test: {len(SELF)} hand-written images match the builders")
    return 1 if bad else 0


# --- comparing across JSON dialects -----------------------------------------

def same(want, got) -> bool:
    if isinstance(want, dict):
        return isinstance(got, dict) and all(same(v, got.get(k)) for k, v in want.items())
    try:
        return int(got) == want
    except (TypeError, ValueError):
        return False


_NOISE = re.compile(
    r"^(at\s|warning\b|note:|help:|-->|\||=\s|\d+\s*\||\^|Note:|WARNING:|SLF4J|Picked up )")


def diagnostic(stderr: bytes) -> str:
    lines = [l.strip() for l in stderr.decode(errors="replace").splitlines()
             if l.strip() and not _NOISE.match(l.strip())]
    return " | ".join(lines[-2:]) if lines else "<no output>"


def run(cmd, argv, wire, cwd):
    p = subprocess.run(cmd + argv, input=wire, cwd=cwd,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return p.returncode, p.stdout.decode(errors="replace"), p.stderr


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()
    if "--self-test" in argv:
        return self_test()

    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    label = head[0]
    cwd = opt(head, "--cwd")
    msg = opt(head, "--message", MSG)
    depth = int(opt(head, "--max-depth", "255"))
    status = opt(head, "--status-verb")
    status_invalid = opt(head, "--status-invalid", "INVALID")
    inv = {"decode": opt(head, "--invalid-pattern")}
    lim = {"decode": opt(head, "--limit-pattern")}
    inv["stream"] = opt(head, "--stream-invalid-pattern", inv["decode"])
    lim["stream"] = opt(head, "--stream-limit-pattern", lim["decode"])
    stream = "--no-stream" not in head
    sizes = [int(s) for s in opt(head, "--sizes", "1,0").split(",")]
    if depth < 3:
        print("FAIL: --max-depth below 3 leaves no row mixing bound and skipped levels")
        return 1
    if not status and not inv["decode"]:
        print("FAIL: no category channel: give --invalid-pattern or --status-verb "
              "(an exit status alone cannot tell INVALID from a crash)")
        return 1

    surfaces = [("decode", "decode", [])] + (
        [("streamdecode", "stream", [str(s)]) for s in sizes] if stream else [])
    table = cases(depth)
    failed = []
    for name, wire, want, why in table:
        for verb, kind, extra in surfaces:
            where = verb + (f" {extra[0]}" if extra else "")
            rc, out, err = run(cmd, [verb, msg] + extra, wire, cwd)
            if want is None:
                text = out + err.decode(errors="replace")
                if rc == 0:
                    failed.append(f"{name} [{where}]: {depth + 1} levels decoded, expected "
                                  f"INVALID -- {why}")
                    continue
                if inv[kind] and not re.search(inv[kind], text):
                    failed.append(f"{name} [{where}]: refused, but not as INVALID (pattern "
                                  f"{inv[kind]!r}): {diagnostic(err)}")
                if lim[kind] and re.search(lim[kind], text):
                    failed.append(f"{name} [{where}]: refused as a receiver cap, not "
                                  f"INVALID: {diagnostic(err)}")
                continue
            if rc != 0:
                failed.append(f"{name} [{where}]: must decode -- {why}; harness exited "
                              f"{rc}: {diagnostic(err)}")
                continue
            try:
                obj = json.loads(out)
            except ValueError:
                failed.append(f"{name} [{where}]: printed no JSON: {out[:200]!r}")
                continue
            if not same(want, obj):
                lost = [k for k in ("x", "y", "after") if k in str(want) and
                        k not in json.dumps(obj)]
                failed.append(f"{name} [{where}]: {why}\n    got  {json.dumps(obj)}\n"
                              f"    want {json.dumps(want)}"
                              + (f"\n    {', '.join(lost)} lost after unwind" if lost else ""))
        if want is None and status:
            _, sout, _ = run(cmd, [status, msg], wire, cwd)
            got = (sout.strip().splitlines() or [""])[0]
            if got != status_invalid:
                failed.append(f"{name} [{status}]: category must be {status_invalid}, "
                              f"got {got!r}")

    if failed:
        print(f"FAIL {label} MAX_DEPTH: {len(failed)} failures")
        for f in failed:
            print("  " + f)
        return 1
    print(f"{label} MAX_DEPTH (generator#646): {len(table)} cases (A..E) -- {depth} levels "
          f"(bound + skipped) decode with every field after the unwind intact, {depth + 1} "
          f"are INVALID closed and unclosed, {SHALLOW_SIBLINGS} shallow unwinds do not "
          f"accumulate; {'one-shot and streamed at splits ' + ','.join(map(str, sizes)) if stream else 'one-shot only'}"
          f"{'; category via ' + status if status else ''}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
