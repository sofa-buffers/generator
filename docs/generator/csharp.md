# C# target — `targets.csharp`

Options accepted under `targets.csharp`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `namespace` | string | `Message` | The `namespace <name>` wrapping the generated classes. Also settable in `generic`. |
| `max_dyn_array_count` | integer | unset = unlimited | Receiver-side decode limit (generator#102): caps the wire element count of arrays the schema left unbounded (no `count`). Baked as `MaxDynArrayCount` with a per-field guard in the generated visitor; exceeding it throws `SofabException(SofabError.LimitExceeded)` at the count header, before allocation. Schema-bounded arrays are untouched — they keep only their generator#100 schema-capacity guard (and, for `string`/`blob`/`struct`/`union` element arrays, the generator#142 over-index guard that throws `SofabException(InvalidMessage)` when a wire element id is `≥ count`). |
| `max_dyn_string_len` | integer | unset = unlimited | Same, for strings without a schema `maxlen` (`MaxDynStringLen`, checked against the wire `total` before any bytes are accumulated). |
| `max_dyn_blob_len` | integer | unset = unlimited | Same, for blobs without a schema `maxlen` (`MaxDynBlobLen`, checked against the wire `total` before any bytes are accumulated). |

```yaml
targets:
  csharp:
    namespace: MyProj.Messages
```
