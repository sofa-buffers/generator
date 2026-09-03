#!/usr/bin/env sh
# Reproducible Python conformance harness: generate -> syntax-check ->
# round-trip -> byte-exact shared-vector conformance against corelib-py.
#
# Usage: tests/conformance/python/run.sh [path-to-corelib-py]   (or set $SOFAB_PY_CORELIB)
# Requires: go, python3, git.
set -eu

# Corelib checkout + ref pinning (docs/CI.md).
. "$(dirname "$0")/../lib/corelib.sh"
# Shared MAX_SIZE fill check (ARCHITECTURE §9.6).
. "$(dirname "$0")/../lib/maxsize_fill.sh"

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CORELIB="${1:-${SOFAB_PY_CORELIB:-}}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

if [ -z "$CORELIB" ]; then
    echo "==> cloning corelib-py"
    clone_corelib corelib-py "$WORK/corelib"
    CORELIB="$WORK/corelib"
fi
echo "==> corelib-py: $CORELIB"
export PYTHONPATH="$CORELIB/src"

# corelib-py ships TWO engines: the pure-Python classes and an optional Cython
# accelerator (`sofab._speedups`). `sofab/__init__.py` imports the accelerator
# when it is importable and falls back to the pure classes otherwise -- silently,
# by design -- and `setup.py` marks the extension `optional=True`, so a compile it
# cannot do is not a failure exit either. A suite that only clones the corelib
# therefore never has an accelerator to import: it runs the pure engine on every
# leg, including the two below that print an engine name (generator#451).
#
# So the accelerator is built here, and it is the IMPORT that is verified, never
# the build's exit status.
#
# (corelib-py's own `SOFAB_REQUIRE_ENGINE` guard is a fixture in ITS pytest suite;
# nothing in a generated harness reads it, so the assertion has to live here.)

# engine_is <native|python> -- true when `import sofab` right now uses that engine.
# For `native`, `IMPL` alone is not enough: the exported Encoder/Decoder must BE
# the accelerator's, which is what a native leg actually exercises.
engine_is() {
    python3 -c '
import sys
import sofab
want = sys.argv[1]
if sofab.IMPL != want:
    sys.exit(1)
if want == "native":
    from sofab import _speedups
    sys.exit(0 if sofab.Encoder is _speedups.Encoder and sofab.Decoder is _speedups.Decoder else 1)
' "$1" 2>/dev/null
}

# active_engine -- the engine name for a message, never for a decision.
active_engine() { python3 -c 'import sofab; print(sofab.IMPL)' 2>/dev/null || echo "<import failed>"; }

# require_engine <native|python> -- the same check, but a mismatch ends the run.
require_engine() {
    engine_is "$1" && return 0
    echo "FAIL: this leg must run on the '$1' engine, but sofab.IMPL is '$(active_engine)'." >&2
    echo "      corelib-py falls back to pure Python whenever sofab._speedups cannot be" >&2
    echo "      imported, which would make this a second pure leg (generator#451)." >&2
    exit 1
}

echo "==> building corelib-py's native accelerator (sofab._speedups)"
unset SOFAB_PUREPYTHON || true
NATIVE=yes
( cd "$CORELIB" && python3 setup.py build_ext --inplace ) > "$WORK/build_ext.log" 2>&1 || NATIVE=no
[ "$NATIVE" = no ] || engine_is native || NATIVE=no
if [ "$NATIVE" = yes ]; then
    echo "==> native accelerator built (sofab.IMPL = native)"
else
    echo "--- last 20 lines of $WORK/build_ext.log ---" >&2
    tail -n 20 "$WORK/build_ext.log" >&2 || true
    echo "-------------------------------------------" >&2
    # The build needs Cython (the generated .c is not committed), setuptools, a C
    # compiler and the CPython headers. Naming the missing one beats a wall of
    # compiler output.
    for MOD in Cython setuptools; do
        python3 -c "import $MOD" 2>/dev/null || echo "    missing build dependency: $MOD (pip install $MOD)" >&2
    done
    if [ "${SOFAB_PY_ALLOW_PURE_ONLY:-}" = 1 ]; then
        echo "!!! SKIP: corelib-py's native accelerator is unavailable here, and" >&2
        echo "!!!       SOFAB_PY_ALLOW_PURE_ONLY=1 was set: this run covers the PURE" >&2
        echo "!!!       engine only and proves nothing about the native encoder/decoder." >&2
    else
        echo "FAIL: could not build corelib-py's native accelerator, so this run would" >&2
        echo "      exercise the pure engine twice and report it as two engines." >&2
        echo "      Install the build dependencies above, or set SOFAB_PY_ALLOW_PURE_ONLY=1" >&2
        echo "      to accept the reduced coverage explicitly." >&2
        exit 1
    fi
fi

cat > "$WORK/cfg.yaml" <<YAML
generic: { emit: project }
targets: { python: {} }
YAML

echo "==> generating Python project"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in examples/messages/example.yaml --out "$WORK/proj" )

echo "==> syntax check"
python3 -m py_compile "$WORK/proj/message.py" "$WORK/proj/harness.py"

echo "==> JSON encode -> decode round-trip"
IN='{"somei8":-5,"somebool":true,"somestring":"hi","someintarray":[1,2,3,4,5],"someuintarray":[1,2,3,4],"somefloatarray":[1.5,2.5,3.5],"someenum":33,"somebitfield":2,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someunion":{"option1":4242},"somefp32":2.5,"someblob":[10,20,30],"someu64":18446744073709551615,"somestringarray":["a","b","c"]}'
OUT=$(cd "$WORK/proj" && printf '%s' "$IN" | python3 harness.py encode myfirstmessage | python3 harness.py decode myfirstmessage)
echo "$OUT" | grep -q '"someu64": 18446744073709551615' || { echo "FAIL: u64 round-trip"; exit 1; }
echo "$OUT" | grep -q '"deepint": -99' || { echo "FAIL: nested struct round-trip"; exit 1; }
echo "==> round-trip OK"

# The two encode-buffer arms (CORELIB_PLAN §5.1). The caller owns the output
# buffer: generated code allocates it and the corelib neither grows nor
# reallocates it. Which shape applies is a property of the SCHEMA, and
# example.yaml is unbounded — it exercises the scratch+sink arm above — so
# without this schema the BOUNDED arm is never executed at all.
#
# The fill message pins the size from both sides: filling every field to its
# declared bound must encode to exactly MAX_SIZE bytes, so the buffer can be
# neither short (a legal message would not fit) nor slack (RAM paid for nothing).
#
# Both engines run every leg. corelib-py's pure-Python and native accelerators
# carry independent over_buffer/_put/_drain implementations, so a leg that only
# runs the default engine proves nothing about the other — and the native
# over_buffer is typed `bytearray`, a mismatch the pure one would accept.
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in "$ROOT/tests/conformance/lib/maxsize_fill.yaml" --out "$WORK/fill" )
OVERFILL="$WORK/overfill.json"
sed 's/"f_str": *"[^"]*"/"f_str": "'"$(printf 'x%.0s' $(seq 1 400))"'"/' \
    "$ROOT/tests/conformance/lib/maxsize_fill.json" > "$OVERFILL"
