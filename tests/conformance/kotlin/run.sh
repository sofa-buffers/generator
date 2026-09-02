#!/usr/bin/env sh
# Reproducible Kotlin conformance harness: publish the corelib to the local
# Maven repo, generate -> gradle installDist -> round-trip -> byte-exact
# shared-vector conformance.
#
# Usage: tests/conformance/kotlin/run.sh [corelib-kotlin-mp]
#        (or set $SOFAB_KOTLIN_CORELIB)
# Requires: go, git, python3, and a JDK the Kotlin Gradle plugin supports (17..24;
# 21 is what CI uses). Gradle comes from the corelib's wrapper, so no system
# Gradle is needed.
#
# The plugin refuses to run on a JDK newer than it knows, and this repo's
# devcontainer deliberately keeps a NEWER one as the default (moving JAVA_HOME
# would rebuild the java and csharp bench rows on another runtime). So the JDK is
# taken from SOFAB_KOTLIN_JDK when set -- the devcontainer exports it, and
# tests/bench/lang/kotlin.sh reads the same knob -- falling back to JAVA_HOME and
# then to the JVM on PATH.
set -eu

if [ -n "${SOFAB_KOTLIN_JDK:-}" ]; then
    JAVA_HOME="$SOFAB_KOTLIN_JDK"
    export JAVA_HOME
    PATH="$JAVA_HOME/bin:$PATH"
    export PATH
fi

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_KOTLIN_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    clone_corelib corelib-kotlin-mp "$WORK/corelib"
    CORELIB="$WORK/corelib"
fi
CORELIB=$(cd "$CORELIB" && pwd)
echo "==> corelib-kotlin-mp: $CORELIB"
GRADLEW="$CORELIB/gradlew"
VER=$(sed -n 's/^version = "\(.*\)"$/\1/p' "$CORELIB/build.gradle.kts" | head -1)
echo "==> publishing corelib-kotlin-mp $VER to the local Maven repo"
# Only the JVM target and the multiplatform root module: the harness is a JVM
# project, and publishing the native targets would pull the whole Kotlin/Native
# toolchain for artifacts nothing here resolves.
( cd "$CORELIB" && "$GRADLEW" --console=plain -q \
    publishJvmPublicationToMavenLocal publishKotlinMultiplatformPublicationToMavenLocal )

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project }
targets: { kotlin: { package: message } }
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
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang kotlin --in "$1" --out "$2" )
    ( cd "$2" && "$GRADLEW" --console=plain -q -Psofab.version="$VER" installDist )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
