package c

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func buildExampleIR(t *testing.T) *ir.Schema {
	t.Helper()
	def := filepath.Join("..", "..", "examples", "messages", "example.yaml")
	doc, err := parser.Load(def)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("example must validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func genExample(t *testing.T) map[string]string {
	t.Helper()
	files, err := (&Backend{}).Generate(buildExampleIR(t), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

func TestGeneratesHeaderAndSource(t *testing.T) {
	files := genExample(t)
	for _, want := range []string{"myfirstmessage.h", "myfirstmessage.c"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("missing generated file %q (got %v)", want, keys(files))
		}
	}
}

func TestStructuralInvariants(t *testing.T) {
	h := genExample(t)["myfirstmessage.h"]
	for _, want := range []string{
		"#ifndef SOFAB_MYFIRSTMESSAGE_H",
		"#include \"sofab/object.h\"",
		"#if SOFAB_API_VERSION != 1",                  // API-version guard
		"#if defined(SOFAB_DISABLE_FIXLEN_SUPPORT)",   // capability guards
		"#if defined(SOFAB_DISABLE_SEQUENCE_SUPPORT)", // struct/union/array-of-string
		"#if defined(SOFAB_DISABLE_INT64_SUPPORT)",    // bignum u64
		"#define SOFAB_MYFIRSTMESSAGE_MAX_SIZE",       // §5.5
		"sofab_myfirstmessage_t;",
		"int8_t someenum;",      // enum -> smallest signed backing
		"uint8_t somebitfield;", // bitfield -> unsigned backing
		"sofab_myfirstmessage_encode(",
		"sofab_myfirstmessage_decode(",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q", want)
		}
	}
	// Identifiers must be valid C (no leftover '/' or '#' from synthetic keys).
	if strings.ContainsAny(h, "/#") {
		for _, line := range strings.Split(h, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "}") && strings.ContainsAny(line, "/#") {
				t.Errorf("invalid C identifier in: %s", line)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	a := genExample(t)["myfirstmessage.c"]
	b := genExample(t)["myfirstmessage.c"]
	if a != b {
		t.Fatal("generation is not deterministic")
	}
}

// TestCompilesAgainstCorelib is the real build gate: it compiles the generated
// sources against corelib-c-cpp with gcc. It runs only when SOFAB_C_CORELIB
// points at a corelib-c-cpp checkout and gcc is present; otherwise it skips
// (the hermetic tests above still run, and tests/c/run.sh covers CI).
func TestCompilesAgainstCorelib(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to a corelib-c-cpp checkout to run the compile gate")
	}
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not found")
	}
	dir := t.TempDir()
	for path, content := range genExample(t) {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inc := filepath.Join(corelib, "src", "include")
	cmd := exec.Command(gcc, "-std=c99", "-Wall", "-Wextra",
		"-I"+inc, "-I"+dir, "-c", filepath.Join(dir, "myfirstmessage.c"),
		"-o", filepath.Join(dir, "msg.o"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated C failed to compile against corelib:\n%s", out)
	}
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}
