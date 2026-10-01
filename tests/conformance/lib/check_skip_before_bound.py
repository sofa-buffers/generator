#!/usr/bin/env python3
"""MESSAGE_SPEC §7.3 before §7.1 -- a mistyped array element past the bound is SKIPPED (generator#627).

Usage:
  check_skip_before_bound.py --emit-schema
  check_skip_before_bound.py --self-test
  check_skip_before_bound.py <label> [--cwd DIR] [--sizes 1,2,3,5,0]
                             [--no-stream] [--status-verb VERB]
                             [--message NAME] -- <harness argv...>

## The rule

An array wrapper's element id IS its index (§5.1), and an index at or past the
schema `count` is INVALID (§7.1). But an element whose wire type contradicts the
array's element type is not an element of that array at all: §7.3 skips it like
an unknown id. When both apply, §7.3 wins -- "the subtype is therefore decided
first and the schema bound applied only to a field that survives it". So the
same over-index element id is

  * SKIPPED when it carries the wrong wire type, and
  * INVALID when it carries the right one.

A decoder that applies the bound first answers INVALID to both; one that never
applies it answers "skipped" (or worse, places the row) to both. The table
straddles the boundary in both directions on every field, so either mistake
fails a row.

## Why it covers rows

Every corelib applies this order inside its own collectors, so a one-level
wrapper array gets it for free. An array of arrays is different wherever a row
needs a collector of its own. The C++ backend used to generate one for a row that
is a wrapper sequence or a bound-checked native row, and that collector saw the
element header first and applied the bound before anything decided the wire type
(generator#627, Crucible G-0045). Since generator#629 it is the corelib's
`sofab::RowSeq` with a generated reader, and this driver is what shows the move
kept the order. So beside the one-level `list` control the schema carries a
wrapper row (`grid`), a row one level deeper (`deep`) and the two native rows the
C++ backend routes the same way (`erows`, `brows`).

## The table

Per field F (outer `count: 2`), with R(i) a correctly typed row at element id i
and W(i) a mistyped one:

    F_read        R(0) R(1)    reads back both rows
    F_skip_in     R(0) W(1)    W skipped, an in-range index (the plain §7.3 case)
    F_skip_over   R(0) W(5)    W skipped, past the bound -- the ordering witness
    F_typed_over  R(0) R(2)    INVALID, the same over-index with the right type

`tail` (a u32 after the array) is decoded on every accepted row: a skip that
consumes one byte too few or too many leaves the decoder in the wrong place and
`tail` comes back wrong. Every payload is complete, so truncation never explains
a verdict.

## Categories

`--status-verb VERB` names a verb that prints `INVALID`/`COMPLETE` on line 1;
with it, every INVALID row must name that category, not just exit non-zero.
"""
import json
import re
import subprocess
import sys

MSG = "sbb"

# Field ids, shared by the emitted schema and the forged headers.
LIST, GRID, EROWS, BROWS, DEEP, TAIL = 0, 1, 2, 3, 4, 5
TAIL_VALUE = 7

# Wire types (CORELIB_PLAN §4.3). Normative; do not renumber.
WT_UNSIGNED, WT_SIGNED, WT_FIXLEN, WT_ARR_U, WT_ARR_S, WT_SEQ = 0, 1, 2, 3, 4, 6
END = b"\x07"
SUB_STRING = 2

ENUM = {"A": 0, "B": 1}


