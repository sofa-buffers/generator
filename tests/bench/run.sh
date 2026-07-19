#!/usr/bin/env bash
#
# SofaBuffers generator — footprint & instruction-cost bench.
#
# Regenerates tests/bench/results.txt. The point is the DIFF: change the
# generator, re-run this, and `git diff tests/bench/results.txt` shows what the
# change cost or saved. See tests/bench/README.md for what the numbers mean.
#
# Phase 1 (this script today) covers the footprint metric: .text/.data/.bss of the
# generated code cross-compiled to the embedded targets the footprint profiles
# actually ship to. Ir/op via Callgrind is Phase 2.
#
# Usage:
#   tests/bench/run.sh                       # all rows -> results.txt
#   tests/bench/run.sh --rows c,cpp-c-cpp    # only these rows; other rows keep
#                                            #   their committed values
#   tests/bench/run.sh --check               # exit 1 if results.txt would change
#   tests/bench/run.sh --out /dev/stdout     # print instead of writing
#
# Corelibs are cloned from their default branch (never pinned): a corelib has to
# match the generated code built against it, and pinning would break the bench on
# exactly the commits that adopt a new corelib API. The resolved SHAs go in the
# results.txt header instead — if numbers move and the header didn't, the
# generator did it. Override a clone with e.g. SOFAB_C_CORELIB=/path.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/tests/bench"
OUT="$BENCH/results.txt"
ONLY=""
CHECK=0

while [ $# -gt 0 ]; do
    case "$1" in
        --rows)  ONLY="$2"; shift 2 ;;
        --out)   OUT="$2"; shift 2 ;;
        --check) CHECK=1; shift ;;
        -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Fixed-length work dirs. Path length leaks into .rodata via __FILE__ and Rust
# panic locations; the recipes remap it away, but keeping the length constant is
# the belt to that braces.
mkdir -p "$WORK/a" "$WORK/g"

