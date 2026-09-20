package c

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestProjectHarnessEscapesKeywordMembers: the generated types mangle a field
// named after a C keyword (cIdent, trailing underscore), so the harness's JSON
// helpers must read and write the mangled member -- `o->return_`, not
// `o->return`, which is a syntax error. The JSON key stays the schema name:
// mangling is a C-identifier problem and must not reach the JSON the shared
// vectors are written in. Found as generator#583, where the corpus loop compiled
// only the generated types and never the harness.
func TestProjectHarnessEscapesKeywordMembers(t *testing.T) {
	const src = `version: 1
$defs:
  struct:
    Nested:
      return: { id: 0, type: boolean }
      class:  { id: 1, type: u32 }
messages:
  kw:
    payload:
      int:    { id: 0, type: u32 }
      for:    { id: 1, type: array, items: { type: u8, count: 3 } }
      struct: { id: 2, type: string, maxlen: 8 }
      double: { id: 3, type: fp64 }
      nested: { id: 4, type: struct, fields: { $ref: '#/$defs/struct/Nested' } }
`
	files := genCFromYAMLCfg(t, src, map[string]any{"emit": "project"})
	h, ok := files["harness/main.c"]
	if !ok {
		t.Fatal("no harness/main.c")
	}
	for _, want := range []string{"o->int_", "o->for_", "o->struct_", "o->double_", "o->return_"} {
		if !strings.Contains(h, want) {
			t.Errorf("harness does not read the mangled member %q:\n%s", want, h)
		}
	}
	// Driven off cKeywords itself, so a keyword added there is covered here too.
	for kw := range cKeywords {
		if regexp.MustCompile(`o->` + kw + `\b`).MatchString(h) {
			t.Errorf("harness accesses the unmangled member o->%s", kw)
		}
	}
	// The wire-side name is untouched: a decoder fed the shared vectors' JSON
	// still finds the field.
	for _, want := range []string{`\"return\":`, `sofab_json_get(j, "return")`, `\"int\":`} {
		if !strings.Contains(h, want) {
			t.Errorf("harness lost the JSON key %q:\n%s", want, h)
		}
	}
}

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
	build := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, strictMakeVar)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("project build failed:\n%s", out)
	}
	harness := filepath.Join(dir, "harness", "harness")
	in := `{"somei8":-5,"somebool":true,"somestring":"hi","someu64":18446744073709551615,"somestruct":{"nestedint":7,"nestedstring":"deep","nestedstruct":{"deepint":-99}},"someblob":[10,20,30]}`
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
	for _, want := range []string{`"someu64":18446744073709551615`, `"deepint":-99`, `"someblob":[10,20,30`, `"somei8":-5`} {
		if !strings.Contains(string(decoded), want) {
			t.Errorf("round-trip missing %q in:\n%s", want, decoded)
		}
	}
}
