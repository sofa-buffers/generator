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
