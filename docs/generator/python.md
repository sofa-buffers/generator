# Python target — `targets.python`

Target-specific options, accepted under `targets.python`. Everything set in the
`generic:` section — `emit`, `license`, the `max_dyn_*` decode limits, … — is
documented once in the [generic config](README.md).

## Options

| key | type | default | meaning |
|---|---|---|---|
| `max_message_size` | integer | `4096` | Ceiling on a message's encoded size. Only reaches generated Python for a message the schema cannot bound, where it is emitted as `MAX_SIZE_LIMIT`; set explicitly it is also a budget a computed worst case may not exceed. Full semantics in the [generic config](README.md) and ARCHITECTURE §9.6. |

## The caller owns the encode buffer

CORELIB_PLAN §5.1: a corelib never allocates or grows an output buffer — the
caller does, and generated code **is** that caller. So `encode()` allocates the
storage and hands it to the encoder. Which shape that takes is decided by the
**schema**, not by the value, and every class carries the number it is sized
from:

```python
@dataclass
class Reading:
    ...
    MAX_SIZE = 49                      # derived: no value can encode to more
```

**Bounded** — every field carries a `count`/`maxlen`, so one exactly-sized
buffer holds any conformant value and no flush sink is needed:

```python
buf = bytearray(Reading.MAX_SIZE)
e = Encoder.over_buffer(buf, 0)
self.serialize(e)
return bytes(memoryview(buf)[: e.bytes_used()])
```

**Unbounded** — one field has no bound, so there is no worst case. `MAX_SIZE`
is then the configured *ceiling* (emitted as `MAX_SIZE_LIMIT`, with `MAX_SIZE`
aliasing it) and must **not** size a buffer: a larger message is legitimate and
would be silently refused. A fixed 512-byte scratch is drained into caller-owned
storage instead, so the memory an encode holds is the scratch, not the message:

```python
out: list[bytes] = []
scratch = bytearray(512)
e = Encoder.over_buffer(scratch, 0, out.append)
self.serialize(e)
e.flush()
return b"".join(out)
```

`list.append` is a **copying** sink in §5.1's terms — it is handed a fresh
snapshot and returns without installing a buffer — so the encoder keeps the
scratch and resumes at 0, with no take-and-replace handover.

`MIN_OUTPUT_BUFFER` binds only a buffer installed **with** a flush sink, so it
constrains the 512-byte scratch (trivially) and never the bounded arm. That is
what lets the bounded buffer be sized *exactly*, down to the degenerate case: a
class with no fields has `MAX_SIZE = 0` and encodes through a zero-byte
`bytearray`.

Three consequences worth knowing before you rely on them:

- **A value filled past its own schema bound is refused, not truncated.** It no
  longer fits the exactly-sized buffer, and `SofaBufferError` propagates out of
  `encode()` with nothing returned. `encode()` has no error channel other than
  the exception, and `sticky` is deliberately left off so the first failure
  surfaces there rather than being latched into `e.error` and discarded. Such a
  message used to be encoded and handed back — bytes every conformant receiver
  rejects as INVALID anyway (MESSAGE_SPEC §7.1).
- **The bounded arm allocates the schema's worst case, not the value's.** An
  array of 10 000 `u64` bounds each element at its 10-byte varint maximum, so
  the class gets `MAX_SIZE = 100003` and every `encode()` call allocates that
  much even for a ten-element value. Worth weighing before declaring an
  aspirational bound; the way out is `serialize()` with an encoder you construct
  yourself — `Encoder.over_buffer(buf, 0, flush=sink)` streams through a buffer
  of any size. There is deliberately no cached or module-level scratch:
  `serialize()` of a nested type can re-enter `encode()` on the same thread, and
  a shared buffer would corrupt the outer message.
- **`MAX_SIZE` is a class attribute**, so a schema field literally named
  `MAX_SIZE` (or `MAX_SIZE_LIMIT`) collides with it — `pyIdent` only mangles
  Python keywords. Java and C# carry the same exposure.

One throughput note, so the trade is on the record rather than a surprise in a
profile: corelib-py's *in-memory* encoder (the one `Encoder()` built) has a fast
path that hands a run at least as long as its buffer straight to the result
without copying it through — §5.1's optional **pass-through of a divisible run**.
A caller-owned encoder never takes it, so a large blob or string is now copied
through the buffer in bites. §5.1 makes pass-through opt-in *at installation*
("the caller has granted it") and `Encoder.over_buffer` exposes no permission
parameter, so generated code cannot grant it today; §5.1 also lets a port always
copy and stay conformant, so this is a corelib capability gap, not a
non-conformance. Small messages are unaffected; a large single field is where it
shows.

