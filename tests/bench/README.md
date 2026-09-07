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
| Ir/op | every row | one op under Callgrind, host x86-64, `-O3` |
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

**That guard did not hold for a while, on exactly one cell,** and what it took to
get it back is worth reading before trusting any other cell. `go` encode was bimodal —
18625 or 20468, the low value on ~13% of 53 readings taken on an unchanged tree — so
running `run.sh` twice flipped that one cell by −9.15% about one run in eight with
nothing changed, 30x the noise band and past any hysteresis. It was not a measurement
that needed a wider band; it was a defect in the harness, and it is fixed (#494). See
[Measured jitter, per row](#measured-jitter-per-row) for the readings before and after.
Every cell in the file is now idempotent as described.

Most rows are exactly reproducible; some are not. `lib/format.py` therefore applies
**hysteresis** — a cell keeps its committed value while a new reading is within 0.3%
of it, and moves only on a change big enough to be real (`stabilize()`). Which rows
need that, and how much, is measured: see
[Measured jitter, per row](#measured-jitter-per-row). The split is **not** toggle vs
subtract, which is what this file used to say.

Because a held cell discards the reading that produced it, every run also writes
`results-raw.txt` — the same table with no hysteresis. See
[The raw sidecar](#the-raw-sidecar).

Rounding was tried first and does not work. Every deterministic rounding has bucket
edges, and a raw value sitting on one flips regardless of how coarse the buckets
are. The idempotence check caught exactly that, on two of three subtract rows in one
run:

```
csharp decode  71100 <-> 71200      (raw ~71,150 — sat on a 3-s.f. edge)
java   encode  16500 <-> 16600      (raw ~16,550 — likewise)
```

The band is a **one-sided deviation from the committed value**, so the quantity to
compare it against is `max |reading - committed| / committed`, not a max-min spread.
The widest that has been measured is 0.122% (`kotlin` encode: a 17221 against the
17200 its readings cluster on; 0.105% against the 17203 the file happens to commit
today). So 0.3% sits ~2.5x above the measurement and an order of magnitude below the
smallest change worth seeing (1%; the wins in `docs/perf-patches/` are tens of
percent). If a row ever flips on its own *jitter*, raise **that row's reps** — which
tightens that jitter — rather than widening the band.

Confirm the flip before spending the reps: re-run the row unchanged a few times and look
at the spread. A crossing can also be an earlier, *real* change that the band held back
until it tipped — and more reps cannot see that one. Check whether a corelib SHA or a
toolchain version in the header moved since that cell last **changed**, and suspect it
first. `kotlin` decode crossed once at +0.3001% and then re-measured around the **new**
value, so its reps were left where they were and the step went to an issue (#488)
instead — see ARCHITECTURE §15.

## Measured jitter, per row

Every row, three or more back-to-back runs on an unchanged tree, raw readings
recorded. Devcontainer, generator `6315cd1`, pinned corelib checkouts at the SHAs the
`results.txt` header of 2026-09-06 names (with one exception — see the caveat below
the table), toolchain as in that header. Nothing was changed between runs of a row,
so every difference below is the measurement's own.

Two run shapes were used, and neither is "one invocation per row per trial" across
the board. The nine `subtract` rows and `go`/`go-unbounded` were driven one row per
`run.sh --rows <id>` invocation. The fifteen `toggle` rows were driven by three
invocations that each passed all fifteen ids — one build of each row per invocation,
so the three trials are still independent runs, but they share a process and a
scratch tree with their siblings. The per-invocation stderr logs were kept outside
the repository (a scratch directory, not committed); what survives here is the
readings themselves.

**The two `go` rows are from a later campaign**, run against the tree that fixed #494
and on corelib-go `d45a5dd`, because their earlier numbers no longer describe the row:
`go` encode read 18625×7 / 20468×46 over 53 runs before the fix (those 53 were the
three above plus 50 further single-row runs) and its decode 60051 / 60100×51 / 60118.
All four halves were re-read directly against the built bench harness, 300 readings
each, rather than through `run.sh`.

That depth is the reason these two rows are the only ones in the table with a spread
worth trusting, and it changed the answer. Both **encode** halves are now exact —
18625 and 13314 on 300 of 300, where 40 readings of the pre-fix binary already give
18625×4 / 20468×36. Both **decode** halves are *not*: each throws four outliers in
300 — `go` 60026 / 60100×296 / 60302×2 / 60385, `go-unbounded` 45088×296 / 45281×2 /
45373×2 — a rate of 1.3%, and the pre-fix binary throws four in 300 as well, because
the fix does not touch decode (it calls `benchAlignSpan` only in the encode branch).
An earlier draft of this table read decode 20 times, saw 60100 twenty times and
recorded 0.000 — but at a 1.3% rate twenty draws miss the outliers 77% of the time,
so that zero was sampling depth, not a property of the row. The pre-fix decode
readings above, three distinct values in 53, were right all along.

Read `n×k` as "that value came up k times". The **spread** column is
`(max − min) / mean` — a property of the readings. It is *not* the quantity
`NOISE_BAND` is compared against, which is a reading's deviation from the *committed*
value; the two are distinguished where the band is sized, below.

| row | method | n | encode readings | spread % | decode readings | spread % |
|---|---|---|---|---|---|---|
| `csharp` | subtract | 3 | 31516×2 31522 | 0.019 | 70709 70710×2 | 0.001 |
| `dart` | subtract | 3 | 25786×2 25788 | 0.008 | 57236 57238 57240 | 0.007 |
| `java` | subtract | 6 | 17008 17009×2 17010 17011 17012 | 0.024 | 30886 30887 30890×2 30891 30898 | 0.039 |
| `kotlin` | subtract | 8 | 17199 17200×5 17203 **17221** | **0.128** | 32751×2 32752×2 32753×2 32755 32756 | 0.015 |
| `python` | subtract | 3 | 1082239×3 | 0.000 | 2310825×3 | 0.000 |
| `python-native` | subtract | 3 | 130539×3 | 0.000 | 801804×3 | 0.000 |
| `ts-bigint` | subtract | 3 | 605520×2 605525 | 0.001 | 768543 768544 768546 | 0.000 |
| `ts-long` | subtract | 3 | 596836 596837×2 | 0.000 | 772111 772112×2 | 0.000 |
| `ts-number` | subtract | 3 | 614908×2 614909 | 0.000 | 767709×2 767710 | 0.000 |
| `c` | toggle | 3 | 25985×3 | 0.000 | 52648×3 | 0.000 |
| `cpp-c-cpp` | toggle | 3 | 33650×3 | 0.000 | 40154×3 | 0.000 |
| `cpp-c-cpp-dyn` | toggle | 3 | 34020×3 | 0.000 | 51157×3 | 0.000 |
| `cpp-cpp` | toggle | 3 | 12608×3 | 0.000 | 29953×3 | 0.000 |
| `cpp-cpp-static` | toggle | 3 | 12300×3 | 0.000 | 22706×3 | 0.000 |
| `cpp-cpp-unbounded` | toggle | 3 | 8543×3 | 0.000 | 21990×3 | 0.000 |
| `go` | toggle | 300 | 18625×300 | 0.000 | 60026 60100×296 60302×2 60385 | **0.597** |
| `go-unbounded` | toggle | 300 | 13314×300 | 0.000 | 45088×296 45281×2 45373×2 | **0.632** |
| `rust-rs` | toggle | 3 | 8273×3 | 0.000 | 23366×3 | 0.000 |
| `rust-rs-no-std` | toggle | 3 | 8079×3 | 0.000 | 29998×3 | 0.000 |
| `rust-rs-no-std-dyn` | toggle | 3 | 8292×3 | 0.000 | 38808×3 | 0.000 |
| `rust-rs-static` | toggle | 3 | 8120×3 | 0.000 | 16564×3 | 0.000 |
| `rust-rs-unbounded` | toggle | 3 | 6762×3 | 0.000 | 21215×3 | 0.000 |
| `zig` | toggle | 3 | 9941×3 | 0.000 | 22495×3 | 0.000 |
| `zig-unbounded` | toggle | 3 | 7837×3 | 0.000 | 18799×3 | 0.000 |

Four things follow, and three of them contradict what this file used to claim.

**The split is the runtime, not the method.** Both `python` rows and all three `ts`
rows are `subtract` and reproduce to within 1–5 Ir, while `go` — the row that was
noisiest in the file by two orders of magnitude — is `toggle`. Fifteen of the
twenty-four rows repeat *exactly*, both halves, at the depth each was sampled.

Read that count with its depth attached, because the two `go` rows say it is soft.
Twenty-two of the rows were sampled 3–8 times; the `go` rows were sampled 300 times,
and only at that depth does either show the 1.3% outlier rate its decode half
carries. So "repeats exactly" here means "no outlier in three to eight draws", which
a rate like that would clear unseen 90–96% of the time. The two `go` rows are also
the counter-example to what this paragraph used to claim — that every non-exact row
is a `subtract` row. They are `toggle`, and they jitter on decode. Whether the other
thirteen would too, sampled as deep, is untested; nothing here says they would not,
and re-sampling them is #522. Note the size of what depth exposed: 0.597% and 0.632%
are both wider than `NOISE_BAND` and wider than every masked step tabulated below.

Read that as **within one build context**, which is all a back-to-back campaign can
measure. It is not the same thing as reproducing across contexts, and for several rows
it is not even close: see the two caveats below the table and the section after it.

**`kotlin` is not the quiet row three runs made it look.** #473 read it six times,
saw encode 17199–17201, and concluded the band was ~50× that row's jitter. Eight more
runs produced one 17221 — a 0.122% excursion, twenty times the spread those six showed.
A three-run spread is a lower bound on jitter, never a characterisation of it.

**So the band has ~2.5× headroom, not ~10× and not ~50×.** Measured like for like —
the band is a deviation from the committed value, so the comparison is
`max |reading − committed| / committed`, not the spread column beside it. The widest is
`kotlin` encode: 17221 against the 17200 its readings cluster on is 0.122%, and
`NOISE_BAND = 0.003` clears that by 2.5×. (Against the 17203 the file happens to commit
today the same reading is 0.105%, i.e. 2.9×; 2.5× is the conservative of the two and is
the figure used everywhere.) `0.001` would sit *below* that excursion and flip the cell
about **one reading in eleven** — the 11 readings taken at this row's committed reps,
`10000 110000`: three from #473 plus the eight of this campaign. (#473 took three more at
`10000 210000`, a different reps setting, which are not pooled here.) `0.002` clears the
excursion by 1.6×, on a tail seen once in those eleven. The band was **kept at 0.003**
for that reason (#489) — the honest answer was that the premise of narrowing it was
wrong, not that the number was.

**One row used to be past any band's reach, and the band was never the lever.**
Before the fix, `go` encode was bimodal — 18625 or 20468, 1843 Ir apart, on ~13% of
53 runs — so that cell moved at random whatever the band was set to, and widening the
band far enough to hold it would have swallowed every regression this tool exists to
catch. It was a defect in the row (#494), and the two `go` lines in the table are its
post-fix readings. What it was, and why the obvious fix is not one:

The 1843 Ir is, function for function, one Go allocator slow path —
`mcache.refill` → `mcentral.cacheSpan` → `mheap.allocSpan`/`initSpan` and their
bookkeeping — and nothing else in the profile moves at all. `Encode()` allocates the
output buffer (1037 bytes on this schema, so Go's 1152 size class), a span of that
class holds seven objects, and whichever allocation takes the seventh makes the next
one refill. Which of the seven the *collected* op gets is decided by how many objects
of that class the process had already taken before it, and that is not a constant:
probed with `runtime.MemStats`, 6 on 38 of 40 runs and 8 on the other 2 — the Go
runtime's own start-up, nothing the harness does. At 6 the single uncollected warmup
takes the last slot and the op pays the refill; at 8 it does not.

**Adding a warmup does not fix it, it moves it.** The op refills whenever
`prior + warmups ≡ 0 (mod 7)`, and `prior` takes two values two apart, so no count
misses both. Measured, by padding that size class before the op to stand in for a
differently-behaved start-up — at one warmup the slow mode appears at pad 0 and 7
(`prior` 6) and pad 5 (`prior` 8); at two warmups it reappears at pad 4 and 6:

```
pad                 0     1     2     3     4     5     6     7     8
1 warmup (before) high   low   low   low   low  high*  low  high*  low
2 warmups         low    low   low   low  high*  low  high*  low   low
after #494        low    low   low   low   low   low   low   low   low
```

`*` = the slow mode on some but not all of three readings at that pad, which is the
whole shape of the defect: `prior` is itself two-valued.

The fix removes the phase instead of guessing it. `benchAlignSpan`, emitted next to
the warmups and likewise uncollected, allocates the op's own buffer size until the
allocator hands back an object that is **not** adjacent to its predecessor — the first
of a fresh span — so the measured op always takes the second slot of one. Over that
same pad sweep the reading is then 18604 on 27 of 27 runs; unpadded it is 18625 on
**300 of 300**, against 18625×4 / 20468×36 in 40 readings of the pre-fix binary.

**18625 is a chosen phase, not an average.** Pinning the op to the second slot of a
span is exactly the guarantee that it never pays a refill, and a steady-state encoder
does: one refill per seven ops on this schema, ~1843/7 ≈ 263 Ir, so the committed cell
sits ~1.4% below what encoding in a loop actually costs. That is the right trade for
what `results.txt` is — a diff tool, where a stable baseline beats an accurate one and
amortising over R ops would fold the allocator's own R-dependence into every row — but
it means the cell is not the amortised cost of one `Encode()`, and a reader comparing
it against a throughput benchmark should expect that gap.

If the alignment ever fails to find a span boundary — 4096 allocations without one —
the harness prints `benchAlignSpan: no span boundary in 4096 allocations; this reading
is not phase-pinned` to stderr rather than returning quietly, because a silent no-op
there puts the row back to being bimodal with nothing in `results.txt` to show it. It
has not fired in any run recorded here; a standalone probe over size classes 8…40960
terminated in 2–1310 iterations, so the bound has roughly 3x headroom, not 100x.

**`go-unbounded` was never immune, only lucky.** It reads exactly (13314 on 300 of 300
here) with
no alignment of its own, because the size class its growing output buffer lands in
happens to sit far from a boundary in this runtime — pad that class by 12 and the row
jumps 13.9%, exactly as `go` did. Aligning on `cap(wire)` does not reach it: that
message's `Encode()` flushes through a scratch buffer in a *different* class from the
one it returns. Left as it is, measured rather than assumed, and worth a follow-up if
it ever moves.

Two caveats on reading the table, and both bite the next section.

**The absolutes are not comparable with `results.txt`.** The campaign ran against
**pinned local corelib checkouts**, frozen so that three runs of a row differ in nothing
at all. That is what makes the spreads trustworthy and the absolute values not directly
comparable with a full run's — a fresh clone of the same SHA does not always build the
same bytes. The `rust` and `python` rows moved by ~0.1% between a pinned copy and a
fresh clone of the identical commit, which is the same magnitude as the steps the next
section is about; compare spreads, not absolutes.

**The three `ts` rows were measured one corelib commit behind the header**, and the
gap between contexts is bigger than the row's own spread by two orders of magnitude.
The pinned checkout is corelib-ts `f8ea3cd`; the full run that wrote the current
`results.txt` cloned `d664f2f`. `ts-long` encode read 596836/596837/596837 here and
596936 there — 100 Ir apart on a row whose three campaign readings span 1 Ir.

That was re-measured rather than argued, because an unexamined corelib SHA move sitting
under a moved reading is the attribution failure #488 exists to prevent. **The SHA owns
none of it.** `f8ea3cd..d664f2f` is a `vitest` devDependency bump and its lockfile: no
`src/` change, and `tsup`, `esbuild`, `rollup` and `typescript` resolve to identical
versions in both lockfiles. Measured to match, `run.sh --rows ts-long` against two
fresh clones of this repository's corelib-ts, one at each SHA, back to back on the same
host:

```
corelib-ts d664f2f (fresh clone)  x2   encode 596996 596996   decode 772111 772112
corelib-ts f8ea3cd (fresh clone)  x2   encode 596996 596996   decode 772112 772112
```

Bit-identical encode across the SHA move, four runs for four. What moves is the
**build context**: the campaign's pinned checkout carries a `dist/` the recipe reuses
rather than rebuilding, while a fresh clone builds it with whatever `npm install`
resolves at that moment — and the full run's own fresh clone read a third value again
(596936). So `ts-long` encode is bit-reproducible *within* a context and spans 160 Ir
(0.027%) across three of them. Treat the `ts` rows'
spreads as sound, their absolutes as belonging to a context, and a `ts` cell that moved
under a corelib bump as unexplained until the bump is measured.

### What the band is hiding right now

*Counts in this section describe the two files as of generator `6315cd1`, 2026-09-06.
They move on the next full run, and nothing regenerates them — re-derive them from the
files rather than trusting the prose.*

*One partial run has moved under them since: `--rows go,go-unbounded` after #494. The
total is unchanged — `go` encode now agrees between the files (18625 in both) and
`go-unbounded` encode now does not (13335 committed, 13314 read, a −0.16% hold, and
the alignment described above is what moved it) — but the second sentence below
becomes thirteen rather than twelve, since `go-unbounded` encode measures at zero
spread, and the count after the two tables becomes twelve rather than thirteen, since
`go` encode no longer jitters and is no longer held.*

The same question, asked of the files in the repository rather than of a campaign:
`results-raw.txt` is what the last full run read, `results.txt` is what it committed,
and **25 of the 48 cells disagree**. Twelve of those sit on rows the table above
measured at *zero* spread, so the row's own jitter explains none of them.

Those twelve do not all say the same thing, and a **third** column is why. The campaign
read the same rows three times each at the same generator SHA and — for every row below
— the same corelib SHA the `results.txt` header names, but in a different build context
(pinned checkouts, not the full run's fresh clones). Seven cells read the same value in
both contexts. Five read a third.

**Seven masked steps.** Two independent contexts, three campaign readings each, every
reading agreeing to the instruction and every one disagreeing with what the file
commits. No jitter to absorb them and no commit that owns them — the #488 disease at
file scale:

| cell | committed | last full run | campaign (×3) | held by |
|---|---|---|---|---|
| `rust-rs` decode | 23329 | 23366 | 23366 | +0.159% |
| `rust-rs-unbounded` decode | 21184 | 21215 | 21215 | +0.146% |
| `rust-rs-no-std-dyn` encode | 8280 | 8292 | 8292 | +0.145% |
| `cpp-c-cpp-dyn` decode | 51084 | 51157 | 51157 | +0.143% |
| `zig` encode | 9929 | 9941 | 9941 | +0.121% |
| `cpp-c-cpp` decode | 40150 | 40154 | 40154 | +0.010% |
| `c` encode | 25986 | 25985 | 25985 | −0.004% |

(`rust-rs` and `rust-rs-unbounded` decode moved by +37 and +31 Ir, in every run of the
campaign — consistent with one step on the decode path both rows share, though nothing
here establishes that.)

**Five that do not reconcile.** Same rows, zero spread *within* each context, three
different values *across* the two of them. The gap between the two contexts runs from
0.03% to 0.15%, and in three of the five it is **larger than the step** a
committed-vs-read reading would claim. So "the row has no jitter" is a statement about
one context, and these five are **unattributed**, not masked steps. The pinned-vs-fresh
caveat above predicts exactly this, on exactly these rows; it does not say which
reading is right.

| cell | committed | last full run | campaign (×3) | do the two contexts agree? |
|---|---|---|---|---|
| `python-native` decode | 803335 | 802289 (−0.130%) | 801804 (−0.191%) | on the sign, not the size |
| `python-native` encode | 130483 | 130661 (+0.136%) | 130539 (+0.043%) | on the sign, not the size |
| `python` encode | 1081554 | 1081889 (+0.031%) | 1082239 (+0.063%) | on the sign, not the size |
| `rust-rs-no-std` encode | 8078 | 8067 (−0.136%) | 8079 (+0.012%) | **no** — opposite signs, and the campaign lands on the committed value |
| `python` decode | 2309178 | 2309212 (+0.001%) | 2310825 (+0.071%) | **no** — the full run reads the committed value, the campaign is 1647 Ir off it |

The last two are the ones to keep in mind when reading the first table: for
`rust-rs-no-std` encode the −0.136% "step" appears only in the full run, and for
`python` decode the +0.001% "hold" is the full run agreeing with the file while the
campaign disagrees with both. Neither is evidence of a masked step.

The remaining twelve of the 25 sit on jittery rows — `csharp` encode −0.111%, the `ts`
rows around ±0.02–0.08% — and there a held cell is doing the job it was built for.
(Before #494 this read thirteen, and `go` encode's −0.161% was the largest of them;
that is the cell the alignment removed.) (The `ts` rows' campaign readings are from a
different corelib SHA; see the caveat above.)

Narrowing the band does not fix the first group, and the measurements above say why it
cannot be narrowed far enough to matter. Recording the reading does, which is the next
section — that table is now in the repository, so the *committed-vs-read* half of this
question can be asked again after any run with `diff <(…) <(…)` rather than a day of
measuring. The third column cannot: separating a masked step from a change of build
context needs a second context, and that means measuring again.

## The raw sidecar

`results-raw.txt` is the same instruction-cost table with the hysteresis left off: what
each row's last measurement actually **read**, as against what `results.txt` commits.
`run.sh` writes both, from the same run.

```sh
git diff tests/bench/results.txt        # the verdict — what changed enough to count
git diff tests/bench/results-raw.txt    # the readings — including the ones held back
git log -p tests/bench/results-raw.txt  # when a held cell actually moved
```

It exists because a held cell used to leave **no trace at all**. When `kotlin` decode
finally crossed the band it surfaced a step that had landed six regenerations earlier,
and no intermediate reading survived to say which of four corelib bumps, a JDK change
and 820 lines of codegen owned it (#488). With the sidecar committed, that question is a
`git log`.

Two properties to know:

* **It is not idempotent, on purpose.** The jittery rows move it run to run. That
  is the reading changing, not the file being wrong — `run.sh --check` prints its diff
  and does *not* fail on it, because staleness is a property of `results.txt`.
* **It carries no header.** Every claim about what produced the numbers — corelib SHAs,
  toolchain versions, schema hashes — is in `results.txt` beside it, written by the same
  run. Two headers could disagree; one cannot.

A `--rows` run updates the rows it measured and carries the rest, exactly as
`results.txt` does, and a run that is refused a true header writes neither file. An
`--out` preview and `bench.yml`'s per-row artifacts write no sidecar at all: a second
measuring device's readings are not this repository's record.

The carry-forward has one asymmetry worth knowing. `results.txt`'s comes from the
committed file, which is always there; the sidecar's comes from the sidecar, which
might not be — delete it, or check out a revision that predates it, and a `--rows` run
would write a record holding only the rows it measured. **That run is refused**, with
the same message either way: regenerate both files with a full run. `--check` also
reports a sidecar whose row set is short of `results.txt`'s, without failing on it.

## Reading a diff

The header records the corelib SHAs and the `## toolchain` table records every
compiler that built a row. Read both first:

| header | numbers | conclusion |
| - | - | - |
| unchanged | moved | **your generator change did it** — the signal |
| moved | moved | the corelib or a toolchain did it |
| moved | unchanged | corelib moved, no impact |

Each `schema:` line carries a sha256 for the same reason: if you edit
`vehicle_telemetry.yaml`, every number measured on it legitimately moves, and the
hash says so. There is one line per schema, and the second names the rows it covers
(see [Schemas](#schemas)) — a single hash would be a true statement about most rows
and a false one about the rest.

**Corelibs are deliberately NOT pinned.** They are cloned from their default branch,
exactly as `tests/conformance/*/run.sh` does. A corelib must match the generated
code built against it — pinning would break this bench on precisely the commits
that adopt a new corelib API, which are the ones most worth measuring. Provenance
in the header replaces pinning.

Consequence, stated plainly: **absolute numbers are not comparable across days**,
because the corelib moves underneath. Compare within a run, or across a run where
the header didn't move.

**A one-row refresh keeps the header.** `run.sh --rows <id>` re-measures those rows
and preserves everything else — the other rows' numbers, and every header line that
attributes them: the corelib SHAs, the `sofab-engine` lines, the toolchain versions
and the schema hashes. Each of those is probed from this run's checkouts, host and
working tree, so without the merge a one-row refresh restated all four for the
twenty-three rows it only carried. Entry by entry, then: the value this run resolved
where this run re-measured every row that entry covers, and otherwise the committed
value, which is what those cells were in fact built with. Nothing in the line
distinguishes the two, because the claim each entry makes — "this is what the numbers
below were measured against" — is the same either way.

**And a refresh that cannot say that is refused.** `--rows` selects per row, but an
entry covers every row it built — eight of the twelve corelibs back more than one
row, `valgrind` backs all of them. Re-measure some of a corelib's rows and let its
SHA move, and the entry would be true of the cells just measured and false of the
ones carried: the header moved and those numbers did not, which reads as *the corelib
bump cost them nothing*. There is no true header to write in that case, so `run.sh`
writes none and leaves `results.txt` alone, naming the rows to add:

```
refusing to write a header this run cannot make true:
  corelib-zig moved be2f1d8 -> 2423a95, but zig-unbounded was not re-measured against it
results.txt is unchanged. Re-run with --rows zig,zig-unbounded
```

An entry that did not move is never refused, and neither is one whose rows this run
measured none of — a `--rows kotlin` run on a box with no `zig` keeps the committed
`zig` version and says so on stderr, because a probe that describes no cell in the
file is a reading about nothing.

A **full** run carries nothing forward and is never refused: it merges nothing, so
its header is one run's, end to end. So is the header of a `--rows` run written
somewhere else with `--out` (this is how `.github/workflows/bench.yml` measures):
that output is a second opinion, not a candidate for the committed file.

Override a clone to test a local corelib:

```sh
SOFAB_C_CORELIB=~/src/corelib-c-cpp tests/bench/run.sh --rows c
```

## Schemas

Two, named in `rows.json`: a top-level `schema`/`payload`/`message` every row
measures, and a per-row override that four rows take.

**The default — `examples/messages/realworld/vehicle_telemetry.yaml`.** Chosen
because every array is bounded (`count`) and every string/blob has a `maxlen`, so it
generates for **every** row — including C and the no_std/c-cpp footprint profiles —
with no `allow_dynamic` and no per-config mutation. Twenty of the rows therefore
measure the identical schema, which is what makes them comparable to each other.

`examples/messages/example.yaml` cannot be used for it: its intentionally-unbounded
`somemap` forces `allow_dynamic: true` on the footprint rows and an injected
`count: 8` for C (see `tests/gen-artifacts.sh`), so the rows would no longer be
measuring the same thing.

**The second — `tests/bench/schema/unbounded_ingest.yaml`.** Full boundedness is
exactly what makes the default schema usable everywhere, and it is also a blind
spot: a **receiver cap** (`max_dyn_array_count` / `_string_len` / `_blob_len`,
ARCHITECTURE §9.5, CORELIB_PLAN §6.2.1) governs schema-**unbounded** fields only, and
a backend emits the constant and the check only where the message reaches one. On
`vehicle_telemetry` no target emits a single `max_dyn_*` constant, cap argument or
guard, so the entire cap machinery was invisible to this file **in both directions**:
neither what it costs nor what removing it saves could show up in a diff.

`unbounded_ingest.yaml` is that machinery's row. Every kind a cap governs appears
there without its schema bound — scalar `string`/`blob`, a native array with no
`count`, a matrix unbounded in both dimensions, `string`/`blob` wrapper arrays, and a
`struct` sequence array (no count header at all, so the cap binds the element index)
— **plus a bounded twin of each**, because the cost being measured is a decoder
telling "schema bound → INVALID" from "receiver cap → LimitExceeded" per field, which
is what every port's cap plumbing implements. `tests/matrix/bench_rows_test.go`
asserts both halves of that on the IR, so a stray `count:` added to the fixture fails
a test instead of silently emptying the rows.

It is not under `examples/` on purpose: it is a measurement fixture, not a showcase,
and being unbounded it generates only for the heap-backed targets. The statically
bounded profiles (`c`, `cpp` `corelib: c-cpp`, `rust` `rs-no-std`) reject an
unbounded field at schema-validation time, so they have no unbounded twin — which is
the same fact as §9.5's "their caps are inert".

### The `-unbounded` rows: a pair, and not a speedup

Four rows take it — `cpp-cpp-unbounded`, `zig-unbounded`, `go-unbounded`,
`rust-rs-unbounded` — each with the **same config** as its bounded sibling. Read each
as a pair with that sibling, the way the `-dyn` and `-static` rows are read, with one
difference: **the two numbers are not comparable as a difference.** They are
different messages of different sizes; the pair is not "caps cost X". What the pair
gives is a place where a cap change moves a number *at all*, and a control row beside
it that must **not** move when it does.

Four rows rather than one because the enforcement **site** differs per port and per
field kind, and that is the thing being measured (ARCHITECTURE §9.5):

| row | where the comparison runs |
| - | - |
| `cpp-cpp-unbounded` | all three kinds ride into the corelib, on the `readString`/`readArray` calls generated code already makes |
| `zig-unbounded` | split: array counts and wrapper indices in the corelib's `arrays.*` allocation calls, string/blob lengths generated |
| `go-unbounded` | the wrapper arrays go to corelib collectors, which own an element's index and length |
| `rust-rs-unbounded` | entirely generated (§9.5.2: the borrow checker rules out the collector shape) |

The fifth shape — Java/Kotlin/C#'s `PayloadAcc` — has no row because those are
`subtract` rows at minutes each, while all four above are `toggle` rows costing one op.
Add one if that path starts moving.

## Not comparable to the corelibs' own benches

Every corelib ships `bench/run_callgrind.sh` reporting Ir/op for four fixed
workloads (`encode_u64_array`, `encode_typical`, …) that call the corelib API
directly. This harness reuses their **method**, not their numbers: our workload is
the generated code for the bench schemas, which is a different thing. Do not put
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

## Configuration axes, and which ones have rows

A row is a `(generator config, corelib)` pair, so anything that changes either can
change the numbers — and only what has a row gets measured. The axes that exist:

| Axis | Where | Covered by |
|---|---|---|
| corelib choice | `cpp`: `cpp` \| `c-cpp`; `rust`: `rs` \| `rs-no-std` | `cpp-cpp` / `cpp-c-cpp`, `rust-rs` / `rust-rs-no-std` |
| `allow_dynamic` | `cpp` (only on `c-cpp`), `rust` (only on `no_std`) | `cpp-c-cpp-dyn`, `rust-rs-no-std-dyn` |
| `int64` mode | `typescript`: `bigint` \| `long` | `ts-bigint`, `ts-long` |
| corelib engine (python) | pure-Python vs the Cython `_speedups` accelerator | `python`, `python-native` |
| receiver caps (§9.5) | live only where the *schema* leaves a field unbounded, so this is a schema axis, not a config one | `cpp-cpp-unbounded`, `zig-unbounded`, `go-unbounded`, `rust-rs-unbounded` |
| corelib build switches | `SOFAB_DISABLE_*` (c-cpp), cargo features (rs-no-std), `sofab_no_strict_utf8` (go) | **nothing** — every row builds the defaults |
| corelib engine (ts) | `setKernel`: `jsKernel` vs native (N-API) / wasm | **nothing** |

The last two are the honest gaps. The build switches are the footprint story itself —
dropping wire types is how a `c-cpp` or `rs-no-std` build gets small — and no row
exercises them, so the file shows the all-features size only. The TypeScript kernels
are a milder case than python's was: `jsKernel` is the deterministic default,
swapping one in takes an explicit `setKernel()` call, and the native addon is a
separate optional package that this repo does not ship — so the row measures a
well-defined configuration, just not every one on offer.

### The `-dyn` rows: read the pair, not the row

`allow_dynamic` decides **where** variable-length fields live: inline, sized from the
schema bound (the default), or in a heap container holding what the message actually
carries. It does nothing on `std` Rust or on `corelib: cpp` — those are heap-backed
either way — so it only pairs with the two footprint rows.

The pair is the measurement; neither number alone is a verdict. Turning it on trades
static bytes for an allocator, and on the two targets that goes in opposite
directions:

* **cpp-c-cpp** `.text` 6589 → 14287 on ARMv6-m. The inline build never calls
  `operator new`, so switching drags in newlib's malloc — more than doubling `.text`
  while the objects get smaller.
* **rust-rs-no-std** `.text` 9145 → 10649, `.bss` 0 → 4. Bare metal ships no
  allocator at all, so the footprint driver supplies the most trivial bump allocator
  that can work (`lang/rust.sh`, appended only when the generated crate pulls in
  `extern crate alloc`). Never freeing makes its `.text` a **floor**: a real firmware
  allocator costs more, never less. The arena sits at a fixed address rather than in a
  static array, so an arbitrary heap size cannot land in `.bss` and pose as a
  measurement — the 4 bytes are the bump cursor.

What neither number can show is the heap those builds now need at runtime.
`.text`/`.data`/`.bss` is a static-section measurement; a dynamic build that looks
cheap in `.bss` still needs RAM the inline build had already accounted for.

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
  flash budget**. Replacing the header's `<string>`/`<vector>` with
  `FixedString<N>`/`FixedBytes` sized from schema `maxlen` would let `cxx_flags` be
  dropped, with a step down expected. The `bss=180` on those rows is static RAM in a
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
* **The `zig` and `csharp` rows carry runner-specific pins** (see `lang/zig.sh`,
  `lang/csharp.sh`) — for two *different* reasons, both of which made the row measure
  as `!` on the `bench.yml` runners while measuring clean in the devcontainer.
  - **zig** builds `-Dcpu=baseline`. `standardTargetOptions` defaults to the host CPU,
    so an unpinned build on a runner with AVX-512 emits 512-bit instructions the
    runner's older Callgrind (3.22 vs the devcontainer's 3.26) cannot decode — the run
    SIGILLs. Baseline is the generic x86-64 that gcc/rustc/go already emit (which is
    why the other native rows are fine) and, as a bonus, makes the row byte-identical
    across the runner and the devcontainer.
  - **csharp** runs the dll matched by `*/bin/Release/*` (not `*Release*`), under
    `--roll-forward Major` and an AVX-512 guard (`DOTNET_EnableAVX512F=0`
    `DOTNET_PreferredVectorBitWidth=256`, like zig's). The real fix is the path anchor:
    a Release build leaves four `harness.dll` (`bin/…` and three under `obj/…`,
    including the `refint` reference assembly), and `find | head -1` returns them in
    directory order — not stable across filesystems. The devcontainer hit `bin/` first,
    the runners hit `obj/…/refint/` first, and that metadata-only assembly has no
    `runtimeconfig.json`, so the host aborts with "`libhostpolicy.so` not found"
    (~2.5M Ir, identical at every rep count) → zero slope → `!`. `Major` then lets the
    `net9.0` app run on a newer major runtime if that is all a runner has.

  All of these knobs are no-ops on the AVX-512-less devcontainer, so `results.txt` is
  unchanged but for the zig decode value baseline moved. When a row does fail to
  measure, the harness now greps its Callgrind log for the tell-tale lines and prints
  them next to the `!` instead of leaving it silent — which is what surfaced the
  `refint` dll after two wrong hypotheses had been chased on the ~2.5M-Ir signature.


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

The `kotlin` row needs a JDK the Kotlin Gradle plugin supports (17..24) — Gradle
itself comes from the corelib's own wrapper, so nothing else is required. The
devcontainer's default JDK is deliberately past that window, so it installs a
second one and exports `SOFAB_KOTLIN_JDK`; set it yourself anywhere else.
It is per-row rather than a plain `JAVA_HOME` export on purpose: pointing the whole
run at another JVM would move the `java` and `csharp` rows for a reason no generator
change caused, and `lib/format.py` resolves the same knob into the `## toolchain`
table so a row measured on a second runtime says so (`kotlin-jdk`).

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

* **Provenance comparison first.** Whatever differs from the header of the committed
  file — toolchain versions, corelib SHAs, schema digests — is named before any
  number, because any of the three moves rows on unchanged generated code.
* **Failed measurements are their own section**, and the only thing that fails the
  job. A `!` cell means a broken run, not a slow row — and it would overwrite a
  committed value if it reached the file.
* **Outliers (≥5%) are separated from ordinary movement (>0.3%)**, so a doubled row
  cannot hide in a list of wobbles.

It reads the toolchain comparison out of the `## toolchain` table on both sides, and
filters it to the tools that built the row being judged — a `go` artifact reporting
that the Zig compiler drifted is true and useless.

That comparison is per **(tool, row)**, not per tool, because the table is not a
tool→version map: `sofab-engine` gets one line per python row, since the two rows run
different corelib-py engines. Keyed by name alone the second line overwrote the
first, and both sides collapsed the same way — so a healthy `python` artifact read as
engine drift on every run, while a `python` row that had really flipped engines
compared `native` against `native` and reported nothing (#492). A tool the committed
table records no line for at this row (a bench row added since `results.txt` was
last written) falls back to the version recorded for it elsewhere, but only when
every line *either* file carries for that tool agrees — so a single committed
`sofab-engine` line, the state right after a second python row is added to
`rows.json`, cannot answer for the row it does not name.

The table is split on its padded columns, not on whitespace, because `format.py`
writes `(not found)` as a version when a probe finds no tool at all — a value with a
space in it. Split on whitespace that line named no real row and was dropped, so the
one drift the writer goes out of its way to record was the one the report could not
read. `format.py`'s own reader of the same table (`parse_previous`, the carry-forward
a `--rows` run depends on) had both defects too, and lost that line entirely: a
partial run following a run where a tool had vanished re-probed the tool and wrote
this box's answer over rows it had only carried (#502).

The other two header entries are compared the same way (#501), and the corelib
comparison is deliberately **context, not a finding**: `run.sh` clones every corelib
from its default branch and never pins it, so a SHA differing from the committed file
is the ordinary case and most runs print that table. It is still worth printing —
an Ir/op number is the cost of the generated code *plus* the corelib it calls, so a
row that moved under a corelib bump is a different question from one that moved on
its own — but it never affects the exit status, which stays reserved for a
measurement that failed. Schema digests are compared per row: the `# schema:` line
with no rows column is the default schema, covering every row that does not name its
own, so an edit to it says nothing about a row measured on another schema.

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

Go additionally needs a **warmup**, which the other `toggle` rows do not. The op
`toggle` collects is the *first* one, and Go's runtime builds interface tables and
resolves type/name offsets lazily on first use (the allocator likewise touches each
size class once). Those one-time costs land on the measured op: on the bench schema
they were 18k Ir of a 55k decode (32%) and 5.5k Ir of a 25k encode (22%). They also
scale with how many distinct types the generated code converts to interfaces, so a
codegen change that adds itabs reads as a per-op regression that does not exist —
it did, at +44% on decode, while the warmed number was *below* the previous value.
The generated harness therefore runs an uncollected `warmup_<w>` first
(`generators/golang/project.go`). One warmup op is enough: Go is AOT-compiled, so
there are no JIT tiers to climb, and every cost being warmed is a global cache
filled on first touch. The warmup duplicates the body rather than calling `run_<w>`
— toggling keys on entering the symbol regardless of caller, so delegating would
collect the warmup too. c, cpp, rust and zig have no lazily-initialized runtime
metadata and need none of this.

**`subtract`** (java, kotlin, python, ts, csharp) — no native symbol exists (the hot
code is JIT'd or interpreted), so run at two rep counts and subtract:

```
Ir/op = ( Ir(R2) - Ir(R1) ) / ( R2 - R1 )
```

which cancels all fixed cost exactly — startup, class loading, JIT compilation,
setup. The scale of what it removes is easy to underestimate: for Python the
intercept is **146M Ir**, against ~841k Ir/op. Measured without subtracting, the
number would be almost entirely interpreter startup.

Three things have to hold, and each one broke in practice before it worked:

1. **A fixed warmup** on the JIT rows (java, kotlin, csharp, ts), independent of `reps`, in
   the generated harness — so the hot methods reach their final tier *before* the
   measured loop and every measured op runs at steady cost. Being identical in both
   runs, it cancels. (`corelib-java/.../Callgrind.java` does the same, and says why.)
   Python needs none: CPython has no tiers, and it measures affine to 0.0004%.
   Python has a different problem instead — **two engines**, see below.
2. **Runtime pinning** (`lang/<x>.sh`), adopted from each corelib's script: one JIT
   tier, no GC, no per-run-seeded hashing.
3. **Rep counts at the corelib's scale** — `10000 110000` for java/kotlin/csharp, per
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

### python has two engines, so it has two rows

corelib-py ships the pure-Python classes *and* a Cython accelerator
(`sofab._speedups`, built by its `setup.py`). `sofab/__init__.py` imports the
accelerator whenever it is importable and falls back to pure Python otherwise — an
`ImportError` caught internally, with nothing in the process announcing which one
won. They are not close:

| row | encode Ir/op | decode Ir/op |
|---|---|---|
| `python` (pure) | 900,876 | 2,180,471 |
| `python-native` | 125,912 | 457,558 |
| | **7.2× apart** | **4.8× apart** |

For a long time only the fallback was measured, and silently: the recipe puts the
corelib's *source tree* on `PYTHONPATH` and built no extension. The cost of that
being invisible is easy to state — corelib-py landed *"restore the accelerator's hot
paths (encode 3.0x, decode 1.5x)"* and the row did not move by one instruction,
because it could not see that code.

**Each row pins its own engine, and that is load-bearing.** The two share one corelib
checkout (`clone_corelib` caches per run), so the `build_ext` done for
`python-native` leaves the extension sitting in the tree — an unpinned `python` row
would then import it and the result would depend on row order. `python` therefore
forces `SOFAB_PUREPYTHON=1` rather than relying on the absence of a `.so`.

**`python-native` refuses to report a number it did not earn.** `setup.py` marks the
extension `optional=True`, so a compile it cannot do is not a failure exit — the
recipe verifies `sofab.IMPL == "native"` and fails the row otherwise. Without that it
would report the pure engine's cost under the native row's name, which is exactly the
confusion these two rows exist to end. It needs Cython: the devcontainer installs it
(`.devcontainer/Dockerfile`), and `bench.yml` pip-installs it for that row only.

Either way the engine that actually ran is recorded, one line per row, in the
`## toolchain` table as `sofab-engine`.

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
  `payload/unbounded_ingest.json` is written to the same rule; there "full" is a
  judgement rather than a schema `count`, so its containers carry the element counts
  a plausible batch would (4–16), well under the caps, because a payload at the cap
  would measure the rejection path instead of the accept path.

## How the `bench` verb is emitted

It is **generated**, not hand-written — a hand-written driver cannot compile against
two generator revisions, and the API-changing commits are precisely the ones worth
measuring (`docs/perf-patches/rust-fixed-arrays.md` changed the emitted struct from
`Vec<T>` to `[T; N]`, since reverted by `count`-is-a-capacity; `java-primitive-arrays` changed `List<Long>` to `long[]`). It
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
