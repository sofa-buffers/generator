#!/usr/bin/env sh
# Reproducible Rust conformance harness: generate -> cargo build -> round-trip ->
# byte-exact shared-vector conformance, run against BOTH Rust corelibs:
#   - corelib-rs-no-std (default)      : #![no_std], heap-free, Cargo feature
#     flags to shrink the binary. The generated crate turns every feature OFF and
#     re-enables only the wire types each schema uses, so building the corpus
#     exercises the full no-std feature-subset matrix (varint-only up to all
#     features; 32-bit value type when no u64/i64 is present).
#   - corelib-rs       (corelib: rs)   : std, high-throughput, every wire type
#     always compiled in (no feature flags, no require! guard).
# Both expose the same sofab:: interface and identical wire output.
#
# Usage: tests/conformance/rust/run.sh [corelib-rs-no-std] [corelib-rs]
#   (or set $SOFAB_RS_CORELIB / $SOFAB_RS_STD_CORELIB)
# Requires: go, cargo, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
NOSTD="${1:-${SOFAB_RS_CORELIB:-}}"
STD="${2:-${SOFAB_RS_STD_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$NOSTD" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-rs-no-std.git "$WORK/nostd" >/dev/null 2>&1
    NOSTD="$WORK/nostd"
fi
if [ -z "$STD" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-rs.git "$WORK/std" >/dev/null 2>&1
    STD="$WORK/std"
fi
echo "==> corelib-rs-no-std: $NOSTD"
echo "==> corelib-rs: $STD"

cat > "$WORK/conf.yaml" <<'YAML'
version: 1
messages:
  vecu: { payload: { a: { id: 0, type: u64 } } }
  veci: { payload: { a: { id: 0, type: i64 } } }
  vecf32: { payload: { a: { id: 0, type: fp32 } } }
  vecf64: { payload: { a: { id: 0, type: fp64 } } }
  vecs: { payload: { a: { id: 0, type: string, maxlen: 4096 } } }
  vecsa: { payload: { a: { id: 0, type: array, items: { type: string, count: 8, maxlen: 16 } } } }
YAML

IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'

# run_variant LABEL CFGBODY CORELIB_PATH
#   CFGBODY - the targets.rust config block contents (e.g. "" or "corelib: rs").
run_variant() {
    label=$1; cfgbody=$2; corelib=$3
    printf 'generic: { emit: project }\ntargets: { rust: { %s } }\n' "$cfgbody" > "$WORK/cfg-$label.yaml"

    rust_build() {  # def-or-yaml out-dir
        ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang rust --in "$1" --out "$2" )
        sed -i "s#\${SOFAB_RS_CORELIB}#$corelib#" "$2/Cargo.toml"
        ( cd "$2" && cargo build -q )
    }

    echo "==> [$label] generating + building example + conformance crates"
    rust_build "$ROOT/examples/messages/example.yaml" "$WORK/ex-$label"
    rust_build "$WORK/conf.yaml" "$WORK/conf-$label"

    echo "==> [$label] JSON encode -> decode round-trip"
    OUT=$(cd "$WORK/ex-$label" && printf '%s' "$IN" | cargo run -q -- encode myfirstmessage | cargo run -q -- decode myfirstmessage)
    echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: [$label] u64 round-trip"; exit 1; }
    echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: [$label] nested struct round-trip"; exit 1; }
    echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: [$label] blob round-trip"; exit 1; }
    echo "==> [$label] round-trip OK"

    # Over-count scalar array (generator#100): someuintarray declares count: 4
    # (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
    # INVALID per MESSAGE_SPEC 3+7 (try_decode rejects, harness exits non-zero);
    # exactly 4 still decode.
    echo "==> [$label] over-count scalar array must reject (generator#100)"
    printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
    printf '\173\004\001\002\003\004' > "$WORK/control.bin"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-count scalar array (5 > count 4) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/control.bin" >/dev/null) || { echo "FAIL: [$label] control (count == 4) must decode"; exit 1; }
    echo "==> [$label] over-count reject OK"

    # Over-index wrapper array (generator#142, #149): the sequence-form analogue of
    # the over-count scalar reject above. somestringarray (id 18) declares count: 5;
    # a well-formed string element at wire index 5 (>= N) is INVALID per MESSAGE_SPEC
    # S5.1/S7 -- the generated visitor sets self.inv (surfaced as Error::InvalidMsg)
    # before the Vec grows (which also bounds an over-index amplification DoS). Both
    # profiles reject: on no_std the over-index guard fires ahead of the heapless
    # capacity drop (issue #126), so the outcome is INVALID, not a silent drop --
    # the fixed-capacity convergence MESSAGE_SPEC S7.1 requires (issue #149 /
    # F-0013). Wire: 96 01 (sequence_begin id 18) 2a (string id 5) 0a 78 (fixlen
    # "x") 07 (sequence_end).
    printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
    printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
    echo "==> [$label] over-index wrapper array must reject (generator#142, #149)"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null) || { echo "FAIL: [$label] control (index 4 < 5) must decode"; exit 1; }
    echo "==> [$label] over-index reject OK"

    # Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12)
    # declares maxlen: 16; a 17-byte blob exceeds it -> INVALID, never truncated.
    # Wire: 62 (blob id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes;
    # control is 16 bytes. Both profiles reject: the generated maxlen guard sets
    # self.inv on std AND no_std (the no_std guard supersedes the heapless
    # BufferFull path, so the outcome is INVALID, not a capacity error).
    echo "==> [$label] over-maxlen string/blob must reject (Option B, S7.1)"
    printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
    printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null) || { echo "FAIL: [$label] control (16 == maxlen) must decode"; exit 1; }
    echo "==> [$label] over-maxlen reject OK"

    echo "==> [$label] shared-vector byte-exact conformance"
    python3 "$ROOT/tests/conformance/rust/check_vectors.py" "$corelib/assets/test_vectors.json" "$WORK/conf-$label"

    echo "==> [$label] corpus + realworld: every definition builds"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
        name=$(basename "$def" .yaml)
        rust_build "$def" "$WORK/corpus-$label/$name"
    done
    echo "==> [$label] corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"
}

