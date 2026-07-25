# Decode reader models across the SofaBuffers corelibs

> Cross-cutting reference. Per-target details live in `docs/generator/<lang>.md`;
> the wire/corelib contract is ARCHITECTURE §9.

## Why there is more than one

ARCHITECTURE §1: **the corelib owns the wire format; the generator makes typed
calls into it.** The spec (`MESSAGE_SPEC`) fixes the *bytes*, not the decode API.
Every corelib reads byte-identical wire (enforced by the shared conformance
vectors, §12), but is deliberately free to expose whatever decode API is fastest
and most idiomatic in its language. Two forces then push the APIs apart:

1. **Each language's performance model.** Per-element callbacks are zero-cost in
   Rust/Zig (monomorphised + inlined) but catastrophic in Python (call overhead);
   zero-copy views are the fastest option in C++ but impossible under Go's GC.
2. **The two optimization profiles** (ARCHITECTURE §1): `footprint` (heap-free,
   fixed-capacity buffers → header-first bounded reads) vs `maxspeed` (zero-copy,
   batched slices). The *same* language can carry two models — C++ has both
   (`cpp` zero-copy pull vs `c-cpp` push-into-buffer).

The one thing that is **not** automatically uniform is *decode semantics* that
span the header and the payload — e.g. MESSAGE_SPEC §5.2 "INVALID dominates
INCOMPLETE" (a message that is both schema-invalid and truncated must be INVALID).
Whether a model gets that ordering for free depends on **when it surfaces the
count/length word** relative to reading the payload. Each model below notes where
it lands; see generator#216 for the family-wide reconciliation.

The running example is a bounded array field `someuintarray : array<u8> count 4`
(id 15). "Over-count" = a wire element count > 4; per §5.2 it must be **INVALID**
even when the array is then truncated.

---

## Terminology

"Reader model" is an informal umbrella; the established terms come from parsing /
deserialization. **There is no GoF "Reader pattern"** — "reader" is just an
ecosystem name (Rust `Read`, .NET `BinaryReader`, Avro `Decoder`). The precise
classification is a **parser / deserialization API style**, along three axes:

- **Push vs Pull** (the primary axis — SAX vs StAX from XML):
  - **Push** (SAX-style): the *corelib drives* and calls your callbacks — this is
    the **Visitor pattern** / event-driven parsing. Several corelibs name the type
    literally `Visitor` (rust, go, dart `MessageVisitor`).
  - **Pull** (StAX-style): *your code drives* and pulls the next token/value
    (`next()`, `read()`) — the **Cursor / Iterator pattern** (ts `Cursor`, py
    `d.next()`).
- **Streaming vs materialized** (DOM-style): process token-by-token vs. build the
  whole value in memory first. Ours are streaming-nah; whole-unit /
  measure-then-deliver materialize per field.
