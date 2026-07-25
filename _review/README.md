# TEMPORARY — delete before merging

Generated output committed **only so the diff is reviewable**. It is not part of
the build, nothing reads it, and it must be removed before this branch merges.

`examples/messages/example.yaml`, both C++ profiles:

| path | config | §7.3 guards in `deserialize` |
|---|---|---|
| `generated/cpp/` | `targets.cpp: { namespace: sofabuffers }` | **15** |
| `generated/c-cpp/` | `+ corelib: c-cpp, allow_dynamic: true` | **49** (unchanged) |

## What to look at

- **`generated/cpp/myfirstmessage.hpp`** — the point of the change. Scalar,
  fp32/fp64, string, blob and struct/union arms carry no wire-type comparison any
  more; the corelib decides inside the typed read (corelib-cpp#53). Compare
  `case 9`…`case 14` and `case 20`…`case 22` against the c-cpp file.
- **`case 11` / `case 12`** — the fixlen kinds now name their subtype through the
  call (`readString` / `readBlob`), and the `maxlen` reject hangs off a
  *successful* read:

  ```cpp
  if (is.readString(somestring) && _size > 50) { is.invalidate(); return; }
  ```

  Checking `_size` first would measure a contradicting fixlen value against a
  bound that does not apply to it — generator#224/#229 on the deliver path.
- **`case 15`…`case 19`** — the arms that keep their guard. Each resets its
  destination (`someuintarray = {}`, `somestringarray.clear()`) *before* reading,
  and that reset must not run for a field §7.3 skips (§7.4: an occurrence skipped
  under §7.3 is not an occurrence). Folding reset + bound + tag into one
  `readArray(dst, bound)` call is the follow-up step; it also closes the
  under-counting of array mismatches in `Result::skipped()`.
- **`generated/c-cpp/myfirstmessage.hpp`** — should be byte-identical to what
  `main` produces. Its corelib reports a bound-type mismatch as a *usage error*
  rather than skipping, so it keeps every guard until its own step.
