package golang

import (
	goparser "go/parser"
	"go/token"
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
		"func (m *Myfirstmessage) unmarshal(d *sofab.Decoder) error",
		"func NewMyfirstmessage() *Myfirstmessage",
		"func DecodeMyfirstmessage(",
		"e.WriteSequenceBegin(",  // nested struct/union framing
		"d.Next()",               // pull-parser decode
		"`json:\"someinteger\"`", // canonical json tags
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("myfirstmessage.go missing %q", want)
		}
	}
	types := files["types.go"]
	if !strings.Contains(types, "type MyfirstmessageSomeenum int8") {
		t.Errorf("enum backing type missing/incorrect:\n%s", firstLines(types, 12))
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
