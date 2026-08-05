# Rust target — `targets.rust`

Target-specific options, accepted under `targets.rust`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets (see below). |
| `no_std` | bool | `true` when `corelib: rs-no-std` | Emit a genuinely `#![no_std]`, heap-free crate (see below). Set `false` for an ordinary `std` crate against the no-std corelib — which also changes the crate's **shape**, its `encode()` return type and how `serde` is gated ([below](#no_std-false--the-no-std-corelib-from-a-std-crate)). Ignored for `corelib: rs`. |
| `allow_dynamic` | bool | *corelib* | Storage for schema-bounded fields: `true` = `String`/`Vec`, `false` = fixed-capacity `heapless` sized from the bound. Defaults to `false` for `corelib: rs-no-std`, `true` for `corelib: rs`. Wire-identical either way. See [Storage mode](#storage-mode-allow_dynamic). |

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

### `no_std: false` — the no-std corelib from a `std` crate

The opt-out is for the **host side of a firmware link**: a desktop tool that has
to speak to a device wants the same schema-sized containers — so a value it can
hold is a value the device can hold — but not the `#![no_std]` environment those
containers usually come with. It changes three things, and field storage is not
one of them:

| | `no_std: true` (default) | `no_std: false` |
|---|---|---|
| crate shape (`emit: project`) | **lib + bin** — `src/lib.rs` (+ `src/message.rs`), `src/main.rs` behind `required-features = ["std"]` | **bin only** — `src/main.rs` + `src/message.rs`, *no* `src/lib.rs` |
| `encode()` returns | `heapless::Vec<u8, MAX_SIZE>` | `Vec<u8>` |
| `serde` | behind the `serde` cargo feature | derived unconditionally |
| field storage | `heapless`, sized from the schema | **unchanged** — `heapless`, sized from the schema |

Storage is [`allow_dynamic`](#storage-mode-allow_dynamic)'s axis, not this one:
turning `no_std` off leaves `heapless::String<8>` a `heapless::String<8>`.

The crate shape is the one that reaches a call site. The `#![no_std]` rows are
libraries, so a consumer writes

```rust
use sofabuffers_generated::Probe;
```

while a `no_std: false` crate is a binary — the same shape `corelib: rs` emits —
so the type is reached as a module of your own program:

```rust
mod message;
use message::Probe;
```

The streaming API is identical either way: `serialize` into an `OStream` with a
flush sink, `decoder()` → `feed`/`finish`. A host tool and the firmware it talks
to can share that code verbatim.

**Unbounded fields.** A string/blob without `maxlen`, or an array without
`count`, cannot be sized, so on the `no_std` path such a field fails generation
with an error naming the field. That holds in **both** storage modes —
`allow_dynamic` picks the container, never whether a bound is needed — so one
schema stays valid for every `no_std` target. For genuinely unbounded fields, use
`corelib: rs`.

### Storage mode (`allow_dynamic`)

The switch chooses where a schema-**bounded** field's bytes live. It is available
against **both** corelibs; only the default differs, for the same reason it
differs in C++ — a firmware target has no heap to spare, a server target would
rather hold what a message carries than its declared worst case.

| schema | `allow_dynamic: false` | `allow_dynamic: true` |
|---|---|---|
| `string, maxlen 8` | `heapless::String<8>` | `String` (`alloc::string::String` under `no_std`) |
| `blob, maxlen 8` | `heapless::Vec<u8, 8>` | `Vec<u8>` |
| `array u32, count 4` | `heapless::Vec<u32, 4>` | `Vec<u32>` |
| `array string, count 2, maxlen 4` | `heapless::Vec<heapless::String<4>, 2>` | `Vec<String>` |

Default: **`false`** for `corelib: rs-no-std`, **`true`** for `corelib: rs`.

Static storage guarantees no allocation at all for the fields it covers: the
worst case is the struct's size, known at compile time. Dynamic storage suits a
target with an allocator — a field then holds what the message actually carries,
which matters once a bound is large enough that the inline struct no longer fits
comfortably on a stack.

**On `corelib: rs` it applies per field, wherever a bound exists.** An unbounded
field simply keeps its `String`/`Vec`; it is never a generate-time error. So the
switch can be turned on against an existing schema without changing it, and a
message can mix both containers. Selecting it adds a `heapless` dependency to the
generated crate — a default `corelib: rs` crate depends on the corelib and serde
alone.

Under `no_std` the rule is stricter, and not because of this switch: every field
must be bounded in both modes (see above), so `allow_dynamic: true` there means
`alloc::String`/`alloc::Vec` and pulls `extern crate alloc`.

Nothing else changes with it. `no_std`-ness is a separate axis: static storage on
`corelib: rs` still derives serde unconditionally, still keeps the decoder's own
stack and reassembly buffer on the heap, and still produces an ordinary std
crate. Storage is about message fields.

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

Each has an `allow_dynamic` twin, and a twin is only readable as a **pair** — the
flag has no absolute number, only a difference against the row it toggles.

`rust-rs-static` is `rust-rs` with `allow_dynamic: false`: bounded fields become
`heapless::String<N>` / `heapless::Vec<T, N>`, so the decode path fills them in
place instead of allocating per field. The trade is `sizeof` — a message holds
its declared worst case — which is why this is a row and not a default.

`rust-rs-no-std-dyn` is `rust-rs-no-std` with `allow_dynamic: true`. Under `no_std`
the flag swaps `heapless` storage sized from the schema
bound for `alloc::String`/`alloc::Vec` — the crate then pulls in `extern crate alloc`,
and a bare-metal target ships no allocator, so the footprint driver appends the most
trivial bump allocator that can work (`tests/bench/lang/rust.sh`, only when the
generated crate needs it). Never freeing makes its `.text` a **floor** — a real
firmware allocator costs more, never less — and the arena lives at a fixed address so
no arbitrary heap size lands in `.bss`. Measured: `.text` 9145 → 10649, `.bss` 0 → 4
(the bump cursor). The runtime heap itself is outside what any static-section
measurement can see.

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

**Only a materialized string is validated (issue #257).** corelib-rs delivers every
fixlen-string field to `fn string(...)` — an unknown id and a §7.3 wire-type
contradiction included — so the callback opens with a `match (self.cur, id)` over
the string destinations that `return`s on anything else. Everything after it
(`acc`, `from_utf8`, the sticky `inv`) therefore runs only for a payload this scope
actually reads, which is what CORELIB_PLAN §6.4 requires, and a skipped payload can
never leave bytes in `acc` for a later declared field to inherit. The `maxlen` and
`max_dyn_string_len` pre-checks sit behind the guard: they are destination-scoped
themselves, so §5.2's INVALID-over-INCOMPLETE ordering is unchanged. `blob()` has no
such guard — bytes carry no encoding.

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
decodes a real scalar arriving at that id after the array. Float arrays are armed
the same way (issue #193): the corelib streams a fixlen array's elements through
the very `fp32()`/`fp64()` callbacks a lone float scalar uses, so those two
callbacks carry the guard too — keyed by element subtype, see the next section.

Under `no_std` the guard is emitted only when the schema turns on the `array`
Cargo feature: without it `corelib-rs-no-std` cannot decode an array wire type at
all, so no element can reach a scalar callback, and referencing the feature-gated
`ArrayKind` would not compile. `corelib-rs` (std) compiles every wire type in, so
the `std` profile always carries the guard — including for a message with no array
field, where `array_begin` is emitted purely to arm it.

## §7.3: a fixlen array is keyed by its element subtype (issue #259)

A fixlen (float) array carries **two** header words: the element count, then the
`fixlen_word` that names the element subtype. `ArrayKind` therefore names that
subtype — `Unsigned = 0`, `Signed = 1`, `Fp32 = 2`, `Fp64 = 3` — and the corelib
delivers `array_begin` only *after* the `fixlen_word` has been read and validated.
(Integer arrays have no second word, so their call still follows the count varint
directly.)

That ordering is what makes the generated decoder's three `array_begin` matches
key on `kind`:

```rust
fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize) {
    self.askip = match kind {
        ArrayKind::Unsigned | ArrayKind::Signed => match (self.cur, id) { /* int arrays */ _ => count },
        ArrayKind::Fp32 => match (self.cur, id) { (_Loc::Root, 1) => 0, _ => count },
        ArrayKind::Fp64 => match (self.cur, id) { (_Loc::Root, 2) => 0, _ => count },
    };
    self.afill = match kind { /* the same keying, armed with `count` */ };
    match (kind, self.cur, id) {
        (ArrayKind::Fp32, _Loc::Root, 1) => { if count > 4 { self.inv = true; return; } self.m.f32s.clear() },
        (ArrayKind::Fp64, _Loc::Root, 2) => { if count > 6 { self.inv = true; return; } self.m.f64s.clear() },
        (_, _Loc::Root, 3) => { if count > 8 { self.inv = true; return; } self.m.ints.clear() },
        _ => {}
    }
}
```

A declared `fp32[N]` appears **only** under `Fp32`, a declared `fp64[N]` **only**
under `Fp64`. An `fp64` header arriving at an `fp32`-declared id matches none of
its arms, so it falls through to `_ => count` on the skip counter and its elements
are discarded — a §7.3 skip, exactly like an array at an unknown id.

### Why the `count` bound sits *inside* the arm

The element count is on the wire **before** the subtype. A bound applied on the
strength of the count alone would therefore be decided before it is known whether
the header belongs to this field at all. Put `if count > N { inv }` ahead of the
kind test and an `fp64` array whose count exceeds a *different* field's declared
`fp32` capacity rejects the whole message as `InvalidMsg` — on a bound that was
never its bound, for a field §7.3 says is skipped whole. For the same reason the
container's `clear()` is inside the arm: a skipped field must not lose the value
it already holds.

The order the arm enforces is: format ceiling (the corelib's own `ARRAY_MAX`, on
the count word) → subtype → §7.3 kind test → schema bound, for a field that
survives all three.

## §7.1: the declared integer width is a validity bound (issue #266)

`u8`/`u16`/`u32` and `i8`/`i16`/`i32` destinations reject a wire value outside
their declared range as `InvalidMsg`. The width is a normative bound, not a
storage hint (MESSAGE_SPEC §1/§7.1), so the value is neither masked nor kept:

```rust
fn unsigned(&mut self, id: Id, value: Unsigned) {
    match (self.cur, id) {
        (_Loc::Root, 0) => { if value > 255 { self.inv = true; return; } self.m.a_u8 = value as u8 },
        (_Loc::Root, 3) => self.m.d_u64 = value as u64,   // u64: no guard, none reachable
        (_Loc::Root, 8) => { if self.afill == 0 { return; } self.afill -= 1;
                             if value > 255 { self.inv = true; return; }
                             self.m.arr_u8.push(value as u8); },
        _ => {}
    }
}

fn signed(&mut self, id: Id, value: Signed) {
    match (self.cur, id) {
        (_Loc::Root, 4) => { if value < -128 || value > 127 { self.inv = true; return; } self.m.e_i8 = value as i8 },
        _ => {}
    }
}
```

Three things the shape encodes:

- **The `as` cast was the bug.** `value as u8` is precisely the mask §7.1
  forbids; the guard has to run before it, or the truncation has already
  happened.
- **`u64`/`i64` get no guard.** Their range *is* the accumulator the value
  arrives in, so a comparison would be dead code — and Clippy would rightly say
  so.
- **In an array arm the guard follows `afill`.** An over-width scalar at an array
  id with no `array_begin` in front of it is a §7.3 skip; guarding ahead of the
  fill check would reject a message the skip clause says is fine.

Both profiles are identical here — the bound is a schema fact, so `no_std`
changes nothing about it.

### Feature gating under `no_std`

In `corelib-rs-no-std` the whole `ArrayKind` enum is `#[cfg(feature = "array")]`
and `Fp32`/`Fp64` are additionally `#[cfg(feature = "fixlen")]`. The generator
names `ArrayKind::Fp32` or `ArrayKind::Fp64` **only when the schema actually
declares a native array of that element subtype** — a schema that declares none
keeps the catch-all, which arms the discard counter for that wire kind anyway. So
a crate never depends on `fixlen` for a variant it has no field for.

When both subtypes *are* declared, `{Unsigned, Signed, Fp32, Fp64}` is exhaustive
and the trailing `_` arm is dropped; emitting it would be an unreachable-pattern
warning in the generated crate.

## §7.3/§5.2: a skip is scoped and inert (issues #268, #270, #271, #272, #273)

Four defects, three causes, one theme: a construct the decoder discards must take
its children with it and must arm nothing behind it.

**`sequence_begin` has a dead scope.** Its default arm used to be
`_ => self.cur` — "stay where you are" — so a sequence the schema does not
declare here was entered and its children bound into the enclosing scope:

```rust
fn sequence_begin(&mut self, id: Id) {
    if self.cur == _Loc::Dead { self.dead = self.dead.saturating_add(1); return; }
    self.stack.push(self.cur);
    self.cur = match (self.cur, id) {
        (_Loc::Root, 10) => _Loc::Root_known,
        _ => _Loc::Dead,          // <- was `self.cur`
    };
}
```

`Dead` matches no callback arm, so the whole subtree is discarded. A sequence
opened *inside* a dead subtree matches no arm either, so it would stay `Dead`
regardless — which is why it is **depth-counted rather than stacked** (see
[§4.9: only live scopes are stacked](#49-only-live-scopes-are-stacked-issue-283)
below).

This is emitted **unconditionally** now, even for a message with no sequence of
its own — corelib-rs's `Visitor` default is a no-op, so a missing override let an
unknown sequence's children arrive with `cur` still on root. With no arms the
emission collapses to `self.cur = _Loc::Dead;` and the parameter is named `_id`,
so the crate stays warning-clean.

**`array_begin` keys one arm per wire kind.** The integer kinds shared an arm
(`ArrayKind::Unsigned | ArrayKind::Signed`) and the schema `count` was reachable
through a wildcard kind. Both let a header §7.3 says to skip reach machinery
belonging to a field it is not — the fill counter stayed armed and absorbed the
next bare scalar (#270), and a fixlen header was measured against an integer
field's `count` (#271). Each arm now names exactly one kind, so the kind check is
the match.

**A wrapper element is replaced, not appended to** (`no_std`, #273). The element
sinks pushed into the destination without clearing it, so a repeated element id
concatenated instead of overwriting (§7.4 last-wins) and the capacity check on the
same line — written for an empty destination — misfired into `Error::BufferFull`
on any repeat at any size. Chunk reassembly happens upstream in `acc`, so every
arm receives one complete value and appending is never correct.

## §4.9: only live scopes are stacked (issue #283)

The scope stack is a `heapless::Vec<_Loc, N>` on `no_std`, sized from the
**schema**: `N` = the number of reachable frames (min 4). Nesting depth, however,
is a property of the **wire** — `MAX_DEPTH` is 255 (CORELIB_PLAN §4.9/§6.2), and
an unknown sequence, which a decoder must accept and skip for forward
compatibility, may nest arbitrarily inside a known one. Those two bounds are
unrelated, so a legal message could overrun the capacity.

`heapless`'s `push` reports that with a `Result`, which the emitted code dropped
(`let _ = …`). Past the capacity the push did nothing, the matching `pop` restored
the *wrong* scope, and a field written after the unwind bound nowhere: the message
decoded **accepted, minus that field** — a wrong value, not an error (Crucible
F-0055; `rust-std` was unaffected because its stack grows). It survived every
depth vector because nesting *only* unknown sequences is self-correcting — every
scope involved is `Dead` and the surplus pops land on `unwrap_or(_Loc::Root)`,
which is the right answer when the base scope is the root. The corruption needs a
**real scope underneath** the overflow.

The fix removes the mismatch instead of widening the buffer to 255 entries, which
a footprint profile should not pay for: a sequence opened while `cur` is already
`Dead` is **counted, not stacked**.

```rust
fn sequence_begin(&mut self, id: Id) {
    if self.cur == _Loc::Dead { self.dead = self.dead.saturating_add(1); return; }
    …
}
fn sequence_end(&mut self) {
    if self.dead > 0 { self.dead -= 1; return; }
    self.cur = self.stack.pop().unwrap_or(_Loc::Root);
}
```

Every scope inside a skipped subtree is `Dead`, so the stack was only recording
which level to come back to — and one `u16` records that for any depth. What
remains on the stack is one entry per *live* scope entered, and a chain of live
scopes cannot be longer than the frame count, so the one place where wire depth
escapes the schema no longer touches the capacity. `dead` is part of the
persistent state: a skipped subtree can straddle a chunk boundary in the
incremental decoder.

One path still pushes without descending: an over-index wrapper element
(`id >= count`) returns with `cur` left on the array frame, so that its own
`sequence_end` pops the entry back off. Nesting those repeatedly can still reach
the capacity — but the guard has set `self.inv` by then, so the message is
`InvalidMsg` whatever the stack does, and the overflow is now *reported* rather
than dropped.

Both profiles emit it — the counter is what makes the capacity argument sound on
`no_std`, and on `std` it keeps the two decoders' scope handling identical while
saving the pushes. The `no_std` push additionally *reports* an overflow now
(`self.err = true` → `Error::BufferFull`) and enters the level as a skipped
subtree, so a scope that was never stacked can never be popped back into.
Discarding that `Result` is exactly what turned a capacity overrun into a silently
wrong value; the machinery to surface it was already there.

Cost, measured (`tests/bench/results.txt`): `.text` +96 B on thumbv6m (+1.0%) and
decode +108 Ir/op (+0.4%) for `rust-rs-no-std`, +112 B / +111 Ir for its
`allow_dynamic` twin, +136 Ir (+0.6%) for `rust-rs`; encode is untouched. Sizing
the stack to 255 entries instead would have cost ~250 B of RAM on the profile that
has the least of it.
