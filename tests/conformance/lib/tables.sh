# The shared side tables of the vector file that need a project of their own:
# `header_limits` / `header_limits_nested` and `invalid_utf8` (generator#651).
#
# Sourced, not executed; $ROOT must be set. One schema carries both drivers'
# messages, so a suite builds ONE extra project for them.
#
#   tables_schema <test_vectors.json> <out.yaml> [--without CAP[,CAP]]
#
# writes <out.yaml> and sets
#
#   TABLES_GENERIC   the `generic:` flow mapping for the suite's config -- `emit:
#                    project` plus the receiver caps the table rows name, or without
#                    them when `--without receiver_caps` (a footprint profile)
#
# `--without` is passed to check_header_limits.py as given, here and on every run
# of the driver, so the schema, the config and the expectations agree.
tables_schema() {
    _tb_vec=$1
    _tb_out=$2
    shift 2
    { echo "version: 1"; echo "messages:"; } > "$_tb_out"
    python3 "$ROOT/tests/conformance/lib/check_header_limits.py" --emit-schema "$_tb_vec" "$@" >> "$_tb_out"
    python3 "$ROOT/tests/conformance/lib/check_invalid_utf8.py" --emit-schema >> "$_tb_out"
    _tb_lim=$(python3 "$ROOT/tests/conformance/lib/check_header_limits.py" --emit-limits "$_tb_vec" "$@")
    TABLES_GENERIC="{ emit: project${_tb_lim:+, $_tb_lim} }"
}
