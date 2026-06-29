#!/usr/bin/env sh
# Generate example sources for one language into a directory, so CI can attach
# them to the run as a downloadable artifact.
#
# Usage: tests/gen-artifacts.sh <lang> <out-dir>
#
# cpp and rust each support two corelibs (selected by the `corelib` config), so
# this also emits the NON-default variant alongside the default one:
#   cpp  -> default `cpp` (corelib-cpp)        + `c-cpp` (corelib-c-cpp wrapper)
#   rust -> default `rs`  (corelib-rs, std)    + `rs-no-std` (corelib-rs-no-std)
# The variant lands next to the default under "<name>-<corelib>/".
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
LANG_KEY="$1"
OUT="$2"

# The showcase example, the real-world multi-file schema, and the edge-case corpus.
DEFS="$ROOT/examples/messages/example.yaml $ROOT/examples/messages/realworld/vehicle_telemetry.yaml"
for d in "$ROOT"/tests/matrix/corpus/defs/*.yaml; do
    DEFS="$DEFS $d"
done

# Alternate corelib to ALSO generate, for the languages that have two.
ALT_CORELIB=""
case "$LANG_KEY" in
    cpp)  ALT_CORELIB="c-cpp" ;;
    rust) ALT_CORELIB="rs-no-std" ;;
esac
ALT_CFG=""
if [ -n "$ALT_CORELIB" ]; then
    ALT_CFG=$(mktemp)
    trap 'rm -f "$ALT_CFG"' EXIT
    printf 'targets: { %s: { corelib: %s } }\n' "$LANG_KEY" "$ALT_CORELIB" > "$ALT_CFG"
fi

count=0
for def in $DEFS; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang "$LANG_KEY" --in "$def" --out "$OUT/$name" )
    count=$((count + 1))
    if [ -n "$ALT_CORELIB" ]; then
        ( cd "$ROOT" && go run ./cmd/sofabgen --config "$ALT_CFG" --lang "$LANG_KEY" --in "$def" --out "$OUT/$name-$ALT_CORELIB" )
    fi
done

if [ -n "$ALT_CORELIB" ]; then
    echo "generated $count definitions for '$LANG_KEY' (default + '$ALT_CORELIB' variant) into $OUT"
else
    echo "generated $count definitions for '$LANG_KEY' into $OUT"
fi
