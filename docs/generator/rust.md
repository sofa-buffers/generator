# Rust target — `targets.rust`

Emits one struct per message and named type. Two corelibs are available and the
choice decides the whole profile — throughput or footprint.

## Options

| key | type | default | effect |
|---|---|---|---|
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets. |
| `no_std` | boolean | `true` for `rs-no-std` | Emit a genuinely `#![no_std]`, heap-free crate. `corelib: rs-no-std` only. |
| `allow_dynamic` | boolean | depends on `corelib` | Storage for schema-bounded fields: `String`/`Vec` or fixed-capacity `heapless` ones. |

The generic options apply here too — including `format`, which decides whether
`sofabgen` runs `rustfmt` over what it emitted (see [Formatting](#formatting));
see the [generic config](README.md).

## `corelib`

| value | runtime | profile |
|---|---|---|
| `rs` | `corelib-rs`, std | throughput |
| `rs-no-std` | `corelib-rs-no-std`, `#![no_std]`, feature-gated per wire type | footprint |

The two produce **identical wire bytes**.

**`rs-no-std` requires a fully bounded schema.** Every `string`, `blob` and
`array` must carry a `maxlen` or `count`; an unbounded field fails generation
rather than falling back to the heap.

## `no_std`

Only meaningful with `corelib: rs-no-std`, where it is on by default. It makes
the generated crate genuinely `#![no_std]`:

- fixed-capacity `heapless::String<N>` / `heapless::Vec<T, N>` fields, sized
  from the schema bound,
- a decode stack bounded at compile time,
- `serde` behind a cargo feature rather than an unconditional dependency.

Set it to `false` to emit an ordinary `std` crate that still links the no-std
corelib — useful when the same schema has to be consumed by a host-side tool
built from the same generated code.

## `allow_dynamic`

Decides the **storage of schema-bounded fields only**. The wire is identical
either way — this is never a format or API decision.

| value | a `maxlen: 32` string becomes |
|---|---|
| `true` | `String`, holding what the message actually carries |
| `false` | `heapless::String<32>`, inline and heap-free |

**The default depends on `corelib`** — `false` for `rs-no-std` (a firmware
target has no heap to spare), `true` for `rs` (a server target would rather
allocate what a message carries than its declared worst case).

Two things worth knowing before switching it off under `corelib: rs`:

- **It adds a `heapless` dependency** to the generated crate.
- **Unbounded fields are unaffected.** They stay in `String` / `Vec`, so the
  switch applies per field wherever a bound exists and static storage can be
  turned on without changing the schema.
- **A fully bounded schema decodes without the heap.** The decoder's own
  scope stack is a fixed array sized from the schema's nesting depth on every
  `std` build, so with no unbounded field left a `try_decode` allocates
  nothing. (A `Decoder` fed in chunks still reassembles a string or blob split
  across two feeds in a heap buffer.)

This is the Rust analogue of the C++ [`allow_dynamic`](cpp.md#allow_dynamic),
and behaves the same way.

## Including the generated module

The generated code builds clean under `RUSTFLAGS="-D warnings"` and
`cargo clippy -- -D warnings`. It carries no crate- or module-wide lint
`allow`, so declare the module the way you would any library surface:

```rust
pub mod message;
use message::*;
```

Declared private (`mod message;`), every part of the API your crate does not
call is dead code to rustc and warns. With `no_std` on, the generated crate is a
lib that re-exports the module, so a dependent crate has nothing to declare.

## Formatting

`sofabgen` can run `rustfmt` over what it generated, and does so only when you
ask: it spawns no external tool on its own, so the same version writes the same
bytes on every machine, whatever happens to be installed.

Asking is one switch — the CLI flag `--format`, or the `generic.format` config
key (the flag wins):

| value | what `sofabgen` does |
|---|---|
| `off` (the default) | Never runs `rustfmt`. The files are the generator's own output: valid, compilable, not canonically formatted. |
| `auto` | Runs `rustfmt` when it is available; when it is not, writes the files unformatted and says so once on stderr. |
| `require` | Runs `rustfmt`, and fails the run when it is not available. |

With the pass on, every generated `.rs` file goes through `rustfmt` once, at the
edition the generated `Cargo.toml` declares, run in the output directory — so a
`rustfmt.toml` of your own that covers that directory is honoured, and the files
come out the way your own `cargo fmt` over that tree would leave them.
`cargo fmt --check` over a tree holding them then passes, so generated files need
no exclusion from a formatting gate and never come back reformatted.

It is a convenience. The generated code is correct and compiles either way; the
switch only decides whether `rustfmt` has already been over it when you receive it,
and running `cargo fmt` over the output directory yourself gets you the same tree.

Under `auto` and under `require` alike, a `rustfmt` that RUNS and rejects a
generated file fails the generation with the file named — that is a generator
bug, and writing the file would only move it into your build.

