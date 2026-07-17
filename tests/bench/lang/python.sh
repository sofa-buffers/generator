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
