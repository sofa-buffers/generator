# Go target — `targets.go`

Options accepted under `targets.go`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `package` | string | `message` | The `package <name>` clause of the generated `.go` files. |
| `module_path` | string | `example.com/generated` | The module path written to the generated `go.mod` (project mode). |
| `go_version` | string | `1.21` | The `go <version>` directive written to the generated `go.mod` (project mode). |

```yaml
targets:
  go:
    package: messages
    module_path: github.com/me/myproj
    go_version: "1.22"
```

## Struct field order (widest-first)

Generated struct fields are declared **widest-first** (8→4→2→1-byte alignment;
strings, slices and nested types rank as 8), not in schema order — Go lays
structs out in declaration order, so this cuts padding between fields. Fields
of equal alignment keep their schema order. This affects **declaration order
only** — encode walks the schema/field-id order, so the wire bytes are
byte-identical to every other target. Construct values with keyed struct
literals (`Point{X: 1, Y: 2}`), not positionally.
