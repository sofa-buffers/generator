package golang

import (
	goparser "go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	defparser "github.com/sofa-buffers/generator/internal/parser"
)

func exampleSchema(t *testing.T) *ir.Schema {
	t.Helper()
	return schemaFromYAMLFile(t, "../../examples/messages/example.yaml")
}

func schemaFromYAMLString(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := defparser.Parse([]byte(src), "vec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return analyzed(t, doc)
}

func schemaFromYAMLFile(t *testing.T, path string) *ir.Schema {
	t.Helper()
	doc, err := defparser.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return analyzed(t, doc)
}

func analyzed(t *testing.T, doc *defparser.Document) *ir.Schema {
	t.Helper()
	resolved, _ := doc.Resolve()
	if errs := defparser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
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

func genGo(t *testing.T, s *ir.Schema, cfg map[string]any) map[string]string {
	t.Helper()
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

func TestGeneratedGoParses(t *testing.T) {
	files := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	fset := token.NewFileSet()
	for path, src := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if _, err := goparser.ParseFile(fset, path, []byte(src), goparser.AllErrors); err != nil {
			t.Errorf("generated %s is not valid Go: %v", path, err)
		}
	}
}

func TestGoStructuralInvariants(t *testing.T) {
	files := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	msg := files["myfirstmessage.go"]
	for _, want := range []string{
		"package messages",
		"func (m *Myfirstmessage) marshal(e *sofab.Encoder)",
		"_visitorBase", // struct embeds the no-op base
		"func (m *Myfirstmessage) Unsigned(id sofab.ID, v uint64) error", // visitor decode
		"func (m *Myfirstmessage) BeginSequence(id sofab.ID) (sofab.Visitor, error)",
		"func NewMyfirstmessage() *Myfirstmessage",
		"func DecodeMyfirstmessage(",
		"sofab.AcceptBytes(data, m)", // zero-copy cursor decode
		"e.WriteSequenceBegin(",      // nested struct/union framing (marshal unchanged)
		"`json:\"somei8\"`",          // canonical json tags
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("myfirstmessage.go missing %q", want)
		}
	}
	for _, notWant := range []string{
		"func (m *Myfirstmessage) unmarshal(d *sofab.Decoder)", // pull-parser removed
		"d.Next()",
	} {
		if strings.Contains(msg, notWant) {
			t.Errorf("myfirstmessage.go should no longer contain %q (pull-parser replaced by visitor)", notWant)
		}
	}
	// The decode prelude (embedded no-op base + collectors) is emitted once.
	prelude := files["sofab_visitor.go"]
	for _, want := range []string{
		"type _visitorBase struct{}",
		"func _narrowU[T ~uint8 | ~uint16 | ~uint32 | ~uint64](v []uint64) []T",
		"type _strSeq struct {",
	} {
		if !strings.Contains(prelude, want) {
			t.Errorf("sofab_visitor.go missing %q", want)
		}
	}
	types := files["types.go"]
	if !strings.Contains(types, "type MyfirstmessageSomeenum int8") {
		t.Errorf("enum backing type missing/incorrect:\n%s", firstLines(types, 12))
	}
}

// A blob field inside a nested struct/union lands in types.go (not the message
// file), and its marshal uses bytes.Equal. Regression for #84: types.go must
// import "bytes" itself rather than relying on the message file's own import.
// go/parser only parses, so it never caught this — the failure is an undefined
// identifier at compile time. Here we assert every generated file that
// references bytes. also imports it.
func TestGoNestedBlobImportsBytes(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  outer:
    payload:
      nested:
        id: 0
        type: struct
        fields:
          bytes_field:
            id: 3
            type: blob
`)
	files := genGo(t, s, map[string]any{"package": "messages"})
	types := files["types.go"]
	if !strings.Contains(types, "bytes.Equal") {
		t.Fatalf("expected nested blob marshal to use bytes.Equal in types.go:\n%s", firstLines(types, 20))
	}
	for path, src := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if strings.Contains(src, "bytes.") && !strings.Contains(src, `"bytes"`) {
			t.Errorf("%s references bytes. but does not import \"bytes\":\n%s", path, firstLines(src, 12))
		}
	}
}

func TestGoDeterministic(t *testing.T) {
	a := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	b := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	if a["myfirstmessage.go"] != b["myfirstmessage.go"] || a["types.go"] != b["types.go"] {
		t.Fatal("Go generation is not deterministic")
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestGoDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated package — constants in the prelude
// plus sofab.WithMax* options on every AcceptBytes call. The cap is raised to
// the largest schema bound of its kind (escape hatch: schema-bounded fields
// stay governed by their own bound), an unset key emits nothing, and a key
// whose kind has no unbounded field is inert.
func TestGoDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
`
	s := schemaFromYAMLString(t, src)
	files := genGo(t, s, map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	})
	prelude, msg := files["sofab_visitor.go"], files["dyn.go"]
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`MaxDynArrayCount\s+= 100000`), // raised to the schema count of barr
		regexp.MustCompile(`MaxDynStringLen\s+= 4096`),
	} {
		if !want.MatchString(prelude) {
			t.Errorf("prelude missing %v", want)
		}
	}
	if strings.Contains(prelude, "MaxDynBlobLen") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	if !strings.Contains(msg, "sofab.AcceptBytes(data, m, sofab.WithMaxArrayCount(MaxDynArrayCount), sofab.WithMaxStringLen(MaxDynStringLen))") {
		t.Error("Decode must pass the active limits into AcceptBytes")
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := genGo(t, s, map[string]any{})
	if strings.Contains(plain["sofab_visitor.go"], "MaxDyn") || strings.Contains(plain["dyn.go"], "WithMax") {
		t.Error("unset limits must emit no limit plumbing")
	}
}
