#!/usr/bin/env sh
# Shared multi-drain encode check (ARCHITECTURE §9.2/§9.6, generator#653).
#
# Every other fixture a suite encodes is smaller than the 512-byte scratch an
# unbounded one-shot `encode()` drains through, so the drain path never ran more
# than once. tests/conformance/lib/check_stream_encode.py encodes a message of
# tens of KB through the one-shot surface and through the harness `streamencode`
# verb at several window sizes; this file only keeps the call sites of the eleven
# suites to one line each.
#
# Needs $ROOT from the sourcing suite.

# stream_encode_schema <out-file> [bounded]
#   The shared schema; `bounded` is the variant with large maxlen/count bounds
#   for the targets that cannot hold an unbounded field.
stream_encode_schema() {
    printf 'version: 1\nmessages:\n' > "$1"
    if [ "${2:-}" = bounded ]; then
        python3 "$ROOT/tests/conformance/lib/check_stream_encode.py" --emit-bounded-schema >> "$1"
    else
        python3 "$ROOT/tests/conformance/lib/check_stream_encode.py" --emit-schema >> "$1"
    fi
}

# check_stream_encode <label> [--int-strings] [--base64-bytes] [--cwd DIR] -- <harness argv...>
check_stream_encode() {
    _label=$1
    shift
    python3 "$ROOT/tests/conformance/lib/check_stream_encode.py" "$_label" "$@" || exit 1
}