Both engines are covered: the pure-Python and native `_speedups` accelerators
carry independent `over_buffer`/`_put`/`_drain` implementations, and the native
one types its buffer parameter `bytearray`, so `tests/conformance/python/run.sh`
runs each encode-buffer leg twice.

## A decoded message owns its bytes

`decode()` returns a message that outlives the buffer it was decoded from: every
destination holds a `str`/`bytes` the corelib built, never a window into the
input, so the input may be reused or mutated the moment `decode()` returns.

Today that is *inherited* rather than arranged — CPython's `str`/`bytes` are
immutable and `io.BytesIO` copies its argument — but it is a guarantee, not an
accident of the implementation, so it is pinned by a test that scribbles over the
input buffer after decoding and re-encodes. A borrowing mode is deliberately not
offered and not configurable (ARCHITECTURE §9.6).

## `serialize` / `deserialize` are public — and are the streaming pair

Both used to be underscore-private (`_marshal` / `_unmarshal`), which understated
what they are: Python's only chunk-capable paths, in both directions.

```python
msg.serialize(Encoder.over_buffer(buf, 0, flush=sink))  # buffer < message
msg.deserialize(Decoder(reader))                        # pulls in chunk_size bites
```

`encode()` / `decode()` are the one-shot conveniences layered on them.

The `unmarshal` spelling is gone family-wide (ARCHITECTURE §8), but the
capability deliberately is not: `deserialize` is **pull**-shaped streaming — the
caller supplies any object with `read(n)` and the corelib pulls, refilling in
`chunk_size` bites. It is not a push `feed(chunk)`, and `corelib-py` has no
resumable decoder to build one on, so this is the shape Python offers.

## Receiver-side decode limits

The `max_dyn_*` caps are [generic options](README.md); what is specific to this
target is how they land in the generated code — as `MAX_DYN_ARRAY_COUNT`,
`MAX_DYN_STRING_LEN` and `MAX_DYN_BLOB_LEN` module constants, passed to the
corelib as `Decoder(max_array_count=…, max_string_len=…, max_blob_len=…)`. A
violation raises `SofaLimitError` before anything is allocated.

## Arrays — `count` is a capacity

Every array field maps to a Python `list`, and the list's length is the array's
length. A schema `count: N` is a **capacity**, not a length: it never reaches the
wire, it bounds the array (an element count or element id past `N` raises
`SofaDecodeError`), and it lets fixed-storage targets pre-size — but it never adds
elements.

What that means from Python:

| field | dataclass default |
| --- | --- |
| `count: 3` native (`u32`) | `field(default_factory=list)` |
| `count: 3` wrapper (`string`) | `field(default_factory=list)` |
| `count: 5` native with `default: [1, 2]` | `field(default_factory=lambda: [1, 2])` |
| count-less (either kind) | `field(default_factory=list)` |

- `Msg()` leaves a `count: N` array **empty** unless the schema declares a
  `default`, and a declared default shorter than `N` is materialized exactly as
  written (never tail-padded to `N`).
- Encode writes **every** element the list holds. `[1, 2, 0, 0]` and `[1, 2]` are
  different values with different bytes.
- Decode yields exactly the elements the wire carried: `len()` after a round trip
  equals `len()` before it, for both the compact scalar form and the wrapper form.
- A field is omitted only when it **equals its default** — for an array with no
  declared default, only when it is empty. An all-zero `[0, 0, 0, 0]` is a
  four-element value and stays on the wire.

Both array kinds now agree with each other and with the fixed-storage camp, whose
`count: N` slot is likewise a capacity holding `0..N` elements.

## Element sparsity is positional

Inside a wrapper-sequence array (string/blob/struct/union/nested-array elements)
the **interior is sparse** and the **last element is always written** — one rule
for both element kinds (MESSAGE_SPEC §2). An interior element equal to its element
default is omitted and leaves an id **gap**, which `unmarshalArray` restores from
that same default; the element at the highest index is the only one whose
*presence* carries the length (§5.1), so it is written whatever its value.

Two generator pieces implement it, and both read the position out of the **value**
at run time — the schema cannot answer it:

