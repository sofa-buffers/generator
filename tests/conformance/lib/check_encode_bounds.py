#!/usr/bin/env python3
"""An over-bound value of a bounded element is REFUSED at encode (generator#656).

Usage:
  check_encode_bounds.py --emit-schema [--bounded-only]
  check_encode_bounds.py --self-test
  check_encode_bounds.py <label> --backend KEY [--cwd DIR] [--bounded-only]
                         [--blob-json array|base64] [--message NAME]
                         [--discover] -- <harness argv...>

## The rule

ARCHITECTURE §9.6: a value filled past its own declared bound "does not fit, and
is reported rather than emitted short". The decoder already enforces every bound
(MESSAGE_SPEC §7.1: over-count array, over-`maxlen` string or blob, over-width
integer, enum or bitfield are INVALID); the encoder is the other half. Only the
generated code knows the bounds, so only it can refuse. Before this driver nothing
asserted it, and the only guard was accidental: a backend with an exactly sized
buffer refuses once the WHOLE message passes `MAX_SIZE`, which a single over-bound
value in a small message never does.

So the same input is, per backend, either refused, written unchecked (and then
refused by the same stack's own decoder), or silently clamped, truncated or masked.
Refused is the answer wherever the encoder sees the over-bound value. The one
exception is a bounded container that clamps on ASSIGNMENT, before any encoder
runs (C++ FixedString/InlineVector, Zig FixedArray, C's length/count companions):
there the clamp is the documented contract (CLAMP_CONTRACT below), and what it
must never do is cut a string inside a UTF-8 character.

## What is asserted

Through the harness `encode` verb, one value per message:

  * every REFUSE case (below) must exit non-zero and, for a crash (a signal, a
    panic), is reported as a crash and not as a refusal;
  * every CONTROL case must encode, and `decode` must hand the same value back --
    every bounded field exactly AT its bound, and the unbounded fields at lengths
    far past any bound a bounded field has, since an unbounded field has no bound
    for an encoder to check.

The REFUSE cases, over the probe message `bnd`:

    string over maxlen     by one byte, 60 bytes, 40 x U+00E9 (80 bytes), and a
                           5-byte value whose cut at maxlen 4 would land inside a
                           UTF-8 character
    blob over maxlen       by one byte, 60 bytes
    array over count       count+1 and count+2 elements
    array<string>          an element over maxlen; count+1 elements
    nested struct's array  over its count
    scalar past its width  u8, i16 (both signs), enum, bitfield -- wherever the
                           backend's type can hold the value

## KNOWN_GAP

The cells that fail TODAY, per backend variant (`--backend KEY`), so the driver can
land before the backend fixes. A listed case that fails is reported as a known gap
and the run stays green; a listed case that PASSES fails the run, so the entry has
to be removed together with the fix. Each backend fix removes its entries; the list
is empty when the last one lands. The case count and the gap list are printed.

## CLAMP_CONTRACT

Per backend variant, the REFUSE cases whose value a bounded container clamps at
assignment by contract (generator#656, decision of 2026-10-07). Such a case must
ENCODE, and `decode` must hand back exactly the clamped value (`clamped()`): a
string cut to its maxlen in bytes at a UTF-8 character boundary, a blob and an
array cut to their bound. Emitting the value unclamped, refusing it, or cutting
inside a character all fail the case. Scalars are never clamped by contract.

## STORAGE_BLOCKED

A cell the harness cannot reach: the value is refused, wrapped or cast to the field's
storage type BEFORE any generated encode code runs (Go's and serde's JSON layers
reject `300` for a `uint8`, System.Text.Json throws, a C, C++, Zig or Kotlin harness
casts it), or a bounded container refuses it at assignment (rs-no-std's
`heapless`). Whatever the harness answers there says nothing about the generated
encoder, so the cell is declared with its reason, still run, and reported as
`blocked`; it is neither a pass nor a gap, and its outcome is not asserted. A
blocked cell is the weakest answer this driver gives: every one of them needs a
harness that hands the encoder the raw value, or is covered by a backend unit test.

## --self-test (the negative control)

Runs the table against a model harness that is this file itself: one that refuses
every over-bound value passes every case, and the same model with ONE guard removed
(`--defect FIELD`) fails exactly the cases of that field. So the driver is shown to
go red on a missing guard, and to stay green on a present one, without a backend.

## Not covered here

A message at exactly `MAX_SIZE` is `maxsize_fill.sh`'s job (every suite runs it);
the probe's enum and bitfield are charged their full width by the size walk, so
its all-at-bound message is shorter than `MAX_SIZE` and could not assert that.
`--discover` runs the table with every list ignored and prints the failing cells
as a tuple ready to paste, which is how the lists below were produced; setting
SOFAB_ENCODE_BOUNDS_DISCOVER=1 does the same for a whole suite run.
"""
import json
import os
import subprocess
import sys

