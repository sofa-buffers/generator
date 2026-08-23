# Python target — `targets.python`

Emits `message.py`: a `@dataclass` per message and per named struct/union, plus
the enums and bitfield constants they use. The generated code calls into
`sofa-buffers-corelib` (import package `sofab`).

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits — are documented in the
[generic config](README.md) and apply here unchanged.
