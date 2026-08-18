#!/usr/bin/env sh
# Reproducible TypeScript conformance harness: build corelib-ts, generate ->
# typecheck -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/typescript/run.sh [path-to-corelib-ts]   (or set $SOFAB_TS_CORELIB)
# Requires: go, node, npm, git.
set -eu

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"
# Shared MAX_SIZE fill check (ARCHITECTURE §9.6).
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_TS_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning + building corelib-ts"
    clone_corelib corelib-ts "$WORK/corelib"
    ( cd "$WORK/corelib" && npm install >/dev/null 2>&1 && npm run build >/dev/null 2>&1 )
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-ts: $CORELIB"
[ -f "$CORELIB/dist/index.js" ] || { echo "FAIL: corelib-ts not built (no dist/)"; exit 1; }

# Conformance def: one single-field message per scalar op.
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
cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project }
targets: { typescript: {} }
YAML

# gen <def> <outdir> [config]  — config defaults to the shared $WORK/cfg.yaml.
# The int64-mode loop MUST pass its own config: without it every mode project is
# generated with the default (bigint) and the mode comparison is vacuous.
gen() { ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang typescript --in "$1" --out "$2" ); }

# Instantiate the differential decode harness into a generated project. Defined
# here rather than beside its first heavy use: the int64 legs above the streaming
# section reach for it too.
mk_stream_check() { # mk_stream_check <projdir> <import-line> <body>
    sed -e "s|//SOFAB_IMPORT|$2|" -e "s|//SOFAB_BODY|$3|" \
        "$ROOT/tests/conformance/typescript/stream_check.ts" > "$1/stream_check.ts"
}

echo "==> generating example + conformance projects"
gen "$ROOT/examples/messages/example.yaml" "$WORK/ex"
gen "$WORK/conf.yaml" "$WORK/conf"

setup() {
    node -e "const p=require('$1/package.json');p.dependencies['@sofa-buffers/corelib']='file:$CORELIB';require('fs').writeFileSync('$1/package.json',JSON.stringify(p))"
    # Retry once; surface the output on a second failure (npm can be flaky).
    ( cd "$1" && npm install --no-audit --no-fund --silent ) \
        || ( cd "$1" && npm install --no-audit --no-fund )
}
setup "$WORK/ex"
setup "$WORK/conf"

echo "==> typecheck generated code"
( cd "$WORK/ex" && npx tsc --noEmit )

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":"18446744073709551615","somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/ex" && printf '%s' "$IN" | npx tsx harness.ts encode myfirstmessage | npx tsx harness.ts decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":"18446744073709551615"' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# The two encode-buffer arms (CORELIB_PLAN §5.1). The caller owns the output
# buffer: generated code allocates it and the corelib neither grows nor
# reallocates it. Which shape applies is a property of the SCHEMA, and
# example.yaml is unbounded — every leg above already drives the scratch+sink arm
# — so without this schema the BOUNDED arm is never executed at all.
#
# The fill message pins the size from both sides: filling every field to its
# declared bound must encode to exactly MAX_SIZE bytes, so the buffer can be
# neither short (a legal message would not fit) nor slack (RAM paid for nothing).
echo "==> bounded encode buffer is exactly MAX_SIZE (ARCHITECTURE §9.6)"
gen "$ROOT/tests/conformance/lib/maxsize_fill.yaml" "$WORK/fill"
ln -s "$WORK/ex/node_modules" "$WORK/fill/node_modules"
( cd "$WORK/fill" && npx tsc --noEmit )
grep -q 'static readonly MAX_SIZE = 234;' "$WORK/fill/message.ts" \
    || { echo "FAIL: bounded message must emit a derived MAX_SIZE, not a ceiling"; exit 1; }
# JSON.parse is the harness's front door and a JS number is a double, so an
# integer above 2^53 in the shared fill input would arrive ROUNDED — u64 max
# reads back as 2^64, which the encoder rightly refuses as out of range. Quote
# those so the generated fromJSON takes its bigint path, the same spelling the
# example round-trip above already uses for u64. Nothing else about the input
# changes, so every field is still filled to its declared bound.
quote_big_ints() {
    python3 -c 'import re,sys; sys.stdout.write(re.sub(r"-?\d+(?![\d.eE\"])", lambda m: "\"%s\"" % m.group(0) if abs(int(m.group(0))) > 2**53 else m.group(0), sys.stdin.read()))'
}
fill_encode() { quote_big_ints | ( cd "$WORK/fill" && npx tsx harness.ts encode fill ); }
check_maxsize_fill typescript fill_encode

# ...and the other side of owning the buffer: a value the caller filled PAST its
# own schema bound does not fit, and §5.1 forbids returning partial output as if
# it were complete. So the encode must FAIL and write nothing — the corelib-grown
# stream used to silently emit an over-bound message every receiver then rejects
# as INVALID. This is also the only encode-side bound the TS backend has: it
# emits no maxlen/count validation of its own.
echo "==> an over-filled bounded value must be refused, not truncated (§5.1)"
OVERFILL="$WORK/overfill.json"
sed 's/"f_str": *"[^"]*"/"f_str": "'"$(printf 'x%.0s' $(seq 1 400))"'"/' \
    "$ROOT/tests/conformance/lib/maxsize_fill.json" > "$OVERFILL"
