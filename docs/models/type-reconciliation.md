# Type reconciliation: moving §7.3 out of generated code

> **Status: proposal, partly built.** §13 steps 1–3 exist as draft PRs
> (generator#237 + corelib-cpp#53) and are behaviour-neutral; §11 is the open
> design question, with the numbers to decide it in §10. Reader models are
> [`decode-reader-models.md`](decode-reader-models.md); the wire/corelib contract
> is ARCHITECTURE §9, the §7.3 decode verdict is ARCHITECTURE §7.3.

MESSAGE_SPEC **§7.3**: a field whose header wire type does not match the one its
declared type maps to — for `fixlen`, including the subtype — is **skipped**,
exactly like an unknown id. The field keeps its default and the message stays
valid.

This document is about **where that rule is implemented**. Today it is
implemented ten times, in generated code. It should be implemented once per
corelib, at a single seam — and that seam then also carries a diagnostic channel
for skipped fields.

---

## 1. The problem

Generated decode for `examples/messages/example.yaml`, counting the wire-type /
subtype comparisons the *generator* emits:

| Target | Model | Type checks in generated code | Where §7.3 actually lives |
|---|---|---|---|
| **c** | E (descriptor) | **0** — the whole decode file is 53 lines of descriptor | corelib (`object.c`, `_expected_wire_opt`) |
| go | C | 0 for values (7 in the maxlen hook only) | corelib (typed callbacks) |
| dart | C | 0 for values (7 in the maxlen hook only) | corelib (typed callbacks) |
| rust | A1 | 18 (`askip`/`afill`) | split |
| zig | A1 | 16 | split |
| java | A1 | 22 | split |
| csharp | A1 | 16 | split |
| **cpp** | D | **49** → **0** after §13 steps 1–3 | generator → corelib |
| **cpp (`corelib: c-cpp`)** | E wrapper | **49** | generator |
| **typescript** | B | **59** | generator |
| **python** | A2 | **57** | generator |

The consequence is visible in the issue history: **#174, #183, #189, #224, #229**
— five issues over several months, every one of them the same shape, *one backend
forgot one type gate*. That is not a run of bad luck; it is the predictable cost
of writing one rule ten times by hand.

Two secondary symptoms of the same cause:

- **`askip`/`afill` in rust/zig/java/cs.** A streaming visitor delivers array
  elements through the *same* callback as scalars (`unsigned(id, value)`), so the
  generated code cannot tell "element 3 of the array at id 15" from "the scalar at
  id 15". Without a discard counter, an array arriving at a scalar id silently
  assigns its last element. That is wire-level disambiguation that leaked into the
  generator because the event stream drops the array context.
- **`corelib-cpp::read<T>()` does not check anything.** Its doc says *"The
  requested type must match the field's wire type"* — a precondition on the
  caller, not a check. It zig-zags on `T`'s signedness alone, which is exactly why
  a mismatched field silently decoded to a wrong value before #174.

---

## 2. Root cause: a typed read is ambiguous — and should not be

A typed read call *can* carry the expectation. `corelib-c-cpp` does exactly that:

```c
static inline void sofab_istream_read_u32(sofab_istream_t *ctx, uint32_t *var)
{
    sofab_istream_read_field(ctx, var, sizeof(uint32_t),
        SOFAB_ISTREAM_OPT_FIELDTYPE(SOFAB_TYPE_VARINT_UNSIGNED));   /* the expectation travels along */
}
```

But a mismatch then has two possible meanings, and the corelib cannot tell them
apart, because the one thing that would distinguish them — the *provenance* of the
expectation — is not in the call:

1. the caller bound the wrong type → a local bug (usage error);
2. the wire disagrees with the schema → §7.3 skip, message still valid.

Today `corelib-c-cpp` answers "1" (`SOFAB_RET_E_USAGE`, `istream.c`), and
`object.c` therefore has to run a *second*, earlier comparison against
`_expected_wire_opt` purely to avoid the usage error its own library would raise
for a perfectly valid message. **Two checks for one comparison, one of them
existing only to suppress the other.**

### The distinction cannot be drawn, and does not need to be

- **Provenance is not observable.** The corelib sees "expected X, wire has Y".
  Whether X came from generated, schema-derived code or from a hand-typed call is
  not visible at the comparison.
- **Skip is always representable.** SofaBuffers has no required payload fields —
  every field has a default, an absent field is a normal state. A skipped field
  can therefore never corrupt data; it yields the default.
- **The usage error is the harmful default.** It aborts the decode of a message
  that is valid per spec, i.e. it converts the forward-compatibility case §7.3
  exists for into a hard failure.
- **The family has already decided this way, de facto.** A decode-time usage error
  is raised in exactly two corelibs: `corelib-c-cpp` (`istream.c`) and
  `corelib-go` (`decoder.go`, the secondary *pull* API — generated Go uses the
  Visitor and never reaches it). Elsewhere the error class is dead taxonomy
  (`corelib-rs`/`rs-no-std`: never raised, "reserved for §6.3 baseline parity"),
  encode-only (`ts`, `java`), absent (`dart`, `zig`), or documented but unused
  (`py`: `SofaStateError`'s docstring claims decode-time type mismatch; it is only
  raised by the encoder). The C reference is the outlier, not the norm.

`corelib-go` also shows the arbitrariness of the split inside one file
(`decoder.go:287`):

```go
// Bytes. A wrong field type is ErrUsage; a mismatched subtype is ErrInvalidMsg.
```

Wrong wire type → *my* fault. Wrong subtype → *your* fault. Same question, same
function, two answers.

---

## 3. Decision: a type mismatch is a skip, always

**A declared-type-vs-wire-type mismatch is skipped, in every corelib, with no
configuration.** The diagnostic value of "the caller may have bound the wrong
type" is real, but it belongs on a *development-time* axis (§6), not in the wire
control flow.

### The boundary — what stays INVALID

"Mismatch = skip" applies **only** to *declared type vs. wire type/subtype*. It
does not apply to:

- a malformed fixlen word (reserved subtype, fp length ≠ 4/8) — §4.6/§7;
- a wrapper-array element index ≥ `count` — §5.1;
- a count/length above the schema bound **at a matching type** — §7.1/§5.2.

Otherwise real corruption would be reclassified as forward compatibility.

---

## 4. The type tag

Wire type and fixlen subtype are only meaningful **together**: `fp32`, `fp64`,
`string` and `blob` all share `Wire::Fixlen`, so the wire type alone identifies
nothing for a quarter of the format. Both #224 and #229 were literally *"the check
looked at the wire type but not at the subtype"*.

> **Contract.** Expected and actual are each **one type tag** that combines wire
> type and fixlen subtype and is compared **as a whole**. The representation is
> language-specific; comparing only half must not be expressible.

This is not primarily a size optimization — it makes the wrong comparison
unwritable.

- **C** — a packed byte, wire type in bits 0..2, subtype in bits 3..5. This
  already exists as `target_opt` with mask `0x3F`. Note that `target_opt` is an
  *options* byte, not a pure tag: bit `0x40` (`STRINGTERM`) is a read-side option
  that does not exist on the wire and is masked out. If the tag becomes a contract
  term, define it as its own 6-bit type rather than exporting the C-internal
  options convention to nine other corelibs.
- **C++** — a small value type (`struct TypeTag { Wire wire; Fix fix; }`) with
  `operator==`; readable in a debugger, one comparison.
- **Managed languages** — two fields or a small value type, as long as equality is
  *one* operation.

The non-fixlen case needs no sentinel: the subtype half is simply unset.

---

## 5. The seam

> **"Seam"** here means *the single point where the generated code's expectation
> meets the corelib's knowledge of the wire, and where the match is decided*. The
> term is borrowed from refactoring literature (Feathers: a place where behavior
> can be changed without editing everywhere else); this document uses it in that
> looser sense — the joint between the two halves, not a dependency-injection
> point. Three things belong at it: the decision (does the type match?), the
> behavior (skip rather than misread), and the report (§6).

One primitive per corelib: *"is `(id, tag)` what you expect?"* — answered by the
generated code (declaratively), applied by the corelib. Where each model stands:

| Model | Corelib knows the expectation? | Work needed |
|---|---|---|
| E — c-cpp (C path) | yes, via descriptor | only the outcome changes (skip instead of `E_USAGE`) |
| E wrapper, D — cpp | yes, via the read call | fixlen subtype must be expressible (`readString`/`readBlob`, or `read<Fix::X>`) |
| B — ts, A2 — py, go pull | yes, via the reader call | evaluate the expectation instead of ignoring it |
| C — go visitor, dart | **no** — dispatches on wire type only | dart already has `shouldRead(id, type)` (generated code does not override it today); go needs the equivalent |
| A1 — rust, zig, java, cs | **no** | same seam |

Two properties fall out for free:

- **The report scope is exactly right.** Generated code only issues a read when its
  `switch` matched the id, so a mismatch *there* means "id known, type wrong" — the
  interesting case — while an unknown id never reaches the seam. No extra
  bookkeeping is needed to tell the two apart.
- **`askip`/`afill` disappear** — provided the seam also covers `arrayBegin`. If
  the generated code answers "not mine" there, the corelib discards the announced
  elements itself, and the streaming models stop needing a discard state machine.

---

## 6. Skip reporting

The seam is the one place where a skip is decided, so it is also the natural place
to report one. This replaces a strict/abort mode (§9): you learn about schema skew
**without** changing any verdict.

**Reported event:** *the field id is known to the schema, but the type tag
contradicts the declaration — the field was skipped, the default retained.*

Unknown ids are **not** reported (ordinary forward compatibility, pure noise), and
neither is anything that is already INVALID.

**Two levels**, because footprint and maxspeed have different budgets:

- **Level 1 — counter.** One `size_t` in the decoder context, one increment at the
  seam. Covers the 95 % case: *"were any fields discarded?"* — `assert == 0` in
  tests, a metric in production.
- **Level 2 — hook.** A callback carrying the record below, for triage.
  Compile-time gated.

**Record** — deliberately minimal, so it is the same in every language and can be
passed by value everywhere:

```
{ id, expected: TypeTag, actual: TypeTag }
```

- **No `depth`.** It was considered and rejected: it does not disambiguate. With
  two structs at root (ids 20 and 21) each holding a field id 1, a mismatch in
  either reports `id=1, depth=1`.
- **No `reason`.** There is exactly one reason today. A reserved enum with no
  consumer is the pattern PR #54 removed from the config schema; the field is
  additive if a second reason ever appears.
- **Scope identification is out of the contract.** Only the C descriptor path has
  it for free (`sofab_object_decoder_t::info`); elsewhere the scope is *generated
  code* knowledge (`cur` location enums in rust/zig/java/cs, the per-type decode
  frame in ts/py, and in cpp a `schema::SeqNode*` that only exists when the message
  carries a bound). A language may add it as an extension; the shared contract does
  not require it. An id path is not cheaply available either: `SOFAB_MAX_DEPTH` is
  255, so a fixed-size path array is 1 KB per event, and
  `sofab_istream_decoder_t` does not record the id its level was opened under.

**Three invariants:**

1. The channel **never** changes the decode verdict.
2. No allocation, no state beyond the message; the counter resets per decode.
3. Compiled off, it folds to nothing.

**Test value.** The §7.3 conformance vectors currently assert *"the field kept its
default"* — which is also green if the decoder never saw the field, or skipped it
for an unrelated reason. With the counter the assertion becomes precise: default
retained **and** `skipped == 1`. This closes the same class of gap that the
`invalid_utf8` vectors once had, where green CI did not mean working validation.

---

## 7. C (`corelib-c-cpp`)

The seam already exists — `object.c`, immediately before binding a target:

```c
if (field->type < ... && ((ctx->target_opt ^ _expected_wire_opt[field->type]) & 0x3F))
{
    _report_skip(ctx, id, field->type);   /* new */
    return;                               /* unchanged: bind nothing -> istream skips */
}
```

The hand-written path (`_call_field_callback_masked` in `istream.c`) changes from
returning `SOFAB_RET_E_USAGE` to the same skip-and-report.

```c
typedef struct {
    sofab_id_t id;
    uint8_t    actual;      /* wire | subtype<<3, low 6 bits (see §4) */
    uint8_t    expected;
} sofab_skip_info_t;

typedef void (*sofab_istream_skip_cb_t)(const sofab_skip_info_t *info, void *usr);

/* level 1 — available when SOFAB_SKIP_REPORT >= 1 */
size_t sofab_istream_skipped_count(const sofab_istream_t *ctx);

/* level 2 — SOFAB_SKIP_REPORT >= 2 */
void   sofab_istream_set_skip_cb(sofab_istream_t *ctx,
                                 sofab_istream_skip_cb_t cb, void *usr);
```

Gated exactly like the existing `SOFAB_STRICT_UTF8` knob: `SOFAB_SKIP_REPORT` = `0`
(field and increment compiled out), `1` (counter), `2` (counter + hook).

The C path may additionally hand the hook `sofab_object_decoder_t::info` — the
descriptor of the current scope. That is a C extension, not a contract term.

**Behavior change to flag:** `E_USAGE` → skip is visible to hand-written C callers
(today an abort, afterwards a silent default). It needs a CHANGELOG entry and must
not ship in a patch release.

---

## 8. C++ (`corelib-cpp`)

The check goes into `read<T>()`, at the comparison that replaces the 49 generated
guards. Everything it needs is already in place: `dispatchOne` sets `type_` /
`fixType_` **before** invoking the callback, and a field the callback does not read
is skipped automatically.

```cpp
template <typename T> bool read(T &value) noexcept {
    if (!wireMatches<T>()) { reportSkip<T>(); return false; }   // §7.3 -> do not consume
    ...
}
```

`std::string` maps to both `string` and `blob`, so the fixlen subtype has to be
expressible at the call site — `readString` / `readBlob`, or `read<Fix::String>`.

```cpp
struct SkipInfo {
    sofab::id id;
    TypeTag   expected, actual;
};

class IStreamImpl {
public:
    using SkipHandler = void (*)(const SkipInfo &, void *);   // no std::function: no allocation

    void   setSkipHandler(SkipHandler h, void *usr) noexcept;  // level 2
    size_t skipped() const noexcept;                           // level 1
};
```

The counter should also be reachable from `IStreamImpl::Result`, so callers that
only use `try_decode` and never see the stream can read it:

```cpp
MyMsg out;
auto r = MyMsg::try_decode(data, len, out);
if (r.ok() && r.skipped() > 0) {
    // valid message — but fields did not match this schema version
}
```

---

## 9. Rejected: a strict / abort mode

A configurable "reject the message instead of skipping" mode was considered and
**rejected** for now.

- It would make **non-conformance configurable**: the same bytes yield different
  verdicts depending on a switch. That is qualitatively different from the existing
  knobs, which change representation or resource policy, not the accept/reject
  decision on spec-valid data.
- The dominant cause of a type mismatch is not corruption but **schema skew** — the
  peer runs a different version. Rejecting it turns the case §7.3 exists for into
  an outage, and the failure looks like corruption at the far end.
- It only catches half of the skew anyway: a field *added* by the peer is an
  unknown id (tolerated), a field *retyped* is a mismatch (rejected). Same cause,
  different treatment.
- The actual need — noticing skew — is met better by §6 at no risk.

The `strict_utf8` precedent does **not** transfer: that switch decides whether an
*invalid* payload is rejected (spec-conformant when ON, a relaxation when OFF).
This one would reject *valid* input, i.e. deviate in the opposite direction. If it
is ever revisited it needs a MESSAGE_SPEC blessing, a distinct outcome
(`SCHEMA_MISMATCH`, following the receiver-side-policy precedent of the decode
limits, which report `LIMIT_EXCEEDED` rather than INVALID), and a tolerant default.

---

## 10. Measured: what the measure phase costs

The cpp PoC (generator#237 + corelib-cpp#53) landed the seam and then folded the
array reset behind it. Instructions per decode, `callgrind`, `-O2`, 5000
iterations, measured against the shapes below:

| step | Ir / decode | Δ |
|---|---|---|
| before the seam (guards in generated code) | 10 518 | — |
| seam only | 10 640 | +1.16 % |
| seam + `readArray`/`prepare` | 10 580 | **+0.59 %** |

Moving the comparison did not make it cheaper — it made it *singular*. Folding
the array reset halved the overhead by removing a double check (generated guard
*and* corelib). Generated wire comparisons went 49 → 0, and the benchmark's
`.text` fell **below** the pre-seam binary (62 722 → 62 306 B): the generated code
shrank more than the corelib grew.

### The descriptor is not just clutter — it is 10 % of decode

Isolating the measure phase (a build that skips it entirely — valid only because
the benchmark feeds one complete message per call, so it is an *upper bound* on
what removing it could win) and the schema consultation inside it (a build whose
`setSchema` is a no-op — not a conformant decoder, purely a cost probe):

| message shape | current | no measure phase | measure share |
|---|---|---|---|
| **30 fields, 91 bytes** (`example.yaml`) | 10 580 | 7 404 | **3 176 → 30.0 %** |
| 1 string, 4000 bytes | 5 824 | 5 683 | 141 → 2.4 % |

For the field-dense shape, split further:

| | Ir / decode | of total |
|---|---|---|
| measure phase, total | 3 176 | 30.0 % |
| — of which the descriptor consultation | **1 078** | **10.2 %** |
| — of which the bare completeness walk | 2 098 | 19.8 % |

The spread is structural: the measure walk costs per **field**, not per byte — it
skips a payload with `p += len`. Thirty small fields mean thirty header parses;
one 4 KB string means one. SofaBuffers' target domain (telemetry, control
messages) sits at the field-dense end, so 30 % is the number that matters.

So the descriptor table is not only duplicated schema knowledge in the generated
file — it is a tenth of the decode instructions.

## 11. Header-first delivery: removing the descriptor entirely

The descriptor exists for one reason: the measure phase must decide two things —
*is the field complete?* (wire knowledge, the corelib has it) and *is it within
the schema bound?* (schema knowledge, only generated code has it) — and it asks
nobody. So the schema knowledge has to be deposited in advance.

But **the typed read already carries all of it**. `readString(name, 8)` states the
wire type, the fixlen subtype and the bound. The problem is not missing
information, it is *ordering*: the call happens after the measure phase has
already ruled the field complete or not. Move delivery from the end of the field
to its **header**, and every column of the descriptor dissolves:

| descriptor column | where it comes from instead |
|---|---|
| `id` | the callback fires for that id |
| `wire` + `subtype` | *which* `read*` is called — `readString` means Fixlen+String |
| `bound` | the argument — `readString(name, 8)` |
| `child` | gone: delivery per level means the child type's own reads carry its bounds |
| `wrapperArray` | gone: the collectors already check the element index themselves |

§5.2 then holds structurally, with no rule written anywhere:

```
header read → callback → readString(name, 8)
                          ├─ tag mismatch          → skip (§7.3)
                          ├─ length > 8            → INVALID  ← decided before it is
                          │                                     known whether the bytes arrived
                          ├─ length ok, bytes there → assign
                          └─ length ok, bytes short → INCOMPLETE, buffer, deliver again later
```

The one requirement is that a read be callable at the header and able to answer
*"not enough bytes yet"*, so the decoder can buffer and re-deliver.

### It holds for every wire kind

| kind | known at the header | bound decidable? | length decidable? | retry idempotent? |
|---|---|---|---|---|
| Unsigned / Signed | id, wire | no bound exists | no (varint) | ✓ assignment follows a complete parse |
| Fixlen (string, **blob**, fp32/64) | id, wire, subtype, **length** | ✓ `maxlen` | ✓ exactly | ✓ `assign` only when the payload is present |
| ArrayUnsigned / ArraySigned | id, wire, **count** | ✓ `count` | **no** — elements are varints | ✓ `readArray` resets before filling |
| ArrayFixlen (fp32/64) | id, wire, count, subtype, element size | ✓ `count` | ✓ exactly `n × esize` | ✓ as above |
| SequenceStart (struct, union, wrapper array) | id, wire only | ✓ per element **index**, on arrival | **no** — only by scanning to SequenceEnd | ✓ `prepare()` clears, elements place by index |

Completeness being undecidable at the header for varint arrays and sequences does
not break the model: it is what the retry is for. A partial pass followed by a
full one is idempotent because every destination is either reset wholesale
(`readArray`, `prepare()`) or assigned by id. An `invalidate()` raised during a
partial pass must **survive** the retry — which is also semantically right: an
over-index element is INVALID whether or not the message was truncated.

### Two consequences worth naming

**#232 resolves itself.** For `ArrayFixlen` the element subtype is available at the
header *if* the read fires after the element-size word rather than after the count
word. The contradiction is then known before the count bound is applied, and
today's inconsistency — skipped when in-count, INVALID when over-count —
disappears without MESSAGE_SPEC having to rule on it. Varint arrays carry no
subtype, so the question does not arise there.

**Chunked input needs care.** The retry re-parses the field from its start and
**re-runs generated code**, not just a scan. Today every `feed` also re-measures
the buffered tail from `topPos_`, but callback-free. A 1000-element array fed byte
by byte would repeat its element assignments 1000 times: O(n²) with a worse
constant than today. The fix follows the measurements: **keep a measure phase only
for the streaming path**. When the whole message arrives in one `feed` (`acc_`
empty — what `try_decode` and every one-shot caller do), deliver header-first with
no measure at all; when it arrives in chunks, buffer to field completeness first,
then deliver.

The descriptor disappears in **both** cases, because the bound decision lives in
the read call, not in the measurement. What remains of the measure phase answers
only *"are the bytes here?"* — pure wire knowledge, no schema.

### Correction to §5 and to the reader-model reference

An earlier justification for the measure phase — *"it is the price of zero-copy
`std::string_view`"* — does not hold for **generated** code, which never asks for a
view: members are `std::string` / `std::vector<uint8_t>`, so `read` copies
(`value.assign(p_, fixLen_)`), exactly once for a one-shot feed and twice for a
chunked one (buffer → `acc_` → member). `grep -c string_view` on generated output
is 0. The zero-copy overload is a real corelib capability for hand-written
callers; it is not what the generated path does, and it cannot be while the
message type owns its storage. What the measure phase actually buys generated code
is that the payload is contiguous and complete, so `assign` runs once instead of
being assembled across chunks — convenience, not zero-copy.

---

## 12. What this resolves — and what it does not

**Resolves**

1. The **#174 / #183 / #189 / #224 / #229 bug class** — one place per corelib
   instead of ten, covered by the shared vectors.
2. The `corelib-go` split (`ErrUsage` vs `ErrInvalidMsg` for the same question).
3. The `corelib-c-cpp` double check — one comparison, one outcome.
4. The dead `Usage` taxonomy in eight corelibs gets a defined meaning or goes.
5. `askip` / `afill` in rust/zig/java/cs (given an `arrayBegin` seam).
6. Imprecise §7.3 vectors (§6, test value).

**Does not resolve**

- **#232** (a fixlen array whose element subtype contradicts is skipped when
  in-count but INVALID when over-count) — *not by the seam alone*. The count word
  precedes the element-size word, so at the moment the bound is decided the subtype
  is still unknown, and the seam only makes it one decision instead of five. §11's
  header-first delivery **does** resolve it, by firing the read after the
  element-size word; until then the MESSAGE_SPEC question stays open.