# corelib-rs (std, the default): always-on, no feature flags.
run_variant rs "corelib: rs" "$STD"

# Receiver-side decode limits (generator#102), std corelib only (the no_std
# profile is statically bounded, the keys are inert there): an unbounded u64
# array (id 0 -> header 0x03 = 0<<3 | unsigned-array) under
# max_dyn_array_count: 4. 5 wire elements MUST fail try_decode with
# LimitExceeded (harness exits non-zero); exactly 4 still decode; and the same
# oversized bytes MUST decode against a no-limits project (unset = unlimited).
echo "==> [rs] receiver-side decode limits (generator#102)"
printf 'version: 1\nmessages:\n  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }\n' > "$WORK/dyn.yaml"
printf 'generic: { emit: project, max_dyn_array_count: 4 }\ntargets: { rust: { corelib: rs } }\n' > "$WORK/cfg-lim.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-lim.yaml" --lang rust --in "$WORK/dyn.yaml" --out "$WORK/lim" )
sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$WORK/lim/Cargo.toml"
( cd "$WORK/lim" && cargo build -q )
printf '\003\005\001\002\003\004\005' > "$WORK/lim-over.bin"
printf '\003\004\001\002\003\004' > "$WORK/lim-ok.bin"
if (cd "$WORK/lim" && cargo run -q -- decode dyn < "$WORK/lim-over.bin" >/dev/null 2>&1); then
    echo "FAIL: 5 elements > max_dyn_array_count 4 must reject (LimitExceeded)"; exit 1