grep -q 'xxxxxxxxxx' "$OVERFILL" || { echo "FAIL: could not build the over-filled input (f_str renamed?)"; exit 1; }
if fill_encode < "$OVERFILL" > "$WORK/overfill.bin" 2>/dev/null; then
    echo "FAIL: a string 400 bytes into a maxlen-9 field must be reported, not encoded"; exit 1
fi
[ ! -s "$WORK/overfill.bin" ] || {
    echo "FAIL: a refused encode emitted $(wc -c < "$WORK/overfill.bin") bytes of partial output"; exit 1
}
echo "==> encode-buffer ownership OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
(cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2). someuintarray declares count 4; a header announcing 6 elements
# (> 4) followed by only 2 elements then EOF is BOTH schema-invalid and truncated.
# The schema count is passed to readUnsignedArray, so the over-count is decided at
# the count word (before the reader's own truncated-array INCOMPLETE) -- the message
# MUST report INVALID. The `status` harness mode surfaces the SofabError.code so the
# distinction (which a bare non-zero exit hides) is asserted directly.
# Wire: 7b (id 15 unsigned-array) 06 (count 6) 01 02 (2 of 6 elements) <EOF>.
echo "==> over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\173\006\001\002' > "$WORK/overcount_trunc.bin"
ST=$( (cd "$WORK/ex" && npx tsx harness.ts status myfirstmessage) < "$WORK/overcount_trunc.bin" | head -n1 )
[ "$ST" = "INVALID" ] || { echo "FAIL: over-count(6>4)+truncated -> $ST (want INVALID)"; exit 1; }
# Precision control: an IN-BOUND count (4 == bound) genuinely truncated (2 of 4
# then EOF) is a clean truncation and MUST stay INCOMPLETE.
printf '\173\004\001\002' > "$WORK/incount_trunc.bin"
ST=$( (cd "$WORK/ex" && npx tsx harness.ts status myfirstmessage) < "$WORK/incount_trunc.bin" | head -n1 )
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(4==4)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> over-count/truncation ordering OK"

# The same ordering one level down, at the ELEMENT (generator#267 residue,
# Crucible F-0043 width_elem_trunc). someuintarray declares u32 elements; an
# element carrying 2^32 is outside that width, which S7.1 makes INVALID, and it
# is established by its own bytes -- so S5.2 keeps the verdict INVALID however
# little of the array follows. The bound is passed INTO readUnsignedArray so it
# is applied at that element; a scan over the returned array cannot fire for one
# that never assembles.
# Wire: 7b (id 15 unsigned-array) 04 (count 4) 80 80 80 80 10 (2^32) <EOF>.
echo "==> over-width element + truncation must be INVALID (generator#267)"
printf '\173\004\200\200\200\200\020' > "$WORK/overwidth_trunc.bin"
ST=$( (cd "$WORK/ex" && npx tsx harness.ts status myfirstmessage) < "$WORK/overwidth_trunc.bin" | head -n1 )
[ "$ST" = "INVALID" ] || { echo "FAIL: over-width element + truncated -> $ST (want INVALID)"; exit 1; }
# Precision control: an IN-RANGE element cut at the same offset decides nothing,
# so the truncation IS the verdict.
printf '\173\004\001' > "$WORK/inwidth_trunc.bin"
ST=$( (cd "$WORK/ex" && npx tsx harness.ts status myfirstmessage) < "$WORK/inwidth_trunc.bin" | head -n1 )
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-range element + truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> element-width/truncation ordering OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
(cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
(cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Over-maxlen AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2), the string/blob analogue of the over-count ordering above.
# someblob (id 12) declares maxlen 16; a length word of 17 (> 16) followed by only 1
# payload byte then EOF is decided at the length word (readBlob's schemaMaxlen,
# before the payload take), so it MUST be INVALID.
# Wire: 62 (blob id 12) 8b 01 (fixlen word: len 17, blob subtype) 01 (1 of 17) <EOF>.
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\142\213\001\001' > "$WORK/overmaxlen_trunc.bin"
ST=$( (cd "$WORK/ex" && npx tsx harness.ts status myfirstmessage) < "$WORK/overmaxlen_trunc.bin" | head -n1 )
[ "$ST" = "INVALID" ] || { echo "FAIL: over-maxlen(17>16)+truncated -> $ST (want INVALID)"; exit 1; }
# Precision control: an IN-BOUND length (16 == maxlen) genuinely truncated (1 of 16
# payload bytes then EOF) is a clean truncation and MUST stay INCOMPLETE.
printf '\142\203\001\001' > "$WORK/inmaxlen_trunc.bin"
ST=$( (cd "$WORK/ex" && npx tsx harness.ts status myfirstmessage) < "$WORK/inmaxlen_trunc.bin" | head -n1 )
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: in-bound(16==16)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
echo "==> over-maxlen/truncation ordering OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to -- for fixlen, including the
# subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0) is declared u8
# (unsigned wire type) and keeps its schema default 7. Wire: 01 = id 0 with wire
# type SIGNED (1), then the zig-zag varint 06 (= 3). Control: 00 09 is the same id
# with the correct unsigned wire type and must decode to 9. A third vector, 06 07,
# gives the same id a SEQUENCE_START header closed by its SEQUENCE_END: skipping
# that one has to drain the whole nested sequence, not just a scalar payload.
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
printf '\006\007' > "$WORK/wiremismatch_seq.bin"
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/wiremismatch.bin" ) \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped field must keep its default 7; got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/wiremismatch_control.bin" ) \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: control must decode to 9; got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/wiremismatch_seq.bin" ) \
    || { echo "FAIL: sequence header on a scalar field must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: skipped sequence must keep the default 7; got: $OUT"; exit 1; }
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
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/reopen_struct.bin" ) \
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
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/reopen_array.bin" ) \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: re-opened array wrapper must start with the second opening's element 0 == \"c\"; got: $OUT"; exit 1; }
if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
    echo "FAIL: re-opened array wrapper must be replaced, not merged (element \"b\" survived); got: $OUT"; exit 1
