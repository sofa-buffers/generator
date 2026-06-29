#!/usr/bin/env sh
# Reproducible C++ conformance harness: generate -> build (g++ C++20 vs
# corelib-cpp) -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/cpp/run.sh [corelib-cpp] [corelib-c-cpp]   (or set the env vars)
# Requires: go, g++, make, python3, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CPP="${1:-${SOFAB_CPP_DIR:-}}"
CC="${2:-${SOFAB_C_DIR:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CPP" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-cpp.git "$WORK/cpp" >/dev/null 2>&1
    CPP="$WORK/cpp"
fi
if [ -z "$CC" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-c-cpp.git "$WORK/c" >/dev/null 2>&1
    CC="$WORK/c"
fi
echo "==> corelib-cpp: $CPP ; corelib-c-cpp (json reader): $CC"

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project, timestamp: false }
targets: { cpp: { namespace: sofabuffers } }
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

echo "==> generating + building example project"
( cd "$ROOT" && go run ./cmd/sbufgen --config "$WORK/cfg.yaml" --lang cpp --in examples/messages/example.yaml --out "$WORK/ex" )
make -C "$WORK/ex" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null

echo "==> JSON encode -> decode round-trip"
IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"bignum":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(printf '%s' "$IN" | "$WORK/ex/harness/harness" encode myfirstmessage | "$WORK/ex/harness/harness" decode myfirstmessage)
echo "$OUT" | grep -q '"bignum":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: blob round-trip"; exit 1; }
echo "==> round-trip OK"

echo "==> shared-vector byte-exact conformance"
( cd "$ROOT" && go run ./cmd/sbufgen --config "$WORK/cfg.yaml" --lang cpp --in "$WORK/conf.yaml" --out "$WORK/conf" )
make -C "$WORK/conf" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
python3 "$ROOT/tests/cpp/check_vectors.py" "$CC/assets/test_vectors.json" "$WORK/conf/harness/harness"

echo "==> corpus: every edge-case definition compiles"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sbufgen --lang cpp --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    for h in "$WORK"/corpus/"$name"/*.hpp; do
        g++ -std=c++20 -fsyntax-only -x c++ -I"$CPP/include" "$h" \
            || { echo "FAIL: corpus def $name did not compile"; exit 1; }
    done
done
echo "==> corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"

echo "PASS"
