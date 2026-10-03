#!/usr/bin/env python3
"""Drive the shared `header_limits` / `header_limits_nested` tables (generator#651).

Usage:
  check_header_limits.py --emit-schema <test_vectors.json> [--without CAP[,CAP]]
  check_header_limits.py --emit-limits <test_vectors.json> [--without CAP[,CAP]]
  check_header_limits.py <test_vectors.json> <label> [--without CAP[,CAP]]
                         [--cwd DIR] [--verb VERB]
                         (--status-verb VERB [--status-limit NAME] [--status-invalid NAME]
                                             [--status-incomplete NAME] [--status-complete NAME]
                          | --limit-pattern REGEX --invalid-pattern REGEX)
                         [--marker NAME[:INVALID:LIMIT]]... [--invalid-name NAME]
                         [--limit-name NAME] -- <harness argv...>

CORELIB_PLAN §6.2.1 / §6.3: bytes that DECLARE a length or a count and then END
must be answered at the declaration. The ceiling's verdict, `LimitExceeded` for a
receiver cap and `INVALID` for a schema `maxlen`, comes at the length word, is
terminal, and is never INCOMPLETE, which says more bytes could change the answer.
Each rejection has an in-cap control that must still be INCOMPLETE, so a decoder
cannot pass by rejecting everything. `header_limits` puts the field at the top
level; `header_limits_nested` repeats it inside one or two open sequence frames,
where INCOMPLETE is a plausible answer for the wrong reason.

The tables live in the shared vector file and this driver reads them there; nothing
is retyped per language. It prints its own schema so the ids and bounds the rows
breach have one definition.

## The schema and the configuration

One message per distinct shape `(field kind, schema-bounded?, frames)`, named
`hl<N>`: the field sits at id `field_id`, inside one struct per entry of `frames`
(outermost first). A `limits` row leaves the field unbounded, so a receiver cap
governs it; a `schema` row declares `maxlen` on it, so the cap must not.

Each row's `limits` names the cap that applies to it. The rows agree on one value
per cap, so a single project serves them all: `--emit-limits` prints the union as
`max_dyn_*: N` pairs for the suite's generic config, and fails if two rows ask for
different values of one cap. With `--without receiver_caps` there is nothing to
print and no `limits` row is generated.

## Capabilities

A row's `requires` names what the target must have: `fixlen`, `array`,
`sequence`, `int64`, `receiver_caps`. A target without one passes `--without`, and
every row that needs it is SKIPPED, counted and printed with the capability that
is missing. `--without` must be identical on `--emit-schema`, `--emit-limits`
and the run. Footprint profiles (C, C++ over corelib-c-cpp, Rust over
rs-no-std) have no receiver caps; they still run the schema-bounded rows.

## The verdict channel

A category is asserted, never an exit status alone: a decoder that answers
INCOMPLETE exits non-zero too.

  * `--status-verb` -- a verb printing the verdict name on line 1; compared with
    `--status-limit`, `--status-invalid`, `--status-incomplete` and
    `--status-complete` (the last only so a wrongly accepted message reads as
    such in the failure).
  * `--limit-pattern` / `--invalid-pattern` -- regexes over the output of
    `--verb` (default `decode`). A row that is not LimitExceeded and not INVALID
    is read as INCOMPLETE, and a refusal row must match its own pattern and not
    the other.

TERMINAL (`expect.terminal`): with `--marker` the harness prints a second verdict
after asking again, `[NAME=X]`, and X must be the category the row breached
(`check_terminal_refusal.py` has the same contract). Without a marker the row
asserts the verdict only, and the run says so.

A row with `chunks` is fed through `--verb streamdecode` one byte at a time,
which cuts the length varint itself; on the one-shot verb it is one buffer, the
same bytes as the unsplit row.
"""

import argparse
import json
import os
import re
import subprocess
import sys

CAPS = ("fixlen", "array", "sequence", "int64", "receiver_caps")
LIMIT_KEYS = {"max_dyn_string_len": "string", "max_dyn_blob_len": "blob",
              "max_dyn_array_count": "array"}
TABLES = ("header_limits", "header_limits_nested")
PREFIX = "hl"
ORDER = {"limit_exceeded": "limit", "invalid": "invalid", "incomplete": "incomplete"}


def die(msg):
    print("FAIL: " + msg)
    sys.exit(1)


def load(path):
    with open(path) as fh:
        V = json.load(fh)
    rows = []
    for table in TABLES:
        if table not in V:
            die(f"{path} has no `{table}` block")
        rows += [dict(r, _table=table) for r in V[table]]
    return rows


def kind_of(row):
    """The field kind a row is about, from its name: string, blob or array."""
    for k in ("string", "blob", "array"):
        if k in row["name"]:
            return k
    die(f"row {row['name']}: cannot tell the field kind (string/blob/array)")


