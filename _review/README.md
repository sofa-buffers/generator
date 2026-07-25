# TEMPORARY — delete before merging

Generated output committed **only so the diff is reviewable**. It is not part of
the build, nothing reads it, and it must be removed before this branch merges.

`example.yaml` here is a deliberately small schema — one field per decode shape
that behaves differently, nothing else — so the generated header is ~200 lines
instead of the ~650 the repo's `examples/messages/example.yaml` produces.

| path | config | §7.3 wire comparisons in `deserialize` |
|---|---|---|
| `generated/cpp/` | `targets.cpp: { namespace: sofabuffers }` | **0** |
| `generated/c-cpp/` | `+ corelib: c-cpp, allow_dynamic: true` | 12 (unchanged from `main`) |

## The point of the change, in one screenful

`generated/cpp/probe.hpp`, the message's own `deserialize`:

```cpp
case 0: is.read(count);                                                  break;  // u32
case 1: is.read(delta);                                                  break;  // i32
case 2: is.read(ratio);                                                  break;  // fp64
case 3: if (is.readString(name) && _size > 8) { is.invalidate(); return; } break;
case 4: if (is.readBlob(data)  && _size > 8) { is.invalidate(); return; } break;
case 5: is.readArray(fixed, 3);                                          break;  // count: 3
case 6: is.readArray(free);                                              break;  // unbounded
case 7: { _StrSeq _r0{tags, 2, 4}; is.read(_r0); }                       break;  // wrapper array
case 8: is.read(inner);                                                  break;  // nested struct
```

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
- `read(collector)` / `read(struct)` — `SequenceStart`. Wrapper-array collectors
  declare `prepare()`, which the corelib calls once the tag matched; that is where
  the replace-whole `clear()` went.

## For comparison

`generated/c-cpp/probe.hpp` still carries all 12 comparisons and is byte-identical
to what `main` produces — its C layer reports a bound-type mismatch as a *usage
error* rather than skipping, so it keeps them until its own step in the sequencing
(see `docs/models/type-reconciliation.md` §11).
