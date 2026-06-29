# TypeScript target — `targets.typescript`

Language-specific options for the TypeScript backend. For shared options
(`emit`, `file_layout`, `buffer`, `omit_defaults`, …) see the
[`generic`](README.md) config.

## Honored options

The TypeScript target currently has **no language-specific options** that change
its output — it is driven entirely by the shared `generic` options. (`emit:
project` scaffolds a buildable package; `omit_defaults` controls default
omission.)

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`module` · `package_name` · `ts_target` · `node_min` · `bigint_policy` ·
`emit_dts` · `decode_style`
