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

## Emitted files — one public class each

Java allows one public top-level class per file, so every generated type gets
its own:

```
src/main/java/<pkg>/
    <Message>.java   public class <Message>  + the package-private <Message>Visitor
    <Type>.java      public class <Type>     one per schema struct/union
    Sbuf.java        package-private shared helpers
```

The schema types are **public**, like every other target's (`pub struct` in Rust,
`export class` in TypeScript, an exported Go struct). They used to sit inside the
message's file, which forced them package-private and made a message's
struct-typed field unusable from anywhere else — `probe.inner.x` did not compile
outside the generated package, and the type could not even be named (#305).

A type reached from two messages is emitted **once**. Per-message emission also
declared it twice in one package, which javac rejects outright (`duplicate
class`), so a schema with a shared `$defs` struct did not build at all.

`Sbuf` and `<Message>Visitor` stay package-private on purpose: they are
generated plumbing, not schema surface.

## Arrays — `count` is a capacity

An array field maps to a **primitive array of its declared width** (numeric, fp)
or to a `List<...>` (boolean, and every wrapper-sequence element kind), and the
array's length is the length of that container:

| element | field | bytes/element |
|---|---|---|
| `u8`, `i8` | `byte[]` | 1 |
| `u16`, `i16` | `short[]` | 2 |
| `u32`, `i32` | `int[]` | 4 |
| `u64`, `i64`, `enum`, `bitfield` | `long[]` | 8 |
| `fp32` / `fp64` | `float[]` / `double[]` | 4 / 8 |
| `boolean` | `List<Boolean>` | — |

A **scalar** field still maps to `long` — Java has no unsigned types and widening
one value costs nothing. An array is where it costs: at 8 bytes an element a
`u8[1000]` is eight kilobytes of which seven are sign bits.

> **An unsigned array element holds the declared width's RAW BITS.** `byte`,
> `short` and `int` are signed in Java, so a `u8` element of 200 reads back as
> `-56` and a `u32` element of 4294967295 as `-1`. Recover the value with
> `Byte.toUnsignedInt(a[i])`, `Short.toUnsignedInt(a[i])`,
> `Integer.toUnsignedLong(a[i])`. Signed widths narrow exactly and need nothing —
> an `i8` element *is* a Java `byte`. This is the same bargain protobuf-java
> strikes for `uint32`, and it is what corelib-java's
> `writeArrayUnsigned(byte[]/short[]/int[])` overloads have always zero-extended
> for. Encode, decode, JSON and the wire all carry the VALUE; only the field's
> Java type is narrow.

The primitive mapping reaches one level in: a nested array of primitives is
`List<int[]>`, **not** `List<List<Long>>` — the outer level is a wrapper sequence
(its element ids are the row indices) but a row is a primitive array like any
other. Only a `bool` row stays `List<Boolean>`, having no primitive `OStream`
overload to be written through. A schema `count: N` is a
**capacity**, not a length: it never reaches the wire, it bounds the array (an
element count or element id past `N` fails the decode as `INVALID_MSG`), and it
lets fixed-storage targets pre-size — but it never adds elements.

What you can observe from Java:

- `new <Msg>()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`). `reset()` re-arms to the same value.
- Encode writes **every** element the container holds. `new int[]{1, 2, 0, 0}`
  and `new int[]{1, 2}` are different values with different bytes.
- Decode yields exactly the elements the wire carried: `length` / `size()` after a
  round trip equals what went in, for the compact scalar form and the wrapper form
  alike. `sequenceEnd()` fills nothing back in.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `new int[]{0, 0, 0, 0}` is
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

## §7.3: a mis-typed array header (issues #183, #193, #254, #259)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not the array kinds: it streams an array's elements through the
**same** `unsigned()/signed()/fp32()/fp64()` callbacks a lone scalar uses, so an
array header at a scalar-declared id of the same shape would be stored element by
element.

