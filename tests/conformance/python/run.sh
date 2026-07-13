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
