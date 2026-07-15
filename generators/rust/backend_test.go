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
		"pub fn try_decode(data: &[u8]) -> Result<Self, sofab::Error>", // fallible entry point (generator#79)
		"is.feed(data, &mut v)?;",                                      // fallible decode propagates feed's Result
		"if overflow { return Err(sofab::Error::BufferFull); }",        // fixed-capacity overflow surfaced (generator#82)
		"if invalid { return Err(sofab::Error::InvalidMsg); }",         // over-count array rejected as INVALID (generator#100)
		"err: bool,",                           // sticky overflow flag on the visitor (generator#82)
		"inv: bool,",                           // sticky malformed-message flag (generator#100)
		"mod myfirstmessage_dec {",             // isolated decode module
		"fn sequence_begin(&mut self, id: Id)", // flat-visitor nesting
		"ArrayKind",                            // example has arrays -> array_begin imports it
		"pub someu64: u64,",
		"#[serde(default)]",
		"pub someuintarray: [u32; 4],",             // fixed native array (was Vec<u32>)
		"pub somefloatarray: [f32; 3],",            // fixed fp array
		"pub someboolarray: [bool; 8],",            // fixed bool array
		"someuintarray: [0, 1, 1000, 4294967295],", // default is an N-element array literal
		"someboolarray: [true, true, false, false, false, false, false, false],",                                   // short default tail-padded to N
		"if self.someuintarray != [0, 1, 1000, 4294967295] {",                                                      // omit-guard is a default compare
		"if self.ai < 4 { self.m.someuintarray[self.ai] = value as u32; self.ai += 1; } else { self.inv = true; }", // bounds-checked store (generator#78); over-count rejects (generator#100)
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
		"let _ = self.acc.extend_from_slice(chunk);",                       // accumulates a chunked string/blob (generator#81)
		"if offset == 0 && chunk.len() >= total {",                         // single-shot fast path, now in no_std too
		"self.err = true;",                                                 // fixed-capacity overflow flagged in the fill (generator#82)
	} {
		if !strings.Contains(n, want) {
			t.Errorf("no_std message.rs missing %q", want)
		}
	}
	// No heap String/Vec, no serde-always-derive under no_std; the string/blob
	// visitor must no longer bail on a non-initial chunk (generator#81).
	for _, notWant := range []string{
		"pub somestring: String,",
		"#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]",
		"String::from_utf8_lossy",
		"if offset != 0 || chunk.len() < total { return; }",
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

// TestRustDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated module — constants plus per-field
// guards on schema-unbounded fields only (an unbounded array's wire count is
// checked in array_begin, an unbounded string/blob's declared total at the top
// of its callback, all before any accumulation). A bounded field gets no limit
// guard: it is governed by its own schema bound (+ the generator#100 guard).
// try_decode surfaces the sticky lim flag as Error::LimitExceeded, after
// InvalidMsg and before BufferFull. Unset keys emit nothing; the keys are inert
// for corelib-rs-no-std (statically bounded, no LimitExceeded in that corelib).
func TestRustDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 3 } }
      b:    { id: 3, type: blob }
      sa:   { id: 4, type: array, items: { type: string } }
      mat:  { id: 5, type: array, items: { type: array, items: { type: u32 } } }
