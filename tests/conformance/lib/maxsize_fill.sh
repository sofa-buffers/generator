#!/usr/bin/env sh
# Shared MAX_SIZE fill check (ARCHITECTURE §9.6).
#
# The worst-case encoded size a target emits as MAX_SIZE is computed from the
# schema by one shared walk (internal/ir/wiresize.go). Two things must hold, and
# they are checked in two different places because neither alone is enough:
#
#   1. every target emits the SAME number, and it matches the walk
#      -> tests/matrix/maxsize_test.go (Go, all targets, no build needed)
#   2. that number is what a real encoder actually produces
#      -> here, per target, against the real corelib
#
# (1) alone would let every target agree on a wrong number. (2) alone would not
# notice a target that emits a needlessly LARGE constant, since an over-sized
# buffer still encodes to the same bytes — but a fixed-buffer target pays for it
# in RAM. Together they pin the number from both sides.
#
# maxsize_fill.yaml carries one field per wire shape, every one bounded;
# maxsize_fill.json fills every one to its declared bound with every varint at
# its widest value. Nothing sits on its default — a field equal to its default is
# omitted from the wire (MESSAGE_SPEC §2) and would make the "full" message
# short.

# The expected byte count. Language-independent by construction: the wire format
# is the same everywhere, so this is a property of maxsize_fill.yaml alone. If
# the schema changes, this number changes with it — in exactly one place.
SOFAB_MAXSIZE_FILL_BYTES=234

# check_maxsize_fill <label> <encode-command...>
#   Runs the encode command with maxsize_fill.json on stdin and requires it to
#   emit exactly SOFAB_MAXSIZE_FILL_BYTES bytes.
check_maxsize_fill() {
    _label=$1
    shift
    _json="$ROOT/tests/conformance/lib/maxsize_fill.json"
    _out=$(mktemp)

    if ! "$@" < "$_json" > "$_out" 2>/dev/null; then
        echo "FAIL: [$_label] max-fill encode failed — MAX_SIZE is too small for a"
        echo "      message the schema permits (the encode buffer is sized from it)"
        rm -f "$_out"
        exit 1
    fi
    _got=$(wc -c < "$_out" | tr -d ' ')
    rm -f "$_out"

    if [ "$_got" != "$SOFAB_MAXSIZE_FILL_BYTES" ]; then
        echo "FAIL: [$_label] a fully filled message encoded to $_got bytes, expected"
        echo "      $SOFAB_MAXSIZE_FILL_BYTES — the worst-case walk and the wire disagree"
        exit 1
    fi
    echo "   [$_label] max-fill encodes to exactly $_got bytes"
}
