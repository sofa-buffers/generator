#!/usr/bin/env sh
# Reproducible Dart conformance harness: generate -> dart pub get + compile
# (vs corelib-dart) -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/dart/run.sh [corelib-dart]   (or set $SOFAB_DART_CORELIB)
# Requires: go, dart, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_DART_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-dart"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-dart.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-dart: $CORELIB"

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project }
YAML
cat > "$WORK/conf.yaml" <<'YAML'
version: 1
messages:
  vecu: { payload: { a: { id: 0, type: u64 } } }
  veci: { payload: { a: { id: 0, type: i64 } } }
  vecf32: { payload: { a: { id: 0, type: fp32 } } }
  vecf32a: { payload: { a: { id: 0, type: array, items: { type: fp32, count: 3 } } } }
  vecf64: { payload: { a: { id: 0, type: fp64 } } }
  vecs: { payload: { a: { id: 0, type: string, maxlen: 4096 } } }
  vecsa: { payload: { a: { id: 0, type: array, items: { type: string, count: 8, maxlen: 16 } } } }
YAML

# Generate a project, wire the corelib path, resolve deps and compile the harness
# to a native exe (fast: no per-invocation JIT startup for the vector loop).
build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang dart --in "$1" --out "$2" )
    sed -i "s#\${SOFAB_DART_CORELIB}#$CORELIB#" "$2/pubspec.yaml"
    ( cd "$2" && dart pub get >/dev/null 2>&1 && dart compile exe bin/harness.dart -o harness >/dev/null 2>&1 )
}

# Lighter check for the corpus sweep: generate, resolve deps, and type-check with
# `dart analyze` (a clean analyze == the generated code + harness compile), which
# is far faster than AOT-compiling an exe for every definition.
check() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang dart --in "$1" --out "$2" )
    sed -i "s#\${SOFAB_DART_CORELIB}#$CORELIB#" "$2/pubspec.yaml"
    ( cd "$2" && dart pub get >/dev/null 2>&1 && dart analyze --fatal-warnings >/dev/null )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"
H="$WORK/ex/harness"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":"18446744073709551615","somestringarray":["a","b","c"]}'
OUT=$(printf '%s' "$IN" | "$H" encode myfirstmessage | "$H" decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":"18446744073709551615"' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b). 5 wire elements MUST be INVALID (MESSAGE_SPEC 3+7);
# exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if "$H" decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
"$H" decode myfirstmessage < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2). someuintarray declares count 4; a header announcing 6 elements
# (> 4) followed by only 2 elements then EOF is BOTH schema-invalid and truncated.
# The over-count is decided at the count word (onArrayBegin, before the truncation
# check), so tryDecode MUST report INVALID -- the whole-slice values.length>4 guard
# in onUnsignedArray never runs on a truncated array, so this pins the header hook.
# Wire: 7b (id 15 unsigned-array) 06 (count 6) 01 02 (2 of 6 elements) <EOF>.
echo "==> over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
ST=$(printf '\173\006\001\002' | "$H" trydecode myfirstmessage | head -n1)
[ "$ST" = "INVALID" ] || { echo "FAIL: over-count(6>4)+truncated -> $ST (want INVALID)"; exit 1; }
# Precision control: an IN-BOUND count (4 == bound) genuinely truncated (2 of 4
# then EOF) is a clean truncation and MUST stay INCOMPLETE.
ST=$(printf '\173\004\001\002' | "$H" trydecode myfirstmessage | head -n1)
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(4==4)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> over-count/truncation ordering OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID (MESSAGE_SPEC
# S5.1/S7), never grown-into. Wire: 96 01 (seq begin id 18) 2a (string id 5) 0a 78
# ("x") 07 (seq end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if "$H" decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
"$H" decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob -> INVALID, never truncated. Wire: 62 8b 01 + 17 bytes;
# control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if "$H" decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
"$H" decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Over-maxlen AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2), the string/blob analogue of the over-count ordering above.
# someblob (id 12) declares maxlen 16; a length word of 17 (> 16) followed by only 1
# payload byte then EOF is decided at the length word (onFixlenHeader, before the
# payload take), so tryDecode MUST report INVALID.
# Wire: 62 (blob id 12) 8b 01 (fixlen word: len 17, blob subtype) 01 (1 of 17) <EOF>.
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#216)"
ST=$(printf '\142\213\001\001' | "$H" trydecode myfirstmessage | head -n1)
[ "$ST" = "INVALID" ] || { echo "FAIL: over-maxlen(17>16)+truncated -> $ST (want INVALID)"; exit 1; }
# Precision control: an IN-BOUND length (16 == maxlen) genuinely truncated (1 of 16
# payload bytes then EOF) is a clean truncation and MUST stay INCOMPLETE.
ST=$(printf '\142\203\001\001' | "$H" trydecode myfirstmessage | head -n1)
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(16==16)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> over-maxlen/truncation ordering OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to is SKIPPED. corelib-dart
# dispatches by resolved type to distinct callbacks, so this is structural. someu8
# (id 0) declared u8 keeps its default 7. Wire: 01 (id 0, SIGNED) 06. Control: 00 09.
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/wiremismatch.bin") \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped field must keep its default 7; got: $OUT"; exit 1; }
OUT=$("$H" decode myfirstmessage < "$WORK/wiremismatch_control.bin") \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: control must decode to 9; got: $OUT"; exit 1; }
echo "==> wire-type skip OK"

