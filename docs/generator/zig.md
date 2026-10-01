# Zig target — `targets.zig`

Emits the generated structs for every message and named type, against
`corelib-zig`.

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits — are documented in the
[generic config](README.md) and apply here unchanged.

## Formatting

Every generated file is `zig fmt` output already: `message.zig` and, under
`emit: project`, the harness, `build.zig` and `build.zig.zon`. A
`zig fmt --check` over a tree that holds generated code passes, so generated
files need no exclusion from such a check and never come back reformatted.
`sofabgen` produces that layout itself and does not run `zig`.

## Type names

Every type is named after its place in the schema: each name along the path in
PascalCase, joined with `_`.

| schema | Zig type |
|---|---|
| message `vehicle_telemetry` | `VehicleTelemetry` |
| `$defs` struct, union, enum or bitfield `point` | `Point` |
| inline struct, union, enum or bitfield of field `a` in message `m` | `M_A` |
| inline element type of the array field `a` in message `m` | `M_A` |
| `$defs` union `shape` used with `default_id` on option `pt` | `Shape__DefaultPt` |

An enum or a bitfield is a namespace of constants: `Color.RED`, `M_Flags.ON`.
Every message also has a private decoder type, `_<Type>__Visitor`, which no
schema name can spell.

A type whose name the generated file already uses for something else gets a
trailing underscore: a message `decoder` is the type `Decoder_`, since every
message declares its own `Decoder`. Those names are `DecodeError`, `Decoder`,
`MAX_SIZE` and `MAX_SIZE_LIMIT`. Everything derived from the type keeps the
unescaped name (`_Decoder__Visitor`).

## Field names

A field's struct field is the field's schema name. A field named like a Zig
keyword is a quoted identifier — the field `error` is `@"error"` — so it keeps
its name. In Zig a struct's fields and declarations share one namespace, so a
field named after a declaration the generated struct carries gets a trailing
underscore instead — the field `encode` is `encode_`. Those declarations are
`serialize` and `isDefault`, and on a message also `encode`, `decode`,
`Decoder`, `decoder`, `MAX_SIZE` and `MAX_SIZE_LIMIT`. Only the field changes:
the wire is keyed by the field id, and the JSON key stays the schema name.

## Unions

A `union` holds exactly one of its options. It is a Zig tagged union
(`union(enum)`) with one field per option, so holding two at once cannot be
written:

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

```zig
pub const M_Shape = union(enum) {
    num: u16,
    name: []const u8,
    pt: M_Shape_Pt,

    pub const num_id: sofab.Id = 0;
    pub const name_id: sofab.Id = 1;
    pub const pt_id: sofab.Id = 2;

    pub const init: M_Shape = .{ .pt = .{} };

    pub fn which(self: *const M_Shape) sofab.Id;
    pub fn numMut(self: *M_Shape) *u16;
    pub fn nameMut(self: *M_Shape) *[]const u8;
    pub fn ptMut(self: *M_Shape) *M_Shape_Pt;
    pub fn serialize(self: *const M_Shape, os: *sofab.OStream) sofab.Error!void;
    pub fn isDefault(self: *const M_Shape) bool;
};
```

A union field is declared `shape: M_Shape = .init`, so a fresh message holds the
union's default.

| operation | Zig |
|---|---|
| which option is held | `x.which()` → the option's id; or `switch (x) { .num => |n| …, … }` |
| option ids | `M_Shape.pt_id` |
| test | `x == .pt` |
| read | `switch (x)`, or `x.pt` once `x == .pt` holds |
| select with a value | `x = .{ .num = 7 }` |
| select at the default and edit in place | `x.ptMut().y = 2` |
| back to the default | `x = .init` |

```zig
var s: M_Shape = .init;           // holds pt at its default: .{ .x = 7, .y = 0 }
s = .{ .num = 7 };               // now num = 7; pt is gone
s.ptMut().y = 2;                 // pt again, from its default: .{ .x = 7, .y = 2 }
if (s == .pt) useIt(s.pt.x);
switch (s) {
    .num => |n| useIt(n),
    .name => |n| useIt(n),
    .pt => |p| useIt(p.x),
}
s = .init;                       // pt at its default again
```

Reading a field that is not the one held is illegal behaviour in Zig (a panic
in `Debug` and `ReleaseSafe`, unchecked in `ReleaseFast`), so read through
`switch` or after a test.

**`<option>Mut()`** selects the option at its own default if another one is
held, and returns a pointer to it. If the option is already held it is returned
as it is, untouched.

**Ownership.** The union holds its option by value, like any struct field: a
slice option (a string, a blob, an array whose storage is a slice) points at
bytes the union does not own — on a decoded message they come from the
allocator passed to `decode`, as every other field's do. Selecting another
option overwrites the one held; nothing is freed. The pointer `<option>Mut()`
returns points into the union and is valid until another option is selected.

**Storage.** An option's type is exactly what a struct member of that type
would be (`[]const u8`, `sofab.FixedArray(T, N)`, a slice, a generated struct
or union). The union is as large as its largest option plus its tag; nothing is
allocated to hold it.

**Names.** The fields are the option names; the accessors are
`<option>Mut` with the option's first letter lower-cased (option `Pt` has
`ptMut()`), and the id constants `<option>_id`. An option named like a Zig
keyword is a quoted identifier (`@"error"`, with `errorMut()` and `error_id`).
An option whose name is one of the declarations listed under
[Field names](#field-names), one of the union's own — `init`, `which` — or
that ends in `_id` or `Mut`, the shape of an id constant or an accessor, gets
a trailing underscore on its field: option `which` is the field `which_`, with
`whichMut()` and `which_id`; an option `x_id` is the field `x_id_` and an
option `xMut` the field `xMut_`, whether or not the union also has an option
`x`. An option's field depends only on its own name, so adding an option never
renames another.

**`$defs` unions** used with different `default_id`s are one type per
`default_id`, named `<Type>__Default<Option>`: `Shape__DefaultPt` and
`Shape__DefaultNum`.

**Defaults.** `init` holds the `default_id` option at that option's own
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

**JSON (project harness).** The harness prints a union as an object with
exactly one member, the held option — `{"pt":{"x":7,"y":2}}`, also when that
is the `default_id` option at its default — and reads the same form. A union
member left out of a message's JSON reads as the union's default.
