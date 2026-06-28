package ir_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// TestExampleIRGolden locks the IR shape for example.yaml (the M1 freeze). If
// the IR projection changes, regenerate with:
//
//	go run ./cmd/sbufgen --dump-ir --in examples/example.yaml > internal/ir/testdata/example.ir.json
//
// A diff here is a deliberate, reviewed change to the frozen format.
func TestExampleIRGolden(t *testing.T) {
	def := filepath.Join("..", "..", "examples", "example.yaml")
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
	schema, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(schema); err != nil {
		t.Fatal(err)
	}

	got := schema.Dump()
	goldenPath := filepath.Join("testdata", "example.ir.json")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden: %v (regenerate with --dump-ir)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("IR drift vs golden %s.\nRegenerate if intentional:\n  go run ./cmd/sbufgen --dump-ir --in examples/example.yaml > %s\n--- got ---\n%s",
			goldenPath, goldenPath, got)
	}
}
