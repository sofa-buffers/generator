#!/usr/bin/env python3
"""MESSAGE_SPEC §7.4 — a field id REPEATED inside one scope (generator#523).

Usage:
  check_repeated_id.py --emit-schema
  check_repeated_id.py <label> [--cwd DIR] [--sizes 1,2,3,5,0]
                       [--no-stream] [--message NAME] -- <harness argv...>

§7.4 says the **last occurrence wins, per field id**, and then splits on what
the field *is*:

  a re-opened SEQUENCE continues its scope  -> struct/union members MERGE, and
    children set by an earlier opening whose ids do not recur are RETAINED;
  an ARRAY WRAPPER is the exception         -> it *is* the value of its array
    field (§5), so a later occurrence REPLACES it whole.

Both halves are checked here, in the same run and on the same message, because
the failure mode of fixing one is breaking the other: a backend that "fixed"
replacement by resetting every re-opened element id would zero a struct
element's unrecurring fields, and a backend that protected the merge by never
resetting anything merges rows. Neither is visible without the other case beside
it.

## Why this is a shared DRIVER and not a shared vector

§7.4 opens by saying such an encoding is not well formed and that producers MUST
NOT emit it. It is a **decoder obligation only**, so no encoder in the family
will ever produce these bytes and no round-trip vector can carry them — the same
reason `check_growth.py` builds its own message rather than reading one. This
driver therefore forges the wire image itself: a handful of varint headers,
built from MESSAGE_SPEC §4.3/§4.7/§4.6/§4.9 and nothing else.

Before it existed, the whole family's only §7.4 regression guard was
`tests/conformance/rust/repeated_id.rs`, added by generator#509 for one backend
and one field shape — and the family demonstrably drifted behind it. Measured
across all eleven backends on the `wrapper_row` case below, before
generator#523: **c, cpp, csharp, java, kotlin, typescript replaced** (§7.4) while
**rust, go, zig, dart and python merged**. Five ports, four of them found only
because this driver runs everywhere rather than where the last bug was.

## The message, and why every position is in it

`--emit-schema` prints one message, `rid`, carrying every array position §7.4
can be tested at — a repeated id means something different at each, and a
backend can get one right and the next wrong:

    nums   array<u32>                  a LEAF native array
    strs   array<string>               a leaf VALUE element inside a wrapper
    mat    array<array<u32>>           a NATIVE ROW  (generator#509)
    matstr array<array<string>>        a WRAPPER ROW (generator#523)
    deep   array<array<array<u32>>>    a wrapper row one level further down
    objs   array<struct>               the MERGING half

Every array is schema-bounded (`count`, and `maxlen` on the payload elements) so
the statically bounded profiles — C, C++ `corelib: c-cpp`, Rust `no_std` — can
build it too. This concern is not gated on `dynamic_arrays`: replacement is
semantics, not storage, and a heapless container clears just as well as a
growing one does.

The occurrences are chosen so that MERGING and REPLACING cannot agree on the
answer. The second occurrence is always SHORTER than the first, so an
implementation that writes on top of the previous one is caught by the length
even where the values coincide — `[["y", "z"]]` against `[["y"]]`, not two
spellings of the same list.

## The chunked path is checked too

The fix every merging backend needed is a DESTRUCTIVE reset, and its safety
rests on the header hook firing exactly ONCE per occurrence. If a corelib ever
re-announced an array or a sequence on resume, a chunked repeated-id message
would silently drop every chunk but the last — and every suite would stay green,
because `check_chunk_invariance.py` replays shared vectors and no shared vector
can carry a repeated id. So each case is also fed through `streamdecode` at
several splits and compared against the one-shot answer. `--no-stream` exists
for a harness that has no streaming verb; it is reported, never silent.

## Loud, never quiet

A coverage test's failure mode is passing while checking nothing, so: a case
whose decode fails is a FAILURE (these bytes are well formed — §7.4 defines them,
it does not reject them); the case count is printed; and the driver refuses to
report success unless every case ran.
"""
import json
import re
import subprocess
import sys

MSG = "rid"

# Field ids, which the forged headers below and the emitted schema share.
NUMS, STRS, MAT, MATSTR, DEEP, OBJS = 0, 1, 2, 3, 4, 5

# Wire types (CORELIB_PLAN §4.3). Normative; do not renumber.
WT_SIGNED, WT_FIXLEN, WT_ARR_U, WT_SEQ = 1, 2, 3, 6
END = b"\x07"                 # sequence end marker (§4.9)
SUB_STRING = 2                # fixlen subtype for `string` (§4.6)


