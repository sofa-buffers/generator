# C# target — `targets.csharp`

Target-specific options, accepted under `targets.csharp`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `namespace` | string | `Message` | The `namespace <name>` wrapping the generated classes. Also settable in `generic`. |

```yaml
targets:
  csharp:
    namespace: MyProj.Messages
```

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `MaxDynArrayCount`,
`MaxDynStringLen` and `MaxDynBlobLen` constants, checked against the wire count
or `total` before allocation. A violation throws `SofabException` carrying
`SofabError.LimitExceeded`.

## Benchmark row

Row `csharp` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Strict UTF-8 (issue #85)

`string` is a Unicode type, so it is **always strict** (MESSAGE_SPEC §8 /
CORELIB_PLAN §6.4) — no config key in generated code. The default
`Encoding.UTF8.GetString` is **lossy** (replacement-fallback → `U+FFFD`), which §8
forbids in every mode, so the visitor decodes through a generated `_Utf8(...)` helper
backed by `new System.Text.UTF8Encoding(false, /*throwOnInvalidBytes*/ true)`; a
`DecoderFallbackException` becomes `SofabException(SofabError.InvalidMessage)` — the
same channel as the over-count guards. The check runs once the full `total` bytes
are present. Encode-side strictness is corelib-side (`OStream.WriteString`).

## §7.3: a mis-typed array header (issues #183, #193, #254)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not the array kinds: it streams an array's elements through the
**same** `Unsigned()/Signed()/Fp32()/Fp64()` callbacks a lone scalar uses, so an
array header at a scalar-declared id of the same shape would be stored element by
element.

The generated visitor therefore carries a skip counter. `ArrayBegin` arms
`askip = count`, then disarms (and arms the mirror fill counter `afill`) only at a
`(cur, id)` pair that really declares a native array **of the announced kind**; the
shared callbacks discard while armed. It self-terminates on the announced count (no
array-end callback needed), survives a chunk boundary (the counter lives in the
visitor), leaves legitimate arrays untouched, and still decodes a real scalar
arriving at that id after the array. Both `kind switch` blocks carry one arm per
wire array kind, and the element types partition across them exactly as the encoder
maps them (#254):

| declared element | wire array kind | elements arrive in |
|---|---|---|
| `u8`…`u64`, `boolean`, `bitfield` | `ArrayKind.Unsigned` | `Unsigned()` |
| `i8`…`i64`, `enum`               | `ArrayKind.Signed`   | `Signed()` |
| `fp32`, `fp64`                    | `ArrayKind.Fixlen`   | `Fp32()` / `Fp64()` |

Arming per kind is only half of the rule. §7.3 also forbids decoding the payload
**into the declared field**, and *sizing* the destination is decoding into it: an
`ARRAY_SIGNED` header at a `byte[]`-declared id used to leave that field holding a
one-element array the wire never carried — the leak was the **length**, not the
element. So every allocation arm in `ArrayBegin` (the `new T[count]`, the `List`
`Clear()`, and a native matrix row's placement) is fronted by
`if (kind != ArrayKind.X) break;`, emitted **before** the schema-`count` bound. The
order is normative: the bound applies only to a field that survives §7.3, so an
over-count *mis-typed* array is skipped rather than rejected as a false
`InvalidMessage`.

The fixlen **subtype** (fp32 vs fp64) is not visible in `ArrayBegin` —
`ArrayKind.Fixlen` collapses both — so a subtype contradiction is caught downstream,
where the element lands in `Fp32()` or `Fp64()` and finds no fill arm.

## Arrays — `count` is a capacity

A schema `count: N` is a **capacity**, not a length: it never reaches the wire, it
bounds the array (an element count or element id past `N` fails the decode as
invalid), and it lets fixed-storage targets pre-size — but it never adds elements.
The wire count `M` **is** a compact array's length, and a wrapper array's length is
*highest present id + 1*.

What that looks like from C#:

- `new <Msg>()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`). Native arrays keep their `T[]` /
  `List<T>` initializer; a wrapper array is `new()`.
- `Marshal` writes **every** element the value holds. `new uint[]{1, 2, 0, 0}` and
  `new uint[]{1, 2}` are different values with different bytes.
- Decode yields exactly the elements the wire carried. A primitive `count: N`
  array allocates `new T[count]` — the `#100` schema-capacity guard already
  rejected `count > N`, so the untrusted count can never over-allocate — and a
  `List<T>` one clears and appends. `SequenceEnd` is a bare scope pop: there is no
  length left to reconstruct.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `new uint[4]` is a
  four-element value and stays on the wire.

## Wrapper arrays: element placement and positional sparsity (issues #247, #248)

A wrapper array's element id **is** the array index (MESSAGE_SPEC §5.1). Two pieces
implement that here, one per direction.

**Sparsity (encode) is positional, and one rule serves both element kinds.** An
element *before the last one* that equals its element default is omitted, leaving an
id **gap** the decoder restores from that same default; the **last** element is
always written, as its value or as an empty frame, because its presence is what
fixes the decoded length. For a leaf that is the `|| _i0 == _n0 - 1` disjunct in the
omit test (`lastElemExpr`); for a `struct`/`union`/nested-row element it is the
**closer**, chosen at run time from the position in the value —
`WriteSequenceEndKeep` at the last index, the dropping `WriteSequenceEnd` in the
interior, where an all-default element writes no child and its lazily-held frame
vanishes into the gap. A native nested row has no frame of its own, so the rule
lands on the write itself. A sequence-typed **field** (a struct field, an array
wrapper) still always takes the dropping closer: an all-default one is omitted and
absence reconstructs it (§2).

So `["a", ""]` → `06 02 0a 61 0a 02 07`, `["", ""]` → `06 0a 02 07`, `["", "x", ""]`
→ `06 0a 0a 78 12 02 07`, and an all-default two-element struct array → `06 0e 07 07`
— element 1 as an empty frame, element 0 as a gap. The three values `["a", ""]`,
`["a"]` and `[]` encode and decode distinctly. A declared `count: N` changes none of
it: a capacity can never restore an elided tail.

**Placement (decode).** Every element kind is placed at `list[id]` after gap-filling
with the element default — never appended. Appending shortens the array by every
interior gap (which sparsity now makes routine) and decodes a **reopened** id as a
second element rather than merging into the first (§7.4, which placement gives for
free). The flat visitor descends into an element scope on `SequenceBegin(id)` (or,
for a native row, on `ArrayBegin(id)`), and the element's own callbacks arrive
*after* that descent, so the id is latched in a per-scope field, `_ix<Scope>`, that
the whole child sub-tree addresses through. Each array scope is a distinct static
location and the scope tree is acyclic, so one latch per scope is enough. The `#142`
over-index guard rejects `id >= N` first, which also bounds the gap-fill against an
over-index amplification DoS — including on the row collectors, which had no bound
of their own while they appended.

`IsDefault()` — every class carries it — is the exact negation of what `Marshal`
writes, evaluated per field and recursively: the explicit form of the "not one child
was written" test the lazy framing performs. Because the last element is always
written, a wrapper array is default exactly when it is **empty**, so the writer and
the predicate cannot drift apart.
