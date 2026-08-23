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
