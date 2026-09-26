#!/usr/bin/env python3
"""MESSAGE_SPEC §4.2 / §7.4.1 — a union holds EXACTLY ONE option (generator#608).

Usage:
  check_union.py --emit-schema        # the WHOLE document: version, $defs, messages
  check_union.py --self-test          # the builders against hand-written hex
  check_union.py <label> [--cwd DIR] [--sizes 1,2,3,5,0] [--no-stream]
                 [--int64-json number|string] [--int64-safe]
                 [--known-gap CASE=REASON]... [--message NAME] -- <harness argv...>

## The rule

* **Value.** A union holds one option. A fresh union holds `default_id` (`D`)
  at that option's own default; selecting another option discards the held one
  and the new one starts from its own default.
* **Encode.** `D` is written like an ordinary field of its kind (omitted at its
  default). Any other held option is written EVEN AT ITS OWN DEFAULT: a scalar as
  its value, a string/blob/compact array in its explicit empty form, a
  struct/union/wrapper-array option as a present frame (`end_keep`). A union
  holding `D` at its default is omitted, so a union frame is never empty on a
  conformant encoder's output.
* **Decode (§7.4.1).** The last correctly-typed option wins: a different option
  replaces the held one and starts from its default, the held option continues
  under §7.4 (a struct merges, everything else is replaced). A child skipped
  under §7.3 (wrong wire type, fixlen subtype or array kind) and an unknown id do
  NOTHING -- no switch. Several children, a re-opened frame and an empty frame
  are all legal, never INVALID.
* **JSON.** A union is `{"<option>": value}` with exactly the held option.

## Why a driver

Several children in one union frame, an empty frame, a mistyped option id: no
conformant encoder emits any of them, so no round-trip vector can carry them.
This driver forges the wire image from MESSAGE_SPEC §4.3-§4.9 and CORELIB_PLAN
§4.4-§4.8 with its own builders (checked by `--self-test` against hand-written
hex), and takes every expectation from its own schema -- nothing is taken from
the backend under test.

## What runs

* ENCODE cases: JSON in, the exact wire out; then that wire decoded one-shot and
  streamed at every `--sizes` split must equal the input with the defaults
  filled in. So every encode case is a streamed decode case as well.
* DECODE cases: forged wire in, JSON out, one-shot and streamed; the cases marked
  `re` also re-encode the decoded JSON and compare against the canonical wire.

The whole decoded message is compared, every field, not only the one a case is
about: a frame desync shows up in the sibling `w` right after `u` and `v`.

Comparison: a union level is STRICT -- the decoded union must be an object with
exactly the expected single member (a harness that prints every option fails
there, which is the point). Below it, each harness's JSON dialect is tolerated:
member order, `null` for an empty list, an integer spelled as a string, a byte
array spelled as base64, an empty blob as `""`/`[]`/`null`. A float compares by
the bit pattern of its declared width.

## 64-bit values

`--int64-json number|string` picks how a 64-bit scalar (`q.big`, `q.sig`) is
spelled in the ENCODE input: `number` (default) is a bare JSON integer, the
dialect of `maxsize_fill.json`; `string` is the quoted decimal, for a harness
whose JSON front door is a double (TypeScript). The two are not
interchangeable -- C, C++ and Zig read a quoted 64-bit value as 0 -- so each
harness is driven in the one it reads exactly and a wrong-dialect run fails
loudly. On the way out, 64-bit values compare by value in either spelling.

`--int64-safe` is for a harness whose 64-bit scalar is a JS `number`: it
replaces E26 `sig` by 2^53-1, E27 `sig` by -(2^53-1) and E28 `big` by 2^53-1,
input, wire and expectation alike.

## Loud, never quiet

Every case runs; a harness failure is a failure; the case count, the int64
dialect and every substitution are printed. `--known-gap CASE=REASON` runs the
case anyway and reports it under KNOWN GAP instead of failing on it; a known gap
that passes is reported as such, so the flag is dropped rather than kept.
"""
import base64
import json
import math
import struct
import subprocess
import sys

MSG = "uni"

# Wire types (CORELIB_PLAN §4.3). Normative; do not renumber.
WT_UNSIGNED, WT_SIGNED, WT_FIXLEN, WT_ARR_U, WT_ARR_S, WT_ARR_F, WT_SEQ = 0, 1, 2, 3, 4, 5, 6
END = b"\x07"                     # sequence end marker (§4.9)
SUB_FP32, SUB_FP64, SUB_STRING, SUB_BLOB = 0, 1, 2, 3   # fixlen subtypes (§4.6)
FP32, FP64 = "fp32", "fp64"


