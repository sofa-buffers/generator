# TypeScript target — `targets.typescript`

Target-specific options, accepted under `targets.typescript`. Everything set
in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `int64` | `bigint` \| `long` \| `number` | `bigint` | Representation of 64-bit integer fields in the generated TS API (see below). All modes are wire-identical. |

### `max_dyn_*` — receiver-side decode limits

The three `max_dyn_*` keys (settable under `generic` or `targets.typescript`)
cap what a *received* message may claim before anything is allocated. They
govern **only** fields the schema left unbounded (`array` without `count`,
`string`/`blob` without `maxlen`); an unset key means unlimited (the previous
behavior). When at least one key is active, the module exports `MAX_DYN_ARRAY_COUNT`
/ `MAX_DYN_STRING_LEN` / `MAX_DYN_BLOB_LEN` constants and every generated
`static decode(bytes)` passes them to its `Cursor` as a corelib `DecodeLimits`
object. Exceeding a cap throws `SofabError` with code
`SofabErrorCode.LimitExceeded` at the count/length header — never a clamp or
truncation. Each cap is raised to the largest schema bound of its kind, so a
schema-bounded field larger than the cap stays governed by its own bound alone
(its over-schema counts are still rejected by the generator#100 guard, and a
`string`/`blob`/`struct`/`union` element array by the generator#142 over-index
guard that throws `SofabError(InvalidMsg)` when a wire element id is `≥ count`).
A key whose kind has no unbounded field in the schema is inert and emits nothing;
with no keys set the output is byte-identical to previous releases. The
plumbing is independent of the `int64` mode.

### `int64` — 64-bit field representation

`bigint` (default) · `long` · `number` — à la protobufjs's `int64` option.
All three modes are **wire-identical**; they only change the generated TS API
and its runtime cost. `bigint` in the 64-bit hot path is the dominant cost of
the TS codec, especially on JavaScriptCore (Bun), which optimizes `bigint`
~2.5–4× worse than V8.

| Mode | u64/i64 arrays | u64/i64 scalars |
|---|---|---|
| `bigint` | `bigint[]` | `bigint` |
| `long` | `Long[]` behind a get/set accessor pair | `bigint` |
| `number` | `Long[]` behind a get/set accessor pair | `number` |

**`long`** backs each 64-bit array with a private `Long[]` field (corelib's
`Long` is a `(low, high)` 32-bit word pair) plus an accessor pair:

```ts
private _u64: Long[] = [];
get u64(): Long[] { return this._u64; }
set u64(vals: readonly (Long | bigint | number)[]) { this._u64 = vals.map(Long.fromValue); }
```

Assignment stays ergonomic (`msg.u64 = [1n, 2n]` or plain numbers) and converts
**once**, off the per-encode path; marshal/decode read and write the backing
field directly via the corelib's `write*ArrayLong`/`read*ArrayLong`, so no
`bigint` is created on the hot path. Caveats:

- The setter maps `Long.fromValue` over its input — even an all-`Long` input is
  re-wrapped in a fresh array. Assign whole arrays, or push `Long`s in place
  (`msg.u64.push(Long.fromNumber(7))`): in-place mutation operates on the
  `Long[]` itself.
- `toJSON()` still prints decimal strings (`Long.toString(signed)`), and
  `fromJSON()` still parses via `BigInt` (off the hot path, through the setter).

**`number`** additionally maps 64-bit *scalars* to plain `number`, using the
corelib writers' existing number fast path. Only choose it when every 64-bit
scalar value is guaranteed to fit the ±2^53 safe-integer range — values beyond
that silently lose precision. (Full-range scalars as `Long` need scalar `Long`
codecs in corelib-ts first; until then they stay `bigint` under `long`.)