grep -q 'xxxxxxxxxx' "$OVERFILL" || { echo "FAIL: could not build the over-filled input (f_str renamed?)"; exit 1; }

ENGINES="native python"
[ "$NATIVE" = yes ] || ENGINES=python
for ENGINE in $ENGINES; do
    if [ "$ENGINE" = python ]; then export SOFAB_PUREPYTHON=1; else unset SOFAB_PUREPYTHON || true; fi
    # A fallback to the pure engine where the native one was expected would make
    # the second pass a duplicate of the first, silently -- so the leg ASSERTS the
    # engine it claims instead of printing whichever one it got.
    require_engine "$ENGINE"
    echo "==> bounded encode buffer is exactly MAX_SIZE, engine=$ENGINE (ARCHITECTURE §9.6)"
    check_maxsize_fill "python/$ENGINE" python3 "$WORK/fill/harness.py" encode fill

    # ...and the other side of owning the buffer: a value the caller filled PAST
    # its own schema bound does not fit, and §5.1 forbids returning partial output
    # as if it were complete. So the encode must FAIL and write nothing — the old
    # corelib-grown buffer silently emitted an over-bound message that every
    # receiver would then reject as INVALID.
    echo "==> an over-filled bounded value must be refused, not truncated (§5.1)"
    if python3 "$WORK/fill/harness.py" encode fill < "$OVERFILL" > "$WORK/overfill.bin" 2>/dev/null; then
        echo "FAIL: [$ENGINE] a string 400 bytes into a maxlen-9 field must be reported, not encoded"; exit 1
    fi
    [ ! -s "$WORK/overfill.bin" ] || {
        echo "FAIL: [$ENGINE] a refused encode emitted $(wc -c < "$WORK/overfill.bin") bytes of partial output"; exit 1
    }
    echo "   [$ENGINE] over-fill refusal OK"
done

# Everything below runs on ONE engine -- the shared-vector byte-exactness check
# included -- and that engine is the native one wherever it exists: it is what a
# user with a compiler gets, and the pure engine has just been through every leg
# of the loop above.
unset SOFAB_PUREPYTHON || true
if [ "$NATIVE" = yes ]; then require_engine native; else require_engine python; fi
echo "==> remaining legs run on the $(active_engine) engine"

# Over-count scalar array (generator#100): someuintarray declares count: 4
# (id 15 -> header 0x7b = 15<<3 | unsigned-array). 5 wire elements MUST be
# INVALID per MESSAGE_SPEC 3+7 (decode exits non-zero); exactly 4 still decode.
echo "==> over-count scalar array must reject (generator#100)"
printf '\173\005\001\002\003\004\005' > "$WORK/overcount.bin"
printf '\173\004\001\002\003\004' > "$WORK/control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overcount.bin" >/dev/null 2>&1; then
    echo "FAIL: over-count scalar array (5 > count 4) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/control.bin" >/dev/null || { echo "FAIL: control (count == 4) must decode"; exit 1; }
echo "==> over-count reject OK"

# Over-count AND truncated: INVALID dominates INCOMPLETE (generator#216 / F-0032,
# MESSAGE_SPEC S5.2). A count header of 6 (> 4) followed by only 2 elements then EOF
# is BOTH over-count and truncated; the count is on the delivered field header
# (fld.count) before any element, so it MUST be reported INVALID (SofaDecodeError),
# not INCOMPLETE (SofaIncompleteError). Wire: 7b (id 15 unsigned-array) 06 01 02 EOF.
echo "==> over-count + truncation must be INVALID, not INCOMPLETE (generator#216)"
printf '\173\006\001\002' > "$WORK/overcount_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overcount_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaDecodeError' || { echo "FAIL: over-count(6>4)+truncated must be INVALID (SofaDecodeError); got: $ERR"; exit 1; }
# Precision control: an in-bound count (4 == bound) that is genuinely truncated
# (2 of 4 elements then EOF) is a clean truncation and MUST stay INCOMPLETE.
printf '\173\004\001\002' > "$WORK/incount_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/incount_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaIncompleteError' || { echo "FAIL: in-bound(4==4)+truncated must be INCOMPLETE (SofaIncompleteError); got: $ERR"; exit 1; }
echo "==> over-count/truncation ordering OK"

# Over-index wrapper array (generator#142): somestringarray declares count: 5
# (id 18). A string element with a wire index >= 5 is INVALID for every target
# (MESSAGE_SPEC S5.1/S7), never grown-into -- which also bounds an over-index
# heap-amplification DoS. Wire: 96 01 (sequence_begin id 18) 2a (string id 5,
# over-index) 0a 78 (fixlen "x") 07 (sequence_end); control puts it at id 4.
echo "==> over-index wrapper array must reject (generator#142)"
printf '\226\001\052\012\170\007' > "$WORK/overindex.bin"
printf '\226\001\042\012\170\007' > "$WORK/overindex_control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overindex.bin" >/dev/null 2>&1; then
    echo "FAIL: over-index wrapper element (id 5 >= count 5) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overindex_control.bin" >/dev/null || { echo "FAIL: control (index 4 < 5) must decode"; exit 1; }
echo "==> over-index reject OK"

# Over-count NESTED NATIVE row (MESSAGE_SPEC S3+S7.1): a row of a nested native
# array declares its own `count`, and a row whose wire element count exceeds that
# capacity is INVALID exactly like a top-level native array -- the bound has to be
# taken at the ROW's count header, which is the row element's header inside the
# wrapper. example.yaml has no nested row, so this uses its own definition.
# Wire: 06 (sequence_begin id 0) 03 (row id 0, unsigned array) N <elements> 07.
echo "==> over-count nested native row must reject (MESSAGE_SPEC S3+S7.1)"
cat > "$WORK/rows-def.yaml" <<YAML
version: 1
messages:
  rows:
    payload:
      a: { id: 0, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in "$WORK/rows-def.yaml" --out "$WORK/rowsproj" )
printf '\006\003\004\001\002\003\004\007' > "$WORK/row-over.bin"
printf '\006\003\003\001\002\003\007' > "$WORK/row-ok.bin"
if (cd "$WORK/rowsproj" && python3 harness.py decode rows) < "$WORK/row-over.bin" >/dev/null 2>&1; then
    echo "FAIL: nested native row of 4 (> inner count 3) must be INVALID"; exit 1
