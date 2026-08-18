# Go target — `targets.go`

Target-specific options, accepted under `targets.go`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `package` | string | `message` | The `package <name>` clause of the generated `.go` files. |
| `module_path` | string | `example.com/generated` | The module path written to the generated `go.mod` (project mode). |
| `go_version` | string | `1.21` | The `go <version>` directive written to the generated `go.mod` (project mode). |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size — see the [shared config reference](README.md) for both of its roles. Go emits it as `<Msg>MaxSizeLimit` for a message the schema cannot bound; a bounded message keeps its computed `<Msg>MaxSize`. |

```yaml
targets:
  go:
    package: messages
    module_path: github.com/me/myproj
    go_version: "1.22"
```

## Buffer ownership — generated code allocates, the corelib never does

A corelib owns no storage and grows none (CORELIB_PLAN §5.1): the buffer an
encode writes into is the caller's, and generated code **is** a caller — which
is why `Encode` allocates and `sofab.NewEncoder` (the compatibility constructor
that allocates a window of its own and reallocates it as the message grows) does
not appear in generated code at all.

The buffer's size has to come from somewhere, and the schema decides which of two
shapes applies:

- **Bounded** — every field carries a `count`/`maxlen`, so the message has a
  worst case. It is emitted as `const <Msg>MaxSize` (the shared walk in
  `internal/ir/wiresize.go`, the same number every target emits) and `Encode`
  hands the corelib exactly that many bytes via `sofab.NewEncoderBuffer`. There
  is no sink, so `MinOutputBuffer` does not apply and a three-byte message uses a
  three-byte buffer.
- **Unbounded** — some field has no bound, so no worst case exists and
  `<Msg>MaxSizeLimit` is the configured `max_message_size` **ceiling**, not a
  size the message cannot exceed. Sizing the buffer from it would refuse a larger
  message the caller legitimately built, so `Encode` writes through a fixed
  512-byte scratch and appends each flush into the result
  (`sofab.NewEncoderSink`). The ceiling never bounds a value.

Both constructors are fallible and both errors reach the caller. So does the one
new behaviour: on a bounded message, a value filled **past its own schema bound**
no longer fits the exactly-sized buffer and comes back as `sofab.ErrBufferFull`
with no bytes written. That message was previously encoded and emitted — bytes
every conformant receiver rejects as `INVALID` anyway (MESSAGE_SPEC §7.1) — and
§5.1 forbids returning partial output as if it were complete.

The bounded arm returns a slice of its worst-case buffer rather than an exact
copy, so a short message retains that array until it is dropped: one allocation
per encode instead of two. There is no cached or pooled buffer — a package-level
scratch would make `Encode` non-reentrant from a `Serialize` on the same
goroutine, which is worse than the allocation for the concurrency Go callers
expect.

The size of that allocation is the schema's, not the value's: a message declaring
`array<u64, count: 10000>` has `MaxSize = 200007` and `Encode` allocates all of it
per call even to emit ten bytes. That is the declared bound being taken
seriously — the same buffer a `c`/`rust no_std` target would reserve statically —
so a schema whose bounds are aspirational rather than real is worth tightening.
A caller that wants the message-sized allocation instead can drive `EncodeTo`
with its own `bytes.Buffer`, or `Serialize` with an encoder it constructed itself.

## Streaming — `EncodeTo(io.Writer)` and `Decode<Msg>From(io.Reader)`

```go
func (m *Msg) Serialize(e *sofab.Encoder)      // fields only; a nested message
                                               // goes into a frame already open
func (m *Msg) EncodeTo(w io.Writer) error      // serialise + flush the tail
func (m *Msg) Encode() ([]byte, error)         // into a buffer this call owns
```

`Serialize` is the streaming writer, exported so a caller outside the generated
package can supply the stream; `EncodeTo` is the entry point that adds the flush.
It uses the same 512-byte caller-owned scratch as the unbounded `Encode`, with
`w` as the drain, so a message never has to exist as one contiguous `[]byte`:
what bounds memory is the scratch, not the message. `io.Writer.Write` may not
retain what it is handed, which makes `w` a *copying* sink in §5.1's terms — it
returns without taking the buffer, so no `SetBuffer` handover is needed.

