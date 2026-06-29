#!/usr/bin/env python3
"""Drive the generated TS harness against the shared wire vectors (byte-exact).

Usage: check_vectors.py <test_vectors.json> <conf-project-dir>
For each single-field, id-0 scalar vector it feeds {"a": value} to
`npx tsx harness.ts encode <message>` and compares the hex output to the vector.
64-bit values are passed as JSON strings (the TS harness parses them with BigInt).
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


def main() -> int:
    vectors_path, proj = sys.argv[1], sys.argv[2]
    data = json.load(open(vectors_path))
    checked = 0
    for v in data["vectors"]:
        if len(v["fields"]) != 1 or v.get("offset", 0) != 0:
            continue
        f = v["fields"][0]
        msg = OP_TO_MSG.get(f["op"])
        if msg is None or f["id"] != 0:
            continue
        op, val = f["op"], f["value"]
        if op in ("fp32", "fp64") and isinstance(val, str):  # inf/-inf
            continue
        if op in ("unsigned", "signed"):
            payload = {"a": str(val)}  # bigint via string
        else:
            payload = {"a": val}
        out = subprocess.run(
            ["npx", "tsx", "harness.ts", "encode", msg],
            input=json.dumps(payload).encode(),
            cwd=proj,
            stdout=subprocess.PIPE,
            check=True,
        ).stdout
        got = out.hex()
        want = v["serialized"]["hex"]
        if got != want:
            print(f"FAIL vector {v['name']}: got {got} want {want}")
            return 1
        checked += 1
    print(f"TypeScript shared-vector conformance: {checked} byte-exact")
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
