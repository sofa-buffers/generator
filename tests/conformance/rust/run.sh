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

# One shared build directory for every crate this run generates. Each crate is
# tiny; the cost is the dependency graph behind it (the corelib, serde,
# serde_json), and with a target/ per crate that graph is rebuilt once per
# project -- roughly a hundred times over a full run. Sharing it compiles the
# dependencies once (measured: five corpus crates, 48s separate vs 9s shared).
#
# Sharing is only safe because crate_bin_name below gives every crate a distinct
# binary name. Every generated Cargo.toml declares the SAME package name
# (sofabuffers-generated) and the same [[bin]] name (harness), and rlibs carry a
# metadata hash while the binary does not -- so a shared target/ collapses all of
# them onto one target/debug/harness and serves whichever was built last.
# Measured on the lim/nolim pair (max_dyn_array_count: 4 vs none, decoding a
# 5-element array): separate target dirs reject/accept correctly, one shared
# target dir rejects BOTH, and a shared target dir with distinct binary names is
# correct again. Never share the directory without the rename.
CARGO_TARGET_DIR="$WORK/target"
export CARGO_TARGET_DIR

# crate_bin_name OUT-DIR -- give a generated crate an identity unique to its
# output directory, so a shared target/ cannot serve one crate's artifacts to
# another. BOTH names have to move: the binary, because target/debug/<name>
# carries no metadata hash, and the package, because the rlib's hash is derived
# from it -- with one package name for every crate, a crate that imports the
# library gets whichever one was compiled last (seen as "cannot find type Vecu"
# in the no-std legs, where conf-* linked ex-*'s library).
#
# Only the [package] and [[bin]] names are touched. The no_std legs emit a lib.rs
# whose harness says `use sofabuffers_generated::*`, and that keeps working
# untouched because their template already pins [lib] name explicitly -- the
# import name is independent of the package name it sits in.
crate_bin_name() {
    uniq=$(printf '%s' "${1#$WORK/}" | tr -c 'A-Za-z0-9' '_')
    sed -i "s/^name = \"sofabuffers-generated\"/name = \"sofabuffers-generated-$uniq\"/" "$1/Cargo.toml"
    sed -i "s/^name = \"harness\"/name = \"harness_$uniq\"/" "$1/Cargo.toml"
}

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
        crate_bin_name "$2"
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
    #
    # TWO INDEPENDENT AXES, and conflating them is what left a hole (generator#306):
    #
    #   which CHECK  follows the field STORAGE. streaming_check.rs assigns
    #                String/Vec directly, which heapless cannot take, so
    #                fixed-capacity storage runs streaming_check_nostd.rs instead.
    #   which SHAPE  follows the CORELIB. A no_std project is a lib crate
    #                (`use sofabuffers_generated::*`), a std one is a bin crate
    #                that includes message.rs as a module.
    #
    # `allow_dynamic: false` sets the first and says nothing about the second, so
    # `rs-static` -- std corelib, heapless fields -- is a real fourth combination.
    # It used to fall into a blanket `*-static` skip whose stated reason was that
    # streaming is "orthogonal to which container a field lives in". Probably true,
    # but that is the same reasoning that hid corelib-ts#91, where a representation
    # switch routed encode through a different corelib method whose fixed-buffer
    # path was missing. Cheap to just run it.
    #
    # no-std-static is the one label still skipped here: it has a dedicated leg
    # further down, against the crate built for the no_std lib checks.
    # Each check file is written against ONE definition -- streaming_check.rs
    # against the example (Myfirstmessage), streaming_check_nostd.rs against
    # conf.yaml (Vecs/Vecsa, whose maxlen/count give the heapless types their
    # capacities). So the schema travels with the check, not with the leg.
    case "$label" in
        no-std-static) STREAM_CHECK="" ;;
        *-static)      STREAM_CHECK=streaming_check_nostd.rs; STREAM_DEF="$WORK/conf.yaml" ;;
        *)             STREAM_CHECK=streaming_check.rs;       STREAM_DEF="$EXAMPLE" ;;
    esac
    if [ -n "$STREAM_CHECK" ]; then
    echo "==> [$label] streaming: serialize through a sink, feed the decoder in chunks ($STREAM_CHECK)"
    rm -rf "$WORK/stream-$label"
    rust_build "$STREAM_DEF" "$WORK/stream-$label"
    case "$label" in
        no-std-*) printf 'use sofabuffers_generated::*;\n' > "$WORK/stream-$label/src/main.rs" ;;
        *)        printf 'mod message;\nuse message::*;\n' > "$WORK/stream-$label/src/main.rs" ;;
    esac
    sed '/^\/\/SOFAB_IMPORT$/d' "$ROOT/tests/conformance/rust/$STREAM_CHECK" \
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

    # Deep unknown nesting must not displace the live scope (generator#283 /
    # Crucible F-0055). The visitor tracks the current scope on a stack whose
    # no_std capacity is sized from the SCHEMA (one entry per reachable frame),
    # while nesting depth is a property of the WIRE: MAX_DEPTH is 255 (§4.9/§6.2),
    # and an unknown sequence -- which a decoder must accept and skip -- may nest
    # arbitrarily inside it. Stacking those skipped levels overran the capacity,
    # the surplus pushes were dropped, and the matching pops then restored the
    # WRONG scope: a field written after the unwind bound nowhere and the message
    # decoded fine, minus that field.
    #
    # Wire: open somestruct (id 20 -> a6 01), open 40 unknown sequences (id 60 ->
    # e6 03) and close them all (07), then -- back at somestruct scope -- a nested
    # struct (id 2 -> 16) carrying deepint = -7 (id 0 signed -> 01, zigzag 0d) and
    # nestedint = 42 (id 0 unsigned -> 00 2a), then close. Both fields must arrive:
    # what the skipped subtree did to the stack has to leave no trace.
    echo "==> [$label] deep unknown nesting must not lose the field after it (generator#283)"
    { printf '\246\001'
      i=0; while [ $i -lt 40 ]; do printf '\346\003'; i=$((i+1)); done
      i=0; while [ $i -lt 40 ]; do printf '\007'; i=$((i+1)); done
      printf '\026\001\015\007\000\052\007'
    } > "$WORK/deepnest.bin"
    OUT=$(cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/deepnest.bin") \
        || { echo "FAIL: [$label] 40 nested unknown sequences are legal (< MAX_DEPTH) and must decode"; exit 1; }
    echo "$OUT" | grep -q '"nestedint":42' || { echo "FAIL: [$label] field after a deep unknown-sequence unwind was lost (#283); got: $OUT"; exit 1; }
    echo "$OUT" | grep -q '"deepint":-7' || { echo "FAIL: [$label] nested struct after a deep unknown-sequence unwind was lost (#283); got: $OUT"; exit 1; }
    echo "==> [$label] deep unknown nesting OK"

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

    # ...and the same violation with the message cut RIGHT AFTER the word that
    # carries it (S5.2, generator#267). The over-maxlen is fully established by the
    # fixlen length word: 17 > maxlen 16 is decided by bytes already on the wire,
    # so running out of input cannot downgrade the verdict to INCOMPLETE.
    #
    # Before the fixlen_begin hook existed in corelib-rs the guard lived in the
    # PAYLOAD callback, which never fires for a message that ends here -- so this
    # reported Incomplete. The precision control below is what makes this an
    # ORDERING assertion rather than a blanket reject.
    #
    #   62      blob, field id 12 ((12<<3)|2)
    #   8b 01   fixlen word: byte length 17, blob subtype -> ((17<<3)|3)
    #   <EOF>   not one payload byte
    echo "==> [$label] over-maxlen + truncation must be INVALID, not INCOMPLETE"
    printf '\142\213\001' > "$WORK/overmaxlen_trunc.bin"
    printf '\142\203\001' > "$WORK/inmaxlen_trunc.bin"
    ERR=$( (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/overmaxlen_trunc.bin" 2>&1 >/dev/null) || true )
    case "$ERR" in
        *InvalidMsg*) ;;
        *) echo "FAIL: [$label] over-maxlen(17>16)+truncated -> $ERR (want InvalidMsg)"; exit 1 ;;
    esac
    # Precision control: an IN-BOUND length (16 == maxlen) cut at the same offset is
    # a clean truncation and MUST stay Incomplete.
    ERR=$( (cd "$WORK/ex-$label" && cargo run -q -- decode myfirstmessage < "$WORK/inmaxlen_trunc.bin" 2>&1 >/dev/null) || true )
    case "$ERR" in
        *Incomplete*) ;;
        *) echo "FAIL: [$label] in-bound(16==16)+truncated -> $ERR (want Incomplete)"; exit 1 ;;
    esac
    echo "==> [$label] maxlen/truncation ordering OK"

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

