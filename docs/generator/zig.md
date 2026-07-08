# Zig target — `targets.zig`

Options accepted under `targets.zig`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |

The Zig target has a single corelib — [`corelib-zig`], the **max-speed** port
of the family (allocation-free streaming encoder, zero-copy contiguous
decoder, comptime duck-typed visitor) — so there is no `corelib` selector.
`sources` emits `src/message.zig`; `project` additionally scaffolds
`build.zig`, `build.zig.zon` and a JSON encode/decode harness
(`src/main.zig`).

Set the corelib path in the generated `build.zig.zon` (the
`${SOFAB_ZIG_CORELIB}` placeholder) before building; `build.zig.zon` path
dependencies must be **relative to the project root**. Build with
`zig build --release=fast` (Zig 0.16+) — the corelib is tuned for
ReleaseFast and the generated `build.zig` prefers it under `--release`.

## Generated shape

One `pub const <Message> = struct { … }` per object, with the **schema
defaults in the field declarations** — a plain `.{}` value carries every
default, which is what makes sparse-canonical decode (MESSAGE_SPEC §2) a
no-op for omitted fields. Enums and bitfields become `pub const` integer
namespaces (`someenum.RED`, `somebitfield.FLAGA`) over the narrowest backing
integer, shared with the Rust backend's rules so all ports agree.

| Field kind | Zig storage |
|---|---|
| numeric / bool / fp | `u8`…`u64`, `i8`…`i64`, `bool`, `f32`, `f64` |
| enum / bitfield | narrowest backing integer (`i8`/`i16`/`i32`, `u8`…`u64`) |
| string / blob | `[]const u8` |
| native numeric/enum/bool/bitfield array (`count N`) | `[N]T` (stack, allocation-free) |
| native array without `count` | `[]const T` |
| string/blob/struct/union/nested array | `[]const T` |
| struct / union | the generated struct type |

Per message:

- `marshal(self, os: *sofab.OStream) sofab.Error!void` — sparse-canonical
  field writes into any caller-configured `OStream` (fixed buffer, or a flush
  sink for streaming).
- `encode(self, alloc) ![]u8` — convenience wrapper: streams through a stack
  scratch buffer into an allocated byte slice via the corelib flush sink.
- `decode(alloc, data) sofab.Error!Message` — one-shot decode on the
  corelib's zero-copy fast path. **The returned message borrows string/blob
  bytes from `data`** (keep the buffer alive as long as the message); array
  storage is allocated from `alloc` — pass an arena and free the whole
  message at once. `MAX_SIZE` bounds the encoded size (schema-sized, capped
  for unbounded fields).

The decoder is the same flat-visitor `(location, id)` state machine as the
Rust backend, monomorphized by the corelib's comptime duck typing (no
vtable). Element stores are bounds-checked explicitly — ReleaseFast compiles
without implicit bounds checks, so hostile counts/ids degrade to dropped
elements, never out-of-bounds writes.

## Unbounded fields

There is no `no_std`-style sizing gate: `corelib-zig` is the family's
max-speed port, and the generated code takes an allocator on the decode
path, so a string/blob without `maxlen` or an array without `count` is fine —
bounded native arrays still lower to fixed `[N]T` stack storage and skip the
allocator entirely.

## Struct field order

Generated struct fields stay in **schema order** — like Rust, Zig reorders
the fields of a default-layout struct itself, so no widest-first reordering
is applied.

[`corelib-zig`]: https://github.com/sofa-buffers/corelib-zig
