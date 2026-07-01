package matrix

import (
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/model"
	defparser "github.com/sofa-buffers/generator/internal/parser"
)

// TestAllBackendsSparse: encoding is always sparse-canonical (MESSAGE_SPEC §2,
// no config toggle), so every backend's marshal is conditional (a per-field
// "!= default" guard) with the default config, and stays well-formed (Go parses).
func TestAllBackendsSparse(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: u32, default: 0 }\n" +
		"      b: { id: 1, type: i32, default: 10 }\n" +
		"      c: { id: 2, type: string, maxlen: 16 }\n" +
		"      d: { id: 3, type: boolean, default: true }\n" +
		"      e: { id: 4, type: u64, default: \"18446744073709551615\" }\n"
	tmp := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(tmp, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := defparser.Load(tmp)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := defparser.Validate(resolved); errs != nil {
		t.Fatal(errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}

	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		files, err := b.Generate(s, map[string]any{})
		if err != nil {
			t.Errorf("[%s] generate: %v", lang, err)
			continue
		}
		sawConditional := false
		for _, f := range files {
			body := string(f.Content)
			if strings.Contains(body, "if ") && (strings.Contains(body, "!= ") || strings.Contains(body, "!==") || strings.Contains(body, "!java")) {
				sawConditional = true
			}
			if lang == "go" && strings.HasSuffix(f.Path, ".go") {
				if _, err := goparser.ParseFile(token.NewFileSet(), f.Path, []byte(body), goparser.AllErrors); err != nil {
					t.Errorf("[go] omit output does not parse %s: %v", f.Path, err)
				}
			}
		}
		// C is sparse via the object.h descriptor (no generated conditional);
		// every other backend must emit a per-field sparse guard.
		if lang != "c" && !sawConditional {
			t.Errorf("[%s] sparse-canonical marshal produced no conditional write", lang)
		}
	}
}

// TestGoMarshalIsSparse: the Go marshal is always sparse-canonical (MESSAGE_SPEC
// §2) — every leaf field is written under an "if != default" guard, with no
// config toggle. (The corelibs are dumb codecs; the sparse rule lives in the
// generated code. Only the C backend defers omission to the object.h descriptor.)
func TestGoMarshalIsSparse(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/scalars.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generator.Lookup("go")
	files, _ := b.Generate(s, map[string]any{})
	for _, f := range files {
		if strings.HasSuffix(f.Path, "scalars.go") {
			if !strings.Contains(string(f.Content), "if m.") {
				t.Error("Go marshal must be sparse-canonical (per-field != default guard)")
			}
		}
	}
}
