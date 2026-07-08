package rust

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleSchema(t *testing.T) *ir.Schema {
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
	return s
}

func exampleModule(t *testing.T, cfg map[string]any) string {
	t.Helper()
	s := exampleSchema(t)
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "src/message.rs" {
			return string(f.Content)
		}
	}
	t.Fatal("no module")
	return ""
}

func TestRustStructural(t *testing.T) {
	// Default corelib is the std corelib-rs: no feature flags, no require! guard.
	m := exampleModule(t, map[string]any{})
	for _, want := range []string{
		"use sofab::{OStream, IStream, Visitor, Id, Unsigned, Signed};",
		"pub struct Myfirstmessage {",
		"pub fn marshal(&self, os: &mut OStream)",
		"pub fn encode(&self) -> Vec<u8>",
		"pub fn decode(data: &[u8]) -> Self",
		"mod myfirstmessage_dec {",             // isolated decode module
		"fn sequence_begin(&mut self, id: Id)", // flat-visitor nesting
		"ArrayKind",                            // example has arrays -> array_begin imports it
		"pub someu64: u64,",
		"#[serde(default)]",
		"pub someuintarray: [u32; 4],",                                           // fixed native array (was Vec<u32>)
		"pub somefloatarray: [f32; 3],",                                          // fixed fp array
		"pub someboolarray: [bool; 8],",                                          // fixed bool array
		"someuintarray: [0, 1, 1000, 4294967295],",                               // default is an N-element array literal
		"someboolarray: [true, true, false, false, false, false, false, false],", // short default tail-padded to N
		"if self.someuintarray != [0, 1, 1000, 4294967295] {",                    // omit-guard is a default compare
		"self.m.someuintarray[self.ai] = value as u32; self.ai += 1;",            // indexed decode store
		"ai: usize", // fill index on the visitor
		"if offset == 0 && chunk.len() >= total {",                                        // string/blob single-shot fast path
		"core::str::from_utf8(&chunk[..total]).map(|s| s.to_owned()).unwrap_or_default()", // invalid UTF-8 -> empty, agrees with no_std (generator#80)
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs (rs) missing %q", want)
		}
	}
	// String/blob arrays and array-of-array stay heap Vec (not fixed).
	// from_utf8_lossy (U+FFFD) would diverge from no_std's empty-on-invalid (generator#80).
	for _, notWant := range []string{
		"pub someuintarray: Vec<u32>",
		"someuintarray.push(",
		"String::from_utf8_lossy",
	} {
		if strings.Contains(m, notWant) {
			t.Errorf("message.rs (rs) should not contain %q (native fixed array must not be Vec/push)", notWant)
		}
	}
	if strings.Contains(m, "require!") {
		t.Error("std corelib-rs must not emit a require! capability guard")
	}

	// corelib-rs-no-std: require! guard asserting the example's capabilities.
	// allow_dynamic keeps a heap fallback for the example's unbounded `somemap`.
	n := exampleModule(t, map[string]any{"corelib": "rs-no-std", "allow_dynamic": true})
	if !strings.Contains(n, "sofab::require!(") {
		t.Error("rs-no-std must emit a require! capability guard")
	}
	for _, cap := range []string{"fixlen", "sequence", "value64", "array"} {
		if !strings.Contains(n, cap) {
			t.Errorf("expected require!(... %s ...)", cap)
		}
	}
	// The no_std profile lowers bounded fields to fixed-capacity heapless storage
	// (serde gated behind a feature), and keeps an alloc fallback for unbounded ones.
	for _, want := range []string{
		"#[cfg(feature = \"serde\")]",                                      // serde import gated
		"#[cfg_attr(feature = \"serde\", derive(Serialize, Deserialize))]", // serde derive gated
		"pub somestring: heapless::String<50>,",                            // bounded string -> heapless
		"pub someblob: heapless::Vec<u8, 16>,",                             // bounded blob -> heapless
		"pub somestringarray: heapless::Vec<heapless::String<16>, 5>,",     // string array -> inline
		"pub somemap: alloc::vec::Vec<",                                    // unbounded -> alloc fallback
		"pub fn encode(&self) -> heapless::Vec<u8,",                        // heap-free encode
		"stack: heapless::Vec<_Loc,",                                       // bounded decode stack
		"if self.somestring.as_str() != \"\" {",                            // string omit via as_str
	} {
		if !strings.Contains(n, want) {
			t.Errorf("no_std message.rs missing %q", want)
		}
	}
	// No heap String/Vec, no serde-always-derive under no_std.
	for _, notWant := range []string{
		"pub somestring: String,",
		"#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]",
		"String::from_utf8_lossy",
	} {
		if strings.Contains(n, notWant) {
			t.Errorf("no_std message.rs should not contain %q", notWant)
		}
	}

	// no_std: an unbounded field without allow_dynamic is a hard error.
	if _, err := (&Backend{}).Generate(exampleSchema(t), map[string]any{"corelib": "rs-no-std"}); err == nil {
		t.Error("expected unbounded-field error under no_std without allow_dynamic")
	} else if !strings.Contains(err.Error(), "somemap") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRustDeterministic(t *testing.T) {
	if exampleModule(t, map[string]any{}) != exampleModule(t, map[string]any{}) {
		t.Fatal("Rust generation not deterministic")
	}
}
