# C# target — `targets.csharp`

Emits one class per message and named type, against `corelib-cs`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `namespace` | string | `Message` | The `namespace <name>` wrapping the generated classes. |
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds a `.csproj` and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. See the [generic config](README.md). |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. See the [generic config](README.md). |

## `namespace`

Wraps every generated type. Unlike Java's and Kotlin's `package`, it has no
effect on file layout — C# does not tie namespaces to directories, so the output
stays a single `Message.cs` in the output directory whichever namespace you
choose.

`generic.namespace` sets it for every target that has one; `targets.csharp.namespace`
overrides that for this target alone. Left unset, this target's own default
(`Message`) applies rather than a generic one — each language keeps its own
idiomatic capitalisation.