# Array wire type at a scalar id (MESSAGE_SPEC S7.3, generator#183): corelib-dart
# delivers a native array whole through a distinct on*Array callback (like Go), so
# an integer/fp array at a scalar id lands in a callback the scalar id switch does
# not have -- it evaporates structurally, no askip guard. someu8 (id 0, default 7)
# and somefp64 (id 9, default pi) must keep their defaults.
echo "==> array wire type at a scalar id must skip (MESSAGE_SPEC S7.3, generator#183/#193)"
printf '\003\001\005' > "$WORK/arr_at_u8.bin"             # unsigned array at id 0
printf '\115\001\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar.bin"  # fp64 array at id 9
printf '\173\004\001\002\003\004' > "$WORK/arr_legit.bin" # someuintarray (id 15) legit
OUT=$("$H" decode myfirstmessage < "$WORK/arr_at_u8.bin") \
    || { echo "FAIL: unsigned array at a scalar u8 id must skip"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped array must leave someu8 at default 7; got: $OUT"; exit 1; }
OUT=$("$H" decode myfirstmessage < "$WORK/fp_arr_at_scalar.bin") \
    || { echo "FAIL: fp array at a scalar fp id must skip"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fp array must leave somefp64 at its default; got: $OUT"; exit 1; }
OUT=$("$H" decode myfirstmessage < "$WORK/arr_legit.bin") \
    || { echo "FAIL: a declared unsigned array must decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2,3,4\]' || { echo "FAIL: declared array must decode to [1,2,3,4]; got: $OUT"; exit 1; }
echo "==> array-at-scalar skip OK"

# Repeated field id (MESSAGE_SPEC S7.4, generator#175): a re-opened struct scope
# CONTINUES (merges); an array wrapper is REPLACED whole. somestruct (id 20) opened
# twice: nestedstring "x" must survive the second (empty nestedstruct) opening.
echo "==> re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/reopen_struct.bin") \
    || { echo "FAIL: re-opened struct must decode"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: re-opened struct must retain nestedstring \"x\"; got: $OUT"; exit 1; }
echo "==> struct scope merge OK"

# somestringarray (id 18) opened twice: 0="a",1="b" then only 0="c". Element 1 must
# NOT survive (replace, not merge).
echo "==> re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/reopen_array.bin") \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: re-opened array wrapper must start with \"c\"; got: $OUT"; exit 1; }
if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
    echo "FAIL: re-opened array wrapper must be replaced, not merged; got: $OUT"; exit 1
fi
echo "==> array wrapper replace OK"

# Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174): somefp64 (id 9) gets
# a fixlen STRING header -> skipped (corelib dispatches to onString, no case 9).
echo "==> fixlen subtype mismatch must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\112\012\170' > "$WORK/fixsubtype.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fixsubtype_control.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/fixsubtype.bin") \
    || { echo "FAIL: mismatched fixlen subtype must skip"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default; got: $OUT"; exit 1; }