- **Zero-copy deserialization** (an optimization, not an axis of its own): hand
  back a view/borrow into the input buffer instead of copying (serde's term;
  cpp's `std::string_view` is exactly this).

Mapping the models below: A1 = push/SAX Visitor; A2 = pull/StAX (`Field` cursor,
batched); B = pull/StAX Cursor; C = push/SAX Visitor, materialized per field;
D = two-pass (validate-then-parse) + zero-copy; E = push/SAX Visitor with
caller-provided output buffers ("bind-and-fill").

---

## Model A — header-first (count/length surfaced before the payload)

These decide a schema bound at the deciding word, so §5.2 ordering is **free**
(the generated guard runs before any truncation is seen). Two sub-shapes.

### A1 — push streaming visitor · `corelib-rs`, `corelib-zig`

The corelib pushes events: an array header event carrying the count, then one
event per element; strings arrive as `(total, offset, chunk)` with `total` on the
first chunk. The generated decoder is a flat `(location, id)` state machine.

**Corelib API (Rust):**
```rust
trait Visitor {
    fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize);
    fn unsigned(&mut self, id: Id, v: Unsigned);
    fn string(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]);
    fn sequence_begin(&mut self, id: Id) /* ... */;
}
```

**Generated decode (Rust) — the bound is a compare inside `array_begin`:**
```rust
fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize) {
    self.ai = 0;
    match (self.cur, id) {
        // generator#216: reject over-count at the count header, before elements.
        (_Loc::Root, 15) => { if count > 4 { self.inv = true; return; } }
        _ => {}
    }
}
fn unsigned(&mut self, id: Id, value: Unsigned) {
    match (self.cur, id) {
        (_Loc::Root, 15) => { if self.ai < 4 { self.m.arr[self.ai] = value as u8; self.ai += 1; } }
        _ => {}
    }
}
// try_decode reads the sticky `inv` before propagating feed's Incomplete:
//   if v.inv { return Err(Error::InvalidMsg) }   // INVALID dominates
```

**Generated decode (Zig) — same shape, `arrayBegin`:**
```zig
pub fn arrayBegin(self: *_dec_M, id: sofab.Id, kind: sofab.ArrayKind, count: usize) void {
    switch (self.cur) {
        .root => switch (id) {
            15 => { if (count > 4) { self.inv = true; return; } },  // generator#216
            else => {},
        },
        else => {},
    }
}
// decode() checks the sticky flag first:  if (v.inv) return error.InvalidMessage;
```

Fixed-capacity storage (`[N]T` in Zig, `heapless::Vec<T, N>` in no_std Rust) maps
the schema bound directly onto the buffer capacity.

### A2 — pull, `Field` carries the count · `corelib-py`

`d.next()` returns a `Field` whose header (`count`, `size`) is already parsed;
the generated code then pulls the value in one batched read (per-element Python
calls would be far too slow).

**Corelib API (Python):**
```python
fld = d.next()          # Field(id, type, count, size) — header only
d.read_unsigned_array() # batched element read
d.fixlen_len()          # peek a string/blob byte length (bound seam)
```

**Generated decode (Python) — the bound is a compare on `fld.count`:**
```python
def _unmarshal(self, d: Decoder) -> None:
    while True:
        fld = d.next()
        if fld is None or fld.type == WireType.SEQUENCE_END:
            return
        if fld.id == 15:
            # generator#216: reject over-count at the header, before read_*_array().
            if fld.count > 4:
                raise SofaDecodeError("arr: array count above schema capacity 4")
            self.arr = d.read_unsigned_array()
            self.arr = _pad_to(self.arr, 4, 0)
        else:
            d.skip()
```
`SofaDecodeError` (INVALID) and `SofaIncompleteError` (INCOMPLETE) are distinct
sibling exceptions, so the header-time raise wins over a later truncation.

---

## Model B — pull cursor readers · `corelib-ts`

A pull `Cursor`: the generated code loops on `readHeader()` and calls a value
reader. The readers now take the schema bound and reject at the deciding word (the
reader is the natural seam), so a truncated over-bound field is INVALID before the
truncation is seen — §5.2 ordering holds (generator#222, corelib-ts#69).

**Corelib API (TypeScript):**
```ts
class Cursor {
  readHeader(): { id: number; type: WireType } | null;
  readUnsignedArray(bound?: number): bigint[];  // rejects wire count > bound at the count word
  readString(bound?: number): string;           // rejects wire length > bound at the length word
}
```

**Generated decode (TypeScript) — the schema bound is passed into the reader:**
```ts
for (;;) {
  const h = c.readHeader();
  if (h === null || h.type === WireType.SequenceEnd) break;
  if (h.id === 15) {
    // the count>4 reject fires inside readUnsignedArray, at the count word,
    // before any element is consumed — so it wins over a later truncation.
    const a = c.readUnsignedArray(4);
    this.arr = _padTo(a, 4, 0n);
  } else c.skip();
}
```

---

## Model C — whole-unit push callback · `corelib-go`, `corelib-dart`

The corelib reads the *entire* array/string and delivers it whole via one
callback — idiomatic under a GC (allocate once, hand over a slice/list; no stable
pointer into a reused buffer). The whole-value read would surface a truncation
before its bound guard, so a header callback now fires *before* the read: it
carries the count/length word and rejects an over-bound field there, before the
payload — §5.2 ordering holds (generator#222; corelib-go#53, corelib-dart#18/#19).

**Corelib API (Go) — `Visitor` interface, whole slice + header callbacks:**
```go
type Visitor interface {
    UnsignedArray(id ID, v []uint64) error
    String(id ID, s string) error
    ArrayBegin(id ID, count int) error                 // at the count word (§5.2 seam)
    FixlenHeader(id ID, subtype int, length int) error // at the fixlen length word
}
```

**Generated decode (Go) — the bound is a compare in the header callback:**
```go
func (m *MyMsg) ArrayBegin(id sofab.ID, count int) error {
    if id == 15 && count > 4 { return sofab.ErrInvalidMsg }   // at the header, before elements
    return nil
}
func (m *MyMsg) UnsignedArray(id sofab.ID, v []uint64) error {
    switch id {
    case 15:
        // reached only if ArrayBegin accepted the count; just store.
        m.arr = padTo(v, 4, 0)
    }
    return nil
}
```

**Corelib API (Dart) — `MessageVisitor`, whole value + header callbacks; `void`:**
```dart
abstract class MessageVisitor {
  bool shouldRead(int id, WireType type);       // id+type only
  void onUnsignedArray(int id, Int64List values);
  void onString(int id, String value);
  void onArrayBegin(int id, int count) {}                 // at the count word (§5.2 seam)
  void onFixlenHeader(int id, int subtype, int length) {} // at the fixlen length word
}
```

**Generated decode (Dart) — header callback sets the sticky INVALID flag:**
```dart
@override
void onArrayBegin(int id, int count) {
  if (id == 15 && count > 4) e.inv = true;   // at the header, before elements
}
@override
void onUnsignedArray(int id, Int64List values) {
  if (id == 15) arr = List<int>.from(values);  // reached only if onArrayBegin accepted
}
// tryDecode returns:  e.inv ? DecodeStatus.invalid : status;   (INVALID dominates)
```

---

## Model D — measure-then-deliver, zero-copy pull · `corelib-cpp`

The maxspeed C++ engine: `feed()` first *measures* the whole top-level field for
completeness, then delivers it via `deserialize`, where the generated code pulls
values — including a **zero-copy `std::string_view` straight into the buffer**
(the fastest option, but it requires the field be fully present and contiguous,
which the measure pass guarantees). The measure walk would otherwise be
schema-blind — a truncated over-bound field would fail measurement (→ INCOMPLETE)
before the `deserialize` guard runs — so the generator now emits a static
`sofab::schema` bound tree and installs it with `setSchema`; the measure walk
consults it at the count/length word and rejects an over-bound field there, before
the truncation is surfaced (generator#223, corelib-cpp#50).

**Corelib API (C++):**
```cpp
virtual void deserialize(IStreamImpl& is, sofab::id id, size_t size, size_t count) noexcept;
// inside: is.read(value);  is.wire();  is.fixType();  is.invalidate();  is.read(dst, maxlen);
```

**Generated decode (C++):**
```cpp
void MyMsg::deserialize(IStreamImpl& is, sofab::id id, size_t size, size_t count) noexcept {
    switch (id) {
    case 15: {
        if (count > 4) { is.invalidate(); return; }   // belt-and-suspenders on the delivered field
        // ... is.read(arr[i]) per element ...
        break;
    }
    }
}
// The over-count reject that MATTERS runs earlier, in the measure phase: the
// generated static sofab::schema tree (installed by setSchema in decode/try_decode)
// makes measureField reject count>4 at the count word, before truncation is seen.
// feed() ordering then favors INVALID:  if (error_) return InvalidMessage;  (before Incomplete)
```

---

## Model E — push into a caller-bound fixed buffer · `corelib-c-cpp`

The footprint C engine (with a thin C++ wrapper): a resumable **byte-at-a-time
state machine** that pushes decoded bytes straight into a destination the callback
binds, carrying its **capacity**. Because the field callback fires *at the header*
and the read call carries the bound, the corelib reconciles wire-count/length
against the capacity right there — so §5.2 is conformant **by construction** (this
is the reference model the other four fixes mirror). Heap-free: fixed buffers only.

**Corelib API (C):**
```c
typedef void (*sofab_istream_field_cb_t)(sofab_istream_t* ctx,
    sofab_id_t id, size_t size, size_t count, void* usr);  // fires at the header

void sofab_istream_read_array(sofab_istream_t* ctx, void* dst,
    size_t capacity, size_t element_size, uint8_t opt);    // capacity == schema N
void sofab_istream_read_string(sofab_istream_t* ctx, char* buf, size_t cap); // cap == maxlen+1
// _bind_array_count: if (wire_count > ctx->target_count) return SOFAB_RET_E_INVALID_MSG;  // at the count word
```

**Generated decode (C, descriptor-driven):** the generator emits static object
descriptors (`{id, type, size, element_size, ...}`); the shared
`sofab_object_field_cb` reads them and calls `read_array(ctx, member, N, elemSize, opt)`,
so the corelib rejects `wire_count > N` at the count word — no per-field generated
`if`.

**Generated decode (C++ wrapper) — the bound is the container's fixed capacity:**
```cpp
void MyMsg::deserialize(IStreamImpl& is, sofab::id id, size_t size, size_t count) noexcept {
    switch (id) {
    case 15:
        // arr is std::array<uint8_t, 4> / a fixed span; its capacity IS the bound.
        is.read_array(arr.data(), arr.size(), sizeof(uint8_t), /*opt*/0);
        // corelib compares count vs arr.size() at the header -> INVALID before payload.
        break;
    // FixedString<N> / FixedBytes<N> bind likewise: is.read_string(s.data(), s.capacity());
    }
}
```

---

## Summary

| Model | Corelibs | Reader shape | Bound delivered via | §5.2 free? |
|---|---|---|---|---|
| A1 push streaming | rust, zig | `array_begin(count)` + element events | compare inside the header event | ✅ |
| A2 pull `Field` | python | `next()→Field.count` + batched read | compare on `fld.count` | ✅ |
| B pull cursor | ts | `readUnsignedArray(bound)` | bound arg on the reader (corelib-ts#69) | ✅ |
| C whole-unit push | go, dart | `UnsignedArray(id, []T)` + `ArrayBegin` | header callback `ArrayBegin`/`onArrayBegin` | ✅ |
| D measure-then-deliver | cpp | `deserialize` + zero-copy `read()` | measure-phase schema (`setSchema`) | ✅ |
| E push into fixed buffer | c-cpp | header callback + `read_array(dst, cap)` | buffer capacity == bound | ✅ (reference) |

**Takeaway.** The wire is uniform; the reader shape is intentionally not — it is
each language's fastest idiom under its profile. A cross-cutting semantic like
§5.2 must therefore be verified per model. The unifying *invariant* (not a single
signature) is Model E's: **surface the count/length at the header and let the
schema bound be checked there, before the payload** — which A1/A2/E always
satisfied and B/C/D were brought to under generator#216 (generator#222 for
ts/go/dart, generator#223 for cpp). All six models now hold §5.2.
