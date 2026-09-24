package c

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

// strictWarnings is the warning policy every build of generated C in these tests
// applies (ARCHITECTURE §12): a warning in generated code is an error in a user's
// -Werror build, so it is one here. strictMakeVar hands the same flags to a
// generated project's Makefile, which reads them from WARNFLAGS.
var strictWarnings = []string{"-Wall", "-Wextra", "-Werror"}

var strictMakeVar = "WARNFLAGS=" + strings.Join(strictWarnings, " ")

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
	args := append([]string{"-std=c99"}, strictWarnings...)
	args = append(args, "-I"+inc, "-I"+dir, "-c", filepath.Join(dir, "myfirstmessage.c"),
		"-o", filepath.Join(dir, "msg.o"))
	if out, err := exec.Command(gcc, args...).CombinedOutput(); err != nil {
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
	return genCFromYAMLCfg(t, src, map[string]any{})
}

// genCFromYAMLCfg is genCFromYAML under an explicit config.
func genCFromYAMLCfg(t *testing.T, src string, cfg map[string]any) map[string]string {
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
	files, err := (&Backend{}).Generate(s, cfg)
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

// TestOverIndexSeqHolderDescriptor: a fixed-count string/blob/struct wrapper
// array lowers to a synthetic element-slot holder (`_elems`), which must be
// emitted as a SOFAB_OBJECT_DESCR_SEQ* form so the corelib rejects an over-index element
// id (>= N) as INVALID instead of skipping it (MESSAGE_SPEC §7/§7.1, generator#149
// / corelib-c-cpp#94). The message object and the struct element's *own* type
// descriptor (`_elem`) are ordinary objects — unknown ids there are
// forward-compat skips — so they keep the plain SOFAB_OBJECT_DESCR form.
func TestOverIndexSeqHolderDescriptor(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      sa: { id: 0, type: array, items: { type: string, count: 4, maxlen: 8 } }
      ba: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 8 } }
      pa: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }
`)
	c := files["m.c"]
	for _, want := range []string{
		"_sa_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(", // string holder
		"_ba_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(", // blob holder
		"_pa_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(", // struct holder
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c holder descriptor not marked fixed-seq: missing %q:\n%s", want, c)
		}
	}
	// The message object and the struct element's own descriptor stay plain: an
	// unknown id there is a valid forward-compat skip, not an over-index reject.
	for _, want := range []string{
		"_message_m = SOFAB_OBJECT_DESCR(", // the message itself
		"_pa_elem = SOFAB_OBJECT_DESCR(",   // struct element type descriptor
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c non-holder object must use plain SOFAB_OBJECT_DESCR: missing %q:\n%s", want, c)
		}
	}
	// A holder must never be the SEQ *and* skip form at once.
	if strings.Contains(c, "_elems = SOFAB_OBJECT_DESCR(") {
		t.Errorf("m.c holder emitted as a plain (skip) descriptor:\n%s", c)
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

// TestCompactArraySized: MESSAGE_SPEC §3 makes `count: N` the array's CAPACITY
// and the wire count M its LENGTH — no element is elided, and no decoder refills
// [M, N). The C backend writes no encode logic of its own; it hands object.c a
// descriptor, and a plain SOFAB_OBJECT_FIELD_ARRAY derives the count structurally
// from sizeof(field)/sizeof(field[0]) — the capacity, and nothing else. So every
// compact array must lower to SOFAB_OBJECT_FIELD_ARRAY_SIZED with a companion
// length member holding 0..N, declared immediately before the buffer.
func TestCompactArraySized(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      u8s:  { id: 0, type: array, items: { type: u8,   count: 4 } }
      u32s: { id: 1, type: array, items: { type: u32,  count: 4 } }
      f64s: { id: 2, type: array, items: { type: fp64, count: 3 } }
      wide: { id: 3, type: array, items: { type: u16,  count: 400 } }
`)
	h, c := files["m.h"], files["m.c"]
	// The length must be at least as wide as one element: it sits immediately
	// before the buffer, and the corelib reads it at <offset − width>, so a
	// narrower one would be padded away (and fail SOFAB_OBJECT_ASSERT_LEN_ADJACENT).
	for _, want := range []string{
		"uint8_t u8s_len; uint8_t u8s[4];",       // byte elements: capacity fits a byte
		"uint32_t u32s_len; uint32_t u32s[4];",   // 4-byte elements force a 4-byte length
		"uint64_t f64s_len; double f64s[3];",     // 8-byte elements force an 8-byte length
		"uint16_t wide_len; uint16_t wide[400];", // capacity 400 needs 2 bytes anyway
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
	for _, want := range []string{
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(0, message_m_t, u8s, u8s_len, SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(2, message_m_t, f64s, f64s_len, SOFAB_OBJECT_FIELDTYPE_ARRAY_FP64),",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing %q:\n%s", want, c)
		}
	}
	// The capacity-only descriptor can express only the length N, so it must be gone.
	if strings.Contains(c, "SOFAB_OBJECT_FIELD_ARRAY(") {
		t.Errorf("m.c still emits a capacity-only SOFAB_OBJECT_FIELD_ARRAY (§3):\n%s", c)
	}
	// An 8-byte length is read through the corelib's _load_uint, whose 8-byte case
	// SOFAB_DISABLE_INT64_SUPPORT compiles out — so such a schema must guard it.
	if !strings.Contains(h, "#if defined(SOFAB_DISABLE_INT64_SUPPORT)") {
		t.Errorf("m.h: an 8-byte array length needs the INT64 capability guard:\n%s", h)
	}
}

