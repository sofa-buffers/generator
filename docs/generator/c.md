# C target — `targets.c`

Options accepted under `targets.c`. For shared options (`emit`, `namespace`,
`tool_banner`, `license`, …) see the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `emit` | `sources` \| `project` | `sources` | See [generic config](README.md); per-target override. |
| `symbol_prefix` | string | `message_` | Prefix applied to every generated C symbol — struct typedefs (`<prefix>Name_t`), descriptor tables, and the encode/decode/init functions. Use it to avoid name collisions when linking generated code from several schemas into one binary. |

```yaml
targets:
  c:
    symbol_prefix: myproj_   # -> myproj_Point_t, myproj_point_encode(), ...
```

## Every field must be bounded (no dynamic containers)

The C object model has **no dynamic containers**, so every field must be sized by
the schema: every `string`/`blob` needs a `maxlen` and every `array` a `count`
(at every nesting level, for every element kind — including a plain numeric
array; a `string`/`blob` array element also needs its own element `maxlen`). An
unbounded field is a **hard generation error** that names the offending field,
e.g.:

```
c: field "somemap" of "myfirstmessage" has no count; the fixed-storage C target
requires a bound on every string/blob (maxlen) and array (count) — the C object
model has no dynamic-container fallback
```

Unlike the C++ `c-cpp` and Rust `no_std` fixed-capacity profiles there is **no
`allow_dynamic` escape** for C: a schema with a genuinely dynamic collection (a
`count`-less map, say) is a heap-target schema, and must be given explicit
capacities before it can be generated for C. `count` never goes on the wire, so
adding one keeps the encoding byte-identical to every other target.

## Struct member order (widest-first)

The members of a generated `<prefix>Name_t` struct are declared **widest-first**
(8→4→2→1-byte alignment; strings, blobs, arrays and nested types rank as 8),
not in schema order, so the compiler inserts less padding between them. Fields
of equal alignment keep their schema order. This affects **declaration order
only** — encode and the descriptor table iterate in schema/field-id order, so
the wire bytes are byte-identical to every other target. Initialize structs by
member name (`_init()` or designated initializers), not positionally.

## String storage (`maxlen + 1`)

A `string` field with `maxlen: N` is stored as `char <name>[N + 1]` — one extra
byte beyond the schema bound. The corelib reads strings as NUL-terminated
(`sofab_istream_read_string` reserves one byte for the `'\0'`, rejecting a wire
length greater than `capacity - 1`), so the `+1` makes the **usable** capacity
equal the schema bound: a wire string of exactly `maxlen` bytes is accepted, and
`maxlen + 1` is still rejected as `SOFAB_RET_E_INVALID_MSG`. The same `+1`
applies to `string` elements of an array (`char items[count][maxlen + 1]`).

## Blob storage (sized blob)

A `blob` is opaque bytes and may be shorter than its `maxlen`, so — unlike a
NUL-terminated string — a bare `uint8_t <name>[maxlen]` cannot recover the used
length: it would re-encode the full `maxlen` (zero-padded) and collapse an
all-zero short blob to empty (silent round-trip data loss, issue #128). A `blob`
field with `maxlen: N` is therefore lowered as a **sized blob** — a companion
used-length member immediately before the buffer, plus the
`SOFAB_OBJECT_FIELD_BLOB_SIZED` descriptor:

```c
typedef struct { …; uint8_t <name>_len; uint8_t <name>[N]; …; } message_M_t;
…
SOFAB_OBJECT_FIELD_BLOB_SIZED(id, message_M_t, <name>, <name>_len)
```

The length member's width is the narrowest unsigned type holding `0..N`
(`uint8_t`/`uint16_t`/`uint32_t`/`uint64_t`). It **must** immediately precede the
buffer (`offsetof(dfield) == offsetof(lfield) + sizeof(lfield)`), which the
generator guarantees by emitting the pair as one adjacent declaration; a byte
buffer has alignment 1, so it always abuts the length with no padding, for any
width and any `N`. On encode only `<name>_len` bytes reach the wire; on decode
the received length is stored back into `<name>_len`. This is the C counterpart
of C++ `sofab::FixedBytes<N>`, and it produces byte-identical wire to a plain
blob of the same actual length.

Because `<name>_len` is **not** a descriptor field, `sofab_object_init` does not
touch it; the generated `<pfx>_init` therefore `memset`s the whole struct first
(so every length starts at 0) and then materialises the used-length of any blob
with a non-empty schema default.

**Blob default & omission caveat.** The corelib's sized-blob omission is
*length-driven*: a blob is omitted from the wire only when `used_len == 0`
(empty), never by comparing content against a default image (the buffer past
`used_len` is indeterminate). So a `blob` with a non-empty schema `default`
materialises to that default on `init`/decode-of-omitted (value parity with the
other backends), but the C encoder **transmits** it rather than omitting it when
the value equals the default — a benign, wire-compatible divergence (every
backend decodes those bytes to the same value). A nested (struct-field) blob's
non-empty default is not materialised (it would need a companion-length write the
top-level `_init` doesn't reach); it decodes as empty. No corpus schema relies on
this.

> **Known gap — blob arrays.** A `blob` *array* element
> (`SOFAB_OBJECT_FIELD(..., BLOB)` over `uint8_t items[count][maxlen]`) is **not
> yet** sized, so a sub-`maxlen` element still round-trips zero-padded to
> `maxlen` (the same class of bug as #128, tracked separately). Only scalar and
> struct-field blobs carry a used-length today.
