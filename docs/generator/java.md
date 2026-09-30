# Java target — `targets.java`

Emits one class per message and named type, against `corelib-java`. Java allows
a single public top-level class per file, so every named struct and union gets
its own alongside the message.

## Options

| key | type | default | effect |
|---|---|---|---|
| `package` | string | `message` | The `package <name>;` declaration, and the source-directory layout. |

The generic options apply here too; see the [generic config](README.md).

## `package`

Two effects, and the second is the one that surprises people.

It sets the `package` declaration in every generated file — and it also decides
**where those files are written**. Java requires the directory structure to
mirror the package, so the output is always laid out under
`src/main/java/<package as directories>/`:

| `package` | output path |
|---|---|
| `message` (default) | `src/main/java/message/Telemetry.java` |
| `com.acme.msg` | `src/main/java/com/acme/msg/Telemetry.java` |

This holds in both `emit` modes — `sources` emits the same tree without the
build files. Changing the package changes the paths, so point `output_dir` at
the root of a source tree, not at the package directory itself.

## Field names

A field's Java field is the field's schema name. Java keeps fields and methods
apart, so a field `encode` sits beside the method `encode()` unchanged. Java has
no way to escape a reserved name, so a field whose name the generated class
cannot take gets a trailing underscore — the field `class` is `class_`. Those
names are:

- the Java keywords;
- the static fields every generated message declares: `MAX_SIZE`,
  `MAX_SIZE_LIMIT`;
- the names the class body uses in front of a dot — `Arrays`, `DecodeStatus`,
  `List`, `OStream`, `Seq`, `System`, and the package root `java`. Inside the
  class a field of such a name would hide the type or package, and a call like
  `Seq.reset(...)` would no longer compile.

Only the field changes: the wire is keyed by the field id, and the JSON key
stays the schema name. A schema name never ends with `_`, so a renamed field
cannot meet another one. The list lives in `generators/java/reserved.go`.

## Class names

Every class is named after its schema path, each name in PascalCase and the
path joined with `_`:

| schema | class | file |
|---|---|---|
| message `vehicle_telemetry` | `VehicleTelemetry` | `VehicleTelemetry.java` |
| `$defs` struct or union `point` | `Point` | `Point.java` |
| inline struct of field `pos` in message `m` | `M_Pos` | `M_Pos.java` |
| inline struct element of array field `pts` in message `m` | `M_Pts` | `M_Pts.java` |
| inline struct option `pt` of union field `u` in message `m` | `M_U_Pt` | `M_U_Pt.java` |
| `$defs` union `shape` used with `default_id` of option `pt` | `Shape__DefaultPt` | `Shape__DefaultPt.java` |

Enums and bitfields lower to `long` and have no class.

A class whose name the generated code already uses for something else gets a
trailing underscore — a message `string` is the class `String_` in
`String_.java`, since the generated classes use `java.lang.String`. Those names
are the nested `Decoder` class every message declares, the corelib-java types
the generated code uses (`OStream`, `IStream`, `Visitor`, `Seq`, …), the
`java.util` and `java.io` types it uses (`List`, `ArrayList`, `Arrays`,
`IOException`), and the `java.lang` types and annotations it uses (`String`,
`Long`, `Object`, `System`, `Deprecated`, `Override`, …). The list is
`javaTypeNames` in `generators/java/reserved.go`.

Each message's decode visitor is a package-private class `_<Message>__Visitor`
in the message's file.

In `emit: project` the harness (`Json`, `Main`) is the subpackage
`<package>.harness` — `src/main/java/<package>/harness/` — so a message `json`
or `main` is an ordinary class of the generated package. The jar's main class is
`<package>.harness.Main`.

## Unions

A `union` holds exactly one of its options. It is a class of its own whose
option slots are private, so an option is only reached through the methods
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

```java
public class M_Shape {
    public static final int NUM_ID = 0;
    public static final int NAME_ID = 1;
    public static final int PT_ID = 2;
    public static final int TAGS_ID = 3;

    public int which();

    public long getNum();
    public boolean hasNum();
    public void setNum(long v);

    public String getName();
    public boolean hasName();
    public void setName(String v);

    public M_Shape_Pt getPt();
    public boolean hasPt();
    public void setPt(M_Shape_Pt v);
    public M_Shape_Pt mutablePt();

    public List<String> getTags();
    public boolean hasTags();
    public void setTags(List<String> v);
    public List<String> mutableTags();

    public void reset();
}
```

| operation | Java |
|---|---|
| which option is held | `x.which()` → the option's id |
| option ids | `M_Shape.PT_ID` (`<OPTION>_ID` constants) |
| test | `x.hasPt()` |
| read | `x.getPt()` |
| select with a value | `x.setNum(7)` |
| select at the default and edit in place | `x.mutablePt().y = 2` |
| back to the default | `x.reset()` |

```java
M m = new M();              // m.shape holds pt at its default: {x: 7, y: 0}
m.shape.setNum(7);          // now num = 7; pt is no longer held
m.shape.mutablePt().y = 2;  // pt again, from its default: {x: 7, y: 2}
if (m.shape.hasPt()) {
    use(m.shape.getPt().x);
}
switch (m.shape.which()) {
case M_Shape.NUM_ID: use(m.shape.getNum()); break;
case M_Shape.PT_ID:  use(m.shape.getPt().y); break;
default: break;
}
m.shape.reset();            // pt at its default again
```

Each option is held in a slot of its own type — an integer option is a `long`
like any integer field, never boxed — so reading or selecting a numeric option
allocates nothing.

**Reading** an option that is not held returns that option's default — a new
struct or union at its default, an empty `List`, a zero-length array for a blob
or a numeric array — and changes nothing; it does not select the option.

**`set<Option>(v)`** selects the option with the value `v`; the option held
before is no longer held. **`mutable<Option>()`** (struct and union options, and
arrays held in a `List`: strings, blobs, booleans, structs, unions, nested
arrays) selects the option at its own default if another one is held, and
returns it. If the option is already held it is returned as it is, untouched. A
numeric array option (`long[]`, `short[]`, `float[]`, …) has no
`mutable<Option>()`: select it with `set<Option>(v)`, and change its elements
through the array `get<Option>()` returns while it is held.

**Ownership.** A setter keeps the object it is given, as assigning a field does:
`setPt(p)` makes `p` the union's `pt`, and `getPt()` returns that same object
while `pt` is held. The union also keeps the object of an option that is no
longer held, and when that option is selected again — through `mutable<Option>()`
or by decoding into the union — it resets that object to its default and uses it
again rather than allocating a new one. So a struct or list obtained from a
getter, from `mutable<Option>()` or passed to `set<Option>(v)` can be reset under
you once another option has been held in between; copy it if you need it to stay
as it was. `reset()` works the same way, and a message's `reset()` resets its
unions in place.

**Names.** The accessors are the option name in Java casing: `get<Option>`,
`set<Option>`, `has<Option>`, `mutable<Option>`; the id constant is the option
name in upper case plus `_ID`. An option named `class` gets a trailing
underscore (`getClass_()`, `setClass_()`), because `getClass()` belongs to
`Object`. An option is held in the private field `_<option>`; the union's own
tag is `which`.

**`$defs` unions** used with different `default_id`s are one class per
`default_id`, named `<Name>__Default<Option>`: `Shape__DefaultPt` and
`Shape__DefaultNum`.

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
