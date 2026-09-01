#!/usr/bin/env sh
# Reproducible C# conformance harness: generate -> dotnet build (vs corelib-cs)
# -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/csharp/run.sh [corelib-cs]   (or set $SOFAB_CS_CORELIB)
# Requires: go, dotnet, git, python3.
set -eu

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_CS_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1

if [ -z "$CORELIB" ]; then
    clone_corelib corelib-cs "$WORK/corelib"
    CORELIB="$WORK/corelib"
fi
export SOFAB_CS_CORELIB="$CORELIB"
echo "==> corelib-cs: $CORELIB"

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project }
targets: { csharp: { namespace: Sofabuffers } }
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
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang csharp --in "$1" --out "$2" )
    ( cd "$2" && dotnet build -v q >/dev/null )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
H="dotnet $WORK/ex/bin/Debug/net9.0/harness.dll"
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

# Contradictory ARRAY wire type at a scalar id (MESSAGE_SPEC S7.3, generator#183):
# the array wire types are wire types like any other, so an integer ARRAY header
# at an id declared as a scalar integer is just as contradictory as a signed
# header at an unsigned field and MUST be SKIPPED. corelib-cs delivers array
# elements one-by-one through the same Unsigned/Signed callbacks a lone scalar
# uses, so this is the one contradiction the (scope, id) dispatch cannot see on
# its own -- the generated visitor arms a skip counter from the ArrayBegin count.
# someu8 (id 0, default 7): 03 = id 0 with wire type 3 (unsigned ARRAY), 01 count,
# 05 element -> must stay 7, NOT become 5.
# somei8 (id 4, default 10): 24 = id 4 with wire type 4 (signed ARRAY), 01 count,
# 06 element (zig-zag 3) -> must stay 10, NOT become 3.
# Controls: 21 06 is id 4 with the correct SIGNED scalar wire type (-> 3), and
# 7b 04 01 02 03 04 is someuintarray (id 15, count 4) legitimately declaring an
# unsigned array -> [1,2,3,4], which must never be disarmed by the skip counter.
echo "==> array wire type at a scalar id must skip (MESSAGE_SPEC S7.3, generator#183)"
printf '\003\001\005' > "$WORK/arr_at_u8.bin"
printf '\044\001\006' > "$WORK/arr_at_i8.bin"
printf '\041\006' > "$WORK/arr_at_i8_control.bin"
printf '\173\004\001\002\003\004' > "$WORK/arr_legit.bin"
OUT=$($H decode myfirstmessage < "$WORK/arr_at_u8.bin") \
    || { echo "FAIL: unsigned array at a scalar u8 id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped array must leave someu8 at its default 7; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/arr_at_i8.bin") \
    || { echo "FAIL: signed array at a scalar i8 id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somei8":10' || { echo "FAIL: skipped array must leave somei8 at its default 10; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/arr_at_i8_control.bin") \
    || { echo "FAIL: control (correct signed scalar wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"somei8":3' || { echo "FAIL: control must decode somei8 to 3; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/arr_legit.bin") \
    || { echo "FAIL: control (declared unsigned array) must decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2,3,4\]' || { echo "FAIL: a declared integer array must still decode to [1,2,3,4]; got: $OUT"; exit 1; }
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
# the fp analogue of the integer case above. corelib-cs streams a fixlen (fp) array
# element-by-element through the very Fp32()/Fp64() callbacks a lone scalar uses, so
# without the ArrayBegin-armed skip counter the element would land in the scalar's
# arm. somefp64 (id 9, declared fp64, default 3.141592653589793) receives an fp64
# ARRAY and must be skipped whole.
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

# Receiver-side decode limits (generator#102): `a` is a count-less array
# (id 0 -> header 0x03 = 0<<3 | unsigned-array), so a configured
# max_dyn_array_count: 4 makes a wire count of 5 fail decode with
# LimitExceeded (non-zero exit) at the count header; exactly 4 still decode,
# and the same oversized bytes decode fine against a project generated with no
# key set -- which is the TARGET DEFAULT, not unlimited (generator#385): 5
# elements is far under it, which is the point of a default sized as an
# amplification barrier rather than as application policy.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg-limit.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limit.yaml" --lang csharp --in "$WORK/dyn.yaml" --out "$WORK/dynlim" )
( cd "$WORK/dynlim" && dotnet build -v q >/dev/null )
build "$WORK/dyn.yaml" "$WORK/dynfree"
HL="dotnet $WORK/dynlim/bin/Debug/net9.0/harness.dll"
HF="dotnet $WORK/dynfree/bin/Debug/net9.0/harness.dll"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if $HL decode dyn < "$WORK/overlimit.bin" >/dev/null 2>&1; then
    echo "FAIL: 5 elements above max_dyn_array_count 4 must fail decode"; exit 1
fi
$HL decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: 4 elements at the limit must decode"; exit 1; }
$HF decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: default-cap project must decode the oversized message"; exit 1; }
echo "==> decode limits OK"

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
# by five orders of magnitude and must still decode Complete -- a skipped field is
# never capped -- while still costing nothing.
#
# The probe REPLACES the generated Program.cs: the project's own Main is the JSON
# harness, and this needs its own entry point in the same assembly.
echo "==> a S7.3-skipped 1 MiB blob allocates nothing (CORELIB_PLAN S6.2.1/S6.6)"
cat > "$WORK/skipblob.yaml" <<'YAML'
version: 1
messages:
  skipblob:
    payload:
      b: { id: 0, type: blob }
      s: { id: 1, type: string, maxlen: 32 }
YAML
cat > "$WORK/cfg-skipblob.yaml" <<'YAML'
generic: { emit: project, max_dyn_blob_len: 8 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-skipblob.yaml" --lang csharp \
    --in "$WORK/skipblob.yaml" --out "$WORK/skipblob" )
rm "$WORK/skipblob/Program.cs"
cp "$ROOT/tests/conformance/csharp/SkippedBlobAlloc.cs" "$WORK/skipblob/"
( cd "$WORK/skipblob" && dotnet build -v q >/dev/null )
dotnet "$WORK/skipblob/bin/Debug/net9.0/harness.dll" \
    || { echo "FAIL: a skipped blob must not be materialised"; exit 1; }
echo "==> skipped-blob allocation OK"

# The string/blob half of the same rule, and the half a diff cannot check
# (CORELIB_PLAN 6.2.1, corelib-cs#101). The generated guard in front of the
# payload callback is gone: the cap now travels INTO PayloadAcc.String/.Blob,
# which compares `total` against it at the length header before it takes a byte.
# A removed guard with nothing behind it reads identically in review, so this
# runs the four cases that tell the two apart.
#
#   ds (id 0, no maxlen)   -> capped at max_dyn_string_len: 8
#   bs (id 1, maxlen: 32)  -> SCHEMA-bounded, and bounded ABOVE the cap
#   db (id 2, no maxlen)   -> capped at max_dyn_blob_len: 8
#
# Wire: header = (id<<3)|2 (fixlen), then a fixlen word = (len<<3)|subtype with
# subtype 2 = string, 3 = blob, then the payload bytes (0x61 = 'a').
echo "==> receiver caps on string/blob length (CORELIB_PLAN 6.2.1)"
cat > "$WORK/caps.yaml" <<'YAML'
version: 1
messages:
  caps:
    payload:
      ds: { id: 0, type: string }
      bs: { id: 1, type: string, maxlen: 32 }
      db: { id: 2, type: blob }
YAML
cat > "$WORK/cfg-caps.yaml" <<'YAML'
generic: { emit: project, max_dyn_string_len: 8, max_dyn_blob_len: 8 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-caps.yaml" --lang csharp --in "$WORK/caps.yaml" --out "$WORK/caps" )
( cd "$WORK/caps" && dotnet build -v q >/dev/null )
HP="dotnet $WORK/caps/bin/Debug/net9.0/harness.dll"

# 1. 9 bytes into the uncapped-by-schema string: one over max_dyn_string_len 8.
printf '\002\112aaaaaaaaa' > "$WORK/cap_str_over.bin"
# 2. exactly 8: at the cap, still decodes -- rejected, never clamped.
printf '\002\102aaaaaaaa' > "$WORK/cap_str_at.bin"
# 3. 16 bytes into bs, whose schema maxlen is 32: OVER the receiver cap but
#    inside the schema bound. 6.2.1 forbids a cap on a field the schema bounds,
#    so this must decode -- it is the case a single global cap gets wrong.
printf '\012\202\001aaaaaaaaaaaaaaaa' > "$WORK/cap_str_bounded.bin"
# 4. 9 bytes at id 7, which this message does not declare: the field is SKIPPED,
#    and "a skipped field is never capped" (6.2.1). Must decode COMPLETE.
printf '\072\112aaaaaaaaa' > "$WORK/cap_str_skipped.bin"
# 5-6. the blob twin: over the cap at the declared id, and over it at a skipped one.
printf '\022\113aaaaaaaaa' > "$WORK/cap_blob_over.bin"
printf '\072\113aaaaaaaaa' > "$WORK/cap_blob_skipped.bin"

# The CATEGORY is asserted, not just the failure: the bytes are well-formed and
# the same message decodes under a looser cap, so this is LimitExceeded and never
# InvalidMessage (CORELIB_PLAN 6.3). Asserting only a non-zero exit would pass on
# any exception at all -- including the one a cap-free build cannot throw.
for v in cap_str_over cap_blob_over; do
    if $HP decode caps < "$WORK/$v.bin" >/dev/null 2>"$WORK/$v.err"; then
        echo "FAIL: $v (9 bytes over a cap of 8) must fail decode"; exit 1
    fi
    grep -q "LimitExceeded" "$WORK/$v.err" || {
        echo "FAIL: $v must be refused as LimitExceeded, not as malformed input; got:"
        cat "$WORK/$v.err"; exit 1; }
done
OUT=$($HP decode caps < "$WORK/cap_str_at.bin") || { echo "FAIL: 8 bytes at the cap must decode"; exit 1; }
echo "$OUT" | grep -q '"ds":"aaaaaaaa"' || { echo "FAIL: at-cap value must arrive whole, never clamped; got: $OUT"; exit 1; }
OUT=$($HP decode caps < "$WORK/cap_str_bounded.bin") \
    || { echo "FAIL: a schema-bounded field must not meet the receiver cap (6.2.1)"; exit 1; }
echo "$OUT" | grep -q '"bs":"aaaaaaaaaaaaaaaa"' || { echo "FAIL: bounded 16-byte value lost; got: $OUT"; exit 1; }
$HP decode caps < "$WORK/cap_str_skipped.bin" >/dev/null \
    || { echo "FAIL: an over-cap string at an undeclared id is SKIPPED, never capped (6.2.1)"; exit 1; }
$HP decode caps < "$WORK/cap_blob_skipped.bin" >/dev/null \
    || { echo "FAIL: an over-cap blob at an undeclared id is SKIPPED, never capped (6.2.1)"; exit 1; }
# ...and the cap is decided AT THE LENGTH WORD, so a message that ends there is a
# policy rejection and not a truncation (CORELIB_PLAN 6.2.1 "Enforcement point":
# "before the allocation it is meant to prevent"; ARCHITECTURE 9.5: "a claimed
# oversize fails fast even if the payload never arrives"). Every row above
# carries its payload, so all of them reach String()/Blob() and a cap applied
# THERE still answers LimitExceeded -- which is why this port read as correct
# while it was not. Take the payload away and that callback never fires:
#
#   02 a2 06        id 0 (fixlen), fixlen word (100 << 3) | 2 -- a 100-byte string
#   12 a3 06        id 2 (fixlen), fixlen word (100 << 3) | 3 -- a 100-byte blob
#   02 82 80 80 04  the same shape claiming 1 MiB
#
# Answering Incomplete here loses the category (6.3 makes the refusal terminal)
# and tells a streaming caller to feed more of a stream this receiver has already
# refused -- five bytes holding a connection open, the amplification the caps
# exist to close.
echo "==> an over-cap length word with NO payload is LimitExceeded, not Incomplete"
printf '\002\242\006'           > "$WORK/cap_eof_str.bin"
printf '\022\243\006'           > "$WORK/cap_eof_blob.bin"
printf '\002\202\200\200\004' > "$WORK/cap_eof_1m.bin"
for v in cap_eof_str cap_eof_blob cap_eof_1m; do
    if $HP decode caps < "$WORK/$v.bin" >/dev/null 2>"$WORK/$v.err"; then
        echo "FAIL: $v -- an over-cap length word then EOF must be refused, not accepted"; exit 1
    fi
    grep -q "LimitExceeded" "$WORK/$v.err" || {
        echo "FAIL: $v -- a truncated over-cap header is LimitExceeded, not Incomplete (6.2.1/6.3); got:"
        cat "$WORK/$v.err"; exit 1; }
done

# The precision controls, and they are the point: the cap must not turn every
# short message into a policy rejection, and it must not reach a field it does
# not govern. TryDecode reports the status instead of throwing, so each asserts
# the WORD rather than merely a clean exit.
#
#   02 42          an IN-cap 8-byte string (= the cap) then EOF -- a clean truncation
#   12 43          the blob twin
#   a2 01 a2 06    a 100-byte string at id 20, an id this message does not declare:
#                  7.3 skips it and "a skipped field is never capped"
#   0a a2 01       a 20-byte length on bs, whose maxlen is 32: over the receiver cap
#                  of 8 but inside the schema bound, which 6.2.1 says governs alone
printf '\002\102'         > "$WORK/cap_eof_incap.bin"
printf '\022\103'         > "$WORK/cap_eof_incapb.bin"
printf '\242\001\242\006' > "$WORK/cap_eof_skip.bin"
printf '\012\242\001'    > "$WORK/cap_eof_bounded.bin"
for v in cap_eof_incap cap_eof_incapb cap_eof_skip cap_eof_bounded; do
    ST=$($HP trydecode caps < "$WORK/$v.bin" 2>"$WORK/$v.err" | head -n1) || {
        echo "FAIL: $v must be a clean truncation, not a rejection; got:"; cat "$WORK/$v.err"; exit 1; }
    [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: $v -> $ST (want INCOMPLETE)"; exit 1; }
done
# ...and the schema bound keeps ITS category at that same word: 100 bytes on the
# maxlen-32 field is InvalidMessage, never the cap's verdict. This is the row that
# proves the enforcement point was always reachable -- it is where the schema
# bound has always fired.
printf '\012\242\006' > "$WORK/cap_eof_maxlen.bin"
if $HP decode caps < "$WORK/cap_eof_maxlen.bin" >/dev/null 2>"$WORK/cap_eof_maxlen.err"; then
    echo "FAIL: 100 bytes above maxlen 32 must be refused at the length word"; exit 1
fi
grep -q "InvalidMessage" "$WORK/cap_eof_maxlen.err" || {
    echo "FAIL: an over-MAXLEN length word is InvalidMessage, never LimitExceeded; got:"
    cat "$WORK/cap_eof_maxlen.err"; exit 1; }

echo "==> string/blob caps OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/csharp/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/bin/Debug/net9.0/harness.dll"

echo "==> §7 decode status through the generated API (generator#105)"
HC="dotnet $WORK/conf/bin/Debug/net9.0/harness.dll"
ST=$(printf '\200' | $HC trydecode vecu | head -n1)   # lone 0x80: dangling varint
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: lone 0x80 -> $ST (want INCOMPLETE)"; exit 1; }
ST=$(printf '' | $HC trydecode vecu | head -n1)       # empty message: valid
[ "$ST" = "COMPLETE" ] || { echo "FAIL: empty message -> $ST (want COMPLETE)"; exit 1; }
echo "==> TryDecode status OK (0x80 INCOMPLETE, empty COMPLETE)"

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    build "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

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
echo "==> declared-width reject OK"

echo "PASS"