- **Bounds** (`count` / `maxlen`). A separate axis, already reconciled family-wide
  by #216 / #222 / #223, and unaffected here.

---

## 13. Sequencing

1. ~~**cpp seam, behavior-neutral.**~~ **Done** — generator#237 + corelib-cpp#53.
   `read()` compares the tag; 49 generated guards → 15.
2. ~~**Level 1 counter**~~ **Done** — on the stream and on `Result`, with the
   counting scope pinned by test (unknown id 0, contradicting tag 1).
3. ~~**`readArray` / `prepare`**~~ **Done** — the array reset folded behind the tag
   match; 15 guards → 0, overhead +1.16 % → +0.59 %.
4. **Header-first delivery in cpp (§11).** Removes the descriptor table *and* the
   second pass. Measured upside: up to 30 % of decode instructions on a field-dense
   message, 10 % of which is the table alone. Keep a measure phase for the chunked
   path only. This step also resolves #232 for fixlen arrays.
5. **c-cpp**: merge the two checks, `E_USAGE` → skip, counter. CHANGELOG + minor
   version.
6. **go / dart** (`shouldRead` equivalent), then **ts / py**.
7. **A1 (rust, zig, java, cs)** last — those need real corelib API changes in four
   repos, and they are the ones that also retire `askip`.
