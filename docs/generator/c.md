# C target — `targets.c`

Emits C structs and a static descriptor table per message and named type,
against `corelib-c-cpp`. This is a footprint target: storage is sized from the
schema and the generated code allocates nothing, so every `string`, `blob` and
`array` must declare a `maxlen` or `count` — an unbounded field fails
generation.

## Options

| key | type | default | effect |
|---|---|---|---|
| `symbol_prefix` | string | `message_` | Prefix on every generated C symbol. |

`emit`, `license` and `max_message_size` apply here too; see the
[generic config](README.md). The `max_dyn_*` decode limits are **not** accepted
by this target — see there for why.

## `symbol_prefix`

C has one flat namespace, so every generated name carries this prefix: struct
typedefs, the descriptor tables, and the `encode` / `decode` / `init` functions.
A message `reading` with the default prefix becomes `message_reading`,
`message_reading_encode`, and so on.

Set it to something project-specific when the generated code is linked
alongside other C in the same binary — two schemas generated with the same
prefix and an overlapping message name collide at link time.

## Booleans

A `boolean` field, and each element of a `boolean` array, is a `uint8_t`.

- **Decode.** Every non-zero wire value, however wide, is stored as `1`.
- **Encode.** The member is written as it is stored. Keep it at `0` or `1` so
  the message carries the canonical `true`.

## Bitfields

A `bitfield` field is a raw unsigned integer, sized from the highest declared
`pos` — it is never narrowed to only the declared flags, so a value a newer
peer's vocabulary set (a bit position this schema doesn't name) still
round-trips instead of being rejected or masked away.

Each declared bit still gets a name: a `#define` beside the struct, so a caller
sets/tests bits by name instead of by position:

```c
out.alarms = MESSAGE_FRIDGE_ALARMS_TEMP_HIGH | MESSAGE_FRIDGE_ALARMS_POWER_LOSS;
if (in.alarms & MESSAGE_FRIDGE_ALARMS_TEMP_HIGH) { ... }
```

The name is `symbol_prefix`, then the path of the bitfield's definition, then
the flag, all upper-cased:

| Bitfield declared as | Macro |
|---|---|
| field `top` of message `m`, flag `x` | `MESSAGE_M_TOP_X` |
| element of array field `arr` of message `m`, flag `y` | `MESSAGE_M_ARR_ELEM_Y` |
| field `inner` of an inline struct field `n` of message `m`, flag `z` | `MESSAGE_M_N_INNER_Z` |
| field `st` of `$defs/struct/Pt`, flag `ok` | `MESSAGE_STRUCT_PT_ST_OK` |
| `$defs/bitfield/Shared`, flag `a` | `MESSAGE_BITFIELD_SHARED_A` |

A bitfield from `$defs` carries no message or field name, whichever field uses
it. It gets one `#define` block per header, however many fields share it.

Each macro has the width of its field, at least `uint32_t`:
`((uint32_t)1 << pos)`, or `((uint64_t)1 << pos)` when the field is a
`uint64_t`. So `v &= ~FLAG` clears only that flag, at any width.

C has one macro namespace across all included headers. If a flag macro would
match another generated macro anywhere in the schema, generation fails and
names both owners. The other macro can be a message's include guard
(`..._H`), its `..._MAX_SIZE`/`..._MAX_SIZE_LIMIT`, or another flag.
