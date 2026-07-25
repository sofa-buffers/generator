# TEMPORARY — delete before merging

Generated output committed **only so the diff is reviewable**. It is not part of
the build, nothing reads it, and it must be removed before this branch merges.

`example.yaml` here is a deliberately small schema — one field per decode shape
that behaves differently, nothing else — so the generated header is ~200 lines
instead of the ~650 the repo's `examples/messages/example.yaml` produces.

| path | config | §7.3 wire comparisons | lines |
|---|---|---|---|
| `generated/cpp/probe.hpp` | `targets.cpp: { namespace: sofabuffers }` | **0** | **118** (was 208) |
| `generated/c-cpp-dynamic/probe.hpp` | `+ corelib: c-cpp, allow_dynamic: true` | 17 | **137** (was 209) |

Both example schemas also carry the full set of field metadata (`description`,
`unit`, `decimals`, `deprecated`, and the message `summary`) so the doc comments
and deprecation attributes are reviewed too. That is additive and lands on both
sides of the comparison equally — it adds 6 lines to each output (124 / 143) and
leaves every `deserialize` arm byte-identical. The `lines` column above is the
count without it, so the before/after delta stays comparable.

The message header now contains the message and nothing else. Three things left
it, in this order:

- the §7.3 wire comparisons, into the corelib's typed reads;
- the wrapper-sequence collectors and the encode trim, into `sofab::`
  (`StringSeq`, `BlobSeq`, `MessageSeq<T>`, `trimTail`);
- the measure-phase bound descriptors — **gone entirely**, not relocated. Header-
  first delivery reaches every §5.2 verdict through the reads themselves, so
  there is nothing left to deposit in advance.

## The point of the change, in one screenful

`generated/cpp/probe.hpp`, the message's own `deserialize`:

```cpp
struct Probe : sofab::Message {                          // the OStream+IStream pair, aliased
    ...
    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t _size, std::size_t) noexcept override {
        switch (id) {
        case 0: is.read(count);                                                    break;  // u32
        case 1: is.read(delta);                                                    break;  // i32
        case 2: is.read(ratio);                                                    break;  // fp64
        case 3: if (is.readString(name) && _size > 8) { is.invalidate(); return; } break;
        case 4: if (is.readBlob(data)  && _size > 8) { is.invalidate(); return; }  break;
        case 5: is.readArray(fixed, 3);                                            break;  // count: 3
        case 6: is.readArray(free);                                                break;  // unbounded
        case 7: { sofab::StringSeq _r0{tags, 2, 4}; is.read(_r0); }                break;  // wrapper
        case 8: is.read(inner);                                                    break;  // nested
        default: break;
        }
    }
```

(reflowed to one line per arm; the generated file puts the body on its own line)

Every arm is *id → typed read*. The read states what the schema declares; the
corelib compares the delivered field's tag against it and skips a contradicting
one (MESSAGE_SPEC §7.3). No wire type appears in generated code at all.

What each call carries:

- `readString` / `readBlob` — the fixlen **subtype**, which the wire type alone
  cannot settle (`fp32`/`fp64`/`string`/`blob` all share `Wire::Fixlen`). The
  `maxlen` reject hangs off a *successful* read, so a contradicting value is never
  measured against a bound that does not apply to it (#224/#229 on the deliver
  path).
- `readArray(dst, count)` — the array kind, the schema `count` (→ `INVALID`), a
  configured `max_dyn_array_count` (→ `LimitExceeded`) and the destination reset,
  applied in that order. The reset lands *behind* the tag match, so an occurrence
  skipped under §7.3 cannot wipe a valid earlier one (§7.4).
- `read(collector)` / `read(struct)` — `SequenceStart`. The collectors are corelib
  types now (`sofab::StringSeq` and friends); their `prepare()` — which the corelib
  calls once the tag matched — is where the replace-whole `clear()` went.

## For comparison

`generated/c-cpp/probe.hpp` still carries its §7.3 comparisons: its C layer reports
a bound-type mismatch as a *usage error* rather than skipping, so the guards stay
until that changes — its own step. What it no longer carries is the prelude: the
collectors moved into corelib-c-cpp as `sofab::FixedStringSeq` / `FixedBlobSeq` /
`FixedMessageSeq` / `MessageSeq` / `trimTail`, named to match corelib-cpp so both
C++ outputs read alike. 209 → 137 lines.