8. **Level 2 hook** wherever level 1 proves insufficient.

Steps 1–3 are behaviour-neutral and already verified against both C++ profiles.
Step 4 is the first that changes the decoder's shape rather than where a
comparison sits, so it wants its own measurement pass — the numbers in §10 are the
baseline it has to beat.

## 14. Open questions

- Default report level per profile — level 1 for maxspeed, `0` or `1` for
  c / c-cpp / rs-no-std. To be **measured** with `tests/bench` (`.text` / `.bss`),
  not guessed.
- Counter on the result, on the stream, or both.
- Config key name and shape (`skip_report: off | count | hook`?), plus the
  config-schema entry and the per-target doc tables — the schema↔code lockstep
  invariant applies.
- Whether the type tag becomes a shared *named* concept in ARCHITECTURE §9 (the
  corelib API contract) or stays a per-corelib type with a documented rule.
- Whether header-first delivery (§11) should also replace the measure phase in the
  **chunked** path via resumable delivery (the corelib-c-cpp model: bind a
  destination, fill it as bytes arrive) instead of buffering to completeness. That
  removes the second pass everywhere, at the cost of a state machine per field.
- Whether an opt-in **non-owning message profile** (`std::string_view` members) is
  worth having, now that it is clear the generated path copies every
  variable-length payload exactly once. That is an object-model question,
  independent of the decoder shape.
