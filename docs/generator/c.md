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
typedefs, the descriptor tables, and the `encode` / `decode` / `init` functions.
A message `reading` with the default prefix becomes `message_reading`,
`message_reading_encode`, and so on.

Set it to something project-specific when the generated code is linked
alongside other C in the same binary — two schemas generated with the same
prefix and an overlapping message name collide at link time.

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
out.alarms = MESSAGE_FRIDGE_ALARMS_TEMP_HIGH | MESSAGE_FRIDGE_ALARMS_POWER_LOSS;
if (in.alarms & MESSAGE_FRIDGE_ALARMS_TEMP_HIGH) { ... }
```

The name is `symbol_prefix`, then the path of the bitfield's definition, then
the flag, all upper-cased:

| Bitfield declared as | Macro |
|---|---|
| field `top` of message `m`, flag `x` | `MESSAGE_M_TOP_X` |
| element of array field `arr` of message `m`, flag `y` | `MESSAGE_M_ARR_ELEM_Y` |
| field `inner` of an inline struct field `n` of message `m`, flag `z` | `MESSAGE_M_N_INNER_Z` |
| field `st` of `$defs/struct/Pt`, flag `ok` | `MESSAGE_STRUCT_PT_ST_OK` |
| `$defs/bitfield/Shared`, flag `a` | `MESSAGE_BITFIELD_SHARED_A` |

A bitfield from `$defs` carries no message or field name, whichever field uses
it. It gets one `#define` block per header, however many fields share it.

Each macro has the width of its field, at least `uint32_t`:
`((uint32_t)1 << pos)`, or `((uint64_t)1 << pos)` when the field is a
`uint64_t`. So `v &= ~FLAG` clears only that flag, at any width.

C has one macro namespace across all included headers. If a flag macro would
match another generated macro anywhere in the schema, generation fails and
names both owners. The other macro can be a message's include guard
(`..._H`), its `..._MAX_SIZE`/`..._MAX_SIZE_LIMIT`, or another flag.

## Field names

A field's struct member is the field's schema name. C has no way to escape a
reserved name, so a field named after a C keyword gets a trailing underscore —
the field `int` is the member `int_`. The keywords include those C23 added
(`nullptr`, `typeof`, `constexpr`, …) and `bool`/`true`/`false`, which are
macros before C23. The same goes for the macros of the standard headers the
generated code includes — `NULL`, `EOF`, `UINT8_MAX` and the other `<stdint.h>`
limits, `EXIT_SUCCESS`, … — which the preprocessor would otherwise replace,
and `linux`/`unix`, which gcc predefines outside its ISO modes. Other
platform-specific macros are not covered; building the generated sources in an
ISO mode (`-std=c99`, `-std=c11`, `-std=c23`) avoids them.
Only the member changes: the wire is keyed by the field id, and the JSON key
stays the schema name.

A sized blob and a native array carry their length in a sibling member,
`<field>_len`. A field that ends up with the same member as another — a field
`b_len` beside a blob `b`, or `int_` beside a mangled `int` — fails generation,
naming both. So does a field named after a macro the generator defines itself
(`MESSAGE_M_MAX_SIZE`, an include guard, an option id): rename the field or
change `symbol_prefix`. The list lives in `generators/c/reserved.go`.

## Unions

A `union` holds exactly one of its options. It is a tag, `which`, followed by a
C `union` of the options:

```c
typedef struct {
    sofab_object_descr_id_t which;  /* the held option: one of the *_ID macros */
    union {
        uint16_t num;
        char name[17];
        message_m_shape_pt_t pt;                             /* struct option: its own type */
        struct { uint8_t len; uint8_t data[8]; } raw;        /* blob option */
        struct { uint16_t len; uint16_t items[4]; } vals;    /* array option */
        message_m_shape_names_elems_t names;                 /* array of strings: a holder */
    } u;
} message_m_shape_t;
```

A `blob` or `array` option keeps its length inside the option
(`x.u.raw.len`, `x.u.vals.len`), because the options share their storage.

Each option id has a `#define`, named like a bitfield flag: `symbol_prefix`,
the path of the union's definition, the option, then `_ID`, all upper-cased —
`MESSAGE_M_SHAPE_PT_ID` for option `pt` of the union field `shape` of message
`m`, `MESSAGE_UNION_SHAPE_PT_ID` for a union from `$defs/union/Shape`. A
`$defs` union used with different `default_id`s is one type per `default_id`,
named `<Name>_default_<option>`: `message_union_Shape_default_pt_t` with the
macros `MESSAGE_UNION_SHAPE_DEFAULT_PT_*_ID`. If an option macro would match
another generated macro anywhere in the schema, generation fails and names both
owners, as it does for bitfield flags.

| operation | C |
|---|---|
| which option is held | `x.which` |
| test | `x.which == MESSAGE_M_SHAPE_PT_ID` |
| read the held option | `x.u.pt`, `x.u.num`, … |
| select a scalar / string / blob / array option | set `which`, then the value (for a blob or array: the bytes and `len`) |
| select a struct, union or array-of-string/blob/struct option at its default | set `which`, then `sofab_object_init(&<option descriptor>, &x.u.<option>)` |
| edit the held option in place | write through `x.u.<option>` |
| back to the default | `<prefix><message>_init` (the whole message) |

```c
message_m_t msg;
message_m_init(&msg);                      /* shape holds its default option */

msg.shape.which = MESSAGE_M_SHAPE_NUM_ID;  /* select num = 7 */
msg.shape.u.num = 7;

msg.shape.which = MESSAGE_M_SHAPE_PT_ID;   /* select pt at its declared default */
sofab_object_init(&_message_descr_named_m_shape_pt, &msg.shape.u.pt);
msg.shape.u.pt.y = 2;                      /* ... and edit it in place */

if (msg.shape.which == MESSAGE_M_SHAPE_PT_ID) { use(msg.shape.u.pt.x); }
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
