# TypeScript codegen: fast encode (allocation-free strings + tidy marshal)

Status: implemented + measured in the arena. Two parts:

1. **corelib-ts** (the decisive win): an allocation-free UTF-8 fast path for
   `OStream.writeString` — **corelib-ts PR #17**, branched off `main`
   (independent of the decode PR #16). No generated-code change is needed for it;
   generated `marshal` already calls `os.writeString(id, str)`.
2. **generated-code** (this doc's patch): small `marshal` tidy-ups captured as
   `languages/typescript/sofab/fast-encode.patch`, applied by
   `languages/typescript/setup.sh` **after** the decode patch
   (`monomorphic-decode.patch`). Fold into the generator
   (`github.com/sofa-buffers/generator`, `generators/typescript/*.go`).

Companion to `docs/perf/bottlenecks.md` (item 9, "the real TS lever") and
`docs/perf/decode-design.md` (same philosophy: contiguous, monomorphic, no
per-field allocation — applied to the write path).

## Problem

Split timing of the arena's canonical 436-byte message showed the round-trip is
**encode-bound**: encode ~7250 ns/op vs protobufjs's ~5014 ns/op entire
round-trip. Attributing the ~7250 ns with isolated micro-benchmarks:

| marshal work (canonical message)        | ns/op |
|-----------------------------------------|------:|
| **6× `writeString`** (1 nested + 5 list)| **4407** |
| bigint arrays `u64[5]`+`i64[5]`         | 1335 |
| 6× small-int arrays                     |  599 |
| bigint scalars `u64`+`i64`              |  358 |
| 6× small-int scalars                    |  193 |
| 12× scalar fp                           |  178 |
| fp arrays `fp32[5]`+`fp64[5]`           |   77 |

Strings were **~60% of encode**. Drilling in: of the 4407 ns, ~3950 ns was
`TextEncoder.encode()` itself — its per-call WHATWG setup, a throwaway
`Uint8Array` allocated per string, and a second copy of those bytes into the
output buffer by `writeRaw`. `TextEncoder.encode ×6 = 3942 ns` vs a manual
length-scan + write into the buffer `= 432 ns` (9×).

Two smaller generated-`marshal` warts, unrelated to the corelib but cheap to fix:

- the blob default-guard `if (!arrEq(this.bytes_field, new Uint8Array()))`
  **allocates a throwaway empty `Uint8Array` every encode** just to compare, and
  drags in the `arrEq` helper;
- the string list is emitted as `this.string_array.forEach((_e0,_i0) => …)`,
  allocating a **closure per marshal** and blocking straightforward inlining.

## Fix 1 — corelib `writeString` (the real win; PR #17, no codegen change)

On the in-memory (growable) path, `writeString` now scans the UTF-8 byte length
(`utf8Length` — needed up front for the fixlen length word), writes the header,
then encodes the characters **straight into the output buffer** (`utf8Write`) —
no intermediate array, no second copy. `utf8Length`/`utf8Write` reproduce
`TextEncoder` byte-for-byte, including collapsing unpaired surrogates to U+FFFD
(`EF BF BD`). The streaming (fixed-buffer) path is unchanged — it still
materialises via `TextEncoder` so a payload can outgrow the caller's buffer and
drain in chunks.

Generated code is untouched by this; it already emits `os.writeString(id, s)`.

## Fix 2 — generated `marshal` tidy-ups (`fast-encode.patch`)

### Target emitted output

Blob default-guard — when the field default is the **empty** blob (the common
case), test emptiness directly instead of allocating a comparison operand:

```ts
// before
if (!arrEq(this.bytes_field, new Uint8Array())) { os.writeBlob(3, this.bytes_field); }
// after
if (this.bytes_field.length !== 0) { os.writeBlob(3, this.bytes_field); }
```

String (and any leaf) list — a plain indexed `for` instead of `forEach`:

```ts
// before
this.string_array.forEach((_e0, _i0) => os.writeString(_i0, _e0));
// after
for (let _i0 = 0; _i0 < this.string_array.length; _i0++) {
  os.writeString(_i0, this.string_array[_i0]!);
}
```

With the blob guard rewritten, the `arrEq` helper is no longer referenced for the
canonical schema and is dropped from the emitted module.

### Generalization rules for the codegen backend (marshal side)

- **Empty-default leaf guard.** For a blob or native scalar-array field whose
  schema default is empty, emit the sparse-canonical guard as
  `if (this.<f>.length !== 0) { … }`. Only emit the element-wise `arrEq(this.<f>,
  <default-literal>)` comparison (and the `arrEq` helper) when a field has a
  genuinely **non-empty** default that requires value comparison — which the
  canonical schema has none of. This removes a per-encode `new Uint8Array()` (and
  any similar default-literal) allocation on the common path.
- **Leaf sequence loop.** For a `sequence` of a leaf type (string/blob/scalar),
  emit an indexed `for (let _iN = 0; _iN < this.<f>.length; _iN++) os.write…(_iN,
  this.<f>[_iN]!)` rather than `.forEach(closure)`. No per-marshal closure
  allocation; the monomorphic body inlines. (Message-typed sequences already emit
  a `for … { child.marshal(os) }` shape and are unaffected.)

These are marshal-shape changes only; the field ids, wire types, ordering and the
sparse-canonical "omit-if-default" predicates are semantically identical.

## Why this preserves the wire / correctness gate

- `utf8Length`/`utf8Write` are asserted **byte-identical** to `TextEncoder` (incl.
  4-byte code points and unpaired-surrogate → U+FFFD) by a new corelib test that
  compares the growable fast path against the `TextEncoder` streaming path and
  round-trips every case through the decoder.
- `this.bytes_field.length !== 0` is exactly `!arrEq(bytes_field, <empty>)` for an
  empty default; the emitted/omitted field set is unchanged.
- `for` vs `forEach` emits the same `writeString(index, element)` calls in the same
  order.

Arena sha256 gate holds: `db362bf24959b41fd153b59958e2afdf59020c6c3501fb60e189526659a72ed4`,
wire **436 B**, unchanged through every measurement below.

## Validation

Host: Node 24, canonical arena message (`schema/state.json`).

- **Isolated encode-only** (`os.reset(); src.marshal(os)` loop):
  **7250 → ~4000 ns/op (-45%)**, now **below protobufjs encode (~5014 ns/op)**.
  The 6-string micro dropped 4407 → 556 ns/op.
- **Combined encode+decode** (what the arena reports, `BENCH_ITERS=400000`):
  **sofab 35.7 → 51.7 MB/s**; protobuf ~62.8 MB/s; speed-adv vs protobuf
  **0.57× → 0.82×**. (Both parts stack on the already-landed monomorphic decode.)
- corelib-ts: `npm test` **381 tests** green (incl. 2 new `writeString` UTF-8
  parity/round-trip tests), `tsc --noEmit` + `tsup` build green (374 tests on the
  main-based PR branch, which lacks the decode branch's `cursor.test.ts`).

## How the arena re-applies it

`languages/typescript/setup.sh` must apply this patch **after** the decode patch,
guarded like the decode one:

```sh
if ! grep -q "bytes_field.length !== 0" "$GEN/message.ts"; then
    patch -p1 -d "$GEN" < "$HERE/sofab/fast-encode.patch" >&2
fi
```

(The corelib string fix arrives automatically once corelib-ts PR #17 merges; until
then the arena builds `vendor/corelib-ts` from the working tree carrying it.)

## Further opportunity (not taken here)

- **Direct-DataView fp packing.** `packFp32`/`packFp64` write through a shared
  8-byte scratch `DataView` and copy the bytes out one at a time. A `DataView` held
  over the output buffer (`view.setFloat32(pos, v, true)`) is ~3.5× faster on the
  fp arrays (measured 38.7 → 10.9 ns for `fp32[5]+fp64[5]`), worth ~150 ns/message.
  Deferred: it adds a `DataView`-lifecycle concern (recreate on grow / honour a
  streaming buffer's `byteOffset`) to shared corelib code, and the string fix
  already clears the target with margin.
- **bigint 64-bit arrays (1335 ns).** The `u64[5]`/`i64[5]` elements genuinely
  exceed `2^53`, so they go through `bigint` zig-zag + varint. protobufjs avoids
  this with a `Long` (two int32) representation; matching that is an
  architecture-level change to the field model, out of scope for an
  encode-only, wire-identical pass.
