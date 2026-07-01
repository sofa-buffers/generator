#!/usr/bin/env python3
"""Drive the generated Java harness against the shared wire vectors (byte-exact).

Usage: check_vectors.py <test_vectors.json> <harness.jar>
Runs `java -jar <harness.jar> encode <message>` for each single-field id-0 scalar
vector and compares the hex output byte-for-byte to the vector's
`serialized_sparse` — the sparse-canonical bytes a generated encoder must produce
(MESSAGE_SPEC S2): empty for a default-valued field, else the dense bytes.
"""
import json
import subprocess
import sys

OP_TO_MSG = {"unsigned": "vecu", "signed": "veci", "fp32": "vecf32", "fp64": "vecf64", "string": "vecs"}


def main() -> int:
    vectors_path, jar = sys.argv[1], sys.argv[2]
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
            ["java", "-jar", jar, "encode", msg],
            input=payload.encode(), stdout=subprocess.PIPE, check=True,
        ).stdout
        got, want = out.hex(), v["serialized_sparse"]["hex"]
        if got != want:
            print(f"FAIL vector {v['name']}: got {got} want {want}")
            return 1
        checked += 1
    print(f"Java shared-vector conformance: {checked} byte-exact")
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
