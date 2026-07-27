package rust

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleSchema(t *testing.T) *ir.Schema { return exampleSchemaOpt(t, false) }

// exampleSchemaBounded is the same example with a capacity on the deliberately
// count-less `somemap`, which the no_std profile requires in both storage modes.
// `count` never reaches the wire, so nothing about the encoding changes — the
// same adjustment tests/conformance/c/run.sh makes for the C target.
func exampleSchemaBounded(t *testing.T) *ir.Schema { return exampleSchemaOpt(t, true) }

func exampleSchemaOpt(t *testing.T, bound bool) *ir.Schema {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if bound {
		b = []byte(strings.Replace(string(b),
			"      somemap:", "      somemap:\n        # bounded for the no_std profile (count never reaches the wire)", 1))
		b = []byte(strings.Replace(string(b),
			"            value:\n              id: 1\n              type: u32",
			"            value:\n              id: 1\n              type: u32\n          count: 8", 1))
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
	if cfg["corelib"] == "rs-no-std" {
		s = exampleSchemaBounded(t)
	}
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
		"pub fn serialize<_F: sofab::Flush>(&self, os: &mut OStream<'_, _F>)",
		"pub fn encode(&self) -> Vec<u8>",
		"pub fn decode(data: &[u8]) -> Self",
		"pub fn try_decode(data: &[u8]) -> Result<Self, sofab::Error>", // fallible entry point (generator#79)
		"fed = is.feed(data, &mut v);",                                 // feed's verdict captured, not propagated (generator#190)
		"if invalid { return Err(sofab::Error::InvalidMsg); }",         // INVALID checked BEFORE feed's error (§5.2, generator#190)
		"fed?;", // then surface a clean Incomplete / structural InvalidMsg
		"if overflow { return Err(sofab::Error::BufferFull); }", // fixed-capacity overflow surfaced (generator#82)
		"err: bool,",                           // sticky overflow flag on the visitor (generator#82)
		"inv: bool,",                           // sticky malformed-message flag (generator#100)
		"mod myfirstmessage_dec {",             // isolated decode module
		"fn sequence_begin(&mut self, id: Id)", // flat-visitor nesting
		"ArrayKind",                            // example has arrays -> array_begin imports it
		"pub someu64: u64,",
		"#[serde(default)]",
		"pub someuintarray: Vec<u32>,",                 // bounded native array -> the profile's dynamic container
		"pub somefloatarray: Vec<f32>,",                // bounded fp array
		"pub someboolarray: Vec<bool>,",                // bounded bool array
		"someuintarray: vec![0, 1, 1000, 4294967295],", // default is an N-element array literal
		"someboolarray: vec![true, true, false, false, false, false, false, false],", // default is exactly N                                   // short default tail-padded to N
		"if &self.someuintarray[..] != &[0, 1, 1000, 4294967295][..] {",              // omit-guard is a default compare
		"if count > 4 { self.inv = true; return; } self.m.someuintarray.clear();",    // bounds-checked store (generator#78); over-count rejects (generator#100)
		"ai: usize", // fill index on the visitor
		"if offset == 0 && chunk.len() >= total {", // string/blob single-shot fast path
		"match core::str::from_utf8(&chunk[..total]) { Ok(_v) => _v.to_owned(), Err(_) => { self.inv = true; String::new() } }", // strict UTF-8: invalid -> INVALID (issue #85, subsumes #80)
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs (rs) missing %q", want)
		}
	}
	// String/blob arrays and array-of-array stay heap Vec (not fixed).
	// Lossy from_utf8_lossy (U+FFFD) is forbidden in every mode (MESSAGE_SPEC §8);
	// strict from_utf8 -> INVALID makes std and no_std agree (issue #85, subsumes #80).
	for _, notWant := range []string{
		"String::from_utf8_lossy",
	} {
		if strings.Contains(m, notWant) {
			t.Errorf("message.rs (rs) should not contain %q ", notWant)
		}
	}
	if strings.Contains(m, "require!") {
		t.Error("std corelib-rs must not emit a require! capability guard")
	}

	// corelib-rs-no-std: require! guard asserting the example's capabilities.
	// Default storage is heapless; the example is taken bounded, since this
	// profile requires a bound whatever storage it uses.
	n := exampleModule(t, map[string]any{"corelib": "rs-no-std"})
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
		"#[cfg(feature = \"serde\")]",                                                                       // serde import gated
		"#[cfg_attr(feature = \"serde\", derive(Serialize, Deserialize))]",                                  // serde derive gated
		"pub somestring: heapless::String<50>,",                                                             // bounded string -> heapless
		"pub someblob: heapless::Vec<u8, 16>,",                                                              // bounded blob -> heapless
		"pub somestringarray: heapless::Vec<heapless::String<16>, 5>,",                                      // string array -> inline
		"pub somemap: heapless::Vec<",                                                                       // bounded -> heapless (default no_std storage)
		"pub fn encode(&self) -> heapless::Vec<u8,",                                                         // heap-free encode
		"stack: heapless::Vec<_Loc,",                                                                        // bounded decode stack
		"if self.somestring.as_str() != \"\" {",                                                             // string omit via as_str
		"let _ = self.acc.extend_from_slice(chunk);",                                                        // accumulates a chunked string/blob (generator#81)
		"if offset == 0 && chunk.len() >= total {",                                                          // single-shot fast path, now in no_std too
		"match core::str::from_utf8(&chunk[..total]) { Ok(_v) => _v, Err(_) => { self.inv = true; \"\" } }", // strict UTF-8 -> INVALID, agrees with std (issue #85)
		"self.err = true;",                                                                                  // fixed-capacity overflow flagged in the fill (generator#82)
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
		"(_Loc::Root, 1) => { if count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } self.m.arr.clear() },",
		"(_Loc::Root, 1) => { if self.afill == 0 { return; } self.afill -= 1; { if !self.lim { self.m.arr.push(value as u64); } }; },",
		// Unbounded nested native inner array: same guard on its array_begin arm
		// (the inner-Vec push is skipped, so the store must be lim-gated too).
		"(_Loc::Root_mat, _) => { if count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } self.m.mat.push(Vec::new()) },",
		"(_Loc::Root_mat, _) => { if self.afill == 0 { return; } self.afill -= 1; { if !self.lim { self.m.mat.last_mut().unwrap().push(value as u32); } }; },",
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
	if strings.Contains(m, "(_Loc::Root, 2) => { if count > MAX_DYN_ARRAY_COUNT") {
		t.Error("bounded array barr must not get a limit guard")
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := gen(map[string]any{})
	for _, notWant := range []string{"MAX_DYN_", "lim:", "LimitExceeded", "limited"} {
		if strings.Contains(plain, notWant) {
			t.Errorf("unset limits must emit no limit plumbing, found %q", notWant)
		}
	}

	// corelib-rs-no-std: the keys are inert there (every field is schema-bounded,
	// and that corelib has no Error::LimitExceeded) — no constants, no guards.
	// The schema is the bounded one, since no_std requires a bound in both storage
	// modes; the caps being inert is exactly what makes that consistent.
	noStdCfg := map[string]any{"corelib": "rs-no-std", "allow_dynamic": true}
	for k, v := range limitsCfg {
		noStdCfg[k] = v
	}
	boundedSrc := `version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string, maxlen: 16 }
      arr:  { id: 1, type: array, items: { type: u64, count: 4 } }
      barr: { id: 2, type: array, items: { type: i32, count: 3 } }
      b:    { id: 3, type: blob, maxlen: 8 }
      sa:   { id: 4, type: array, items: { type: string, count: 4, maxlen: 16 } }
`
	n := moduleFromYAML(t, boundedSrc, noStdCfg)
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

// moduleFromYAML runs the full parse->validate->IR pipeline over an inline
// schema and returns the generated src/message.rs.
func moduleFromYAML(t *testing.T, src string, cfg map[string]any) string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "inline.yaml")
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

