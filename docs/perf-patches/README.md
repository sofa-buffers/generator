# Codegen performance improvements

> **Status: implemented in the generator.** All four fixes below now emit from
> `sofabgen` itself (Rust `[T; N]` arrays + string/blob single-shot; Java
> primitive `long[]/float[]/double[]` + single-shot; C# string/blob single-shot;
> Go `sofab.AcceptBytes` visitor decode). Wire output is unchanged — the generator
> test suite stays green and the Go decode path is round-trip-verified against a
> real `corelib-go`. The per-language arena `setup.sh` re-apply blocks can now be
> deleted (they become no-ops). The guides below remain the design rationale.

This directory documents four **generated-code performance fixes** discovered while
performance-tuning the multi-language SofaBuffers benchmark arena (encode+decode
of one canonical message, SofaBuffers vs Protobuf, per language). Each is currently
carried in the arena as a post-generation `*.patch` applied by that language's
`setup.sh`; **this directory is the spec for folding them into the generator itself**,
after which the arena's re-apply step can be deleted.

## Why these exist

The same wire format runs at **1.4× (faster than Protobuf) in C++** but was
0.46×–0.86× in the other languages. Profiling showed the byte-level corelib codec
is fine everywhere; the deficit is in **what the generator emits** — the per-message
code and its data model. Three recurring mistakes, all fixed below:

1. **String/blob decoded byte-by-byte** into a growable accumulator then copied out
   again, instead of read from the single contiguous chunk the corelib already
   delivers.
2. **Heap/boxed arrays** (`Vec<T>`, `List<Long>`) instead of the fixed-size stack
   arrays the schema allows (like C++'s `std::array<T,5>`).
3. **The pull decoder read byte-by-byte** through a buffered reader instead of the
   corelib's contiguous cursor (Go only).

The cross-language analysis lives in the arena repo under `docs/perf/bottlenecks.md`
and `docs/perf/decode-design.md`.

## The patches

| # | language | change | generator files | measured (arena) |
|---|----------|--------|-----------------|------------------|
| 1 | **Rust** | fixed `[T; N]` arrays instead of `Vec<T>` + string single-shot | `generators/rust/{backend,visitor}.go` | 0.85× → **1.42×** |
| 2 | **Java** | primitive `long[]/float[]/double[]` instead of boxed `List<Long>` + string single-shot | `generators/java/{backend,visitor,project}.go` | 0.62× → **0.86×** |
| 3 | **C#** | string/blob single-shot decode | `generators/csharp/visitor.go` | 0.81× → **0.89×** |
| 4 | **Go** | decode via the corelib's zero-copy `AcceptBytes` visitor instead of the pull API | `generators/golang/backend.go` | 0.46× → **0.99×** |
| 5 | **TypeScript** | monomorphic `decodeFrom(Cursor)` per type instead of the megamorphic push/visitor decoder | `generators/typescript/{backend,visitor,project}.go` | decode **+22%** (see note) |

Each row links to a `<lang>-*.md` implementation guide and a `<lang>-*.patch`
reference diff (the exact before→after on the arena's generated output).

> **TypeScript** got an earlier, smaller fix — the `ChunkAcc` string single-shot —
> folded into codegen in **v0.5.1**; that is the worked example of this whole loop
> closing (once the generator emits the optimized form, the arena's `setup.sh` patch
> step becomes a no-op and is removed). Fix #5 above is the *deeper* TS change — the
> full push/visitor → monomorphic decoder redesign — and requires a **companion
> corelib addition** (a pull `Cursor`, corelib-ts PR #16), so its guide covers both
> the corelib and codegen sides. Note its measured impact is **decode-only**: the
> arena's TS combined metric is encode-bound, so the round-trip number barely moves
> until TS *encode* is also tuned (tracked separately).

## What a reference `.patch` is (and isn't)

Each `.patch` is the **before→after diff of the arena's *generated output*** for the
canonical schema (`message.sofab.yaml`, one message `Example` with scalars, a nested
struct, eight fixed-size integer arrays + two float arrays, and a string array). It
is **not** a diff of the generator source — it shows you concretely what the emitted
code must become. Your job is to change the generator so it *emits* the "after" side
for **any** schema, not just this one. Read the `.md` guide for the generalization
rules (how array sizes, ids, and types map), then use the `.patch` to check your
generalization reproduces the exact bytes on this schema.

## Correctness bar (non-negotiable)

Every fix must be **byte-for-byte wire-identical** — the arena gates on a shared
`sha256` of the encoded message (`db362b…` for this schema across all languages).
The changes are pure in-memory representation / decode-path changes; they must not
alter a single emitted byte. Steps to validate:

1. Build your generator, regenerate the arena's target for that language, and run
   its bench — the `BENCH` line's `sha256` must be unchanged.
2. Run the arena for that language and confirm it still reports `OK`.
3. Delete the corresponding `setup.sh` re-apply block in the arena (it becomes a
   no-op once the generator emits the optimized form — the marker `grep` guard will
   simply always match).

## General principles behind all four

- **Prefer contiguous over streamed.** The whole message is in memory; read straight
  from the buffer/chunk the corelib hands you. Only keep the accumulator/streaming
  path as a fallback for genuinely split payloads.
- **Prefer fixed/primitive over heap/boxed.** Fixed-size schema arrays map to stack
  arrays; never box a scalar to fill a collection.
- **Pre-size, don't grow.** When the element count is known (`arrayBegin` gives it,
  or the schema fixes it), allocate once at the right size and fill by index.
- **Keep the streaming/skip API.** These changes optimize the common in-memory
  decode; do not remove the visitor/streaming path that large or chunked inputs need.
