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

## Benchmark row

Row `csharp` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Strict UTF-8 (issue #85)

`string` is a Unicode type, so it is **always strict** (MESSAGE_SPEC §8 /
CORELIB_PLAN §6.4) — no config key in generated code. The default
`Encoding.UTF8.GetString` is **lossy** (replacement-fallback → `U+FFFD`), which §8
forbids in every mode, so the visitor decodes through a generated `_Utf8(...)` helper
backed by `new System.Text.UTF8Encoding(false, /*throwOnInvalidBytes*/ true)`; a
`DecoderFallbackException` becomes `SofabException(SofabError.InvalidMessage)` — the
same channel as the over-count guards. The check runs once the full `total` bytes
are present. Encode-side strictness is corelib-side (`OStream.WriteString`).
