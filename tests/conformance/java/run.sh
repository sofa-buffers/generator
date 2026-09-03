#!/usr/bin/env sh
# Reproducible Java conformance harness: install corelib, generate -> mvn package
# -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/java/run.sh [corelib-java]   (or set $SOFAB_JAVA_CORELIB)
# Requires: go, javac/java (JDK 17+), mvn, git, python3.
set -eu

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"

# Shared MAX_SIZE fill check (ARCHITECTURE §9.6).
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_JAVA_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    clone_corelib corelib-java "$WORK/corelib"
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
# The decode-side message (generator#444). Printed by the driver that asserts
# against it, so the ids it declares and the ids that driver expects to read
# back cannot drift apart.
python3 "$ROOT/tests/conformance/lib/check_vectors_decode.py" --emit-schema \
    >> "$WORK/conf.yaml"

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

# The BOUNDED encode arm (CORELIB_PLAN §5.1, ARCHITECTURE §9.6, generator#415).
# Generated code owns the output buffer and the corelib never grows or
# reallocates it, so the worst-case size the backend derives from the schema --
# emitted as MAX_SIZE and handed straight to OStream.overScratch -- is the only
# thing between a legal message and a BUFFER_FULL. That number ships in every
# generated class and every encode above already runs through it -- what nothing
# here asserted is that it is EXACT.
#
# The fill message pins it from both sides: filling every field to its declared
# bound must encode to exactly MAX_SIZE bytes, so the buffer can be neither short
# (a legal message would not fit) nor slack (RAM paid for nothing). Why that
# needs both a constant leg and an encoder leg is argued once, in the driver.
echo "==> bounded encode buffer is exactly MAX_SIZE (ARCHITECTURE §9.6)"
build "$ROOT/tests/conformance/lib/maxsize_fill.yaml" "$WORK/fill"
check_maxsize_constant java "$WORK/fill/src/main/java/message/Fill.java" \
    "public static final int MAX_SIZE = $SOFAB_MAXSIZE_FILL_BYTES;\$"
check_maxsize_fill java java -jar "$WORK/fill/target/harness.jar" encode fill

# A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412):
# no value the codec delivers may outlive the callback it arrived in, so the
# buffer a message was decoded from may be reused or overwritten the moment the
# call returns.
#
# Nothing else here reaches it. Every decode above and below hands the harness a
# buffer that stays alive and unmodified for the whole run -- the chunk-invariance
# row (generator#413) included, which compares two readers of the SAME live bytes
# and would see identical values out of an aliased destination. The oracle has to
# DESTROY the input between decode and re-encode, in the same process, holding the
# decoded object; that is what OwnershipCheck.java does, on both surfaces the
# generated code offers and across a sweep of chunk sizes.
#
# It rides in $WORK/ex rather than in a project of its own: it needs example.yaml's
# aliasing-capable fields, and the extra class does not disturb the harness jar
# (the pom names Main as its Main-Class).
echo "==> a decoded message owns its bytes (CORELIB_PLAN §6.7, generator#412)"
cp "$ROOT/tests/conformance/java/OwnershipCheck.java" "$WORK/ex/src/main/java/message/"
( cd "$WORK/ex" && mvn -q -Dsofab.version="$VER" package )
java -cp "$WORK/ex/target/harness.jar" message.OwnershipCheck \
    || { echo "FAIL: a decoded field aliased the buffer it was decoded from"; exit 1; }
echo "==> decode ownership OK"

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

# Fixlen-array element subtype decides BEFORE the schema count bound
# (CORELIB_PLAN S4.8, generator#259 / Crucible F-0042). somefloatarray declares
# fp32 with count: 3 at id 17 -> array-fixlen header 8d 01 (17<<3 | 5). A fixlen
# array carries a fixlen_word after its count: 20 = 4-byte elements (fp32),
# 41 = 8-byte elements (fp64).
#
#   fp64 header, count 5 at the fp32-declared id: the subtype contradicts the
#   declared element type, so the field is SKIPPED (MESSAGE_SPEC S7.3) and its
#   count is not this field's count -- it must NOT be measured against 3. The
#   payload is 5 x 8 zero bytes, so the message is complete and must DECODE.
#
#   fp32 header, count 5 at the same id: the subtype matches, so this really is
#   the field's count and the schema bound applies -- still INVALID (S3+S7).
#
# Before the subtype reached the array header hook the two were indistinguishable
# and both rejected, which is the defect this pins.
echo "==> fixlen-array subtype decides before the count bound (generator#259)"
printf '\215\001\005\101\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' > "$WORK/fp64_at_fp32.bin"
printf '\215\001\005\040\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' > "$WORK/fp32_overcount.bin"
$H decode myfirstmessage < "$WORK/fp64_at_fp32.bin" >/dev/null || { echo "FAIL: fp64 array at an fp32-declared id must be skipped, not bounded"; exit 1; }
if $H decode myfirstmessage < "$WORK/fp32_overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: fp32 array with count 5 > 3 at its own id must stay INVALID"; exit 1
fi
echo "==> fixlen-array subtype ordering OK"

