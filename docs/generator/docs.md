# Documentation target — `targets.docs`

Renders the schema itself rather than code: one page documenting every message
and named type, with their fields, ids, types, defaults and bounds.

## Options

| key | type | default | effect |
|---|---|---|---|
| `format` | `html` | `html` | Output format. |

## `format`

`html` is currently the only value. It produces a **single self-contained page**
— inline CSS, no external assets, no scripts — so the file can be opened from
disk, committed, or served as-is without anything alongside it.

The key is accepted now so that adding a second format later does not change the
shape of an existing config.
