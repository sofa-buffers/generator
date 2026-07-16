package c

import (
	"fmt"
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
		"char s[5];",                   // string: maxlen 4 + 1 for the NUL
		"uint8_t b_len; uint8_t b[4];", // blob: companion used-length + exactly maxlen buffer (issue #128)
		"char items[3][9];",            // string element of a holder array: maxlen 8 + 1
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
}

// TestBlobSized: a scalar/struct-field blob lowers to a sized blob — a companion
// used-length member adjacent to (and immediately before) the buffer, plus the
// SOFAB_OBJECT_FIELD_BLOB_SIZED descriptor — so a sub-maxlen blob keeps its exact
// length on the wire instead of being zero-padded to maxlen or dropped when empty
// (issue #128). _init must zero the struct first (the _len companion is not a
// descriptor field, so sofab_object_init leaves it untouched), and a non-empty
// blob default must materialize its used-length there.
func TestBlobSized(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      plain: { id: 0, type: blob, maxlen: 4 }
      big:   { id: 1, type: blob, maxlen: 300 }
      dflt:  { id: 2, type: blob, maxlen: 8, default: "SGVsbG8=" }
`)
	h, c := files["m.h"], files["m.c"]
	for _, want := range []string{
		"uint8_t plain_len; uint8_t plain[4];", // narrow length (maxlen<=255 -> uint8_t), adjacent, before the buffer
		"uint16_t big_len; uint8_t big[300];",  // wider length when maxlen exceeds a uint8_t
		"uint8_t dflt_len; uint8_t dflt[8];",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
	for _, want := range []string{
		"SOFAB_OBJECT_FIELD_BLOB_SIZED(0, message_m_t, plain, plain_len),",
		"SOFAB_OBJECT_FIELD_BLOB_SIZED(1, message_m_t, big, big_len),",
		"memset(msg, 0, sizeof(*msg));", // zero first so the non-descriptor _len members are deterministic
		"msg->dflt_len = 5;",            // "Hello" default materializes its used-length
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing %q:\n%s", want, c)
		}
	}
	// A blob must never use the plain fixed-capacity descriptor (the #128 bug).
	if strings.Contains(c, "message_m_t, plain, SOFAB_OBJECT_FIELDTYPE_BLOB)") {
		t.Errorf("m.c still emits the unsized plain-BLOB descriptor for a blob field (issue #128):\n%s", c)
	}
}

// TestBlobArraySized: a blob *array* element is a sized blob too (issue #130) —
// the wrapper-sequence holder stores each element as a { len; buf[maxlen]; }
// struct (length immediately before the byte buffer) and emits a per-element
// SOFAB_OBJECT_FIELD_BLOB_SIZED descriptor, so a sub-maxlen element keeps its
// exact length instead of being zero-padded to maxlen. A string array stays a
// plain char[count][maxlen+1] (NUL-recovered, no companion length).
func TestBlobArraySized(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      ba: { id: 0, type: array, items: { type: blob, count: 3, maxlen: 4 } }
      sa: { id: 1, type: array, items: { type: string, count: 2, maxlen: 8 } }
`)
	h, c := files["m.h"], files["m.c"]
	if !strings.Contains(h, "struct { uint8_t len; uint8_t buf[4]; } items[3];") {
		t.Errorf("m.h missing sized blob-array holder:\n%s", h)
	}
	if !strings.Contains(h, "char items[2][9];") { // string array element unchanged (maxlen 8 + NUL)
		t.Errorf("m.h string-array element storage changed unexpectedly:\n%s", h)
	}
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf("BLOB_SIZED(%d, message_m_ba_elems_t, items[%d].buf, items[%d].len),", i, i, i)
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing per-element sized descriptor %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "items[0], SOFAB_OBJECT_FIELDTYPE_BLOB)") {
		t.Errorf("m.c still emits the unsized plain-BLOB descriptor for a blob-array element (issue #130):\n%s", c)
	}
}

// TestDeprecatedFieldRendering: a field marked deprecated must (a) carry the
// native __attribute__((deprecated)) marker on its struct member and a Doxygen
// @deprecated note in the member's doc comment, and (b) keep the generated .c
// warning-clean — the descriptor field table (sizeof(((T*)0)->field)) and any
// defaults designated-initializer that name the deprecated member are wrapped in
// a -Wdeprecated-declarations diagnostic push/pop.
func TestDeprecatedFieldRendering(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      keep:   { id: 0, type: u16, description: "Current identifier." }
      legacy: { id: 1, type: u32, description: "Old identifier kept for compatibility.", deprecated: true, default: 7 }
`)
	h := files["m.h"]
	// (a) native marker + @deprecated doc note on the deprecated member, and the
	// description text is preserved alongside the note.
	for _, want := range []string{
		"uint32_t legacy __attribute__((deprecated));",
		"Old identifier kept for compatibility. @deprecated",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
	// A non-deprecated member must NOT get the marker or the note.
	if strings.Contains(h, "uint16_t keep __attribute__((deprecated))") {
		t.Errorf("non-deprecated member wrongly marked deprecated:\n%s", h)
	}

	c := files["m.c"]
	// (b) the descriptor emission that references the deprecated member by name is
	// guarded so the generated .c compiles clean under -Wdeprecated-declarations.
	for _, want := range []string{
		"#pragma GCC diagnostic push",
		`#pragma GCC diagnostic ignored "-Wdeprecated-declarations"`,
		"#pragma GCC diagnostic pop",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing deprecation guard %q:\n%s", want, c)
		}
	}
	// The guard must open before the field table and the designated initializer
	// (.legacy = 7) must fall inside the guarded region (between push and pop).
	push := strings.Index(c, "#pragma GCC diagnostic push")
	pop := strings.Index(c, "#pragma GCC diagnostic pop")
	init := strings.Index(c, ".legacy = 7")
	table := strings.Index(c, "SOFAB_OBJECT_FIELD(1, message_m_t, legacy,")
	if push < 0 || pop < 0 || table < 0 || init < 0 || !(push < table && table < pop) || !(push < init && init < pop) {
		t.Errorf("descriptor references to the deprecated member are not inside the diagnostic guard:\n%s", c)
	}
}

// TestNonDeprecatedNoGuard: a schema with no deprecated field must not emit any
// diagnostic pragma (the guard is strictly opt-in, byte-cost-free otherwise).
func TestNonDeprecatedNoGuard(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u16, default: 3 }
`)
	if strings.Contains(files["m.c"], "#pragma GCC diagnostic") {
		t.Errorf("no deprecated field, but m.c emitted a diagnostic pragma:\n%s", files["m.c"])
	}
	if strings.Contains(files["m.h"], "__attribute__((deprecated))") {
		t.Errorf("no deprecated field, but m.h emitted a deprecated attribute:\n%s", files["m.h"])
	}
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestCMapRejected(t *testing.T) {
	src := `
version: 1
messages:
  M:
    payload:
      counts: { type: map, id: 1, key: { type: u32 }, value: { type: u8 }, count: 8 }
`
	doc, err := parser.Parse([]byte(src), "map.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Backend{}).Generate(s, map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "not yet supported by the c backend") {
		t.Fatalf("expected c map rejection, got %v", err)
	}
}
