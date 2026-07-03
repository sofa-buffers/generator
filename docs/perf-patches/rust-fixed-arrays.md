# Rust — fixed-size `[T; N]` arrays instead of `Vec<T>`

**Impact:** arena 0.85× → **1.42×** (now beats Protobuf, level with C++).
**Reference diff:** `rust-fixed-arrays.patch`
**Generator files:** `generators/rust/backend.go`, `generators/rust/visitor.go`

## Problem

For a **fixed-size** native array field (the schema declares a known element
count, e.g. `u8[5]`), the generator currently emits a heap `Vec<T>`:

```rust
pub struct ExampleArrays {
    pub u8: Vec<u8>,
    // …
}
```

and the decode visitor fills it element-by-element with `.push()`:

```rust
(_Loc::Root_arrays, 0) => self.m.arrays.u8.push(value as u8),
```

Every decode therefore heap-allocates one `Vec` per array (this message: ~10) and
grows it as elements arrive — **~15–20 `malloc`/`free` per decode**, which profiling
identified as Rust's dominant per-decode cost. C++ emits `std::array<T, 5>` on the
stack with zero heap.

## Fix

When the array has a **statically known length `N`**, emit a fixed array `[T; N]`
and fill it by index. The reference diff shows the exact target:

```rust
pub struct ExampleArrays {
    pub u8: [u8; 5],
    // …
}
impl Default for ExampleArrays {
    fn default() -> Self { Self { u8: [0; 5], /* fp: [0.0; 5] */ … } }
}
impl ExampleArrays {
    pub fn marshal(&self, os: &mut OStream) {
        if self.u8 != [0; 5] { let _ = os.write_array_unsigned(0, &self.u8); }
        // …
    }
}
```

Decode fills by an index counter reset in `array_begin`:

```rust
struct V<'a> { …, acc: Vec<u8>, ai: usize }   // add `ai`
// element:
(_Loc::Root_arrays, 0) => { self.m.arrays.u8[self.ai] = value as u8; self.ai += 1; }
// array_begin: fixed arrays are pre-allocated in the struct — just reset:
fn array_begin(&mut self, id: Id, _kind: ArrayKind, _count: usize) { self.ai = 0; }
```

`serde` handles the JSON round-trip for `[T; N]` unchanged (arrays up to 32 are
supported), so no JSON changes are needed. `&self.u8` (`&[T; N]`) coerces to the
`&[T]` the `OStream` writers expect.

## Where in the generator

- **Field type — `generators/rust/backend.go`.** The field type comes from
  `g.rustType(fld)` (emitted at the `pub %s: %s,` line in `emitObject`, ~L169).
  Route a fixed-length native-numeric/fp array to `[T; N]` instead of `Vec<T>`.
  The element Rust type is the existing `numRustType(fld.Elem)`; `N` is the array's
  fixed count from the IR (`fld.ElemItems` / the `ArrayElem` length — confirm which
  field on `ir` carries the fixed count).
- **Default + marshal — `backend.go`.** The `Default` impl must init `[0; N]`
  (integers) / `[0.0; N]` (fp). The marshal empty-guard changes from
  `!self.x.is_empty()` to a default comparison `self.x != [0; N]` (fixed arrays are
  never "empty"; mirror the C++ backend, which guards `!= std::array{}`).
- **Decode — `generators/rust/visitor.go`.** The array-element line at ~L222
  (`… => %s.%s.push(value as %s)`) becomes an indexed store
  `{ %s.%s[self.ai] = value as %s; self.ai += 1; }`. Add an `ai: usize` field to the
  `V` struct (init `ai: 0` at ~L182) and make the emitted `array_begin` reset
  `self.ai = 0` instead of `.clear()`-ing each Vec. Do the same for the fp-array
  path (`fp32`/`fp64` element handlers).

## Generalization / caveats

- **Only for statically-sized arrays.** Keep `Vec<T>` for arrays whose length is
  not fixed by the schema (unbounded/dynamic). Gate the new emission on "IR reports
  a fixed count."
- **Nested-native / struct / string arrays** are a different code path in
  `visitor.go` (`fkNestedNative`, `fkStructArr`, `fkSeqArr`) and are **out of scope**
  here — this fix is native numeric/fp fixed arrays only. Leave the others as-is.
- **Bounds:** filling `[T; N]` by index assumes the wire delivers exactly `N`
  elements (guaranteed for a conformant fixed-size array). If you want defensive
  behavior on malformed input, bound the index — but the corelib should reject a
  wrong element count before the visitor sees it.

## Also folded into this patch: string/blob single-shot

The same diff includes the string/blob single-shot decode (see
`csharp-string-single-shot.md` for the rationale — identical idea): when
`offset == 0 && chunk.len() >= total`, build the `String`/`Vec<u8>` straight from
the chunk instead of `acc.extend_from_slice` + `from_utf8_lossy(&acc).into_owned()`
(a double copy). Emit this in `visitor.go`'s `string`/`blob` handlers. It is
independent of the array change and worth emitting for every language.

## Validate

Regenerate the arena's `languages/rust`, run its bench: `sha256` must stay
`db362b…`, and `speed adv` should jump to ~1.4×. Then remove the
`fixed-arrays.patch` re-apply block from `languages/rust/setup.sh`.
