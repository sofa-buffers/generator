# Python target — `targets.python`

Emits `message.py`: a `@dataclass` per message and per named struct/union, plus
the enums and bitfield constants they use. The generated code calls into
`sofa-buffers-corelib` (import package `sofab`).

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits — are documented in the
[generic config](README.md) and apply here unchanged.

## The reassembly buffer

`sofab.Decoder` joins a construct split across two fed chunks in a buffer the
caller supplies, sized once and never grown, and it holds no size of its own —
the size decides which well-formed messages a receiver can stream, so it is the
receiver's number. The generated module states it, derived from the schema and
the `max_dyn_*` limits:

```python
MAX_FIELD_SPAN = 4194309          # the largest single value this schema carries
REASSEMBLY = MAX_FIELD_SPAN + 65536
```

`MAX_FIELD_SPAN` is one *construct*: a `string` or `blob` payload, or one native
array's whole element run, with every varint charged its widest form and each
`max_dyn_*` limit standing in for a missing schema bound. A nested message is not
one construct — its fields are read one at a time — and neither is a wrapper
array of strings, blobs or structs, whose elements arrive one per sequence child.
Raising a `max_dyn_*` limit raises this number with it.

A field the receiver *skips* — an unknown id, or one whose wire tag contradicts
the schema — never enters the buffer at all, whatever its size, so the number
covers what a receiver reads and not what a sender might send.

`REASSEMBLY` adds room for one fed chunk, because the corelib holds the chunk it
was just handed alongside what it carried. 64 KiB is what `decoder()` assumes; a
caller streaming larger pieces passes its own:

```python
d = Telemetry.decoder(reassembly=MAX_FIELD_SPAN + (1 << 20))
```

The one-shot `Telemetry.decode(data)` needs neither: a message fed in a single
call never touches the buffer, so it is built with `MAX_FIELD_SPAN` — the most a
*truncated* message can leave behind.
