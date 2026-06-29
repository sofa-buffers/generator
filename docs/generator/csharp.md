# C# target — `targets.csharp`

Language-specific options for the C# backend. For shared options (`emit`,
`file_layout`, `buffer`, `omit_defaults`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `namespace` | string | `Sofabuffers` | The `namespace <name>` wrapping the generated classes. Also settable in `generic`. |

```yaml
targets:
  csharp:
    namespace: MyProj.Messages
```

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`target_framework` · `nullable` · `lang_version` · `use_records` ·
`generate_doc_file`
