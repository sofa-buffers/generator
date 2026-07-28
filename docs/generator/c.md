# C target — `targets.c`

Target-specific options, accepted under `targets.c`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `symbol_prefix` | string | `message_` | Prefix applied to every generated C symbol — struct typedefs (`<prefix>Name_t`), descriptor tables, and the encode/decode/init functions. Use it to avoid name collisions when linking generated code from several schemas into one binary. |

```yaml
targets:
  c:
    symbol_prefix: myproj_   # -> myproj_Point_t, myproj_point_encode(), ...
```

That is the whole per-target surface — the generic `namespace` is ignored here,
since C has no namespaces; `symbol_prefix` is its counterpart. The C target has no storage or bounds
option, because it has no choice to offer: the C object model has no dynamic
containers, so every field is schema-sized and every bound is mandatory (next
section). The C++ target's `corelib` / `allow_dynamic` pair has no counterpart
here — for a heap-capable target, generate C++ against the same corelib
(`targets.cpp` with `corelib: c-cpp`, see [cpp.md](cpp.md)).

### Behaviour the corelib decides, not the generator

Several things a C user thinks of as options are compile-time macros on
`corelib-c-cpp`, not config keys, so flipping them never requires regenerating:
`SOFAB_ENABLE_STRICT_UTF8` (validate `string` payloads), `SOFAB_ENABLE_SKIP_COUNTER`
(count fields skipped on a §7.3 type contradiction), `SOFAB_OBJECT_DESCR_PROFILE`
(descriptor integer widths), `SOFAB_DISABLE_INT64_SUPPORT` and the
`SOFAB_DISABLE_{FIXLEN,ARRAY,SEQUENCE}_SUPPORT` feature gates. See that repo's
README for the footprint each one costs or saves.

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
capacities before it can be generated for C. `count` itself never goes on the
wire, and it is encoding-neutral: it is a **capacity**, so it bounds the storage
and rejects an over-long message, but it never adds an element to a value (see
below).

## `count` is a capacity: every array carries a length

MESSAGE_SPEC §3/§5.1 make `count: N` the array's **capacity**, never its length.
The wire count `M` **is** a compact array's length; a wrapper array's length is
*highest present id + 1*; and nothing that carries the length may be elided —
`[1,2,3,0,0]` and `[1,2,3]` are different values, so no trailing-default run is
trimmed and no decoder fills `[M, N)`. Inside an array the *interior* is sparse
(an element equal to its element default is skipped, leaf and sequence form
alike, leaving an id gap) and the **last** element is always written — as its
value, or as an empty frame.

C is the one target where **none of that is emitted by the generator**. Every
other backend writes its own array call; C emits only a struct plus a static
descriptor table, and `object.c` (corelib-c-cpp 0.8.x, commits `45a857d` +
`55d5161`) implements the whole rule from the descriptor. What the generator owes
it is the one thing a descriptor cannot infer — **the length** — because
`SOFAB_OBJECT_FIELD_ARRAY` / `SOFAB_OBJECT_DESCR_SEQ` derive the element count
*structurally* from `sizeof(field)/sizeof(field[0])` and from `field_count`,
i.e. from the capacity and nothing else. A capacity-only object can hold no array
shorter than `N`, and a decode of `M < N` re-encodes as `N`.

So every array form is emitted in its **length-carrying** variant: a companion
member declared immediately before the storage, whose width the descriptor
records.

**Compact (numeric/enum/boolean/bitfield) arrays** — `SOFAB_OBJECT_FIELD_ARRAY_SIZED`:

```c
typedef struct { …; uint32_t <name>_len; uint32_t <name>[N]; …; } message_M_t;
…
SOFAB_OBJECT_FIELD_ARRAY_SIZED(id, message_M_t, <name>, <name>_len,
                               SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED)
```

Encode writes exactly `<name>_len` elements — trailing element defaults included;
decode stores the received `M` back into it. `M > N` stays `SOFAB_RET_E_INVALID_MSG`.

**Wrapper-array holders** (`string`/`struct`/`union` elements, and an array of
arrays whose inner element is itself a wrapper) — `SOFAB_OBJECT_DESCR_SEQ_SIZED`:

```c
typedef struct { uint8_t len; char items[N][maxlen + 1]; } message_M_sa_elems_t;
…
SOFAB_OBJECT_DESCR_SEQ_SIZED(fields, N, NULL, 0, message_M_sa_elems_t, items[0], len)
```