fi
echo "==> array wrapper replace OK"

# Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174). Under S7.3 a fixlen
# field's type is its wire type PLUS its subtype, so 4a 0a 78 (id 9 somefp64,
# Fixlen wire type but STRING subtype) must be SKIPPED and leave the default
# 3.141592653589793. corelib-ts now records the delivered subtype as Cursor.fixSub
# (corelib-ts#58), so the generated guard checks `c.fixSub !== FixlenSubtype.Fp64`
# and skips on a mismatch instead of throwing from the wrong-typed reader.
# Control 4a 41 <8 bytes 2.5> carries the correct fp64 subtype and must decode to 2.5.
echo "==> fixlen subtype mismatch must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\112\012\170' > "$WORK/subtype_mismatch.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/subtype_control.bin"
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/subtype_mismatch.bin" ) \
    || { echo "FAIL: fixlen subtype mismatch must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default; got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/subtype_control.bin" ) \
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
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/skipped_occ_array.bin" ) \
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
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/skipped_occ_struct.bin" ) \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

# Receiver-side decode limits (generator#102): a count-less u64 array with
# max_dyn_array_count: 4 baked into the generated module (id 0 -> header 0x03 =
# 0<<3 | unsigned-array). A wire count of 5 MUST throw the corelib's
# LIMIT_EXCEEDED (decode exits non-zero); a count of 4 still decodes; and the
# same 5-element bytes MUST decode in a project generated WITHOUT limits.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg_lim.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
targets: { typescript: {} }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg_lim.yaml" --lang typescript --in "$WORK/dyn.yaml" --out "$WORK/lim" )
gen "$WORK/dyn.yaml" "$WORK/nolim"
ln -s "$WORK/ex/node_modules" "$WORK/lim/node_modules"
ln -s "$WORK/ex/node_modules" "$WORK/nolim/node_modules"
( cd "$WORK/lim" && npx tsc --noEmit )
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if (cd "$WORK/lim" && npx tsx harness.ts decode dyn) < "$WORK/overlimit.bin" >/dev/null 2>"$WORK/limerr.txt"; then
    echo "FAIL: dynamic array count 5 must exceed max_dyn_array_count 4"; exit 1
fi
grep -q "maxArrayCount" "$WORK/limerr.txt" || { echo "FAIL: over-limit error must mention the limit"; cat "$WORK/limerr.txt"; exit 1; }
(cd "$WORK/lim" && npx tsx harness.ts decode dyn) < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: count == limit (4) must decode"; exit 1; }
(cd "$WORK/nolim" && npx tsx harness.ts decode dyn) < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits project must accept count 5"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/typescript/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf"

