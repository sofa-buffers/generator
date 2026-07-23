#!/usr/bin/env sh
# Reproducible Go conformance harness: generate -> build -> round-trip ->
# byte-exact shared-vector conformance against corelib-go.
#
# Usage: tests/conformance/go/run.sh [path-to-corelib-go]   (or set $SOFAB_GO_CORELIB)
# Requires: go, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_GO_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-go"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-go.git "$WORK/corelib" >/dev/null 2>&1
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
echo "==> over-index reject OK"

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
(cd "$WORK/nolim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/over102.bin" >/dev/null) || { echo "FAIL: without limits the same bytes must decode"; exit 1; }
echo "==> decode limits OK (over-cap rejected, in-cap + unlimited accepted)"

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
