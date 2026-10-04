#!/usr/bin/env python3
"""Round-trip every corpus and realworld message at runtime, not just build it.

Usage:
  check_corpus_roundtrip.py <label> --ir FILE --def NAME [--tally FILE]
                            [--cwd DIR] [--int64-json number|string] [--int64-safe]
                            [--exclude MESSAGE=REASON ...] [--exclude-all REASON]
                            -- <harness argv...>
  check_corpus_roundtrip.py --summary <label> --tally FILE

`FILE` is `sofabgen --dump-ir --lang <lang> --in <definition>`: the resolved IR,
so a cross-file `$ref` is already spelled out and a $defs-only file has no
message. `NAME` is the definition's name (its file without `.yaml`).

## The gap this closes

Every suite generates and builds `tests/matrix/corpus/defs/*.yaml` and
`examples/messages/realworld/*.yaml`, and until this driver none of them ran the
result. The shapes that exist only in those files -- cross-file `$ref` types,
nested wrapper rows of string/blob/struct, a declared field id at ID_MAX (2^31-1,
a 5-byte header varint) -- were compiled and never executed, so a backend could
emit code for them that compiles and decodes wrongly with every suite green.

## What is asserted, per message

  1. A value-filled message is derived from the IR, deterministically. Every
     field is OFF its default and inside its declared bounds; 64-bit values lie
     above 2^53; a wrapper array carries a default element in its interior and a
     non-default last element; a union holds its default option at a non-default
     value. The schema's defaults are not in the IR, so the harness tells: it
     decodes an EMPTY message, and any candidate equal to what comes back is
     replaced by the next one (`check_nondefault.py` then confirms it).
  2. It is encoded and decoded through `decode` and through `streamdecode`
     (which drips the message), and every field the fixture sets must come back
     with the same value (`json_equal.diff`, so a 64-bit integer printed as a
     decimal string and a byte array printed as base64 are the same data). The
     decoded message carries more than the fixture -- the defaults it left out
     -- and that is not a difference.
  3. A message holding a union gets a second round, with each union holding an
     option other than its default.

A message that cannot be carried is an explicit exclusion with a reason, printed
and counted; nothing is skipped silently. `--summary` prints the totals the
suites quote: `<n> definitions, <m> messages round-tripped, <k> excluded`.

The SHA-256 of each encoding is appended to the tally (`H` lines), so a later
step can compare them across suites: every backend must produce the same bytes.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import check_nondefault  # noqa: E402
import harness_dialect as hd  # noqa: E402
import json_equal  # noqa: E402


class W(int):
    """A 64-bit integer, spelled per the harness's JSON dialect on the way in."""


# Candidates per scalar kind, in the order they are tried. The first is always
# used unless it equals the field's default; 64-bit ones lie above 2^53.
INTS = {
    "u8": [200, 77, 5],
    "u16": [51234, 4321, 7],
    "u32": [3000000001, 70001, 9],
    "u64": [18446744073709551557, 9007199254740993, 11],
    "i8": [-100, 99, -3],
    "i16": [-31000, 12345, 3],
    "i32": [-2000000001, 123456789, 5],
    "i64": [-9007199254740993, 9223372036854775800, -7],
}
WIDE_KINDS = ("u64", "i64")
# The same two kinds for a harness that holds a 64-bit integer as a double
# (TypeScript `int64: number`): large, and still below 2^53.
SAFE_INTS = {
    "u64": [4503599627370497, 123456789012, 11],
    "i64": [-4503599627370497, 98765432101, -7],
}
# Exactly representable in 32 bits, so a narrowing is value-preserving.
FLOATS = {
    "fp32": [1.5, -2.25, 0.125, 3.5],
    "fp64": [-2.5, 12345.678, 0.1, 6.25],
}
BYTES = [0, 255, 128, 7, 1, 2, 3, 4]
WORDS = ["abc", "xyz", "hello", "q"]