SCHEMA = """\
# The tagged-union message (generator#608), printed by
# tests/conformance/lib/check_union.py so the schema and the driver that forges
# union bytes against it have one definition between them. Every array is
# bounded, so the statically sized profiles build it too.
version: 1
$defs:
  union:
    Pick:                                  # one $defs union, three sites, two default_ids -> split
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
      s: { id: 2, type: string, maxlen: 4 }
messages:
  uni:
    payload:
      u:                                   # default_id on a STRUCT option that is NOT the first
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:    { id: 5, type: blob, maxlen: 4 }
          inner: { id: 6, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
          box:   { id: 7, type: struct, fields: { z: { id: 0, type: u8, default: 3 } } }
          e:     { id: 8, type: enum, enum: { A: 0, B: 1, C: 2 }, default: 2 }
          fl:    { id: 9, type: bitfield, bits: { r: { pos: 0 }, w: { pos: 1, default: true } } }
          f:     { id: 10, type: fp32, default: 1.5 }
          fa:    { id: 11, type: array, items: { type: fp32, count: 2 } }
          bo:    { id: 12, type: boolean, default: true }
      v:                                   # array of unions, default_id on the NON-first option
        id: 1
        type: array
        items:
          type: union
          count: 4
          default_id: 1
          oneof:
            i: { id: 0, type: i32 }
            s: { id: 1, type: string, maxlen: 8 }
      w: { id: 2, type: u8 }               # a sibling right after u and v: a frame desync shows up here
      v2:                                  # array of unions whose D is a STRUCT with a NON-ZERO default
        id: 3
        type: array
        items:
          type: union
          count: 3
          default_id: 1
          oneof:
            a: { id: 0, type: u8 }
            p: { id: 1, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } }
      q:                                   # 64-bit options: a u64 non-D and an i64 D
        id: 4
        type: union
        default_id: 1
        oneof:
          big: { id: 0, type: u64 }
          sig: { id: 1, type: i64 }
      pf: { id: 5, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      pe: { id: 6, type: array, items: { type: union, count: 3, default_id: 0, oneof: { $ref: "#/$defs/union/Pick" } } }
      po: { id: 7, type: union, oneof: { $ref: "#/$defs/union/Pick" } }
      r:                                   # D is itself a UNION whose own D is NOT its first option
        id: 8
        type: union
        default_id: 0
        oneof:
          nu: { id: 0, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8, default: 4 } } }
          ar: { id: 1, type: array, items: { type: u8, count: 2 } }
      r2:                                  # D is a COMPACT ARRAY
        id: 9
        type: union
        default_id: 0
        oneof:
          ca: { id: 0, type: array, items: { type: u8, count: 2 } }
          x:  { id: 1, type: u8 }
      g:                                   # union ELEMENT two array levels down, non-first D at a non-zero default
        id: 10
        type: array
        items: { type: array, count: 2, items: { type: union, count: 2, default_id: 1, oneof: { lo: { id: 0, type: u8 }, hi: { id: 1, type: u32, default: 4 } } } }
      r3:                                  # D is a WRAPPER ARRAY
        id: 11
        type: union
        default_id: 0
        oneof:
          ws: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }
          x:  { id: 1, type: u8 }
"""


# --- the schema, as the comparator sees it ----------------------------------
#
# A mirror of SCHEMA above: what each member is, and its default. It drives the
# default fill (an absent member IS its default) and the comparison (a union
# level is strict). Keep the two in step.

class Leaf:
    def __init__(self, kind, default=0):
        self.kind, self.default = kind, default


class Struct:
    def __init__(self, **fields):
        self.fields = fields


class Union:
    def __init__(self, dflt, **options):
        assert dflt in options
        self.dflt, self.options = dflt, options


class Array:
    def __init__(self, elem):
        self.elem = elem


def u(d=0):
    return Leaf("int", d)


PICK_OPTS = dict(n=u(6), t=Struct(k=u(2)), s=Leaf("string", ""))

SHAPE = Struct(
    u=Union("pt",
            num=u(5), s=Leaf("string", ""), pt=Struct(x=u(7), y=u()),
            arr=Array(u()), strs=Array(Leaf("string", "")), bl=Leaf("blob", b""),
            inner=Union("b", a=u(), b=u(-2)), box=Struct(z=u(3)),
            e=u(2), fl=u(2), f=Leaf(FP32, 1.5), fa=Array(Leaf(FP32, 0.0)),
            bo=Leaf("bool", True)),
    v=Array(Union("s", i=u(), s=Leaf("string", ""))),
    w=u(),
    v2=Array(Union("p", a=u(), p=Struct(q=u(9)))),
    q=Union("sig", big=Leaf("u64"), sig=Leaf("i64")),
    pf=Union("t", **PICK_OPTS),
    pe=Array(Union("n", **PICK_OPTS)),
    po=Union("n", **PICK_OPTS),
    r=Union("nu", nu=Union("b", a=u(), b=u(4)), ar=Array(u())),
    r2=Union("ca", ca=Array(u()), x=u()),
    g=Array(Array(Union("hi", lo=u(), hi=u(4)))),
    r3=Union("ws", ws=Array(Leaf("string", "")), x=u()),
)


