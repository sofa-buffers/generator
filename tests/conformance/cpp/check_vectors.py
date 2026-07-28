#!/usr/bin/env python3
"""Drive the generated C++ harness against the shared wire vectors (byte-exact).

Usage: check_vectors.py <test_vectors.json> <harness-binary>
For each single-field, id-0 scalar vector it feeds {"a": value} to
`<harness> encode <message>` and compares the hex output byte-for-byte to the
vector's `serialized_sparse` — the sparse-canonical bytes a generated encoder must
produce (MESSAGE_SPEC S2): empty for a default-valued field, else the dense bytes.
The vectorgen (corelib-c-cpp) is the single source of truth; this driver no longer
re-derives "is this value the default".

Two wrapper-ARRAY shapes are recognised on top of the scalars, one per element
kind, because S2's element rule is where the two kinds have to agree: a string
array (-> `vecsa`) and a struct array (-> `vecpa`). They are what pins that an
interior element equal to its element default leaves an id gap while the last one
survives — as its value for a leaf, as an empty frame for a sequence element.
"""
import json
import subprocess
import sys

OP_TO_MSG = {"unsigned": "vecu", "signed": "veci", "fp32": "vecf32", "fp64": "vecf64", "string": "vecs"}


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


def struct_array_values(fields):
    """Ordered `k` values when `fields` is a single id-0 wrapper sequence whose
    children are per-element sub-sequences carrying at most one unsigned child at
    id 0 — the shape of the MESSAGE_SPEC §2 sequence-ELEMENT vectors
    (array_struct_interior_default_element, array_struct_all_default_elements);
    else None. Encoded against the `vecpa` harness message.

    An element that carries no child at all is an all-default struct, i.e. k == 0.
    Element ids must run 0, 1, 2, … : the vectors are the DENSE ops the sparse
    pass then drops from, so a gap here would mean this driver had guessed the
    array's length rather than read it.
    """
    if len(fields) < 2 or fields[0].get("op") != "sequence_begin" or fields[0].get("id") != 0:
        return None
    if fields[-1].get("op") != "sequence_end":
        return None
    mid, out, i = fields[1:-1], [], 0
    while i < len(mid):
        if mid[i].get("op") != "sequence_begin" or mid[i].get("id") != len(out):
            return None
        i += 1
        val = 0
        if i < len(mid) and mid[i].get("op") == "unsigned" and mid[i].get("id") == 0:
            val = mid[i]["value"]
            i += 1
        if i >= len(mid) or mid[i].get("op") != "sequence_end":
            return None
        i += 1
        out.append(val)
    return out or None


def main() -> int:
    vectors_path, harness = sys.argv[1], sys.argv[2]
    data = json.load(open(vectors_path))
    checked = 0
    for v in data["vectors"]:
        if v.get("offset", 0) != 0:
            continue
        arr = string_array_values(v["fields"])
        objs = struct_array_values(v["fields"])
        if arr is not None:
            msg, payload = "vecsa", json.dumps({"a": arr})
        elif objs is not None:
            msg, payload = "vecpa", json.dumps({"a": [{"k": k} for k in objs]})
        elif len(v["fields"]) == 1:
            f = v["fields"][0]
            msg = OP_TO_MSG.get(f["op"])
            if msg is None or f["id"] != 0:
                continue
            val = f["value"]
            if f["op"] in ("fp32", "fp64") and isinstance(val, str):  # inf/-inf
                continue
            payload = json.dumps({"a": val})
        else:
            continue
        out = subprocess.run(
            [harness, "encode", msg], input=payload.encode(), stdout=subprocess.PIPE, check=True
        ).stdout
        got, want = out.hex(), v["serialized_sparse"]["hex"]
        if got != want:
            print(f"FAIL vector {v['name']}: got {got} want {want}")
            return 1
        checked += 1
    print(f"C++ shared-vector conformance: {checked} byte-exact")
    return 0 if checked else 1


if __name__ == "__main__":
    sys.exit(main())
