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
		"someboolarray: vec![true, true, false],",      // the declared default exactly as written -- `count` never pads it
		"if &self.someuintarray[..] != &[0, 1, 1000, 4294967295][..] {",           // omit-guard is a default compare
		"if count > 4 { self.inv = true; return; } self.m.someuintarray.clear() ", // over-count rejects (generator#100/#216), then the wire's M elements are collected
		"acc: sofab::PayloadAcc,", // the corelib owns chunk reassembly (generator#345)
		"let _p = match self.acc.feed(total, offset, chunk) { Some(_v) => _v, None => return };",                   // ...and generated code only calls it
		"match core::str::from_utf8(_p) { Ok(_v) => _v.to_owned(), Err(_) => { self.inv = true; String::new() } }", // strict UTF-8 on the ASSEMBLED payload: invalid -> INVALID (issue #85, subsumes #80)
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs (rs) missing %q", want)
		}
	}
	// String/blob arrays and array-of-array stay heap Vec (not fixed).
	// Lossy from_utf8_lossy (U+FFFD) is forbidden in every mode (MESSAGE_SPEC §8);
	// strict from_utf8 -> INVALID makes std and no_std agree (issue #85, subsumes #80).
	// The hand-rolled accumulator is gone with it: a helper the corelib now owns
	// must not also be emitted, or the two can drift apart (generator#345).
	for _, notWant := range []string{
		"String::from_utf8_lossy",
		"self.acc.extend_from_slice(chunk)",
		"if offset == 0 && chunk.len() >= total {",
		"if offset == 0 { self.acc.clear(); }",
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
		"#[cfg(feature = \"serde\")]",                                      // serde import gated
		"#[cfg_attr(feature = \"serde\", derive(Serialize, Deserialize))]", // serde derive gated
		"pub somestring: heapless::String<50>,",                            // bounded string -> heapless
		"pub someblob: heapless::Vec<u8, 16>,",                             // bounded blob -> heapless
		"pub somestringarray: heapless::Vec<heapless::String<16>, 5>,",     // string array -> inline
		"pub somemap: heapless::Vec<",                                      // bounded -> heapless (default no_std storage)
		"pub fn encode(&self) -> heapless::Vec<u8,",                        // heap-free encode
		"stack: heapless::Vec<_Loc,",                                       // bounded decode stack
		"if self.somestring.as_str() != \"\" {",                            // string omit via as_str
		"acc: sofab::PayloadAcc<",                                          // the corelib's accumulator, over storage this crate names (generator#345)
		"match self.acc.feed(total, offset, chunk) { Ok(Some(_v)) => _v, Ok(None) => return, Err(_) => { self.err = true; return; } };", // ...whose finite storage adds the BufferFull arm
		"match core::str::from_utf8(_p) { Ok(_v) => _v, Err(_) => { self.inv = true; \"\" } }",                                          // strict UTF-8 -> INVALID, agrees with std (issue #85)
		"self.err = true;", // fixed-capacity overflow flagged in the fill (generator#82)
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
		"self.acc.extend_from_slice(chunk)",
		"if offset == 0 && chunk.len() >= total {",
		"if offset == 0 { self.acc.clear(); }",
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
		"(ArrayKind::Unsigned, _Loc::Root, 1) => { if count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } self.m.arr.clear() },",
		"(_Loc::Root, 1) => { if self.afill == 0 { return; } self.afill -= 1; { if !self.lim { self.m.arr.push(value as u64); } }; },",
		// Unbounded nested native inner array: same guard on its array_begin arm
		// (the inner-Vec push is skipped, so the store must be lim-gated too).
		// A count-less matrix: the ROW's id is the outer array's length, so the cap
		// binds it (generator#387), and the row's own element count is capped
		// beside it -- two bounds, id first, both LimitExceeded.
		"(ArrayKind::Unsigned, _Loc::Root_mat, _) => { if id as usize >= MAX_DYN_ARRAY_COUNT { self.lim = true; self.afill = 0; return; } if count > MAX_DYN_ARRAY_COUNT { self.lim = true; self.afill = 0; return; } while self.m.mat.len() <= id as usize { self.m.mat.push(Default::default()); } self._ix0 = id as usize; },",
		"(_Loc::Root_mat, _) => { if self.afill == 0 { return; } self.afill -= 1; if value > 4294967295 { self.inv = true; return; } { if !self.lim { if let Some(_r) = self.m.mat.get_mut(self._ix0) { _r.push(value as u32); }; } }; },",
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

	// No keys configured -> the target's finite DEFAULTS, not "unlimited"
	// (§9.5, generator#385). Rust std is on the server tier.
	plain := gen(map[string]any{})
	for _, want := range []string{
		"const MAX_DYN_ARRAY_COUNT: usize = 65536;",
		"const MAX_DYN_STRING_LEN: usize = 1048576;",
		"const MAX_DYN_BLOB_LEN: usize = 4194304;",
		"if limited { return Err(sofab::Error::LimitExceeded); }",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("default limits missing %q", want)
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
		// The unbounded string field ds carries no SCHEMA maxlen guard -- what it
		// does carry is the receiver cap, which is a different bound with a
		// different verdict (LimitExceeded, not INVALID).
		if strings.Contains(m, "(_Loc::Root, 3) => if total > 8") {
			t.Errorf("(%v) unbounded string must not carry a maxlen guard", cfg)
		}
		if cfg["corelib"] != "rs-no-std" &&
			!strings.Contains(m, "(_Loc::Root, 3) => if total > MAX_DYN_STRING_LEN { self.lim = true; return; },") {
			t.Errorf("(%v) unbounded string must carry the default receiver cap", cfg)
		}
	}
}

// MESSAGE_SPEC §3 (af536c4): `count: N` is a CAPACITY, so the wire count M IS the
// array's length and nothing that carries a length may be elided. The
// trailing-default-run trim a fixed-count array used to apply is therefore gone,
// for every element family and both storage modes -- the value goes to the wire
// exactly as it stands. The corelib still ships the helpers; the generator simply
// stops calling them.
func TestRustNativeArraysAreNeverTrimmed(t *testing.T) {
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
	// those two fields.
	srcBounded := strings.Replace(src, "      dynu:    { id: 5, type: array, items: { type: u32 } }\n", "", 1)
	srcBounded = strings.Replace(srcBounded, "      dynf32:  { id: 6, type: array, items: { type: fp32 } }\n", "", 1)

	for _, cfg := range []map[string]any{
		{},                       // std corelib-rs
		{"corelib": "rs-no-std"}, // #![no_std], heapless storage
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
				"os.write_array_unsigned(5, &self.dynu)",
				"os.write_array_fp32(6, &self.dynf32)",
			} {
				if !strings.Contains(m, want) {
					t.Errorf("message.rs (%v) missing %q", cfg, want)
				}
			}
		}
		// A `count: N` array writes exactly what it holds, like a count-less one.
		for _, want := range []string{
			"os.write_array_unsigned(0, &self.fixedu)",
			"os.write_array_signed(1, &self.fixedi)",
			"os.write_array_fp32(2, &self.fixedf32)",
			"os.write_array_fp64(3, &self.fixedf64)",
			"os.write_array_unsigned(4, &_t0)", // bool via its 0/1 u8 image
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q", cfg, want)
			}
		}
		// No trim of any kind, from the corelib or a local prelude.
		for _, bad := range []string{"trim_tail", "_trim_seq", "fn is_default"} {
			if strings.Contains(m, bad) {
				t.Errorf("message.rs (%v) must not contain %q -- `count` is a capacity, so nothing is elided", cfg, bad)
			}
		}
	}
}