# ...and step 3 of that same order (CORELIB_PLAN S4.8.1, generator#411): a fixlen
# array whose subtype is neither fp32 nor fp64 -- a string, a blob, or a reserved
# 0x4-0x7 -- is INVALID, not a skip. The block above pins steps 4 and 5; step 3
# sits before both, and before the schema: S4.8 admits no fixlen array of string
# or blob, so no schema could have declared one and the bytes are malformed
# whatever follows. Routing that into the S7.3 skip would accept a construct the
# format does not have -- and generated Java could not notice, since its array
# arm only asks whether the announced kind is the one it declared and returns
# quietly when it is not. The fixlen_word never reaches it; the corelib decides
# at the word.
#
# One shared driver for all eleven suites (ARCHITECTURE S12). It derives every
# fixture from the schema's own somefloatarray declaration, so the ids it writes
# and the values it asserts cannot drift from what the harness was built with,
# and it compares the skipped field's default as JSON numbers rather than by
# grep, which is what lets one table serve backends that render it three ways.
#
# Run on BOTH decode surfaces. The verdict is the corelib's, taken at the
# fixlen_word, and several corelibs reach that word twice -- one arm for a
# whole-buffer decode and a separate one for the chunked path -- so a table that
# only ever ran the one-shot verb passes with the streaming copy mutated. This is
# the sweep the shared-vector and growth drivers beside it already do.
echo "==> a string/blob/reserved fixlen-array subtype is INVALID (generator#411)"
for surface in decode streamdecode; do
    python3 "$ROOT/tests/conformance/lib/check_fixlen_array_subtype.py" "java" \
        --verb "$surface" --invalid-pattern 'INVALID_MSG' \
        -- $H
done

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

# ...and the same violation with the message cut RIGHT AFTER the word that
# carries it (MESSAGE_SPEC S5.2, generator#267). The over-maxlen is fully
# established by the fixlen length word -- 17 > maxlen 16 is decided by bytes
# already on the wire -- so running out of input cannot downgrade the verdict.
#
# The guard used to live in the PAYLOAD callback, which never fires for a message
# that ends here, so this reported INCOMPLETE. The corelib hook announcing a
# fixlen field at its length word is what makes the verdict available in time.
#
#   62      blob, field id 12 ((12<<3)|2)
#   8b 01   fixlen word: byte length 17, blob subtype -> ((17<<3)|3)
#   <EOF>   not one payload byte
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#267)"
printf '\142\213\001' > "$WORK/overmaxlen_trunc.bin"
printf '\142\203\001' > "$WORK/inmaxlen_trunc.bin"
# The two verdicts arrive on DIFFERENT channels here, which is this backend's
# documented contract rather than an accident: tryDecode returns
# COMPLETE/INCOMPLETE and malformed input THROWS. So the assertion is that the
# over-bound message throws (INVALID) while the in-bound one returns INCOMPLETE
# -- and the second half is what makes this an ordering check rather than a
# blanket reject.
if $H trydecode myfirstmessage < "$WORK/overmaxlen_trunc.bin" >/dev/null 2>"$WORK/omt.err"; then
    echo "FAIL: over-maxlen(17>16)+truncated must be INVALID, not a returned status"; exit 1
fi
grep -q "INVALID_MSG\|InvalidMessage" "$WORK/omt.err" || {
    echo "FAIL: over-maxlen(17>16)+truncated must report INVALID; got:"; cat "$WORK/omt.err"; exit 1; }
ST=$($H trydecode myfirstmessage < "$WORK/inmaxlen_trunc.bin" | sed -n 1p)
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(16==16)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> maxlen/truncation ordering OK"

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

