# C target — `targets.c`

Emits C structs and a static descriptor table per message and named type,
against `corelib-c-cpp`. This is a footprint target: storage is sized from the
schema and the generated code allocates nothing, so every `string`, `blob` and
`array` must declare a `maxlen` or `count` — an unbounded field fails
generation.

## Options

| key | type | default | effect |
|---|---|---|---|
| `symbol_prefix` | string | `message_` | Prefix on every generated C symbol. |

`emit`, `license` and `max_message_size` apply here too; see the
[generic config](README.md). The `max_dyn_*` decode limits are **not** accepted
by this target — see there for why.

## `symbol_prefix`

C has one flat namespace, so every generated name carries this prefix: struct
typedefs, the descriptors, the macros, and the `encode` / `decode` / `init`
functions. A message `reading` with the default prefix becomes the type
`message_reading_t` with `message_reading__encode`, and so on (see
[Generated names](#generated-names)).

Set it to something project-specific when the generated code is linked
alongside other C in the same binary — two schemas generated with the same
prefix and an overlapping message name collide at link time. The prefix must
start with a letter.

## Generated names

Schema names are kept as they are, case included. A name never contains `__`,
so the generator joins the parts of a name with underscore runs that no schema
name has, and every schema the validator accepts generates without two names
colliding:

- a **path** — a message or `$defs` name, then the field and option names down
  to an inline type — is joined with `___`;
- something the generator adds to a type (a function, the decoder, the
  descriptor, the element holder of an array) follows the type with `__`;
- macros are upper-cased.

| Generated for | Name (message `m`, default prefix) |
|---|---|
| message `m` | `message_m_t` |
| inline struct of field `a` of `m`, or struct element of an array field `a` | `message_m___a_t` |
| `$defs` struct `point` | `message_point_t` |
| functions | `message_m__init`, `message_m__encode`, `message_m__encode_to`, `message_m__decode` |
| incremental decoder | `message_m__decoder_t`, `message_m__decoder_init`, `message_m__decoder_feed` |
| descriptor | `message_m__descr`, `message_m___a__descr`, … |
| element holder of the array field `arr` (strings, blobs, structs, unions, rows) | `message_m___arr__elems_t` |
| include guard, size | `MESSAGE_M__H`, `MESSAGE_M__MAX_SIZE` |
| files | `m_sofab.h`, `m_sofab.c` |

The file name is the message name in lower case plus `_sofab`, so a message
named like a header the build includes (`stdint`, `string`) cannot shadow it.
Names that start with `_` (`_message_m__fields`, …) are the generator's own and
are not meant to be used.

A type name that the prefix makes equal to a typedef of the C library or the
corelib gets a trailing underscore: with `symbol_prefix: sofab_`, a message
`ostream` is `sofab_ostream_t_` (the corelib's is `sofab_ostream_t`), while its
functions keep `sofab_ostream__init`, … . The typedefs covered are those of
the C standard headers (`size_t`, `uint8_t` and the other `<stdint.h>` types,
`wchar_t`, …), the common POSIX ones (`ssize_t`, `off_t`, `time_t`, `pid_t`, …)
and every typedef of the corelib (`sofab_ostream_t`, `sofab_istream_t`,
`sofab_ret_t`, …).

## Booleans

A `boolean` field, and each element of a `boolean` array, is a `uint8_t`.

- **Decode.** Every non-zero wire value, however wide, is stored as `1`.
- **Encode.** The member is written as it is stored. Keep it at `0` or `1` so
  the message carries the canonical `true`.

## Bitfields

A `bitfield` field is a raw unsigned integer, sized from the highest declared
`pos` — it is never narrowed to only the declared flags, so a value a newer
peer's vocabulary set (a bit position this schema doesn't name) still
round-trips instead of being rejected or masked away.

Each declared bit still gets a name: a `#define` beside the struct, so a caller
sets/tests bits by name instead of by position:

```c
out.alarms = MESSAGE_FRIDGE___ALARMS___TEMP_HIGH | MESSAGE_FRIDGE___ALARMS___POWER_LOSS;
if (in.alarms & MESSAGE_FRIDGE___ALARMS___TEMP_HIGH) { ... }
```

The name is `symbol_prefix`, then the path of the bitfield's definition, then
the flag, joined with `___` and upper-cased:

| Bitfield declared as | Macro |
|---|---|
| field `top` of message `m`, flag `x` | `MESSAGE_M___TOP___X` |
| element of array field `arr` of message `m`, flag `y` | `MESSAGE_M___ARR___Y` |
| field `inner` of an inline struct field `n` of message `m`, flag `z` | `MESSAGE_M___N___INNER___Z` |
| field `st` of `$defs/struct/Pt`, flag `ok` | `MESSAGE_PT___ST___OK` |
| `$defs/bitfield/Shared`, flag `a` | `MESSAGE_SHARED___A` |

A bitfield from `$defs` carries no message or field name, whichever field uses
it. It gets one `#define` block per header, however many fields share it.

Each macro has the width of its field, at least `uint32_t`:
`((uint32_t)1 << pos)`, or `((uint64_t)1 << pos)` when the field is a
`uint64_t`. So `v &= ~FLAG` clears only that flag, at any width.

C has one macro namespace across all included headers. A flag macro joins
its parts with `___` and a message's own macros (`__H`, `__MAX_SIZE`) with
`__`, which no schema name contains, so no two of them can meet.

## Field names

A field's struct member is the field's schema name. C has no way to escape a
reserved name, so a field named after a C keyword gets a trailing underscore —
the field `int` is the member `int_`. The keywords include those C23 added
(`nullptr`, `typeof`, `constexpr`, …) and `bool`/`true`/`false`, which are
macros before C23. The same goes for the macros of the standard headers the
generated code includes — `NULL`, `EOF`, `UINT8_MAX` and the other `<stdint.h>`
limits, `EXIT_SUCCESS`, … — which the preprocessor would otherwise replace,
`linux`/`unix`, which gcc predefines outside its ISO modes, and every name
starting with `SOFAB_`, the corelib's macros and build switches. Other
platform-specific macros are not covered; building the generated sources in an
ISO mode (`-std=c99`, `-std=c11`, `-std=c23`) avoids them.
Only the member changes: the wire is keyed by the field id, and the JSON key
stays the schema name.

A sized blob and a native array carry their length in a sibling member,
`<field>__len` (`b__len` for a blob `b`), which no field name can spell, so a
field `b_len` beside it is simply another member. Every macro the generator
defines contains `__` too, so no field is replaced by one.

## Unions

A `union` holds exactly one of its options. It is a tag, `which`, followed by a
C `union` of the options:

```c
typedef struct {
    sofab_object_descr_id_t which;  /* the held option: one of the *__ID macros */
    union {
        uint16_t num;
        char name[17];
        message_m___shape___pt_t pt;                         /* struct option: its own type */
        struct { uint8_t len; uint8_t data[8]; } raw;        /* blob option */
        struct { uint16_t len; uint16_t items[4]; } vals;    /* array option */
        message_m___shape___names__elems_t names;            /* array of strings: a holder */
    } u;
} message_m___shape_t;
```

A `blob` or `array` option keeps its length inside the option
(`x.u.raw.len`, `x.u.vals.len`), because the options share their storage.

Each option id has a `#define`: `symbol_prefix`, the path of the union's
definition and the option joined with `___`, then `__ID`, all upper-cased —
`MESSAGE_M___SHAPE___PT__ID` for option `pt` of the union field `shape` of
message `m`, `MESSAGE_SHAPE___PT__ID` for a union from `$defs/union/Shape`. A
`$defs` union used with different `default_id`s is one type per `default_id`,
named `<name>__default_<option>`: `message_Shape__default_pt_t`. Its variants
share the option-id macros.

| operation | C |
|---|---|
| which option is held | `x.which` |
| test | `x.which == MESSAGE_M___SHAPE___PT__ID` |
| read the held option | `x.u.pt`, `x.u.num`, … |
| select a scalar / string / blob / array option | set `which`, then the value (for a blob or array: the bytes and `len`) |
| select a struct, union or array-of-string/blob/struct option at its default | set `which`, then `sofab_object_init(&<option descriptor>, &x.u.<option>)` |
| edit the held option in place | write through `x.u.<option>` |
| back to the default | `<prefix><message>__init` (the whole message) |

```c
message_m_t msg;
message_m__init(&msg);                           /* shape holds its default option */

msg.shape.which = MESSAGE_M___SHAPE___NUM__ID;   /* select num = 7 */
msg.shape.u.num = 7;

msg.shape.which = MESSAGE_M___SHAPE___PT__ID;    /* select pt at its declared default */
sofab_object_init(&message_m___shape___pt__descr, &msg.shape.u.pt);
msg.shape.u.pt.y = 2;                            /* ... and edit it in place */

if (msg.shape.which == MESSAGE_M___SHAPE___PT__ID) { use(msg.shape.u.pt.x); }
```

The header declares the descriptor of every option that is a struct, a union
or an array of string/blob/struct/array, for exactly that
`sofab_object_init` call. A scalar, string, blob or compact-array option needs
none: writing its value is all there is to select it.

Selecting an option overwrites the storage of the one held before — there is
nothing else to clear, and nothing to free. A pointer into `x.u.<option>` taken
earlier points into the same bytes afterwards, now holding the other option.

**Defaults.** `_init` puts every union at its `default_id` option, at that
option's own default (an omitted `default_id` means the option with the lowest
id). Each element of an array of unions starts the same way, and so does an
element a decoded array skips.

**Encode.** The held option is the only one written. If it is the
`default_id` option at its default, the union is at its default and is left
out. Any other held option is written **even at its own default** — `0`, an
empty string, an empty blob or array, or an empty frame for a struct option —
because the receiver's fresh union holds `default_id`, and leaving it out
would read back as that.

**Decode.** The option received last wins. A different option replaces the
held one and starts from its own default; the same struct option received again
continues where it was; a field whose wire type does not match its option, and
an unknown id, change nothing.

**JSON (project harness).** A union is an object with exactly one member, the
held option: `{"pt": {"x": 7, "y": 2}}`, also when that is the `default_id`
option at its default.

**Corelib cost.** The union walk lives in `corelib-c-cpp`'s object API and is
part of every build of it that keeps sequence support, whether or not the
schema has a union: about 100&nbsp;B of flash on Cortex-M and RV32, about
220&nbsp;B on AVR, and a small per-field cost at run time. It has no switch of
its own; a corelib built with `SOFAB_DISABLE_SEQUENCE_SUPPORT` leaves it out,
and a schema with a union (or any struct) cannot use such a corelib.

A header whose message uses a union refuses to compile against a corelib built
with `SOFAB_DISABLE_SEQUENCE_SUPPORT`, and against a corelib that predates
unions (one without `SOFAB_OBJECT_DESCR_UNION`), each with an `#error` naming
the cause.
