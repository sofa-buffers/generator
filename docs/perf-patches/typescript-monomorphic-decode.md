# TypeScript codegen: monomorphic pull decode (design C)

Status: implemented + measured in the arena as a re-applied patch
(`languages/typescript/sofab/monomorphic-decode.patch`, wired into
`languages/typescript/setup.sh`). This doc is the spec for folding the change
into the generator (`github.com/sofa-buffers/generator`,
`generators/typescript/{backend,visitor,project}.go`). Companion to
`docs/perf/decode-design.md` (the cross-language "design C") and
`docs/perf/bottlenecks.md` (item 8, and the TS-specific item 9).

## Problem

sofabgen emits a **push / visitor** decoder per message type:

```ts
static decode(bytes: Uint8Array): Example {
  const o = new Example();
  decode(bytes, o._visitor());   // corelib drives; calls back per field
  return o;
}
_visitor(): Visitor {
  const self = this;
  const acc = new ChunkAcc();
  return {
    unsigned(id, value) { switch (id) { case 0: self.u8 = Number(value); ... } },
    sequenceBegin(id)   { switch (id) { case 10: self.nested = new ExampleNested();
                                                 return self.nested._visitor(); ... } },
    ...
  };
}
```

The corelib's contiguous decoder (`FastDecoder.run`) walks the buffer and calls
`top.unsigned?.(id, v)`, `top.string?.(…)`, etc., where `top` is swapped to a
**different visitor object shape** for each nested message type (Example,
ExampleNested, ExampleArrays, ExampleArraysNested, plus the string-list visitor).
One decode drives those fixed call sites through 5+ hidden classes, so they go
**megamorphic** — V8 cannot build a monomorphic inline cache and cannot inline the
call. Every field pays a generic property load + indirect call. This is TS's
single largest decode cost (`docs/perf/decode-design.md`, "TypeScript — the worst
case: megamorphism").

Secondary waste per decode: ~5 closure-laden visitor objects and ~5 `ChunkAcc`
allocations, **two of which are entirely dead** (the array-only visitors for
`ExampleArrays` / `ExampleArraysNested` never call `str()`/`blob()` but still
`new ChunkAcc()`).

protobufjs wins precisely because it generates **one monomorphic `decode(reader)`
per type** reading straight into a plain object.

## Target emitted output

Drive a corelib **pull cursor** with one `switch(id)` per type that reads directly
into `this.<field>`. Every reader call site has exactly one caller (the generated
per-type decoder), so it stays monomorphic and inlines. No per-field closures, no
`ChunkAcc`, no visitor objects.

```ts
import { OStream, Cursor } from "@sofa-buffers/corelib";

export class Example {
  // ... fields, marshal, toJSON, fromJSON unchanged ...

  static decode(bytes: Uint8Array): Example {
    return Example.decodeFrom(new Cursor(bytes));
  }

  static decodeFrom(c: Cursor): Example {
    const o = new Example();
    while (c.readHeader()) {
      switch (c.id) {
      case 0: o.u8 = Number(c.readUnsigned()); break;   // u8..u32: number
      case 6: o.u64 = c.readUnsigned() as bigint; break; // u64/i64: number-first raw
      case 1: o.i8 = Number(c.readSigned()); break;
      case 10: o.nested = ExampleNested.decodeFrom(c); break;   // nested message
      case 100: o.arrays = ExampleArrays.decodeFrom(c); break;
      case 200: {                                                // list-of-string sequence
        const arr: string[] = [];
        while (c.readHeader()) arr.push(c.readString());
        o.string_array = arr;
        break;
      }
      default: c.skip(c.wire); break;                    // forward-compat: unknown id
      }
    }
    return o;
  }
}
```

Array / float / string / blob fields:

```ts
case 0: o.u8 = c.readUnsignedArray() as number[]; break;  // u8..u32 arrays
case 6: o.u64 = c.readUnsignedArray() as bigint[]; break; // u64/i64 arrays (number-first)
case 1: o.i8 = c.readSignedArray() as number[]; break;
case 0: o.fp32 = c.readFp32Array(); break;                // fp32/fp64 arrays
case 0: o.f32 = c.readFp32(); break;                      // scalar fp32
case 1: o.f64 = c.readFp64(); break;                      // scalar fp64
case 2: o.str = c.readString(); break;                    // string
case 3: o.bytes_field = c.readBlob(); break;              // blob (zero-copy view)
```

The whole `ChunkAcc` class, the `stringListVisitor` / `blobListVisitor` /
`structListVisitor` helpers, the module `_utf8`, and every `_visitor()` method are
**no longer emitted**. The imports drop `decode` and `type Visitor` and add
`Cursor`. `marshal`, `toJSON`, `fromJSON` are untouched.

## Corelib primitive it relies on (already landed / in PR for corelib-ts)

A new `Cursor` class (`src/decode/cursor.ts`, exported from the package root)
alongside the existing `FastDecoder`/visitor path (which is kept for streaming):

- `readHeader(): boolean` — advance to the next field header; sets `id`/`wire` and
  returns `true` for a field, or returns `false` (consuming the marker) at
  end-of-buffer **or** at the sequence-end closing the current sequence. This one
  method makes root and nested decoders loop identically: the root ends at
  end-of-buffer, a nested `decodeFrom` ends at its matching `SequenceEnd`.
- `readUnsigned()/readSigned()`: number-first `number | bigint` (bigint only above
  `2^53-1`) — identical semantics to the visitor's `unsigned`/`signed`.
- `readFp32()/readFp64()`: consume the fixlen sub-header + payload.
- `readString(): string` / `readBlob(): Uint8Array` (zero-copy `subarray` view).
- `readUnsignedArray()/readSignedArray(): (number|bigint)[]`,
  `readFp32Array()/readFp64Array(): number[]` — read count + elements in a tight
  loop into a fresh array.
- `skip(wire: number)`: consume an unknown field's value (a `SequenceStart` skips
  the whole nested subtree), keeping the cursor in sync for forward-compat.

