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
    echo "dotnet ${dll:-$1/bin/Release/net9.0/harness.dll} bench $2"
}

# bench_ir_env <proj> <corelib>
#
# DOTNET_EnableAVX512F=0 / DOTNET_PreferredVectorBitWidth=256 cap RyuJIT at AVX2.
# .NET 8+ emits AVX-512 for the host CPU when it reports the feature, and the CI
# runners (Ice Lake / EPYC) do — but the Callgrind that reads the committed file is
# older there (3.22 vs the devcontainer's 3.26) and cannot decode AVX-512, so the run
# SIGILLs under Valgrind and the row measures as `!`. The devcontainer host has no
# AVX-512, so these knobs are a no-op where results.txt is generated and only pin CI
# to the same AVX2 codegen the committed numbers were read at. Same class of fix as
# the zig row's -Dcpu=baseline (lang/zig.sh); gcc/rustc/go/JVM/V8 don't reach for
# AVX-512 here, which is why the other rows measure clean under the older Valgrind.
bench_ir_env() {
    echo "DOTNET_gcServer=0 DOTNET_TieredCompilation=0 DOTNET_ReadyToRun=0 \
DOTNET_GCHeapHardLimit=0x100000000 DOTNET_GCgen0size=0x80000000 \
DOTNET_EnableAVX512F=0 DOTNET_PreferredVectorBitWidth=256"
}
