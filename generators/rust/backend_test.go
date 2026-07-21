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
		"pub someuintarray: [u32; 4],",             // fixed native array (was Vec<u32>)
		"pub somefloatarray: [f32; 3],",            // fixed fp array
		"pub someboolarray: [bool; 8],",            // fixed bool array
		"someuintarray: [0, 1, 1000, 4294967295],", // default is an N-element array literal
		"someboolarray: [true, true, false, false, false, false, false, false],",                                   // short default tail-padded to N
		"if self.someuintarray != [0, 1, 1000, 4294967295] {",                                                      // omit-guard is a default compare
		"if self.ai < 4 { self.m.someuintarray[self.ai] = value as u32; self.ai += 1; } else { self.inv = true; }", // bounds-checked store (generator#78); over-count rejects (generator#100)
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
		"#[cfg(feature = \"serde\")]",                                                                       // serde import gated
		"#[cfg_attr(feature = \"serde\", derive(Serialize, Deserialize))]",                                  // serde derive gated
		"pub somestring: heapless::String<50>,",                                                             // bounded string -> heapless
		"pub someblob: heapless::Vec<u8, 16>,",                                                              // bounded blob -> heapless
		"pub somestringarray: heapless::Vec<heapless::String<16>, 5>,",                                      // string array -> inline
		"pub somemap: alloc::vec::Vec<",                                                                     // unbounded -> alloc fallback
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
// index, and a struct-element over-index remains a separate axis (still dropped
// on no_std, tracked apart from F-0013).
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
		"if id as usize >= 4 { self.inv = true; return; } while self.m.bs.len()",       // bounded string
		"if id as usize >= 3 { self.inv = true; return; } while self.m.bb.len()",       // bounded blob
		"self.m.bp.push(Default::default()); if id as usize >= 2 { self.inv = true; }", // bounded struct
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
	mn := moduleFromYAML(t, src, map[string]any{"corelib": "rs-no-std", "allow_dynamic": true})
	for _, want := range []string{
		"if id as usize >= 4 { self.inv = true; return; } while self.m.bs.len()", // bounded string
		"if id as usize >= 3 { self.inv = true; return; } while self.m.bb.len()", // bounded blob
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
	for _, cfg := range []map[string]any{
		{}, // std
		{"corelib": "rs-no-std", "no_std": true, "allow_dynamic": true}, // no_std must also reject as INVALID
	} {
		m := moduleFromYAML(t, src, cfg)
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
	for _, cfg := range []map[string]any{
		{}, // std corelib-rs
		{"corelib": "rs-no-std", "allow_dynamic": true}, // #![no_std] + heapless
		{"corelib": "rs-no-std", "no_std": false},       // no-std corelib, std crate
	} {
		m := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			// Fixed-count native arrays are trimmed, per element family.
			"os.write_array_unsigned(0, _trim_tail(&self.fixedu[..], 0))",
			"os.write_array_signed(1, _trim_tail(&self.fixedi[..], 0))",
			"os.write_array_fp32(2, _trim_tail_f32(&self.fixedf32[..]))",
			"os.write_array_fp64(3, _trim_tail_f64(&self.fixedf64[..]))",
			// bool trims its 0/1 u8 image (false <-> 0).
			"os.write_array_unsigned(4, _trim_tail(&_t0[..], 0))",
			// Dynamic arrays keep every element.
			"os.write_array_unsigned(5, &self.dynu)",
			"os.write_array_fp32(6, &self.dynf32)",
			// Floats compare by bit pattern so a trailing -0.0 is not trimmed.
			"while n > 0 && f32::to_bits(a[n - 1]) == 0 { n -= 1; }",
			"while n > 0 && f64::to_bits(a[n - 1]) == 0 { n -= 1; }",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q", cfg, want)
			}
		}
		// The helpers borrow rather than allocate and touch no std/alloc path, so
		// the same text serves the #![no_std] crate.
		for _, want := range []string{
			"fn _trim_tail<T: PartialEq + Copy>(a: &[T], zero: T) -> &[T] {\n    let mut n = a.len();\n    while n > 0 && a[n - 1] == zero { n -= 1; }\n    &a[..n]\n}",
			"fn _trim_tail_f32(a: &[f32]) -> &[f32] {",
			"fn _trim_tail_f64(a: &[f64]) -> &[f64] {",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing helper %q", cfg, want)
			}
		}
		for _, bad := range []string{"_trim_tail(&self.dynu", "_trim_tail_f32(&self.dynf32"} {
			if strings.Contains(m, bad) {
				t.Errorf("message.rs (%v) must not contain %q", cfg, bad)
			}
		}
	}
}

