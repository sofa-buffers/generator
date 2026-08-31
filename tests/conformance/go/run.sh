#!/usr/bin/env sh
# Reproducible Go conformance harness: generate -> build -> round-trip ->
# byte-exact shared-vector conformance against corelib-go.
#
# Usage: tests/conformance/go/run.sh [path-to-corelib-go]   (or set $SOFAB_GO_CORELIB)
# Requires: go, git.
set -eu

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"
# Shared MAX_SIZE fill check (ARCHITECTURE §9.6).
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_GO_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-go"
    clone_corelib corelib-go "$WORK/corelib"
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-go: $CORELIB"

cat > "$WORK/cfg.yaml" <<YAML
generic: { emit: project }
targets: { go: { package: message, module_path: example.com/gen, go_version: "1.21" } }
YAML

echo "==> generating Go project"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in examples/messages/example.yaml --out "$WORK/proj" )

echo "==> wiring corelib + building"
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/proj/go.mod"
( cd "$WORK/proj" && GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1 && go build ./... )

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/proj" && printf '%s' "$IN" | GOFLAGS=-mod=mod go run ./harness encode myfirstmessage | GOFLAGS=-mod=mod go run ./harness decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Streaming decode: the same bytes through the io.Reader-driven entry point
# (CORELIB_PLAN S5.6, generator#312 / corelib-go#130). DecodeXFrom drives
# corelib-go's Decoder.FeedFrom, which feeds the decoder whatever the reader
# delivered and resumes on the next chunk, instead of requiring the whole wire
# image resident the way AcceptBytes does by construction.
#
# The harness feeds it ONE BYTE PER Read. That is the point of the check: a
# reader handing the message over in a single Read would exercise the new
# signature without ever making the decoder suspend and resume, which is the
# half that can actually be wrong. Every byte position becomes a boundary --
# which since corelib-go#130 also means every string and blob payload is
# delivered one byte at a time, so the destination's piecewise assembly
# (sofab.PayloadAcc) is exercised at every offset it can suspend at.
#
# The assertion is EQUIVALENCE, not "it decodes": byte-identical JSON to the
# in-memory path. A streaming decode that quietly dropped a field would still
# exit 0.
echo "==> streaming decode must equal the in-memory decode (generator#312)"
printf '%s' "$IN" | (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness encode myfirstmessage) > "$WORK/rt.bin"
WHOLE=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/rt.bin")
DRIP=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness streamdecode myfirstmessage < "$WORK/rt.bin")
[ "$WHOLE" = "$DRIP" ] || {
    echo "FAIL: streamdecode differs from decode"
    echo "  decode:       $WHOLE"
    echo "  streamdecode: $DRIP"
    exit 1
}
echo "==> streaming decode OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1); then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/control.bin" >/dev/null) || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Fixlen-array element subtype decides BEFORE the schema count bound
# (CORELIB_PLAN S4.8, generator#259 / Crucible F-0042). somefloatarray declares
# fp32 with count: 3 at id 17 -> array-fixlen header 0x8d 0x01 (17<<3 | 5).
# A fixlen array carries a fixlen_word after its count: 0x20 = 4-byte elements
# (fp32), 0x41 = 8-byte elements (fp64).
#
#   fp64 header, count 5 at the fp32-declared id: the subtype contradicts the
#   declared element type, so the whole field is SKIPPED (MESSAGE_SPEC S7.3) and
#   its count is NOT this field's count -- it must NOT be measured against 3.
#   Payload is 5 x 8 zero bytes, so the message is complete and must DECODE.
#
#   fp32 header, count 5 at the same id: the subtype matches, so this really is
#   the field's count and the schema bound applies -- still INVALID (S3+S7).
#
# Before the subtype reached the header hook the two were indistinguishable and
# both rejected, which is the defect this pins.
echo "==> fixlen-array subtype decides before the count bound (generator#259)"
printf '\215\001\005\101\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' > "$WORK/fp64_at_fp32.bin"
printf '\215\001\005\040\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' > "$WORK/fp32_overcount.bin"
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/fp64_at_fp32.bin" >/dev/null) || { echo "FAIL: fp64 array at an fp32-declared id must be skipped, not bounded"; exit 1; }
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/fp32_overcount.bin" >/dev/null 2>&1); then
    echo "FAIL: fp32 array with count 5 > 3 at its own id must stay INVALID"; exit 1
fi
echo "==> fixlen-array subtype ordering OK"