// A `count: N` array is fixed-length: the encoder emits only the elements up to
// the last non-default one and the decoder rebuilds the trailing default run
// from N (MESSAGE_SPEC §3). A dynamic (count-less) array has no N to refill
// from, so its trailing default elements are significant and must survive.
// TestRustOverIndexWrapperArray: on the std profile a fixed-count wrapper array
// (string/blob/struct elements) rejects an element id >= N as INVALID (self.inv,
// surfaced as Error::InvalidMsg) before the Vec grows (issue #142 / MESSAGE_SPEC
// §5.1/§7). A dynamic array keeps every index. On the no_std (heapless) profile a
// string/blob element now rejects the same way — the guard fires ahead of the
// heapless capacity drop, so the verdict matches std instead of silently dropping
// (issue #149 / F-0013 / MESSAGE_SPEC §7.1). A dynamic array still keeps every
// index. Since generator#247 a STRUCT element rejects through the same clause on
// both profiles: it is placed at out[id], so the reject has to run before the
// gap-fill — which converges the last over-index axis and, on no_std, keeps the
// index inside the heapless capacity instead of dropping the element silently.
func TestRustOverIndexWrapperArray(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }
      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }
      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }
      ds: { id: 3, type: array, items: { type: string } }
`
	// std profile: rejects.
	m := moduleFromYAML(t, src, map[string]any{})
	for _, want := range []string{
		"if id as usize >= 4 { self.inv = true; return; } while self.m.bs.len()", // bounded string
		"if id as usize >= 3 { self.inv = true; return; } while self.m.bb.len()", // bounded blob
		"if id as usize >= 2 { self.inv = true; return; } while self.m.bp.len()", // bounded struct
	} {
		if !strings.Contains(m, want) {
			t.Errorf("std message.rs missing over-index guard %q", want)
		}
	}
	// Dynamic string array keeps every index (no guard on the ds arm).
	if strings.Contains(m, "self.m.ds.len() <= id as usize") && strings.Contains(m, "ds.len() <= id as usize { self.m.ds.push(Default::default()); } self.m.ds[id as usize] = _s; }") {
		// ds fill present; ensure it is NOT preceded by an inv guard on the same arm.
		if strings.Contains(m, "self.inv = true; return; } while self.m.ds.len()") {
			t.Errorf("dynamic string array must not carry an over-index guard")
		}
	}
	// no_std profile: a string/blob element rejects an over-index id ahead of the
	// heapless capacity drop, converging with std (issue #149 / F-0013).
	// no_std requires a bound on every array in both storage modes, so its leg
	// drops the deliberately count-less `ds` — a dynamic array is a std-profile
	// shape by construction, and the guard under test here is the bounded one.
	srcNoStd := strings.Replace(src, "      ds: { id: 3, type: array, items: { type: string } }\n", "", 1)
	mn := moduleFromYAML(t, srcNoStd, map[string]any{"corelib": "rs-no-std", "allow_dynamic": true})
	for _, want := range []string{
		"if id as usize >= 4 { self.inv = true; return; } while self.m.bs.len()", // bounded string
		"if id as usize >= 3 { self.inv = true; return; } while self.m.bb.len()", // bounded blob
		"if id as usize >= 2 { self.inv = true; return; } while self.m.bp.len()", // bounded struct (generator#247)
	} {
		if !strings.Contains(mn, want) {
			t.Errorf("no_std message.rs missing over-index guard %q:\n%s", want, mn)
		}
	}
	// Dynamic string array (ds) is the alloc fallback under allow_dynamic (cap -1),
	// so it still carries no over-index guard.
	if strings.Contains(mn, "self.inv = true; return; } while self.m.ds.len()") {
		t.Errorf("no_std dynamic string array must not carry an over-index guard:\n%s", mn)
	}
}

// TestRustMaxlenReject: a bounded string/blob (scalar or wrapper-array element)
// rejects a wire byte length above its schema maxlen as INVALID (self.inv) before
// the read, never truncated (MESSAGE_SPEC §7.1). Emitted on BOTH profiles — on
// no_std the guard supersedes the heapless BufferFull path (outcome is INVALID).
func TestRustMaxlenReject(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 8 }
      b:  { id: 1, type: blob,   maxlen: 8 }
      sa: { id: 2, type: array, items: { type: string, count: 3, maxlen: 5 } }
      ds: { id: 3, type: string }
`
	// no_std requires a maxlen on every string in both storage modes, so its leg
	// drops the deliberately unbounded `ds`; the bounded rejects are the point.
	srcNoStd := strings.Replace(src, "      ds: { id: 3, type: string }\n", "", 1)
	for _, cfg := range []map[string]any{
		{},                                       // std
		{"corelib": "rs-no-std", "no_std": true}, // no_std must also reject as INVALID
	} {
		in := src
		if cfg["corelib"] == "rs-no-std" {
			in = srcNoStd
		}
		m := moduleFromYAML(t, in, cfg)
		for _, want := range []string{
			"(_Loc::Root, 0) => if total > 8 { self.inv = true; return; },",    // scalar string
			"(_Loc::Root, 1) => if total > 8 { self.inv = true; return; },",    // scalar blob
			"(_Loc::Root_sa, _) => if total > 5 { self.inv = true; return; },", // wrapper string element
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing maxlen guard %q", cfg, want)
			}
		}
		// The unbounded string field ds carries no maxlen guard.
		if strings.Contains(m, "(_Loc::Root, 3) => if total >") {
			t.Errorf("(%v) unbounded string must not carry a maxlen guard", cfg)
		}
	}
}

