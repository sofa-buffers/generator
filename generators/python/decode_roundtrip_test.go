package python

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPythonDecodedMessageOwnsItsBytes: a decoded field OWNS its bytes. A
// destination that kept a window into the input buffer would tie the message's
// lifetime to that buffer silently, so every generated destination copies —
// borrowing is deliberately not offered and not configurable.
//
// Nothing here asserts what the destination LOOKS like: the driver overwrites
// the whole input buffer after decoding and re-encodes, which fails for any
// field that turned out to be a view.
//
// It is deliberately not a check on today's corelib. Python's ownership is
// INHERITED — from immutable str/bytes and from BytesIO copying its argument —
// rather than asserted anywhere, and a reader handing the decoder memoryview
// slices would break it without changing a line of emitted code. This is what
// notices.
//
// Both engines run it: corelib-py's pure-Python and native decoders are separate
// implementations, so a green default engine proves nothing about the other.
func TestPythonDecodedMessageOwnsItsBytes(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	for path, content := range genPy(t, schemaFile(t, "../../examples/messages/example.yaml"), map[string]any{}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const driver = `
import sys
import sofab
from message import Myfirstmessage

m = Myfirstmessage()
m.somestring = "héllo wörld"
m.someblob = b"\x01\x02\x03\x04\x05"
m.someuintarray = [9, 8, 7, 6]
m.someintarray = [-1, -2, -3, -4, -5]
m.somefloatarray = [1.5, -2.5, 3.5]
m.somestringarray = ["a", "bb", "ccc"]

want = m.encode()
# A MUTABLE input, so the scribble below is possible at all: decode() must have
# detached from it by the time it returns.
buf = bytearray(want)
got = Myfirstmessage.decode(buf)
for i in range(len(buf)):
    buf[i] ^= 0xFF
again = got.encode()
if again != want:
    sys.exit("[%s] a decoded field aliased the input buffer:\n want %s\n got  %s"
             % (sofab.IMPL, want.hex(), again.hex()))
print("%s ok" % sofab.IMPL)
`
	if err := os.WriteFile(filepath.Join(dir, "driver.py"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	// Which engine the default (no-override) run resolves to. corelib-py falls
	// back to the pure implementation when the compiled extension was never
	// built, and that fallback is silent, so the expectation is probed rather
	// than assumed: asserting "native" outright would fail on a machine without
	// a compiler, and asserting a bare "ok" would let a fallback masquerade as
	// native coverage.
	probe := exec.Command(py, "-c", "import sofab; print(sofab.IMPL)")
	probe.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(corelib, "src"), "SOFAB_PUREPYTHON=")
	implOut, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("probing sofab.IMPL: %v\n%s", err, implOut)
	}
	defaultImpl := strings.TrimSpace(string(implOut))
	if defaultImpl != "native" {
		t.Logf("note: the compiled extension is absent (default engine %q), "+
			"so both legs cover the pure engine only", defaultImpl)
	}

	for _, pure := range []string{"", "1"} {
		cmd := exec.Command(py, filepath.Join(dir, "driver.py"))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PYTHONPATH="+filepath.Join(corelib, "src")+string(os.PathListSeparator)+dir,
			"SOFAB_PUREPYTHON="+pure)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("SOFAB_PUREPYTHON=%q: %v\n%s", pure, err, out)
		}
		// The driver prints the engine it actually ran on, and the match is
		// EXACT: a run that silently fell back to the pure engine reports
		// "python ok", which must not satisfy the native leg.
		want := defaultImpl + " ok"
		if pure == "1" {
			want = "python ok"
		}
		if strings.TrimSpace(string(out)) != want {
			t.Fatalf("SOFAB_PUREPYTHON=%q: expected exactly %q from the driver:\n%s", pure, want, out)
		}
		t.Logf("SOFAB_PUREPYTHON=%q: %s", pure, strings.TrimSpace(string(out)))
	}
}

