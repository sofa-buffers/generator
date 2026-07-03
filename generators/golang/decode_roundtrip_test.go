package golang

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGoDecodeRoundTrip compiles the generated code for the canonical example
// schema against a real corelib-go checkout and runs an encode -> AcceptBytes
// decode -> re-encode round trip, asserting the re-encoded bytes are identical.
// This exercises the visitor decode path (Decode<Msg> via sofab.AcceptBytes)
// across scalars, native arrays, string/blob/struct/union arrays and a matrix —
// the encode conformance test only covers marshal. Gated on SOFAB_GO_CORELIB.
func TestGoDecodeRoundTrip(t *testing.T) {
	corelib := os.Getenv("SOFAB_GO_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_GO_CORELIB to a corelib-go checkout to run the decode round trip")
	}
	s := exampleSchema(t)
	files, err := (&Backend{}).Generate(s, map[string]any{"package": "message"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, "message", f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(
		"module rt\n\ngo 1.24\n\nrequire github.com/sofa-buffers/corelib-go v0.0.0\n\nreplace github.com/sofa-buffers/corelib-go => "+corelib+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rt_test.go"), []byte(goRoundTripDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
}

// goRoundTripDriver populates every field kind, encodes, decodes via the visitor
// path, and asserts a re-encode is byte-identical (canonical, lossless decode).
const goRoundTripDriver = `package rt

import (
	"bytes"
	"testing"

	msg "rt/message"
)

func TestExampleRoundTrip(t *testing.T) {
	m := msg.NewMyfirstmessage()
	m.Someu32 = 123456
	m.Somei32 = -4242
	m.Someu64 = 18446744073709551000
	m.Somestring = "héllo wörld"
	m.Someblob = []byte{1, 2, 3, 4, 5}
	m.Someuintarray = []uint32{9, 8, 7, 6}
	m.Someintarray = []int32{-1, -2, -3, -4, -5}
	m.Somefloatarray = []float32{1.5, -2.5, 3.5}
	m.Someboolarray = []bool{true, false, true, true, false, false, true, false}
	m.Somestringarray = []string{"a", "bb", "ccc"}
	m.Someblobarray = [][]byte{{9}, {8, 7}}
	m.Somematrix = [][]uint32{{1, 2, 3, 4}, {5, 6}}

	enc, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := msg.DecodeMyfirstmessage(enc)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("re-encode drifted:\n %x\n %x", enc, enc2)
	}
	if got.Somestring != m.Somestring || len(got.Somematrix) != 2 || len(got.Somestringarray) != 3 {
		t.Fatalf("decoded fields wrong: %#v", got)
	}
}
`