// A nested array-of-array row is written exactly as it stands too: the only
// thing the interior may drop is a row indistinguishable from absence (the empty
// one), never a trailing default ELEMENT inside a row.
func TestRustNestedArrayRowsNotTrimmed(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      grid: { id: 0, type: array, items: { type: array, items: { type: u32, count: 3 } } }
`
	m := moduleFromYAML(t, src, map[string]any{})
	if strings.Contains(m, "trim_tail") || strings.Contains(m, "_trim_seq") {
		t.Errorf("nested array row must not be trimmed:\n%s", m)
	}
	// The row itself takes the positional rule: an interior EMPTY row is skipped,
	// the last row is written whatever it holds.
	if !strings.Contains(m, "if !_e0.is_empty() || _i0 + 1 == self.grid.len() {") {
		t.Errorf("a native row must take the interior-sparse / last-always rule:\n%s", m)
	}
}

// `count: N` is a CAPACITY, never a length (MESSAGE_SPEC §3), so it materializes
// nothing: a fresh count:N array is EMPTY, and a declared `default` shorter than N
// stands exactly as written rather than being tail-padded out to N. The omit
// guard compares against that same unpadded literal, so a field sitting on its
// default is still omitted.
func TestRustCountDoesNotPadTheDefault(t *testing.T) {
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
	for _, tc := range []struct {
		name  string
		cfg   map[string]any
		wants []string
	}{
		{"std", map[string]any{}, []string{
			"short: vec![1, 2],",
			"none: Vec::new(),",
			"fullf: vec![1.5],",
			"boolp: vec![true],",
		}},
		{"no-std-static", map[string]any{"corelib": "rs-no-std"}, []string{
			"short: { let mut _v = heapless::Vec::new(); let _ = _v.extend_from_slice(&[1, 2]); _v },",
			"none: heapless::Vec::new(),",
			"fullf: { let mut _v = heapless::Vec::new(); let _ = _v.extend_from_slice(&[1.5]); _v },",
			"boolp: { let mut _v = heapless::Vec::new(); let _ = _v.extend_from_slice(&[true]); _v },",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := moduleFromYAML(t, src, tc.cfg)
			for _, want := range tc.wants {
				if !strings.Contains(m, want) {
					t.Errorf("message.rs missing %q:\n%s", want, m)
				}
			}
			// The write guard reads the SAME unpadded literal.
			if !strings.Contains(m, "if &self.short[..] != &[1, 2][..] {") {
				t.Errorf("the omit guard must compare against the unpadded default:\n%s", m)
			}
			// A default-less count:N array is default only when EMPTY: an all-zero
			// N-element value is a length-N array and stays on the wire.
			if !strings.Contains(m, "if !self.none.is_empty() {") {
				t.Errorf("a default-less count:N array must be omitted only when empty:\n%s", m)
			}
		})
	}
}

// The wrapper half of the same rule: a `count: N` wrapper array is constructed
// EMPTY on every profile. `count` sizes the container's capacity (heapless::Vec<_,
// N> under no_std) and bounds the decode, but it never adds elements, so the
// field's initial value is the empty array -- the same value an absent field
// decodes back to, and the same one its dropping closer omits.
func TestRustCountNWrapperArrayStartsEmpty(t *testing.T) {
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
			for _, field := range []string{"strs", "blobs", "structs", "rows", "nums"} {
				want := fmt.Sprintf("%s: %s,", field, tc.ctor)
				if !strings.Contains(m, want) {
					t.Errorf("message.rs missing %q:\n%s", want, m)
				}
			}
			// No fill of any kind, at construction or when the scope closes.
			if strings.Contains(m, "while _v.len() <") {
				t.Errorf("a count:N array must not be pre-filled -- count is a capacity:\n%s", m)
			}
			for _, field := range []string{"strs", "structs"} {
				if strings.Contains(m, fmt.Sprintf("while self.m.%s.len() < ", field)) {
					t.Errorf("a count:N array must not be filled to N on sequence_end:\n%s", m)
				}
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

// Decode side of MESSAGE_SPEC §3: the wire count M IS the array's length, so a
// present native array is CLEARED and then collects exactly the M elements that
// arrived -- no pre-size to N, no wipe of a schema-default tail, no refill of
// [M, N). The over-count reject stays at the count header (generator#216): it must
// be decided before the elements are read, so INVALID dominates a truncated tail
// (§5.2). Both storage modes take the same shape now that a count:N native array
// is a capacity-bounded container rather than an inline [T; N].
func TestRustNativeArrayDecodeClearsAndCollects(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      defd:   { id: 0, type: array, items: { type: u32, count: 5 }, default: [1, 2, 3] }
      nodef:  { id: 2, type: array, items: { type: u32, count: 3 } }
      fdef:   { id: 3, type: array, items: { type: fp32, count: 3 }, default: [1.5] }
`
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std"}} {
		m := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			"(ArrayKind::Unsigned, _Loc::Root, 0) => { if count > 5 { self.inv = true; return; } self.m.defd.clear() },",
			"(ArrayKind::Unsigned, _Loc::Root, 2) => { if count > 3 { self.inv = true; return; } self.m.nodef.clear() },",
			// The fp32 array's arm is keyed to its own subtype, so an fp64 header
			// at id 3 never reaches this bound (generator#259).
			"(ArrayKind::Fp32, _Loc::Root, 3) => { if count > 3 { self.inv = true; return; } self.m.fdef.clear() },",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q:\n%s", cfg, want, m)
			}
		}
		// The elements are collected, not written into a pre-sized tail: no resize,
		// no indexed store, and no fill index on the visitor at all.
		for _, bad := range []string{".resize(", "self.ai", "= [0; 5]"} {
			if strings.Contains(m, bad) {
				t.Errorf("message.rs (%v) must not contain %q -- M is the length:\n%s", cfg, bad, m)
			}
		}
		if !strings.Contains(m, "self.m.defd.push(value as u32)") {
			t.Errorf("message.rs (%v) must collect elements by push:\n%s", cfg, m)
		}
	}
}