# MIS-TYPED ARRAY KIND at an ARRAY-declared id (MESSAGE_SPEC S7.3, generator#254):
# the residual case of the rule above. someuintarray (id 15) declares u32[4], which
# maps to the ARRAY_UNSIGNED wire type; a header carrying ARRAY_SIGNED at that id is
# just as contradictory as an array at a scalar id and MUST be skipped whole --
# which includes NOT RESIZING the declared field from the skipped header's count
# ("MUST NOT decode its payload into the declared field"). What used to leak here is
# the LENGTH, not the element: the message re-encoded as a ONE-element array holding
# 0, so the assertion is that someuintarray is still its full 4-element default.
# Wire: 7c = id 15 with wire type ARRAY_SIGNED (4), 01 = count 1, 06 = zig-zag 3.
# Mirror: 83 01 = id 16 (declared i32[5]) with ARRAY_UNSIGNED (3), count 1, 05.
# Over-count mis-typed: 7c 05 ... = 5 elements at a count:4 field. The schema bound
# applies only to a field that SURVIVES S7.3, so this is skipped, NOT a false
# INVALID -- contrast overcount.bin above, which is correctly typed and IS INVALID.
# Control: 7b 01 05 is id 15 with the correct ARRAY_UNSIGNED wire type -> [5].
echo "==> mis-typed array kind at an array id must skip, not resize (S7.3, generator#254)"
printf '\174\001\006' > "$WORK/mistyped_array.bin"
printf '\203\001\001\005' > "$WORK/mistyped_array_mirror.bin"
printf '\174\005\002\004\006\010\012' > "$WORK/mistyped_array_overcount.bin"
printf '\173\001\005' > "$WORK/mistyped_array_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/mistyped_array.bin") \
    || { echo "FAIL: mis-typed array kind must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[0,1,1000,4294967295\]' \
    || { echo "FAIL: a skipped array must not resize someuintarray (default [0,1,1000,4294967295]); got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/mistyped_array_mirror.bin") \
    || { echo "FAIL: mis-typed array kind (unsigned header at a signed array) must skip"; exit 1; }
echo "$OUT" | grep -q '"someintarray":\[10,20,300,4000,50000\]' \
    || { echo "FAIL: a skipped array must not resize someintarray (default [10,20,300,4000,50000]); got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/mistyped_array_overcount.bin") \
    || { echo "FAIL: an over-count MIS-TYPED array must be skipped, not INVALID"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[0,1,1000,4294967295\]' \
    || { echo "FAIL: over-count mis-typed array must leave someuintarray at its default; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/mistyped_array_control.bin") \
    || { echo "FAIL: control (correct ARRAY_UNSIGNED wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[5\]' \
    || { echo "FAIL: control must decode someuintarray to [5]; got: $OUT"; exit 1; }
echo "==> mis-typed array kind skip OK"

# fp ARRAY delivered to a SCALAR-declared fp id (MESSAGE_SPEC S7.3, generator#193):
# the fp analogue of the integer case above. corelib-java streams a fixlen (fp)
# array element-by-element through the very fp32()/fp64() callbacks a lone scalar
# uses, so without the arrayBegin-armed skip counter the element would land in the
# scalar's arm. somefp64 (id 9, declared fp64, default 3.141592653589793) receives
# an fp64 ARRAY and must be skipped whole.
# Wire: 4d = id 9 wire type ARRAY_FIXLEN (5), 01 = count 1, 41 = fixlen word (len 8,
#       FP64 subtype), then 2.5 little-endian.
# Control: 4a 41 + 2.5 is id 9 with the correct scalar FIXLEN wire type -> 2.5.
printf '\115\001\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/fp_arr_at_scalar.bin") \
    || { echo "FAIL: fp array at a scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fp array must leave somefp64 at its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/fp_arr_at_scalar_control.bin") \
    || { echo "FAIL: control (correct scalar fixlen wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode somefp64 to 2.5; got: $OUT"; exit 1; }
echo "==> fp array-at-scalar skip OK"

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

# The same skip, one rule over: a string a decoder STEPS OVER is never
# UTF-8-validated (CORELIB_PLAN S6.4.5, generator#417). Validation belongs where
# a string is MATERIALIZED, and it is taken on the complete payload (S6.4.4), so
# the two halves have to be asserted on the same bytes: a backend that validates
# too eagerly passes the declared half and fails the skipped one, a backend that
# never validates passes the skipped half and fails the declared one, and neither
# failure is visible from the other side. The driver runs four accept rows, not
# two: an undeclared id, a BLOB subtype at the id that DOES declare a string, a
# well-formed STRING at a scalar-declared id, and that last shape again one
# scope down, inside a sequence-framed struct.
#
# One shared driver for all eleven suites (ARCHITECTURE S12); it derives every
# fixture from the schema's own somestring/somefp64/someu8 declarations, and every
# skip row carries a trailing someu8 = 42 so a skip that ate one byte too many or
# too few cannot pass while the string sits at its default.
#
# Nothing is gated here: Java `String` is a S6.4.1 Unicode type, always strict,
# and the option MAY be omitted entirely -- there is no switch to turn the check
# off, and gating the declared half would only hide a regression.
#
# What the skip rows pin on the generated side is the #257/#258 destination
# guard: Myfirstmessage.string(int id, ...) opens with a (location, id) switch
# whose `default: return;` fires BEFORE the payload accumulator is touched, and
# corelib-java validates inside PayloadAcc -> Utf8.decode. Delete that guard and
# the skip rows go red; nothing else in this suite would notice.
#
# The category comes from the error text: `decode` exits 1 for INCOMPLETE too, so
# a bare non-zero exit would accept a wrongly-INCOMPLETE verdict -- which is what
# a decoder that mis-measures a skipped payload reports the moment it walks off
# its end. INVALID_MSG is the same channel the generator#411 call above uses.
echo "==> a skipped string is not UTF-8-validated (CORELIB_PLAN S6.4.5, generator#417)"
for surface in decode streamdecode; do
    python3 "$ROOT/tests/conformance/lib/check_skipped_string_utf8.py" "java" \
        --verb "$surface" --invalid-pattern 'INVALID_MSG' \
        -- $H
done

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
# the same 5-element bytes MUST decode against a project with no key set, which
# carries the TARGET DEFAULT rather than no cap (generator#385).
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
$HN decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: default-cap project must decode 5 elements"; exit 1; }

# ...and the cap must NOT reach a field that is SKIPPED (CORELIB_PLAN S6.2.1, the
# clause generator#410 is filed against): a limit bounds an allocation, and a
# field the handler walks past allocates nothing. Both skip shapes carry an
# over-cap count of 5 and both MUST leave the decode COMPLETE:
#   04 05 ...  a SIGNED array at the unsigned-declared id 0 (wire type
#              contradiction, MESSAGE_SPEC S7.3)
#   3b 05 ...  an UNKNOWN id 7 carrying an unsigned array
# The unknown-id row is the serious one -- a receiver that refuses a message
# whose only offence is a field it does not know has broken the forward
# compatibility unknown-id skipping exists to provide.
printf '\004\005\000\000\000\000\000' > "$WORK/skiplimit_mistyped.bin"
printf '\073\005\001\002\003\004\005' > "$WORK/skiplimit_unknown.bin"
for v in skiplimit_mistyped skiplimit_unknown; do
    OUT=$($HL decode dyn < "$WORK/$v.bin" 2>"$WORK/limerr.txt") || {
        echo "FAIL: $v -- an over-cap SKIPPED array must leave the decode COMPLETE; got:"; cat "$WORK/limerr.txt"; exit 1; }
    echo "$OUT" | grep -q '"a":\[\]' || { echo "FAIL: $v must leave the declared array untouched; got: $OUT"; exit 1; }
done
echo "==> decode limits OK (rejects at the count header, never on a skipped field)"

# The string/blob half of the same rule, and the half no "does it decode" row can
# check. A blob at an id the schema does not declare is SKIPPED: its bytes are
# walked over, never materialised (MESSAGE_SPEC S7.3; CORELIB_PLAN S6.2.1 "a
# skipped field is never capped", S6.6 "the codec allocates no payload storage").
# A decoder that copies the payload out and then drops it satisfies every status
# assertion in this file, so this one MEASURES: the probe decodes a message whose
# only field is a 1 MiB blob at an undeclared id and requires the decode to
# allocate a small fraction of it.
#
# max_dyn_blob_len: 8 is set on purpose. The same message is over the receiver cap
# by five orders of magnitude and must still decode COMPLETE -- a skipped field is
# never capped -- while still costing nothing.
echo "==> a S7.3-skipped 1 MiB blob allocates nothing (CORELIB_PLAN S6.2.1/S6.6)"
cat > "$WORK/skipblob.yaml" <<'YAML'
version: 1
messages:
  skipblob:
    payload:
      b: { id: 0, type: blob }
      s: { id: 1, type: string, maxlen: 32 }
YAML
cat > "$WORK/skipblobcfg.yaml" <<'YAML'
generic: { emit: project, max_dyn_blob_len: 8 }
targets: { java: { package: message } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/skipblobcfg.yaml" --lang java \
    --in "$WORK/skipblob.yaml" --out "$WORK/skipblob" )
cp "$ROOT/tests/conformance/java/SkippedBlobAlloc.java" "$WORK/skipblob/src/main/java/message/"
( cd "$WORK/skipblob" && mvn -q -Dsofab.version="$VER" package )
java -cp "$WORK/skipblob/target/harness.jar" message.SkippedBlobAlloc \
    || { echo "FAIL: a skipped blob must not be materialised"; exit 1; }
echo "==> skipped-blob allocation OK"

# A WRAPPER array carries no count header, so the cap has to bind its element
# INDEX instead (generator#387). Gap filling (S5.1) is exactly why: an interior
# element equal to the default is omitted, so the array's length is highest
# present id + 1 and two delivered elements can be a huge container. Capping how
# many elements ARRIVED would bound nothing.
#
# `w` is a string array with no count. Both messages below are produced by the
# UNCAPPED harness and fed to the capped one, and the encoder's own interior
# elision (S2) is what makes them sparse: an element equal to the default is
# omitted, so ["x","","","x"] goes out as ids 0 and 3 and nothing between.
#
#   sparse:  ids 0 and 3 -> length 4, ACCEPTED at cap 4 (highest id is 3).
#   overidx: ids 0 and 4 -> length 5 > cap 4 -> LIMIT_EXCEEDED.
#
# TWO elements are delivered either way. That is the whole point: a cap on how
# many elements ARRIVED would accept both, so the bound has to be the index.
echo "==> wrapper-array element index must be capped (generator#387)"
cat > "$WORK/widx.yaml" <<'YAML'
version: 1
messages:
  wr: { payload: { w: { id: 0, type: array, items: { type: string } } } }
YAML
cat > "$WORK/widxcfg.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
targets: { java: { package: message } }
YAML
build "$WORK/widx.yaml" "$WORK/widx" "$WORK/widxcfg.yaml"
build "$WORK/widx.yaml" "$WORK/widxnolim"
HW="java -jar $WORK/widx/target/harness.jar"
HWN="java -jar $WORK/widxnolim/target/harness.jar"
printf '{"w":["x","","","x"]}'     | $HWN encode wr > "$WORK/widx_sparse.bin"
printf '{"w":["x","","","","x"]}'  | $HWN encode wr > "$WORK/widx_over.bin"
# The premise: both carry exactly two elements, and the second is one byte longer
# only because its element id is one higher.
$HWN decode wr < "$WORK/widx_sparse.bin" >/dev/null || { echo "FAIL: uncapped harness must round-trip the sparse array"; exit 1; }
$HW  decode wr < "$WORK/widx_sparse.bin" >/dev/null || { echo "FAIL: sparse ids 0,3 (length 4 = cap) must decode"; exit 1; }
if $HW decode wr < "$WORK/widx_over.bin" >/dev/null 2>"$WORK/widxerr.txt"; then
    echo "FAIL: element id 4 >= max_dyn_array_count 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/widxerr.txt" || { echo "FAIL: an over-index element is LIMIT_EXCEEDED, not INVALID (CORELIB_PLAN S6.2.1)"; exit 1; }
$HWN decode wr < "$WORK/widx_over.bin" >/dev/null || { echo "FAIL: the same bytes must decode under the default cap"; exit 1; }
echo "==> wrapper index cap OK (sparse accepted, over-index LIMIT_EXCEEDED)"

# A string/blob LENGTH cap is no longer a guard in generated code: it is an
# argument to PayloadAcc.string / PayloadAcc.blob, which compares it against the
# announced `total` before it materializes or buffers a byte (CORELIB_PLAN
# S6.2.1 -- "A corelib MAY take a limit as an argument and perform the check
# itself, and a port that does is conformant"). A deleted guard with nothing in
# its place looks identical in review, so the three rules are pinned END TO END:
#
#   * the cap still REJECTS, with LIMIT_EXCEEDED and not INVALID -- the same
#     bytes decode against a project carrying the target default;
#   * it does NOT reach a field the schema bounds, even when that field's maxlen
#     is far above the cap (S6.2.1 forbids it there; the schema bound governs and
#     ITS breach is INVALID);
#   * it does NOT reach a SKIPPED field -- an over-cap payload at an id this
#     message does not declare, and one whose wire type contradicts the declared
#     one (MESSAGE_SPEC S7.3), both leave the decode COMPLETE.
#
# The last is the one a unit test cannot show and the one that has bitten this
# family before (generator#410, corelib-cpp's pre-guard in front of readString).
echo "==> string/blob length caps travel as an argument (S6.2.1)"
cat > "$WORK/plen.yaml" <<'YAML'
version: 1
messages:
  pl:
    payload:
      ds: { id: 0, type: string }
      bs: { id: 1, type: string, maxlen: 32 }
      db: { id: 2, type: blob }
      n:  { id: 3, type: u32 }
YAML
cat > "$WORK/plencfg.yaml" <<'YAML'
generic: { emit: project, max_dyn_string_len: 8, max_dyn_blob_len: 8 }
targets: { java: { package: message } }
YAML
build "$WORK/plen.yaml" "$WORK/plen" "$WORK/plencfg.yaml"
build "$WORK/plen.yaml" "$WORK/plennolim"
HPC="java -jar $WORK/plen/target/harness.jar"
HPD="java -jar $WORK/plennolim/target/harness.jar"

# Bytes produced by the DEFAULT-cap harness, fed to the capped one.
printf '{"ds":"abcdefghijklmnop"}'                    | $HPD encode pl > "$WORK/pl_ds16.bin"
printf '{"ds":"abcdefgh"}'                            | $HPD encode pl > "$WORK/pl_ds8.bin"
printf '{"bs":"abcdefghijklmnop"}'                    | $HPD encode pl > "$WORK/pl_bs16.bin"
printf '{"bs":"0123456789012345678901234567890123"}'  | $HPD encode pl > "$WORK/pl_bs34.bin"
printf '{"db":[1,2,3,4,5,6,7,8,9,10,11,12]}'          | $HPD encode pl > "$WORK/pl_db12.bin"

if $HPC decode pl < "$WORK/pl_ds16.bin" >/dev/null 2>"$WORK/plerr.txt"; then
    echo "FAIL: a 16-byte unbounded string above max_dyn_string_len 8 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/plerr.txt" || {
    echo "FAIL: an over-cap string is LIMIT_EXCEEDED, not INVALID (S6.2.1); got:"; cat "$WORK/plerr.txt"; exit 1; }
$HPC decode pl < "$WORK/pl_ds8.bin" >/dev/null || { echo "FAIL: 8 bytes AT the cap must decode"; exit 1; }
$HPD decode pl < "$WORK/pl_ds16.bin" >/dev/null || { echo "FAIL: the same bytes must decode under the default cap"; exit 1; }
if $HPC decode pl < "$WORK/pl_db12.bin" >/dev/null 2>"$WORK/plerr.txt"; then
    echo "FAIL: a 12-byte unbounded blob above max_dyn_blob_len 8 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/plerr.txt" || { echo "FAIL: an over-cap blob must carry LIMIT_EXCEEDED"; exit 1; }

# maxlen 32 is FOUR TIMES the cap and still governs: the receiver cap must not be
# applied to a field the schema bounds, and the schema's own breach is INVALID.
OUT=$($HPC decode pl < "$WORK/pl_bs16.bin") || { echo "FAIL: a 16-byte maxlen:32 string must decode under a cap of 8"; exit 1; }
echo "$OUT" | grep -q '"bs":"abcdefghijklmnop"' || { echo "FAIL: the schema-bounded string must survive intact; got: $OUT"; exit 1; }
if $HPC decode pl < "$WORK/pl_bs34.bin" >/dev/null 2>"$WORK/plerr.txt"; then
    echo "FAIL: 34 bytes above maxlen 32 must be INVALID"; exit 1
fi
grep -q "INVALID_MSG\|InvalidMessage" "$WORK/plerr.txt" || {
    echo "FAIL: an over-MAXLEN string is INVALID, never LIMIT_EXCEEDED; got:"; cat "$WORK/plerr.txt"; exit 1; }

# ...and the cap is decided AT THE LENGTH WORD, so a message that ends there is
# a policy rejection and not a truncation (CORELIB_PLAN S6.2.1 "Enforcement
# point": "before the allocation it is meant to prevent"; ARCHITECTURE S9.5: "a
# claimed oversize fails fast even if the payload never arrives"). Every row
# above carries its payload, so all of them reach the string()/blob() callback
# and a cap applied THERE still answers LIMIT_EXCEEDED -- which is why this port
# read as correct while it was not. Take the payload away and that callback never
# fires:
#
#   02 a2 06        id 0 (fixlen), fixlen word (100 << 3) | 2 -- a 100-byte string
#   12 a3 06        id 2 (fixlen), fixlen word (100 << 3) | 3 -- a 100-byte blob
#   02 82 80 80 04  the same shape claiming 1 MiB
#
# Answering INCOMPLETE here loses the category (S6.3 makes the refusal terminal)
# and tells a streaming caller to feed more of a stream this receiver has already
# refused -- five bytes holding a connection open, which is the amplification the
# caps exist to close.
echo "==> an over-cap length word with NO payload is LIMIT_EXCEEDED, not INCOMPLETE"
printf '\002\242\006'         > "$WORK/pl_eof_s.bin"
printf '\022\243\006'         > "$WORK/pl_eof_b.bin"
printf '\002\202\200\200\004' > "$WORK/pl_eof_1m.bin"
for v in pl_eof_s pl_eof_b pl_eof_1m; do
    if $HPC decode pl < "$WORK/$v.bin" >/dev/null 2>"$WORK/plerr.txt"; then
        echo "FAIL: $v -- an over-cap length word then EOF must be refused, not accepted"; exit 1
    fi
    grep -q "LIMIT_EXCEEDED" "$WORK/plerr.txt" || {
        echo "FAIL: $v -- a truncated over-cap header is LIMIT_EXCEEDED, not INCOMPLETE (S6.2.1/S6.3); got:"
        cat "$WORK/plerr.txt"; exit 1; }
done

# The precision controls, and they are the point: the cap must not turn every
# short message into a policy rejection, and it must not reach a field it does
# not govern. tryDecode reports the status instead of throwing, so each of these
# asserts the WORD "INCOMPLETE" rather than merely a clean exit.
#
#   02 42     an IN-cap 8-byte string (= the cap) then EOF -- a clean truncation
#   12 43     the blob twin
#   a2 01 a2 06   a 100-byte string at id 20, an id this message does not declare:
#                 S7.3 skips it and "a skipped field is never capped"
#   0a a2 01  a 20-byte length on bs, whose maxlen is 32: over the receiver cap of
#             8 but inside the schema bound, which S6.2.1 says governs alone
printf '\002\102'             > "$WORK/pl_eof_incap.bin"
printf '\022\103'             > "$WORK/pl_eof_incapb.bin"
printf '\242\001\242\006'   > "$WORK/pl_eof_skip.bin"
printf '\012\242\001'        > "$WORK/pl_eof_bounded.bin"
for v in pl_eof_incap pl_eof_incapb pl_eof_skip pl_eof_bounded; do
    ST=$($HPC trydecode pl < "$WORK/$v.bin" 2>"$WORK/plerr.txt" | head -n1) || {
        echo "FAIL: $v must be a clean truncation, not a rejection; got:"; cat "$WORK/plerr.txt"; exit 1; }
    [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: $v -> $ST (want INCOMPLETE)"; exit 1; }
done
# ...and the schema bound keeps ITS category at that same word: 100 bytes on the
# maxlen-32 field is INVALID_MSG, never the cap's verdict. This is the row that
# proves the enforcement point was always reachable -- it is where the schema
# bound has always fired.
printf '\012\242\006' > "$WORK/pl_bs_eof.bin"
if $HPC decode pl < "$WORK/pl_bs_eof.bin" >/dev/null 2>"$WORK/plerr.txt"; then
    echo "FAIL: 100 bytes above maxlen 32 must be refused at the length word"; exit 1
fi
grep -q "INVALID_MSG" "$WORK/plerr.txt" || {
    echo "FAIL: an over-MAXLEN length word is INVALID_MSG, never LIMIT_EXCEEDED; got:"
    cat "$WORK/plerr.txt"; exit 1; }

# A skipped field is never capped. Both payloads are 16 bytes, twice the cap.
#   a2 01  id 20, wire type fixlen -- an id this message does not declare
#   1a     id 3, wire type fixlen -- declared u32, so S7.3 says skip
#   82 01  fixlen word: 16 bytes, STRING subtype ((16<<3)|2)
#   83 01  fixlen word: 16 bytes, BLOB subtype   ((16<<3)|3)
printf '\242\001\202\001abcdefghijklmnop' > "$WORK/pl_skip_s.bin"
printf '\032\202\001abcdefghijklmnop'     > "$WORK/pl_mis_s.bin"
printf '\242\001\203\001abcdefghijklmnop' > "$WORK/pl_skip_b.bin"
printf '\032\203\001abcdefghijklmnop'     > "$WORK/pl_mis_b.bin"
for v in pl_skip_s pl_mis_s pl_skip_b pl_mis_b; do
    OUT=$($HPC decode pl < "$WORK/$v.bin" 2>"$WORK/plerr.txt") || {
        echo "FAIL: $v -- an over-cap SKIPPED payload must leave the decode COMPLETE; got:"; cat "$WORK/plerr.txt"; exit 1; }
done
# ...and the mis-typed pair must not have touched the field whose id they reused.
OUT=$($HPC decode pl < "$WORK/pl_mis_s.bin")
echo "$OUT" | grep -q '"n":0' || { echo "FAIL: a skipped payload must leave n at its default; got: $OUT"; exit 1; }
echo "==> payload length caps OK (rejects, off schema-bounded fields, off skipped ones)"

# The LATCH -- the one thing generator#461 added that is new logic rather than a
# rename. A refusal is terminal and never comes back as a status, so the
# generated feed records what it MEANT before rethrowing, and Decoder.status()
# answers from that memory. Every reject vector exits non-zero whatever the latch
# recorded, so no amount of vector replay can tell a correct mapping from an
# inverted one, from one that records nothing, or from a catch arm deleted
# outright -- and this backend has TWO arms to get right, because a Visitor
# cannot declare a checked exception, so a generated guard arrives wrapped in an
# UncheckedIOException while the corelib's own raise does not. The harness
# therefore PRINTS the remembered status on its error path and this block reads
# it: malformed bytes are INVALID, a receiver cap is INCOMPLETE and never INVALID
# (CORELIB_PLAN S6.3 -- a policy stop is this side's decision, not a verdict on
# the wire).
echo "==> a refusal latches into the remembered status (generator#461)"
# A varint past the 64-bit bound: 10 continuation bytes and an eleventh. Refused
# by the CORELIB, so it arrives through the BARE SofabException arm (S4.1).
printf '\000\377\377\377\377\377\377\377\377\377\377\001' > "$WORK/varint_overflow.bin"
latch() {   # <fixture> <want-status> <message> <harness...>
    lfx=$1 lwant=$2 lmsg=$3
    shift 3
    if "$@" streamdecode "$lmsg" < "$lfx" >/dev/null 2>"$WORK/latch.err"; then
        echo "FAIL: $(basename "$lfx") must be refused by the streaming decoder"; exit 1
    fi
    grep -q "\[status=$lwant\]" "$WORK/latch.err" || {
        echo "FAIL: $(basename "$lfx") -- the refusal must latch status=$lwant; got:"
        cat "$WORK/latch.err"; exit 1; }
}
latch "$WORK/varint_overflow.bin" INVALID    myfirstmessage $H
latch "$WORK/overcount.bin"       INVALID    myfirstmessage $H
latch "$WORK/pl_bs_eof.bin"       INVALID    pl             $HPC
latch "$WORK/pl_ds16.bin"         INCOMPLETE pl             $HPC
echo "==> refusal latch OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/java/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/target/harness.jar"

# ...and the other direction (generator#444): feed each vector's DENSE bytes
# into a message that declares u64 on the anchors and nothing else, so every
# other field on the wire is an unknown id or a MESSAGE_SPEC S7.3 wire-type
# mismatch and must be SKIPPED -- with the anchor behind it still exact.
#
# Run on BOTH decode surfaces. `streamdecode` drips the message in ONE BYTE PER
# feed, so every position inside every skipped payload becomes a suspend/resume
# boundary; that is where a resync bug the single-buffer path hides shows up
# (generator#456).
echo "==> shared-vector decode conformance (skip matrix)"
for surface in decode streamdecode; do
    python3 "$ROOT/tests/conformance/lib/check_vectors_decode.py" \
        "$CORELIB/assets/test_vectors.json" "Java" --mode "$surface" \
        -- java -jar "$WORK/conf/target/harness.jar"
done

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

# Declared integer width is a VALIDITY bound (MESSAGE_SPEC S7.1 + documentation#32,
# generator#266, Crucible F-0033 / codegen defect G-0026). A value outside the
# declared width is INVALID: it MUST NOT be masked to the width, and MUST NOT be
# kept. someu8 is id 0 (header 0x00 = 0<<3 | unsigned), someu16 is id 1 (0x08).
#   00 ff 7f = 16383 into a u8 -- the reported reproducer
#   00 80 02 = 256   into a u8 -- one past the width
#   08 f0 a2 04 = 70000 into a u16
#   00 ff 01 = 255   into a u8 -- the in-range control: must decode and keep 255
echo "==> over-width scalar must be INVALID (S7.1, generator#266)"
printf '\000\377\177'     > "$WORK/w_u8_16383.bin"
printf '\000\200\002'     > "$WORK/w_u8_256.bin"
printf '\010\360\242\004' > "$WORK/w_u16_70000.bin"
printf '\000\377\001'     > "$WORK/w_u8_255_ctl.bin"
for v in w_u8_16383 w_u8_256 w_u16_70000; do
    if $H decode myfirstmessage < "$WORK/$v.bin" >/dev/null 2>&1; then
        echo "FAIL: $v must be INVALID (S7.1) -- neither masked to the width nor kept"; exit 1
    fi
done
OUT=$($H decode myfirstmessage < "$WORK/w_u8_255_ctl.bin") || { echo "FAIL: in-range control 255 must decode"; exit 1; }
echo "$OUT" | tr -d ' ' | grep -q '"someu8":255' || { echo "FAIL: control must keep 255 exactly; got: $OUT"; exit 1; }

# The same bound on an ARRAY ELEMENT. Worth its own vector because the element
# check does not have to live where the scalar one does: this backend hands a
# schema-bounded array's destination to the corelib whole and checks the declared
# width as one pass afterwards, so an element bound could be dropped without the
# scalar vectors above noticing. someuintarray is id 15, u32[4]; the header is
# (15 << 3) | ARRAY_UNSIGNED(3) = 0x7B, then the element count, then the elements.
#   7b 02 01 80 80 80 80 10 = [1, 4294967296] -- one past the width
#   7b 02 01 ff ff ff ff 0f = [1, 4294967295] -- the in-range control
printf '\173\002\001\200\200\200\200\020' > "$WORK/w_arr_u32_over.bin"
printf '\173\002\001\377\377\377\377\017' > "$WORK/w_arr_u32_ctl.bin"
if $H decode myfirstmessage < "$WORK/w_arr_u32_over.bin" >/dev/null 2>&1; then
    echo "FAIL: an over-width ARRAY ELEMENT must be INVALID (S7.1) -- neither masked nor kept"; exit 1
fi
OUT=$($H decode myfirstmessage < "$WORK/w_arr_u32_ctl.bin") \
    || { echo "FAIL: in-range array control must decode"; exit 1; }
echo "$OUT" | tr -d ' ' | grep -q '"someuintarray":\[1,4294967295\]' \
    || { echo "FAIL: control must keep the array exactly; got: $OUT"; exit 1; }
echo "==> declared-width reject OK (scalar and array element)"

# CORELIB_PLAN S7.2 item 8 -- the shared file's `sequence_growth` block
# (generator#449). A wrapper array carries no element count: its length is
# highest present id + 1, so it GROWS as elements arrive, and the element INDEX
# is what the receiver cap bounds. Two ports that grow differently emit
# IDENTICAL bytes, so no vector can reach this -- the cases are a delivery
# sequence of ids, and the driver builds the message from them.
#
# The index check lives in GENERATED code in every backend, which is why this
# runs here and not only in the corelibs.
echo "==> sequence_growth: a wrapper array grows to its highest id, and the index is the bound"
printf 'version: 1\nmessages:\n' > "$WORK/growth.yaml"
python3 "$ROOT/tests/conformance/lib/check_growth.py" --emit-schema >> "$WORK/growth.yaml"
build "$WORK/growth.yaml" "$WORK/growth" "$WORK/limcfg.yaml"
# --cap must equal the max_dyn_array_count the config above generated with:
# the cases' indices are offsets onto it, so a mismatch moves the boundary.
python3 "$ROOT/tests/conformance/lib/check_growth.py" \
    "$CORELIB/assets/test_vectors.json" "Java" --cap 4 \
    -- java -jar "$WORK/growth/target/harness.jar"

# CORELIB_PLAN S5.2/S6.0/S5.2.3, one property: the verdict AND the decoded value
# must not depend on where the chunks were cut (generator#413). A resume bug -- a
# half-read varint, a payload accumulator that is not carried, a scope stack that
# unwinds one level too far -- is invisible until the split lands in the wrong
# place, and the one-shot `decode` path never suspends at all.
#
# The fixtures are the ones this suite ALREADY built for its negative cases, so
# the table costs nothing but the feeding: every §7.1 reject, every §7.3 skip,
# every truncation, and the controls beside them.
#
# No --oneshot here, unlike the python and dart suites: this backend's one-shot
# `decode` is the documented back-compat BEST-EFFORT surface (ARCHITECTURE §7) --
# it hands back a half-filled object for a truncated message, where
# Decoder.finish() rejects it. Both are correct, and comparing them would report
# that contract difference on every truncation fixture as if it were a chunking
# bug. `trydecode` beside this block is where the one-shot verdict is asserted.
echo "==> a chunk boundary must not change the verdict or the value (generator#413)"
python3 "$ROOT/tests/conformance/lib/check_chunk_invariance.py" "Java" \
    --message myfirstmessage --expect 22 \
    "$WORK/control.bin" "$WORK/overcount.bin" \
    "$WORK/overindex.bin" "$WORK/overindex_control.bin" \
    "$WORK/overmaxlen.bin" "$WORK/overmaxlen_control.bin" \
    "$WORK/overmaxlen_trunc.bin" "$WORK/inmaxlen_trunc.bin" \
    "$WORK/fp64_at_fp32.bin" "$WORK/fp32_overcount.bin" \
    "$WORK/fp_arr_at_scalar.bin" "$WORK/fp_arr_at_scalar_control.bin" \
    "$WORK/wiremismatch.bin" "$WORK/wiremismatch_control.bin" \
    "$WORK/fixsubtype.bin" "$WORK/fixsubtype_control.bin" \
    "$WORK/reopen_struct.bin" "$WORK/reopen_array.bin" \
    "$WORK/skipped_occ_struct.bin" "$WORK/skipped_occ_array.bin" \
    "$WORK/mistyped_array.bin" "$WORK/mistyped_array_overcount.bin" \
    -- $H

echo "PASS"