import harness_dialect as hd

MSG = "bnd"
CTRL = "unb"

ENUM = {"A": 0, "B": 1, "C": 2}

# Variants that generate different code each get a key. A key not listed here is an
# error, so a new variant cannot silently run without a decision.
VARIANTS = (
    "c",
    "cpp", "cpp-static", "c-cpp", "c-cpp-static",
    "rust", "rust-static", "rs-no-std", "rs-no-std-dynamic", "rs-no-std-std",
    "go", "java", "kotlin", "csharp", "dart", "zig",
    "typescript", "typescript-long", "typescript-number",
    "python", "python-pure",
)

# Cells that fail today. Filled from `--discover` runs, never guessed.
STRINGS_AND_ARRAYS = (
    "s_over_1", "s_over_60", "s_over_utf8_80", "s_over_midchar", "b_over_1", "b_over_60",
    "au_count_plus_1", "au_count_plus_2", "as_elem_over_maxlen", "as_count_plus_1",
    "n_arr_count_plus_1",
)
SCALARS = ("u8_over", "i16_over", "i16_under", "e_over", "f_over")

# One line per variant, so that a backend change edits only its own lines.
KNOWN_GAP = {
    "c": STRINGS_AND_ARRAYS + SCALARS,
    "cpp": STRINGS_AND_ARRAYS + SCALARS,
    "cpp-static": STRINGS_AND_ARRAYS + SCALARS,
    "c-cpp": STRINGS_AND_ARRAYS + SCALARS,
    "c-cpp-static": STRINGS_AND_ARRAYS + SCALARS,
    "rust": (),
    "rust-static": (),
    "rs-no-std": (),
    "rs-no-std-dynamic": (),
    "rs-no-std-std": (),
    "go": (),
    "java": STRINGS_AND_ARRAYS + SCALARS,
    "kotlin": STRINGS_AND_ARRAYS + SCALARS,
    "csharp": STRINGS_AND_ARRAYS + SCALARS,
    "dart": (),
    "zig": (),
    "typescript": (),
    "typescript-long": (),
    "typescript-number": (),
    "python": STRINGS_AND_ARRAYS + SCALARS,
    "python-pure": STRINGS_AND_ARRAYS + SCALARS,
}
assert set(KNOWN_GAP) == set(VARIANTS)

# Cells clamped at assignment by contract (see CLAMP_CONTRACT in the docstring).
# One line per variant; a cell here is asserted as clamped, never as refused.
CLAMP_CONTRACT = {
    "c": (),
    "cpp": (),
    "cpp-static": (),
    "c-cpp": (),
    "c-cpp-static": (),
    "rust": (),
    "rust-static": (),
    "rs-no-std": (),
    "rs-no-std-dynamic": (),
    "rs-no-std-std": (),
    "go": (),
    "java": (),
    "kotlin": (),
    "csharp": (),
    "dart": (),
    "zig": ("au_count_plus_1", "au_count_plus_2", "n_arr_count_plus_1"),
    "typescript": (),
    "typescript-long": (),
    "typescript-number": (),
    "python": (),
    "python-pure": (),
}
assert set(CLAMP_CONTRACT) == set(VARIANTS)

# Cells whose value the harness (or the storage type) cannot carry to the encoder:
# {variant: {case: reason}}.
STORAGE_BLOCKED = {v: {} for v in VARIANTS}


def _block(variants, cells, reason):
    for v in variants:
        for c in cells:
            STORAGE_BLOCKED[v][c] = reason
        KNOWN_GAP[v] = tuple(c for c in KNOWN_GAP[v] if c not in STORAGE_BLOCKED[v])


_block(("go",), SCALARS,
       "the Go storage type is exactly the declared width (uint8, int16, enum int8, bitfield "
       "uint8), so no value past it can reach the encoder; encoding/json refuses the number "
       "before encode")
_CAST = ("the harness casts the JSON number to the field's fixed-width type (300 -> 0x2c) "
         "before the generated code sees it")