// A NESTED ROW's own `count` must bound its elements, exactly like the
// top-level over-count reject above (generator#216 / F-0032).
//
// The defect this pins: array_begin's row arm checked only the ROW id against the
// OUTER count and then opened the row, so the row's inner `count: M` was not a
// decode bound at all. On corelib-rs a `count: 3` row filled to whatever element
// count the header announced (measured: 200_000 elements from a 583 KB message,
// accepted); on rs-no-std the elements past the heapless capacity were dropped
// and the message was ALSO accepted -- the two profiles disagreeing on the same
// bytes, which §7.1 convergence (issue #149 / F-0013) forbids. Both must be
// INVALID, decided at the count header so it dominates a truncated tail (§5.2).
//
// The reject must also DISARM the fill: array_begin arms afill with the announced
// count before the arm runs, so a reject that only returned left the rejected
// row's elements streaming into whatever row the index slot still named (measured:
// an over-index row's 64 elements grew the legal row 0 from 3 to 67).
func TestRustNestedRowInnerCountBoundsItsElements(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      mat:  { id: 0, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      fmat: { id: 1, type: array, items: { type: array, count: 2, items: { type: fp32, count: 4 } } }
`
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std"}} {
		m := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			// row id vs the OUTER count, then element count vs the INNER count,
			// both before the row is opened or grown, both disarming the fill.
			"(ArrayKind::Unsigned, _Loc::Root_mat, _) => { if id as usize >= 2 { self.inv = true; self.afill = 0; return; } if count > 3 { self.inv = true; self.afill = 0; return; } while self.m.mat.len() <= id as usize {",
			"(ArrayKind::Fp32, _Loc::Root_fmat, _) => { if id as usize >= 2 { self.inv = true; self.afill = 0; return; } if count > 4 { self.inv = true; self.afill = 0; return; } while self.m.fmat.len() <= id as usize {",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing %q:\n%s", cfg, want, m)
			}
		}
		// The reject is at the header, not at the element store: the store stays a
		// plain guarded push (a per-element bound would let a truncated over-count
		// row report INCOMPLETE instead of INVALID).
		if !strings.Contains(m, "if let Some(_r) = self.m.mat.get_mut(self._ix0) {") {
			t.Errorf("message.rs (%v) row store must stay unconditional:\n%s", cfg, m)
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
// element kind disarms it (integer arrays under Unsigned/Signed, fp32 arrays
// under Fp32, fp64 arrays under Fp64 -- generator#259), and a schema with no
// native array at all still emits the guard so a stray array header is skipped.
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
			"ArrayKind::Unsigned => match (self.cur, id) {",
			"(_Loc::Root, 2) => 0,",                     // declared u32 array: elements store normally
			"(_Loc::Root, 3) => 0,",                     // declared i32 array: likewise
			"ArrayKind::Fp32 => match (self.cur, id) {", // subtype-keyed fixlen arm (#259)
			"(_Loc::Root, 4) => 0,",                     // declared fp32 array: disarms under Fp32 (#193)
			"_ => count,",                               // every other id (scalar or unknown) discards
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

// TestRustFixlenArrayArmsAreKeyedByElementSubtype: a fixlen array header is
// routed by its ELEMENT SUBTYPE, not by a collapsed "fixlen" category
// (CORELIB_PLAN §4.8, generator#259 / Crucible F-0042).
//
// The corelib now reads count -> fixlen_word -> array_begin, so `kind` names the
// subtype actually on the wire (ArrayKind::Fp32 / ArrayKind::Fp64, ordinals 2 and
// 3). A declared fp32[N] must therefore appear ONLY under the Fp32 arm and a
// declared fp64[N] ONLY under Fp64, in all three of array_begin's matches.
//
// The defect this pins: with one collapsed fp arm, an fp64 header arriving at a
// declared fp32[N] id disarmed the discard counter, cleared the declared
// container and applied that field's schema `count` as a bound — so a header that
// §7.3 says is a SKIPPED field could both wipe the field's value and reject the
// whole message as INVALID on a bound that was never its bound. The count word is
// read before the fixlen_word, so the bound has to sit INSIDE the kind-matched
// arm; ahead of the kind test it decides on evidence the subtype later
// contradicts.
func TestRustFixlenArrayArmsAreKeyedByElementSubtype(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      f32s: { id: 1, type: array, items: { type: fp32, count: 4 } }
      f64s: { id: 2, type: array, items: { type: fp64, count: 6 } }
      ints: { id: 3, type: array, items: { type: u32, count: 8 } }
`
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std"}} {
		m := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			// Skip counter: each fp field disarms only under its own subtype's arm,
			// so the other subtype falls through to `_ => count` and is discarded.
			"ArrayKind::Fp32 => match (self.cur, id) {\n                (_Loc::Root, 1) => 0,\n                _ => count,\n            },",
			"ArrayKind::Fp64 => match (self.cur, id) {\n                (_Loc::Root, 2) => 0,\n                _ => count,\n            },",
			// Fill counter: same keying, so a contradicting header arms nothing.
			"ArrayKind::Fp32 => match (self.cur, id) {\n                (_Loc::Root, 1) => count,\n                _ => 0,\n            },",
			"ArrayKind::Fp64 => match (self.cur, id) {\n                (_Loc::Root, 2) => count,\n                _ => 0,\n            },",
			// Target match: keyed by (kind, loc, id), with the schema `count` bound
			// and the clear both INSIDE the kind-matched arm.
			"match (kind, self.cur, id) {",
			"(ArrayKind::Fp32, _Loc::Root, 1) => { if count > 4 { self.inv = true; return; } self.m.f32s.clear() },",
			"(ArrayKind::Fp64, _Loc::Root, 2) => { if count > 6 { self.inv = true; return; } self.m.f64s.clear() },",
			// Integer arrays are unaffected: no second header word, so no subtype to
			// contradict.
			"(ArrayKind::Unsigned, _Loc::Root, 3) => { if count > 8 { self.inv = true; return; } self.m.ints.clear() },",
		} {
			if !strings.Contains(m, want) {
				t.Errorf("message.rs (%v) missing subtype-keyed fixlen arm %q:\n%s", cfg, want, m)
			}
		}
		// The declared fp32 field must not be reachable from an Fp64 header, and
		// vice versa — the whole point of the split.
		for _, bad := range []string{
			"(ArrayKind::Fp64, _Loc::Root, 1)",
			"(ArrayKind::Fp32, _Loc::Root, 2)",
			"(_, _Loc::Root, 1)",
			"(_, _Loc::Root, 2)",
			"ArrayKind::Fixlen",
		} {
			if strings.Contains(m, bad) {
				t.Errorf("message.rs (%v) must not contain %q -- a fixlen arm is keyed to one subtype:\n%s", cfg, bad, m)
			}
		}
		// Both subtypes named => {Unsigned, Signed, Fp32, Fp64} is exhaustive, so
		// no trailing wildcard (it would be an unreachable pattern warning in the
		// generated crate).
		if strings.Contains(m, "            _ => count,\n        };") {
			t.Errorf("message.rs (%v) must not emit an unreachable catch-all when both fp arms are named:\n%s", cfg, m)
		}
	}

	// A schema that declares only ONE fp subtype must name only that variant and
	// keep the catch-all. Under corelib-rs-no-std both Fp32 and Fp64 are
	// #[cfg(feature = "fixlen")], so naming a variant the crate has no array for
	// is a needless dependency on a feature the schema does not otherwise force;
	// the catch-all arms the discard counter for the absent subtype instead.
	only32 := moduleFromYAML(t, `
version: 1
messages:
  m: { payload: { f: { id: 1, type: array, items: { type: fp32, count: 4 } } } }
`, map[string]any{"corelib": "rs-no-std"})
	if !strings.Contains(only32, "ArrayKind::Fp32 => match (self.cur, id) {") {
		t.Errorf("fp32-only message.rs must name the Fp32 arm:\n%s", only32)
	}
	if strings.Contains(only32, "ArrayKind::Fp64") {
		t.Errorf("fp32-only message.rs must not name Fp64 (feature-gated under no_std):\n%s", only32)
	}
	if !strings.Contains(only32, "            _ => count,\n        };") {
		t.Errorf("fp32-only message.rs must keep the catch-all that discards an fp64 header:\n%s", only32)
	}

	// A schema with NO fp array at all names neither variant: the generated crate
	// must compile under no_std whatever fixlen provisioning it ends up with, and
	// an fp header at any id is discarded through the catch-all.
	noFP := moduleFromYAML(t, `
version: 1
messages:
  m: { payload: { a: { id: 1, type: array, items: { type: u16, count: 4 } } } }
`, map[string]any{"corelib": "rs-no-std"})
	for _, bad := range []string{"ArrayKind::Fp32", "ArrayKind::Fp64"} {
		if strings.Contains(noFP, bad) {
			t.Errorf("fp-free message.rs must not name %q:\n%s", bad, noFP)
		}
	}
	if !strings.Contains(noFP, "            _ => count,\n        };") {
		t.Errorf("fp-free message.rs must discard an fp array header through the catch-all:\n%s", noFP)
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

// MESSAGE_SPEC §2 (af536c4): ONE sparse rule for both element kinds, and it is
// POSITIONAL IN THE VALUE, not static from the schema. An element before the last
// one that equals its element default is omitted and leaves an id GAP -- a
// string/blob leaf is not written, a struct/union/nested element is not framed --
// while the LAST element is always written, as its value or as an empty frame.
// `count: N` changes nothing: it is a capacity, so it can never restore an elided
// tail. Sequence-form elements previously had a carve-out and were framed
// unconditionally, and a count:N array elided its whole trailing run instead.
func TestRustWrapperElementSparsityIsPositional(t *testing.T) {
	src := `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      dstrs:   { id: 3, type: array, items: { type: string, maxlen: 8 } }
      dblobs:  { id: 4, type: array, items: { type: blob, maxlen: 8 } }
`
	// std only: the no_std profile rejects the deliberately count-less fields.
	got := moduleFromYAML(t, src, map[string]any{"corelib": "rs"})

	// Both struct arrays loop over the value itself -- no narrowing, count or not.
	for _, want := range []string{
		"for (_i0, _e0) in self.fixed.iter().enumerate() {",
		"for (_i0, _e0) in self.dynamic.iter().enumerate() {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a wrapper array must loop over its own elements, missing %q:\n%s", want, got)
		}
	}
	// The element's CLOSER is chosen at run time from its index: the keeping one at
	// the last index, the dropping one in the interior (which is what turns an
	// all-default interior element into an id gap). The field's own wrapper still
	// always takes the dropping closer.
	for _, want := range []string{
		"let _ = os.write_sequence_begin_lazy(_i0 as Id); _e0.serialize(os);\n            if _i0 + 1 == self.fixed.len() { let _ = os.write_sequence_end_keep(); } else { let _ = os.write_sequence_end(); }\n        }\n        let _ = os.write_sequence_end();",
		"let _ = os.write_sequence_begin_lazy(_i0 as Id); _e0.serialize(os);\n            if _i0 + 1 == self.dynamic.len() { let _ = os.write_sequence_end_keep(); } else { let _ = os.write_sequence_end(); }\n        }\n        let _ = os.write_sequence_end();",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("positional closer expected:\n%s", got)
		}
	}
	// The leaf elements take the same rule through the same expression, count:N and
	// count-less alike.
	for _, want := range []string{
		"for (_i0, _e0) in self.fstrs.iter().enumerate() { if !_e0.is_empty() || _i0 + 1 == self.fstrs.len() { let _ = os.write_str(_i0 as Id, _e0); } }",
		"for (_i0, _e0) in self.dstrs.iter().enumerate() { if !_e0.is_empty() || _i0 + 1 == self.dstrs.len() { let _ = os.write_str(_i0 as Id, _e0); } }",
		"for (_i0, _e0) in self.dblobs.iter().enumerate() { if !_e0.is_empty() || _i0 + 1 == self.dblobs.len() { let _ = os.write_blob(_i0 as Id, _e0); } }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.rs missing %q:\n%s", want, got)
		}
	}
	// Nothing is narrowed and no all-default predicate survives: the writer emits a
	// child for every element it holds, so "no child written" IS "the array is
	// empty" and the two can no longer drift.
	for _, bad := range []string{"_trim_seq", "fn is_default"} {
		if strings.Contains(got, bad) {
			t.Errorf("message.rs must not contain %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "let _ = os.write_sequence_begin_lazy(0);\n        for (_i0, _e0) in self.fixed") {
		t.Errorf("the field wrapper is still opened lazily:\n%s", got)
	}
}

// generator#247, extended: a wrapper array's element id IS the array index (§5.1),
// so an element is PLACED at dest[id] after gap-filling -- never appended. Under
// the af536c4 rule an interior gap is REACHABLE for every element kind (an
// all-default interior element is omitted), so the matrix-row and wrapper-row
// collectors -- which appended id-blind -- would shift every later row down by
// one. They place by id now, bounded by the outer array's count, which also closes
// the over-index hole they had.
func TestRustWrapperElementsArePlacedByID(t *testing.T) {
	src := `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      strs: { id: 1, type: array, items: { type: string, count: 3, maxlen: 8 } }
      mat:  { id: 2, type: array, items: { type: array, count: 4, items: { type: u32, count: 3 } } }
      rows: { id: 3, type: array, items: { type: array, count: 4, items: { type: string, count: 3, maxlen: 8 } } }
`
	for _, cfg := range []map[string]any{
		{"corelib": "rs"}, // std
		{},                // no_std, heapless
		{"corelib": "rs-no-std", "allow_dynamic": true}, // no_std, alloc
	} {
		got := moduleFromYAML(t, src, cfg)
		// Struct elements: gap-fill to id under the over-index guard, then descend
		// into out[id].
		if !strings.Contains(got, "(_Loc::Root_objs, _) => { if id as usize >= 4 { self.inv = true; return; } while self.m.objs.len() <= id as usize {") {
			t.Errorf("(%v) struct element must gap-fill under the over-index guard:\n%s", cfg, got)
		}
		if !strings.Contains(got, "self._ix0 = id as usize; _Loc::Root_objs_e },") {
			t.Errorf("(%v) struct element must record the element id as its index:\n%s", cfg, got)
		}
		if !strings.Contains(got, "self.m.objs[self._ix0].k") {
			t.Errorf("(%v) element fields must be addressed by index:\n%s", cfg, got)
		}
		// Matrix rows: array_begin opens the row the id names, and elements push into
		// THAT row rather than into the last one appended.
		if !strings.Contains(got, "(ArrayKind::Unsigned, _Loc::Root_mat, _) => { if id as usize >= 4 { self.inv = true; self.afill = 0; return; } if count > 3 { self.inv = true; self.afill = 0; return; } while self.m.mat.len() <= id as usize {") ||
			!strings.Contains(got, "self._ix1 = id as usize; },") {
			t.Errorf("(%v) a matrix row must be opened at out[id], bounded by the outer count:\n%s", cfg, got)
		}
		if !strings.Contains(got, "if let Some(_r) = self.m.mat.get_mut(self._ix1) {") {
			t.Errorf("(%v) matrix elements must land in the row the id named:\n%s", cfg, got)
		}
		// Wrapper rows: same, through the row's own sequence_begin.
		if !strings.Contains(got, "(_Loc::Root_rows, _) => { if id as usize >= 4 { self.inv = true; return; } while self.m.rows.len() <= id as usize {") ||
			!strings.Contains(got, "self._ix2 = id as usize; _Loc::Root_rows_e },") {
			t.Errorf("(%v) a wrapper row must be placed at out[id]:\n%s", cfg, got)
		}
		if !strings.Contains(got, "self.m.rows[self._ix2]") {
			t.Errorf("(%v) wrapper-row elements must address the row by index:\n%s", cfg, got)
		}
		// The defect this replaced: append + last_mut() ignored the id entirely.
		if strings.Contains(got, ".last_mut().unwrap()") {
			t.Errorf("(%v) no collector may append id-blind any more:\n%s", cfg, got)
		}
		// The indices survive between feed calls -- an element can straddle a chunk.
		for _, ix := range []string{"_ix0", "_ix1", "_ix2"} {
			if !strings.Contains(got, ix+": usize,") || !strings.Contains(got, "self."+ix+" = "+ix+";") {
				t.Errorf("(%v) %s must be part of the persistent state:\n%s", cfg, ix, got)
			}
		}
		// No fill-to-N when a scope closes: `count` is a capacity, and the elements
		// the wire carried are the whole value.
		if strings.Contains(got, "while self.m.objs.len() < 4") || strings.Contains(got, "while self.m.strs.len() < 3") {
			t.Errorf("(%v) a count:N wrapper array must not be filled to N:\n%s", cfg, got)
		}
	}
}

// A count-less wrapper array is treated identically -- placement by id was never
// about the count, and there is nothing to fill on either kind.
func TestRustDynamicWrapperArrayIsNeverFilled(t *testing.T) {
	got := moduleFromYAML(t, `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`, map[string]any{"corelib": "rs"})
	if strings.Contains(got, "_Loc::Root_objs => { while self.m.objs.len() <") {
		t.Errorf("a wrapper array must not be filled:\n%s", got)
	}
	if !strings.Contains(got, "self._ix0 = id as usize; _Loc::Root_objs_e },") {
		t.Errorf("a dynamic wrapper array must still place elements by id:\n%s", got)
	}
}

// TestRustSkippedStringIsNotValidated: a `string` payload the visitor will not
// materialize must be skipped whole — its bytes jumped over, never inspected
// (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038). corelib-rs hands EVERY
// fixlen-string field to the generated `string()` callback, unknown ids and
// §7.3 wire-type contradictions included, so the callback itself is what decides
// whether a payload is read. It used to transcode first and dispatch second, so
// a lone continuation byte at an id the scope does not declare set the sticky
// `inv` flag and turned an otherwise valid message into INVALID.
//
// The fix is order: resolve the destination first and return when nothing
// matches, so no byte is buffered into `acc`, transcoded, or checked.
func TestRustSkippedStringIsNotValidated(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 16 }
      n:
        id: 1
        type: struct
        fields:
          t: { id: 2, type: string, maxlen: 8 }
      sa: { id: 3, type: array, items: { type: string, count: 4, maxlen: 8 } }
`
	for _, cfg := range []map[string]any{{}, {"corelib": "rs-no-std"}} {
		m := moduleFromYAML(t, src, cfg)
		fn := sliceFn(t, m, "    fn string(")
		// The destination guard is the FIRST statement of string(), ahead of the
		// maxlen guard and ahead of every from_utf8.
		guard := fn[:strings.Index(fn, "_ => return,")+len("_ => return,")]
		for _, want := range []string{
			"(_Loc::Root, 0) => {},",    // the scalar string
			"(_Loc::Root_n, 2) => {},",  // the nested struct's string
			"(_Loc::Root_sa, _) => {},", // every id of the string-array row
		} {
			if !strings.Contains(guard, want) {
				t.Errorf("string() (%v) missing destination arm %q:\n%s", cfg, want, fn)
			}
		}
		gi, ui := strings.Index(fn, "_ => return,"), strings.Index(fn, "from_utf8")
		if gi < 0 || ui < 0 || gi > ui {
			t.Errorf("string() (%v): the destination guard must precede every UTF-8 check:\n%s", cfg, fn)
		}
		// ...and ahead of the accumulator, so a skipped payload can never leave
		// bytes behind for a later declared field to inherit.
		if ai := strings.Index(fn, "self.acc"); ai >= 0 && gi > ai {
			t.Errorf("string() (%v): the destination guard must precede the accumulator:\n%s", cfg, fn)
		}
		// The maxlen guard stays destination-scoped behind it, so a declared
		// over-maxlen payload is still INVALID before any byte accumulates.
		if mi := strings.Index(fn, "self.inv = true; return; },"); mi < 0 || gi > mi {
			t.Errorf("string() (%v): the maxlen reject must survive behind the guard:\n%s", cfg, fn)
		}
	}
}

// A blob carries no encoding, so its callback keeps the plain shape: the guard is
// a string-only concern and blob() must not grow one.
func TestRustSkippedBlobKeepsPlainShape(t *testing.T) {
	m := moduleFromYAML(t, `
version: 1
messages:
  m: { payload: { b: { id: 0, type: blob, maxlen: 16 } } }
`, map[string]any{})
	if strings.Contains(sliceFn(t, m, "    fn blob("), "_ => return,") {
		t.Errorf("blob() must not carry a destination guard:\n%s", m)
	}
}

// sliceFn returns the generated function body starting at `head` up to the next
// top-level `    fn ` line, so an assertion about ordering inside one callback
// cannot accidentally match text from a neighbouring one.
func sliceFn(t *testing.T, module, head string) string {
	t.Helper()
	i := strings.Index(module, head)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", head, module)
	}
	rest := module[i+len(head):]
	if j := strings.Index(rest, "\n    fn "); j >= 0 {
		return module[i : i+len(head)+j]
	}
	return module[i:]
}

// widthSrc: one field per declared integer width plus a narrow array — the shape
// every backend's §7.1 test uses.
const widthSrc = `
version: 1
messages:
  W:
    payload:
      a_u8:   { id: 0, type: u8 }
      b_u16:  { id: 1, type: u16 }
      c_u32:  { id: 2, type: u32 }
      d_u64:  { id: 3, type: u64 }
      e_i8:   { id: 4, type: i8 }
      f_i16:  { id: 5, type: i16 }
      g_i32:  { id: 6, type: i32 }
      h_i64:  { id: 7, type: i64 }
      arr_u8: { id: 8, type: array, items: { type: u8, count: 4 } }
`

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound, not a storage hint.
// A value outside it is INVALID — never masked to the width by the `as u8` cast,
// never kept. u64/i64 span the delivery accumulator, so they get no guard at all.
func TestRustDeclaredWidthIsAValidityBound(t *testing.T) {
	for _, corelib := range []string{"rs", "rs-no-std"} {
		got := moduleFromYAML(t, widthSrc, map[string]any{"corelib": corelib})
		for _, want := range []string{
			"if value > 255 { self.inv = true; return; } self.m.a_u8 = value as u8",
			"if value > 65535 { self.inv = true; return; } self.m.b_u16 = value as u16",
			"if value > 4294967295 { self.inv = true; return; } self.m.c_u32 = value as u32",
			"if value < -128 || value > 127 { self.inv = true; return; } self.m.e_i8 = value as i8",
			"if value < -32768 || value > 32767 { self.inv = true; return; } self.m.f_i16 = value as i16",
			"if value < -2147483648 || value > 2147483647 { self.inv = true; return; } self.m.g_i32 = value as i32",
			// An ARRAY element carries the same bound, and the guard follows the fill
			// guard: an over-width scalar at an array id with no array_begin is a
			// §7.3 skip, which must not become an INVALID.
			"if self.afill == 0 { return; } self.afill -= 1; if value > 255 { self.inv = true; return; }",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("[%s] message.rs missing width guard %q:\n%s", corelib, want, got)
			}
		}
		// The 64-bit destinations must NOT be guarded: their range IS the u64/i64
		// the value arrives in, so a comparison would be dead code (and Clippy
		// would say so). Their arms carry the store and nothing else.
		for _, want := range []string{
			"(_Loc::Root, 3) => { self.m.d_u64 = value as u64 },",
			"(_Loc::Root, 7) => { self.m.h_i64 = value as i64 },",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("[%s] a 64-bit destination must store unguarded (%q):\n%s", corelib, want, got)
			}
		}
	}
}

// generator#270 (Crucible F-0045) and generator#271 (F-0046) are one slip seen
// from two sides: array_begin keyed its arms on the kind FAMILY
// (`ArrayKind::Unsigned | ArrayKind::Signed` in a single arm) and applied the
// schema `count` through a wildcard-kind arm. Both let a header whose wire kind
// §7.3 says to skip reach machinery that belongs to a field it is not.
//
//	#270: an ARRAY_UNSIGNED header at a declared `i8[]` was skipped but left the
//	      fill counter armed, so the NEXT bare scalar was absorbed into the array.
//	#271: an ARRAY_FIXLEN header at a declared `i8[]` was measured against that
//	      field's `count`, rejecting the message on a bound that was never its.
//
// Every arm is now keyed to exactly one wire kind, so the §7.3 check is decided
// by the match itself — before the counter is armed and before any bound.
func TestRustArrayBeginArmsAreKeyedByWireKind(t *testing.T) {
	const src = `
version: 1
messages:
  probe:
    payload:
      arrays:
        id: 100
        type: struct
        fields:
          u8s: { id: 0, type: array, items: { type: u8, count: 5 } }
          i8s: { id: 1, type: array, items: { type: i8, count: 5 } }
`
	for _, cfg := range []map[string]any{{"corelib": "rs"}, {"corelib": "rs-no-std"}} {
		got := moduleFromYAML(t, src, cfg)
		for _, want := range []string{
			// One arm per wire kind: never `Unsigned | Signed` collapsed together.
			"ArrayKind::Unsigned => match (self.cur, id) {",
			"ArrayKind::Signed => match (self.cur, id) {",
			// The u8 array disarms the discard counter ONLY under Unsigned, the i8
			// array ONLY under Signed — that is the §7.3 kind check (#270).
			"            ArrayKind::Unsigned => match (self.cur, id) {\n                (_Loc::Root_arrays, 0) => 0,\n                _ => count,\n            },",
			"            ArrayKind::Signed => match (self.cur, id) {\n                (_Loc::Root_arrays, 1) => 0,\n                _ => count,\n            },",
			// ... and the fill counter is armed under the same keying, so a
			// kind-mismatched header leaves it at 0 and the next bare scalar is not
			// absorbed (#270).
			"            ArrayKind::Unsigned => match (self.cur, id) {\n                (_Loc::Root_arrays, 0) => count,\n                _ => 0,\n            },",
			// The schema `count` bound names the declared kind, so a fixlen header
			// at an integer id matches no arm and is never measured (#271).
			"(ArrayKind::Unsigned, _Loc::Root_arrays, 0) => { if count > 5 { self.inv = true; return; } self.m.arrays.u8s.clear() },",
			"(ArrayKind::Signed, _Loc::Root_arrays, 1) => { if count > 5 { self.inv = true; return; } self.m.arrays.i8s.clear() },",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("(%v) array_begin must key on the wire kind, missing %q:\n%s", cfg, want, got)
			}
		}
		// The defects themselves: a collapsed integer family, and a bound reachable
		// through a wildcard kind.
		if strings.Contains(got, "ArrayKind::Unsigned | ArrayKind::Signed") {
			t.Errorf("(%v) the integer kinds must not share one arm (#270):\n%s", cfg, got)
		}
		if strings.Contains(got, "(_, _Loc::Root_arrays,") {
			t.Errorf("(%v) a schema bound must not be reachable through a wildcard kind (#271):\n%s", cfg, got)
		}
	}
}

// generator#273 (Crucible F-0048): the no_std wrapper-array element sinks
// appended into the destination where every sibling sink replaces it. A repeated
// element id therefore concatenated instead of overwriting — violating
// MESSAGE_SPEC §7.4 last-occurrence-wins — and the capacity check on the same
// line, written for an empty destination, misfired into Error::BufferFull on any
// repeat at any size. Chunk reassembly is handled upstream in `acc`, so every arm
// here receives one complete value and appending is never correct.
func TestRustNoStdWrapperElementIsReplacedNotAppended(t *testing.T) {
	const src = `
version: 1
messages:
  probe:
    payload:
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
      blob_array:   { id: 201, type: array, items: { type: blob,   count: 5, maxlen: 64 } }
      scalar_str:   { id: 202, type: string, maxlen: 64 }
`
	got := moduleFromYAML(t, src, map[string]any{"corelib": "rs-no-std"})
	for _, want := range []string{
		"if let Some(_e) = self.m.string_array.get_mut(id as usize) { _e.clear(); let _ = _e.push_str(_s);",
		"if let Some(_e) = self.m.blob_array.get_mut(id as usize) { _e.clear(); let _ = _e.extend_from_slice(_b);",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a wrapper element must be replaced, not appended to, missing %q:\n%s", want, got)
		}
	}
	// The defect: a push into an element that was never cleared.
	for _, bad := range []string{
		"get_mut(id as usize) { let _ = _e.push_str(_s);",
		"get_mut(id as usize) { let _ = _e.extend_from_slice(_b);",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("a wrapper element must not be appended to (%q):\n%s", bad, got)
		}
	}
	// The scalar sink already got this right and must keep doing so.
	if !strings.Contains(got, "self.m.scalar_str.clear(); let _ = self.m.scalar_str.push_str(_s);") {
		t.Errorf("the scalar string sink must still clear before writing:\n%s", got)
	}
}

// generator#268 (Crucible F-0044) and generator#272 (F-0047): sequence_begin's
// default arm was `_ => self.cur`, i.e. "stay where you are". A sequence the
// schema does not declare at this position was therefore ENTERED, and its
// children bound into the ENCLOSING scope:
//
//	#268: an unknown sequence id carrying a child id 3 set the ROOT's field 3.
//	#272: a sequence opened at a string-array ELEMENT position (a §7.3 wire-type
//	      contradiction) bound its string as that element.
//
// Both are one missing default: an undeclared (scope, id) must move to a dead
// scope that matches no arm, so the whole subtree — children included — is
// discarded. The live scope is restored at the matching end by the stack, or by
// the `dead` depth counter for the levels inside the skipped subtree
// (generator#283).
func TestRustUnknownSequenceIsSkippedWhole(t *testing.T) {
	const src = `
version: 1
messages:
  probe:
    payload:
      a:            { id: 3, type: i16 }
      known:        { id: 10, type: struct, fields: { k: { id: 0, type: u32 } } }
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
`
	for _, cfg := range []map[string]any{{"corelib": "rs"}, {"corelib": "rs-no-std"}} {
		got := moduleFromYAML(t, src, cfg)
		if !strings.Contains(got, "    Dead,\n}") {
			t.Errorf("(%v) _Loc needs a Dead scope for skipped subtrees:\n%s", cfg, got)
		}
		// The declared positions still descend...
		if !strings.Contains(got, "(_Loc::Root, 10) => _Loc::Root_known,") {
			t.Errorf("(%v) a declared nested struct must still be entered:\n%s", cfg, got)
		}
		// ... and everything else is discarded whole.
		if !strings.Contains(got, "            _ => _Loc::Dead,") {
			t.Errorf("(%v) sequence_begin's default arm must skip, not stay:\n%s", cfg, got)
		}
		// The defect itself.
		if strings.Contains(got, "_ => self.cur,") {
			t.Errorf("(%v) `_ => self.cur` lets a skipped subtree's children bind into the enclosing scope (#268/#272):\n%s", cfg, got)
		}
	}
}

// The same skip has to exist for a message that declares NO sequence of its own.
// The corelib cannot know which ids the schema declares, so it delivers every
// sequence; a message with no sequence_begin override at all would let the
// children of an unknown sequence arrive with `cur` still on root and bind there.
func TestRustScalarOnlyMessageStillSkipsAnUnknownSequence(t *testing.T) {
	const src = `
version: 1
messages:
  probe:
    payload:
      a: { id: 3, type: i16 }
`
	for _, cfg := range []map[string]any{{"corelib": "rs"}, {"corelib": "rs-no-std"}} {
		got := moduleFromYAML(t, src, cfg)
		// Emitted even though nothing here is a sequence, and `id` is named _id so
		// the generated crate stays warning-clean with no arms to read it.
		want := "    fn sequence_begin(&mut self, _id: Id) {\n" +
			"        // Inside a skipped subtree: count the level and stay Dead.\n" +
			"        if self.cur == _Loc::Dead { self.dead = self.dead.saturating_add(1); return; }\n" +
			"        self.stack.push(self.cur);\n" +
			"        self.cur = _Loc::Dead;\n" +
			"    }"
		if cfg["corelib"] == "rs-no-std" {
			want = strings.Replace(want, "        self.stack.push(self.cur);\n",
				"        if self.stack.push(self.cur).is_err() { self.err = true; self.dead = self.dead.saturating_add(1); self.cur = _Loc::Dead; return; }\n", 1)
		}
		if !strings.Contains(got, want) {
			t.Errorf("(%v) a scalar-only message must still override sequence_begin to skip:\n%s", cfg, got)
		}
		// A wildcard-only match would be a Clippy match_single_binding.
		if strings.Contains(got, "match (self.cur, id) {\n            _ => _Loc::Dead,") {
			t.Errorf("(%v) with no arms, emit the assignment rather than a single-binding match:\n%s", cfg, got)
		}
	}
}

// generator#283 (Crucible F-0055): the visitor's scope stack held one entry per
// OPENED sequence, so its depth was a property of the WIRE (up to MAX_DEPTH ==
// 255, §4.9/§6.2) while its no_std capacity was sized from the SCHEMA (the
// reachable frame count). A legal message that nested deeper overran it, the
// no_std push dropped the surplus silently, and the matching pops then restored
// the wrong scope — a field written after the unwind bound nowhere, and the
// message decoded ACCEPTED but missing that field.
//
// The fix is that only real scopes are stacked: a sequence opened while `cur` is
// already Dead is depth-counted instead, because every scope inside a skipped
// subtree is Dead and the stack would record nothing but the level to come back
// to. That makes the frame count a true bound on the stack, whatever the wire
// nests, on both profiles.
func TestRustSkippedSubtreeDepthIsCountedNotStacked(t *testing.T) {
	const src = `
version: 1
messages:
  probe:
    payload:
      known: { id: 10, type: struct, fields: { k: { id: 0, type: u32 } } }
`
	for _, cfg := range []map[string]any{{"corelib": "rs"}, {"corelib": "rs-no-std"}} {
		got := moduleFromYAML(t, src, cfg)
		if !strings.Contains(got, "    dead: u16,") {
			t.Errorf("(%v) the visitor needs a skipped-subtree depth counter:\n%s", cfg, got)
		}
		// It has to survive between feed calls: an unknown subtree can straddle a
		// chunk boundary in the incremental decoder.
		if !strings.Contains(got, "self.dead = dead;") {
			t.Errorf("(%v) `dead` must be part of the Decoder's persistent state:\n%s", cfg, got)
		}
		// Nothing is pushed for a level inside a skipped subtree...
		if !strings.Contains(got, "        if self.cur == _Loc::Dead { self.dead = self.dead.saturating_add(1); return; }") {
			t.Errorf("(%v) a sequence opened inside a skipped subtree must be counted, not stacked:\n%s", cfg, got)
		}
		// ... so nothing may be popped for it either.
		if !strings.Contains(got, "    fn sequence_end(&mut self) {\n"+
			"        // Closing a level of a skipped subtree: nothing was stacked for it.\n"+
			"        if self.dead > 0 { self.dead -= 1; return; }\n"+
			"        self.cur = self.stack.pop().unwrap_or(_Loc::Root);") {
			t.Errorf("(%v) sequence_end must unwind the counted levels before popping:\n%s", cfg, got)
		}
	}
	// The overflow that used to be silent is now reportable: no_std's push returns
	// a Result, and dropping it is what turned a capacity overrun into a wrong
	// value. (It is unreachable with the counter in place — this pins that the
	// belt-and-braces path stays.)
	got := moduleFromYAML(t, src, map[string]any{"corelib": "rs-no-std"})
	if !strings.Contains(got, "if self.stack.push(self.cur).is_err() { self.err = true;") {
		t.Errorf("a no_std scope-stack overflow must set the sticky err flag, not be discarded:\n%s", got)
	}
	if strings.Contains(got, "let _ = self.stack.push(self.cur);") {
		t.Errorf("the scope-stack push must not discard its Result (#283):\n%s", got)
	}
}

// TestRustStaticStorageOnStd: allow_dynamic: false against corelib-rs lowers
// schema-bounded fields to fixed-capacity heapless storage WITHOUT turning the
// crate into a no_std one, and without requiring the schema to bound everything.
//
// That last part is the whole difference from the no_std profile, and the reason
// the switch is adoptable: it applies PER FIELD wherever a bound exists, so a
// schema with one deliberately unbounded field still generates. no_std rejects
// the same schema at generate time (asserted in TestRustStructural), because a
// firmware target cannot fall back to a heap it does not have.
func TestRustStaticStorageOnStd(t *testing.T) {
	// The UNMODIFIED example: `somemap` carries no count on purpose.
	m := exampleModule(t, map[string]any{"allow_dynamic": false})

	for _, want := range []string{
		// Bounded fields take their bound as capacity.
		"pub somestring: heapless::String<50>,",
		"pub someblob: heapless::Vec<u8, 16>,",
		"pub someuintarray: heapless::Vec<u32, 4>,",
		"pub somestringarray: heapless::Vec<heapless::String<16>, 5>,",
		// Still a std crate: serde derived unconditionally, decoder scratch on the
		// heap. staticStore governs message fields, not the environment.
		"#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]",
		"stack: Vec<_Loc>,",
		// A fixed-capacity destination is filled in place and its overflow is
		// reported, rather than being moved into as an owned String.
		"self.err = true;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("std static-storage message.rs missing %q", want)
		}
	}
	// The unbounded field keeps a dynamic container instead of failing generation.
	if !strings.Contains(m, "pub somemap: Vec<") {
		t.Error("an unbounded field must stay in Vec under static storage on std (per-field, not all-or-nothing)")
	}
	for _, notWant := range []string{
		"#![no_std]",
		"alloc::string::String",
		"#[cfg_attr(feature = \"serde\", derive(Serialize, Deserialize))]",
	} {
		if strings.Contains(m, notWant) {
			t.Errorf("static storage on std must not make the crate no_std; found %q", notWant)
		}
	}

	// Same schema, same generator, dynamic storage: the DEFAULT against corelib-rs
	// is unchanged, so turning the switch on is opt-in and turning it off restores
	// byte-identical output.
	d := exampleModule(t, map[string]any{})
	if !strings.Contains(d, "pub somestring: String,") || strings.Contains(d, "heapless::") {
		t.Error("the corelib-rs default must stay String/Vec with no heapless anywhere")
	}
	if d == m {
		t.Error("allow_dynamic: false must actually change the emitted storage")
	}
}

// TestRustStaticStorageCargoDep: heapless reaches the generated std crate only
// when static storage is selected. A project that never asked for inline storage
// should not gain a third-party container dependency.
func TestRustStaticStorageCargoDep(t *testing.T) {
	cargo := func(cfg map[string]any) string {
		t.Helper()
		cfg["emit"] = "project"
		files, err := (&Backend{}).Generate(exampleSchema(t), cfg)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		for _, f := range files {
			if f.Path == "Cargo.toml" {
				return string(f.Content)
			}
		}
		t.Fatal("no Cargo.toml")
		return ""
	}
	if c := cargo(map[string]any{"allow_dynamic": false}); !strings.Contains(c, "heapless") {
		t.Error("static storage on std must declare the heapless dependency")
	}
	if c := cargo(map[string]any{}); strings.Contains(c, "heapless") {
		t.Error("the corelib-rs default must not pull in heapless")
	}
}

// A schema bound that the fixlen LENGTH WORD already decides must be latched at
// that word, not once payload bytes arrive (CORELIB_PLAN §5.2, generator#267).
//
// The bounds are not new -- a scalar/element `maxlen` and a wrapper element's
// `id >= count` were both already rejected -- but they lived in the PAYLOAD
// callback, which never fires for a message truncated immediately after the
// length word. That reported INCOMPLETE where the same bytes read whole are
// INVALID, and §5.2 makes INVALID dominate precisely because the violation is
// already established by the bytes seen.
//
// The assertions below pin the three things that make this correct rather than
// merely present: the hook exists, both bounds are inside it, and every guard
// sits behind the DECLARED-subtype test -- the hook fires for whatever subtype
// arrived at a field id, and a contradicting one is a §7.3 skip, not this
// field's length.
func TestRustFixlenBeginLatchesBoundsAtTheLengthWord(t *testing.T) {
	m := moduleFromYAML(t, `version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 8 }
      b:  { id: 1, type: blob, maxlen: 4 }
      sa: { id: 2, type: array, items: { type: string, count: 3, maxlen: 6 } }