The generated visitor therefore carries a skip counter, and **skipping is the
default**. `arrayBegin` arms `askip = count` up front; only a `(scope, id)` arm
for an id that really declares a native array **of the announced kind** disarms it
and arms the mirror fill counter `afill`. The shared callbacks discard while
armed. It self-terminates on the announced count (no array-end callback needed),
survives a chunk boundary (the counter lives in the visitor), leaves legitimate
arrays untouched, and still decodes a real scalar arriving at that id after the
array. Each arm carries its own `if (kind != ArrayKind.X) break;`, and the element
types map to kinds exactly as the encoder does (#254):

| declared element | wire array kind | elements arrive in |
|---|---|---|
| `u8`…`u64`, `boolean`, `bitfield` | `ArrayKind.UNSIGNED` | `unsigned()` |
| `i8`…`i64`, `enum`               | `ArrayKind.SIGNED`   | `signed()` |
| `fp32`                            | `ArrayKind.FP32`     | `fp32()` |
| `fp64`                            | `ArrayKind.FP64`     | `fp64()` |

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

### Where an array element is stored

`arrayBegin` resolves the destination once — kind test, schema-`count` bound,
disarm/arm, reset the container — and parks *which* destination in `atgt`. The
element callbacks then start with

```java
if (afill != 0) { afill--; switch (atgt) { … } return; }
```

so an element is stored against the already-resolved target instead of being
routed through the scope switch and an id switch again, once per element. Only a
fill `arrayBegin` armed can be open when a callback runs — the decoder delivers
exactly `count` elements before the array ends, and nothing else in between — so
`afill != 0` is the whole test, and the array ids leave the scalar switches
entirely. The `askip` discard guard follows it: an armed skip and an armed fill
are mutually exclusive, since `arrayBegin` sets exactly one of them.

This is a codegen shape, not a rule: the §7.3 arming, the bound, the width check
per element and the growth ceiling are all unchanged and all still there.

For an integer array the schema bounds with a `count: N`, the destination is
already exactly `count` long by the time the elements arrive, so it can skip the
element callbacks altogether. `arrayBegin` parks it in `abulk`, corelib-java's
`Visitor.arrayBulk(id, kind, count)` hands it over, the decoder fills it directly
(ZigZag already applied for a signed array), and `arrayBulkEnd(id, n)` clears the
fill counter and runs the declared-width check as one pass over what was written.

The offer is made **only** for a schema-bounded array: `count` is the wire's
claim, and an unbounded array must not be allocated against it (#96) — it keeps
the capped reservation and the grow-as-you-go element fill. Boolean arrays (a
`List`), fp arrays (not `long`-backed) and matrix rows (whose cap bounds the row's
*id*, not its element count) keep the element path too.

The two methods are emitted **without `@Override`** on purpose. `Visitor` declares
both with a default, so a corelib that has them calls into the fast path while one
that predates them simply never does — and the generated code still compiles
against it, because the per-element arms fill the very same array. `@Override`
would turn an optimisation into a hard corelib requirement.

The width check moving from per-element to per-array is a change in *when*, not in
*what*: an out-of-range element is still `INVALID_MSG` (§7.1) and INVALID is still
terminal, so no caller reads a value the check rejects — `decode` throws, and for
`tryDecode` returning `INVALID` the destination's contents are not defined.

### The fixlen arm is keyed by subtype (issue #259)

A fixlen array header carries a **second** word after the count — the
`fixlen_word` naming the element subtype and its width. CORELIB_PLAN §4.8 fixes
the order in which that header is judged: the count is read under the format
ceiling only (it allocates nothing), then the `fixlen_word`, then §7.3 decides
whether the field is contradicted, and only a field that *survives* is measured
against its schema `count`. corelib-java announces the array from the
`fixlen_word` handler accordingly, and `ArrayKind` lost the collapsed `FIXLEN`
member in favour of `FP32` and `FP64` (ordinals are normative family-wide:
`UNSIGNED = 0`, `SIGNED = 1`, `FP32 = 2`, `FP64 = 3`).

The generated visitor mirrors that split: a declared `fp32[N]` field's arm tests
for `FP32` and a declared `fp64[N]`'s tests for `FP64`. An `fp64` header arriving
at an `fp32[N]` slot therefore leaves that arm before anything happens — the
discard counter stays armed and drops exactly `count` elements, and the declared
`float[]` is not sized, cleared or allocated. Previously both subtypes shared one
arm, so the fp64 header *did* size the declared `float[]` before the mismatch was
noticed downstream in `fp64()`.

The schema-`count` bound stays **inside** the matched arm, behind
`if (kind != ArrayKind.FP32) break;`, for the same reason it sits behind the
integer kind tests: a header of the *other* subtype must reach the skip path, not
the reject path. An over-count `fp64` array at a declared `fp32[3]` is a §7.3
skip, not `INVALID_MSG`.

This split also stops a skipped `fp64` header from sizing a declared `float[]`,
but it is not the whole of finding F-0039 — that finding's primary face is a
non-fixlen `ARRAY_SIGNED` header at a `u8[]` slot, a different codegen path.

## §2: sequence framing — which closer `serialize` emits

MESSAGE_SPEC **§2** omits a sequence-typed **field** whose value equals its
declared default instead of framing it empty, while a wrapper-array **element**
keeps its frame: element presence is what carries a dynamic array's length
(*highest present id + 1*, §5.1), so dropping an all-default element would change
the decoded length, not merely the bytes.

The generated `serialize` opens **every** sequence with `os.writeSequenceBeginLazy(id)`,
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
its arms is literally the corresponding `serialize` write guard, so the two cannot
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
elements"* (§3/§5.1) — so `serialize` narrows the container to M **before** the
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
empty. Materializing the elements does not put the field on the wire: `serialize`
narrows to `M == 0` and the lazily-opened wrapper is dropped, so an untouched
message still encodes to zero bytes.

The narrowing expression is generated once (`elemTrimExpr`) and used by both the
serialize loop and `isDefault`. If the predicate narrowed a field the writer did not
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
predicate would call a dynamic `[""]` all-default and omit a field the serialize
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
a caller who feeds chunks himself, with no `tryDecode` to hook, needs the same
ability to re-arm a destination between messages.

That caller is `Myfirstmessage.Decoder` (generator#239). Until it existed the
paragraph above described something unreachable: the generated `<Msg>Visitor` is
package-private, so "drives the `Visitor` directly" was only possible from inside
the generated package. `decoder()` now hands out a public handle on the corelib's
resumable `IStream`, and the Visitor stays package-private because the decoder
wraps it:

```java
Myfirstmessage.Decoder d = Myfirstmessage.decoder();
for (byte[] chunk : chunks) d.feed(chunk);   // any chunk size, down to 1 byte
Myfirstmessage m = d.finish();               // rejects a stream that ended mid-field
```

`finish()` throws `IllegalStateException`, not `SofabException`: `SofabError` has
no `INCOMPLETE` (only `ARGUMENT` / `BUFFER_FULL` / `INVALID_MSG` /
`LIMIT_EXCEEDED`), and reporting a truncated message as `INVALID_MSG` would
collapse two outcomes §7 keeps apart — an incomplete message is not a malformed
one. `status()` and `message()` give the same verdict without an exception.

The §7.4 clear in the visitor is unchanged and still required: it covers a
*re-opened* wrapper within one message, which must replace the array whole.
`reset()` covers the field that never appears at all. The two are different
events and both are needed.


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

`Visitor.fixlenBegin(int id, FixlenType subtype, int total)` (corelib-java#62) is
where both bounds now sit:

```java
public void fixlenBegin(int id, FixlenType subtype, int total) {
    if (subtype == FixlenType.STRING) {
        switch (cur) {
        case 0: switch (id) { case 0: if (total > 8) throw …; break; default: break; } break;
        case 1: if (id >= 3) throw …; if (total > 6) throw …; break;
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
contract, not an accident.** `tryDecode` returns COMPLETE/INCOMPLETE; malformed
input **throws** `SofabException` out of `feed`. So a conformance check on this
ordering has to assert the over-bound message *throws* and the in-bound control
*returns* INCOMPLETE — reading both from one status word does not work here.

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a value outside its
declared range through the same unchecked `INVALID_MSG` channel as the maxlen
guard. The width is a normative bound, not a storage hint (MESSAGE_SPEC
§1/§7.1): the value is neither masked nor kept.

```java
case 0: if (value < 0 || value > 255L) throw new java.io.UncheckedIOException(
    new SofabException(SofabError.INVALID_MSG, "a_u8: value outside declared width u8"));
    m.a_u8 = value; break;
case 3: m.d_u64 = value; break;   // u64: nothing narrower to bound
```

**The `value < 0` term is load-bearing, not defensive noise.** The corelib
delivers an unsigned wire value as a Java `long`, which has no unsigned
counterpart: a `u64` at or above 2^63 arrives with its sign bit set, so
`value > 255L` alone would read it as negative and wave through exactly the
largest values. Every narrow maximum is below 2^63, so treating negative as
out-of-range is correct for all of them. C# needs no such term (`Unsigned`
delivers a `ulong`); Dart does, for the same reason as Java.

In an array arm the guard follows the fill guard, never precedes it: an
over-width scalar at an array id with no `arrayBegin` in front of it is a §7.3
skip, and guarding earlier would turn that skip into a spurious INVALID.

## §7.3/§5.2: an undeclared sequence is skipped whole (issues #268, #272)

`sequenceBegin` dispatches on `switch (cur)` with an inner `switch (id)`, and
neither had a default arm — so a sequence the schema does not declare at this
position left `cur` on the enclosing scope and its children bound there. A child
id 3 inside an unknown sequence set the root's own field 3; a sequence opened at a
string-array element position bound its string as that element.

Both switches now end in `default: cur = _DEAD; break;`, where `_DEAD` is a scope
no callback case matches:

```java
switch (cur) {
case 0: switch (id) {
    case 10: cur = 1; break;
    default: cur = _DEAD; break;      // undeclared id in a scope that has some
} break;
case 1: cur = _DEAD; break;           // a scope that declares none at all
default: cur = _DEAD; break;          // and any scope with no case above
}
```

The third arm is the one #272 needed: a leaf array scope had no `case` in the
outer switch, so the dispatch fell straight through and left `cur` untouched.
`sequenceEnd` pops the stack as before, which is what restores the live scope.
