#!/usr/bin/env python3
"""max_message_size: the ceiling never refuses a legal message (generator#637).

Usage:
  check_max_message_size.py --emit-schema
  check_max_message_size.py --emit-bounded-schema
  check_max_message_size.py encode <label> --ceiling N [--cwd DIR] -- <harness argv...>
  check_max_message_size.py budget <label> <lang> --root DIR --work DIR
                            [--target-config 'k: v, k2: v2'] [--static]

ARCHITECTURE §9.6: for a message with an unbounded field `max_message_size` is
an IMPOSED ceiling (`MAX_SIZE_LIMIT`), not a bound. It must not size a buffer and
must not make an encode fail: a message above it is legal. Before this driver no
suite set the key, so a backend that sized its one-shot `encode()` buffer from it
(Java and C# did) passed everything while refusing every message above 4096 bytes.

## `encode`

Runs against a harness built from the `--emit-schema` messages with the ceiling
`--ceiling` (the caller must generate with the same number; 4096 is the default
when the key is unset). For each message -- an unbounded string, blob and native
unsigned array -- it encodes values below, at and well above the ceiling (up to
more than 4x of it) through the harness `encode` verb and asserts:

  * the encode succeeds,
  * the bytes equal the wire this driver builds itself (one field, so the wire
    is a header, a length word and the payload -- MESSAGE_SPEC §4),
  * `decode` hands the same value back.

## `budget`

The generate-time half of the key: a BOUNDED schema whose worst case exceeds an
explicit `max_message_size` fails generation, and the same schema generates under
a larger one. `--static` marks a target that rejects an unbounded field at
generate time (c, cpp `corelib: c-cpp`, rust `no_std`): there the unbounded
schema must fail generation too, and the `encode` leg does not apply.
"""
import json
import os
import subprocess
import sys

MESSAGES = {"us": "s", "ub": "b", "ua": "a"}
SMALL_BUDGET = 64
MAX_ARRAY_ELEMENTS = 8000
BOUNDED_BLOB_MAXLEN = 100  # worst case 103 bytes, above SMALL_BUDGET


def emit_schema() -> int:
    print("# The max_message_size messages (generator#637), printed by")
    print("# tests/conformance/lib/check_max_message_size.py. Every field is UNBOUNDED")
    print("# on purpose: only then is max_message_size an imposed ceiling.")
    print("  us:")
    print("    payload:")
    print("      s: { id: 0, type: string }")
    print("  ub:")
    print("    payload:")
    print("      b: { id: 0, type: blob }")
    print("  ua:")
    print("    payload:")
    print("      a: { id: 0, type: array, items: { type: u32 } }")
    return 0


def emit_bounded_schema() -> int:
    print("version: 1")
    print("messages:")
    print("  bm:")
    print("    payload:")
    print(f"      b: {{ id: 0, type: blob, maxlen: {BOUNDED_BLOB_MAXLEN} }}")
    return 0


def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def blob_bytes(n: int) -> list:
    return [(i * 7 + 3) % 256 for i in range(n)]


def array_values(n: int) -> list:
    # Mixed varint widths; never the default, and not all one width.
    return [(i * 2654435761) % 4000000000 + 1 for i in range(n)]


def case(msg: str, n: int):
    """(json input, expected wire bytes, decoded value) for a payload of n."""
    if msg == "us":
        text = "x" * n
        wire = varint((0 << 3) | 2) + varint((n << 3) | 2) + text.encode()
        return {"s": text}, wire, text
    if msg == "ub":
        data = blob_bytes(n)
        wire = varint((0 << 3) | 2) + varint((n << 3) | 3) + bytes(data)
        return {"b": data}, wire, data
    vals = array_values(n)
    wire = varint((0 << 3) | 3) + varint(n) + b"".join(varint(v) for v in vals)
    return {"a": vals}, wire, vals


def same_value(msg: str, got, want) -> bool:
    if msg == "ub" and isinstance(got, str):
        import base64
        return list(base64.b64decode(got)) == want
    return got == want


