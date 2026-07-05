# TypeScript target — `targets.typescript`

Language-specific options for the TypeScript backend. For shared options
(`emit`, `file_layout`, `buffer`, …) see the
[`generic`](README.md) config.

## Honored options

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

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`module` · `package_name` · `ts_target` · `node_min` · `bigint_policy`
(subsumed by `int64` for 64-bit representation) · `emit_dts` · `decode_style`
