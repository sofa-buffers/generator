#!/usr/bin/env sh
# Reproducible Rust conformance harness: generate -> cargo build -> round-trip ->
# byte-exact shared-vector conformance, run against BOTH Rust corelibs:
#   - corelib-rs-no-std (default)      : #![no_std], heap-free, Cargo feature
#     flags to shrink the binary. The generated crate turns every feature OFF and
#     re-enables only the wire types each schema uses, so building the corpus
#     exercises the full no-std feature-subset matrix (varint-only up to all
#     features; 32-bit value type when no u64/i64 is present).
#   - corelib-rs       (corelib: rs)   : std, high-throughput, every wire type
#     always compiled in (no feature flags, no require! guard).
# Both expose the same sofab:: interface and identical wire output.
#
# Usage: tests/conformance/rust/run.sh [corelib-rs-no-std] [corelib-rs]
#   (or set $SOFAB_RS_CORELIB / $SOFAB_RS_STD_CORELIB)
# Requires: go, cargo, git, python3.
set -eu

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
NOSTD="${1:-${SOFAB_RS_CORELIB:-}}"
STD="${2:-${SOFAB_RS_STD_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$NOSTD" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-rs-no-std.git "$WORK/nostd" >/dev/null 2>&1
    NOSTD="$WORK/nostd"
fi
if [ -z "$STD" ]; then
    git clone --depth 1 https://github.com/sofa-buffers/corelib-rs.git "$WORK/std" >/dev/null 2>&1
    STD="$WORK/std"
fi
echo "==> corelib-rs-no-std: $NOSTD"
echo "==> corelib-rs: $STD"

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

IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'