# fp32 signaling-NaN bit-for-bit round-trip (issue #235). A JS number is a 64-bit
# double, and widening an fp32 sNaN into one QUIETS it (0x7F800001 -> 0x7FC00001),
# so generated code that stores the number alone can never re-emit those bits
# (MESSAGE_SPEC S4.6). The fix routes both fp32 positions through corelib-ts's raw
# channel (readFp32Raw / readFp32ArrayRaw, writeFixlen(fp32) / writeFp32ArrayRaw).
# Nothing exercised this before, which is how the defect survived: `recode` is
# wire -> object -> wire with no JSON detour, because JSON renders every NaN as
# null and could not tell a signaling one from a quiet one. Covers a signaling
# (0x7F800001), a quiet/payload (0x7FC00001) and a negative NaN, at the scalar
# position AND at an fp32[] element position.
echo "==> fp32 signaling-NaN bit-exact round-trip (issue #235)"
recode_exact() { # label message octal-wire
    # shellcheck disable=SC2059  # $3 is a controlled octal escape sequence, not user data
    printf "$3" > "$WORK/fp32in.bin"
    (cd "$WORK/conf" && npx tsx harness.ts recode "$2") < "$WORK/fp32in.bin" > "$WORK/fp32out.bin" \
        || { echo "FAIL: $1 must decode"; exit 1; }
    cmp -s "$WORK/fp32in.bin" "$WORK/fp32out.bin" \
        || { echo "FAIL: $1 not bit-exact (an fp32 NaN was quieted): $(od -An -tx1 "$WORK/fp32in.bin" | tr -d ' \n') -> $(od -An -tx1 "$WORK/fp32out.bin" | tr -d ' \n')"; exit 1; }
}
# scalar: 02 (id 0, fixlen) 20 (fixlen word: len 4, fp32 subtype) + 4 LE bytes.
recode_exact "scalar sNaN"  vecf32 '\002\040\001\000\200\177'
recode_exact "scalar qNaN"  vecf32 '\002\040\001\000\300\177'
recode_exact "scalar -NaN"  vecf32 '\002\040\001\000\300\377'
recode_exact "scalar -sNaN" vecf32 '\002\040\001\000\200\377'
# array: 05 (id 0, array-fixlen) 03 (count) 20 (fixlen word) + 3 x 4 LE bytes. The
# wire count IS the array's length (MESSAGE_SPEC S3), so all three come back; the
# middle element is an ordinary 1.0, which must survive beside the NaNs.
recode_exact "array NaNs"   vecf32a '\005\003\040\001\000\200\177\000\000\200\077\001\000\200\377'
# Control: a non-NaN scalar keeps its plain number path (nothing regressed for the
# 99.9% of fp32 values a JS number carries exactly).
recode_exact "scalar 2.5"   vecf32 '\002\040\000\000\040\100'
# Regression guard for the mistake that is easiest to make here: carrying raw wire
# bytes must NOT be read as "the field was present". S2 decides presence from the
# VALUE, so an explicit +0.0 (the field's default) must still normalize away to the
# empty message on re-encode -- as it does on the other 12 drivers.
printf '\002\040\000\000\000\000' > "$WORK/fp32zero.bin"
(cd "$WORK/conf" && npx tsx harness.ts recode vecf32) < "$WORK/fp32zero.bin" > "$WORK/fp32zero.out" \
    || { echo "FAIL: explicit +0.0 must decode"; exit 1; }
[ ! -s "$WORK/fp32zero.out" ] || { echo "FAIL: explicit +0.0 must re-encode as the empty message (S2), got $(od -An -tx1 "$WORK/fp32zero.out" | tr -d ' \n')"; exit 1; }
echo "==> fp32 sNaN round-trip OK"

# int64: long / number — the Long-backed 64-bit hot path must be wire-identical
# to the default bigint representation (issue #51; corelib-ts #19/#20).
echo "==> int64 modes: Long-backed 64-bit path is wire-identical"
cat > "$WORK/i64.yaml" <<'YAML'
version: 1
messages:
  m64:
    payload:
      us: { id: 0, type: array, items: { type: u64, count: 8 } }
      is: { id: 1, type: array, items: { type: i64, count: 8 } }
      ud: { id: 2, type: array, items: { type: u64, count: 2 }, default: [1, "18446744073709551615"] }
      u:  { id: 3, type: u64 }
      i:  { id: 4, type: i64 }
      # A 64-bit pair one level down. Every other 64-bit field in the corpus sits
      # at message level, so without this nothing exercises the representation in
      # a NESTED position — where the field is emitted by the same code but reached
      # through a nested class's own serialize/decodeFrom, and (under the Long
      # modes) through that class's private backing field.
      n:  { id: 5, type: struct, fields: { nu: { id: 0, type: u64 }, ni: { id: 1, type: i64, default: -7 } } }
      # Two NARROW destinations in an otherwise 64-bit message. Under `int64: long`
      # this schema is over the line for corelib-ts's opt-in Long channel
      # (generator#344), where every integer — these two included — arrives as a
      # Long and has to be narrowed back. Their declared-width verdict is the thing
      # that can break there, so the leg below feeds them values that only the
      # HIGH half carries.
      w:  { id: 6, type: u8 }
      sw: { id: 7, type: i8 }
      # A native matrix of 64-bit rows — the _MatSeq collector, whose element
      # converter is shared with the fp32/fp64 hooks and therefore narrows
      # differently from every other arm on the Long channel (generator#344).
      rows: { id: 8, type: array, items: { type: array, count: 3, items: { type: u64, count: 3 } } }
YAML
for mode in bigint long number; do
    cat > "$WORK/cfg_$mode.yaml" <<YAML
generic: { emit: project }
targets: { typescript: { int64: $mode } }
YAML
    gen "$WORK/i64.yaml" "$WORK/i64-$mode" "$WORK/cfg_$mode.yaml"
    ln -s "$WORK/ex/node_modules" "$WORK/i64-$mode/node_modules"
