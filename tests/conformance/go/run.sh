#!/usr/bin/env sh
# Reproducible Go conformance harness: generate -> build -> round-trip ->
# byte-exact shared-vector conformance against corelib-go.
#
# Usage: tests/conformance/go/run.sh [path-to-corelib-go]   (or set $SOFAB_GO_CORELIB)
# Requires: go, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_GO_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-go"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-go.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-go: $CORELIB"

cat > "$WORK/cfg.yaml" <<YAML
generic: { emit: project }
targets: { go: { package: message, module_path: example.com/gen, go_version: "1.21" } }
YAML

echo "==> generating Go project"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in examples/messages/example.yaml --out "$WORK/proj" )

echo "==> wiring corelib + building"
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/proj/go.mod"
( cd "$WORK/proj" && GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1 && go build ./... )

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/proj" && printf '%s' "$IN" | GOFLAGS=-mod=mod go run ./harness encode myfirstmessage | GOFLAGS=-mod=mod go run ./harness decode myfirstmessage)
echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1); then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/control.bin" >/dev/null) || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element carrying a wire index >= 5 is a schema-bound
# violation -- MESSAGE_SPEC S5.1/S7 make it INVALID for every target, never
# grown-into (this also bounds an over-index heap-amplification DoS). Wire:
#   96 01  sequence_begin, id 18 ((18<<3)|6, varint)
#   2a     string, id 5  ((5<<3)|2) -- over-index (>= count 5)
#   0a 78  fixlen word (len 1, subtype string) + "x"
#   07     sequence_end
# The control places the same element at id 4 (< 5), which still decodes.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1); then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null) || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (generator Option B / MESSAGE_SPEC S7.1): someblob (id 12)
# declares maxlen: 16. A wire byte length above the schema maxlen is malformed input,
# INVALID for every target, never truncated. Wire:
#   62      blob, field id 12 ((12<<3)|2)
#   8b 01   fixlen word (varint): byte length 17, blob subtype 3 ((17<<3)|3)
#   01 x17  the 17-byte payload
# The control is a 16-byte blob (== maxlen), which still decodes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if (cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1); then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && GOFLAGS=-mod=mod go run ./harness decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null) || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn102.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg-limits.yaml" <<YAML
generic: { emit: project, max_dyn_array_count: 4 }
targets: { go: { package: message, module_path: example.com/gen, go_version: "1.21" } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limits.yaml" --lang go --in "$WORK/dyn102.yaml" --out "$WORK/lim102" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/lim102/go.mod"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$WORK/dyn102.yaml" --out "$WORK/nolim102" )
sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/nolim102/go.mod"
printf '\003\005\001\002\003\004\005' > "$WORK/over102.bin"   # id0 array, count 5 > cap 4
printf '\003\004\001\002\003\004' > "$WORK/in102.bin"         # count 4 == cap
if (cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/over102.bin" >/dev/null 2>&1); then
    echo "FAIL: over-cap dynamic array (count 5 > max_dyn_array_count 4) must fail (ErrLimitExceeded)"; exit 1
fi
(cd "$WORK/lim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/in102.bin" >/dev/null) || { echo "FAIL: in-cap dynamic array must decode"; exit 1; }
(cd "$WORK/nolim102" && GOFLAGS=-mod=mod go run ./harness decode dyn < "$WORK/over102.bin" >/dev/null) || { echo "FAIL: without limits the same bytes must decode"; exit 1; }
echo "==> decode limits OK (over-cap rejected, in-cap + unlimited accepted)"

echo "==> shared-vector byte-exact conformance"
( cd "$ROOT" && SOFAB_GO_CORELIB="$CORELIB" go test ./generators/golang/ -run "Conformance|Wire" -count=1 )

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang go --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    sed -i "s#\${SOFAB_GO_CORELIB}#$CORELIB#" "$WORK/corpus/$name/go.mod"
    ( cd "$WORK/corpus/$name" && GOFLAGS=-mod=mod go build ./... )
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