Decode is symmetric, through a second entry point rather than a `feed()`:

```go
func Decode<Msg>(data []byte) (*Msg, error)      // AcceptBytes over a buffer you hold
func Decode<Msg>From(r io.Reader) (*Msg, error)  // AcceptStream, reader-driven
```

`AcceptBytes` takes a `[]byte`, so it requires the whole wire image resident **by
construction** — and `Decoder.Accept` is no better, because it slurps the reader
into one contiguous buffer before dispatching. `AcceptStream` (corelib-go#71/#72)
drives the pull primitives directly and dispatches each field as the reader
delivers it, so peak memory is the largest single field rather than the message.
That is what CORELIB_PLAN §5.6 asks for, and generator#312 is where it landed.

**Both entry points share one visitor.** `AcceptStream` is event-equivalent to
`AcceptBytes` — same callbacks, same `HeaderVisitor` hooks, same
INVALID/INCOMPLETE verdicts at every byte boundary — so the generated `Msg` is
unchanged and only the thing feeding it differs. One consequence worth knowing:
the blob arm still copies (`append([]byte(nil), v...)`), which `AcceptStream`
does not need since it hands over freshly read buffers. The visitor cannot tell
which entry point is driving it — and the ownership rule below requires the copy
on the `AcceptBytes` path anyway — so the copy stays.

**A decoded message owns its bytes.** `AcceptBytes` hands a payload over as a
window into the buffer you passed in, but every generated destination copies —
the blob arm explicitly, a `string` through Go's copying `[]byte`→`string`
conversion, and a native array by taking the fresh slice the cursor built. So the
message outlives `data`, and `data` may be reused or overwritten the moment
`Decode<Msg>` returns. This is deliberate rather than incidental: a borrowing
destination would be faster and is not offered, because a message whose lifetime
is silently tied to a decode buffer is the wrong default. It is pinned by a test
that scribbles over the input buffer after decoding and re-encodes
(`TestDecodedMessageOwnsItsBytes`).

**Go has no `feed(chunk)`, and that is still a corelib property.** `corelib-go`
streams **pull**-shaped; a resumable push decoder cannot be synthesised over that
without inverting control. `Decode<Msg>From` gives the memory bound §5.6 is
about without needing one — the caller hands over a reader instead of pushing
chunks.

Note for issue #239: `Serialize` is now an exported member of every generated
message type, so it joins the reserved-name set a schema field must not collide
with. `Decode<Msg>From` is a package-level function keyed on the message name,
so it collides only where `Decode<Msg>` already would. `<Msg>MaxSize` (and
`<Msg>MaxSizeLimit`) are package-level constants keyed the same way, so two
messages named `foo` and `fooMaxSize` in one schema would collide there.

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `MaxDyn*` package constants,
passed into every `Decode<Msg>` via `sofab.WithMaxArrayCount`,
`WithMaxStringLen` and `WithMaxBlobLen`. A violation returns
`sofab.ErrLimitExceeded`.

The corelib enforces them globally per decode rather than per field, so each cap
is raised to the largest schema bound of its kind — schema-bounded fields stay
governed by their own bound. A key whose kind has no unbounded field emits
nothing.

## Arrays — `count` is a capacity

Every array field maps to a Go slice, and the slice's length is the array's
length. A schema `count: N` is a **capacity**, not a length: it never reaches the
wire, it bounds the array (an element count or element id past `N` fails the
decode as invalid), and it lets fixed-storage targets pre-size — but it never
adds elements.

The consequences you can observe from Go:

- `New<Msg>()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`).
- Encode writes **every** element the slice holds. `[]uint32{1, 2, 0, 0}` and
  `[]uint32{1, 2}` are different values with different bytes.
- Decode yields exactly the elements the wire carried: `len()` after a round trip
  equals `len()` before it, for both the compact scalar form and the wrapper form.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `[]uint32{0, 0, 0, 0}` is a
  four-element value and stays on the wire.

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse**: an element equal to the element default is dropped and
leaves an id gap, which decode restores from that same default. The **last**
element is always written — as its value, or as an empty frame for a
struct/union/nested element — because its presence is what carries the length.
So `[]string{"a", ""}`, `[]string{"a"}` and `[]string{}` are three distinct values
that encode and decode distinctly.

