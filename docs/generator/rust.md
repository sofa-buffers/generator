# Rust target — `targets.rust`

Language-specific options for the Rust backend. For shared options (`emit`,
`file_layout`, `buffer`, `omit_defaults`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets (see below). |

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

[`corelib-rs-no-std`]: https://github.com/sofa-buffers/corelib-rs-no-std
[`corelib-rs`]: https://github.com/sofa-buffers/corelib-rs

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`module` · `edition` · `no_std` · `alloc` · `string_storage` · `derives`