# Receiver-side decode limits (generator#102) and their §6.2.1 precision rules,
# std corelib only (the no_std profile is statically bounded, so the keys are
# inert there -- an unbounded field is a generate-time error, never a capped one).
#
# Rust std is the one target that enforces every cap in GENERATED CODE and keeps
# it there (ARCHITECTURE §9.5.2): its visitor owns the whole message mutably, so
# there is no corelib call to hang the number on. That makes the placement of
# each guard this repo's business alone, and these rows are what pins it:
#
#   * the cap fires at the count/length header of a field actually READ, and
#     answers LimitExceeded -- a policy verdict on well-formed bytes (§6.3);
#   * a field MESSAGE_SPEC §7.3 skips -- an unknown id, or a wire type that
#     contradicts the declared one -- is NEVER capped, so the decode stays
#     COMPLETE and the field keeps its default (generator#410). In Rust this
#     falls out of the dispatch itself: every guard sits in a match arm keyed by
#     (wire callback, location, id), so a skipped field reaches no arm. That is a
#     property to prove, not to assume -- the arms are generated, and an arm
#     widened to `_` would cap the skip silently;
#   * a SCHEMA-bounded field is never governed by a cap: its own bound governs
#     and its violation is InvalidMsg (MESSAGE_SPEC §7.1). Both halves are
#     pinned below with the schema bound deliberately ABOVE the cap (maxlen 32
#     and count 6 against caps of 8 and 4), so a field that answered the cap
#     would be visible as a rejection where the schema still permits the value.
#     That is the precision a decoder-level cap cannot have (§6.2.1).
echo "==> [rs] receiver-side decode limits (generator#102, CORELIB_PLAN §6.2.1)"
cat > "$WORK/dyn.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      a:  { id: 0, type: array, items: { type: u64 } }
      s:  { id: 1, type: string }
      b:  { id: 2, type: blob }
      bs: { id: 3, type: string, maxlen: 32 }
      ba: { id: 4, type: array, items: { type: u64, count: 6 } }
