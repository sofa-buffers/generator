# TypeScript target — `targets.typescript`

Emits one class per message and named type, against `corelib-ts`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `int64` | `bigint` \| `long` \| `number` | `bigint` | How 64-bit fields are represented in the generated API. |

The generic options apply here too — including `format`, which decides whether
`sofabgen` runs `prettier` over what it emitted (see [Formatting](#formatting));
see the [generic config](README.md).

## `int64`

JavaScript has no 64-bit integer type that is both exact and cheap, so `u64` and
`i64` fields need a representation chosen per project. **All three modes are
wire-identical** — this only changes what the generated API hands you.

| mode | `u64`/`i64` scalar | `u64`/`i64` array |
|---|---|---|
| `bigint` | `bigint` | `BigUint64Array` / `BigInt64Array` |
| `long` | corelib `Long`, via a get/set accessor pair | `Long[]` |
| `number` | `number` | `Long[]` |

**`bigint`** — exact, and the plainest to use: values are ordinary `bigint`
literals (`123n`). The cost is that every 64-bit value allocates a `bigint`
during encode and decode.

**`long`** — keeps `bigint` out of the hot path entirely. Each 64-bit field
becomes an accessor pair backed by the corelib's `Long`, and assignment accepts
`Long | bigint | number`, so writing values stays convenient while the codec
never boxes one. Choose this when 64-bit fields are frequent and throughput
matters.

**`number`** — arrays behave as under `long`, but 64-bit **scalars** are a plain
`number`. This is the only mode that can lose information: **you** guarantee the
values fit JavaScript's ±2⁵³ safe-integer range. A value outside it is silently
imprecise, not rejected. Choose it only when the schema's 64-bit fields are known
to carry small values — timestamps in milliseconds, counters — and the ergonomics
of a plain number are worth the guarantee.

## Arrays

Every array of a **numeric** element is a typed array — the exact width the
schema declares — and that array is what the codec reads and writes directly. No
copy, no conversion and no per-element range check sits between it and the wire.

| element | member type |
|---|---|
| `u8` `u16` `u32` | `Uint8Array` `Uint16Array` `Uint32Array` |
| `i8` `i16` `i32` | `Int8Array` `Int16Array` `Int32Array` |
| `u64` `i64` | per [`int64`](#int64): `BigUint64Array` / `BigInt64Array`, else `Long[]` |
| `fp32` `fp64` | `Float32Array` `Float64Array` |
| `boolean` | `Uint8Array` — `0` or `1`, the byte the wire carries |
| `enum` | `<EnumName>__Array`, see below |
| `bitfield` | the unsigned width its highest `pos` implies (`Uint8Array`..`BigUint64Array`) |
| `string` `blob` | `string[]` `Uint8Array[]` |
| struct, union | `<TypeName>[]` |
| array | the element's own container, in a `[]` (`Uint32Array[]`) |

An **enum** array keeps the enum type on its elements. Each enum gets a companion
alias beside it:

```ts
export enum Mode { Off = 0, Active = 1 }
export type Mode__Array = Int8Array & { [index: number]: Mode };
```

so the storage is a plain `Int8Array` and `m.modes[0]` still reads as `Mode`.

Three consequences worth knowing:

- **A typed array has a fixed length.** `push` does not exist; build the value
  with `new Uint16Array(n)`, `Uint16Array.from([...])` or `.set()`. A schema
  `count: N` is a *capacity*, so a shorter array is legal — it is not padded.
- **A typed array masks on store.** `a[0] = 70000` on a `Uint16Array` silently
  stores `4464`. That is JavaScript's rule for the container you asked for; the
  generator adds no check in front of it. Decoding is unaffected: an over-width
  element on the wire is rejected as `InvalidMsg`, never masked.
- **`toJSON` emits plain JSON arrays** and `fromJSON` builds the typed container
  back, so JSON round-trips are unchanged. A 64-bit array prints as decimal
  strings, as a 64-bit scalar does under `int64: bigint`.

## Bitfields

A `bitfield` is carried in the narrowest type that holds its highest declared
flag position, which in TypeScript means one of two:

| highest `pos` | field type | flag masks |
|---|---|---|
| 0–30 | `number` | `export enum` members |
| 31–63 | `bigint` | a `const` object of `bigint` masks (`as const`) |

A `number` is a double, so it holds a mask exactly only to bit 52 — but the band
ends earlier than that, at 30, because **combining** is what a mask is for.
JavaScript narrows both operands of `|` and `&` to 32-bit *signed*, so a mask
with bit 31 set comes back negative: `Flags.A | Flags.AtBit31` evaluates to a
negative number, which the encoder rejects as an unsigned value out of range.
Positions 0–30 combine cleanly (`|` over them tops out at `0x7FFFFFFF`). A
`bigint` has neither limit, and a TS `enum` member can only be a number, which is
why the wide masks are a `const` object instead:

```ts
m.caps = Caps.Read | Caps.WriteAt63;   // bigint | bigint
```

`int64` does not reach bitfields. It chooses how a 64-bit **integer** is
represented, and a mask has no lossy-number reading to opt into.

In JSON, a `bigint`-carried bitfield is a decimal **string**, as `u64` is under
`int64: bigint` — `fromJSON` accepts both the string and a plain number. A
`number`-carried one is a plain JSON number.

## Field names

A field's member is the field's schema name. Every keyword is a valid class
member in TypeScript, so `class` or `typeof` stay as they are. A field whose name
the generated class already uses as an instance member gets a trailing
underscore — the field `encode` is `encode_`. Those names are:

- `constructor`, which a class body rejects as a field;
- the methods every generated class declares: `serialize`, `toJSON`,
  `isDefault`, and on a message also `encode`;
- the members every object inherits: `toString`, `toLocaleString`, `valueOf`,
  `hasOwnProperty`, `isPrototypeOf`, `propertyIsEnumerable`.

The statics (`fromJSON`, `decode`, `MAX_SIZE`) are a namespace of their own, so
a field may take their names. Only the member changes: the wire is keyed by the
field id, and the JSON key stays the schema name. The list lives in
`generators/typescript/reserved.go`.

A member the generator derives from a field — the raw-bytes companion of an
`fp32` field `f`, `fFp32Raw` — gets a trailing underscore instead when a
sibling field already has that name: beside a field `fFp32Raw`, `f`'s companion
is `fFp32Raw_`. The schema's own names always keep their spelling.

## Type names

`message.ts` is one flat module. Every message and named type is a class, enum
or bitfield named after its schema path, each part in PascalCase and the parts
joined with `_`:

| schema | TypeScript |
|---|---|
| message `vehicle_telemetry` | `VehicleTelemetry` |
| `$defs` type `point` | `Point` |
| inline struct of field `pos` in message `m` | `M_Pos` |
| inline struct element of an array field `rows` in message `m` | `M_Rows` |
| inline struct of option `pt` of union field `u` in message `m` | `M_U_Pt` |
| one `default_id` variant of a `$defs` union `shape` | `Shape__DefaultPt` |

What the generator adds for a type follows `__`: the incremental decoder of a
message `m` is `M__Decoder`, the array alias of an enum `mode` is
`Mode__Array`. A schema name never contains `__`, so none of these can meet a
type name.

Enum constants and bitfield flags are the constant's name in PascalCase
(`Mode.Active`, `Flags.On`).

A type whose name would be one the module already uses — a name it imports from
`corelib-ts` (`OStream`, `Long`, `Visitor`, ...), a JavaScript global it refers
to (`Uint8Array`, `Number`, `Record`, ...), or one of the `MAX_DYN_*` limit
constants — gets a trailing underscore: a message `long` is the class `Long_`,
and its decoder is still `Long__Decoder`.

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

```ts
export class M_Shape {
  static readonly NUM_ID = 0;
  static readonly NAME_ID = 1;
  static readonly PT_ID = 2;
  static readonly TAGS_ID = 3;

  get which(): number;

  get num(): number;
  set num(v: number);
  hasNum(): boolean;

  get name(): string;
  set name(v: string);
  hasName(): boolean;

  get pt(): M_Shape_Pt;
  set pt(v: M_Shape_Pt);
  hasPt(): boolean;
  mutablePt(): M_Shape_Pt;

  get tags(): string[];
  set tags(v: string[]);
  hasTags(): boolean;
  mutableTags(): string[];

  clear(): void;
}
```

| operation | TypeScript |
|---|---|
| which option is held | `x.which` → the option's id |
| option ids | `M_Shape.PT_ID` (`<OPTION>_ID` constants) |
| test | `x.hasPt()` |
| read | `x.pt` |
| select with a value | `x.num = 7` |
| select at the default and edit in place | `x.mutablePt().y = 2` |
| back to the default | `x.clear()` |

```ts
const m = new M();          // m.shape holds pt at its default: {x: 7, y: 0}
m.shape.num = 7;            // now num = 7; pt is no longer held
m.shape.mutablePt().y = 2;  // pt again, from its default: {x: 7, y: 2}
if (m.shape.hasPt()) {
  use(m.shape.pt.x);
}
switch (m.shape.which) {
  case M_Shape.NUM_ID: use(m.shape.num); break;
  case M_Shape.PT_ID:  use(m.shape.pt.y); break;
}
m.shape.clear();            // pt at its default again
```

Each option has a property of its own, typed as a field of that kind would be —
`num` is a `number`, `pt` an `M_Shape_Pt` — so a numeric option is never held in a
property that also holds strings or objects.

**Reading** an option that is not held returns that option's default — a new
struct or union at its default, an empty array, an empty `Uint8Array` for a
blob, the option's declared default for a number — and changes nothing; it does
not select the option.

**Assigning** an option selects that option with the value assigned; the option
held before is no longer held, and whatever it held is let go.
**`mutable<Option>()`** (struct and union options, and arrays whose elements are
strings, blobs, structs, unions or arrays) selects the option at its own
default if another one is held, and returns it. If the option is already held it
is returned as it is, untouched. A numeric array option (a typed array, or a
`Long[]`) has no `mutable<Option>()`: select it by assigning an array, and change
its elements through the array the getter returns while it is held.

**64-bit options** follow the [`int64`](#int64) mode like any field: a `bigint`,
a `Long` or a `number`. Under `long` and `number` a 64-bit option accepts
`Long | bigint | number` (and a 64-bit array option any mix of those) and
converts once, on assignment, as a message's own 64-bit field does.

**fp32 options** keep the wire bytes of a decoded NaN beside the value, as an
fp32 field does: `x.<option>Fp32Raw` (`null` while another option is held).
Assigning the value drops them; assigning the bytes selects the option.

**Ownership.** Assigning an option keeps the object it is given, as assigning a
field does: after `x.pt = p`, `x.pt` returns that same `p` while `pt` is held.
Selecting a struct, union or array option that is not held — through
`mutable<Option>()`, by decoding into the union, or by `clear()` for the
`default_id` option — always starts from a new object at the option's default;
an object obtained earlier is never reset behind your back, it is merely no
longer held.

**Names.** The members are the option name: the getter/setter `<option>`,
`has<Option>()`, `mutable<Option>()` and the constant `<OPTION>_ID` (the name
upper-cased). An option whose name is reserved for a field (see
[Field names](#field-names)) or is one of the union's own members (`which`,
`clear`) gets a trailing underscore: an
option `which` is `which_`, with `hasWhich()` and `WHICH_ID`. A derived member
— `has<Option>()`, `mutable<Option>()`, `<option>Fp32Raw` — that would land on
another member gets a trailing underscore instead, and the option keeps its
name: beside an option `hasX`, the option `x` has `hasX_()`; an option
`own_property` has `hasOwnProperty_()`, so Object's `hasOwnProperty` stays
intact.

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

**JSON.** `toJSON()` returns an object with exactly one member, the held
option — `{"pt":{"x":7,"y":2}}`, also when that is the `default_id` option at
its default — and `fromJSON()` selects the option it finds. A union member left
out of a message's JSON reads as the union's default.

## Formatting

`sofabgen` can run `prettier` over what it generated, and does so only when you
ask: it spawns no external tool on its own, so the same version writes the same
bytes on every machine, whatever happens to be installed. That matters here for
the same reason it does for Python — `prettier` is not part of the TypeScript
toolchain, so it may simply not be there.

Asking is one switch — the CLI flag `--format`, or the `generic.run_formatter`
config key (the flag wins):

| value | what `sofabgen` does |
|---|---|
| `off` (the default) | Never runs `prettier`. The files are the generator's own output: valid, type-checking, not canonically formatted. |
| `auto` | Runs `prettier` when it is available; when it is not, writes the files unformatted and says so once on stderr. |
| `require` | Runs `prettier`, and fails the run when it is not available. |

With the pass on, every generated `.ts` file goes through `prettier` once. The
output directory's own `node_modules/.bin/prettier` is preferred over anything on
`PATH`, so a project that pins prettier as a devDependency is formatted by the
version it pins. prettier runs in the output directory, so a `.prettierrc`,
`.editorconfig` or `.prettierignore` of your own that covers it is honoured, and
the files come out the way your own `prettier --write` over that tree would leave
them. `prettier --check` over a tree holding them then passes, so generated files
need no exclusion from a formatting gate. prettier's output changes between
releases, so a tree formatted by one version and checked by another can still
report a difference: use the same version for both.

It is a convenience. The generated code is correct and type-checks either way;
the switch only decides whether `prettier` has already been over it when it
reaches you, and running `prettier --write` over the output directory yourself
gets you the same tree.

Under `auto` and under `require` alike, a `prettier` that RUNS and rejects a
generated file fails the generation with that file named — that is a generator
bug, and writing the file would only move it into your build.

The rest of an `emit: project` tree — `package.json`, `tsconfig.json` and
`README.md` — is written the way prettier writes it whatever the switch says,
including at `off`, where nothing is run at all.
