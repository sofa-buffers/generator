#!/usr/bin/env sh
# Reproducible Zig conformance harness: generate -> zig build -> round-trip ->
# byte-exact shared-vector conformance, against corelib-zig (the max-speed
# port: allocation-free streaming encoder, zero-copy contiguous decode).
#
# Usage: tests/conformance/zig/run.sh [corelib-zig]
#   (or set $SOFAB_ZIG_CORELIB)
# Requires: go, zig (0.16+), git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_ZIG_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-zig.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
# build.zig.zon path dependencies must be relative to the build root, so every
# generated project points at a sibling symlink to the corelib checkout.
CORELIB=$(cd "$CORELIB" && pwd)
ln -sfn "$CORELIB" "$WORK/corelib-link"
echo "==> corelib-zig: $CORELIB"

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

printf 'generic: { emit: project }\n' > "$WORK/cfg.yaml"

# zig_build DEF OUT-DIR [CFG] -- generate a project and build its harness. The
# relative depth of the corelib symlink depends on the output nesting, so the
# placeholder is resolved with a computed relative path.
zig_build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang zig --in "$1" --out "$2" )
    rel=$(python3 -c "import os,sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))" "$WORK/corelib-link" "$2")
    sed -i "s#\${SOFAB_ZIG_CORELIB}#$rel#" "$2/build.zig.zon"
    # Hermetic caches: CI zig setups (mlugg/setup-zig) restore a shared zig
    # cache across runs, and every generated package carries the same
    # build.zig.zon name + fingerprint (one package identity) - a restored or
    # shared cache can then serve a stale harness for an A/B pair that differs
    # only in generator config (seen on the #102 lim/nolim projects: the
    # no-limits harness rejected with LimitExceeded). A per-project local
    # cache and a per-WORK global cache key every build to this run only.
    ( cd "$2" && zig build --release=fast --cache-dir .zig-cache --global-cache-dir "$WORK/zig-global-cache" )
}

echo "==> generating + building example + conformance projects"
zig_build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
zig_build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(printf '%s' "$IN" | "$WORK/ex/zig-out/bin/harness" encode myfirstmessage | "$WORK/ex/zig-out/bin/harness" decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: blob round-trip"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["a","b","c"\]' || { echo "FAIL: string array round-trip"; exit 1; }
echo "$OUT" | grep -q '"somefp32":2.5' || { echo "FAIL: fp32 round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
"$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
"$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
"$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
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
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/wiremismatch.bin") \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped field must keep its default 7; got: $OUT"; exit 1; }
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/wiremismatch_control.bin") \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: control must decode to 9; got: $OUT"; exit 1; }
echo "==> wire-type skip OK"

# Integer ARRAY delivered to a SCALAR-declared id (MESSAGE_SPEC S7.3,
# generator#183). This is the one wire-type contradiction the generated id
# dispatch cannot see on its own: corelib-zig streams array elements through the
# very unsigned()/signed() callbacks a lone scalar uses, so without the
# arrayBegin-armed skip counter the element would land in the scalar's arm.
# someu8 (id 0, declared u8, default 7) receives an UNSIGNED ARRAY, and somei8
# (id 4, declared i8, default 10) a SIGNED ARRAY -- both must be skipped whole.
# Wire: 03 = id 0 wire type ARRAY_UNSIGNED (3), 01 = count 1, 05 = element 5.
#       24 = id 4 wire type ARRAY_SIGNED (4), 01 = count 1, 06 = zig-zag 3.
# Control: 21 06 is id 4 with the correct SIGNED wire type and must decode to 3,
# which pins that the counter self-terminates instead of eating later scalars.
echo "==> integer array at a scalar id must skip (MESSAGE_SPEC S7.3, generator#183)"
printf '\003\001\005' > "$WORK/arr_at_scalar_u.bin"
printf '\044\001\006' > "$WORK/arr_at_scalar_i.bin"
printf '\041\006' > "$WORK/arr_at_scalar_control.bin"
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/arr_at_scalar_u.bin") \
    || { echo "FAIL: unsigned array at a scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: scalar receiving an unsigned array must keep its default 7; got: $OUT"; exit 1; }
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/arr_at_scalar_i.bin") \
    || { echo "FAIL: signed array at a scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somei8":10' || { echo "FAIL: scalar receiving a signed array must keep its default 10; got: $OUT"; exit 1; }
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/arr_at_scalar_control.bin") \
    || { echo "FAIL: control (correct signed wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"somei8":3' || { echo "FAIL: control must decode to 3; got: $OUT"; exit 1; }
# A legitimate array field is untouched by the skip counter: someuintarray (id 15)
# still fills from its own ARRAY_UNSIGNED header.
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/control.bin") \
    || { echo "FAIL: legitimate array must still decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2,3,4\]' || { echo "FAIL: legitimate array must still fill; got: $OUT"; exit 1; }