def fill(desc, value):
    """`value` with every absent member replaced by its declared default."""
    if isinstance(desc, Leaf):
        return desc.default if value is None else value
    if isinstance(desc, Struct):
        value = value or {}
        return {k: fill(d, value.get(k)) for k, d in desc.fields.items()}
    if isinstance(desc, Union):
        if value is None:
            return {desc.dflt: fill(desc.options[desc.dflt], None)}
        (opt, v), = value.items()
        return {opt: fill(desc.options[opt], v)}
    return [fill(desc.elem, x) for x in (value or [])]


# --- the wire image ---------------------------------------------------------

def varint(n: int) -> bytes:
    assert n >= 0
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def zigzag(n: int) -> int:
    return (-n << 1) - 1 if n < 0 else n << 1


def header(fid: int, wt: int) -> bytes:
    """`(id << 3) | wire_type`, MESSAGE_SPEC §4.3."""
    return varint((fid << 3) | wt)


def unsigned(fid: int, n: int) -> bytes:
    """An unsigned integer (also boolean and bitfield), §4.4."""
    return header(fid, WT_UNSIGNED) + varint(n)


def signed(fid: int, n: int) -> bytes:
    """A signed integer (also enum), zig-zag encoded, §4.5."""
    return header(fid, WT_SIGNED) + varint(zigzag(n))


def fixlen(fid: int, sub: int, payload: bytes) -> bytes:
    """`fixlen_word` = (length << 3) | subtype, then the payload, §4.6."""
    return header(fid, WT_FIXLEN) + varint((len(payload) << 3) | sub) + payload


def string(fid: int, s: str) -> bytes:
    return fixlen(fid, SUB_STRING, s.encode())


def blob(fid: int, b: bytes) -> bytes:
    return fixlen(fid, SUB_BLOB, b)


def fp32(fid: int, x: float) -> bytes:
    return fixlen(fid, SUB_FP32, struct.pack("<f", x))


def fp64(fid: int, x: float) -> bytes:
    return fixlen(fid, SUB_FP64, struct.pack("<d", x))


def uarray(fid: int, values) -> bytes:
    """An unsigned-integer array: header, count, the elements (§4.7)."""
    return header(fid, WT_ARR_U) + varint(len(values)) + b"".join(varint(v) for v in values)


def sarray(fid: int, values) -> bytes:
    """A signed-integer array: zig-zag elements (§4.7)."""
    return header(fid, WT_ARR_S) + varint(len(values)) + b"".join(varint(zigzag(v)) for v in values)


def farray(fid: int, values, sub: str) -> bytes:
    """A fixlen array: count, ONE `fixlen_word` even when the count is 0, then the
    packed elements (CORELIB_PLAN §4.8)."""
    word, fmt = (0x20, "<f") if sub == FP32 else (0x41, "<d")
    return (header(fid, WT_ARR_F) + varint(len(values)) + varint(word)
            + b"".join(struct.pack(fmt, v) for v in values))


def seq(fid: int, *parts: bytes) -> bytes:
    """A sequence: `sequence start` at fid, the children, the end marker (§4.9)."""
    return header(fid, WT_SEQ) + b"".join(parts) + END


# Field ids of the message (the schema above).
U, V, W, V2, Q, PF, PE, PO, R, R2, G, R3 = range(12)

SAFE = 2**53 - 1
MAX_LINES = 12        # failure lines printed per case


# --- the cases --------------------------------------------------------------