Measured on the full-scale arena message (best-of-3, corelib-ts #19/#20):

| Mode | Bun/JSC MB/s | vs protobufjs | Node/V8 MB/s | vs protobufjs |
|---|--:|--:|--:|--:|
| `bigint` | 25.5 | 0.66 | 39.2 | 0.90 |
| `long` | 38.0 | 0.95 | 47.3 | 1.17 |
| `number` | 40.2 | 1.04 | 50.8 | 1.18 |

## Encode: sequence framing

`marshal(os)` opens **every** nested sequence with the corelib's
`os.writeSequenceBeginLazy(id)`, which holds the header back until a child field
is actually written. The closer alone then decides whether a contentless sequence
survives:

| Position | Closer | Effect |
|---|---|---|
| `struct`/`union` **field** | `os.writeSequenceEnd()` | an all-default nested object is **omitted**, not framed empty |
| wrapper-array **field** | `os.writeSequenceEnd()` | an **empty** array is omitted; absence reconstructs the field's construction default (the empty collection, `count: N` or not) |
| wrapper-array **element** (`struct`/`union`, nested row) | **positional** — `os.writeSequenceEndKeep()` at the array's LAST index, `os.writeSequenceEnd()` in the interior | the last element's presence carries the array's length (*highest present id + 1*); an interior all-default element becomes an id gap (see below) |

The first two rows are decided at generation time from the position in the schema.
The third cannot be: it depends on the position in the **value**, so it is the one
run-time predicate the generated marshal carries.

This is MESSAGE_SPEC §2 / CORELIB_PLAN §6. The visible consequence: a message
whose every field equals its default now encodes to **zero bytes**, and a nested
all-default object costs nothing instead of a two-byte empty frame. Decoding is
unchanged — a decoder still accepts the empty frame and treats it as the omitted
field.

## On-demand corelib imports

The generated `message.ts` imports from `@sofa-buffers/corelib` only the names its
own body can name — `WireType`, `FixlenSubtype`, `Long`, `SofabError`/
`SofabErrorCode` are each gated on the schema (`schemaHasFixlenGuard`,
`scanHelpers` in `helpers.go`), so no module carries an unused import. Every gate
is a *mirror of an emitter* and has to stay in lockstep with **where** that
emitter fires.

The trap this walked into once (generator#246): the §7.3 wire guard is emitted at
**two** levels — `tsWireGuardCond` for the field and `tsElemWireGuardCond` for
every wrapper-sequence **element** down the array element chain. A gate that
inspects field kinds plus one level of *native* array element misses
`array<string>`, `array<blob>` and nested rows such as `array<array<fp32>>`,
which name `FixlenSubtype` from an element guard while no field is fixlen; the
emitted module then throws `ReferenceError` in `decodeInto` and fails `tsc`. The
element-chain walk lives in `fieldHasFixlenGuard` (visitor.go); the maxlen /
over-index gates already descend the same way (`arrayHasBoundedStrBlob`,
`arrayOverIndexed`).

## Arrays: `count` is a capacity, the wire carries the length

MESSAGE_SPEC `af536c4` settles `count` on the schema's side: it is a **capacity**,
never a length. A field carries `0 .. N` elements; the wire count `M` **is** a
compact array's length, and a wrapper array's length is *highest present id + 1*.
Nothing that carries a length may be elided, so the trim-on-encode /
fill-on-decode pair this backend used to ship (`_trimTail`/`_trimTailLong`,
`_padTo`, `_trimStrs`/`_trimBlobs`/`_trimObjs`/`_trimRows`) is **gone** — with it
the padding of a short `default` out to `N` and the materialization of a fresh
`count: N` array to `N` elements. The corelib still exports those helpers; the
generator simply stops calling them.

What that leaves is one sparse rule, the same for both element kinds and the same
with or without a declared `count`:

> An element **before the last one** that equals its element default is omitted,
> leaving an id **gap** — a `string`/`blob` leaf simply not written, a
> `struct`/`union`/nested-array element **not framed** either. The **last** element
> is **always** written: a leaf as its value, a sequence element as an **empty
> frame**.

A `count: N` array is therefore written and read exactly like its count-less
sibling. `count` still *bounds* the array — an element id `≥ N` (generator#142)
and a wire count `M > N` (generator#100) are both `InvalidMsg` — but it never adds
an element the wire did not carry, and `[1,2,3,0,0]` and `[1,2,3]` are two
different values with two different encodings.

### The closer is positional, at run time

The consequence for the framing table above: the wrapper-array **element** row is
no longer decided from the schema. `emitSeqEnd` (backend.go) takes a `keepIf`
condition and emits the two-armed closer when it is non-empty:

```ts
this.objs.forEach((_e0, _i0, _a0) => {
  os.writeSequenceBeginLazy(_i0);
  _e0.marshal(os);
  if (_i0 === _a0.length - 1) {   // lastElemExpr
    os.writeSequenceEndKeep();    // last: the empty frame survives
  } else {
    os.writeSequenceEnd();        // interior: an all-default element is a gap
  }
});
os.writeSequenceEnd();            // the FIELD wrapper still drops unconditionally
```

The nested `marshal` writes no child exactly when the element equals its declared
default, so the closer alone decides. A leaf element expresses the same rule as an
unconditional `|| _i0 === _a0.length - 1` disjunct next to its omit test
(`lastElemExpr`), and a **native** nested row — which has no frame of its own —
gets it on the write itself:

```ts
this.rows.forEach((_e0, _i0, _a0) => {
  if (_e0.length !== 0 || _i0 === _a0.length - 1) {
    os.writeUnsignedArray(_i0, _e0);
  }
});
```

A sequence-typed **field** (a `struct`/`union` field, an array wrapper) still takes
the dropping closer unconditionally: an empty array is omitted and absence
reconstructs it (§2).

Byte targets, all regenerated shared vectors (`serialized_sparse`) and all verified
against corelib-ts through a built project:

| value | wire |
|---|---|
| `["a",""]` | `06020a610a0207` |
| `["",""]` | `060a0207` |
| `["","x",""]` | `060a0a78120207` |
| `["a","","c"]` | `06020a61120a6307` |
| `[{k:1},{k:0},{k:3}]` | `06060001071600030707` (interior frame gone) |
| `[{k:0},{k:0}]` | `060e0707` (last frame kept) |
| `[1,2,0,0]` (`count: 4` u32) | `030401020000` |

Every generated class still carries `isDefault(): boolean` — the explicit form of
the "not one child was written" test the lazy framing applies implicitly, generated
from the very same per-field expressions `marshal` uses so the two cannot drift
apart. For an array field the predicate is now simply `.length === 0`: the writer
emits a child for **every** element the value holds, so "no child is written" is
exactly "the array is empty".

### Decode: placed by id, never filled to `N`

A wrapper element is **placed at `arr[id]`** after gap-filling with default
elements — never appended, because the element id *is* the array index (§5.1). A
reopened element id then merges into the element already there (§7.4,
`T.decodeInto(c, arr[_id]!)`) instead of appending a second one (generator#247).

That placement now covers **nested rows** too. `seqCollectBody`'s row arm used to
`arr.push(...)` id-blind, which was unreachable while every row was written; an
interior gap makes it reachable, and an appending collector shifts every later row
down by one index. Rows are placed at `arr[_id]`, gap-filled with the empty row,
bounded by the outer array's `count` — which is also what closes the over-index
hole that arm had.

A row whose elements are themselves a wrapper sequence — `array<array<string>>`,
`array<array<blob>>`, `array<array<struct|union>>`, and the same one level
deeper — is read by an inline IIFE collector emitted at the point of use, and
that collector is **typed with the row's own type**. `tsArrayType` already answers
with the container type for the level it is handed, so appending another `[]`
declared `const _r: string[][]` for a row that the next statements fill with leaf
strings; the emitted module failed `tsc` with TS2345 *"Argument of type 'string'
is not assignable to parameter of type 'string[]'"* and TS2322 follow-ons on the
same line. The recursion is what keeps this to one rule rather than three special
cases: depth 3 wraps the depth-2 collector, so a string row lands on the string
arm and a blob row on the blob arm. A **native** row (`array<array<u32>>`) never
goes through a collector at all — it reads in one corelib call
(`c.readUnsignedArray(N)`) — and is the control that told the two apart.

Nothing is filled in afterwards. The `M` elements that arrived **are** the value,
so a decoded array's length is the wire's, `count: N` or not, and a `count: N`
field constructs **empty**:

```ts
strs: string[] = [];      // count: 3, string   -- a capacity adds no elements
nums: number[] = [];      // count: 3, u32
objs: VecObjsElem[] = []; // count: 2, struct
dyn:  string[] = [];      // dynamic: identical
short: number[] = [1, 2]; // count: 5, default [1,2] -- NOT padded out to 5
```

The field's declared default is what the schema wrote and nothing more, on both
sides of the omit test — which is what keeps an all-zero length-`N` value (a
length-`N` array) distinct from the empty one and on the wire.

## fp32 signaling NaN (issue #235)

A JS `number` is a 64-bit double, so widening an `fp32` through it **quiets** a
signaling NaN (`0x7F800001` → `0x7FC00001`) and a decoded value could never be
re-encoded bit-for-bit, violating the MESSAGE_SPEC §4.6 float round-trip.
TypeScript was the last of the 13 drivers with this gap; every other fp32 value
is unaffected, because any non-NaN fp32 narrows back to its own bits exactly.

`corelib-ts` supplies the bit-preserving channel on both paths, so the fix is
purely in what the generated code consumes — **no corelib change**:

| direction | scalar | native array |
|---|---|---|
| read | `Cursor.readFp32Raw()` | `Cursor.readFp32ArrayRaw(schemaCount?)` |
| write | `OStream.writeFixlen(id, bytes, FixlenSubtype.Fp32)` | `OStream.writeFp32ArrayRaw(id, payload)` |

There is deliberately no scalar `writeFp32Raw`: `writeFixlen` with subtype fp32
emits the identical `fixlenHead(id, 4, Fp32)` + 4 raw bytes, and corelib-ts's own
doc comment on `readFp32Raw` prescribes that route.

**Generated shape.** Every fp32 position — a scalar field and a native `fp32[]`
field, in messages and named types alike — grows a companion slot beside the
value:

```ts
f32: number = 0;
f32Fp32Raw: Uint8Array | null = null;   // wire bytes, captured only for a NaN
```

- **Decode** reads the raw bytes, derives the convenience number from those same
  bytes (`_fp32FromRaw`, an allocation-free shared scratch word), and stores a
  **copy** of the bytes when the value is a NaN — the raw readers return a view
  aliasing the decoder's buffer, valid only until it is reused (`readBlob`'s
  contract), and a decoded object outlives one feed. The store is unconditional,
  so a re-opened field id (§7.4) drops what an earlier occurrence captured.
- **Encode** consults the capture only for a value that is *still* a NaN: the
  scalar writes `writeFixlen(…, FixlenSubtype.Fp32)`, the array re-renders its
  payload through `_fp32ArrayRaw`, which takes captured bits per element and
  renders every other element from its number. So assigning `msg.f32 = 2.5` (or
  `msg.arr[0] = 2.5`) after a decode always wins over a stale capture.
- **The omission test does not move.** §2 decides presence from the *value*:
  emit iff it differs from the default. Reading "carries raw bytes" as "was
  present" makes an explicit `+0.0` re-encode instead of normalizing away — a
  divergence from the other 12 drivers, pinned by
  `TestTSFp32RawDoesNotMoveTheOmissionTest`.
- The companion is wire state, not value state: it stays out of `toJSON()` /
  `fromJSON()`, so the JSON surface and the harness's `encode`/`decode` output are
  byte-identical to before. The new `recode` harness mode (wire → object → wire,
  no JSON) is what exercises this — JSON renders every NaN as `null` and cannot
  tell a signaling one from a quiet one. `tests/conformance/typescript/run.sh`
  round-trips a signaling, a quiet, a negative and a negative-signaling NaN at the
  scalar position and at an `fp32[]` element position.

fp32 only: a JS number **is** an fp64, so an fp64 NaN payload already survives
and its paths are untouched. **Known limit:** an fp32 row nested inside a wrapper
array (`array<array<fp32>>`) still widens through `readFp32Array` — a row has no
field of its own to hang the companion on. Same class of gap as the C++ nested
wrapper rows; not reachable from the two positions this issue measures.

## Benchmark row

Row `ts-bigint` and `ts-long` (one per `int64` mode) in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
