#!/usr/bin/env python3
"""Replay the shared file's `sequence_growth` block against a generated harness.

Usage:
  check_growth.py --emit-schema
  check_growth.py <test_vectors.json> <label> --cap N
                  [--cwd DIR] [--limit-word W] -- <harness argv...>

`test_vectors.json` carries three top-level blocks. `vectors` is driven by
`check_vectors.py` (encode) and `check_vectors_decode.py` (decode); this one
drives the third, `sequence_growth` -- CORELIB_PLAN §7.2 item 8 (generator#449).

## Why these cases cannot be vectors, and why they land in this repo

A wrapper (sequence) array carries no element count on the wire: its length is
*highest present id + 1* (MESSAGE_SPEC §5.1), so the size is known only once the
array ends and the container **grows** as elements arrive. That is invisible in
the bytes -- two implementations that grow completely differently emit identical
bytes and reach identical outcomes -- which is why the block is keyed by a
**delivery sequence of element ids** rather than by a byte string, and why the
port builds the message itself and asserts `expect`.

For Rust it lands here and nowhere else. corelib-rs owns no wrapper-array
container at all: under the ARCHITECTURE §8 rule only `PayloadAcc` moved into it
(corelib-rs#87), and the placement and the growth stayed **generated**, because
there was no uniform shape to hoist. A §7.2 item 8 test placed in corelib-rs
would have to bring its own destination and would then assert its own logic
rather than the library's. The obligation is real; it belongs to the layer that
owns the behaviour, which is the code this repo emits.

The same argument reaches further than Rust. Every backend's *element-index
check* -- "bound the index before the container grows" -- sits in generated code,
so no corelib test can make it fail. This driver is the only place it is
reachable, which is why it is a shared driver rather than a Rust-only script.

## The messages

`--emit-schema` prints two messages, so the block's `field_id: 0` is honoured
literally rather than remapped:

    wstr -- a wrapper array of `string` at id 0   (element_type "string")
    wobj -- a wrapper array of a one-field struct at id 0 (element_type "struct")

Both are deliberately **unbounded** (no `count`): a schema bound would be the
field's own bound and would decide the outcome before any receiver cap could,
which is the opposite of what these cases measure.

The two kinds are not redundant. A `string` element reaches the container
through the leaf path and a `struct` element through the sequence path, and an
implementation can get one right and the other wrong.

## The cap

Indices are **cap-relative**: `id_from_cap` and `length_from_cap` are offsets
onto the target's own configured `max_dyn_array_count`, because CORELIB_PLAN
§6.2.1 deliberately fixes no family-wide number. `--cap` states the number this
run configured -- the caller must pass the same value it generated with -- and
the driver resolves every relative index against it. Cases assume at least 4.

## What is asserted, and what cannot be

Per the block's own rule: the **outcome** and the **container length**, nothing
else -- no allocator instrumentation, which is what keeps the cases portable.

  outcome `complete`       -> harness exits 0; the decoded array's length is
                              compared, and `default_ids` are compared against
                              the element default.
  outcome `limit_exceeded` -> harness exits non-zero with a LimitExceeded-class
                              category. Not INVALID: the bytes are well-formed
                              and decode under a looser cap.
  `terminal`               -> covered by the cases that deliver a further legal
                              element *behind* the rejected one: it is fed in the
                              same message, so a decoder that recovered would
                              report `complete` and fail the outcome assertion.
  `max_length`             -> NOT asserted, and this is reported rather than
                              quietly skipped. It asks how far the container was
                              extended *before* a rejection, and a fallible
                              `decode` hands back an error, not a partial
                              container -- the JSON harness has no surface on
                              which a partly-grown array is observable. Same
                              reasoning the block's README gives for growth
                              geometry: say so rather than report the case passed.

`requires: ["dynamic_arrays"]` gates the whole block. A statically bounded
profile (C, C++ `corelib: c-cpp`, Rust `no_std`) is capacity-bound by
construction, never grows, and must not run this -- an unsatisfied
`dynamic_arrays` means *skip*, never *reject*. Such a target simply does not call
this driver; there is no flag for it here, because a driver that decided its own
applicability is exactly the quiet narrowing this suite refuses.
"""
import json
import re
import subprocess
import sys

