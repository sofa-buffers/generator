# C target — `targets.c`

Options accepted under `targets.c`. For shared options (`emit`, `namespace`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `symbol_prefix` | string | `message_` | Prefix applied to every generated C symbol — struct typedefs (`<prefix>Name_t`), descriptor tables, and the encode/decode/init functions. Use it to avoid name collisions when linking generated code from several schemas into one binary. |

```yaml
targets:
  c:
    symbol_prefix: myproj_   # -> myproj_Point_t, myproj_point_encode(), ...
```

## Every field must be bounded (no dynamic containers)

The C object model has **no dynamic containers**, so every field must be sized by
the schema: every `string`/`blob` needs a `maxlen` and every `array` a `count`
(at every nesting level, for every element kind — including a plain numeric
array; a `string`/`blob` array element also needs its own element `maxlen`). An
unbounded field is a **hard generation error** that names the offending field,
e.g.:

```
c: field "somemap" of "myfirstmessage" has no count; the fixed-storage C target
requires a bound on every string/blob (maxlen) and array (count) — the C object
model has no dynamic-container fallback
```

Unlike the C++ `c-cpp` and Rust `no_std` fixed-capacity profiles there is **no
`allow_dynamic` escape** for C: a schema with a genuinely dynamic collection (a
`count`-less map, say) is a heap-target schema, and must be given explicit
capacities before it can be generated for C. `count` never goes on the wire, so
adding one keeps the encoding byte-identical to every other target.

## Struct member order (widest-first)

The members of a generated `<prefix>Name_t` struct are declared **widest-first**
(8→4→2→1-byte alignment; strings, blobs, arrays and nested types rank as 8),
not in schema order, so the compiler inserts less padding between them. Fields
of equal alignment keep their schema order. This affects **declaration order
only** — encode and the descriptor table iterate in schema/field-id order, so
the wire bytes are byte-identical to every other target. Initialize structs by
member name (`_init()` or designated initializers), not positionally.
