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
# Usage: tests/rust/run.sh [corelib-rs-no-std] [corelib-rs]
#   (or set $SOFAB_RS_CORELIB / $SOFAB_RS_STD_CORELIB)
# Requires: go, cargo, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
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
YAML

IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"bignum":18446744073709551615,"somestringarray":["a","b","c"]}'

# run_variant LABEL CFGBODY CORELIB_PATH
#   CFGBODY - the targets.rust config block contents (e.g. "" or "corelib: rs").
run_variant() {
    label=$1; cfgbody=$2; corelib=$3
    printf 'generic: { emit: project, timestamp: false }\ntargets: { rust: { %s } }\n' "$cfgbody" > "$WORK/cfg-$label.yaml"

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
    echo "$OUT" | grep -q '"bignum":18446744073709551615' || { echo "FAIL: [$label] u64 round-trip"; exit 1; }
    echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: [$label] nested struct round-trip"; exit 1; }
    echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: [$label] blob round-trip"; exit 1; }
    echo "==> [$label] round-trip OK"

    echo "==> [$label] shared-vector byte-exact conformance"
    python3 "$ROOT/tests/rust/check_vectors.py" "$corelib/assets/test_vectors.json" "$WORK/conf-$label"

    echo "==> [$label] corpus: every edge-case definition builds"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
        name=$(basename "$def" .yaml)
        rust_build "$def" "$WORK/corpus-$label/$name"
    done
    echo "==> [$label] corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"
}

# corelib-rs-no-std (default): corpus builds span the feature-subset matrix.
run_variant no-std "" "$NOSTD"

# corelib-rs (std): always-on, no feature flags.
run_variant std "corelib: rs" "$STD"

echo "==> no-std feature-subset smoke: a varint-only schema builds with no features"
printf 'version: 1\nmessages:\n  tiny: { payload: { a: { id: 0, type: i32 }, b: { id: 1, type: u16 }, c: { id: 2, type: boolean } } }\n' > "$WORK/tiny.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-no-std.yaml" --lang rust --in "$WORK/tiny.yaml" --out "$WORK/tiny" )
grep -q 'default-features = false }' "$WORK/tiny/Cargo.toml" || { echo "FAIL: varint-only schema should need no sofab features"; exit 1; }
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/tiny/Cargo.toml"
( cd "$WORK/tiny" && cargo build -q )
echo "==> minimal no-std footprint build OK"

echo "PASS"
