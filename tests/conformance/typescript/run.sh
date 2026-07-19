#!/usr/bin/env sh
# Reproducible TypeScript conformance harness: build corelib-ts, generate ->
# typecheck -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/typescript/run.sh [path-to-corelib-ts]   (or set $SOFAB_TS_CORELIB)
# Requires: go, node, npm, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_TS_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning + building corelib-ts"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-ts.git "$WORK/corelib" >/dev/null 2>&1
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

# Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174) is deliberately NOT
# covered here. Under S7.3 a fixlen field's type is its wire type PLUS its
# subtype, so 4a 0a 78 (id 9 somefp64, Fixlen wire type but STRING subtype) must
# be skipped and leave the default 3.141592653589793. corelib-ts's Cursor exposes
# no fixlen-subtype accessor, so the generated guard cannot make that check at
# all -- the vector fails on this target by construction. Do not re-add it until
# corelib-ts#58 lands an accessor; the other eight harnesses do assert it.

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
I64FULL='{"us":["1","18446744073709551615","4294967296"],"is":["-1","-9223372036854775808","9223372036854775807"],"ud":["1","18446744073709551615"],"u":"18446744073709551615","i":"-9223372036854775808"}'
enc64 bigint "$I64FULL" > "$WORK/i64_full_bigint.bin"
enc64 long   "$I64FULL" > "$WORK/i64_full_long.bin"
cmp -s "$WORK/i64_full_bigint.bin" "$WORK/i64_full_long.bin" || { echo "FAIL: int64: long wire drift"; exit 1; }
# Safe-integer scalars (fit 2^53): bigint vs number.
I64SAFE='{"us":["1","18446744073709551615"],"is":["-9223372036854775808"],"ud":["5","6"],"u":"9007199254740991","i":"-9007199254740991"}'
enc64 bigint "$I64SAFE" > "$WORK/i64_safe_bigint.bin"
enc64 number "$I64SAFE" > "$WORK/i64_safe_number.bin"
cmp -s "$WORK/i64_safe_bigint.bin" "$WORK/i64_safe_number.bin" || { echo "FAIL: int64: number wire drift"; exit 1; }
# Decode parity: long mode reproduces the bigint mode's JSON from the same bytes.
DEC_A=$( cd "$WORK/i64-bigint" && npx tsx harness.ts decode m64 < "$WORK/i64_full_bigint.bin" )
DEC_B=$( cd "$WORK/i64-long"   && npx tsx harness.ts decode m64 < "$WORK/i64_full_bigint.bin" )
[ "$DEC_A" = "$DEC_B" ] || { echo "FAIL: int64: long decode drift"; exit 1; }
echo "==> int64 modes OK (bigint == long == number on the wire)"

echo "==> corpus + realworld: every definition typechecks"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    gen "$def" "$WORK/corpus/$name"
    ln -s "$WORK/ex/node_modules" "$WORK/corpus/$name/node_modules"
    ( cd "$WORK/corpus/$name" && npx tsc --noEmit )
done
echo "==> corpus typechecks ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
