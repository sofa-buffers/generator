# C# target — `targets.csharp`

Options accepted under `targets.csharp`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `namespace` | string | `Message` | The `namespace <name>` wrapping the generated classes. Also settable in `generic`. |

```yaml
targets:
  csharp:
    namespace: MyProj.Messages
```