func TestRustTrimsFixedCountArraysOnly(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      fixedu:  { id: 0, type: array, items: { type: u32, count: 5 } }
      fixedi:  { id: 1, type: array, items: { type: i16, count: 4 } }
      fixedf32: { id: 2, type: array, items: { type: fp32, count: 3 } }
      fixedf64: { id: 3, type: array, items: { type: fp64, count: 3 } }
      fixedb:  { id: 4, type: array, items: { type: boolean, count: 3 } }
      dynu:    { id: 5, type: array, items: { type: u32 } }
      dynf32:  { id: 6, type: array, items: { type: fp32 } }
`
	// The count-less arrays belong to the std profile only: no_std requires a bound
	// on every array in both storage modes, so its legs take the same schema minus
	// those two fields. What they demonstrate — a dynamic array is never trimmed,
	// having no N to refill from — is a std-profile property by construction.
	srcBounded := strings.Replace(src, "      dynu:    { id: 5, type: array, items: { type: u32 } }\n", "", 1)
	srcBounded = strings.Replace(srcBounded, "      dynf32:  { id: 6, type: array, items: { type: fp32 } }\n", "", 1)

	for _, cfg := range []map[string]any{
		{}, // std corelib-rs
		{"corelib": "rs-no-std", "allow_dynamic": true}, // #![no_std], alloc storage
		{"corelib": "rs-no-std", "no_std": false},       // no-std corelib, std crate
	} {
		in := src
		if cfg["corelib"] == "rs-no-std" && cfg["no_std"] != false {
			in = srcBounded
		}
		m := moduleFromYAML(t, in, cfg)
		if in == src {
			for _, want := range []string{
				// Dynamic arrays keep every element.
				"os.write_array_unsigned(5, &self.dynu)",
				"os.write_array_fp32(6, &self.dynf32)",
			} {
				if !strings.Contains(m, want) {
					t.Errorf("message.rs (%v) missing %q", cfg, want)
				}
			}
		}
		for _, want := range []string{
			// Fixed-count native arrays are trimmed, per element family.
			"os.write_array_unsigned(0, sofab::trim_tail(&self.fixedu[..], 0))",
			"os.write_array_signed(1, sofab::trim_tail(&self.fixedi[..], 0))",
			"os.write_array_fp32(2, sofab::trim_tail_f32(&self.fixedf32[..]))",
			"os.write_array_fp64(3, sofab::trim_tail_f64(&self.fixedf64[..]))",
			// bool trims its 0/1 u8 image (false <-> 0).
			"os.write_array_unsigned(4, sofab::trim_tail(&_t0[..], 0))",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q", cfg, want)
			}
		}
		// The helpers live in the corelib (corelib-rs / corelib-rs-no-std), not in
		// a per-crate prelude: identical text served both profiles, which is what
		// made them corelib material in the first place.
		if strings.Contains(m, "fn _trim_tail") {
			t.Errorf("message.rs (%v) must not carry a trim prelude; the corelib owns it", cfg)
		}
		for _, bad := range []string{"trim_tail(&self.dynu", "trim_tail_f32(&self.dynf32"} {
			if strings.Contains(m, bad) {
				t.Errorf("message.rs (%v) must not contain %q", cfg, bad)
			}
		}
	}
}

// Only a fixed-count array is trimmed. A schema with no fixed-count native
// array must not reach for the corelib trim at all.
func TestRustTrimsOnlyFixedCountArrays(t *testing.T) {
	const noFixed = `
version: 1
messages:
  m:
    payload:
      dynu: { id: 0, type: array, items: { type: u32 } }
`
	if m := moduleFromYAML(t, noFixed, map[string]any{}); strings.Contains(m, "trim_tail") {
		t.Error("no fixed-count array: nothing to trim, so no trim call")
	}
	const onlyU = `
version: 1
messages:
  m:
    payload:
      fixedu: { id: 0, type: array, items: { type: u32, count: 4 } }
`
	m := moduleFromYAML(t, onlyU, map[string]any{})
	if !strings.Contains(m, "sofab::trim_tail(&self.fixedu[..], 0)") {
		t.Error("a fixed-count array must be trimmed via the corelib helper")
	}
}

// A nested array-of-array row has `count:`-shaped storage but is not a
// fixed-length field, so its elements are never trimmed.
func TestRustNestedArrayRowsNotTrimmed(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      grid: { id: 0, type: array, items: { type: array, items: { type: u32, count: 3 } } }
`
	m := moduleFromYAML(t, src, map[string]any{})
	if strings.Contains(m, "_trim_tail") {
		t.Errorf("nested array row must not be trimmed:\n%s", m)
	}
}