`, map[string]any{})

	if !strings.Contains(m, "fn fixlen_begin(&mut self, id: Id, subtype: FixlenType, total: usize)") {
		t.Fatal("no fixlen_begin override")
	}
	// FixlenType is imported on demand -- naming it without the import does not compile.
	if !strings.Contains(m, ", FixlenType};") {
		t.Error("FixlenType must be imported where fixlen_begin names it")
	}
	// Scalar maxlen, keyed by (scope, field id), under the declared subtype.
	if !strings.Contains(m, "FixlenType::Str => match (self.cur, id) {") ||
		!strings.Contains(m, "(_Loc::Root, 0) => if total > 8 { self.inv = true; return; },") {
		t.Error("a scalar string maxlen must be latched under FixlenType::Str")
	}
	if !strings.Contains(m, "FixlenType::Blob => match (self.cur, id) {") ||
		!strings.Contains(m, "(_Loc::Root, 1) => if total > 4 { self.inv = true; return; },") {
		t.Error("a scalar blob maxlen must be latched under FixlenType::Blob")
	}
	// A wrapper element carries BOTH bounds, over-index first: an element that is
	// not this array's element at all must not be measured against its bound.
	if !strings.Contains(m, "(_Loc::Root_sa, _) => { if id as usize >= 3 { self.inv = true; return; } if total > 6 { self.inv = true; return; } },") {
		t.Error("a wrapper element must latch over-index then element maxlen")
	}
	// The payload-side guards STAY: unreachable now, but the only thing still
	// bounding a consumer built against a corelib without the hook.
	if !strings.Contains(m, "fn string(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {") ||
		strings.Count(m, "total > 8") < 2 {
		t.Error("the payload-side maxlen guard must remain as defense")
	}
}

// TestRustWrapperIndexCap: a DYNAMIC wrapper array's element index is bounded by
// the receiver cap, checked before the Vec grows (ARCHITECTURE §9.5,
// generator#387).
//
// A wrapper array carries no count header, so `max_dyn_array_count` never
// reached it: its elements are keyed by an unbounded varint index and the
// collector grows to id + 1. Gap filling (§5.1) is why the INDEX and not the
// element count is the bound -- two delivered elements at id 0 and id 16383 are
// a 16384-slot Vec, so the index IS the length.
//
// The category is LimitExceeded, not InvalidMsg: the bytes are well formed and
// the same message decodes under a looser cap (CORELIB_PLAN §6.2.1).
func TestRustWrapperIndexCap(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      dstrs: { id: 0, type: array, items: { type: string } }
      dblbs: { id: 1, type: array, items: { type: blob } }
      dobjs: { id: 2, type: array, items: { type: struct, fields: { x: { id: 0, type: u32 } } } }
      dmat:  { id: 3, type: array, items: { type: array, items: { type: u32 } } }
      bstrs: { id: 4, type: array, items: { type: string, count: 4 } }
`
	doc, err := parser.Parse([]byte(src), "m.yaml")
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
	var m string
	for _, f := range files {
		if f.Path == "src/message.rs" {
			m = string(f.Content)
		}
	}

	for _, want := range []string{
		"(_Loc::Root_dstrs, _) => { if id as usize >= MAX_DYN_ARRAY_COUNT { self.lim = true; return; } while self.m.dstrs.len() <= id as usize",
		"(_Loc::Root_dblbs, _) => { if id as usize >= MAX_DYN_ARRAY_COUNT { self.lim = true; return; } while self.m.dblbs.len() <= id as usize",
		"(_Loc::Root_dobjs, _) => { if id as usize >= MAX_DYN_ARRAY_COUNT { self.lim = true; return; } while self.m.dobjs.len() <= id as usize",
		// A native matrix ROW takes the index cap too: its id is the outer array's
		// length. Its own element count is capped beside it, id first.
		"_Loc::Root_dmat, _) => { if id as usize >= MAX_DYN_ARRAY_COUNT { self.lim = true; self.afill = 0; return; } if count > MAX_DYN_ARRAY_COUNT",
		// and the flag is surfaced as the policy category, never as InvalidMsg.
		"if limited { return Err(sofab::Error::LimitExceeded); }",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.rs missing wrapper index cap %q:\n%s", want, m)
		}
	}
	// The cap governs only what the schema left unbounded (§9.5): a count:N array
	// keeps its own bound and its own category.
	if !strings.Contains(m, "(_Loc::Root_bstrs, _) => { if id as usize >= 4 { self.inv = true; return; }") {
		t.Errorf("a count:N wrapper array must keep its InvalidMsg schema bound:\n%s", m)
	}
}