done
( cd "$WORK/i64-long" && npx tsc --noEmit )
( cd "$WORK/i64-number" && npx tsc --noEmit )
enc64() { ( cd "$WORK/i64-$1" && printf '%s' "$2" | npx tsx harness.ts encode m64 ); }
# Full 64-bit range (scalars beyond 2^53): bigint vs long. ud == its schema
# default exercises the longArrEq omission guard.
I64FULL='{"us":["1","18446744073709551615","4294967296"],"is":["-1","-9223372036854775808","9223372036854775807"],"ud":["1","18446744073709551615"],"u":"18446744073709551615","i":"-9223372036854775808","n":{"nu":"18446744073709551615","ni":"-9223372036854775808"},"rows":[["1","18446744073709551615"],["0","4294967296"]]}'
enc64 bigint "$I64FULL" > "$WORK/i64_full_bigint.bin"
enc64 long   "$I64FULL" > "$WORK/i64_full_long.bin"
cmp -s "$WORK/i64_full_bigint.bin" "$WORK/i64_full_long.bin" || { echo "FAIL: int64: long wire drift"; exit 1; }
# Safe-integer scalars (fit 2^53): bigint vs number.
I64SAFE='{"us":["1","18446744073709551615"],"is":["-9223372036854775808"],"ud":["5","6"],"u":"9007199254740991","i":"-9007199254740991","n":{"nu":"42","ni":"-42"},"rows":[["7","8"],["9"]]}'
enc64 bigint "$I64SAFE" > "$WORK/i64_safe_bigint.bin"
enc64 number "$I64SAFE" > "$WORK/i64_safe_number.bin"
cmp -s "$WORK/i64_safe_bigint.bin" "$WORK/i64_safe_number.bin" || { echo "FAIL: int64: number wire drift"; exit 1; }
# ...and long mode on the same safe payload: its scalars are Long-backed too
# (generator#339), so they encode through writeUnsignedLong/writeSignedLong —
# a separate corelib codec from the bigint writers, and it must emit the same
# bytes for a small value as it does for a full-range one.
enc64 long "$I64SAFE" > "$WORK/i64_safe_long.bin"
cmp -s "$WORK/i64_safe_bigint.bin" "$WORK/i64_safe_long.bin" || { echo "FAIL: int64: long wire drift (safe-integer scalars)"; exit 1; }
# Decode parity: long mode reproduces the bigint mode's JSON from the same bytes.
for bin in "$WORK/i64_full_bigint.bin" "$WORK/i64_safe_bigint.bin"; do
    DEC_A=$( cd "$WORK/i64-bigint" && npx tsx harness.ts decode m64 < "$bin" )
    DEC_B=$( cd "$WORK/i64-long"   && npx tsx harness.ts decode m64 < "$bin" )
    [ "$DEC_A" = "$DEC_B" ] || { echo "FAIL: int64: long decode drift ($bin)"; exit 1; }
done
# ...and the DECLARED type must be the type the field actually holds. The cursor
# readers are number-first (a `number` up to 2^53-1, a `bigint` only past it), so
# a bare `as bigint` cast on the pull path produced a `bigint`-typed field holding
# a `number`, and a `bigint[]`-typed array holding a MIX of both — while the
# streaming visitor and fromJSON, reaching the same fields, converted properly.
# Nothing above catches that: every check here goes through the wire or through
# toJSON, and `String(1n) === String(1)`. So assert the runtime type directly, on
# BOTH decode surfaces, or the two paths can disagree again (generator#340).
echo "==> int64: bigint — decoded 64-bit fields must really be bigint"
cat > "$WORK/i64-bigint/typecheck64.ts" <<'TS'
import { M64, M64Decoder } from "./message.js";
import { readFileSync } from "node:fs";
const wire = new Uint8Array(readFileSync(process.argv[2]!));
const bad: string[] = [];
const want = (label: string, v: unknown) => {
  if (typeof v !== "bigint") bad.push(`${label}: ${typeof v}`);
};
// Surface 1: the one-shot pull decoder.
const m = M64.decode(wire);
want("u", m.u);
want("i", m.i);
want("n.nu", m.n.nu);
want("n.ni", m.n.ni);
m.us.forEach((v, k) => want(`us[${k}]`, v));
m.is.forEach((v, k) => want(`is[${k}]`, v));
m.rows.forEach((r, j) => r.forEach((v, k) => want(`rows[${j}][${k}]`, v)));
// Surface 2: the streaming decoder, fed as one chunk. Same bytes, same fields —
// it must land on the same runtime type, not merely the same JSON.
const d = new M64Decoder();
d.feed(wire);
const t = d.finish();
want("stream u", t.u);
want("stream i", t.i);
want("stream n.nu", t.n.nu);
want("stream n.ni", t.n.ni);
t.us.forEach((v, k) => want(`stream us[${k}]`, v));
t.is.forEach((v, k) => want(`stream is[${k}]`, v));
t.rows.forEach((r, j) => r.forEach((v, k) => want(`stream rows[${j}][${k}]`, v)));
if (bad.length) {
  process.stderr.write(`declared bigint, decoded as: ${bad.join(", ")}\n`);
  process.exit(1);
}
TS
# BOTH payloads: the full-range one catches the array elements (its scalars are
# past 2^53 and come back bigint even from a broken build), the safe-integer one
# catches the SCALAR position, which only misbehaves for a value the reader can
# return as a number. Either alone leaves half the defect uncovered.
for bin in "$WORK/i64_full_bigint.bin" "$WORK/i64_safe_bigint.bin"; do
    ( cd "$WORK/i64-bigint" && npx tsx typecheck64.ts "$bin" ) \
        || { echo "FAIL: int64: bigint decoded a non-bigint into a bigint field ($bin)"; exit 1; }
