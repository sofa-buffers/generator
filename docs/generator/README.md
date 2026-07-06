# Per-language config options

Each file here documents the options for one `sofabgen` target — the keys you
may set under `targets.<lang>` in your config. The config schema
(`schema/sofabgen-config-schema.json`) is **closed** and lists **only honored
options**: every key it accepts changes generator behaviour, and unknown keys
are rejected at load time rather than silently ignored.

## Generic options — `generic:`

Shared options live in the top-level `generic:` block and apply to every
target; a per-target value overrides the `generic` value for that target
(precedence: built-in default < `generic` < `targets.<lang>`).

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | `sources`: just the message-type source files. `project`: additionally scaffold a buildable project (build files + the canonical-JSON conformance harness). Also settable per target. |
| `namespace` | string | per-language | Namespace wrapping the generated types, for targets that have one (C++, C#). Deliberately not defaulted generically — each backend applies its idiomatic default (C++ `message`, C# `Message`). |
| `input_dir` | string | — | Directory of message definitions to generate from. The CLI `--in` flag overrides it. |
| `output_dir` | string | — | Directory the generated files are written to. The CLI `--out` flag overrides it. |
| `tool_banner` | string | `sofabgen` | Tool name stamped into every generated file header. |
| `license` | string | none | SPDX identifier for the `SPDX-License-Identifier` header stamped into every generated file (e.g. `MIT`, `Apache-2.0`, `LicenseRef-Acme`). Unset or `none` emits no SPDX line. |

## Per-target options — `targets.<lang>:`

| Target | Doc | Language-specific options |
|--------|-----|---------------------------|
| `c` | [c.md](c.md) | `symbol_prefix` |
| `cpp` | [cpp.md](cpp.md) | `corelib`, `namespace`, `allow_dynamic` |
| `rust` | [rust.md](rust.md) | `corelib`, `no_std`, `allow_dynamic` |
| `go` | [go.md](go.md) | `package`, `module_path`, `go_version` |
| `python` | [python.md](python.md) | — |
| `java` | [java.md](java.md) | `package` |
| `csharp` | [csharp.md](csharp.md) | `namespace` |
| `typescript` | [typescript.md](typescript.md) | `int64` |
| `docs` | [docs.md](docs.md) | `format` |

> **History.** The schema used to validate a set of *reserved* planning-era
> keys (`buffer`, `validation`, `naming`, `file_layout`, `timestamp`, …) that
> no backend ever consumed. They were removed: the schema now stays in
> lockstep with the set of keys the generator actually reads, so a key that
> validates is a key that works.
