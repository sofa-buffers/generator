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

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CPP="${1:-${SOFAB_CPP_DIR:-}}"
CC="${2:-${SOFAB_C_DIR:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CPP" ]; then
    clone_corelib corelib-cpp "$WORK/cpp"
    CPP="$WORK/cpp"
fi
if [ -z "$CC" ]; then
    clone_corelib corelib-c-cpp "$WORK/c"
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
  vecpa: { payload: { a: { id: 0, type: array, items: { type: struct, count: 8, fields: { k: { id: 0, type: u32 } } } } } }
YAML

# Exercises every field-type family (ints, u64, fp, bool, string, enum, bitfield,
# fixed array, blob, string array, blob array, nested struct, union).
#
# someenumarray / someboolarray are filled deliberately. They used to be left at
# their schema default, so no message this harness produced ever carried one --
# and the c-cpp decode arm for both, which reached the member through a
# reinterpret_cast of the CONTAINER (a std::vector's begin/end/capacity words
# taken as its first N elements, overwritten by wire bytes), was never executed.
# The leg stayed green while emitting a use-after-free reachable from any
# received message. An array kind nothing fills is an array kind nothing tests.
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someblobarray":[[1],[2],[3]],"someu64":18446744073709551615,"somestringarray":["a","b","c","d","e"],"someenumarray":[2,1,2,0],"someboolarray":[true,false,true,true,false,true,true,false],"somebitfieldarray":[1,2,3]}'