// A `count: N` array's Default image must be exactly N elements: a short schema
// default is tail-padded with the element default, and a default-less field is
// the zero repeat literal. (A default longer than N is rejected upstream by
// parser.Validate, so N is always the rendered length.) This is what makes the
// decode-side trailing-default run well defined (MESSAGE_SPEC §3).
func TestRustFixedArrayDefaultIsExactlyN(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      short: { id: 0, type: array, items: { type: u32, count: 5 }, default: [1, 2] }
      none:  { id: 1, type: array, items: { type: u32, count: 3 } }
      fullf: { id: 2, type: array, items: { type: fp32, count: 2 }, default: [1.5] }
      boolp: { id: 3, type: array, items: { type: boolean, count: 3 }, default: [true] }
`
	m := moduleFromYAML(t, src, map[string]any{})
	for _, want := range []string{
		"short: vec![1, 2, 0, 0, 0],",
		"none: vec![0; 3],",
		"fullf: vec![1.5, 0.0],",
		"boolp: vec![true, false, false],",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs missing %q", want)
		}
	}
}

// A `count: N` WRAPPER array's value is N elements long whether or not the field
// ever reaches the wire (MESSAGE_SPEC S5.1: the length "is N for every target"),
// so Default materializes N element defaults exactly like the native array next
// to it. Without this the field disagreed with itself -- the sequence_end refill
// only fires on a sequence that was actually opened, so an absent count:3 string
// array decoded at length 0 while one carrying a single element decoded at 3.
//
// The filled set is exactly the set sequence_end refills: a DYNAMIC array has no
// N and stays empty, and nested-array ROWS are excluded from both (their writer
// emits every row unconditionally, so materialized rows would reach the wire).
func TestRustFixedCountWrapperArrayMaterializesN(t *testing.T) {
	const src = `
version: 1
$defs:
  struct:
    Kv:
      k: { id: 0, type: u32 }
messages:
  m:
    payload:
      strs:    { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }
      blobs:   { id: 1, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      structs: { id: 2, type: array, items: { type: struct, count: 2, fields: { $ref: '#/$defs/struct/Kv' } } }
      rows:    { id: 3, type: array, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }
      nums:    { id: 4, type: array, items: { type: u32, count: 3 } }
`
	// heapless::Vec is capacity-bounded, not pre-sized: its Default is length 0,
	// so the static no_std profile needs the same fill as the std one.
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		ctor string
	}{
		{"std", map[string]any{}, "Vec::new()"},
		{"no-std-static", map[string]any{"corelib": "rs-no-std"}, "heapless::Vec::new()"},
		{"no-std-dynamic", map[string]any{"corelib": "rs-no-std", "allow_dynamic": true}, "alloc::vec::Vec::new()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := moduleFromYAML(t, src, tc.cfg)
			fill := func(field string, n int) string {
				return fmt.Sprintf("%s: { let mut _v = %s; while _v.len() < %d {", field, tc.ctor, n)
			}
			for _, want := range []string{fill("strs", 3), fill("blobs", 2), fill("structs", 2)} {
				if !strings.Contains(m, want) {
					t.Errorf("message.rs missing %q:\n%s", want, m)
				}
			}
			// A nested-array row container is never filled.
			if want := fmt.Sprintf("rows: %s,", tc.ctor); !strings.Contains(m, want) {
				t.Errorf("nested rows must stay empty, missing %q", want)
			}
		})
	}
}

// The count-less half of the same rule: a DYNAMIC wrapper array has no N to
// materialize and must stay empty (std profile only -- no_std requires a bound).
func TestRustDynamicWrapperArrayStaysEmpty(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      dyns: { id: 0, type: array, items: { type: string, maxlen: 8 } }
`
	m := moduleFromYAML(t, src, map[string]any{})
	if !strings.Contains(m, "dyns: Vec::new(),") {
		t.Errorf("a dynamic wrapper array must default to the empty container:\n%s", m)
	}
}

// Decode side of MESSAGE_SPEC S3: the encoder trims the trailing default run, so
// positions [M, N) of a PRESENT fixed-count array are never stored and must read
// back as the ELEMENT default (zero). A non-zero schema `default:` would leak
// through that untouched tail, so array_begin wipes it first. The reset is
// emitted only where it is needed, so every other schema stays byte-identical.
func TestRustFixedArrayResetsNonZeroDefaultOnDecode(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      defd:   { id: 0, type: array, items: { type: u32, count: 5 }, default: [1, 2, 3] }
      zerod:  { id: 1, type: array, items: { type: u32, count: 3 }, default: [0, 0, 0] }
      nodef:  { id: 2, type: array, items: { type: u32, count: 3 } }
      fdef:   { id: 3, type: array, items: { type: fp32, count: 3 }, default: [1.5] }
      bdef:   { id: 4, type: array, items: { type: boolean, count: 3 }, default: [true] }
`
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std"}} {
		m := moduleFromYAML(t, src, cfg)
		// Non-zero defaults reset to the element-default image on array_begin, now
		// behind the over-count guard (generator#216): the count header is rejected
		// as INVALID before the reset, so a truncated over-count array cannot mask
		// the violation as INCOMPLETE (MESSAGE_SPEC S5.2).
		// What follows the guard depends on the storage: an inline [T; N] is wiped
		// to the element-default image, a Vec is cleared and refilled by push.
		resets := []string{
			"(_Loc::Root, 0) => { if count > 5 { self.inv = true; return; } self.m.defd.clear(); self.m.defd.resize(5, 0); },",
			"(_Loc::Root, 3) => { if count > 3 { self.inv = true; return; } self.m.fdef.clear(); self.m.fdef.resize(3, 0.0); },",
			"(_Loc::Root, 4) => { if count > 3 { self.inv = true; return; } self.m.bdef.clear(); self.m.bdef.resize(3, false); },",
		}
		if cfg["corelib"] == "rs-no-std" {
			resets = []string{
				"(_Loc::Root, 0) => { if count > 5 { self.inv = true; return; } self.m.defd = [0; 5]; },",
				"(_Loc::Root, 3) => { if count > 3 { self.inv = true; return; } self.m.fdef = [0.0; 3]; },",
				"(_Loc::Root, 4) => { if count > 3 { self.inv = true; return; } self.m.bdef = [false; 3]; },",
			}
		}
		for _, want := range resets {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing reset %q", cfg, want)
			}
		}
		// The over-count reject is emitted at the count header for EVERY fixed-count
		// array, including one with no reset (generator#216): nodef (count 3, no
		// default) gets a bare guard arm and still no element assignment.
		if !strings.Contains(m, "(_Loc::Root, 2) => { if count > 3 { self.inv = true; return; }") {
			t.Errorf("message.rs (%v) missing over-count guard for nodef (id 2)", cfg)
		}
		// An all-zero or absent default already reads back as zero: no reset, so
		// these schemas' generated code is unchanged.
		for _, bad := range []string{
			"self.m.zerod = [0; 3]",
			"self.m.nodef = [0; 3]",
		} {
			if strings.Contains(m, bad) {
				t.Errorf("message.rs (%v) must not emit a redundant reset %q", cfg, bad)
			}
		}
	}
}

// TestRustArrayAtScalarIdSkips: an ARRAY header delivered to a SCALAR-declared
// field id is a wire-type contradiction and must be skipped like an unknown id
// (MESSAGE_SPEC §7.3, issues #183 for integers and #193 for fp). corelib-rs
// streams array elements through the very unsigned()/signed()/fp32()/fp64()
// callbacks a lone scalar uses, so the id dispatch alone cannot tell them apart;
// array_begin arms `askip` with the announced count and the scalar callbacks
// discard exactly that many. A legitimately declared array of the matching
// element kind disarms it (integer arrays under Unsigned/Signed, fp arrays under
// Fixlen), and a schema with no native array at all still emits the guard so a
// stray array header is skipped.
func TestRustArrayAtScalarIdSkips(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      u:  { id: 0, type: u8 }
      i:  { id: 1, type: i32 }
      ua: { id: 2, type: array, items: { type: u32, count: 4 } }
      ia: { id: 3, type: array, items: { type: i32, count: 4 } }
      fa: { id: 4, type: array, items: { type: fp32, count: 4 } }
`
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std", "allow_dynamic": true}} {
		m := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			"askip: usize,", // the discard counter
			"if self.askip > 0 { self.askip -= 1; return; }", // consumed by unsigned/signed/fp32/fp64
			"self.askip = match kind {",
			"ArrayKind::Unsigned | ArrayKind::Signed => match (self.cur, id) {",
			"(_Loc::Root, 2) => 0,", // declared u32 array: elements store normally
			"(_Loc::Root, 3) => 0,", // declared i32 array: likewise
			"(_Loc::Root, 4) => 0,", // declared fp32 array: disarms under the fp (_) branch (#193)
			"_ => count,",           // every other id (scalar or unknown) discards
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing §7.3 array-at-scalar guard %q:\n%s", cfg, want, m)
			}
		}
		// The guard sits in every callback a scalar shares: unsigned(), signed(),
		// and fp32() (the schema has an fp32 array, so that callback is emitted;
		// there is no fp64, so three occurrences, not four).
		if n := strings.Count(m, "if self.askip > 0 { self.askip -= 1; return; }"); n != 3 {
			t.Errorf("message.rs (%v): want the §7.3 guard in unsigned(), signed() and fp32(), got %d", cfg, n)
		}
	}

	// A schema with no native array at all still needs the guard on the std
	// profile: corelib-rs compiles every wire type in, so an array header can
	// still arrive at a scalar id. array_begin is emitted purely to arm it.
	scalarOnly := moduleFromYAML(t, `
version: 1
messages:
  m: { payload: { u: { id: 0, type: u8 } } }
`, map[string]any{})
	for _, want := range []string{
		"fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize) {",
		"self.askip = match kind {",
	} {
		if !strings.Contains(scalarOnly, want) {
			t.Errorf("scalar-only message.rs missing %q:\n%s", want, scalarOnly)
		}
	}

	// no_std, scalar-only schema: the decoder still needs the guard. §7.3 requires
	// skipping an array wire type that arrives at any id (an unknown id may carry
	// one), so the no_std decoder provisions the full wire-type set including
	// `array` regardless of the schema (generator#215 / Crucible F-0027) — same as
	// std. array_begin is emitted purely to arm the guard.
	nostdScalar := moduleFromYAML(t, `
version: 1
messages:
  m: { payload: { u: { id: 0, type: u8 } } }
`, map[string]any{"corelib": "rs-no-std"})
	for _, want := range []string{
		"fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize) {",
		"self.askip = match kind {",
	} {
		if !strings.Contains(nostdScalar, want) {
			t.Errorf("no_std scalar-only message.rs missing §7.3 array-skip guard %q (generator#215):\n%s", want, nostdScalar)
		}
	}
	// ...and the require!() guard asserts the FULL wire-type set even though the
	// schema declares no array/fp64/64-bit field: the decoder is provisioned to
	// skip any of them (generator#215).
	for _, cap := range []string{"array", "fixlen", "fp64", "sequence", "value64"} {
		if !strings.Contains(nostdScalar, "require!") || !strings.Contains(nostdScalar, cap) {
			t.Errorf("no_std scalar-only require!() must assert full wire-type set incl %q (generator#215):\n%s", cap, nostdScalar)
		}
	}
}