_block(("c",), SCALARS, _CAST)
_block(("cpp",), SCALARS, _CAST)
_block(("cpp-static",), SCALARS, _CAST)
_block(("c-cpp",), SCALARS, _CAST)
_block(("c-cpp-static",), SCALARS, _CAST)
_block(("zig",), SCALARS,
       "the field's Zig type is exactly the declared width (u8, i16, and the i8/u8 an enum/"
       "bitfield implies), so no over-width value can be stored; the harness casts the JSON "
       "number into it (300 -> 0x2c) before the generated code sees it")
_block(("csharp",), SCALARS,
       "System.Text.Json throws on the number before encode (unhandled harness exception)")
_block(("kotlin",), ("u8_over", "i16_over", "i16_under"),
       "the harness narrows the number to UByte/Short before the generated code sees it")
_RS_WIDTH = ("the storage type guarantees the width: a u8/i16 field, an enum's i8 and a bitfield's "
             "u8 backing hold exactly the declared width, so no over-width value exists to encode "
             "(serde refuses the number at assignment, harness panic)")
_block(("rust",), SCALARS, _RS_WIDTH)
_block(("rs-no-std-dynamic",), SCALARS, _RS_WIDTH)
_HEAPLESS = ("the storage type guarantees the bound: a heapless::String<N> / heapless::Vec<T, N> "
             "cannot hold more than its schema maxlen/count (serde refuses the value at assignment, "
             "harness panic), so no over-bound value exists to encode")
_block(("rust-static",), STRINGS_AND_ARRAYS, _HEAPLESS)
_block(("rust-static",), SCALARS, _RS_WIDTH)
_block(("rs-no-std",), STRINGS_AND_ARRAYS, _HEAPLESS)
_block(("rs-no-std",), SCALARS, _RS_WIDTH)
_block(("rs-no-std-std",), STRINGS_AND_ARRAYS, _HEAPLESS)
_block(("rs-no-std-std",), SCALARS, _RS_WIDTH)


def emit_schema(bounded_only=False) -> int:
    print("# The encode-bound probe (generator#656), printed by")
    print("# tests/conformance/lib/check_encode_bounds.py so the bounds the schema")
    print("# declares and the values the driver overfills with have one definition.")
    print(f"  {MSG}:")
    print("    payload:")
    print("      s: { id: 0, type: string, maxlen: 4 }")
    print("      b: { id: 1, type: blob, maxlen: 4 }")
    print("      u8: { id: 2, type: u8 }")
    print("      i16: { id: 3, type: i16 }")
    print("      au: { id: 4, type: array, items: { type: u32, count: 3 } }")
    print("      as: { id: 5, type: array, items: { type: string, count: 3, maxlen: 4 } }")
    print("      e: { id: 6, type: enum, enum: { A: 0, B: 1, C: 2 } }")
    print("      f: { id: 7, type: bitfield, bits: { x: { pos: 0 }, y: { pos: 1 }, z: { pos: 2 } } }")
    print("      n:")
    print("        id: 8")
    print("        type: struct")
    print("        fields:")
    print("          arr: { id: 0, type: array, items: { type: u16, count: 2 } }")
    if not bounded_only:
        print(f"  {CTRL}:")
        print("    payload:")
        print("      s: { id: 0, type: string }")
        print("      b: { id: 1, type: blob }")
        print("      a: { id: 2, type: array, items: { type: u32 } }")
    return 0


def blob(data: bytes, dialect: str):
    import base64
    return base64.b64encode(data).decode() if dialect == "base64" else list(data)


