# Kotlin target — `targets.kotlin`

Target-specific options, accepted under `targets.kotlin`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

The target emits **Kotlin Multiplatform-ready** sources against
[`corelib-kotlin-mp`](https://github.com/sofa-buffers/corelib-kotlin-mp): the
generated message files use the Kotlin standard library and `sofab` and nothing
else, so one set of files compiles for the JVM, for Node and the browser, and as
a native binary — with the same wire bytes on every one of them. Only the
`emit: project` scaffolding is JVM-specific, because a harness needs a `main`, a
process exit code and a stdin.

## Options

| Option | Type | Default | Effect |
|--------|------|---------|--------|
| `package` | string | `message` | The `package <name>` declaration of the generated files, and the source-directory layout in project mode. |

```yaml
targets:
  kotlin:
    package: com.myproj.messages
```

## Emitted files — one declaration each

```
src/main/kotlin/<pkg>/
    <Message>.kt   public class <Message>  + the internal <Message>Visitor
    <Type>.kt      public class <Type>     one per schema struct/union
                   public object <Type>    one per schema enum/bitfield (named constants)
```

There is no shared support file. Element placement, array growth, payload
reassembly and UTF-8 materialisation are static under ARCHITECTURE §8 — the same
shape for every schema, with the capacity, the index and the length carried as
arguments — so they are the corelib's `Seq`, `PayloadAcc` and `Utf8`, and
generated code calls them (generator#345).

`<Type>` is the canonical IR name: a `$defs` type keeps its category prefix
(`#/$defs/enum/Mode` → `EnumMode`, `#/$defs/struct/Point` → `StructPoint`) and an
inline one is qualified by where it was declared (`MyMsgSomestruct`), so two
inline types of the same field name in different messages cannot collide. This is
the same naming every backend uses.

A type reached from two messages is emitted **once**: Kotlin would allow several
public declarations per file, but two declarations of one type in a package do
not compile, and one-file-per-type is the simplest spelling of that rule.

`<Message>Visitor` is `internal` on purpose — generated plumbing, not schema
surface.

## The public API

The closed name set (CORELIB_PLAN §6.1.1), in Kotlin casing:

```kotlin
val person = Person()
person.name = "Ada"

val bytes = person.encode()          // one-shot convenience
val back  = Person.decode(bytes)     // one-shot convenience

person.serialize(os)                 // streaming out: this object's fields, nothing else
person.encodeTo(os)                  // ...and flush the tail the last write left

val st  = Person.tryDecode(bytes, into)   // the status, without an exception for INCOMPLETE
val dec = Person.decoder()                // streaming in
dec.feed(chunk1); dec.feed(chunk2)        // COMPLETE / INCOMPLETE / INVALID per feed
val msg = dec.finish()                    // once the caller's framing says the input is over
```

`serialize` and `encodeTo` are **not** two spellings of one thing. `serialize`
writes this object's fields and nothing else, so a nested message can be written
into a frame its parent already opened; `encodeTo` is the entry point for a
caller who owns the stream, and it flushes the tail. Calling only `serialize` on
a top-level message and forgetting the flush truncates the output, which is the
whole reason the second name exists.

### `decode` is strict about the three-valued outcome

Kotlin is an exception language, so the one-shot `decode` "fails in the language's
own way" for **both** non-`COMPLETE` outcomes:

| input | `decode(bytes)` | `tryDecode(bytes, out)` |
|---|---|---|
| valid, complete | the object | `COMPLETE` |
| ends mid-field / inside an open sequence | `IllegalStateException` | `INCOMPLETE` |
| malformed | `SofabException(INVALID_MSG)` | `SofabException(INVALID_MSG)` |

The two exceptions are deliberately different types: an incomplete message is not
a malformed one — nothing is wrong with the bytes, the caller declared
end-of-input at a point they did not agree with. This target has no back-compat
surface to preserve (the same position Zig is in), so `decode` does not quietly
hand back a half-filled object; reach for `tryDecode` when the verdict is the
answer you want rather than an exception. `Decoder.finish()` draws the same line
for the streaming path.

`tryDecode` **resets `out` first**. Absence is the encoding of an all-default
field (MESSAGE_SPEC §2) and an absent field fires no callback at all, so a reused
destination has to be re-armed before the feed — the last point at which absence
is still observable. `reset()` is public for a caller who drives the corelib's
`IStream` directly and has no `tryDecode` to hook.

## Types — the narrowest correct one

Every integer maps to its **exact declared width**, unsigned included
(ARCHITECTURE §8): a `u8` is a `UByte`, not a widened `Long` the caller has to
mask, and a `u8[]` is a `UByteArray`. That is the C# position rather than Java's —
Java widens everything to `long` because it has no unsigned type, which is not a
constraint here. The unsigned types are inline value classes, so a scalar member
costs the same machine word its signed peer does and nothing is boxed on the hot
path.

| schema | field | array field |
|---|---|---|
| `u8` `u16` `u32` `u64` | `UByte` `UShort` `UInt` `ULong` | `UByteArray` `UShortArray` `UIntArray` `ULongArray` |
| `i8` `i16` `i32` `i64` | `Byte` `Short` `Int` `Long` | `ByteArray` `ShortArray` `IntArray` `LongArray` |
| `fp32` `fp64` | `Float` `Double` | `FloatArray` `DoubleArray` |
| `boolean` | `Boolean` | `BooleanArray` |
| `string` | `String` | `MutableList<String>` |
| `blob` | `ByteArray` | `MutableList<ByteArray>` |
| `enum` | `Int` | `IntArray` |
| `bitfield` | `ULong` | `ULongArray` |
| `struct` / `union` | the generated class | `MutableList<T>` |
| `array` (nested) | — | `MutableList<`*inner container*`>` |

Every native element kind has a primitive array — `boolean` included — so a
native array never boxes. That reaches one level in: a nested array of natives is
`MutableList<UIntArray>`, not a list of boxed lists.

What is Kotlin-specific is that the exact width costs **nothing at the corelib
boundary**. The unsigned array types are inline classes over their signed peers,
so `asIntArray()` and friends are a reinterpretation rather than a conversion: a
`UIntArray` field's own backing array is what reaches
`writeArrayUnsigned(IntArray)`, and the corelib's bulk decode offer is handed that
same view, so it fills the field itself. No copy and no per-element widening, in
either direction.

### `enum` and `bitfield` are the deliberate exceptions

Both are pinned at the width that **cannot lose a legal value**, not at the
narrowest one that fits the declared members:

- **`enum` → `Int`.** An enum is bounded by the signed 32-bit range
  (MESSAGE_SPEC §1), so `Int` holds every value the schema can declare, exactly.
  A narrower backing (a `Byte` for a two-member enum) would have to truncate a
  wire value outside it, and silent truncation is the one answer §7.1 rules out
  everywhere it speaks. A value outside the 32-bit range is rejected as
  `INVALID`, like any other over-width integer.
- **`bitfield` → `ULong`.** Flag positions run 0..63 and the wire word is an
  unsigned varint, so `ULong` *is* the domain; there is no value to lose and no
  guard to emit.

Neither lowers to a Kotlin `enum class`. A closed `enum class` cannot represent a
wire value the schema does not declare, and a decoder has to survive one —
forward compatibility is not optional. What it *can* have is the documentation:
the declared members are emitted as named constants in an `object` beside the
field, each carrying its schema `description` (and, for a flag, its `default`).
So this target renders per-constant metadata that C and Java have no symbol for.

```kotlin
telemetry.mode = EnumMode.RUNNING            // an Int constant, not a closed variant
telemetry.status = telemetry.status or BitfieldFlags.ready
```

### Field names

Kotlin has a real escape, so a field whose name is a hard keyword keeps the
schema's spelling: `object` becomes `` `object` ``, at the declaration and at
every use site. Soft keywords (`where`, `data`, `by`, …) are already legal
identifiers and are left alone. The wire is unaffected (fields are keyed by id)
and the JSON name stays the schema's.

A name that would collide with a **generated member** (`encode`, `reset`,
`serialize`, …) is a different problem — an escape cannot help against another
declaration — so it is mangled with a trailing underscore instead.

## Arrays — `count` is a capacity

A schema `count: N` is a **capacity**, not a length. It never reaches the wire,
it bounds the array (an element count or element id past `N` fails the decode as
`INVALID`), and it lets fixed-storage targets pre-size — but it never adds
elements. What you can observe from Kotlin:

- `Msg()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialised exactly as
  written (never tail-padded). `reset()` re-arms to the same value.
- Encode writes **every** element the container holds. `uintArrayOf(1u, 2u, 0u, 0u)`
  and `uintArrayOf(1u, 2u)` are different values with different bytes.
- Decode yields exactly the elements the wire carried; `size` after a round trip
  is what went in, for the compact scalar form and the wrapper form alike.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero four-element array is a
  four-element value and stays on the wire.

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse**: an element equal to the element default is dropped
and leaves an id gap, which decode restores from that same default. The **last**
element is always written — as its value, or as an empty frame for a
struct/union/nested element — because its presence is what carries the length
(*highest present id + 1*). So `["a", ""]`, `["a"]` and `[]` are three distinct
values that encode and decode distinctly.

Because an interior gap is ordinary, every element is **placed at its element id**
on decode — matrix rows included (`Seq.reserveRow*`) — never appended. Appending
would shorten the array by any interior gap *and* turn a re-opened element id into
a second element instead of merging into the first (§7.4), which placement gives
for free.

## Storage: heap only, and why there is no `allow_dynamic`

ARCHITECTURE §14 asks every target to answer the storage question explicitly.
Kotlin's answer is that the axis does not exist here: the platform types are heap
objects on every one of the target runtimes (a `UIntArray` is a JVM array, a typed
array on JS, an allocation on native), and a schema bound cannot be turned into
inline storage the way a C++ `InlineVector<T, N>` or a Rust `heapless::Vec<T, N>`
can. What *is* reachable — and is taken — is holding a
value at its declared width instead of a widened one, and letting the corelib
fill a schema-bounded array in place rather than through a per-element callback.

`max_message_size` is honored: a bounded message encodes through one exactly-sized
buffer, an unbounded one through a fixed scratch drained by a flush sink (below).

## Encode buffers — generated code owns them

The corelib allocates nothing (CORELIB_PLAN §5.1), so the buffer is the generated
code's, and the two cases are different shapes on purpose:

- **schema-bounded** — one exactly-sized `ByteArray(MAX_SIZE)` handed to
  `OStream(buf)`. No flush can occur, so a value filled past its own declared
  bound does not fit and is *reported* (buffer-full) rather than emitted short.
- **unbounded** — `MAX_SIZE` is then an imposed ceiling (`MAX_SIZE_LIMIT`, from
  `max_message_size`) rather than a worst case, and must **not** size a buffer: a
  larger message is legal and would be silently refused. Such a message encodes
  through a fixed 512-byte scratch with a `FlushSink` draining into a growable
  accumulator, so memory is bounded by the scratch and not by the message.

`encodeTo(os)` is the sink shape for both, the caller's stream being the drain.

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land — as constants on the generated visitor, checked at the
wire count/length header **before any allocation or accumulation**, never as a
clamp. A violation throws `SofabException(SofabError.LIMIT_EXCEEDED)`,
deliberately a different category from `INVALID_MSG`: the bytes are well formed
and decode under a looser or unset limit, so policy divergence between two
differently configured receivers is not a conformance divergence.

They govern **only** fields the schema left unbounded. A schema-bounded field
keeps its own bound, whose violation is `INVALID` — so the two never both fire.
A configured limit is inert (no constants, no guards, byte-identical output) when
the schema has no unbounded field of its kind.

## Strict UTF-8

A Kotlin `String` is a Unicode type, so this target is **always strict**
(MESSAGE_SPEC §8): the corelib's `SOFAB_STRICT_UTF8` switch is a documented no-op
here and no config key is threaded into generated code. `ByteArray.decodeToString()`
with its default settings substitutes `U+FFFD`, which §8 forbids in every mode, so
the corelib's `Utf8.decode(...)` validates the raw bytes first and converts only
what passes — which is what `PayloadAcc.string(...)` hands back on the chunk that
completes the payload. Invalid bytes become `SofabException(INVALID_MSG)` — the
same channel as the schema-bound guards. Encode-side strictness is the corelib's (`OStream.writeString` refuses an
unpaired surrogate before writing a byte).

**Only a materialized string is validated.** The corelib delivers every
fixlen-string payload to `string(...)` — an unknown id and a §7.3 wire-type
contradiction included — so the callback opens by resolving the destination and
returns before a byte is buffered, decoded or checked. That is what makes the
skip a true skip, and it also stops a skipped payload's bytes from entering the
shared reassembly accumulator where a later declared field could inherit them.
The `maxlen` and `max_dyn_string_len` pre-checks sit behind that guard; they are
destination-scoped themselves, so the INVALID-over-INCOMPLETE ordering is
unchanged. A schema that declares no string at all gets an **empty**
`string(...)` body rather than a guarded one: every string reaching it is skipped
by definition, and decoding one only to drop it is the same violation with every
string skipped instead of some.

`blob(...)` needs no such validation — bytes carry no encoding.

## A mis-typed array header is skipped, not decoded

MESSAGE_SPEC §7.3 skips a field whose header wire type contradicts its declared
type. corelib-kotlin-mp settles almost every case *structurally* — a mismatched
header lands in a differently-typed callback with no arm for that id — but not
the array kinds: it streams an array's elements through the **same**
`unsigned()`/`signed()`/`fp32()`/`fp64()` callbacks a lone scalar uses, so an
array header at a scalar-declared id of the same shape would otherwise be stored
element by element.

So **skipping is the default**. `arrayBegin` arms `askip = count` up front; only
an arm for an id that really declares a native array **of the announced kind**
disarms it and arms the mirror fill counter. The shared callbacks discard while
armed. It self-terminates on the announced count (no array-end callback needed),
survives a chunk boundary (the counter lives in the visitor), leaves a legitimate
array untouched, and still decodes a real scalar arriving at that id afterwards.

One arm per wire kind, matching the encoder exactly:

| declared element | wire array kind | elements arrive in |
|---|---|---|
| `u8`…`u64`, `boolean`, `bitfield` | `ArrayKind.UNSIGNED` | `unsigned()` |
| `i8`…`i64`, `enum` | `ArrayKind.SIGNED` | `signed()` |
| `fp32` | `ArrayKind.FP32` | `fp32()` |
| `fp64` | `ArrayKind.FP64` | `fp64()` |

Arming per kind is only half the rule. §7.3 also forbids decoding the payload
*into the declared field*, and **sizing** the destination is decoding into it: an
`ArrayKind.SIGNED` header at a `UIntArray`-declared id must not leave that field
holding a one-element array the wire never carried — the leak is the length, not
the element. So every arm is fronted by its `if (kind == ArrayKind.X)` test,
emitted **before** the schema-`count` bound. The order is normative: the bound
applies only to a field that survives §7.3, so an over-count *mis-typed* array is
skipped rather than rejected as a false `INVALID`.

The fixlen kinds are per subtype rather than one collapsed bucket, because a
fixlen array's `fixlen_word` names its element width and the corelib announces the
array only after reading it (CORELIB_PLAN §4.8). An `fp64` header arriving at a
declared `fp32[N]` slot therefore leaves that arm untouched: the discard counter
stays armed, and the declared `FloatArray` is not sized, cleared or allocated.

### Where an array element is stored

`arrayBegin` resolves the destination once — kind test, schema bound,
disarm/arm, size the container — and parks *which* destination in `atgt`. The
element callbacks then open with

```kotlin
if (afill != 0) { afill--; when (atgt) { … }; return }
```

so an element is stored against the already-resolved target instead of being
routed through the scope switch and an id switch again, once per element. Only a
fill `arrayBegin` armed can be open when a callback runs, so `afill != 0` is the
whole test and the array ids leave the scalar switches entirely.

For an integer array the schema bounds with a `count: N`, the destination is
already exactly `count` long by the time the elements arrive, so it can skip the
element callbacks altogether: `arrayBegin` parks it in `abulk`,
`Visitor.arrayBulk(id, kind, count)` hands it over, and the decoder fills it
directly (ZigZag already applied for a signed array). Its **element width is what
tells the decoder the declared width** — a `ByteArray` says `u8`/`i8`, so a value
that does not fit fails with `INVALID_MSG` rather than being truncated, checked in
the same pass that decodes. Kotlin's unsigned arrays make this free: `asIntArray()`
is the field's own backing array, so there is nothing to copy back.

The offer is made **only** for a schema-bounded integer array: `count` is the
wire's claim, and an unbounded array must not be allocated against it — it keeps a
capped reservation and the grow-as-you-go element fill. Boolean arrays, fp arrays
and matrix rows (whose cap bounds the row's *id*, not its element count) keep the
element path.

## Sequence framing — which closer `serialize` emits

MESSAGE_SPEC §2 omits a sequence-typed **field** whose value equals its declared
default instead of framing it empty, while a wrapper-array **element** keeps its
frame: element presence is what carries the array's length, so dropping an
all-default element would change the decoded length rather than merely the bytes.

`serialize` opens **every** sequence with `os.writeSequenceBeginLazy(id)`, which
holds the header back until a child field is actually written. Since the per-field
sparse rule already omits every child equal to its default, "not one child was
written" *is* "the object equals its declared default", evaluated per field and
recursively — no byte image is ever compared, and no sub-message is buffered.
What differs is the **closer**:

| emission site | closer | why |
|---|---|---|
| `struct` / `union` field | `writeSequenceEnd()` | absence reconstructs the same value |
| array field (the wrapper) | `writeSequenceEnd()` | its default is the empty collection |
| wrapper-array element (`struct`/`union`/nested row) | `writeSequenceEndKeep()` at the last index | presence carries the array length |

The closer for an element is positional in the **value** (last vs interior), not
static from the schema: an interior all-default element is dropped and leaves an
id gap, and only the last one is forced out. Consequence: an all-default message
encodes to **zero bytes**.

The predicate behind the lazy framing is also emitted explicitly, as an
`internal fun isDefault()` on every generated class. Each of its arms is literally
the corresponding `serialize` write guard, built from the same expression, so the
two cannot state different truth tables — a predicate that narrows where the
writer does not (or the reverse) omits a field that is on the wire, or keeps one
that is not.

## Schema bounds are latched at the word that carries them

`INVALID` dominates `INCOMPLETE` (CORELIB_PLAN §5.2), so a bound that is already
established by the bytes seen must be decided there and not once the payload
arrives — otherwise a message truncated right after the deciding word reports
`INCOMPLETE` while the same bytes read whole report `INVALID`, and the verdict
depends on where the chunk boundaries fell.

| bound | decided at |
|---|---|
| array `count: N` exceeded | the count word, in `arrayBegin` |
| wrapper element id `≥ N` | the element's fixlen word, in `fixlenBegin` (and again at the store) |
| string/blob `maxlen` exceeded | the fixlen length word, in `fixlenBegin` (and again at the store) |
| integer outside its declared width | the store arm, which is where the value first exists |

Every `fixlenBegin` guard sits **inside** the declared-subtype test. The hook
fires for whatever subtype arrived — the corelib resolves what arrived but cannot
know what was declared — so a contradicting subtype is a §7.3 skip and must not
have its length measured against this field's bound.

For a wrapper element the over-index test comes **before** the element `maxlen`:
an element that is not this array's element at all must not have its length
measured against the element bound.

The payload-side guards stay in place. They are unreachable for a message that
reaches the header hook, and they are the only thing still bounding a consumer
built against a corelib that predates it.

## The declared integer width is a validity bound

A wire value outside the declared width is malformed input (MESSAGE_SPEC §7.1):
it **must not** be masked to the width and **must not** be kept. The check cannot
live in the corelib — it accumulates every integer into a 64-bit accumulator and
delivers *that*; only the schema knows the destination was declared `u8` — so the
guard is emitted in the store arm, beside the `count`, element-id and `maxlen`
guards. It covers message fields, struct/union members and native array elements
alike, plus `enum` at its signed 32-bit range. The 64-bit kinds span the delivery
accumulator itself, so no reachable value can breach them and **no guard is
emitted** for them.

Placement inside an array arm is normative rather than cosmetic: the guard goes
*after* the fill test, never before. An over-width scalar arriving at an array id
with no `arrayBegin` in front of it is a §7.3 skip, and hoisting the guard would
turn that skip into a spurious `INVALID`.

## An undeclared sequence is skipped whole

A skip is only correct if it is **scoped** (the children go with the parent),
**inert** (it arms nothing a later field will read) and **free to unwind**.
`sequenceBegin` therefore has a dead-scope default in every scope — including a
message that declares no sequence at all — so a sequence at an id this scope does
not declare moves `cur` to a value no callback arm matches and the whole subtree,
nested sequences included, is discarded. The live scope is restored at the
matching end.

The scope stack is grown, not schema-sized, so an unknown sequence nested to the
format's `MAX_DEPTH` cannot overflow it.

## Benchmark row

Row `kotlin` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured
with the **subtract** method — Kotlin compiles to JVM bytecode and the JIT
compiles the hot path at runtime, so there is no native symbol to collect on.
Tracked: Ir/op.

The Kotlin Gradle plugin refuses to run on a JDK newer than it knows, so both the
bench row and `tests/conformance/kotlin/run.sh` take their JDK from
`SOFAB_KOTLIN_JDK` when it is set (the devcontainer exports it), falling back to
`JAVA_HOME` and then to the JVM on `PATH`. It is a separate knob on purpose:
pointing the whole run at another JVM would move the `java` and `csharp` rows for
a reason no generator change caused, and `lib/format.py` resolves the same knob
into the `## toolchain` table so a row measured on a second runtime says so.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

## Known limitation: an fp32 signaling NaN on Kotlin/JS

`fp32` maps to `Float`, which is a true IEEE-754 binary32 on Kotlin/JVM and
Kotlin/Native — a value, NaN payloads included, round-trips bit-for-bit. On
**Kotlin/JS** every `Float` is a double at runtime, so passing a *signaling* NaN
through one quiets it, exactly as it would in JavaScript or Dart.

Generated code takes the value path (`writeFp32` / `Visitor.fp32`) rather than
the raw-bits pair the corelib also offers (`writeFp32Bits` /
`Visitor.fp32Bits`), which is the same choice every native-`f32` target in the
family makes, and the shared conformance vectors exclude NaN from the fp32 cases
for this reason. A caller who needs bit-exact NaN relay on the JS target can drive
the raw-bits pair directly — it is corelib API and needs no generated support.

## Project mode

`emit: project` adds the scaffolding around the sources:

```
settings.gradle.kts   mavenLocal first, so a conformance run resolves the corelib under test
build.gradle.kts      kotlin("jvm") + application; -Psofab.version pins the corelib
src/main/kotlin/<pkg>/Json.kt   canonical-JSON to/from, plus a hand-written reader
src/main/kotlin/<pkg>/Main.kt   the harness: encode / decode / trydecode / stream / bench
README.md
```

```sh
./gradlew installDist
build/install/harness/bin/harness encode <message> < in.json
```

The JSON reader is hand-written rather than a dependency so the project needs
nothing the corelib does not, and so a `u64` above 2^53 survives: a number is kept
as its literal **text** and parsed at the field's declared width, which a
double-based parser cannot do.

The `stream` mode is what covers the two paths `encode`/`decode` cannot. It
encodes through a **one-byte** output window with a flush sink and asserts the
bytes are identical to `encode()`'s — the corelib splits every atomic unit, so any
buffer at or above `MIN_OUTPUT_BUFFER` must produce the one-shot bytes, and a
generated `serialize` that held something back itself would break exactly that.
Then it feeds those bytes to the generated `Decoder` **one at a time**, so decoder
state has to survive a boundary at every byte offset, and prints the result for
the conformance runner to compare against the one-shot decode.

The message sources themselves are unchanged between the two emit modes, and stay
platform-free — move `src/main/kotlin` into a multiplatform `commonMain` as it is.

That is checked rather than asserted, twice and at two different depths. A
hermetic unit test sweeps the whole corpus for any JVM-only reference
(`java.`/`javax.`/`kotlin.jvm`/`ThreadLocal`/`StandardCharsets`/…), which is the
realistic way to break it — each of those has a common-Kotlin answer here
(the corelib's `PayloadAcc` for the chunk accumulator, its `Utf8.decode` for the
strict decode, a per-call buffer instead of a thread-local scratch), and the guard
is what keeps them. The conformance harness then type-checks the same sources as
`commonMain` with the real compiler (`compileCommonMainKotlinMetadata`), which
sees the whole common stdlib surface rather than a list of names. Metadata only:
a JS or native compilation would prove the same thing and pull a Node
distribution or the Kotlin/Native toolchain to do it.
