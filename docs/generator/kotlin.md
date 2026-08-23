# Kotlin target — `targets.kotlin`

Emits one class per message and named type, against `corelib-kotlin-mp`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `package` | string | `message` | The `package <name>` declaration, and the source-directory layout in project mode. |
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds a Gradle build and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. See the [generic config](README.md). |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |

## `package`

Sets the `package` declaration in every generated file, and also decides **where
those files are written** — the output is laid out under
`src/main/kotlin/<package as directories>/`:

| `package` | output path |
|---|---|
| `message` (default) | `src/main/kotlin/message/Telemetry.kt` |
| `com.acme.msg` | `src/main/kotlin/com/acme/msg/Telemetry.kt` |

This holds in both `emit` modes — `sources` emits the same tree without the
Gradle build. Changing the package changes the paths, so point `output_dir` at
the root of a source tree, not at the package directory itself.
