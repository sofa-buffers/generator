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
forbids in every mode, so the verdict comes from `sofab.Utf8.Decode(...)`, which
validates before it materializes and reports a malformed range as
`SofabException(SofabError.InvalidMessage)` — the same channel as the over-count
guards. `sofab.PayloadAcc.String(...)` is what calls it, once the full `total`
bytes are present. Both are the corelib's (see [support layer](#support-layer));
encode-side strictness is corelib-side too (`OStream.WriteString`).

**Only a materialized string is validated (issue #257).** corelib-cs delivers every
fixlen-string field to `String(...)` — an unknown id and a §7.3 wire-type
contradiction included — so the callback opens with a `switch ((cur, id))` over the
string destinations whose `default` `return`s. The accumulator — where the
buffering and the UTF-8 verdict both happen — is therefore reached only for a
payload this scope actually reads, which is what CORELIB_PLAN §6.4 requires, and a
skipped payload can never leave bytes in it for a later declared field to
inherit. The `maxlen` and `max_dyn_string_len` pre-checks sit behind the
guard: they are destination-scoped themselves, so §5.2's INVALID-over-INCOMPLETE
ordering is unchanged. `Blob(...)` has no such guard — bytes carry no encoding.
A schema that declares no string at all gets an **empty** `String(...)` body —
every string reaching it is skipped by definition, and decoding one only to drop
it is the same violation.

## Support layer

Four decode-side helpers the visitor used to carry itself now come from the
corelib (ARCHITECTURE §8, generator#345, corelib-cs#92) — each has the same shape
for every schema, with its schema dependence entirely in its arguments and type
parameters, so the generated file calls them instead of redefining them:

| generated code calls | what it replaced |
|---|---|
| `sofab.Utf8.Decode(data, off, len)` | the emitted `_strictUtf8` codec and its `_Utf8(...)` wrapper |
| `sofab.PayloadAcc.String / .Blob` | a `List<byte> acc` field plus a byte-at-a-time reassembly loop inlined into both callbacks |
| `sofab.Seq.EnsureCap<T>(a, i, cap)` | the emitted generic array-growth helper |
| `sofab.Seq.ArrayInitCap` | `private const int ArrayInitCap = 16` |

A generated file is 14–33 lines shorter for it, and the rationale for each — why
validation must precede conversion, why an untrusted count is a growth ceiling and
never a first allocation, why a payload is validated at completion and not per
chunk — is written once in the corelib instead of being re-emitted into every
user's source tree. This needs a corelib at or past corelib-cs#92; an older one
fails at compile time on the missing symbol.

The encode scratch buffer and its sink stay generated: CORELIB_PLAN §5.1 assigns
output-buffer allocation to the generated layer.

## §7.3: a mis-typed array header (issues #183, #193, #254, #259)

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
| `fp32`                            | `ArrayKind.Fp32`     | `Fp32()` |
| `fp64`                            | `ArrayKind.Fp64`     | `Fp64()` |

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

### The fixlen subtype is part of the key (issue #259)

A fixlen array carries a second header word, the `fixlen_word`, naming its element
*subtype*. CORELIB_PLAN **§4.8** fixes the decode order around it: read the element
count (format ceiling only, allocating nothing), read the `fixlen_word`, and only
then offer the field — because a subtype that contradicts the declared element type
makes the field a §7.3 **skip**, and a skipped field's element count is not this
array's count, so no schema bound may be applied to it.

corelib-cs implements that order: `IVisitor.ArrayBegin` fires *after* the
`fixlen_word`, and `ArrayKind` names the subtype (`Fp32 = 2`, `Fp64 = 3`) instead of
one collapsed `Fixlen` category. So the generated arms are keyed by subtype exactly
as they are keyed `Unsigned` apart from `Signed`: a declared `fp32[N]` appears only
under `ArrayKind.Fp32`, a declared `fp64[N]` only under `ArrayKind.Fp64`, in both
`kind switch` blocks and in the `if (kind != ArrayKind.X) break;` clause fronting
the allocation arm.

The bound stays **inside** the matched arm, behind that kind test. An fp64 header
arriving at an `fp32[4]`-declared id therefore takes the skip path whatever its
count says: the declared `float[]` is not sized, cleared or allocated, the `count`
bound is never consulted, and `askip` discards the elements that follow. Hoisting
the bound out — or folding the two subtypes back into one arm — turns an
over-count fp64 header at an fp32 slot into a false `InvalidMessage` and lets a
`float[]` be sized from a header that was never this field's value.

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
- `Serialize` writes **every** element the value holds. `new uint[]{1, 2, 0, 0}` and
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

`IsDefault()` — every class carries it — is the exact negation of what `Serialize`
writes, evaluated per field and recursively: the explicit form of the "not one child
was written" test the lazy framing performs. Because the last element is always
written, a wrapper array is default exactly when it is **empty**, so the writer and
the predicate cannot drift apart.


## Schema bounds are latched at the word that carries them

A `maxlen` or a wrapper element's `id >= count` is fully established by the
fixlen **length word** — the number that violates the bound is already on the
wire, and no later byte can make it legal. CORELIB_PLAN §5.2 makes INVALID
dominate INCOMPLETE for exactly that reason, so the verdict has to be taken
there.

It used to be taken in the **payload** callback, which only fires once payload
bytes arrive. Truncate a message immediately after the length word and the guard
never ran: the decode reported INCOMPLETE, while the same bytes read whole are
INVALID. The untruncated controls were unanimous across the family — the
disagreement was only about *when*, which is observable solely under truncation.

`IVisitor.FixlenBegin(int id, FixlenType subtype, int total)` (corelib-cs#53) is
where both bounds now sit, keyed by the same `(cur, id)` tuple the payload guard
uses:

```csharp
public void FixlenBegin(int id, FixlenType subtype, int total) {
    if (subtype == FixlenType.String) {
        switch ((cur, id)) {
        case (Root, 0): if (total > 8) throw …; break;
        case (Root_sa, _): if (id >= 3) throw …; if (total > 6) throw …; break;
```

Over-index comes **before** the element `maxlen`: an element that is not this
array's element at all must not have its length measured against the element
bound. Every guard sits inside the declared-subtype test — the hook fires for
whatever fixlen subtype arrived at a field id, and a contradicting one is a §7.3
skip, not this field's length.

The payload-side guards **stay**. They are unreachable for a message that reaches
the header hook, and they are the only thing still bounding a consumer built
against a corelib predating it.

**The verdict arrives on a different channel from INCOMPLETE, and that is the
contract, not an accident.** `TryDecode` returns COMPLETE/INCOMPLETE; malformed
input **throws** out of `Feed`. A conformance check on this ordering therefore
asserts the over-bound message *throws* and the in-bound control *returns*
INCOMPLETE.

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a value outside its
declared range with `SofabError.InvalidMessage`. The width is a normative bound,
not a storage hint (MESSAGE_SPEC §1/§7.1): the `(byte)value` cast that follows IS
the mask §7.1 forbids, so the guard precedes it.

```csharp
case (Root, 0): if (value > 255) throw new SofabException(SofabError.InvalidMessage,
    "a_u8: value outside declared width u8"); m.a_u8 = (byte)value; break;
case (Root, 3): m.d_u64 = (ulong)value; break;   // u64: nothing narrower to bound
```

Unlike Java and Dart, C# needs no negative-value term on the unsigned side:
`Unsigned` delivers a `ulong`, so the comparison is already unsigned.

In an array arm the guard follows the fill guard, never precedes it — an
over-width scalar at an array id with no `ArrayBegin` in front of it is a §7.3
skip, not an INVALID.

## §7.3/§5.2: an undeclared sequence is skipped whole (issues #268, #272)

`SequenceBegin` switches on the `(cur, id)` tuple and had no default arm, so a
sequence the schema does not declare at this position left `cur` on the enclosing
scope and its children bound there — a child id 3 inside an unknown sequence set
the root's own field 3 (#268), and a sequence opened at a string-array element
position bound its string as that element (#272).

One arm covers both, since the switch is flat:

```csharp
switch ((cur, id)) {
    case (Root, 10): cur = Root_known; break;
    default: cur = _DEAD; break;
}
```

`_DEAD` is a scope no callback case matches, so the whole subtree is discarded;
`SequenceEnd` pops the stack and restores the live scope at the matching end.