// TestWrapperHolderSized: MESSAGE_SPEC §5.1 gives a wrapper array the length
// *highest present id + 1*, i.e. any of 0..N, with `count` bounding it only. A C
// holder materializes all N slots, so without a length member it can express
// nothing but 0 (every slot default — the enclosing object omits the field) and
// N. The holder therefore leads with an element-count member and is emitted as
// SOFAB_OBJECT_DESCR_SEQ_SIZED. The count is the holder's FIRST member, which is
// where the descriptor reads it (offset 0).
func TestWrapperHolderSized(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      sa: { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }
      pa: { id: 1, type: array, items: { type: struct, count: 3, fields: { x: { id: 0, type: i32 } } } }
      wa: { id: 2, type: array, items: { type: struct, count: 3, fields: { x: { id: 0, type: u64 } } } }
      na: { id: 3, type: array, items: { type: array,  count: 2, items: { type: string, count: 2, maxlen: 4 } } }
`)
	h, c := files["m.h"], files["m.c"]
	for _, want := range []string{
		"uint8_t len; char items[3][9];",                    // string slots are byte-aligned: any width works
		"uint32_t len; message_m_pa_elem_t items[3];",       // i32 element struct -> 4-byte alignment
		"uint64_t len; message_m_wa_elem_t items[3];",       // u64 element struct -> 8-byte alignment
		"uint8_t len; message_m_na_elems_inner_t items[2];", // holder of holders: inner leads with a uint8_t
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
	for _, want := range []string{
		"_sa_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(_message_fields_message_m_sa_elems, 3, NULL, 0, message_m_sa_elems_t, len);",
		"_pa_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(",
		"_wa_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(",
		"_na_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(",
		"_na_elems_inner = SOFAB_OBJECT_DESCR_SEQ_SIZED(",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing %q:\n%s", want, c)
		}
	}
	// No holder here may keep the length-less form: each would then be stuck at
	// the two lengths 0 and N.
	if strings.Contains(c, "_elems = SOFAB_OBJECT_DESCR_SEQ(") {
		t.Errorf("m.c left a wrapper holder without its element count (§5.1):\n%s", c)
	}
	// The macro names no element slot: anchoring the count at the holder's start is
	// what lets EVERY element kind carry one (see TestSizedElementHolderCarriesCount).
	if strings.Contains(c, "SOFAB_OBJECT_DESCR_SEQ_SIZED(") && strings.Contains(c, ", items[0], len);") {
		t.Errorf("m.c still locates the holder count relative to slot 0:\n%s", c)
	}
}

// TestSizedElementHolderCarriesCount: the two element kinds whose slot is itself
// length-carrying — a blob element and a native inner-array row (issues #128/#130)
// — begin with their own used-length, so a holder count placed one width before
// slot 0 would BE that length. Anchoring the holder count at offset 0 instead
// separates the two, and these holders carry their element count like every other
// kind. Each row/element still keeps its OWN sized descriptor: a row's wire count
// is its length too (§3).
func TestSizedElementHolderCarriesCount(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      ba:   { id: 0, type: array, items: { type: blob,  count: 3, maxlen: 4 } }
      rows: { id: 1, type: array, items: { type: array, count: 2, items: { type: u16, count: 3 } } }
`)
	h, c := files["m.h"], files["m.c"]
	for _, want := range []string{
		// The holder count comes FIRST; the per-slot length stays inside the slot.
		"uint8_t len; struct { uint8_t len; uint8_t buf[4]; } items[3];",
		"uint16_t len; struct { uint16_t len; uint16_t vals[3]; } items[2];",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
	for _, want := range []string{
		"_ba_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(",
		"_rows_elems = SOFAB_OBJECT_DESCR_SEQ_SIZED(",
		"SOFAB_OBJECT_FIELD_BLOB_SIZED(0, message_m_ba_elems_t, items[0].buf, items[0].len),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(0, message_m_rows_elems_t, items[0].vals, items[0].len, SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(1, message_m_rows_elems_t, items[1].vals, items[1].len, SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED),",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "_elems = SOFAB_OBJECT_DESCR_SEQ(") {
		t.Errorf("m.c left a self-sized-slot holder without its element count (§5.1):\n%s", c)
	}
}

