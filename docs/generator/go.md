# Go target — `targets.go`

Language-specific options for the Go backend. For shared options (`emit`,
`file_layout`, `buffer`, `omit_defaults`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `package` | string | `messages` | The `package <name>` clause of the generated `.go` files. |
| `module_path` | string | `example.com/generated` | The module path written to the generated `go.mod` (project mode). |
| `go_version` | string | `1.21` | The `go <version>` directive written to the generated `go.mod` (project mode). |

```yaml
targets:
  go:
    package: messages
    module_path: github.com/me/myproj
    go_version: "1.22"
```

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`decode_style` · `accessors`