def encode_cases(safe: bool):
    """(name, JSON input, expected wire, what it pins)."""
    e26 = SAFE if safe else 2**60 + 1
    e27 = -SAFE if safe else -(2**60 + 1)
    e28 = SAFE if safe else 2**64 - 1
    return [
        ("E1", {}, b"", "a fresh message is zero bytes (§2)"),
        ("E2", {"u": {"pt": {"x": 7, "y": 0}}}, b"", "D at its default is omitted"),
        ("E3", {"u": {"pt": {"x": 1}}}, seq(U, seq(2, signed(0, 1))), "D framed normally"),
        ("E4", {"u": {"num": 5}}, seq(U, unsigned(0, 5)), "forced write, scalar at its own default"),
        ("E5", {"u": {"s": ""}}, seq(U, string(1, "")), "forced write, empty string"),
        ("E6", {"u": {"arr": []}}, seq(U, uarray(3, [])), "forced write, compact array count 0"),
        ("E7", {"u": {"strs": []}}, seq(U, seq(4)), "end_keep on an empty wrapper option"),
        ("E8", {"u": {"bl": []}}, seq(U, blob(5, b"")), "forced write, empty blob"),
        ("E9", {"u": {"inner": {"b": -2}}}, seq(U, seq(6)), "end_keep, union option at its default"),
        ("E10", {"u": {"inner": {"a": 0}}}, seq(U, seq(6, unsigned(0, 0))),
         "forced write one level down"),
        ("E11", {"u": {"box": {"z": 3}}}, seq(U, seq(7)),
         "end_keep on a non-D struct option: present, empty"),
        ("E12", {"v": [{"i": 4}, {"s": ""}, {"i": 0}, {"s": "z"}]},
         seq(V, seq(0, signed(0, 4)), seq(2, signed(0, 0)), seq(3, string(1, "z"))),
         "element 1 = element default (gap); element 2 = non-D at its default, framed"),
        ("E13", {"v": [{"i": 1}, {"s": ""}]}, seq(V, seq(0, signed(0, 1)), seq(1)),
         "last element end_keep"),
        ("E15", {"u": {"e": 2}}, seq(U, signed(8, 2)), "forced write through the enum guard"),
        ("E16", {"u": {"fl": 2}}, seq(U, unsigned(9, 2)), "forced write through the bitfield guard"),
        ("E17", {"u": {"f": 1.5}}, seq(U, fp32(10, 1.5)), "forced write through the float guard"),
        ("E18", {"u": {"bo": True}}, seq(U, unsigned(12, 1)), "forced write through the boolean guard"),
        ("E19", {"u": {"fa": []}}, seq(U, farray(11, [], FP32)),
         "an empty fp32 array keeps its fixlen_word (CORELIB_PLAN §4.8)"),
        ("E20", {"v2": [{"a": 1}, {"p": {"q": 9}}]}, seq(V2, seq(0, unsigned(0, 1)), seq(1)),
         "struct-D element at its default, last: end_keep"),
        ("E21", {"v2": [{"p": {"q": 9}}, {"a": 2}]}, seq(V2, seq(1, unsigned(0, 2))),
         "interior gap = D at its NON-ZERO default"),
        ("E22", {"v2": [{"p": {"q": 4}}]}, seq(V2, seq(0, seq(1, unsigned(0, 4)))),
         "struct D framed normally inside an element"),
        ("E23", {"q": {"big": 0}}, seq(Q, unsigned(0, 0)), "forced write of a 64-bit option"),
        ("E24", {"q": {"sig": 0}}, b"", "64-bit D omitted"),
        ("E25", {"q": {"sig": 2**32}}, seq(Q, signed(1, 2**32)),
         "i64 D whose low word is zero: the omission test must look at the high word"),
        ("E26", {"q": {"sig": e26}}, seq(Q, signed(1, e26)), "i64 D, wide"),
        ("E27", {"q": {"sig": e27}}, seq(Q, signed(1, e27)), "i64 D, wide negative"),
        ("E28", {"q": {"big": e28}}, seq(Q, unsigned(0, e28)), "u64 non-D, max"),
        ("E29", {"pf": {"t": {"k": 2}}}, b"", "$defs field: its D (t) at its default"),
        ("E30", {"pf": {"n": 6}}, seq(PF, unsigned(0, 6)), "n is NOT D at this site: forced"),
        ("E31", {"po": {"n": 6}}, b"", "omitted default_id = lowest id (n), at its default"),
        ("E32", {"po": {"t": {"k": 2}}}, seq(PO, seq(1)), "end_keep on the omitted-default site"),
        ("E33", {"pe": [{"t": {"k": 2}}]}, seq(PE, seq(0, seq(1))),
         "option end_keep inside the last element's end_keep"),
        ("E34", {"pe": [{"n": 6}, {"s": "x"}]}, seq(PE, seq(1, string(2, "x"))),
         "element gap = the ELEMENT site's D (n = 6), not the field's (t)"),
        ("E35", {"r": {"nu": {"b": 4}}}, b"", "union D holding its own D at default: omitted"),
        ("E36", {"r": {"nu": {"a": 0}}}, seq(R, seq(0, unsigned(0, 0))),
         "union D holding its non-D at 0 is NOT default: written"),
        ("E37", {"r": {"nu": {"b": 5}}}, seq(R, seq(0, unsigned(1, 5))), "union D framed normally"),
        ("E38", {"r": {"ar": []}}, seq(R, uarray(1, [])), "forced write, count 0"),
        ("E39", {"r2": {"ca": []}}, b"", "compact-array D at its (empty) default: omitted"),
        ("E40", {"r2": {"ca": [1]}}, seq(R2, uarray(0, [1])), "compact-array D set"),
        ("E41", {"r2": {"x": 0}}, seq(R2, unsigned(1, 0)), "scalar non-D beside an array D: forced"),
        ("E42", {"r3": {"ws": []}}, b"", "wrapper-array D at its default: omitted"),
        ("E43", {"r3": {"ws": ["a"]}}, seq(R3, seq(0, string(0, "a"))), "wrapper-array D framed"),
        ("E44", {"g": [[{"hi": 4}, {"lo": 1}]]}, seq(G, seq(0, seq(1, unsigned(0, 1)))),
         "inner element 0 = D (hi = 4) -> gap; two array levels down"),
    ]


