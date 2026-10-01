# Documentation target — `targets.docs`

Renders the schema itself rather than code: one page documenting every message
and named type, with their fields, ids, types, defaults and bounds.

## Options

| key | type | default | effect |
|---|---|---|---|
| `format` | `html` | `html` | Output format. |

This target takes no other options — not even `emit`; there is no project to
scaffold around a document.

## `format`

`html` is currently the only value. It produces a **single self-contained page**
— inline CSS, no external assets, no scripts — so the file can be opened from
disk, committed, or served as-is without anything alongside it.

The key is accepted now so that adding a second format later does not change the
shape of an existing config.

## Unions

A union type gets its own section, badged `union`, whose table lists the
options. The note under the heading reads the union as **one of** its options:

* exactly one option is held at a time;
* a fresh value holds the **default option** at that option's own default. The
  note names it with its id and says where it comes from: `default_id` when a
  field that uses the union writes one, or *implicit* when none does — the
  lowest option id then applies. That option's row carries a `default` badge;
* a held option other than the default is always written, even at its own
  default, and when a message carries several options the last one received
  wins.

A field of union type shows the same default option in its **Default** column,
e.g. `pt (id 2)`, or `num (id 0, implicit)` when that field omits `default_id`.

A union defined once under `$defs` and used by fields with different
`default_id`s is documented once per default, titled `<name> (default <option>)`
— one section for each type the code targets generate — and each field links
to the one it uses. Every named type is titled by its schema path: a `$defs`
type by its name, an inline type by the dotted path to the field that declares
it (`myfirstmessage.somestruct`). An array of unions links to its element's union section; every element
the message does not carry holds that section's default option.
