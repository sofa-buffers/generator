#!/usr/bin/env sh
# Reproducible Python conformance harness: generate -> syntax-check ->
# round-trip -> byte-exact shared-vector conformance against corelib-py.
#
# Usage: tests/conformance/python/run.sh [path-to-corelib-py]   (or set $SOFAB_PY_CORELIB)
# Requires: go, python3, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_PY_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-py"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-py.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-py: $CORELIB"
export PYTHONPATH="$CORELIB/src"

cat > "$WORK/cfg.yaml" <<YAML
generic: { emit: project }
targets: { python: {} }
YAML

echo "==> generating Python project"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in examples/messages/example.yaml --out "$WORK/proj" )

echo "==> syntax check"
python3 -m py_compile "$WORK/proj/message.py" "$WORK/proj/harness.py"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/proj" && printf '%s' "$IN" | python3 harness.py encode myfirstmessage | python3 harness.py decode myfirstmessage)
echo "$OUT" | grep -q '"someu64": 18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint": -99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2). A count header of 6 (> 4) followed by only 2 elements then EOF
# is BOTH over-count and truncated; the count is on the delivered field header
# (fld.count) before any element, so it MUST be reported INVALID (SofaDecodeError),
# not INCOMPLETE (SofaIncompleteError). Wire: 7b (id 15 unsigned-array) 06 01 02 EOF.
echo "==> over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\173\006\001\002' > "$WORK/overcount_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overcount_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaDecodeError' || { echo "FAIL: over-count(6>4)+truncated must be INVALID (SofaDecodeError); got: $ERR"; exit 1; }
# Precision control: an in-bound count (4 == bound) that is genuinely truncated
# (2 of 4 elements then EOF) is a clean truncation and MUST stay INCOMPLETE.
printf '\173\004\001\002' > "$WORK/incount_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/incount_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaIncompleteError' || { echo "FAIL: in-bound(4==4)+truncated must be INCOMPLETE (SofaIncompleteError); got: $ERR"; exit 1; }
echo "==> over-count/truncation ordering OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to -- for fixlen, including the
# subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0) is declared u8
# (unsigned wire type) and keeps its schema default 7. Wire: 01 = id 0 with wire
# type SIGNED (1), then the zig-zag varint 06 (= 3). Reading it as the schema type
# would yield 3 (or the raw 6); skipping leaves the default. Control: 00 09 is the
# same id with the correct unsigned wire type and must decode to 9. A third
# vector, 06 07, gives the same id a SEQUENCE_START header closed by its
# SEQUENCE_END: skipping that one has to drain the whole nested sequence, not
# just a scalar payload, so it exercises the riskiest branch of skip().
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
printf '\006\007' > "$WORK/wiremismatch_seq.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/wiremismatch.bin" ) \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8": 7' || { echo "FAIL: skipped field must keep its default 7, got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/wiremismatch_control.bin" ) \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8": 9' || { echo "FAIL: control must decode to 9, got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/wiremismatch_seq.bin" ) \
    || { echo "FAIL: sequence header on a scalar field must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8": 7' || { echo "FAIL: skipped sequence must keep the default 7, got: $OUT"; exit 1; }
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
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/reopen_struct.bin" ) \
    || { echo "FAIL: re-opened struct must decode"; exit 1; }
echo "$OUT" | grep -q '"nestedstring": "x"' || { echo "FAIL: re-opened struct must retain nestedstring \"x\", got: $OUT"; exit 1; }
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
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/reopen_array.bin" ) \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
printf '%s' "$OUT" | python3 -c '
import json, sys
a = json.load(sys.stdin)["somestringarray"]
if "b" in a:
    sys.exit("FAIL: re-opened array wrapper must be replaced, not merged (element \"b\" survived): %r" % (a,))
if not a or a[0] != "c":
    sys.exit("FAIL: re-opened array wrapper must hold the second opening'"'"'s element 0 == \"c\": %r" % (a,))
' || exit 1
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
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/fixsubtype.bin" ) \
    || { echo "FAIL: mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64": 3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/fixsubtype_control.bin" ) \
    || { echo "FAIL: control (correct fp64 subtype) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64": 2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
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
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/skipped_occ_array.bin" ) \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"somestringarray": \["a"' || { echo "FAIL: skipped occurrence must not clear the array (element 0 == \"a\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps array OK"

# S7.3 x S7.4, struct: same rule for a struct scope. somestruct (id 20) is opened
# correctly with nestedstring (id 1) = "x", then id 20 recurs carrying the
# UNSIGNED wire type. That occurrence is skipped, so nestedstring MUST still
# be "x" rather than falling back to its default "Nested".
# Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
#       a0 01 (id 20, UNSIGNED) 05
echo "==> mis-typed later occurrence must not clear the struct (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\240\001\005' > "$WORK/skipped_occ_struct.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/skipped_occ_struct.bin" ) \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring": "x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

# Receiver-side decode limits (generator#102): max_dyn_array_count: 4 caps a
# count-less (schema-unbounded) u64 array. Wire header 0x03 = id 0, unsigned
# array; a wire count of 5 MUST fail decode with the corelib limit error,
# exactly 4 still decodes, and the same oversized bytes decode fine against a
# project generated WITHOUT the limit (unset = unlimited).
echo "==> receiver-side decode limits must reject over-cap counts (generator#102)"
cat > "$WORK/limit-def.yaml" <<YAML
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u64 } }
YAML
cat > "$WORK/limit-cfg.yaml" <<YAML
generic: { emit: project, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/limit-cfg.yaml" --lang python --in "$WORK/limit-def.yaml" --out "$WORK/limitproj" )
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in "$WORK/limit-def.yaml" --out "$WORK/nolimitproj" )
printf '\003\005\001\002\003\004\005' > "$WORK/limit-over.bin"
printf '\003\004\001\002\003\004' > "$WORK/limit-ok.bin"
if (cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/limit-over.bin" >/dev/null 2>"$WORK/limit-err.txt"; then
    echo "FAIL: wire count 5 > max_dyn_array_count 4 must fail decode"; exit 1
fi
grep -qi "limit" "$WORK/limit-err.txt" || { echo "FAIL: over-cap decode error should mention the limit"; cat "$WORK/limit-err.txt"; exit 1; }
(cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/limit-ok.bin" >/dev/null || { echo "FAIL: wire count 4 must decode under limit 4"; exit 1; }
(cd "$WORK/nolimitproj" && python3 harness.py decode dyn) < "$WORK/limit-over.bin" >/dev/null || { echo "FAIL: unset limit must keep count 5 decodable"; exit 1; }
echo "==> decode-limit reject OK"

echo "==> shared-vector byte-exact conformance"
( cd "$ROOT" && SOFAB_PY_CORELIB="$CORELIB" go test ./generators/python/ -run Conformance -count=1 )

echo "==> corpus + realworld: every definition imports"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang python --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    PYTHONPATH="$CORELIB/src:$WORK/corpus/$name" python3 -c "import message" \
        || { echo "FAIL: corpus def $name did not import"; exit 1; }
done
echo "==> corpus imports ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