YAML
cat > "$WORK/wrap.yaml" <<'YAML'
version: 1
$defs:
  struct:
    Kv:
      k: { id: 0, type: u32 }
messages:
  wrap:
    payload:
      strs: { id: 0, type: array, items: { type: string } }
      objs: { id: 1, type: array, items: { type: struct, fields: { $ref: '#/$defs/struct/Kv' } } }
YAML
printf 'generic: { emit: project, max_dyn_array_count: 4, max_dyn_string_len: 8, max_dyn_blob_len: 8 }\ntargets: { rust: { corelib: rs } }\n' > "$WORK/cfg-lim.yaml"
lim_project() { # DEF OUT -- generate, point at the std corelib, build
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-lim.yaml" --lang rust --in "$1" --out "$2" )
    sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$2/Cargo.toml"
    crate_bin_name "$2"
    ( cd "$2" && cargo build -q )
}
lim_project "$WORK/dyn.yaml" "$WORK/lim"
lim_project "$WORK/wrap.yaml" "$WORK/wlim"

# lim_run DIR MSG OCTAL -- decode one hand-built message; JSON in LIM_OUT, the
# harness's `decode error: <category>` line in LIM_ERR, exit status in LIM_RC.
# Only that line is kept: `cargo run` re-emits the crate's build warnings on
# stderr, and a diagnostic drowned in them is a test nobody can read.
lim_run() {
    LIM_RC=0
    (printf "$3" | (cd "$1" && cargo run -q -- decode "$2") >"$WORK/lim.out") 2>"$WORK/lim.err" || LIM_RC=$?
    LIM_OUT=$(tr -d ' ' < "$WORK/lim.out")
    LIM_ERR=$(grep 'decode error' "$WORK/lim.err" || tail -2 "$WORK/lim.err")
}
lim_complete() { # DIR MSG OCTAL EXPECT-JSON-SUBSTRING DESC
    lim_run "$1" "$2" "$3"
    [ "$LIM_RC" = 0 ] || { echo "FAIL: $5 must decode COMPLETE; got: $LIM_ERR"; exit 1; }
    echo "$LIM_OUT" | grep -q "$4" || { echo "FAIL: $5 -- expected $4 in: $LIM_OUT"; exit 1; }
}
lim_reject() { # DIR MSG OCTAL CATEGORY DESC
    lim_run "$1" "$2" "$3"
    [ "$LIM_RC" != 0 ] || { echo "FAIL: $5 must be refused as $4; decoded: $LIM_OUT"; exit 1; }
    echo "$LIM_ERR" | grep -q "$4" || { echo "FAIL: $5 must be $4 (§6.3 keeps the categories apart); got: $LIM_ERR"; exit 1; }
}

