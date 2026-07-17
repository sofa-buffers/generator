#!/usr/bin/env bash
# Zig Ir/op recipe — see tests/bench/README.md.
#
# AOT with unmangled `export fn` symbols, so it uses `toggle` like the other native
# rows: --toggle-collect=run_<workload> names the symbol directly.
#
# --release=fast is what the emitted build.zig calls for ("corelib-zig is the
# max-speed port: --release resolves to ReleaseFast, the configuration the corelib
# is tuned for. Plain zig build = Debug."). Debug would measure code that never
# ships.
#
# Zig has no footprint row here: corelib-zig is a maxspeed target, so bench_size is
# absent.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2" rel
    # build.zig.zon wants a path relative to the project, like the conformance run.
    rel="$(realpath --relative-to="$proj" "$corelib" 2>/dev/null || echo "$corelib")"
    sed -i "s#\${SOFAB_ZIG_CORELIB}#$rel#" "$proj/build.zig.zon" 2>/dev/null || true
    ( cd "$proj" && zig build --release=fast ) >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>
bench_cmd_ir() {
    echo "$1/zig-out/bin/harness bench $2"
}
