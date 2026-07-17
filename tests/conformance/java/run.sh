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
generic: { emit: project }
targets: { java: { package: message } }
YAML
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

build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang java --in "$1" --out "$2" )
    ( cd "$2" && mvn -q -Dsofab.version="$VER" package )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
H="java -jar $WORK/ex/target/harness.jar"
OUT=$(printf '%s' "$IN" | $H encode myfirstmessage | $H decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if $H decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
$H decode myfirstmessage < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if $H decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
$H decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Receiver-side decode limits (generator#102): `a` is an UNBOUNDED u64 array
# (id 0 -> header 0x03 = 0<<3 | unsigned-array). With max_dyn_array_count: 4
# a wire count of 5 MUST fail with LIMIT_EXCEEDED (decode exits non-zero,
# checked at the count header before allocation); exactly 4 still decodes; and
# the same 5-element bytes MUST decode against a project built without limits.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/lim.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/limcfg.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
targets: { java: { package: message } }
YAML
build "$WORK/lim.yaml" "$WORK/lim" "$WORK/limcfg.yaml"
build "$WORK/lim.yaml" "$WORK/nolim"
HL="java -jar $WORK/lim/target/harness.jar"
HN="java -jar $WORK/nolim/target/harness.jar"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if $HL decode dyn < "$WORK/overlimit.bin" >/dev/null 2>"$WORK/limerr.txt"; then
    echo "FAIL: dyn array count 5 above max_dyn_array_count 4 must be rejected"; exit 1
fi
grep -q "LIMIT_EXCEEDED" "$WORK/limerr.txt" || { echo "FAIL: rejection must carry LIMIT_EXCEEDED"; exit 1; }
$HL decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: count 4 at the limit must decode"; exit 1; }
$HN decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits project must decode 5 elements"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/java/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/target/harness.jar"

echo "==> §7 decode status through the generated API (generator#105)"
HC="java -jar $WORK/conf/target/harness.jar"
ST=$(printf '\200' | $HC trydecode vecu | head -n1)   # lone 0x80: dangling varint
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: lone 0x80 -> $ST (want INCOMPLETE)"; exit 1; }
ST=$(printf '' | $HC trydecode vecu | head -n1)       # empty message: valid
[ "$ST" = "COMPLETE" ] || { echo "FAIL: empty message -> $ST (want COMPLETE)"; exit 1; }
echo "==> tryDecode status OK (0x80 INCOMPLETE, empty COMPLETE)"

echo "==> corpus + realworld: every definition compiles (javac vs corelib jar)"
JAR="$HOME/.m2/repository/org/sofabuffers/corelib/$VER/corelib-$VER.jar"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang java --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    mkdir -p "$WORK/corpus/$name/out"
    javac -cp "$JAR" -d "$WORK/corpus/$name/out" "$WORK"/corpus/"$name"/src/main/java/message/*.java \
        || { echo "FAIL: corpus def $name did not compile"; exit 1; }
done
echo "==> corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
