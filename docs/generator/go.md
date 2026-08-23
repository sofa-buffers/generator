# Go target — `targets.go`

Emits the generated types for every message and named type, against
`corelib-go`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `package` | string | `message` | The `package <name>` clause of the generated files. |
| `module_path` | string | `example.com/generated` | The module path written to `go.mod`. Project mode only. |
| `go_version` | string | `1.21` | The `go <version>` directive written to `go.mod`. Project mode only. |

The generic options apply here too; see the [generic config](README.md).

## `package`

Sets the package clause on every generated `.go` file. It does not affect the
output directory — that is `output_dir` / `--out`, and Go does not require the
two to match.

## `module_path`

Only reaches the output under `emit: project`, where it becomes the `module`
line of the generated `go.mod`. It is what an importing module writes in its own
`require`, so set it to the path you will actually publish the generated code
under; the default is a placeholder that compiles but is not resolvable.

With `emit: sources` there is no `go.mod` and the key does nothing.

## `go_version`

Only reaches the output under `emit: project`, where it becomes the `go` line of
the generated `go.mod`. Raise it if the surrounding build needs a newer language
version; the generated code itself does not require one.