def decode_cases():
    """(name, wire, expected members, re-encode wire or None, what it pins)."""
    return [
        ("D0", b"", {}, None, "the empty message is every field's default"),
        ("D1", seq(U, unsigned(0, 9), string(1, "x")), {"u": {"s": "x"}},
         seq(U, string(1, "x")), "several children: the last wins (§7.4.1)"),
        ("D2", seq(U, unsigned(0, 9), string(1, "x"), seq(2, signed(1, 3))),
         {"u": {"pt": {"x": 7, "y": 3}}}, None, "a new struct option starts from its default"),
        ("D3", seq(U, unsigned(0, 9)) + seq(U, string(1, "x")), {"u": {"s": "x"}}, None,
         "switch across re-opened frames"),
        ("D4", seq(U, seq(2, signed(0, 1))) + seq(U, seq(2, signed(1, 2))),
         {"u": {"pt": {"x": 1, "y": 2}}}, None, "the same struct option merges across frames (§7.4)"),
        ("D5", seq(U, seq(2, signed(0, 1)), seq(2, signed(1, 2))),
         {"u": {"pt": {"x": 1, "y": 2}}}, None, "the same struct option merges within a frame"),
        ("D6", seq(U, seq(2, signed(0, 1))) + seq(U, unsigned(0, 5)) + seq(U, seq(2, signed(1, 2))),
         {"u": {"pt": {"x": 7, "y": 2}}}, None, "away and back: the discarded state does not survive"),
        ("D7", seq(U, string(1, "ab"), string(0, "zz")), {"u": {"s": "ab"}}, None,
         "a §7.3-mistyped other option does not switch"),
        ("D8", seq(U, string(1, "ab"), unsigned(1, 4)), {"u": {"s": "ab"}}, None,
         "a §7.3-mistyped held option keeps its value"),
        ("D9", seq(U, unsigned(0, 9), seq(1, signed(0, 1))), {"u": {"num": 9}}, None,
         "a sequence at a string option id is skipped whole, no switch"),
        ("D10", seq(U, unsigned(0, 9), unsigned(99, 1)), {"u": {"num": 9}}, None,
         "an unknown id does not switch"),
        ("D11", seq(U), {}, b"", "an empty union frame is accepted and means the default"),
        ("D12", seq(U, unsigned(0, 9)) + seq(U), {"u": {"num": 9}}, None,
         "an empty re-opened frame keeps the held option"),
        ("D13", seq(U, string(1, "ab"), string(1, "c")), {"u": {"s": "c"}}, None,
         "a repeated string option is replaced"),
        ("D14", seq(U, uarray(3, [1, 2, 3]), uarray(3, [4])), {"u": {"arr": [4]}}, None,
         "a repeated array option is replaced (§7.4)"),
        ("D15", seq(V, seq(0, signed(0, 4)), seq(2, string(1, "q"))),
         {"v": [{"i": 4}, {"s": ""}, {"s": "q"}]},
         seq(V, seq(0, signed(0, 4)), seq(2, string(1, "q"))),
         "an element gap is default_id (a NON-first option) at its default"),
        ("D16", seq(V, seq(0, signed(0, 4)), seq(0, string(1, "z"))), {"v": [{"s": "z"}]}, None,
         "an element re-opened with another option switches"),
        ("D17", seq(V, seq(0), seq(1, signed(0, 3))), {"v": [{"s": ""}, {"i": 3}]}, None,
         "an empty element frame is the element default"),
        ("D18", seq(U, seq(6, unsigned(0, 1)), seq(6, signed(1, -5))), {"u": {"inner": {"b": -5}}},
         None, "a nested union switches too"),
        ("D19", seq(U, unsigned(0, 9), string(1, "abcdefgh")) + unsigned(W, 3),
         {"u": {"s": "abcdefgh"}, "w": 3}, None, "a string option split across feeds after a switch"),
        ("D20", seq(U, unsigned(0, 9), blob(1, b"\x01")), {"u": {"num": 9}}, None,
         "subtype gate: a blob at a string option does not switch"),
        ("D21", seq(U, string(1, "ab"), blob(1, b"\x01")), {"u": {"s": "ab"}}, None,
         "a blob at the HELD string option is not bound into it"),
        ("D22", seq(U, unsigned(0, 9), string(5, "q")), {"u": {"num": 9}}, None,
         "subtype gate: a string at a blob option does not switch"),
        ("D23", seq(U, unsigned(0, 9), sarray(3, [1])), {"u": {"num": 9}}, None,
         "array-kind gate: a signed array at a u16 array option"),
        ("D24", seq(U, unsigned(0, 9), farray(3, [1.0], FP32)), {"u": {"num": 9}}, None,
         "array-kind gate: an fp32 array at a u16 array option"),
        ("D25", seq(U, unsigned(0, 9), uarray(4, [1])), {"u": {"num": 9}}, None,
         "a compact array at a wrapper option does not switch"),
        ("D26", seq(U, unsigned(0, 9), fp64(10, 2.0)), {"u": {"num": 9}}, None,
         "fp subtype gate: fp64 at an fp32 option"),
        ("D27", seq(U, unsigned(0, 9), farray(11, [2.0], FP64)), {"u": {"num": 9}}, None,
         "fixlen-array subtype gate: an fp64 array at an fp32 array option"),
        ("D28", seq(U, seq(6, unsigned(0, 1))) + seq(U, unsigned(0, 5)) + seq(U, seq(6)),
         {"u": {"inner": {"b": -2}}}, None, "a re-selected union option restarts at ITS default"),
        ("D29", seq(U, unsigned(0, 9), uarray(3, [1, 2, 3])), {"u": {"arr": [1, 2, 3]}}, None,
         "a compact array after a switch, split: the switch must not wipe earlier chunks"),
        ("D30", seq(U, unsigned(0, 9), blob(5, b"\x01\x02\x03\x04")), {"u": {"bl": b"\x01\x02\x03\x04"}},
         None, "a blob after a switch, split per chunk"),
        ("D31", seq(U, unsigned(0, 9), seq(4, string(0, "ab"), string(1, "cd"))),
         {"u": {"strs": ["ab", "cd"]}}, None,
         "a wrapper option after a switch: a resumed sequence arm must not reset it"),
        ("D32", seq(PO, seq(1)), {"po": {"t": {"k": 2}}}, None,
         "an empty option frame selects t at its default on the omitted-default site"),
        ("D33", seq(PE, seq(0), seq(1, seq(1))), {"pe": [{"n": 6}, {"t": {"k": 2}}]}, None,
         "an empty element frame is THAT site's D (n), not the field site's (t)"),
        ("D34", seq(U, seq(4, string(0, "ab"), string(1, "cd")), seq(4, string(0, "x"))),
         {"u": {"strs": ["x"]}}, seq(U, seq(4, string(0, "x"))),
         "a repeated wrapper option in one frame is REPLACED (§7.4), not merged"),
        ("D35", seq(U, seq(4, string(0, "ab"), string(1, "cd"))) + seq(U, seq(4, string(0, "x"))),
         {"u": {"strs": ["x"]}}, None, "the same across re-opened frames"),
        ("D36", seq(R, seq(0)), {"r": {"nu": {"b": 4}}}, None,
         "an empty union-D frame is nu at ITS default (b = 4), not its first option"),
        ("D37", seq(G, seq(0, seq(1, unsigned(0, 1)))), {"g": [[{"hi": 4}, {"lo": 1}]]}, None,
         "an inner-row gap is the element type's D (hi = 4), two array levels down"),
    ]


