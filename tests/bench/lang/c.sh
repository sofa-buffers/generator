#!/usr/bin/env bash
# C footprint recipe — see tests/bench/README.md.
#
# generator#589: bench_size used to compile ONLY the generated sources to an
# unlinked object and size that. The reasoning was "everything reachable in
# that object is generated code, so there is no dead weight to strip, unlike
# rust" -- true, but it answers the wrong question. corelib-c-cpp's four .c
# files were never in the object at all, so the row was blind to ~75% of what
# a real c-cpp build ships, and specifically blind to code moving between
# generated code and the corelib (generator#587) in the direction that
# flatters the move: the row goes DOWN while the shipped image is unchanged
# or bigger. See tests/bench/README.md, "What is measured" -- the footprint
# rows are supposed to price the whole package, generated code plus the
# corelib it calls, same as every other row.
#
# bench_size below now links generated sources + corelib-c-cpp's
# object.c/ostream.c/istream.c/utf8.c into one freestanding image and sizes
# THAT, mirroring what rust.sh already had to do (and documents why). This is
# a real methodology change: the `c` row's numbers jump by several KB and are
# not comparable with what was committed before generator#589.
#
# This is now a DIFFERENT question from what corelib-c-cpp/tools/footprint.sh
# answers for the corelib alone (`size` on the unlinked static-library archive,
# no driver, no --gc-sections -- "how big is the corelib on its own"). This
# recipe answers "how big is what a c-cpp consumer ships", which needs a link.

# ---- Ir/op (method: toggle) -------------------------------------------------
#
# Built at -O3 -g -DNDEBUG, matching corelib-c-cpp/bench/CMakeLists.txt, and
# deliberately NOT the -Os of the footprint recipe or the unoptimized build
# conformance uses. ARCHITECTURE §8 makes bounds checks debug-only assertions, so a
# debug build measures code that never ships. -g is free for Ir and lets
# callgrind_annotate attribute later.
#
# Note this overrides the Makefile's own empty `CFLAGS ?=`, which carries no -O at
# all (generators/c/project.go); its warnings ride in WARNFLAGS and are kept.

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
#
# Links the generated sources with corelib-c-cpp's object.c/ostream.c/istream.c/
# utf8.c into one freestanding image, then sizes the linked ELF -- see the
# generator#589 note above for why an unlinked object of the generated sources
# alone is the wrong thing to size.
#
# -nostdlib -nostartfiles: no libc, no crt0. footprint_root.c below defines the
# handful of libc symbols object.c/istream.c actually call (memset/memcpy/
# memcmp/strncmp/strlen) as the plainest byte-loop implementations that could
# work -- a floor on their cost, not an estimate (the same idea as the bump
# allocator in tests/bench/lang/rust.sh). -DNDEBUG voids every assert() call
# (ARCHITECTURE §8: bounds checks are debug-only assertions, so a debug build
# measures code that never ships), which is what keeps __assert_func off that
# list.
#
# footprint.ld is a minimal FLASH+RAM layout for linking only: nothing produced
# here is ever flashed or run, only linked and sized, the same as rust.sh's
# link.x. reset() is the single --gc-sections root, hardcoded to the default
# bench schema/message (tests/bench/rows.json), the same as the cpp and rust
# drivers.
#
# --specs=picolibc.specs (RV32IMC's `flags`, needed at compile time so the
# compiler can find <assert.h>/<string.h>/... -- see
# utils/riscv32/toolchain-riscv32.cmake in corelib-c-cpp) carries its own
# default linker script, which fights footprint.ld over where .bss lands if it
# is still present at link time. link_flags below drops any --specs=* token
# before the link step; -march/-mabi stay, since they select the -lgcc
# multilib.
#
# .data should read 0: the generated code holds only const descriptor tables
# (.rodata, i.e. flash). A nonzero .data means one of them landed in RAM.
bench_size() {
    local cc="$1" size_tool="$2" flags="$3" gen="$4" corelib="$5" work="$6"
    local build="$work/c-fp" hdr
    rm -rf "$build" && mkdir -p "$build" || return 1
    hdr="$(basename "$(find "$gen" -name '*.h' | head -1)")"

    cat > "$build/footprint_root.c" <<EOF
#include "$hdr"
#include <stddef.h>
#include <stdint.h>

/* The plainest byte-loop implementations that could work -- a floor on their
 * cost, not an estimate. Never executed: this image is linked and sized, not
 * flashed. */
void *memset(void *dst, int c, size_t n) {
    unsigned char *d = dst;
    while (n--) { *d++ = (unsigned char)c; }
    return dst;
}
void *memcpy(void *dst, const void *src, size_t n) {
    unsigned char *d = dst;
    const unsigned char *s = src;
    while (n--) { *d++ = *s++; }
    return dst;
}
int memcmp(const void *a, const void *b, size_t n) {
    const unsigned char *pa = a, *pb = b;
    while (n--) {
        if (*pa != *pb) { return *pa - *pb; }
        pa++; pb++;
    }
    return 0;
}
int strncmp(const char *a, const char *b, size_t n) {
    while (n && *a && *a == *b) { a++; b++; n--; }
    return n == 0 ? 0 : (unsigned char)*a - (unsigned char)*b;
}
size_t strlen(const char *s) {
    const char *p = s;
    while (*p) { p++; }
    return (size_t)(p - s);
}

/* The single --gc-sections root: volatile in/out so the optimizer cannot
 * const-fold or elide the encode/decode work; everything reachable from here
 * is what a real firmware consumer pays for. */
void reset(void) {
    static uint8_t buf[MESSAGE_VEHICLETELEMETRY_MAX_SIZE];
    message_VehicleTelemetry_t v;
    message_vehicletelemetry_init(&v);
    v.odometer_m = *(volatile uint64_t *)0x20001000;

    size_t used = 0;
    message_vehicletelemetry_encode(&v, buf, sizeof(buf), &used);

    message_VehicleTelemetry_t d;
    message_vehicletelemetry_init(&d);
    message_vehicletelemetry_decode(&d, buf, used);

    *(volatile uint64_t *)0x20000000 = d.odometer_m ^ (uint64_t)used;
    for (;;) { }
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
    # shellcheck disable=SC2086
    for src in "$gen"/*.c "$corelib"/src/object.c "$corelib"/src/ostream.c \
               "$corelib"/src/istream.c "$corelib"/src/utf8.c "$build/footprint_root.c"; do
        obj="$build/$(basename "${src%.c}").o"
        "$cc" $flags -std=c99 -Os -ffunction-sections -fdata-sections -DNDEBUG \
            -ffile-prefix-map="$gen=/bench" -ffile-prefix-map="$corelib=/bench" \
            -ffile-prefix-map="$build=/bench" \
            -I"$corelib/src/include" -I"$gen" \
            -c "$src" -o "$obj" 2>"$work/c.err" || return 1
        objs+=("$obj")
    done

    local link_flags="" tok
    for tok in $flags; do
        case "$tok" in
            --specs=*) ;;
            *) link_flags="$link_flags $tok" ;;
        esac
    done

    # shellcheck disable=SC2086
    "$cc" $link_flags -nostdlib -nostartfiles -Wl,--gc-sections \
        -Wl,-T,"$build/footprint.ld" "${objs[@]}" -lgcc \
        -o "$build/out.elf" 2>>"$work/c.err" || return 1

    "$size_tool" "$build/out.elf" | awk 'NR==2 {print $1, $2, $3}'
}
