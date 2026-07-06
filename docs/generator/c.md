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

## Struct member order (widest-first)

The members of a generated `<prefix>Name_t` struct are declared **widest-first**
(8→4→2→1-byte alignment; strings, blobs, arrays and nested types rank as 8),
not in schema order, so the compiler inserts less padding between them. Fields
of equal alignment keep their schema order. This affects **declaration order
only** — encode and the descriptor table iterate in schema/field-id order, so
the wire bytes are byte-identical to every other target. Initialize structs by
member name (`_init()` or designated initializers), not positionally.