// The trim helpers are emitted only for the element families the schema uses,
// and not at all for a schema with no fixed-count native array.
func TestRustTrimHelpersGatedOnUse(t *testing.T) {
	const noFixed = `
version: 1
messages:
  m:
    payload:
      dynu: { id: 0, type: array, items: { type: u32 } }
`
	if m := moduleFromYAML(t, noFixed, map[string]any{}); strings.Contains(m, "_trim_tail") {
		t.Error("no fixed-count array: trim helpers must not be emitted")
	}
	const onlyU = `
version: 1
messages:
  m:
    payload:
      fixedu: { id: 0, type: array, items: { type: u32, count: 4 } }
`
	m := moduleFromYAML(t, onlyU, map[string]any{})
	if !strings.Contains(m, "fn _trim_tail<T: PartialEq + Copy>") {
		t.Error("integer fixed-count array: _trim_tail must be emitted")
	}
	for _, bad := range []string{"fn _trim_tail_f32", "fn _trim_tail_f64"} {
		if strings.Contains(m, bad) {
			t.Errorf("no float fixed-count array: %q must not be emitted", bad)
		}
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
		"short: [1, 2, 0, 0, 0],",
		"none: [0; 3],",
		"fullf: [1.5, 0.0],",
		"boolp: [true, false, false],",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs missing %q", want)
		}
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
		// Non-zero defaults reset to the element-default image on array_begin.
		for _, want := range []string{
			"(_Loc::Root, 0) => self.m.defd = [0; 5],",
			"(_Loc::Root, 3) => self.m.fdef = [0.0; 3],",
			"(_Loc::Root, 4) => self.m.bdef = [false; 3],",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing reset %q", cfg, want)
			}
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

// TestRustArrayAtScalarIdSkips: an integer ARRAY header delivered to a
// SCALAR-declared field id is a wire-type contradiction and must be skipped like
// an unknown id (MESSAGE_SPEC §7.3, issue #183). corelib-rs streams array
// elements through the very unsigned()/signed() callbacks a lone scalar uses, so
// the id dispatch alone cannot tell them apart; array_begin arms `askip` with the
// announced count and the scalar callbacks discard exactly that many. A
// legitimately declared integer array disarms it, an fp array never arms it (its
// elements go to fp32/fp64), and a schema with no integer array at all still
// emits the guard so a stray array header is skipped.
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
			"if self.askip > 0 { self.askip -= 1; return; }", // consumed by unsigned() and signed()
			"self.askip = match kind {",
			"ArrayKind::Unsigned | ArrayKind::Signed => match (self.cur, id) {",
			"(_Loc::Root, 2) => 0,", // declared u32 array: elements store normally
			"(_Loc::Root, 3) => 0,", // declared i32 array: likewise
			"_ => count,",           // every other id (scalar or unknown) discards
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing §7.3 array-at-scalar guard %q:\n%s", cfg, want, m)
			}
		}
		// The fp array is delivered via fp32/fp64, never a scalar callback, so it
		// must NOT be listed as a disarming arm (arming it would be dead weight).
		if strings.Contains(m, "(_Loc::Root, 4) => 0,") {
			t.Errorf("message.rs (%v) must not disarm the §7.3 guard for an fp array", cfg)
		}
		// Both scalar callbacks carry the guard, not just one.
		if n := strings.Count(m, "if self.askip > 0 { self.askip -= 1; return; }"); n != 2 {
			t.Errorf("message.rs (%v): want the §7.3 guard in both unsigned() and signed(), got %d", cfg, n)
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

	// no_std without the `array` Cargo feature: that corelib cannot decode an
	// array wire type at all, so no element can reach a scalar callback and the
	// guard (which would reference the feature-gated ArrayKind) must be absent.
	nostdScalar := moduleFromYAML(t, `
version: 1
messages:
  m: { payload: { u: { id: 0, type: u8 } } }
`, map[string]any{"corelib": "rs-no-std"})
	for _, bad := range []string{"askip", "ArrayKind", "array_begin"} {
		if strings.Contains(nostdScalar, bad) {
			t.Errorf("no_std scalar-only message.rs must not reference %q (the `array` feature is off):\n%s", bad, nostdScalar)
		}
	}
}