# --- self-test: the builders against hand-written hex ------------------------

SELF_TEST = {
    "E4": "06 00 05 07",
    "E7": "06 26 07 07",
    "E12": "0e 06 01 08 07 16 01 00 07 1e 0a 0a 7a 07 07",
    "E19": "06 5d 00 20 07",
    "E25": "26 09 80 80 80 80 20 07",
    "E33": "36 06 0e 07 07 07",
    "E36": "46 06 00 00 07 07",
    "E44": "56 06 0e 00 01 07 07 07",
    "D6": "06 16 01 02 07 07 06 00 05 07 06 16 09 04 07 07",
    "D24": "06 00 09 1d 01 20 00 00 80 3f 07",
    "D34": "06 26 02 12 61 62 0a 12 63 64 07 26 02 0a 78 07 07",
}


def self_test() -> int:
    wires = {n: w for n, _, w, _ in encode_cases(False)}
    wires.update({n: w for n, w, _, _, _ in decode_cases()})
    bad = 0
    for name, want in SELF_TEST.items():
        got = wires[name].hex(" ")
        if got != want:
            print(f"FAIL self-test {name}: builders give {got}, hand-written {want}")
            bad += 1
    safe = {n: w.hex(" ") for n, _, w, _ in encode_cases(True)}
    for name, want in (("E26", "26 09 fe ff ff ff ff ff ff 1f 07"),
                       ("E27", "26 09 fd ff ff ff ff ff ff 1f 07"),
                       ("E28", "26 00 ff ff ff ff ff ff ff 0f 07")):
        if safe[name] != want:
            print(f"FAIL self-test {name} --int64-safe: builders give {safe[name]}, hand-written {want}")
            bad += 1
    # Every expectation must fill against the schema mirror (catches a typo'd
    # option or member name in a case before any harness sees it).
    for _, obj, _, _ in encode_cases(False):
        fill(SHAPE, obj)
    for _, _, want, _, _ in decode_cases():
        fill(SHAPE, want)
    n = len(SELF_TEST) + 3
    if bad:
        print(f"check_union self-test: {bad} of {n} FAILED")
        return 1
    print(f"check_union self-test: {n} builder images match their hand-written hex")
    return 0