H="$WORK/ex/build/install/harness/bin/harness"
OUT=$(printf '%s' "$IN" | $H encode myfirstmessage | $H decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Both streaming halves, against the one-shot pair as the oracle (PLAN S5.6).
# The `stream` harness mode encodes through a ONE-BYTE output window with a flush
# sink and asserts the bytes are identical to encode()'s -- the corelib splits
# every atomic unit, so any buffer at or above MIN_OUTPUT_BUFFER must produce the
# one-shot bytes, and that is what makes a message larger than RAM encodable.
# It then feeds those bytes to the generated Decoder ONE AT A TIME, so decoder
# state has to survive a boundary at every byte offset, and prints the result as
# JSON -- which must equal what the one-shot decode produced.
echo "==> streaming: 1-byte encode window + 1-byte decode chunks"
STREAMED=$(printf '%s' "$IN" | $H stream myfirstmessage) \
    || { echo "FAIL: streaming encode/decode must succeed"; exit 1; }
[ "$STREAMED" = "$OUT" ] \
    || { echo "FAIL: streaming differs from the one-shot pair"; printf 'one-shot: %s\nstream:   %s\n' "$OUT" "$STREAMED"; exit 1; }
echo "==> streaming OK"

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
#   count is not this field's count -- it must NOT be measured against 3.
#
#   fp32 header, count 5 at the same id: the subtype matches, so this really is
#   the field's count and the schema bound applies -- still INVALID (S3+S7).
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
# format does not have -- and generated Kotlin could not notice, since its array
# arm only asks whether the announced kind is the one it declared and returns
# quietly when it is not. The fixlen_word never reaches it; the corelib decides
# at the word.
#
# One shared driver for all eleven suites (ARCHITECTURE S12). It derives every
# fixture from the schema's own somefloatarray declaration, so the ids it writes
# and the values it asserts cannot drift from what the harness was built with,
# and it compares the skipped field's default as JSON numbers rather than by
# grep, which is what lets one table serve backends that render it three ways.
echo "==> a string/blob/reserved fixlen-array subtype is INVALID (generator#411)"
python3 "$ROOT/tests/conformance/lib/check_fixlen_array_subtype.py" "kotlin" \
    --invalid-pattern 'INVALID_MSG' \
    -- "$H"

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

# Over-maxlen scalar blob (MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62
# (blob id 12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is
# 16 bytes.
echo "==> over-maxlen string/blob must reject (S7.1)"
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
#   62      blob, field id 12 ((12<<3)|2)
#   8b 01   fixlen word: byte length 17, blob subtype -> ((17<<3)|3)
#   <EOF>   not one payload byte
#
# The two verdicts arrive on DIFFERENT channels, which is this backend's
# documented contract rather than an accident: tryDecode RETURNS
# COMPLETE/INCOMPLETE and malformed input THROWS. So the assertion is that the
# over-bound message throws (INVALID) while the in-bound one returns INCOMPLETE
# -- and the second half is what makes this an ordering check rather than a
# blanket reject.
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#267)"
printf '\142\213\001' > "$WORK/overmaxlen_trunc.bin"
printf '\142\203\001' > "$WORK/inmaxlen_trunc.bin"
if $H trydecode myfirstmessage < "$WORK/overmaxlen_trunc.bin" >/dev/null 2>"$WORK/omt.err"; then
    echo "FAIL: over-maxlen(17>16)+truncated must be INVALID, not a returned status"; exit 1
fi
grep -q "INVALID_MSG" "$WORK/omt.err" || {
    echo "FAIL: over-maxlen(17>16)+truncated must report INVALID; got:"; cat "$WORK/omt.err"; exit 1; }
ST=$($H trydecode myfirstmessage < "$WORK/inmaxlen_trunc.bin" | sed -n 1p)
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(16==16)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> maxlen/truncation ordering OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose
# header wire type is not the one its declared type maps to -- for fixlen,
# including the subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0)
# is declared u8 and keeps its schema default 7. Wire: 01 = id 0 with wire type
# SIGNED (1), then the zig-zag varint 06 (= 3). Control: 00 09 is the same id
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

# Contradictory ARRAY wire type at a SCALAR id (MESSAGE_SPEC S7.3,
# generator#183): the corelib delivers array elements one-by-one through the very
# unsigned()/signed() callbacks a lone scalar uses, so a generated visitor that
# dispatched on the id alone would store the elements instead of skipping them.
# someu8 (id 0) is declared u8 and MUST keep its default 7; somei8 (id 4) is
# declared i8 and MUST keep its default 10.
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
OUT=$($H decode myfirstmessage < "$WORK/control.bin") \
    || { echo "FAIL: declared array control must decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2,3,4\]' || { echo "FAIL: declared array must still decode to [1,2,3,4]; got: $OUT"; exit 1; }
echo "==> array-at-scalar skip OK"

# MIS-TYPED ARRAY KIND at an ARRAY-declared id (MESSAGE_SPEC S7.3,
# generator#254): someuintarray (id 15) declares u32[4] -> ARRAY_UNSIGNED; a
# header carrying ARRAY_SIGNED at that id MUST be skipped whole -- which includes
# NOT RESIZING the declared field from the skipped header's count.
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

# fp ARRAY delivered to a SCALAR-declared fp id (MESSAGE_SPEC S7.3,
# generator#193): the fp analogue of the integer case above. somefp64 (id 9,
# default 3.141592653589793) receives an fp64 ARRAY and must be skipped whole.
printf '\115\001\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/fp_arr_at_scalar.bin") \
    || { echo "FAIL: fp array at a scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fp array must leave somefp64 at its default; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/fp_arr_at_scalar_control.bin") \
    || { echo "FAIL: control (correct scalar fixlen wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode somefp64 to 2.5; got: $OUT"; exit 1; }
echo "==> fp array-at-scalar skip OK"

# Repeated field id (MESSAGE_SPEC S7.4, generator#175): a re-opened sequence
# CONTINUES its scope, so a struct merges and children an earlier opening set
# whose ids do not recur are retained.
echo "==> re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
OUT=$($H decode myfirstmessage < "$WORK/reopen_struct.bin") \
    || { echo "FAIL: re-opened struct must decode"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: re-opened struct must retain nestedstring \"x\"; got: $OUT"; exit 1; }
echo "==> struct scope merge OK"

# Repeated field id, array wrapper (MESSAGE_SPEC S7.4 + S5): an array wrapper IS
# the array's value, so unlike a struct it is REPLACED whole by a later
# occurrence rather than merged.
echo "==> re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
OUT=$($H decode myfirstmessage < "$WORK/reopen_array.bin") \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: re-opened array wrapper must start with the second opening's element 0 == \"c\"; got: $OUT"; exit 1; }
if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
    echo "FAIL: re-opened array wrapper must be replaced, not merged (element \"b\" survived); got: $OUT"; exit 1
fi
echo "==> array wrapper replace OK"

# Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174): a header carrying
# the right Fixlen wire type but the WRONG subtype is just as contradictory as a
# wrong wire type and MUST be SKIPPED like an unknown id.
echo "==> fixlen subtype mismatch must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\112\012\170' > "$WORK/fixsubtype.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fixsubtype_control.bin"
OUT=$($H decode myfirstmessage < "$WORK/fixsubtype.bin") \
    || { echo "FAIL: mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default; got: $OUT"; exit 1; }
OUT=$($H decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
    || { echo "FAIL: control (correct fp64 subtype) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
echo "==> fixlen subtype skip OK"

# S7.3 x S7.4, array wrapper: "An occurrence skipped under S7.3 is not an
# occurrence for this clause: a correctly typed earlier occurrence survives a
# mis-typed later one." The failure this guards is an EMPTY array, i.e. generated
# code clearing the wrapper before it checks the wire type.
echo "==> mis-typed later occurrence must not clear the array (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\007\220\001\005' > "$WORK/skipped_occ_array.bin"
OUT=$($H decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["a"' || { echo "FAIL: skipped occurrence must not clear the array (element 0 == \"a\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps array OK"

# S7.3 x S7.4, struct: same rule for a struct scope.
echo "==> mis-typed later occurrence must not clear the struct (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\240\001\005' > "$WORK/skipped_occ_struct.bin"
OUT=$($H decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

# Receiver-side decode limits (generator#102): `a` is an UNBOUNDED u64 array
# (id 0 -> header 0x03). With max_dyn_array_count: 4 a wire count of 5 MUST fail
# with LIMIT_EXCEEDED (checked at the count header, before allocation); exactly 4
# still decodes; and the same 5-element bytes MUST decode against a project with
# no key set, which carries the TARGET DEFAULT rather than no cap
# (generator#385).
#
# The other three fields cover the caps this backend hands to the CORELIB rather
# than comparing itself (CORELIB_PLAN §6.2.1): a payload length goes to
# PayloadAcc.string/blob and a wrapper row index to Seq.reserveRow*. A removed
# guard with nothing replacing it reads exactly like a working one in a diff, so
# each is exercised over the wire, at the cap and one past it. `bs` is the
# exclusivity case: a schema `maxlen: 6` under a `max_dyn_string_len: 4` must
# still decode six bytes -- the receiver cap may not touch a bounded field -- and
# seven must fail as INVALID_MSG, the other category.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/lim.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      a:   { id: 0, type: array, items: { type: u64 } }
      s:   { id: 1, type: string }
      b:   { id: 2, type: blob }
      mat: { id: 3, type: array, items: { type: array, items: { type: u64 } } }
      bs:  { id: 4, type: string, maxlen: 6 }
YAML
cat > "$WORK/limcfg.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4, max_dyn_string_len: 4, max_dyn_blob_len: 4 }
targets: { kotlin: { package: message } }
YAML
build "$WORK/lim.yaml" "$WORK/lim" "$WORK/limcfg.yaml"
build "$WORK/lim.yaml" "$WORK/nolim"
HL="$WORK/lim/build/install/harness/bin/harness"
HN="$WORK/nolim/build/install/harness/bin/harness"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if $HL decode dyn < "$WORK/overlimit.bin" >/dev/null 2>"$WORK/limerr.txt"; then
    echo "FAIL: dyn array count 5 above max_dyn_array_count 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/limerr.txt" || { echo "FAIL: rejection must carry LIMIT_EXCEEDED"; exit 1; }
$HL decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: count 4 at the limit must decode"; exit 1; }
$HN decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: default-cap project must decode 5 elements"; exit 1; }

# CORELIB_PLAN §6.2.1, "a skipped field is never capped": a limit bounds an
# ALLOCATION, and a field MESSAGE_SPEC §7.3 skips is walked, not materialised, so
# no cap may reach it. A decode that steps over an over-cap field it was never
# going to read stays COMPLETE (generator#410).
#
# §7.3 skips two shapes and they fail independently, so both run:
#
#   04 05 …  id 0 IS declared, but as array<u64> -- UNSIGNED. A SIGNED array
#            header (wire type 4) there was never this field's value.
#   4b 05 …  id 9, an id this message does not declare at all.
#
# Both carry a count of 5, one over max_dyn_array_count: 4. What keeps them
# COMPLETE is ordering: the emitted arrayBegin arms the skip counter FIRST, and
# the cap sits inside `when (cur) { <idx> -> if (kind == ArrayKind.Unsigned) { … } }`
# -- behind the id dispatch and behind the kind test. Hoist it out of the `when`,
# or widen the arm, and both rows answer LIMIT_EXCEEDED while every assertion
# above still passes, because the cap is still there and still fires on the
# control. tryDecode is what reads the CATEGORY (§6.3) rather than an exit status.
echo "==> a §7.3-skipped field is never capped (CORELIB_PLAN §6.2.1, generator#410)"
printf '\004\005\000\000\000\000\000' > "$WORK/skipmistyped.bin"
printf '\113\005\001\001\001\001\001' > "$WORK/skipunknown.bin"
for v in skipmistyped skipunknown; do
    ST=$($HL trydecode dyn < "$WORK/$v.bin" 2>"$WORK/$v.err" | head -n1) || {
        echo "FAIL: $v -- an over-cap SKIPPED array must not be rejected; got:"; cat "$WORK/$v.err"; exit 1; }
    [ "$ST" = "COMPLETE" ] || { echo "FAIL: $v -> $ST (want COMPLETE)"; exit 1; }
    # ...and skipped means untouched: `a` must keep its default, not be resized
    # from the skipped header's count (§7.3, §7.4).
    OUT=$($HL decode dyn < "$WORK/$v.bin")
    echo "$OUT" | grep -q '"a":\[\]' || { echo "FAIL: $v -- a skipped field must bind nothing; got: $OUT"; exit 1; }
done
# The control that keeps the pair honest: the SAME count at the SAME id with the
# MATCHING kind is this field's own count and is still the policy category. A
# backend that simply stopped capping would pass both rows above. The cap is
# raised by the generated visitor, which tryDecode does not catch, so the
# CATEGORY is read off the exception rather than off a status word.
if $HL trydecode dyn < "$WORK/overlimit.bin" >/dev/null 2>"$WORK/ctlcap.err"; then
    echo "FAIL: the matching-kind over-cap control must still be refused"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/ctlcap.err" \
    || { echo "FAIL: the control must stay LIMIT_EXCEEDED, not malformed input; got:"; cat "$WORK/ctlcap.err"; exit 1; }
echo "==> skipped-field cap exclusivity OK"

# Unbounded string, id 1: header 0x0a, fixlen_word (len<<3)|2. Five bytes is one
# over max_dyn_string_len: 4; four is exactly at it.
printf '\012\052\141\141\141\141\141' > "$WORK/overstr.bin"
printf '\012\042\141\141\141\141' > "$WORK/atstr.bin"
if $HL decode dyn < "$WORK/overstr.bin" >/dev/null 2>"$WORK/serr.txt"; then
    echo "FAIL: string length 5 above max_dyn_string_len 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/serr.txt" || { echo "FAIL: over-cap string must carry LIMIT_EXCEEDED"; cat "$WORK/serr.txt"; exit 1; }
$HL decode dyn < "$WORK/atstr.bin" >/dev/null || { echo "FAIL: string length 4 at the cap must decode"; exit 1; }
$HN decode dyn < "$WORK/overstr.bin" >/dev/null || { echo "FAIL: default-cap project must decode a 5-byte string"; exit 1; }

# Unbounded blob, id 2: header 0x12, fixlen_word (len<<3)|3.
printf '\022\053\001\001\001\001\001' > "$WORK/overblob.bin"
printf '\022\043\001\001\001\001' > "$WORK/atblob.bin"
if $HL decode dyn < "$WORK/overblob.bin" >/dev/null 2>"$WORK/berr.txt"; then
    echo "FAIL: blob length 5 above max_dyn_blob_len 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/berr.txt" || { echo "FAIL: over-cap blob must carry LIMIT_EXCEEDED"; cat "$WORK/berr.txt"; exit 1; }
$HL decode dyn < "$WORK/atblob.bin" >/dev/null || { echo "FAIL: blob length 4 at the cap must decode"; exit 1; }
$HN decode dyn < "$WORK/overblob.bin" >/dev/null || { echo "FAIL: default-cap project must decode a 5-byte blob"; exit 1; }

# Dynamic matrix, id 3: sequence start 0x1e, then one ROW whose element id IS its
# index -- 0x23 is index 4 (over max_dyn_array_count: 4), 0x1b is index 3 (at the
# last accepted slot). One element each, so only the INDEX is in question.
printf '\036\043\001\001\007' > "$WORK/overrow.bin"
printf '\036\033\001\001\007' > "$WORK/atrow.bin"
if $HL decode dyn < "$WORK/overrow.bin" >/dev/null 2>"$WORK/rerr.txt"; then
    echo "FAIL: matrix row index 4 above max_dyn_array_count 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/rerr.txt" || { echo "FAIL: over-cap row index must carry LIMIT_EXCEEDED"; cat "$WORK/rerr.txt"; exit 1; }
$HL decode dyn < "$WORK/atrow.bin" >/dev/null || { echo "FAIL: matrix row index 3 at the cap must decode"; exit 1; }
$HN decode dyn < "$WORK/overrow.bin" >/dev/null || { echo "FAIL: default-cap project must decode row index 4"; exit 1; }

# The exclusivity rule (§6.2.1): `bs` declares maxlen 6, so the configured cap of
# 4 must not reach it -- six bytes decode. Seven is over the SCHEMA bound and is
# INVALID_MSG, never the policy category.
printf '\042\062\141\141\141\141\141\141' > "$WORK/bsok.bin"
printf '\042\072\141\141\141\141\141\141\141' > "$WORK/bsbad.bin"
$HL decode dyn < "$WORK/bsok.bin" >/dev/null \
    || { echo "FAIL: a schema-bounded string must not be governed by the receiver cap"; exit 1; }
if $HL decode dyn < "$WORK/bsbad.bin" >/dev/null 2>"$WORK/bserr.txt"; then
    echo "FAIL: string length 7 above the declared maxlen 6 must be rejected"; exit 1
fi
grep -q "INVALID_MSG" "$WORK/bserr.txt" || { echo "FAIL: over-maxlen must be INVALID_MSG, not a policy rejection"; cat "$WORK/bserr.txt"; exit 1; }
# ...and every one of those caps is decided AT THE LENGTH WORD, so a message that
# ends there is a policy rejection and not a truncation (CORELIB_PLAN §6.2.1
# "Enforcement point": "before the allocation it is meant to prevent";
# ARCHITECTURE §9.5: "a claimed oversize fails fast even if the payload never
# arrives"). Every string/blob row above carries its payload, so all of them reach
# the string()/blob() callback and a cap applied THERE still answers
# LIMIT_EXCEEDED -- which is why this port read as correct while it was not. Take
# the payload away and that callback never fires:
#
#   0a a2 06        id 1 (fixlen), fixlen word (100 << 3) | 2 -- a 100-byte string
#   12 a3 06        id 2 (fixlen), fixlen word (100 << 3) | 3 -- a 100-byte blob
#   0a 82 80 80 04  the same shape claiming 1 MiB
#
# Answering INCOMPLETE here loses the category (§6.3 makes the refusal terminal)
# and tells a streaming caller to feed more of a stream this receiver has already
# refused -- five bytes holding a connection open, the amplification the caps
# exist to close.
echo "==> an over-cap length word with NO payload is LIMIT_EXCEEDED, not INCOMPLETE"
printf '\012\242\006'           > "$WORK/eof_str.bin"
printf '\022\243\006'           > "$WORK/eof_blob.bin"
printf '\012\202\200\200\004' > "$WORK/eof_1m.bin"
for v in eof_str eof_blob eof_1m; do
    if $HL decode dyn < "$WORK/$v.bin" >/dev/null 2>"$WORK/$v.err"; then
        echo "FAIL: $v -- an over-cap length word then EOF must be refused, not accepted"; exit 1
    fi
    grep -q "LIMIT_EXCEEDED" "$WORK/$v.err" || {
        echo "FAIL: $v -- a truncated over-cap header is LIMIT_EXCEEDED, not INCOMPLETE (§6.2.1/§6.3); got:"
        cat "$WORK/$v.err"; exit 1; }
done
# The same bytes decode against a project carrying the target default, which is
# what makes this a POLICY verdict on well-formed input rather than malformation
# -- but they are still truncated, so the verdict there is INCOMPLETE.
ST=$($HN trydecode dyn < "$WORK/eof_str.bin" | head -n1)
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: under the default cap the same bytes are a plain truncation; got $ST"; exit 1; }

# The precision controls, and they are the point: the cap must not turn every
# short message into a policy rejection, and it must not reach a field it does not
# govern. `decode` throws on INCOMPLETE here, so these use tryDecode and assert
# the WORD rather than merely an exit status.
#
#   0a 22        an IN-cap 4-byte string (= max_dyn_string_len) then EOF
#   12 23        the blob twin
#   a2 01 a2 06  a 100-byte string at id 20, an id this message does not declare:
#                §7.3 skips it and "a skipped field is never capped"
#   22 2a        a 5-byte length on bs, whose maxlen is 6: OVER the receiver cap of
#                4 but inside the schema bound, which §6.2.1 says governs alone
printf '\012\042'         > "$WORK/eof_incap.bin"
printf '\022\043'         > "$WORK/eof_incapb.bin"
printf '\242\001\242\006' > "$WORK/eof_skip.bin"
printf '\042\052'         > "$WORK/eof_bounded.bin"
for v in eof_incap eof_incapb eof_skip eof_bounded; do
    ST=$($HL trydecode dyn < "$WORK/$v.bin" 2>"$WORK/$v.err" | head -n1) || {
        echo "FAIL: $v must be a clean truncation, not a rejection; got:"; cat "$WORK/$v.err"; exit 1; }
    [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: $v -> $ST (want INCOMPLETE)"; exit 1; }
done
# ...and the schema bound keeps ITS category at that same word: 100 bytes on the
# maxlen-6 field is INVALID_MSG, never the cap's verdict. This is the row that
# proves the enforcement point was always reachable -- it is where the schema
# bound has always fired.
printf '\042\242\006' > "$WORK/eof_maxlen.bin"
if $HL decode dyn < "$WORK/eof_maxlen.bin" >/dev/null 2>"$WORK/eof_maxlen.err"; then
    echo "FAIL: 100 bytes above maxlen 6 must be refused at the length word"; exit 1
fi
grep -q "INVALID_MSG" "$WORK/eof_maxlen.err" || {
    echo "FAIL: an over-MAXLEN length word is INVALID_MSG, never LIMIT_EXCEEDED; got:"
    cat "$WORK/eof_maxlen.err"; exit 1; }

echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/kotlin/check_vectors.py" "$CORELIB/assets/test_vectors.json" \
    "$WORK/conf/build/install/harness/bin/harness"

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
        "$CORELIB/assets/test_vectors.json" "Kotlin" --mode "$surface" \
        -- "$WORK/conf/build/install/harness/bin/harness"
done

echo "==> S7 decode status through the generated API"
HC="$WORK/conf/build/install/harness/bin/harness"
ST=$(printf '\200' | $HC trydecode vecu | head -n1)   # lone 0x80: dangling varint
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: lone 0x80 -> $ST (want INCOMPLETE)"; exit 1; }
ST=$(printf '' | $HC trydecode vecu | head -n1)       # empty message: valid
[ "$ST" = "COMPLETE" ] || { echo "FAIL: empty message -> $ST (want COMPLETE)"; exit 1; }
# ...and the one-shot `decode` is STRICT about the same verdict. This target has
# no back-compat surface to preserve, so a truncated message raises rather than
# handing back a half-filled object (the zig precedent, ARCHITECTURE S9.3).
if printf '\200' | $HC decode vecu >/dev/null 2>&1; then
    echo "FAIL: one-shot decode of a truncated message must not succeed"; exit 1
fi
echo "==> decode status OK (0x80 INCOMPLETE, empty COMPLETE, strict decode raises)"

# Declared integer width is a VALIDITY bound (MESSAGE_SPEC S7.1 +
# documentation#32, generator#266). A value outside the declared width is
# INVALID: it MUST NOT be masked to the width, and MUST NOT be kept.
#   00 ff 7f    = 16383 into a u8 -- the reported reproducer
#   00 80 02    = 256   into a u8 -- one past the width
#   08 f0 a2 04 = 70000 into a u16
#   00 ff 01    = 255   into a u8 -- the in-range control
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
# check does not have to live where the scalar one does: a schema-bounded array's
# destination is handed to the corelib WHOLE (Visitor.arrayBulk), whose element
# WIDTH is what carries the bound, so a regression there is invisible to the
# scalar vectors above. someuintarray is id 15, u32[4] -> header 0x7B.
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

# Invalid UTF-8 in a materialized string is INVALID (MESSAGE_SPEC S8): a Kotlin
# String is a Unicode type, so the strict path is the only non-mutating one and
# U+FFFD substitution is forbidden in every mode. somestring is id 11 ->
# header 0x5a; the fixlen word 0x12 is (2 << 3) | STRING.
#   5a 12 c3 28  -- an invalid two-byte sequence: INVALID
#   5a 12 c3 a9  -- the same length, valid ("e-acute"): must decode
# ...and a payload at an id this message does NOT declare must still be SKIPPED
# rather than validated (CORELIB_PLAN S6.4, generator#257): id 40 is undeclared,
# so its header is (40 << 3) | fixlen(2) = 322 -> the varint c2 02.
echo "==> invalid UTF-8 in a materialized string must be INVALID (S8)"
printf '\132\022\303\050' > "$WORK/badutf8.bin"
printf '\132\022\303\251' > "$WORK/goodutf8.bin"
printf '\302\002\022\303\050' > "$WORK/badutf8_skipped.bin"
if $H decode myfirstmessage < "$WORK/badutf8.bin" >/dev/null 2>&1; then
    echo "FAIL: invalid UTF-8 in a declared string must be INVALID, never replaced"; exit 1
fi
$H decode myfirstmessage < "$WORK/goodutf8.bin" >/dev/null || { echo "FAIL: valid UTF-8 control must decode"; exit 1; }
$H decode myfirstmessage < "$WORK/badutf8_skipped.bin" >/dev/null \
    || { echo "FAIL: an invalid-UTF-8 string at an UNDECLARED id must be skipped, not validated"; exit 1; }
echo "==> UTF-8 strictness OK (declared rejects, skipped is not validated)"

# The claim this target is built around: the generated MESSAGE sources are plain
# `commonMain` Kotlin -- the standard library and `sofab`, nothing else -- so one
# source set serves the JVM, Node/browser and native. A JVM-only reference
# compiles perfectly well in the JVM harness above, so that build cannot see it;
# the COMMON metadata compilation can, because it type-checks against the common
# stdlib surface alone.
#
# Metadata only: a JS or native compilation would prove the same thing and pull a
# Node distribution or the whole Kotlin/Native toolchain to do it. The message
# sources are copied WITHOUT the project scaffolding (Main.kt / Json.kt), which is
# deliberately JVM-specific -- a harness needs a `main`, an exit code and a stdin.
echo "==> generated sources type-check as commonMain (multiplatform)"
mkdir -p "$WORK/mp/src/commonMain/kotlin"
cp -r "$WORK/ex/src/main/kotlin/message" "$WORK/mp/src/commonMain/kotlin/message"
rm -f "$WORK/mp/src/commonMain/kotlin/message/Main.kt" "$WORK/mp/src/commonMain/kotlin/message/Json.kt"
cat > "$WORK/mp/settings.gradle.kts" <<'YAML'
rootProject.name = "mp"
pluginManagement { repositories { gradlePluginPortal(); mavenCentral() } }
dependencyResolutionManagement { repositories { mavenLocal(); mavenCentral() } }
YAML
cat > "$WORK/mp/build.gradle.kts" <<KTS
plugins { kotlin("multiplatform") version "2.4.10" }
repositories { mavenLocal(); mavenCentral() }
kotlin {
    jvm()
    js(IR) { nodejs() }
    sourceSets {
        commonMain.dependencies { implementation("org.sofabuffers:corelib-kotlin-mp:$VER") }
    }
}
KTS
( cd "$WORK/mp" && "$GRADLEW" --console=plain -q compileCommonMainKotlinMetadata ) \
    || { echo "FAIL: the generated sources are not commonMain-clean"; exit 1; }
echo "==> commonMain type-check OK"

echo "==> corpus + realworld: every definition compiles"
mkdir -p "$WORK/corpus"
# One Gradle project per definition would pay the toolchain cost N times, so the
# corpus is compiled as ONE project with a source set per definition -- each in
# its own package, which is what keeps two corpus defs from colliding.
cat > "$WORK/corpus/settings.gradle.kts" <<'YAML'
rootProject.name = "corpus"
pluginManagement { repositories { gradlePluginPortal(); mavenCentral() } }
dependencyResolutionManagement { repositories { mavenLocal(); mavenCentral() } }
YAML
cat > "$WORK/corpus/build.gradle.kts" <<KTS
plugins { kotlin("jvm") version "2.4.10" }
repositories { mavenLocal(); mavenCentral() }
dependencies { implementation("org.sofabuffers:corelib-kotlin-mp:$VER") }
kotlin { jvmToolchain((findProperty("sofab.jdk") as String? ?: "21").toInt()) }
KTS
ndefs=0
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    cat > "$WORK/corpuscfg.yaml" <<YAML
targets: { kotlin: { package: corpus.$name } }
YAML
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/corpuscfg.yaml" --lang kotlin --in "$def" --out "$WORK/corpus" >/dev/null )
    ndefs=$((ndefs + 1))
done
( cd "$WORK/corpus" && "$GRADLEW" --console=plain -q compileKotlin ) \
    || { echo "FAIL: corpus definitions did not compile"; exit 1; }
echo "==> corpus compiles ($ndefs definitions incl. the realworld example)"

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
    "$CORELIB/assets/test_vectors.json" "Kotlin" --cap 4 \
    -- "$WORK/growth/build/install/harness/bin/harness"

echo "PASS"
