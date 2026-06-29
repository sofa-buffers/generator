#!/usr/bin/env sh
# Reproducible Python conformance harness: generate -> syntax-check ->
# round-trip -> byte-exact shared-vector conformance against corelib-py.
#
# Usage: tests/python/run.sh [path-to-corelib-py]   (or set $SOFAB_PY_CORELIB)
# Requires: go, python3, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
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
generic: { emit: project, timestamp: false }
targets: { python: { package: messages } }
YAML

echo "==> generating Python project"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in examples/messages/example.yaml --out "$WORK/proj" )

echo "==> syntax check"
python3 -m py_compile "$WORK/proj/messages.py" "$WORK/proj/harness.py"

echo "==> JSON encode -> decode round-trip"
IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"bignum":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/proj" && printf '%s' "$IN" | python3 harness.py encode myfirstmessage | python3 harness.py decode myfirstmessage)
echo "$OUT" | grep -q '"bignum": 18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint": -99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

echo "==> shared-vector byte-exact conformance"
( cd "$ROOT" && SOFAB_PY_CORELIB="$CORELIB" go test ./generators/python/ -run Conformance -count=1 )

echo "==> corpus: every edge-case definition imports"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang python --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    PYTHONPATH="$CORELIB/src:$WORK/corpus/$name" python3 -c "import messages" \
        || { echo "FAIL: corpus def $name did not import"; exit 1; }
done
echo "==> corpus imports ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"

echo "PASS"
