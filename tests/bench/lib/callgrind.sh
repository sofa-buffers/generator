#!/usr/bin/env bash
# Callgrind Ir/op measurement — the two methods, ported from the corelibs' own
# bench/run_callgrind.sh. See tests/bench/README.md.
#
# Which method a row uses is `method` in rows.json, and it is decided by one thing:
# whether there is a native symbol to toggle collection on.

# ir_toggle <workload> <work> <argv...>
#
# Native rows (c, cpp, rust, go, zig). Collection starts OFF and is toggled around
# run_<workload>, which performs exactly one op — so process start, JSON parsing and
# the decode input's pre-encode are all excluded by construction. This is the whole
# reason the number means "generated code + corelib" and nothing else.
#
# The wrapper is noinline with external linkage purely so the symbol survives for
# --toggle-collect; the code under test still inlines freely inside it.
#
# Go mangles differently: main.run_<workload>. Pass the symbol via SYM if it is not
# run_<workload>.
ir_toggle() {
    local w="$1" work="$2"; shift 2
    local sym="${SYM:-run_$w}"
    local out="$work/cg.$w.out"

    valgrind --tool=callgrind --collect-atstart=no --toggle-collect="$sym" \
        --callgrind-out-file="$out" "$@" >/dev/null 2>"$work/cg.$w.log" || return 1

    local ir
    ir="$(grep -m1 '^summary:' "$out" 2>/dev/null | awk '{print $2}')"

    # A --toggle-collect that matches NO symbol is not an error: callgrind just
    # collects nothing and reports 0. Silently, that reads as an infinite speedup.
    # Refuse to return it.
    if [ -z "$ir" ] || [ "$ir" = "0" ]; then
        echo "ir_toggle: '$sym' matched no symbol (inlined away? mangled?)" >&2
        return 1
    fi
    echo "$ir"
}

# ir_subtract <workload> <work> <r1> <r2> <argv-prefix...>
#
# JIT/interpreted rows (java, python, ts, csharp). No native symbol exists to toggle
# — the hot code is compiled at runtime — so run the workload at two rep counts and
# subtract:
#
#     Ir/op = ( Ir(R2) - Ir(R1) ) / ( R2 - R1 )
#
# which cancels ALL fixed cost exactly: startup, class loading, JIT compilation and
# setup. For the subtraction to be clean the two runs must differ ONLY in the rep
# count, which is what the per-runtime pinning flags in the lang/<x>.sh recipes are
# for (EpsilonGC, -XX:-TieredCompilation, -XX:hashCode=2, node --predictable, ...).
# The reps are appended as the final argv element.
#
# THREE rep points are measured, not two, and the two resulting slopes must agree.
# This is not paranoia — it caught a real lie. With no warmup, V8 tiers up DURING
# the measured loop, so Ir is a step function rather than affine in reps:
#
#     slope(200 -> 1200)  =   214,702
#     slope(1200 -> 2200) = 2,019,691     <- 9.4x apart
#
# Two points cannot tell that apart from a real measurement: they just return
# whichever slope the tier transition happened to land on, and it looks perfectly
# plausible. The generated harnesses run a fixed warmup (independent of reps, so it
# cancels) to make the loop steady-state; this gate verifies that it worked, and
# refuses to report a number when it did not.
ir_subtract() {
    local w="$1" work="$2" r1="$3" r2="$4"; shift 4
    local r3=$(( 2 * r2 - r1 ))   # equal spacing: r1, r2, r3
    local i1 i2 i3 s1 s2

    # Every run needs the SAME payload on stdin, and the first would otherwise
    # consume it — leaving the rest to parse an empty input and "measure" the error
    # path. Spool it once and feed each run from the file.
    local payload="$work/payload.$w"
    cat > "$payload"

    _ir_at() { # <reps> <tag>
        valgrind --tool=callgrind --callgrind-out-file="$work/cg.$w.$2" \
            "${CMD[@]}" "$1" <"$payload" >/dev/null 2>"$work/cg.$w.$2.log" || return 1
        grep -m1 '^summary:' "$work/cg.$w.$2" | awk '{print $2}'
    }
    local CMD=("$@")

    i1="$(_ir_at "$r1" lo)" || return 1
    i2="$(_ir_at "$r2" mid)" || return 1
    i3="$(_ir_at "$r3" hi)" || return 1
    [ -n "$i1" ] && [ -n "$i2" ] && [ -n "$i3" ] || return 1

    s1="$(awk -v a="$i1" -v b="$i2" -v n="$((r2 - r1))" 'BEGIN{ print (b-a)/n }')"
    s2="$(awk -v b="$i2" -v c="$i3" -v n="$((r3 - r2))" 'BEGIN{ print (c-b)/n }')"

    # Affineness. 1% is loose enough for a JIT that has genuinely settled and tight
    # enough to catch a tier transition inside the measured range.
    awk -v s1="$s1" -v s2="$s2" -v w="$w" 'BEGIN{
        if (s2 <= 0) { printf "ir_subtract: %s: non-positive slope (%.0f)\n", w, s2 > "/dev/stderr"; exit 1 }
        d = (s1 > s2 ? s1 - s2 : s2 - s1) / s2 * 100
        if (d > 1.0) {
            printf "ir_subtract: %s: Ir is not affine in reps (slopes %.0f vs %.0f, %.1f%% apart).\n", w, s1, s2, d > "/dev/stderr"
            printf "  The runtime is still tiering up inside the measured loop; raise the warmup or the reps.\n" > "/dev/stderr"
            exit 1
        }
    }' || return 1

    # The widest lever arm has the least intercept contamination.
    awk -v a="$i1" -v c="$i3" -v n="$((r3 - r1))" 'BEGIN{ printf "%d", (c-a)/n }'
}

# Raw Ir is reported as measured; results.txt is stabilized in lib/format.py instead.
#
# An earlier version quantized here (3 s.f.) to absorb the ~0.03% jitter that
# corelib-java/bench/run_callgrind.sh documents on the subtract rows. That does not
# work, and the idempotence check caught it: EVERY deterministic rounding has bucket
# edges, and a raw value sitting on one flips anyway. Observed on two of three
# subtract rows in the same run —
#
#     csharp decode  71100 <-> 71200      (raw ~71,150, bucket edge at 71,150)
#     java   encode  16500 <-> 16600      (raw ~16,550, bucket edge at 16,550)
#
# Rounding cannot fix this because the underlying value is not bit-reproducible on a
# JIT; only a deterministic runtime (CPython, and the toggle rows) is. So the raw
# number is passed through and format.py applies hysteresis against the committed
# value: it keeps the old number while the new one is inside the noise band, and
# only moves on a change big enough to be real. See format.py's `stabilize`.
