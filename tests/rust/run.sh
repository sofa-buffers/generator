#!/usr/bin/env sh
# Reproducible Rust conformance harness: generate -> cargo build (vs
# corelib-rs-no-std) -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/rust/run.sh [corelib-rs-no-std]   (or set $SOFAB_RS_CORELIB)
# Requires: go, cargo, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CORELIB="${1:-${SOFAB_RS_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-rs-no-std.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-rs-no-std: $CORELIB"

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project, timestamp: false }
targets: { rust: {} }
YAML
cat > "$WORK/conf.yaml" <<'YAML'
version: 1
messages:
  vecu: { payload: { a: { id: 0, type: u64 } } }
  veci: { payload: { a: { id: 0, type: i64 } } }
  vecf32: { payload: { a: { id: 0, type: fp32 } } }
  vecf64: { payload: { a: { id: 0, type: fp64 } } }
  vecs: { payload: { a: { id: 0, type: string, maxlen: 4096 } } }
YAML

build() {
    ( cd "$ROOT" && go run ./cmd/sbufgen --config "$WORK/cfg.yaml" --lang rust --in "$1" --out "$2" )
    sed -i "s#\${SOFAB_RS_CORELIB}#$CORELIB#" "$2/Cargo.toml"
    ( cd "$2" && cargo build -q )
}

echo "==> generating + building example + conformance crates"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"bignum":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/ex" && printf '%s' "$IN" | cargo run -q -- encode myfirstmessage | cargo run -q -- decode myfirstmessage)
echo "$OUT" | grep -q '"bignum":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/rust/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf"

echo "==> corpus: every edge-case definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    name=$(basename "$def" .yaml)
    build "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"

echo "PASS"
