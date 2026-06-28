package c

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func genProject(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := (&Backend{}).Generate(buildExampleIR(t), map[string]any{"emit": "project", "symbol_prefix": "sofab_"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

func TestProjectScaffolding(t *testing.T) {
	files := genProject(t)
	for _, want := range []string{
		"Makefile",
		"CMakeLists.txt",
		".devcontainer/devcontainer.json",
		"harness/main.c",
		"README.md",
		"run.sh",
		"generated/myfirstmessage.h", // sources moved under generated/
		"generated/myfirstmessage.c",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("project missing %q", want)
		}
	}
	// The harness must not contain Go-fmt artifacts from mis-passed format args.
	if strings.Contains(string(files["harness/main.c"]), "MISSING") {
		t.Error("harness contains a Go fmt artifact (%!...(MISSING))")
	}
}

// TestProjectBuildsAndRoundTrips is the real M3 gate: build the generated
// project with make against corelib-c-cpp and round-trip JSON through the
// harness. Gated on SOFAB_C_CORELIB + make + gcc.
func TestProjectBuildsAndRoundTrips(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to run the project build gate")
	}
	for _, tool := range []string{"make", "gcc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	dir := t.TempDir()
	for path, content := range genProject(t) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("project build failed:\n%s", out)
	}
	harness := filepath.Join(dir, "harness", "harness")
	in := `{"someinteger":-5,"somebool":true,"somestring":"hi","bignum":18446744073709551615,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someblob":[10,20,30]}`
	enc := exec.Command(harness, "encode")
	enc.Stdin = strings.NewReader(in)
	encoded, err := enc.Output()
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	dec := exec.Command(harness, "decode")
	dec.Stdin = strings.NewReader(string(encoded))
	decoded, err := dec.Output()
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	for _, want := range []string{`"bignum":18446744073709551615`, `"deepint":-99`, `"someblob":[10,20,30`, `"someinteger":-5`} {
		if !strings.Contains(string(decoded), want) {
			t.Errorf("round-trip missing %q in:\n%s", want, decoded)
		}
	}
}
