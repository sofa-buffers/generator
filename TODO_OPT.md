# TODO_OPT — SofaBuffers performance backlog

Evidence base: full `tests/bench/run.sh` (2026-07-20, all cells green) plus
`callgrind_annotate` function-level attribution on every native row. Numbers are
Ir/op (instructions retired per encode/decode of the `vehicle_telemetry` message).
Lower is better. Baselines that appear below are the committed `results.txt` values.

## Headline

Across every language the hot functions are **corelib wire primitives** (varint,
fixlen, istream-feed, dispatch); the generated per-field code is a thin shim
(1–18 % of Ir, mostly the inlined field loop). This matches ARCHITECTURE §1
("corelib owns the wire format"). **⇒ Ir/op optimization potential is ~90 % in the
corelibs, not the generator.** The generator only matters for footprint `.text`
and for feeding decode-time array lengths through to reduce allocation.

Reference points: **Zig** is the decode leader (18922) thanks to a bump/arena
allocator; **Rust** is the encode leader (10021).

## Priority backlog

### P1 — corelib-go varint (biggest, most concentrated win) — DECODE DONE
- Decode: `(*cursor).uvarint` fast path landed (local, unpushed). Single-byte
  varints (field headers id<16, small scalars/counts) now take a lean load-and-
  return; the multi-byte loop is out of line in `uvarintSlow` (`//go:noinline`).
  **Measured: decode 38603 → 37044 Ir/op (−4.0 %), encode unchanged, tests+vet
  green.** Profile confirms: varint work 12369 → 10810 Ir (uvarint 5471 +
  uvarintSlow 5339). It stays ~24 over the inline budget (two-value return + call),
  so the win is the leaner path, not inlining.
- Encode: `(*Encoder).putVarint` = 35.5 % (8771 Ir) but **already well tuned**
  (commit #51). A 1-byte-fast-path attempt **regressed encode +4.9 %** (guard
  overhead with no inlining, and this payload has many multi-byte varints) — reverted.
  Leave encode alone unless it can be made genuinely inlinable.
- [x] ~~**Typed Visitor API (`sofab.TypedArrayVisitor` + `ArrayReader`)**~~ — **BUILT,
  MEASURED, REVERTED. Negative result: fewer allocations, far MORE instructions.**
  Optional visitor extension letting generated code decode arrays straight into its
  typed slice (no widened `[]uint64`, no narrowing pass); matrix collectors included.
  Correct (round-trip byte-exact, full Go conformance green) and it did cut
  allocations **46.0 → 39.5 /op** — better than the `unsafe` hack's 41.0. But decode
  Ir **38603 → 44169** (+14 %), and hoisting the per-array type assertion out of the
  field loop made it **66595** (+72 %). Reproducible; not an allocation effect
  (allocs unchanged) and not the cursor escaping (fixed that — reader holds its
  cursor by value behind a pointer field — and the regression stayed).
  **Why it loses:** the element loop moves from a tight loop over a stack-resident
  cursor filling a plain slice into a generic call (`ReadArrayUnsigned[T]`, go.shape
  dictionary) against a heap-resident reader, plus per-array interface dispatch.
  Go's small-object allocator is *cheap* (`mallocgcTiny`/`SmallNoscan` fast paths) —
  trading ~6 cheap allocations for a slower per-element loop is a bad deal.
  **Lesson: allocs/op is not the objective function here; measure Ir/op.**
- [x] ~~**Allocation, round 1 — in-place integer-array narrow (generator, go backend).**~~
  **REVERTED** — see the `unsafe` assessment below; kept out of the tree.
  The corelib hands the visitor a freshly-allocated widened `[]uint64`/`[]int64`
  and never retains it; the generated `_narrowU`/`_narrowS` used to `make([]T)` a
  second slice and copy. Now they narrow **in place** into that backing array
  (`unsafe.Slice`, front-to-back so writes trail reads — safe for any T ≤ 8 bytes).
  **Removes 1 alloc per integer array.** Measured: **46 → 41 allocs/op**; go decode
  **37044 → 35896 Ir/op**. Combined with the uvarint fast path: **decode 38603 →
  35896 (−7.0 %)**, encode unchanged. Conformance green (shared vectors + 16-corpus
  + realworld), round-trip byte-exact. NOTE: introduces `unsafe` into generated Go
  (project had none) and over-retains backing memory — deliberate maxspeed trade.
- **Conclusion on the array-allocation lever: it is closed for now.** Three designs
  were built and measured — safe (baseline), `unsafe` in-place narrow, and the typed
  Visitor API. Only the `unsafe` one improved Ir (−3.1 %), and its benefit sits below
  this host's wall-clock noise; the "clean" typed redesign is a large regression.
  The widened `[]uint64` stays. Don't retry without a design that keeps the element
  loop monomorphic and stack-local.
- [ ] Remaining alloc buckets (mostly necessary): string/blob copies (`string(b)`,
  blob `Bytes` — buf not retained, so copies are semantically required), nested
  `BeginSequence` struct allocs.
- Baseline: enc 24698 / dec 38603 (worst native maxspeed: 2.5× rust enc, 2.0× zig dec).
  Now: enc 24698 / **dec 35896**.
- **Arena confirmation vs protobuf** (`LANGS=go RUNS=3`, wire gate 434B/494B ok):
  msg/s advantage **1.01× → 1.08× / 1.11×** (two runs); sofab 210800 → 222306 /
  217608 msgs/s. Read **msgs/s**, not MB/s — MB/s penalizes sofab's smaller wire.
  Host is noisy (protobuf control drifted ~6% between runs), so the noise-free
  Ir/op (−7.0 % decode) is the corroborating metric.

