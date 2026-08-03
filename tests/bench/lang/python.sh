#!/usr/bin/env bash
# Python Ir/op recipe — see tests/bench/README.md.
#
# CPython is a deterministic bytecode interpreter, but there is no native symbol to
# --toggle-collect on (the work happens inside the eval loop), so this is the
# `subtract` method: two rep counts, subtract the totals. Same as
# corelib-py/bench/run_callgrind.sh.
#
# The subtraction is only clean if the two runs differ in NOTHING but the rep count:
#   PYTHONDONTWRITEBYTECODE=1  a .pyc written on the first run and not the second
#                              would land wholly in the delta
#   PYTHONHASHSEED=0           str hashing is seeded per-process by default
#   gc.disable()               in the generated harness (see python/project.go)
#
# Nothing to build; there is no footprint row (CPython ships no bare-metal target).
#
# WHICH ENGINE THIS MEASURES. corelib-py ships two: the pure-Python classes and a
# Cython accelerator (`sofab._speedups`, built by its setup.py). `sofab/__init__.py`
# imports the accelerator when it is importable and silently falls back otherwise.
# PYTHONPATH below points at the corelib's SOURCE tree and nothing builds the
# extension, so today this row measures the pure-Python engine — which is why the
# corelib's "restore the accelerator's hot paths (encode 3.0x, decode 1.5x)" landed
# without moving the row by a single instruction.
#
# That is not pinned here on purpose. Forcing SOFAB_PUREPYTHON=1 would make the row
# permanently blind to the engine that actually ships, and python is a maxspeed row.
# Instead format.py records the engine that ran in the `## toolchain` table
# (`sofab-engine`), so the day the extension does get built the switch shows up in
# the diff instead of silently rebasing every python number. To measure what ships:
# install Cython and `pip install -e <corelib>` (or build_ext --inplace) before
# running this row — the devcontainer has no pip today, so it cannot be done here.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1"
    python3 -m py_compile "$proj/harness.py" >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
bench_cmd_ir() {
    echo "python3 $1/harness.py bench $2"
}

# bench_ir_env <proj> <corelib>
#   corelib-py provides `sofab`; the generated harness does `import message` from
#   its own project dir.
bench_ir_env() {
    echo "PYTHONDONTWRITEBYTECODE=1 PYTHONHASHSEED=0 PYTHONPATH=$2/src:$1"
}