// TestArrayDefaultKeepsItsLength: a declared `default` shorter than `count` is an
// array of ITS OWN length — §3 dropped the padding to N that the fixed-length
// reading implied. The length lives in the companion member, and sofab_object_init
// seeds that from the default image (at offset − width), so the image must carry
// it. An all-ZERO declared default still has a length: [0,0,0] is the three-element
// array of zeros, not the empty array, so the length entry is emitted even when the
// value image is elided as all-zero storage.
func TestArrayDefaultKeepsItsLength(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      few: { id: 0, type: array, items: { type: u32, count: 5 }, default: [1, 2, 3] }
      zeros: { id: 1, type: array, items: { type: u32, count: 5 }, default: [0, 0, 0] }
      none:  { id: 2, type: array, items: { type: u32, count: 5 } }
`)
	c := files["m.c"]
	for _, want := range []string{
		".few_len = 3,",
		".few = { 1, 2, 3 },",
		".zeros_len = 3,", // the length is the value even when every element is zero
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing %q:\n%s", want, c)
		}
	}
	// No declared default -> the empty array -> length 0, which the memset already
	// leaves; nothing may be written into the image for it.
	if strings.Contains(c, ".none_len") || strings.Contains(c, ".none =") {
		t.Errorf("m.c invented a default for an array that declares none:\n%s", c)
	}
	// The superseded reading padded the declared default out to the capacity.
	if strings.Contains(c, ".few = { 1, 2, 3, 0, 0 }") {
		t.Errorf("m.c padded a short declared default out to count (§2/§3/§6):\n%s", c)
	}
}

// TestHarnessRendersArrayLength: the project harness is what the conformance
// round-trips run through, so it has to speak the same language as the wire — a
// JSON array is read into the length member and rendered from it, never from the
// capacity. Rendering `count` elements would print a decoded 2-of-5 array as five.
func TestHarnessRendersArrayLength(t *testing.T) {
	files := genCProject(t, `
version: 1
messages:
  m:
    payload:
      a:  { id: 0, type: array, items: { type: u32, count: 5 } }
      sa: { id: 1, type: array, items: { type: string, count: 3, maxlen: 8 } }
      ba: { id: 2, type: array, items: { type: blob, count: 3, maxlen: 4 } }
`)
	hs := files["harness/main.c"]
	for _, want := range []string{
		"_i0 < (int)(o->a_len)",  // compact array renders its length
		"_i0 < (int)(o->sa.len)", // string holder renders its element count
		"_i0 < (int)(o->ba.len)", // blob holder does too, now that it has one
		"o->a_len = (uint32_t)_n0;",
		"o->sa.len = (uint8_t)_n0;",
		"o->ba.len = (uint8_t)_n0;",
	} {
		if !strings.Contains(hs, want) {
			t.Errorf("harness/main.c missing %q:\n%s", want, hs)
		}
	}
	// Nothing may loop to a capacity any more: every array form carries a length.
	for _, bad := range []string{"_i0 < (int)(5)", "_i0 < (int)(3)"} {
		if strings.Contains(hs, bad) {
			t.Errorf("harness/main.c still renders an array to its capacity (%q):\n%s", bad, hs)
		}
	}
}

// genCProject generates with emit:project so the harness sources are included.
func genCProject(t *testing.T, src string) map[string]string {
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
	files, err := (&Backend{}).Generate(s, map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

// intConstRE matches a decimal integer constant and the suffix glued to it, the
// way a C compiler tokenizes one: a run of digits not preceded by an identifier
// character, a hex prefix or a decimal point, followed by its unsigned/long
// suffix letters. The optional leading sign is captured only so a NEGATIVE
// spelling can be recognised and dropped — C has no negative constants, so
// "-9223372036854775808" is the unary minus of a constant whose magnitude the
// rules below would otherwise judge on its own and report as untyped.
var intConstRE = regexp.MustCompile(`(?:^|[^0-9A-Za-z_.])(-?)([0-9]+)([uUlL]*)`)

// intConsts yields each decimal constant in src as (value, full text), skipping
// the ones these rules cannot judge: a negated spelling (see intConstRE) and a
// magnitude wider than uint64, which is a different defect.
func intConsts(src string, fn func(v uint64, lit string)) {
	for _, m := range intConstRE.FindAllStringSubmatch(src, -1) {
		if m[1] == "-" {
			continue
		}
		v, err := strconv.ParseUint(m[2], 10, 64)
		if err != nil {
			continue
		}
		fn(v, m[2]+m[3])
	}
}

// hugeSignedlessConstants returns every decimal constant in src that is above
// INT64_MAX and carries no unsigned suffix — that is, every constant C11
// 6.4.4.1 gives no type at all, because an unsuffixed decimal constant is looked
// up in the SIGNED list only.
//
// This applies the compiler's own rule to the emitted text. A substring
// assertion cannot: "9223372036854775808ULL" contains "9223372036854775808", so
// a grep for the good spelling passes just as happily on the broken one.
func hugeSignedlessConstants(src string) []string {
	var bad []string
	intConsts(src, func(v uint64, lit string) {
		if v > math.MaxInt64 && !strings.ContainsAny(lit, "uU") {
			bad = append(bad, lit)
		}
	})
	return bad
}

// constantsSpelling returns the full text of every decimal constant in src whose
// digits spell want, suffix included.
func constantsSpelling(src string, want uint64) []string {
	var out []string
	intConsts(src, func(v uint64, lit string) {
		if v == want {
			out = append(out, lit)
		}
	})
	return out
}

// The guard above judges MAGNITUDES, so it has to know a minus sign when it sees
// one: 9223372036854775808 is untyped on its own but is exactly how INT64_MIN's
// magnitude is written, and reporting that as a defect would be a phantom
// finding on any schema with an i64-min default. Neither backend emits that
// spelling today (intLit writes `(-9223372036854775807LL - 1)`), which is why the
// case is pinned here rather than left to the emitted text to disprove.
func TestCHugeSignedlessConstantsIgnoresNegatives(t *testing.T) {
	for _, src := range []string{
		"int64_t x = -9223372036854775808;",
		"int64_t x = (-9223372036854775807LL - 1);",
	} {
		if bad := hugeSignedlessConstants(src); len(bad) != 0 {
			t.Errorf("%q: a negated magnitude is not an untyped constant, got %v", src, bad)
		}
	}
	// ...and the unsigned spellings it does exist to judge.
	if bad := hugeSignedlessConstants("uint64_t x = 9223372036854775808;"); len(bad) != 1 {
		t.Errorf("an unsuffixed 2^63 must be reported, got %v", bad)
	}
	if bad := hugeSignedlessConstants("uint64_t x = 9223372036854775808ULL;"); len(bad) != 0 {
		t.Errorf("a suffixed 2^63 is well-typed, got %v", bad)
	}
}

// A bitfield whose highest defaulted flag sits at position 63 has the default
// 2^63, and cDefaultInit spelled it into the const default image as a bare
// decimal. C11 6.4.4.1 looks an unsuffixed decimal constant up in the SIGNED
// list only, so it has no type at all in strict C99+; GCC accepts it as an
// extension with "integer constant is so large that it is unsigned", which is a
// warning under -pedantic and an ERROR under -pedantic-errors (measured, gcc
// 15.2.0 -std=c11, generator#480).
func TestCBitfieldDefaultAtBit63IsUnsignedConstant(t *testing.T) {
	src := "version: 1\n" +
		"messages:\n" +
		"  bf:\n" +
		"    payload:\n" +
		"      flags: { id: 0, type: bitfield, bits: { low: { pos: 0 }, high: { pos: 63, default: true } } }\n"
	files := genCFromYAML(t, src)
	all := strings.Join([]string{files["bf.h"], files["bf.c"]}, "\n")

	lits := constantsSpelling(all, 1<<63)
	if len(lits) != 1 {
		t.Fatalf("expected the 2^63 default once (the const default image), got %d: %v\n%s",
			len(lits), lits, all)
	}
	if !strings.ContainsAny(lits[0], "uU") {
		t.Errorf("2^63 spelled as %q: an unsuffixed decimal constant is looked up in "+
			"the SIGNED list only, so it has no type\n%s", lits[0], all)
	}
	if bad := hugeSignedlessConstants(all); len(bad) != 0 {
		t.Errorf("constants above INT64_MAX with no unsigned suffix: %v\n%s", bad, all)
	}
}

// The same hole one level in, and the repro above does not reach it: an ARRAY of
// bitfield renders its default element by element in cArrayDefaultInit, whose
// bitfield element fell through to intLit's untyped default arm. That is the
// shape generator#477's review found in the java twin after the scalar half had
// been fixed, so it is pinned here rather than assumed.
func TestCBitfieldArrayDefaultAtBit63IsUnsignedConstant(t *testing.T) {
	src := "version: 1\n" +
		"messages:\n" +
		"  bf3:\n" +
		"    payload:\n" +
		"      masks: { id: 0, type: array, items: { type: bitfield, count: 2, bits: { low: { pos: 0 }, high: { pos: 63 } } }, default: [1, 9223372036854775808] }\n"
	files := genCFromYAML(t, src)
	all := strings.Join([]string{files["bf3.h"], files["bf3.c"]}, "\n")

	lits := constantsSpelling(all, 1<<63)
	if len(lits) != 1 {
		t.Fatalf("expected the 2^63 element default once, got %d: %v\n%s", len(lits), lits, all)
	}
	if !strings.ContainsAny(lits[0], "uU") {
		t.Errorf("2^63 array element spelled as %q: no signed type holds it\n%s", lits[0], all)
	}
	if bad := hugeSignedlessConstants(all); len(bad) != 0 {
		t.Errorf("constants above INT64_MAX with no unsigned suffix: %v\n%s", bad, all)
	}
	// The whole brace initializer goes through the mask renderer, not only the
	// element that happens to overflow.
	if !strings.Contains(all, "{ 1ULL, 9223372036854775808ULL }") {
		t.Errorf("array default not rendered through the mask literal:\n%s", all)
	}
}

// TestValueWidthIdGuard: CORELIB_PLAN §6.2 lets a profile build the value type
// 32 bits wide (SOFAB_DISABLE_INT64_SUPPORT), and the field header (id<<3)|type
// is accumulated in it — so SOFAB_ID_MAX drops to UINT32_MAX>>3 and a larger id
// is refused at run time, per field, with InvalidArgument. The descriptor guard
// beside it does not catch that: on the BIG profile SOFAB_OBJECT_DESCR_ID_MAX is
// UINT32_MAX, four times the narrowed ceiling, so the build stays silent
// (generator#529). The guard is width-aware for free, because SOFAB_ID_MAX is.
func TestValueWidthIdGuard(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 1, type: u32 }
      b: { id: 536870912, type: u32 }
`)
	h := files["m.h"]
	if !strings.Contains(h, "#if 536870912 > SOFAB_ID_MAX") {
		t.Errorf("m.h missing the value-width id guard:\n%s", h)
	}
	if !strings.Contains(h, "#if 536870912 > SOFAB_OBJECT_DESCR_ID_MAX") {
		t.Errorf("m.h lost the descriptor width guard:\n%s", h)
	}
}