OUT=$("$H" decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
    || { echo "FAIL: control (correct fp64 subtype) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
echo "==> fixlen subtype skip OK"

# Fixlen subtype mismatch AT A BOUNDED FIELD (generator#224, MESSAGE_SPEC S7.3):
# onFixlenHeader fires for ANY fixlen subtype at a field id, so a maxlen guard that
# compares length alone measures a CONTRADICTING value against this field's bound
# and rejects it, where S7.3 requires it be skipped. The guard must be gated on the
# declared subtype. someblob (id 12) declares maxlen 16: a 17-byte STRING at that id
# is a subtype mismatch (skip -> someblob keeps its "Hello" default), while a
# 17-byte BLOB there is the genuine over-maxlen INVALID (asserted above).
# Wire: 62 (id 12 fixlen) 8a 01 (fixlen word: len 17, subtype STRING) + 17 bytes.
echo "==> over-bound fixlen at a MISMATCHED subtype must skip (S7.3, generator#224)"
printf '\142\212\001aaaaaaaaaaaaaaaaa' > "$WORK/fixsub_bounded.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/fixsub_bounded.bin") \
    || { echo "FAIL: 17-byte string at a maxlen-16 BLOB id must skip, not be measured against maxlen"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[72,101,108,108,111\]' \
    || { echo "FAIL: skipped fixlen field must keep its default; got: $OUT"; exit 1; }

# The reported shape: a fixlen FP value whose fixed width exceeds a small maxlen.
# Needs maxlen < 8, which the example has no field for, so use a dedicated schema.
cat > "$WORK/fixsub.yaml" <<'YAML'
version: 1
messages:
  probe: { payload: { s: { id: 2, type: string, maxlen: 32 }, b: { id: 3, type: blob, maxlen: 4 } } }
YAML
build "$WORK/fixsub.yaml" "$WORK/fixsub"
FH="$WORK/fixsub/harness"
# fp64 1.5 (8 bytes > maxlen 4) at the blob id 3 -> subtype mismatch -> skip.
# Wire: 1a (id 3 fixlen) 41 (len 8, subtype FP64) + 8 payload bytes.
OUT=$(printf '\032\101\000\000\000\000\000\000\370\077' | "$FH" decode probe) \
    || { echo "FAIL: fp64 at a maxlen-4 blob id must skip (generator#224)"; exit 1; }
echo "$OUT" | grep -q '"b":\[\]' || { echo "FAIL: skipped fp64 must leave b at its default; got: $OUT"; exit 1; }
# fp32 1.5 (4 bytes, within the bound) at the same id: also a mismatch, also skipped.
OUT=$(printf '\032\040\000\000\300\077' | "$FH" decode probe) \
    || { echo "FAIL: fp32 at a maxlen-4 blob id must skip"; exit 1; }
echo "$OUT" | grep -q '"b":\[\]' || { echo "FAIL: skipped fp32 must leave b at its default; got: $OUT"; exit 1; }
# Precision controls: the bound still bites on the MATCHING subtype, and still
# dominates truncation (generator#216) -- the gate must not disarm either.
"$FH" decode probe < /dev/null >/dev/null || { echo "FAIL: empty probe must decode"; exit 1; }
printf '\032\043\001\002\003\004' | "$FH" decode probe >/dev/null \
    || { echo "FAIL: 4-byte blob (== maxlen 4) must decode"; exit 1; }
if printf '\032\053\001\002\003\004\005' | "$FH" decode probe >/dev/null 2>&1; then
    echo "FAIL: 5-byte blob (> maxlen 4) must be INVALID"; exit 1
fi
ST=$(printf '\032\053\001' | "$FH" trydecode probe | head -n1)
[ "$ST" = "INVALID" ] || { echo "FAIL: over-maxlen(5>4)+truncated -> $ST (want INVALID)"; exit 1; }
ST=$(printf '\032\043\001' | "$FH" trydecode probe | head -n1)
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(4==4)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> bounded-field subtype gate OK"

# S7.3 x S7.4: a mis-typed later occurrence must not clear a valid earlier array/
# struct. The clear lives inside onSequenceStart, which the corelib only calls for
# an actual sequence header -- so a mis-typed (non-sequence) occurrence never clears.
echo "==> mis-typed later occurrence must not clear the array (MESSAGE_SPEC S7.4)"
printf '\226\001\002\012\141\007\220\001\005' > "$WORK/skipped_occ_array.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["a"' || { echo "FAIL: skipped occurrence must not clear the array; got: $OUT"; exit 1; }
echo "==> mis-typed later occurrence must not clear the struct (MESSAGE_SPEC S7.4)"
printf '\246\001\012\012\170\007\240\001\005' > "$WORK/skipped_occ_struct.bin"
OUT=$("$H" decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: skipped occurrence must not clear the struct; got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps array/struct OK"

# Receiver-side decode limits (generator#102): `a` is a count-less u64 array (id 0),
# so a configured max_dyn_array_count: 4 makes a wire count of 5 fail decode with
# limitExceeded; exactly 4 still decode; and the same oversized bytes decode fine
# against a no-limits build.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg-limit.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limit.yaml" --lang dart --in "$WORK/dyn.yaml" --out "$WORK/dynlim" )
sed -i "s#\${SOFAB_DART_CORELIB}#$CORELIB#" "$WORK/dynlim/pubspec.yaml"
( cd "$WORK/dynlim" && dart pub get >/dev/null 2>&1 && dart compile exe bin/harness.dart -o harness >/dev/null 2>&1 )
build "$WORK/dyn.yaml" "$WORK/dynfree"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if "$WORK/dynlim/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null 2>&1; then
    echo "FAIL: 5 elements above max_dyn_array_count 4 must fail decode"; exit 1
fi
"$WORK/dynlim/harness" decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: 4 elements at the limit must decode"; exit 1; }
"$WORK/dynfree/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits build must decode the oversized message"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/dart/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/harness"

# fp32 signaling-NaN bit-for-bit round-trip (issue #226): a Dart double quiets an
# fp32 sNaN, so the generated code must route through corelib-dart's raw-bits API
# (onFp32Bits / writeFp32Bits) and a bit-exact Float32List copy. `recode` is
# wire -> object -> wire (no JSON), so an sNaN must survive decode+re-encode. Cover
# a signaling (0x7F800001), a payload/quiet (0x7FC00001), and a negative NaN, both
# as a scalar (vecf32, id 0) and as fp32 array elements (vecf32a, count 3).
echo "==> fp32 signaling-NaN bit-exact round-trip (issue #226)"
recode_exact() { # label message octal-wire
    # shellcheck disable=SC2059  # $3 is a controlled octal escape sequence, not user data
    printf "$3" > "$WORK/fp32in.bin"
    "$WORK/conf/harness" recode "$2" < "$WORK/fp32in.bin" > "$WORK/fp32out.bin" \
        || { echo "FAIL: $1 must decode"; exit 1; }
    cmp -s "$WORK/fp32in.bin" "$WORK/fp32out.bin" \
        || { echo "FAIL: $1 not bit-exact (an fp32 NaN was quieted)"; exit 1; }
}
# scalar: 02 (id0 fixlen) 20 (fp32 subtype) + 4 LE bytes
recode_exact "scalar sNaN"    vecf32 '\002\040\001\000\200\177'
recode_exact "scalar qNaN"    vecf32 '\002\040\001\000\300\177'
recode_exact "scalar -NaN"    vecf32 '\002\040\001\000\300\377'
# array: 05 (id0 arrayFixlen) 03 (count) 20 (fp32 subtype) + 3x4 LE bytes, all
# non-zero so the canonical trailing-default trim keeps all three elements.
recode_exact "array 3xNaN"    vecf32a '\005\003\040\001\000\200\177\001\000\300\177\001\000\300\377'
echo "==> fp32 sNaN round-trip OK"

echo "==> §7 decode status through the generated API"
ST=$(printf '\200' | "$WORK/conf/harness" trydecode vecu | head -n1)   # lone 0x80: dangling varint
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: lone 0x80 -> $ST (want INCOMPLETE)"; exit 1; }
ST=$(printf '' | "$WORK/conf/harness" trydecode vecu | head -n1)       # empty message: valid
[ "$ST" = "COMPLETE" ] || { echo "FAIL: empty message -> $ST (want COMPLETE)"; exit 1; }
echo "==> tryDecode status OK (0x80 INCOMPLETE, empty COMPLETE)"

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    check "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
