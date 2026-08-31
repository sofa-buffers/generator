#!/usr/bin/env sh
# Reproducible Zig conformance harness: generate -> zig build -> round-trip ->
# byte-exact shared-vector conformance, against corelib-zig (the max-speed
# port: allocation-free streaming encoder, zero-copy contiguous decode).
#
# Usage: tests/conformance/zig/run.sh [corelib-zig]
#   (or set $SOFAB_ZIG_CORELIB)
# Requires: go, zig (0.16+), git, python3.
set -eu

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_ZIG_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    clone_corelib corelib-zig "$WORK/corelib"
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
  vecua: { payload: { a: { id: 0, type: array, items: { type: u32, count: 8 } } } }
YAML

printf 'generic: { emit: project }\n' > "$WORK/cfg.yaml"

# zig_gen DEF OUT-DIR [CFG] -- generate a project and point it at the corelib.
# The relative depth of the corelib symlink depends on the output nesting, so
# the placeholder is resolved with a computed relative path.
zig_gen() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang zig --in "$1" --out "$2" )
    rel=$(python3 -c "import os,sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))" "$WORK/corelib-link" "$2")
    sed -i "s#\${SOFAB_ZIG_CORELIB}#$rel#" "$2/build.zig.zon"
}

# Hermetic caches (both builders): CI zig setups (mlugg/setup-zig) restore a
# shared zig cache across runs, and every generated package carries the same
# build.zig.zon name + fingerprint (one package identity) - a restored or shared
# cache can then serve a stale harness for an A/B pair that differs only in
# generator config (seen on the #102 lim/nolim projects: the no-limits harness
# rejected with LimitExceeded). A per-project local cache and a per-WORK global
# cache key every build to this run only.

# zig_build DEF OUT-DIR [CFG] -- generate and build a harness that is EXECUTED
# below. zig is the maxspeed port, so the binaries that actually run are built
# the way they ship: --release=fast, safety checks off.
zig_build() {
    zig_gen "$1" "$2" "${3:-}"
    ( cd "$2" && zig build --release=fast --cache-dir .zig-cache --global-cache-dir "$WORK/zig-global-cache" )
}

# zig_typecheck DEF OUT-DIR [CFG] -- generate and COMPILE ONLY. The corpus
# projects assert one thing, "every definition produces code that builds", and
# their binaries are never run; a Debug build proves exactly that while skipping
# the LLVM optimisation pipeline (measured ~7x faster per project, and the
# corpus is 21 of the harness's 28 builds). The peer harnesses draw the same
# line: rust builds its projects with `cargo build -q` (no --release), cpp
# checks its compile-only headers with `g++ -fsyntax-only`.
zig_typecheck() {
    zig_gen "$1" "$2" "${3:-}"
    ( cd "$2" && zig build --cache-dir .zig-cache --global-cache-dir "$WORK/zig-global-cache" )
}

echo "==> generating + building example + conformance projects"
zig_build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
zig_build "$WORK/conf.yaml" "$WORK/conf"