// TestValueWidthIdGuardCoversNestedIds: a nested object's fields ride the same
// (id<<3)|type header as the top message's, so the ceiling caps them too. The
// top message's own ids are small here — only the nested struct carries the id
// that would fail at run time.
func TestValueWidthIdGuardCoversNestedIds(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 1, type: u32 }
      nested:
        id: 2
        type: struct
        fields:
          deep: { id: 536870912, type: u32 }
`)
	h := files["m.h"]
	if !strings.Contains(h, "#if 536870912 > SOFAB_ID_MAX") {
		t.Errorf("m.h: the guard must take the max over nested objects too:\n%s", h)
	}
}

const int64Guard = "#if defined(SOFAB_DISABLE_INT64_SUPPORT)"

// TestWideBitfieldNeedsValue64: a bitfield whose highest declared bit is >= 32 is
// backed by uint64_t, so its descriptor entry carries element_size 8 — the arm
// object.c compiles out under SOFAB_DISABLE_INT64_SUPPORT (both _load_uint's
// 8-byte case and the 8 in _SOFAB_WIDTH_SET). Without the capability guard such a
// schema built cleanly and then returned SOFAB_RET_E_ARGUMENT from every encode
// of that field, at run time (generator#539). The capability is derived from the
// STORAGE width, so a kind list cannot drift away from it again.
func TestWideBitfieldNeedsValue64(t *testing.T) {
	bf := func(pos int) string {
		return fmt.Sprintf(`