def cases(blob_json: str, bounded_only: bool):
    """(name, message, value sent, 'refuse' | 'control')."""
    B = lambda n: blob(b"\xab" * n, blob_json)
    out = [
        ("s_over_1", MSG, {"s": "xxxxx"}, "refuse"),
        ("s_over_60", MSG, {"s": "x" * 60}, "refuse"),
        ("s_over_utf8_80", MSG, {"s": "é" * 40}, "refuse"),
        ("s_over_midchar", MSG, {"s": "xxxé"}, "refuse"),
        ("b_over_1", MSG, {"b": B(5)}, "refuse"),
        ("b_over_60", MSG, {"b": B(60)}, "refuse"),
        ("au_count_plus_1", MSG, {"au": [1, 2, 3, 4]}, "refuse"),
        ("au_count_plus_2", MSG, {"au": [1, 2, 3, 4, 5]}, "refuse"),
        ("as_elem_over_maxlen", MSG, {"as": ["ab", "xxxxx"]}, "refuse"),
        ("as_count_plus_1", MSG, {"as": ["a", "b", "c", "d"]}, "refuse"),
        ("n_arr_count_plus_1", MSG, {"n": {"arr": [1, 2, 3]}}, "refuse"),
        ("u8_over", MSG, {"u8": 300}, "refuse"),
        ("i16_over", MSG, {"i16": 40000}, "refuse"),
        ("i16_under", MSG, {"i16": -40000}, "refuse"),
        ("e_over", MSG, {"e": 200}, "refuse"),
        ("f_over", MSG, {"f": 256}, "refuse"),
        ("at_bound", MSG, {
            "s": "xxxx", "b": B(4), "u8": 255, "i16": -32768,
            "au": [4294967295, 0, 1], "as": ["abcd", "", "éé"],
            "e": 2, "f": 7, "n": {"arr": [65535, 1]}}, "control"),
        ("at_bound_utf8", MSG, {"s": "éé"}, "control"),
        ("at_bound_other_edge", MSG, {"u8": 255, "i16": 32767, "e": 127, "f": 255}, "control"),
    ]
    if not bounded_only:
        out += [
            ("unbounded_string", CTRL, {"s": "y" * 600}, "control"),
            ("unbounded_blob", CTRL, {"b": B(600)}, "control"),
            ("unbounded_array", CTRL, {"a": list(range(300))}, "control"),
        ]
    return out


def _cut_utf8(text: str, limit: int) -> str:
    """text cut to at most `limit` UTF-8 bytes, never inside a character."""
    raw = text.encode()[:limit]
    while raw:
        try:
            return raw.decode()
        except UnicodeDecodeError:
            raw = raw[:-1]
    return ""


def clamped(sent: dict, b64: str) -> dict:
    """What a clamping container holds after `sent` is assigned (CLAMP_CONTRACT)."""
    out = {}
    for k, v in sent.items():
        if k == "s":
            out[k] = _cut_utf8(v, 4)
        elif k == "b":
            out[k] = blob(hd.bytes_of(v)[:4] if not isinstance(v, list) else bytes(v[:4]), b64)
        elif k == "au":
            out[k] = v[:3]
        elif k == "as":
            out[k] = [_cut_utf8(x, 4) for x in v[:3]]
        elif k == "n":
            out[k] = {"arr": v.get("arr", [])[:2]}
        else:
            raise ValueError(f"{k} is never clamped by contract")
    return out


def same(want, got) -> bool:
    if isinstance(want, list):
        if got is None:
            got = []
        return isinstance(got, list) and len(want) == len(got) \
            and all(same(w, g) for w, g in zip(want, got))
    if isinstance(want, dict):
        return isinstance(got, dict) and all(same(v, got.get(k)) for k, v in want.items())
    if isinstance(want, int) and not isinstance(want, bool):
        if isinstance(got, str) and got in ENUM:
            return ENUM[got] == want
        return hd.as_int(got) == want
    return want == got


def same_field(name, want, got) -> bool:
    """A blob compares by its bytes, whatever its spelling."""
    if name == "b":
        return hd.bytes_of(got) == (bytes(want) if isinstance(want, list) else hd.bytes_of(want))
    return same(want, got)


def run(cmd, argv, data, cwd):
    return subprocess.run(cmd + argv, input=data, cwd=cwd,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def tail(b: bytes) -> str:
    lines = [l.strip() for l in b.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:])[:400] if lines else "<no output>"


def crashed(p) -> bool:
    return p.returncode < 0 or p.returncode >= 126 or b"panicked" in p.stderr


REFUSALS = {}  # case -> the diagnostic of a refusal, printed by --discover


