package rust

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleModule(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/example.yaml")
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
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "src/messages.rs" {
			return string(f.Content)
		}
	}
	t.Fatal("no module")
	return ""
}

func TestRustStructural(t *testing.T) {
	m := exampleModule(t)
	for _, want := range []string{
		"use sofab::{OStream, IStream, Visitor, Id, Unsigned, Signed, ArrayKind};",
		"sofab::require!(", // capability guard
		"pub struct Myfirstmessage {",
		"pub fn marshal(&self, os: &mut OStream)",
		"pub fn encode(&self) -> Vec<u8>",
		"pub fn decode(data: &[u8]) -> Self",
		"mod myfirstmessage_dec {",             // isolated decode module
		"fn sequence_begin(&mut self, id: Id)", // flat-visitor nesting
		"pub bignum: u64,",
		"#[serde(default)]",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("messages.rs missing %q", want)
		}
	}
	// capabilities present for this example (string/blob/fp, sequence, value64, array)
	for _, cap := range []string{"fixlen", "sequence", "value64", "array"} {
		if !strings.Contains(m, cap) {
			t.Errorf("expected require!(... %s ...)", cap)
		}
	}
}

func TestRustDeterministic(t *testing.T) {
	if exampleModule(t) != exampleModule(t) {
		t.Fatal("Rust generation not deterministic")
	}
}
