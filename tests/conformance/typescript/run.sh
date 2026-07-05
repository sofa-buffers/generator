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

gen() { ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang typescript --in "$1" --out "$2" ); }

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
    gen "$WORK/i64.yaml" "$WORK/i64-$mode"
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