# run_variant LABEL CFGBODY CORELIB_PATH
#   CFGBODY - the targets.rust config block contents (e.g. "" or "corelib: rs").
run_variant() {
    label=$1; cfgbody=$2; corelib=$3
    printf 'generic: { emit: project }\ntargets: { rust: { %s } }\n' "$cfgbody" > "$WORK/cfg-$label.yaml"

    rust_build() {  # def-or-yaml out-dir
        ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang rust --in "$1" --out "$2" )
        sed -i "s#\${SOFAB_RS_CORELIB}#$corelib#" "$2/Cargo.toml"
        ( cd "$2" && cargo build -q )
    }

    echo "==> [$label] generating + building example + conformance crates"
    rust_build "$ROOT/examples/messages/example.yaml" "$WORK/ex-$label"
    rust_build "$WORK/conf.yaml" "$WORK/conf-$label"

    echo "==> [$label] JSON encode -> decode round-trip"
    OUT=$(cd "$WORK/ex-$label" && printf '%s' "$IN" | cargo run -q -- encode myfirstmessage | cargo run -q -- decode myfirstmessage)
    echo "$OUT" | grep -q '"someu64":18446744073709551615' || { echo "FAIL: [$label] u64 round-trip"; exit 1; }
    echo "$OUT" | grep -q '"deepint":-99' || { echo "FAIL: [$label] nested struct round-trip"; exit 1; }
    echo "$OUT" | grep -q '"someblob":\[10,20,30\]' || { echo "FAIL: [$label] blob round-trip"; exit 1; }
    echo "==> [$label] round-trip OK"

    # Over-count scalar array (generator#100): someuintarray declares count: 4
    # (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
    # INVALID per MESSAGE_SPEC 3+7 (try_decode rejects, harness exits non-zero);
    # exactly 4 still decode.
    echo "==> [$label] over-count scalar array must reject (generator#100)"
    printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
    printf '\173\004\001\002\003\004' > "$WORK/control.bin"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overcount.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-count scalar array (5 > count 4) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/control.bin" >/dev/null) || { echo "FAIL: [$label] control (count == 4) must decode"; exit 1; }
    echo "==> [$label] over-count reject OK"

    # Over-index wrapper array (generator#142, #149): the sequence-form analogue of
    # the over-count scalar reject above. somestringarray (id 18) declares count: 5;
    # a well-formed string element at wire index 5 (>= N) is INVALID per MESSAGE_SPEC
    # S5.1/S7 -- the generated visitor sets self.inv (surfaced as Error::InvalidMsg)
    # before the Vec grows (which also bounds an over-index amplification DoS). Both
    # profiles reject: on no_std the over-index guard fires ahead of the heapless
    # capacity drop (issue #126), so the outcome is INVALID, not a silent drop --
    # the fixed-capacity convergence MESSAGE_SPEC S7.1 requires (issue #149 /
    # F-0013). Wire: 96 01 (sequence_begin id 18) 2a (string id 5) 0a 78 (fixlen
    # "x") 07 (sequence_end).
    printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
    printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
    echo "==> [$label] over-index wrapper array must reject (generator#142, #149)"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overindex.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overindex_control.bin" >/dev/null) || { echo "FAIL: [$label] control (index 4 < 5) must decode"; exit 1; }
    echo "==> [$label] over-index reject OK"

    # Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12)
    # declares maxlen: 16; a 17-byte blob exceeds it -> INVALID, never truncated.
    # Wire: 62 (blob id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes;
    # control is 16 bytes. Both profiles reject: the generated maxlen guard sets
    # self.inv on std AND no_std (the no_std guard supersedes the heapless
    # BufferFull path, so the outcome is INVALID, not a capacity error).
    echo "==> [$label] over-maxlen string/blob must reject (Option B, S7.1)"
    printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
    printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overmaxlen.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overmaxlen_control.bin" >/dev/null) || { echo "FAIL: [$label] control (16 == maxlen) must decode"; exit 1; }
    echo "==> [$label] over-maxlen reject OK"

    # Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose
    # header wire type is not the one its declared type maps to -- for fixlen,
    # including the subtype -- is SKIPPED, exactly like an unknown id. someu8
    # (id 0) is declared u8 (unsigned wire type) and keeps its schema default 7.
    # Wire: 01 = id 0 with wire type SIGNED (1), then the zig-zag varint 06 (= 3).
    # Control: 00 09 is the same id with the correct unsigned wire type and must
    # decode to 9.
    echo "==> [$label] contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
    printf '\001\006' > "$WORK/wiremismatch.bin"
    printf '\000\011' > "$WORK/wiremismatch_control.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/wiremismatch.bin") \
        || { echo "FAIL: [$label] mismatched wire type must skip, not fail the decode"; exit 1; }
    echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: [$label] skipped field must keep its default 7; got: $OUT"; exit 1; }
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/wiremismatch_control.bin") \
        || { echo "FAIL: [$label] control (correct wire type) must decode"; exit 1; }
    echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: [$label] control must decode to 9; got: $OUT"; exit 1; }
    echo "==> [$label] wire-type skip OK"

    # Integer ARRAY delivered to a SCALAR-declared id (MESSAGE_SPEC S7.3,
    # generator#183). This is the one wire-type contradiction the generated id
    # dispatch cannot see on its own: corelib-rs streams array elements through the
    # very unsigned()/signed() callbacks a lone scalar uses, so without the
    # array_begin-armed skip counter the element would land in the scalar's arm.
    # someu8 (id 0, declared u8, default 7) receives an UNSIGNED ARRAY, and somei8
    # (id 4, declared i8, default 10) a SIGNED ARRAY -- both must be skipped whole.
    # Wire: 03 = id 0 wire type ARRAY_UNSIGNED (3), 01 = count 1, 05 = element 5.
    #       24 = id 4 wire type ARRAY_SIGNED (4), 01 = count 1, 06 = zig-zag 3.
    # Control: 21 06 is id 4 with the correct SIGNED wire type and must decode to 3,
    # which pins that the counter self-terminates instead of eating later scalars.
    echo "==> [$label] integer array at a scalar id must skip (MESSAGE_SPEC S7.3, generator#183)"
    printf '\003\001\005' > "$WORK/arr_at_scalar_u.bin"
    printf '\044\001\006' > "$WORK/arr_at_scalar_i.bin"
    printf '\041\006' > "$WORK/arr_at_scalar_control.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/arr_at_scalar_u.bin") \
        || { echo "FAIL: [$label] unsigned array at a scalar id must skip, not fail the decode"; exit 1; }
    echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: [$label] scalar receiving an unsigned array must keep its default 7; got: $OUT"; exit 1; }
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/arr_at_scalar_i.bin") \
        || { echo "FAIL: [$label] signed array at a scalar id must skip, not fail the decode"; exit 1; }
    echo "$OUT" | grep -q '"somei8":10' || { echo "FAIL: [$label] scalar receiving a signed array must keep its default 10; got: $OUT"; exit 1; }
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/arr_at_scalar_control.bin") \
        || { echo "FAIL: [$label] control (correct signed wire type) must decode"; exit 1; }
    echo "$OUT" | grep -q '"somei8":3' || { echo "FAIL: [$label] control must decode to 3; got: $OUT"; exit 1; }
    # A legitimate array field is untouched by the skip counter: someuintarray
    # (id 15) still fills from its own ARRAY_UNSIGNED header.
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/control.bin") \
        || { echo "FAIL: [$label] legitimate array must still decode"; exit 1; }
    echo "$OUT" | grep -q '"someuintarray":\[1,2,3,4\]' || { echo "FAIL: [$label] legitimate array must still fill; got: $OUT"; exit 1; }
    echo "==> [$label] array-at-scalar skip OK"

    # fp ARRAY delivered to a SCALAR-declared fp id (MESSAGE_SPEC S7.3,
    # generator#193): the fp analogue of the integer case above. corelib-rs streams
    # a fixlen (fp) array element-by-element through the very fp32()/fp64() callbacks
    # a lone scalar uses, so without the array_begin-armed skip counter the element
    # would land in the scalar's arm. somefp64 (id 9, declared fp64, default
    # 3.141592653589793) receives an fp64 ARRAY and must be skipped whole.
    # Wire: 4d = id 9 wire type ARRAY_FIXLEN (5), 01 = count 1, 41 = fixlen word
    #       (len 8, FP64 subtype), then 2.5 little-endian.
    # Control: 4a 41 + 2.5 is id 9 with the correct scalar FIXLEN wire type and must
    # decode to 2.5, pinning that the counter self-terminates.
    echo "==> [$label] fp array at a scalar id must skip (MESSAGE_SPEC S7.3, generator#193)"
    printf '\115\001\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar.bin"
    printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fp_arr_at_scalar_control.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/fp_arr_at_scalar.bin") \
        || { echo "FAIL: [$label] fp array at a scalar id must skip, not fail the decode"; exit 1; }
    echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: [$label] scalar receiving an fp array must keep its default 3.141592653589793; got: $OUT"; exit 1; }
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/fp_arr_at_scalar_control.bin") \
        || { echo "FAIL: [$label] control (correct scalar fixlen wire type) must decode"; exit 1; }
    echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: [$label] control must decode to 2.5; got: $OUT"; exit 1; }
    echo "==> [$label] fp array-at-scalar skip OK"

    # Repeated field id (MESSAGE_SPEC S7.4, generator#175): last occurrence wins
    # per field id. A re-opened sequence CONTINUES its scope, so a struct merges
    # and the children an earlier opening set whose ids do not recur are retained.
    # somestruct (id 20) is opened twice: the first opening sets nestedstring
    # (id 1) to "x", the second opens only the empty nestedstruct (id 2).
    # nestedstring MUST survive -- decoding the re-opening into a fresh object
    # would reset it to "Nested".
    # Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
    #       a6 01 (seq start id 20) 16 07 (empty seq id 2) 07 (seq end)
    echo "==> [$label] re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
    printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/reopen_struct.bin") \
        || { echo "FAIL: [$label] re-opened struct must decode"; exit 1; }
    echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: [$label] re-opened struct must retain nestedstring \"x\"; got: $OUT"; exit 1; }
    echo "==> [$label] struct scope merge OK"

    # Repeated field id, array wrapper (MESSAGE_SPEC S7.4 + S5): an array wrapper
    # IS the array's value, so unlike a struct it is REPLACED whole by a later
    # occurrence rather than merged. somestringarray (id 18) is opened twice: the
    # first opening sets elements 0="a" and 1="b", the second sets only element
    # 0="c". Element 1 MUST NOT survive as "b" -- merging by index is the bug
    # this pins.
    # Wire: 96 01 (seq start id 18) 02 0a 61 (string id 0 "a") 0a 0a 62 (string id 1 "b")
    #       07 (seq end) 96 01 (seq start id 18) 02 0a 63 (string id 0 "c") 07 (seq end)
    echo "==> [$label] re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
    printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/reopen_array.bin") \
        || { echo "FAIL: [$label] re-opened array wrapper must decode"; exit 1; }
    echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: [$label] re-opened array wrapper must start with the second opening's element 0 == \"c\"; got: $OUT"; exit 1; }
    if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
        echo "FAIL: [$label] re-opened array wrapper must be replaced, not merged (element \"b\" survived); got: $OUT"; exit 1
    fi
    echo "==> [$label] array wrapper replace OK"

    # Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174): for a fixlen field
    # the declared type maps to a wire type PLUS a subtype, so a header that carries
    # the right Fixlen wire type but the WRONG subtype is just as contradictory as a
    # wrong wire type and MUST be SKIPPED like an unknown id. somefp64 (id 9) is
    # declared fp64 and keeps its schema default 3.141592653589793.
    # Wire: 4a (id 9, fixlen) 0a (fixlen word: len 1, STRING subtype) 78 ("x")
    # Control: 4a 41 (fixlen word: len 8, FP64 subtype) + 2.5 little-endian.
    echo "==> [$label] fixlen subtype mismatch must skip (MESSAGE_SPEC S7.3, generator#174)"
    printf '\112\012\170' > "$WORK/fixsubtype.bin"
    printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fixsubtype_control.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/fixsubtype.bin") \
        || { echo "FAIL: [$label] mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
    echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: [$label] skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
        || { echo "FAIL: [$label] control (correct fp64 subtype) must decode"; exit 1; }
    echo "$OUT" | grep -q '"somefp64":2.5' || { echo "FAIL: [$label] control must decode to 2.5; got: $OUT"; exit 1; }
    echo "==> [$label] fixlen subtype skip OK"

    # S7.3 x S7.4, array wrapper (generator#174 + generator#175): "An occurrence
    # skipped under S7.3 is not an occurrence for this clause: a correctly typed
    # earlier occurrence survives a mis-typed later one." somestringarray (id 18) is
    # opened correctly with element 0 = "a", then id 18 recurs carrying the UNSIGNED
    # wire type. The mis-typed occurrence is skipped, so the array MUST still hold
    # "a" -- the failure this guards is an EMPTY array, i.e. generated code clearing
    # the wrapper before it checks the wire type.
    # Wire: 96 01 (seq start id 18) 02 0a 61 (string id 0 "a") 07 (seq end)
    #       90 01 (id 18, UNSIGNED) 05
    # Asserted as a prefix: heap profiles render ["a"], fixed-capacity ones pad.
    echo "==> [$label] mis-typed later occurrence must not clear the array (MESSAGE_SPEC S7.4, generator#175)"
    printf '\226\001\002\012\141\007\220\001\005' > "$WORK/skipped_occ_array.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
        || { echo "FAIL: [$label] mis-typed later occurrence must decode, not error"; exit 1; }
    echo "$OUT" | grep -q '"somestringarray":\["a"' || { echo "FAIL: [$label] skipped occurrence must not clear the array (element 0 == \"a\" lost); got: $OUT"; exit 1; }
    echo "==> [$label] skipped occurrence keeps array OK"

    # S7.3 x S7.4, struct: same rule for a struct scope. somestruct (id 20) is opened
    # correctly with nestedstring (id 1) = "x", then id 20 recurs carrying the
    # UNSIGNED wire type. That occurrence is skipped, so nestedstring MUST still
    # be "x" rather than falling back to its default "Nested".
    # Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
    #       a0 01 (id 20, UNSIGNED) 05
    echo "==> [$label] mis-typed later occurrence must not clear the struct (MESSAGE_SPEC S7.4, generator#175)"
    printf '\246\001\012\012\170\007\240\001\005' > "$WORK/skipped_occ_struct.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
        || { echo "FAIL: [$label] mis-typed later occurrence must decode, not error"; exit 1; }
    echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: [$label] skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
    echo "==> [$label] skipped occurrence keeps struct OK"

    echo "==> [$label] shared-vector byte-exact conformance"
    python3 "$ROOT/tests/conformance/rust/check_vectors.py" "$corelib/assets/test_vectors.json" "$WORK/conf-$label"

    echo "==> [$label] corpus + realworld: every definition builds"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
        name=$(basename "$def" .yaml)
        rust_build "$def" "$WORK/corpus-$label/$name"
    done
    echo "==> [$label] corpus builds ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"
}