// TestRustCapGuardsSitInsideKeyedArms: a §7.3-skipped field must never be capped
// (CORELIB_PLAN §6.2.1, generator#410). Rust keeps every receiver cap in the
// generated visitor (ARCHITECTURE §9.5.2), so nothing but this repo decides where
// the guard sits — and the property that makes the skip safe is structural: every
// cap lives inside a match arm keyed by the wire callback it is written in plus
// `(location, id)`, so an unknown id, or a wire type that contradicts the declared
// one, reaches no arm at all and is walked past uncapped.
//
// The test is on the arms, not on a decode, because the failure it guards against
// is a widened arm: a cap hoisted to the top of a callback, or an arm relaxed to
// match any location, still compiles, still passes every substring assertion about
// the cap being present, and silently caps the field §7.3 requires to be skipped.
// tests/conformance/rust/run.sh decodes the bytes that prove the same property end
// to end; this pins the shape that keeps it true.
func TestRustCapGuardsSitInsideKeyedArms(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:   { id: 0, type: string }
      b:   { id: 1, type: blob }
      arr: { id: 2, type: array, items: { type: u64 } }
      sa:  { id: 3, type: array, items: { type: string } }
      bs:  { id: 4, type: string, maxlen: 32 }
`
	doc, err := parser.Parse([]byte(src), "dyn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	sc, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(sc); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(sc, map[string]any{
		"max_dyn_array_count": 4,
		"max_dyn_string_len":  16,
		"max_dyn_blob_len":    8,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m string
	for _, f := range files {
		if f.Path == "src/message.rs" {
			m = string(f.Content)
		}
	}
	if m == "" {
		t.Fatal("no module")
	}

	seen := 0
	for _, line := range strings.Split(m, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "MAX_DYN_") ||
			strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "const ") {
			continue
		}
		seen++
		// Every remaining occurrence is a comparison, and it must be the body of a
		// match arm whose pattern names the location (and, for arrays, the wire
		// kind) this field decodes at. A guard that is not one caps whatever the
		// callback was handed, skipped or not.
		if !strings.HasPrefix(trimmed, "(_Loc::") && !strings.HasPrefix(trimmed, "(ArrayKind::") {
			t.Errorf("a receiver cap outside a keyed match arm (§7.3-skipped fields would be capped):\n%s", trimmed)
		}
	}
	if seen < 4 {
		t.Fatalf("expected a cap on each unbounded kind (string, blob, array, wrapper array), found %d", seen)
	}

	// The schema-bounded twin is compared against its own maxlen and never against
	// the cap: §6.2.1 forbids a cap on a field the schema already bounds, and the
	// verdict differs too (InvalidMsg, not LimitExceeded).
	if strings.Contains(m, "(_Loc::Root, 4) => if total > MAX_DYN_STRING_LEN") {
		t.Error("a maxlen-bounded string must not be measured against the receiver cap")
	}
	if !strings.Contains(m, "(_Loc::Root, 4) => if total > 32 { self.inv = true; return; },") {
		t.Errorf("the maxlen-bounded string must keep its own INVALID guard:\n%s", m)
	}
}
