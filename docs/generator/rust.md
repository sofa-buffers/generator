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

[`corelib-rs-no-std`]: https://github.com/sofa-buffers/corelib-rs-no-std
[`corelib-rs`]: https://github.com/sofa-buffers/corelib-rs