### Java (cloud-critical: beating protobuf is a hard requirement) — near a local optimum
Profiled with async-profiler (CPU + alloc; `perf_events` is blocked on this VPS, use
`event=itimer`). No dominant hotspot — cost is spread: IStream ~21 %, visitor
callbacks ~17 %, OStream ~16 %. The code already has fast paths everywhere
(IStream fast* paths, ASCII fast paths in both UTF-8 directions, ThreadLocal encode
buffer).

- [x] **Reuse the stream objects** (`IStream.reset()` + `OStream.reset(byte[])` added
  to corelib-java; generated `encode`/`decode`/`tryDecode` hold them per-thread and
  reset on *entry* so a throwing call cannot poison the next). Ir: encode
  16328→16312 (−0.10 %), decode 36899→36881 (−0.05 %) — **marginal**. The real
  argument is GC pressure, which Ir does not capture: `IStream` and `OStream`
  disappear from the allocation profile entirely (**−5.8 % of allocated bytes/op**),
  which is what matters for a long-running service. Conformance green.
- [x] ~~**Word-at-a-time ASCII scan in `_utf8ok`**~~ (protobuf-java's `Utf8` trick, via
  a byte[] `VarHandle` long view instead of Unsafe) — **REVERTED, regression.**
  decode 36881 → 37040 (+158 Ir). This schema's strings are short (maxlen 24), so
  the `i+8<=end` guard costs more than the word step saves; protobuf's version pays
  off only on long strings. Correctness was *not* the problem — a differential test
  over 17,243,008 inputs (exhaustive 1–3 byte sequences + 400k random buffers vs the
  old validator AND the JDK strict decoder) found 0 mismatches. Keep that test idea
  if this is ever retried with a length threshold.
- **Arena is not usable for attributing changes this small on this VPS**: between two
  runs sofab moved +18.5 % and protobuf +11.2 %, i.e. host drift swamps a 0.05 %
  effect. Ratio went 1.06× → 1.13× msgs/s but that is NOT attributable. Use Ir/op.
- [ ] **Remaining structural levers — both need an API change, neither is free:**
  - **`long[]` storage for narrow int arrays (38.6 % of allocated bytes).** Generated
    Java stores u8/u16/u32/i8/i16/i32 arrays all as `long[]` (8 B/elem). Narrowing to
    `byte[]/short[]/int[]` would cut allocation and zeroing sharply — but it is a
    **breaking change to generated public field types**. Needs a decision, not a patch.
  - **Per-element visitor callbacks for arrays** (`unsigned`+`signed` = 8.6 %, plus a
    double `switch` per element). A bulk array callback would remove the per-element
    dispatch. ⚠ The Go analogue of exactly this idea regressed badly (see above) —
    prototype and measure before committing.

### P2 — corelib-cs (worst maxspeed target overall)
- enc 42528 (4.2× rust) / dec 72871 (3.9× zig); **2.6× enc / 2.0× dec vs Java**,
  same managed/subtract method. Generated C# is verified thin (direct
  `os.WriteXxx(id,val)`), so the gap is in **corelib-cs** OStream/IStream.
- No native-style attribution (JIT). Action: profile with `dotnet-trace`/EventPipe.
  Suspects: per-field allocation, missing `[MethodImpl(AggressiveInlining)]`,
  bounds checks in the Write*/Read* primitives.

### P3 — corelib-cpp decode (maxspeed anti-patterns)
- dec 34373 (1.8× zig): `dispatchLevel(std::function<…>)` **~14 %** +
  `_Function_handler::_M_invoke` (type erasure) **and ~10 % `malloc`/`free`** in the
  hot loop.
- Ideas: replace `std::function` dispatch with a templated/inlined visitor;
  arena/no-alloc decode (the Zig model).

### P4 — corelib-rs decode allocation
- dec 25235 (1.33× zig): `_int_malloc` ~11 % + `Vec` `finish_grow` ~2 % + `malloc`.
  Zig wins decode purely on its arena allocator (its alloc is only 6.5 %).
- Ideas: arena/bump allocator or pre-size `Vec`s from decoded array counts.

## Cross-cutting

- **Strict-UTF-8 is the family-wide decode regression** seen vs committed (every
  decode rose: c +8.1 %, ts +6.2 %, rest 1–3 %; encode unchanged). Shows up as
  `utf8.Valid`/`from_utf8`/`utf8_valid` (3–4 %) newly in the decode path. Correct
  behavior — optionally optimize with a SIMD validator, else accept as the
  correctness cost.
- **Allocation is the recurring decode tax.** Arena/bump (Zig) beats per-field
  malloc (Rust, Go, C++). Generic lever for Rust/Go/C++ decode.

## Explicitly low priority

- **c decode** `sofab_istream_feed` = 63 % (27171 Ir), highest native decode — but
  `c` is a *footprint* target at 1636 B `.text`; the high Ir is the deliberate
  size/speed trade. Don't spend Ir budget here at the cost of `.text`.
- **Python / TS**: 740k–2.1M Ir/op is inherent interpreter bytecode-dispatch, not a
  corelib/codegen lever.

## Ground rule

Corelib perf work stays **local** (no push/PR) unless explicitly requested; the
bench is the arbiter — A/B every change with `run.sh --rows <x>` against a fixed
corelib checkout via `SOFAB_<LANG>_CORELIB`.