- `lastElemExpr` — the `or _i0 == len(...) - 1` disjunct on the `string`/`blob`
  leaf omit test, and on the write of a **native row** (which has no frame of its
  own, so the rule lands on the write itself).
- `emitSeqEnd` — the closer of a sequence-form element: `write_sequence_end_keep`
  at the last index, `write_sequence_end` (which drops the lazily-held frame) in
  the interior. A sequence-typed **field** — a struct/union field, an array
  wrapper — always takes the dropping closer, decided statically.

So `["a", ""]` → `06020a610a0207`, `["", ""]` → `060a0207`, `["", "x", ""]` →
`060a0a78120207`, and an interior all-default struct element vanishes
(`[{1},{},{3}]` → `06060001071600030707`) while a trailing one survives as an
empty frame (`[{},{}]` → `060e0707`). `[]` still vanishes entirely, so the empty
array keeps its zero-byte encoding — `["a", ""]`, `["a"]` and `[]` are three
distinct values with three distinct encodings.

Because an interior gap is now reachable, **every** element kind is placed at
`target[id]` on decode after gap-filling, never appended. The leaf paths always
did this; the matrix-row and wrapper-row collectors did not, and an appending
collector shifts every later row down by one as soon as a row is elided.

## On-demand corelib imports

The generated `message.py` imports from `sofab` only the names its own body can
name: `SofaDecodeError` (over-count / over-index / over-maxlen rejects) and
`FixlenSubtype` (the §7.3 guards where the wire type alone is ambiguous) are
emitted per schema, so a module never carries a dead name. Each gate
(`schemaHas*` in `backend.go`) is therefore a *mirror of an emitter* and must be
kept in lockstep with it — including where that emitter fires. Python has none of the compile-time help a typed language gives here: a
missing name is a `NameError` raised from `deserialize`, i.e. at decode time, on
an otherwise importable module.

