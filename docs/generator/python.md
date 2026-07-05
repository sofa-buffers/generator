# Python target — `targets.python`

Options accepted under `targets.python`. For shared options (`emit`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

The Python target has **no language-specific options** — it is driven entirely
by the shared generic options.

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. `emit: project` scaffolds a buildable package. |