`len` holds `0..N`; encode walks the slots `[0, len)` and always writes the one
at `len - 1`; decode stores *highest present id + 1* back into it, so a received
`["a"]` re-encodes as one element instead of growing back to `N`. `len == 0` is
the empty array, which the enclosing object's ≠-default test omits whole (the
canonical encoding, §2).

### How the length's width is chosen

The descriptor stores only the length's **width** and reads it at
*storage offset − width*, so the two members have to be **adjacent** — and unlike
a sized blob's byte buffer (alignment 1, which abuts any width) an element slot
can be aligned strictly enough to pad a narrower length away from it:
`{ uint8_t len; uint32_t v[4]; }` puts `v` at offset 4, three bytes past the
length. The generator therefore picks

> width = max(narrowest width holding `0..count`, the element's alignment)

— `uint8_t` for byte-wide elements and for a `string` holder (`char[]`, alignment
1), `uint32_t` for a `u32`/`fp32` array or a struct holder whose element's widest
member is 4 bytes, `uint64_t` for `u64`/`fp64`/8-byte-aligned struct elements.
Both inputs are powers of two ≤ 8, so the wider one is too, and the storage that
follows is automatically aligned. corelib-c-cpp asserts the adjacency at compile
time anyway (`SOFAB_OBJECT_ASSERT_LEN_ADJACENT`, a negative array bound), so a
mistake here is a build error rather than a silent misread.

An 8-byte length is read through the corelib's `_load_uint`, whose 8-byte case
`SOFAB_DISABLE_INT64_SUPPORT` compiles out — so a schema that forces one also
emits the `SOFAB_DISABLE_INT64_SUPPORT` capability guard, even when no field is a
64-bit integer.

### Two holders that cannot carry a length

A `blob` array element and a native inner-array **row** are themselves
length-carrying (issues #128/#130): each keeps its own used-length in the byte
immediately before its buffer — which is the one address the holder's element
count would have to occupy. Those two holders therefore stay
`SOFAB_OBJECT_DESCR_SEQ` (un-sized): their value occupies every slot, the last
index is `count - 1`, and the lengths `1 … N-1` are not expressible. Both round
trip exactly (an interior default element is omitted and reconstructed in place,
an all-default holder is omitted whole as the empty array); what is lost is only
the ability to *say* "two of a possible three" for those two element kinds. Each
row/element keeps its own `SOFAB_OBJECT_FIELD_ARRAY_SIZED` /
`SOFAB_OBJECT_FIELD_BLOB_SIZED`, so an inner array's own length is exact.

### Declared array defaults are not padded

A `default` shorter than `count` is an array of **its own** length: `count: 5,
default: [1,2,3]` is the three-element `[1,2,3]`, not `[1,2,3,0,0]`. The length is
part of the default image (`.<name>_len = 3` beside `.<name> = { 1, 2, 3 }`), which
`sofab_object_init` seeds from — and it is emitted even when every declared element
is zero, because `[0,0,0]` is the length-3 array of zeros and not the empty array.

**Minimum corelib.** `SOFAB_OBJECT_FIELD_ARRAY_SIZED` and
`SOFAB_OBJECT_DESCR_SEQ_SIZED` arrived in corelib-c-cpp 0.8.x; generated C does
not compile against an older one. This is the flag day of the `count`-is-a-capacity
change — the earlier trim-on-encode / fill-on-decode pair (Crucible F-0010,
sofabgen 0.17.2) is gone from both sides.

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

**Blob arrays.** A `blob` *array* element is a sized blob too (issue #130): the
wrapper-sequence holder stores each element as a `struct { <len>; uint8_t
buf[maxlen]; } items[count]` (the length immediately before each byte buffer) and
emits a per-element `SOFAB_OBJECT_FIELD_BLOB_SIZED(i, holder, items[i].buf,
items[i].len)`, so a sub-`maxlen` element keeps its exact length. A `used_len == 0`
element is omitted by index, so an empty element round-trips in place (the gap is
preserved). A `string` array element stays `char items[count][maxlen + 1]` — it
recovers its length from the NUL, so it needs no companion.

## Benchmark row

Row `c` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **toggle** method. Tracked: Ir/op + `.text`/`.data`/`.bss` on ARMv6-M, ARMv7-M+fp.dp, RV32IMC.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.
