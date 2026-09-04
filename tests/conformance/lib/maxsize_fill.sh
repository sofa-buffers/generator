#!/usr/bin/env sh
# Shared MAX_SIZE fill check (ARCHITECTURE §9.6).
#
# The worst-case encoded size a target emits as MAX_SIZE is computed from the
# schema by one shared walk (internal/ir/wiresize.go). Three things must hold,
# and they are checked in three different places because none alone is enough:
#
#   1. every target emits the SAME number, and it matches the walk
#      -> tests/matrix/maxsize_test.go (Go, all targets, no build needed)
#   2. the number a target really puts in the source it generates for THIS
#      schema is that number
#      -> check_maxsize_constant, per target, over the generated file
#   3. that number is what a real encoder produces — to the byte, not just to
#      the count
#      -> check_maxsize_fill, per target, against the real corelib
#
# (1) alone would let every target agree on a wrong number, and it never builds
# anything. (3) alone would not notice a target that emits a needlessly LARGE
# constant: slack in an over-sized buffer is never written, so an inflated
# MAX_SIZE encodes to exactly the same bytes and passes — while a fixed-buffer
# target pays for it in RAM. (2) closes that direction by reading the constant
# back out of the generated source, against SOFAB_MAXSIZE_FILL_BYTES rather than
# a literal, so a schema change moves the expected number in one place.
#
# maxsize_fill.yaml carries one field per wire shape, every one bounded;
# maxsize_fill.json fills every one to its declared bound with every varint at
# its widest value. Nothing sits on its default — a field equal to its default is
# omitted from the wire (MESSAGE_SPEC §2) and would make the "full" message
# short.

# The expected byte count. Language-independent by construction: the wire format
# is the same everywhere, so this is a property of maxsize_fill.yaml alone. If
# the schema changes, this number changes with it — in exactly one place.
SOFAB_MAXSIZE_FILL_BYTES=269

# check_maxsize_fill <label> <encode-command...>
#   Runs the encode command with maxsize_fill.json on stdin and requires it to
#   emit exactly SOFAB_MAXSIZE_FILL_BYTES bytes — and exactly the bytes frozen
#   in maxsize_fill.hex.
#
#   The count answers the §9.6 question: does the fullest legal message reach
#   its own worst case, and stop there. The bytes answer one the count cannot
#   see. A backend that mis-encodes at the SAME length — swapped field ids, a
#   wrapper sequence framed wrong, element indices off by one — passes on the
#   count alone, and this schema is the only place several of those shapes (an
#   fp64 fixlen array, a blob wrapper array, a struct behind a two-byte header,
#   a wrapper array whose elements are themselves sequences, an all-flags-set
#   bitfield) meet an encoder at all: the shared byte-exact vectors use a
#   different schema and reach none of them.
check_maxsize_fill() {
    _label=$1
    shift
    _json="$ROOT/tests/conformance/lib/maxsize_fill.json"
    _hex="$ROOT/tests/conformance/lib/maxsize_fill.hex"
    _out=$(mktemp)

    if ! "$@" < "$_json" > "$_out" 2>/dev/null; then
        echo "FAIL: [$_label] max-fill encode failed — MAX_SIZE is too small for a"
        echo "      message the schema permits (the encode buffer is sized from it)"
        rm -f "$_out"
        exit 1
    fi
    _got=$(wc -c < "$_out" | tr -d ' ')

    if [ "$_got" != "$SOFAB_MAXSIZE_FILL_BYTES" ]; then
        rm -f "$_out"
        echo "FAIL: [$_label] a fully filled message encoded to $_got bytes, expected"
        echo "      $SOFAB_MAXSIZE_FILL_BYTES — the worst-case walk and the wire disagree"
        exit 1
    fi

    # The comments and the layout in the .hex file are for the reader only.
    _want_hex=$(sed 's/#.*//' "$_hex" | tr -d ' \t\n')
    _got_hex=$(od -An -tx1 -v "$_out" | tr -d ' \n')
    rm -f "$_out"

    if [ "$_got_hex" != "$_want_hex" ]; then
        echo "FAIL: [$_label] the filled message is $SOFAB_MAXSIZE_FILL_BYTES bytes but"
        echo "      not the RIGHT bytes (tests/conformance/lib/maxsize_fill.hex):"
        echo "      want $_want_hex"
        echo "      got  $_got_hex"
        exit 1
    fi
    echo "   [$_label] max-fill encodes to exactly $_got bytes, byte-exact"
}

# check_maxsize_constant <label> <generated-file> <grep-pattern>
#   Requires the source generated for maxsize_fill.yaml to carry the DERIVED
#   constant — leg (2) above. The pattern is a basic regex and every caller
#   anchors it at end of line, so a wider constant sharing the prefix (2690 for
#   269) cannot match. Only the file path and the spelling of the constant are
#   per-language; the reason is stated once, in the header above.
check_maxsize_constant() {
    grep -q "$3" "$2" || {
        echo "FAIL: [$1] the generated source must carry the DERIVED MAX_SIZE"
        echo "      ($SOFAB_MAXSIZE_FILL_BYTES for maxsize_fill.yaml), not a ceiling"
        echo "      and not a stale number: no match for /$3/ in $2"
        exit 1
    }
}