# MAX_SIZE fill check (ARCHITECTURE §9.6): MAX_SIZE sizes the encode buffer, so a
# fully filled message must fit it AND reach it exactly.
echo "==> MAX_SIZE fill check"
zig_build "$ROOT/tests/conformance/lib/maxsize_fill.yaml" "$WORK/fill"
check_maxsize_fill zig "$WORK/fill/zig-out/bin/harness" encode fill

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(printf '%s' "$IN" | "$WORK/ex/zig-out/bin/harness" encode myfirstmessage | "$WORK/ex/zig-out/bin/harness" decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: blob round-trip"; exit 1; }
# somestringarray declares count: 5, but `count` is a CAPACITY, never a length
# (MESSAGE_SPEC S3, documentation af536c4): the wire carries 0..5 elements and
# the wire count IS the length. Three strings in, three strings back -- nothing
# is filled in at [M, N). The five-element form asserted here before pinned the
# superseded fixed-length reading of `count`.
echo "$OUT" | grep -q '"somestringarray":\["a","b","c"\]' || { echo "FAIL: string array round-trip (count:5 is a capacity, not a length)"; exit 1; }
echo "$OUT" | grep -q '"somefp32":2.5' || { echo "FAIL: fp32 round-trip"; exit 1; }
echo "==> round-trip OK"

# `count` is a CAPACITY, never a length (MESSAGE_SPEC S3, documentation af536c4).
# someuintarray declares count: 4 (id 15 -> header 0x7b). Two things follow, and
# both are asserted here because each was the opposite before:
#   - the wire count M IS the length: a 2-element wire decodes to 2 elements, not
#     to 4 with the tail refilled from the schema count;
#   - nothing that carries the length may be elided: [1,2,0,0] keeps its trailing
#     default run on the wire and round-trips unchanged, instead of collapsing to
#     the 2-element [1,2] that the decoder then padded back out.
echo "==> count:N native array carries its length (MESSAGE_SPEC S3)"
printf '\173\002\001\002' > "$WORK/shortcount.bin"
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/shortcount.bin") \
    || { echo "FAIL: a wire count below the schema count must decode"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2\]' || { echo "FAIL: M < N must decode as M elements, not be filled to N; got: $OUT"; exit 1; }
OUT=$(printf '%s' '{"someuintarray":[1,2,0,0]}' | "$WORK/ex/zig-out/bin/harness" encode myfirstmessage \
    | "$WORK/ex/zig-out/bin/harness" decode myfirstmessage) \
    || { echo "FAIL: trailing-default array must round-trip"; exit 1; }
echo "$OUT" | grep -q '"someuintarray":\[1,2,0,0\]' || { echo "FAIL: a trailing default run must stay on the wire; got: $OUT"; exit 1; }
echo "==> count-is-capacity OK"

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
"$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/fp64_at_fp32.bin" >/dev/null || { echo "FAIL: fp64 array at an fp32-declared id must be skipped, not bounded"; exit 1; }
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/fp32_overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: fp32 array with count 5 > 3 at its own id must stay INVALID"; exit 1
fi
echo "==> fixlen-array subtype ordering OK"

# Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2). someuintarray count 4; a header of 6 (> 4) then only 2
# elements + EOF is BOTH over-count and truncated. arrayBegin sets the sticky inv
# at the count header (before the elements), and decode() reads inv before
# .incomplete, so the outcome is InvalidMessage, not IncompleteMessage. Wire:
# 7b (id 15 unsigned-array) 06 (count 6) 01 02 (2 of 6 elements) <EOF>.
echo "==> over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\173\006\001\002' > "$WORK/overcount_trunc.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overcount_trunc.bin" >/dev/null 2>"$WORK/oct.err"; then
    echo "FAIL: over-count(6>4)+truncated must reject"; exit 1
fi
grep -q "InvalidMessage" "$WORK/oct.err" || { echo "FAIL: over-count(6>4)+truncated must be InvalidMessage; got:"; cat "$WORK/oct.err"; exit 1; }
# Precision control: an in-bound count (4 == bound) that is genuinely truncated
# (2 of 4 elements then EOF) is a clean truncation and MUST stay IncompleteMessage.
printf '\173\004\001\002' > "$WORK/incount_trunc.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/incount_trunc.bin" >/dev/null 2>"$WORK/ict.err"; then
    echo "FAIL: in-bound(4==4)+truncated must reject"; exit 1
fi
grep -q "IncompleteMessage" "$WORK/ict.err" || { echo "FAIL: in-bound(4==4)+truncated must be IncompleteMessage; got:"; cat "$WORK/ict.err"; exit 1; }
echo "==> over-count/truncation ordering OK"

# ...and the same violation with the message cut RIGHT AFTER the word that
# carries it (MESSAGE_SPEC S5.2, generator#267). The over-maxlen is fully
# established by the fixlen length word -- 17 > maxlen 16 is decided by bytes
# already on the wire -- so running out of input cannot downgrade the verdict.
#
# The guard used to live in the PAYLOAD callback, which never fires for a message
# that ends here, so this reported INCOMPLETE. The corelib hook that announces a
# fixlen field at its length word is what makes the verdict available in time.
#
#   62      blob, field id 12 ((12<<3)|2)
#   8b 01   fixlen word: byte length 17, blob subtype -> ((17<<3)|3)
#   <EOF>   not one payload byte
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#267)"
printf '\142\213\001' > "$WORK/overmaxlen_trunc.bin"
printf '\142\203\001' > "$WORK/inmaxlen_trunc.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overmaxlen_trunc.bin" >/dev/null 2>"$WORK/omt.err"; then
    echo "FAIL: over-maxlen(17>16)+truncated must reject"; exit 1
fi
grep -q "InvalidMessage" "$WORK/omt.err" || { echo "FAIL: over-maxlen(17>16)+truncated must be InvalidMessage; got:"; cat "$WORK/omt.err"; exit 1; }
# Precision control: an IN-BOUND length (16 == maxlen) cut at the same offset is a
# clean truncation and MUST stay IncompleteMessage. Without it this is a blanket
# reject rather than an ordering assertion.
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/inmaxlen_trunc.bin" >/dev/null 2>"$WORK/imt.err"; then
    echo "FAIL: in-bound(16==16)+truncated must reject"; exit 1
fi
grep -q "IncompleteMessage" "$WORK/imt.err" || { echo "FAIL: in-bound(16==16)+truncated must be IncompleteMessage; got:"; cat "$WORK/imt.err"; exit 1; }
echo "==> maxlen/truncation ordering OK"

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
# Asserted as a prefix: every profile renders ["a"] now that `count` adds no
# elements, but a fixed-capacity target that still padded would print ["a","",...].
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

# Chunk invariance of the incremental decoder (generator#293, Crucible F-0058 /
# codegen defect G-0036). Decoding must not depend on how the input is split:
# the same bytes must give the same values fed whole or one byte at a time.
#
# Nothing else in this file reaches that property — every check above hands the
# harness a complete message on stdin, and a payload that arrives whole is never
# reassembled. The defect this pins lived entirely in the reassembly path: one
# shared accumulator on the visitor, handed to the store as a view, so a second
# split payload overwrote the element assembled before it and growth rebased it
# onto the freed block.
#
# stream_check.zig replaces the generated JSON harness as src/main.zig, so it
# builds against the same generated code and the same corelib as everything else
# here, and asserts the values directly rather than through JSON.
echo "==> chunked decode is chunk-invariant (generator#293)"
cat > "$WORK/probe.yaml" <<'YAML'
version: 1
messages:
  probe:
    payload:
      string_array: { id: 200, type: array, items: { type: string, count: 8, maxlen: 64 } }
      blob_array: { id: 201, type: array, items: { type: blob, count: 8, maxlen: 64 } }
      a: { id: 0, type: string, maxlen: 64 }
      b: { id: 1, type: string, maxlen: 64 }
      c: { id: 2, type: string, maxlen: 64 }
      d: { id: 3, type: string, maxlen: 64 }
YAML
zig_build "$WORK/probe.yaml" "$WORK/probe"
cp "$ROOT/tests/conformance/zig/stream_check.zig" "$WORK/probe/src/main.zig"
( cd "$WORK/probe" && zig build --release=fast --cache-dir .zig-cache --global-cache-dir "$WORK/zig-global-cache" )
"$WORK/probe/zig-out/bin/harness" || { echo "FAIL: chunked decode is not chunk-invariant"; exit 1; }
echo "==> chunk invariance OK"

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
"$WORK/nolim/zig-out/bin/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: default-cap project must accept count 5"; exit 1; }
echo "==> decode limits OK"