done
# The same assertion for long mode, where the answer is `Long` in EVERY 64-bit
# position — scalars included since generator#339. This is the mode's whole
# point: the cursor's scalar readers are number-first, so without the dedicated
# readUnsignedLong/readSignedLong (corelib-ts#143) a `Long`-declared scalar would
# hold a `number` for any value below 2^53 and a `bigint` above it. toJSON hides
# that completely — `Long.toString()`, `String(1n)` and `String(1)` all print
# "1" — so the type is asserted directly, on both decode surfaces.
echo "==> int64: long — decoded 64-bit fields must really be Long"
cat > "$WORK/i64-long/typecheck64.ts" <<'TS'
import { Long } from "@sofa-buffers/corelib";
import { M64, M64Decoder } from "./message.js";
import { readFileSync } from "node:fs";
const wire = new Uint8Array(readFileSync(process.argv[2]!));
const bad: string[] = [];
const want = (label: string, v: unknown) => {
  if (!(v instanceof Long)) bad.push(`${label}: ${typeof v}`);
};
// Surface 1: the one-shot pull decoder (c.readUnsignedLong / readSignedLong).
const m = M64.decode(wire);
want("u", m.u);
want("i", m.i);
// One level down: same emission, reached through the nested class's own
// serialize/decodeFrom and its own private backing field.
want("n.nu", m.n.nu);
want("n.ni", m.n.ni);
m.us.forEach((v, k) => want(`us[${k}]`, v));
m.is.forEach((v, k) => want(`is[${k}]`, v));
// A native matrix row goes through the _MatSeq collector, the one converter the
// fp hooks share — so it narrows differently on the Long channel (#344).
m.rows.forEach((r, j) => r.forEach((v, k) => want(`rows[${j}][${k}]`, v)));
// Surface 2: the streaming decoder. Its visitor hooks are number-first, so the
// generated arm converts through the field's setter — same field, same runtime
// type, whichever decode API produced it (the invariant of generator#335).
const d = new M64Decoder();
d.feed(wire);
const t = d.finish();
want("stream u", t.u);
want("stream i", t.i);
want("stream n.nu", t.n.nu);
want("stream n.ni", t.n.ni);
t.us.forEach((v, k) => want(`stream us[${k}]`, v));
t.is.forEach((v, k) => want(`stream is[${k}]`, v));
t.rows.forEach((r, j) => r.forEach((v, k) => want(`stream rows[${j}][${k}]`, v)));
// A default-valued field is a Long as well: the declared default is materialised
// at construction, and an absent field decodes back to it.
want("fresh u", new M64().u);
// And the accessor pair takes what its type says it takes, converting once.
const w = new M64();
w.u = 7;
want("set number", w.u);
w.u = 7n;
want("set bigint", w.u);
w.u = Long.fromValue(7n);
want("set Long", w.u);
if (String(w.u.toBigInt()) !== "7") bad.push(`setter value: ${w.u.toBigInt()}`);
if (bad.length) {
  process.stderr.write(`declared Long, decoded as: ${bad.join(", ")}\n`);
  process.exit(1);
}
TS
# BOTH payloads, for the reason spelled out above: the safe-integer one is the
# one a number-first reader answers with a `number`, so it is what actually
# proves the scalar position — the full-range one would come back a `bigint`
# from a broken build and still not be a Long.
for bin in "$WORK/i64_full_bigint.bin" "$WORK/i64_safe_bigint.bin"; do
    ( cd "$WORK/i64-long" && npx tsx typecheck64.ts "$bin" ) \
        || { echo "FAIL: int64: long decoded a non-Long into a Long field ($bin)"; exit 1; }
done
# The Long channel's one real risk (generator#344, corelib-ts#146). With
# `Visitor.longs` the corelib delivers EVERY integer as a Long — the flag is read
# once from the root and is not per field — so a narrow destination narrows back
# in generated code. Take the low half alone and a value of 2^32 lands in a u8 as
# ZERO: not merely a wrong number, but an INVALID message (§7.1) silently
# accepted. The emitted _u/_i helpers therefore fall back to the full value
# whenever the high half is not the low one's sign extension, and this is what
# proves it — on BOTH decode surfaces, at six chunk sizes, through the same
# differential harness the other reject legs use.
echo "==> int64: long — the Long channel must not mask an over-width value"
mk_stream_check "$WORK/i64-long" \
    'import { M64, M64Decoder } from "./message.js";' \
    'checkReject("u8 <- 2^32 (high half only)", new Uint8Array([0x30, 0x80, 0x80, 0x80, 0x80, 0x10]), M64.decode, () => new M64Decoder());'
