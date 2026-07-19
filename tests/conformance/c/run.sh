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
# Exact match (closing bracket): a scalar blob is a sized blob, so a sub-maxlen
# value must round-trip with no trailing zero padding (issue #128). A prefix match
# would have silently passed the old padded "[10,20,30,0,0,...]" output.
echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: blob round-trip (sub-maxlen padded? issue #128)"; exit 1; }
# Blob-array elements are sized blobs too (issue #130): sub-maxlen elements must
# not be zero-padded to their maxlen (8) on round-trip.
echo "$OUT" | grep -q '"someblobarray":\[\[1\],\[2\],\[3\]\]' || { echo "FAIL: blob-array element padded/dropped (issue #130)"; exit 1; }
echo "==> project harness round-trip OK"

# Over-index wrapper array (generator#149 / F-0013): somestringarray (id 18)
# declares count: 5, lowering to a fixed-count sequence holder (element slots
# 0..4). A well-formed string element at wire index 5 (>= N) is INVALID per
# MESSAGE_SPEC S5.1/S7 -- the descriptor is emitted as SOFAB_OBJECT_DESCR_SEQ, so
# object.c rejects the unmatched element id via sofab_istream_invalidate
# (corelib-c-cpp#94) rather than skipping it. Wire: 96 01 (sequence_begin id 18)
# 2a (string id 5) 0a 78 (fixlen "x") 07 (sequence_end); control uses index 4.
echo "==> over-index wrapper array must reject (generator#149)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if "$WORK/proj/harness/harness" decode < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
"$WORK/proj/harness/harness" decode < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to -- for fixlen, including the
# subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0) is declared u8
# (unsigned wire type) and keeps its schema default 7. Wire: 01 = id 0 with wire
# type SIGNED (1), then the zig-zag varint 06 (= 3); control 00 09 -> 9.
# The C target needs no generated guard: object.c compares the descriptor's
# expected wire opt against the delivered one and leaves target_ptr NULL on a
# mismatch, so the istream skips the field (corelib-c-cpp#101). This pins that the
# generator keeps emitting descriptors the corelib can make that decision from.
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
OUT=$("$WORK/proj/harness/harness" decode < "$WORK/wiremismatch.bin") \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped field must keep its default 7; got: $OUT"; exit 1; }
OUT=$("$WORK/proj/harness/harness" decode < "$WORK/wiremismatch_control.bin") \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: control must decode to 9; got: $OUT"; exit 1; }
echo "==> wire-type skip OK"

# Repeated field id (MESSAGE_SPEC S7.4, generator#175): last occurrence wins per
# field id. A re-opened sequence CONTINUES its scope, so a struct merges and the
# children an earlier opening set whose ids do not recur are retained. somestruct
# (id 20) is opened twice: the first opening sets nestedstring (id 1) to "x", the
# second opens only the empty nestedstruct (id 2). nestedstring MUST survive.
# Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
#       a6 01 (seq start id 20) 16 07 (empty seq id 2) 07 (seq end)
echo "==> re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
OUT=$("$WORK/proj/harness/harness" decode < "$WORK/reopen_struct.bin") \
    || { echo "FAIL: re-opened struct must decode"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: re-opened struct must retain nestedstring \"x\"; got: $OUT"; exit 1; }
echo "==> struct scope merge OK"

# Repeated field id, array wrapper (MESSAGE_SPEC S7.4 + S5): an array wrapper IS
# the array's value, so unlike a struct it is REPLACED whole by a later occurrence
# rather than merged. somestringarray (id 18) is opened twice: elements 0="a" and
# 1="b" first, then only element 0="c". Element 1 MUST NOT survive as "b".
# In the C object API a wrapper is distinguished from a struct by the fixed_seq
# flag the generator already emits (SOFAB_OBJECT_DESCR_SEQ), which is what lets
# object.c reset the wrapper's slots on open while structs keep merging
# (corelib-c-cpp#101) -- so this target needs no generated clear, only the
# descriptor kind it already emits. Slots the second opening does not set fall
# back to their element defaults, so only element 1 != "b" is asserted.
# Wire: 96 01 (seq start id 18) 02 0a 61 ("a") 0a 0a 62 ("b") 07 (seq end)
#       96 01 (seq start id 18) 02 0a 63 ("c") 07 (seq end)
echo "==> re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
OUT=$("$WORK/proj/harness/harness" decode < "$WORK/reopen_array.bin") \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: re-opened array wrapper must start with the second opening's element 0 == \"c\"; got: $OUT"; exit 1; }
if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
    echo "FAIL: re-opened array wrapper must be replaced, not merged (element \"b\" survived); got: $OUT"; exit 1
fi
echo "==> array wrapper replace OK"

# issue #128: a scalar/struct-field blob carries a used-length, so every length
# 0..maxlen round-trips byte-exactly — including an all-zero blob (previously
# dropped to empty) and a single 0x00 (previously collapsed). Blobs are skipped by
# the shared-vector harness (scalarJSON), so this is the dedicated blob gate.
echo "==> M3b: sized-blob round-trips every length 0..maxlen (issue #128)"
cat > "$WORK/blob.yaml" <<'YAML'
version: 1
messages:
  Blob: { payload: { b: { id: 0, type: blob, maxlen: 4 } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/proj.yaml" --lang c --in "$WORK/blob.yaml" --out "$WORK/blobproj" >/dev/null )
make -C "$WORK/blobproj" SOFAB_C_CORELIB="$CORELIB" >/dev/null
BH="$WORK/blobproj/harness/harness"
# An empty blob (used_len 0) is omitted -> 0-byte wire (can't be re-fed: the
# corelib asserts datalen>0), so assert the omission rather than round-trip it.
n=$(printf '{"b":[]}' | "$BH" encode | wc -c)
[ "$n" -eq 0 ] || { echo "FAIL: empty blob must encode to 0 bytes, got $n"; exit 1; }
# Every non-empty length round-trips byte-exactly, incl. a single 0x00 and an
# all-zero full-capacity blob (both previously dropped/collapsed).
for want in '[0]' '[0,0,0,0]' '[255]' '[1,2,3]' '[1,2,3,4]'; do
    got=$(printf '{"b":%s}' "$want" | "$BH" encode | "$BH" decode)
    echo "$got" | grep -q "\"b\":$(printf '%s' "$want" | sed 's/[][]/\\&/g')" \
        || { echo "FAIL: blob $want round-tripped as $got (issue #128)"; exit 1; }
done
echo "==> sized-blob length round-trips OK"

# issue #130: a blob *array* element is a sized blob too — each element keeps its
# exact length, incl. an all-zero element and an empty element in the middle (a
# used_len-0 element is omitted by index, so gaps round-trip in place).
echo "==> M3c: sized blob-array elements round-trip every length (issue #130)"
cat > "$WORK/barr.yaml" <<'YAML'
version: 1
messages:
  Barr: { payload: { a: { id: 0, type: array, items: { type: blob, count: 3, maxlen: 4 } } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/proj.yaml" --lang c --in "$WORK/barr.yaml" --out "$WORK/barrproj" >/dev/null )
make -C "$WORK/barrproj" SOFAB_C_CORELIB="$CORELIB" >/dev/null
AH="$WORK/barrproj/harness/harness"
for want in '[[1],[2,3],[4,5,6,7]]' '[[0],[0,0,0,0],[255]]' '[[9],[],[7,7]]'; do
    got=$(printf '{"a":%s}' "$want" | "$AH" encode | "$AH" decode)
    echo "$got" | grep -q "\"a\":$(printf '%s' "$want" | sed 's/[][]/\\&/g')" \
        || { echo "FAIL: blob array $want round-tripped as $got (issue #130)"; exit 1; }
done
echo "==> sized blob-array round-trips OK"

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