`
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
	gen := func(cfg map[string]any) string {
		t.Helper()
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
	limitsCfg := map[string]any{
		"max_dyn_array_count": 4,
		"max_dyn_string_len":  16,
		"max_dyn_blob_len":    8,
	}

	m := gen(limitsCfg)
	for _, want := range []string{
		// Constants baked as configured (no raise: guards are per-field).
		"const MAX_DYN_ARRAY_COUNT: usize = 4;",
		"const MAX_DYN_STRING_LEN: usize = 16;",
		"const MAX_DYN_BLOB_LEN: usize = 8;",
		// Sticky flag on the visitor, sibling of inv.
		"lim: bool,",
		// Unbounded array: count checked in array_begin before any elements land,
		// and the element store is dropped once the flag is set.
		"(_Loc::Root, 1) => { if _count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } self.m.arr.clear() },",
		"(_Loc::Root, 1) => { if !self.lim { self.m.arr.push(value as u64); } },",
		// Unbounded nested native inner array: same guard on its array_begin arm
		// (the inner-Vec push is skipped, so the store must be lim-gated too).
		"(_Loc::Root_mat, _) => { if _count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } self.m.mat.push(Vec::new()) },",
		"(_Loc::Root_mat, _) => { if !self.lim { self.m.mat.last_mut().unwrap().push(value as u32); } },",
		// Unbounded string/blob: declared total checked at the top of the callback,
		// scalar fields and wrapper-sequence string elements alike.
		"(_Loc::Root, 0) => if total > MAX_DYN_STRING_LEN { self.lim = true; return; },",
		"(_Loc::Root_sa, _) => if total > MAX_DYN_STRING_LEN { self.lim = true; return; },",
		"(_Loc::Root, 3) => if total > MAX_DYN_BLOB_LEN { self.lim = true; return; },",
		// try_decode surfaces the flag as LimitExceeded.
		"if limited { return Err(sofab::Error::LimitExceeded); }",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs (limits) missing %q", want)
		}
	}
	// Precedence order in try_decode: inv first, then lim, then err.
	invIdx := strings.Index(m, "if invalid { return Err(sofab::Error::InvalidMsg); }")
	limIdx := strings.Index(m, "if limited { return Err(sofab::Error::LimitExceeded); }")
	errIdx := strings.Index(m, "if overflow { return Err(sofab::Error::BufferFull); }")
	if invIdx < 0 || limIdx < 0 || errIdx < 0 || !(invIdx < limIdx && limIdx < errIdx) {
		t.Errorf("try_decode checks out of order: inv=%d lim=%d err=%d (want inv < lim < err)", invIdx, limIdx, errIdx)
	}
	// The BOUNDED array (barr, id 2, fixed [i32; 3]) must NOT get a limit guard:
	// its schema count governs it (generator#100 over-count guard).
	if strings.Contains(m, "(_Loc::Root, 2) => { if _count > MAX_DYN_ARRAY_COUNT") {
		t.Error("bounded array barr must not get a limit guard")
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := gen(map[string]any{})
	for _, notWant := range []string{"MAX_DYN_", "lim:", "LimitExceeded", "limited"} {
		if strings.Contains(plain, notWant) {
			t.Errorf("unset limits must emit no limit plumbing, found %q", notWant)
		}
	}

	// corelib-rs-no-std: the keys are inert (statically bounded storage, no
	// Error::LimitExceeded in that corelib) — no constants, no guards.
	noStdCfg := map[string]any{"corelib": "rs-no-std", "allow_dynamic": true}
	for k, v := range limitsCfg {
		noStdCfg[k] = v
	}
	n := gen(noStdCfg)
	for _, notWant := range []string{"MAX_DYN_", "LimitExceeded"} {
		if strings.Contains(n, notWant) {
			t.Errorf("rs-no-std must ignore max_dyn_* keys, found %q", notWant)
		}
	}
}

// TestRustMetadataComments checks that message-definition metadata is rendered
// into the generated source: enum-constant descriptions and bitfield-flag
// descriptions (plus a `(default: true/false)` note when the flag has a schema
// default) as rustdoc `///` lines, and a deprecated field carrying both the
// native `#[deprecated]` attribute and a `**Deprecated.**` doc note, with
// `#[allow(deprecated)]` over the impl blocks that read it so the crate stays
// warning-clean.
func TestRustMetadataComments(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode:
      Off:    { value: 0, description: "Node is powered down." }
      Active: { value: 1, description: "Node is sampling and transmitting." }
  bitfield:
    StatusFlags:
      ready:      { pos: 0, default: true, description: "Node has completed initialization." }
      overheated: { pos: 1, description: "Core temperature exceeded the safe threshold." }
messages:
  Telemetry:
    payload:
      legacyId: { id: 0, type: u32, description: "Old identifier.", deprecated: true }
      mode:     { id: 1, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 2, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" } }
`
	doc, err := parser.Parse([]byte(src), "meta.yaml")
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
	gen := func(cfg map[string]any) string {
		t.Helper()
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

	// Both profiles must render the metadata identically.
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std", "allow_dynamic": true}} {
		m := gen(cfg)
		for _, want := range []string{
			// Enum-constant descriptions.
			"    /// Node is powered down.\n    pub const OFF: i8 = 0;",
			"    /// Node is sampling and transmitting.\n    pub const ACTIVE: i8 = 1;",
			// Bitfield-flag description + default note (and no note when no default).
			"    /// Node has completed initialization. (default: true)\n    pub const READY: u8 = 1 << 0;",
			"    /// Core temperature exceeded the safe threshold.\n    pub const OVERHEATED: u8 = 1 << 1;",
			// Deprecated field: doc note + native attribute on the field.
			"    /// Old identifier.\n    ///\n    /// **Deprecated.**\n    #[deprecated]\n    pub legacyId: u32,",
			// Warning suppression over the impl blocks that read the field.
			"#[allow(deprecated)]\nimpl Default for Telemetry {",
			"#[allow(deprecated)]\nimpl Telemetry {",
			"#[allow(deprecated)]\nimpl<'a> Visitor for V<'a> {",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q", cfg, want)
			}
		}
		// A default-less flag must not gain a (default: ...) note.
		if strings.Contains(m, "Core temperature exceeded the safe threshold. (default") {
			t.Errorf("message.rs (%v): flag with no schema default must not carry a default note", cfg)
		}
	}
}

func TestRustDeterministic(t *testing.T) {
	if exampleModule(t, map[string]any{}) != exampleModule(t, map[string]any{}) {
		t.Fatal("Rust generation not deterministic")
	}
}
