# Java target — `targets.java`

Target-specific options, accepted under `targets.java`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `package` | string | `message` | The `package <name>;` declaration of the generated classes (and the Maven source directory layout in project mode). |

The three `max_dyn_*` keys are also accepted under `generic:` (shared across
targets). A configured limit is inert — no constants, no guards, byte-identical
output — when the schema has no unbounded field of its kind. A limit violation
surfaces from `decode()` as a `RuntimeException` and from `tryDecode()` as a
`java.io.UncheckedIOException`, in both cases wrapping the
`SofabException(LIMIT_EXCEEDED)` cause (same shape as the generator#100
over-count rejection). The over-count reject's wrapper-array analogue
(generator#142) throws `SofabException(INVALID_MSG)` the same way when a
`string`/`blob`/`struct`/`union` element array with `count: N` sees a wire
element id `≥ N`, before the `List` grows.

```yaml
targets:
  java:
    package: com.myproj.messages
```

## Arrays — `count` is a capacity

An array field maps to a `long[]`/`float[]`/`double[]` (numeric, enum, bitfield,
fp) or to a `List<...>` (boolean, and every wrapper-sequence element kind), and
the array's length is the length of that container. A schema `count: N` is a
**capacity**, not a length: it never reaches the wire, it bounds the array (an
element count or element id past `N` fails the decode as `INVALID_MSG`), and it
lets fixed-storage targets pre-size — but it never adds elements.

What you can observe from Java:

- `new <Msg>()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`). `reset()` re-arms to the same value.
- Encode writes **every** element the container holds. `new long[]{1, 2, 0, 0}`
  and `new long[]{1, 2}` are different values with different bytes.
- Decode yields exactly the elements the wire carried: `length` / `size()` after a
  round trip equals what went in, for the compact scalar form and the wrapper form
  alike. `sequenceEnd()` fills nothing back in.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `new long[]{0, 0, 0, 0}` is
  a four-element value and stays on the wire.

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse**: an element equal to the element default is dropped and
leaves an id gap, which decode restores from that same default. The **last**
element is always written — as its value, or as an empty frame for a
struct/union/nested element — because its presence is what carries the length. So
`List.of("a", "")`, `List.of("a")` and an empty list are three distinct values that
encode and decode distinctly.

Because an interior gap is now ordinary, every element kind is **placed at its
element id** on decode, matrix rows included (`Sbuf.placeRow`), never appended.

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as constants on the generated
class, checked before allocation. A violation throws `SofabException` carrying
`SofabError.LIMIT_EXCEEDED`.

## Benchmark row