# A string/blob receiver cap is decided at the fixlen LENGTH WORD, not at payload
# completion (CORELIB_PLAN §6.2.1 "Enforcement point"). The cap used to sit only
# in the payload callback, behind the reassembly helper that returns nothing
# until the whole payload is in hand, so:
#   * bytes the cap forbids were BUFFERED before it was consulted -- a chunked
#     sender could stream an arbitrarily long over-cap payload into the
#     accumulator and the rejection came after the last byte;
#   * an over-cap length followed by EOF reported IncompleteMessage, because the
#     callback never fired. The verdict was available at the length word and the
#     rejection is terminal (§6.3), so INCOMPLETE both lost the category and
#     invited the sender to hold the connection open.
# Headers below: (id << 3) | wire type; a fixlen word is (length << 3) | subtype
# (2 = string, 3 = blob). Caps: string 8, blob 8.
echo "==> a string/blob cap fires at the length word, not at payload completion (§6.2.1)"
cat > "$WORK/dynsb.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      s:  { id: 0, type: string }
      b:  { id: 1, type: blob }
      bs: { id: 2, type: string, maxlen: 32 }
YAML
printf 'generic: { emit: project, max_dyn_string_len: 8, max_dyn_blob_len: 8 }\n' > "$WORK/cfg_sb.yaml"
zig_build "$WORK/dynsb.yaml" "$WORK/sblim" "$WORK/cfg_sb.yaml"
# sb_expect OCTAL EXPECTED-STDERR-SUBSTRING DESC ("" == must decode)
sb_expect() {
    SB_RC=0
    (printf "$1" | "$WORK/sblim/zig-out/bin/harness" decode dyn >/dev/null) 2>"$WORK/sb.err" || SB_RC=$?
    if [ -z "$2" ]; then
        [ "$SB_RC" = 0 ] || { echo "FAIL: $3 must decode; got: $(cat "$WORK/sb.err")"; exit 1; }
        return
    fi
    [ "$SB_RC" != 0 ] || { echo "FAIL: $3 must be refused as $2; it decoded"; exit 1; }
    grep -q "$2" "$WORK/sb.err" || { echo "FAIL: $3 must be $2; got: $(cat "$WORK/sb.err")"; exit 1; }
}
sb_expect '\002\112ABC'  LimitExceeded     "an over-cap string length (9 > 8) then EOF"
sb_expect '\012\113ABC'  LimitExceeded     "an over-cap blob length (9 > 8) then EOF"
# Precision: an IN-cap length genuinely truncated is a clean truncation and MUST
# stay INCOMPLETE -- the cap must not turn every short message into a rejection.
sb_expect '\002\102ABC'  IncompleteMessage "an in-cap string length (8 == 8) then EOF"
# A schema-bounded field is governed by its OWN bound and its own category: the
# cap of 8 must not reach `bs`, and its maxlen of 32 is INVALID, not the cap
# (§6.2.1 forbids folding the two).
sb_expect '\022\242\001ABCDEFGHIJKLMNOPQRST' ""             "a 20-byte string on a maxlen-32 field (the cap of 8 must not apply)"
sb_expect '\022\302\002ABC'                  InvalidMessage "a 40-byte string over maxlen 32, then EOF"
# A §7.3-skipped field is never capped (#410), truncated or not: an over-cap blob
# at the string-declared id 0 leaves only the truncation to report.
sb_expect '\002\113ABC'  IncompleteMessage "an over-cap BLOB at the string-declared id 0 (§7.3 skip), then EOF"
sb_expect '\072\112ABC'  IncompleteMessage "an over-cap string at the UNKNOWN id 7, then EOF"
echo "==> string/blob cap enforcement point OK"
# The receiver cap on a WRAPPER array's element INDEX, and on a matrix row's own
# element count (CORELIB_PLAN 6.2.1). These are the caps corelib-zig compares --
# generated code passes max_dyn_array_count into arrays.setElemCapped (string and
# blob elements), arrays.growCapped (struct elements, and a matrix row's index)
# and arrays.allocNCapped (the row's count), and emits no guard of its own -- so
# a codegen change that stopped passing the number would look identical in review
# and would be caught only here. Every case is driven as raw wire bytes, because
# an over-cap message is one the encoder will not produce.
#
#   s (id 0)  wrapper array of string: 06 = sequence start id 0; element header
#             (id << 3) | 2 = fixlen; fixlen word (1 << 3) | 2 = 0x0a (1 byte,
#             string); payload 'x'; 07 = sequence end.
#   o (id 1)  wrapper array of struct: 0e = sequence start id 1; element
#             (id << 3) | 6 = nested sequence; child x (id 1) = 1; 07 07.
#   m (id 2)  matrix: 16 = sequence start id 2; row = unsigned-array header
#             (id << 3) | 3, then its own count and elements; 07.
#
# max_dyn_array_count is 4, so index 3 is the last admissible one and index 4 is
# over -- a wrapper array's length being its highest present id + 1 (MESSAGE_SPEC
# 5.1), which is why it is the INDEX that is capped and not a count.
echo "==> receiver cap on wrapper element index and row count (CORELIB_PLAN 6.2.1)"
cat > "$WORK/wrap.yaml" <<'YAML'
version: 1
messages:
  wrap:
    payload:
      s: { id: 0, type: array, items: { type: string } }
      o: { id: 1, type: array, items: { type: struct, fields: { x: { id: 1, type: u32 } } } }
      m: { id: 2, type: array, items: { type: array, items: { type: u32 } } }
