#!/usr/bin/env python3
"""A message with no fields round-trips: the ping (generator#738).

Usage:
  check_empty_payload.py --emit-schema
  check_empty_payload.py --self-test
  check_empty_payload.py <label> [--cwd DIR] [--verb VERB] [--no-struct]
                         -- <harness argv...>

## The rule

MESSAGE_SPEC §2 omits every field equal to its default, so the all-default message
IS the empty byte string. A message whose payload declares NO fields is therefore a
legal schema whose every value encodes to zero bytes -- the ping, the signal, the
keep-alive -- and `MAX_SIZE == 0` for it.

## Why a driver on the generated harness

Because no schema in `tests/` or `examples/` had one, and eleven backends had
eleven answers (generator#738): C++ aborted on `corelib: c-cpp` (`std::vector`'s
`data()` is null at size 0 and the C install asserted non-null), Java threw
(`OStream.overScratch(0)` refused the size), the TypeScript project emit PANICKED
(the bench sink folded `fields[0]`), the Zig harness did not compile (an unused
parameter and a `var` that is never mutated are errors there), C++ and Rust warned
where the suite treats a warning as an error, and the C header declared an empty
struct and a zero-length array -- GCC extensions, not ISO C. Every one of those is
invisible to a schema that has a field, which is every other schema in the repo.

The declaration was not the coverage: `payload: {}` validated all along.

## The schema

The driver prints it (`--emit-schema`), so the shapes it asserts and the schema that
is generated cannot drift apart:

    ping   {}                                  -- a message with no fields
    holder { s: struct {}, n: u32 }             -- a field whose struct has none

`--no-struct` drops `holder` for a backend that cannot express a field-less struct;
no suite passes it today.

## The table

    ping_encode    `{}` encodes to EXACTLY zero bytes, and the encode succeeds --
                   a refusal and this message produce the same bytes, so the status
                   is the only thing that separates them (generator#736)
    ping_decode    zero bytes decode as a complete message, every field at its
                   default (there are none), and re-encode to zero bytes again:
                   decode -> encode is the path any relay takes
    holder_encode  `{"n": 7}` encodes to `08 07` -- the field-less struct is equal
                   to its default and is omitted (§2), so it adds no bytes
    holder_struct  the struct carried EXPLICITLY (`{"s": {}, "n": 7}`) is still
                   omitted: an empty sequence of no fields has no value to frame
    holder_decode  those bytes decode back with n = 7, and the struct reads as the
                   object with no members
"""

import json
import subprocess
import sys

import harness_dialect as hd

PING, HOLDER = "ping", "holder"
S, N = 0, 1          # holder's field ids
N_VALUE = 7
HOLDER_WIRE = bytes([(N << 3) | 0, N_VALUE])   # unsigned header + varint (§4.3)


def emit_schema(no_struct=False) -> int:
    print("# The empty-payload messages (generator#738), printed by")
    print("# tests/conformance/lib/check_empty_payload.py so the shapes it asserts and")
    print("# the schema that is generated have one definition between them.")
    print(f"  {PING}:")
    print("    payload: {}")
    if not no_struct:
        print(f"  {HOLDER}:")
        print("    payload:")
        print(f"      s: {{ id: {S}, type: struct, fields: {{}} }}")
        print(f"      n: {{ id: {N}, type: u32 }}")
    return 0


def self_test() -> int:
    if HOLDER_WIRE.hex(" ") != "08 07":
        print(f"FAIL self-test: holder wire is {HOLDER_WIRE.hex(' ')}, want 08 07")
        return 1
    print("self-test: the holder image matches the hand-written bytes (08 07)")
    return 0


def run(cmd, argv, data, cwd):
    p = subprocess.run(cmd + argv, input=data, cwd=cwd,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return p


def tail(b: bytes) -> str:
    lines = [l.strip() for l in b.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:])[:300] if lines else "<no output>"


def check(cmd, cwd, verb, no_struct):
    """Yields a problem string per failing row; nothing when all hold."""
    # --- ping: the message that encodes to nothing --------------------------
    p = run(cmd, ["encode", PING], b"{}", cwd)
    if p.returncode != 0:
        yield f"ping_encode: the message with no fields must encode, harness exited {p.returncode}: {tail(p.stderr)}"
    elif p.stdout != b"":
        yield f"ping_encode: expected zero bytes, got {len(p.stdout)}: {p.stdout[:16].hex(' ')}"

    d = run(cmd, [verb, PING], b"", cwd)
    if d.returncode != 0:
        yield f"ping_decode: zero bytes are a complete message, harness exited {d.returncode}: {tail(d.stderr)}"
    else:
        try:
            got = hd.loads_out(d.stdout.decode())
        except ValueError:
            yield f"ping_decode: printed no JSON: {d.stdout[:120]!r}"
            got = None
        if got not in (None, {}):
            yield f"ping_decode: a message with no fields must read back empty, got {json.dumps(got)[:80]}"
        if got is not None:
            r = run(cmd, ["encode", PING], json.dumps(got).encode(), cwd)
            if r.returncode != 0 or r.stdout != b"":
                yield (f"ping_decode: decode -> encode must come back to zero bytes, "
                       f"got rc {r.returncode} and {len(r.stdout)} byte(s): {tail(r.stderr)}")

    if no_struct:
        return

    # --- holder: a field-less struct is a value equal to its default --------
    for row, sent in (("holder_encode", {"n": N_VALUE}),
                      ("holder_struct", {"s": {}, "n": N_VALUE})):
        h = run(cmd, ["encode", HOLDER], json.dumps(sent).encode(), cwd)
        if h.returncode != 0:
            yield f"{row}: encode failed, harness exited {h.returncode}: {tail(h.stderr)}"
            continue
        if h.stdout != HOLDER_WIRE:
            yield (f"{row}: expected {HOLDER_WIRE.hex(' ')} (the struct is omitted, §2), "
                   f"got {h.stdout.hex(' ')}")
            continue
        if row != "holder_encode":
            continue
        b = run(cmd, [verb, HOLDER], h.stdout, cwd)
        if b.returncode != 0:
            yield f"holder_decode: decode failed, harness exited {b.returncode}: {tail(b.stderr)}"
            continue
        try:
            got = hd.loads_out(b.stdout.decode())
        except ValueError:
            yield f"holder_decode: printed no JSON: {b.stdout[:120]!r}"
            continue
        if hd.as_int(got.get("n")) != N_VALUE:
            yield f"holder_decode: n should read back as {N_VALUE}, got {json.dumps(got.get('n'))}"
        if "s" in got and got["s"] not in ({}, None):
            yield f"holder_decode: the field-less struct must read back empty, got {json.dumps(got['s'])}"


def main(argv) -> int:
    if "--emit-schema" in argv:
        return emit_schema("--no-struct" in argv)
    if "--self-test" in argv:
        return self_test()
    if "--" not in argv:
        print(__doc__)
        return 2
    label = argv[0]
    rest, cmd = argv[: argv.index("--")], argv[argv.index("--") + 1:]
    cwd = rest[rest.index("--cwd") + 1] if "--cwd" in rest else None
    verb = rest[rest.index("--verb") + 1] if "--verb" in rest else "decode"
    no_struct = "--no-struct" in rest

    problems = list(check(cmd, cwd, verb, no_struct))
    rows = 3 if no_struct else 5
    for p in problems:
        print(f"FAIL {label} {p}")
    if problems:
        return 1
    print(f"   {label} empty payload [{verb}]: {rows} row(s) -- the ping encodes to "
          f"nothing, decodes from nothing and round-trips")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
