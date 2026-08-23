# TypeScript target — `targets.typescript`

Emits one class per message and named type, against `corelib-ts`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `int64` | `bigint` \| `long` \| `number` | `bigint` | How 64-bit fields are represented in the generated API. |
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds a `package.json` / `tsconfig.json` and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. See the [generic config](README.md). |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |

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