def shape_of(row):
    return (kind_of(row), "schema" in row, tuple(row.get("frames", ())))


def runnable(rows, without):
    """(run, skipped): skipped is [(row, missing capability)]."""
    run, skipped = [], []
    for r in rows:
        missing = [c for c in r.get("requires", []) if c in without]
        unknown = [c for c in r.get("requires", []) if c not in CAPS]
        if unknown:
            die(f"row {r['name']}: unknown capability {unknown}; add it to CAPS")
        (skipped.append((r, missing[0])) if missing else run.append(r))
    return run, skipped


def field_body(shape, row):
    kind, bounded, _ = shape
    if kind == "array":
        return {"type": "array", "items": {"type": "u32"}}
    body = {"type": kind}
    if bounded:
        body["maxlen"] = row["schema"]["maxlen"]
    return body


def schema_of(shape, row):
    """The payload of the message for `shape`: the field, wrapped in one struct per
    frame, outermost first."""
    inner = {f"f{row['field_id']}": {"id": row["field_id"], **field_body(shape, row)}}
    for fid in reversed(shape[2]):
        inner = {f"f{fid}": {"id": fid, "type": "struct", "fields": inner}}
    return inner


def shapes_of(rows):
    shapes, first = [], {}
    for r in rows:
        s = shape_of(r)
        if s not in shapes:
            shapes.append(s)
            first[s] = r
    return shapes, first


def emit_schema(rows):
    shapes, first = shapes_of(rows)
    print("# The header-ceiling messages (generator#651), printed by")
    print("# tests/conformance/lib/check_header_limits.py from the vector file's own")
    print("# header_limits / header_limits_nested blocks.")
    for n, s in enumerate(shapes):
        print(f"  {PREFIX}{n}: "
              f"{json.dumps({'payload': schema_of(s, first[s])}, separators=(', ', ': '))}")
    return 0


def limits_of(rows):
    out = {}
    for r in rows:
        for key, val in r.get("limits", {}).items():
            if key not in LIMIT_KEYS:
                die(f"row {r['name']}: unknown limit {key}; add it to LIMIT_KEYS")
            if out.setdefault(key, val) != val:
                die(f"rows disagree on {key}: {out[key]} vs {val} (row {r['name']}); "
                    f"one project can no longer serve them all")
    return out


def emit_limits(rows):
    print(", ".join(f"{k}: {v}" for k, v in sorted(limits_of(rows).items())))
    return 0


def rc_varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | 0x80 if n else b)
        if not n:
            return bytes(out)


def spawn_raw(argv, cwd, data):
    p = subprocess.run(argv, cwd=cwd, input=data, stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE)
    return p.returncode, p.stdout, p.stderr


def spawn(argv, cwd, data):
    rc, out, err = spawn_raw(argv, cwd, data)
    return rc, out.decode("utf-8", "replace"), err.decode("utf-8", "replace")


def parse_marker(spec, inv, lim):
    parts = spec.split(":")
    if len(parts) == 1:
        return parts[0], inv, lim
    if len(parts) == 3:
        return parts[0], parts[1], parts[2]
    die(f"--marker {spec!r}: want NAME or NAME:INVALID:LIMIT")


def add_channel_args(ap):
    """The verdict-channel options, shared with `check_invalid_utf8.py`."""
    ap.add_argument("--cwd", default=None)
    ap.add_argument("--verb", default="decode")
    ap.add_argument("--status-verb", default=None)
    ap.add_argument("--status-limit", default="LIMITEXCEEDED")
    ap.add_argument("--status-invalid", default="INVALID")
    ap.add_argument("--status-incomplete", default="INCOMPLETE")
    ap.add_argument("--status-complete", default="COMPLETE")
    ap.add_argument("--limit-pattern", default=None)
    ap.add_argument("--invalid-pattern", default=None)


def need_channel(args):
    if args.status_verb is None and not (args.limit_pattern and args.invalid_pattern):
        die("no category channel: give --status-verb or both --limit-pattern and "
            "--invalid-pattern (an exit status cannot tell INCOMPLETE from a refusal)")


