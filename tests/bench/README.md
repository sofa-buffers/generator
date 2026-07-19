# tests/bench — footprint & instruction cost of generated code

Detects performance and code-size changes **caused by generator changes**.

```sh
tests/bench/run.sh                    # regenerate results.txt (in the devcontainer)
git diff tests/bench/results.txt      # <- the point
```

**Run it in the devcontainer, by hand** — same as the benchmark arena: somebody runs
it and reads the diff. CI never writes this file; see
[One measuring device](#one-measuring-device).

`results.txt` is committed. Change the generator, re-run, and the cost or saving
shows up in the PR diff next to the code that caused it. It is a **diff tool**, not
a leaderboard — see [Reading a diff](#reading-a-diff).

This is tier 3 of the test suite (`tests/README.md`): tier 1 is the hermetic matrix,
tier 2 per-language conformance, tier 3 this.

## What is measured

The **whole package**: generated code *plus* the corelib it calls, as it ships.
Not the generator's own runtime, and not the corelib in isolation (each corelib
benches itself — see below).

| metric | rows | how |
| - | - | - |
| Ir/op | all 12 | one op under Callgrind, host x86-64, `-O3` |
| `.text`/`.data`/`.bss` | the 3 `footprint` rows | cross-compiled to the embedded targets those profiles ship to, `-Os` |

## Why instruction counts, not wall-clock

Ir (instructions retired, via Callgrind) is deterministic and independent of CPU
clock and OS scheduling, so it is stable enough to **commit to a file**. Wall-clock
is not: a committed file of MB/s numbers would change on every run and on every
machine, and nobody would trust or regenerate it.

That is also why determinism is treated as a hard requirement here rather than a
nice-to-have. Anything that makes `results.txt` wobble when nothing changed makes
the whole thing worthless. Guarded by `run.sh` being idempotent (run it twice, the
file must be byte-identical) and by `-ffile-prefix-map`/`--remap-path-prefix` (see
[Traps](#traps)).

The toggle rows are *exactly* reproducible. The subtract rows are not: a JIT's
instruction count is not bit-reproducible, and `corelib-java/bench/run_callgrind.sh`
documents ~0.03% surviving jitter. So `lib/format.py` applies **hysteresis** — a
cell keeps its committed value while a new reading is within 0.3% of it, and moves
only on a change big enough to be real (`stabilize()`).

Rounding was tried first and does not work. Every deterministic rounding has bucket
edges, and a raw value sitting on one flips regardless of how coarse the buckets
are. The idempotence check caught exactly that, on two of three subtract rows in one
run:

```
csharp decode  71100 <-> 71200      (raw ~71,150 — sat on a 3-s.f. edge)
java   encode  16500 <-> 16600      (raw ~16,550 — likewise)
```

The 0.3% band sits an order of magnitude above the measured jitter (~0.03%) and an
order of magnitude below the smallest change worth seeing (1%; the wins in
`docs/perf-patches/` are tens of percent). If a row ever *does* flip, raise **that
row's reps** — which tightens its raw jitter — rather than widening the band.

## Reading a diff

The header records the corelib SHAs and the `## toolchain` table records every
compiler that built a row. Read both first:

| header | numbers | conclusion |
| - | - | - |
| unchanged | moved | **your generator change did it** — the signal |
| moved | moved | the corelib or a toolchain did it |
| moved | unchanged | corelib moved, no impact |

The `schema:` line carries a sha256 for the same reason: if you edit
`vehicle_telemetry.yaml`, every number legitimately moves, and the hash says so.

**Corelibs are deliberately NOT pinned.** They are cloned from their default branch,
exactly as `tests/conformance/*/run.sh` does. A corelib must match the generated
code built against it — pinning would break this bench on precisely the commits
that adopt a new corelib API, which are the ones most worth measuring. Provenance
in the header replaces pinning.

Consequence, stated plainly: **absolute numbers are not comparable across days**,
because the corelib moves underneath. Compare within a run, or across a run where
the header didn't move.

Override a clone to test a local corelib:

```sh
SOFAB_C_CORELIB=~/src/corelib-c-cpp tests/bench/run.sh --rows c
```

## Schema

`examples/messages/realworld/vehicle_telemetry.yaml`. Chosen because every array is
bounded (`count`) and every string/blob has a `maxlen`, so it generates for **every**
row — including C and the no_std/c-cpp footprint profiles — with no `allow_dynamic`
and no per-config mutation. Every row therefore measures the identical schema.

`examples/messages/example.yaml` cannot be used: its intentionally-unbounded
`somemap` forces `allow_dynamic: true` on the footprint rows and an injected
`count: 8` for C (see `tests/gen-artifacts.sh`), so the rows would no longer be
measuring the same thing.

## Not comparable to the corelibs' own benches

Every corelib ships `bench/run_callgrind.sh` reporting Ir/op for four fixed
workloads (`encode_u64_array`, `encode_typical`, …) that call the corelib API
directly. This harness reuses their **method**, not their numbers: our workload is
the generated code for `vehicle_telemetry`, which is a different thing. Do not put
the two tables side by side.

Likewise `corelib-c-cpp/tools/footprint.sh` measures the corelib library; this
measures generated code. Complementary, not comparable.

## Not the same as conformance

`tests/conformance/` and this must never be merged. They want opposite things:

| | conformance | bench |
| - | - | - |
| build | debug/unoptimized | `-Os` (footprint), `-O3` (Ir/op) |
| corelib | default branch | default branch (same) |
| failure | red | a number in a diff |

Optimization level is not a detail here: ARCHITECTURE §8 makes bounds checks
debug-only assertions, so a debug build measures code that does not ship.

## Per-row recipes

| row | corelib | arches | shape |
| - | - | - | - |
| `c` | corelib-c-cpp | ARMv6-M, ARMv7-M+fp.dp, RV32IMC | `emit: sources` → compile `.c` → `size` the object |
| `cpp-c-cpp` | corelib-c-cpp | ARMv6-M, ARMv7-M+fp.dp | header-only, so a driver TU instantiates `encodeTo`/`try_decode` |
| `rust-rs-no-std` | corelib-rs-no-std | thumbv6m | staticlib + `rust-lld --gc-sections`, then size the linked ELF |

Each lives in `lang/<lang>.sh` and implements `bench_size`. The differences are not
arbitrary — see the header comment in each file. In particular:

* **C** can be sized as an object because everything in it is reachable generated
  code. **Rust cannot.** Quoting `corelib-rs-no-std/tools/footprint.sh`: *"A bare
  staticlib archive is NOT dead-stripped, so measuring it directly massively
  over-counts; the link step is what makes the code numbers meaningful."* Measured
  here: the rlib reports ~14 KB against ~8.2 KB linked — a 42% over-count.
* **C++** emits a header-only `.hpp`; an empty TU including it sizes to 0, because
  nothing instantiates until something calls the API. Hence the driver TU. It calls
  only `encodeTo`/`try_decode` — the convenience `encode()` returns `std::vector`
  and would drag the allocator into every number.

## Known gaps

These are properties of the generator/targets, not of this harness. The bench
exists partly to keep them visible.

* **atmega8 is impossible for any fp64 schema.** avr-gcc auto-defines
  `SOFAB_DISABLE_FP64_SUPPORT` (AVR's `double` is 32-bit), and the generated header
  correctly hard-errors. `vehicle_telemetry` uses fp64, so the AVR row from
  `corelib-c-cpp/tools/footprint.sh` has no counterpart here.
* **The C++ footprint profile cannot build freestanding.** The generated `.hpp`
  includes `<string>`/`<vector>`, which libstdc++ rejects under `-ffreestanding`
  ("This header is not available in freestanding mode"). So the `cpp-*` rows drop
  `-ffreestanding` (`cxx_flags` in `rows.json`) and are a **bloat tracker, not a
  flash budget**. `docs/plans/cpp-embedded-footprint.md` is the plan to fix this
  (`FixedString<N>`/`FixedBytes` from schema `maxlen`); when it lands, drop
  `cxx_flags` and expect a step down. The `bss=180` on those rows is static RAM in a
  profile that advertises none.
* **C++ is ARM-only.** Neither riscv64-unknown-elf nor avr ships a bare-metal C++
  standard library.
* **The `ts-*` rows measure the V8 BASELINE JIT, not fully-optimized TurboFan** — a
  deliberate choice, not a gap, but worth knowing. Default V8 tiering makes Ir
  non-monotonic in reps (more iterations → harder optimization → less total work),
  which the affineness gate rejects. Capping at the baseline tier
  (`--max-opt=1 --predictable --single-threaded`, see `lang/typescript.sh`) removes
  the tier transition and makes the subtraction affine and reproducible. The number
  is a stable relative reference — right for "did my change help?" — but lower-tier
  than production. This is the one row where the measured tier differs from what
  ships; every other row measures the shipping configuration.


## Prereqs

```sh
sudo apt-get install -y gcc-arm-none-eabi libstdc++-arm-none-eabi-newlib \
                        gcc-riscv64-unknown-elf picolibc-riscv64-unknown-elf \
                        valgrind
rustup target add thumbv6m-none-eabi
rustup component add llvm-tools-preview      # llvm-size, rust-lld
```

`libstdc++-arm-none-eabi-newlib` is easy to miss and its absence looks like "C++
cannot cross-compile" rather than a missing package.

The devcontainer already carries all of it. Watch `PATH`: with `/root/.cargo/bin`
missing, `cargo` resolves to apt's instead of rustup's, and the two rustc versions
move the Rust rows ~8%. The `thumbv6m` footprint fails loudly in that case; the two
Ir rows do **not** — they just come out wrong. The `## toolchain` table catches it
after the fact: the recorded `rustc` version will not be the one you expected.

## The toolchain table

`results.txt` carries a `## toolchain` section — every compiler that built a row,
its version, and which rows it built:

```
tool                      version       rows
gcc                       15.2.0        c
go                        1.24.4        go
rustc                     1.97.1        rust-rs,rust-rs-no-std
valgrind                  3.26.0        all
```

Ir/op is the instruction count of a *particular binary*, so each of these moves
numbers on an unchanged generator and an unchanged corelib. Recording only the host
`gcc` and `rustc` — as this file used to — left five languages able to shift a row
with nothing to show for it, which is precisely how a Go 1.24 → 1.26 difference once
read as a doubled `go` encode row.

The row mapping comes from `rows.json`, so it cannot drift from the rows actually in
the file, and `all` is shorthand for "built every row". A tool that is missing is
recorded as `(not found)` rather than dropped: its absence is itself a reason a
number could move.

## One measuring device

Ir/op is the instruction count of a *particular binary*, so it depends on the
compiler that produced it. A CI runner pins its own toolchain versions, which makes
it a second measuring device — and two devices disagree about code that did not
change. Measured: the bench workflow on `ubuntu-24.04` with Go 1.26 read the `go`
encode row at **56,237** Ir/op; the devcontainer with Go 1.24 read the same commit
at **24,698**. Neither is wrong. They are different scales.

So one environment owns `results.txt`, and it is the devcontainer. If a diff ever
needs settling, re-run both sides *here* rather than comparing against a number
produced somewhere else.

### The workflow is still there, for asking a second opinion

`.github/workflows/bench.yml` runs on **`workflow_dispatch` only**. Use it when a
local number looks implausible, or to see a row on a toolchain you do not have
locally. It never writes `results.txt`.

It is not a PR trigger on purpose: since it measures on another scale, it would post
a large diff on every PR for reasons no PR caused, and a signal that always fires is
one reviewers learn to skip — worse than no signal, because it costs CI minutes and
looks like coverage.

Its report (`lib/report.py`) is built around that hazard:

* **Toolchain comparison first.** Whatever differs from the header of the committed
  file is named before any number, because that alone moves rows.
* **Failed measurements are their own section**, and the only thing that fails the
  job. A `!` cell means a broken run, not a slow row — and it would overwrite a
  committed value if it reached the file.
* **Outliers (≥5%) are separated from ordinary movement (>0.3%)**, so a doubled row
  cannot hide in a list of wobbles.

It reads the toolchain comparison out of the `## toolchain` table on both sides, and
filters it to the tools that built the row being judged — a `go` artifact reporting
that the Zig compiler drifted is true and useless.

## The two Ir/op methods

Which one a row uses is `method` in `rows.json`, decided by one thing: whether a
native symbol exists to toggle collection on. Both mirror the corelibs.

**`toggle`** (c, cpp, rust, go, zig) — `--collect-atstart=no
--toggle-collect=run_<workload>` around a single op. The `run_<w>` wrapper is
`noinline` with external linkage. The barrier is on the **wrapper only**: encode /
try_decode and the corelib still inline into it, and that inlining is exactly what
maxspeed rows exist to measure.

Go's symbols are package-mangled, so its toggle target is `main.run_<w>`
(`bench_ir_sym` in `lang/go.sh`); Zig's `export fn` is unmangled and uses the plain
name. Go also needs its runtime tamed — `GOMAXPROCS=1 GODEBUG=asyncpreemptoff=1
GOGC=off` — or the "single op" is not a single op.

**`subtract`** (java, python, ts, csharp) — no native symbol exists (the hot code is
JIT'd or interpreted), so run at two rep counts and subtract:

```
Ir/op = ( Ir(R2) - Ir(R1) ) / ( R2 - R1 )
```

which cancels all fixed cost exactly — startup, class loading, JIT compilation,
setup. The scale of what it removes is easy to underestimate: for Python the
intercept is **146M Ir**, against ~841k Ir/op. Measured without subtracting, the
number would be almost entirely interpreter startup.

Three things have to hold, and each one broke in practice before it worked:

1. **A fixed warmup** on the JIT rows (java, csharp, ts), independent of `reps`, in
   the generated harness — so the hot methods reach their final tier *before* the
   measured loop and every measured op runs at steady cost. Being identical in both
   runs, it cancels. (`corelib-java/.../Callgrind.java` does the same, and says why.)
   Python needs none: CPython has no tiers, and it measures affine to 0.0004%.
2. **Runtime pinning** (`lang/<x>.sh`), adopted from each corelib's script: one JIT
   tier, no GC, no per-run-seeded hashing.
3. **Rep counts at the corelib's scale** — `10000 110000` for java/csharp, per
   `REPS_CHEAP` in every corelib `bench/run_callgrind.sh` (python and ts, being
   cheaper per op, use a smaller delta — the counts are per-row in `rows.json`).
   **This matters more than it looks.** The delta carries the signal and fixed noise
   is divided by it, so at a delta of 1000 ops Java decode came out 2–5% off affine
   and *irreproducible between runs*; at the corelib's delta of 100000 the same
   measurement is 0.03%. If a row fails the affineness gate, raise the reps before
   touching anything else.
4. **A single process, no forking transform** (ts only). tsx — the usual way to run
   `.ts` — forks a child node (callgrind traces only the parent) *and* transforms
   the TS per-process with an asymmetric on-disk cache: the first of the three rep
   points pays a cache-write the other two don't, and it does not cancel. The
   symptom was maddening — affine in an isolated one-shot, rejected under run.sh's
   three-point path, reproducibly. Fix: precompile TS→JS with `tsc` once and run
   plain `node dist/harness.js`. A forking wrapper has no place in an
   instruction-exact measurement; see `lang/typescript.sh`.

## Traps

Every one of these silently produces a number that *looks* like signal. Most were
found the hard way while building this.

* **A `--toggle-collect` that matches no symbol is not an error.** Callgrind
  collects nothing and reports `summary: 0` — silently, which reads as an infinite
  speedup. `ir_toggle` refuses to return 0 for this reason.
* **`export fn` does not imply "not inlined".** Zig at `--release=fast` inlined the
  body into the caller and left the exported symbol as an unreferenced copy, so the
  toggle matched a function that was never entered → `Ir = 0`. Fixed by
  `@call(.never_inline, run_<w>, .{})` at the **call site**. The same class of bug
  would hit any backend where the wrapper's barrier is assumed rather than enforced.
* **Two rep points cannot tell a real measurement from a JIT step.** With no warmup,
  V8 tiered up inside the measured loop and the two slopes came out
  `214,702` and `2,019,691` — 9.4× apart. Two points return whichever slope the
  transition landed on, and it looks entirely plausible. Hence `ir_subtract`
  measures **three** points and refuses to report unless the slopes agree to 1%.
  Do not widen that tolerance to make a row pass: a 5% noise floor would swamp
  exactly the 2–5% regressions this tool exists to catch.
* **Path length leaks into `.rodata`.** Rust embeds panic locations, C embeds
  `__FILE__` in asserts. Build from a longer directory and `.rodata` grows — reading
  exactly like a code-size regression, and making the committed file dirty on every
  run. Neutralised with `-ffile-prefix-map` / `--remap-path-prefix`; there is a
  regression test for it (run from a 100-char-longer path, numbers must not move).
* **Sizing the linked harness measures the wrong thing.** `generators/c/project.go`
  links `harness/main.c` plus the corelib's JSON test helper plus libc; a JSON parser
  and `printf` swamp the signal. That is why the footprint recipes size the generated
  code, not `emit: project` output.
* **Sizing a Rust archive over-counts by ~42%** — see [Per-row recipes](#per-row-recipes).
* **A payload left at schema defaults measures the omission path.** Sparse-canonical
  encoding drops default-valued fields, so a lazily-built payload silently halves the
  workload. `payload/vehicle_telemetry.json` sets every field to a non-default value
  with every bounded container full, and `rows.json` carries a `wire_len` floor.

## How the `bench` verb is emitted

It is **generated**, not hand-written — a hand-written driver cannot compile against
two generator revisions, and the API-changing commits are precisely the ones worth
measuring (`docs/perf-patches/rust-fixed-arrays.md` changed the emitted struct from
`Vec<T>` to `[T; N]`; `java-primitive-arrays` changed `List<Long>` to `long[]`). It
lives in each backend's `project.go` beside the `encode`/`decode` verbs and is
IR-driven like them, so it needs no new config key and no schema coupling.

```
# toggle rows
harness bench <workload>            # exactly one op; setup in main, outside collection

# subtract rows
harness bench <workload> <reps>     # fixed warmup, then <reps> measured ops
```

One residual risk is worth knowing: the loop template is emitted by the revision
under test, so a change to it moves every number for reasons that are not the
generated message code. That happened once already during development (the Rust
wrapper's return type changed and `rust-rs` encode moved 9890 → 10000). If you edit
a `run_*`/bench-loop template, expect the whole column to shift and say so in the PR.

## Timing

Valgrind is ~50-100x, and the `subtract` rows run **three** points at 10000/110000/
210000 reps — Java and TypeScript are minutes per row, not seconds. The `toggle`
rows are cheap by comparison (one op each). Budget accordingly, and prefer
`run.sh --rows <id>` while iterating.