def emit_schema() -> int:
    print("# The §7.4 repeated-id message (generator#523), printed by")
    print("# tests/conformance/lib/check_repeated_id.py so the schema and the driver")
    print("# that forges repeated-id bytes against it have one definition between them.")
    print("#")
    print("# One field per array position a repeated id means something different at:")
    print("# a leaf native array, a leaf value element, a native row, a WRAPPER row,")
    print("# a wrapper row one level down, and the struct element that must MERGE.")
    print("# Every array is bounded so the statically sized profiles can build it.")
    print(f"  {MSG}:")
    print("    payload:")
    print(f"      nums:   {{ id: {NUMS}, type: array, items: {{ type: u32, count: 4 }} }}")
    print(f"      strs:   {{ id: {STRS}, type: array,"
          " items: { type: string, count: 5, maxlen: 16 } }")
    print(f"      mat:    {{ id: {MAT}, type: array, items: {{ type: array, count: 2,"
          " items: { type: u32, count: 4 } } }")
    print(f"      matstr: {{ id: {MATSTR}, type: array, items: {{ type: array, count: 2,"
          " items: { type: string, count: 3, maxlen: 8 } } }")
    print(f"      deep:   {{ id: {DEEP}, type: array, items: {{ type: array, count: 2,"
          " items: { type: array, count: 2, items: { type: u32, count: 3 } } } }")
    print(f"      objs:   {{ id: {OBJS}, type: array, items: {{ type: struct, count: 2,"
          " fields: { x: { id: 0, type: i32 }, y: { id: 1, type: i32 } } } }")
    return 0


# --- the wire image ---------------------------------------------------------

def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def header(fid: int, wt: int) -> bytes:
    """`(id << 3) | wire_type`, MESSAGE_SPEC §4.3."""
    return varint((fid << 3) | wt)


def uarray(fid: int, values) -> bytes:
    """An unsigned-integer array: header, element count, then the elements (§4.7)."""
    return header(fid, WT_ARR_U) + varint(len(values)) + b"".join(varint(v) for v in values)


def string(fid: int, s: str) -> bytes:
    """A string: header, `fixlen_word` = (length << 3) | subtype, payload (§4.6)."""
    payload = s.encode()
    return header(fid, WT_FIXLEN) + varint((len(payload) << 3) | SUB_STRING) + payload


def signed(fid: int, n: int) -> bytes:
    """A signed integer, zig-zag encoded (§4.5)."""
    return header(fid, WT_SIGNED) + varint((n << 1) ^ (n >> 63) if n < 0 else n << 1)


def seq(fid: int, *parts: bytes) -> bytes:
    """A sequence: `sequence start` at fid, the children, the end marker (§4.9)."""
    return header(fid, WT_SEQ) + b"".join(parts) + END


# --- the cases --------------------------------------------------------------
#
# (name, field, wire image, expected decoded value, what it pins)
#
# Each `expect` is the REPLACING or MERGING answer §7.4 mandates, and is chosen
# so the other reading produces a different LENGTH as well as different values.

CASES = [
    (
        "leaf_array", "nums",
        uarray(NUMS, [1, 2, 3, 4]) + uarray(NUMS, [9]),
        [9],
        "a repeated LEAF array id replaces the array whole (§7.4)",
    ),
    (
        "string_element", "strs",
        seq(STRS, string(0, "first"), string(0, "last")),
        ["last"],
        "a repeated string ELEMENT id replaces that element (it is a leaf value)",
    ),
    (
        "native_row", "mat",
        seq(MAT, uarray(0, [1, 2, 3]), uarray(0, [4, 5])),
        [[4, 5]],
        "a repeated NATIVE ROW id replaces the row, not merges into it (generator#509)",
    ),
    (
        "native_row_bound", "mat",
        seq(MAT, uarray(0, [1, 2, 3]), uarray(0, [4, 5, 6, 7])),
        [[4, 5, 6, 7]],
        "a merged row would breach its own declared inner count: each occurrence is "
        "legal (3 and 4 <= count 4), appended they are 7, which no occurrence declared",
    ),
    (
        "wrapper_row", "matstr",
        seq(MATSTR, seq(0, string(0, "a"), string(1, "z")), seq(0, string(0, "y"))),
        [["y"]],
        "a repeated WRAPPER ROW id replaces the row: the row is an array field, so it "
        "is the replacing kind and not the merging one (generator#523)",
    ),
    (
        "deep_row", "deep",
        seq(DEEP, seq(0, uarray(1, [3, 4])), seq(0, uarray(0, [9]))),
        [[[9]]],
        "the same rule one level further down: re-opening a middle wrapper element "
        "replaces it, so the row it held at a DIFFERENT id is gone too",
    ),
    (
        "scope_merge", "objs",
        seq(OBJS, seq(0, signed(0, 11), signed(1, 22)), seq(0, signed(0, 33))),
        [{"x": 33, "y": 22}],
        "THE OTHER HALF: a re-opened STRUCT element continues its scope, so y "
        "survives from the first opening while x is overwritten by the second",
    ),
]