version: 1
messages:
  m:
    payload:
      flags:
        id: 1
        type: bitfield
        bits:
          low: { pos: 0 }
          high: { pos: %d }
`, pos)
	}

	h := genCFromYAML(t, bf(40))["m.h"]
	if !strings.Contains(h, "uint64_t flags;") {
		t.Fatalf("a bit at 40 must be backed by uint64_t:\n%s", h)
	}
	if !strings.Contains(h, int64Guard) {
		t.Errorf("a uint64_t-backed bitfield needs the INT64 capability guard:\n%s", h)
	}

	// Control: one bit lower fits uint32_t, which the narrow corelib reads fine —
	// the guard is a property of the storage width, not of "is a bitfield".
	ctl := genCFromYAML(t, bf(31))["m.h"]
	if !strings.Contains(ctl, "uint32_t flags;") {
		t.Fatalf("a bit at 31 must fit uint32_t:\n%s", ctl)
	}
	if strings.Contains(ctl, int64Guard) {
		t.Errorf("a uint32_t-backed bitfield must not demand INT64:\n%s", ctl)
	}
}

// TestCBitfieldFlagConstants (generator#606): the FIELD stays a raw integer —
// MESSAGE_SPEC §1 admits any value inside the declared width, named or not,
// so a closed type would reject a value a newer peer's vocabulary produced —
// but the declared bit POSITIONS get named #defines beside it, the same way
// Go emits typed consts and Kotlin emits const vals next to an equally raw
// field. Before this, C emitted neither: a caller had to know the schema by
// heart to set/test any bit (see docs/ARCHITECTURE.md's "C and Java lower
// enum/bitfield fields to a raw integer and emit no named constants").
func TestCBitfieldFlagConstants(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  fridge:
    payload:
      alarms:
        id: 0
        type: bitfield
        description: Active alarm conditions
        bits:
          door_open_too_long: { pos: 0, default: false, description: "A door has been open past the timeout" }
          temp_high: { pos: 1, description: "A zone is above its warning threshold" }
`)
	h := files["fridge.h"]

	// The field itself is unchanged: still a raw, permissive integer.
	if !strings.Contains(h, "uint8_t alarms;") {
		t.Fatalf("alarms must stay a raw uint8_t, not narrow to the declared flags:\n%s", h)
	}

	for _, want := range []string{
		"#define MESSAGE_FRIDGE_ALARMS_DOOR_OPEN_TOO_LONG ((uint32_t)1 << 0)",
		"#define MESSAGE_FRIDGE_ALARMS_TEMP_HIGH ((uint32_t)1 << 1)",
		"/*! A door has been open past the timeout (default: false) */",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q:\n%s", want, h)
		}
	}
	// A flag with no declared default carries no (default: ...) note — nothing
	// to claim, unlike Go's which always states one because a Go field always
	// has a zero value.
	if strings.Contains(h, "temp_high") && strings.Contains(h, "A zone is above its warning threshold (default:") {
		t.Errorf("temp_high has no declared default and must not claim one:\n%s", h)
	}
}

