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
every mode, so the visitor decodes through a generated `_utf8(...)` helper backed by
a REPORTing `CharsetDecoder` (`onMalformedInput`/`onUnmappableCharacter` =
`REPORT`); a `CharacterCodingException` becomes `SofabException(INVALID_MSG)` — the
same channel as the over-count guards. The check runs once the full `total` bytes
are present. Encode-side strictness is corelib-side (`OStream.writeString`).

## §7.3: an integer array at a scalar id (issue #183)

MESSAGE_SPEC **§7.3** skips a field whose header wire type contradicts its
declared type. This backend's corelib settles almost every case *structurally* —
a mismatched header lands in a differently-typed visitor callback with no case for
that id — but not one: it streams an integer array's elements through the **same**
`unsigned()/signed()` callbacks a lone scalar uses, so an integer array header at a
scalar-declared id of the same signedness would be stored element by element.

The generated visitor therefore carries a skip counter. `arrayBegin` arms
`askip = count` when the announced kind is the unsigned or signed integer kind
and the `(scope, id)` pair is **not** a declared integer-element native array;
the two scalar callbacks then discard while armed. It self-terminates on the
announced count (no array-end callback needed), survives a chunk boundary (the
counter lives in the visitor), leaves legitimate arrays untouched, and still
decodes a real scalar arriving at that id after the array. The fp arrays are never
armed — their elements go to the float callbacks and cannot reach a scalar arm.

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

A wrapper array's declared `default` is not materialized by this backend (the
field initializer is the empty collection), so absent and explicitly-empty denote
the same value and the plain dropping close is correct. If that gap is ever closed,
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
  from, so a trailing default element is significant and keeps its frame.
- **Decode default-fills a `count: N` wrapper array back out to `N`** when the
  sequence scope closes (`sequenceEnd`). §5.1 makes the length `N` "for every
  target — a growable-list target MUST default-fill to `N` exactly like a
  pre-sized one". Native arrays already got this from `arrayBegin`; wrapper arrays
  are `List`-backed and did not. Without it the trailing elision would not
  *re-shape* the array, it would **shorten** it on every round trip.

The narrowing expression is generated once (`elemTrimExpr`) and used by both the
marshal loop and `isDefault`. If the predicate narrowed a field the writer did not
(or the reverse), the result would be a field omitted though it is on the wire, or
kept though it is not.

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
| `List`-backed array (wrapper, `boolean`) | `Sbuf.resetList` — `clear()`, keeping the backing capacity; a `boolean` array's materialized default is then `addAll`ed back |
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
