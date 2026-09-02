#!/usr/bin/env python3
"""Assert that where the chunks were cut changes NOTHING (generator#413).

Usage:
  check_chunk_invariance.py <label> <fixture.bin>... --message NAME
                            [--sizes 1,2,3,5,16,0] [--cwd DIR]
                            -- <harness argv...>

CORELIB_PLAN §5.2 makes the decode outcome computable at *any* byte boundary,
§6.0 borrows a fed chunk only for the duration of `feed`, and §5.2.3 fixes the
verdict precedence. Together they state one property, and this driver is that
property written down:

    the verdict AND the decoded value must not depend on the chunk split.

## Why this needs its own driver

A resume bug -- a half-read varint, a payload accumulator that is not carried,
a scope stack that unwinds one level too far -- shows up as *nothing* until the
split lands in the wrong place. The one-shot `decode` path never suspends, so it
cannot see any of them; a single 1-byte pass sees only the boundary that falls
between every pair of bytes, which is not the same thing as a boundary landing
in the middle of a multi-byte construct at an offset the accumulator handles
differently. Feeding the SAME bytes at several sizes is what turns "it resumed
once" into "resumption does not matter", which is what the spec claims.

## What it does, per fixture

The reference is `streamdecode <msg> 0` -- the whole message in ONE feed through
the very same streaming API. Every other size is then that same API fed the same
bytes, cut differently, and must produce the same verdict and the same value.

Making the reference the unchunked STREAM rather than the one-shot `decode` verb
is deliberate. The two are not always the same contract: Java's and C#'s
one-shot `decode` is the documented back-compat *best-effort* surface, which
hands back a half-filled object for a truncated message instead of rejecting,
while `Decoder.finish()`/`Finish()` is strict about end-of-input (ARCHITECTURE
§7). Comparing those two would flag that difference on every truncation fixture
and call it a chunking bug, which it is not. Size `0` has no such gap: it is the
degenerate split of the path under test, and it separates "the streaming path is
wrong" from "it is wrong *when it suspends*".

`--oneshot` adds the cross-check back for the suites where `decode` IS fallible
(python, dart): there the one-shot verdict and value must match too, which
catches a streaming path that is self-consistently wrong at every size.

Every verdict must equal the reference. Every accepted decode must equal the
reference VALUE -- a status-only comparison would pass a decoder that resumes
into the wrong field and quietly returns a different message, which is the whole
class §6.0 exists to prevent.

## Loud, never quiet

The failure mode a coverage test has is passing while checking nothing, so:

  * a fixture that does not exist is a FAILURE, not a skip -- a renamed `.bin`
    must not silently shrink the table (the shape generator#444's driver and the
    upstream C harness both had to be taught);
  * the table must straddle the accept/reject boundary. A streaming path that
    accepts everything, or rejects everything, agrees with itself perfectly at
    every chunk size; such a table is not evidence;
  * the fixture count and the accept/reject split are printed, and `--expect`
    pins the count so a suite that stops building fixtures fails here.
"""
import concurrent.futures
import json
import os
import subprocess
import sys

# Six splits, cheap and deliberately mixed. 1 is every-boundary; 2/3/5 are
# coprime-ish strides that land mid-varint, mid-length-word and mid-payload at
# offsets 1 alone never produces; 16 exceeds most single constructs, so a chunk
# usually spans several whole fields plus a fragment; 0 is the whole buffer.
DEFAULT_SIZES = "1,2,3,5,16,0"


def opt(args, name, default=None):
    return args[args.index(name) + 1] if name in args else default


def workers():
    return min(8, (os.cpu_count() or 2))


