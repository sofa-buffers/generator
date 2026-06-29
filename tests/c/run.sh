#!/usr/bin/env sh
# Reproducible C conformance harness (M2/M4): generate -> compile -> round-trip
# the generated code against the real corelib-c-cpp.
#
# Usage:
#   tests/c/run.sh [path-to-corelib-c-cpp]
# If no path is given (or $SOFAB_C_CORELIB is unset), the corelib is cloned into
# a temp dir. Requires: go, gcc, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CORELIB="${1:-${SOFAB_C_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-c-cpp"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-c-cpp.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
INC="$CORELIB/src/include"
SRC="$CORELIB/src"
echo "==> corelib: $CORELIB"

echo "==> generating C for examples/messages/example.yaml"
( cd "$ROOT" && go run ./cmd/sbufgen --lang c --in examples/messages/example.yaml --out "$WORK/gen" )

echo "==> compiling generated code + harness against corelib"
gcc -std=c99 -Wall -Wextra \
    -I"$INC" -I"$WORK/gen" \
    "$ROOT/tests/c/example_roundtrip.c" \
    "$WORK"/gen/*.c \
    "$SRC/object.c" "$SRC/ostream.c" "$SRC/istream.c" \
    -o "$WORK/rt"

echo "==> running round-trip"
"$WORK/rt"

echo "==> verifying capability guards fire when a feature is stripped"
if gcc -std=c99 -DSOFAB_DISABLE_SEQUENCE_SUPPORT -I"$INC" -I"$WORK/gen" \
        -c "$WORK"/gen/myfirstmessage.c -o /dev/null 2>/dev/null; then
    echo "FAIL: expected a capability-guard #error with SEQUENCE disabled"
    exit 1
fi
echo "==> guard fired as expected"

echo "==> M3: emit:project -> build harness -> JSON encode/decode round-trip"
cat > "$WORK/proj.yaml" <<YAML
generic: { emit: project, timestamp: false }
targets: { c: { symbol_prefix: sofab_ } }
YAML
( cd "$ROOT" && go run ./cmd/sbufgen --config "$WORK/proj.yaml" --lang c --in examples/messages/example.yaml --out "$WORK/proj" )
make -C "$WORK/proj" SOFAB_C_CORELIB="$CORELIB" >/dev/null
IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"someblobarray":[[1],[2],[3]],"bignum":18446744073709551615,"somestringarray":["a","b","c","d","e"]}'
OUT=$(printf '%s' "$IN" | "$WORK/proj/harness/harness" encode | "$WORK/proj/harness/harness" decode)
echo "$OUT" | grep -q '"bignum":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30' || { echo "FAIL: blob round-trip"; exit 1; }
echo "==> project harness round-trip OK"

echo "==> M4: shared-vector byte-exact conformance + gated Go build tests"
( cd "$ROOT" && SOFAB_C_CORELIB="$CORELIB" go test ./generators/c/ \
    -run 'Conformance|Compiles|Project' -count=1 )

echo "==> corpus: every edge-case definition compiles"
# BIG descriptor profile so wide field ids (up to 2^31-1) fit the descriptor.
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sbufgen --lang c --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    for c in "$WORK"/corpus/"$name"/*.c; do
        gcc -std=c99 -Wall -DSOFAB_OBJECT_DESCR_PROFILE=3 -I"$INC" -I"$WORK/corpus/$name" -c "$c" -o /dev/null \
            || { echo "FAIL: corpus def $name did not compile"; exit 1; }
    done
done
echo "==> corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"

echo "PASS"