fi
(cd "$WORK/rowsproj" && python3 harness.py decode rows) < "$WORK/row-ok.bin" >/dev/null || { echo "FAIL: control (row of 3 == count 3) must decode"; exit 1; }
# Over-count AND truncated: INVALID dominates INCOMPLETE (S5.2) -- the count is
# known from the row header before a single element is consumed.
printf '\006\003\004\001' > "$WORK/row-over-trunc.bin"
ERR=$( (cd "$WORK/rowsproj" && python3 harness.py decode rows) < "$WORK/row-over-trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaDecodeError' || { echo "FAIL: over-count(4>3)+truncated row must be INVALID (SofaDecodeError); got: $ERR"; exit 1; }
echo "==> nested-row over-count reject OK"

# Over-maxlen scalar blob (Option B / MESSAGE_SPEC S7.1): someblob (id 12) declares
# maxlen: 16. A 17-byte blob exceeds it -> INVALID, never truncated. Wire: 62 (blob
# id12) 8b 01 (fixlen word len 17, blob subtype 3) + 17 bytes; control is 16 bytes.
echo "==> over-maxlen string/blob must reject (Option B, S7.1)"
printf '\142\213\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen.bin"
printf '\142\203\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001\001' > "$WORK/overmaxlen_control.bin"
if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen.bin" >/dev/null 2>&1; then
    echo "FAIL: over-maxlen blob (17 > maxlen 16) must be INVALID"; exit 1
fi
(cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen_control.bin" >/dev/null || { echo "FAIL: control (16 == maxlen) must decode"; exit 1; }
# ... and the same violation with the message cut RIGHT AFTER the length word
# (generator#267 / Crucible F-0043). 17 > maxlen 16 is fully established by that
# word, and S5.2 makes INVALID dominate INCOMPLETE, so the bound must be measured
# against the peeked wire length BEFORE the payload is read -- reading first and
# measuring the decoded bytes never reaches the check on such a message.
# Wire: 62 (blob id 12) 8b 01 (len 17, subtype blob) <EOF>
echo "==> over-maxlen + truncation must be INVALID, not INCOMPLETE (generator#267)"
printf '\142\213\001' > "$WORK/overmaxlen_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overmaxlen_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaDecodeError' \
    || { echo "FAIL: over-maxlen(17>16)+truncated must be INVALID (SofaDecodeError); got: $ERR"; exit 1; }
# Precision control: an in-bound length cut at the same offset stays INCOMPLETE.
printf '\142\203\001' > "$WORK/inmaxlen_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/inmaxlen_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaIncompleteError' \
    || { echo "FAIL: in-bound(16==16)+truncated must stay INCOMPLETE; got: $ERR"; exit 1; }
echo "==> over-maxlen reject OK"

# The same ordering one level down, at the ELEMENT (generator#267 residue,
# Crucible F-0043 width_elem_trunc). someuintarray (id 15) declares u32 elements;
# an element carrying 2^32 is outside that width, which S7.1 makes INVALID, and it
# is established by its own bytes -- so S5.2 keeps the verdict INVALID however
# little of the array follows.
#
# The declared width is STATED in on_array_begin, which the decoder calls at the
# array header, and applied by it AT each element -- so the verdict does not
# depend on how much of the array followed. A handler cannot do this itself: by
# the time it holds the list, an array that never arrived is indistinguishable
# from one that did.
# Wire: 7b (id 15 unsigned-array) 04 (count 4) 80 80 80 80 10 (2^32) <EOF>.
echo "==> over-width element + truncation must be INVALID (generator#267)"
printf '\173\004\200\200\200\200\020' > "$WORK/overwidth_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/overwidth_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaDecodeError' \
    || { echo "FAIL: over-width element + truncated must be INVALID (SofaDecodeError); got: $ERR"; exit 1; }
# Precision control: an IN-RANGE element cut at the same offset decides nothing,
# so the truncation IS the verdict.
printf '\173\004\001' > "$WORK/inwidth_trunc.bin"
ERR=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/inwidth_trunc.bin" 2>&1 >/dev/null || true )
echo "$ERR" | grep -q 'SofaIncompleteError' \
    || { echo "FAIL: in-range element + truncated must stay INCOMPLETE; got: $ERR"; exit 1; }
echo "==> element-width/truncation ordering OK"

# Contradictory wire type (MESSAGE_SPEC S7.3, generator#174): a field whose header
# wire type is not the one its declared type maps to -- for fixlen, including the
# subtype -- is SKIPPED, exactly like an unknown id. someu8 (id 0) is declared u8
# (unsigned wire type) and keeps its schema default 7. Wire: 01 = id 0 with wire
# type SIGNED (1), then the zig-zag varint 06 (= 3). Reading it as the schema type
# would yield 3 (or the raw 6); skipping leaves the default. Control: 00 09 is the
# same id with the correct unsigned wire type and must decode to 9. A third
# vector, 06 07, gives the same id a SEQUENCE_START header closed by its
# SEQUENCE_END: skipping that one has to drain the whole nested sequence, not
# just a scalar payload, so it exercises the riskiest branch of skip().
echo "==> contradictory wire type must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\001\006' > "$WORK/wiremismatch.bin"
printf '\000\011' > "$WORK/wiremismatch_control.bin"
printf '\006\007' > "$WORK/wiremismatch_seq.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/wiremismatch.bin" ) \
    || { echo "FAIL: mismatched wire type must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8": 7' || { echo "FAIL: skipped field must keep its default 7, got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/wiremismatch_control.bin" ) \
    || { echo "FAIL: control (correct wire type) must decode"; exit 1; }
echo "$OUT" | grep -q '"someu8": 9' || { echo "FAIL: control must decode to 9, got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/wiremismatch_seq.bin" ) \
    || { echo "FAIL: sequence header on a scalar field must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"someu8": 7' || { echo "FAIL: skipped sequence must keep the default 7, got: $OUT"; exit 1; }
echo "==> wire-type skip OK"

# Repeated field id (MESSAGE_SPEC S7.4, generator#175): last occurrence wins per
# field id. A re-opened sequence CONTINUES its scope, so a struct merges and the
# children an earlier opening set whose ids do not recur are retained. somestruct
# (id 20) is opened twice: the first opening sets nestedstring (id 1) to "x", the
# second opens only the empty nestedstruct (id 2). nestedstring MUST survive --
# decoding the re-opening into a fresh object would reset it to "Nested".
# Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
#       a6 01 (seq start id 20) 16 07 (empty seq id 2) 07 (seq end)
echo "==> re-opened struct scope must merge (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\246\001\026\007\007' > "$WORK/reopen_struct.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/reopen_struct.bin" ) \
    || { echo "FAIL: re-opened struct must decode"; exit 1; }
echo "$OUT" | grep -q '"nestedstring": "x"' || { echo "FAIL: re-opened struct must retain nestedstring \"x\", got: $OUT"; exit 1; }
echo "==> struct scope merge OK"

# Repeated field id, array wrapper (MESSAGE_SPEC S7.4 + S5): an array wrapper IS
# the array's value, so unlike a struct it is REPLACED whole by a later occurrence
# rather than merged. somestringarray (id 18) is opened twice: the first opening
# sets elements 0="a" and 1="b", the second sets only element 0="c". Element 1 MUST
# NOT survive as "b" -- merging by index is the bug this pins.
# Wire: 96 01 (seq start id 18) 02 0a 61 (string id 0 "a") 0a 0a 62 (string id 1 "b")
#       07 (seq end) 96 01 (seq start id 18) 02 0a 63 (string id 0 "c") 07 (seq end)
echo "==> re-opened array wrapper must replace (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\012\012\142\007\226\001\002\012\143\007' > "$WORK/reopen_array.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/reopen_array.bin" ) \
    || { echo "FAIL: re-opened array wrapper must decode"; exit 1; }
printf '%s' "$OUT" | python3 -c '
import json, sys
a = json.load(sys.stdin)["somestringarray"]
if "b" in a:
    sys.exit("FAIL: re-opened array wrapper must be replaced, not merged (element \"b\" survived): %r" % (a,))
if not a or a[0] != "c":
    sys.exit("FAIL: re-opened array wrapper must hold the second opening'"'"'s element 0 == \"c\": %r" % (a,))
' || exit 1
echo "==> array wrapper replace OK"

# Fixlen SUBTYPE mismatch (MESSAGE_SPEC S7.3, generator#174): for a fixlen field
# the declared type maps to a wire type PLUS a subtype, so a header that carries
# the right Fixlen wire type but the WRONG subtype is just as contradictory as a
# wrong wire type and MUST be SKIPPED like an unknown id. somefp64 (id 9) is
# declared fp64 and keeps its schema default 3.141592653589793.
# Wire: 4a (id 9, fixlen) 0a (fixlen word: len 1, STRING subtype) 78 ("x")
# Control: 4a 41 (fixlen word: len 8, FP64 subtype) + 2.5 little-endian.
echo "==> fixlen subtype mismatch must skip (MESSAGE_SPEC S7.3, generator#174)"
printf '\112\012\170' > "$WORK/fixsubtype.bin"
printf '\112\101\000\000\000\000\000\000\004\100' > "$WORK/fixsubtype_control.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/fixsubtype.bin" ) \
    || { echo "FAIL: mismatched fixlen subtype must skip, not fail the decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64": 3.14159265358979' || { echo "FAIL: skipped fixlen field must keep its default 3.141592653589793; got: $OUT"; exit 1; }
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/fixsubtype_control.bin" ) \
    || { echo "FAIL: control (correct fp64 subtype) must decode"; exit 1; }
echo "$OUT" | grep -q '"somefp64": 2.5' || { echo "FAIL: control must decode to 2.5; got: $OUT"; exit 1; }
echo "==> fixlen subtype skip OK"

# The same skip, one rule over: a string a decoder STEPS OVER is never
# UTF-8-validated (CORELIB_PLAN S6.4.5, generator#417). Validation belongs where
# a string is MATERIALIZED, and it is taken on the complete payload (S6.4.4), so
# the two halves have to be asserted on the same bytes: a backend that validates
# too eagerly passes the declared half and fails the skipped one, a backend that
# never validates passes the skipped half and fails the declared one, and neither
# failure is visible from the other side. The driver runs four accept rows, not
# two: an undeclared id, a BLOB subtype at the id that DOES declare a string, a
# well-formed STRING at a scalar-declared id, and that last shape again one
# scope down, inside a sequence-framed struct.
#
# One shared driver for all eleven suites (ARCHITECTURE S12); it derives every
# fixture from the schema's own somestring/somefp64/someu8 declarations, and every
# skip row carries a trailing someu8 = 42 so a skip that ate one byte too many or
# too few cannot pass while the string sits at its default.
#
# Nothing is gated here: Python `str` is a S6.4.1 Unicode type, always strict, and
# the option MAY be omitted entirely -- there is no switch to turn the check off,
# and gating the declared half would only hide a regression.
#
# What the skip rows pin on the generated side is the S6.4.5 guard in on_field.
# The corelib cannot make this decision: a field a visitor ACCEPTS is a field it
# reads, so the moment on_field returns True for a fixlen header announcing a
# STRING payload, corelib-py materializes those bytes and validates them -- and
# then drops the value at an on_string arm that does not exist. Before
# generator#417 the id chain declined only string, blob and native-array ids, so
# `4a 0a ff` (a STRING subtype at the fp64-declared somefp64) and `a2 01 0a ff`
# (the same at somestruct) came back INVALID on both engines and both surfaces,
# where S6.4.5 requires COMPLETE. Delete the guard and skipped_string_at_scalar
# goes red.
#
# Both engines, for the reason the generator#411 block above runs on both: the
# accelerator reimplements the payload path, so a single-engine run leaves the
# half that actually ships unmeasured. The engine is put back afterwards.
echo "==> a skipped string is not UTF-8-validated (CORELIB_PLAN S6.4.5, generator#417)"
for ENGINE in $ENGINES; do
    if [ "$ENGINE" = python ]; then export SOFAB_PUREPYTHON=1; else unset SOFAB_PUREPYTHON || true; fi
    require_engine "$ENGINE"
    # Both surfaces name the category, in the two different ways this harness
    # has. `decode` lets the exception out, so the CLASS is the channel:
    # SofaDecodeError is INVALID and SofaIncompleteError is INCOMPLETE --
    # siblings, neither deriving from the other. `streamdecode` never raises
    # here; it reads the status the last feed returned and prints its NAME, so
    # the channel is that line, and `decode failed: INCOMPLETE` is what the same
    # row would print if the decoder mis-measured the skipped payload and walked
    # off its end. Exit status alone would accept either.
    for surface in decode streamdecode; do
        if [ "$surface" = decode ]; then
            U8_CAT="SofaDecodeError"
        else
            U8_CAT="decode failed: INVALID"
        fi
        python3 "$ROOT/tests/conformance/lib/check_skipped_string_utf8.py" "python/$ENGINE" \
            --cwd "$WORK/proj" --verb "$surface" --invalid-pattern "$U8_CAT" \
            -- python3 harness.py
    done
done
unset SOFAB_PUREPYTHON || true
if [ "$NATIVE" = yes ]; then require_engine native; else require_engine python; fi

# ...and the same question one level up, on a fixlen ARRAY, where the answer is
# the other one (CORELIB_PLAN S4.8.1, generator#411). S4.8.1 fixes five steps and
# the order of the middle three is normative: read the count; read the
# fixlen_word; a subtype that is neither fp32 nor fp64 -- a string, a blob, or a
# reserved 0x4-0x7 -- is INVALID before any schema is consulted (step 3); a
# fixed-width subtype that merely CONTRADICTS the declared element type is the
# S7.3 skip just tested (step 4), and the schema count MUST NOT be applied to it;
# only a matching subtype reaches the schema bound (step 5).
#
# So the STRING subtype that is a skip on the scalar somefp64 above is INVALID on
# an array: S4.8 admits no fixlen array of string or blob, so no schema could
# have declared one. Generated Python could not tell the two apart -- its array
# arm only asks whether the announced element kind is the one it declared and
# returns quietly when it is not, and the fixlen_word never reaches it at all.
# The corelib decides at the word.
#
# One shared driver for all eleven suites (ARCHITECTURE S12). It derives every
# fixture from the schema's own somefloatarray declaration, so the ids it writes
# and the values it asserts cannot drift from what the harness was built with,
# and it compares the skipped field's default as JSON numbers rather than by
# grep, which is what lets one table serve backends that render it three ways.
#
# The category is asserted by exception class, the channel the blocks above use:
# SofaDecodeError is INVALID, SofaIncompleteError is INCOMPLETE, and neither
# derives from the other. A bare non-zero exit would accept either, and
# INCOMPLETE is exactly what a corelib that mis-routes step 3 into the skip
# reports once it walks off the end of the shorter payload.
#
# This is the one leg below the engine split that runs on BOTH engines. The rule
# is decided entirely inside the corelib, and corelib-py carries two independent
# decoders -- the pure `sofab.decoder` and the Cython `sofab._speedups` -- each
# with its own fixlen_word arm, so a single-engine run leaves one of the two
# gates wholly unexercised. Nine subprocesses per engine is a cheap price for
# that. The engine is put back to the file's default afterwards.
#
# Run on BOTH decode surfaces. The verdict is the corelib's, taken at the
# fixlen_word, and several corelibs reach that word twice -- one arm for a
# whole-buffer decode and a separate one for the chunked path -- so a table that
# only ever ran the one-shot verb passes with the streaming copy mutated. This is
# the sweep the shared-vector and growth drivers beside it already do.
echo "==> a string/blob/reserved fixlen-array subtype is INVALID (generator#411)"
for ENGINE in $ENGINES; do
    if [ "$ENGINE" = python ]; then export SOFAB_PUREPYTHON=1; else unset SOFAB_PUREPYTHON || true; fi
    require_engine "$ENGINE"
    # The exception-class assertion rides `decode`, which prints the type.
    # `streamdecode` prints str(e) instead, where the class name does not appear,
    # so the same pattern would assert nothing there: that pass runs on exit
    # status, and the one-shot pass is what keeps a wrongly-INCOMPLETE verdict
    # out.
    for surface in decode streamdecode; do
        if [ "$surface" = decode ]; then FA_CAT="--invalid-pattern SofaDecodeError"; else FA_CAT=""; fi
        python3 "$ROOT/tests/conformance/lib/check_fixlen_array_subtype.py" "python/$ENGINE" \
            --cwd "$WORK/proj" --verb "$surface" $FA_CAT \
            -- python3 harness.py
    done
done
unset SOFAB_PUREPYTHON || true
if [ "$NATIVE" = yes ]; then require_engine native; else require_engine python; fi

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
echo "==> mis-typed later occurrence must not clear the array (MESSAGE_SPEC S7.4, generator#175)"
printf '\226\001\002\012\141\007\220\001\005' > "$WORK/skipped_occ_array.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/skipped_occ_array.bin" ) \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"somestringarray": \["a"' || { echo "FAIL: skipped occurrence must not clear the array (element 0 == \"a\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps array OK"

# S7.3 x S7.4, struct: same rule for a struct scope. somestruct (id 20) is opened
# correctly with nestedstring (id 1) = "x", then id 20 recurs carrying the
# UNSIGNED wire type. That occurrence is skipped, so nestedstring MUST still
# be "x" rather than falling back to its default "Nested".
# Wire: a6 01 (seq start id 20) 0a 0a 78 (string id 1, len 1, "x") 07 (seq end)
#       a0 01 (id 20, UNSIGNED) 05
echo "==> mis-typed later occurrence must not clear the struct (MESSAGE_SPEC S7.4, generator#175)"
printf '\246\001\012\012\170\007\240\001\005' > "$WORK/skipped_occ_struct.bin"
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/skipped_occ_struct.bin" ) \
    || { echo "FAIL: mis-typed later occurrence must decode, not error"; exit 1; }
echo "$OUT" | grep -q '"nestedstring": "x"' || { echo "FAIL: skipped occurrence must not clear the struct (nestedstring \"x\" lost); got: $OUT"; exit 1; }
echo "==> skipped occurrence keeps struct OK"

# refused_as <class> <label> <dir> <message> <fixture> -- the CATEGORY, by
# exception CLASS, for a `decode` that must fail. sofab.types keeps
# SofaLimitError a SIBLING of SofaDecodeError, so the class name is the caller's
# channel: a cap breach is SofaLimitError and a schema-bound breach is
# SofaDecodeError, and CORELIB_PLAN S6.3 forbids either standing in for the other
# (generator#416). Both halves are asserted -- the wanted class present AND the
# other one absent -- because a chained traceback names both when a decode
# re-raises, and only the negative half sees that.
#
# A bare `if ... decode ...; then FAIL; fi` sees neither: both categories exit
# non-zero, which is exactly the collapse under test.
refused_as() {
    ra_want=$1; ra_label=$2; ra_dir=$3; ra_msg=$4; ra_fixture=$5
    case $ra_want in
        SofaLimitError) ra_other=SofaDecodeError ;;
        SofaDecodeError) ra_other=SofaLimitError ;;
        *) echo "FAIL: refused_as: unknown class $ra_want"; exit 1 ;;
    esac
    if (cd "$ra_dir" && python3 harness.py decode "$ra_msg") < "$ra_fixture" >/dev/null 2>"$WORK/cat-err.txt"; then
        echo "FAIL: $ra_label -- the decode SUCCEEDED, and must be refused as $ra_want"; exit 1
    fi
    grep -q "$ra_want" "$WORK/cat-err.txt" \
        || { echo "FAIL: $ra_label -- refused, but not as $ra_want (S6.3); got:"; cat "$WORK/cat-err.txt"; exit 1; }
    grep -q "$ra_other" "$WORK/cat-err.txt" \
        && { echo "FAIL: $ra_label -- reported as $ra_other; S6.3 keeps the two apart"; cat "$WORK/cat-err.txt"; exit 1; }
    return 0
}

