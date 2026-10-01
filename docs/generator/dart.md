# Dart target — `targets.dart`

Emits the generated classes for every message and named type, against
`corelib-dart`.

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits, and `format` (whether
`sofabgen` runs `dart format` over what it emitted, see
[Formatting](#formatting)) — are documented in the
[generic config](README.md) and apply here unchanged.

## Field types

Scalars are plain Dart values. Every field whose payload the codec writes in
place — a string, a blob, a numeric array — is one of corelib-dart's **inline
destinations**: a typed list of fixed capacity (`storage`) plus the number of
elements in use (`length`). Decoding writes straight into that storage, so a
decode makes no copy and, into a reused object, allocates nothing.

| schema | member type |
|---|---|
| `u8`..`u64` `i8`..`i64` `enum` `bitfield` | `int` |
| `fp32` `fp64` | `double` (an fp32 NaN's raw bits in `<name>Fp32Bits`) |
| `boolean` | `bool` |
| `string` | `final sofab.InlineString` — its UTF-8 bytes |
| `blob` | `final sofab.InlineBytes` |
| array of `u*` `i*` `enum` `bitfield` | `final sofab.InlineInt64Array` |
| array of `boolean` | `final sofab.InlineInt64Array` — `0` is `false`, anything else `true` |
| array of `fp32` / `fp64` | `final sofab.InlineFloat32Array` / `InlineFloat64Array` |
| array of `string` / `blob` | `List<sofab.InlineString>` / `List<sofab.InlineBytes>` |
| array of numeric arrays (matrix) | `List<sofab.InlineInt64Array>` (or the float kinds) |
| struct | the generated class |
| union | the generated class, holding one option (see [Unions](#unions)) |
| array of struct / union / other arrays | a `List` of the element type |

Reading and writing a destination:

```dart
m.name.assignString('Ada');          // set a string
print(m.name.toString());            // read it
m.samples.assign([1, 2, 3]);         // set a numeric array
for (var i = 0; i < m.samples.length; i++) {
  use(m.samples[i]);                 // or m.samples.toList()
}
m.tags = [sofab.InlineString.of('a'), sofab.InlineString.of('b')];
```

Things worth knowing:

- **A destination field is `final`.** Change its value with `assign`,
  `assignString` or by setting `length`; the object itself stays, and so does
  its storage.
- **`length` is the value, `storage` is capacity.** Elements past `length` are
  left over from an earlier value and are not part of this one. A schema
  `count: N` / `maxlen: N` is a capacity too: a fresh array is empty, and a
  shorter value is legal and not padded.
- **Storage is sized once.** A destination whose schema bound fits in 1 KiB is
  allocated at that bound when the object is built. A larger or unbounded one
  starts empty and gets storage of exactly the announced size when a decode
  first needs it — after the size has been checked against the schema bound or
  the `max_dyn_*` limit. A reused object (`tryDecode`, `decoder`) keeps every
  byte of storage it has; `reset()` only sets the lengths back.
- **Strings are bytes.** A string field holds UTF-8; the codec validates it on
  decode, and the encoder validates it on the way out, so `assign` of bytes
  that are not UTF-8 makes `encode()` throw `SofabException`
  (`invalidArgument`). `toString()` builds the Dart `String` on demand.
- **A decoded bool array holds the wire's integers.** Any non-zero element is
  `true`; encoding writes `1` for it, and a value is compared with its default
  as booleans.
- **fp32 arrays keep raw bits.** An `InlineFloat32Array` stores the 32-bit
  patterns, so a signaling or payload NaN survives a round trip as long as it
  is read through `storage`, not widened into a `double`.
- **A destination is complete when the decode reports `complete`.** After
  `incomplete` or a refusal its contents are unspecified.

## Field names

A field's member is the field's schema name. In Dart a class member is in scope
throughout the class body and hides every outer name of the same spelling — a
type, a constant, an import prefix — so a field whose name the generated class
uses gets a trailing underscore; Dart has no way to escape a name. The field
`num` is `num_`. Those names are:

- the Dart keywords and built-in identifiers, and the core types the generated
  code names (`int`, `double`, `String`, `List`, `Uint8List`, …);
- the members every generated class declares: `serialize`, `reset`, and on a
  message also `encode`, `encodeTo`, `decode`, `tryDecode`, `decoder`,
  `maxSize`, `maxSizeLimit`, `maxDepth`;
- the members every class inherits from `Object`: `hashCode`, `runtimeType`,
  `toString`, `noSuchMethod`;
- the other outer names the class body uses: the `sofab` import prefix,
  `BytesBuilder`, `Deprecated`, `Float32List`, `Float64List`, `Int64List`, and
  the limit constants `maxDynArrayCount`, `maxDynBlobLen`, `maxDynStringLen`.

A field whose name starts with an upper-case letter gets the trailing
underscore too: every generated class starts with one (see
[Type names](#type-names)), and a class body names its own class and the class
of every struct, union or list element it holds. The field `M` is `M_`, whether
or not a class `M` exists. A name whose type class would itself carry the
underscore takes two: `String` is `String__`, since a type `string` is the
class `String_`.

A field's member depends on the field's own name only: adding, removing or
renaming another field, option or type never renames it. Only the member
changes: the wire is keyed by the field id, and the JSON key stays the schema
name.

An `fp32` field `f` has its raw-bits companion in `fFp32Bits` (the name with
its first letter lower-cased, plus `Fp32Bits`: the field `Speed` has
`speedFp32Bits`). A field whose own name ends in `Fp32Bits` is the one that
takes the underscore, so the companion keeps its spelling: the field
`fFp32Bits` is `fFp32Bits_`.

Enum constants and bitfield flags take the underscore when they are one of the
names listed above (`class` is `class_`; `LOW` keeps its name), and one more when the result is spelled like
their own class: the constant `E` of the enum `e` is `E_`. A constant spelled
like any other class keeps its name.

## Type names

Every class is top-level in `message.dart`, named after the schema path of its
type, each name in PascalCase and joined with `_`:

| schema | class |
|---|---|
| message `vehicle_telemetry` | `VehicleTelemetry` |
| `$defs` struct, union, enum or bitfield `point` | `Point` |
| inline struct, union, enum or bitfield of field `a` in message `m` | `M_A` |
| inline element struct or union of an array field `a` in message `m` | `M_A` |
| inline option `pt` of that union | `M_A_Pt` |
| a `$defs` union `shape` used with `default_id` of its option `pt` | `Shape__DefaultPt` |
| a message's incremental decoder | `M__Decoder` |

A type spelled like a name the generated code uses unqualified — a
`dart:core` or `dart:typed_data` type such as `String`, `List`, `Map`,
`Object`, `BigInt`, `Uint8List`, `Endian`, `Deprecated` — takes a trailing
underscore: the message `string` is `String_`, with the decoder
`String__Decoder`. The generated harness imports `dart:io`, `dart:convert` and
`dart:typed_data` under a prefix, so their other names (`File`, `Platform`,
`Int32List`, …) are free.

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

```dart
class M_Shape {
  static const int numId = 0;
  static const int nameId = 1;
  static const int ptId = 2;
  static const int tagsId = 3;

  int get which;

  int get num_;                          // `num` is a Dart type: num_
  set num_(int v);
  bool get hasNum;

  sofab.InlineString get name;
  bool get hasName;
  sofab.InlineString mutableName();

  M_Shape_Pt get pt;
  set pt(M_Shape_Pt v);
  bool get hasPt;
  M_Shape_Pt mutablePt();

  List<sofab.InlineString> get tags;
  set tags(List<sofab.InlineString> v);
  bool get hasTags;
  List<sofab.InlineString> mutableTags();

  void reset();
}
```

| operation | Dart |
|---|---|
| which option is held | `x.which` → the option's id |
| option ids | `M_Shape.ptId` (`<option>Id` constants) |
| test | `x.hasPt` |
| read | `x.pt` |
| select with a value | `x.num_ = 7`, `x.pt = p` |
| select at the default and edit in place | `x.mutablePt().y = 2`, `x.mutableName().assignString('Ada')` |
| back to the default | `x.reset()` |

```dart
final m = M();                   // m.shape holds pt at its default: {x: 7, y: 0}
m.shape.num_ = 7;                // now num = 7; pt is no longer held
m.shape.mutablePt().y = 2;       // pt again, from its default: {x: 7, y: 2}
if (m.shape.hasPt) {
  use(m.shape.pt.x);
}
switch (m.shape.which) {
  case M_Shape.numId:
    use(m.shape.num_);
  case M_Shape.ptId:
    use(m.shape.pt.y);
}
m.shape.mutableName().assignString('Ada'); // name, from empty
m.shape.reset();                 // pt at its default again
```

Each option is held in a field of its own type — `num` is an `int`, `name` an
`InlineString` — so reading or selecting an option never boxes or casts.

**Reading** an option that is not held returns that option's default — a new
struct or union at its default, an empty list, an empty destination — and
changes nothing; it does not select the option, and writing into what it
returned does not reach the union.

**Selecting.** Assigning a scalar, struct, union or wrapper-array option selects
it with the value assigned; the option held before is no longer held.
**`mutable<Option>()`** (every option but the scalars) selects the option at its
own default if another one is held, and returns it; if the option is already
held it is returned as it is, untouched. A string, blob or numeric-array option
has no setter — it is filled in place through `mutable<Option>()`, exactly as a
struct's destination member is `final` and filled through `assign` /
`assignString`.

**Storage and ownership.** An option's storage is created the first time the
option is selected, and kept when another option is selected: selecting it
again — through `mutable<Option>()`, by decoding into the union, or by `reset()`
for the `default_id` option — resets that same object in place (a struct or
union to its defaults, a list cleared, a destination back to length 0). That is
what lets `tryDecode` into a reused message decode without allocating. It also
means an object obtained earlier from the getter or `mutable<Option>()`, or
passed to a setter, is reset and then overwritten when its option is selected
again after another one; copy it if you need to keep it. Assigning an option
keeps the object it is given, as assigning a field does. A new union creates the
storage of its `default_id` option only.

**fp32 options** keep the raw wire bits of a NaN in `<option>Fp32Bits`, like an
fp32 member: assigning the value clears them; to write a signaling NaN, assign
`double.nan` and then the bits.

**Names.** The getter and setter are the option name; `has<Option>` and
`mutable<Option>()` use it in PascalCase; the constant is `<option>Id` and an
fp32 option's bits are `<option>Fp32Bits`, both with the name's first letter
lower-cased (the option `Pt` has `ptId`). An option takes a trailing underscore
where a field does (see [Field names](#field-names)) — `num` is `num_`, `Pt`
is `Pt_` — and also when it is `which`, or is spelled like one of these derived
members: when its name ends in `Id` or `Fp32Bits`, or is `has` or `mutable`
alone or followed by an upper-case letter. Its own constant and bits then carry
the underscore as well: the option `aId` is `aId_` with the constant
`aId_Id`, and `hasX` is `hasX_` with `hasX_Id` and `hasHasX`. So the option
`a` always has the constant `aId`, and `x` the test `hasX`, whatever other
options the union has; an option's names depend on its own name only.

**`$defs` unions** used with different `default_id`s are one class per
`default_id`, named after the union and the default option:
`Shape__DefaultPt` and `Shape__DefaultNum`.

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
option at its default. The generated `message.dart` itself has no JSON code. A
union member left out of a message's JSON reads as the union's default. A
64-bit option (like any 64-bit field) reads a decimal string as well as a JSON
number; spell a value outside ±2^53 as a string.

## Formatting

`sofabgen` can run `dart format` over what it generated, and does so only when
you ask: it spawns no external tool on its own, so the same version writes the
same bytes on every machine, whatever happens to be installed.

Asking is one switch — the CLI flag `--format`, or the `generic.run_formatter`
config key (the flag wins):

| value | what `sofabgen` does |
|---|---|
| `off` (the default) | Never runs `dart format`. The files are the generator's own output: valid, compilable, not canonically formatted. |
| `auto` | Runs `dart format` when it is available; when it is not, writes the files unformatted and says so once on stderr. |
| `require` | Runs `dart format`, and fails the run when it is not available. |

With the pass on, every generated `.dart` file goes through `dart format` once,
at the language version of the package the files are going into. That version is
not a detail: `dart format` chooses its style by it — the short style below 3.7,
the tall style from 3.7 on — so it decides what "formatted" means.

Where it comes from:

| your run | the language version used |
|---|---|
| `emit: project` | the `sdk:` lower bound of the generated `pubspec.yaml` |
| `emit: sources` (the default) | the `sdk:` lower bound of the `pubspec.yaml` above the output directory — your package's |
| neither (the output directory is in no package) | 3.4 |

So the files arrive in the style your own `dart format` over that package
produces, and `dart format --output=none --set-exit-if-changed` over a tree
holding them passes.

It is a convenience. The generated code is correct and compiles either way; the
switch only decides whether `dart format` has already been over it when it reaches you,
and running `dart format` over the output directory yourself gets you the
same tree.

Under `auto` and under `require` alike, a formatter that RUNS and rejects a
generated file fails the generation with that file named — that is a generator
bug, and writing the file would only move it into your build.

