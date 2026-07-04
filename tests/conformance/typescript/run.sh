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
generic: { emit: project, timestamp: false }
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

echo "==> corpus + realworld: every definition typechecks"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    gen "$def" "$WORK/corpus/$name"
    ln -s "$WORK/ex/node_modules" "$WORK/corpus/$name/node_modules"
    ( cd "$WORK/corpus/$name" && npx tsc --noEmit )
done
echo "==> corpus typechecks ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
