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

// TestOmitDefaultsGenerates: with omit_defaults=true, every backend still
// generates valid output (Go parses), and the marshal becomes conditional —
// proving the option flows through and the codegen stays well-formed.
func TestOmitDefaultsGenerates(t *testing.T) {
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

	cfg := map[string]any{"omit_defaults": true}
	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		files, err := b.Generate(s, cfg)
		if err != nil {
			t.Errorf("[%s] generate with omit_defaults: %v", lang, err)
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
		// C is sparse via object.h (no generated conditional); every other
		// backend must emit an omit guard.
		if lang != "c" && !sawConditional {
			t.Errorf("[%s] omit_defaults produced no conditional write", lang)
		}
	}
}

// TestOmitDefaultsOffIsDense: without the option, the Go marshal writes
// unconditionally (no per-field guard) — the default behavior is unchanged.
func TestOmitDefaultsOffIsDense(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/scalars.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generator.Lookup("go")
	files, _ := b.Generate(s, map[string]any{})
	for _, f := range files {
		if strings.HasSuffix(f.Path, "scalars.go") {
			if strings.Contains(string(f.Content), "if m.") {
				t.Error("default (omit off) Go marshal should be unconditional")
			}
		}
	}
}
