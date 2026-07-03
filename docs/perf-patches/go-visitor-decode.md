# Go — decode via the corelib's `AcceptBytes` visitor, not the pull API

**Impact:** arena 0.46× → **0.99×** (worst target → parity with Protobuf).
This is the most structural of the four — it replaces the emitted decode strategy.
**Reference diff:** `go-visitor-decode.patch` (touches `example.go` + `types.go`)
**Generator file:** `generators/golang/backend.go` (`emitObject` ~L112, `emitUnmarshalField` ~L309, `emitUnmarshalArray`/`unmarshalArray` ~L343)

## Problem

`emitObject` emits a **pull-parser** `unmarshal` (backend.go ~L134):

```go
func (m *Example) unmarshal(d *sofab.Decoder) error {
    for {
        fld, err := d.Next()          // reads a header…
        …
        switch fld.ID {
        case 0: v, _ := d.Unsigned(); m.U8 = uint8(v)   // …then a typed read
        …
        }
    }
}
func DecodeExample(data []byte) (*Example, error) {
    m := NewExample()
    m.unmarshal(sofab.NewDecoder(bytes.NewReader(data)))   // reader-backed
    …
}
```

`sofab.Decoder` reads through a `bufio.Reader`, so **every varint byte goes through
`bufio.ReadByte`** (a per-byte call), and `d.ReadFloat32Array` etc. `make()` a small
buffer **per float element**. The corelib already ships a zero-copy contiguous
cursor for exactly this case — `sofab.AcceptBytes(buf, visitor)`, documented as *"the
fastest entry point when the message is already in memory (e.g. generated Unmarshal
code)"* — but the generator does not use it.

Measured on this message: decode 5135 → 2240 ns/op, 41 → 28 allocs/op.

## Fix

Emit the generated types as `sofab.Visitor` implementations and decode with
`AcceptBytes`. The reference diff shows the full shape; the key pieces:

**A no-op base** (emit once per package) so each type overrides only what it uses:

```go
type _visitorBase struct{}
func (_visitorBase) Unsigned(sofab.ID, uint64) error { return nil }
// … all 12 Visitor methods as no-ops; BeginSequence returns _visitorBase{} …
```

**Each message type** embeds `_visitorBase` and implements its fields:

```go
type Example struct { _visitorBase; U64 uint64; … }   // embed the base

func (m *Example) Unsigned(id sofab.ID, v uint64) error {
    switch id { case 0: m.U8 = uint8(v); case 2: m.U16 = uint16(v); … }
    return nil
}
func (m *Example) BeginSequence(id sofab.ID) (sofab.Visitor, error) {
    switch id {
    case 10:  return &m.Nested, nil                       // nested struct is itself a Visitor
    case 100: return &m.Arrays, nil
    case 200: m.StringArray = m.StringArray[:0]; return &_stringSeq{out: &m.StringArray}, nil
    }
    return _visitorBase{}, nil
}
func DecodeExample(data []byte) (*Example, error) {
    m := NewExample()
    if err := sofab.AcceptBytes(data, m); err != nil { return nil, err }
    return m, nil
}
```

**Native arrays** arrive as `UnsignedArray(id, []uint64)` / `SignedArray(id, []int64)`
/ `Float32Array/Float64Array`; narrow to the declared element type (a small generic
helper `_narrowU[T]`/`_narrowS[T]`), or assign directly when it already matches
(`u64`/`i64`, and the float slices). **String-array sequences** need a tiny collector
visitor (`_stringSeq` above) whose `String` appends to the target slice.
**Blob fields** must copy (`append([]byte(nil), v...)`) — `AcceptBytes` aliases the
input buffer.

## Where in the generator

- **`emitObject` (backend.go ~L112).** Stop emitting the pull `unmarshal` loop.
  Instead emit, per type, the `Visitor` methods it needs: `Unsigned`/`Signed` (scalar
  + numeric-array narrowing), `Float32`/`Float64`, `String`/`Bytes`, `*Array`, and
  `BeginSequence`/`EndSequence` for nested scopes. Embed `_visitorBase` in the struct
  so unused callbacks default to no-ops.
- **`DecodeExample` emission.** Replace `m.unmarshal(sofab.NewDecoder(bytes.NewReader
  (data)))` with `sofab.AcceptBytes(data, m)`. Drop the now-unused `bytes`/`io`
  imports from the decode file.
- **Prelude.** Emit `_visitorBase`, the `_narrowU`/`_narrowS` generics, and the
  `_stringSeq` collector once per generated package (guard so multi-message schemas
  emit them a single time).
- **Field/scope mapping.** The existing `emitUnmarshalField`/`unmarshalArray` logic
  ( id → field, scope tracking) maps directly onto the visitor method bodies — reuse
  the same id/scope tables you already compute for the pull switch.

## Generalization / caveats

- **Keep the streaming path available.** `AcceptBytes` is the in-memory fast path;
  the pull `Decoder`/`Accept(reader)` still exists in the corelib for true streaming
  callers. This change only swaps what generated `Decode*` uses.
- **BeginSequence returns the child visitor** — for a nested struct return
  `&m.Field` (which is itself a generated `Visitor`); for a native-array scope the
  parent stays the visitor; for a scalar-array-in-sequence (string array) return a
  collector. Returning `&m.Field` does not allocate (a pointer fits an interface
  without boxing).
- **Array element widening:** the corelib delivers integer arrays widened to 64-bit
  (`[]uint64`/`[]int64`); narrow per the field's declared width. Float arrays arrive
  as `[]float32`/`[]float64` and can be assigned directly.
- **Companion corelib PR:** the Go *encoder* was also a bottleneck (per-byte
  `bufio.Writer`); that fix is a **corelib-go** change (byte-slice buffer), submitted
  separately to `sofa-buffers/corelib-go` — not a generator change.

## Validate

Regenerate `languages/go`, run its bench: `sha256` stays `db362b…`, `speed adv` ~1.0×.
Then remove the `decode-visitor.patch` block from `languages/go/setup.sh` (note it
runs after the existing `bytes`-import fixup — order matters).
