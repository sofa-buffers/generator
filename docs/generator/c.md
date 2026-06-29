# C target — `targets.c`

Language-specific options for the C backend. For shared options (`emit`,
`file_layout`, `buffer`, `omit_defaults`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `symbol_prefix` | string | `sofab_` | Prefix applied to every generated C symbol — struct typedefs (`<prefix>Name_t`), descriptor tables, and the encode/decode/init functions. Use it to avoid name collisions when linking generated code from several schemas into one binary. |

```yaml
targets:
  c:
    symbol_prefix: myproj_   # -> myproj_Point_t, myproj_point_encode(), ...
```

## Reserved options

Accepted by the schema validator but not yet honored by the generator (they have
no effect today):

`c_standard` · `header_extension` · `source_extension` · `include_guard` ·
`descriptor_profile` · `string_storage`