YAML
zig_build "$WORK/wrap.yaml" "$WORK/wrap" "$WORK/cfg_lim.yaml"
WRAPH="$WORK/wrap/zig-out/bin/harness"

# accepts: at the cap, every shape still decodes.
for row in \
    'string element at index 3:\006\032\012\170\007' \
    'struct element at index 3:\016\036\010\001\007\007' \
    'matrix row at index 3:\026\033\001\001\007' \
    'matrix row of 4 elements:\026\003\004\001\002\003\004\007'
do
    what=${row%%:*}; bytes=${row#*:}
    printf "$bytes" | "$WRAPH" decode wrap >/dev/null 2>&1 || {
        echo "FAIL: $what is at the cap and must decode"; exit 1; }
done

# rejects: one past the cap, and as LimitExceeded -- the policy category. The
# bytes are well formed and the same message decodes under a looser cap, so
# reporting InvalidMessage here would be the fold CORELIB_PLAN 6.2.1 forbids.
for row in \
    'string element at index 4:\006\042\012\170\007' \
    'struct element at index 4:\016\046\010\001\007\007' \
    'matrix row at index 4:\026\043\001\001\007' \
    'matrix row of 5 elements:\026\003\005\001\002\003\004\005\007'
do
    what=${row%%:*}; bytes=${row#*:}
    if printf "$bytes" | "$WRAPH" decode wrap >/dev/null 2>&1; then
        echo "FAIL: $what is over the cap and must be rejected"; exit 1
    fi
    printf "$bytes" | "$WRAPH" decode wrap 2>&1 | grep -q LimitExceeded || {
        echo "FAIL: $what must be refused as LimitExceeded, not as another category"; exit 1; }
done

# A skipped field is never capped (6.2.1). An unsigned ARRAY arrives at id 0,
# which the schema declares a string wrapper array: the wire type contradicts the
# declared one, so MESSAGE_SPEC 7.3 skips the field -- and its 9-element count,
# over the cap of 4, is not this field's count and must not be measured against
# it. The decode stays COMPLETE.
printf '\003\011\001\002\003\004\005\006\007\010\011' | "$WRAPH" decode wrap >/dev/null 2>&1 || {
    echo "FAIL: a 7.3-skipped over-cap array must not be capped"; exit 1; }
echo "==> wrapper index / row count caps OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/zig/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf"

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    zig_typecheck "$def" "$WORK/corpus/$name"
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
    if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/$v.bin" >/dev/null 2>&1; then
        echo "FAIL: $v must be INVALID (S7.1) -- neither masked to the width nor kept"; exit 1
    fi
done
OUT=$("$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/w_u8_255_ctl.bin") || { echo "FAIL: in-range control 255 must decode"; exit 1; }
echo "$OUT" | tr -d ' ' | grep -q '"someu8":255' || { echo "FAIL: control must keep 255 exactly; got: $OUT"; exit 1; }
echo "==> declared-width reject OK"

echo "PASS"
