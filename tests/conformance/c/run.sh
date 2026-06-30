#!/usr/bin/env sh
# Reproducible C conformance harness (M2/M4): generate -> compile -> round-trip
# the generated code against the real corelib-c-cpp.
#
# Usage:
#   tests/conformance/c/run.sh [path-to-corelib-c-cpp]
# If no path is given (or $SOFAB_C_CORELIB is unset), the corelib is cloned into
# a temp dir. Requires: go, gcc, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
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
( cd "$ROOT" && go run ./cmd/sofabgen --lang c --in examples/messages/example.yaml --out "$WORK/gen" )

echo "==> compiling generated code + harness against corelib"
gcc -std=c99 -Wall -Wextra \
    -I"$INC" -I"$WORK/gen" \
    "$ROOT/tests/conformance/c/example_roundtrip.c" \
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
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/proj.yaml" --lang c --in examples/messages/example.yaml --out "$WORK/proj" )
make -C "$WORK/proj" SOFAB_C_CORELIB="$CORELIB" >/dev/null
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someblobarray":[[1],[2],[3]],"someu64":18446744073709551615,"somestringarray":["a","b","c","d","e"]}'
OUT=$(printf '%s' "$IN" | "$WORK/proj/harness/harness" encode | "$WORK/proj/harness/harness" decode)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30' || { echo "FAIL: blob round-trip"; exit 1; }
echo "==> project harness round-trip OK"

echo "==> M4: shared-vector byte-exact conformance + gated Go build tests"
( cd "$ROOT" && SOFAB_C_CORELIB="$CORELIB" go test ./generators/c/ \
    -run 'Conformance|Compiles|Project' -count=1 )

echo "==> corpus + realworld: every definition compiles"
# BIG descriptor profile so wide field ids (up to 2^31-1) fit the descriptor.
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang c --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    for c in "$WORK"/corpus/"$name"/*.c; do
        gcc -std=c99 -Wall -DSOFAB_OBJECT_DESCR_PROFILE=3 -I"$INC" -I"$WORK/corpus/$name" -c "$c" -o /dev/null \
            || { echo "FAIL: corpus def $name did not compile"; exit 1; }
    done
done
echo "==> corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

# corelib feature-subset configs. corelib-c-cpp can be built with SOFAB_DISABLE_*
# macros to drop wire types for a smaller footprint. The generated code guards
# every feature it uses with an #error, so a definition that avoids the disabled
# feature must still compile against that stripped corelib. Each row pairs a set
# of disable macros with a definition that uses only the features left enabled.
echo "==> corelib feature-subset configs: generated C compiles against each"
subset_c() {  # label  "DISABLE flags"  "yaml"
    name=$1; flags=$2; yaml=$3
    printf '%s' "$yaml" > "$WORK/sub_$name.yaml"
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang c --in "$WORK/sub_$name.yaml" --out "$WORK/sub_$name" >/dev/null )
    for c in "$WORK"/sub_$name/*.c; do
        gcc -std=c99 -Wall -DSOFAB_OBJECT_DESCR_PROFILE=3 $flags -I"$INC" -I"$WORK/sub_$name" -c "$c" -o /dev/null \
            || { echo "FAIL: [$name] generated C did not compile against the corelib subset"; exit 1; }
    done
    echo "   [$name] compiles ($flags)"
}
ALL='-DSOFAB_DISABLE_FIXLEN_SUPPORT -DSOFAB_DISABLE_ARRAY_SUPPORT -DSOFAB_DISABLE_SEQUENCE_SUPPORT -DSOFAB_DISABLE_FP64_SUPPORT -DSOFAB_DISABLE_INT64_SUPPORT'
subset_c min "$ALL" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u8}, b: {id: 1, type: i16}, c: {id: 2, type: i32}, d: {id: 3, type: boolean} } } }'
subset_c array "-DSOFAB_DISABLE_FIXLEN_SUPPORT -DSOFAB_DISABLE_SEQUENCE_SUPPORT -DSOFAB_DISABLE_FP64_SUPPORT -DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32}, arr: {id: 1, type: array, items: {type: u8, count: 4}} } } }'
subset_c fixlen "-DSOFAB_DISABLE_ARRAY_SUPPORT -DSOFAB_DISABLE_SEQUENCE_SUPPORT -DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32}, s: {id: 1, type: string, maxlen: 16}, b: {id: 2, type: blob, maxlen: 8}, f: {id: 3, type: fp32}, g: {id: 4, type: fp64} } } }'
subset_c sequence "-DSOFAB_DISABLE_ARRAY_SUPPORT -DSOFAB_DISABLE_FP64_SUPPORT -DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32}, st: {id: 1, type: struct, fields: { x: {id: 0, type: i32} }}, sa: {id: 2, type: array, items: {type: string, count: 3}} } } }'
subset_c nofp64 "-DSOFAB_DISABLE_FP64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u64}, f: {id: 1, type: fp32}, s: {id: 2, type: string, maxlen: 16}, arr: {id: 3, type: array, items: {type: u8, count: 4}} } } }'
subset_c noint64 "-DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u32}, b: {id: 1, type: i32}, f: {id: 2, type: fp32}, s: {id: 3, type: string, maxlen: 16} } } }'

echo "==> negative: a guard fires when a used feature is disabled in the corelib"
# (the full example uses every feature; each disable macro must trip its #error)
for flag in FIXLEN_SUPPORT ARRAY_SUPPORT SEQUENCE_SUPPORT FP64_SUPPORT INT64_SUPPORT; do
    if gcc -std=c99 -DSOFAB_OBJECT_DESCR_PROFILE=3 -DSOFAB_DISABLE_$flag -I"$INC" -I"$WORK/gen" \
            -c "$WORK"/gen/myfirstmessage.c -o /dev/null 2>/dev/null; then
        echo "FAIL: expected a capability-guard #error with $flag disabled"
        exit 1
    fi
done
echo "==> all guards fired as expected"

echo "PASS"
