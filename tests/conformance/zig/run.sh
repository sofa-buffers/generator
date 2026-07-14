#!/usr/bin/env sh
# Reproducible Zig conformance harness: generate -> zig build -> round-trip ->
# byte-exact shared-vector conformance, against corelib-zig (the max-speed
# port: allocation-free streaming encoder, zero-copy contiguous decode).
#
# Usage: tests/conformance/zig/run.sh [corelib-zig]
#   (or set $SOFAB_ZIG_CORELIB)
# Requires: go, zig (0.16+), git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_ZIG_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-zig.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
# build.zig.zon path dependencies must be relative to the build root, so every
# generated project points at a sibling symlink to the corelib checkout.
CORELIB=$(cd "$CORELIB" && pwd)
ln -sfn "$CORELIB" "$WORK/corelib-link"
echo "==> corelib-zig: $CORELIB"

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

printf 'generic: { emit: project }\n' > "$WORK/cfg.yaml"

# zig_build DEF OUT-DIR [CFG] -- generate a project and build its harness. The
# relative depth of the corelib symlink depends on the output nesting, so the
# placeholder is resolved with a computed relative path.
zig_build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "${3:-$WORK/cfg.yaml}" --lang zig --in "$1" --out "$2" )
    rel=$(python3 -c "import os,sys; print(os.path.relpath(sys.argv[1], sys.argv[2]))" "$WORK/corelib-link" "$2")
    sed -i "s#\${SOFAB_ZIG_CORELIB}#$rel#" "$2/build.zig.zon"
    ( cd "$2" && zig build --release=fast )
}

echo "==> generating + building example + conformance projects"
zig_build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
zig_build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(printf '%s' "$IN" | "$WORK/ex/zig-out/bin/harness" encode myfirstmessage | "$WORK/ex/zig-out/bin/harness" decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: blob round-trip"; exit 1; }
echo "$OUT" | grep -q '"somestringarray":\["a","b","c"\]' || { echo "FAIL: string array round-trip"; exit 1; }
echo "$OUT" | grep -q '"somefp32":2.5' || { echo "FAIL: fp32 round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if "$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
"$WORK/ex/zig-out/bin/harness" decode myfirstmessage < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Receiver-side decode limits (generator#102): a count-less u64 array with
# max_dyn_array_count: 4 baked into the generated module (id 0 -> header 0x03 =
# 0<<3 | unsigned-array). A wire count of 5 MUST fail decode with the corelib's
# error.LimitExceeded (exits non-zero); a count of 4 still decodes; and the
# same 5-element bytes MUST decode in a project generated WITHOUT limits.
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
printf 'generic: { emit: project, max_dyn_array_count: 4 }\n' > "$WORK/cfg_lim.yaml"
zig_build "$WORK/dyn.yaml" "$WORK/lim" "$WORK/cfg_lim.yaml"
zig_build "$WORK/dyn.yaml" "$WORK/nolim"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if "$WORK/lim/zig-out/bin/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null 2>&1; then
    echo "FAIL: dynamic array count 5 must exceed max_dyn_array_count 4"; exit 1
fi
"$WORK/lim/zig-out/bin/harness" decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: count == limit (4) must decode"; exit 1; }
"$WORK/nolim/zig-out/bin/harness" decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits project must accept count 5"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/zig/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf"

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    zig_build "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