class Gen:
    def __init__(self, ir, safe=False):
        self.named = {n["key"]: n for n in ir.get("named") or []}
        self.ints = dict(INTS, **SAFE_INTS) if safe else INTS
        self.limit = 1 << 53 if safe else 1 << 64

    # -- helpers -----------------------------------------------------------

    @staticmethod
    def same(a, b):
        """Equal as data, across the harness's spelling of a 64-bit integer or a
        byte array."""
        if b is None:
            return False
        return not list(json_equal.diff(a, b, int_strings=True, base64_bytes=True)) and not list(
            json_equal.diff(b, a, int_strings=True, base64_bytes=True))

    def consts(self, ref):
        return [c["value"] for c in self.named[ref].get("consts") or []]

    def mask_values(self, ref):
        """Non-zero combinations of the declared flags, widest set first."""
        bits = [1 << f["pos"] for f in self.named[ref].get("flags") or []]
        full = 0
        for b in bits:
            full |= b
        out = [full]
        out += [b for b in reversed(bits)]
        return [x for x in out if x < self.limit] or [0]

    # -- one value ---------------------------------------------------------

    def scalar(self, kind, ref, maxlen, decimals, v):
        """Candidate `v` of a leaf kind."""
        if kind in self.ints:
            c = self.ints[kind][v % len(self.ints[kind])]
            return W(c) if kind in WIDE_KINDS else c
        if kind in FLOATS:
            if decimals is not None and decimals == 0:
                return float([3, 5, 9, 7][v % 4])
            return FLOATS[kind][v % len(FLOATS[kind])]
        if kind == "boolean":
            return v % 2 == 0
        if kind == "enum":
            vals = self.consts(ref)
            nz = [x for x in vals if x != 0] or vals
            return nz[v % len(nz)]
        if kind == "bitfield":
            vals = self.mask_values(ref)
            c = vals[v % len(vals)]
            return W(c) if c >= 1 << 53 else c
        if kind == "string":
            n = 5 if maxlen is None else max(1, min(maxlen, 5))
            return (WORDS[v % len(WORDS)] * 3)[:n]
        if kind == "blob":
            n = 3 if maxlen is None else max(1, min(maxlen, 3))
            return [BYTES[(v + i) % len(BYTES)] for i in range(n)]
        raise ValueError(f"no value for kind {kind}")

    def zero(self, kind, ref):
        """The element a sparse array may leave out: a default one."""
        if kind in INTS:
            return W(0) if kind in WIDE_KINDS else 0
        if kind in FLOATS:
            return 0.0
        if kind == "boolean":
            return False
        if kind == "enum":
            return 0 if 0 in self.consts(ref) else None
        if kind == "bitfield":
            return 0
        if kind == "string":
            return ""
        if kind == "blob":
            return []
        if kind == "struct":
            return {}
        return None

    def value(self, spec, base, v=0, alt=False, row=None):
        """A value for `spec` (a dict with kind/ref/maxlen/...), off `base`. `row`
        is the index of this array within the array that holds it, if any."""
        kind = spec["kind"]
        if kind == "struct":
            return self.fields(self.named[spec["ref"]]["fields"], base, alt, v)
        if kind == "union":
            return self.union(spec["ref"], base, alt, v)
        if kind == "array":
            for attempt in range(4):
                val = self.array(spec, v + attempt, alt, row)
                if not self.same(val, base):
                    return val
            return val
        for attempt in range(6):
            val = self.scalar(kind, spec.get("ref"), spec.get("maxlen"), spec.get("decimals"), v + attempt)
            if not self.same(val, base):
                return val
        return val

    def fields(self, fields, base, alt, v=0):
        """Every field, each at its own candidate (`v` plus its position), so two
        fields of one type -- a struct's `min` and `max` -- never carry the same
        value and a mix-up between them is visible."""
        out = {}
        base = base if isinstance(base, dict) else {}
        for i, f in enumerate(fields):
            out[f["name"]] = self.value(field_spec(f), base.get(f["name"]), v + i, alt)
        return out

    def union(self, ref, base, alt, v=0):
        u = self.named[ref]
        opts = u["fields"]
        by_id = {o["id"]: o for o in opts}
        held = by_id.get(u.get("default_id"), opts[0])
        if alt:
            others = [o for o in opts if o is not held]
            held = others[0] if others else held
        sub = base.get(held["name"]) if isinstance(base, dict) and not alt else None
        return {held["name"]: self.value(field_spec(held), sub, v, alt)}

    def array(self, spec, v, alt, row=None):
        cap = spec.get("count") or 0
        n = 3 if cap == 0 else min(cap, 4)
        elem = spec["elem"]
        out = []
        for i in range(n):
            # A default element in the interior of an array of three or more; in a
            # two-element row, the first element of the second row.
            at = 1 if n >= 3 else (0 if n == 2 and row == 1 else None)
            interior_default = i == at and elem["kind"] not in ("array", "union")
            if interior_default:
                z = self.zero(elem["kind"], elem.get("ref"))
                if z is not None:
                    out.append(z)
                    continue
            out.append(self.value(elem, None, v + i, alt, i if elem["kind"] == "array" else None))
        return out