# (1) The cap itself, on each of the three unbounded kinds. Headers are
# (id << 3) | wire type; a fixlen word is (length << 3) | subtype (2 = string,
# 3 = blob). Caps: array count 4, string len 8, blob len 8.
lim_reject "$WORK/lim" dyn '\003\005\001\002\003\004\005'  LimitExceeded "5 elements > max_dyn_array_count 4"
lim_reject "$WORK/lim" dyn '\012\112ABCDEFGHI'             LimitExceeded "a 9-byte string > max_dyn_string_len 8"
lim_reject "$WORK/lim" dyn '\022\113ABCDEFGHI'             LimitExceeded "a 9-byte blob > max_dyn_blob_len 8"
lim_complete "$WORK/lim" dyn '\003\004\001\002\003\004'    '"a":\[1,2,3,4\]' "4 elements == the cap"

# (1b) An over-cap count/length followed by END OF INPUT is LimitExceeded, not
# INCOMPLETE. The cap is decided at the count/length header (CORELIB_PLAN §6.2.1
# "Enforcement point") and the rejection is terminal (§6.3): the header has
# arrived, the verdict is in, and no continuation can lift it -- so INCOMPLETE
# would both lose the category and invite the caller to feed bytes that cannot
# help. That is the hostile-sender shape this pins: a 4-byte message that holds a
# connection open. The rest of the family already answered LimitExceeded here;
# Rust was reporting feed's Incomplete because try_decode surfaced it ahead of
# the sticky lim flag.
lim_reject "$WORK/lim" dyn '\003\005\001\002' LimitExceeded "an over-cap count (5 > 4) then EOF"
lim_reject "$WORK/lim" dyn '\012\112ABC'      LimitExceeded "an over-cap string length (9 > 8) then EOF"
lim_reject "$WORK/lim" dyn '\022\113ABC'      LimitExceeded "an over-cap blob length (9 > 8) then EOF"
# The precision controls. An IN-cap header that is genuinely truncated is a clean
# truncation and MUST stay Incomplete -- the cap must not turn every short
# message into a policy rejection.
lim_reject "$WORK/lim" dyn '\003\004\001\002' Incomplete "an in-cap count (4 == 4) then EOF"
lim_reject "$WORK/lim" dyn '\012\102ABC'      Incomplete "an in-cap string length (8 == 8) then EOF"
# ...and a §7.3-skipped field is never capped (#410), truncated or not: an
# over-cap array at an unknown id leaves only the truncation to report.
lim_reject "$WORK/lim" dyn '\073\005\001\002' Incomplete "an over-cap array at the UNKNOWN id 7, then EOF"

# (2) generator#410: a §7.3-skipped field is never capped. Every row is over its
# kind's cap, and every row must decode COMPLETE with the declared field left at
# its default -- a receiver refusing a message whose only offence is a field it
# was never going to read is the defect this pins.
lim_complete "$WORK/lim" dyn '\004\005\000\000\000\000\000' '"a":\[\]'  "a SIGNED array at the unsigned-declared id 0 (§7.3 skip)"
lim_complete "$WORK/lim" dyn '\073\005\001\002\003\004\005' '"a":\[\]'  "an over-cap array at the UNKNOWN id 7"
lim_complete "$WORK/lim" dyn '\012\113ABCDEFGHI'            '"s":""'    "a BLOB at the string-declared id 1 (§7.3 skip)"
lim_complete "$WORK/lim" dyn '\022\112ABCDEFGHI'            '"b":\[\]'  "a STRING at the blob-declared id 2 (§7.3 skip)"
lim_complete "$WORK/lim" dyn '\072\112ABCDEFGHI'            '"s":""'    "an over-cap string at the UNKNOWN id 7"
lim_complete "$WORK/lim" dyn '\072\113ABCDEFGHI'            '"b":\[\]'  "an over-cap blob at the UNKNOWN id 7"

