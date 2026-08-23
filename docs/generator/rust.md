# Rust target — `targets.rust`

Emits one struct per message and named type. Two corelibs are available and the
choice decides the whole profile — throughput or footprint.

## Options

| key | type | default | effect |
|---|---|---|---|
| `corelib` | `rs` \| `rs-no-std` | `rs` | Which Rust corelib the generated crate targets. |
| `no_std` | boolean | `true` for `rs-no-std` | Emit a genuinely `#![no_std]`, heap-free crate. `corelib: rs-no-std` only. |
| `allow_dynamic` | boolean | depends on `corelib` | Storage for schema-bounded fields: `String`/`Vec` or fixed-capacity `heapless` ones. |
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds a Cargo crate and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. See the [generic config](README.md). |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |

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

This is the Rust analogue of the C++ [`allow_dynamic`](cpp.md#allow_dynamic),
and behaves the same way.