fi
(cd "$WORK/lim" && cargo run -q -- decode dyn < "$WORK/lim-ok.bin" >/dev/null) || { echo "FAIL: 4 elements == cap must decode"; exit 1; }
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs } }\n' > "$WORK/cfg-nolim.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-nolim.yaml" --lang rust --in "$WORK/dyn.yaml" --out "$WORK/nolim" )
sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$WORK/nolim/Cargo.toml"
( cd "$WORK/nolim" && cargo build -q )
(cd "$WORK/nolim" && cargo run -q -- decode dyn < "$WORK/lim-over.bin" >/dev/null) || { echo "FAIL: no-limits project must decode oversized input"; exit 1; }
echo "==> [rs] decode limits OK"

# corelib-rs-no-std is now the genuinely #![no_std], heap-free profile (heapless
# fixed-capacity fields). The rich example.yaml has an unbounded field (somemap),
# so it needs allow_dynamic: true to keep an alloc fallback for that one field —
# the Rust analog of the c-cpp allow_dynamic variant. The corpus spans the
# feature-subset matrix under the same config.
run_variant no-std "corelib: rs-no-std, allow_dynamic: true" "$NOSTD"

# The point of the no_std profile is a crate that builds as #![no_std] and
# heap-free. A bin cannot be no_std on a hosted target, so prove it on the lib
# target: `cargo build --lib --no-default-features` drops serde/std and compiles
# the pure heapless (+ optional alloc) crate. Exercise BOTH allow_dynamic configs,
# mirroring the c-cpp bounded-vs-allow_dynamic split.
echo "==> no_std lib builds heap-free (--lib --no-default-features), allow_dynamic on AND off"

# (a) allow_dynamic: true — example.yaml keeps an alloc fallback for somemap, so
# the crate pulls `extern crate alloc` yet still compiles as #![no_std] on a lib.
grep -q 'extern crate alloc' "$WORK/ex-no-std/src/lib.rs" || { echo "FAIL: allow_dynamic crate should pull extern crate alloc"; exit 1; }
( cd "$WORK/ex-no-std" && cargo build -q --lib --no-default-features )
echo "==> [allow_dynamic=true] no_std lib (heapless + alloc fallback) builds"

# (b) allow_dynamic: false (default) — a fully bounded schema must lower to pure
# heapless with NO allocator at all (no `extern crate alloc`), and an unbounded
# field must instead be a hard generation error.
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs-no-std } }\n' > "$WORK/cfg-strict.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-strict.yaml" --lang rust --in "$WORK/conf.yaml" --out "$WORK/strict" )
if grep -q 'extern crate alloc' "$WORK/strict/src/lib.rs"; then echo "FAIL: strict (bounded, no allow_dynamic) crate must not pull alloc"; exit 1; fi
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/strict/Cargo.toml"
( cd "$WORK/strict" && cargo build -q --lib --no-default-features )
echo "==> [allow_dynamic=false] strict no_std lib (pure heapless, no alloc) builds"

# an unbounded field without allow_dynamic is rejected, not silently heaped.
printf 'version: 1\nmessages:\n  m: { payload: { s: { id: 0, type: string } } }\n' > "$WORK/unbounded.yaml"
if ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-strict.yaml" --lang rust --in "$WORK/unbounded.yaml" --out "$WORK/unbounded" 2>/dev/null ); then
    echo "FAIL: unbounded field under no_std without allow_dynamic should error"; exit 1
fi
echo "==> [allow_dynamic=false] unbounded field is correctly rejected"

echo "==> no-std feature-subset smoke: a varint-only schema builds with no features"
printf 'version: 1\nmessages:\n  tiny: { payload: { a: { id: 0, type: i32 }, b: { id: 1, type: u16 }, c: { id: 2, type: boolean } } }\n' > "$WORK/tiny.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-no-std.yaml" --lang rust --in "$WORK/tiny.yaml" --out "$WORK/tiny" )
grep -q 'default-features = false' "$WORK/tiny/Cargo.toml" || { echo "FAIL: varint-only schema should need no sofab features"; exit 1; }
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/tiny/Cargo.toml"
( cd "$WORK/tiny" && cargo build -q )
echo "==> minimal no-std footprint build OK"

echo "PASS"