# Receiver-side decode limits (generator#102): max_dyn_array_count: 4 caps a
# count-less (schema-unbounded) u64 array. Wire header 0x03 = id 0, unsigned
# array; a wire count of 5 MUST fail decode with the corelib limit error,
# exactly 4 still decodes, and the same oversized bytes decode fine against a
# project generated WITHOUT the limit (unset = unlimited).
echo "==> receiver-side decode limits must reject over-cap counts (generator#102)"
cat > "$WORK/limit-def.yaml" <<YAML
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u64 } }
YAML
cat > "$WORK/limit-cfg.yaml" <<YAML
generic: { emit: project, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/limit-cfg.yaml" --lang python --in "$WORK/limit-def.yaml" --out "$WORK/limitproj" )
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python --in "$WORK/limit-def.yaml" --out "$WORK/nolimitproj" )
printf '\003\005\001\002\003\004\005' > "$WORK/limit-over.bin"
printf '\003\004\001\002\003\004' > "$WORK/limit-ok.bin"
# The old assertion here was `grep -qi limit` over the traceback, which is no
# assertion at all: the frames name this very project directory
# ($WORK/limitproj), so the pattern matched whatever was raised -- a
# SofaDecodeError included (generator#416).
refused_as SofaLimitError "wire count 5 > max_dyn_array_count 4" \
    "$WORK/limitproj" dyn "$WORK/limit-over.bin"
(cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/limit-ok.bin" >/dev/null || { echo "FAIL: wire count 4 must decode under limit 4"; exit 1; }
(cd "$WORK/nolimitproj" && python3 harness.py decode dyn) < "$WORK/limit-over.bin" >/dev/null || { echo "FAIL: unset limit must keep count 5 decodable"; exit 1; }
echo "==> decode-limit reject OK"

# The two refusals of CORELIB_PLAN §6.3, on one schema and one harness
# (generator#416). A configured receiver cap on a schema-UNBOUNDED field is a
# policy verdict -- SofaLimitError, because the bytes are well formed and the
# same message decodes under a looser cap -- while a field the SCHEMA bounds
# answers SofaDecodeError when the wire breaches that bound, and §6.3 adds the
# other direction: LimitExceeded is "never raised for a field the schema bounds".
# Reporting either as the other tells the caller the wrong party is broken.
#
# One shared driver, wired here and in tests/conformance/dart/run.sh
# (ARCHITECTURE §12) -- the two suites generator#416 names. The other nine still
# make this assertion each in its own #102 block and to its own depth (go's is
# exit status alone), which is what a driver in lib/ exists to replace.
#
# It prints its own `refusal` message, so the ids its fixtures breach and the
# bounds it asserts against cannot drift from what this project was generated
# with, and each refusing row asserts its own class AND that the other one did
# not appear.
# Its accepting rows (at the cap; over the cap but inside the schema bound) read
# their value back, so a decoder cannot pass by refusing everything, and they pin
# the cap's own value against the config written here.
#
# Four shapes, because a cap reaches four different pieces of machinery: the
# count cap in the generated on_array_begin, the string and blob length caps in
# the generated on_fixlen_header -- two separate numbers -- and the count-less
# WRAPPER array, whose element index is its length and is compared where the
# elements are collected.
#
# BOTH engines. The array-count cap is raised inside corelib-py, and the
# accelerator reimplements that path (`_speedups` _visit_varints) independently
# of decoder.py -- so a native-only pass would leave the pure decoder's category
# unmeasured, and the rest of this receiver-limits region runs native-only.
#
# BOTH decode surfaces, as dart runs them. The two paths reach the corelib
# separately, so a table that only ever ran the one-shot `decode` passes with the
# chunked copy mis-routed. `decode` reads the class off its traceback; the
# generated `streamdecode` now names the category itself -- `decode error:
# <class>: <msg>` for the raise a cap makes, `decode failed: <STATUS>` for the
# status a schema-bound breach comes back as -- because `str(e)` never carried
# the class and `Status` is an IntEnum whose `%s` printed `2`.
echo "==> a cap is SofaLimitError, a schema bound is SofaDecodeError (§6.3, generator#416)"
printf 'version: 1\nmessages:\n' > "$WORK/refusal.yaml"
python3 "$ROOT/tests/conformance/lib/check_refusal_category.py" --emit-schema >> "$WORK/refusal.yaml"
cat > "$WORK/refusal-cfg.yaml" <<YAML
generic: { emit: project, max_dyn_array_count: 4, max_dyn_string_len: 8, max_dyn_blob_len: 8 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/refusal-cfg.yaml" --lang python --in "$WORK/refusal.yaml" --out "$WORK/refusalproj" )
for ENGINE in $ENGINES; do
    if [ "$ENGINE" = python ]; then export SOFAB_PUREPYTHON=1; else unset SOFAB_PUREPYTHON || true; fi
    require_engine "$ENGINE"
    python3 "$ROOT/tests/conformance/lib/check_refusal_category.py" "python/$ENGINE" \
        --cwd "$WORK/refusalproj" \
        --max-dyn-array-count 4 --max-dyn-string-len 8 --max-dyn-blob-len 8 \
        --limit-pattern SofaLimitError --invalid-pattern SofaDecodeError \
        -- python3 harness.py
    python3 "$ROOT/tests/conformance/lib/check_refusal_category.py" "python/$ENGINE" \
        --cwd "$WORK/refusalproj" --verb streamdecode \
        --max-dyn-array-count 4 --max-dyn-string-len 8 --max-dyn-blob-len 8 \
        --limit-pattern 'decode error: SofaLimitError' \
        --invalid-pattern 'decode failed: INVALID' \
        -- python3 harness.py
done
unset SOFAB_PUREPYTHON || true
if [ "$NATIVE" = yes ]; then require_engine native; else require_engine python; fi

# CORELIB_PLAN S6.2.1, the two rules a scope-wide cap could not honour. Both are
# end-to-end: the generator's unit tests can only see emitted substrings.
echo "==> a cap must not reach a schema-bounded field, nor a skipped one (S6.2.1)"
cat > "$WORK/excl.yaml" <<'YAML'
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u64 } }
      b: { id: 1, type: array, items: { type: i32, count: 100000 } }
      w: { id: 2, type: array, items: { type: string } }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/limit-cfg.yaml" --lang python --in "$WORK/excl.yaml" --out "$WORK/exclproj" )
