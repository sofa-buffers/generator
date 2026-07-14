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
	// The shared example intentionally leaves `somemap` unbounded (a dynamic map
	// for heap targets). The heapless C target requires a bound on every array
	// (checkBounded rejects unbounded fields), so give it an explicit capacity —
	// exactly what a C-target schema author does. `count` never reaches the wire,
	// so this does not affect the shared conformance vectors.
	boundArrayField(s, "somemap", 8)
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// boundArrayField gives the named top-level array field an explicit count so a
// schema written for heap targets can be generated for the heapless C target.
func boundArrayField(s *ir.Schema, name string, count int64) {
	for _, m := range s.Messages {
		for _, f := range m.Fields {
			if f.Name == name {
				f.HasCount = true
				f.Count = count
			}
		}
	}
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
		"#ifndef MESSAGE_MYFIRSTMESSAGE_H", // include guard from the symbol_prefix (default message_)
		"#include \"sofab/object.h\"",
		"#if SOFAB_API_VERSION != 1",                  // API-version guard (corelib macro)
		"#if defined(SOFAB_DISABLE_FIXLEN_SUPPORT)",   // capability guards (corelib macros)
		"#if defined(SOFAB_DISABLE_SEQUENCE_SUPPORT)", // struct/union/array-of-string
		"#if defined(SOFAB_DISABLE_INT64_SUPPORT)",    // someu64 / somei64
		"#define MESSAGE_MYFIRSTMESSAGE_MAX_SIZE",     // §5.5
		"message_myfirstmessage_t;",
		"int8_t someenum;",      // enum -> smallest signed backing
		"uint8_t somebitfield;", // bitfield -> unsigned backing
		"message_myfirstmessage_encode(",
		"message_myfirstmessage_decode(",
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
// (the hermetic tests above still run, and tests/conformance/c/run.sh covers CI).
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

func genCErr(t *testing.T, src string) error {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("schema must validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	_, err = (&Backend{}).Generate(s, map[string]any{})
	return err
}

func genCFromYAML(t *testing.T, src string) map[string]string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("schema must validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

// TestUnboundedFieldsRejected: the C object model has no dynamic containers, so
// every string/blob (maxlen) and array (count, at every nesting level, ANY
// element kind) must be sized by the schema. An unbounded such field is a hard
// generate-time error naming the field — not a silently invented char[1]/T[0]
// that then rejects every real message at runtime (#104). There is no
// allow_dynamic escape for C.
func TestUnboundedFieldsRejected(t *testing.T) {
	cases := []struct {
		name, yaml, wantField, wantMissing string
	}{
		{"string no maxlen",
			"version: 1\nmessages:\n  m: { payload: { s: { id: 0, type: string } } }", "s", "maxlen"},
		{"blob no maxlen",
			"version: 1\nmessages:\n  m: { payload: { b: { id: 0, type: blob } } }", "b", "maxlen"},
		{"native scalar array no count",
			"version: 1\nmessages:\n  m: { payload: { a: { id: 0, type: array, items: { type: u32 } } } }", "a", "count"},
		{"string array no count",
			"version: 1\nmessages:\n  m: { payload: { a: { id: 0, type: array, items: { type: string, maxlen: 8 } } } }", "a", "count"},
		{"string array no element maxlen",
			"version: 1\nmessages:\n  m: { payload: { a: { id: 0, type: array, items: { type: string, count: 4 } } } }", "a", "element maxlen"},
		{"struct array no count",
			"version: 1\nmessages:\n  m: { payload: { a: { id: 0, type: array, items: { type: struct, fields: { x: { id: 0, type: u8 } } } } } }", "a", "count"},
		{"unbounded string inside nested struct",
			"version: 1\nmessages:\n  m: { payload: { n: { id: 0, type: struct, fields: { s: { id: 0, type: string } } } } }", "s", "maxlen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := genCErr(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected a generate-time error for %q", tc.name)
			}
			for _, want := range []string{tc.wantField, tc.wantMissing} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "allow_dynamic") {
				t.Errorf("C error must not suggest allow_dynamic (no such escape): %q", err)
			}
		})
	}
}

// TestBoundedSchemaGenerates: a fully bounded schema — every string/blob has a
// maxlen and every array a count — generates without error.
func TestBoundedSchemaGenerates(t *testing.T) {
	err := genCErr(t, `
version: 1
messages:
  m:
    payload:
      s:    { id: 0, type: string, maxlen: 16 }
      b:    { id: 1, type: blob, maxlen: 8 }
      a:    { id: 2, type: array, items: { type: u32, count: 4 } }
      sa:   { id: 3, type: array, items: { type: string, count: 3, maxlen: 8 } }
`)
	if err != nil {
		t.Fatalf("a fully bounded schema must generate: %v", err)
	}
}

// TestStringStorageReservesTerminator: a bounded string member must get maxlen+1
// bytes of storage. The corelib's read_string reserves one byte for the NUL
// (istream.c rejects wire length > capacity-1), so char[maxlen] would reject a
// wire string of exactly maxlen bytes — its declared schema bound (#103). A blob
// (no terminator) keeps exactly maxlen; a string element of a holder array gets
// the same +1.
func TestStringStorageReservesTerminator(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      s:    { id: 0, type: string, maxlen: 4 }
      b:    { id: 1, type: blob, maxlen: 4 }
      arr:  { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`)
	h := files["m.h"]
	for _, want := range []string{
		"char s[5];",        // string: maxlen 4 + 1 for the NUL
		"uint8_t b[4];",     // blob: exactly maxlen, no terminator
		"char items[3][9];", // string element of a holder array: maxlen 8 + 1
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}