# corelib-rs (std, the default): always-on, no feature flags.
run_variant rs "corelib: rs" "$STD"

# Receiver-side decode limits (generator#102), std corelib only (the no_std
# profile is statically bounded, the keys are inert there): an unbounded u64
# array (id 0 -> header 0x03 = 0<<3 | unsigned-array) under
# max_dyn_array_count: 4. 5 wire elements MUST fail try_decode with
# LimitExceeded (harness exits non-zero); exactly 4 still decode; and the same
# oversized bytes MUST decode against a no-limits project (unset = unlimited).
echo "==> [rs] receiver-side decode limits (generator#102)"
printf 'version: 1\nmessages:\n  dyn: { payload: { a: { id: 0, type: array, items: { type: u64 } } } }\n' > "$WORK/dyn.yaml"
printf 'generic: { emit: project, max_dyn_array_count: 4 }\ntargets: { rust: { corelib: rs } }\n' > "$WORK/cfg-lim.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-lim.yaml" --lang rust --in "$WORK/dyn.yaml" --out "$WORK/lim" )
sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$WORK/lim/Cargo.toml"
( cd "$WORK/lim" && cargo build -q )
printf '\003\005\001\002\003\004\005' > "$WORK/lim-over.bin"
printf '\003\004\001\002\003\004' > "$WORK/lim-ok.bin"
if (cd "$WORK/lim" && cargo run -q -- decode dyn < "$WORK/lim-over.bin" >/dev/null 2>&1); then
    echo "FAIL: 5 elements > max_dyn_array_count 4 must reject (LimitExceeded)"; exit 1
