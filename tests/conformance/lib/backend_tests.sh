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
#   e.g. run_backend_tests generators/golang SOFAB_GO_CORELIB "$CORELIB"
run_backend_tests() {
    _bt_out=$(cd "$ROOT" && env "$2=$3" go test "./$1/" -count=1 -v 2>&1) || {
        echo "$_bt_out"
        echo "FAIL: go test ./$1/ against the corelib"
        exit 1
    }
    _bt_skipped=$(echo "$_bt_out" | grep -E '^[[:space:]]*--- SKIP' || true)
    if [ -n "$_bt_skipped" ]; then
        echo "$_bt_out" | grep -E -B1 '^[[:space:]]*--- SKIP'
        echo "FAIL: ./$1/ skipped tests in a lang job -- every gated test must run here"
        exit 1
    fi
    echo "./$1/: $(echo "$_bt_out" | grep -cE '^--- PASS') tests passed against the corelib, none skipped"
}
