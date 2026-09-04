# TypeScript target — `targets.typescript`

Emits one class per message and named type, against `corelib-ts`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `int64` | `bigint` \| `long` \| `number` | `bigint` | How 64-bit fields are represented in the generated API. |

The generic options apply here too; see the [generic config](README.md).

## `int64`

JavaScript has no 64-bit integer type that is both exact and cheap, so `u64` and
`i64` fields need a representation chosen per project. **All three modes are
wire-identical** — this only changes what the generated API hands you.

| mode | `u64`/`i64` scalar | `u64`/`i64` array |
|---|---|---|
| `bigint` | `bigint` | `bigint[]` |
| `long` | corelib `Long`, via a get/set accessor pair | `Long`-backed |
| `number` | `number` | `Long`-backed |

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

## Bitfields

A `bitfield` is carried in the narrowest type that holds its highest declared
flag position, which in TypeScript means one of two:

| highest `pos` | field type | flag masks |
|---|---|---|
| 0–31 | `number` | `export enum` members |
| 32–63 | `bigint` | a frozen `const` object of `bigint` masks |

A `number` is a double, so it holds a mask exactly only to bit 52, and JavaScript
narrows both operands of `|` and `&` to 32 bits — a flag above position 31 is
neither storable nor combinable in one. A `bigint` has neither limit, and a TS
`enum` member can only be a number, which is why the wide masks are a `const`
object instead:

```ts
m.caps = Caps.Read | Caps.WriteAt63;   // bigint | bigint
```

`int64` does not reach bitfields. It chooses how a 64-bit **integer** is
represented, and a mask has no lossy-number reading to opt into.

In JSON, a `bigint`-carried bitfield is a decimal **string**, as `u64` is under
`int64: bigint` — `fromJSON` accepts both the string and a plain number. A
`number`-carried one is a plain JSON number.