fi
(cd "$WORK/lim" && cargo run -q -- decode dyn < "$WORK/lim-ok.bin" >/dev/null) || { echo "FAIL: 4 elements == cap must decode"; exit 1; }
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs } }\n' > "$WORK/cfg-nolim.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-nolim.yaml" --lang rust --in "$WORK/dyn.yaml" --out "$WORK/nolim" )
sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$WORK/nolim/Cargo.toml"
( cd "$WORK/nolim" && cargo build -q )
(cd "$WORK/nolim" && cargo run -q -- decode dyn < "$WORK/lim-over.bin" >/dev/null) || { echo "FAIL: no-limits project must decode oversized input"; exit 1; }
echo "==> [rs] decode limits OK"

# corelib-rs-no-std is now the genuinely #![no_std], heap-free profile (heapless
# fixed-capacity fields). The rich example.yaml has an unbounded field (somemap),
# so it needs allow_dynamic: true to keep an alloc fallback for that one field —
# the Rust analog of the c-cpp allow_dynamic variant. The corpus spans the
# feature-subset matrix under the same config.
run_variant no-std "corelib: rs-no-std, allow_dynamic: true" "$NOSTD"

# The point of the no_std profile is a crate that builds as #![no_std] and
# heap-free. A bin cannot be no_std on a hosted target, so prove it on the lib
# target: `cargo build --lib --no-default-features` drops serde/std and compiles
# the pure heapless (+ optional alloc) crate. Exercise BOTH allow_dynamic configs,
# mirroring the c-cpp bounded-vs-allow_dynamic split.
echo "==> no_std lib builds heap-free (--lib --no-default-features), allow_dynamic on AND off"