Row `java` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Strict UTF-8 (issue #85)

`String` is a Unicode type, so it is **always strict** (MESSAGE_SPEC §8 /
CORELIB_PLAN §6.4) — no config key in generated code. The platform
`new String(bytes, UTF_8)` is **lossy** (substitutes `U+FFFD`), which §8 forbids in
every mode, so the visitor decodes through a generated `_utf8(...)` helper: an
allocation-free `Utf8.valid(...)` well-formedness scan, then the JVM-intrinsic
`new String(b, off, len, UTF_8)` (which never substitutes on already-valid input).
Invalid bytes become `SofabException(INVALID_MSG)` — the same channel as the
over-count guards. (This replaced a REPORTing `CharsetDecoder`, which allocated a
decoder + `CharBuffer` per call; the scan-then-intrinsic pair measured ~43 % faster
on the arena strings at zero per-string allocation.) The check runs once the full `total` bytes
are present. Encode-side strictness is corelib-side (`OStream.writeString`).

**Only a materialized string is validated (issue #257).** corelib-java delivers
every fixlen-string field to `string(...)` — an unknown id and a §7.3 wire-type
contradiction included — so the callback opens with a `switch (cur)` over the string
destinations whose every non-matching path `return`s. `_utf8(...)` and `acc`
therefore run only for a payload this scope actually reads, which is what
CORELIB_PLAN §6.4 requires, and a skipped payload can never leave bytes in `acc` for
a later declared field to inherit. The `maxlen` and `max_dyn_string_len` pre-checks
sit behind the guard: they are destination-scoped themselves, so §5.2's
INVALID-over-INCOMPLETE ordering is unchanged. `blob(...)` has no such guard — bytes
carry no encoding. A schema that declares no string at all gets an **empty**
`string(...)` body rather than a guarded one — every string reaching it is skipped
by definition, and decoding one only to drop it is the same violation.

## §7.3: a mis-typed array header (issues #183, #193, #254)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not the array kinds: it streams an array's elements through the
**same** `unsigned()/signed()/fp32()/fp64()` callbacks a lone scalar uses, so an
array header at a scalar-declared id of the same shape would be stored element by
element.

The generated visitor therefore carries a skip counter. `arrayBegin` arms
`askip = count`, then disarms (and arms the mirror fill counter `afill`) only at a
`(scope, id)` pair that really declares a native array **of the announced kind**;
the shared callbacks discard while armed. It self-terminates on the announced count
(no array-end callback needed), survives a chunk boundary (the counter lives in the
visitor), leaves legitimate arrays untouched, and still decodes a real scalar
arriving at that id after the array. There is one arm per wire array kind, and the
element types partition across them exactly as the encoder maps them (#254):

| declared element | wire array kind | elements arrive in |
|---|---|---|
| `u8`…`u64`, `boolean`, `bitfield` | `ArrayKind.UNSIGNED` | `unsigned()` |
| `i8`…`i64`, `enum`               | `ArrayKind.SIGNED`   | `signed()` |
| `fp32`, `fp64`                    | `ArrayKind.FIXLEN`   | `fp32()` / `fp64()` |

Arming per kind is only half of the rule. §7.3 also forbids decoding the payload
**into the declared field**, and *sizing* the destination is decoding into it: an
`ARRAY_SIGNED` header at a `u8[]`-declared id used to leave that field holding a
one-element array the wire never carried — the leak was the **length**, not the
element. So every allocation arm in `arrayBegin` (the primitive `new T[…]`, the
`List` `clear()`, and a native matrix row's `placeRow`) is fronted by
`if (kind != ArrayKind.X) break;`, emitted **before** the schema-`count` bound.
The order is normative: the bound applies only to a field that survives §7.3, so an
over-count *mis-typed* array is skipped rather than rejected as a false
`INVALID_MSG`.

The fixlen **subtype** (fp32 vs fp64) is not visible in `arrayBegin` —
`ArrayKind.FIXLEN` collapses both — so a subtype contradiction is caught downstream,
where the element lands in `fp32()` or `fp64()` and finds no fill arm.

## §2: sequence framing — which closer `marshal` emits

MESSAGE_SPEC **§2** omits a sequence-typed **field** whose value equals its
declared default instead of framing it empty, while a wrapper-array **element**
keeps its frame: element presence is what carries a dynamic array's length
(*highest present id + 1*, §5.1), so dropping an all-default element would change
the decoded length, not merely the bytes.

The generated `marshal` opens **every** sequence with `os.writeSequenceBeginLazy(id)`,
which holds the header back until a child field is actually written. Since the
per-field sparse rule already omits every child equal to its default, "not one
child was written" *is* "the object equals its declared default", evaluated per
field and recursively — no byte image is ever compared. What differs is the
**closer**, chosen statically from the position in the schema, never from the
value:

| Emission site | Closer | Why |
|---------------|--------|-----|
| `struct` / `union` field | `os.writeSequenceEnd()` | absence reconstructs the same value |
| array field (the wrapper) | `os.writeSequenceEnd()` | its default is the empty collection |
| wrapper-array element (`struct`/`union`) | `os.writeSequenceEndKeep()` | presence carries the array length |
| nested array row | `os.writeSequenceEndKeep()` | a row is an element |

Consequence: an all-default message encodes to **zero bytes**. An *interior*
all-default struct element still round-trips, because its keeping closer holds
the id gap open; the **trailing** run does not — see the next section.

The predicate behind the lazy framing is also emitted explicitly, as a
package-private `isDefault()` on every generated `struct`/`union` class. Each of
its arms is literally the corresponding `marshal` write guard, so the two cannot
state different truth tables. It exists because a wrapper-array **element** has to
be judged *before* its frame is opened, which the implicit "no child was written"
test cannot answer in time.

A wrapper array's declared `default` is not materialized by this backend (its
initial value is `N` *element* defaults for a `count: N` array, the empty
collection for a dynamic one), so absent and explicitly-empty denote the same
value and the plain dropping close is correct. If that gap is ever closed,
the wrapper needs an `if (value != default) { … os.writeSequenceEndKeep(); }` guard
so a value differing from a non-empty default still reaches the wire as the empty
wrapper — the only encoding of "explicitly empty" (§2, §3).

## §3/§5.1: wrapper arrays — element ids are indexes, and the tail is elided

A wrapper array's element id **is** the array index (§5.1). Decode therefore
gap-fills with default elements up to the id and decodes **into** `list.get(id)`;
it never appends. Appending shortened the array by any interior id gap and decoded
a *re-opened* element id as a second element instead of merging into the first
(the §7.4 struct-merge, which placement gives for free). The leaf `string`/`blob`
element paths always did this; the `struct`/`union` path now agrees with them.

Java's `Visitor` is **flat** — there is no per-element child visitor to carry the
position the way a nested-visitor backend returns a pointer to `dest[id]` — so
`sequenceBegin` parks the index in a per-array `_ex_<loc>` field, and the element's
child-field accessors resolve through `list.get(_ex_<loc>)`. The existing
over-index guard (element id `≥ N` is `INVALID`) still applies, and now also bounds
the gap-fill.

Encode is the mirror. A `count: N` wrapper array's canonical wire stops at **M**,
one past its last non-default element — explicitly *"even for sequence-form
elements"* (§3/§5.1) — so `marshal` narrows the container to M **before** the
element loop, via the `Sbuf.trimTail*` helpers (a `subList` view, so no
allocation). Only the trailing run goes; interior all-default elements keep their
frame. `M == 0` writes no child at all, so the lazily-opened wrapper is dropped by
the field-level closer and the field is omitted entirely (§2).

Two invariants hold this together:

- **A dynamic (count-less) array is never narrowed.** It has no `N` to refill
  from, so a trailing default element is significant and keeps its frame. That
  makes `Sbuf.trimTailStrings`/`trimTailBlobs` a `count: N`-only path; the
  dynamic side goes through `Sbuf.orEmpty`, which is the identity minus the null
  the trims used to absorb.
- **Decode default-fills a `count: N` wrapper array back out to `N`** when the
  sequence scope closes (`sequenceEnd`). §5.1 makes the length `N` "for every
  target — a growable-list target MUST default-fill to `N` exactly like a
  pre-sized one". Native arrays already got this from `arrayBegin`; wrapper arrays
  are `List`-backed and did not. Without it the trailing elision would not
  *re-shape* the array, it would **shorten** it on every round trip.

`sequenceEnd` can only refill a sequence that was actually **opened**, so it
covers only half the rule. An **absent** field fires no callback at all, and §2
makes absence the encoding of an all-default array — the common case. The other
half is therefore materialization at construction: a `count: N` wrapper array's
field initializer is `N` element defaults (`""`, `new byte[0]`, `new <Elem>()`,
an empty inner row), emitted by a per-field `_seqdef_<name>` filler that both the
initializer and `reset()` call. The native fixed-count arrays beside it have
always been materialized this way, from their padded default literal; without the
same treatment the same schema disagreed with itself — an absent `count: 3`
string array decoded at length 0 while one element on the wire, or an explicitly
empty wrapper, decoded at 3. A **dynamic** array has no `N` and still starts
empty. Materializing the elements does not put the field on the wire: `marshal`
narrows to `M == 0` and the lazily-opened wrapper is dropped, so an untouched
message still encodes to zero bytes.

The narrowing expression is generated once (`elemTrimExpr`) and used by both the
marshal loop and `isDefault`. If the predicate narrowed a field the writer did not
(or the reverse), the result would be a field omitted though it is on the wire, or
kept though it is not.

### The last element of a dynamic array is always present

§2, tightened by documentation#29 (`a3e35e2`), makes one position exempt from the
per-element sparse rule: **the last element of a dynamic wrapper array is always
written**, whatever its value. The array recovers its length as *highest present
id + 1* (§5.1), so that element's PRESENCE is the only thing carrying the length.
Omitting it lost data outright — `["a", ""]` encoded exactly like `["a"]` and
decoded one element short, and `["", ""]` encoded to nothing and decoded as `[]`,
so two different values shared one encoding.

Only the **leaf** (`string`/`blob`) element paths needed it: a `struct`/`union`/
nested-row element is framed unconditionally by `writeSequenceEndKeep()` and was
already conformant, which is exactly the two-standards gap this closes. The guard
is the `|| _i == _t.size() - 1` disjunct `lastElemGuard` appends to the element's
omit test, emitted only when the array is dynamic. A `count: N` array is exempt —
its length is `N` whatever the wire carries — so it still elides the entire
trailing default run and its bytes are unchanged.

Measured on this backend (probe field `dyn: { type: array, items: { type: string,
maxlen: 8 } }`, before → after):

| value | before | after |
|-------|--------|-------|
| `["a", ""]` | `06020a6107` → `["a"]` | `06020a610a0207` → `["a", ""]` |
| `["a"]` | `06020a6107` | `06020a6107` |
| `["", ""]` | *(nothing)* → `[]` | `060a0207` → `["", ""]` — final element alone, at id 1 |
| `[]` | *(nothing)* | *(nothing)* |
| `["", "b"]` | `060a0a6207` | `060a0a6207` — interior gap still elided |

Both halves move together. `elemTrimExpr`'s leaf trim becomes `count: N`-only at
the same time, because `isDefault` is generated from it: left trimming, the
predicate would call a dynamic `[""]` all-default and omit a field the marshal
loop now writes.

## `reset()` — the decode side of §2

`tryDecode(byte[] data, M out)` takes its destination from the caller, and callers
reuse it. Once §2 made **absence** the encoding of an all-default field, an
omitted field stopped firing any callback at all — so the visitor's clears, which
hang off `sequenceBegin`/`arrayBegin`, no longer run for it. A reused destination
then kept the *previous* decode's elements: the decoded array held data that was
not in the message.

Absence is only observable before the feed starts, so that is where the
destination is re-armed. Every generated class carries

```java
/** Restores every field to its declared default, in place; … */
public void reset()
```

and `tryDecode` calls `out.reset()` as its first statement. `decode(byte[])`
constructs a fresh instance and does not pay for it.

`reset()` restores the declared defaults **in place**, because not re-allocating
is the entire point of accepting a destination:

| Field | Reset |
|-------|-------|
| scalar / `String` / `blob` | assigned the same literal the field initializer uses |
| `List`-backed array (wrapper, `boolean`) | `Sbuf.resetList` — `clear()`, keeping the backing capacity; a `boolean` array's materialized default is then `addAll`ed back, and a `count: N` wrapper array is refilled to its `N` element defaults by `_seqdef_<name>` so `reset()` lands on the value `new <Msg>()` has |
| primitive array with a default (always so for `count: N`) | `System.arraycopy` from the shared `_arrdef_*` static when the length already matches; `clone()` only otherwise |
| dynamic primitive array | the shared zero-length `Sbuf.EMPTY_*` constant |
| `struct` / `union` | `reset()` recursively, never a new object |

It is **public** for the same reason corelib-cpp exposes `IStreamImpl::reset()`:
a caller who drives the `Visitor` directly — feeding chunks itself, with no
`tryDecode` to hook — needs the same ability to re-arm a destination between
messages.

The §7.4 clear in the visitor is unchanged and still required: it covers a
*re-opened* wrapper within one message, which must replace the array whole.
`reset()` covers the field that never appears at all. The two are different
events and both are needed.