// TestPythonBoundedElementRejectsBeforeThePayload is generator#377 (Crucible
// F-0062 / G-0039): a bounded blob element of a wrapper array whose declared
// wire length exceeds the element maxlen is INVALID the moment the fixlen_word
// has been read — even when the message then ends before a single payload byte
// arrives (MESSAGE_SPEC §7, CORELIB_PLAN §5.2: INVALID dominates INCOMPLETE for
// a construct the decoder has actually read).
//
// The Python backend used to measure the MATERIALIZED bytes for this one site,
// so a truncated over-maxlen element ran out of input inside d.bytes() and was
// reported INCOMPLETE. It was the one site of five in a generated message.py
// that did not peek with fixlen_len() — the string element one field earlier
// did, and so did the three plain string/blob fields. This is the same ordering
// class as #267 / F-0043 (fixed there for plain fields only).
//
// The three controls are what make this a defect report rather than a
// relaxation: dropping the bound entirely would also turn the first row green.
// With an IN-BOUND declared length the same truncation must stay INCOMPLETE, an
// in-bound complete element must still round-trip, and the sibling string
// element must be unaffected.
//
// Run against the live corelib because the verdict is a property of the
// generated code AND the decoder together: what the emitted source looks like
// is asserted in TestPythonMaxlenReject; what it DOES is asserted here. Both
// engines run it — the two share message.py, but not the decoder underneath it.
func TestPythonBoundedElementRejectsBeforeThePayload(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	// The Crucible probe's shape: a bounded blob element beside the bounded
	// string element it must now behave like, plus a bounded plain blob field
	// (the #267 site) so a regression there is caught in the same run.
	const src = `
version: 1
messages:
  Probe:
    payload:
      nested:
        id: 100
        type: struct
        fields:
          bytes_field: { id: 3, type: blob, maxlen: 4 }
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
      blob_array:   { id: 201, type: array, items: { type: blob,   count: 5, maxlen: 64 } }
`
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, src), map[string]any{}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const driver = `
import binascii, sys
import sofab
from sofab import SofaDecodeError, SofaIncompleteError
from message import Probe

# ce 0c = (201 << 3) | 6, blob_array sequence start
# 22    = (4 << 3) | 2,   wrapper element index 4 (in bounds), fixlen
# e3 30 = (780 << 3) | 3, fixlen_word: subtype blob, declared length 780
VECTORS = [
    ("ce 0c 22 e3 30",             "INVALID",    "over-maxlen blob element, zero payload bytes"),
    ("ce 0c 22 23",                "INCOMPLETE", "control: declared length 4 <= 64, cut identically"),
    ("ce 0c 22 23 de ad be ef 07", "OK",         "control: in-bound and complete"),
    ("c6 0c 22 e2 30",             "INVALID",    "control: the same shape on string_array"),
    ("a6 06 1a e3 30",             "INVALID",    "control: over-maxlen blob as a plain field"),
]

bad = 0
for hexs, want, desc in VECTORS:
    try:
        Probe.decode(binascii.unhexlify(hexs.replace(" ", "")))
    except SofaIncompleteError:
        got = "INCOMPLETE"
    except SofaDecodeError:
        got = "INVALID"
    else:
        got = "OK"
    if got != want:
        bad += 1
        print("[%s] %s: want %s, got %s (%s)" % (sofab.IMPL, hexs, want, got, desc))
if bad:
    sys.exit("%d vector(s) misjudged" % bad)
print("%s ok" % sofab.IMPL)
`
	if err := os.WriteFile(filepath.Join(dir, "driver.py"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pure := range []string{"", "1"} {
		cmd := exec.Command(py, filepath.Join(dir, "driver.py"))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PYTHONPATH="+filepath.Join(corelib, "src")+string(os.PathListSeparator)+dir,
			"SOFAB_PUREPYTHON="+pure)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("SOFAB_PUREPYTHON=%q: %v\n%s", pure, err, out)
		}
		t.Logf("SOFAB_PUREPYTHON=%q: %s", pure, strings.TrimSpace(string(out)))
	}
}
