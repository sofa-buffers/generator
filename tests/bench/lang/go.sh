#!/usr/bin/env bash
# Go Ir/op recipe — see tests/bench/README.md.
#
# Go is AOT-compiled with no JIT, so it uses `toggle` like the native rows rather
# than the rep-subtraction the JIT runtimes need. Its symbols are package-mangled,
# so the toggle target is main.run_<workload>, not run_<workload> — hence SYM.
# corelib-go/bench/run_callgrind.sh does exactly this.
#
# The runtime still has to be tamed for a single op to be deterministic under
# Valgrind. Quoting that script:
#
#   GOMAXPROCS=1 (one OS thread), GODEBUG=asyncpreemptoff=1 (no preemption signal
#   storms, which Valgrind serializes oddly), GOGC=off (no GC during the op).
#
# Go has no footprint row: it ships no bare-metal target, so bench_size is absent.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2"
    sed -i "s#\${SOFAB_GO_CORELIB}#$corelib#" "$proj/go.mod" 2>/dev/null || true
    ( cd "$proj" && go mod tidy >/dev/null 2>&1 && \
        go build -trimpath -o harness_bin ./harness ) >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>
bench_cmd_ir() {
    echo "$1/harness_bin bench $2"
}

# bench_ir_sym <workload>       -> the --toggle-collect target
# bench_ir_env <proj> <corelib> -> env applied to the measured run
bench_ir_sym()  { echo "main.run_$1"; }
bench_ir_env()  { echo "GOMAXPROCS=1 GODEBUG=asyncpreemptoff=1 GOGC=off"; }