// TestRustIncrementalDecoder pins the public incremental decoder: the corelib's
// IStream is incremental by design, but decode/try_decode own it for the length
// of one call, so without this type the caller must hold the whole message as a
// single contiguous slice — at a transport that means buffering it entirely
// before decoding, which is what streaming exists to avoid.
//
// The decoder owns the message and the visitor's persistent state as plain
// fields, and V borrows them for the duration of one feed, so the type is not
// self-referential and needs no unsafe.
func TestRustIncrementalDecoder(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: string, maxlen: 16 }
      arr: { id: 2, type: array, items: { type: u32, count: 4 } }
`
	for _, cfg := range []map[string]any{
		{},                      // no_std, heapless
		{"allow_dynamic": true}, // no_std, alloc
		{"corelib": "rs"},       // std
	} {
		m := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			// Re-exported under the message's own name; the decoder module stays private.
			"pub use m_dec::Decoder as MDecoder;",
			"pub fn decoder() -> MDecoder {",
			"pub struct Decoder {",
			"pub fn feed(&mut self, chunk: &[u8]) -> Result<(), sofab::Error> {",
			"pub fn finish(mut self) -> Result<M, sofab::Error> {",
			// finish must probe end-of-input, or a stream cut mid-field would be
			// handed back as a half-filled value instead of rejected. The corelib
			// exposes no finalize(); an empty chunk is the documented probe.
			"self.feed(&[])?;",
			// The state the visitor needs across chunks lives in the decoder, not
			// in a borrow: a self-referential struct would need unsafe.
			"let mut v = V { m: &mut self.m,",
			"let r = self.is.feed(chunk, &mut v);",
			"let V {",
			// INVALID dominates a truncated tail, so it is checked before feed's
			// own Incomplete verdict is returned.
			"if self.inv { return Err(sofab::Error::InvalidMsg); }",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q", cfg, want)
			}
		}
	}
}

// A count:N wrapper array's canonical wire stops at M -- one past its last
// non-default element (MESSAGE_SPEC §3/§5.1, "even for sequence-form elements")
// -- and M == 0 leaves the whole wrapper omitted (§2). generator#248: the element
// loop used to run to len(), framing every trailing all-default element, so a
// decoder that accepted the non-canonical form re-encoded it unchanged instead of
// normalising. A DYNAMIC array has no N to refill from, so its trailing default
// element is significant and must still be framed.
func TestRustFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	src := `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`
	// std only: the no_std profile rejects the deliberately count-less `dynamic`.
	got := moduleFromYAML(t, src, map[string]any{"corelib": "rs"})

	// The fixed array narrows to M before framing anything...
	if !strings.Contains(got, "for (_i0, _e0) in _trim_seq(&self.fixed, |_x| _x.is_default()).iter().enumerate() {") {
		t.Errorf("count:N struct array must loop to M, not len:\n%s", got)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(got, "for (_i0, _e0) in self.dynamic.iter().enumerate() {") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", got)
	}
	// An interior all-default element is still framed: only the TRAILING run goes,
	// and M == 0 leaves the wrapper contentless for the DROPPING closer to omit.
	if !strings.Contains(got, "for (_i0, _e0) in _trim_seq(&self.fixed, |_x| _x.is_default()).iter().enumerate() {\n            let _ = os.write_sequence_begin_lazy(_i0 as Id); _e0.serialize(os); let _ = os.write_sequence_end_keep();\n        }\n        let _ = os.write_sequence_end();") {
		t.Errorf("interior framing + dropping field closer expected:\n%s", got)
	}
	// The element predicate is generated from the writer's own guard, negated.
	if !strings.Contains(got, "fn is_default(&self) -> bool {\n        if !(self.k == 0) { return false; }\n        true\n    }") {
		t.Errorf("the element type must carry the writer's negated guard:\n%s", got)
	}
	// A string element is a leaf the writer already omits individually, so
	// narrowing it changes no bytes -- but it must run off the SAME expression, or
	// the predicate and the writer could drift.
	if !strings.Contains(got, "_trim_seq(&self.fstrs, |_x| _x.is_empty())") {
		t.Errorf("count:N string array must be narrowed through the shared helper:\n%s", got)
	}
}

// The narrowing helper and the is_default predicate are emitted only for schemas
// that need them: a footprint build must not carry a predicate no writer calls.
func TestRustTrimHelperIsGatedOnUse(t *testing.T) {
	noArrays := moduleFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: struct, fields: { k: { id: 0, type: u32 } } }
`, map[string]any{"corelib": "rs"})
	if strings.Contains(noArrays, "_trim_seq") || strings.Contains(noArrays, "fn is_default") {
		t.Errorf("a schema with no count:N wrapper array must carry neither:\n%s", noArrays)
	}
	// A DYNAMIC wrapper array is never narrowed, so it needs neither.
	dyn := moduleFromYAML(t, `
version: 1
messages:
  m:
    payload:
      d: { id: 0, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`, map[string]any{"corelib": "rs"})
	if strings.Contains(dyn, "_trim_seq") || strings.Contains(dyn, "fn is_default") {
		t.Errorf("a dynamic wrapper array must not be narrowed:\n%s", dyn)
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at dest[id] after gap-filling -- never appended. Appending
// shortened the array by the size of any interior id gap and decoded a REOPENED
// id as a second element instead of merging into the first (§7.4). The leaf
// string/blob path next to it always got this right.
//
// The N-fill when the sequence scope closes is what makes the §3/§5.1 trailing
// elision lossless: without it, re-encoding a decoded fixed array shortens it on
// every round trip.
func TestRustWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
	src := `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      strs: { id: 1, type: array, items: { type: string, count: 3, maxlen: 8 } }
`
	for _, cfg := range []map[string]any{
		{"corelib": "rs"}, // std
		{},                // no_std, heapless
		{"corelib": "rs-no-std", "allow_dynamic": true}, // no_std, alloc
	} {
		got := moduleFromYAML(t, src, cfg)
		// Placement, not append: gap-fill to id, then descend into out[id]. The
		// over-index reject runs first, so it bounds the fill.
		if !strings.Contains(got, "(_Loc::Root_objs, _) => { if id as usize >= 4 { self.inv = true; return; } while self.m.objs.len() <= id as usize {") {
			t.Errorf("(%v) struct element must gap-fill under the over-index guard:\n%s", cfg, got)
		}
		if !strings.Contains(got, "self._ix0 = id as usize; _Loc::Root_objs_e },") {
			t.Errorf("(%v) struct element must record the element id as its index:\n%s", cfg, got)
		}
		// Every read of the element addresses out[id], not the last element pushed.
		if !strings.Contains(got, "self.m.objs[self._ix0].k") {
			t.Errorf("(%v) element fields must be addressed by index:\n%s", cfg, got)
		}
		// The defect this replaced: append + last_mut() ignored the id entirely.
		if strings.Contains(got, "self.m.objs.last_mut().unwrap()") {
			t.Errorf("(%v) struct elements must not be appended id-blind:\n%s", cfg, got)
		}
		// The index survives between feed calls -- an element can straddle a chunk.
		if !strings.Contains(got, "_ix0: usize,") || !strings.Contains(got, "self._ix0 = _ix0;") {
			t.Errorf("(%v) the element index must be part of the persistent state:\n%s", cfg, got)
		}
		// N-fill when the sequence scope closes, for BOTH element kinds.
		if !strings.Contains(got, "_Loc::Root_objs => { while self.m.objs.len() < 4 {") {
			t.Errorf("(%v) a count:N struct array must be filled to N on sequence_end:\n%s", cfg, got)
		}
		if !strings.Contains(got, "_Loc::Root_strs => { while self.m.strs.len() < 3 {") {
			t.Errorf("(%v) a count:N string array must be filled to N on sequence_end:\n%s", cfg, got)
		}
	}
}

// A dynamic (count-less) wrapper array has no N to refill from: its length is
// highest-present-id + 1, so it must never be default-filled.
func TestRustDynamicWrapperArrayIsNeverFilled(t *testing.T) {
	got := moduleFromYAML(t, `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`, map[string]any{"corelib": "rs"})
	if strings.Contains(got, "_Loc::Root_objs => { while self.m.objs.len() <") {
		t.Errorf("a dynamic wrapper array must not be filled:\n%s", got)
	}
	// It still places by id -- #247 is independent of the count.
	if !strings.Contains(got, "self._ix0 = id as usize; _Loc::Root_objs_e },") {
		t.Errorf("a dynamic wrapper array must still place elements by id:\n%s", got)
	}
}