# run_variant LABEL CORELIB DYNAMIC INCLUDE MAKEVARS...
#   CORELIB  - "" for pure corelib-cpp, "c-cpp" for the corelib-c-cpp wrapper.
#   DYNAMIC  - "true"/"false": the c-cpp storage mode. Both are real profiles and
#              both need running. false is the DEFAULT one a schema author gets
#              from a bare `corelib: c-cpp`, and it has storage types of its own
#              (FixedString<N>, FixedBytes<N>, InlineVector<T,N>) with their own
#              decode paths -- it was previously never built here at all.
#   INCLUDE  - -I flag for the corpus syntax-only compile.
#   MAKEVARS - vars passed to `make` for the generated project.
run_variant() {
    label=$1; corelib=$2; dynamic=$3; include=$4; shift 4
    # corelib-cpp is header-only; the c-cpp wrapper needs the C sources linked.
    STREAM_OBJS=""
    if [ -n "$corelib" ]; then
        for u in ostream istream object utf8; do
            gcc -c -I"$CC/src/include" -o "$WORK/$u.o" "$CC/src/$u.c"
        done
        STREAM_OBJS="$WORK/ostream.o $WORK/istream.o $WORK/object.o $WORK/utf8.o"
    fi
    echo "==> [$label] generating + building example project"
    if [ -n "$corelib" ]; then
        # corelib-c-cpp is the embedded profile: every string/blob needs a maxlen
        # and every array a count, in BOTH storage modes. allow_dynamic selects
        # std::string/std::vector storage for those bounded fields (a target with
        # a heap) instead of inline containers — it does not make a bound
        # optional, so the schemas below are bounded first (see EXAMPLE).
        printf 'generic: { emit: project }\ntargets: { cpp: { namespace: sofabuffers, corelib: %s, allow_dynamic: %s } }\n' "$corelib" "$dynamic" > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers, corelib: %s, allow_dynamic: %s } }\n' "$corelib" "$dynamic" > "$WORK/cfg-corpus-$label.yaml"
    elif [ -n "$dynamic" ]; then
        # Pure corelib-cpp with an explicit storage mode. allow_dynamic works on
        # this corelib too, and here it is an optimisation rather than a
        # requirement: bounded fields go inline, unbounded ones keep their dynamic
        # container, so the SAME schemas as the default leg below are used -- no
        # bounding pass, and the wire must come out identical either way.
        printf 'generic: { emit: project }\ntargets: { cpp: { namespace: sofabuffers, allow_dynamic: %s } }\n' "$dynamic" > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers, allow_dynamic: %s } }\n' "$dynamic" > "$WORK/cfg-corpus-$label.yaml"
    else
        printf 'generic: { emit: project }\ntargets: { cpp: { namespace: sofabuffers } }\n' > "$WORK/cfg-$label.yaml"
        printf 'targets: { cpp: { namespace: sofabuffers } }\n' > "$WORK/cfg-corpus-$label.yaml"
    fi
    # example.yaml leaves `somemap` deliberately count-less to show the dynamic
    # form. The embedded profile cannot size that, so this leg gives it a capacity
    # — exactly what a schema author targeting an MCU does, and exactly what
    # tests/conformance/c/run.sh already does for the C target. `count` never
    # reaches the wire, so the round-trip and the shared vectors are unchanged.
    EXAMPLE="$ROOT/examples/messages/example.yaml"
    if [ -n "$corelib" ]; then
        EXAMPLE="$WORK/example-$label.yaml"
        awk '
          /^      somemap:/ { inmap=1 }
          inmap && /^          type: struct$/ { print; print "          count: 8"; inmap=0; next }
          { print }
        ' "$ROOT/examples/messages/example.yaml" > "$EXAMPLE"
    fi
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp --in "$EXAMPLE" --out "$WORK/ex-$label" )
    make -C "$WORK/ex-$label" "$@" >/dev/null

    # MAX_SIZE fill check (ARCHITECTURE §9.6): _maxSize sizes the encode buffer,
    # so a fully filled message must fit it AND reach it exactly.
    echo "==> [$label] MAX_SIZE fill check"
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp \
        --in "$ROOT/tests/conformance/lib/maxsize_fill.yaml" --out "$WORK/fill-$label" )
    make -C "$WORK/fill-$label" "$@" >/dev/null
    check_maxsize_fill "$label" "$WORK/fill-$label/harness/harness" encode fill

    # Streaming behaviour: both corelibs stream in both directions and always
    # have, but nothing drove either -- the capability was demonstrable and
    # unverified. Property: streaming is indistinguishable from the one-shot path.
    echo "==> [$label] streaming: serialize through a sink, feed in chunks"
    g++ -std=c++20 -Wall -Werror $include -I"$WORK/ex-$label" \
        -DMSG_TYPE=sofabuffers::Myfirstmessage -include myfirstmessage.hpp \
        -o "$WORK/stream-$label" "$ROOT/tests/conformance/cpp/streaming_check.cpp" \
        $STREAM_OBJS
    "$WORK/stream-$label"

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
        '"someenumarray":\[2,1,2,0\]' \
        '"someboolarray":\[true,false,true,true,false,true,true,false\]' \
        '"somebitfieldarray":\[1,2,3\]' \
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

        # Schema-bound INVALID dominates truncation (generator#216 / F-0032,
        # MESSAGE_SPEC S5.2). corelib-cpp measures a whole field for completeness
        # before delivering it, so a field that is BOTH over-bound and truncated
        # would misreport INCOMPLETE. The generated measure-phase schema
        # (setSchema, corelib-cpp#50) rejects at the deciding word instead. The
        # `status` harness mode surfaces the verdict the bare non-zero exit hides.
        # Each over-bound+truncated input MUST be INVALID; each in-bound+truncated
        # control MUST stay INCOMPLETE. Pure corelib-cpp only (no measure phase in
        # the c-cpp wrapper).
        echo "==> [$label] schema-bound + truncation ordering (generator#216)"
        # over-count: someuintarray (id 15) count 4; 7b 06 (count 6>4) 01 02 <EOF>.
        ST=$(printf '\173\006\001\002' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INVALID" ] || { echo "FAIL: [$label] over-count(6>4)+truncated -> $ST (want INVALID)"; exit 1; }
        ST=$(printf '\173\004\001\002' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: [$label] in-bound(4==4)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
        # over-maxlen: someblob (id 12) maxlen 16; 62 8b 01 (len 17>16) 01 <EOF>.
        ST=$(printf '\142\213\001\001' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INVALID" ] || { echo "FAIL: [$label] over-maxlen(17>16)+truncated -> $ST (want INVALID)"; exit 1; }
        ST=$(printf '\142\203\001\001' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: [$label] in-bound(16==16)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
        # over-index: somestringarray (id 18) count 5. A string element rides the
        # FIXLEN wire type, and S7.3 is decided from the fixlen word's subtype -- a
        # header alone does not yet say whether this is an element of THIS array.
        # So the over-index reject fires from the fixlen word on, not on the element
        # header (corelib-cpp#59 / c-cpp#119). 96 01 (seq id 18) 2a (elem index
        # 5 >= 5, fixlen) 0a (fixlen word: len 1, subtype string) <EOF>: the payload
        # is truncated, and the bound still wins.
        ST=$(printf '\226\001\052\012' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INVALID" ] || { echo "FAIL: [$label] over-index(id5>=5)+truncated -> $ST (want INVALID)"; exit 1; }
        # Cut BETWEEN the element header and its fixlen word: the subtype is not yet
        # known, so S7.3 cannot be decided and no schema bound may be applied yet.
        # INCOMPLETE, the analogue of S4.8's ruling for a fixlen array's two words.
        ST=$(printf '\226\001\052' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: [$label] over-index cut before the fixlen word -> $ST (want INCOMPLETE)"; exit 1; }
        ST=$(printf '\226\001\042' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: [$label] in-bound(id4<5)+truncated -> $ST (want INCOMPLETE)"; exit 1; }
        echo "==> [$label] schema-bound/truncation ordering OK"

        # The measure-phase bound is gated on the DECLARED fixlen subtype
        # (MESSAGE_SPEC S7.3, generator#229). fp32/fp64/string/blob all share the
        # Fixlen wire type, so a schema row that matched the wire type alone
        # measured a CONTRADICTING value against the field's maxlen and rejected
        # it, where S7.3 requires it be skipped like an unknown id. someblob
        # (id 12) declares blob, maxlen 16 and defaults to "Hello".
        # Wire: 62 (id 12, fixlen) 8a 01 (fixlen word: len 17, STRING subtype 2)
        #       + 17 bytes -> the STRING contradicts the declared blob, so the
        # field is skipped whole and someblob keeps its default. Pre-fix this was
        # INVALID (17 > 16 measured against the blob's maxlen).
        echo "==> [$label] a contradicting fixlen subtype carries no bound (S7.3, generator#229)"
        printf '\142\212\001\101\101\101\101\101\101\101\101\101\101\101\101\101\101\101\101\101' > "$WORK/subtypebound.bin"
        OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/subtypebound.bin") \
            || { echo "FAIL: [$label] an over-maxlen STRING at a blob id must skip, not reject"; exit 1; }
        echo "$OUT" | grep -q '"someblob":\[72,101,108,108,111\]' || { echo "FAIL: [$label] skipped fixlen field must keep its default \"Hello\"; got: $OUT"; exit 1; }
        # Control: the MATCHING subtype at the same length still hits the bound
        # (62 8b 01 = len 17, BLOB subtype 3) -- covered as INVALID above -- and
        # the S5.2 anti-folding order is unchanged for it. The truncated form of
        # the skipped shape is INCOMPLETE, never INVALID: a skipped field is still
        # measured for completeness.
        ST=$(printf '\142\212\001\101' | "$WORK/ex-$label/harness/harness" status myfirstmessage | head -n1)
        [ "$ST" = "INCOMPLETE" ] || { echo "FAIL: [$label] truncated contradicting subtype -> $ST (want INCOMPLETE)"; exit 1; }
        echo "==> [$label] subtype-gated bound OK"

        # Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose
        # header wire type is not the one its declared type maps to -- for fixlen,
        # including the subtype -- is SKIPPED, exactly like an unknown id. someu8
        # (id 0) is declared u8 (unsigned wire type) and keeps its schema default 7.
        # Wire: 01 = id 0 with wire type SIGNED (1), then the zig-zag varint 06.
        # read<T>() does not check the wire type (it zig-zags on T's signedness
        # alone), so without the generated guard this silently decoded to 6.
        # Control: 00 09 is the same id with the correct wire type -> 9. A third
        # vector, 06 07, gives the same id a SEQUENCE_START header closed by its
        # SEQUENCE_END, so the skip has to drain a whole nested sequence.
        # Pure corelib-cpp only: the guard needs is.wire()/is.fixType(), which the
        # c-cpp wrapper does not expose (corelib-cpp#43 landed for corelib-cpp only).
        echo "==> [$label] contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
        printf '\001\006' > "$WORK/wiremismatch.bin"
        printf '\000\011' > "$WORK/wiremismatch_control.bin"
        printf '\006\007' > "$WORK/wiremismatch_seq.bin"
        OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/wiremismatch.bin") \
            || { echo "FAIL: [$label] mismatched wire type must skip, not fail the decode"; exit 1; }
        echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: [$label] skipped field must keep its default 7; got: $OUT"; exit 1; }
        OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/wiremismatch_control.bin") \
            || { echo "FAIL: [$label] control (correct wire type) must decode"; exit 1; }
        echo "$OUT" | grep -q '"someu8":9' || { echo "FAIL: [$label] control must decode to 9; got: $OUT"; exit 1; }
        OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/wiremismatch_seq.bin") \
            || { echo "FAIL: [$label] sequence header on a scalar field must skip, not fail the decode"; exit 1; }
        echo "$OUT" | grep -q '"someu8":7' || { echo "FAIL: [$label] skipped sequence must keep the default 7; got: $OUT"; exit 1; }
        echo "==> [$label] wire-type skip OK"
    fi

    # Repeated field id (MESSAGE_SPEC S7.4, generator#175): last occurrence wins per
    # field id. A re-opened sequence CONTINUES its scope, so a struct merges and the
    # children an earlier opening set whose ids do not recur are retained. somestruct
    # (id 20) is opened twice: the first opening sets nestedstring (id 1) to "x", the
    # second opens only the empty nestedstruct (id 2). nestedstring MUST survive.
    # Both profiles already read nested messages into the existing member, so this is
    # a regression guard rather than a fix.
    # Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
    #       a6 01 (seq start id 20) 16 07 (empty seq id 2) 07 (seq end)
    echo "==> [$label] re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
    printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/reopen_struct.bin") \
        || { echo "FAIL: [$label] re-opened struct must decode"; exit 1; }
    echo "$OUT" | grep -q '"nestedstring":"x"' || { echo "FAIL: [$label] re-opened struct must retain nestedstring \"x\"; got: $OUT"; exit 1; }
    echo "==> [$label] struct scope merge OK"

    # Repeated field id, array wrapper (MESSAGE_SPEC S7.4 + S5): an array wrapper IS
    # the array's value, so unlike a struct it is REPLACED whole by a later
    # occurrence rather than merged. somestringarray (id 18) is opened twice: the
    # first opening sets elements 0="a" and 1="b", the second sets only element
    # 0="c". Element 1 MUST NOT survive as "b" -- the _StrSeq / _FixedStrSeq
    # collectors place by element index and never reset, so before generator#175 the
    # second opening merged into the first one's elements.
    # Wire: 96 01 (seq start id 18) 02 0a 61 (string id 0 "a") 0a 0a 62 (string id 1
    #       "b") 07 (seq end) 96 01 (seq start id 18) 02 0a 63 ("c") 07 (seq end)
    echo "==> [$label] re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
    printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/reopen_array.bin") \
        || { echo "FAIL: [$label] re-opened array wrapper must decode"; exit 1; }
    echo "$OUT" | grep -q '"somestringarray":\["c"' || { echo "FAIL: [$label] re-opened array wrapper must start with the second opening's element 0 == \"c\"; got: $OUT"; exit 1; }
    if echo "$OUT" | grep -q '"somestringarray":\["c","b"'; then
        echo "FAIL: [$label] re-opened array wrapper must be replaced, not merged (element \"b\" survived); got: $OUT"; exit 1
    fi
    echo "==> [$label] array wrapper replace OK"

    # The three S7.3/S7.4 vectors below now run for BOTH profiles. corelib-c-cpp
    # exposes wire()/fixType() with the same sofab::Wire/sofab::Fix surface as
    # corelib-cpp (corelib-c-cpp#104), so the generated guard is emitted for the
    # c-cpp path too -- above the array-wrapper clear, which is also what makes the
    # S7.4 interaction rule hold there.

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
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/fixsubtype.bin") \
        || { echo "FAIL: [$label] mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
    echo "$OUT" | grep -q '"somefp64":3.14159265358979' || { echo "FAIL: [$label] skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/fixsubtype_control.bin") \
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
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/skipped_occ_array.bin") \
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
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/skipped_occ_struct.bin") \
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
        if "$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/$v.bin" >/dev/null 2>&1; then
            echo "FAIL: [$label] $v must be INVALID (S7.1) -- neither masked nor kept"; exit 1
        fi
    done
    OUT=$("$WORK/ex-$label/harness/harness" decode myfirstmessage < "$WORK/w_u8_255_ctl.bin") || { echo "FAIL: [$label] in-range control 255 must decode"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '"someu8":255' || { echo "FAIL: [$label] control must keep 255; got: $OUT"; exit 1; }
    echo "==> [$label] declared-width reject OK"

    # Declared ELEMENT width is a validity bound too (MESSAGE_SPEC S1/S7.1,
    # generator#279 / Crucible F-0052). The scalar half is asserted above; this is
    # the array half, which lives inside the corelib because readArray converts the
    # elements itself -- generated code has to ARM the bound or the unbounded decode
    # runs and masks. someuintarray (id 15) declares u32.
    # Wire: 7b (id 15, array-unsigned) 01 (count 1) then the element varint.
    echo "==> [$label] an over-width ARRAY ELEMENT must be INVALID (S7.1, generator#279)"
    if printf '\173\001\200\200\200\200\020' | "$WORK/ex-$label/harness/harness" decode myfirstmessage >/dev/null 2>&1; then
        echo "FAIL: [$label] 2^32 into a u32 array element must be INVALID, not masked and kept"; exit 1
    fi
    # Control: u32 max is in range and must still decode, keeping its exact value.
    OUT=$(printf '\173\001\377\377\377\377\017' | "$WORK/ex-$label/harness/harness" decode myfirstmessage) \
        || { echo "FAIL: [$label] u32 max must decode as an array element"; exit 1; }
    echo "$OUT" | tr -d ' ' | grep -q '4294967295' \
        || { echo "FAIL: [$label] the in-range element must survive exactly; got: $OUT"; exit 1; }
    echo "==> [$label] element-width reject OK"

    echo "==> [$label] shared-vector byte-exact conformance"
    ( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-$label.yaml" --lang cpp --in "$WORK/conf.yaml" --out "$WORK/conf-$label" )
    make -C "$WORK/conf-$label" "$@" >/dev/null
    python3 "$ROOT/tests/conformance/cpp/check_vectors.py" "$CC/assets/test_vectors.json" "$WORK/conf-$label/harness/harness"

    echo "==> [$label] corpus + realworld: every definition compiles"
    for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
        # no_maxlen.yaml and seq_elements_dyn.yaml exist to exercise genuinely
        # unbounded fields — unbounded string/blob, and the count-less wrapper
        # arrays that must never be narrowed or refilled. The embedded profile
        # rejects those by design — in both storage modes — so they are not
        # definitions this leg can compile, and skipping them is the honest
        # outcome rather than a bound invented for the test. Their bounded
        # counterparts (seq_elements, nested_rows) do compile here, so this leg
        # keeps its wrapper-element coverage.
        case "$corelib:$(basename "$def")" in
        c-cpp:no_maxlen.yaml | c-cpp:seq_elements_dyn.yaml) continue ;;
        esac
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
run_variant cpp "" "" "-I$CPP/include" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC"
# Same corelib, heap-free storage for every field the schema bounds. Runs the
# unmodified schemas: on this corelib an unbounded field is not an error, it just
# keeps its dynamic container.
run_variant cpp-static "" false "-I$CPP/include" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC"

# C++ wrapper over the C library, corelib-c-cpp (corelib: c-cpp). Only needs
# SOFAB_C_DIR; the generated Makefile compiles + links its C sources.
run_variant c-cpp-dynamic "c-cpp" true "-I$CC/src/include" SOFAB_C_DIR="$CC"
run_variant c-cpp-static  "c-cpp" false "-I$CC/src/include" SOFAB_C_DIR="$CC"

# Receiver-side decode limits (generator#102), pure corelib-cpp only (the c-cpp
# profile is statically schema-bounded). An unbounded array claiming more than
# the configured max_dyn_array_count must fail the decode (LimitExceeded, raised
# inside sofab::readArray, which is handed the cap); the same bytes decode fine
# without a configured limit.
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
"$WORK/nolim102/harness/harness" decode dyn < "$WORK/over102.bin" >/dev/null || { echo "FAIL: [cpp] under the target default the same bytes must decode"; exit 1; }
echo "==> [cpp] decode limits OK (over-cap rejected, in-cap preserves elements, unlimited accepted)"

# The string/blob half of the same rule, and the §7.3 ordering it turns on
# (generator#420). The cap is PASSED INTO readString/readBlob rather than tested
# in front of them, because the MESSAGE_SPEC §7.3 tag test lives inside the call:
# CORELIB_PLAN §6.2.1, "a skipped field is never capped". A guard in front runs
# BEFORE the tag test and so caps exactly the field it was required to skip --
# which is what this backend emitted until #420, and what the last case here
# pins. A unit test cannot see the difference: a removed guard with nothing
# replacing it reads the same in a diff, so the first two cases run the binary.
echo "==> [cpp] receiver caps ride into the read, behind the §7.3 tag test (generator#420)"
# The array field is load-bearing, and not as a subject: it is what makes the
# derived reassembly cap (SOFAB_MAX_DYN_BUFFERED_FIELD, #228) wide enough that
# these images reach readString at all. Sized off the string/blob caps alone it
# comes out at cap + 2 bytes, and an over-cap field always spans cap + 3 -- so
# every case below would be rejected by the byte budget before the per-field cap
# was ever consulted, and the test would pass while proving nothing.
cat > "$WORK/dyn420.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      s: { id: 0, type: string }
      b: { id: 1, type: blob }
      a: { id: 2, type: array, items: { type: u64 } }
YAML
cat > "$WORK/cfg-420.yaml" <<'YAML'
generic: { emit: project, max_dyn_string_len: 8, max_dyn_blob_len: 8, max_dyn_array_count: 4 }
targets: { cpp: { namespace: sofabuffers } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-420.yaml" --lang cpp --in "$WORK/dyn420.yaml" --out "$WORK/lim420" )
make -C "$WORK/lim420" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
# No cap may be tested in generated code in front of a read; the whole point of
# #420 is that the corelib gets handed the number instead.
grep -q 'is.exceedLimit()' "$WORK/lim420/dyn.hpp" && {
    echo "FAIL: [cpp] a cap is checked in front of the read (§6.2.1, generator#420)"; exit 1; }
grep -q 'sofab::readString(is, s, -1, SOFAB_MAX_DYN_STRING_LEN);' "$WORK/lim420/dyn.hpp" || {
    echo "FAIL: [cpp] the string cap must be an argument to readString"; exit 1; }
# Header byte `id << 3 | wire`, wire 2 = Fixlen; then the length word
# `len << 3 | subtype`, subtype 2 = String, 3 = Blob (MESSAGE_SPEC §4).
printf '\002\112123456789' > "$WORK/over420.bin"  # s: 9 bytes > cap 8  -> reject
printf '\002\102' > "$WORK/in420.bin"             # s: 8 bytes == cap   -> accept
printf '12345678' >> "$WORK/in420.bin"
printf '\012\113AAAAAAAAA' > "$WORK/overb420.bin"  # b (id 1): 9 bytes > cap 8 -> reject
printf '\002\113AAAAAAAAA' > "$WORK/skip420.bin"  # a 9-byte BLOB at the string id
if "$WORK/lim420/harness/harness" decode dyn < "$WORK/over420.bin" >/dev/null 2>&1; then
    echo "FAIL: [cpp] over-cap unbounded string (9 > max_dyn_string_len 8) must be rejected"; exit 1
fi
if "$WORK/lim420/harness/harness" decode dyn < "$WORK/overb420.bin" >/dev/null 2>&1; then
    echo "FAIL: [cpp] over-cap unbounded blob (9 > max_dyn_blob_len 8) must be rejected"; exit 1
fi
DEC=$("$WORK/lim420/harness/harness" decode dyn < "$WORK/in420.bin") || {
    echo "FAIL: [cpp] at-cap string must decode -- a cap rejects, it never truncates"; exit 1; }
echo "$DEC" | grep -q '"s":"12345678"' || {
    echo "FAIL: [cpp] at-cap string lost its bytes; got: $DEC"; exit 1; }
# The one the pre-guard got wrong. A blob at a string-declared id contradicts the
# schema, so §7.3 skips it -- and a skipped field is never capped, however long
# it claims to be. The decode stays COMPLETE and `s` keeps its default.
DEC=$("$WORK/lim420/harness/harness" decode dyn < "$WORK/skip420.bin") || {
    echo "FAIL: [cpp] an over-cap BLOB at a string id must be SKIPPED, not capped (§6.2.1/§7.3)"; exit 1; }
echo "$DEC" | grep -q '"s":""' || {
    echo "FAIL: [cpp] the skipped field must bind nothing; got: $DEC"; exit 1; }
echo "==> [cpp] string/blob caps OK (over-cap rejected, at-cap intact, mis-typed skipped)"

# The WRAPPER-array half (generator#402 item 3, CORELIB_PLAN §6.2.1), and the
# only measurement that settles it: a nine-byte message must not be able to
# allocate.
#
# A wrapper array carries no count header -- its length is *highest present id +
# 1* (MESSAGE_SPEC §5.1) -- so the element INDEX is the array's length, and one
# element at a large id forces an arbitrarily large allocation out of nothing.
# §6.2.1 names the index for exactly that reason and puts the check "before the
# container it indexes into is extended". Before this cap was stated the images
# below decoded to **Complete** while allocating 134 MB, 100 MB and 100 MB.
#
# The verdict alone would not prove the fix, so the probe counts every global
# operator new and reads the counter around each decode: the assertion is that
# the allocation NEVER HAPPENED, not merely that an error came back afterwards.
# It also pins the two things the cap must NOT do -- reach a schema-bounded array
# (its own `count:` governs, INVALID) and reject anything under the cap.
echo "==> [cpp] a wrapper array's element index is capped, and nothing is allocated (generator#402)"
cat > "$WORK/dyn402.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      w: { id: 0, type: array, items: { type: string } }
      p: { id: 1, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }
      n: { id: 2, type: array, items: { type: array, items: { type: string } } }
      b: { id: 3, type: array, items: { type: string, count: 4 } }
YAML
cat > "$WORK/cfg-402.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 64, max_dyn_string_len: 32, max_dyn_blob_len: 32 }
targets: { cpp: { namespace: sofabuffers } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-402.yaml" --lang cpp --in "$WORK/dyn402.yaml" --out "$WORK/lim402" )
cat > "$WORK/probe402.cpp" <<'CPP'
#include <cstdio>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <new>
#include <string>
#include <vector>

#include "dyn.hpp"

static std::size_t g_bytes = 0;

void *operator new(std::size_t n)
{
    g_bytes += n;
    void *p = std::malloc(n ? n : 1);
    if (!p) throw std::bad_alloc();
    return p;
}
void *operator new[](std::size_t n) { return operator new(n); }
void operator delete(void *p) noexcept { std::free(p); }
void operator delete[](void *p) noexcept { std::free(p); }
void operator delete(void *p, std::size_t) noexcept { std::free(p); }
void operator delete[](void *p, std::size_t) noexcept { std::free(p); }

static void putVarint(std::vector<std::uint8_t> &out, std::uint64_t v)
{
    while (v >= 0x80) { out.push_back(static_cast<std::uint8_t>((v & 0x7f) | 0x80)); v >>= 7; }
    out.push_back(static_cast<std::uint8_t>(v));
}

/* (id << 3) | wire; wire 2 = Fixlen, 6 = SequenceStart, 7 = SequenceEnd (§4.3, §4.9). */
static void hdr(std::vector<std::uint8_t> &out, std::uint64_t id, unsigned wire)
{
    putVarint(out, (id << 3) | wire);
}

static int failures = 0;

static const char *name(sofab::DecodeStatus st)
{
    switch (st)
    {
        case sofab::DecodeStatus::Complete:        return "Complete";
        case sofab::DecodeStatus::Incomplete:      return "Incomplete";
        case sofab::DecodeStatus::Invalid:         return "Invalid";
        case sofab::DecodeStatus::LimitExceeded:   return "LimitExceeded";
        case sofab::DecodeStatus::InvalidArgument: return "InvalidArgument";
    }
    return "?";
}

/* budget: the decode of a REJECTED image must allocate essentially nothing. It
 * is not zero-tolerant on purpose -- what is being excluded is the wire-sized
 * allocation, which is four orders of magnitude above this. */
static void run(const char *what, const std::vector<std::uint8_t> &wire, const char *want)
{
    const std::size_t budget = 65536;
    sofabuffers::Dyn out;
    /* Warm once so the measured decode counts wire-driven growth, not first touch. */
    (void)sofabuffers::Dyn::try_decode(wire.data(), wire.size(), out);
    out = sofabuffers::Dyn{};
    const std::size_t before = g_bytes;
    auto r = sofabuffers::Dyn::try_decode(wire.data(), wire.size(), out);
    const std::size_t used = g_bytes - before;
    std::printf("    %-34s verdict=%-15s allocated=%zu bytes\n", what, name(r.status()), used);
    if (std::strcmp(name(r.status()), want) != 0)
    {
        std::printf("FAIL: [cpp] %s: expected %s, got %s\n", what, want, name(r.status()));
        ++failures;
    }
    if (used > budget)
    {
        std::printf("FAIL: [cpp] %s: allocated %zu bytes -- the allocation the cap exists to prevent HAPPENED\n",
                    what, used);
        ++failures;
    }
}

int main()
{
    const std::uint64_t big = 2000000; /* ~64 MB of std::string, asked for in 8 bytes */

    std::vector<std::uint8_t> w;                  /* array<string>, schema-unbounded */
    hdr(w, 0, 6);
    hdr(w, big, 2);
    putVarint(w, (1 << 3) | 2);                   /* fixlen word: 1 byte, subtype String */
    w.push_back('A');
    hdr(w, 0, 7);
    std::printf("    attack message: %zu bytes, one element at index %llu\n",
                w.size(), static_cast<unsigned long long>(big));
    run("array<string> over-index", w, "LimitExceeded");

    std::vector<std::uint8_t> p;                  /* array<struct>: the object collector */
    hdr(p, 1, 6); hdr(p, big, 6); hdr(p, 0, 7); hdr(p, 0, 7);
    run("array<struct> over-index", p, "LimitExceeded");

    std::vector<std::uint8_t> n;                  /* array<array<string>>: the generated placer */
    hdr(n, 2, 6); hdr(n, big, 6); hdr(n, 0, 7); hdr(n, 0, 7);
    run("array<array<string>> over-index", n, "LimitExceeded");

    /* The schema bounds this one, so the cap must not touch it: its own `count: 4`
     * governs and an over-index element is INVALID (MESSAGE_SPEC §7.1). */
    std::vector<std::uint8_t> b;
    hdr(b, 3, 6); hdr(b, big, 2); putVarint(b, (1 << 3) | 2); b.push_back('A'); hdr(b, 0, 7);
    run("bounded array<string> over-index", b, "Invalid");

    /* The element LENGTH cap, the collector's second axis: one element longer
     * than max_dyn_string_len, at a perfectly ordinary index. */
    std::vector<std::uint8_t> l;
    hdr(l, 0, 6); hdr(l, 0, 2); putVarint(l, (64 << 3) | 2);
    for (int i = 0; i < 64; ++i) l.push_back('A');
    hdr(l, 0, 7);
    run("array<string> over-long element", l, "LimitExceeded");

    /* The control: a sparse array under the caps decodes intact, at its wire
     * length -- highest present id + 1 -- and a cap never truncates. */
    std::vector<std::uint8_t> ok;
    hdr(ok, 0, 6); hdr(ok, 3, 2); putVarint(ok, (2 << 3) | 2);
    ok.push_back('h'); ok.push_back('i'); hdr(ok, 0, 7);
    sofabuffers::Dyn good;
    auto r = sofabuffers::Dyn::try_decode(ok.data(), ok.size(), good);
    if (!r.ok() || good.w.size() != 4 || good.w[3] != "hi")
    {
        std::printf("FAIL: [cpp] an in-cap sparse wrapper array must decode intact (len=%zu)\n", good.w.size());
        ++failures;
    }
    else
        std::printf("    %-34s verdict=Complete        len=%zu, last=\"%s\"\n",
                    "in-cap control", good.w.size(), good.w[3].c_str());

    return failures ? 1 : 0;
}
CPP
g++ -std=c++20 -O2 -Wall -I"$WORK/lim402" -I"$CPP/include" "$WORK/probe402.cpp" -o "$WORK/probe402"
"$WORK/probe402" || { echo "FAIL: [cpp] wrapper-array receiver caps (generator#402, §6.2.1)"; exit 1; }
echo "==> [cpp] wrapper index + element caps OK (rejected before the allocation, bounded array untouched)"

# The derived reassembly cap is a BYTE budget (generator#228). It used to be
# derived from element COUNTS -- a different dimension -- so the corelib's
# exceedsBuffer rejected messages the per-field guards accept, through the
# generated try_decode, where a bare feed accepted them. Both shapes below are
# valid input that MUST decode; the amplification control pins that the cap still
# bites on input that is genuinely oversized.
echo "==> [cpp] reassembly cap is a byte budget, not a count (generator#228)"
cat > "$WORK/cfg-lim228.yaml" <<'YAML'
generic: { emit: project, max_dyn_array_count: 8 }
targets: { cpp: { namespace: sofabuffers } }
YAML
cat > "$WORK/dyn228.yaml" <<'YAML'
version: 1
messages:
  m: { payload: { a: { id: 0, type: array, items: { type: u32 } } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-lim228.yaml" --lang cpp --in "$WORK/dyn228.yaml" --out "$WORK/lim228" )
make -C "$WORK/lim228" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
# The issue's vector: an array of exactly 8 elements, at max_dyn_array_count 8.
# 10 bytes on the wire -- above the count 8, which is why a count-derived cap
# rejected it. 03 (id 0, unsigned array) 08 (count 8) + 8 one-byte elements.
printf '\003\010\000\001\002\003\004\005\006\007' > "$WORK/atcap228.bin"
ST=$("$WORK/lim228/harness/harness" status m < "$WORK/atcap228.bin" | head -n1)
[ "$ST" = "COMPLETE" ] || { echo "FAIL: [cpp] an at-cap array (8 == max_dyn_array_count) -> $ST (want COMPLETE)"; exit 1; }
DEC=$("$WORK/lim228/harness/harness" decode m < "$WORK/atcap228.bin") || { echo "FAIL: [cpp] at-cap array must decode"; exit 1; }
echo "$DEC" | grep -q '"a":\[0,1,2,3,4,5,6,7\]' || { echo "FAIL: [cpp] at-cap array lost its elements; got: $DEC"; exit 1; }
# Control: one element past the cap is still LimitExceeded (the count guard).
printf '\003\011\000\001\002\003\004\005\006\007\010' > "$WORK/overcap228.bin"
ST=$("$WORK/lim228/harness/harness" status m < "$WORK/overcap228.bin" | head -n1)
[ "$ST" = "LIMIT_EXCEEDED" ] || { echo "FAIL: [cpp] over-cap array (9 > 8) -> $ST (want LIMIT_EXCEEDED)"; exit 1; }
# The sharper shape: a FULLY schema-bounded wrapper array (5 string elements of
# maxlen 16, 97 bytes on the wire) under a config that only caps the unbounded
# string. Nothing here is dynamic, yet a count-derived cap (5 * 10 = 50) rejected
# it -- the exact opposite of the "legitimately schema-bounded fields always still
# fit" property the cap exists to preserve.
cat > "$WORK/cfg-lim228b.yaml" <<'YAML'
generic: { emit: project, max_dyn_string_len: 16 }
targets: { cpp: { namespace: sofabuffers } }
YAML
cat > "$WORK/bnd228.yaml" <<'YAML'
version: 1
messages:
  m:
    payload:
      sa: { id: 0, type: array, items: { type: string, count: 5, maxlen: 16 } }
      s:  { id: 1, type: string }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-lim228b.yaml" --lang cpp --in "$WORK/bnd228.yaml" --out "$WORK/lim228b" )
make -C "$WORK/lim228b" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
python3 -c "
import sys
def varint(x):
    out = bytearray()
    while True:
        b = x & 0x7f; x >>= 7
        out.append(b | 0x80 if x else b)
        if not x: return bytes(out)
out = bytearray([0x06])                                  # sequence start, id 0
for i in range(5):                                       # element id IS its index
    out += varint((i << 3) | 2) + varint((16 << 3) | 2) + b'A' * 16
out += bytes([0x07])                                     # sequence end
sys.stdout.buffer.write(bytes(out))" > "$WORK/bnd228.bin"
ST=$("$WORK/lim228b/harness/harness" status m < "$WORK/bnd228.bin" | head -n1)
[ "$ST" = "COMPLETE" ] || { echo "FAIL: [cpp] a fully schema-bounded string array -> $ST (want COMPLETE)"; exit 1; }
# Amplification control: a fixlen field claiming far more than any legitimate
# field span is still stopped at the length word, before the bytes are buffered.
python3 -c "
import sys
def varint(x):
    out = bytearray()
    while True:
        b = x & 0x7f; x >>= 7
        out.append(b | 0x80 if x else b)
        if not x: return bytes(out)
sys.stdout.buffer.write(bytes([0x0a]) + varint((1000000 << 3) | 2) + b'A' * 4)" > "$WORK/amp228.bin"
ST=$("$WORK/lim228b/harness/harness" status m < "$WORK/amp228.bin" | head -n1)
[ "$ST" = "LIMIT_EXCEEDED" ] || { echo "FAIL: [cpp] a 1 MB fixlen claim -> $ST (want LIMIT_EXCEEDED)"; exit 1; }
echo "==> [cpp] byte-dimensioned reassembly cap OK"

# generator#229 verbatim reproducer: a NESTED maxlen-4 blob (so the bound lives in
# a child SeqNode, the descend-into-child measure path) carrying an fp64 value. The
# subtype contradicts the declared blob, so S7.3 skips the field -- but the
# measure-phase schema used to maxlen-check any Fixlen at the id and reject the
# 8-byte payload as INVALID. Pure corelib-cpp only (no measure phase in c-cpp).
echo "==> [cpp] nested maxlen-4 blob vs a contradicting fp64 (generator#229)"
cat > "$WORK/probe229.yaml" <<'YAML'
version: 1
messages:
  probe:
    payload:
      nested:
        id: 10
        type: struct
        fields:
          bytes_field: { id: 3, type: blob, maxlen: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg-nolimits.yaml" --lang cpp --in "$WORK/probe229.yaml" --out "$WORK/probe229" )
make -C "$WORK/probe229" SOFAB_CPP_DIR="$CPP" SOFAB_C_DIR="$CC" >/dev/null
# 56 (seq start id 10) 1a (fixlen id 3) 41 (fixlen word: len 8, FP64 subtype 1)
# + the 8 bytes of 1.5 + 07 (seq end).
printf '\126\032\101\000\000\000\000\000\000\370\077\007' > "$WORK/f229_fp64.bin"
DEC=$("$WORK/probe229/harness/harness" decode probe < "$WORK/f229_fp64.bin") \
    || { echo "FAIL: [cpp] an fp64 at a maxlen-4 blob id must skip, not reject (generator#229)"; exit 1; }
echo "$DEC" | grep -q '"bytes_field":\[\]' || { echo "FAIL: [cpp] the skipped fp64 must leave bytes_field at its default; got: $DEC"; exit 1; }
# Control: the same 8-byte payload with the MATCHING blob subtype (43 = len 8,
# BLOB subtype 3) is genuinely over maxlen 4 -> INVALID; the bound still bites.
printf '\126\032\103\000\000\000\000\000\000\370\077\007' > "$WORK/f229_blob8.bin"
ST=$("$WORK/probe229/harness/harness" status probe < "$WORK/f229_blob8.bin" | head -n1)
[ "$ST" = "INVALID" ] || { echo "FAIL: [cpp] an 8-byte BLOB at maxlen 4 -> $ST (want INVALID)"; exit 1; }
# Control: over-bound AND truncated with the matching subtype still resolves to
# INVALID, i.e. the gate did not weaken the S5.2 anti-folding order.
ST=$(printf '\126\032\103\000\000' | "$WORK/probe229/harness/harness" status probe | head -n1)
[ "$ST" = "INVALID" ] || { echo "FAIL: [cpp] over-maxlen(8>4)+truncated -> $ST (want INVALID)"; exit 1; }
# Control: an in-bound blob (23 = len 4, BLOB subtype 3) decodes to its bytes.
printf '\126\032\043\001\002\003\004\007' > "$WORK/f229_blob4.bin"
DEC=$("$WORK/probe229/harness/harness" decode probe < "$WORK/f229_blob4.bin") \
    || { echo "FAIL: [cpp] an in-bound blob must decode"; exit 1; }
echo "$DEC" | grep -q '"bytes_field":\[1,2,3,4\]' || { echo "FAIL: [cpp] in-bound blob lost its bytes; got: $DEC"; exit 1; }
echo "==> [cpp] nested subtype-gated bound OK"

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
messages: { m: { payload: { a: {id: 0, type: i32}, s: {id: 1, type: string, maxlen: 16}, st: {id: 2, type: struct, fields: {x: {id: 0, type: i32}}}, sa: {id: 3, type: array, items: {type: string, count: 3, maxlen: 16}} } } }'
subset_cpp nofp64 ok "-DSOFAB_DISABLE_FP64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: i32}, f: {id: 1, type: fp32}, s: {id: 2, type: string, maxlen: 16}, arr: {id: 3, type: array, items: {type: u8, count: 4}} } } }'
subset_cpp noint64 ok "-DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u32}, b: {id: 1, type: i32}, f: {id: 2, type: fp32}, s: {id: 3, type: string, maxlen: 16}, st: {id: 4, type: struct, fields: {x: {id: 0, type: i32}}} } } }'
subset_cpp stripped ok "-DSOFAB_DISABLE_ARRAY_SUPPORT -DSOFAB_DISABLE_FP64_SUPPORT -DSOFAB_DISABLE_INT64_SUPPORT" \
    'version: 1
messages: { m: { payload: { a: {id: 0, type: u8}, b: {id: 1, type: i16}, c: {id: 2, type: i32}, s: {id: 3, type: string, maxlen: 16}, bl: {id: 4, type: blob, maxlen: 8}, st: {id: 5, type: struct, fields: {x: {id: 0, type: i32}}}, sa: {id: 6, type: array, items: {type: string, count: 3, maxlen: 16}} } } }'
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
