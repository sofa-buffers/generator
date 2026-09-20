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
| `enum` | `<EnumName>Array`, see below |
| `bitfield` | the unsigned width its highest `pos` implies (`Uint8Array`..`BigUint64Array`) |
| `string` `blob` | `string[]` `Uint8Array[]` |
| struct, union | `<TypeName>[]` |
| array | the element's own container, in a `[]` (`Uint32Array[]`) |

An **enum** array keeps the enum type on its elements. Each enum gets a companion
alias beside it:

```ts
export enum Mode { Off = 0, Active = 1 }
export type ModeArray = Int8Array & { [index: number]: Mode };
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