def field_spec(f):
    """A field (or union option) of the IR as one recursive spec."""
    spec = {k: f[k] for k in ("kind", "ref", "maxlen", "decimals", "count") if k in f}
    if f["kind"] == "array":
        spec["elem"] = elem_spec(f["elem"], f.get("elem_ref"), f.get("elem_maxlen"),
                                 f.get("elem_items"), f.get("decimals"))
    return spec


def elem_spec(elem, ref, maxlen, items, decimals=None):
    if elem == "array":
        return {"kind": "array", "count": items.get("count"),
                "elem": elem_spec(items["elem"], items.get("elem_ref"), items.get("elem_maxlen"),
                                  items.get("elem_items"), decimals)}
    s = {"kind": elem}
    if ref:
        s["ref"] = ref
    if maxlen is not None:
        s["maxlen"] = maxlen
    if decimals is not None:
        s["decimals"] = decimals
    return s


def has_union(ir, fields, seen=None):
    seen = seen if seen is not None else set()
    named = {n["key"]: n for n in ir.get("named") or []}

    def spec_has(spec):
        if spec["kind"] == "union":
            return True
        if spec["kind"] == "struct":
            key = spec["ref"]
            if key in seen:
                return False
            seen.add(key)
            return any(spec_has(field_spec(f)) for f in named[key]["fields"])
        if spec["kind"] == "array":
            return spec_has(spec["elem"])
        return False

    return any(spec_has(field_spec(f)) for f in fields)


def plain(o):
    if isinstance(o, dict):
        return {k: plain(v) for k, v in o.items()}
    if isinstance(o, list):
        return [plain(v) for v in o]
    if isinstance(o, W):
        return int(o)
    return o


def spell(o, dialect):
    if isinstance(o, dict):
        return {k: spell(v, dialect) for k, v in o.items()}
    if isinstance(o, list):
        return [spell(v, dialect) for v in o]
    if isinstance(o, W):
        return hd.int64_in(int(o), dialect)
    return o


def null_as_empty(exp, act):
    """A backend prints an empty array it holds no storage for as null (Go's nil
    slice); that is rendering, so null reads as [] wherever the fixture has one."""
    if act is None and exp == []:
        return []
    if isinstance(exp, dict) and isinstance(act, dict):
        return {k: null_as_empty(exp[k], v) if k in exp else v for k, v in act.items()}
    if isinstance(exp, list) and isinstance(act, list):
        return [null_as_empty(e, a) for e, a in zip(exp, act)] + act[len(exp):]
    return act


def die(msg):
    sys.stderr.write(f"check_corpus_roundtrip: {msg}\n")
    raise SystemExit(2)


