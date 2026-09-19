#!/usr/bin/env sh
# Run a backend's whole Go test package against the real corelib.
#
# Some backend tests need a corelib checkout (and the target toolchain). They
# skip without one so the hermetic job stays corelib- and toolchain-free, and
# the lang-<x> job is where they run. That job used to pick them with a `-run`
# allowlist, so a new gated test ran nowhere until someone also extended the
# regex -- and CI stayed green while it skipped (generator#562's depth bound).
#
# Here the lang job runs the package unfiltered, with the corelib variable set
# and the toolchain present, so nothing has a reason to skip: every `--- SKIP`
# is a failure. A gated test is picked up by the lang job the day it is written.
#
# Usage: run_backend_tests <go package dir> <corelib env var> <corelib path>
#                          [absent-tool]
#   e.g. run_backend_tests generators/golang SOFAB_GO_CORELIB "$CORELIB"
#
# [absent-tool] names a tool the calling suite has already established is NOT
# installed on this box -- today only a canonical formatter, which is optional
# by design (ARCHITECTURE §12 gate 10): a suite must run where ruff or rustfmt
# is not installed, and the backend test that drives the real formatter then has
# nothing to drive. A skip whose reason names that tool is tolerated, with the
# same loud banner check_format prints and the same SOFAB_FORMAT_STRICT=1
# escape hatch -- so CI, which installs the tool and sets the variable, still
# demands that every test runs. EVERY other skip remains a failure, as does any
# skip at all when the argument is empty or absent. Passing it requires
# tests/conformance/lib/check_format.sh to be sourced too (skip_without_tool).
run_backend_tests() {
    _bt_note=""
    _bt_out=$(cd "$ROOT" && env "$2=$3" go test "./$1/" -count=1 -v 2>&1) || {
        echo "$_bt_out"
        echo "FAIL: go test ./$1/ against the corelib"
        exit 1
    }
    _bt_absent=${4:-}
    _bt_skipped=$(echo "$_bt_out" | grep -E '^[[:space:]]*--- SKIP' || true)
    if [ -n "$_bt_skipped" ]; then
        _bt_n=$(printf '%s\n' "$_bt_skipped" | grep -c .)
        # Every skip must be accounted for by the absent tool, and it is only
        # accounted for when its own reason line names it: one unexplained skip
        # among them fails the run, exactly as before.
        _bt_named=0
        if [ -n "$_bt_absent" ]; then
            _bt_named=$(echo "$_bt_out" | grep -E -B1 '^[[:space:]]*--- SKIP' \
                | grep -v -e '--- SKIP' -e '^--$' | grep -c -- "$_bt_absent" || true)
        fi
        if [ -n "$_bt_absent" ] && [ "$_bt_named" -eq "$_bt_n" ]; then
            skip_without_tool "$_bt_absent" "$_bt_n $_bt_absent-gated test(s) in ./$1/"
            _bt_note=", $_bt_n skipped for a missing $_bt_absent"
        else
            echo "$_bt_out" | grep -E -B1 '^[[:space:]]*--- SKIP'
            echo "FAIL: ./$1/ skipped tests in a lang job -- every gated test must run here"
            exit 1
        fi
    fi
    echo "./$1/: $(echo "$_bt_out" | grep -cE '^--- PASS') tests passed against the corelib${_bt_note:-, none skipped}"
}