echo "==> array-at-scalar skip OK"

# fp ARRAY delivered to a SCALAR-declared fp id (MESSAGE_SPEC S7.3, generator#193):
# the fp analogue of the integer case above. corelib-zig streams a fixlen (fp) array
# element-by-element through the very fp32()/fp64() callbacks a lone scalar uses, so
# without the arrayBegin-armed skip counter the element would land in the scalar's
# arm. somefp64 (id 9, declared fp64, default 3.141592653589793) receives an fp64
# ARRAY and must be skipped whole.
# Wire: 4d = id 9 wire type ARRAY_FIXLEN (5), 01 = count 1, 41 = fixlen word (len 8,
#       FP64 subtype), then 2.5 little-endian.
# Control: 4a 41 + 2.5 is id 9 with the correct scalar FIXLEN wire type -> 2.5.
echo "==> fp array at a scalar id must skip (MESSAGE_SPEC S7.3, generator#193)"
printf '\115\001\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar_control.bin"
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/fp_arr_at_scalar.bin") \
    || { echo "FAIL: fp array at a scalar id must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: scalar receiving an fp array must keep its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/fp_arr_at_scalar_control.bin") \
    || { echo "FAIL: control (correct scalar fixlen wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
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
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/reopen_struct.bin") \
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
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/reopen_array.bin") \
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
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/fixsubtype.bin") \
    || { echo "FAIL: mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
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
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
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
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

# Decode outcome tri-state (MESSAGE_SPEC §7, generator#120): corelib-zig
# reports INCOMPLETE as a non-error `Status` from feed(); the generated
# one-shot decode() owns end-of-input, so a trailing .incomplete must fail
# with error.IncompleteMessage — distinct from InvalidMessage, never silently
# accepted. A lone 0x80 is a dangling varint header (INCOMPLETE); empty input
# is a valid all-defaults message (COMPLETE).
echo "==> §7 tri-state: truncated input is IncompleteMessage (generator#120)"
printf '\200' > "$WORK/dangling.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/dangling.bin" >/dev/null 2>"$WORK/trunc.err"; then
    echo "FAIL: lone 0x80 (dangling varint) must not decode"; exit 1
fi
grep -q "IncompleteMessage" "$WORK/trunc.err" || { echo "FAIL: truncation must surface IncompleteMessage, not InvalidMessage"; cat "$WORK/trunc.err"; exit 1; }
printf '' | "$WORK/ex/zig-out/bin/harness" decode myfirstmessage >/dev/null || { echo "FAIL: empty input (COMPLETE) must decode to defaults"; exit 1; }
echo "==> tri-state OK"

# Receiver-side decode limits (generator#102): a count-less u64 array with
# max_dyn_array_count: 4 baked into the generated module (id 0 -> header 0x03 =
# 0<<3 | unsigned-array). A wire count of 5 MUST fail decode with the corelib's
# error.LimitExceeded (exits non-zero); a count of 4 still decodes; and the
# same 5-element bytes MUST decode in a project generated WITHOUT limits.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
printf 'generic: { emit: project, max_dyn_array_count: 4 }\n' > "$WORK/cfg_lim.yaml"
zig_build "$WORK/dyn.yaml" "$WORK/lim" "$WORK/cfg_lim.yaml"
zig_build "$WORK/dyn.yaml" "$WORK/nolim"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if "$WORK/lim/zig-out/bin/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null 2>&1; then
    echo "FAIL: dynamic array count 5 must exceed max_dyn_array_count 4"; exit 1
fi
"$WORK/lim/zig-out/bin/harness" decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: count == limit (4) must decode"; exit 1; }
"$WORK/nolim/zig-out/bin/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits project must accept count 5"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/zig/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf"

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    zig_build "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