# A SKIPPED string is never UTF-8-validated (CORELIB_PLAN S6.4, generator#257 /
# Crucible F-0038). Validation belongs where a `string` is MATERIALIZED -- read
# into a declared destination -- never on a payload the decoder jumps over. The
# corelib's visitor path deliberately does not validate (its cursor cannot tell a
# bound field from a skipped one), so the generated destination arms do, via
# sofab.UTF8Valid.
#
#   id 99 is undeclared: 9a 06 (99<<3|2, fixlen) 0a (fixlen word: len 1, subtype
#   string) 8a (a lone continuation byte, invalid UTF-8). The field is skipped,
#   so the byte is never inspected and the message decodes to all-defaults.
#
#   somestring is declared at id 11: 5a (11<<3|2) 0a 8a. Same byte, now
#   materialized -- INVALID.
echo "==> a skipped string is not UTF-8-validated (generator#257)"
printf '\232\006\012\212' > "$WORK/skipped_bad_utf8.bin"
printf '\132\012\212'      > "$WORK/declared_bad_utf8.bin"
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/skipped_bad_utf8.bin" >/dev/null) || { echo "FAIL: invalid UTF-8 at a SKIPPED id must not fail the decode"; exit 1; }
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/declared_bad_utf8.bin" >/dev/null 2>&1); then
    echo "FAIL: invalid UTF-8 in a DECLARED string must be INVALID"; exit 1
fi
echo "==> skipped-string UTF-8 OK"

# Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2). someuintarray declares count 4; a header announcing 6 elements
# (> 4) followed by only 2 elements then EOF is BOTH schema-invalid and truncated.
# The over-count is decided at the count word (sofab.HeaderVisitor.ArrayBegin, before
# the truncation check), so the message MUST be INVALID, not INCOMPLETE. The
# whole-slice len(v)>4 guard in UnsignedArray never runs on a truncated array, so
# this pins the header hook specifically.
# Wire: 7b (id 15 unsigned-array) 06 (count 6) 01 02 (2 of 6 elements) <EOF>.
echo "==> over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\173\006\001\002' > "$WORK/overcount_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overcount_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'invalid message' || { echo "FAIL: over-count(6>4)+truncated must be INVALID (invalid message); got: $ERR"; exit 1; }
# Precision control: an IN-BOUND count (4 == bound) that is genuinely truncated
# (2 of 4 elements then EOF) is a clean truncation and MUST stay INCOMPLETE -- the
# header hook must not turn every short array into INVALID.
printf '\173\004\001\002' > "$WORK/incount_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/incount_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'incomplete message' || { echo "FAIL: in-bound(4==4)+truncated must be INCOMPLETE; got: $ERR"; exit 1; }
echo "==> over-count/truncation ordering OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element carrying a wire index >= 5 is a schema-bound
# violation -- MESSAGE_SPEC S5.1/S7 make it INVALID for every target, never
# grown-into (this also bounds an over-index heap-amplification DoS). Wire:
#   96 01  sequence_begin, id 18 ((18<<3)|6, varint)
#   2a     string, id 5  ((5<<3)|2) -- over-index (>= count 5)
#   0a 78  fixlen word (len 1, subtype string) + "x"
#   07     sequence_end
# The control places the same element at id 4 (< 5), which still decodes.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1); then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null) || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
# ... and the same violation with the message cut RIGHT AFTER the word that
# carries it (generator#267 / Crucible F-0043). S5.2 makes INVALID dominate
# INCOMPLETE: once the bytes seen so far are already malformed, running out of
# input cannot downgrade the verdict. The element id 5 is fully established by
# the element header, so this is INVALID even though nothing follows.
# Wire: 96 01 (seq start id 18) 2a (element id 5, fixlen) 0a (len 1, string) <EOF>
echo "==> over-index + truncation must be INVALID, not INCOMPLETE (generator#267)"
printf '\226\001\052\012' > "$WORK/overindex_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overindex_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'invalid message' \
    || { echo "FAIL: over-index(5>=5)+truncated must be INVALID, not INCOMPLETE; got: $ERR"; exit 1; }
# Precision control: an IN-RANGE element id truncated at the same offset is a
# clean truncation and MUST stay INCOMPLETE -- the bound must not turn every
# short element into INVALID.
printf '\226\001\042\012' > "$WORK/inindex_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/inindex_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'incomplete message' \
    || { echo "FAIL: in-range(4<5)+truncated must stay INCOMPLETE; got: $ERR"; exit 1; }
echo "==> over-index reject OK"

# The same ordering one level down, at the ELEMENT (generator#267 residue,
# Crucible F-0043 width_elem_trunc). someuintarray (id 15) declares u32 elements;
# an element carrying 2^32 is outside that width, which S7.1 makes INVALID, and it
# is established by its own bytes -- so S5.2 keeps the verdict INVALID however
# little of the array follows. The `for _, _x := range v` scan cannot fire for an
# array that never assembles, so the bound is also declared to the corelib as
# sofab.ElemBoundVisitor, which applies it while the elements go past.
# Wire: 7b (id 15 unsigned-array) 04 (count 4) 80 80 80 80 10 (2^32) <EOF>.
echo "==> over-width element + truncation must be INVALID (generator#267)"
printf '\173\004\200\200\200\200\020' > "$WORK/overwidth_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overwidth_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'invalid message' \
    || { echo "FAIL: over-width element + truncated must be INVALID; got: $ERR"; exit 1; }
# Precision control: an IN-RANGE element cut at the same offset decides nothing,
# so the truncation IS the verdict.
printf '\173\004\001' > "$WORK/inwidth_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/inwidth_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'incomplete message' \
    || { echo "FAIL: in-range element + truncated must stay INCOMPLETE; got: $ERR"; exit 1; }
