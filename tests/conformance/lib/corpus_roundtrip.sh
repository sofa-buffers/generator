# Round-trip every corpus and realworld message through a suite's harness
# (generator#655), sourced by every tests/conformance/<lang>/run.sh. The driver
# is lib/check_corpus_roundtrip.py; this wraps the two steps no suite should
# spell out per definition: dumping the resolved IR the driver reads, and the
# tally the final summary line is printed from.
#
#   corpus_roundtrip LABEL LANG DEF TALLY [driver flags] -- HARNESS...
#       Round-trip every message of DEF through HARNESS. The flags are the
#       driver's: --cwd, --int64-json, --exclude MESSAGE=REASON, --exclude-all.
#   corpus_roundtrip_summary LABEL TALLY
#       Print `<n> definitions, <m> messages round-tripped, <k> excluded`.
#
# Needs $ROOT. A definition the profile cannot hold is recorded with
# `--exclude-all REASON` (or `--exclude MESSAGE=REASON`), never skipped silently.
corpus_roundtrip() {
    _cr_label=$1; _cr_lang=$2; _cr_def=$3; _cr_tally=$4
    shift 4
    _cr_ir=$(mktemp)
    ( cd "$ROOT" && go run ./cmd/sofabgen --dump-ir --lang "$_cr_lang" --in "$_cr_def" ) > "$_cr_ir" \
        || { echo "FAIL: $_cr_label: --dump-ir failed for $_cr_def"; rm -f "$_cr_ir"; exit 1; }
    python3 "$ROOT/tests/conformance/lib/check_corpus_roundtrip.py" "$_cr_label" \
        --ir "$_cr_ir" --def "$(basename "$_cr_def" .yaml)" --tally "$_cr_tally" "$@" \
        || { rm -f "$_cr_ir"; exit 1; }
    rm -f "$_cr_ir"
}

corpus_roundtrip_summary() {
    python3 "$ROOT/tests/conformance/lib/check_corpus_roundtrip.py" --summary "$1" --tally "$2" || exit 1
}
