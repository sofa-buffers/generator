# Python target — `targets.python`

Emits `message.py`: a `@dataclass` per message and per named struct/union, plus
the enums and bitfield constants they use. The generated code calls into
`sofa-buffers-corelib` (import package `sofab`).

## Options

| key | type | default | effect |
|---|---|---|---|
| `emit` | `sources` \| `project` | `sources` | `project` additionally scaffolds `pyproject.toml`, a JSON conformance harness and a README. |
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. |
| `max_dyn_array_count` | integer | unset | Receiver-side decode limit. |
| `max_dyn_string_len` | integer | unset | Receiver-side decode limit. |
| `max_dyn_blob_len` | integer | unset | Receiver-side decode limit. |

All five are generic options with the same meaning everywhere; see the
[generic config](README.md). This target has no options of its own.
