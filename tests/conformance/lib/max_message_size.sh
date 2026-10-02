#!/usr/bin/env sh
# Shared max_message_size checks (ARCHITECTURE §9.6, generator#637).
#
# For a message with an unbounded field `max_message_size` is an imposed ceiling,
# emitted as MAX_SIZE_LIMIT. It must never size an encode buffer or refuse an
# encode; a bounded schema over an explicit key must fail generation. The
# behaviour is checked by tests/conformance/lib/check_max_message_size.py; this
# file only keeps the call sites of the eleven suites one line each.
#
# Needs $ROOT and $WORK from the sourcing suite.

# mms_schema <out-file> -- the shared unbounded messages (string, blob, array).
mms_schema() {
    printf 'version: 1\nmessages:\n' > "$1"
    python3 "$ROOT/tests/conformance/lib/check_max_message_size.py" --emit-schema >> "$1"
}

# check_max_size_limit <label> <ceiling> <file-or-dir> <grep -E pattern, @@ = ceiling>
#   The generated source must carry the configured ceiling as MAX_SIZE_LIMIT.
#   Patterns are anchored at end of line, so 64 cannot match 640.
check_max_size_limit() {
    _pat=$(printf '%s' "$4" | sed "s/@@/$2/")
    grep -rEq "$_pat" "$3" || {
        echo "FAIL: [$1] the generated source must carry MAX_SIZE_LIMIT = $2 for an"
        echo "      unbounded message: no match for /$_pat/ in $3"
        exit 1
    }
}

# check_max_message_size <label> <ceiling> [--cwd DIR] -- <harness argv...>
check_max_message_size() {
    _label=$1
    _ceiling=$2
    shift 2
    python3 "$ROOT/tests/conformance/lib/check_max_message_size.py" encode "$_label" \
        --ceiling "$_ceiling" "$@" || exit 1
}

# check_max_message_budget <label> <sofabgen --lang> <target-config> [--static]
#   The generate-time half; a --static target (fixed storage) also has to reject
#   an unbounded field.
check_max_message_budget() {
    _label=$1
    _lang=$2
    _body=$3
    shift 3
    python3 "$ROOT/tests/conformance/lib/check_max_message_size.py" budget "$_label" "$_lang" \
        --root "$ROOT" --work "$WORK/mms-budget-$_label" --target-config "$_body" "$@" || exit 1
}
