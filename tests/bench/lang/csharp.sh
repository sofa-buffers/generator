#!/usr/bin/env bash
# C# Ir/op recipe — see tests/bench/README.md.
#
# The CLR JIT-compiles the hot path at runtime, so there is no native symbol to
# --toggle-collect on: this is the `subtract` method. Ported from
# corelib-cs/bench/run_callgrind.sh, whose header explains each flag.
#
# The subtraction is only clean if the two runs differ in NOTHING but the rep count.
# The generated harness supplies a fixed warmup (so the measured ops are already
# JITted); these supply the rest:
#
#   DOTNET_TieredCompilation=0   one JIT tier, compiled on first call, so no tier
#                                transition happens inside the measured loop.
#   DOTNET_gcServer=0            workstation GC; server GC adds per-core heaps and
#                                background threads whose work would land in the delta.
#   DOTNET_GCgen0size            a gen0 big enough that the bounded run never collects.
#   DOTNET_GCHeapHardLimit       caps the GC's address-space reservation.
#
# gen0 is raised above the corelib's value: EpsilonGC has no C# equivalent, so "no
# GC" is approximated by a gen0 the run cannot fill -- and our 32-field message
# allocates far more per op than the corelib's tiny "typical" workload. Same failure
# mode as the java row's heap (see lang/java.sh): too small and Ir stops being
# affine in reps, and lib/callgrind.sh's gate rejects the row.
#
# No footprint row: corelib-cs is a maxspeed target.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2"
    ( cd "$proj" && SOFAB_CS_CORELIB="$corelib" dotnet build -c Release ) >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
bench_cmd_ir() {
    local dll
    dll="$(find "$1" -name 'harness.dll' -path '*Release*' 2>/dev/null | head -1)"
    # --roll-forward Major is a host option (before the dll): the harness targets net9.0
    # and a runner may carry only a newer major runtime. The CLI form is used in
    # addition to the DOTNET_ROLL_FORWARD env because it has higher precedence in
    # hostfxr — it wins even in an environment where the env var is not honored, which
    # is what the bench.yml runners turned out to be.
    echo "dotnet --roll-forward Major ${dll:-$1/bin/Release/net9.0/harness.dll} bench $2"
}

# bench_ir_env <proj> <corelib>
#
# DOTNET_ROLL_FORWARD=Major lets the net9.0 harness run on a newer major runtime.
# The harness targets net9.0 (generators/csharp/project.go), and a framework-dependent
# app does NOT cross a major version by default: with only the .NET 10 runtime present
# — as on the bench.yml runners, whose `dotnet` is 10.x and where setup-dotnet's 9.0
# runtime is not on the resolution path — `dotnet harness.dll` exits 150 ("Framework
# 'Microsoft.NETCore.App' version '9.0.0' not found") before running a single op. Under
# Callgrind that aborted launch still writes a `summary:` line (~2.5M Ir, identical for
# every rep count), so ir_subtract sees a zero slope and the row measures as `!`.
# Major rolls 9.0 up to the runtime that IS there; where 9.0 exists (the devcontainer)
# it is a no-op, so the committed numbers are unchanged.
#
# DOTNET_EnableAVX512F=0 / DOTNET_PreferredVectorBitWidth=256 cap RyuJIT at AVX2 —
# defensively, like the zig row's -Dcpu=baseline (lang/zig.sh). .NET 8+ emits AVX-512
# for the host CPU, the runners (Ice Lake / EPYC) have it, and their Callgrind (3.22)
# is older than the devcontainer's (3.26); once the app actually runs there, 512-bit
# codegen it cannot decode would SIGILL. A no-op on the AVX-512-less devcontainer.
bench_ir_env() {
    echo "DOTNET_gcServer=0 DOTNET_TieredCompilation=0 DOTNET_ReadyToRun=0 \
DOTNET_GCHeapHardLimit=0x100000000 DOTNET_GCgen0size=0x80000000 \
DOTNET_ROLL_FORWARD=Major DOTNET_EnableAVX512F=0 DOTNET_PreferredVectorBitWidth=256"
}
