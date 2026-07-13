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
		"case 15: m.someuintarray = new long[Math.min(count, ARRAY_INIT_CAP)]; break;",                // bounded reservation, not new long[count] (#96)
		"private static long[] ensureCap(long[] a, int i, int cap) {",                                 // lazy-growth helper
		"private static float[] ensureCap(float[] a, int i, int cap) {",                               // fp32 overload
		"if (offset == 0 && chunkLength >= total) {",                                                  // string/blob single-shot
		"public List<Boolean> someboolarray",                                                          // boolean array stays boxed List
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
