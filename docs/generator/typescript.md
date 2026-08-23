# TypeScript target — `targets.typescript`

Target-specific options, accepted under `targets.typescript`. Everything set
in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `int64` | `bigint` \| `long` \| `number` | `bigint` | Representation of 64-bit integer fields in the generated TS API (see below). All modes are wire-identical. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. It reaches generated TypeScript only for a message the schema cannot bound, where it is emitted as `MAX_SIZE_LIMIT`; set explicitly it is also a budget a computed worst case may not exceed. Full semantics in the [generic config](README.md) and ARCHITECTURE §9.6. |

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

### Two decoders, and the contract they share

The backend emits **two** decode paths per type, and they are not interchangeable
implementations of one thing:

- `Msg.decode(bytes)` runs the monomorphic `Cursor` over a contiguous buffer.
  This is the fast path and the default.
- `new MsgDecoder()` → `feed(chunk)` / `finish()` drives a generated `Visitor`
  over the corelib's resumable `IStream`, for callers who receive the message in
  pieces. The cursor cannot be fed in chunks, which is why a second decoder
  exists at all.

Both must reach the **same verdict on the same bytes**, and that covers how a
message is rejected, not only what a decoded one contains. Every §7 verdict now
exists twice, so the risk is drift — and it is real: the cursor decodes strings
inside the corelib (`Cursor.readString`), while the visitor is handed raw wire
bytes and materialises them itself, so for a while one library reported invalid
UTF-8 as `SofabError(InvalidMsg)` through `decode()` and as a bare platform
`TypeError` through `feed()` (generator#297). Both transcodes are the corelib's
own `decodeUtf8` today (#345), which is the strongest form of that fix. A `TypeError` walks straight past a caller's
`catch (e) { if (e instanceof SofabError) … }`, which for a decoder fed
untrusted bytes is the difference between a rejected message and an unhandled
exception.

Two things hold the paths together, and a new verdict should use both:

- emit it from the **same helper** the cursor path uses, so a rule change lands
  in both;
- gate it in `tests/conformance/typescript/stream_check.ts`, which feeds the same
  bytes through both paths at six chunk sizes (one byte included) and requires
  identical values *and* identical `SofabError` codes on rejection.

### The class's decode surface is `decode` and nothing else (issue #384)

CORELIB_PLAN §6.1.1 closes the generated object's name set to `encode`, `decode`,
`try_decode`, `serialize`, `deserialize` and `decoder`, and names `decode_from`
and `decode_into` among the spellings a port must **not** invent beside them. The
only adaptation the clause allows is casing/idiom, so `decodeFrom` is not a new
name — it *is* `decode_from`, written the way TypeScript writes it. Generated
types land in the user's namespace; a developer should not have to learn a
per-language second entry point into the same operation.

The two cursor-level steps still exist, because §7.4 needs them (a re-opened
scope decodes *into* the object an earlier opening populated, so the loop must be
separable from the fresh-object entry). They are **module-level functions** in the
generated file:

```ts
export class Probe {
  static decode(bytes: Uint8Array): Probe {
    return _decodeFromProbe(new Cursor(bytes));
  }
}

function _decodeFromProbe(c: Cursor): Probe { return _decodeIntoProbe(c, new Probe()); }
function _decodeIntoProbe(c: Cursor, o: Probe): Probe { /* the switch(id) loop */ }
```

Not exported, so they are reachable from every sibling class in the module — the
classes decode into one another — and from nowhere outside it. This is the
TypeScript analogue of the Dart backend's library-private `_decodeInto`; §6.1.1's
closing paragraph puts anything genuinely *below* the generated layer in the
corelib, and these are above it, so out of the module is as far as they go.

One consequence is worth naming. The loop is no longer a member of the class it
writes into, so a `private` backing field — the `_name` slot behind a Long-backed
accessor pair (see `int64` below) — cannot be reached with a dot. It is reached
with `o["_name"]` instead: TypeScript's `private` is a compile-time rule and
element access is its sanctioned escape hatch, emitting the identical property
write. The hot path is therefore unchanged — still no getter call and no setter
conversion per decoded field — and the accessor pair stays the only *public* way
in (`decodeStorage` in `helpers.go`).

### `int64` — 64-bit field representation

`bigint` (default) · `long` · `number` — à la protobufjs's `int64` option.
All three modes are **wire-identical**; they only change the generated TS API
and its runtime cost. `bigint` in the 64-bit hot path is the dominant cost of
the TS codec, especially on JavaScriptCore (Bun), which optimizes `bigint`
~2.5–4× worse than V8.

| Mode | u64/i64 arrays | u64/i64 scalars |
|---|---|---|
| `bigint` | `bigint[]` | `bigint` |
| `long` | `Long[]` behind a get/set accessor pair | `Long` behind a get/set accessor pair |
| `number` | `Long[]` behind a get/set accessor pair | `number` |

**`long`** backs every 64-bit position — arrays and scalars alike — with a
private `Long` / `Long[]` field (corelib's `Long` is a `(low, high)` 32-bit word
pair) plus an accessor pair:

```ts
private _u64: Long[] = [];
get u64(): Long[] { return this._u64; }
set u64(vals: readonly (Long | bigint | number)[]) { this._u64 = vals.map(Long.fromValue); }

private _big: Long = Long.ZERO;
get big(): Long { return this._big; }
set big(v: Long | bigint | number) { this._big = Long.fromValue(v); }
```

Assignment stays ergonomic (`msg.u64 = [1n, 2n]`, `msg.big = 7`) and converts
**once**, off the per-encode path; serialize/decode read and write the backing
field directly via the corelib's `write*ArrayLong`/`read*ArrayLong` and
`write*Long`/`read*Long`, so no `bigint` is created on the hot path in either
direction. Caveats:

- The setter maps `Long.fromValue` over its input — even an all-`Long` input is
  re-wrapped in a fresh array. Assign whole arrays, or push `Long`s in place
  (`msg.u64.push(Long.fromNumber(7))`): in-place mutation operates on the
  `Long[]` itself.
- `toJSON()` still prints decimal strings (`Long.toString(signed)`), and
  `fromJSON()` still parses via `BigInt` (off the hot path, through the setter).
- A scalar's omission test is a `(low, high)` compare against halves computed at
  generation time (`this._big.low === 0 && this._big.high === 0`), because `===`
  on a `Long` would compare object identity. Nothing is allocated per call, and
  `serialize` and `isDefault()` share the one helper so they cannot disagree.
- A default is materialised at construction: `Long.ZERO` for the (overwhelmingly
  common) zero, `Long.fromValue(<lit>n)` otherwise. `Long.ZERO` is shared and
  immutable — a zero default costs no `bigint` arithmetic per constructed object.

#### The push decoder's Long channel

The two decode surfaces reach a 64-bit field by different routes. The pull cursor
takes the corelib's `read{Unsigned,Signed}Long` and is done. The push visitor's
hooks are **number-first** by default, so the generated arm converts —
`Long.fromValue(v)`, which for anything a `number` can hold goes through
`BigInt(...)`, i.e. a bigint allocated per value on the one path these modes
exist to keep bigint-free.

corelib-ts's opt-in `Visitor.longs` channel removes that: with the flag, the four
integer hooks deliver a `Long` on both `decode()` and `IStream`. This backend
takes it — `readonly longs: true = true` on every generated visitor — but **only
where the schema says it pays**, because the flag is read once from the root and
covers every integer field and element in the message:

- a 64-bit position saves its whole conversion;
- a **narrow** position pays for it, since the corelib now allocates a `Long`
  where a plain `number` used to arrive. It narrows back through two emitted
  helpers, `_u` / `_i`, which return exactly what `Number(v)` returned on the
  number-first channel — so no declared-width verdict depends on the channel.

Measured on chunked decode of one message (Ir/op, subtract method under
Callgrind, baseline-tier node as in `tests/bench`; the two projects differ in
nothing but the flag):

| 64-bit positions | narrow positions | Long channel |
|--:|--:|--:|
| 4 | 0 | **−29.6%** |
| 4 | 4 | **−20.3%** |
| 4 | 8 | **−15.8%** |
| 4 | 14 (`vehicle_telemetry`) | +1.5% |

Hence the rule: take the channel when a schema carries at most `longsThreshold`
(= 2) narrow integer positions per 64-bit one. It is deliberately conservative —
an array position counts once here whatever its runtime length, because a static
count cannot know that length and a declared `count` is a capacity, not a
promise.

Three things the channel would break if it were applied naively, all pinned by
`tests/conformance/typescript/run.sh`:

- **`Boolean(v)` is wrong.** A `Long` is an object, so `Boolean(Long)` is `true`
  for the 64-bit zero. The conversion reads the halves.
- **`v.low` alone is wrong for a narrow destination.** A `u8` fed 2^32 would take
  the low half and store **zero** — an INVALID message (§7.1) silently accepted,
  not merely a wrong number. `_u`/`_i` fall back to the full value whenever the
  high half is not the low one's sign extension; the conformance leg feeds
  exactly that wire (and its signed twin, −2^32 into an `i8`) through both
  surfaces at six chunk sizes.
- **A native matrix row's converter is called from the fp hooks too**, which stay
  `number` on both channels, so it narrows with a `typeof` rather than a cast.

The `int64` axis alone decides this: `bigint` and `number` scalars would have to
convert either way, so both stay on the number-first channel.

Scalars became `Long` in [#339](https://github.com/sofa-buffers/generator/issues/339),
once corelib-ts grew the scalar codecs
([corelib-ts#143](https://github.com/sofa-buffers/corelib-ts/issues/143)); before
that they stayed `bigint` under `long` for want of them. That gap was the whole
distance between `long` and an exact representational match for the arena's
opponent: protobufjs 8.7.1 with `long@5.3.2` returns a long.js `Long` for **every**
64-bit field, scalars and small values included
(`protobufjs/src/reader.js`: `var fn = util.Long ? "toLong" : "toNumber";`).

**`number`** instead maps 64-bit *scalars* to plain `number`, using the corelib
writers' existing number fast path. Only choose it when every 64-bit scalar value
is guaranteed to fit the ±2^53 safe-integer range — values beyond that silently
lose precision. It is not a cheaper `long`: a plain `number` is cheaper than the
arena baseline's `Long`, so it does not match the opponent either.

Measured on the full-scale arena message (best-of-3, corelib-ts #19/#20 — i.e.
with `long`'s scalars still on `bigint`, before #339):

| Mode | Bun/JSC MB/s | vs protobufjs | Node/V8 MB/s | vs protobufjs |
|---|--:|--:|--:|--:|
| `bigint` | 25.5 | 0.66 | 39.2 | 0.90 |
| `long` | 38.0 | 0.95 | 47.3 | 1.17 |
| `number` | 40.2 | 1.04 | 50.8 | 1.18 |

## The caller owns the encode buffer

CORELIB_PLAN §5.1: a corelib never allocates or grows an output buffer — the
caller does, and generated code **is** that caller. So every message class
carries an `encode(): Uint8Array` that allocates the storage and hands it to the
corelib's `OStream`, together with the number it is sized from.

corelib-ts's no-argument `new OStream()` is exactly the shape this replaces. It
is deprecated there as an alias for `growingOStream()`, which allocates a slab
and doubles it as the message grows — the corelib owning the storage — and
`os.bytes()` then hands back a *view* into that slab. Nothing this backend emits
constructs it: `TestTSCallerOwnsTheEncodeBuffer` generates a whole project and
fails if the string reappears in *any* emitted file, harness and bench recipe
included — the conformance run cannot catch that one, because a growing stream
still produces the same bytes and would sail through every byte-exact leg.

**Bounded** — every field carries a `count`/`maxlen`, so the schema has a worst
case and one exactly-sized buffer holds any conformant value:

```ts
static readonly MAX_SIZE = 49;          // derived: no value can encode to more

encode(): Uint8Array {
  const _buf = new Uint8Array(Reading.MAX_SIZE);
  const _os = new OStream(_buf);        // no sink, no owner: nothing can grow
  this.serialize(_os);
  return _buf.slice(0, _os.bytesUsed);  // a copy: the result must own its bytes
}
```

No flush sink means `MIN_OUTPUT_BUFFER` does not apply (corelib-ts imposes the
floor only when one is installed), so a field-less message legitimately encodes
through a 0-byte buffer, and `reserveBulk` still takes the bulk string/array fast
paths because room at the cursor counts on any buffer.

**Unbounded** — one field has no bound, so there is no worst case. `MAX_SIZE` is
then the configured *ceiling* (emitted as `MAX_SIZE_LIMIT`, with `MAX_SIZE`
aliasing it) and must **not** size a buffer: a larger message is legitimate and
would be silently refused. A fixed 512-byte scratch drains into caller-owned
storage instead, so what an encode holds resident is the scratch, not the
message:

```ts
const _out: Uint8Array[] = [];
const _os = new OStream(new Uint8Array(512), 0, (_c) => { _out.push(_c.slice()); … });
this.serialize(_os);
_os.flush();
// … concatenate _out (a single drain is returned as-is)
```

The sink **copies** and returns without calling `setBuffer`, which is §5.1's
copy-and-continue case: the encoder keeps the scratch and resumes at offset 0,
with no take-and-replace handover.

Four consequences worth knowing before you rely on them:

- **A value filled past its own schema bound is refused, not truncated.** It no
  longer fits the exactly-sized buffer, and `SofabError(BUFFER_FULL)` propagates
  out of `encode()` with nothing returned. Such a message used to be encoded and
  handed back — bytes every conformant receiver rejects as INVALID anyway
  (MESSAGE_SPEC §7.1) — and §5.1 forbids returning partial output as if it were
  complete. This is the *only* encode-side bound the TypeScript backend has: it
  emits no `maxlen`/`count` validation of its own.
- **`encode()` reports through the exception channel.** It returns
  `Uint8Array` with no error return, which is deliberate and not a swallowed
  error: `decode()` already throws `SofabError`, so this adds no new *kind* of
  failure path, and both the constructor's argument check and the writer's
  buffer-full throw propagate out untouched. The TypeScript profile is
  `maxspeed`, which permits exceptions.
- **The bounded arm allocates the schema's worst case, not the value's.**
  `array<u64, count: 10000>` means a 90 KB `Uint8Array` per `encode()` call even
  for a ten-element value. Worth weighing before declaring an aspirational bound.
  There are two escape hatches, both already public: `serialize(os)` with an
  `OStream` you construct yourself over a buffer of your choosing, and the same
  with a flush sink so the message streams out instead of being assembled. There
  is deliberately no cached or module-level scratch — a nested type's
  `serialize` can re-enter `encode()`, and a shared buffer would corrupt the
  outer message.
- **`MAX_SIZE` is a static class member**, so a schema field literally named
  `MAX_SIZE` (or `MAX_SIZE_LIMIT`, or `encode`) collides with it. Java, C# and
  Python carry the same exposure.

## A decoded message owns its bytes

`decode()` returns a message that outlives the buffer it was decoded from: the
input may be reused or mutated the moment it returns.

That needed fixing on the cursor path. `Cursor.readBlob` returns a **zero-copy
`Uint8Array` view** into the source buffer — correct of the corelib, which
allocated nothing — and the generated destination kept it, so overwriting the
input buffer changed a decoded message in place. Every blob destination now
copies (`.slice()`), at the scalar and the wrapper-element position, bounded or
not; the `maxlen` guard still runs on the view, before the copy, so nothing
over-bound is duplicated first. Strings were never affected (`readString`
transcodes into a JS string) and the fp32 raw companions already copied.

The two decoders disagreed on this until now — the streaming visitor has always
copied — and the differential test could not see it, because it compares
*values*, which are equal either way until the buffer is reused. It now scribbles
over the input buffer after decoding and re-encodes, so the rule is pinned by
behaviour. A borrowing mode is deliberately not offered and not configurable
(ARCHITECTURE §9.6).

## Encode: sequence framing

`serialize(os)` opens **every** nested sequence with the corelib's
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
run-time predicate the generated serialize carries.

This is MESSAGE_SPEC §2 / CORELIB_PLAN §6. The visible consequence: a message
whose every field equals its default now encodes to **zero bytes**, and a nested
all-default object costs nothing instead of a two-byte empty frame. Decoding is
unchanged — a decoder still accepts the empty frame and treats it as the omitted
field.

## The generated layer's support lives in the corelib (issue #345)

Five pieces of this backend's output had the *same shape for every schema*, with
their schema dependence carried entirely by arguments — which is precisely what
ARCHITECTURE §8 puts in the corelib. corelib-ts#151 added them to its public API,
and the backend now calls them instead of re-emitting a copy per package:

| was emitted | now called |
|---|---|
| `class _Acc` — join a payload split across fed chunks | `PayloadAcc` |
| `class _StrSeq` — collect a `string` wrapper array | `StringSeq(out, acc, cap, elemMax, name)` |
| `class _BlobSeq` — collect a `blob` wrapper array | `BlobSeq(out, acc, cap, elemMax, name)` |
| `const _dec` + `function _str` — strict transcode of a payload | `decodeUtf8(bytes)` |
| `function arrEq` — the array half of the ≠-default test | `elementsEqual(a, b)` |

The bounds those collectors enforce are unchanged and still *schema* bounds: the
`count` capacity and the element `maxlen` travel as constructor arguments, exactly
as `Cursor` already takes them on the pull path, and the rejection messages are
byte-identical. `decodeUtf8` is not merely a move — it carries a measured ASCII
fast path (858 vs 2006 Ir/op on a 13-byte field) the generated `TextDecoder` did
not have, which is why the corelib exports it rather than keeping it private.

Two things deliberately stay emitted here. `longArrEq` compares `Long` elements by
`(low, high)` word pair, which is this backend's representation choice, not the
corelib's; and `_ObjSeq` / `_MatSeq` / `_RowSeq` are typed by the *generated*
element type and construct it, so they are not schema-free. The encode sink and
`growingOStream` are off-limits for a different reason: they allocate, and
CORELIB_PLAN §5.1 gives the buffer to the caller.

One typing consequence, and it is the whole of the change under `int64: long`. A
corelib collector is a plain `Visitor` — it has no integer hook, so there is
nothing for the `longs` flag to type — while the generated visitors on the Long
channel are `LongVisitor`. `sequenceBegin` therefore declares corelib-ts's own
`AnyVisitor` on that channel (`childVis` in `streamdecode.go`), which both shapes
satisfy. Nothing about the decode changes: the flag is read **once, from the root
visitor**, so a child's own flag is never consulted.

## On-demand corelib imports

The generated `message.ts` imports from `@sofa-buffers/corelib` only the names its
own body can name — `WireType`, `FixlenSubtype`, `Long`, `SofabError`/
`SofabErrorCode`, and the generated-layer support above, are each gated on the
schema (`schemaHasFixlenGuard`, `schemaHasStringField`, `scanStreamUse`,
`scanHelpers` in `helpers.go`), so no module carries an unused import. Every gate
is a *mirror of an emitter* and has to stay in lockstep with **where** that
emitter fires.

The trap this walked into once (generator#246): the §7.3 wire guard is emitted at
**two** levels — `tsWireGuardCond` for the field and `tsElemWireGuardCond` for
every wrapper-sequence **element** down the array element chain. A gate that
inspects field kinds plus one level of *native* array element misses
`array<string>`, `array<blob>` and nested rows such as `array<array<fp32>>`,
which name `FixlenSubtype` from an element guard while no field is fixlen; the
emitted module then throws `ReferenceError` in the pull decoder and fails `tsc`. The
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
  _e0.serialize(os);
  if (_i0 === _a0.length - 1) {   // lastElemExpr
    os.writeSequenceEndKeep();    // last: the empty frame survives
  } else {
    os.writeSequenceEnd();        // interior: an all-default element is a gap
  }
});
os.writeSequenceEnd();            // the FIELD wrapper still drops unconditionally
```

The nested `serialize` writes no child exactly when the element equals its declared
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
from the very same per-field expressions `serialize` uses so the two cannot drift
apart. For an array field the predicate is now simply `.length === 0`: the writer
emits a child for **every** element the value holds, so "no child is written" is
exactly "the array is empty".

### Decode: placed by id, never filled to `N`

A wrapper element is **placed at `arr[id]`** after gap-filling with default
elements — never appended, because the element id *is* the array index (§5.1). A
reopened element id then merges into the element already there (§7.4,
`_decodeInto<T>(c, arr[_id]!)`) instead of appending a second one (generator#247).

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

The emitted harness also carries a **`stream_<msg>`** workload — the chunked
decode surface, which `decode_*` (the whole-buffer pull cursor) does not touch at
all, so a change confined to the push path is invisible without it. No
`results.txt` row uses it yet; it is what the Long-channel numbers above were
measured with.

Long scalars (#339) moved the `ts-long` row's **encode** by −0.67% (603 385 →
599 322 Ir/op, `--rows ts-bigint,ts-long` against corelib-ts's scalar-`Long`
branch): the two 64-bit scalars of `vehicle_telemetry` no longer split a `bigint`
through the encoder's scratch. Decode stayed inside the file's 0.3% hold gate on
this schema — the saving there is two `BigInt` materialisations against a 678 k
Ir/op message dominated by arrays and strings, and it is the arena's much smaller
`FullScaleExample` where those two scalars are worth ≈1.8% of a round trip (#335,
#339). `results.txt` itself is unchanged in this commit: a `--rows` run drops the
other corelibs' SHAs from the header, and the ts SHA to record does not exist on
`corelib-ts` `main` until its scalar-codec PR lands. Refresh it from a **full**
run once it has.

The measured encode body is now `obj.encode()`, so it counts the buffer
allocation and the copy of the finished message — the work every caller pays.
The former body folded `os.bytes().length`, a *view* into a corelib-grown slab,
and paid for neither, so the encode figures currently in `results.txt` are not
comparable across that change; the next full run resets them.

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a value outside its
declared range with `SofabErrorCode.InvalidMsg` — the same channel as the maxlen
and count rejects. A decoded integer lives in a JS `number`, so nothing masked it
here; the defect was that an out-of-range value was **kept**.

```ts
case 0: { const _v = c.readUnsigned();
          if (_v > 255) throw new SofabError(SofabErrorCode.InvalidMsg,
            "a_u8: value outside declared width u8");
          o.a_u8 = _v as number; break; }
case 3: o.d_u64 = c.readUnsigned() as bigint; break;   // u64: nothing to bound
```

The read lands in a temporary so the check can precede the store. `u64`/`i64`
keep their bare read in both int64 modes (bigint and Long): their range is the
reader's own.

There is no `Number()` around a *guarded* read. The reader is number-first: it
returns a `bigint` only past 2^53-1, which is far outside every width guarded
here, so the guard rejects any `bigint` before the store — and a value that
passes it is therefore already a `number` (JS compares a `bigint` against a
`number` by value, so the guard itself is exact either way). `_v as number` is a
type assertion and emits nothing. An **unguarded** destination keeps the
conversion, since nothing there proves the narrowing.

### The bound goes into the reader, not into a scan after it (issue #267)

A native array first read into `_a` and then scanned once was the right verdict at
the wrong time. **A scan over the assembled array cannot fire for an array that
never assembles**: truncate the message right after an out-of-range element and
`readSignedArray` raises INCOMPLETE first, so the verdict is lost — while §5.2
makes INVALID dominate INCOMPLETE precisely because the violation is *already
established* by the bytes seen.

The bound therefore travels with the read, alongside the schema count that is
already passed there for the same reason — `readUnsignedArray(count, max)` and
`readSignedArray(count, min, max)` (corelib-ts#90):

```ts
c.readUnsignedArray(5, 255) as number[]          // u8[5]
c.readSignedArray(undefined, -128, 127) as number[]   // dynamic i8[]
```

A **dynamic** array keeps `undefined` in the count slot and still carries the
width bound: width is a property of the element *type*, not of the array
*length*.

The post-read scan that used to follow the read is **gone** (issue #339). It was
kept as defense in depth for a consumer on an older corelib whose reader ignored
the new arguments — but "one pass over an array already in hand" is not free on a
decode hot path, and the same reasoning that moved the bound into the reader
condemns the copy left behind it: a scan over the assembled array is strictly
later and strictly weaker than the verdict it duplicates, and it cannot reach the
truncation case the reader-side bound exists for. The same applies to the
whole-array `_a.length > N` count re-check, the whole-string `_utf8Len(_s) > N`
maxlen re-scan (whose helper is now gone entirely), and the fp32 array's
`_n > N`. Measured on the arena's `FullScaleExample`, dropping all four took
decode from 48 201 to 43 578 Ir/op — 10% of the whole decode spent re-deciding
settled questions. `tests/conformance/typescript/run.sh` exercises every one of
those rejects through the reader (over-count, over-count + truncation,
over-width element + truncation, over-maxlen, over-maxlen + truncation), so a
corelib that stopped honouring an argument fails there rather than silently.

A bounded **blob** keeps its `_b.length > N` check: reading `.length` on a view
the reader already produced is O(1), so there is nothing to reclaim.

Passing the bound was necessary but not sufficient, and the gap is worth
recording because it is invisible from this side. `Cursor.arrayCount` rejected a
count larger than the bytes remaining as INCOMPLETE — an allocation guard
(corelib-ts#38) that decided the outcome from the count word *before* the element
loop, so the bound the generator had just handed over was never reached. It is
now a cap on the **allocation** rather than a rejection (corelib-ts#99). Nothing
changed in generated code; the fix was entirely below it. Over 10 442 truncations
× 6 chunk sizes it took the driver's chunk-invariance mismatches from **100 to
8**, which is the whole `whole=INCOMPLETE / chunked=INVALID` direction of #300 —
so on that class the *contiguous* path was the wrong one, not the chunked one.

The scalar `string`/`blob` `maxlen` had the same shape on the **streaming**
visitor, where the check lived in the payload callback and a message ending right
after an over-`maxlen` length word degraded to INCOMPLETE. Arrays already had
`arrayBegin` at the count word for exactly this; strings and blobs had no
counterpart until corelib-ts#89 added `Visitor.fixlenBegin(id, subtype, total)`:

```ts
fixlenBegin(id: number, sub: FixlenSubtype, total: number): void {
  switch (id) {
    case 2: if (sub === FixlenSubtype.String && total > 32) throw …; break;
    case 3: if (sub === FixlenSubtype.Blob   && total > 4)  throw …; break;
```

**Testing the announced subtype is not optional.** One callback serves both
kinds, so ignoring `sub` would measure a blob field's `maxlen` against a string
arriving at that id — a §7.3 mismatch to *skip*, not to bound. The wrapper-element
collectors get the same treatment: their over-index *and* `maxlen` checks sat in
the payload callback too (generator#303, closing #300).

## The decode hot path (issue #339)

The arena (`sofa-buffers/arena`, `typescript` row) measured this target at
**0.76× protobufjs on MB/s** while every other maxspeed target beat its baseline.
Attributing that with Callgrind — Ir/op under full TurboFan, subtracted between
two rep counts, one payload group emptied at a time — put the whole gap on
**decode**:

| Ir/op, `FullScaleExample` | sofab | protobufjs |
| - | - | - |
| encode | 22 644 | 27 630 |
| decode | 48 201 | 29 235 |

Encode was already 18% ahead. So the question was never "is TypeScript slow", it
was "what does decode do that encode does not", and the answer turned out to be
two things the encoder had already fixed for itself years earlier — an
un-inlinable varint routine, and a per-string `TextEncoder`/`TextDecoder` round
trip. Both fixes live in corelib-ts (`perf/decode-hot-path`); the generator's own
share is above:

- the four whole-value re-scans, §7.1 above — **−4 623 Ir/op**;
- the `Number()` around a guarded narrow read, §7.1 above;
- the fp32 array's per-read `new DataView(_p.buffer, …)`, replaced by the shared
  4-byte scratch `_fp32FromRaw` already emitted for the scalar position. The
  encode half (`_fp32ArrayRaw`) likewise builds its bytes through
  `_fp32RawFrom` instead of constructing two DataViews per call;
- the `DecodeLimits` literal, hoisted to one frozen module-level `_LIMITS`.
  `max_dyn_array_count` / `max_dyn_string_len` / `max_dyn_blob_len` resolve to
  compile-time constants, so a fresh `{ maxArrayCount: … }` at every
  `decode(bytes)` call site was an allocation with a constant value on the decode
  path — paid by every schema that configures a cap at all, and by the streaming
  `IStream` too. A schema with no cap configured is unaffected: `cursorLimits()`
  still renders nothing.

Together with the corelib change the round trip goes **73 121 → 56 398 Ir/op**
against protobufjs's 60 590 — from 0.84× to 1.07× on messages/second, and from
0.76× to 0.94× on MB/s (the MB/s column is measured on each side's own wire, so
SofaBuffers' 12%-smaller message counts against it there).

### A `bigint` field must hold a `bigint` (issue #340)

The cursor readers are **number-first**: `readUnsigned` returns a `number` for
anything up to 2^53-1 and a `bigint` only past that. The pull path used to bridge
that with a bare cast — `c.readUnsigned() as bigint`,
`c.readUnsignedArray(n) as bigint[]` — which converts nothing, so the declared
type was false:

```ts
const m = Example.decode(wire);
typeof m.u64          // "number"  — declared bigint
m.u64 + 1n            // TypeError: Cannot mix BigInt and other types
m.arrays.u64          // [0, 4611686018427387904n, …] — number AND bigint, same array
```

The array case is the worse of the two: the element type depends on the element's
*value*, so one array holds both, which is a lie a consumer trips over
(`arr.map((v) => v * 2n)`) **and** a mixed-type array that defeats the engine's
element-kind specialisation — the cast was not even buying speed.

What settles the intended semantics is that the *other two* paths into the same
field were already right: the streaming visitor stores `BigInt(v)` and `fromJSON`
maps `BigInt(...)`. So the same field had a different runtime type depending on
which decode API the caller used, and the pull path was the odd one out. It now
converts like the others:

```ts
o.u64 = BigInt(c.readUnsigned());
o.u64 = c.readUnsignedArray(5).map((_e) => BigInt(_e));
```

`int64: long` / `number` are unaffected on the array side (they hand back uniform
`Long` objects). Their scalars took the same conversion at the time; `long` has
since moved its scalars onto `Long` as well (#339), which removes the conversion
rather than paying it, and `number` converts with `Number()`. On the push path
the same field may now come off the Long channel instead (#344, above) — a
different route to the identical runtime type, which is the invariant that
matters.

**This costs, and the cost is the type.** A real `bigint[]` needs one `bigint`
allocation per element — that is what the cast was avoiding by not producing one.
Round trip on `FullScaleExample`: `bigint` 56 627 → 59 747 Ir/op (+5.5%), `long`
56 400 → 57 391 (+1.8%, its two 64-bit scalars only), `number` unchanged. All
three still clear protobufjs's 60 590, and a caller who wants the last of it has
`int64: long`, which is now both correct and faster.

Nothing above the runtime type changes: the wire is byte-identical and every
existing conformance check passed before the fix, because they all compare wire
bytes or `toJSON()` output and `String(1n) === String(1)`. That is exactly why
`tests/conformance/typescript/run.sh` now asserts `typeof` directly, on **both**
decode surfaces and against **both** the full-range and the safe-integer payload
— the full-range one catches the array elements, the safe-integer one catches the
scalar (a scalar past 2^53 came back `bigint` even from the broken build). The
`long` mode has the same assertion in `instanceof Long` form (#339), for the same
reason and against the same two payloads.

### All three `int64` modes, and both runtimes

The `int64` axis (`bigint` | `long` | `number`, §Options) changes the 64-bit hot
path, so the work above was re-measured across all three rather than only the
arena's `long`. It holds uniformly, and every mode now clears protobufjs:

| round trip, Ir/op | before | after | after + #340 fix |
| - | - | - | - |
| `bigint` (default) | 72 974 | 56 627 | **59 747** |
| `long` (arena) | 73 124 | 56 400 | **57 391** |
| `number` | 73 164 | 56 360 | **56 360** |
| *protobufjs* | | | *60 590* |

On **Node/V8** the three modes land within 0.5% of each other — the axis is
nearly free here, contrary to what the option's description implies. That
description is about **JavaScriptCore**, and there it holds: on Bun, best-of-5
over four interleaved rounds, `long` runs the round trip in 0.606 s against
`bigint`'s 0.854 s. Bun is also where this change pays most, because an
un-inlinable varint routine and a per-string payload view cost JSC far more than
V8:

| Bun / JavaScriptCore | msg/s | MB/s |
| - | - | - |
| protobufjs | 100 494 | 49.6 |
| sofab before (`long`) | 79 376 | 34.4 |
| sofab after (`long`) | **164 966** | **71.6** |

**Method note, because it decided two of these calls.** Measure at the tier the
consumer runs. `tests/bench` pins V8 to the baseline JIT (`--max-opt=1`) to make
Ir affine in reps, and at that tier the same ranking does not hold: the ASCII
string fast path measures *slower* there and faster under TurboFan, and the
protobufjs decode gap reads 27% instead of 65%. And measure the round trip, not
decode alone: a decoder that appends with `+=` builds a rope, which looks 40%
cheaper until the encoder's UTF-8 pass flattens it — decode-only said that design
won by 1 700 Ir/op, the round trip said it lost by 4 500.
