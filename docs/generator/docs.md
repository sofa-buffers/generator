# Docs target — `targets.docs`

The `docs` target is the one non-code backend: instead of source files it
renders the message definitions as **human-readable reference documentation**.

```sh
sofabgen --lang docs --in examples/messages/example.yaml --out out/docs
# -> out/docs/message.html
```

The HTML output is a **single self-contained page** (`message.html`) — inline
CSS, no external assets, light/dark via `prefers-color-scheme` — so it can be
attached to CI artifacts, mailed around, or dropped on a static server as-is.
It documents:

- every **message** with its summary and a field table (id, name, type,
  default, unit, description, deprecation),
- every **named type** in the shared graph (structs, unions, enums, bitfields —
  `$defs` and inline), cross-linked from the fields that use them,
- type details as authored: `maxlen` bounds, array capacities (`u32[4]`,
  `string[5] (maxlen 16)`, dynamic `[]`), enum/union defaults resolved to their
  constant/option names.

All definition text (summaries, descriptions) passes through HTML-escaped, so
a definition can never inject markup into the page.

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `format` | `html` | `html` | Documentation output format. `html` is currently the only format; the key exists so further formats can be added without a config break. |

Of the shared generic options, `docs` honors `input_dir`, `output_dir`,
`tool_banner` (the generated-by comment and page footer), and `license` (an
`SPDX-License-Identifier` comment at the top of the page). `emit` and
`namespace` do not apply — there is no project scaffold and nothing to
namespace.
