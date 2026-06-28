#!/usr/bin/env sh
# Reproducible C conformance harness (M2/M4): generate -> compile -> round-trip
# the generated code against the real corelib-c-cpp.
#
# Usage:
#   tests/c/run.sh [path-to-corelib-c-cpp]
# If no path is given (or $SOFAB_C_CORELIB is unset), the corelib is cloned into
# a temp dir. Requires: go, gcc, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CORELIB="${1:-${SOFAB_C_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-c-cpp"
    git clone --depth 1 https://github.com/sofa-buffers/corelib-c-cpp.git "$WORK/corelib" >/dev/null 2>&1
    CORELIB="$WORK/corelib"
fi
INC="$CORELIB/src/include"
SRC="$CORELIB/src"
echo "==> corelib: $CORELIB"

echo "==> generating C for examples/example.yaml"
( cd "$ROOT" && go run ./cmd/sbufgen --lang c --in examples/example.yaml --out "$WORK/gen" )

echo "==> compiling generated code + harness against corelib"
gcc -std=c99 -Wall -Wextra \
    -I"$INC" -I"$WORK/gen" \
    "$ROOT/tests/c/example_roundtrip.c" \
    "$WORK"/gen/*.c \
    "$SRC/object.c" "$SRC/ostream.c" "$SRC/istream.c" \
    -o "$WORK/rt"

echo "==> running round-trip"
"$WORK/rt"

echo "==> verifying capability guards fire when a feature is stripped"
if gcc -std=c99 -DSOFAB_DISABLE_SEQUENCE_SUPPORT -I"$INC" -I"$WORK/gen" \
        -c "$WORK"/gen/myfirstmessage.c -o /dev/null 2>/dev/null; then
    echo "FAIL: expected a capability-guard #error with SEQUENCE disabled"
    exit 1
fi
echo "==> guard fired as expected"
echo "PASS"