q() { python3 -c "
import json,sys
d=json.load(open('$BENCH/rows.json'))
$1" ; }

# ---- corelib checkouts ------------------------------------------------------

clone_corelib() { # <repo> -> echoes <dir>; caches per run
    local repo="$1" dir="$WORK/a/$1"
    if [ -d "$dir" ]; then echo "$dir"; return; fi
    git clone --depth 1 -q "https://github.com/sofa-buffers/$repo.git" "$dir" >&2
    echo "$dir"
}

CORELIBS="$WORK/corelibs.tsv"   # repo \t dir — feeds the results.txt header
: > "$CORELIBS"

corelib_for() { # <repo> <env-var> -> echoes <dir>
    local repo="$1" override="${!2:-}" dir
    if [ -n "$override" ]; then dir="$override"; else dir="$(clone_corelib "$repo")"; fi
    printf '%s\t%s\n' "$repo" "$dir" >> "$CORELIBS"
    echo "$dir"
}

# ---- measure ---------------------------------------------------------------

SIZES="$WORK/sizes.tsv"   # row \t arch \t text \t data \t bss
IRS="$WORK/irs.tsv"       # row \t encode_ir \t decode_ir
: > "$SIZES"; : > "$IRS"

# shellcheck source=lib/callgrind.sh
. "$BENCH/lib/callgrind.sh"

generate() { # <lang> <config> <out>
    local lang="$1" config="$2" out="$3" cfg="$WORK/cfg.yaml"
    rm -rf "$out"; mkdir -p "$out"
    printf '%s\n' "$config" > "$cfg"
    ( cd "$ROOT" && go run ./cmd/sofabgen ${config:+--config "$cfg"} \
        --lang "$lang" --in "$(q "print(d['schema'])")" --out "$out" ) >/dev/null
}

# measure_ir <row-id> <lang> <corelib> <method>
#
# Needs `emit: project` (the harness carries the bench verb), so it generates
# separately from the footprint pass, which wants bare sources.
measure_ir() {
    local id="$1" lang="$2" corelib="$3" method="$4"
    declare -f bench_build_ir >/dev/null || return 0   # backend has no bench verb yet

    # Rows explicitly marked `"ir": false` are known-unmeasurable, with the reason
    # recorded in rows.json. Skipping beats burning minutes to have the affineness
    # gate reject them every run.
    [ "$(q "print(next(r.get('ir', True) for r in d['rows'] if r['id']=='$id'))")" = "False" ] && return 0

    local proj="$WORK/g/$id-ir" msg
    msg="$(q "
import re,sys
# the workload suffix is the message name, lowercased — one message in the bench schema
print('vehicletelemetry')")"
    generate "$lang" "generic: { emit: project }
$(q "print(next(r['config'] for r in d['rows'] if r['id']=='$id'))" | grep -v '^generic:' || true)" "$proj"

    if ! bench_build_ir "$proj" "$corelib"; then
        echo "  !! $id ir: build failed" >&2
        printf '%s\t!\t!\n' "$id" >> "$IRS"
        return 0
    fi

    # Per-language runtime pinning. For toggle rows this keeps the single op single
    # (Go's GC/preemption); for subtract rows it is what makes the two runs differ in
    # NOTHING but the rep count, without which the subtraction is not clean.
    local envs=""
    declare -f bench_ir_env >/dev/null && envs="$(bench_ir_env "$proj" "$corelib")"

    local reps=""
    [ "$method" = subtract ] && reps="$(q "print(next(r.get('reps','200 1200') for r in d['rows'] if r['id']=='$id'))")"

    local enc dec w
    for w in encode decode; do
        local v
        if [ "$method" = subtract ]; then
            # shellcheck disable=SC2046,SC2086
            v="$(env $envs bash -c "
                . '$BENCH/lib/callgrind.sh'
                ir_subtract '${w}_$msg' '$WORK' $reps $(bench_cmd_ir "$proj" "${w}_$msg") \
                  < '$BENCH/payload/vehicle_telemetry.json'" 2>/dev/null || true)"
        else
            # Go's symbols are package-mangled (main.run_<w>); most languages are not.
            local sym="run_${w}_$msg"
            declare -f bench_ir_sym >/dev/null && sym="$(bench_ir_sym "${w}_$msg")"
            # shellcheck disable=SC2046,SC2086
            v="$(SYM="$sym" env $envs bash -c "
                . '$BENCH/lib/callgrind.sh'
                ir_toggle '${w}_$msg' '$WORK' $(bench_cmd_ir "$proj" "${w}_$msg") \
                  < '$BENCH/payload/vehicle_telemetry.json'" 2>/dev/null || true)"
        fi
        [ "$w" = encode ] && enc="$v" || dec="$v"
    done

    if [ -z "$enc" ] || [ -z "$dec" ]; then
        echo "  !! $id ir: measurement failed" >&2
        # Surface WHY. The callgrind runs write their stderr to $WORK/cg.*.log, which
        # measure otherwise swallows (2>/dev/null on the subshell) and the EXIT trap
        # then deletes — so a failed row used to be an inscrutable `!`. A Valgrind that
        # cannot decode an instruction (e.g. AVX-512 on an older Valgrind) prints here.
        for lg in "$WORK"/cg.*.log; do
            [ -s "$lg" ] || continue
            echo "     --- $(basename "$lg") (tail) ---" >&2
            tail -n 5 "$lg" | sed 's/^/     /' >&2
        done
        printf '%s\t!\t!\n' "$id" >> "$IRS"
        return 0
    fi
    # Raw, as measured. format.py stabilizes against the committed value — rounding
    # here cannot (see lib/callgrind.sh).
    echo "  ok $id ir: encode=$enc decode=$dec" >&2
    printf '%s\t%s\t%s\n' "$id" "$enc" "$dec" >> "$IRS"
}

measure_row() { # <row-id>
    local id="$1" lang corelib_repo corelib_env config archs method
    lang="$(q "print(next(r['lang'] for r in d['rows'] if r['id']=='$id'))")"
    corelib_repo="$(q "print(next(r['corelib'] for r in d['rows'] if r['id']=='$id'))")"
    corelib_env="$(q "print(next(r['corelib_env'] for r in d['rows'] if r['id']=='$id'))")"
    config="$(q "print(next(r['config'] for r in d['rows'] if r['id']=='$id'))")"
    archs="$(q "print(' '.join(next(r['archs'] for r in d['rows'] if r['id']=='$id')))")"
    method="$(q "print(next(r['method'] for r in d['rows'] if r['id']=='$id'))")"

    # Rows whose recipe does not exist yet are simply not measured (Phase 2/3 work
    # in progress); they keep whatever results.txt already holds for them.
    if [ ! -f "$BENCH/lang/$lang.sh" ]; then
        return 0
    fi

    local corelib gen
    corelib="$(corelib_for "$corelib_repo" "$corelib_env")"

    # Each lang recipe defines bench_size / bench_build_ir / bench_cmd_ir. Undefine
    # first: bash functions are global, so a row whose backend lacks the Ir verb
    # would otherwise inherit the previous row's.
    unset -f bench_size bench_size_rust bench_build_ir bench_cmd_ir 2>/dev/null || true
    # shellcheck source=/dev/null
    . "$BENCH/lang/$lang.sh"

    measure_ir "$id" "$lang" "$corelib" "$method"

    [ -n "$archs" ] || return 0

    gen="$WORK/g/$id"
    generate "$lang" "$config" "$gen"

    local arch out
    for arch in $archs; do
        if [ "$lang" = "rust" ]; then
            out="$(bench_size_rust "$(q "print(d['archs']['$arch']['rust_target'])")" \
                     "$gen" "$corelib" "$WORK" 2>/dev/null || true)"
        else
            # cpp uses cxx/cxx_flags (which drop -ffreestanding — see rows.json).
            local tool_key="cc" flag_key="flags"
            [ "$lang" = "cpp" ] && { tool_key="cxx"; flag_key="cxx_flags"; }
            out="$(bench_size \
                     "$(q "print(d['archs']['$arch']['$tool_key'])")" \
                     "$(q "print(d['archs']['$arch']['size'])")" \
                     "$(q "print(d['archs']['$arch']['$flag_key'])")" \
                     "$gen" "$corelib" "$WORK" 2>/dev/null || true)"
        fi
        if [ -z "$out" ]; then
            echo "  !! $id/$arch size FAILED (see $WORK)" >&2
            printf '%s\t%s\t!\t!\t!\n' "$id" "$arch" >> "$SIZES"
        else
            echo "  ok $id/$arch size: $out" >&2
            printf '%s\t%s\t%s\n' "$id" "$arch" "$(echo "$out" | tr ' ' '\t')" >> "$SIZES"
        fi
    done
}

echo ">> measuring (cross-compiling + callgrind; this is slow) ..." >&2
ROW_IDS="$(q "print(' '.join(r['id'] for r in d['rows']))")"
for id in $ROW_IDS; do
    if [ -n "$ONLY" ] && ! echo ",$ONLY," | grep -q ",$id,"; then continue; fi
    measure_row "$id"
done

# ---- render ----------------------------------------------------------------

# Preserve rows we did not measure this run (--rows), so a partial run does not
# blank the rest of the committed file.
python3 "$BENCH/lib/format.py" \
    --rows "$BENCH/rows.json" \
    --sizes "$SIZES" \
    --irs "$IRS" \
    --previous "$BENCH/results.txt" \
    --root "$ROOT" \
    --corelibs "$CORELIBS" \
    > "$WORK/new.txt"

if [ "$CHECK" = "1" ]; then
    if diff -u "$BENCH/results.txt" "$WORK/new.txt" > "$WORK/diff" 2>&1; then
        echo "results.txt is up to date" >&2
        exit 0
    fi
    echo "results.txt is STALE — regenerate with tests/bench/run.sh" >&2
    cat "$WORK/diff" >&2
    exit 1
fi

cp "$WORK/new.txt" "$OUT"
echo ">> wrote $OUT" >&2
