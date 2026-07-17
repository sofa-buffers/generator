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

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
echo "==> over-maxlen reject OK"

# Receiver-side decode limits (generator#102): max_dyn_array_count: 4 caps a
# count-less (schema-unbounded) u64 array. Wire header 0x03 = id 0, unsigned
# array; a wire count of 5 MUST fail decode with the corelib limit error,
# exactly 4 still decodes, and the same oversized bytes decode fine against a
# project generated WITHOUT the limit (unset = unlimited).
echo "==> receiver-side decode limits must reject over-cap counts (generator#102)"
cat > "$WORK/limit-def.yaml" <<YAML
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u64 } }
YAML
cat > "$WORK/limit-cfg.yaml" <<YAML
generic: { emit: project, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/limit-cfg.yaml" --lang python --in "$WORK/limit-def.yaml" --out "$WORK/limitproj" )
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in "$WORK/limit-def.yaml" --out "$WORK/nolimitproj" )
printf '\003\005\001\002\003\004\005' > "$WORK/limit-over.bin"
printf '\003\004\001\002\003\004' > "$WORK/limit-ok.bin"
if (cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/limit-over.bin" >/dev/null 2>"$WORK/limit-err.txt"; then
    echo "FAIL: wire count 5 > max_dyn_array_count 4 must fail decode"; exit 1
fi
grep -qi "limit" "$WORK/limit-err.txt" || { echo "FAIL: over-cap decode error should mention the limit"; cat "$WORK/limit-err.txt"; exit 1; }
(cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/limit-ok.bin" >/dev/null || { echo "FAIL: wire count 4 must decode under limit 4"; exit 1; }
(cd "$WORK/nolimitproj" && python3 harness.py decode dyn) < "$WORK/limit-over.bin" >/dev/null || { echo "FAIL: unset limit must keep count 5 decodable"; exit 1; }
echo "==> decode-limit reject OK"

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