# b (id 1, signed array, count 6) is bounded by its own `count: 100000`, so the
# cap of 4 must not touch it.
printf '\014\006\002\002\002\002\002\002' > "$WORK/bounded6.bin"
(cd "$WORK/exclproj" && python3 harness.py decode dyn) < "$WORK/bounded6.bin" >/dev/null \
    || { echo "FAIL: a schema-bounded array must not be judged against the receiver cap"; exit 1; }
# ...while the unbounded sibling at the same cap still rejects at 6.
printf '\003\006\001\001\001\001\001\001' > "$WORK/unbounded6.bin"
refused_as SofaLimitError "the unbounded sibling must still be capped at 4" \
    "$WORK/exclproj" dyn "$WORK/unbounded6.bin"
# A field the handler SKIPS is never capped (S6.2.1: it allocates nothing). id 9
# is declared nowhere, so an over-cap array there must decode.
printf '\113\005\001\001\001\001\001' > "$WORK/skipcap.bin"
(cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/skipcap.bin" >/dev/null \
    || { echo "FAIL: an over-cap array at an UNDECLARED id must be skipped, not capped"; exit 1; }
# ...and neither is a field whose wire kind contradicts the declaration (S7.3):
# id 0 is declared array<u64> (unsigned), so a SIGNED array there is skipped.
printf '\004\005\002\002\002\002\002' > "$WORK/mistyped.bin"
(cd "$WORK/limitproj" && python3 harness.py decode dyn) < "$WORK/mistyped.bin" >/dev/null \
    || { echo "FAIL: an over-cap array of the WRONG kind must be skipped, not capped"; exit 1; }
# The same rule one level down, where the number generated code states is the
# only one in play: a WRAPPER array carries no count header, so its element
# INDEX is its length and takes max_dyn_array_count (4). `w` (id 2) is a
# count-less string array; an element whose fixlen subtype contradicts `string`
# is a S7.3 skip, so it never grows the list and the index cap must not fire on
# it -- which needs the tag test to run AHEAD of the index compare.
# Wire: 16 seq_begin(id 2) | 2a element id 5 | 0b fixlen_word (len 1, subtype
# BLOB, contradicting the declared `string`) | 'x' | 07 end.
printf '\026\052\013\170\007' > "$WORK/wrapmistyped.bin"
(cd "$WORK/exclproj" && python3 harness.py decode dyn) < "$WORK/wrapmistyped.bin" >/dev/null \
    || { echo "FAIL: a mis-subtyped wrapper element above the index cap must be skipped, not capped"; exit 1; }
# ...told apart from the very same element as a STRING, which this scope DOES
# read and which the index cap therefore does bound.
printf '\026\052\012\170\007' > "$WORK/wrapovercap.bin"
refused_as SofaLimitError "a string element at index 5 exceeds max_dyn_array_count 4" \
    "$WORK/exclproj" dyn "$WORK/wrapovercap.bin"
echo "==> cap exclusivity OK (bounded sibling decodes; unknown id, mis-typed kind and mis-subtyped wrapper element all skipped)"

# A wrapper string element's own byte LENGTH, and a matrix ROW's own element
# count: two numbers the generated visitor is the only thing that can bound,
# since neither reaches a schema bound when the schema declares none.
echo "==> a wrapper element's length and a matrix row's count are capped (S6.2.1)"
cat > "$WORK/elem.yaml" <<'YAML'
version: 1
messages:
  el:
    payload:
      ws: { id: 0, type: array, items: { type: string } }
      m:  { id: 1, type: array, items: { type: array, items: { type: u32 } } }
YAML
cat > "$WORK/elem-cfg.yaml" <<YAML
generic: { emit: project, max_dyn_string_len: 4, max_dyn_array_count: 4 }
YAML
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/elem-cfg.yaml" --lang python --in "$WORK/elem.yaml" --out "$WORK/elemproj" )
# 06 seq_begin(id 0) | 02 string elem id 0 | 2a fixlen_word (len 5, subtype
# string) | "xxxxx" (5 bytes > cap 4) | 07 end
printf '\006\002\052\170\170\170\170\170\007' > "$WORK/elemover.bin"
refused_as SofaLimitError "a wrapper string element 5 bytes long exceeds max_dyn_string_len 4" \
    "$WORK/elemproj" el "$WORK/elemover.bin"
# ...4 bytes is at the cap and decodes.
printf '\006\002\042\170\170\170\170\007' > "$WORK/elemok.bin"
(cd "$WORK/elemproj" && python3 harness.py decode el) < "$WORK/elemok.bin" >/dev/null \
    || { echo "FAIL: a wrapper string element at the cap must decode"; exit 1; }
# 0e seq_begin(id 1) | 03 unsigned array elem id 0 | count 5 > cap 4
printf '\016\003\005\001\002\003\004\005\007' > "$WORK/rowover.bin"
refused_as SofaLimitError "a matrix row of 5 elements exceeds max_dyn_array_count 4" \
    "$WORK/elemproj" el "$WORK/rowover.bin"
printf '\016\003\004\001\002\003\004\007' > "$WORK/rowok.bin"
(cd "$WORK/elemproj" && python3 harness.py decode el) < "$WORK/rowok.bin" >/dev/null \
    || { echo "FAIL: a matrix row at the cap must decode"; exit 1; }
echo "==> element length + row count caps OK"

# Conformance covers the per-field scalar vectors; WireArraySparsity covers the
# array ones -- the MESSAGE_SPEC S2 element rule and S3's "count is a capacity",
# both byte-exact against the regenerated shared vectors, both executed through a
# generated project driving corelib-py. SchemaBoundIsDeclaredNotCopied is the
# S6.2.1/S6.3 split the same way: a schema-bounded field decodes past a tighter
# receiver cap, its own bound still rejects as INVALID, and a header that
# contradicts the declared type is skipped rather than measured against it (S7.3).
echo "==> shared-vector byte-exact conformance"
( cd "$ROOT" && SOFAB_PY_CORELIB="$CORELIB" go test ./generators/python/ \
    -run 'Conformance|WireArraySparsity|NestedNativeRowCountBound|SchemaBoundIsDeclaredNotCopied' -count=1 )

# ...and the decode direction (generator#444): each vector's DENSE bytes fed into
# a message that declares u64 on the anchors and nothing else, so every other
# field on the wire is an unknown id or a MESSAGE_SPEC S7.3 wire-type mismatch
# and must be SKIPPED -- with the anchor behind it still exact.
#
# Run on BOTH engines. The skip path is one of the parts the accelerator
# reimplements, so a pure-only run would leave the native one unmeasured
# (generator#451).
echo "==> shared-vector decode conformance (skip matrix)"
printf 'version: 1\nmessages:\n' > "$WORK/vecskip.yaml"
python3 "$ROOT/tests/conformance/lib/check_vectors_decode.py" --emit-schema >> "$WORK/vecskip.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/cfg.yaml" --lang python \
    --in "$WORK/vecskip.yaml" --out "$WORK/vecskip" >/dev/null )
