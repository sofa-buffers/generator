#!/usr/bin/env python3
"""Drive the generated TS harness against the shared wire vectors (byte-exact).

Usage: check_vectors.py <test_vectors.json> <conf-project-dir> [--int64-safe]
For each single-field, id-0 scalar vector it feeds {"a": value} to
`npx tsx harness.ts encode <message>` and compares the hex output byte-for-byte to
the vector's `serialized_sparse` — the sparse-canonical bytes a generated encoder
must produce (MESSAGE_SPEC S2): empty for a default-valued field, else the dense
bytes. 64-bit values are passed as JSON strings (the TS harness parses BigInt).

--int64-safe is for a build generated with `int64: number`, whose 64-bit SCALAR is
a double and documented lossy above 2^53. A vector whose unsigned/signed value
lies outside +-(2^53-1) cannot be encoded exactly from such a scalar, so it is
NOT asserted there; each one is counted and named in the output rather than
dropped quietly. Every other vector is still byte-exact.
"""
import json
import subprocess
import sys

OP_TO_MSG = {
    "unsigned": "vecu",
    "signed": "veci",
    "fp32": "vecf32",
    "fp64": "vecf64",
    "string": "vecs",
}


def string_array_values(fields):
    """Ordered element values when `fields` is a single id-0 wrapper sequence
    whose children are all string ops (a wrapper-array of string) — the shape the
    MESSAGE_SPEC S2 element-omission vectors use; else None. Encoded against the
    `vecsa` harness message."""
    if len(fields) < 2 or fields[0].get("op") != "sequence_begin" or fields[0].get("id") != 0:
        return None
    if fields[-1].get("op") != "sequence_end":
        return None
    mid = fields[1:-1]
    if not mid or any(op.get("op") != "string" for op in mid):
        return None
    return [op["value"] for op in mid]


def main() -> int:
    vectors_path, proj = sys.argv[1], sys.argv[2]
    safe = "--int64-safe" in sys.argv[3:]
    data = json.load(open(vectors_path))
    checked = 0
    unsafe = []
    for v in data["vectors"]:
        if v.get("offset", 0) != 0:
            continue
        arr = string_array_values(v["fields"])
        if arr is not None:
            msg, payload = "vecsa", {"a": arr}
        elif len(v["fields"]) == 1:
            f = v["fields"][0]
            msg = OP_TO_MSG.get(f["op"])
            if msg is None or f["id"] != 0:
                continue
            op, val = f["op"], f["value"]
            if op in ("fp32", "fp64") and isinstance(val, str):  # inf/-inf
                continue
            if op in ("unsigned", "signed"):
                if safe and abs(int(val)) > 2**53 - 1:
                    unsafe.append(v["name"])
                    continue
                payload = {"a": str(val)}  # bigint via string
            else:
                payload = {"a": val}
        else:
            continue
        out = subprocess.run(
            ["npx", "tsx", "harness.ts", "encode", msg],
            input=json.dumps(payload).encode(),
            cwd=proj,
            stdout=subprocess.PIPE,
            check=True,
        ).stdout
        got, want = out.hex(), v["serialized_sparse"]["hex"]
        if got != want:
            print(f"FAIL vector {v['name']}: got {got} want {want}")
            return 1
        checked += 1
    print(f"TypeScript shared-vector conformance: {checked} byte-exact")
    if unsafe:
        print(f"  --int64-safe: {len(unsafe)} vectors above 2^53 not asserted: {', '.join(unsafe)}")
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
