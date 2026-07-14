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

# The shared example intentionally leaves `somemap` unbounded (a dynamic map for
# heap targets). The heapless C target requires a bound on every array, so derive
# a C-appropriate example that gives it an explicit capacity — exactly what a
# C-target schema author does. `count` never reaches the wire, so the round-trip
# and shared vectors are unchanged.
EXAMPLE="$WORK/example_c.yaml"
awk '
  /^      somemap:/ { inmap=1 }
  inmap && /^          type: struct$/ { print; print "          count: 8"; inmap=0; next }
  { print }
' "$ROOT/examples/messages/example.yaml" > "$EXAMPLE"

echo "==> generating C for the (bounded) example"
( cd "$ROOT" && go run ./cmd/sofabgen --lang c --in "$EXAMPLE" --out "$WORK/gen" )

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
generic: { emit: project }
targets: { c: { symbol_prefix: sofab_ } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/proj.yaml" --lang c --in "$EXAMPLE" --out "$WORK/proj" )
make -C "$WORK/proj" SOFAB_C_CORELIB="$CORELIB" >/dev/null
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someblobarray":[[1],[2],[3]],"someu64":18446744073709551615,"somestringarray":["a","b","c","d","e"]}'
OUT=$(printf '%s' "$IN" | "$WORK/proj/harness/harness" encode | "$WORK/proj/harness/harness" decode)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30' || { echo "FAIL: blob round-trip"; exit 1; }
echo "==> project harness round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (the C reference already rejects: the object
# descriptor binds capacity N and the istream returns SOFAB_RET_E_INVALID_MSG);
# exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if "$WORK/proj/harness/harness" decode < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
"$WORK/proj/harness/harness" decode < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

echo "==> M5: default omission is byte-exact sparse (non-zero scalar + string defaults)"
# The C backend emits a const default image + SOFAB_OBJECT_DESCR_WITH_DEFAULTS, so
# a field equal to its (possibly non-zero) schema default is dropped from the wire
# and reconstructed from the default on decode. This gates that end-to-end against
# the real corelib — with a zero-baseline encoder every field below would serialize.
cat > "$WORK/defmsg.yaml" <<'YAML'
version: 1
messages:
  DefMsg:
    payload:
      a:     { id: 0, type: u32, default: 7 }
      b:     { id: 1, type: i32, default: 10 }
      flag:  { id: 2, type: boolean, default: true }
      label: { id: 3, type: string, maxlen: 16, default: "hi" }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/proj.yaml" --lang c --in "$WORK/defmsg.yaml" --out "$WORK/defproj" )
make -C "$WORK/defproj" SOFAB_C_CORELIB="$CORELIB" >/dev/null
DH="$WORK/defproj/harness/harness"

# Every field at its default -> empty payload (nothing serialized).
n=$(printf '%s' '{"a":7,"b":10,"flag":true,"label":"hi"}' | "$DH" encode | wc -c)
[ "$n" -eq 0 ] || { echo "FAIL: all-default message must encode to 0 bytes, got $n"; exit 1; }

# A field overriding its default is on the wire; the omitted defaults are
# reconstructed on decode (proves "absent == default", not "absent == zero").
RT=$(printf '%s' '{"a":99,"b":10,"flag":true,"label":"hi"}' | "$DH" encode | "$DH" decode)
echo "$RT" | grep -q '"a":99'        || { echo "FAIL: overridden field lost"; exit 1; }
echo "$RT" | grep -q '"b":10'        || { echo "FAIL: omitted default b not reconstructed"; exit 1; }
echo "$RT" | grep -q '"flag":true'   || { echo "FAIL: omitted default flag not reconstructed"; exit 1; }
echo "$RT" | grep -q '"label":"hi"'  || { echo "FAIL: omitted default string not reconstructed"; exit 1; }

# A value differing from a non-zero default is serialized (bool default true -> false).
n=$(printf '%s' '{"a":7,"b":10,"flag":false,"label":"hi"}' | "$DH" encode | wc -c)
[ "$n" -gt 0 ] || { echo "FAIL: value != non-zero default must be on the wire"; exit 1; }

# A non-default string is serialized and round-trips (exercises the string compare).
RT=$(printf '%s' '{"a":7,"b":10,"flag":true,"label":"hey"}' | "$DH" encode | "$DH" decode)
echo "$RT" | grep -q '"label":"hey"' || { echo "FAIL: non-default string not round-tripped"; exit 1; }
echo "==> default omission byte-exact OK"

echo "==> M4: shared-vector byte-exact conformance + gated Go build tests"
( cd "$ROOT" && SOFAB_C_CORELIB="$CORELIB" go test ./generators/c/ \
    -run 'Conformance|Compiles|Project' -count=1 )

echo "==> corpus + realworld: every definition compiles"
# BIG descriptor profile so wide field ids (up to 2^31-1) fit the descriptor.
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    # no_maxlen is a deliberately-unbounded schema (dynamic-path coverage for heap
    # targets); the heapless C target requires bounds on every field, so it is not
    # a valid C input — the negative test below asserts it is rejected.
    [ "$name" = "no_maxlen" ] && continue
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
messages: { m: { payload: { a: {id: 0, type: i32}, st: {id: 1, type: struct, fields: { x: {id: 0, type: i32} }}, sa: {id: 2, type: array, items: {type: string, count: 3, maxlen: 8}} } } }'
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

# The C object model has no dynamic containers, so an unbounded string/blob/array
# (no maxlen/count) is a hard generation error naming the field — not a silently
# invented char[1]/T[0] that then rejects every real message at runtime (#104).
# There is no allow_dynamic escape for C.
echo "==> negative: unbounded fields are rejected at generate time (generator#104)"
neg_reject() {  # label  yaml
    name=$1; yaml=$2
    printf '%s' "$yaml" > "$WORK/neg_$name.yaml"
    if ( cd "$ROOT" && go run ./cmd/sofabgen --lang c --in "$WORK/neg_$name.yaml" --out "$WORK/neg_$name" >"$WORK/neg_$name.out" 2>&1 ); then
        echo "FAIL: [$name] unbounded field should be rejected"; exit 1
    fi
    grep -q 'has no' "$WORK/neg_$name.out" || { echo "FAIL: [$name] error should name the missing bound:"; cat "$WORK/neg_$name.out"; exit 1; }
    if grep -q 'allow_dynamic' "$WORK/neg_$name.out"; then
        echo "FAIL: [$name] C has no allow_dynamic escape:"; cat "$WORK/neg_$name.out"; exit 1
    fi
    echo "   [$name] rejected: $(grep -o 'field .* has no [a-z ]*' "$WORK/neg_$name.out" | head -1)"
}
neg_reject string       'version: 1
messages: { m: { payload: { s: { id: 0, type: string } } } }'
neg_reject nativearray  'version: 1
messages: { m: { payload: { a: { id: 0, type: array, items: { type: u32 } } } } }'
neg_reject the_corpus_no_maxlen "$(cat "$ROOT"/tests/matrix/corpus/defs/no_maxlen.yaml)"
echo "==> unbounded fields correctly rejected"

echo "PASS"
