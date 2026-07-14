# TypeScript target — `targets.typescript`

Options accepted under `targets.typescript`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `int64` | `bigint` \| `long` \| `number` | `bigint` | Representation of 64-bit integer fields in the generated TS API (see below). All modes are wire-identical. |
| `max_dyn_array_count` | integer | unset (unlimited) | Receiver-side decode limit (generator#102): maximum element count accepted for an **unbounded** array (one without a schema `count`). See below. |
| `max_dyn_string_len` | integer | unset (unlimited) | Receiver-side decode limit: maximum byte length accepted for an **unbounded** string (one without a schema `maxlen`). See below. |
| `max_dyn_blob_len` | integer | unset (unlimited) | Receiver-side decode limit: maximum byte length accepted for an **unbounded** blob (one without a schema `maxlen`). See below. |

### `max_dyn_*` — receiver-side decode limits

The three `max_dyn_*` keys (settable under `generic` or `targets.typescript`)
cap what a *received* message may claim before anything is allocated. They
govern **only** fields the schema left unbounded (`array` without `count`,
`string`/`blob` without `maxlen`); an unset key means unlimited (the previous
behavior). When at least one key is active, the module exports `MAX_DYN_ARRAY_COUNT`
/ `MAX_DYN_STRING_LEN` / `MAX_DYN_BLOB_LEN` constants and every generated
`static decode(bytes)` passes them to its `Cursor` as a corelib `DecodeLimits`
object. Exceeding a cap throws `SofabError` with code
`SofabErrorCode.LimitExceeded` at the count/length header — never a clamp or
truncation. Each cap is raised to the largest schema bound of its kind, so a
schema-bounded field larger than the cap stays governed by its own bound alone
(its over-schema counts are still rejected by the generator#100 guard). A key
whose kind has no unbounded field in the schema is inert and emits nothing;
with no keys set the output is byte-identical to previous releases. The
plumbing is independent of the `int64` mode.

### `int64` — 64-bit field representation

`bigint` (default) · `long` · `number` — à la protobufjs's `int64` option.
All three modes are **wire-identical**; they only change the generated TS API
and its runtime cost. `bigint` in the 64-bit hot path is the dominant cost of
the TS codec, especially on JavaScriptCore (Bun), which optimizes `bigint`
~2.5–4× worse than V8.

| Mode | u64/i64 arrays | u64/i64 scalars |
|---|---|---|
| `bigint` | `bigint[]` | `bigint` |
| `long` | `Long[]` behind a get/set accessor pair | `bigint` |
| `number` | `Long[]` behind a get/set accessor pair | `number` |

**`long`** backs each 64-bit array with a private `Long[]` field (corelib's
`Long` is a `(low, high)` 32-bit word pair) plus an accessor pair:

```ts
private _u64: Long[] = [];
get u64(): Long[] { return this._u64; }
set u64(vals: readonly (Long | bigint | number)[]) { this._u64 = vals.map(Long.fromValue); }
```

Assignment stays ergonomic (`msg.u64 = [1n, 2n]` or plain numbers) and converts
**once**, off the per-encode path; marshal/decode read and write the backing
field directly via the corelib's `write*ArrayLong`/`read*ArrayLong`, so no
`bigint` is created on the hot path. Caveats:

- The setter maps `Long.fromValue` over its input — even an all-`Long` input is
  re-wrapped in a fresh array. Assign whole arrays, or push `Long`s in place
  (`msg.u64.push(Long.fromNumber(7))`): in-place mutation operates on the
  `Long[]` itself.
- `toJSON()` still prints decimal strings (`Long.toString(signed)`), and
  `fromJSON()` still parses via `BigInt` (off the hot path, through the setter).

**`number`** additionally maps 64-bit *scalars* to plain `number`, using the
corelib writers' existing number fast path. Only choose it when every 64-bit
scalar value is guaranteed to fit the ±2^53 safe-integer range — values beyond
that silently lose precision. (Full-range scalars as `Long` need scalar `Long`
codecs in corelib-ts first; until then they stay `bigint` under `long`.)

Measured on the full-scale arena message (best-of-3, corelib-ts #19/#20):

| Mode | Bun/JSC MB/s | vs protobufjs | Node/V8 MB/s | vs protobufjs |
|---|--:|--:|--:|--:|
| `bigint` | 25.5 | 0.66 | 39.2 | 0.90 |
| `long` | 38.0 | 0.95 | 47.3 | 1.17 |
| `number` | 40.2 | 1.04 | 50.8 | 1.18 |
