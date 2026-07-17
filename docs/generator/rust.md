# Rust target — `targets.rust`

Options accepted under `targets.rust`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets (see below). |
| `no_std` | bool | `true` when `corelib: rs-no-std` | Emit a genuinely `#![no_std]`, heap-free crate (see below). Set `false` to emit an ordinary `std` crate against the no-std corelib. Ignored for `corelib: rs`. |
| `allow_dynamic` | bool | `false` | Under `no_std`, keep an `alloc` heap fallback for genuinely unbounded fields instead of failing generation. |
| `max_dyn_array_count` | int | unset = unlimited | Receiver-side decode limit (generator#102): max element count accepted for an **unbounded** array (no schema `count`). `corelib: rs` only (see below). |
| `max_dyn_string_len` | int | unset = unlimited | Receiver-side decode limit: max byte length accepted for an **unbounded** string (no schema `maxlen`). `corelib: rs` only. |
| `max_dyn_blob_len` | int | unset = unlimited | Receiver-side decode limit: max byte length accepted for an **unbounded** blob (no schema `maxlen`). `corelib: rs` only. |

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
before the `Vec` grows. Under `corelib: rs-no-std` that element is instead
dropped into the capacity-bounded `heapless::Vec` (issue #126). The `inv` flag
likewise carries the **over-`maxlen`** reject (Option B, MESSAGE_SPEC §7.1): a
`string`/`blob` (scalar or wrapper element) whose wire byte length exceeds its
schema `maxlen` sets `inv` at the length header, on **both** profiles — on
`no_std` the guard fires ahead of the heapless `BufferFull` path, so an
over-`maxlen` value is `InvalidMsg`, not a capacity error.

**std profile only.** The limits apply to `corelib: rs` (std). Under
`corelib: rs-no-std` the keys are inert: heapless storage is statically
schema-bounded already (an unbounded field is either rejected at generation
time or consciously opted into a heap fallback via `allow_dynamic`), and that
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
  `Cargo.toml` sets `default-features = false` and re-enables **only** the
  features the schema actually uses (`fixlen`, `array`, `sequence`, `fp64`,
  `value64`), so the binary carries no code for unused wire types; a
  `sofab::require!(…)` guard in the generated module asserts the same set. The
  generated decoder also overrides only the `Visitor` callbacks the schema needs,
  so a varint-only schema pulls in none of the feature-gated wire types and a
  schema with no `u64`/`i64` builds against the 32-bit value type.

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
with an error naming the field — unless `allow_dynamic: true` keeps an
`alloc::String`/`alloc::Vec` fallback for it (which pulls `extern crate alloc`;
bounded fields still go `heapless`). This makes "no hidden allocation" the default
guarantee: size your schema, or consciously opt a field into a heap fallback.

```yaml
targets:
  rust:
    corelib: rs-no-std       # no_std is then on by default
    allow_dynamic: true      # optional: alloc fallback for unbounded fields
```

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
