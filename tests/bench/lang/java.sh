#!/usr/bin/env bash
# Java Ir/op recipe — see tests/bench/README.md.
#
# The JVM JIT-compiles the hot path at runtime, so there is no native symbol to
# --toggle-collect on: this is the `subtract` method. Ported from
# corelib-java/bench/run_callgrind.sh, whose header explains why each flag is here.
#
# The subtraction is only clean if the two runs differ in NOTHING but the rep count.
# The generated harness supplies a fixed warmup (so the measured ops are already at
# their final tier); these flags supply the rest:
#
#   -XX:-TieredCompilation -XX:-BackgroundCompilation -XX:CompileThreshold=2000
#       one synchronous compile tier, reached during the harness's fixed WARMUP, so
#       no tier transition happens inside the measured loop. This is the thing V8
#       cannot be told to do, which is why the ts-* rows are unmeasurable.
#   -XX:+UseEpsilonGC -Xms4g -Xmx4g
#       no garbage collection at all and a fully-committed heap, so GC and heap
#       growth add no variable instructions (a bounded run never fills it).
#
#       4g, NOT the 1g corelib-java uses: EpsilonGC never frees, so the WHOLE run's
#       allocation has to fit. The corelib benches a tiny "typical" message; our
#       32-field VehicleTelemetry allocates far more per decode, and at 1g the run
#       ran out of headroom and Ir stopped being affine in reps (slopes 4.96% apart,
#       rejected by lib/callgrind.sh's gate). At 4g the same measurement is 0.16%.
#       If a future schema grows, this is the first knob to check -- and note that
#       RAISING the warmup makes it worse, not better, since that allocates too.
#   -XX:hashCode=2
#       a constant identity hashCode; the default seeds identity hashes from a
#       per-run PRNG, which would differ between the two runs.
#
# No footprint row: corelib-java is a maxspeed target.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2" ver
    ( cd "$corelib" && mvn -q -DskipTests install ) >/dev/null 2>&1 || return 1
    ver="$(cd "$corelib" && mvn -q -Dexec.executable=echo -Dexec.args='${project.version}' \
             --non-recursive exec:exec 2>/dev/null | tail -1)"
    ( cd "$proj" && mvn -q ${ver:+-Dsofab.version="$ver"} package ) >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
bench_cmd_ir() {
    echo "java -XX:+UnlockExperimentalVMOptions -XX:+UseEpsilonGC -Xms4g -Xmx4g \
-XX:-TieredCompilation -XX:-BackgroundCompilation -XX:CompileThreshold=2000 \
-XX:hashCode=2 -jar $1/target/harness.jar bench $2"
}
