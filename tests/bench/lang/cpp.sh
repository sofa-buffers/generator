#!/usr/bin/env bash
# C++ footprint recipe — see tests/bench/README.md.
#
# The C++ backend emits a header-only <msg>.hpp: an empty TU that merely #includes
# it compiles to text=0, because nothing is instantiated until something calls the
# API. So unlike C, this recipe needs a driver TU.
#
# The driver deliberately calls ONLY the heap-free path — encodeTo() + try_decode().
# The convenience encode() returns std::vector and would drag the allocator in,
# which is not what a footprint consumer pays. Keep it that way: an accidental
# encode() call here would silently add heap machinery to every reported number.
#
# Note this exceeds the corelib convention: corelib-c-cpp/tools/footprint.sh sets
# -DSOFAB_ENABLE_CPP=OFF and never measures C++ footprint on any arch.
#
# Arch coverage is ARM-only: neither riscv64-unknown-elf nor avr ships a
# bare-metal C++ standard library, and the generated header needs <cstdint>/<array>/
# <span>. On ARM this requires libstdc++-arm-none-eabi-newlib to be installed.

# ---- Ir/op (method: toggle) -------------------------------------------------
#
# Built at -O3 -g -DNDEBUG, matching corelib-c-cpp/bench/CMakeLists.txt, overriding
# the Makefile's emitted `CXXFLAGS ?= -O2 -Wall`.
#
# C++ is the one row needing TWO corelib checkouts: SOFAB_CPP_DIR for the C++
# corelib and SOFAB_C_DIR for the JSON test helper the harness links. rows.json
# names one corelib per row, so the other is taken from the environment (run.sh
# exports both) and falls back to the row's own corelib — which is correct for the
# cpp-c-cpp row, where they are the same checkout.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2"
    local cpp_dir="${SOFAB_CPP_DIR:-$corelib}" c_dir="${SOFAB_C_DIR:-$corelib}"
    make -C "$proj" SOFAB_CPP_DIR="$cpp_dir" SOFAB_C_DIR="$c_dir" \
        CXXFLAGS="-O3 -g -DNDEBUG -Wall" >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>
bench_cmd_ir() {
    echo "$1/harness/harness bench $2"
}

# ---- footprint --------------------------------------------------------------

# bench_size <cxx> <size_tool> <arch_flags> <gen_dir> <corelib> <work>
#   echoes "<text> <data> <bss>"
bench_size() {
    local cxx="$1" size_tool="$2" flags="$3" gen="$4" corelib="$5" work="$6"
    local obj="$work/cpp.o" drv="$work/driver.cpp"
    local hdr
    hdr="$(basename "$(find "$gen" -name '*.hpp' | head -1)")"

    cat > "$drv" <<EOF
#include "$hdr"
// Referenced from nowhere, but extern "C" and non-static so the compiler must
// emit it — which is what instantiates the generated encode/decode paths.
extern "C" unsigned sofab_footprint_probe(unsigned char *buf, unsigned cap) {
    message::VehicleTelemetry v;
    unsigned n = (unsigned)v.encodeTo(buf, cap);
    message::VehicleTelemetry out;
    (void)message::VehicleTelemetry::try_decode(buf, n, out);
    return n + (unsigned)out.odometer_m;
}
EOF

    # -Os -fno-exceptions -fno-rtti mirror the flags the cpp backend itself emits
    # for the c-cpp profile (generators/cpp/project.go).
    # shellcheck disable=SC2086
    "$cxx" $flags -std=c++20 -Os -fno-exceptions -fno-rtti \
        -ffile-prefix-map="$work=/bench" -ffile-prefix-map="$gen=/bench" \
        -I"$corelib/src/include" -I"$gen" \
        -c "$drv" -o "$obj" 2>"$work/cpp.err" || return 1

    "$size_tool" "$obj" | awk 'NR==2 {print $1, $2, $3}'
}