def emit_schema() -> int:
    print("# The §7.3-before-§7.1 message (generator#627), printed by")
    print("# tests/conformance/lib/check_skip_before_bound.py so the schema and the")
    print("# driver that forges its bytes have one definition between them.")
    print("version: 1")
    print("messages:")
    print(f"  {MSG}:")
    print("    payload:")
    print(f"      list:  {{ id: {LIST}, type: array, items: {{ type: string, count: 2, maxlen: 8 }} }}")
    print(f"      grid:  {{ id: {GRID}, type: array, items: {{ type: array, count: 2,"
          " items: { type: string, count: 3, maxlen: 8 } } }")
    print(f"      erows: {{ id: {EROWS}, type: array, items: {{ type: array, count: 2,"
          " items: { type: enum, count: 3, enum: { A: 0, B: 1 } } } }")
    print(f"      brows: {{ id: {BROWS}, type: array, items: {{ type: array, count: 2,"
          " items: { type: boolean, count: 3 } } }")
    print(f"      deep:  {{ id: {DEEP}, type: array, items: {{ type: array, count: 2,"
          " items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 8 } } } }")
    print(f"      tail:  {{ id: {TAIL}, type: u32 }}")
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


def zigzag(n: int) -> int:
    return (n << 1) ^ (n >> 63)


def header(fid: int, wt: int) -> bytes:
    """`(id << 3) | wire_type`, MESSAGE_SPEC §4.3."""
    return varint((fid << 3) | wt)


def unsigned(fid: int, n: int) -> bytes:
    return header(fid, WT_UNSIGNED) + varint(n)


def signed(fid: int, n: int) -> bytes:
    return header(fid, WT_SIGNED) + varint(zigzag(n))


def string(fid: int, s: str) -> bytes:
    payload = s.encode()
    return header(fid, WT_FIXLEN) + varint((len(payload) << 3) | SUB_STRING) + payload


def uarray(fid: int, values) -> bytes:
    return header(fid, WT_ARR_U) + varint(len(values)) + b"".join(varint(v) for v in values)


def sarray(fid: int, values) -> bytes:
    return header(fid, WT_ARR_S) + varint(len(values)) + b"".join(varint(zigzag(v)) for v in values)


def seq(fid: int, *parts: bytes) -> bytes:
    return header(fid, WT_SEQ) + b"".join(parts) + END


# --- the cases --------------------------------------------------------------
#
# Per field: (name, id, row builder R(i, k) -> (bytes, decoded), mistype W(i)).
# k picks one of two distinct row values, so the two rows of F_read differ.

FIELDS = [
    ("list", LIST,
     lambda i, k: (string(i, "ab"[k]), "ab"[k]),
     lambda i: unsigned(i, 5)),
    ("grid", GRID,
     lambda i, k: (seq(i, string(0, "ab"[k])), ["ab"[k]]),
     lambda i: signed(i, 24)),
    ("erows", EROWS,
     lambda i, k: (sarray(i, [1 - k, k]), [1 - k, k]),
     lambda i: seq(i)),
    ("brows", BROWS,
     lambda i, k: (uarray(i, [1 - k, k]), [bool(1 - k), bool(k)]),
     lambda i: seq(i)),
    ("deep", DEEP,
     lambda i, k: (seq(i, seq(0, string(0, "ab"[k]))), [["ab"[k]]]),
     lambda i: signed(i, 24)),
]


def cases():
    """(name, field, wire, expected value or None for INVALID, why)."""
    out = []
    tail = unsigned(TAIL, TAIL_VALUE)
    for field, fid, row, wrong in FIELDS:
        r0, v0 = row(0, 0)
        r1, v1 = row(1, 1)
        r2, _ = row(2, 1)
        out += [
            (f"{field}_read", field, seq(fid, r0, r1) + tail, [v0, v1],
             "two correctly typed rows read back"),
            (f"{field}_skip_in", field, seq(fid, r0, wrong(1)) + tail, [v0],
             "a mistyped element at an in-range index is skipped (§7.3)"),
            (f"{field}_skip_over", field, seq(fid, r0, wrong(5)) + tail, [v0],
             "a mistyped element PAST the bound is skipped: §7.3 is decided "
             "before the §7.1 bound"),
            (f"{field}_typed_over", field, seq(fid, r0, r2) + tail, None,
             "a correctly typed element past the bound is INVALID (§7.1)"),
        ]
    return out