( cd "$WORK/i64-long" && npx tsx stream_check.ts )
mk_stream_check "$WORK/i64-long" \
    'import { M64, M64Decoder } from "./message.js";' \
    'checkReject("u8 <- 256", new Uint8Array([0x30, 0x80, 0x02]), M64.decode, () => new M64Decoder());'
( cd "$WORK/i64-long" && npx tsx stream_check.ts )
mk_stream_check "$WORK/i64-long" \
    'import { M64, M64Decoder } from "./message.js";' \
    'checkReject("i8 <- -2^32 (sign extension only)", new Uint8Array([0x39, 0xff, 0xff, 0xff, 0xff, 0x1f]), M64.decode, () => new M64Decoder());'
( cd "$WORK/i64-long" && npx tsx stream_check.ts )
# ...and the in-range control still decodes to the exact value, so the reject is
# a bound and not a blanket.
printf '\060\377\001' > "$WORK/w_long_u8_255.bin"
OUT=$( (cd "$WORK/i64-long" && npx tsx harness.ts decode m64) < "$WORK/w_long_u8_255.bin" ) \
    || { echo "FAIL: in-range control 255 must decode on the Long channel"; exit 1; }
echo "$OUT" | tr -d " " | grep -q '"w":255' || { echo "FAIL: control must keep 255 exactly; got: $OUT"; exit 1; }
echo "==> Long-channel narrowing OK (over-width rejected, in-range exact)"

echo "==> int64 modes OK (bigint == long == number on the wire)"

echo "==> corpus + realworld: every definition typechecks"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    # nested_rows.yaml (array<array<string|blob|struct>>, and the same one level
    # deeper) was skipped here while the backend handed the row CONTAINER to the
    # leaf collector, so a string[][] reached a string[] parameter. That is fixed
    # (the row collector is typed with the ROW's type), so the definition is back
    # in the loop and this leg is green without omissions.
    gen "$def" "$WORK/corpus/$name"
    ln -s "$WORK/ex/node_modules" "$WORK/corpus/$name/node_modules"
    ( cd "$WORK/corpus/$name" && npx tsc --noEmit )
done
echo "==> corpus typechecks ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

# ...and the same definitions again under `int64: long`, for every one that has a
# 64-bit field. The loop above generates in the DEFAULT mode, so nothing here used
# to typecheck the Long-backed shapes in a nested position — a struct or union
# member, a wrapper row — even though the mode changes every 64-bit position in
# the tree (#339 made that the scalars too). The mode touches nothing else, so
# definitions without a u64/i64 are skipped rather than compiled twice.
echo "==> corpus: 64-bit definitions typecheck under int64: long"
cat > "$WORK/cfg_corpus_long.yaml" <<'YAML'
generic: { emit: project }
targets: { typescript: { int64: long } }
YAML
n64=0
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    grep -Eq '\b(u64|i64)\b' "$def" || continue
    name=$(basename "$def" .yaml)
    gen "$def" "$WORK/corpus-long/$name" "$WORK/cfg_corpus_long.yaml"
    ln -s "$WORK/ex/node_modules" "$WORK/corpus-long/$name/node_modules"
    ( cd "$WORK/corpus-long/$name" && npx tsc --noEmit )
    n64=$((n64 + 1))
done
echo "==> int64: long corpus typechecks ($n64 definitions with a 64-bit field)"

# Nested WRAPPER rows round-trip, not only typecheck. Typechecking alone would
# accept a collector that compiles but drops rows, so the shape that used to fail
# tsc is exercised end to end: a string row, a blob row, a struct row and a
# depth-3 row of rows, each carrying an INTERIOR element equal to the element
# default ("" / empty blob / zero-valued Point) that §2 omits on the wire and the
# id-keyed placement has to restore.
echo "==> nested wrapper rows round-trip"
NR='{"strrows":[["a","bb","ccc"],["","","zz"]],"blobrows":[[[1,2],[3]],[[],[9,9,9,9]]],"structrows":[[{"x":1,"y":2},{"x":0,"y":0}],[{"x":-7,"y":8},{"x":3,"y":4}]],"strcube":[[["p","q"],["","r"]],[["s",""],["t","u"]]],"numrows":[[1,2,3],[4,5,6]],"fprows":[[1.5,2.5],[0,3.25]]}'
NROUT=$(cd "$WORK/corpus/nested_rows" && printf '%s' "$NR" | npx tsx harness.ts encode NestedRows | npx tsx harness.ts decode NestedRows)
[ "$NROUT" = "$NR" ] || { echo "FAIL: nested wrapper row round-trip drift"; echo "  in : $NR"; echo "  out: $NROUT"; exit 1; }
echo "==> nested wrapper rows OK"