# (a) allow_dynamic: true — example.yaml keeps an alloc fallback for somemap, so
# the crate pulls `extern crate alloc` yet still compiles as #![no_std] on a lib.
grep -q 'extern crate alloc' "$WORK/ex-no-std/src/lib.rs" || { echo "FAIL: allow_dynamic crate should pull extern crate alloc"; exit 1; }
( cd "$WORK/ex-no-std" && cargo build -q --lib --no-default-features )
echo "==> [allow_dynamic=true] no_std lib (heapless + alloc fallback) builds"

# (b) allow_dynamic: false (default) — a fully bounded schema must lower to pure
# heapless with NO allocator at all (no `extern crate alloc`), and an unbounded
# field must instead be a hard generation error.
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs-no-std } }\n' > "$WORK/cfg-strict.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-strict.yaml" --lang rust --in "$WORK/conf.yaml" --out "$WORK/strict" )
if grep -q 'extern crate alloc' "$WORK/strict/src/lib.rs"; then echo "FAIL: strict (bounded, no allow_dynamic) crate must not pull alloc"; exit 1; fi
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/strict/Cargo.toml"
( cd "$WORK/strict" && cargo build -q --lib --no-default-features )
echo "==> [allow_dynamic=false] strict no_std lib (pure heapless, no alloc) builds"

# an unbounded field without allow_dynamic is rejected, not silently heaped.
printf 'version: 1\nmessages:\n  m: { payload: { s: { id: 0, type: string } } }\n' > "$WORK/unbounded.yaml"
if ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-strict.yaml" --lang rust --in "$WORK/unbounded.yaml" --out "$WORK/unbounded" 2>/dev/null ); then
    echo "FAIL: unbounded field under no_std without allow_dynamic should error"; exit 1
fi
echo "==> [allow_dynamic=false] unbounded field is correctly rejected"

echo "==> no-std feature-subset smoke: a varint-only schema builds with no features"
printf 'version: 1\nmessages:\n  tiny: { payload: { a: { id: 0, type: i32 }, b: { id: 1, type: u16 }, c: { id: 2, type: boolean } } }\n' > "$WORK/tiny.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-no-std.yaml" --lang rust --in "$WORK/tiny.yaml" --out "$WORK/tiny" )
grep -q 'default-features = false' "$WORK/tiny/Cargo.toml" || { echo "FAIL: varint-only schema should need no sofab features"; exit 1; }
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/tiny/Cargo.toml"
( cd "$WORK/tiny" && cargo build -q )
echo "==> minimal no-std footprint build OK"

echo "PASS"