#
# ...and on BOTH decode surfaces. `streamdecode` drips the message in ONE BYTE
# PER feed, so every position inside every skipped payload becomes a
# suspend/resume boundary; that is where a resync bug the single-buffer path
# hides shows up (generator#456).
if [ "$NATIVE" = yes ]; then
    unset SOFAB_PUREPYTHON || true
    require_engine native
    for surface in decode streamdecode; do
        python3 "$ROOT/tests/conformance/lib/check_vectors_decode.py" \
            "$CORELIB/assets/test_vectors.json" "Python (native)" --mode "$surface" \
            --cwd "$WORK/vecskip" -- python3 harness.py
    done
fi
SOFAB_PUREPYTHON=1
export SOFAB_PUREPYTHON
require_engine python
for surface in decode streamdecode; do
    python3 "$ROOT/tests/conformance/lib/check_vectors_decode.py" \
        "$CORELIB/assets/test_vectors.json" "Python (pure)" --mode "$surface" \
        --cwd "$WORK/vecskip" -- python3 harness.py
done
unset SOFAB_PUREPYTHON || true

echo "==> corpus + realworld: every definition imports"
for def in "$ROOT"/tests/matrix/corpus/defs/*.yaml "$ROOT"/examples/messages/realworld/vehicle_telemetry.yaml; do
    name=$(basename "$def" .yaml)
    ( cd "$ROOT" && go run ./cmd/sofabgen --lang python --in "$def" --out "$WORK/corpus/$name" >/dev/null )
    PYTHONPATH="$CORELIB/src:$WORK/corpus/$name" python3 -c "import message" \
        || { echo "FAIL: corpus def $name did not import"; exit 1; }
done
echo "==> corpus imports ($(ls "$ROOT"/tests/matrix/corpus/defs/*.yaml | wc -l) definitions + realworld example)"

# Declared integer width is a VALIDITY bound (MESSAGE_SPEC S7.1 + documentation#32,
# generator#266, Crucible F-0033 / codegen defect G-0026). A value outside the
# declared width is INVALID: it MUST NOT be masked to the width, and MUST NOT be
# kept. someu8 is id 0 (header 0x00 = 0<<3 | unsigned), someu16 is id 1 (0x08).
#   00 ff 7f = 16383 into a u8 -- the reported reproducer
#   00 80 02 = 256   into a u8 -- one past the width
#   08 f0 a2 04 = 70000 into a u16
#   00 ff 01 = 255   into a u8 -- the in-range control: must decode and keep 255
echo "==> over-width scalar must be INVALID (S7.1, generator#266)"
printf '\000\377\177'     > "$WORK/w_u8_16383.bin"
printf '\000\200\002'     > "$WORK/w_u8_256.bin"
printf '\010\360\242\004' > "$WORK/w_u16_70000.bin"
printf '\000\377\001'     > "$WORK/w_u8_255_ctl.bin"
for v in w_u8_16383 w_u8_256 w_u16_70000; do
    if (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/$v.bin" >/dev/null 2>&1; then
        echo "FAIL: $v must be INVALID (S7.1) -- neither masked to the width nor kept"; exit 1
    fi
done
OUT=$( (cd "$WORK/proj" && python3 harness.py decode myfirstmessage) < "$WORK/w_u8_255_ctl.bin" ) || { echo "FAIL: in-range control 255 must decode"; exit 1; }
echo "$OUT" | tr -d ' ' | grep -q '"someu8":255' || { echo "FAIL: control must keep 255 exactly; got: $OUT"; exit 1; }
echo "==> declared-width reject OK"

# CORELIB_PLAN S7.2 item 8 -- the shared file's `sequence_growth` block
# (generator#449). A wrapper array carries no element count: its length is
# highest present id + 1, so it GROWS as elements arrive, and the element INDEX
# is what the receiver cap bounds. Two ports that grow differently emit
# IDENTICAL bytes, so no vector can reach this -- the cases are a delivery
# sequence of ids, and the driver builds the message from them.
#
# The index check lives in GENERATED code in every backend, which is why this
# runs here and not only in the corelibs.
echo "==> sequence_growth: a wrapper array grows to its highest id, and the index is the bound"
printf 'version: 1\nmessages:\n' > "$WORK/growth.yaml"
python3 "$ROOT/tests/conformance/lib/check_growth.py" --emit-schema >> "$WORK/growth.yaml"
( cd "$ROOT" && go run ./cmd/sofabgen --config "$WORK/limit-cfg.yaml" --lang python --in "$WORK/growth.yaml" --out "$WORK/growth" >/dev/null )
# --cap must equal the max_dyn_array_count the config above generated with:
# the cases' indices are offsets onto it, so a mismatch moves the boundary.
python3 "$ROOT/tests/conformance/lib/check_growth.py" \
    "$CORELIB/assets/test_vectors.json" "Python" --cap 4 \
    --cwd "$WORK/growth" -- python3 harness.py

# CORELIB_PLAN S5.2/S6.0/S5.2.3, one property: the verdict AND the decoded value
# must not depend on where the chunks were cut (generator#413). A resume bug -- a
# half-read varint, a payload accumulator that is not carried, a scope stack that
# unwinds one level too far -- is invisible until the split lands in the wrong
# place, and the one-shot `decode` path never suspends at all.
#
# The fixtures are the ones this suite ALREADY built for its negative cases, so
# the table costs nothing but the feeding: every §7.1 reject, every §7.3 skip,
# every truncation, and the controls beside them.
#
# Run on BOTH engines, for the reason the shared-vector block runs on both
# (generator#451): the accelerator reimplements the resume machinery, so a
# pure-only run would leave the half that actually ships unmeasured.
echo "==> a chunk boundary must not change the verdict or the value (generator#413)"
chunk_invariance() {
    python3 "$ROOT/tests/conformance/lib/check_chunk_invariance.py" "$1" \
        --message myfirstmessage --expect 21 --oneshot \
        "$WORK/control.bin" "$WORK/overcount.bin" \
        "$WORK/overcount_trunc.bin" "$WORK/incount_trunc.bin" \
        "$WORK/overindex.bin" "$WORK/overindex_control.bin" \
        "$WORK/overmaxlen.bin" "$WORK/overmaxlen_control.bin" \
        "$WORK/overmaxlen_trunc.bin" "$WORK/inmaxlen_trunc.bin" \
        "$WORK/overwidth_trunc.bin" "$WORK/inwidth_trunc.bin" \
        "$WORK/wiremismatch.bin" "$WORK/wiremismatch_control.bin" \
        "$WORK/wiremismatch_seq.bin" \
        "$WORK/fixsubtype.bin" "$WORK/fixsubtype_control.bin" \
        "$WORK/reopen_struct.bin" "$WORK/reopen_array.bin" \
        "$WORK/skipped_occ_struct.bin" "$WORK/skipped_occ_array.bin" \
        --cwd "$WORK/proj" -- python3 harness.py
}
if [ "$NATIVE" = yes ]; then
    unset SOFAB_PUREPYTHON || true
    require_engine native
    chunk_invariance "Python (native)"
fi
SOFAB_PUREPYTHON=1
export SOFAB_PUREPYTHON
require_engine python
chunk_invariance "Python (pure)"
unset SOFAB_PUREPYTHON || true

echo "PASS"
