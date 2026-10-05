# The bounded profiles (C, cpp `corelib: c-cpp`, rust `corelib: rs-no-std`) refuse a
# schema with an unbounded string, blob or array at generate time (MESSAGE_SPEC
# S7.2: a heap-less profile requires the bound in order to pre-size). Their corpus
# loops cannot build the three deliberately-unbounded corpus definitions, so they
# skip them; this asserts the refusal instead, so a skipped schema is a checked
# rejection and not a hole a silently invented bound could slip through.
#
#   assert_unbounded_rejected LANG CONFIG LABEL
#       Run the generator on no_maxlen, seq_elements_dyn and array_lengths_dyn with
#       --config CONFIG (empty: none). Each must exit non-zero, name the missing bound
#       (`has no maxlen|count`, `add a maxlen|count`) and write no source file.
#       Prints one line per file and the count; fails the suite otherwise.
#
# Needs $ROOT. Sourced by every bounded suite, like backend_tests.sh.
UNBOUNDED_CORPUS="no_maxlen seq_elements_dyn array_lengths_dyn"

assert_unbounded_rejected() {
    _au_lang=$1; _au_cfg=$2; _au_label=$3
    _au_n=0
    for _au_name in $UNBOUNDED_CORPUS; do
        _au_out=$(mktemp -d); _au_log=$(mktemp)
        if [ -n "$_au_cfg" ]; then
            ( cd "$ROOT" && go run ./cmd/sofabgen --config "$_au_cfg" --lang "$_au_lang" \
                --in "$ROOT/tests/matrix/corpus/defs/$_au_name.yaml" --out "$_au_out/gen" ) >"$_au_log" 2>&1 && _au_rc=0 || _au_rc=$?
        else
            ( cd "$ROOT" && go run ./cmd/sofabgen --lang "$_au_lang" \
                --in "$ROOT/tests/matrix/corpus/defs/$_au_name.yaml" --out "$_au_out/gen" ) >"$_au_log" 2>&1 && _au_rc=0 || _au_rc=$?
        fi
        [ "$_au_rc" -ne 0 ] \
            || { echo "FAIL: [$_au_label] $_au_name.yaml has unbounded fields and must be refused at generate time"; exit 1; }
        grep -Eq 'has no (maxlen|count)|add a (maxlen|count)' "$_au_log" \
            || { echo "FAIL: [$_au_label] $_au_name.yaml was refused without naming the missing bound:"; cat "$_au_log"; exit 1; }
        [ -z "$(find "$_au_out" -type f 2>/dev/null)" ] \
            || { echo "FAIL: [$_au_label] $_au_name.yaml was refused but wrote files:"; find "$_au_out" -type f; exit 1; }
        echo "   [$_au_label] $_au_name.yaml refused as unbounded: $(grep -Eo 'field "[^"]+".*' "$_au_log" | head -1)"
        rm -rf "$_au_out" "$_au_log"
        _au_n=$((_au_n + 1))
    done
    [ "$_au_n" -eq 3 ] || { echo "FAIL: [$_au_label] expected 3 refusals, asserted $_au_n"; exit 1; }
    echo "==> [$_au_label] $_au_n unbounded corpus definitions refused at generate time, asserted"
}
