# Go target — `targets.go`

Emits the generated types for every message and named type, against
`corelib-go`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `package` | string | `message` | The `package <name>` clause of the generated files. |
| `module_path` | string | `example.com/generated` | The module path written to `go.mod`. Project mode only. |
| `go_version` | string | `1.21` | The `go <version>` directive written to `go.mod`. Project mode only. |

The generic options apply here too; see the [generic config](README.md).

## `package`

Sets the package clause on every generated `.go` file. It does not affect the
output directory — that is `output_dir` / `--out`, and Go does not require the
two to match.

## `module_path`

Only reaches the output under `emit: project`, where it becomes the `module`
line of the generated `go.mod`. It is what an importing module writes in its own
`require`, so set it to the path you will actually publish the generated code
under; the default is a placeholder that compiles but is not resolvable.

With `emit: sources` there is no `go.mod` and the key does nothing.

## `go_version`

Only reaches the output under `emit: project`, where it becomes the `go` line of
the generated `go.mod`. Raise it if the surrounding build needs a newer language
version; the generated code itself does not require one.

## Formatting

Generated Go is `gofmt` output: `gofmt -l` over it prints nothing, so generated
files need no exclusion from a gofmt gate and never come back reformatted.
The formatting is done in-process (`go/format`), so no Go toolchain is needed
at generation time; source that library cannot parse is a generator bug, and
generation then fails with an error naming the file instead of writing it.

## Field names

A field's Go field is the schema name in Go casing: underscores are folded into
camel case and the first letter is upper-cased, so `max_speed` is `MaxSpeed`.
Go has no way to escape a reserved name, so a field whose Go name the generated
type already uses gets a trailing underscore — the field `encode` is `Encode_`,
and `string` is `String_`. Those names are:

- the `sofab.Visitor` decode callbacks every generated type implements:
  `Unsigned`, `Signed`, `Float32`, `Float64`, `FixlenBegin`, `String`, `Bytes`,
  `ArrayBegin`, `ArrayUnsigned`, `ArraySigned`, `ArrayFloat32`, `ArrayFloat64`,
  `ArrayEnd`, `BeginSequence`, `EndSequence`, and the embedded `VisitorBase`;
- the embedded `StringCheck` of a type with a string field, and what it
  provides: `UTF8Valid`, `SetStringCheck`;
- the methods every generated type declares: `Serialize`, and on a message also
  `Encode`, `EncodeTo`.

Only the Go field changes: the wire is keyed by the field id, and the `json` tag
keeps the schema name. The list lives in `generators/golang/reserved.go`.

## Type, function and file names

Every schema the validator accepts generates, whatever its names; none is
refused because two generated names would collide. All generated types share
one package, so each name is built so that no other can spell it:

| what | Go name | example |
|---|---|---|
| message, `$defs` type | the name in Go casing | message `vehicle_state` is `VehicleState`, `$defs` struct `point` is `Point` |
| inline type | its path (the message, then each field or option leading to it) in Go casing, joined with `_` | struct field `pos` of message `m` is `M_Pos`; the element struct of an array field `pts` is `M_Pts` |
| a message's functions and constants | the message type, `__`, the role | `M__New()`, `M__Decode(b)`, `M__DecodeFrom(r)`, `M__MaxSize`, `M__MaxSizeLimit`, `M__MaxDepth` |
| enum constant, bitfield flag | the type, `_`, the name | `Color_Red`, `Flags_On` |
| union option id | the option's path, `__ID` | `M_Shape_Pt__ID` |
| `$defs` union used with several `default_id`s | the union, `__Default`, the option | `Shape__DefaultPt` |
| message file | the message name in lower case, underscores dropped | message `vehicle_state` is `vehiclestate.go` |

A type that would be spelled like one of the package's own exported constants
(`MaxDynArrayCount`, `MaxDynStringLen`, `MaxDynBlobLen`) gets a trailing
underscore: a message `max_dyn_string_len` is `MaxDynStringLen_`. The names
derived from it keep the plain spelling (`MaxDynStringLen__New`).

The named types live in `sofab_types.go` and the package-wide decode support in
`sofab_visitor.go`. A message file never contains an underscore, so it can
neither take one of those names nor end in `_test` or a `_<GOOS>`/`_<GOARCH>`
suffix that would make the Go tool skip it.

## Unions

A `union` holds exactly one of its options. It is a struct whose option slots
are unexported, so an option is only reached through the methods below and two
options cannot be set side by side:

```yaml
shape:
  id: 3
  type: union
  default_id: 2
  oneof:
    num:  { id: 0, type: u16, default: 5 }
    name: { id: 1, type: string, maxlen: 16 }
    pt:   { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
```

