# Python target — `targets.python`

Options accepted under `targets.python`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. `emit: project` scaffolds a buildable package. |
| `max_dyn_array_count` | integer | unset = unlimited | Receiver-side decode limit (generator#102): caps the wire element count of arrays the schema left unbounded (no `count`). Baked as `MAX_DYN_ARRAY_COUNT` and passed to the corelib `Decoder(max_array_count=…)`; exceeding it raises `SofaLimitError` before allocation. Raised to the largest schema `count`, so schema-bounded arrays stay governed by their own bound. |
| `max_dyn_string_len` | integer | unset = unlimited | Same, for strings without a schema `maxlen` (`MAX_DYN_STRING_LEN`, `Decoder(max_string_len=…)`). |
| `max_dyn_blob_len` | integer | unset = unlimited | Same, for blobs without a schema `maxlen` (`MAX_DYN_BLOB_LEN`, `Decoder(max_blob_len=…)`). |