# --- comparing a decoded value across eleven JSON dialects -------------------

def same(want, got) -> bool:
    """Structural comparison that tolerates each harness's JSON spelling.

    The dialects are measured, not guessed: `null` for an empty container (Go
    marshals a nil slice that way, and a nil slice has length 0), and a wide
    integer carried as a string (the ports whose JSON front door is a double).
    Neither is a difference in the decoded value, which is what these cases
    assert. A struct is compared on the keys `want` names, so a harness that
    renders extra fields is not failed for it.
    """
    if want is None:
        return got is None
    if isinstance(want, list):
        if got is None:
            got = []
        return isinstance(got, list) and len(want) == len(got) \
            and all(same(w, g) for w, g in zip(want, got))
    if isinstance(want, dict):
        return isinstance(got, dict) and all(k in got and same(v, got[k])
                                             for k, v in want.items())
    if isinstance(want, bool):
        return want is got
    if isinstance(want, int):
        try:
            return int(got) == want
        except (TypeError, ValueError):
            return False
    return want == got


# Build noise rather than the harness's own output: `cargo run` re-emits crate
# warnings, a JVM prints a stack trace under its exception, rustc's diagnostics
# carry continuation lines. Same list as check_growth.py, same reason.
_NOISE = re.compile(
    r"^(at\s|warning\b|note:|help:|-->|\||=\s|\d+\s*\||\^|Note:|WARNING:|SLF4J|Picked up )")


def diagnostic(stderr: bytes) -> str:
    lines = [l.strip() for l in stderr.decode(errors="replace").splitlines()
             if l.strip() and not _NOISE.match(l.strip())]
    return " | ".join(lines[-2:]) if lines else "<no output>"


def run(cmd, argv, wire, cwd):
    """One harness invocation; returns (parsed JSON or None, diagnostic)."""
    p = subprocess.run(cmd + argv, input=wire, cwd=cwd,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode != 0:
        return None, f"exited {p.returncode}: {diagnostic(p.stderr)}"
    try:
        return json.loads(p.stdout.decode()), ""
    except ValueError:
        return None, f"printed no JSON: {p.stdout.decode(errors='replace')[:200]!r}"


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()

    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    label = head[0]
    cwd = opt(head, "--cwd")
    msg = opt(head, "--message", MSG)
    stream = "--no-stream" not in head
    sizes = [int(s) for s in opt(head, "--sizes", "1,2,3,5,0").split(",")]

    ran = 0
    for name, field, wire, want, why in CASES:
        obj, err = run(cmd, ["decode", msg], wire, cwd)
        if obj is None:
            print(f"FAIL case {name}: a repeated id is WELL DEFINED, never a refusal "
                  f"(§7.4 defines it rather than rejecting it) -- harness {err}")
            return 1
        if field not in obj:
            print(f"FAIL case {name}: decoded object has no field {field!r}: {sorted(obj)}")
            return 1
        if not same(want, obj[field]):
            print(f"FAIL case {name}: {why}\n"
                  f"  got  {field} = {json.dumps(obj[field])}\n"
                  f"  want {field} = {json.dumps(want)}")
            return 1

        # ...and the same bytes CHUNKED, against the one-shot answer above. The
        # resets §7.4 needs are destructive, so they are only safe while the hook
        # that carries them fires once per occurrence; a corelib that re-announced
        # a header on resume would drop every chunk but the last.
        if stream:
            for size in sizes:
                got, err = run(cmd, ["streamdecode", msg, str(size)], wire, cwd)
                if got is None:
                    print(f"FAIL case {name}: chunk size {size}: harness {err}")
                    return 1
                if not same(want, got.get(field)):
                    print(f"FAIL case {name}: the §7.4 answer must not depend on the "
                          f"chunk split -- {why}\n"
                          f"  chunk size {size}: {field} = {json.dumps(got.get(field))}\n"
                          f"  want               {field} = {json.dumps(want)}")
                    return 1
        ran += 1

    if ran != len(CASES):
        print(f"FAIL: ran {ran} of {len(CASES)} repeated-id cases")
        return 1
    chunks = (f"one-shot and streamed at splits {','.join(str(s) for s in sizes)}"
              if stream else "one-shot ONLY (--no-stream: this harness has no "
                             "streaming verb, so the destructive resets are "
                             "unchecked on resume)")
    print(f"{label} §7.4 repeated id: {ran} cases -- wrappers replace (leaf array, "
          f"value element, native row, wrapper row, depth-3 row) and scopes merge; "
          f"{chunks}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