// TestCBitfieldFlagConstantsDedupSharedRef: a $ref-shared bitfield used by two
// fields must emit its #define block once, not once per field — a second
// copy would be a duplicate-macro-definition compile error, not just noise.
func TestCBitfieldFlagConstantsDedupSharedRef(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
$defs:
  bitfield:
    Shared:
      a: { pos: 0 }
messages:
  m:
    payload:
      first: { id: 0, type: bitfield, bits: { $ref: "#/$defs/bitfield/Shared" } }
      second: { id: 1, type: bitfield, bits: { $ref: "#/$defs/bitfield/Shared" } }
`)
	h := files["m.h"]
	if n := strings.Count(h, "#define MESSAGE_BITFIELD_SHARED_A ((uint32_t)1 << 0)"); n != 1 {
		t.Errorf("expected the shared bitfield's #define exactly once, got %d:\n%s", n, h)
	}
}

// TestCBitfieldFlagConstantsInArray: a native array of bitfield (each element
// the same raw integer type) must still name the flags once, from the
// element's ref, not skip them the way an unrelated composite kind should.
func TestCBitfieldFlagConstantsInArray(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      flags:
        id: 0
        type: array
        items: { type: bitfield, count: 3, bits: { ready: { pos: 0 } } }
`)
	h := files["m.h"]
	if !strings.Contains(h, "#define MESSAGE_M_FLAGS_ELEM_READY ((uint32_t)1 << 0)") {
		t.Errorf("array-of-bitfield element flags must still be named:\n%s", h)
	}
}