# --- comparing a decoded value across eleven JSON dialects -------------------

def as_container(got):
    """A byte container some harnesses spell as base64 (Go, Java: a u8 array or a
    blob), back to its element values."""
    if not isinstance(got, str):
        return got
    try:
        return list(base64.b64decode(got, validate=True))
    except Exception:
        return got


def as_int(v):
    if isinstance(v, bool):
        return None
    if isinstance(v, int):
        return v
    if isinstance(v, float) and v.is_integer():
        return int(v)
    if isinstance(v, str):
        try:
            return int(v)
        except ValueError:
            return None
    return None


def float_bits(x, kind):
    try:
        x = float(x)
    except (TypeError, ValueError):
        return None
    if kind == FP32:
        if math.isfinite(x) and abs(x) > 3.4028234663852886e38:
            return None
        return struct.unpack("<I", struct.pack("<f", x))[0]
    return struct.unpack("<Q", struct.pack("<d", x))[0]


def compare(desc, want, got, path, out):
    """Append one line per difference to `out`; union levels are strict."""
    def diff(why):
        out.append(f"{path or '<message>'}: {why}: got {json.dumps(got)}, want {show(want)}")

    if isinstance(desc, Leaf):
        k = desc.kind
        if k in ("int", "u64", "i64"):
            if as_int(got) != want:
                diff("integer differs")
        elif k == "bool":
            if got is not want:
                diff("boolean differs")
        elif k == "string":
            if got != want:
                diff("string differs")
        elif k == "blob":
            g = [] if got in (None, "") else as_container(got)
            if not isinstance(g, list) or g != list(want):
                diff("blob differs")
        else:
            if float_bits(got, k) != float_bits(want, k):
                diff(f"{k} differs (by bit pattern)")
        return
    if isinstance(desc, Struct):
        if not isinstance(got, dict):
            return diff("not an object")
        for k, d in desc.fields.items():
            at = f"{path}.{k}" if path else k
            if k not in got:
                out.append(f"{at}: missing from the decoded object")
                continue
            compare(d, want[k], got[k], at, out)
        return
    if isinstance(desc, Union):
        (opt, w), = want.items()
        if not isinstance(got, dict) or list(got) != [opt]:
            return diff(f"a union holds exactly ONE option, {opt!r} (§4.2, §7.4.1)")
        return compare(desc.options[opt], w, got[opt], f"{path}.{opt}", out)
    g = [] if got is None else as_container(got)
    if not isinstance(g, list):
        return diff("not a list")
    if len(g) != len(want):
        return diff(f"length {len(g)}, want {len(want)}")
    for i, (w, x) in enumerate(zip(want, g)):
        compare(desc.elem, w, x, f"{path}[{i}]", out)


def show(v):
    return json.dumps(v, default=lambda b: list(b))


def jsonable(obj, dialect):
    """The encode input: 64-bit values in the dialect `--int64-json` names."""
    out = json.loads(show(obj))
    q = out.get("q")
    if dialect == "string" and isinstance(q, dict):
        out["q"] = {k: str(v) for k, v in q.items()}
    return json.dumps(out).encode()


# --- running a harness --------------------------------------------------------

def diagnostic(stderr: bytes) -> str:
    lines = [l.strip() for l in stderr.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:]) if lines else "<no output>"


