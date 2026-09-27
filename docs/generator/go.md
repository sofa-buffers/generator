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
type MShape struct {
	// unexported: the option held, and one slot per option
}

const (
	MShapeNumID  sofab.ID = 0
	MShapeNameID sofab.ID = 1
	MShapePtID   sofab.ID = 2
)

func (m *MShape) Which() sofab.ID
func (m *MShape) HasNum() bool
func (m *MShape) Num() uint16
func (m *MShape) SetNum(v uint16)
func (m *MShape) HasName() bool
func (m *MShape) Name() string
func (m *MShape) SetName(v string)
func (m *MShape) HasPt() bool
func (m *MShape) Pt() MShapePt
func (m *MShape) SetPt(v MShapePt)
func (m *MShape) MutPt() *MShapePt
func (m *MShape) Clear()
func (m MShape) MarshalJSON() ([]byte, error)
func (m *MShape) UnmarshalJSON(b []byte) error
```

| operation | Go |
|---|---|
| which option is held | `x.Which()` → the option's id |
| option ids | `MShapePtID` (package-level `<Type><Option>ID` constants) |
| test | `x.HasPt()` |
| read | `x.Pt()` |
| select with a value | `x.SetNum(7)` |
| select at the default and edit in place | `x.MutPt().Y = 2` |
| back to the default | `x.Clear()` |

```go
m := message.NewM()      // m.Shape holds pt at its default: {X: 7, Y: 0}
m.Shape.SetNum(7)        // now num = 7; pt is gone
m.Shape.MutPt().Y = 2    // pt again, from its default: {X: 7, Y: 2}
if m.Shape.HasPt() {
	useIt(m.Shape.Pt().X)
}
switch m.Shape.Which() {
case message.MShapeNumID:
	useIt(m.Shape.Num())
case message.MShapePtID:
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
`var s MShape` holds it at the zero value instead; `s.Clear()` puts it at the
declared default. A union in a message from `New<Message>`, in a decoded
message, and every element of a decoded array of unions is already there.

**Names.** The accessors are the option name in Go casing: `<Option>`,
`Set<Option>`, `Has<Option>`, `Mut<Option>`. A getter whose name would be one of
the union's own methods — the decode callbacks (`String`, `Bytes`, `Unsigned`,
…), `Which`, `Clear`, `Serialize`, `MarshalJSON`, `UnmarshalJSON` — gets a
trailing underscore: an option named `string` reads as `String_()` and keeps
`SetString`/`HasString`. Two options that would produce the same method (`foo`
and `set_foo` both give `SetFoo`), an option whose `Set…` would shadow a
promoted method (`string_check` → `SetStringCheck`), and an id constant that
lands on another package-level name fail generation, naming both.

**`$defs` unions** used with different `default_id`s are one type per
`default_id`, named after `<Name>_default_<option>`: `UnionShapeDefaultPt` and
`UnionShapeDefaultNum`.

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