// TestCBitfieldFlagConstantsWidePos: the shifted literal takes the FIELD's
// width, not the flag's. A bare `1u` is only guaranteed 16 bits, so shifting it
// to pos 40 is undefined; and a 32-bit literal for the low flag of a uint64_t
// field breaks `v &= ~FLAG`, whose zero-extended complement clears bits 32..63
// too. Every flag of a uint64_t field is therefore a uint64_t.
func TestCBitfieldFlagConstantsWidePos(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      wide:
        id: 0
        type: bitfield
        bits:
          low: { pos: 1 }
          high: { pos: 40 }
`)
	h := files["m.h"]
	if !strings.Contains(h, "uint64_t wide;") {
		t.Fatalf("a pos-40 flag must widen the field to uint64_t:\n%s", h)
	}
	for _, want := range []string{
		"#define MESSAGE_M_WIDE_LOW ((uint64_t)1 << 1)",
		"#define MESSAGE_M_WIDE_HIGH ((uint64_t)1 << 40)",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q:\n%s", want, h)
		}
	}
}

// TestCBitfieldFlagConstantsCollideWithSizeMacro: a field/flag name pair that
// joins to the same identifier as the message's own MAX_SIZE macro must be a
// generate-time error, not a silently redefined #define (one of the two
// symbols would otherwise quietly take the wrong value).
func TestCBitfieldFlagConstantsCollideWithSizeMacro(t *testing.T) {
	err := genCErr(t, `
version: 1
messages:
  m:
    payload:
      max:
        id: 0
        type: bitfield
        bits:
          size: { pos: 0 }
`)
	if err == nil {
		t.Fatal("expected a generate-time error for a flag macro colliding with MAX_SIZE")
	}
	if !strings.Contains(err.Error(), "MESSAGE_M_MAX_SIZE") {
		t.Errorf("error %q should name the colliding macro", err)
	}
}

// TestCBitfieldFlagConstantsCollideAcrossFields: two differently-split
// field/flag names can join to the identical macro identifier (field "a_b"
// flag "c" vs. field "a" flag "b_c") — same collision, same required error.
func TestCBitfieldFlagConstantsCollideAcrossFields(t *testing.T) {
	err := genCErr(t, `
version: 1
messages:
  m:
    payload:
      a_b:
        id: 0
        type: bitfield
        bits:
          c: { pos: 0 }
      a:
        id: 1
        type: bitfield
        bits:
          b_c: { pos: 0 }
`)
	if err == nil {
		t.Fatal("expected a generate-time error for two flags joining to the same macro name")
	}
	if !strings.Contains(err.Error(), "MESSAGE_M_A_B_C") {
		t.Errorf("error %q should name the colliding macro", err)
	}
}

// TestCBitfieldFlagConstantsCollideWithSiblingGuard: C's macro namespace spans
// every header a translation unit includes, so the collision check spans the
// schema. Message "a"'s field "b" with flag "h" is MESSAGE_A_B_H, the include
// guard of message "a_b": including a.h first would silently skip a_b.h.
func TestCBitfieldFlagConstantsCollideWithSiblingGuard(t *testing.T) {
	err := genCErr(t, `
version: 1
messages:
  a:
    payload:
      b: { id: 0, type: bitfield, bits: { h: { pos: 0 } } }
  a_b:
    payload:
      x: { id: 0, type: u8 }
`)
	if err == nil {
		t.Fatal("expected a generate-time error for a flag macro equal to another message's include guard")
	}
	if !strings.Contains(err.Error(), "MESSAGE_A_B_H") {
		t.Errorf("error %q should name the colliding macro", err)
	}
}

// TestCBitfieldFlagConstantsSharedAcrossMessages: a $ref-shared bitfield used
// by two messages emits the same #define in both headers. That is an identical
// redefinition, which C admits, so it must not trip the collision check.
func TestCBitfieldFlagConstantsSharedAcrossMessages(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
$defs:
  bitfield:
    Shared:
      a: { pos: 0 }
messages:
  m:
    payload:
      f: { id: 0, type: bitfield, bits: { $ref: "#/$defs/bitfield/Shared" } }
  n:
    payload:
      f: { id: 0, type: bitfield, bits: { $ref: "#/$defs/bitfield/Shared" } }
`)
	for _, name := range []string{"m.h", "n.h"} {
		if !strings.Contains(files[name], "#define MESSAGE_BITFIELD_SHARED_A ((uint32_t)1 << 0)") {
			t.Errorf("%s must carry the shared flag macro:\n%s", name, files[name])
		}
	}
}

