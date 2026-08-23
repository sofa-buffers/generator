# Zig target — `targets.zig`

Emits the generated structs for every message and named type, against
`corelib-zig`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds `build.zig` and the JSON conformance harness. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. |

All five are generic options with the same meaning everywhere; see the
[generic config](README.md). This target has no options of its own.
