#!/usr/bin/env bash
# Dart Ir/op recipe — see tests/bench/README.md.
#
# The Dart runtime has no native `run_<workload>` symbol to --toggle-collect on,
# so this is the `subtract` method: two rep counts, subtract the totals. The
# harness is AOT-compiled to a native exe (`dart compile exe`), so — unlike a JIT
# `dart run` — there is no tier transition inside the measured loop; combined with
# the generated harness's fixed warmup, the two rep runs differ in nothing but the
# rep count and the startup cost cancels exactly.
#
# No footprint row: corelib-dart is a maxspeed target with no bare-metal build.

# bench_build_ir <gen_proj> <corelib>
#   The generated pubspec.yaml carries the ${SOFAB_DART_CORELIB} path placeholder;
#   wire it, resolve deps, and AOT-compile the harness to ./harness.
bench_build_ir() {
    local proj="$1" corelib="$2"
    ( cd "$proj" \
        && sed -i "s#\${SOFAB_DART_CORELIB}#$corelib#" pubspec.yaml \
        && dart pub get >/dev/null 2>&1 \
        && dart compile exe bin/harness.dart -o harness ) >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
bench_cmd_ir() {
    echo "$1/harness bench $2"
}

# bench_ir_env <proj> <corelib>
#   The AOT exe is self-contained; no per-runtime pinning is needed (there is no
#   JIT tier, GC server mode, or hash seed that would move the delta).
bench_ir_env() {
    echo ""
}