# The two decoders must not drift. decode() runs the monomorphic Cursor over a
# contiguous buffer; decoder()/feed() drives a visitor over the resumable
# IStream. Two decoders per type means every S7 verdict exists twice, so this
# feeds the SAME bytes through both -- at six chunk sizes, one byte at a time
# included -- and requires deeply equal values. Run over the shared example (every
# field shape) and over nested_rows (the wrapper-row collectors, depth 3).
echo "==> streaming: decode() and feed() must agree"
mk_stream_check "$WORK/ex" \
    'import { Myfirstmessage, MyfirstmessageDecoder } from "./message.js";' \
    'const _m = Myfirstmessage.fromJSON(JSON.parse(process.argv[2])); check("example", _m, Myfirstmessage.decode, () => new MyfirstmessageDecoder());'
( cd "$WORK/ex" && npx tsx stream_check.ts "$IN" )

mk_stream_check "$WORK/corpus/nested_rows" \
    'import { NestedRows, NestedRowsDecoder } from "./message.js";' \
    'const _m = NestedRows.fromJSON(JSON.parse(process.argv[2])); check("nested_rows", _m, NestedRows.decode, () => new NestedRowsDecoder());'
( cd "$WORK/corpus/nested_rows" && npx tsx stream_check.ts "$NR" )

# ...and a NESTED row's element width is a validity bound too (S7.1), on both
# paths. The differential above only carries in-range values, which is how
# generator#352 stayed invisible: the pull path passed the bound into
# readUnsignedArray while the push collector stored whatever arrived, so these
# bytes were INVALID through decode() and COMPLETE through feed(), leaving a
# number[][] holding a value one past its declared u32.
#   26              numrows (id 4), sequence start
#   03 01           row 0: id 0, wire ArrayUnsigned, count 1
#   80 80 80 80 10  element 0 = 2^32 -- one past u32
#   07              sequence end
mk_stream_check "$WORK/corpus/nested_rows" \
    'import { NestedRows, NestedRowsDecoder } from "./message.js";' \
    'checkReject("nested row element over u32", new Uint8Array([0x26, 0x03, 0x01, 0x80, 0x80, 0x80, 0x80, 0x10, 0x07]), NestedRows.decode, () => new NestedRowsDecoder());'
( cd "$WORK/corpus/nested_rows" && npx tsx stream_check.ts )

# The two paths must also REJECT alike. Values only cover messages that decode;
# a rejection additionally has an exception TYPE, and the paths reach it through
# different code -- the cursor decodes strings inside the corelib, the visitor
# transcodes in generated code. Only the cursor converted the fatal TextDecoder's
# TypeError, so feed() threw a raw TypeError past any `instanceof SofabError`
# guard (generator#297, Crucible F-0060 / codegen defect G-0037).
#   5a  somestring (id 11) << 3 | 2 (FIXLEN)
#   12  fixlen word: string subtype, length 2
#   ff ff  two bytes that are not valid UTF-8
mk_stream_check "$WORK/ex" \
    'import { Myfirstmessage, MyfirstmessageDecoder } from "./message.js";' \
    'checkReject("invalid utf-8", new Uint8Array([0x5a, 0x12, 0xff, 0xff]), Myfirstmessage.decode, () => new MyfirstmessageDecoder());'
( cd "$WORK/ex" && npx tsx stream_check.ts )

# ...and on the same VERDICT, not just the same exception type. An array header
# whose element kind contradicts the declared field is skipped whole (S7.3), so
# its count is NOT this field's count and must never be measured against this
# field's capacity. The visitor bounded it by id alone, which turned a skippable
# contradiction into INVALID -- visible only when the header arrives without the
# elements behind it, i.e. only when chunked (generator#300, Crucible F-0061 /
# codegen defect G-0038).
#   7c  someuintarray (id 15, count 4) carrying the SIGNED-array wire type
#   7f  count 127, then EOF -> a truncated skip -> INCOMPLETE on both paths
mk_stream_check "$WORK/ex" \
    'import { Myfirstmessage, MyfirstmessageDecoder } from "./message.js";' \
    'checkReject("contradictory array kind", new Uint8Array([0x7c, 0x7f]), Myfirstmessage.decode, () => new MyfirstmessageDecoder());'
( cd "$WORK/ex" && npx tsx stream_check.ts )

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
    if (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/$v.bin" >/dev/null 2>&1; then
        echo "FAIL: $v must be INVALID (S7.1) -- neither masked to the width nor kept"; exit 1
    fi
done
OUT=$( (cd "$WORK/ex" && npx tsx harness.ts decode myfirstmessage) < "$WORK/w_u8_255_ctl.bin" ) || { echo "FAIL: in-range control 255 must decode"; exit 1; }
echo "$OUT" | tr -d ' ' | grep -q '"someu8":255' || { echo "FAIL: control must keep 255 exactly; got: $OUT"; exit 1; }
echo "==> declared-width reject OK"

echo "PASS"