# (3) The shared property: a schema-bounded field is governed by its OWN bound,
# never by a cap. `bs` declares maxlen 32 against a string cap of 8, `ba` a count
# of 6 against an array cap of 4 -- so a value between the two proves the cap is
# not consulted, and a value above the schema bound proves the verdict is
# InvalidMsg and not the cap's category.
lim_complete "$WORK/lim" dyn '\032\242\001ABCDEFGHIJKLMNOPQRST' '"bs":"ABCDEFGHIJKLMNOPQRST"' "a 20-byte string on a maxlen-32 field (cap 8 must not apply)"
lim_complete "$WORK/lim" dyn '\043\005\001\002\003\004\005'     '"ba":\[1,2,3,4,5\]'          "5 elements on a count-6 field (cap 4 must not apply)"
lim_reject "$WORK/lim" dyn '\032\302\002ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl' InvalidMsg "a 40-byte string over maxlen 32"
lim_reject "$WORK/lim" dyn '\043\007\001\002\003\004\005\006\007'                InvalidMsg "7 elements over count 6"

# (4) The same three rules on WRAPPER arrays, whose element index is itself
# capped (generator#387/#397) and whose element payloads are capped per element.
# `strs` and `objs` are sequence-framed: the sequence header is (id << 3) | 6,
# each element's id IS its array index, and 0x07 closes the sequence.
lim_complete "$WORK/wlim" wrap '\006\002\022AB\007'      '"strs":\["AB"\]' "a well-typed string element at index 0"
lim_reject   "$WORK/wlim" wrap '\006\112\022AB\007'      LimitExceeded     "an element index 9 >= max_dyn_array_count 4"
lim_reject   "$WORK/wlim" wrap '\006\002\112ABCDEFGHI\007' LimitExceeded   "a 9-byte string element > max_dyn_string_len 8"
lim_complete "$WORK/wlim" wrap '\016\006\000\052\007\007' '"k":42'         "a well-typed object element at index 0"
lim_reject   "$WORK/wlim" wrap '\016\116\000\052\007\007' LimitExceeded     "an object element index 9 >= max_dyn_array_count 4"
# #410 on the wrapper leg: a mistyped element is skipped, so neither the index
# cap nor the element cap may fire -- including at an index far above the cap.
lim_complete "$WORK/wlim" wrap '\006\112\023AB\007'        '"strs":\[\]' "a BLOB element at a string-array index 9 (§7.3 skip)"
lim_complete "$WORK/wlim" wrap '\006\002\113ABCDEFGHI\007' '"strs":\[\]' "an over-cap BLOB element at a string-array index 0 (§7.3 skip)"
lim_complete "$WORK/wlim" wrap '\016\112\022AB\007'        '"objs":\[\]' "a STRING at an object-array index 9 (§7.3 skip)"

# The keys are what set the number, but an unset key is the TARGET DEFAULT and
# never "unlimited" (generator#385): the same oversized bytes decode against a
# project with no key set, because 5 elements is far under that default.
printf 'generic: { emit: project }\ntargets: { rust: { corelib: rs } }\n' > "$WORK/cfg-nolim.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-nolim.yaml" --lang rust --in "$WORK/dyn.yaml" --out "$WORK/nolim" )
sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$WORK/nolim/Cargo.toml"
crate_bin_name "$WORK/nolim"
( cd "$WORK/nolim" && cargo build -q )
printf '\003\005\001\002\003\004\005' > "$WORK/lim-over.bin"
(cd "$WORK/nolim" && cargo run -q -- decode dyn < "$WORK/lim-over.bin" >/dev/null) || { echo "FAIL: default-cap project must decode oversized input"; exit 1; }
echo "==> [rs] decode limits OK"

