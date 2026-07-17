#!/usr/bin/env bash
# TypeScript Ir/op recipe — see tests/bench/README.md.
#
# The CLR/JVM subtract rows are straightforward; TypeScript needed TWO fixes, each
# for a distinct reason, both found the hard way against the affineness gate.
#
# (1) V8 TIERING. The subtraction needs a constant per-op cost, but V8's default
#     tiering makes Ir non-monotonic in reps: the more iterations it sees, the harder
#     it optimizes, so more reps does LESS total work. Flag combos measured:
#       --jitless                                     44%  (interpreter ICs warming)
#       --always-turbofan          wu=5000/20000  131% / 282%  (reopt mid-loop, s<0)
#       --predictable --single-threaded               20%  (closer, tiering still moves)
#       --predictable --single-threaded --max-opt=1  ~0.05%  AFFINE  <-- this
#     --max-opt=1 caps V8 at the Sparkplug BASELINE JIT, so TurboFan never engages and
#     no tier transition happens inside the measured loop. --predictable makes it
#     single-threaded and deterministic. The fixed warmup drives ignition->baseline
#     before the measured ops.
#
# (2) tsx CORRUPTS THE SUBTRACTION. Running harness.ts through tsx forks a child node
#     (callgrind traces only one PID) AND transforms the TS per-process with an
#     asymmetric on-disk cache — the first of the three rep points pays a cache-write
#     the other two don't, which does not cancel. Symptom: affine in an isolated
#     one-shot, but the same config REJECTED under run.sh's three-point path, both
#     rows, reproducibly. Fix: precompile TS->JS with tsc once in bench_build_ir and
#     run plain `node dist/harness.js` — one process, no per-run transform, nothing to
#     cache asymmetrically. Plain node is then affine to <0.1% AND reproducible across
#     trials (trial1 encode s2=741720, trial2 s2=741720 — identical).
#
# CONSEQUENCE, also in the README and the results.txt note: the ts rows measure the
# V8 BASELINE tier, not fully-optimized TurboFan. That is a stable, deterministic
# reference and correct for a relative diff tool ("did my change help?"); it is not
# the absolute-fastest production number. It is why ts needs flags the other subtract
# rows do not.
#
# Flags go on the `node` command line, NOT NODE_OPTIONS (which rejects --predictable).
#
# No footprint row: corelib-ts is a maxspeed target.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2"

    # The emitted package.json depends on "@sofa-buffers/corelib": "*", which would
    # resolve from the registry. Repoint it at the checkout, exactly as
    # tests/conformance/typescript/run.sh does.
    if [ ! -f "$corelib/dist/index.js" ]; then
        ( cd "$corelib" && npm install --no-audit --no-fund --silent \
            && npm run build --silent ) >/dev/null 2>&1 || return 1
    fi
    node -e "const p=require('$proj/package.json');
             p.dependencies['@sofa-buffers/corelib']='file:$corelib';
             require('fs').writeFileSync('$proj/package.json',JSON.stringify(p))" || return 1
    ( cd "$proj" && npm install --no-audit --no-fund --silent ) >/dev/null 2>&1 || return 1

    # Precompile TS -> JS so the measured process is plain node (see fix (2) in the
    # header). The emitted tsconfig is noEmit (tsx does the transform at runtime); we
    # override it. The generated imports are ESM (./message.js), so ESNext/bundler
    # match how tsx would have loaded them.
    ( cd "$proj" && ./node_modules/.bin/tsc --noEmit false --outDir dist \
        --target ES2020 --module ESNext --moduleResolution bundler \
        harness.ts message.ts ) >/dev/null 2>&1 || return 1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
# Plain node on the precompiled JS (fix (2)); --predictable --single-threaded
# --max-opt=1 pin V8 to the deterministic baseline tier (fix (1)).
bench_cmd_ir() {
    echo "node --predictable --single-threaded --max-opt=1 $1/dist/harness.js bench $2"
}

# bench_ir_env <proj> <corelib>
# Warmup must exceed the baseline-JIT threshold so the measured loop runs at steady
# cost; 2000 is well past it (Sparkplug compiles far earlier than TurboFan, which
# --max-opt=1 disables). Independent of reps, so it cancels in the subtraction, and
# small enough to keep each callgrind run cheap (the warmup runs on every rep point).
bench_ir_env() {
    echo "SOFAB_BENCH_WARMUP=2000"
}
