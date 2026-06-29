#!/usr/bin/env sh
# Reproducible C# conformance harness: generate -> dotnet build (vs corelib-cs)
# -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/csharp/run.sh [corelib-cs]   (or set $SOFAB_CS_CORELIB)
# Requires: go, dotnet, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_CS_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1

if [ -z "$CORELIB" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-cs.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
export SOFAB_CS_CORELIB="$CORELIB"
echo "==> corelib-cs: $CORELIB"

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project, timestamp: false }
targets: { csharp: { namespace: Sofabuffers } }
YAML
cat > "$WORK/conf.yaml" <<'YAML'
version: 1
messages:
  vecu: { payload: { a: { id: 0, type: u64 } } }
  veci: { payload: { a: { id: 0, type: i64 } } }
  vecf32: { payload: { a: { id: 0, type: fp32 } } }
  vecf64: { payload: { a: { id: 0, type: fp64 } } }
  vecs: { payload: { a: { id: 0, type: string, maxlen: 4096 } } }
YAML

build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang csharp --in "$1" --out "$2" )
    ( cd "$2" && dotnet build -v q >/dev/null )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"someinteger":-5,"somebool":true,"somestring":"hi","somearray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"test":2.5,"someblob":[10,20,30],"bignum":18446744073709551615,"somestringarray":["a","b","c"]}'
H="dotnet $WORK/ex/bin/Debug/net9.0/harness.dll"
OUT=$(printf '%s' "$IN" | $H encode myfirstmessage | $H decode myfirstmessage)
echo "$OUT" | grep -q '"bignum":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/csharp/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/bin/Debug/net9.0/harness.dll"

echo "==> corpus: every edge-case definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    name=$(basename "$def" .yaml)
    build "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions)"

echo "PASS"