// TestScalarFP64DoesNotNeedValue64: `double` is 8 bytes too, but its descriptor
// type is FP64 — it never reaches _load_uint and has SOFAB_DISABLE_FP64_SUPPORT
// of its own. Deriving value64 from the storage width must not swallow it.
func TestScalarFP64DoesNotNeedValue64(t *testing.T) {
	h := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      d: { id: 1, type: fp64 }
`)["m.h"]
	if !strings.Contains(h, "#if defined(SOFAB_DISABLE_FP64_SUPPORT)") {
		t.Errorf("m.h must still guard FP64:\n%s", h)
	}
	if strings.Contains(h, int64Guard) {
		t.Errorf("a scalar fp64 field must not demand INT64:\n%s", h)
	}
}

// TestBooleanUsesTheBooleanFieldTypes pins generator#581 on the C target: a
// boolean is described as BOOLEAN / ARRAY_BOOLEAN, never UNSIGNED. The wire form
// is the same unsigned varint, but only the boolean tags make corelib-c-cpp read
// it under CORELIB_PLAN §4.4 -- every non-zero value is true and normalized to 1,
// with no width bound. Described as UNSIGNED, 2 was stored raw and 256 was
// rejected as INVALID by the one-byte width check. All three positions a boolean
// reaches the descriptor from: a scalar, an array, and a nested-array row.
func TestBooleanUsesTheBooleanFieldTypes(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      flag:  { id: 0, type: boolean }
      flags: { id: 1, type: array, items: { type: boolean, count: 5 } }
      rows:  { id: 2, type: array, items: { type: array, count: 2, items: { type: boolean, count: 3 } } }
`)
	h, c := files["m.h"], files["m.c"]
	for _, want := range []string{
		"uint8_t flag;",
		"uint8_t flags_len; uint8_t flags[5];",
		"struct { uint8_t len; uint8_t vals[3]; } items[2];",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.h missing %q:\n%s", want, h)
		}
	}
	for _, want := range []string{
		"SOFAB_OBJECT_FIELD(0, message_m_t, flag, SOFAB_OBJECT_FIELDTYPE_BOOLEAN),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(1, message_m_t, flags, flags_len, SOFAB_OBJECT_FIELDTYPE_ARRAY_BOOLEAN),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(0, message_m_rows_elems_t, items[0].vals, items[0].len, SOFAB_OBJECT_FIELDTYPE_ARRAY_BOOLEAN),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(1, message_m_rows_elems_t, items[1].vals, items[1].len, SOFAB_OBJECT_FIELDTYPE_ARRAY_BOOLEAN),",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("m.c missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "FIELDTYPE_UNSIGNED") || strings.Contains(c, "FIELDTYPE_ARRAY_UNSIGNED") {
		t.Errorf("m.c still describes a boolean as unsigned (§4.4):\n%s", c)
	}
}

// TestHarnessEmitsOnlyCalledJSONHelpers: the project harness's static JSON
// helpers are emitted only when the harness calls them. An uncalled static
// function is a -Wunused-function diagnostic, so a schema with no string or blob
// field must not carry the string/blob helpers.
func TestHarnessEmitsOnlyCalledJSONHelpers(t *testing.T) {
	helpers := []string{"json_str", "json_bytes", "json_to_str", "json_to_bytes"}
	hs := genCProject(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
`)["harness/main.c"]
	for _, h := range helpers {
		if strings.Contains(hs, h+"(") {
			t.Errorf("harness defines or calls %s with no string or blob field:\n%s", h, hs)
		}
	}
	hs = genCProject(t, `
version: 1
messages:
  M:
    payload:
      s: { id: 0, type: string, maxlen: 8 }
      b: { id: 1, type: blob, maxlen: 8 }
`)["harness/main.c"]
	for _, h := range helpers {
		if !strings.Contains(hs, "static ") || !strings.Contains(hs, " "+h+"(") {
			t.Errorf("harness must define %s when it calls it:\n%s", h, hs)
		}
	}
}

// TestCHarnessWithoutMessages: a schema of shared types alone generates no C
// type (the target emits per message), so its project harness converts nothing
// and declares no bench sink -- it used to index the absent first message and
// panic, and would otherwise reference types that do not exist.
func TestCHarnessWithoutMessages(t *testing.T) {
	out := genCFromYAMLCfg(t, "version: 1\n$defs:\n  struct:\n    P: { x: { id: 0, type: u8 } }\n", map[string]any{"emit": "project"})
	main, ok := out["harness/main.c"]
	if !ok {
		t.Fatal("no harness/main.c")
	}
	for _, bad := range []string{"_to_json", "_from_json", "sink", "sofab_ret_name", "size_t i;"} {
		if strings.Contains(main, bad) {
			t.Errorf("message-less harness contains %q:\n%s", bad, main)
		}
	}
	if !strings.Contains(main, "int main(int argc, char **argv) {") {
		t.Errorf("message-less harness has no main:\n%s", main)
	}
}
