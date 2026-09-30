# Kotlin target — `targets.kotlin`

Emits one class per message and named type, against `corelib-kotlin-mp`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `package` | string | `message` | The `package <name>` declaration, and the source-directory layout. |

The generic options apply here too; see the [generic config](README.md).

## `package`

Sets the `package` declaration in every generated file, and also decides **where
those files are written** — the output is laid out under
`src/main/kotlin/<package as directories>/`:

| `package` | output path |
|---|---|
| `message` (default) | `src/main/kotlin/message/Telemetry.kt` |
| `com.acme.msg` | `src/main/kotlin/com/acme/msg/Telemetry.kt` |

This holds in both `emit` modes — `sources` emits the same tree without the
Gradle build. Changing the package changes the paths, so point `output_dir` at
the root of a source tree, not at the package directory itself.

## Field names

A field's property is the field's schema name. A field named like a Kotlin hard
keyword is escaped with backticks — the field `class` is `` `class` `` — so it
keeps its name. A field whose name the generated class already uses for another
declaration gets a trailing underscore instead, because backticks cannot help
there — the field `encode` is `encode_`. Those names are:

- the members every generated class or its companion object declares:
  `serialize`, `isDefault`, `reset`, `encode`, `encodeTo`, `decode`,
  `tryDecode`, `decoder`, `Decoder`, `MAX_SIZE`, `MAX_SIZE_LIMIT`,
  `ENC_SCRATCH`;
- the names the class body uses in front of a dot — `DecodeStatus`, `Long`,
  `Seq`. Inside the class a property of such a name would take precedence, and
  a call like `Seq.boolsToBytes(...)` would no longer compile.

Only the property changes: the wire is keyed by the field id, and the JSON key
stays the schema name. Two fields that end up with the same property or the
same JVM accessor — `encode` and `encode_`, `foo` and `Foo` (both `getFoo`),
or `isOpen` and `open` (both `setOpen`) — fail generation, naming both. The list lives in
`generators/kotlin/reserved.go`.

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

```kotlin
public class MShape {
    public var which: Int
        private set

    public var num: UShort
    public fun hasNum(): Boolean

    public var name: String
    public fun hasName(): Boolean

    public var pt: MShapePt
    public fun hasPt(): Boolean
    public fun mutablePt(): MShapePt

    public var tags: MutableList<String>
    public fun hasTags(): Boolean
    public fun mutableTags(): MutableList<String>

    public fun reset()

    public companion object {
        public const val NUM_ID: Int = 0
        public const val NAME_ID: Int = 1
        public const val PT_ID: Int = 2
        public const val TAGS_ID: Int = 3
    }
}
```

| operation | Kotlin |
|---|---|
| which option is held | `x.which` → the option's id |
| option ids | `MShape.PT_ID` (`<OPTION>_ID` constants) |
| test | `x.hasPt()` |
| read | `x.pt` |
| select with a value | `x.num = 7u` |
| select at the default and edit in place | `x.mutablePt().y = 2` |
| back to the default | `x.reset()` |

```kotlin
val m = M()                   // m.shape holds pt at its default: {x: 7, y: 0}
m.shape.num = 7u              // now num = 7; pt is no longer held
m.shape.mutablePt().y = 2     // pt again, from its default: {x: 7, y: 2}
if (m.shape.hasPt()) {
    use(m.shape.pt.x)
}
when (m.shape.which) {
    MShape.NUM_ID -> use(m.shape.num)
    MShape.PT_ID -> use(m.shape.pt.y)
}
m.shape.reset()               // pt at its default again
```

Each option is held in a slot of its own type — a `u16` option is a `UShort`
like any `u16` field, never boxed — so reading or selecting a numeric option
allocates nothing.

**Reading** an option that is not held returns that option's default — a new
struct or union at its default, an empty `MutableList`, a zero-length array for
a blob or a numeric or boolean array — and changes nothing; it does not select
the option.

**Assigning** an option's property selects the option with that value; the
option held before is no longer held. **`mutable<Option>()`** (struct and union
options, and arrays held in a `MutableList`: strings, blobs, structs, unions,
nested arrays) selects the option at its own default if another one is held,
and returns it. If the option is already held it is returned as it is,
untouched. An array option held in a primitive array (`UShortArray`,
`FloatArray`, `BooleanArray`, …) has no `mutable<Option>()`: select it by
assigning the property, and change its elements through the array the property
returns while it is held. Writing into what the property returns while the
option is **not** held changes nothing the union keeps: it is a detached
default.

**Ownership.** Assigning keeps the object it is given, as assigning a field
does: `x.pt = p` makes `p` the union's `pt`, and `x.pt` returns that same object
while `pt` is held. The union also keeps the object of a struct, union or list
option that is no longer held, and when that option is selected again — through
`mutable<Option>()` or by decoding into the union — it resets that object to
its default and uses it again rather than allocating a new one. So a struct or
list obtained from the property, from `mutable<Option>()` or assigned to it can
be reset under you once another option has been held in between; copy it if you
need it to stay as it was. `reset()` works the same way, and a message's
`reset()` resets its unions in place.

**Names.** The property is the option name, escaped with backticks where it is
a Kotlin keyword (`` `class` ``); `has<Option>` and `mutable<Option>` carry the
option name in upper camel case; the id constant is the option name in upper
case plus `_ID`. An option whose name is reserved for a field (see
[Field names](#field-names)) or is the union's own `which` gets a trailing
underscore: `x.which_`. Two options that would produce the same member
(`foo_bar` and `fooBar` both give `hasFooBar`, `a`'s `A_ID` and an option named
`A_ID`) fail generation, naming both.

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
option at its default. A union member left out of a message's JSON reads as the
union's default.