## The `count` bound is keyed on the wire array kind

A bounded native array (unsigned/signed/fp32/fp64 elements) gets an arm in the
generated `ArrayBegin` hook of `sofab.HeaderVisitor`, so an over-count array is
rejected at the **header**, before the corelib's truncation check — that is what
makes `INVALID` dominate `INCOMPLETE` when the same field is also truncated
(MESSAGE_SPEC §5.2). The hook is keyed by field id:

```go
func (m *M) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {
	switch id {
	case 0: // declared array<fp32, count 5>
		if kind == sofab.ArrayFp32 && count > 5 {
			return sofab.ErrInvalidMsg
		}
	case 1: // declared array<u32, count 4>
		if kind == sofab.ArrayUnsigned && count > 4 {
			return sofab.ErrInvalidMsg
		}
	}
	return nil
}
```

`kind` is the element kind the **wire header** declares, and the compare sits
behind it. The reason is MESSAGE_SPEC §7.3: `ArrayBegin` fires for *any* array
header arriving at that field id — the corelib resolves the wire kind but cannot
know the declared one, which is schema knowledge only the generated code has —
and an array whose element kind contradicts the declaration is **skipped**. It
was never this field's value, so its element count is not this field's count and
the schema capacity must not be measured against it. Un-gated, an 8-element
`fp64` array landing on a declared `array<fp32, count 5>` failed the whole
message as invalid instead of being skipped (Crucible F-0042, generator#259).

`sofab.ArrayKind` distinguishes **four** kinds, not three: `ArrayUnsigned`,
`ArraySigned`, `ArrayFp32`, `ArrayFp64`. fp32 and fp64 share one wire type and
are told apart by the `fixlen_word`, so the corelib reads that word *before*
calling the hook (CORELIB_PLAN §4.8) and the kind handed in is never a guess.
Two intended consequences: a message that ends between the count word and the
`fixlen_word` is `INCOMPLETE` (no bound can be judged yet), and a declared fp32
array is bounded under `ArrayFp32` alone — never under its fp64 sibling. Enum
arrays are keyed as `ArraySigned`, boolean and bitfield arrays as
`ArrayUnsigned`, matching the wire type each rides.

The same gating shape applies to `FixlenHeader`, where the `maxlen` compare sits
behind the declared fixlen `subtype` (generator#224).

A wrapper-sequence array (string/blob/struct/union/nested elements) descends
through `BeginSequence`, so the bound reaches its **collector** rather than the
message — but the collector needs the header hook for the same reason the message
does. `_strSeq`/`_bytesSeq` therefore implement `FixlenHeader` themselves, and an
over-index element (`id ≥ count`) or an over-`maxlen` element is rejected at the
word that carries it instead of once the payload arrives (generator#267/#277).
The `cap` the collector already held is what the guard compares against; the
payload-side check stays as defense.

`sofab.HeaderVisitor` is **all-or-nothing**: it declares `ArrayBegin` *and*
`FixlenHeader`, and the cursor reaches both through a single `v.(HeaderVisitor)`
type assertion. A collector that implements only the one it needs leaves that
assertion failing and silently disables the other, so the two are always emitted
together — the one with no arms is an empty switch. corelib-go fires both hooks
identically on `Accept` and on the reader-driven `AcceptStream` (corelib-go#71/#72
pins the equivalence), so these verdicts carry over unchanged when generated
decode moves to the streaming entry point (generator#312).

### An array element's declared width (`ArrayElemBound`, generator#267)

One position deeper, and the same shape a third time. `UnsignedArray`/
`SignedArray` hand over the whole slice, so the emitted

```go
for _, _x := range v {
    if _x < -128 || _x > 127 { return sofab.ErrInvalidMsg }
}
```

is exact for an array that **arrives** and never runs for one that does not —
while §7.1 makes an out-of-width element invalid and §5.2 makes that INVALID
outrank the truncation behind it. So the bound goes to the decoder:

```go
func (m *M) ArrayElemBound(id sofab.ID, kind sofab.ArrayKind) (int64, int64, bool) {
	switch id {
	case 1:
		if kind == sofab.ArraySigned {
			return -128, 127, true
		}
	}
	return 0, 0, false
}
```

Gated on `kind` for the reason `ArrayBegin` is (§7.3), and emitted for every
narrowed integer element — a **dynamic** array carries it too, since width is a
property of the element *type*, not of the array *length*. `u64`/`i64`, enums,
bitfields and `bool` declare nothing: their range is already the callback
parameter's.

`sofab.ElemBoundVisitor` is a **separate** interface, not a third method on
`HeaderVisitor`, precisely because of the all-or-nothing rule above — adding one
there would leave the assertion failing for every visitor generated before it
existed, silently disabling the header rejects. So it is emitted on its own
condition: a type may declare an element width without declaring any
count/`maxlen`, and gets `ArrayElemBound` without `ArrayBegin` coming along.

The post-read scan **stays**, unreachable but load-bearing for a consumer built
against a corelib that does not know the interface — the same reasoning as the
payload-side `maxlen` guards above.

## Struct field order (widest-first)

Generated struct fields are declared **widest-first** (8→4→2→1-byte alignment;
strings, slices and nested types rank as 8), not in schema order — Go lays
structs out in declaration order, so this cuts padding between fields. Fields
of equal alignment keep their schema order. This affects **declaration order
only** — encode walks the schema/field-id order, so the wire bytes are
byte-identical to every other target. Construct values with keyed struct
literals (`Point{X: 1, Y: 2}`), not positionally.

## Benchmark row

Row `go` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **toggle (symbol `main.run_<workload>`)** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

The emitted harness runs an uncollected `warmup_<workload>` before the measured
`run_<workload>`, and that is load-bearing for this target specifically. `toggle`
collects the *first* op, and Go's runtime builds interface tables and resolves
type/name offsets lazily on first use — one-time costs that land on the measured
op and that grow with the number of distinct types the generated code converts to
interfaces. Unwarmed they were 32% of decode and 22% of encode here, enough that
adding the generics visitor read as a 44% decode regression while the warmed
number had actually improved. If you change how many itabs the generated code
needs, this is the row to distrust first. See `emitBench` in `project.go`.

## Strict UTF-8 — validated at the destination (issues #85, #257)

corelib-go's visitor path deliberately does **not** UTF-8-validate: its cursor
cannot tell a field this visitor binds from one it is skipping, and a skipped
payload must never be inspected (CORELIB_PLAN §6.4). So the check belongs to the
destination, and generated code makes it — `sofab.UTF8Valid(bytes)` in every arm
that stores a `string`: the scalar fields, and the `_strSeq` collector for a
wrapper-array element. A skipped field reaches no arm, so it is never inspected,
which is the whole point.

The primitive carries its own **compile-time** gate (a footprint build folds it to
a constant `true`), so generated code calls it unconditionally and never has to be
regenerated for a different corelib build. Invalid bytes at a materialized position
are `sofab.ErrInvalidMsg`, the same channel as the schema-bound rejects. `blob` is
never validated — bytes carry no encoding. Encode-side strictness stays
corelib-side (`Encoder.WriteString`).

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a wire value outside its
declared range with `sofab.ErrInvalidMsg`. The width is a normative bound, not a
storage hint (MESSAGE_SPEC §1/§7.1) — the `uint8(v)` conversion that follows IS
the mask §7.1 forbids, so the guard has to precede it:

```go
func (m *W) Unsigned(id sofab.ID, v uint64) error {
	switch id {
	case 0:
		if v > 255 {
			return sofab.ErrInvalidMsg
		}
		m.AU8 = uint8(v)
	case 3:
		m.DU64 = uint64(v) // u64: range is the parameter's own, no guard
	}
	return nil
}
```

Native array ELEMENTS carry the same bound. The corelib hands the whole array
over as `[]uint64`/`[]int64` and `sofab.Narrow*` converts element-wise
afterwards, so the raw values are still visible and one scan precedes the
narrowing — a single out-of-range element makes the message INVALID.

No negative-value term is needed on the unsigned side: `Unsigned` delivers a
`uint64`, so the comparison is already unsigned.