def sizes(ceiling: int):
    # Below, at (either side of the encoded size reaching it), and well above:
    # the last is more than 4x the ceiling, several KB for the default one.
    return sorted({1, ceiling // 2, ceiling - 8, ceiling, ceiling + 8, 4 * ceiling + 17})


def run(cmd, data, cwd):
    return subprocess.run(cmd, input=data, cwd=cwd, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE)


def tail(stderr: bytes) -> str:
    lines = [l.strip() for l in stderr.decode(errors="replace").splitlines() if l.strip()]
    return " | ".join(lines[-2:]) if lines else "<no output>"


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def do_encode(argv) -> int:
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]
    label = head[1]
    ceiling = int(opt(head, "--ceiling"))
    cwd = opt(head, "--cwd")
    biggest = 0
    ran = 0
    for msg in MESSAGES:
        for n in sizes(ceiling):
            # The array's element count stays under every target's default
            # max_dyn_array_count (the smallest is 16384): the cap is a receiver
            # policy this driver is not measuring, and 8000 five-byte elements
            # are still ~10x the default ceiling.
            n = min(n, MAX_ARRAY_ELEMENTS) if msg == "ua" else n
            obj, want_wire, want_val = case(msg, n)
            enc = run(cmd + ["encode", msg], json.dumps(obj).encode(), cwd)
            if enc.returncode != 0:
                print(f"FAIL: [{label}] {msg} of {n} elements: encode refused a message "
                      f"above the ceiling {ceiling}; max_message_size must never bound an "
                      f"unbounded encode (ARCHITECTURE §9.6): {tail(enc.stderr)}")
                return 1
            if enc.stdout != want_wire:
                print(f"FAIL: [{label}] {msg} of {n} elements (ceiling {ceiling}): encoded "
                      f"{len(enc.stdout)} bytes, want {len(want_wire)}; first bytes "
                      f"{enc.stdout[:12].hex()} vs {want_wire[:12].hex()}")
                return 1
            dec = run(cmd + ["decode", msg], enc.stdout, cwd)
            if dec.returncode != 0:
                print(f"FAIL: [{label}] {msg} of {n} elements: decode of its own bytes "
                      f"failed: {tail(dec.stderr)}")
                return 1
            try:
                got = json.loads(dec.stdout.decode())[MESSAGES[msg]]
            except (ValueError, KeyError):
                print(f"FAIL: [{label}] {msg} of {n} elements: decode printed no "
                      f"{MESSAGES[msg]!r} field: {dec.stdout.decode(errors='replace')[:200]!r}")
                return 1
            if not same_value(msg, got, want_val):
                print(f"FAIL: [{label}] {msg} of {n} elements did not round-trip")
                return 1
            ran += 1
            if len(want_wire) > biggest:
                biggest = len(want_wire)
    print(f"   [{label}] max_message_size: unbounded message of {biggest} bytes "
          f"(> ceiling {ceiling}) encoded byte-exact and round-tripped "
          f"({ran} encodes, string/blob/array)")
    if biggest <= 4 * ceiling:
        print(f"FAIL: [{label}] the largest message ({biggest} bytes) is not above 4x the "
              f"ceiling {ceiling}")
        return 1
    return 0


def do_budget(argv) -> int:
    label, lang = argv[1], argv[2]
    root = opt(argv, "--root")
    work = opt(argv, "--work")
    body = opt(argv, "--target-config", "")
    static = "--static" in argv
    os.makedirs(work, exist_ok=True)

    def generate(schema_text, budget, tag):
        sch = os.path.join(work, f"{tag}.yaml")
        with open(sch, "w") as fh:
            fh.write(schema_text)
        cfg = os.path.join(work, f"{tag}-cfg.yaml")
        keys = (body + ", " if body else "") + f"max_message_size: {budget}"
        with open(cfg, "w") as fh:
            fh.write(f"targets: {{ {lang}: {{ {keys} }} }}\n")
        out = os.path.join(work, f"{tag}-out")
        return subprocess.run(
            ["go", "run", "./cmd/sofabgen", "--format=off", "--config", cfg, "--lang", lang,
             "--in", sch, "--out", out],
            cwd=root, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

    bounded = subprocess.run([sys.executable, __file__, "--emit-bounded-schema"],
                             stdout=subprocess.PIPE, check=True).stdout.decode()
    p = generate(bounded, SMALL_BUDGET, "over")
    err = p.stderr.decode(errors="replace")
    if p.returncode == 0 or "max_message_size" not in err:
        print(f"FAIL: [{label}] a bounded schema whose worst case (103 bytes) exceeds an "
              f"explicit max_message_size of {SMALL_BUDGET} must fail generation; "
              f"exit {p.returncode}: {err[-300:]}")
        return 1
    p = generate(bounded, 4096, "fits")
    if p.returncode != 0:
        print(f"FAIL: [{label}] the same schema must generate under max_message_size 4096: "
              f"{p.stderr.decode(errors='replace')[-300:]}")
        return 1
    if static:
        unb = subprocess.run([sys.executable, __file__, "--emit-schema"],
                             stdout=subprocess.PIPE, check=True).stdout.decode()
        p = generate("version: 1\nmessages:\n" + unb, 4096, "unbounded")
        if p.returncode == 0:
            print(f"FAIL: [{label}] a fixed-storage target must reject an unbounded "
                  f"field at generate time")
            return 1
    print(f"   [{label}] max_message_size budget: 103-byte message refused under "
          f"{SMALL_BUDGET}, accepted under 4096" + ("; unbounded fields rejected" if static else ""))
    return 0


def main() -> int:
    argv = sys.argv[1:]
    if "--emit-schema" in argv:
        return emit_schema()
    if "--emit-bounded-schema" in argv:
        return emit_bounded_schema()
    if argv and argv[0] == "encode":
        return do_encode(argv)
    if argv and argv[0] == "budget":
        return do_budget(argv)
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main())