def run(cmd, argv, data, cwd):
    """One harness invocation; returns (stdout bytes or None, diagnostic)."""
    p = subprocess.run(cmd + argv, input=data, cwd=cwd,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode != 0:
        return None, f"exited {p.returncode}: {diagnostic(p.stderr)}"
    return p.stdout, ""


def run_json(cmd, argv, data, cwd):
    out, err = run(cmd, argv, data, cwd)
    if out is None:
        return None, err
    try:
        return json.loads(out.decode()), ""
    except ValueError:
        return None, f"printed no JSON: {out.decode(errors='replace')[:200]!r}"


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        sys.stdout.write(SCHEMA)
        return 0
    if "--self-test" in argv:
        return self_test()
    if "--" not in argv:
        print(__doc__.strip().splitlines()[3], file=sys.stderr)
        return 2

    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    if not head or not cmd:
        print("FAIL: need a label and a harness argv after `--`", file=sys.stderr)
        return 2
    label = head[0]
    cwd = opt(head, "--cwd")
    msg = opt(head, "--message", MSG)
    stream = "--no-stream" not in head
    sizes = [int(s) for s in opt(head, "--sizes", "1,2,3,5,0").split(",")]
    dialect = opt(head, "--int64-json", "number")
    if dialect not in ("number", "string"):
        print(f"FAIL: --int64-json must be number or string, got {dialect!r}", file=sys.stderr)
        return 2
    safe = "--int64-safe" in head
    gaps = {}
    for i, a in enumerate(head):
        if a == "--known-gap":
            case, _, reason = head[i + 1].partition("=")
            gaps[case] = reason or "(no reason given)"

    if self_test() != 0:
        return 1

    results = {}          # case -> list of failure lines

    def decoded_ok(name, wire, want):
        """decode + streamdecode of `wire` against the filled `want`."""
        fails = results.setdefault(name, [])
        got, err = run_json(cmd, ["decode", msg], wire, cwd)
        if got is None:
            fails.append(f"decode of {wire.hex(' ') or '<empty>'}: these bytes are well formed, "
                         f"never a refusal (§7.4.1) -- harness {err}")
            return None
        diffs = []
        compare(SHAPE, want, got, "", diffs)
        fails.extend(f"decode: {d}" for d in diffs)
        if stream:
            for size in sizes:
                sgot, err = run_json(cmd, ["streamdecode", msg, str(size)], wire, cwd)
                if sgot is None:
                    fails.append(f"streamdecode split {size}: harness {err}")
                    continue
                diffs = []
                compare(SHAPE, want, sgot, "", diffs)
                fails.extend(f"streamdecode split {size}: {d}" for d in diffs)
        return got

    for name, obj, wire, why in encode_cases(safe):
        fails = results.setdefault(name, [])
        out, err = run(cmd, ["encode", msg], jsonable(obj, dialect), cwd)
        if out is None:
            fails.append(f"encode of {show(obj)}: harness {err}")
        elif out != wire:
            fails.append(f"encode of {show(obj)}: {why}\n      got  {out.hex(' ') or '<empty>'}\n"
                         f"      want {wire.hex(' ') or '<empty>'}")
        decoded_ok(name, wire, fill(SHAPE, obj))

    for name, wire, members, re_wire, why in decode_cases():
        got = decoded_ok(name, wire, fill(SHAPE, members))
        if re_wire is not None and got is not None:
            out, err = run(cmd, ["encode", msg], json.dumps(got).encode(), cwd)
            if out is None:
                results[name].append(f"re-encode of the decoded JSON: harness {err}")
            elif out != re_wire:
                results[name].append(f"re-encode of the decoded JSON is not canonical\n"
                                     f"      got  {out.hex(' ') or '<empty>'}\n"
                                     f"      want {re_wire.hex(' ') or '<empty>'}")
        if results[name]:
            results[name].insert(0, f"({why})")

    total = len(encode_cases(safe)) + len(decode_cases())
    if len(results) != total:
        print(f"FAIL {label}: ran {len(results)} of {total} union cases")
        return 1
    failed = [n for n, f in results.items() if f and n not in gaps]
    for n in failed:
        print(f"FAIL case {n}:")
        for line in results[n][:MAX_LINES]:
            print(f"    {line}")
        if len(results[n]) > MAX_LINES:
            print(f"    ... {len(results[n]) - MAX_LINES} more")
    for n, reason in gaps.items():
        if n not in results:
            print(f"FAIL: --known-gap names no case {n!r}")
            return 1
        if results[n]:
            print(f"KNOWN GAP {n} ({reason}): still fails")
            for line in results[n][:MAX_LINES]:
                print(f"    {line}")
        else:
            print(f"KNOWN GAP NOW PASSES -- drop the flag: {n} ({reason})")
    if failed:
        print(f"FAIL {label} §4.2/§7.4.1 tagged unions: {len(failed)} of {total} cases failed")
        return 1
    chunks = (f"one-shot and streamed at splits {','.join(str(s) for s in sizes)}"
              if stream else "one-shot ONLY (--no-stream: no streaming verb)")
    extra = []
    if safe:
        extra.append("--int64-safe: E26/E27/E28 use +-(2^53-1)")
    if gaps:
        extra.append(f"known gaps: {', '.join(sorted(gaps))}")
    print(f"{label} §4.2/§7.4.1 tagged unions: {total} cases "
          f"({len(encode_cases(safe))} encode, {len(decode_cases())} decode) -- one option held, "
          f"non-default options forced, last option wins, §7.3-skipped/unknown ids never switch; "
          f"int64 input as {dialect}; {chunks}"
          + ("; " + "; ".join(extra) if extra else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