STR_FIELD = "a"          # the wrapper array field in both messages
STRUCT_VALUE_FIELD = "k"  # the struct element's one unsigned field, id 0
MSG = {"string": "wstr", "struct": "wobj"}


def emit_schema() -> int:
    print("# The sequence_growth messages (generator#449), printed by")
    print("# tests/conformance/lib/check_growth.py so the schema and the driver that")
    print("# replays CORELIB_PLAN S7.2 item 8 against it have one definition between them.")
    print("# Both arrays are UNBOUNDED on purpose: a schema `count` would decide the")
    print("# outcome before any receiver cap could, which is what these cases measure.")
    print("  wstr:")
    print("    payload:")
    print(f"      {STR_FIELD}: {{ id: 0, type: array, items: {{ type: string }} }}")
    print("  wobj:")
    print("    payload:")
    print(f"      {STR_FIELD}: {{ id: 0, type: array, items: {{ type: struct,"
          f" fields: {{ {STRUCT_VALUE_FIELD}: {{ id: 0, type: u32 }} }} }} }}")
    return 0


def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def build(case, cap: int) -> bytes:
    """The wire image for one growth case.

    Headers are `(id << 3) | wire type` (MESSAGE_SPEC §4.3); a wrapper array is
    wire type 6 (sequence) closed by the end marker 0x07 (§4.9), and each
    element's id IS its array index (§5.1). A string element is a fixlen leaf,
    whose `fixlen_word` is `(length << 3) | subtype` with subtype 2 = string
    (§4.6). A struct element is itself a framed sub-sequence carrying one
    unsigned field at id 0.
    """
    fid = case["field_id"]
    out = bytearray(varint((fid << 3) | 6))
    for el in case["deliver"]:
        idx = el["id"] if "id" in el else cap + el["id_from_cap"]
        if case["element_type"] == "string":
            payload = el["value"].encode()
            out += varint((idx << 3) | 2)
            out += varint((len(payload) << 3) | 2)
            out += payload
        else:
            out += varint((idx << 3) | 6)      # element frame
            out += varint((0 << 3) | 0)        # unsigned field, id 0
            out += varint(el["value"])
            out += b"\x07"                     # close the element
    out += b"\x07"                             # close the array
    return bytes(out)


def wanted_length(case, cap: int):
    e = case["expect"]
    return e["length"] if "length" in e else cap + e["length_from_cap"]


def decoded_array(payload, label):
    """The wrapper array out of a harness's decode JSON, or a reason string."""
    try:
        obj = json.loads(payload.decode())
    except ValueError:
        return None, f"harness printed no JSON: {payload.decode(errors='replace')[:200]!r}"
    if STR_FIELD not in obj:
        return None, f"decoded object has no field {STR_FIELD!r}: {sorted(obj)}"
    arr = obj[STR_FIELD]
    # JSON null is an empty container, not a missing one. Go marshals a nil
    # slice to `null` and a nil slice has length 0, so the two render
    # differently and mean the same thing here; the cases assert a length, and
    # every language that can produce `null` produces it for the zero length.
    if arr is None:
        return [], None
    if not isinstance(arr, list):
        return None, f"field {STR_FIELD!r} decoded as {type(arr).__name__}, not a list"
    return arr, None


def is_default_element(case, el) -> bool:
    """The element default: the empty string for a leaf, an all-default struct."""
    if case["element_type"] == "string":
        return el == ""
    if isinstance(el, dict):
        return all(v in (0, "0", None) for v in el.values())
    return el in (0, None)


def limit_exceeded(text: str) -> bool:
    """True when the harness reported the LimitExceeded *category*.

    The eleven backends spell it differently -- measured, not guessed:
    `LimitExceeded` (rust, cs), `decode limit exceeded` (go),
    `SofaLimitError: ... exceeds max_array_count` (python), `LIMIT_EXCEEDED`
    (kotlin) -- so matching a literal would be matching one port's vocabulary.

    What every one of them has in common is the word "limit", and what none of
    the other two terminal categories has is that word: the INVALID family is
    spelled InvalidMsg / INVALID_MSG / InvalidMessage / SofaInvalidError, and the
    INCOMPLETE family Incomplete / INCOMPLETE / incomplete message. So "limit,
    and neither of the others" is the discriminator, and it is exactly the
    distinction these cases are making: the bytes are well-formed and decode
    under a looser cap, so a rejection MUST be the policy category and not the
    malformed one (CORELIB_PLAN §6.2.1/§6.3).
    """
    flat = re.sub(r"[^a-z]", "", text.lower())
    return "limit" in flat and "invalid" not in flat and "incomplete" not in flat


