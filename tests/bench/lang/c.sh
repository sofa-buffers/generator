#!/usr/bin/env bash
# C footprint recipe — see tests/bench/README.md.
#
# The generated sources ARE the artifact: `emit: sources` writes <msg>.c/<msg>.h and
# nothing else, so compiling them to an object measures exactly the generator's
# output. Everything reachable in that object is generated code, so there is no
# dead weight to strip and sizing the object directly is accurate (contrast the
# rust recipe, where the archive massively over-counts).
#
# This is what corelib-c-cpp/tools/footprint.sh does for the corelib itself:
# build only the library, `size` it, track .text. That script also reports
# .data/.bss = 0 for the C core; the generated code holds const descriptor tables,
# so watch .data here — a nonzero value means a table landed in RAM, not flash.

# ---- Ir/op (method: toggle) -------------------------------------------------
#
# Built at -O3 -g -DNDEBUG, matching corelib-c-cpp/bench/CMakeLists.txt, and
# deliberately NOT the -Os of the footprint recipe or the unoptimized build
# conformance uses. ARCHITECTURE §8 makes bounds checks debug-only assertions, so a
# debug build measures code that never ships. -g is free for Ir and lets
# callgrind_annotate attribute later.
#
# Note this overrides the Makefile's own `CFLAGS ?= -Wall -Wextra`, which carries no
# -O at all (generators/c/project.go).

# bench_build_ir <gen_proj> <corelib> -> builds harness/harness in place
bench_build_ir() {
    local proj="$1" corelib="$2"
    make -C "$proj" SOFAB_C_CORELIB="$corelib" \
        CFLAGS="-O3 -g -DNDEBUG -Wall -Wextra" >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload> -> echoes the argv to run under callgrind
bench_cmd_ir() {
    echo "$1/harness/harness bench $2"
}

# ---- footprint --------------------------------------------------------------

# bench_size <cc> <size_tool> <arch_flags> <gen_dir> <corelib> <work>
#   echoes "<text> <data> <bss>"
bench_size() {
    local cc="$1" size_tool="$2" flags="$3" gen="$4" corelib="$5" work="$6"
    local obj="$work/c.o"

    # -Os matches the footprint profile's intent (and corelib footprint.sh).
    # -ffile-prefix-map keeps __FILE__/assert strings out of .rodata as a function
    # of the build path: without it, running from a longer directory silently
    # inflates .rodata and reads as a code-size regression.
    # shellcheck disable=SC2086
    "$cc" $flags -std=c99 -Os \
        -ffile-prefix-map="$gen=/bench" \
        -I"$corelib/src/include" -I"$gen" \
        -c "$gen"/*.c -o "$obj" 2>"$work/c.err" || return 1

    "$size_tool" "$obj" | awk 'NR==2 {print $1, $2, $3}'
}
