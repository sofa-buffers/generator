# Decode design: the push/visitor model is the ceiling — move to direct switch-into-fields

Status: design proposal, 2026-07-03. Companion to `docs/perf/bottlenecks.md`.

## TL;DR

Every language except C++ decodes through a **push / visitor** model: the corelib
drives the byte stream and calls a typed visitor method per field, and the
generated visitor then does a **second `switch(id)`** to store the value into a
field. C++ instead uses **one `switch(id)` that reads straight into the struct
field**. The visitor indirection is the single largest *design*-level cost in the
slower languages — decisively so in TypeScript (megamorphic dispatch) and
significantly in Go/Java/C#/Rust.

**Recommendation:** keep the visitor API as the *streaming / ergonomic* path, but
generate a **direct, monomorphic decoder** for the in-memory case (which is what
every benchmark and most real callers hit). We do **not** need different designs
per language — a single "cursor + generated `switch(id)` into fields" shape maps
cleanly onto all of them and is what protobuf's own generators do.

## The three designs, concretely

Using one integer field `u32` at id 4 as the example.

**A. Pull (Go today).** Generated code drives; reads each value explicitly.
```go
for {
    fld, err := d.Next()      // reads a header varint (per byte via bufio.ReadByte)
    switch fld.ID {
    case 4: v,_ := d.Unsigned(); m.U32 = uint32(v)   // second read call
    }
}
```
Cost: the driver is generated (good), but Go's `Decoder` reads through
`bufio.Reader` — a per-byte interface call — and returns `(value, error)` per
field. Two dispatches (`Next` + typed reader) per field.

**B. Push / visitor (C#, Java, TS, Rust today).** Corelib drives; calls a method
per field; the method does a second switch.
```csharp
void Unsigned(int id, ulong value) {          // (1) interface call from corelib
    switch ((cur, id)) {                        // (2) second switch to find the field
        case (Root, 4): m.u32 = (uint)value; break;
    }
}
```
Cost: **two dispatches per field** — the interface call *and* the inner switch —
plus a live `cur` scope variable and a `stack` to track nesting, plus a fresh
visitor object per decode. In a JIT that can't devirtualize (see TS below) the
interface call is the killer.

**C. Direct switch-into-fields (C++ today — the target).** Corelib drives, but
calls **one** method that switches and reads the value *itself*, straight into the
field, over the contiguous buffer.
```cpp
void deserialize(IStreamImpl& is, id id, ...) {
    switch (id) {
        case 4: is.read(u32); break;   // one dispatch; reads into the field in place
    }
}
```
Cost: **one dispatch per field**, no scratch objects, no boxing, no second switch;
under LTO the whole thing inlines into a flat loop the optimizer can vectorize.

Design C is strictly the fewest operations per field. The question is only whether
it ports to managed/dynamic languages. It does.

## Why the visitor model specifically hurts each language

### TypeScript — the worst case: megamorphism *(highest impact)*
`FastDecoder.run` calls `top.unsigned?.(id, v)`, `top.string?.(…)`, etc., where
`top` is swapped to a **different visitor object shape** for each nested message
(Example, ExampleNested, ExampleArrays, …). One decode drives the same call sites
through 5+ hidden classes → the sites are **megamorphic**, and V8 cannot inline or
build a monomorphic inline cache; every field pays a generic property lookup +
indirect call. protobufjs wins precisely because it generates **one monomorphic
`decode(reader)` per type** that reads directly into a plain object. Design C (or a
per-type pull decoder) makes SofaBuffers monomorphic too — this is the change most
likely to take TS from 0.55× past parity.

### Go — the visitor path exists and is fast, but generated code uses the *pull*
path over `bufio` (per-byte interface calls) instead. Even the intermediate step —
switch decode to the corelib's existing `AcceptBytes(buf, visitor)` (design B over a
zero-copy cursor) — removes the per-byte reader and per-element allocation. Design C
(a generated `switch(id)` reading straight from an exposed cursor) is the endpoint.

### Java / C# — dispatch is monomorphic (only one visitor type at the call site) so
the interface call is cheap, but the **second `switch(cur,id)`**, the `cur`/`stack`
scope bookkeeping, and the per-decode visitor+stream+acc allocations remain pure
overhead that design C deletes. (Their *dominant* cost is the boxed array model —
see `bottlenecks.md` Mistake 2 — but the visitor is the next layer.)

### Rust — the `Visitor` trait fills `Vec`s element-by-element behind per-field
`match (cur,id)`. Design C decodes into fixed `[T;5]` fields via a direct `match id`,
removing both the trait indirection and the per-decode `stack`/`acc` bookkeeping Vecs.

## "Can we keep one design across all languages?" — Yes.

The user's hope (one shared design, performance permitting) **survives**. Design C
is not C++-specific; it's the same shape everywhere:

> a corelib **cursor** over the contiguous input buffer, exposing primitive
> `readUnsigned()/readSigned()/readFixlenInto()/…` that advance the cursor, plus a
> generated per-message `decode`/`deserialize` with **one `switch(id)`** that calls
> those readers **straight into its own fields**; nested messages recurse into the
> child's `decode`; fixed arrays are read with a **bulk** reader.

That maps to: C++ (already there), Rust (`match id` into `[T;5]`), Go (exported
cursor + generated `switch`), C# (`ref struct` cursor over `ReadOnlySpan<byte>`),
Java (`byte[]`+int cursor), TS (numeric cursor over `Uint8Array`, monomorphic per
type). One mental model, one codegen template shape, per-language reader primitives.

## Migration plan (keep the visitor, add the fast decoder)

1. **Keep** the visitor/`Accept` API — it's the right *streaming* and
   *skip-subtree* interface, and some callers want push. Don't remove it.
2. **Expose a cursor reader** in each corelib (Go's `cursor` is already there, just
   unexported; C#/Java/TS need a small `ref struct`/`byte[]+pos`/`Uint8Array+pos`
   reader; Rust/C++ already have the pieces).
3. **Generate a direct `decode`** per message type that drives the cursor with a
   single `switch(id)` into fields, used by `Decode()`/`decode()` for the in-memory
   case. Fall back to the visitor path only for true streaming.
4. **Validate** byte-for-byte against the conformance vectors (the sha256 gate in
   the arena already catches divergence) and re-measure.

This is a generator change plus a small corelib addition per language, not a
rewrite, and it preserves the shared-design goal while removing the one structural
cost that keeps four languages below protobuf.

## Order of operations (with the other fixes)

The visitor→direct-switch change is **item 8** in the backlog because two cheaper
things should land first and independently:
1. the string/blob single-shot fix (done for C#, mechanical for Rust/Java), and
2. the primitive fixed-array model (biggest single Java win),

both of which reduce allocation regardless of the dispatch design. Design C then
removes what remains. Sequencing them this way keeps each change measurable in
isolation against the arena's `sha256` correctness gate.
