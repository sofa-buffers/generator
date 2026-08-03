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
# TWO ENGINES, TWO ROWS. corelib-py ships the pure-Python classes and a Cython
# accelerator (`sofab._speedups`, built by its setup.py); `sofab/__init__.py` imports
# the accelerator when it is importable and silently falls back otherwise. They are
# 7.2x (encode) / 4.8x (decode) apart, so one number cannot stand for both: `python`
# measures the fallback, `python-native` measures what a built wheel ships.
#
# Each row PINS its engine, and that is not optional. The two share one corelib
# checkout (clone_corelib caches per run), so a build_ext done for python-native
# would leave the extension sitting in the tree for the pure row to import — making
# the result depend on row order. `pure` therefore forces SOFAB_PUREPYTHON=1 rather
# than relying on the absence of a .so.
#
# python-native REFUSES to report a number it did not earn: without Cython the build
# fails, and were it to fall through it would report the pure engine's cost under the
# native row's name — the exact confusion these two rows exist to end. The engine that
# actually ran is recorded either way, in the `## toolchain` table as `sofab-engine`.

# bench_build_ir <gen_proj> <corelib>
bench_build_ir() {
    local proj="$1" corelib="$2"

    if [ "${SOFAB_BENCH_ENGINE:-}" = native ]; then
        # In-place: bench_ir_env puts <corelib>/src on PYTHONPATH, which is where
        # --inplace drops the .so, so no install and no venv is involved.
        ( cd "$corelib" && python3 setup.py build_ext --inplace ) >/dev/null 2>&1 || {
            echo "     python-native: building sofab._speedups failed (Cython installed?)" >&2
            return 1
        }
        # setup.py marks the extension optional=True, so a compile it cannot do is
        # NOT a failure exit — verify the import instead of trusting the build.
        PYTHONPATH="$corelib/src" python3 -c \
            "import sofab, sys; sys.exit(0 if sofab.IMPL == 'native' else 1)" 2>/dev/null || {
            echo "     python-native: built, but sofab.IMPL is not 'native'" >&2
            return 1
        }
    fi

    python3 -m py_compile "$proj/harness.py" >/dev/null 2>&1
}

# bench_cmd_ir <gen_proj> <workload>  — reps are appended by ir_subtract
bench_cmd_ir() {
    echo "python3 $1/harness.py bench $2"
}

# bench_ir_env <proj> <corelib>
#   corelib-py provides `sofab`; the generated harness does `import message` from
#   its own project dir. SOFAB_PUREPYTHON pins the fallback engine for the `python`
#   row — see the header: the extension may be sitting in the shared checkout.
bench_ir_env() {
    local pin=""
    [ "${SOFAB_BENCH_ENGINE:-}" = native ] || pin="SOFAB_PUREPYTHON=1 "
    echo "PYTHONDONTWRITEBYTECODE=1 PYTHONHASHSEED=0 ${pin}PYTHONPATH=$2/src:$1"
}
