# Java target — `targets.java`

Options accepted under `targets.java`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `package` | string | `message` | The `package <name>;` declaration of the generated classes (and the Maven source directory layout in project mode). |
| `max_dyn_array_count` | integer | unset (unlimited) | Receiver-side decode limit (generator#102): maximum element count accepted for an array the schema left unbounded (no `count`). Baked into the generated visitor as `MAX_DYN_ARRAY_COUNT`; a larger wire count fails the decode with `SofabError.LIMIT_EXCEEDED` at the count header, before allocation — never a clamp. Arrays with a schema `count` are governed by that bound instead (generator#100). |
| `max_dyn_string_len` | integer | unset (unlimited) | Receiver-side decode limit (generator#102): maximum byte length accepted for a string the schema left unbounded (no `maxlen`). Checked against the wire `total` at the top of the visitor's `string()` callback, before any accumulation (single-shot and chunked paths alike); a violation fails the decode with `SofabError.LIMIT_EXCEEDED`. Strings with a schema `maxlen` are unaffected. |
| `max_dyn_blob_len` | integer | unset (unlimited) | Receiver-side decode limit (generator#102): maximum byte length accepted for a blob the schema left unbounded (no `maxlen`). Checked against the wire `total` at the top of the visitor's `blob()` callback, before any accumulation; a violation fails the decode with `SofabError.LIMIT_EXCEEDED`. Blobs with a schema `maxlen` are unaffected. |

The three `max_dyn_*` keys are also accepted under `generic:` (shared across
targets). A configured limit is inert — no constants, no guards, byte-identical
output — when the schema has no unbounded field of its kind. A limit violation
surfaces from `decode()` as a `RuntimeException` and from `tryDecode()` as a
`java.io.UncheckedIOException`, in both cases wrapping the
`SofabException(LIMIT_EXCEEDED)` cause (same shape as the generator#100
over-count rejection). The over-count reject's wrapper-array analogue
(generator#142) throws `SofabException(INVALID_MSG)` the same way when a
`string`/`blob`/`struct`/`union` element array with `count: N` sees a wire
element id `≥ N`, before the `List` grows.

```yaml
targets:
  java:
    package: com.myproj.messages
```

## Benchmark row

Row `java` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
