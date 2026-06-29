# Per-language config options

Each file here documents the **language-specific** options for one `sofabgen`
target — the keys you may set under `targets.<lang>` in your config.

Shared options (`emit`, `validation`, `naming`, `file_layout`, `buffer`,
`omit_defaults`, `namespace`, `tool_banner`, `license`, `timestamp`, …) live in
the top-level `generic:` block and apply to every target; they are **not**
repeated here. A per-target value overrides the `generic` value for that target.

| Target | Doc | Honored language-specific options |
|--------|-----|-----------------------------------|
| `c` | [c.md](c.md) | `symbol_prefix` |
| `cpp` | [cpp.md](cpp.md) | `corelib`, `namespace` |
| `rust` | [rust.md](rust.md) | `corelib` |
| `go` | [go.md](go.md) | `package`, `module_path`, `go_version` |
| `python` | [python.md](python.md) | — |
| `java` | [java.md](java.md) | `package` |
| `csharp` | [csharp.md](csharp.md) | `namespace` |
| `typescript` | [typescript.md](typescript.md) | — |

> **Reserved options.** The config schema is the full *intended* surface, so it
> validates several per-target keys the generator does not act on yet. Each doc
> lists its target's reserved keys under "Reserved options" — they pass
> validation but currently have no effect.
>
> The schema also defines a `cpp-embedded` target that has no backend yet; it is
> not documented here.
