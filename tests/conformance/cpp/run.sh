#!/usr/bin/env sh
# Reproducible C++ conformance harness: generate -> build (g++ C++20) ->
# round-trip -> byte-exact shared-vector conformance, run against BOTH C++
# corelibs:
#   - corelib-cpp    (default)        : pure C++20, header-only.
#   - corelib-c-cpp  (corelib: c-cpp) : C++ wrapper over the C library.
# Both expose the same sofab:: interface; the generated code adapts its decode
# (and project Makefile) to the selected corelib.
#
# Usage: tests/conformance/cpp/run.sh [corelib-cpp] [corelib-c-cpp]   (or set the env vars)
# Requires: go, g++, gcc, make, python3, git.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CPP="${1:-${SOFAB_CPP_DIR:-}}"
CC="${2:-${SOFAB_C_DIR:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CPP" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-cpp.git "$WORK/cpp" >/dev/null 2>&1
    CPP="$WORK/cpp"
fi
if [ -z "$CC" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-c-cpp.git "$WORK/c" >/dev/null 2>&1
    CC="$WORK/c"
fi
echo "==> corelib-cpp: $CPP"
echo "==> corelib-c-cpp: $CC"

# Shared definition for the byte-exact shared-vector conformance check.
cat > "$WORK/conf.yaml" <<'YAML'
version: 1
messages:
  vecu: { payload: { a: { id: 0, type: u64 } } }
  veci: { payload: { a: { id: 0, type: i64 } } }
  vecf32: { payload: { a: { id: 0, type: fp32 } } }
  vecf64: { payload: { a: { id: 0, type: fp64 } } }
  vecs: { payload: { a: { id: 0, type: string, maxlen: 4096 } } }
  vecsa: { payload: { a: { id: 0, type: array, items: { type: string, count: 8, maxlen: 16 } } } }
YAML

# Exercises every field-type family (ints, u64, fp, bool, string, enum, bitfield,
# fixed array, blob, string array, blob array, nested struct, union).
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someblobarray":[[1],[2],[3]],"someu64":18446744073709551615,"somestringarray":["a","b","c","d","e"]}'

# run_variant LABEL CORELIB INCLUDE MAKEVARS...
#   CORELIB  - "" for pure corelib-cpp, "c-cpp" for the corelib-c-cpp wrapper.
#   INCLUDE  - -I flag for the corpus syntax-only compile.
#   MAKEVARS - vars passed to `make` for the generated project.
run_variant() {
    label=$1; corelib=$2; include=$3; shift 3
    echo "==> [$label] generating + building example project"
    if [ -n "$corelib" ]; then
        # corelib-c-cpp defaults to the fixed-capacity (embedded) containers
        # profile; allow_dynamic keeps a std::vector/std::string fallback for the
        # intentionally-unbounded fields in example.yaml (somemap) and the
        # no_maxlen corpus def, so the rich corpus still exercises both paths.
        printf 'generic: { emit: project }\ntargets: { cpp: { namespace: sofabuffers, corelib: %s, allow_dynamic: true } }\n' "$corelib" > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers, corelib: %s, allow_dynamic: true } }\n' "$corelib" > "$WORK/cfg-corpus-$label.yaml"
    else
        printf 'generic: { emit: project }\ntargets: { cpp: { namespace: sofabuffers } }\n' > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers } }\n' > "$WORK/cfg-corpus-$label.yaml"
    fi
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp --in examples/messages/example.yaml --out "$WORK/ex-$label" )
    make -C "$WORK/ex-$label" "$@" >/dev/null

    echo "==> [$label] JSON encode -> decode round-trip"
    OUT=$(printf '%s' "$IN" | "$WORK/ex-$label/harness/harness" encode myfirstmessage | "$WORK/ex-$label/harness/harness" decode myfirstmessage)
    for chk in \
        '"someu64":18446744073709551615' \
        '"somei8":-5' \
        '"someenum":33' \
        '"somebitfield":2' \
        '"someintarray":\[1,2,3,4,5\]' \
        '"someblob":\[10,20,30\]' \
        '"somestringarray":\["a","b","c","d","e"\]' \
        '"someblobarray":\[\[1\],\[2\],\[3\]\]' \
        '"deepint":-99' \
        '"option1":4242'; do
        echo "$OUT" | grep -q "$chk" || { echo "FAIL: [$label] round-trip missing $chk"; echo "  got: $OUT"; exit 1; }
    done
    echo "==> [$label] round-trip OK"

    # Over-count scalar array (generator#100): someuintarray declares count: 4
    # (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
    # INVALID per MESSAGE_SPEC 3+7 (pure cpp: the generated guard calls
    # is.invalidate(); c-cpp: the C runtime rejects the count/capacity
    # mismatch); exactly 4 still decode.
    echo "==> [$label] over-count scalar array must reject (generator#100)"
    printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
    printf '\173\004\001\002\003\004' > "$WORK/control.bin"
    if "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1; then
        echo "FAIL: [$label] over-count scalar array (5 > count 4) must be INVALID"; exit 1
    fi
    "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/control.bin" >/dev/null || { echo "FAIL: [$label] control (count == 4) must decode"; exit 1; }
    echo "==> [$label] over-count reject OK"

    # A string/blob-array element index >= the field's fixed capacity N must not
    # hang the decoder (issue #126): the c-cpp fixed profile's _FixedStrSeq /
    # _FixedBlobSeq used to spin forever growing an InlineVector<T,N> that caps at
    # N. somestringarray (id 18) has N=5; feed SEQUENCE_START id 18 (0x96 0x01)
    # then an element token with index 7 >= 5 (SEQUENCE_START id 7 = 0x3e). The
    # decode must terminate (INCOMPLETE) rather than loop; a wall-clock cap catches
    # the regression on both profiles (the heap profile grows a std::vector, so it
    # already terminated).
    echo "==> [$label] over-capacity seq element must not hang (issue #126)"
    printf '\226\001\076' > "$WORK/dos126.bin"
    # The malformed input decodes to INCOMPLETE (harness exits non-zero); capture
    # the code with `|| rc=$?` so `set -e` doesn't abort on it. Only a timeout
    # (124) — i.e. an actual hang — is the failure this guards against.
    rc=0
    timeout 10 "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/dos126.bin" >/dev/null 2>&1 || rc=$?
    [ "$rc" -eq 124 ] && { echo "FAIL: [$label] decode hung on over-capacity sequence element (issue #126)"; exit 1; }
    echo "==> [$label] no-hang OK"

    # Over-index wrapper array (generator#142, #149): the sequence-form analogue of
    # the over-count scalar reject above. somestringarray (id 18) declares count: 5;
    # a well-formed string element at wire index 5 (>= N) is INVALID per MESSAGE_SPEC
    # S5.1/S7. BOTH profiles reject: the heap _StrSeq and the c-cpp fixed-capacity
    # _FixedStrSeq/_FixedBlobSeq both call is.invalidate() before growing (which also
    # bounds an over-index amplification DoS) -- c-cpp via the callback→decoder abort
    # channel added in corelib-c-cpp#92 (generator#149 / F-0013). Wire: 96 01
    # (sequence_begin id 18) 2a (string id 5) 0a 78 (fixlen "x") 07 (sequence_end).
    printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
    printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
    echo "==> [$label] over-index wrapper array must reject (generator#142, #149)"
    if "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1; then
        echo "FAIL: [$label] over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
    fi
    "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: [$label] control (index 4 < 5) must decode"; exit 1; }
    echo "==> [$label] over-index reject OK"

    if [ -z "$corelib" ]; then
        # Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12)
        # declares maxlen: 16; a 17-byte blob exceeds it -> INVALID, never truncated.
        # Wire: 62 (blob id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes;
        # control is 16 bytes. Pure corelib-cpp only: the c-cpp FixedBytes profile
        # currently clamps to N (corelib-c-cpp#90), so it would accept the truncation.
        echo "==> [$label] over-maxlen string/blob must reject (Option B, S7.1)"
        printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
        printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
        if "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
            echo "FAIL: [$label] over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
        fi
        "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: [$label] control (16 == maxlen) must decode"; exit 1; }
        echo "==> [$label] over-maxlen reject OK"
    fi

    echo "==> [$label] shared-vector byte-exact conformance"
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp --in "$WORK/conf.yaml" --out "$WORK/conf-$label" )
    make -C "$WORK/conf-$label" "$@" >/dev/null
    python3 "$ROOT/tests/conformance/cpp/check_vectors.py" "$CC/assets/test_vectors.json" "$WORK/conf-$label/harness/harness"

    echo "==> [$label] corpus + realworld: every definition compiles"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
        name=$(basename "$def" .yaml)
        ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-corpus-$label.yaml" --lang cpp --in "$def" --out "$WORK/corpus-$label/$name" >/dev/null )
        for h in "$WORK"/corpus-"$label"/"$name"/*.hpp; do
            g++ -std=c++20 -fsyntax-only -x c++ $include "$h" \
                || { echo "FAIL: [$label] corpus def $name did not compile"; exit 1; }
        done
    done
    echo "==> [$label] corpus compiles ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"
}

# Pure C++20 corelib-cpp (default).
run_variant cpp "" "-I$CPP/include" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC"

# C++ wrapper over the C library, corelib-c-cpp (corelib: c-cpp). Only needs
# SOFAB_C_DIR; the generated Makefile compiles + links its C sources.
run_variant c-cpp "c-cpp" "-I$CC/src/include" SOFAB_C_DIR="$CC"

# Receiver-side decode limits (generator#102), pure corelib-cpp only (the c-cpp
# profile is statically schema-bounded). An unbounded array claiming more than
# the configured max_dyn_array_count must fail the decode (LimitExceeded via
# is.exceedLimit()); the same bytes decode fine without a configured limit.
echo "==> [cpp] receiver-side decode limits (generator#102)"
cat > "$WORK/dyn102.yaml" <<'YAML'
version: 1
messages:
  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }
YAML
cat > "$WORK/cfg-limits.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 4 }
targets: { cpp: { namespace: sofabuffers } }
YAML
cat > "$WORK/cfg-nolimits.yaml" <<'YAML'
generic: { emit: project }
targets: { cpp: { namespace: sofabuffers } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-limits.yaml" --lang cpp --in "$WORK/dyn102.yaml" --out "$WORK/lim102" )
make -C "$WORK/lim102" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-nolimits.yaml" --lang cpp --in "$WORK/dyn102.yaml" --out "$WORK/nolim102" )
make -C "$WORK/nolim102" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
printf '\003\005\001\002\003\004\005' > "$WORK/over102.bin"   # id0 array, count 5 > cap 4
printf '\003\004\001\002\003\004' > "$WORK/in102.bin"         # count 4 == cap
if "$WORK/lim102/harness/harness" decode dyn < "$WORK/over102.bin" >/dev/null 2>&1; then
    echo "FAIL: [cpp] over-cap dynamic array (count 5 > max_dyn_array_count 4) must fail"; exit 1
fi
# The in-cap decode must not only succeed but PRESERVE the elements: a schema-
# unbounded native array is a std::vector<T> sized to the wire count, not a
# std::array<T, 0> that silently decodes empty (generator#112). Assert the values
# survive the round-trip, not just that decode returns success.
DEC=$("$WORK/lim102/harness/harness" decode dyn < "$WORK/in102.bin") || { echo "FAIL: [cpp] in-cap dynamic array must decode"; exit 1; }
echo "$DEC" | grep -q '"a":\[1,2,3,4\]' || { echo "FAIL: [cpp] unbounded native array lost its elements (regression generator#112); got: $DEC"; exit 1; }
"$WORK/nolim102/harness/harness" decode dyn < "$WORK/over102.bin" >/dev/null || { echo "FAIL: [cpp] without limits the same bytes must decode"; exit 1; }
echo "==> [cpp] decode limits OK (over-cap rejected, in-cap preserves elements, unlimited accepted)"

# corelib-c-cpp feature-subset configs. The C++ wrapper (sofab/sofab.hpp) gates
# its methods on ARRAY / FP64 / INT64 (SOFAB_CPP_HAVE_*), so generated C++ that
# avoids a disabled feature must still compile against the stripped wrapper. The
# wrapper hard-requires FIXLEN and SEQUENCE (it #errors if either is disabled —
# use the C API for those), so those two are only checked as expected rejections.
# (corelib-cpp is always all-features, so this applies to corelib-c-cpp only.)
# allow_dynamic: these subset schemas include string arrays without an element
# maxlen; the fixed profile keeps a std::vector<std::string> fallback for those
# (bounded strings still become FixedString<N>, exercised via the scalar fields).
cat > "$WORK/cfg-clib.yaml" <<'YAML'
targets: { cpp: { namespace: sofabuffers, corelib: c-cpp, allow_dynamic: true } }
YAML
echo "==> corelib-c-cpp feature-subset configs (generated C++ vs the gated wrapper)"
subset_cpp() {  # label  expect(ok|fail)  "DISABLE flags"  "yaml"
    name=$1; expect=$2; flags=$3; yaml=$4
    printf '%s' "$yaml" > "$WORK/subc_$name.yaml"
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-clib.yaml" --lang cpp --in "$WORK/subc_$name.yaml" --out "$WORK/subc_$name" >/dev/null )
    if g++ -std=c++20 -fsyntax-only -x c++ $flags -I"$CC/src/include" "$WORK"/subc_$name/*.hpp 2>/dev/null; then got=ok; else got=fail; fi
    [ "$got" = "$expect" ] || { echo "FAIL: [$name] expected $expect, got $got ($flags)"; exit 1; }
    echo "   [$name] $got"
}
# Definitions that AVOID the disabled feature must still compile.
subset_cpp noarray ok "-DSOFAB_DISABLE_ARRAY_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32}, s: {id: 1, type: string, maxlen: 16}, st: {id: 2, type: struct, fields: {x: {id: 0, type: i32}}}, sa: {id: 3, type: array, items: {type: string, count: 3}} } } }'
subset_cpp nofp64 ok "-DSOFAB_DISABLE_FP64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32}, f: {id: 1, type: fp32}, s: {id: 2, type: string, maxlen: 16}, arr: {id: 3, type: array, items: {type: u8, count: 4}} } } }'
subset_cpp noint64 ok "-DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u32}, b: {id: 1, type: i32}, f: {id: 2, type: fp32}, s: {id: 3, type: string, maxlen: 16}, st: {id: 4, type: struct, fields: {x: {id: 0, type: i32}}} } } }'
subset_cpp stripped ok "-DSOFAB_DISABLE_ARRAY_SUPPORT -DSOFAB_DISABLE_FP64_SUPPORT -DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u8}, b: {id: 1, type: i16}, c: {id: 2, type: i32}, s: {id: 3, type: string, maxlen: 16}, bl: {id: 4, type: blob, maxlen: 8}, st: {id: 5, type: struct, fields: {x: {id: 0, type: i32}}}, sa: {id: 6, type: array, items: {type: string, count: 3}} } } }'
# Definitions that USE the disabled feature must fail to compile.
subset_cpp use_array fail "-DSOFAB_DISABLE_ARRAY_SUPPORT" \
    'version: 1
messages: { m: { payload: { arr: {id: 0, type: array, items: {type: u8, count: 4}} } } }'
subset_cpp use_fp64 fail "-DSOFAB_DISABLE_FP64_SUPPORT" \
    'version: 1
messages: { m: { payload: { g: {id: 0, type: fp64} } } }'
subset_cpp use_int64 fail "-DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u64} } } }'
# The wrapper itself requires FIXLEN and SEQUENCE: disabling either is rejected.
subset_cpp req_fixlen fail "-DSOFAB_DISABLE_FIXLEN_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32} } } }'
subset_cpp req_sequence fail "-DSOFAB_DISABLE_SEQUENCE_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32} } } }'
echo "==> C++ feature-subset configs OK"

echo "PASS"
