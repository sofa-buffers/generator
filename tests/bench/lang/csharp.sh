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
#
# The path filter MUST be `*/bin/Release/*`, not `*Release*`. A Release build leaves
# FOUR harness.dll under the project — bin/Release/…, obj/Release/…, obj/Release/…/ref
# and obj/Release/…/refint — and only the bin/ one is a runnable, framework-dependent
# app with a runtimeconfig.json beside it. The obj/ copies are reference assemblies
# (metadata only). `find | head -1` returns them in directory order, which is not
# stable across filesystems: the devcontainer happened to yield bin/ first, the
# bench.yml runners yielded obj/…/refint first, and running that one aborts with
# "libhostpolicy.so not found" (~2.5M Ir, identical at every rep count) → zero slope →
# `!`. Anchoring to bin/ picks the single runnable dll regardless of find order.
#
# --roll-forward Major (a host option, before the dll): the harness targets net9.0 and
# a runner may carry only a newer major runtime, which a framework-dependent app will
# not cross by default. No-op where a 9.0 runtime is present (the devcontainer).
bench_cmd_ir() {
    local dll
    dll="$(find "$1" -name 'harness.dll' -path '*/bin/Release/*' 2>/dev/null | head -1)"
    echo "dotnet --roll-forward Major ${dll:-$1/bin/Release/net9.0/harness.dll} bench $2"
}

# bench_ir_env <proj> <corelib>
#
# DOTNET_EnableAVX512F=0 / DOTNET_PreferredVectorBitWidth=256 cap RyuJIT at AVX2 —
# defensively, like the zig row's -Dcpu=baseline (lang/zig.sh). .NET 8+ emits AVX-512
# for the host CPU, the runners (Ice Lake / EPYC) have it, and their Callgrind (3.22)
# is older than the devcontainer's (3.26); once the app runs there, 512-bit codegen it
# cannot decode would SIGILL. A no-op on the AVX-512-less devcontainer.
bench_ir_env() {
    echo "DOTNET_gcServer=0 DOTNET_TieredCompilation=0 DOTNET_ReadyToRun=0 \
DOTNET_GCHeapHardLimit=0x100000000 DOTNET_GCgen0size=0x80000000 \
DOTNET_EnableAVX512F=0 DOTNET_PreferredVectorBitWidth=256"
}