```go
type M_Shape struct {
	// unexported: the option held, and one slot per option
}

const (
	M_Shape_Num__ID  sofab.ID = 0
	M_Shape_Name__ID sofab.ID = 1
	M_Shape_Pt__ID   sofab.ID = 2
)

func (m *M_Shape) Which() sofab.ID
func (m *M_Shape) HasNum() bool
func (m *M_Shape) Num() uint16
func (m *M_Shape) SetNum(v uint16)
func (m *M_Shape) HasName() bool
func (m *M_Shape) Name() string
func (m *M_Shape) SetName(v string)
func (m *M_Shape) HasPt() bool
func (m *M_Shape) Pt() M_Shape_Pt
func (m *M_Shape) SetPt(v M_Shape_Pt)
func (m *M_Shape) MutPt() *M_Shape_Pt
func (m *M_Shape) Clear()
func (m M_Shape) MarshalJSON() ([]byte, error)
func (m *M_Shape) UnmarshalJSON(b []byte) error
```

| operation | Go |
|---|---|
| which option is held | `x.Which()` → the option's id |
| option ids | `M_Shape_Pt__ID` (package-level constants: the option's path, then `__ID`) |
| test | `x.HasPt()` |
| read | `x.Pt()` |
| select with a value | `x.SetNum(7)` |
| select at the default and edit in place | `x.MutPt().Y = 2` |
| back to the default | `x.Clear()` |

```go
m := message.M__New()    // m.Shape holds pt at its default: {X: 7, Y: 0}
m.Shape.SetNum(7)        // now num = 7; pt is gone
m.Shape.MutPt().Y = 2    // pt again, from its default: {X: 7, Y: 2}
if m.Shape.HasPt() {
	useIt(m.Shape.Pt().X)
}
switch m.Shape.Which() {
case message.M_Shape_Num__ID:
	useIt(m.Shape.Num())
case message.M_Shape_Pt__ID:
	useIt(m.Shape.Pt().Y)
}
m.Shape.Clear()          // pt at its default again
```

**Reading** an option that is not held returns that option's default — a fresh
struct or union at its default, an empty (`nil`) slice for a blob or an array —
and changes nothing; it does not select the option.

**`Set<Option>(v)`** selects the option with the value `v`; the option held
before is discarded. **`Mut<Option>()`** (struct, union and array options)
selects the option at its own default if another one is held, and returns a
pointer to it. If the option is already held it is returned as it is,
untouched.

**Ownership.** Options are held by value, like struct fields: `Set<Option>(v)`
copies `v` into the union and a getter returns a copy, so a struct option read
with `Pt()` and then changed does not change the union — `MutPt()` is what
edits it in place. A slice option (a blob, an array) shares its backing array
the way assigning a slice field does. The pointer `Mut<Option>()` returns points
into the union: it stays valid, but once another option has been selected and
this one is selected again, the slot it points at starts from its default.

**Zero value.** The zero value of a union holds its `default_id` option. Where
that option declares a default other than Go's zero value (here `pt.x = 7`), a
`var s M_Shape` holds it at the zero value instead; `s.Clear()` puts it at the
declared default. A union in a message from `<Message>__New`, in a decoded
message, and every element of a decoded array of unions is already there.

**Names.** The accessors are the option name in Go casing: `<Option>`,
`Set<Option>`, `Has<Option>`, `Mut<Option>`. A getter whose name is reserved for
a field (see [Field names](#field-names)) or is one of the union's own methods
(`Which`, `Clear`, `MarshalJSON`, `UnmarshalJSON`) gets a trailing underscore:
an option named `string` reads as `String_()` and keeps
`SetString`/`HasString`. So does a getter that reads like another option's
accessor, `Set`, `Has` or `Mut` followed by an upper-case letter: beside an
option `foo`, the option `set_foo` reads as `SetFoo_()` and is set with
`SetSetFoo`. An accessor that would hide a method the union already has gets an
underscore after its prefix: the setter of an option `string_check` is
`Set_StringCheck`, since `SetStringCheck` carries the decode's UTF-8 policy.

**`$defs` unions** used with different `default_id`s are one type per
`default_id`, named `<Name>__Default<Option>`: `Shape__DefaultPt` and
`Shape__DefaultNum`. They share one set of option id constants, `Shape_Pt__ID`
and `Shape_Num__ID`.

**Defaults.** A fresh union holds the `default_id` option at that option's own
default; an omitted `default_id` means the option with the lowest id. Each
element of an array of unions starts the same way — including an element a
decoded array skips.

**Encode.** Only the held option is written. If it is the `default_id` option
at its default, the union is at its default and is left out. Any other held
option is written **even at its own default** — `0`, an empty string, an empty
blob or array, or an empty frame for a struct option, a union option, or an
array whose elements are strings, blobs, structs, unions or arrays — because
the receiver's fresh union holds `default_id`, and leaving it out would read
back as that.

**Decode.** The option received last wins. A different option replaces the
held one and starts from its own default; the held option received again
continues where it was (a struct or union option merges, anything else is
replaced). A field whose wire type does not match its option, and an unknown
id, change nothing.

**JSON.** The generated `MarshalJSON` and `UnmarshalJSON` make
`encoding/json` render a union as an object with exactly one member, the held
option — `{"pt":{"x":7,"y":2}}`, also when that is the `default_id` option at
its default — and read the same form; any other member count is an error. A
union member left out of a message's JSON reads as the union's default.