# A skipped payload is WALKED, not materialised -- and that is a MEASUREMENT,
# because every row above passes with a decoder that materialises the payload and
# then drops it. The generated blob callback used to do exactly that: it fed every
# delivered payload into `self.acc`, which sizes its buffer from the wire `total`
# and copies the bytes in, and only then dispatched on (loc, id) and found no arm.
# A 1 MiB blob at an unknown id cost 1 MiB for a field nobody reads (CORELIB_PLAN
# §6.2.1, §6.6, §6.7.2 / MESSAGE_SPEC §7.3).
#
# Built against the CAPPED config on purpose (max_dyn_blob_len: 8), so the row
# carries both halves at once: over the cap by five orders of magnitude and still
# COMPLETE, because a skipped field is never capped -- and still free.
#
# The check replaces the crate's main with a counting global allocator, the same
# way the streaming legs replace it with streaming_check.rs.
echo "==> [rs] a §7.3-skipped 1 MiB blob allocates nothing (CORELIB_PLAN §6.2.1/§6.6)"
rm -rf "$WORK/skipalloc"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-lim.yaml" --lang rust --in "$WORK/dyn.yaml" --out "$WORK/skipalloc" )
sed -i "s#\${SOFAB_RS_CORELIB}#$STD#" "$WORK/skipalloc/Cargo.toml"
crate_bin_name "$WORK/skipalloc"
printf 'mod message;\nuse message::*;\n' > "$WORK/skipalloc/src/main.rs"
sed '/^\/\/SOFAB_IMPORT$/d' "$ROOT/tests/conformance/rust/skipped_blob_alloc.rs" \
    >> "$WORK/skipalloc/src/main.rs"
( cd "$WORK/skipalloc" && cargo run -q ) || { echo "FAIL: a skipped blob must not be materialised"; exit 1; }
echo "==> [rs] skipped-blob allocation OK"

# corelib-rs-no-std is the genuinely #![no_std] profile. Every field is
# schema-bounded there whatever storage it uses, and allow_dynamic selects that
# storage: alloc::String/alloc::Vec instead of heapless containers, for a target
# that has an allocator. This leg exercises the alloc mode; the heapless default
# is proven below. The corpus spans the feature-subset matrix under the same
# config.
# Static storage on the STD corelib (allow_dynamic: false against corelib-rs):
# schema-bounded fields become heapless containers in an otherwise ordinary std
# crate. The whole matrix runs again under it, because the property that matters
# is that storage is invisible on the wire -- same schema, same bytes, same
# shared vectors, only where the bytes live differs.
#
# Deliberately fed the UNMODIFIED example, whose `somemap` carries no count: on
# this profile an unbounded field simply keeps its Vec instead of being a
# generate-time error, which is the difference from the no_std legs below and the
# reason the switch can be turned on without touching a schema.
run_variant rs-static "corelib: rs, allow_dynamic: false" "$STD"

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
crate_bin_name "$WORK/no-std-static"
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
# never fires in a test is a guard nobody has verified, so every one of the five
# features corelib-rs-no-std gates gets stripped in turn and the build has to
# fail with that feature named. This mirrors what tests/conformance/c/run.sh
# already does for corelib-c-cpp's SOFAB_DISABLE_*_SUPPORT macros, where all five
# are exercised; before this, rust checked exactly one of them.
#
# Each case names the features that REMAIN, rather than the one to delete: the
# list is order-sensitive to a sed, and `fixlen` cannot be removed on its own
# because `fp64 = ["fixlen"]` pulls it straight back in (which is how this check
# was wrong on its first attempt) -- so the fixlen case drops fp64 with it.
#
# cargo check, not build: require!() is a compile-time assertion, so type-checking
# is enough to trip it and there is no reason to pay for codegen five times.
echo "==> the capability guard fires for every gated corelib wire feature"
cp "$WORK/no-std-static/Cargo.toml" "$WORK/no-std-static/Cargo.toml.bak"
guard_case() {  # missing-feature  "remaining features, comma-separated and quoted"
    missing=$1; remain=$2
    cp "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"
    sed -i "s/features = \[\"array\", \"fixlen\", \"fp64\", \"sequence\", \"value64\"\]/features = [$remain]/" \
        "$WORK/no-std-static/Cargo.toml"
    grep -q "features = \[$remain\]" "$WORK/no-std-static/Cargo.toml" || {
        echo "FAIL: [$missing] could not rewrite the feature list -- the generated form changed"
        mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"; exit 1
    }
    if ( cd "$WORK/no-std-static" && cargo check -q --lib --no-default-features 2>"$WORK/guard.err" ); then
        echo "FAIL: building without the corelib's \`$missing\` feature must not succeed"
        mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"; exit 1
    fi
    # The corelib does not word every one of these the same way -- `value64` says
    # "requires the 64-bit value width (the default `value64` feature is disabled)"
    # where the others say "requires the `array` feature". Both name the feature in
    # backticks and both carry the sofab: prefix, so match on that pair rather than
    # on one phrasing.
    grep -q "sofab:.*\`$missing\`" "$WORK/guard.err" || {
        echo "FAIL: [$missing] build failed, but not with the require!() capability message:"
        head -20 "$WORK/guard.err"
        mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"; exit 1
    }
    echo "   [$missing] guard fired"
}
guard_case array    '"fixlen", "fp64", "sequence", "value64"'
guard_case fp64     '"array", "fixlen", "sequence", "value64"'
guard_case sequence '"array", "fixlen", "fp64", "value64"'
guard_case value64  '"array", "fixlen", "fp64", "sequence"'
guard_case fixlen   '"array", "sequence", "value64"'
mv "$WORK/no-std-static/Cargo.toml.bak" "$WORK/no-std-static/Cargo.toml"
echo "==> all five capability guards fired as expected"

