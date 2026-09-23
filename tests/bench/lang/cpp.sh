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
#
# generator#589: bench_size below only ever runs for the cpp-c-cpp and
# cpp-c-cpp-dyn rows -- rows.json gives every cpp-cpp* row an empty `archs`, so
# run.sh never calls into the size step for them (corelib-cpp is header-only;
# everything used arrives through the driver, which is why those rows were
# never affected by this bug). It used to compile ONLY the driver TU to an
# unlinked object, which never pulled corelib-c-cpp's four .c files in at all --
# the same undercount c.sh had, and the same blind spot for code moving between
# generated code and the corelib (generator#587). bench_size now links the
# driver against object.c/ostream.c/istream.c/utf8.c and sizes the linked
# image, mirroring c.sh and rust.sh. Both cpp-c-cpp rows' numbers jump by
# several KB and are not comparable with what was committed before
# generator#589.

# ---- Ir/op (method: toggle) -------------------------------------------------
#
# Built at -O3 -g -DNDEBUG, matching corelib-c-cpp/bench/CMakeLists.txt, overriding
# the Makefile's emitted `CXXFLAGS ?= -O2` (its warnings ride in WARNFLAGS).
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
#
# Only ever called for cpp-c-cpp / cpp-c-cpp-dyn (see the generator#589 note
# above), so unlike the Ir/op build there is exactly one corelib here and it
# always has the four .c files this links against.
#
# -nostdlib -nostartfiles: no libc, no libstdc++ runtime. The driver below
# supplies the handful of freestanding symbols that are reachable even from the
# heap-free path: memset/memcpy/memmove/memcmp/strncmp/strlen (object.c/
# istream.c), a trivial operator new/delete pair (a polymorphic base's virtual
# destructor thunk references operator delete unconditionally, whether or not
# anything actually deletes through it), and the Itanium/ARM guard functions
# (a function-local static inside the generated decode path). -DNDEBUG voids
# every assert() call, and -fno-exceptions/-fno-rtti mirror the c-cpp profile's
# own flags (generators/cpp/project.go) -- but <vector>'s bounds/allocation
# failure paths still compile calls to std::__throw_*() even with exceptions
# off (guarded by __cpp_exceptions, not by whether this TU itself throws), so
# the dyn row's stubs for those turn an unreachable failure into an infinite
# loop instead of an undefined reference.
#
# The bump allocator (operator new) never frees -- its .text is a floor, not an
# estimate, the same idea as tests/bench/lang/rust.sh's. It is reachable, and
# therefore present, in BOTH rows (the vtable reference to operator delete
# alone forces it in); only cpp-c-cpp-dyn's default member initializers
# (non-empty std::vector fields) actually call it at what-would-be runtime.
# Read the pair, not either row alone (rows.json's own note on -dyn).
#
# footprint.ld / --gc-sections / .data=0 expectation: see c.sh, which this
# mirrors. RV32IMC's --specs=picolibc.specs special-case does not apply here --
# ARM is the only arch this row ever runs on.
bench_size() {
    local cxx="$1" size_tool="$2" flags="$3" gen="$4" corelib="$5" work="$6"
    local cc="${cxx/g++/gcc}"
    local build="$work/cpp-fp" hdr
    rm -rf "$build" && mkdir -p "$build" || return 1
    hdr="$(basename "$(find "$gen" -name '*.hpp' | head -1)")"

    cat > "$build/driver.cpp" <<EOF
#include "$hdr"
#include <cstddef>
#include <cstdint>

/* The plainest byte-loop implementations that could work -- a floor on their
 * cost, not an estimate. Never executed: this image is linked and sized, not
 * flashed. */
extern "C" void *memset(void *dst, int c, std::size_t n) {
    unsigned char *d = (unsigned char *)dst;
    while (n--) { *d++ = (unsigned char)c; }
    return dst;
}
extern "C" void *memmove(void *dst, const void *src, std::size_t n) {
    unsigned char *d = (unsigned char *)dst;
    const unsigned char *s = (const unsigned char *)src;
    if (d < s) { while (n--) { *d++ = *s++; } }
    else { d += n; s += n; while (n--) { *--d = *--s; } }
    return dst;
}
extern "C" void *memcpy(void *dst, const void *src, std::size_t n) {
    unsigned char *d = (unsigned char *)dst;
    const unsigned char *s = (const unsigned char *)src;
    while (n--) { *d++ = *s++; }
    return dst;
}
extern "C" int memcmp(const void *a, const void *b, std::size_t n) {
    const unsigned char *pa = (const unsigned char *)a, *pb = (const unsigned char *)b;
    while (n--) {
        if (*pa != *pb) { return *pa - *pb; }
        pa++; pb++;
    }
    return 0;
}
extern "C" int strncmp(const char *a, const char *b, std::size_t n) {
    while (n && *a && *a == *b) { a++; b++; n--; }
    return n == 0 ? 0 : (unsigned char)*a - (unsigned char)*b;
}
extern "C" std::size_t strlen(const char *s) {
    const char *p = s;
    while (*p) { p++; }
    return (std::size_t)(p - s);
}

/* A bump allocator: never frees, so its .text is a floor, not an estimate (see
 * the bench_size comment above for why this is reachable in both rows). */
namespace {
alignas(8) unsigned char arena[4096];
std::size_t arena_used = 0;
}
void *operator new(std::size_t n) {
    void *p = arena + arena_used;
    arena_used += (n + 7) & ~std::size_t(7);
    return p;
}
void operator delete(void *) noexcept {}
void operator delete(void *, std::size_t) noexcept {}
void *operator new[](std::size_t n) { return operator new(n); }
void operator delete[](void *) noexcept {}
void operator delete[](void *, std::size_t) noexcept {}

/* ARM EABI guard variable: first byte is the "initialized" flag. */
extern "C" int __cxa_guard_acquire(int *g) { return !*(char *)g; }
extern "C" void __cxa_guard_release(int *g) { *(char *)g = 1; }
extern "C" void __cxa_guard_abort(int *) {}

/* libstdc++'s <vector> compiles these calls even under -fno-exceptions
 * (guarded by __cpp_exceptions, not by whether this TU itself throws). Only a
 * genuine bound/allocation failure reaches them, which this arena cannot
 * recover from either, so looping is the same "never coming back" outcome
 * std::terminate() would have been. */
namespace std {
[[noreturn]] void __throw_length_error(const char *) { for (;;) {} }
[[noreturn]] void __throw_bad_alloc() { for (;;) {} }
[[noreturn]] void __throw_out_of_range(const char *) { for (;;) {} }
[[noreturn]] void __throw_out_of_range_fmt(const char *, ...) { for (;;) {} }
[[noreturn]] void __throw_logic_error(const char *) { for (;;) {} }
[[noreturn]] void __throw_bad_array_new_length() { for (;;) {} }
}

// The single --gc-sections root, hardcoded to the default bench schema/message
// (tests/bench/rows.json). Volatile in/out so the optimizer cannot const-fold
// or elide the encode/decode work; everything reachable from here is what a
// real firmware consumer pays for.
extern "C" void reset() {
    static std::uint8_t buf[message::VehicleTelemetry::_maxSize];
    message::VehicleTelemetry v;
    v.odometer_m = *(volatile std::uint64_t *)0x20001000;
    std::size_t n = v.encodeTo(buf, sizeof(buf));

    message::VehicleTelemetry out;
    (void)message::VehicleTelemetry::try_decode(buf, n, out);

    *(volatile std::uint64_t *)0x20000000 = out.odometer_m ^ (std::uint64_t)n;
    for (;;) {}
}
EOF

    cat > "$build/footprint.ld" <<'EOF'
MEMORY { FLASH (rx): ORIGIN = 0, LENGTH = 256K  RAM (rwx): ORIGIN = 0x20000000, LENGTH = 64K }
ENTRY(reset)
SECTIONS {
  .text : { KEEP(*(.vectors)) *(.text .text.*) *(.rodata .rodata.*) } > FLASH
  .data : { *(.data .data.*) } > RAM AT> FLASH
  .bss  : { *(.bss .bss.*) }  > RAM
  /DISCARD/ : { *(.ARM.exidx*) *(.comment) }
}
EOF

    local objs=() src obj
    # -Os -fno-exceptions -fno-rtti mirror the flags the cpp backend itself emits
    # for the c-cpp profile (generators/cpp/project.go).
    # shellcheck disable=SC2086
    "$cxx" $flags -std=c++20 -Os -ffunction-sections -fdata-sections \
        -fno-exceptions -fno-rtti -DNDEBUG \
        -ffile-prefix-map="$build=/bench" -ffile-prefix-map="$gen=/bench" \
        -I"$corelib/src/include" -I"$gen" \
        -c "$build/driver.cpp" -o "$build/driver.o" 2>"$work/cpp.err" || return 1
    objs+=("$build/driver.o")

    for src in "$corelib"/src/object.c "$corelib"/src/ostream.c \
               "$corelib"/src/istream.c "$corelib"/src/utf8.c; do
        obj="$build/$(basename "${src%.c}").o"
        # shellcheck disable=SC2086
        "$cc" $flags -std=c99 -Os -ffunction-sections -fdata-sections -DNDEBUG \
            -ffile-prefix-map="$corelib=/bench" \
            -I"$corelib/src/include" \
            -c "$src" -o "$obj" 2>>"$work/cpp.err" || return 1
        objs+=("$obj")
    done

    # shellcheck disable=SC2086
    "$cxx" $flags -nostdlib -nostartfiles -fno-exceptions -fno-rtti \
        -Wl,--gc-sections -Wl,-T,"$build/footprint.ld" "${objs[@]}" -lgcc \
        -o "$build/out.elf" 2>>"$work/cpp.err" || return 1

    "$size_tool" "$build/out.elf" | awk 'NR==2 {print $1, $2, $3}'
}
