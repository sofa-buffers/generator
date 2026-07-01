# Python target — `targets.python`

Language-specific options for the Python backend. For shared options (`emit`,
`file_layout`, `buffer`, …) see the [`generic`](README.md)
config.

## Honored options

The Python target currently has **no language-specific options** that change its
output — it is driven entirely by the shared `generic` options. (`emit: project`
scaffolds a buildable package)

## Reserved options

Accepted by the schema validator but not yet honored by the generator:

`package` · `python_min` · `class_style` · `frozen` · `type_hints` ·
`decode_style`
