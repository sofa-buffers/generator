#!/usr/bin/env sh
# Generate example sources for one language into a directory, so CI can attach
# them to the run as a downloadable artifact.
#
# Usage: tests/gen-artifacts.sh <lang> <out-dir>
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
LANG_KEY="$1"
OUT="$2"

# The showcase example, the real-world multi-file schema, and the edge-case corpus.
DEFS="$ROOT/examples/example.yaml $ROOT/examples/realworld/vehicle_telemetry.yaml"
for d in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    DEFS="$DEFS $d"
done

for def in $DEFS; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sbufgen --lang "$LANG_KEY" --in "$def" --out "$OUT/$name" )
done
echo "generated $(echo "$DEFS" | wc -w) definitions for '$LANG_KEY' into $OUT"
