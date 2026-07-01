# Java target — `targets.java`

Language-specific options for the Java backend. For shared options (`emit`,
`file_layout`, `buffer`, …) see the [`generic`](README.md)
config.

## Honored options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `package` | string | `messages` | The `package <name>;` declaration of the generated classes (and the Maven source directory layout in project mode). |

```yaml
targets:
  java:
    package: com.myproj.messages
```

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`java_version` · `use_records` · `build` · `group_id` · `artifact_id`