def verdict(args, harness, msg, wire):
    """Run the harness on `wire` and name the category it answered: "limit",
    "invalid" or "incomplete". Returns (category, output text)."""
    verb = args.status_verb or args.verb
    rc, out, err = spawn(harness + [verb, msg], args.cwd, wire)
    text = out + err
    if args.status_verb:
        first = out.strip().splitlines()[0].strip() if out.strip() else ""
        got = {args.status_limit: "limit", args.status_invalid: "invalid",
               args.status_incomplete: "incomplete",
               args.status_complete: "complete"}.get(first)
        if got is None:
            die(f"[{args.verb}] status verb printed {first!r}, which is none of "
                f"{args.status_limit}/{args.status_invalid}/{args.status_incomplete}/"
                f"{args.status_complete}\n{text.strip()}")
        return got, text
    lim = re.search(args.limit_pattern, text) is not None
    inv = re.search(args.invalid_pattern, text) is not None
    if lim and inv:
        die(f"output names BOTH categories\n{text.strip()}")
    return ("limit" if lim else "invalid" if inv else "incomplete"), text


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("vectors")
    ap.add_argument("label", nargs="?")
    ap.add_argument("--emit-schema", action="store_true")
    ap.add_argument("--emit-limits", action="store_true")
    ap.add_argument("--without", default="")
    add_channel_args(ap)
    ap.add_argument("--marker", action="append", default=[])
    ap.add_argument("--invalid-name", default=None)
    ap.add_argument("--limit-name", default=None)

    argv = sys.argv[1:]
    harness = []
    if "--" in argv:
        sep = argv.index("--")
        argv, harness = argv[:sep], argv[sep + 1:]
    args = ap.parse_args(argv)

    without = {c for c in args.without.split(",") if c}
    bad = without - set(CAPS)
    if bad:
        die(f"--without {', '.join(sorted(bad))}: not one of {', '.join(CAPS)}")
    rows, skipped = runnable(load(args.vectors), without)
    if args.emit_schema:
        return emit_schema(rows)
    if args.emit_limits:
        return emit_limits(rows)

    if not args.label or not harness:
        die("need a label and a harness argv after `--`")
    need_channel(args)
    markers = [parse_marker(m, args.invalid_name, args.limit_name) for m in args.marker]
    for name, inv, lim in markers:
        if inv is None or lim is None:
            die(f"--marker {name} needs --invalid-name and --limit-name (or NAME:INVALID:LIMIT)")

    shapes, _ = shapes_of(rows)
    ran = {t: 0 for t in TABLES}
    total = {t: 0 for t in TABLES}
    for r, _ in skipped:
        total[r["_table"]] += 1
    terminal = 0
    for r in rows:
        total[r["_table"]] += 1
        msg = f"{PREFIX}{shapes.index(shape_of(r))}"
        wire = bytes.fromhex(r["serialized"])
        want = ORDER[r["expect"]["outcome"]]
        got, text = verdict(args, harness, msg, wire)
        if got != want:
            die(f"[{args.label}] {r['name']} ({r['group']}): want {r['expect']['outcome']}, "
                f"got {got}; declared {r['declared']} at id {r['field_id']}"
                f"{' in frames ' + str(r['frames']) if r.get('frames') else ''}, "
                f"bytes {r['serialized']}\n{text.strip()}")
        if want != "incomplete" and markers and args.verb == "streamdecode" \
                and not args.status_verb:
            # TERMINAL: the harness asked again after the refusal and printed the answer.
            for mark, inv_name, lim_name in markers:
                m = re.search(r"\[%s=([^\]]*)\]" % re.escape(mark), text)
                exp = inv_name if want == "invalid" else lim_name
                if not m:
                    die(f"[{args.label}] {r['name']}: no [{mark}=...] marker after the "
                        f"refusal, so terminality is unasserted\n{text.strip()}")
                if m.group(1) != exp:
                    die(f"[{args.label}] {r['name']}: a refusal is terminal "
                        f"(CORELIB_PLAN §6.3): [{mark}] must stay {exp!r}, got "
                        f"{m.group(1)!r}\n{text.strip()}")
            terminal += 1
        ran[r["_table"]] += 1

    why = {}
    for r, cap in skipped:
        why.setdefault(cap, []).append(r["name"])
    refusals = sum(1 for r in rows if r["expect"]["outcome"] != "incomplete")
    print(f"   [{args.label}] header_limits: {ran['header_limits']}/{total['header_limits']}, "
          f"header_limits_nested: {ran['header_limits_nested']}/{total['header_limits_nested']} "
          f"[{args.status_verb or args.verb}]")
    if markers and args.verb == "streamdecode" and not args.status_verb:
        print(f"   [{args.label}] terminal asserted on {terminal} of {refusals} refusals "
              f"(markers {', '.join(m[0] for m in markers)})")
    elif refusals:
        print(f"   [{args.label}] terminality not asserted on this surface "
              f"({refusals} refusals; no --marker on {args.status_verb or args.verb})")
    for cap, names in sorted(why.items()):
        print(f"   [{args.label}] skipped {len(names)}: target lacks `{cap}` "
              f"({', '.join(names)})")
    if not rows:
        die("every row was skipped; the driver would assert nothing")
    return 0


if __name__ == "__main__":
    sys.exit(main())