echo "==> [no-std-static] streaming behaviour"
# Same check file rs-static runs above, with the lib-crate import: the file is
# shared across storage-equal/shape-different legs, so it carries //SOFAB_IMPORT
# rather than a hardcoded `use`.
printf 'use sofabuffers_generated::*;\n' > "$WORK/no-std-static/src/main.rs"
sed '/^\/\/SOFAB_IMPORT$/d' "$ROOT/tests/conformance/rust/streaming_check_nostd.rs" \
    >> "$WORK/no-std-static/src/main.rs"
( cd "$WORK/no-std-static" && cargo run -q --features std )

# The footprint half of the skipped-blob rule, and the half that is a hard
# failure rather than a measurement. corelib-rs-no-std's PayloadAcc is a FIXED
# arena sized from the schema's largest bounded payload, and `feed` answers
# Err(Argument) the moment `total` exceeds it. So while the blob callback fed
# every delivered payload into that arena before resolving a destination, a blob
# at an id the schema does not declare was not merely copied for nothing --
# anything larger than the arena failed the WHOLE DECODE with BufferFull. A sender
# adding a field this receiver has not been rebuilt for is the ordinary
# forward-compatibility case MESSAGE_SPEC §7.3 exists to make safe, and on this
# profile it was a denial of service in one field (CORELIB_PLAN §6.7.2).
#
# Its own tiny crate: the arena has to be SMALL for the row to mean anything, and
# it is sized from the schema -- conf.yaml's maxlen 4096 would swallow the test.
echo "==> [no-std-static] a §7.3-skipped blob larger than the fixed accumulator still decodes"
printf 'version: 1\nmessages:\n  sb: { payload: { b: { id: 0, type: blob, maxlen: 8 }, s: { id: 1, type: string, maxlen: 8 } } }\n' > "$WORK/sb.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-no-std-static.yaml" --lang rust --in "$WORK/sb.yaml" --out "$WORK/skipnostd" )
sed -i "s#\${SOFAB_RS_CORELIB}#$NOSTD#" "$WORK/skipnostd/Cargo.toml"
crate_bin_name "$WORK/skipnostd"
printf 'use sofabuffers_generated::*;\n' > "$WORK/skipnostd/src/main.rs"
sed '/^\/\/SOFAB_IMPORT$/d' "$ROOT/tests/conformance/rust/skipped_blob_nostd.rs" \
    >> "$WORK/skipnostd/src/main.rs"
( cd "$WORK/skipnostd" && cargo run -q --features std ) \
    || { echo "FAIL: a skipped blob must not be fed to the fixed accumulator"; exit 1; }

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
crate_bin_name "$WORK/tiny"
( cd "$WORK/tiny" && cargo build -q )
echo "==> minimal no-std footprint build OK"

# Every generated crate must have gone through crate_bin_name. One that did not
# still carries the default identity, and out of the shared target/ it links
# another crate's artifacts -- which only surfaces as a compile error when the
# two schemas happen to differ in shape (it showed up as "cannot find type Vecu"
# while this was being written). Between similar schemas it would be a GREEN run
# on the wrong binary, so a forgotten call has to fail loudly instead.
stale=$(grep -rl '^name = "sofabuffers-generated"$' "$WORK" --include=Cargo.toml 2>/dev/null || true)
if [ -n "$stale" ]; then
    echo "FAIL: generated crates never got a unique identity (missing crate_bin_name):"
    echo "$stale"
    exit 1
fi
echo "==> every generated crate has a unique identity"

echo "PASS"
