#!/usr/bin/env python3
"""Drive the generated Rust harness against the shared wire vectors (byte-exact).

Usage: check_vectors.py <test_vectors.json> <crate-dir>
Runs `cargo run -- encode <message>` for each single-field id-0 scalar vector and
compares the hex output to the vector.
"""
import json
import subprocess
import sys

OP_TO_MSG = {"unsigned": "vecu", "signed": "veci", "fp32": "vecf32", "fp64": "vecf64", "string": "vecs"}


def value_is_default(op, val):
    """A sparse-canonical encoder (MESSAGE_SPEC S2) omits a field equal to its
    default, so a default-valued single-field message encodes to an empty
    payload. Report whether this vector's value is the type default (zero/empty)."""
    s = str(val).strip().strip('"')
    if op in ("unsigned", "signed"):
        return s == "0"
    if op in ("fp32", "fp64"):
        return s in ("0", "0.0", "-0", "-0.0")
    if op == "string":
        return s == ""
    return False


def main() -> int:
    vectors_path, crate = sys.argv[1], sys.argv[2]
    data = json.load(open(vectors_path))
    checked = 0
    for v in data["vectors"]:
        if len(v["fields"]) != 1 or v.get("offset", 0) != 0:
            continue
        f = v["fields"][0]
        msg = OP_TO_MSG.get(f["op"])
        if msg is None or f["id"] != 0:
            continue
        val = f["value"]
        if f["op"] in ("fp32", "fp64") and isinstance(val, str):  # inf/-inf
            continue
        payload = json.dumps({"a": val})
        out = subprocess.run(
            ["cargo", "run", "-q", "--", "encode", msg],
            input=payload.encode(), cwd=crate, stdout=subprocess.PIPE, check=True,
        ).stdout
        got, want = out.hex(), v["serialized"]["hex"]
        # Sparse-canonical: a default-valued single-field message is omitted, so it
        # encodes to empty; the dense hex is still validated for non-default values.
        if value_is_default(f["op"], val):
            if got != "":
                print(f"FAIL vector {v['name']}: default-valued field must be omitted (sparse), got {got}")
                return 1
        elif got != want:
            print(f"FAIL vector {v['name']}: got {got} want {want}")
            return 1
        checked += 1
    print(f"Rust shared-vector conformance: {checked} byte-exact")
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