def verdict(cmd, cwd, name, msg, sent, kind, b64, clamp=False):
    """None when the case behaves as the rule wants, else the problem. With clamp,
    a refuse case is held to the clamp contract instead (CLAMP_CONTRACT)."""
    p = run(cmd, ["encode", msg], json.dumps(sent).encode(), cwd)
    if kind == "refuse" and clamp:
        if p.returncode != 0:
            return f"{name}: clamp contract: encode must succeed, harness exited {p.returncode}: {tail(p.stderr)}"
        d = run(cmd, ["decode", msg], p.stdout, cwd)
        if d.returncode != 0:
            return f"{name}: clamp contract: the clamped bytes do not decode: {tail(d.stderr)}"
        try:
            got = hd.loads_out(d.stdout.decode())
        except ValueError:
            return f"{name}: decode printed no JSON: {d.stdout[:120]!r}"
        for k, v in clamped(sent, b64).items():
            if not same_field(k, v, got.get(k)):
                return (f"{name}: clamp contract: {k} should read back as {json.dumps(v)[:60]}, "
                        f"got {json.dumps(got.get(k))[:60]}")
        return None
    if kind == "refuse":
        if crashed(p):
            return f"{name}: CRASHED instead of refusing (rc {p.returncode}): {tail(p.stderr)}"
        if p.returncode == 0:
            return (f"{name}: sent {json.dumps(sent)[:80]}, encode SUCCEEDED with "
                    f"{len(p.stdout)} byte(s) {p.stdout[:24].hex(' ')}")
        REFUSALS[name] = tail(p.stderr)
        return None
    if p.returncode != 0:
        return f"{name}: control must encode, harness exited {p.returncode}: {tail(p.stderr)}"
    d = run(cmd, ["decode", msg], p.stdout, cwd)
    if d.returncode != 0:
        return f"{name}: control encoded but does not decode: {tail(d.stderr)}"
    try:
        got = hd.loads_out(d.stdout.decode())
    except ValueError:
        return f"{name}: decode printed no JSON: {d.stdout[:120]!r}"
    for k, v in sent.items():
        if not same_field(k, v, got.get(k)):
            return f"{name}: round trip changed {k}: sent {json.dumps(v)[:60]}, got {json.dumps(got.get(k))[:60]}"
    return None


def model_harness(argv) -> int:
    """A harness for the probe schema that enforces every bound, minus `--defect`'s
    field. `encode` prints the JSON back as the "wire", `decode` echoes it."""
    defect = argv[argv.index("--defect") + 1] if "--defect" in argv else None
    verb = next(a for a in argv if a in ("encode", "decode"))
    data = sys.stdin.buffer.read()
    if verb == "decode":
        sys.stdout.buffer.write(data)
        return 0
    obj = json.loads(data)
    if "--clamp" in argv and argv[argv.index("encode") + 1] != CTRL:
        # A clamping container: strings, blobs and arrays are cut at assignment
        # (--cut-midchar cuts a string at its byte bound, ignoring characters).
        clampable = {k: v for k, v in obj.items() if k in ("s", "b", "au", "as", "n")}
        cut = clamped(clampable, "array")
        if "--cut-midchar" in argv and "s" in obj:
            cut["s"] = obj["s"].encode()[:4].decode(errors="replace")
        if defect in cut:
            del cut[defect]
        obj.update(cut)
        data = json.dumps(obj).encode()
    bad = []
    for k, v in obj.items():
        if k == defect or argv[argv.index("encode") + 1] == CTRL:
            continue
        if k == "s" and len(v.encode()) > 4:
            bad.append(k)
        elif k == "b" and len(hd.bytes_of(v)) > 4:
            bad.append(k)
        elif k == "au" and len(v) > 3:
            bad.append(k)
        elif k == "as" and (len(v) > 3 or any(len(x.encode()) > 4 for x in v)):
            bad.append(k)
        elif k == "n" and len(v.get("arr", [])) > 2:
            bad.append(k)
        elif k == "u8" and not 0 <= v <= 255:
            bad.append(k)
        elif k == "i16" and not -32768 <= v <= 32767:
            bad.append(k)
        elif k == "e" and not -128 <= v <= 127:
            bad.append(k)
        elif k == "f" and not 0 <= v <= 255:
            bad.append(k)
    if bad:
        print("error: over-bound " + ",".join(bad), file=sys.stderr)
        return 1
    sys.stdout.buffer.write(data)
    return 0