def run_one(cmd, cwd, mode, message, size, data):
    """One harness process. `size` is None for the one-shot `decode` surface."""
    argv = cmd + [mode, message]
    if size is not None:
        argv.append(str(size))
    p = subprocess.run(
        argv, input=data, cwd=cwd,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    return p.returncode, p.stdout, p.stderr


def value_of(out):
    """The decoded message as a comparable value.

    Parsed rather than compared as bytes: the two surfaces print through the
    same generated writer, so the text would normally match, but parsing keeps
    the comparison about the VALUE and not about whitespace a future harness
    might emit differently. A harness that printed nothing decodable is itself
    the finding, so a parse error is reported rather than swallowed.
    """
    text = out.decode("utf-8", errors="replace").strip()
    if not text:
        return None, "no output"
    try:
        return json.loads(text), None
    except ValueError as e:
        return None, f"unparseable output: {e}"


def main() -> int:
    argv = sys.argv[1:]
    sep = argv.index("--")
    head, cmd = argv[:sep], argv[sep + 1:]

    label = head[0]
    message = opt(head, "--message")
    cwd = opt(head, "--cwd")
    sizes = [int(s) for s in opt(head, "--sizes", DEFAULT_SIZES).split(",")]
    expect = opt(head, "--expect")
    oneshot = "--oneshot" in head
    # Fixtures are the bare paths in the head; options are name/value pairs, and
    # no option VALUE ends in .bin, so this needs no positional bookkeeping.
    fixtures = [a for a in head[1:] if a.endswith(".bin")]

    if not message:
        print("FAIL: --message is required")
        return 1
    if not fixtures:
        print("FAIL: no fixtures given — this driver must never run empty")
        return 1
    if len(sizes) < 4:
        print(f"FAIL: {len(sizes)} chunk sizes given, at least 4 are required")
        return 1
    if 0 not in sizes:
        # 0 is the reference split; without it there is nothing to compare against.
        print("FAIL: --sizes must include 0 (the whole-buffer reference feed)")
        return 1

    data = {}
    for f in fixtures:
        # A missing fixture is a FAILURE, not a skip. Silently continuing would
        # turn a renamed .bin into a green run that checked less than it says.
        if not os.path.isfile(f):
            print(f"FAIL: fixture {f} missing (renamed?)")
            return 1
        data[f] = open(f, "rb").read()

    # One process per (fixture, surface). Startup dominates -- a JVM or a
    # `dotnet` host costs far more than decoding a few dozen bytes -- and the
    # runs are independent, so a pool turns minutes into seconds. Results are
    # collected by key and judged in fixture order, so which one is reported
    # never depends on which process finished first.
    jobs = [(f, s) for f in fixtures for s in sizes]
    if oneshot:
        jobs += [(f, None) for f in fixtures]
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers()) as pool:
        results = dict(zip(jobs, pool.map(
            lambda j: run_one(cmd, cwd, "decode" if j[1] is None else "streamdecode",
                              message, j[1], data[j[0]]),
            jobs)))

    def where(s):
        return "the whole buffer in one feed" if s == 0 else f"{s}-byte chunks"

    accepted = rejected = 0
    for f in fixtures:
        name = os.path.basename(f)
        # The reference: the same streaming API, fed unchunked.
        rc0, out0, _ = results[(f, 0)]
        ref = "accept" if rc0 == 0 else "reject"
        refval = None
        if ref == "accept":
            refval, err = value_of(out0)
            if err:
                print(f"FAIL {name}: the unchunked reference feed {err}")
                return 1

        legs = [(f"streamdecode at {where(s)}", results[(f, s)]) for s in sizes]
        if oneshot:
            legs.append(("the one-shot decode", results[(f, None)]))

        for leg, (rc, out, errb) in legs:
            got = "accept" if rc == 0 else "reject"
            if got != ref:
                detail = errb.decode(errors="replace").strip()
                print(f"FAIL {name}: unchunked={ref} but {leg}={got}"
                      f"{': ' + detail if detail else ''}")
                return 1
            if ref != "accept":
                continue
            val, err = value_of(out)
            if err:
                print(f"FAIL {name}: {leg} {err}")
                return 1
            if val != refval:
                # The verdict agreeing while the VALUE does not is the resume bug
                # a status-only check cannot see.
                print(f"FAIL {name}: accepted everywhere, but the decoded VALUE "
                      f"differs at {leg}")
                return 1
        if ref == "accept":
            accepted += 1
        else:
            rejected += 1

    if expect is not None and len(fixtures) != int(expect):
        print(f"FAIL: expected {expect} fixtures, checked {len(fixtures)}")
        return 1
    # Both outcomes must appear: a path that accepts everything (or rejects
    # everything) is self-consistent at every chunk size and proves nothing.
    if not (accepted and rejected):
        print(f"FAIL: table is one-sided ({accepted} accept / {rejected} reject)")
        return 1

    sz = ", ".join("whole" if s == 0 else str(s) for s in sizes)
    also = " (+ the one-shot decode)" if oneshot else ""
    print(f"{label} chunk invariance: {len(fixtures)} fixtures "
          f"({accepted} accept, {rejected} reject) x {len(sizes)} splits [{sz}]{also}; "
          f"verdict and value identical at every split")
    return 0


if __name__ == "__main__":
    sys.exit(main())
