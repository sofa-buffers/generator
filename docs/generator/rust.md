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
| native numeric/enum/bool array (`count N`) | `Vec<T>` | `heapless::Vec<T, N>` |

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
| `array u32, count 4` | `heapless::Vec<u32, 4>` | `alloc::vec::Vec<u32>` |
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

## Arrays — `count` is a capacity

Every array field maps to a variable-length container, and the container's length
is the array's length. A schema `count: N` is a **capacity**, not a length: it
never reaches the wire, it bounds the array (an element count or element id past
`N` fails the decode as `Error::InvalidMsg`), and it sizes the fixed-capacity
container the `no_std` profile uses — but it never adds elements.

That is why a native `count: N` array is **not** an inline `[T; N]` on any
profile. A Rust array of exactly `N` cannot express 0..N, so a three-element
message would round-trip into a five-element value. Under `no_std` the container
is `heapless::Vec<T, N>` instead: capacity `N`, `Default` length 0, still inline
and heap-free, and able to hold exactly what arrived.

The consequences you can observe from Rust:

- `Default` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`).
- Encode writes **every** element the container holds. `vec![1u32, 2, 0, 0]` and
  `vec![1u32, 2]` are different values with different bytes, and nothing is
  trimmed — the corelib still ships `trim_tail`/`trim_tail_f32`/`trim_tail_f64`,
  the generator simply stops calling them.
- Decode yields exactly the elements the wire carried: `len()` after a round trip
  equals `len()` before it, for both the compact scalar form and the wrapper form.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `vec![0u32; 4]` is a
  four-element value and stays on the wire.

Practical consequence for hand-written code: fill an array by **pushing**, not by
assigning through `iter_mut()`. A fresh `count: N` array has no elements to
iterate over.

## Wrapper arrays: element placement and the sparse interior

A **wrapper array** is an array whose elements are `string`, `blob`,
`struct`/`union`, or another array: instead of a native array wire type it
lowers to a sequence whose **child id is the element's array index**
(MESSAGE_SPEC §5.1). Two rules follow from that, and the flat visitor has to
implement both.

**Placement, not append.** Decode gap-fills the container with element defaults
up to the wire id and then decodes **into** `container[id]`. Appending would
shorten the array by the size of any interior id gap (`elem0, elem2` is a
*three*-element array whose middle element is the default), and would decode a
**reopened** element id as a second element instead of merging into the first —
where placement gives §7.4's struct-merge for free.

The corelib's `Visitor` is flat — sequence begin/end events, no child visitor
object — so there is nowhere for a child collector to hold the index. Each
wrapper-array frame that decodes elements at an id therefore owns one `usize`
slot in the visitor state (`_ix0`, `_ix1`, …), set from the element id at
`sequence_begin` (or at `array_begin`, for a row of a matrix), and the element's
own location addresses it as `<path>[self._ixN]`. One slot per frame, not a
stack: a decode location can only be active once at a time, so no depth
arithmetic is needed. The slots are part of the *persistent* state, because an
element can straddle a chunk boundary in the incremental decoder. The over-index
reject (`id >= N` → `InvalidMsg`) runs **before** the gap-fill, which is what
bounds it — and, on `no_std`, what keeps the index inside the `heapless::Vec`
capacity.

This applies to **every** element kind, including the two row collectors — a
matrix (`array of array of u32`) and an array of wrapper arrays. Those appended
in arrival order for as long as a row was always written; an interior gap is
reachable now, and an appending collector would have shifted every later row down
by one. Placing by id also gave those two collectors the over-index bound they
previously lacked.

**A native row carries two bounds, and `array_begin` decides both.** A matrix row
is opened by `array_begin`, which sees the row id *and* the row's announced
element count, so both of the schema's bounds are checked there, before the row is
opened or grown: the **row id** against the OUTER `count` (over-index →
`InvalidMsg`) and the row's **element count** against the row's own INNER `count`
(over-count → `InvalidMsg`, the twin of the top-level reject in `generator#216`).
Deciding the element count at the header rather than at the store is what makes
`INVALID` dominate a truncated tail (§5.2). Checking only the id was the earlier
hole: the inner `count: M` was then not a decode bound at all — on `corelib-rs` a
row filled to whatever count the wire announced, and on `no_std` the elements past
the `heapless::Vec` capacity were silently dropped and the message *still*
accepted, the cross-profile divergence §7.1 forbids.

Both rejects also **disarm the fill** (`self.afill = 0`). `array_begin` arms
`afill` with the announced element count before the reject arm runs, and a row's
elements arrive afterwards through the ordinary `unsigned`/`signed`/`fp`
callbacks, which store into whatever row the index slot still names. A reject that
only returned would therefore let the elements it just rejected stream into the
previously opened row, unbounded; zeroing `afill` routes them into the
`afill == 0` skip instead, so they are discarded like a bare scalar at an array id.

**One sparse rule, positional in the value.** An element before the last one that
equals its element default is **omitted**, leaving an id gap that decode restores
from that same default:

- a `string`/`blob` leaf is simply not written;
- a `struct`/`union` element is **not framed** — its lazily-opened frame takes the
  *dropping* closer, so an all-default element vanishes entirely;
- a native row (a matrix row) has no frame of its own, so the rule lands on the
  write: an empty row is not written at all;
- a wrapper row takes the dropping closer like a struct element.

The **last** element is always written — as its value, or as an empty frame —
because its presence is what carries the length (§5.1: highest present id + 1).
So `vec!["a".to_string(), String::new()]`, `vec!["a".to_string()]` and `vec![]`
are three distinct values that encode and decode distinctly, and `[{}, {}]` stays
distinguishable from `[]`.

The choice is made from the position in the **value**, at run time — the schema
cannot answer it — so the generated loop carries `_i0 + 1 == <arr>.len()` for the
leaf kinds and picks between `write_sequence_end_keep()` and
`write_sequence_end()` for the sequence kinds. A sequence-typed **field** (a
struct field, or an array's own wrapper) is different: it always takes the
dropping closer, because an all-default field is omitted and absence
reconstructs it (§2).

`count: N` changes none of this. It is a capacity, so it can never restore an
elided tail, and the same rule applies with or without one. The all-default
predicate the old trailing-run narrowing needed (`is_default()`, `_trim_seq`) is
gone with it: the writer now emits a child for every element the array holds, so
"no child was written" is exactly "the array is empty" and the two cannot drift.

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
