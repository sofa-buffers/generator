# C# target — `targets.csharp`

Emits one class per message and named type, against `corelib-cs`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `namespace` | string | `Message` | The `namespace <name>` wrapping the generated classes. |

The generic options apply here too; see the [generic config](README.md).

## `namespace`

Wraps every generated type. Unlike Java's and Kotlin's `package`, it has no
effect on file layout — C# does not tie namespaces to directories, so the output
stays a single `Message.cs` in the output directory whichever namespace you
choose.

`generic.namespace` sets it for every target that has one; this key overrides
that for C# alone. Left unset, this target's own default (`Message`) applies
rather than a generic one — each language keeps its own idiomatic capitalisation.

## Field names

A field's C# field is the field's schema name. A field named like a C# keyword
is a verbatim identifier — the field `class` is `@class` — so it keeps its
name. In C# a class's fields, methods, properties and nested types share one
namespace, so a field whose name the generated class already uses gets a
trailing underscore instead — the field `Encode` is `Encode_`. Those names are:

- the members every generated class declares: `Serialize`, `IsDefault`, and on
  a message also `Reset`, `Encode`, `EncodeTo`, `Decode`, `TryDecode`, the
  nested `Decoder` class, `MaxSize` and `MaxSizeLimit`;
- the members every class inherits from `object`: `Equals`, `GetHashCode`,
  `GetType`, `ToString`, `MemberwiseClone`, `Finalize`, `ReferenceEquals`;
- the names the class body uses in front of a dot — `Array`, `DecodeStatus`,
  `System`. Inside the class a field of such a name would be found first, and a
  call like `System.Array.Empty<byte>()` would no longer compile.

Only the field changes: the wire is keyed by the field id, and a renamed field
carries `[JsonPropertyName]` with the schema name, so its JSON key does not
change either. Two fields that end up with the same field — `Encode` and
`Encode_` — fail generation, naming both, and so does a field named like its
own class (a field `M` in the message `m`), which C# does not allow. Enum
constants and bitfield flags are PascalCase members too: two that give the same
one (`a_b` and `aB` are both `AB`) fail generation as well. The list
lives in `generators/csharp/reserved.go`.

## Unions

A `union` holds exactly one of its options. It is a class of its own whose
option slots are private, so an option is only reached through the members
below and two options cannot be set side by side:

```yaml
shape:
  id: 3
  type: union
  default_id: 2
  oneof:
    num:  { id: 0, type: u16, default: 5 }
    name: { id: 1, type: string, maxlen: 16 }
    pt:   { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
    tags: { id: 3, type: array, items: { type: string, count: 4, maxlen: 8 } }
```

```csharp
public sealed class MShape {
    public const int NumId = 0;
    public const int NameId = 1;
    public const int PtId = 2;
    public const int TagsId = 3;

    public int Which { get; }

    public ushort Num { get; set; }
    public bool HasNum { get; }

    public string Name { get; set; }
    public bool HasName { get; }

    public MShapePt Pt { get; set; }
    public bool HasPt { get; }
    public MShapePt MutablePt();

    public List<string> Tags { get; set; }
    public bool HasTags { get; }
    public List<string> MutableTags();

    public void Clear();
}
```

| operation | C# |
|---|---|
| which option is held | `x.Which` → the option's id |
| option ids | `MShape.PtId` (`<Option>Id` constants) |
| test | `x.HasPt` |
| read | `x.Pt` |
| select with a value | `x.Num = 7` |
| select at the default and edit in place | `x.MutablePt().y = 2` |
| back to the default | `x.Clear()` |

```csharp
var m = new M();            // m.shape holds pt at its default: {x: 7, y: 0}
m.shape.Num = 7;            // now num = 7; pt is no longer held
m.shape.MutablePt().y = 2;  // pt again, from its default: {x: 7, y: 2}
if (m.shape.HasPt) {
    Use(m.shape.Pt.x);
}
switch (m.shape.Which) {
case MShape.NumId: Use(m.shape.Num); break;
case MShape.PtId:  Use(m.shape.Pt.y); break;
}
m.shape.Clear();            // pt at its default again
```

Each option is held in a field of its own type — `num` is a `ushort`, never
boxed — so reading or selecting a numeric option allocates nothing.

**Reading** an option that is not held returns that option's default — a new
struct or union at its default, an empty `List`, a zero-length array for a blob
or a numeric array — and changes nothing; it does not select the option.

**Assigning** an option's property selects that option with the value assigned;
the option held before is no longer held. **`Mutable<Option>()`** (struct and
union options, and arrays held in a `List`: strings, blobs, booleans, structs,
unions, nested arrays) selects the option at its own default if another one is
held, and returns it. If the option is already held it is returned as it is,
untouched. A numeric array option (`ushort[]`, `float[]`, …) has no
`Mutable<Option>()`: select it by assigning an array, and change its elements
through the array the property returns while it is held.

**Ownership.** Assigning an option keeps the object it is given, as assigning a
field does: after `x.Pt = p`, `x.Pt` returns that same `p` while `pt` is held.
Selecting a struct, union or list option that is not held — through
`Mutable<Option>()`, by decoding into the union, or by `Clear()` for the
`default_id` option — always starts from a new object at the option's default;
an object obtained earlier is never reset behind your back, it is merely no
longer held.

**Names.** The members are the option name in PascalCase: the property
`<Option>`, `Has<Option>`, `Mutable<Option>()` and the constant `<Option>Id`. An
option whose name would land on a name reserved for a field (see [Field
names](#field-names)), on one of the union's own members (`Which`, `Clear`) or
on the union type's own name gets a trailing underscore: an option `which` is
`Which_`, with `HasWhich_` and `Which_Id`. Two options that would produce the
same member (`foo_bar` and `fooBar` both give `FooBar`; `a`'s `AId` and an
option named `a_id`; `x`'s `HasX` and an option named `has_x`) fail generation,
naming both.

**`$defs` unions** used with different `default_id`s are one class per
`default_id`, named after `<Name>_default_<option>`: `UnionShapeDefaultPt` and
`UnionShapeDefaultNum`.

**Defaults.** A new union holds the `default_id` option at that option's own
default; an omitted `default_id` means the option with the lowest id. Each
element of an array of unions starts the same way — including an element a
decoded array skips.

**Encode.** Only the held option is written. If it is the `default_id` option
at its default, the union is at its default and is left out. Any other held
option is written **even at its own default** — `0`, an empty string, an empty
blob or array, or an empty frame for a struct option, a union option, or an
array whose elements are strings, blobs, structs, unions or arrays — because
the receiver's new union holds `default_id`, and leaving it out would read
back as that.

**Decode.** The option received last wins. A different option replaces the
held one and starts from its own default; the held option received again
continues where it was (a struct or union option merges, anything else is
replaced). A field whose wire type does not match its option, and an unknown
id, change nothing.

**JSON** (the project harness). A union is an object with exactly one member,
the held option — `{"pt":{"x":7,"y":2}}`, also when that is the `default_id`
option at its default. The harness registers a `System.Text.Json` converter per
union type for this; the generated `Message.cs` itself has no JSON code. A union
member left out of a message's JSON reads as the union's default.