echo "==> element-width/truncation ordering OK"

# Over-maxlen scalar blob (generator Option B / MESSAGE_SPEC S7.1): someblob (id 12)
# declares maxlen: 16. A wire byte length above the schema maxlen is malformed input,
# INVALID for every target, never truncated. Wire:
#   62      blob, field id 12 ((12<<3)|2)
#   8b 01   fixlen word (varint): byte length 17, blob subtype 3 ((17<<3)|3)
#   01 x17  the 17-byte payload
# The control is a 16-byte blob (== maxlen), which still decodes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1); then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null) || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Over-maxlen AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2), the string/blob analogue of the over-count ordering above.
# someblob (id 12) declares maxlen 16; a length word of 17 (> 16) followed by only
# 1 payload byte then EOF is BOTH schema-invalid and truncated. The over-maxlen is
# decided at the length word (sofab.HeaderVisitor.FixlenHeader, before take() can
# report the payload short), so it MUST be INVALID, not INCOMPLETE.
# Wire: 62 (blob id 12) 8b 01 (fixlen word: len 17, blob subtype) 01 (1 of 17) <EOF>.
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\142\213\001\001' > "$WORK/overmaxlen_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overmaxlen_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'invalid message' || { echo "FAIL: over-maxlen(17>16)+truncated must be INVALID (invalid message); got: $ERR"; exit 1; }
# Precision control: an IN-BOUND length (16 == maxlen) that is genuinely truncated
# (1 of 16 payload bytes then EOF) is a clean truncation and MUST stay INCOMPLETE.
printf '\142\203\001\001' > "$WORK/inmaxlen_trunc.bin"
ERR=$( (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/inmaxlen_trunc.bin" 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'incomplete message' || { echo "FAIL: in-bound(16==16)+truncated must be INCOMPLETE; got: $ERR"; exit 1; }
echo "==> over-maxlen/truncation ordering OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to -- for fixlen, including the
# subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0) is declared u8
# (unsigned wire type) and keeps its schema default 7. Wire: 01 = id 0 with wire
# type SIGNED (1), then the zig-zag varint 06 (= 3). Control: 00 09 is the same id
# with the correct unsigned wire type and must decode to 9.
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/wiremismatch.bin") \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped field must keep its default 7; got: $OUT"; exit 1; }
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/wiremismatch_control.bin") \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: control must decode to 9; got: $OUT"; exit 1; }
echo "==> wire-type skip OK"

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
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/reopen_struct.bin") \
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
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/reopen_array.bin") \
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
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/fixsubtype.bin") \
    || { echo "FAIL: mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
    || { echo "FAIL: control (correct fp64 subtype) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
echo "==> fixlen subtype skip OK"

# Fixlen subtype mismatch AT A BOUNDED FIELD (generator#224, MESSAGE_SPEC S7.3):
# FixlenHeader fires for ANY fixlen subtype at a field id, so a maxlen guard that
# compares length alone measures a CONTRADICTING value against this field's bound
# and rejects it, where S7.3 requires it be skipped. The guard must be gated on the
# declared subtype. someblob (id 12) declares maxlen 16: a 17-byte STRING at that id
# is a subtype mismatch (skip -> someblob keeps its "Hello" default), while a
# 17-byte BLOB there is the genuine over-maxlen INVALID (asserted above).
# Wire: 62 (id 12 fixlen) 8a 01 (fixlen word: len 17, subtype STRING) + 17 bytes.
echo "==> over-bound fixlen at a MISMATCHED subtype must skip (S7.3, generator#224)"
printf '\142\212\001aaaaaaaaaaaaaaaaa' > "$WORK/fixsub_bounded.bin"
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/fixsub_bounded.bin") \
    || { echo "FAIL: 17-byte string at a maxlen-16 BLOB id must skip, not be measured against maxlen"; exit 1; }
echo "$OUT" | grep -q '"someblob":"SGVsbG8="' || { echo "FAIL: skipped fixlen field must keep its default; got: $OUT"; exit 1; }

# The reported shape: a fixlen FP value whose fixed width exceeds a small maxlen.
# Needs maxlen < 8, which the example has no field for, so use a dedicated schema.
# It also pins the HeaderVisitor method set: sofab.HeaderVisitor declares BOTH
# ArrayBegin and FixlenHeader and the cursor reaches them through ONE type
# assertion, so this maxlen-only message (no counted array) must still implement
# both -- emitting only FixlenHeader leaves the assertion failing and silently
# disables every header reject, which the truncation controls below would miss.
cat > "$WORK/fixsub.yaml" <<'YAML'
version: 1
messages:
  probe: { payload: { s: { id: 2, type: string, maxlen: 32 }, b: { id: 3, type: blob, maxlen: 4 } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$WORK/fixsub.yaml" --out "$WORK/fixsub" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/fixsub/go.mod"
( cd "$WORK/fixsub" && GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1 && go build ./... )
fixsub_decode() { (cd "$WORK/fixsub" && GOFLAGS=-mod=mod go run ./harness decode probe) }
# fp64 1.5 (8 bytes > maxlen 4) at the blob id 3 -> subtype mismatch -> skip.
# Wire: 1a (id 3 fixlen) 41 (len 8, subtype FP64) + 8 payload bytes.
OUT=$(printf '\032\101\000\000\000\000\000\000\370\077' | fixsub_decode) \
    || { echo "FAIL: fp64 at a maxlen-4 blob id must skip (generator#224)"; exit 1; }
echo "$OUT" | grep -q '"b":null' || { echo "FAIL: skipped fp64 must leave b at its default; got: $OUT"; exit 1; }
# fp32 1.5 (4 bytes, within the bound) at the same id: also a mismatch, also skipped.
OUT=$(printf '\032\040\000\000\300\077' | fixsub_decode) \
    || { echo "FAIL: fp32 at a maxlen-4 blob id must skip"; exit 1; }
echo "$OUT" | grep -q '"b":null' || { echo "FAIL: skipped fp32 must leave b at its default; got: $OUT"; exit 1; }
# Precision controls: the bound still bites on the MATCHING subtype, and still
# dominates truncation (generator#216) -- the gate must not disarm either.
printf '\032\043\001\002\003\004' | fixsub_decode >/dev/null \
    || { echo "FAIL: 4-byte blob (== maxlen 4) must decode"; exit 1; }
if printf '\032\053\001\002\003\004\005' | fixsub_decode >/dev/null 2>&1; then
    echo "FAIL: 5-byte blob (> maxlen 4) must be INVALID"; exit 1
fi
ERR=$( (printf '\032\053\001' | fixsub_decode 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'invalid message' || { echo "FAIL: over-maxlen(5>4)+truncated must be INVALID; got: $ERR"; exit 1; }
ERR=$( (printf '\032\043\001' | fixsub_decode 2>&1 >/dev/null) || true )
echo "$ERR" | grep -q 'incomplete message' || { echo "FAIL: in-bound(4==4)+truncated must be INCOMPLETE; got: $ERR"; exit 1; }
echo "==> bounded-field subtype gate OK"

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
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
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
OUT=$(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn102.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg-limits.yaml" <<YAML
generic: { emit: project, max_dyn_array_count: 4 }
targets: { go: { package: message, module_path: example.com/gen, go_version: "1.21" } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limits.yaml" --lang go --in "$WORK/dyn102.yaml" --out "$WORK/lim102" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/lim102/go.mod"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$WORK/dyn102.yaml" --out "$WORK/nolim102" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/nolim102/go.mod"
printf '\003\005\001\002\003\004\005' > "$WORK/over102.bin"   # id0 array, count 5 > cap 4
printf '\003\004\001\002\003\004' > "$WORK/in102.bin"         # count 4 == cap
if (cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/over102.bin" >/dev/null 2>&1); then
    echo "FAIL: over-cap dynamic array (count 5 > max_dyn_array_count 4) must fail (ErrLimitExceeded)"; exit 1
fi
(cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/in102.bin" >/dev/null) || { echo "FAIL: in-cap dynamic array must decode"; exit 1; }
(cd "$WORK/nolim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/over102.bin" >/dev/null) || { echo "FAIL: under the target default the same bytes must decode"; exit 1; }
echo "==> decode limits OK (over-cap rejected, in-cap + target-default accepted)"

# An over-cap count followed by END OF INPUT is LimitExceeded, not INCOMPLETE.
# The cap is decided at the count/length header (CORELIB_PLAN §6.2.1 "Enforcement
# point") and the rejection is terminal (§6.3): the header has arrived, the
# verdict is in, and no continuation can lift it -- so INCOMPLETE would both lose
# the category and invite the caller to feed bytes that cannot help. Go already
# answers this correctly because its visitor callback returns an error and aborts
# the feed on the spot; the case is pinned here as the family reference, since
# the ports whose callback is infallible have to CHOOSE the order and two of them
# had chosen the truncation.
echo "==> over-cap + truncation must be LimitExceeded, not INCOMPLETE (§6.2.1/§6.3)"
printf '\003\005\001\002' > "$WORK/over102trunc.bin"   # count 5 > cap 4, then EOF
printf '\003\004\001\002' > "$WORK/in102trunc.bin"     # count 4 == cap, then EOF
ERR=$( (cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/over102trunc.bin" >/dev/null) 2>&1 || true )
echo "$ERR" | grep -q 'decode limit exceeded' \
    || { echo "FAIL: over-cap(5>4)+truncated must be LimitExceeded, not INCOMPLETE; got: $ERR"; exit 1; }
# Precision control: an IN-cap count genuinely truncated is a clean truncation
# and MUST stay INCOMPLETE -- the cap must not turn every short message into a
# policy rejection.
ERR=$( (cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/in102trunc.bin" >/dev/null) 2>&1 || true )
echo "$ERR" | grep -q 'incomplete message' \
    || { echo "FAIL: in-cap(4==4)+truncated must stay INCOMPLETE; got: $ERR"; exit 1; }
echo "==> over-cap/truncation ordering OK"

# CORELIB_PLAN S6.2.1, the two rules a decoder-wide cap could not honour. Both
# are end-to-end assertions on generated code, because the generator's own unit
# tests can only see the emitted substrings and neither of these is a substring.
echo "==> a cap must not reach a schema-bounded field, nor a skipped one (S6.2.1)"
cat > "$WORK/excl.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u64 } }
      b: { id: 1, type: array, items: { type: i32, count: 100000 } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limits.yaml" --lang go --in "$WORK/excl.yaml" --out "$WORK/excl" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/excl/go.mod"
# b (id 1, signed array, count 6) is bounded by its own `count: 100000` and the
# cap of 4 must not touch it. Under the raise this decoded only because the cap
# had been lifted to 100000 for EVERY field, `a` included.
printf '\014\006\002\002\002\002\002\002' > "$WORK/bounded6.bin"
(cd "$WORK/excl" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/bounded6.bin" >/dev/null) \
    || { echo "FAIL: a schema-bounded array must not be judged against the receiver cap"; exit 1; }
# ...while the unbounded sibling at the same cap still rejects at 6.
printf '\003\006\001\001\001\001\001\001' > "$WORK/unbounded6.bin"
if (cd "$WORK/excl" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/unbounded6.bin" >/dev/null 2>&1); then
    echo "FAIL: the unbounded sibling must still be capped at 4"; exit 1
fi
# A field the visitor SKIPS is never capped (S6.2.1: it allocates nothing).
# id 9 is declared nowhere, so an over-cap array there must stay COMPLETE --
# this is generator#410, which the decoder-wide cap got wrong by construction.
printf '\113\005\001\001\001\001\001' > "$WORK/skipcap.bin"
(cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/skipcap.bin" >/dev/null) \
    || { echo "FAIL: an over-cap array at an UNDECLARED id must be skipped, not capped"; exit 1; }
echo "==> cap exclusivity OK (bounded sibling decodes, skipped field decodes)"

# The ENFORCEMENT POINT, pinned end-to-end (CORELIB_PLAN S6.2.1: a limit "MUST be
# enforced at the count/length header -- before the allocation it is meant to
# prevent -- for the same reason INVALID is decided there").
#
# This is what keeps the scalar cap in the generated FixlenBegin arm rather than
# on the one corelib call further down the path, sofab.PayloadAcc.Take. Take is
# reached from the PAYLOAD callback: for a message that ends right after an
# over-cap length word it is never called at all, so the same bytes would answer
# INCOMPLETE instead of LimitExceeded and nothing would have been rejected until
# after the payload was buffered. A guard that moves there looks identical in
# review -- hence a byte-level probe rather than a codegen assertion.
#
# Wire: 02 (id 0, fixlen) a2 06 (fixlen_word = (100 << 3) | 2 -> a 100-byte
# string) and then end of input. The cap is 24.
echo "==> an over-cap length must be refused AT THE HEADER, not after the payload"
cat > "$WORK/dynstr.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { s: { id: 0, type: string } } }
YAML
cat > "$WORK/cfg-strlim.yaml" <<YAML
generic: { emit: project, max_dyn_string_len: 24 }
targets: { go: { package: message, module_path: example.com/gen, go_version: "1.21" } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-strlim.yaml" --lang go --in "$WORK/dynstr.yaml" --out "$WORK/strlim" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/strlim/go.mod"
printf '\002\242\006' > "$WORK/overcap_trunc.bin"
OUT=$( (cd "$WORK/strlim" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/overcap_trunc.bin" 2>&1) || true )
case "$OUT" in
    *"limit exceeded"*) ;;
    *) echo "FAIL: an over-cap length word followed by truncation must be LimitExceeded at the header, got: $OUT"; exit 1 ;;
esac
# The control: the same header under the cap, equally truncated, is INCOMPLETE --
# so the case above is the cap firing and not the truncation being misreported.
# 02 62 = fixlen_word (12 << 3) | 2, a 12-byte string, no payload.
printf '\002\142' > "$WORK/incap_trunc.bin"
OUT=$( (cd "$WORK/strlim" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/incap_trunc.bin" 2>&1) || true )
case "$OUT" in
    *"limit exceeded"*) echo "FAIL: an UNDER-cap truncated string must not be LimitExceeded, got: $OUT"; exit 1 ;;
esac
echo "==> header enforcement point OK"

# A native matrix ROW's own element count. The outer `count:` bounds the row ID;
# the row's count header was bounded by nothing generated code passed on -- it
# rode on the decoder-wide cap, and with that gone the row bound has to travel to
# the collector as its own sofab.Bounds, with the array cap behind it.
echo "==> a matrix row's element count is bounded (MESSAGE_SPEC S7.1)"
cat > "$WORK/mat.yaml" <<'YAML'
version: 1
messages:
  mat:
    payload:
      m: { id: 0, type: array, items: { type: array, count: 2, items: { type: u32, count: 2 } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$WORK/mat.yaml" --out "$WORK/mat" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/mat/go.mod"
printf '\006\003\002\001\002\007' > "$WORK/row_ok.bin"    # row 0: 2 elements == count 2
printf '\006\003\003\001\002\003\007' > "$WORK/row_over.bin" # row 0: 3 elements > count 2
(cd "$WORK/mat" && GOFLAGS=-mod=mod go run ./harness decode mat < "$WORK/row_ok.bin" >/dev/null) \
    || { echo "FAIL: an at-count matrix row must decode"; exit 1; }
if (cd "$WORK/mat" && GOFLAGS=-mod=mod go run ./harness decode mat < "$WORK/row_over.bin" >/dev/null 2>&1); then
    echo "FAIL: a matrix row over its schema count must be INVALID"; exit 1
fi
echo "==> matrix row count OK"

# The WRAPPER-array half of S6.2.1 (generator#402 item 3), and the only
# measurement that settles it: a nine-byte message must not be able to allocate.
#
# A wrapper array carries no count header -- its length is *highest present id +
# 1* (MESSAGE_SPEC S5.1) -- so the element INDEX is the array's length, and one
# element at a large id forces an arbitrarily large allocation out of nothing.
# S6.2.1 names the index for that reason and puts the check "before the container
# it indexes into is extended". Against the generator and corelib-go this branch
# replaces, the four images below decoded to COMPLETE while allocating 172 MB,
# 41 MB and 250 MB.
#
# The verdict alone would not prove it, so the probe reads runtime.MemStats
# around each decode and asserts the allocation NEVER HAPPENED: a cap that
# rejects after the container was grown has prevented nothing. It also pins the
# two things the cap must NOT do -- reach a schema-bounded array (its own `count:`
# governs, ErrInvalidMsg) and reject anything under the cap.
echo "==> a wrapper array's element index is capped, and nothing is allocated (generator#402)"
cat > "$WORK/dyn402.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      w: { id: 0, type: array, items: { type: string } }
      p: { id: 1, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }
      n: { id: 2, type: array, items: { type: array, items: { type: string } } }
      b: { id: 3, type: array, items: { type: string, count: 4 } }
YAML
cat > "$WORK/cfg-402.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 64, max_dyn_string_len: 32, max_dyn_blob_len: 32 }
targets: { go: { package: message, module_path: example.com/gen, go_version: "1.21" } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-402.yaml" --lang go --in "$WORK/dyn402.yaml" --out "$WORK/lim402" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/lim402/go.mod"
mkdir -p "$WORK/lim402/probe"
cat > "$WORK/lim402/probe/main.go" <<'GO'
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	message "example.com/gen/message"
	sofab "github.com/sofa-buffers/corelib-go"
)

func varint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// hdr: (id << 3) | wire. Wire 2 = Fixlen, 6 = SequenceStart, 7 = SequenceEnd
// (MESSAGE_SPEC S4.3, S4.9).
func hdr(out []byte, id, wire uint64) []byte { return varint(out, (id<<3)|wire) }

// Four orders of magnitude below the wire-sized allocation: what is being
// excluded is the container, not every byte the decode touches.
const budget = 1 << 16

var failures int

func verdict(err error) string {
	switch {
	case err == nil:
		return "Complete"
	case errors.Is(err, sofab.ErrLimitExceeded):
		return "LimitExceeded"
	case errors.Is(err, sofab.ErrInvalidMsg):
		return "Invalid"
	case errors.Is(err, sofab.ErrIncomplete):
		return "Incomplete"
	case errors.Is(err, sofab.ErrArgument):
		return "InvalidArgument"
	}
	return err.Error()
}

func run(what string, wire []byte, want string) {
	m := message.NewDyn()
	_ = sofab.AcceptBytes(wire, m) // warm, so the measured decode counts wire-driven growth

	var before, after runtime.MemStats
	m = message.NewDyn()
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := sofab.AcceptBytes(wire, m)
	runtime.ReadMemStats(&after)
	used := after.TotalAlloc - before.TotalAlloc
	got := verdict(err)
	fmt.Printf("    %-34s verdict=%-15s allocated=%d bytes\n", what, got, used)
	if got != want {
		fmt.Printf("FAIL: %s: expected %s, got %s\n", what, want, got)
		failures++
	}
	if used > budget {
		fmt.Printf("FAIL: %s: allocated %d bytes -- the allocation the cap exists to prevent HAPPENED\n", what, used)
		failures++
	}
}

func main() {
	const big = 2000000 // ~64 MB of string headers, asked for in nine bytes

	var w []byte // array<string>, schema-unbounded
	w = hdr(w, 0, 6)
	w = hdr(w, big, 2)
	w = varint(w, (1<<3)|2) // fixlen word: 1 byte, subtype String
	w = append(w, 'A')
	w = hdr(w, 0, 7)
	fmt.Printf("    attack message: %d bytes, one element at index %d\n", len(w), big)
	run("array<string> over-index", w, "LimitExceeded")

	var p []byte // array<struct>
	p = hdr(p, 1, 6)
	p = hdr(p, big, 6)
	p = hdr(p, 0, 7)
	p = hdr(p, 0, 7)
	run("array<struct> over-index", p, "LimitExceeded")

	var n []byte // array<array<string>>
	n = hdr(n, 2, 6)
	n = hdr(n, big, 6)
	n = hdr(n, 0, 7)
	n = hdr(n, 0, 7)
	run("array<array<string>> over-index", n, "LimitExceeded")

	// The schema bounds this one, so the cap must not touch it: `count: 4`
	// governs and an over-index element is INVALID (MESSAGE_SPEC S7.1).
	var b []byte
	b = hdr(b, 3, 6)
	b = hdr(b, big, 2)
	b = varint(b, (1<<3)|2)
	b = append(b, 'A')
	b = hdr(b, 0, 7)
	run("bounded array<string> over-index", b, "Invalid")

	// The element LENGTH cap, the collector's second axis: one element longer
	// than max_dyn_string_len, at a perfectly ordinary index.
	var l []byte
	l = hdr(l, 0, 6)
	l = hdr(l, 0, 2)
	l = varint(l, (64<<3)|2)
	for i := 0; i < 64; i++ {
		l = append(l, 'A')
	}
	l = hdr(l, 0, 7)
	run("array<string> over-long element", l, "LimitExceeded")

	// The control: a sparse array under the caps decodes intact, at its wire
	// length -- highest present id + 1 -- and a cap never truncates.
	var ok []byte
	ok = hdr(ok, 0, 6)
	ok = hdr(ok, 3, 2)
	ok = varint(ok, (2<<3)|2)
	ok = append(ok, 'h', 'i')
	ok = hdr(ok, 0, 7)
	good, err := message.DecodeDyn(ok)
	if err != nil || len(good.W) != 4 || good.W[3] != "hi" {
		fmt.Printf("FAIL: an in-cap sparse wrapper array must decode intact: %v\n", err)
		failures++
	} else {
		fmt.Printf("    %-34s verdict=Complete        len=%d, last=%q\n", "in-cap control", len(good.W), good.W[3])
	}

	if failures > 0 {
		os.Exit(1)
	}
}
GO
(cd "$WORK/lim402" && GOFLAGS=-mod=mod go run ./probe) \
    || { echo "FAIL: wrapper-array receiver caps (generator#402, S6.2.1)"; exit 1; }
echo "==> wrapper index + element caps OK (rejected before the allocation, bounded array untouched)"

# Declared integer width is a VALIDITY bound (MESSAGE_SPEC S7.1 + documentation#32,
# generator#266, Crucible F-0033 / codegen defect G-0026). A value outside the
# declared width is INVALID: it MUST NOT be masked to the width and MUST NOT be
# kept. The example schema has no narrow-int field at a known id, so probe with a
# dedicated one.
#
# Wire: id 0 unsigned -> header 0x00, then the varint.
#   ff 7f    = 16383   (the reported reproducer)
#   80 02    = 256     (one past a u8)
#   ff 01    = 255     (the in-range control: must decode AND round-trip)
#   f0 a2 04 = 70000   (one field up, at the u16)
cat > "$WORK/width.yaml" <<'YAML'
version: 1
messages:
  probe: { payload: { a: { id: 0, type: u8 }, b: { id: 1, type: u16 }, c: { id: 2, type: u64 } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$WORK/width.yaml" --out "$WORK/width" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/width/go.mod"
( cd "$WORK/width" && GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1 && go build ./... )
width_decode() { (cd "$WORK/width" && GOFLAGS=-mod=mod go run ./harness decode probe) }

echo "==> over-width scalar must be INVALID (S7.1, generator#266)"
if printf '\000\377\177' | width_decode >/dev/null 2>&1; then
    echo "FAIL: 16383 into a u8 must be INVALID, not masked to 255 and not kept"; exit 1
fi
if printf '\000\200\002' | width_decode >/dev/null 2>&1; then
    echo "FAIL: 256 into a u8 must be INVALID"; exit 1
fi
if printf '\010\360\242\004' | width_decode >/dev/null 2>&1; then
    echo "FAIL: 70000 into a u16 must be INVALID"; exit 1
fi
# The in-range control still decodes, keeps its exact value, and round-trips: the
# guard must reject only what is genuinely out of range.
OUT=$(printf '\000\377\001' | width_decode) || { echo "FAIL: in-range control 255 must decode"; exit 1; }
echo "$OUT" | grep -q '"a":255' || { echo "FAIL: control must keep 255 exactly; got: $OUT"; exit 1; }
# A u64 destination has no narrower bound: the same large value is simply valid.
OUT=$(printf '\020\377\377\377\377\377\377\377\377\377\001' | width_decode) \
    || { echo "FAIL: a u64 field must accept the full 64-bit range"; exit 1; }
echo "$OUT" | grep -q '"c":18446744073709551615' || { echo "FAIL: u64 max must survive; got: $OUT"; exit 1; }
echo "==> declared-width reject OK"

# The verdict must not depend on the chunking either (CORELIB_PLAN S6.4 / S7.2
# item 4: a chunk boundary MUST NOT affect the outcome). Every malformed fixture
# built above is replayed through the byte-at-a-time reader and must land on the
# SAME side as the in-memory decode -- with the well-formed controls alongside,
# so this cannot pass by rejecting everything.
#
# Written as a table rather than as one assertion per file: the interesting
# property is that decode and streamdecode never disagree, and that is a claim
# about the whole set.
echo "==> a chunk boundary must not change the verdict (generator#312)"
CHECKED=0
ACCEPTED=0
REJECTED=0
for f in overcount control fp64_at_fp32 fp32_overcount skipped_bad_utf8 declared_bad_utf8 \
         overcount_trunc incount_trunc overindex overindex_control overindex_trunc \
         inindex_trunc overmaxlen overmaxlen_control rt; do
    # A missing fixture is a FAILURE, not a skip. Silently continuing would turn
    # a renamed .bin into a green run that checked nothing -- the exact shape
    # that makes a conformance suite lie.
    [ -f "$WORK/$f.bin" ] || { echo "FAIL: fixture $f.bin missing (renamed?)"; exit 1; }
    CHECKED=$((CHECKED + 1))
    if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/$f.bin" >/dev/null 2>&1); then
        W=accept
    else
        W=reject
    fi
    if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness streamdecode myfirstmessage < "$WORK/$f.bin" >/dev/null 2>&1); then
        D=accept
    else
        D=reject
    fi
    [ "$W" = "$D" ] || { echo "FAIL: $f.bin -> decode=$W streamdecode=$D"; exit 1; }
    if [ "$W" = accept ]; then ACCEPTED=$((ACCEPTED + 1)); else REJECTED=$((REJECTED + 1)); fi
done
[ "$CHECKED" -eq 15 ] || { echo "FAIL: expected 15 fixtures, checked $CHECKED"; exit 1; }
# Both outcomes must appear. A streaming path that accepted everything, or one
# that rejected everything, agrees with itself perfectly -- the table is only
# evidence if it straddles the boundary.
[ "$ACCEPTED" -gt 0 ] && [ "$REJECTED" -gt 0 ] || {
    echo "FAIL: table is one-sided ($ACCEPTED accept / $REJECTED reject)"; exit 1
}
echo "==> chunk-invariant verdicts OK ($CHECKED fixtures: $ACCEPTED accept, $REJECTED reject)"

# The BOUNDED encode arm (CORELIB_PLAN §5.1). A schema whose every field carries
# a count/maxlen has a worst case, so Encode allocates exactly MaxSize bytes and
# hands them to the corelib, which never grows or reallocates them. example.yaml
# is unbounded (it exercises the scratch+sink arm), so without this schema the
# bounded arm is never executed at all.
#
# The fill message pins the size from both sides: filling every field to its
# declared bound must encode to exactly MAX_SIZE bytes, so the buffer can be
# neither short (a legal message would not fit) nor slack (RAM paid for nothing).
echo "==> bounded encode buffer is exactly MAX_SIZE (ARCHITECTURE §9.6)"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$ROOT/tests/conformance/lib/maxsize_fill.yaml" --out "$WORK/fill" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/fill/go.mod"
( cd "$WORK/fill" && GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1 && go build -o harness_bin ./harness )
check_maxsize_fill go "$WORK/fill/harness_bin" encode fill

# ...and the other side of owning the buffer: a value the caller filled PAST its
# own schema bound does not fit, and §5.1 forbids returning partial output as if
# it were complete. So the encode must FAIL and write nothing — the old
# corelib-grown buffer silently emitted an over-bound message that every receiver
# would then reject as INVALID.
echo "==> an over-filled bounded value must be refused, not truncated (§5.1)"
sed 's/"f_str": *"[^"]*"/"f_str": "'"$(printf 'x%.0s' $(seq 1 400))"'"/' \
    "$ROOT/tests/conformance/lib/maxsize_fill.json" > "$WORK/overfill.json"
grep -q 'xxxxxxxxxx' "$WORK/overfill.json" || { echo "FAIL: could not build the over-filled input (f_str renamed?)"; exit 1; }
if "$WORK/fill/harness_bin" encode fill < "$WORK/overfill.json" > "$WORK/overfill.bin" 2>/dev/null; then
    echo "FAIL: a string 400 bytes into a maxlen-9 field must be reported, not encoded"; exit 1
fi
[ ! -s "$WORK/overfill.bin" ] || {
    echo "FAIL: a refused encode emitted $(wc -c < "$WORK/overfill.bin") bytes of partial output"; exit 1
}
echo "==> over-fill refusal OK"

echo "==> shared-vector byte-exact conformance"
( cd "$ROOT" && SOFAB_GO_CORELIB="$CORELIB" go test ./generators/golang/ -run "Conformance|Wire" -count=1 )

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/corpus/$name/go.mod"
    ( cd "$WORK/corpus/$name" && GOFLAGS=-mod=mod go build ./... )
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