The varint core (number-first `lo`/`hi` halves, no bigint below `2^53`) is shared
verbatim with `FastDecoder`.

## Generalization rules for the codegen backend

For each message type `T`, emit `static decode(bytes)` → `T.decodeFrom(new Cursor(bytes))`
and `static decodeFrom(c: Cursor): T` = `const o = new T(); while (c.readHeader()) switch (c.id) { … } return o;`.

Per field `id` of schema type → case body:

| schema field type            | case body                                         |
|------------------------------|---------------------------------------------------|
| u8/u16/u32 (fits `number`)   | `o.f = Number(c.readUnsigned()); break;`          |
| i8/i16/i32                   | `o.f = Number(c.readSigned()); break;`            |
| u64 (`bigint` field)         | `o.f = c.readUnsigned() as bigint; break;`        |
| i64 (`bigint` field)         | `o.f = c.readSigned() as bigint; break;`          |
| fp32 / fp64 scalar           | `o.f = c.readFp32(); / c.readFp64(); break;`      |
| string                       | `o.f = c.readString(); break;`                    |
| blob                         | `o.f = c.readBlob(); break;`                       |
| u8..u32 array                | `o.f = c.readUnsignedArray() as number[]; break;` |
| i8..i32 array                | `o.f = c.readSignedArray() as number[]; break;`   |
| u64/i64 array                | `o.f = c.readUnsignedArray()/readSignedArray() as bigint[]; break;` |
| fp32 / fp64 array            | `o.f = c.readFp32Array(); / c.readFp64Array(); break;` |
| nested message `M`           | `o.f = M.decodeFrom(c); break;`                   |
| list-of-string sequence      | inline `{ const arr:string[]=[]; while (c.readHeader()) arr.push(c.readString()); o.f = arr; break; }` |
| list-of-blob sequence        | same with `readBlob()`                            |
| list-of-message `M` sequence | inline `{ const arr:M[]=[]; while (c.readHeader()) arr.push(M.decodeFrom(c)); o.f = arr; break; }` |

Always emit a `default: c.skip(c.wire); break;` so an unknown id is consumed
(matches the visitor path, which silently drops unknown fields).

The `as number[]` / `as bigint[]` casts bridge the reader's number-first
`(number|bigint)[]` return to the field's declared element type; the runtime values
are byte-for-byte the same the visitor produced (u8..u32 elements are always
`number`; u64/i64 are number-first, `bigint` only above `2^53`).

## Why this preserves the wire / correctness gate

The produced object graph is **identical** to the visitor path's, field for field:
same number-vs-bigint choice (shared number-first helpers), same zero-copy blob
`subarray` views, same string decode, same fresh arrays, same nested-object
defaults. Re-encoding it therefore yields the same bytes. Validated by the arena's
sha256 gate — `db362bf24959b41fd153b59958e2afdf59020c6c3501fb60e189526659a72ed4`,
wire 436 B, **unchanged** — and by corelib-ts's `Cursor` test suite.

## Validation

- Arena bench (`BENCH_ITERS=500000 bash languages/typescript/bench.sh`), same host,
  apples-to-apples before/after:
  - **decode-only** (isolated): **80.5 → 98.6 MB/s** (5414 → 4434 ns/op), the
    megamorphic → monomorphic win; gap to protobufjs decode (2898 ns/op) narrows
    from 1.87× to 1.53×.
  - **combined encode+decode** (what the arena reports): **~31.5 → ~33.6 MB/s**,
    speed-adv vs protobuf **0.54× → 0.58×**. The combined metric is capped because
    encode is ~64% of the loop and is out of scope here (already pooled/fast); even
    a free decode would only reach ~0.63×. The decode change is the full available
    decode-side win.
- corelib-ts: `npm test` (379 tests incl. new `test/cursor.test.ts`) + `tsc --noEmit`
  green.

## Further opportunity (not taken here)

`new T()` eagerly constructs default child-message objects (`nested`, `arrays`, …)
that `decodeFrom` immediately overwrites — ~8% additional decode headroom measured
by dropping them. It is **not** applied because it changes decode semantics for an
**absent** nested field (would become `undefined` instead of a default instance)
and makes a bare-constructed `marshal` throw. A generator-level fix would need lazy
/ deferred child defaulting (construct the default only for fields not seen on the
wire), preserving the "absent ⇒ default" contract while skipping the throwaway
allocation on the common all-present path.
