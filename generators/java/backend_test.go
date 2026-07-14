package java

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(b, "example.yaml")
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
	files, err := (&Backend{}).Generate(s, map[string]any{"package": "messages"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "Myfirstmessage.java") {
			return string(f.Content)
		}
	}
	t.Fatal("no message file")
	return ""
}

func TestJavaStructural(t *testing.T) {
	m := exampleFile(t)
	for _, want := range []string{
		"package messages;",
		"import org.sofabuffers.sofab.*;",
		"public class Myfirstmessage {",
		"public void marshal(OStream os) throws IOException",
		"public byte[] encode()",
		"public static Myfirstmessage decode(byte[] data)",
		"public static DecodeStatus tryDecode(byte[] data, Myfirstmessage out) throws SofabException", // status-surfacing decode (#105)
		"class MyfirstmessageVisitor implements Visitor {",
		"public void sequenceBegin(int id)", // flat-visitor nesting
		"public long someu64 = Long.parseUnsignedLong(\"18446744073709551615\");",
		"class MyfirstmessageSomestructNestedstruct {",                                                // nested types in file
		"public long[] someuintarray = new long[]{0L, 1L, 1000L, 4294967295L};",                       // primitive array (was List<Long>)
		"public float[] somefloatarray = new float[]{0.0f, -1.5f, 3.25f};",                            // primitive fp array
		"public long[] someenumarray = new long[]{2L, 1L, 0L, 0};",                                    // short default tail-padded to count
		"os.writeArrayUnsigned(15, this.someuintarray);",                                              // direct write, no Sbuf box
		"if (!java.util.Arrays.equals(this.someuintarray, new long[]{0L, 1L, 1000L, 4294967295L})) {", // Arrays.equals guard
		"m.someuintarray = ensureCap(m.someuintarray, ai, acap); m.someuintarray[ai++] = value;",      // grow-on-demand indexed decode (#96)
		"case 15: if (count > 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"someuintarray: array count above schema capacity 4\")); m.someuintarray = new long[Math.min(count, ARRAY_INIT_CAP)]; break;", // over-count rejected (#100), bounded reservation (#96)
		"private static long[] ensureCap(long[] a, int i, int cap) {",   // lazy-growth helper
		"private static float[] ensureCap(float[] a, int i, int cap) {", // fp32 overload
		"if (offset == 0 && chunkLength >= total) {",                    // string/blob single-shot
		"public List<Boolean> someboolarray",                            // boolean array stays boxed List
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Myfirstmessage.java missing %q", want)
		}
	}
}

func TestJavaDeterministic(t *testing.T) {
	if exampleFile(t) != exampleFile(t) {
		t.Fatal("Java generation not deterministic")
	}
}

// genJavaFromYAML generates from an inline definition and returns the emitted
// files keyed by path.
func genJavaFromYAML(t *testing.T, src string, cfg map[string]any) map[string]string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "dyn.yaml")
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

// TestJavaDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated visitor — named constants plus a
// LIMIT_EXCEEDED guard on every schema-unbounded field, checked at the wire
// count / total header before any allocation. Schema-bounded fields keep only
// their generator#100 INVALID_MSG guard; an unset key (or a key whose kind has
// no unbounded field) emits nothing, keeping the output byte-identical.
func TestJavaDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 6 } }
`
	cfg := map[string]any{
		"max_dyn_array_count": 4,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	}
	m := genJavaFromYAML(t, src, cfg)["src/main/java/message/Dyn.java"]
	for _, want := range []string{
		"static final long MAX_DYN_ARRAY_COUNT = 4L;",
		"static final long MAX_DYN_STRING_LEN = 4096L;",
		// Unbounded array: count checked against the cap before the (lazy) reservation.
		`case 1: if (count > MAX_DYN_ARRAY_COUNT) throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "arr: array count above configured limit 4")); m.arr = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// Bounded array: only the generator#100 schema guard, never the cap.
		`case 2: if (count > 6) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "barr: array count above schema capacity 6")); m.barr = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// Unbounded string: total checked at the top of string(), before accumulation.
		"if (total > MAX_DYN_STRING_LEN) {",
		`case 0: throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "s: string length above configured limit 4096"));`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Dyn.java missing %q", want)
		}
	}
	if strings.Contains(m, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}

	// No limits configured -> no limit plumbing at all.
	plain := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/Dyn.java"]
	if strings.Contains(plain, "MAX_DYN") || strings.Contains(plain, "LIMIT_EXCEEDED") {
		t.Error("unset limits must emit no limit plumbing")
	}
}
