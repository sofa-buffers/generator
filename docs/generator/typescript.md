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
is actually written. Which closer follows is decided at generation time from the
position in the schema — never from the value, so there is no runtime predicate
in the generated code:

| Position | Closer | Effect |
|---|---|---|
| `struct`/`union` **field** | `os.writeSequenceEnd()` | an all-default nested object is **omitted**, not framed empty |
| wrapper-array **field** | `os.writeSequenceEnd()` | an empty array is omitted; absence reconstructs the (empty) default |
| wrapper-array **element** (`struct`/`union`, nested row) | `os.writeSequenceEndKeep()` | the frame always survives — element presence is what carries the array's length (*highest present id + 1*) |

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

## Wrapper arrays: `M` on encode, `N` on decode

A `count: N` wrapper array's canonical wire stops at **`M`** — one past its last
element that differs from the element default — "even for sequence-form elements"
(MESSAGE_SPEC §3/§5.1). `marshal` therefore narrows the container *before* the
element loop, through one of `_trimStrs` / `_trimBlobs` / `_trimObjs` /
`_trimRows`; only the **trailing** run goes, an interior all-default element keeps
its frame (element presence is what carries the length). `M === 0` writes no child
at all, so the lazily-opened wrapper is dropped by `writeSequenceEnd()` and the
whole field is omitted (§2). A **dynamic** (count-less) array has no `N` to refill
from, so its trailing default element is significant and is never narrowed.

Every generated class carries `isDefault(): boolean` for this: the explicit form
of the "no child was written" test the lazy framing already encodes implicitly for
a *field*, needed here because an *element* must be judged before the loop opens.
It is generated as the exact negation of `marshal`'s per-field write guards, and
reads the **same** narrowed expression the element loop walks — a predicate that
narrowed a field the writer did not (or the reverse) would either omit a field
that is on the wire or keep one that is not (generator#248).

The decode counterpart, and what makes the elision lossless: a wrapper element is
**placed at `arr[id]`** after gap-filling with default elements — never appended,
because the element id *is* the array index (§5.1). A reopened element id then
merges into the element already there (§7.4, `T.decodeInto(c, arr[_id]!)`) instead
of appending a second one (generator#247). When the sequence scope closes, a
`count: N` array is default-filled back out to `N`, the wrapper-array counterpart
of `_padTo` on native arrays: without it the trailing elision would not re-shape
the bytes, it would **shorten** the decoded array on every round trip. The
generator#142 over-index guard still rejects an element id `≥ N`, which also
bounds the gap-fill.

## Benchmark row

Row `ts-bigint` and `ts-long` (one per `int64` mode) in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