The trap this walked into once (generator#246): a §7.3 guard is emitted at **two**
levels — for the field, and for every wrapper-sequence **element** down the array
element chain (`pyElemWireGuard`). A gate that inspects field kinds plus one level
of *native* array element misses `array<string>`, `array<blob>` and nested rows
such as `array<array<fp32>>`, all of which name `FixlenSubtype` from an element
guard while no field is fixlen. When adding a gate, ask which emitter it mirrors
and whether that emitter can fire for an element or a nested row; if it can, the
walk must descend. Every gate here descends today: `fieldHasFixlenGuard`,
`schemaHasCountedWrapperArray`, `schemaHasMaxlenStringBlob` — and
`schemaHasCountedNativeArray`, which mirrors the over-count reject and was the
second half of the same trap. That reject was once emitted for array *fields*
alone, so the gate scanned fields alone; when the reject moved to the native
*read* (see "Array counts are bounded at the read", below) it started firing for
nested rows too, and a schema whose only counted native array is a row —
`rows: array<array<u32 count: 3>>`, outer count-less — would have raised a
`NameError` instead of the reject. Assume nothing is top-level only.

## Array counts are bounded at the read

A wire element count `M` above a native array's schema capacity `N` is INVALID
(§3+§7.1) — rejected whole, never clamped. `emitCountGuard` emits that bound
alongside the native read in `unmarshalArray`, from the `.count` of the header
that delivered the array (`arrayFieldVar`: the message loop's `fld` at the top
level, the enclosing wrapper loop's `_ef<depth-1>` for a row), and always
*before* the read, so a message that is both over-count and truncated is INVALID
rather than INCOMPLETE (§5.2).

It lives with the read rather than at the field because a **nested native row**
(`array<array<u32>>`) carries its own `count` and is read through that very same
branch: the row's count header is the row element's header inside the wrapper
loop, which is the only place a row's capacity can be enforced at all. Bounding
the field instead left every row unbounded — Python accepted rows the rest of the
family rejects. Wrapper-element arrays have no count header and are bounded by
element id instead (§5.1, the over-index guard).

## Benchmark row

Row `python` in [`tests/bench/`](../../tests/bench/) (ARCHITECTURE §15), measured with
the **subtract** method. Tracked: Ir/op.

Change codegen here, then `./tests/bench/run.sh` and read the diff in
`tests/bench/results.txt`.

**Two engines, two rows.** corelib-py ships the pure-Python classes and a Cython
accelerator (`sofab._speedups`), and `sofab/__init__.py` takes the accelerator
whenever it imports, falling back silently otherwise. They are 7.2× (encode) and
4.8× (decode) apart, so one number cannot stand for both:

| row | encode Ir/op | decode Ir/op |
|---|---|---|
| `python` | 900,876 | 2,180,471 |
| `python-native` | 125,912 | 457,558 |

`python-native` builds the extension and verifies `sofab.IMPL == "native"` before
reporting — `setup.py` marks it `optional=True`, so a failed compile is not a failed
build, and without the check the row would quietly report the pure engine's cost. It
needs Cython (in the devcontainer image; pip-installed for that row in `bench.yml`).
`python` pins `SOFAB_PUREPYTHON=1`, because both rows share one corelib checkout and
would otherwise depend on which ran first. The engine each row got is recorded in the
`## toolchain` table as `sofab-engine`.

## §7.1: the declared integer width is a validity bound (issue #266)

A `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination rejects a value outside its
declared range with `SofaDecodeError`. Python's `int` is unbounded, so nothing
here ever masked the value — the defect was that an out-of-range value was simply
**kept**:

```python
self.a_u8 = d.unsigned()
if self.a_u8 > 255:
    raise SofaDecodeError("a_u8: value outside declared width u8")
self.d_u64 = d.unsigned()          # u64: nothing narrower to bound
```

The read-then-check order works here rather than reading into a temporary: the
guard reads better beside the store, and a raised decode never returns the object
it was filling. It is safe **only because a scalar integer arrives whole** — the
value and the bytes that carry it are the same word, so there is no truncation
that lands between them.

A `maxlen` is different, and reads the other way round. `string` and `blob` both
peek the parsed wire length **before** the payload is read:

```python
if d.fixlen_len() > 4:          # non-consuming peek
    raise SofaDecodeError("b: blob byte length above schema maxlen 4")
self.b = d.bytes()
```

§5.2 makes INVALID dominate INCOMPLETE, so a message truncated right after the
length word — where the violation is already fully established — must still be
INVALID. Reading first and measuring the decoded bytes afterwards never reaches
the check on such a message and reported INCOMPLETE instead. The string arm had
always peeked; the blob arm was reading first, and now does the same
(generator#267/#277).

Bounded string/blob **elements** of a wrapper array peek the same way — the blob
element arm only since generator#377. It was the one site of five in a generated
`message.py` still measuring the materialized bytes, so the identical truncation
one field over (`string_array`) was INVALID while `blob_array` reported
INCOMPLETE. The pull decoder makes the gap worse than a late verdict: the guard
is preceded by `d.schema_bounded()`, which switches the receiver-side
`max_blob_len` cap off for that element (§6.2.1), so nothing at all stood between
a sender-declared length — up to the §4.6 ceiling of 2 147 483 647 — and the
payload the decoder then waited for. Declaring the bound is a promise to enforce
it, and `fixlen_len()` is where that promise is kept.

A native array's **elements** are the same story one position deeper. The reader
returns the whole list, so an `any(...)` scan over it is exact for an array that
*arrives* — and never runs for one that does not, which is the case §5.2 is
about. So the width travels **with the read**, where the schema count and a
blob's `maxlen` already go:

```python
self.arr_u8 = d.read_unsigned_array(255)
if any(_v > 255 for _v in self.arr_u8):
    raise SofaDecodeError("arr_u8 element: value outside declared width u8")
self.arr_i16 = d.read_signed_array(-32768, 32767)
self.arr_u64 = d.read_unsigned_array()   # u64: nothing narrower to bound
```

A **dynamic** array passes the bound too: width is a property of the element
*type*, not of the array *length*. Enum and bitfield elements pass nothing — the
corelib already returns their full range.

The scan **stays** alongside it (generator#267). The two answer different
questions: the reader's bound is what makes a *truncated* array INVALID, and the
scan is what still bounds the elements if a consumer builds against a corelib
whose readers ignore the arguments. It costs one pass over a list already in hand.

corelib-py applies the bound at different points in its two engines, and the
difference is deliberate rather than an inconsistency: the Cython engine checks
**at the element**, before the value is boxed, because two typed compares in C
are free; the pure engine checks **at the truncation**, turning the
`SofaIncompleteError` it was about to raise into a `SofaDecodeError` when an
element already decoded breaches the bound, so its Python decode loop pays
nothing per element. Same verdicts either way — which is what the shared vectors
pin, and what the conformance suite's reproducer-plus-control pair asserts.
