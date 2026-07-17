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
generic: { emit: project }
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
  vecsa: { payload: { a: { id: 0, type: array, items: { type: string, count: 8, maxlen: 16 } } } }
YAML

build() {
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang csharp --in "$1" --out "$2" )
    ( cd "$2" && dotnet build -v q >/dev/null )
}

echo "==> generating + building example + conformance projects"
build "$ROOT/examples/messages/example.yaml" "$WORK/ex"
build "$WORK/conf.yaml" "$WORK/conf"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
H="dotnet $WORK/ex/bin/Debug/net9.0/harness.dll"
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

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if $H decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
$H decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Receiver-side decode limits (generator#102): `a` is a count-less array
# (id 0 -> header 0x03 = 0<<3 | unsigned-array), so a configured
# max_dyn_array_count: 4 makes a wire count of 5 fail decode with
# LimitExceeded (non-zero exit) at the count header; exactly 4 still decode,
# and the same oversized bytes decode fine against a project generated
# without limits (unset = unlimited).
echo "==> receiver-side decode limits (generator#102)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg-limit.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limit.yaml" --lang csharp --in "$WORK/dyn.yaml" --out "$WORK/dynlim" )
( cd "$WORK/dynlim" && dotnet build -v q >/dev/null )
build "$WORK/dyn.yaml" "$WORK/dynfree"
HL="dotnet $WORK/dynlim/bin/Debug/net9.0/harness.dll"
HF="dotnet $WORK/dynfree/bin/Debug/net9.0/harness.dll"
printf '\003\005\001\002\003\004\005' > "$WORK/overlimit.bin"
printf '\003\004\001\002\003\004' > "$WORK/atlimit.bin"
if $HL decode dyn < "$WORK/overlimit.bin" >/dev/null 2>&1; then
    echo "FAIL: 5 elements above max_dyn_array_count 4 must fail decode"; exit 1
fi
$HL decode dyn < "$WORK/atlimit.bin" >/dev/null || { echo "FAIL: 4 elements at the limit must decode"; exit 1; }
$HF decode dyn < "$WORK/overlimit.bin" >/dev/null || { echo "FAIL: no-limits project must decode the oversized message"; exit 1; }
echo "==> decode limits OK"

echo "==> shared-vector byte-exact conformance"
python3 "$ROOT/tests/conformance/csharp/check_vectors.py" "$CORELIB/assets/test_vectors.json" "$WORK/conf/bin/Debug/net9.0/harness.dll"

echo "==> §7 decode status through the generated API (generator#105)"
HC="dotnet $WORK/conf/bin/Debug/net9.0/harness.dll"
ST=$(printf '\200' | $HC trydecode vecu | head -n1)   # lone 0x80: dangling varint
[ "$ST" = "INCOMPLETE" ] || { echo "FAIL: lone 0x80 -> $ST (want INCOMPLETE)"; exit 1; }
ST=$(printf '' | $HC trydecode vecu | head -n1)       # empty message: valid
[ "$ST" = "COMPLETE" ] || { echo "FAIL: empty message -> $ST (want COMPLETE)"; exit 1; }
echo "==> TryDecode status OK (0x80 INCOMPLETE, empty COMPLETE)"

echo "==> corpus + realworld: every definition builds"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    build "$def" "$WORK/corpus/$name"
done
echo "==> corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

echo "PASS"