# Lines that are build noise rather than the harness's verdict. `cargo run`
# re-emits the crate's warnings, a JVM prints a stack trace under its exception,
# and rustc's diagnostics carry `-->`/`|`/`=`/`help:` continuation lines. A
# verdict drowned in those is a test nobody can read -- and, worse, matching the
# CATEGORY against them would read a warning's wording as the outcome.
_NOISE = re.compile(
    r"^(at\s|warning\b|note:|help:|-->|\||=\s|\d+\s*\||\^|Note:|WARNING:|SLF4J|Picked up )")


def meaningful(stderr: bytes) -> list:
    """The harness's own output lines, with build noise and stack frames gone."""
    return [l.strip() for l in stderr.decode(errors="replace").splitlines()
            if l.strip() and not _NOISE.match(l.strip())]


def diagnostic(stderr: bytes) -> str:
    lines = meaningful(stderr)
    return " | ".join(lines[-2:]) if lines else "<no output>"


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()

    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    vectors_path, label = head[0], head[1]
    cwd = opt(head, "--cwd")
    cap = int(opt(head, "--cap"))
    if cap < 4:
        print(f"FAIL: the growth cases assume a cap of at least 4; --cap {cap} "
              f"cannot resolve their cap-relative indices")
        return 1

    data = json.load(open(vectors_path))
    cases = data.get("sequence_growth")
    if not cases:
        print("FAIL: this test_vectors.json carries no `sequence_growth` block -- "
              "the corelib checkout predates CORELIB_PLAN §7.2 item 8")
        return 1

    ran = unobservable = 0
    for c in cases:
        msg = MSG[c["element_type"]]
        wire = build(c, cap)
        p = subprocess.run(cmd + ["decode", msg], input=wire, cwd=cwd,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        clean = meaningful(p.stderr)
        err = diagnostic(p.stderr)
        want = c["expect"]["outcome"]

        if want == "complete":
            if p.returncode != 0:
                print(f"FAIL case {c['name']}: must decode COMPLETE, exited "
                      f"{p.returncode}: {err}")
                return 1
            arr, why = decoded_array(p.stdout, label)
            if why:
                print(f"FAIL case {c['name']}: {why}")
                return 1
            n = wanted_length(c, cap)
            if len(arr) != n:
                print(f"FAIL case {c['name']}: array is {len(arr)} long, want {n} "
                      f"(length is highest present id + 1, MESSAGE_SPEC §5.1); got {arr}")
                return 1
            for gap in c["expect"].get("default_ids", []):
                if not is_default_element(c, arr[gap]):
                    print(f"FAIL case {c['name']}: index {gap} is an omitted interior "
                          f"element and must hold the element default; got {arr[gap]!r}")
                    return 1
        else:
            if p.returncode == 0:
                print(f"FAIL case {c['name']}: an element index at the cap must be "
                      f"refused; decoded {p.stdout.decode(errors='replace').strip()}")
                return 1
            if not limit_exceeded(" ".join(clean)):
                print(f"FAIL case {c['name']}: the rejection must be the "
                      f"LimitExceeded category, not the malformed one -- the bytes are "
                      f"well-formed and decode under a looser cap (§6.2.1); got:\n"
                      + "\n".join("      " + l for l in clean[-10:]))
                return 1
            # `max_length` asks how far the container grew BEFORE the rejection.
            # A fallible decode returns an error, not a partial container, so the
            # JSON harness cannot show it. Counted and reported, never silently
            # treated as asserted.
            unobservable += "max_length" in c["expect"]
        ran += 1

    if ran != len(cases):
        print(f"FAIL: ran {ran} of {len(cases)} growth cases")
        return 1
    print(f"{label} sequence_growth (CORELIB_PLAN §7.2 item 8): {ran} cases at "
          f"max_dyn_array_count={cap}, both element kinds; {unobservable} of them "
          f"also carry `max_length`, which a fallible decode cannot expose "
          f"(no partial container) and which is therefore NOT asserted")
    return 0 if ran else 1


if __name__ == "__main__":
    sys.exit(main())
