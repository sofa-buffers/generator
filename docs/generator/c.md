# C target — `targets.c`

Emits C structs and a static descriptor table per message and named type,
against `corelib-c-cpp`. This is a footprint target: storage is sized from the
schema and the generated code allocates nothing.

## Options

| key | type | default | effect |
|---|---|---|---|
| `symbol_prefix` | string | `message_` | Prefix on every generated C symbol. |
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds a build and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. See the [generic config](README.md). |

The `max_dyn_*` decode limits are **not accepted** here — see below.

## `symbol_prefix`

C has one flat namespace, so every generated name carries this prefix: struct
typedefs, the descriptor tables, and the `encode` / `decode` / `init` functions.
A message `reading` with the default prefix becomes `message_reading`,
`message_reading_encode`, and so on.

Set it to something project-specific when the generated code is linked
alongside other C in the same binary — two schemas generated with the same
prefix and an overlapping message name collide at link time.

## Why there are no `max_dyn_*` limits

The receiver-side decode limits bound fields the schema leaves **unbounded**.
This target has none: every `string`, `blob` and `array` must declare a `maxlen`
or `count`, or generation fails —

```
error: backend "c": field "somemap" of "myfirstmessage" has no count; the
fixed-storage C target requires a bound on every string/blob (maxlen) and
array (count)
```

— because the C object model has no dynamic-container fallback. Storage is
fixed-capacity and sized from those bounds, so the schema bound already *is* the
limit and a runtime cap would have nothing left to bound.

The config schema is closed, so setting one of the keys here is a load-time
error rather than a silently ignored line.