# Hand-written images for --self-test: the issue's own reproducer shapes.
SELF = {
    "grid_skip_over": "0e 06 02 0a 61 07 29 30 07 28 07",
    "grid_typed_over": "0e 06 02 0a 61 07 16 02 0a 62 07 07 28 07",
    "list_skip_over": "06 02 0a 61 28 05 07 28 07",
    "erows_skip_over": "16 04 02 02 00 2e 07 07 28 07",
}


def self_test() -> int:
    wires = {n: w.hex(" ") for n, _, w, _, _ in cases()}
    bad = [f"{n}: built {wires.get(n)}, want {h}" for n, h in SELF.items() if wires.get(n) != h]
    for b in bad:
        print("FAIL self-test " + b)
    if not bad:
        print(f"self-test: {len(SELF)} hand-written images match the builders")
    return 1 if bad else 0


# --- comparing across JSON dialects -----------------------------------------

def same(want, got) -> bool:
    """Tolerates each harness's spelling: `null` for an empty list, an enum as its
    name or its value, a boolean as 0/1."""
    if isinstance(want, list):
        if got is None:
            got = []
        return isinstance(got, list) and len(want) == len(got) \
            and all(same(w, g) for w, g in zip(want, got))
    if isinstance(want, bool):
        return got is want or (type(got) is int and got == int(want))
    if isinstance(want, int):
        if isinstance(got, str) and got in ENUM:
            return ENUM[got] == want
        try:
            return int(got) == want
        except (TypeError, ValueError):
            return False
    return want == got


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
    status = opt(head, "--status-verb")
    stream = "--no-stream" not in head
    sizes = [int(s) for s in opt(head, "--sizes", "1,2,3,5,0").split(",")]

    surfaces = [("decode", [])] + ([("streamdecode", [str(s)]) for s in sizes] if stream else [])
    table = cases()
    failed = []
    for name, field, wire, want, why in table:
        for verb, extra in surfaces:
            where = verb + (f" {extra[0]}" if extra else "")
            rc, out, err = run(cmd, [verb, msg] + extra, wire, cwd)
            if want is None:
                if rc == 0:
                    failed.append(f"{name} [{where}]: must be INVALID but decoded -- {why}; "
                                  f"bytes {wire.hex(' ')}")
                continue
            if rc != 0:
                failed.append(f"{name} [{where}]: must decode -- {why}; harness exited "
                              f"{rc}: {diagnostic(err)}; bytes {wire.hex(' ')}")
                continue
            try:
                obj = json.loads(out)
            except ValueError:
                failed.append(f"{name} [{where}]: printed no JSON: {out[:200]!r}")
                continue
            if not same(want, obj.get(field)):
                failed.append(f"{name} [{where}]: {why}\n    got  {field} = "
                              f"{json.dumps(obj.get(field))}\n    want {field} = {json.dumps(want)}")
            if not same(TAIL_VALUE, obj.get("tail")):
                failed.append(f"{name} [{where}]: the skip desynchronised the stream -- "
                              f"tail = {json.dumps(obj.get('tail'))}, want {TAIL_VALUE}")
        if want is None and status:
            _, sout, serr = run(cmd, [status, msg], wire, cwd)
            got = (sout.strip().splitlines() or [""])[0]
            if got != "INVALID":
                failed.append(f"{name} [{status}]: category must be INVALID, got {got!r}")

    if failed:
        print(f"FAIL {label} §7.3 before §7.1: {len(failed)} failures")
        for f in failed:
            print("  " + f)
        return 1
    print(f"{label} §7.3 before §7.1 (generator#627): {len(table)} cases on "
          f"{len(FIELDS)} arrays (one-level, wrapper row, enum row, boolean row, depth 3) -- "
          f"a mistyped element past the bound is skipped, a typed one is INVALID; "
          f"{'one-shot and streamed at splits ' + ','.join(map(str, sizes)) if stream else 'one-shot only'}"
          f"{'; category via ' + status if status else ''}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
