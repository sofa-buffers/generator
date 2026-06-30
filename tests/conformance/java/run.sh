#!/usr/bin/env sh
# Reproducible Java conformance harness: install corelib, generate -> mvn package
# -> round-trip -> byte-exact shared-vector conformance.
#
# Usage: tests/conformance/java/run.sh [corelib-java]   (or set $SOFAB_JAVA_CORELIB)
# Requires: go, javac/java (JDK 17+), mvn, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_JAVA_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-java.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-java: $CORELIB"
VER=$(grep -m1 '<version>' "$CORELIB/pom.xml" | sed 's/.*<version>\(.*\)<\/version>.*/\1/')
echo "==> installing corelib-java $VER to local repo"
( cd "$CORELIB" && mvn -q -DskipTests install )

cat > "$WORK/cfg.yaml" <<'YAML'
generic: { emit: project, timestamp: false }
targets: { java: { package: messages } }
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
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang java --in "$1" --out "$2" )
    ( cd "$2" && mvn -q -Dsofab.version="$VER" package )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
H="java -jar $WORK/ex/target/harness.jar"
OUT=$(printf '%s' "$IN" | $H encode myfirstmessage | $H decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/java/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/target/harness.jar"

echo "==> corpus + realworld: every definition compiles (javac vs corelib jar)"
JAR="$HOME/.m2/repository/org/sofabuffers/sofab/$VER/sofab-$VER.jar"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang java --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    mkdir -p "$WORK/corpus/$name/out"
    javac -cp "$JAR" -d "$WORK/corpus/$name/out" "$WORK"/corpus/"$name"/src/main/java/messages/*.java \
        || { echo "FAIL: corpus def $name did not compile"; exit 1; }
done
echo "==> corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
