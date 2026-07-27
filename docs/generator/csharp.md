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

## §7.3: an integer array at a scalar id (issue #183)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not one: it streams an integer array's elements through the **same**
`Unsigned()/Signed()` callbacks a lone scalar uses, so an integer array header at a
scalar-declared id of the same signedness would be stored element by element.

The generated visitor therefore carries a skip counter. `ArrayBegin` arms
`askip = count` when the announced kind is the unsigned or signed integer kind
and the `(scope, id)` pair is **not** a declared integer-element native array;
the two scalar callbacks then discard while armed. It self-terminates on the
announced count (no array-end callback needed), survives a chunk boundary (the
counter lives in the visitor), leaves legitimate arrays untouched, and still
decodes a real scalar arriving at that id after the array. The fp arrays are never
armed — their elements go to the float callbacks and cannot reach a scalar arm.

## Wrapper arrays: placement, N-fill and the trailing run (issues #247, #248)

A wrapper array's element id **is** the array index (MESSAGE_SPEC §5.1), and a
`count: N` array's canonical wire stops at `M`, one past its last non-default
element — "even for sequence-form elements" (§3/§5.1). Three pieces implement that
here, and they only work together.

**Placement (decode).** The flat visitor descends into an element scope on
`SequenceBegin(id)`. The element's own fields arrive *after* that descent, so the
id is latched in a per-scope field, `_ix<Scope>`, and the whole child sub-tree
addresses `list[_ix<Scope>]`. The list is gap-filled with default elements up to
`id` first. Appending instead — "the element just added" — shortened the array by
any interior id gap and decoded a **reopened** id as a second element rather than
merging into the first (§7.4, which placement gives for free). Each array scope is
a distinct static location and the scope tree is acyclic, so one latch per scope is
enough. The `#142` over-index guard still rejects `id >= N` first, which also
bounds the gap-fill.

**N-fill (decode).** `SequenceEnd` runs while `cur` still names the scope being
closed, so it default-fills that array back out to `N` — §5.1 requires the length
to be `N` "for every target", and a growable `List<T>` must fill exactly like
pre-sized storage. This is the prerequisite for the trim below: without it the
elision would not *normalise* a decoded array, it would **shorten** it on every
round trip. A count-less array has no `N` and is never filled (its length is
highest-present-id + 1).

**Trim (encode).** The element loop runs to `M`, not to `Count`. Interior
all-default elements keep their frame (`WriteSequenceEndKeep`) — element presence
is what carries the length — and `M == 0` writes no child at all, so the lazily
opened wrapper is dropped and the field is omitted (§2). `M` comes from
`elemTrimExpr`: a static `TrimTail` on the element class (backed by the generated
`IsDefault()`, emitted only for types actually used as a `count: N` element), or
`SofabFixedArray.TrimStrs/TrimBlobs/TrimRows` for the other element kinds. A
**dynamic** array is never narrowed — with no `N` to refill from, a trailing
default element is significant.

`IsDefault()` — every class carries it — is the exact negation of what `Marshal`
writes, evaluated per field and recursively: the explicit form of the "no child was
written" test the lazy framing already performs for a *field*, needed because an
*element* must be judged **before** the loop opens. It and the marshal loop are
generated from the one `elemTrimExpr`, so the writer and the predicate cannot
drift: a predicate that narrowed a field the writer does not would omit a field
that is on the wire.
