#!/usr/bin/env sh
# Reproducible Java conformance harness: install corelib, generate -> mvn package
# -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/java/run.sh [corelib-java]   (or set $SOFAB_JAVA_CORELIB)
# Requires: go, javac/java (JDK 17+), mvn, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_JAVA_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-java.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-java: $CORELIB"
VER=$(grep -m1 '<version>' "$CORELIB/pom.xml" | sed 's/.*<version>\(.*\)<\/version>.*/\1/')
echo "==> installing corelib-java $VER to local repo"
( cd "$CORELIB" && mvn -q -DskipTests install )

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project }
targets: { java: { package: message } }
YAML
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

build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang java --in "$1" --out "$2" )
    ( cd "$2" && mvn -q -Dsofab.version="$VER" package )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
H="java -jar $WORK/ex/target/harness.jar"
OUT=$(printf '%s' "$IN" | $H encode myfirstmessage | $H decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if $H decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
$H decode myfirstmessage < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if $H decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
$H decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if $H decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
$H decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to -- for fixlen, including the
# subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0) is declared u8
# (unsigned wire type) and keeps its schema default 7. Wire: 01 = id 0 with wire
# type SIGNED (1), then the zig-zag varint 06 (= 3). Control: 00 09 is the same id
# with the correct unsigned wire type and must decode to 9.
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/wiremismatch.bin") \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped field must keep its default 7; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/wiremismatch_control.bin") \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: control must decode to 9; got: $OUT"; exit 1; }
echo "==> wire-type skip OK"