def self_test() -> int:
    table = cases("array", False)
    bad = []
    for defect in (None, "s", "b", "au", "as", "n", "u8", "i16", "e", "f"):
        cmd = [sys.executable, os.path.abspath(__file__), "--model-harness"] \
            + (["--defect", defect] if defect else [])
        got = {name for name, msg, sent, kind in table
               if verdict(cmd, None, name, msg, sent, kind, "array") is not None}
        want = set() if defect is None else \
            {n for n, _, sent, k in table if k == "refuse" and defect in sent}
        if got != want:
            bad.append(f"defect {defect}: failing cells {sorted(got)}, want {sorted(want)}")
    # The clamp contract: a clamping model passes every clampable case held to it,
    # and one that cuts mid-character or skips a field's clamp turns exactly those red.
    clampable = [(n, m, sent, k) for n, m, sent, k in table
                 if k == "refuse" and set(sent) <= {"s", "b", "au", "as", "n"}]
    base = [sys.executable, os.path.abspath(__file__), "--model-harness", "--clamp"]
    for extra, want in (([], set()),
                        (["--cut-midchar"], {"s_over_midchar"}),
                        (["--defect", "au"], {"au_count_plus_1", "au_count_plus_2"})):
        got = {n for n, m, sent, k in clampable
               if verdict(base + extra, None, n, m, sent, k, "array", clamp=True) is not None}
        if got != want:
            bad.append(f"clamp {extra}: failing cells {sorted(got)}, want {sorted(want)}")
    for b in bad:
        print("FAIL self-test " + b)
    if not bad:
        print(f"self-test: the model harness passes {len(table)} cases and each of 9 removed "
              f"guards turns exactly its own refuse cases red; the clamp contract holds "
              f"{len(clampable)} cases and catches a mid-character cut and a missing clamp")
    return 1 if bad else 0


def main() -> int:
    argv = sys.argv[1:]
    if "--model-harness" in argv:
        return model_harness(argv)
    if "--self-test" in argv:
        return self_test()
    bounded_only = "--bounded-only" in argv
    if "--emit-schema" in argv:
        return emit_schema(bounded_only)
    if "--" not in argv:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    label = head[0]

    def opt(name, default=None):
        return head[head.index(name) + 1] if name in head else default

    backend = opt("--backend")
    cwd = opt("--cwd")
    blob_json = opt("--blob-json", "array")
    discover = "--discover" in head or bool(os.environ.get("SOFAB_ENCODE_BOUNDS_DISCOVER"))
    if not discover and backend not in KNOWN_GAP:
        print(f"FAIL: --backend must be one of {sorted(KNOWN_GAP)}, got {backend!r}", file=sys.stderr)
        return 2
    table = cases(blob_json, bounded_only)
    names = {c[0] for c in table} | {c[0] for c in cases(blob_json, False)}
    for b in VARIANTS:
        assert not set(KNOWN_GAP[b]) & set(STORAGE_BLOCKED[b]), f"{b}: a cell is both a gap and blocked"
        assert not set(CLAMP_CONTRACT[b]) & set(STORAGE_BLOCKED[b]), f"{b}: a cell is both clamped and blocked"
        for n in CLAMP_CONTRACT[b]:
            assert n in names, f"CLAMP_CONTRACT[{b!r}] names an unknown case {n!r}"
        for n in KNOWN_GAP[b]:
            assert n in names, f"KNOWN_GAP[{b!r}] names an unknown case {n!r}"
        for n in STORAGE_BLOCKED[b]:
            assert n in names, f"STORAGE_BLOCKED[{b!r}] names an unknown case {n!r}"

    gap = set() if discover else set(KNOWN_GAP[backend])
    blocked = {} if discover else STORAGE_BLOCKED[backend]
    clamp = set() if discover else set(CLAMP_CONTRACT[backend])
    failures, open_gaps, stale, blk = [], [], [], []
    for name, msg, sent, kind in table:
        problem = verdict(cmd, cwd, name, msg, sent, kind, blob_json, clamp=name in clamp)
        if name in blocked:
            blk.append(name)
        elif name in gap:
            if problem is None:
                stale.append(f"{name}: passes now but is listed in KNOWN_GAP[{backend!r}] --"
                             " remove the entry")
            else:
                open_gaps.append(name)
        elif problem is not None:
            failures.append(problem)

    if discover:
        print(f"==> [{label}] discover: {len(failures)} of {len(table)} cell(s) fail")
        for f in failures:
            print("  - " + f)
        for n, why in REFUSALS.items():
            print(f"  . refused {n}: {why}")
        print("DISCOVER " + json.dumps([f.split(":")[0] for f in failures]))
        return 0
    failures += stale
    if failures:
        print(f"FAIL: [{label}] encode bounds (generator#656):", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1
    held = sorted(clamp & {c[0] for c in table})
    print(f"==> [{label}] encode bounds (generator#656): {len(table)} case(s),"
          f" {len(table) - len(open_gaps) - len(blk)} pass ({len(held)} by clamp contract),"
          f" {len(blk)} storage-blocked {sorted(blk)}, KNOWN_GAP[{backend}] = {sorted(open_gaps)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