def run(argv, data, cwd):
    return subprocess.run(argv, input=data, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def fail(label, where, msg):
    sys.stderr.write(f"FAIL: {label}: {where}: {msg}\n")
    raise SystemExit(1)


def roundtrip(label, name, msg, gen, args):
    """Round-trip one message. Returns the SHA-256 of the main encoding."""
    where = f"{name}.{msg['name']}"

    def harness(verb, data):
        r = run(args.harness + [verb, msg["name"]], data, args.cwd)
        if r.returncode != 0:
            fail(label, where, f"`{verb}` exited {r.returncode}: "
                 f"{(r.stderr or r.stdout).decode(errors='replace').strip()[-400:]}")
        return r.stdout

    def load(raw, what):
        try:
            return json.loads(raw)
        except ValueError as e:
            fail(label, where, f"{what} is not JSON ({e}): {raw[:200]!r}")

    base = load(harness("decode", harness("encode", b"{}")), "the empty message's decode")
    digest = None
    rounds = [("", False)]
    if has_union(gen.ir, msg["fields"]):
        rounds.append((" (non-default union options)", True))
    for tag, alt in rounds:
        fixture = gen.fields(msg["fields"], base if not alt else None, alt)
        enc = harness("encode", json.dumps(spell(fixture, args.int64_json)).encode())
        if not enc:
            fail(label, where + tag, "a value-filled message encoded to zero bytes")
        if not alt:
            digest = hashlib.sha256(enc).hexdigest()
            lines = list(check_nondefault.walk(base, plain(fixture), set()))
            if lines:
                fail(label, where, "the fixture sits on the schema default:\n" + "\n".join(lines))
        for verb in ("decode", "streamdecode"):
            got = null_as_empty(plain(fixture), load(harness(verb, enc), f"`{verb}` output"))
            lines = [ln for ln in json_equal.diff(plain(fixture), got, int_strings=True, base64_bytes=True)
                     if "EXTRA" not in ln]
            if lines:
                fail(label, where + tag, f"`{verb}` does not return what was encoded:\n" + "\n".join(lines))
    return digest


def main():
    ap = argparse.ArgumentParser(add_help=False)
    ap.add_argument("label", nargs="?")
    ap.add_argument("--ir")
    ap.add_argument("--def", dest="name")
    ap.add_argument("--tally")
    ap.add_argument("--summary", action="store_true")
    ap.add_argument("--cwd")
    ap.add_argument("--int64-json", default="number")
    ap.add_argument("--int64-safe", action="store_true")
    ap.add_argument("--exclude", action="append", default=[])
    ap.add_argument("--exclude-all")
    argv = sys.argv[1:]
    harness = []
    if "--" in argv:
        sep = argv.index("--")
        harness = argv[sep + 1:]
        argv = argv[:sep]
    args = ap.parse_args(argv)
    args.harness = harness
    args.int64_json = hd.check_dialect(args.int64_json)
    label = args.label or "corpus"

    if args.summary:
        if not args.tally or not os.path.exists(args.tally):
            die("--summary needs a --tally file that exists")
        defs, ok, excl, reasons = set(), 0, 0, []
        with open(args.tally) as f:
            lines = f.read().splitlines()
        for ln in lines:
            p = ln.split("\t")
            if p[0] == "D":
                defs.add(p[1])
            elif p[0] == "R":
                ok += 1
            elif p[0] == "X":
                excl += 1
                reasons.append(f"  excluded {p[1]}.{p[2]}: {p[3]}")
        print(f"{label} corpus round-trip: {len(defs)} definitions, {ok} messages round-tripped, {excl} excluded")
        for r in reasons:
            print(r)
        if ok == 0:
            sys.stderr.write(f"FAIL: {label}: no message was round-tripped\n")
            return 1
        return 0

    if not args.ir or not args.name:
        die("--ir and --def are required")
    with open(args.ir) as f:
        ir = json.load(f)
    msgs = ir.get("messages") or []
    excl = dict(e.split("=", 1) for e in args.exclude)
    gen = Gen(ir, args.int64_safe)
    gen.ir = ir
    out = [f"D\t{args.name}"]
    done = skipped = 0
    for m in msgs:
        reason = args.exclude_all or excl.get(m["name"])
        if reason:
            out.append(f"X\t{args.name}\t{m['name']}\t{reason}")
            print(f"  {label}: {args.name}.{m['name']} excluded: {reason}")
            skipped += 1
            continue
        if not args.harness:
            die("no harness argv given (put it after `--`)")
        digest = roundtrip(label, args.name, m, gen, args)
        out.append(f"R\t{args.name}\t{m['name']}")
        out.append(f"H\t{args.name}\t{m['name']}\t{digest}")
        done += 1
    if not msgs:
        print(f"  {label}: {args.name} declares no message ($defs only, covered where it is referenced)")
    print(f"  {label}: {args.name}: {done} message(s) round-tripped, {skipped} excluded")
    if args.tally:
        with open(args.tally, "a") as f:
            f.write("\n".join(out) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
