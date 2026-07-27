# Rust target — `targets.rust`

Target-specific options, accepted under `targets.rust`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets (see below). |
| `no_std` | bool | `true` when `corelib: rs-no-std` | Emit a genuinely `#![no_std]`, heap-free crate (see below). Set `false` to emit an ordinary `std` crate against the no-std corelib. Ignored for `corelib: rs`. |
| `allow_dynamic` | bool | `false` | `corelib: rs-no-std` only. Store bounded fields in `alloc::String`/`alloc::Vec` instead of heapless containers, for a target with an allocator. Bounds stay mandatory either way. |

### `max_dyn_*` — receiver-side decode limits

The `max_dyn_array_count` / `max_dyn_string_len` / `max_dyn_blob_len` keys
(generic or `targets.rust`) bake receiver-side decode limits (generator#102)
into the generated module as `MAX_DYN_*` constants. They govern **only**
schema-unbounded fields — an array without `count`, a string/blob without
`maxlen`; a schema-bounded field stays governed by its own bound (plus the
generator#100 over-count guard). The generated visitor checks the wire count /
declared total **at the header, before any elements or bytes accumulate**;
exceeding a cap makes `try_decode` return `sofab::Error::LimitExceeded` — never
a clamp. The best-effort `decode()` is unchanged. Precedence when several
verdicts apply: `InvalidMsg` (over-schema count), then `LimitExceeded`, then
`BufferFull`. The `InvalidMsg` verdict also covers the **wrapper-array**
analogue (generator#142): a `string`/`blob`/`struct`/`union` element array with
a schema `count: N` sets the same `inv` flag when a wire element id is `≥ N`,
before the container grows. For a `string`/`blob` element under
`corelib: rs-no-std` the guard fires ahead of the capacity-bounded
`heapless::Vec` drop (issue #126); for a `struct`/`union` element the reject is
emitted on **both** profiles, because the element is *placed* at
`container[id]` (see below) and the guard is what keeps that index inside the
container. The `inv` flag
likewise carries the **over-`maxlen`** reject (Option B, MESSAGE_SPEC §7.1): a
`string`/`blob` (scalar or wrapper element) whose wire byte length exceeds its
schema `maxlen` sets `inv` at the length header, on **both** profiles — on
`no_std` the guard fires ahead of the heapless `BufferFull` path, so an
over-`maxlen` value is `InvalidMsg`, not a capacity error.

**std profile only.** The limits apply to `corelib: rs` (std). Under
`corelib: rs-no-std` the keys are inert: heapless storage is statically
schema-bounded already (an unbounded field is either rejected at generation
time), and that
corelib has no `Error::LimitExceeded`.

### `corelib`

Both corelibs expose the same `sofab::` interface and produce **identical wire
bytes**; they differ in `std` usage and feature gating.

- **`rs`** (default) — [`corelib-rs`]: `std`, tuned for throughput. Every wire
  type is always compiled in, so there are no Cargo features and no `require!`
  guard. The generated `Cargo.toml` depends on it as
  `sofab = { package = "SofaBuffers", … }`.
- **`rs-no-std`** — [`corelib-rs-no-std`]: `#![no_std]`, heap-free, tuned for
  small footprint. Wire types are gated behind Cargo features. The generated
  `Cargo.toml` sets `default-features = false` and re-enables the **full**
  wire-type set (`fixlen`, `array`, `sequence`, `fp64`, `value64`), and a
  `sofab::require!(…)` guard in the generated module asserts the same set. The set
  is **not** derived from the wire types the schema declares: `corelib-rs-no-std`
  gates wire-type *parse/skip* (not just field storage) behind these features, and
  MESSAGE_SPEC §7.3 requires a decoder to skip any wire type an unknown id may
  carry — an array, an fp64, a 64-bit value — regardless of whether the schema
  itself has such a field. A schema-derived subset would leave the decoder unable
  to skip those, so it would **reject** a well-formed skippable field with
  `InvalidMsg` (generator#215 / Crucible F-0027). The footprint saving from
  dropping a wire type is therefore not available to a §7.3-conformant decoder;
  making `corelib-rs-no-std`'s skip path itself feature-independent is the
  alternative that would restore it.

```yaml
targets:
  rust:
    corelib: rs-no-std    # default: rs
```

Set the corelib path in the generated `Cargo.toml` (the `${SOFAB_RS_CORELIB}`
placeholder) before building.

### `no_std` — the heap-free profile

With `corelib: rs-no-std`, `no_std` is on by default and the generated crate is
genuinely `#![no_std]` and **heap-free** — the analog of the C++ `corelib: c-cpp`
fixed-capacity profile. Wire output is unchanged; this is purely an in-memory
representation change. What it produces vs the `std` path (all sized from the
schema's `maxlen`/`count`):

| Field kind | `std` | `no_std` |
|---|---|---|
| string (`maxlen N`) | `String` | `heapless::String<N>` |
| blob (`maxlen N`) | `Vec<u8>` | `heapless::Vec<u8, N>` |
| string/blob/struct/nested array (`count N`) | `Vec<T>` | `heapless::Vec<T, N>` |
| native numeric/enum/bool array (`count N`) | `[T; N]` | `[T; N]` (already fixed) |

The generated code also: emits `#![no_std]` on the crate root (`src/lib.rs`);
encodes into a fixed `heapless::Vec<u8, MAX_SIZE>` (no `vec!`); decodes with a
bounded location stack (no heap scratch); and gates `serde` behind a cargo
`serde` feature (pulled by the default `std` feature so the JSON harness builds) —
so the firmware build carries no serde and no allocator. The `heapless` crate
(sized from the schema) provides the containers; the corelib itself stays purely
storage-agnostic.

The crate is a **lib + bin**: the `src/lib.rs` lib is the firmware artifact and
the `src/main.rs` bin is a `std` JSON test harness gated on the `std` feature (a
binary cannot be `#![no_std]` on a hosted target). Build the genuinely heap-free
crate with `cargo build --lib --no-default-features`.

**Unbounded fields.** A string/blob without `maxlen`, or an array without
`count`, cannot be sized, so on the `no_std` path such a field fails generation
with an error naming the field. That holds in **both** storage modes —
`allow_dynamic` picks the container, never whether a bound is needed — so one
schema stays valid for every `no_std` target. For genuinely unbounded fields, use
`corelib: rs`.

### Storage mode (`allow_dynamic`)

With every field bounded, the switch chooses where those fields live:

| schema | default (heapless) | `allow_dynamic: true` |
|---|---|---|
| `string, maxlen 8` | `heapless::String<8>` | `alloc::string::String` |
| `blob, maxlen 8` | `heapless::Vec<u8, 8>` | `alloc::vec::Vec<u8>` |
| `array u32, count 4` | `[u32; 4]` | `alloc::vec::Vec<u32>` |
| `array string, count 2, maxlen 4` | `heapless::Vec<heapless::String<4>, 2>` | `alloc::vec::Vec<alloc::string::String>` |

Heapless is the default and the one that guarantees no allocation at all: the
worst case is the struct's size, known at compile time. The alloc mode suits a
target that has an allocator — a field then holds what the message actually
carries rather than its declared worst case, which matters once a bound is large
enough that the inline struct no longer fits comfortably on a stack. It pulls
`extern crate alloc`.

The bounds do not weaken. What was the container's capacity becomes an explicit
check on the decode path: a declared length above a `maxlen`, or a wire count
above a `count`, sets the sticky `inv` flag and the decode reports
`Error::InvalidMsg` — before any bytes accumulate, so an over-long field never
allocates what the bound exists to prevent. Encode output is identical in both
modes.

```yaml
targets:
  rust:
    corelib: rs-no-std       # no_std is then on by default
    allow_dynamic: true      # optional: alloc storage (needs an allocator)
```

## Wrapper arrays: element placement, the N-fill, and the trailing run

A **wrapper array** is an array whose elements are `string`, `blob`,
`struct`/`union`, or another array: instead of a native array wire type it
lowers to a sequence whose **child id is the element's array index**
(MESSAGE_SPEC §5.1). Three rules follow from that, and the flat visitor has to
implement all three (generator#247, generator#248).

**Placement, not append.** Decode gap-fills the container with element defaults
up to the wire id and then decodes **into** `container[id]`. Appending would
shorten the array by the size of any interior id gap (`elem0, elem2` is a
*three*-element array whose middle element is the default), and would decode a
**reopened** element id as a second element instead of merging into the first —
where placement gives §7.4's struct-merge for free. The `string`/`blob` element
path always did this; `struct`/`union` elements now do too.

The corelib's `Visitor` is flat — sequence begin/end events, no child visitor
object — so there is nowhere for a child collector to hold the index. Each
`struct`/`union` wrapper-array frame therefore owns one `usize` slot in the
visitor state (`_ix0`, `_ix1`, …), set from the element id at
`sequence_begin`, and the descended element location addresses its object as
`<path>[self._ixN]`. One slot per frame, not a stack: a decode location can only
be active once at a time, so no depth arithmetic is needed. The slots are part
of the *persistent* state, because an element can straddle a chunk boundary in
the incremental decoder. The over-index reject (`id >= N` → `InvalidMsg`) runs
**before** the gap-fill, which is what bounds it — and, on `no_std`, what keeps
the index inside the `heapless::Vec` capacity.

**The N-fill.** When a `count: N` wrapper array's sequence scope closes, decode
default-fills it back out to `N`: §5.1 says the length "is N for every target —
a growable-list target MUST default-fill to N exactly like a pre-sized one".
This is visible in the decoded value (a `count: 5` string array carrying three
elements reads back as five, the last two empty). A **dynamic** (count-less)
array is never filled: its length is highest-present-id + 1.

**The trailing run.** Encode narrows a `count: N` wrapper array to `M` — one
past its last element differing from the element default — before the element
loop, because that is what its canonical wire carries (§3/§5.1, "even for
sequence-form elements"). Interior all-default elements keep their frame, since
element presence is what carries the length; only the trailing run goes. `M == 0`
writes no child at all, so the lazily-opened wrapper is dropped by its closer and
the whole field is omitted (§2). The narrowing is only *lossless* because of the
N-fill above — without it, re-encoding a decoded array would shorten it on every
round trip rather than normalise it.

Both the writer loop and the all-default predicate are generated from **one**
expression (`elemTrimExpr`, threading `HasCount` through unchanged), and a
`struct`/`union` element type gets an `is_default()` built by negating the very
guards its `serialize` writes. A predicate that narrowed a field the writer does
not — or the reverse — would omit a field that is on the wire, or keep one that
is not. `is_default()` and the `_trim_seq` helper are emitted only for the
schemas that use them, so a footprint build carries neither.

**Nested-array rows are excluded** from the narrowing and the fill. A row's
writer emits an array header unconditionally, so a row is never "not written" the
way a default string element or an all-default struct element is: dropping a
trailing empty row would remove a child that *is* on the wire, and there is no
row refill to put it back. Under `no_std` a `count: M` inner array is also a
`[T; M]`, which has no empty state to test for.

## Struct field order

Generated struct fields stay in **schema order** — unlike the C, C++ and Go
targets, no widest-first reordering is applied, because the Rust compiler
already reorders the fields of a default-`repr` struct itself to minimize
padding.

[`corelib-rs-no-std`]: https://github.com/sofa-buffers/corelib-rs-no-std
[`corelib-rs`]: https://github.com/sofa-buffers/corelib-rs

## Benchmark row

Row `rust-rs` (corelib `rs`) and `rust-rs-no-std` (corelib `rs-no-std`) in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **toggle** method. Tracked: Ir/op for both; `rust-rs-no-std` also `.text`/`.data`/`.bss` on thumbv6m.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Strict UTF-8 (issue #85)

`String` is a Unicode type, so it is **always strict** (MESSAGE_SPEC §8 /
CORELIB_PLAN §6.4) — there is no config key in generated code. The string visitor
materializes with `core::str::from_utf8` and, on `Err`, sets the sticky `inv` flag
so `try_decode` returns `Error::InvalidMsg` (`INVALID`) — never a lossy
`from_utf8_lossy`/`U+FFFD`, never empty. The `std` and `no_std` profiles use the
same strict path and now agree on invalid input, **subsuming #80**. Validity is a
property of the complete payload: a split multi-byte sequence stays `INCOMPLETE`.
Encode-side strictness is corelib-side (`OStream::write_string`).

## §7.3: an integer array at a scalar id (issue #183)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not one: it streams an integer array's elements through the **same**
`unsigned()/signed()` callbacks a lone scalar uses, so an integer array header at a
scalar-declared id of the same signedness would be stored element by element.

The generated visitor therefore carries a skip counter. `array_begin` arms
`askip = count` when the announced kind is the unsigned or signed integer kind
and the `(scope, id)` pair is **not** a declared integer-element native array;
the two scalar callbacks then discard while armed. It self-terminates on the
announced count (no array-end callback needed), survives a chunk boundary (the
counter lives in the visitor), leaves legitimate arrays untouched, and still
decodes a real scalar arriving at that id after the array. The fp arrays are never
armed — their elements go to the float callbacks and cannot reach a scalar arm.

Under `no_std` the guard is emitted only when the schema turns on the `array`
Cargo feature: without it `corelib-rs-no-std` cannot decode an array wire type at
all, so no element can reach a scalar callback, and referencing the feature-gated
`ArrayKind` would not compile. `corelib-rs` (std) compiles every wire type in, so
the `std` profile always carries the guard — including for a message with no array
field, where `array_begin` is emitted purely to arm it.