# Contradictory ARRAY wire type at a SCALAR id (MESSAGE_SPEC S7.3, generator#183):
# the integer-array wire types are the residual case of the rule above -- the
# corelib delivers array elements one-by-one through the very unsigned()/signed()
# callbacks a lone scalar uses, so a generated visitor that dispatches on the id
# alone would store the elements instead of skipping them. someu8 (id 0) is
# declared u8 and MUST keep its schema default 7; somei8 (id 4) is declared i8 and
# MUST keep its default 10.
# Wire: 03 = id 0 with wire type ARRAY_UNSIGNED (3), count 01, element 05.
#       24 = id 4 with wire type ARRAY_SIGNED (4), count 01, element 06 (zig-zag 3).
# Control: 21 06 is id 4 with the correct SIGNED wire type and must decode to 3.
echo "==> array wire type at a scalar id must skip (MESSAGE_SPEC S7.3, generator#183)"
printf '\003\001\005' > "$WORK/arr_at_uscalar.bin"
printf '\044\001\006' > "$WORK/arr_at_iscalar.bin"
printf '\041\006' > "$WORK/arr_at_scalar_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/arr_at_uscalar.bin") \
    || { echo "FAIL: unsigned array at a u8 scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped array element must not land in someu8 (default 7); got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/arr_at_iscalar.bin") \
    || { echo "FAIL: signed array at an i8 scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somei8":10' || { echo "FAIL: skipped array element must not land in somei8 (default 10); got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/arr_at_scalar_control.bin") \
    || { echo "FAIL: control (correct signed wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"somei8":3' || { echo "FAIL: control must decode to 3; got: $OUT"; exit 1; }
# A genuinely declared array must be unaffected: someuintarray (id 15, count 4).
OUT=$($H decode myfirstmessage < "$WORK/control.bin") \
    || { echo "FAIL: declared array control must decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2,3,4\]' || { echo "FAIL: declared array must still decode to [1,2,3,4]; got: $OUT"; exit 1; }
echo "==> array-at-scalar skip OK"

# Repeated field id (MESSAGE_SPEC S7.4, generator#175): last occurrence wins per
# field id. A re-opened sequence CONTINUES its scope, so a struct merges and the
# children an earlier opening set whose ids do not recur are retained. somestruct
# (id 20) is opened twice: the first opening sets nestedstring (id 1) to "x", the
# second opens only the empty nestedstruct (id 2). nestedstring MUST survive --
# decoding the re-opening into a fresh object would reset it to "Nested".
# Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
#       a6 01 (seq start id 20) 16 07 (empty seq id 2) 07 (seq end)
echo "==> re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
OUT=$($H decode myfirstmessage < "$WORK/reopen_struct.bin") \
    || { echo "FAIL: re-opened struct must decode"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: re-opened struct must retain nestedstring \"x\"; got: $OUT"; exit 1; }
echo "==> struct scope merge OK"

# Repeated field id, array wrapper (MESSAGE_SPEC S7.4 + S5): an array wrapper IS
# the array's value, so unlike a struct it is REPLACED whole by a later occurrence
# rather than merged. somestringarray (id 18) is opened twice: the first opening
# sets elements 0="a" and 1="b", the second sets only element 0="c". Element 1 MUST
# NOT survive as "b" -- merging by index is the bug this pins.
# Wire: 96 01 (seq start id 18) 02 0a 61 (string id 0 "a") 0a 0a 62 (string id 1 "b")
#       07 (seq end) 96 01 (seq start id 18) 02 0a 63 (string id 0 "c") 07 (seq end)
echo "==> re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
OUT=$($H decode myfirstmessage < "$WORK/reopen_array.bin") \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: re-opened array wrapper must start with the second opening's element 0 == \"c\"; got: $OUT"; exit 1; }
if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
    echo "FAIL: re-opened array wrapper must be replaced, not merged (element \"b\" survived); got: $OUT"; exit 1
fi
echo "==> array wrapper replace OK"

# Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174): for a fixlen field
# the declared type maps to a wire type PLUS a subtype, so a header that carries
# the right Fixlen wire type but the WRONG subtype is just as contradictory as a
# wrong wire type and MUST be SKIPPED like an unknown id. somefp64 (id 9) is
# declared fp64 and keeps its schema default 3.141592653589793.
# Wire: 4a (id 9, fixlen) 0a (fixlen word: len 1, STRING subtype) 78 ("x")
# Control: 4a 41 (fixlen word: len 8, FP64 subtype) + 2.5 little-endian.
echo "==> fixlen subtype mismatch must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\112\012\170' > "$WORK/fixsubtype.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fixsubtype_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/fixsubtype.bin") \
    || { echo "FAIL: mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
    || { echo "FAIL: control (correct fp64 subtype) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
echo "==> fixlen subtype skip OK"

# S7.3 x S7.4, array wrapper (generator#174 + generator#175): "An occurrence
# skipped under S7.3 is not an occurrence for this clause: a correctly typed
# earlier occurrence survives a mis-typed later one." somestringarray (id 18) is
# opened correctly with element 0 = "a", then id 18 recurs carrying the UNSIGNED
# wire type. The mis-typed occurrence is skipped, so the array MUST still hold
# "a" -- the failure this guards is an EMPTY array, i.e. generated code clearing
# the wrapper before it checks the wire type.
# Wire: 96 01 (seq start id 18) 02 0a 61 (string id 0 "a") 07 (seq end)
#       90 01 (id 18, UNSIGNED) 05
# Asserted as a prefix: heap profiles render ["a"], fixed-capacity ones pad.
echo "==> mis-typed later occurrence must not clear the array (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\007\220\001\005' > "$WORK/skipped_occ_array.bin"
OUT=$($H decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["a"' || { echo "FAIL: skipped occurrence must not clear the array (element 0 == \"a\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps array OK"

# S7.3 x S7.4, struct: same rule for a struct scope. somestruct (id 20) is opened
# correctly with nestedstring (id 1) = "x", then id 20 recurs carrying the
# UNSIGNED wire type. That occurrence is skipped, so nestedstring MUST still
# be "x" rather than falling back to its default "Nested".
# Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
#       a0 01 (id 20, UNSIGNED) 05
echo "==> mis-typed later occurrence must not clear the struct (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\240\001\005' > "$WORK/skipped_occ_struct.bin"
OUT=$($H decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

# Receiver-side decode limits (generator#102): `a` is an UNBOUNDED u64 array
# (id 0 -> header 0x03 = 0<<3 | unsigned-array). With max_dyn_array_count: 4
# a wire count of 5 MUST fail with LIMIT_EXCEEDED (decode exits non-zero,
# checked at the count header before allocation); exactly 4 still decodes; and
# the same 5-element bytes MUST decode against a project built without limits.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/lim.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/limcfg.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
targets: { java: { package: message } }
YAML
build "$WORK/lim.yaml" "$WORK/lim" "$WORK/limcfg.yaml"
build "$WORK/lim.yaml" "$WORK/nolim"
HL="java -jar $WORK/lim/target/harness.jar"
HN="java -jar $WORK/nolim/target/harness.jar"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if $HL decode dyn < "$WORK/overlimit.bin" >/dev/null 2>"$WORK/limerr.txt"; then
    echo "FAIL: dyn array count 5 above max_dyn_array_count 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/limerr.txt" || { echo "FAIL: rejection must carry LIMIT_EXCEEDED"; exit 1; }
$HL decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: count 4 at the limit must decode"; exit 1; }
$HN decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits project must decode 5 elements"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/java/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/target/harness.jar"

echo "==> §7 decode status through the generated API (generator#105)"
HC="java -jar $WORK/conf/target/harness.jar"
ST=$(printf '\200' | $HC trydecode vecu | head -n1)   # lone 0x80: dangling varint
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: lone 0x80 -> $ST (want INCOMPLETE)"; exit 1; }
ST=$(printf '' | $HC trydecode vecu | head -n1)       # empty message: valid
[ "$ST" = "COMPLETE" ] || { echo "FAIL: empty message -> $ST (want COMPLETE)"; exit 1; }
echo "==> tryDecode status OK (0x80 INCOMPLETE, empty COMPLETE)"

echo "==> corpus + realworld: every definition compiles (javac vs corelib jar)"
JAR="$HOME/.m2/repository/org/sofabuffers/corelib/$VER/corelib-$VER.jar"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang java --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    mkdir -p "$WORK/corpus/$name/out"
    javac -cp "$JAR" -d "$WORK/corpus/$name/out" "$WORK"/corpus/"$name"/src/main/java/message/*.java \
        || { echo "FAIL: corpus def $name did not compile"; exit 1; }
done
echo "==> corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
