// Package matrix is the M7 corpus runner: it validates a corner-case corpus of
// definitions, generates each across ALL registered language backends, and
// confirms the invalid corpus is rejected. It is hermetic (no toolchains) so it
// runs in the core CI job.
package matrix

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	defparser "github.com/sofa-buffers/generator/internal/parser"

	// Register every backend.
	_ "github.com/sofa-buffers/generator/generators/c"
	_ "github.com/sofa-buffers/generator/generators/cpp"
	_ "github.com/sofa-buffers/generator/generators/csharp"
	_ "github.com/sofa-buffers/generator/generators/docs"
	_ "github.com/sofa-buffers/generator/generators/golang"
	_ "github.com/sofa-buffers/generator/generators/java"
	_ "github.com/sofa-buffers/generator/generators/python"
	_ "github.com/sofa-buffers/generator/generators/rust"
	_ "github.com/sofa-buffers/generator/generators/typescript"
	_ "github.com/sofa-buffers/generator/generators/zig"
)

// fixedOnlyTarget reports whether a backend has a single fixed-capacity
// (heapless) profile with no dynamic-container fallback. Such a target cannot
// generate a schema with an unbounded field — that is a deliberate hard error
// (generator#104). The cpp c-cpp and rust no_std fixed profiles are opt-in
// configs, so their DEFAULT config (heap / maxspeed) still generates unbounded
// schemas in this matrix; only C is heapless by default.
func fixedOnlyTarget(lang string) bool { return lang == "c" }

// hasUnboundedField reports whether any field lowers to storage a fixed-capacity
// target cannot size from the schema: a string/blob without maxlen, an array (at
// any depth, ANY element kind) without count, or a string/blob array element
// without its own maxlen. Mirrors the C backend's checkBounded so the matrix can
// skip exactly the (fixed-only-target, unbounded-schema) combos that error by
// design; those are covered by the per-backend tests and conformance harnesses.
func hasUnboundedField(s *ir.Schema) bool {
	seen := map[string]bool{}
	var walkFields func(key string, fields []*ir.Field) bool
	var walkArray func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool) bool
	walkArray = func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool) bool {
		if count <= 0 {
			return true
		}
		switch elem {
		case ir.KindString, ir.KindBlob:
			return !elemMaxHas
		case ir.KindStruct, ir.KindUnion:
			return walkFields(ref.Key, ref.Target.Fields)
		case ir.KindArray:
			return walkArray(items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas)
		}
		return false
	}
	walkFields = func(key string, fields []*ir.Field) bool {
		if seen[key] {
			return false
		}
		seen[key] = true
		for _, f := range fields {
			switch f.Kind {
			case ir.KindString, ir.KindBlob:
				if !f.HasMaxlen {
					return true
				}
			case ir.KindStruct, ir.KindUnion:
				if walkFields(f.Ref.Key, f.Ref.Target.Fields) {
					return true
				}
			case ir.KindArray:
				if walkArray(f.Elem, f.ElemRef, f.ElemItems, f.Count, f.ElemMaxHas) {
					return true
				}
			}
		}
		return false
	}
	for _, m := range s.Messages {
		if walkFields("message/"+m.Name, m.Fields) {
			return true
		}
	}
	return false
}

func buildIR(t *testing.T, path string) (*ir.Schema, error) {
	t.Helper()
	doc, err := defparser.Load(path)
	if err != nil {
		return nil, err
	}
	resolved, err := doc.Resolve()
	if err != nil {
		return nil, err
	}
	if errs := defparser.Validate(resolved); errs != nil {
		return nil, errs
	}
	s, err := model.Build(doc)
	if err != nil {
		return nil, err
	}
	if err := analysis.Analyze(s); err != nil {
		return nil, err
	}
	return s, nil
}

// TestCorpusGeneratesEverywhere: every positive def validates, builds an IR, and
// generates non-empty output for every registered backend; generated Go parses.
func TestCorpusGeneratesEverywhere(t *testing.T) {
	defs, _ := filepath.Glob("corpus/defs/*.yaml")
	if len(defs) == 0 {
		t.Fatal("no corpus defs found")
	}
	langs := generator.Registered()
	if len(langs) < 8 {
		t.Fatalf("expected >=8 backends, got %v", langs)
	}
	for _, def := range defs {
		def := def
		t.Run(filepath.Base(def), func(t *testing.T) {
			s, err := buildIR(t, def)
			if err != nil {
				t.Fatalf("should validate: %v", err)
			}
			for _, lang := range langs {
				if fixedOnlyTarget(lang) && hasUnboundedField(s) {
					continue // deliberate hard error; covered by the c backend tests
				}
				b, _ := generator.Lookup(lang)
				files, err := b.Generate(s, map[string]any{})
				if err != nil {
					t.Errorf("[%s] generate: %v", lang, err)
					continue
				}
				if len(files) == 0 {
					t.Errorf("[%s] no files", lang)
				}
				for _, f := range files {
					if len(f.Content) == 0 {
						t.Errorf("[%s] empty file %s", lang, f.Path)
					}
					if lang == "go" && strings.HasSuffix(f.Path, ".go") {
						if _, perr := parser.ParseFile(token.NewFileSet(), f.Path, f.Content, parser.AllErrors); perr != nil {
							t.Errorf("[go] generated %s does not parse: %v", f.Path, perr)
						}
					}
				}
			}
		})
	}
}

// TestInvalidCorpusRejected: every invalid def fails the hard gate.
func TestInvalidCorpusRejected(t *testing.T) {
	bad, _ := filepath.Glob("corpus/invalid/*.yaml")
	if len(bad) == 0 {
		t.Fatal("no invalid corpus found")
	}
	for _, def := range bad {
		def := def
		t.Run(filepath.Base(def), func(t *testing.T) {
			if _, err := buildIR(t, def); err == nil {
				t.Fatalf("%s should have been rejected but validated", def)
			}
		})
	}
}

// TestDanglingRefRejected: a $ref with no target fails at resolve time.
func TestDanglingRefRejected(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      a: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Nope' } }\n"
	doc, err := defparser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Resolve(); err == nil {
		t.Fatal("dangling $ref should fail to resolve")
	}
}

// TestNestingDepthCap: a struct nested past MAX_NESTING_DEPTH is rejected.
func TestNestingDepthCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nmessages:\n  Deep:\n    payload:\n")
	indent := "      "
	// build a chain of nested structs deeper than the cap
	depth := ir.MaxNestingDepth + 2
	for i := 0; i < depth; i++ {
		b.WriteString(indent + "f:\n")
		b.WriteString(indent + "  id: 0\n")
		b.WriteString(indent + "  type: struct\n")
		b.WriteString(indent + "  fields:\n")
		indent += "    "
	}
	b.WriteString(indent + "leaf: { id: 0, type: u8 }\n")

	tmp := filepath.Join(t.TempDir(), "deep.yaml")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildIR(t, tmp); err == nil {
		t.Fatalf("nesting depth %d should exceed the cap (%d)", depth, ir.MaxNestingDepth)
	}
}
