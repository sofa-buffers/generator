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

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
NOSTD="${1:-${SOFAB_RS_CORELIB:-}}"
STD="${2:-${SOFAB_RS_STD_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$NOSTD" ]; then
    clone_corelib corelib-rs-no-std "$WORK/nostd"
    NOSTD="$WORK/nostd"
fi
if [ -z "$STD" ]; then
    clone_corelib corelib-rs "$WORK/std"
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

    # example.yaml leaves `somemap` deliberately count-less to show the dynamic
    # form. The no_std profile requires a bound in both storage modes, so this leg
    # gives it a capacity — the same thing tests/conformance/{c,cpp}/run.sh do.
    # `count` never reaches the wire, so the round-trip and the shared vectors are
    # unchanged.
    EXAMPLE="$ROOT/examples/messages/example.yaml"
    case "$label" in no-std-*)
        EXAMPLE="$WORK/example-$label.yaml"
        awk '
          /^      somemap:/ { inmap=1 }
          inmap && /^          type: struct$/ { print; print "          count: 8"; inmap=0; next }
          { print }
        ' "$ROOT/examples/messages/example.yaml" > "$EXAMPLE" ;;
    esac

    echo "==> [$label] generating + building example + conformance crates"
    rust_build "$EXAMPLE" "$WORK/ex-$label"
    rust_build "$WORK/conf.yaml" "$WORK/conf-$label"

    # MAX_SIZE fill check (ARCHITECTURE §9.6): MAX_SIZE sizes the encode buffer
    # (a heapless::Vec in the no_std profile), so a fully filled message must fit
    # it AND reach it exactly.
    echo "==> [$label] MAX_SIZE fill check"
    rust_build "$ROOT/tests/conformance/lib/maxsize_fill.yaml" "$WORK/fill-$label"
    ( cd "$WORK/fill-$label" && check_maxsize_fill "$label" cargo run -q -- encode fill )

    # Streaming behaviour (PR #242): the generator tests only assert that the
    # streaming API appears in the output. This runs it, and pins the property
    # that matters -- streaming must be indistinguishable from the one-shot path.
    # The shared check assigns String/Vec directly, which heapless cannot take —
    # the static leg is driven by streaming_check_nostd.rs further down instead.
    if [ "$label" != "no-std-static" ]; then
    echo "==> [$label] streaming: serialize through a sink, feed the decoder in chunks"
    rm -rf "$WORK/stream-$label"
    rust_build "$EXAMPLE" "$WORK/stream-$label"
    case "$label" in
        no-std-*) printf 'use sofabuffers_generated::*;\n' > "$WORK/stream-$label/src/main.rs" ;;
        *)        printf 'mod message;\nuse message::*;\n' > "$WORK/stream-$label/src/main.rs" ;;
    esac
    sed '/^\/\/SOFAB_IMPORT$/d' "$ROOT/tests/conformance/rust/streaming_check.rs" \
        >> "$WORK/stream-$label/src/main.rs"
    case "$label" in
        # The lib is #![no_std] without this; the binary above needs it linked
        # for println!/Vec in the check itself.
        no-std-*) ( cd "$WORK/stream-$label" && cargo run -q --features std ) ;;
        *)        ( cd "$WORK/stream-$label" && cargo run -q ) ;;
    esac

    fi

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

    # Fixlen-array element subtype decides BEFORE the schema count bound
    # (CORELIB_PLAN S4.8, generator#259 / Crucible F-0042). somefloatarray declares
    # fp32 with count: 3 at id 17 -> array-fixlen header 8d 01 (17<<3 | 5). A fixlen
    # array carries a fixlen_word after its count: 20 = 4-byte elements (fp32),
    # 41 = 8-byte elements (fp64).
    #
    #   fp64 header, count 5 at the fp32-declared id: the subtype contradicts the
    #   declared element type, so the field is SKIPPED (MESSAGE_SPEC S7.3) and its
    #   count is not this field's count -- it must NOT be measured against 3. The
    #   payload is 5 x 8 zero bytes, so the message is complete and must DECODE.
    #
    #   fp32 header, count 5 at the same id: the subtype matches, so this really is
    #   the field's count and the schema bound applies -- still INVALID (S3+S7).
    #
    # Before the subtype reached the array header hook the two were indistinguishable
    # and both rejected, which is the defect this pins.
    echo "==> [$label] fixlen-array subtype decides before the count bound (generator#259)"
    printf '\215\001\005\101\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' > "$WORK/fp64_at_fp32.bin"
    printf '\215\001\005\040\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' > "$WORK/fp32_overcount.bin"
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/fp64_at_fp32.bin" >/dev/null) || { echo "FAIL: [$label] fp64 array at an fp32-declared id must be skipped, not bounded"; exit 1; }
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/fp32_overcount.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] fp32 array with count 5 > 3 at its own id must stay INVALID"; exit 1
    fi
    echo "==> [$label] fixlen-array subtype ordering OK"

    # Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
    # MESSAGE_SPEC S5.2). someuintarray declares count 4; a header announcing 6
    # elements (> 4) followed by only 2 elements then EOF is BOTH schema-invalid and
    # truncated. The over-count is decided at the count header (before the cut-off
    # elements), so the message MUST report INVALID (InvalidMsg), not INCOMPLETE.
    # Wire: 7b (id 15 unsigned-array) 06 (count 6) 01 02 (2 of 6 elements) <EOF>.
    printf '\173\006\001\002' > "$WORK/overcount_trunc.bin"
    echo "==> [$label] over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
    ERR=$( (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overcount_trunc.bin" 2>&1 >/dev/null) || true )
    echo "$ERR" | grep -q 'InvalidMsg' || { echo "FAIL: [$label] over-count(6>4)+truncated must be INVALID (InvalidMsg); got: $ERR"; exit 1; }
    # Precision control: an IN-BOUND count (4 == bound) that is genuinely truncated
    # (2 of 4 elements then EOF) is a clean truncation and MUST stay INCOMPLETE — the
    # over-count guard must not turn every short array into INVALID.
    printf '\173\004\001\002' > "$WORK/incount_trunc.bin"
    ERR=$( (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/incount_trunc.bin" 2>&1 >/dev/null) || true )
    echo "$ERR" | grep -q 'Incomplete' || { echo "FAIL: [$label] in-bound(4==4)+truncated must be INCOMPLETE; got: $ERR"; exit 1; }
    echo "==> [$label] over-count/truncation ordering OK"

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

    # A NESTED ROW carries BOTH bounds, and array_begin decides both.
    # somematrix (id 24) is `array of array of u32` with an OUTER count of 2 and an
    # INNER count of 4. The row id is bounded by the outer count (the wrapper case
    # above, one level down); the row's own ELEMENT COUNT is bounded by the inner
    # count, exactly like the top-level over-count reject at the start of this
    # function. Checking only the id left the inner `count: 4` as no decode bound at
    # all: corelib-rs filled the row to whatever count the wire announced, and the
    # no_std profile silently dropped the elements past its heapless capacity and
    # accepted the message anyway -- the cross-profile divergence S7.1 forbids.
    # Wire: c6 01 (sequence_begin id 24) 03 (row id 0, unsigned-array) <count>
    # <elements> 07 (sequence_end).
    echo "==> [$label] over-count nested row must reject (MESSAGE_SPEC S3+S7)"
    printf '\306\001\003\005\001\002\003\004\005\007' > "$WORK/rowovercount.bin"
    printf '\306\001\003\004\001\002\003\004\007' > "$WORK/rowovercount_control.bin"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/rowovercount.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-count matrix row (5 > inner count 4) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/rowovercount_control.bin" >/dev/null) || { echo "FAIL: [$label] control (row of 4 == inner count) must decode"; exit 1; }
    # Row id >= the OUTER count is the other half of the same arm.
    printf '\306\001\023\001\001\007' > "$WORK/rowoverindex.bin"
    printf '\306\001\013\001\001\007' > "$WORK/rowoverindex_control.bin"
    if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/rowoverindex.bin" >/dev/null 2>&1); then
        echo "FAIL: [$label] over-index matrix row (id 2 >= outer count 2) must be INVALID"; exit 1
    fi
    (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/rowoverindex_control.bin" >/dev/null) || { echo "FAIL: [$label] control (row id 1 < 2) must decode"; exit 1; }
    # Decided at the COUNT HEADER, so INVALID dominates a truncated tail (S5.2):
    # a row announcing 6 elements (> 4) with only 2 present then EOF is INVALID.
    printf '\306\001\003\006\001\002' > "$WORK/rowovercount_trunc.bin"
    ERR=$( (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/rowovercount_trunc.bin" 2>&1 >/dev/null) || true )
    echo "$ERR" | grep -q 'InvalidMsg' || { echo "FAIL: [$label] over-count(6>4)+truncated row must be INVALID (InvalidMsg); got: $ERR"; exit 1; }
    echo "==> [$label] nested-row bounds OK"

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


    # Declared integer width is a VALIDITY bound (MESSAGE_SPEC S7.1 + documentation#32,
    # generator#266, Crucible F-0033 / codegen defect G-0026). A value outside the
    # declared width is INVALID: it MUST NOT be masked to the width, and MUST NOT be
    # kept. someu8 is id 0 (header 0x00 = 0<<3 | unsigned), someu16 is id 1 (0x08).
    #   00 ff 7f = 16383 into a u8 -- the reported reproducer
    #   00 80 02 = 256   into a u8 -- one past the width
    #   08 f0 a2 04 = 70000 into a u16
    #   00 ff 01 = 255   into a u8 -- the in-range control: must decode and keep 255
    echo "==> [$label] over-width scalar must be INVALID (S7.1, generator#266)"
    printf '\000\377\177'     > "$WORK/w_u8_16383.bin"
    printf '\000\200\002'     > "$WORK/w_u8_256.bin"
    printf '\010\360\242\004' > "$WORK/w_u16_70000.bin"
    printf '\000\377\001'     > "$WORK/w_u8_255_ctl.bin"
    for v in w_u8_16383 w_u8_256 w_u16_70000; do
        if (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/$v.bin" >/dev/null 2>&1); then
            echo "FAIL: [$label] $v must be INVALID (S7.1) -- neither masked nor kept"; exit 1
        fi
    done
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/w_u8_255_ctl.bin") || { echo "FAIL: [$label] in-range control 255 must decode"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"someu8":255' || { echo "FAIL: [$label] control must keep 255; got: $OUT"; exit 1; }
    echo "==> [$label] declared-width reject OK"


    # §7.3 / §5.2 skip family (generator#268 #270 #271 #272 #273 -- Crucible F-0044
    # F-0045 F-0046 F-0047 F-0048). Each of these is a construct the decoder must
    # DISCARD reaching machinery that belongs to a field it is not. The example
    # schema has none of the required id/type positions, so probe with Crucible's
    # shape.
    cat > "$WORK/skip.yaml" <<'YAML'
version: 1
messages:
  probe:
    payload:
      a: { id: 3, type: i16 }
      arrays:
        id: 100
        type: struct
        fields:
          u8s: { id: 0, type: array, items: { type: u8, count: 5 } }
          i8s: { id: 1, type: array, items: { type: i8, count: 5 } }
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
YAML
    rust_build "$WORK/skip.yaml" "$WORK/skip-$label"
    skip_decode() { (cd "$WORK/skip-$label" && cargo run -q -- decode probe) }

    echo "==> [$label] a skipped subtree must not leak into the enclosing scope"
    # #268: id 24 is absent from the schema, so the whole sequence is skipped --
    # INCLUDING its child id 3, which at ROOT is the declared i16. Binding it would
    # re-encode to the child header itself.
    OUT=$(printf '\306\001\031\326\014\007' | skip_decode) \
        || { echo "FAIL: [$label] an unknown sequence must be skipped, not fail"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"a":0' \
        || { echo "FAIL: [$label] a child of an UNKNOWN sequence must not bind into root (#268); got: $OUT"; exit 1; }
    # #272: the §7.3 twin -- a string_array ELEMENT position opened as a sequence
    # must be skipped whole, so the string inside it is not that element.
    OUT=$(printf '\306\014\306\014\002\022\113\101\007\007' | skip_decode) \
        || { echo "FAIL: [$label] a mistyped sequence element must be skipped, not fail"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"string_array":\[\]' \
        || { echo "FAIL: [$label] a child of a MISTYPED sequence element must not bind as that element (#272); got: $OUT"; exit 1; }
    # Control: the same element correctly typed still decodes.
    OUT=$(printf '\306\014\002\022\113\101\007' | skip_decode) \
        || { echo "FAIL: [$label] a correctly typed string element must decode"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"string_array":\["KA"\]' \
        || { echo "FAIL: [$label] control: a well-typed element must still bind; got: $OUT"; exit 1; }
    echo "==> [$label] skipped-subtree containment OK"

    echo "==> [$label] a §7.3-skipped array must leave no residue"
    # #270: ARRAY_UNSIGNED at the declared i8[] (ARRAY_SIGNED) is skipped -- and must
    # leave the fill counter DISARMED, or the bare scalar that follows at id 0 is
    # absorbed into arrays.u8s.
    OUT=$(printf '\246\006\013\001\004\000\000\007' | skip_decode) \
        || { echo "FAIL: [$label] a kind-mismatched array must be skipped, not fail"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"u8s":\[\]' \
        || { echo "FAIL: [$label] a skipped array must not absorb the NEXT scalar (#270); got: $OUT"; exit 1; }
    # #271: ARRAY_FIXLEN at the same declared i8[] carries count 127, above that
    # field's `count: 5`. The bound belongs to a field this header is not, so the
    # array is skipped and the message is merely truncated -- INCOMPLETE, not INVALID.
    ERR=$( (printf '\246\006\015\177\040' | skip_decode 2>&1 >/dev/null) || true )
    echo "$ERR" | grep -q 'InvalidMsg' \
        && { echo "FAIL: [$label] a bound must not be applied to a kind-mismatched array (#271); got: $ERR"; exit 1; }
    # Control: over-count detection still works where the bound DOES apply.
    ERR=$( (printf '\246\006\014\177\007' | skip_decode 2>&1 >/dev/null) || true )
    echo "$ERR" | grep -q 'InvalidMsg' \
        || { echo "FAIL: [$label] control: a well-typed over-count array must still be INVALID; got: $ERR"; exit 1; }
    echo "==> [$label] skipped-array residue OK"

    # #273: a repeated wrapper-array element id is REPLACED, not appended to
    # (MESSAGE_SPEC §7.4 last-wins). Appending also tripped the capacity check into
    # a bogus BufferFull on no_std.
    echo "==> [$label] a repeated wrapper element must be replaced (§7.4)"
    OUT=$(printf '\306\014\002\022\101\102\002\022\103\104\007' | skip_decode) \
        || { echo "FAIL: [$label] a repeated element id must decode, not report BufferFull (#273)"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"string_array":\["CD"\]' \
        || { echo "FAIL: [$label] last occurrence must win (#273); got: $OUT"; exit 1; }
    echo "==> [$label] repeated-element replace OK"

    echo "==> [$label] shared-vector byte-exact conformance"
    python3 "$ROOT/tests/conformance/rust/check_vectors.py" "$corelib/assets/test_vectors.json" "$WORK/conf-$label"

    echo "==> [$label] corpus + realworld: every definition builds"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
        # no_maxlen.yaml exists to exercise genuinely unbounded string/blob fields.
        # The no_std profile rejects those by design — in both storage modes — so
        # it is not a definition this leg can compile, and skipping it is the
        # honest outcome rather than a bound invented for the test.
        # seq_elements_dyn.yaml is the same category: count-less wrapper arrays,
        # which the no_std profile rejects for the same reason. Its bounded
        # counterparts (seq_elements, nested_rows) do compile here.
        case "$label:$(basename "$def")" in
        no-std-*:no_maxlen.yaml | no-std-*:seq_elements_dyn.yaml) continue ;;
        esac
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

# corelib-rs-no-std is the genuinely #![no_std] profile. Every field is
# schema-bounded there whatever storage it uses, and allow_dynamic selects that
# storage: alloc::String/alloc::Vec instead of heapless containers, for a target
# that has an allocator. This leg exercises the alloc mode; the heapless default
# is proven below. The corpus spans the feature-subset matrix under the same
# config.
run_variant no-std-dynamic "corelib: rs-no-std, allow_dynamic: true" "$NOSTD"

# The pure heapless profile through the same matrix. It is the DEFAULT for
# corelib: rs-no-std, and until now only its builds were checked.
run_variant no-std-static "corelib: rs-no-std" "$NOSTD"

# The point of the no_std profile is a crate that builds as #![no_std] and
# heap-free. A bin cannot be no_std on a hosted target, so prove it on the lib
# target: `cargo build --lib --no-default-features` drops serde/std and compiles
# the pure heapless (+ optional alloc) crate. Exercise BOTH allow_dynamic configs,
# mirroring the c-cpp bounded-vs-allow_dynamic split.
echo "==> no_std lib builds heap-free (--lib --no-default-features), allow_dynamic on AND off"

# (a) allow_dynamic: true — every variable-length field is an alloc container, so
# the crate pulls `extern crate alloc` yet still compiles as #![no_std] on a lib.
# Deliberately inspects the crate run_variant built for the dynamic leg -- the
# assertion is about THAT leg's output, so borrowing it is the point here.
grep -q 'extern crate alloc' "$WORK/ex-no-std-dynamic/src/lib.rs" || { echo "FAIL: allow_dynamic crate should pull extern crate alloc"; exit 1; }
( cd "$WORK/ex-no-std-dynamic" && cargo build -q --lib --no-default-features )
echo "==> [no-std-dynamic] lib builds (alloc fallback)"

# (b) allow_dynamic: false (default) — a fully bounded schema must lower to pure
# heapless with NO allocator at all (no `extern crate alloc`), and an unbounded
# field must instead be a hard generation error.
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs-no-std } }\n' > "$WORK/cfg-no-std-static.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-no-std-static.yaml" --lang rust --in "$WORK/conf.yaml" --out "$WORK/no-std-static" )
if grep -q 'extern crate alloc' "$WORK/no-std-static/src/lib.rs"; then echo "FAIL: no-std-static crate must not pull alloc"; exit 1; fi
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/no-std-static/Cargo.toml"
( cd "$WORK/no-std-static" && cargo build -q --lib --no-default-features )
echo "==> [no-std-static] lib builds (pure heapless, no alloc)"

# Building the no-std-static crate is not the same as running it. The heapless profile
# has its own `acc` -- the buffer that reassembles a string/blob split across
# feed chunks is a fixed-capacity heapless::Vec here, a distinct path from the
# alloc leg's growable Vec. Drive it.
# A declared feature combination that is never built breaks silently. `serde`
# without `std` is exactly that: a bare-metal consumer that wants the derives but
# no std. It is offered, so it has to compile.
echo "==> no_std + serde without std compiles"
( cd "$WORK/no-std-static" && cargo build -q --lib --no-default-features --features serde )

# The generated crate emits sofab::require!(...) to fail the build when the
# corelib was compiled without a wire feature the schema needs. A guard that
# never fires in a test is a guard nobody has verified -- so strip one and
# require the build to fail. `sequence` is the right lever: nothing implies it,
# whereas removing `fixlen` proves nothing because `fp64 = ["fixlen"]` pulls it
# straight back in (which is how this check was wrong on its first attempt).
echo "==> the capability guard fires when a corelib feature is stripped"
cp "$WORK/no-std-static/Cargo.toml" "$WORK/no-std-static/Cargo.toml.bak"
sed -i 's/, "sequence"//' "$WORK/no-std-static/Cargo.toml"
if ( cd "$WORK/no-std-static" && cargo build -q --lib --no-default-features 2>"$WORK/guard.err" ); then
    echo "FAIL: building without the corelib's \`sequence\` feature must not succeed"
    mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"
    exit 1
fi
grep -q 'requires the `sequence` feature' "$WORK/guard.err" || {
    echo "FAIL: build failed, but not with the require!() capability message:"
    head -20 "$WORK/guard.err"
    mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"
    exit 1
}
mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"
echo "==> guard fired as expected"

echo "==> [no-std-static] streaming behaviour"
cp "$ROOT/tests/conformance/rust/streaming_check_nostd.rs" "$WORK/no-std-static/src/main.rs"
( cd "$WORK/no-std-static" && cargo run -q --features std )

# An unbounded field is rejected under no_std in BOTH storage modes: allow_dynamic
# chooses the container, never whether a bound is needed, so one schema stays
# valid for every no_std target.
printf 'version: 1\nmessages:\n  m: { payload: { s: { id: 0, type: string } } }\n' > "$WORK/unbounded.yaml"
# Own configs, under names no other step writes. run_variant creates
# cfg-<label>.yaml as a side effect of building its leg; reusing one of those
# here would make this step depend on which legs ran, and in what order.
printf 'targets: { rust: { corelib: rs-no-std } }\n' > "$WORK/reject-static.yaml"
printf 'targets: { rust: { corelib: rs-no-std, allow_dynamic: true } }\n' > "$WORK/reject-dynamic.yaml"
for c in reject-static reject-dynamic; do
    if ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/$c.yaml" --lang rust --in "$WORK/unbounded.yaml" --out "$WORK/unbounded-$c" 2>/dev/null ); then
        echo "FAIL: unbounded field under no_std ($c) should error"; exit 1
    fi
done
echo "==> unbounded field is rejected in both storage modes"

# The point of this one is the SMALLEST build the generator can produce, so it
# uses the static profile: no allocator, and a varint-only schema needs none of
# the corelib's wire features. It previously borrowed the dynamic leg's config,
# which pulls alloc in -- the opposite of what a minimal-footprint check wants.
echo "==> no-std feature-subset smoke: a varint-only schema builds with no features"
printf 'version: 1\nmessages:\n  tiny: { payload: { a: { id: 0, type: i32 }, b: { id: 1, type: u16 }, c: { id: 2, type: boolean } } }\n' > "$WORK/tiny.yaml"
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs-no-std } }\n' > "$WORK/cfg-tiny.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-tiny.yaml" --lang rust --in "$WORK/tiny.yaml" --out "$WORK/tiny" )
grep -q 'default-features = false' "$WORK/tiny/Cargo.toml" || { echo "FAIL: varint-only schema should need no sofab features"; exit 1; }
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/tiny/Cargo.toml"
( cd "$WORK/tiny" && cargo build -q )
echo "==> minimal no-std footprint build OK"

echo "PASS"
