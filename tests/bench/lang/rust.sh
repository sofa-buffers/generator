#!/usr/bin/env bash
# Rust footprint recipe — see tests/bench/README.md.
#
# Ported from corelib-rs-no-std/tools/footprint.sh, including the reason it works
# the way it does. Quoting that script:
#
#   "A bare staticlib archive is NOT dead-stripped, so measuring it directly
#    massively over-counts; the link step is what makes the code numbers
#    meaningful."
#
# That is not a small effect: sizing the generated crate's rlib reports ~14 KB of
# .text on thumbv6m, against ~1.6 KB for the equivalent C object. The difference is
# unreachable monomorphisations and panic machinery that --gc-sections strips. So
# unlike the c/cpp recipes (which size an object directly), rust MUST link.
#
# The shape, mirroring the corelib script:
#   * a throwaway #![no_std] staticlib depending on the generated crate,
#   * ENTRY(reset) -> a reset() that volatile-reads its inputs, calls
#     marshal + try_decode, and volatile-writes the result. The volatile access is
#     what stops the optimizer folding the whole thing away, and reset() is the
#     single root --gc-sections keeps reachable code from,
#   * link with rust-lld --gc-sections against a minimal link.x,
#   * flash = .text + .data of the linked image (Berkeley `size`).
#
# Prereq: rustup target add thumbv6m-none-eabi (+ llvm-tools-preview for llvm-size).

# ---- Ir/op (method: toggle) -------------------------------------------------
#
# The std harness bin carries the bench verb. Built --release (the emitted profile),
# not the debug build conformance uses: ARCHITECTURE §8 makes bounds checks
# debug-only assertions, so a debug build measures code that never ships.
#
# codegen-units=1 is a deliberate deviation from the emitted default of 16: CGU
# partitioning (and therefore inlining) shifts with unrelated module-structure
# changes, which is noise rather than generator signal.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2"
    sed -i "s#\${SOFAB_RS_CORELIB}#$corelib#" "$proj/Cargo.toml" 2>/dev/null || true
    ( cd "$proj" && RUSTFLAGS="--remap-path-prefix=$proj=/bench -C codegen-units=1" \
        cargo build --release --quiet ) >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>
bench_cmd_ir() {
    echo "$1/target/release/harness bench $2"
}

# ---- footprint --------------------------------------------------------------

# bench_size <rust_target> <gen_proj> <corelib> <work>
#   echoes "<text> <data> <bss>"
bench_size_rust() {
    local target="$1" gen="$2" corelib="$3" work="$4"
    local crate="$work/fp"
    local sysroot host bindir
    sysroot="$(rustc --print sysroot)"
    host="$(rustc -vV | sed -n 's/host: //p')"
    bindir="$sysroot/lib/rustlib/$host/bin"

    # Point the generated crate at the corelib checkout (same placeholder the
    # conformance harness substitutes).
    sed -i "s#\${SOFAB_RS_CORELIB}#$corelib#" "$gen/Cargo.toml"

    # Reuse the generated crate's own `sofab = {...}` dependency line verbatim: it
    # carries the exact corelib package name, path and feature set the generated
    # code requires (sofab::require!(...) fails the build otherwise). Copying it
    # keeps this driver from drifting as the backend's feature selection changes.
    local sofab_dep
    sofab_dep="$(grep -m1 '^sofab = ' "$gen/Cargo.toml")"

    mkdir -p "$crate/src"
    cat > "$crate/Cargo.toml" <<EOF
[package]
name = "sofab_gen_footprint"
version = "0.0.0"
edition = "2021"
[lib]
crate-type = ["staticlib"]
[dependencies]
gen = { package = "sofabuffers-generated", path = "$gen", default-features = false }
$sofab_dep
[profile.release]
opt-level = "z"
lto = true
codegen-units = 1
panic = "abort"
strip = true
EOF

    cat > "$crate/src/lib.rs" <<'EOF'
#![no_std]
use core::panic::PanicInfo;
use gen::VehicleTelemetry;
use sofab::OStream;

#[panic_handler]
fn ph(_: &PanicInfo) -> ! {
    loop {}
}

// The single --gc-sections root. Volatile in/out so the optimizer cannot const-fold
// or elide the encode/decode work; everything reachable from here is what a real
// firmware consumer pays for.
#[no_mangle]
pub extern "C" fn reset() -> ! {
    let mut buf = [0u8; VehicleTelemetry::MAX_SIZE];
    let mut v = VehicleTelemetry::default();
    v.odometer_m = unsafe { core::ptr::read_volatile(0x2000_1000 as *const u64) };
    let n = {
        let mut os = OStream::new(&mut buf);
        v.marshal(&mut os);
        os.bytes_used()
    };
    let acc = match VehicleTelemetry::try_decode(&buf[..n]) {
        Ok(d) => d.odometer_m,
        Err(_) => 0,
    };
    unsafe { core::ptr::write_volatile(0x2000_0000 as *mut u64, acc ^ n as u64) };
    loop {}
}
EOF

    cat > "$crate/link.x" <<'EOF'
MEMORY { FLASH (rx): ORIGIN = 0, LENGTH = 256K  RAM (rwx): ORIGIN = 0x20000000, LENGTH = 64K }
ENTRY(reset)
SECTIONS {
  .text : { KEEP(*(.vectors)) *(.text .text.*) *(.rodata .rodata.*) } > FLASH
  .data : { *(.data .data.*) } > RAM AT> FLASH
  .bss  : { *(.bss .bss.*) }  > RAM
  /DISCARD/ : { *(.ARM.exidx*) *(.comment) }
}
EOF

    (
        cd "$crate" || exit 1
        # --remap-path-prefix: panic locations otherwise embed the build path into
        # .rodata, so a longer work dir silently inflates the number.
        RUSTFLAGS="--remap-path-prefix=$work=/bench --remap-path-prefix=$gen=/bench -C codegen-units=1" \
            cargo build --release --target "$target" --quiet 2>"$work/rust.err" || exit 1
        "$bindir/rust-lld" -flavor gnu -T link.x --gc-sections -o out.elf \
            --whole-archive "target/$target/release/libsofab_gen_footprint.a" 2>>"$work/rust.err" || exit 1
    ) || return 1

    "$bindir/llvm-size" "$crate/out.elf" | awk 'NR==2 {print $1, $2, $3}'
}
