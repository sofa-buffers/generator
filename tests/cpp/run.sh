#!/usr/bin/env sh
# Reproducible C++ conformance harness: generate -> build (g++ C++20) ->
# round-trip -> byte-exact shared-vector conformance, run against BOTH C++
# corelibs:
#   - corelib-cpp    (default)        : pure C++20, header-only.
#   - corelib-c-cpp  (corelib: c-cpp) : C++ wrapper over the C library.
# Both expose the same sofab:: interface; the generated code adapts its decode
# (and project Makefile) to the selected corelib.
#
# Usage: tests/cpp/run.sh [corelib-cpp] [corelib-c-cpp]   (or set the env vars)
# Requires: go, g++, gcc, make, python3, git.
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
echo "==> corelib-cpp: $CPP"
echo "==> corelib-c-cpp: $CC"

# Shared definition for the byte-exact shared-vector conformance check.
cat > "$WORK/conf.yaml" <<'YAML'
version: 1
messages:
  vecu: { payload: { a: { id: 0, type: u64 } } }
  veci: { payload: { a: { id: 0, type: i64 } } }
  vecf32: { payload: { a: { id: 0, type: fp32 } } }
  vecf64: { payload: { a: { id: 0, type: fp64 } } }
  vecs: { payload: { a: { id: 0, type: string, maxlen: 4096 } } }
YAML

# Exercises every field-type family (ints, u64, fp, bool, string, enum, bitfield,
# fixed array, blob, string array, blob array, nested struct, union).
IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"someblobarray":[[1],[2],[3]],"bignum":18446744073709551615,"somestringarray":["a","b","c","d","e"]}'

# run_variant LABEL CORELIB INCLUDE MAKEVARS...
#   CORELIB  - "" for pure corelib-cpp, "c-cpp" for the corelib-c-cpp wrapper.
#   INCLUDE  - -I flag for the corpus syntax-only compile.
#   MAKEVARS - vars passed to `make` for the generated project.
run_variant() {
    label=$1; corelib=$2; include=$3; shift 3
    echo "==> [$label] generating + building example project"
    if [ -n "$corelib" ]; then
        printf 'generic: { emit: project, timestamp: false }\ntargets: { cpp: { namespace: sofabuffers, corelib: %s } }\n' "$corelib" > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers, corelib: %s } }\n' "$corelib" > "$WORK/cfg-corpus-$label.yaml"
    else
        printf 'generic: { emit: project, timestamp: false }\ntargets: { cpp: { namespace: sofabuffers } }\n' > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers } }\n' > "$WORK/cfg-corpus-$label.yaml"
    fi
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp --in examples/messages/example.yaml --out "$WORK/ex-$label" )
    make -C "$WORK/ex-$label" "$@" >/dev/null

    echo "==> [$label] JSON encode -> decode round-trip"
    OUT=$(printf '%s' "$IN" | "$WORK/ex-$label/harness/harness" encode myfirstmessage | "$WORK/ex-$label/harness/harness" decode myfirstmessage)
    for chk in \
        '"bignum":18446744073709551615' \
        '"someinteger":-5' \
        '"someenum":33' \
        '"somebitfield":2' \
        '"somearray":\[1,2,3,4,5\]' \
        '"someblob":\[10,20,30\]' \
        '"somestringarray":\["a","b","c","d","e"\]' \
        '"someblobarray":\[\[1\],\[2\],\[3\]\]' \
        '"deepint":-99' \
        '"option1":4242'; do
        echo "$OUT" | grep -q "$chk" || { echo "FAIL: [$label] round-trip missing $chk"; echo "  got: $OUT"; exit 1; }
    done
    echo "==> [$label] round-trip OK"

    echo "==> [$label] shared-vector byte-exact conformance"
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp --in "$WORK/conf.yaml" --out "$WORK/conf-$label" )
    make -C "$WORK/conf-$label" "$@" >/dev/null
    python3 "$ROOT/tests/cpp/check_vectors.py" "$CC/assets/test_vectors.json" "$WORK/conf-$label/harness/harness"

    echo "==> [$label] corpus: every edge-case definition compiles"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
        name=$(basename "$def" .yaml)
        ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-corpus-$label.yaml" --lang cpp --in "$def" --out "$WORK/corpus-$label/$name" >/dev/null )
        for h in "$WORK"/corpus-"$label"/"$name"/*.hpp; do
            g++ -std=c++20 -fsyntax-only -x c++ $include "$h" \
                || { echo "FAIL: [$label] corpus def $name did not compile"; exit 1; }
        done
    done
    echo "==> [$label] corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"
}

# Pure C++20 corelib-cpp (default).
run_variant cpp "" "-I$CPP/include" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC"

# C++ wrapper over the C library, corelib-c-cpp (corelib: c-cpp). Only needs
# SOFAB_C_DIR; the generated Makefile compiles + links its C sources.
run_variant c-cpp "c-cpp" "-I$CC/src/include" SOFAB_C_DIR="$CC"

echo "PASS"
