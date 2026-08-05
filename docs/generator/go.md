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

```yaml
targets:
  go:
    package: messages
    module_path: github.com/me/myproj
    go_version: "1.22"
```

## Streaming encode — `EncodeTo(io.Writer)`

`corelib-go`'s `Encoder` targets an `io.Writer` and drains its internal buffer
into it as that fills, so a message never has to exist as one contiguous
`[]byte`. That was true all along and unreachable all along: the generated
writer was an **unexported** `marshal`, so no caller outside the generated
package could supply a writer. It is now exported as `Serialize`, with
`EncodeTo` as the entry point that adds the flush:

```go
func (m *Msg) Serialize(e *sofab.Encoder)      // fields only; a nested message
                                               // goes into a frame already open
func (m *Msg) EncodeTo(w io.Writer) error      // serialise + flush the tail
func (m *Msg) Encode() ([]byte, error)         // EncodeTo into a bytes.Buffer
```

`Encode` is `EncodeTo` with a `bytes.Buffer`, so the two cannot drift.

**Decode has no `feed()`, and that is a corelib property, not an oversight.**
`corelib-go` streams **pull**-shaped: `Decoder.Next` walks an `io.Reader` field
by field, never materialising the message. That is real, memory-bounded
streaming — but it is not a resumable push decoder, and one cannot be
synthesised over it without inverting control. The generated `Decode<Msg>` uses
the zero-copy `AcceptBytes` cursor, which needs the whole message contiguous. A
`feed(chunk)` here waits on a resumable decoder in `corelib-go`.

Note for issue #239: `Serialize` is now an exported member of every generated
message type, so it joins the reserved-name set a schema field must not collide
with.

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
destination, and generated code makes it — `sofab.Utf8Valid(bytes)` in every arm
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
