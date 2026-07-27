package zig

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

func exampleFiles(t *testing.T, cfg map[string]any) map[string]string {
	t.Helper()
	s := exampleSchema(t)
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

func TestZigStructural(t *testing.T) {
	m := exampleFiles(t, map[string]any{})["src/message.zig"]
	if m == "" {
		t.Fatal("no src/message.zig")
	}
	for _, want := range []string{
		"const sofab = @import(\"sofab\");",
		"pub const Myfirstmessage = struct {",
		"pub fn marshal(self: *const Myfirstmessage, os: *sofab.OStream) sofab.Error!void {",
		"pub fn encode(self: *const Myfirstmessage, alloc: std.mem.Allocator)",
		"pub const DecodeError = sofab.Error || error{IncompleteMessage};",
		"pub fn decode(alloc: std.mem.Allocator, data: []const u8) DecodeError!Myfirstmessage {",
		"const st = try sofab.decode(data, &v);",                 // corelib-zig feed(chunk)->Status: bind it (generator#120)
		"if (st == .incomplete) return error.IncompleteMessage;", // truncated input rejected, distinct from INVALID
		"const _dec_Myfirstmessage = struct {",                   // flat-visitor decoder
		"pub fn sequenceBegin(self: *_dec_Myfirstmessage",        // location-stack nesting
		"pub const MAX_SIZE: usize =",
		"someu64: u64 = 18446744073709551615,",                                                // schema default in the declaration
		"someuintarray: [4]u32 = .{ 0, 1, 1000, 4294967295 },",                                // fixed native array
		"somefloatarray: [3]f32 =",                                                            // fixed fp array
		"someboolarray: [8]bool = .{ true, true, false, false, false, false, false, false },", // tail-padded default
		"somestring: []const u8 = \"\",",                                                      // zero-copy string storage
		"someblob: []const u8 = &.{ 72, 101, 108, 108, 111 },",                                // blob default bytes
		"somemap: []const MyfirstmessageSomemap",                                              // dynamic composite array -> slice
		"if (!std.mem.eql(u32, self.someuintarray[0..], &.{ 0, 1, 1000, 4294967295 })) {",     // omit-guard vs default
		"std.mem.sliceAsBytes",                                                                // bool array 0/1 lowering
		"sofab.arrays.putChecked(&self.m.someuintarray, &self.ai,",                            // capacity-checked indexed store (generator#100)
		"if (v.inv) return error.InvalidMessage;",                                             // over-count array rejected as INVALID (generator#100)
		"if (offset != 0) return;",                                                            // single-shot payload guard
		"if (total > 50) { self.inv = true; } else { if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { self.m.somestring = chunk; } },", // bounded string: over-maxlen -> INVALID (§7.1); strict UTF-8 -> INVALID (issue #85); else zero-copy
		"/// Unsigned 8-bit integer", // descriptions as doc comments
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q", want)
		}
	}
	// A sequence-typed field is opened LAZILY and closed with the dropping end
	// (MESSAGE_SPEC S2): the write stays unconditional -- there is no generated
	// omit-guard -- but the corelib drops the frame when the nested marshal wrote
	// no child, i.e. when the object equals its declared default.
	if !strings.Contains(m, "try os.writeSequenceBeginLazy(20);") {
		t.Error("nested struct field must be opened with writeSequenceBeginLazy")
	}
	if !strings.Contains(m, "try os.writeSequenceBeginLazy(20);\n        try self.somestruct.marshal(os);\n        try os.writeSequenceEnd();") {
		t.Error("nested struct field must close with the dropping writeSequenceEnd")
	}
	// The eager begin is gone from the corelib; no call site may still use it.
	if strings.Contains(m, "os.writeSequenceBegin(") {
		t.Error("eager writeSequenceBegin must not be emitted any more")
	}
	// No heap containers in the message type: storage is fixed arrays + slices.
	for _, notWant := range []string{
		"ArrayList(", // only the encode sink may use a list, and only via _EncodeSink
	} {
		if strings.Count(m, notWant) > 1 { // once inside _EncodeSink
			t.Errorf("message.zig should not use %q for field storage", notWant)
		}
	}
}

// TestZigDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated module — private constants plus a
// per-field guard on every schema-unbounded field, feeding the sticky `lim`
// flag that decode() turns into error.LimitExceeded (after the generator#100
// InvalidMessage check). The configured value is emitted as-is (enforcement is
// per-field, so schema-bounded fields keep only their own #100 guard), an
// unset key emits nothing, and a key whose kind has no unbounded field is
// inert. Independently of the config, the dynamic-array decode path must use
// the hardened capped-eager-allocation _allocN/_put pair (a lying wire count
// must not force a huge allocation).
// buildSchema compiles an inline YAML schema for a focused backend test.
func buildSchema(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
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

// TestZigFixedArrayTrailingDefaultRun: a `count: N` array is FIXED-LENGTH, so
// its canonical encoding drops the trailing run of element-default elements --
// the decoder rebuilds it from the schema count (MESSAGE_SPEC S3, generator#136
// / F-0010). Every native element kind trims via the shared _trimTail helper.
//
// Scope guards, all asserted here:
//   - a DYNAMIC (count-less) array is never trimmed: it has no N to refill from,
//     so a trailing default element is significant data;
//   - a NESTED array row (an ArrayElem, not a field) is never trimmed: the rule
//     is scoped to fields, and a nested row is a slice anyway;
//   - a wrapper-sequence element array (string/blob/struct) has no native array
//     to trim at all.
//
// TestZigOverIndexWrapperArray: a fixed-count wrapper array (string/blob/struct
// elements) rejects an element id >= N as INVALID (self.inv, surfaced as
// error.InvalidMessage) before the slice grows (issue #142 / MESSAGE_SPEC
// §5.1/§7). A dynamic array (no count) keeps every index.
func TestZigOverIndexWrapperArray(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }
      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }
      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }
      ds: { id: 3, type: array, items: { type: string } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		// The count:N over-index guard (#142) wraps the maxlen:16 over-length
		// element reject (MESSAGE_SPEC §7.1); both flag self.inv before sofab.arrays.setElem grows.
		`.root_bs => if (id >= 4) { self.inv = true; } else { if (total > 16) { self.inv = true; } else { if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { sofab.arrays.setElem`, // string element: strict UTF-8 wraps the store
		`.root_bb => if (id >= 3) { self.inv = true; } else { if (total > 16) { self.inv = true; } else { sofab.arrays.setElem`,                                                           // blob element: opaque, stored verbatim
		".root_bp => blk: {\n                if (id >= 2) { self.inv = true; break :blk .dead; }\n",                                                                                       // bounded struct: rejected BEFORE the gap-fill grows
		`if (v.inv) return error.InvalidMessage;`, // surfaced as INVALID
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing over-index guard %q", want)
		}
	}
	// The dynamic string array keeps every index (no over-index guard); its store
	// is still strict-UTF-8-wrapped (issue #85) since a string element is materialized.
	if !strings.Contains(m, `.root_ds => if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { sofab.arrays.setElem([]const u8, self.alloc, &(self.m.ds), id, "", chunk); },`) {
		t.Errorf("dynamic string array must not carry an over-index guard:\n%s", m)
	}
}

// TestZigMaxlenReject: a bounded string/blob whose wire byte length exceeds its
// schema maxlen is malformed input, rejected as INVALID (self.inv, surfaced as
// error.InvalidMessage) at the length header before the value is stored, never
// truncated (MESSAGE_SPEC §7.1). Covers a scalar string and blob, a bounded
// wrapper string element, and asserts an unbounded string carries no maxlen
// guard.
func TestZigMaxlenReject(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      bs: { id: 0, type: string, maxlen: 8 }
      bb: { id: 1, type: blob,   maxlen: 8 }
      ws: { id: 2, type: array,  items: { type: string, maxlen: 5 } }
      us: { id: 3, type: string }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		// Bounded scalar string and blob: reject over-maxlen before storing.
		`0 => if (total > 8) { self.inv = true; } else { if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { self.m.bs = chunk; } },`, // string: strict UTF-8 wraps the store
		`1 => if (total > 8) { self.inv = true; } else { self.m.bb = chunk; },`,                                                             // blob: opaque, verbatim
		// Bounded wrapper string element: maxlen guard, then strict UTF-8, wrap the sofab.arrays.setElem placement.
		`if (total > 5) { self.inv = true; } else { if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { sofab.arrays.setElem([]const u8, self.alloc, &(self.m.ws), id, "", chunk); } }`,
		// Surfaced as INVALID.
		`if (v.inv) return error.InvalidMessage;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing maxlen guard %q:\n%s", want, m)
		}
	}
	// The unbounded scalar string (no maxlen, no configured limit) has no length
	// guard, but its store is still strict-UTF-8-wrapped (issue #85): invalid
	// UTF-8 is INVALID (self.inv), never lossy — applies to unbounded strings too.
	if !strings.Contains(m, `3 => if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { self.m.us = chunk; },`) {
		t.Errorf("unbounded string must store straight through (utf8-checked):\n%s", m)
	}
	// With no maxlen and no configured limits, no length guard exists at all.
	if strings.Contains(m, "self.lim") {
		t.Errorf("no configured limit -> no lim plumbing expected:\n%s", m)
	}
}

func TestZigFixedArrayTrailingDefaultRun(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      fu:    { id: 1, type: array, items: { type: u32, count: 5 } }
      fi:    { id: 2, type: array, items: { type: i32, count: 5 } }
      ff:    { id: 3, type: array, items: { type: fp32, count: 3 } }
      fd:    { id: 4, type: array, items: { type: fp64, count: 3 } }
      fb:    { id: 5, type: array, items: { type: boolean, count: 4 } }
      fe:    { id: 6, type: array, items: { type: enum, count: 3, enum: { RED: 0, GREEN: 1 } } }
      fbf:   { id: 7, type: array, items: { type: bitfield, count: 3, bits: { a: { pos: 0 }, b: { pos: 1 } } } }
      dyn:   { id: 8, type: array, items: { type: u32 } }
      dynf:  { id: 9, type: array, items: { type: fp32 } }
      nest:  { id: 10, type: array, items: { type: array, count: 2, items: { type: u32, count: 4 } } }
      strs:  { id: 11, type: array, items: { type: string, count: 3, maxlen: 8 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	// A fixed native array of every kind trims its trailing default run.
	for _, want := range []string{
		"try os.writeArrayUnsigned(1, sofab.arrays.trimTail(self.fu[0..]));",
		"try os.writeArraySigned(2, sofab.arrays.trimTail(self.fi[0..]));",
		"try os.writeArrayFp32(3, sofab.arrays.trimTail(self.ff[0..]));",
		"try os.writeArrayFp64(4, sofab.arrays.trimTail(self.fd[0..]));",
		// bool lowers to its 0/1 byte image; trimming that image is equivalent.
		"try os.writeArrayUnsigned(5, sofab.arrays.trimTail(std.mem.sliceAsBytes(self.fb[0..])));",
		"try os.writeArraySigned(6, sofab.arrays.trimTail(self.fe[0..]));",
		"try os.writeArrayUnsigned(7, sofab.arrays.trimTail(self.fbf[0..]));",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q", want)
		}
	}

	// A dynamic (count-less) array must NOT be trimmed.
	for _, want := range []string{
		"try os.writeArrayUnsigned(8, self.dyn);",
		"try os.writeArrayFp32(9, self.dynf);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("dynamic array must not be trimmed: missing %q", want)
		}
	}
	for _, notWant := range []string{
		"sofab.arrays.trimTail(self.dyn)",
		"sofab.arrays.trimTail(self.dynf)",
	} {
		if strings.Contains(m, notWant) {
			t.Errorf("dynamic array must not be trimmed, found %q", notWant)
		}
	}

	// A nested array row is a wrapper-sequence element, not a `count: N` field:
	// it writes the loop variable straight through, untrimmed.
	if !strings.Contains(m, "try os.writeArrayUnsigned(@intCast(_i0), _e0);") {
		t.Error("nested array row must not be trimmed")
	}
	if strings.Contains(m, "sofab.arrays.trimTail(_e0)") {
		t.Error("nested array row must not be trimmed, found sofab.arrays.trimTail(_e0)")
	}
	// A string-element array is a wrapper sequence: no native array to trim.
	if strings.Contains(m, "sofab.arrays.trimTail(self.strs") {
		t.Error("wrapper-sequence array must not be trimmed")
	}
}

// TestZigFixedArrayDefaultReset: a `count: N` array decodes to exactly N
// elements -- M from the wire, the ELEMENT default (zero) at [M,N)
// (MESSAGE_SPEC S3). The [N]T destination starts at the field's declaration
// default, so a field with a non-zero SCHEMA default must be cleared on
// arrayBegin: otherwise the tail the encoder trimmed off would decode back as
// that schema default (e.g. default [1,2,3] on count:5, value [1,2,0,0,0]
// encodes to the 2-element wire [1,2] and would decode as [1,2,3,0,0]).
//
// A field with no schema default already declares an all-zero array, so it needs
// no reset and its generated code stays unchanged.
func TestZigFixedArrayDefaultReset(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      d:     { id: 1, type: array, items: { type: u32, count: 5 }, default: [1, 2, 3] }
      zeros: { id: 2, type: array, items: { type: u32, count: 5 }, default: [0, 0] }
      plain: { id: 3, type: array, items: { type: u32, count: 5 } }
      f:     { id: 4, type: array, items: { type: fp32, count: 3 }, default: [1.5] }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	// The schema default is tail-padded to exactly N in the declaration.
	for _, want := range []string{
		"d: [5]u32 = .{ 1, 2, 3, 0, 0 },",
		"f: [3]f32 = .{ 1.5, 0.0, 0.0 },",
		"plain: [5]u32 = @splat(0),",
		// A non-zero schema default is cleared to the element default first, now
		// behind the over-count guard (generator#216): the count header is rejected
		// as INVALID before the reset, so a truncated over-count array cannot mask
		// the violation as INCOMPLETE (MESSAGE_SPEC S5.2).
		"1 => { if (count > 5) { self.inv = true; return; } self.m.d = @splat(0); },",
		"4 => { if (count > 3) { self.inv = true; return; } self.m.f = @splat(0.0); },",
		// The over-count reject is emitted at the count header for EVERY fixed-count
		// array, including ones with no reset (all-zero/absent default).
		"2 => { if (count > 5) { self.inv = true; return; } },",
		"3 => { if (count > 5) { self.inv = true; return; } },",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q", want)
		}
	}
	// An all-zero (or absent) default already matches the element default: no
	// reset, so those schemas keep their previous generated code.
	for _, notWant := range []string{
		"2 => self.m.zeros = @splat(0),",
		"3 => self.m.plain = @splat(0),",
	} {
		if strings.Contains(m, notWant) {
			t.Errorf("all-zero default needs no reset, found %q", notWant)
		}
	}
}

func TestZigDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
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
		files, err := (&Backend{}).Generate(s, cfg)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		return string(files[0].Content)
	}

	m := gen(map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	})
	for _, want := range []string{
		// Constants carry the configured values as-is (never raised to the
		// schema count of barr; that field is governed by its own bound).
		"const max_dyn_array_count: usize = 65536;",
		"const max_dyn_string_len: usize = 4096;",
		// Unbounded fields are guarded at the count/length header, before the
		// field's storage is taken.
		"1 => if (count > max_dyn_array_count) { self.lim = true; self.an = 0; } else { self.m.arr = _allocN(u64, self.alloc, count); },",
		"0 => if (total > max_dyn_string_len) { self.lim = true; } else { if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { self.m.s = chunk; } },",
		// InvalidMessage (generator#100) takes precedence over LimitExceeded.
		"if (v.inv) return error.InvalidMessage;",
		"if (v.lim) return error.LimitExceeded;",
		// The schema-bounded array keeps its generator#100 guard, now behind the
		// generator#188 fill guard (a bare scalar at this array id is skipped).
		"2 => { if (self.afill != 0) { self.afill -= 1; sofab.arrays.putChecked(&self.m.barr, &self.ai, @truncate(value), &self.inv); } },",
		// Hardened eager allocation: the untrusted wire count is capped here, and
		// sofab.arrays.putGrowing extends the slice as elements actually arrive.
		"return sofab.arrays.allocN(T, a, @min(n, 1024));",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("limits message.zig missing %q", want)
		}
	}
	if strings.Contains(m, "max_dyn_blob_len") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	// Exactly the two unbounded fields are guarded (bounded barr is not).
	if got := strings.Count(m, "self.lim = true"); got != 2 {
		t.Errorf("want exactly 2 limit guards, got %d", got)
	}

	// No limits configured -> no limit plumbing at all; the eager-allocation
	// hardening stays (it is a bugfix, not an option).
	plain := gen(map[string]any{})
	for _, notWant := range []string{"max_dyn", "lim: bool", "self.lim", "LimitExceeded"} {
		if strings.Contains(plain, notWant) {
			t.Errorf("unset limits must not emit %q", notWant)
		}
	}
	if !strings.Contains(plain, "@min(n, 1024)") {
		t.Error("no-config output must keep the hardened capped allocation")
	}
}

// TestZigMetadataDocs: enum-constant descriptions, bitfield-flag descriptions
// with a default note, and a deprecated field's `///` note all reach the
// generated source as clean Zig doc comments (Zig has no native deprecation
// attribute, so the doc line is the only marker).
func TestZigMetadataDocs(t *testing.T) {
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
      legacyId: { id: 0, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 1, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 2, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" } }
`
	doc, err := parser.Parse([]byte(src), "meta.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc.Resolve()
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
	m := string(files[0].Content)
	for _, want := range []string{
		// Enum-constant descriptions.
		"/// Node is powered down.",
		"/// Node is sampling and transmitting.",
		// Bitfield-flag description; a flag with a default carries the note,
		// one without does not.
		"/// Node has completed initialization. (default: true)",
		"/// Core temperature exceeded the safe threshold.",
		// Deprecated field: description kept, plus the `///` deprecation note.
		"/// Old identifier retained for backward compatibility.",
		"/// Deprecated.",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metadata message.zig missing %q", want)
		}
	}
	// A flag without a default must NOT get a default note.
	if strings.Contains(m, "safe threshold. (default:") {
		t.Error("flag without a default must not carry a (default: ...) note")
	}
}

func TestZigProjectMode(t *testing.T) {
	files := exampleFiles(t, map[string]any{"emit": "project"})
	for _, path := range []string{"src/message.zig", "src/main.zig", "build.zig", "build.zig.zon", "README.md"} {
		if files[path] == "" {
			t.Errorf("project mode missing %s", path)
		}
	}
	if !strings.Contains(files["build.zig.zon"], "${SOFAB_ZIG_CORELIB}") {
		t.Error("build.zig.zon must carry the corelib path placeholder")
	}
	if !strings.Contains(files["build.zig.zon"], ".name = .sofabuffers_generated") {
		t.Error("build.zig.zon must pin the fixed package name (its fingerprint depends on it)")
	}
	h := files["src/main.zig"]
	for _, want := range []string{
		"fromJson_Myfirstmessage(alloc, v)",
		"toJson_Myfirstmessage(&obj, out)",
		".number_string => |s| return std.fmt.parseInt(u64, s, 10) catch 0,", // u64 > 2^53 stays exact
		"std.json.Stringify.encodeJsonString",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("main.zig missing %q", want)
		}
	}
	// Sources mode emits no project scaffolding.
	src := exampleFiles(t, map[string]any{})
	if len(src) != 1 {
		t.Errorf("sources mode should emit only src/message.zig, got %d files", len(src))
	}
}

func TestZigKeywordEscaping(t *testing.T) {
	b, err := os.ReadFile("../../tests/matrix/corpus/defs/keywords.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(b, "keywords.yaml")
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
	m := string(files[0].Content)
	for _, want := range []string{
		"@\"const\": u32 = 0,", // Zig keyword -> quoted identifier
		"@\"fn\": u32 = 0,",
		"@\"switch\": u32 = 0,",
		"type: u32 = 0,", // primitive-type names are legal field names
	} {
		if !strings.Contains(m, want) {
			t.Errorf("keywords message.zig missing %q", want)
		}
	}
}

func TestZigDeterministic(t *testing.T) {
	a := exampleFiles(t, map[string]any{"emit": "project"})
	b := exampleFiles(t, map[string]any{"emit": "project"})
	for path, content := range a {
		if b[path] != content {
			t.Fatalf("Zig generation not deterministic (%s)", path)
		}
	}
}

// TestZigArrayAtScalarSkip: an ARRAY header delivered to a SCALAR-declared field
// id is a wire-type contradiction and must be skipped like an unknown id
// (MESSAGE_SPEC §7.3, issue #183 for integers, #193 for fp). corelib-zig streams
// array elements through the very unsigned()/signed()/fp32()/fp64() callbacks a
// lone scalar uses, so the id dispatch alone cannot tell them apart; arrayBegin
// arms `askip` with the announced count and the scalar callbacks discard exactly
// that many. A declared array of the matching element kind disarms it — integer
// arrays under .unsigned/.signed, fp arrays under .fixlen — and a message with no
// native array at all still emits arrayBegin purely to arm the guard, with `_`
// for the unused id, which Zig requires.
func TestZigArrayAtScalarSkip(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      u:  { id: 0, type: u8 }
      i:  { id: 1, type: i32 }
      ua: { id: 2, type: array, items: { type: u32, count: 4 } }
      ia: { id: 3, type: array, items: { type: i32, count: 4 } }
      fa: { id: 4, type: array, items: { type: fp32, count: 4 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		"askip: usize = 0,", // the discard counter
		"if (self.askip > 0) { self.askip -= 1; return; }",
		"pub fn arrayBegin(self: *_dec_M, id: sofab.Id, kind: sofab.ArrayKind, count: usize) void {",
		"self.askip = switch (kind) {",
		"            .unsigned, .signed => switch (self.cur) {",
		"            .fixlen => switch (self.cur) {",
		"                    2 => 0,", // declared u32 array disarms under .unsigned/.signed
		"                    3 => 0,", // declared i32 array disarms
		"                    4 => 0,", // declared fp32 array disarms under .fixlen (#193)
		"                    else => count,",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing §7.3 array-at-scalar guard %q:\n%s", want, m)
		}
	}
	// The guard sits in every callback a scalar shares: unsigned(), signed() and
	// fp32() (the schema has an fp32 array, so that callback is emitted; there is
	// no fp64, so three occurrences).
	if n := strings.Count(m, "if (self.askip > 0) { self.askip -= 1; return; }"); n != 3 {
		t.Errorf("want the §7.3 guard in unsigned(), signed() and fp32(), got %d", n)
	}

	// A message with no native array still needs the guard (corelib-zig can
	// deliver an array header at any id), and Zig rejects an unused parameter, so
	// arrayBegin takes `_` for the id it never switches on.
	sc := buildSchema(t, `
version: 1
messages:
  m: { payload: { u: { id: 0, type: u8 } } }
`)
	scf, err := (&Backend{}).Generate(sc, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	scalarOnly := string(scf[0].Content)
	if !strings.Contains(scalarOnly, "pub fn arrayBegin(self: *_dec_M, _: sofab.Id, kind: sofab.ArrayKind, count: usize) void {") {
		t.Errorf("scalar-only message.zig must emit arrayBegin with an unused id:\n%s", scalarOnly)
	}
}

// TestZigLazySequenceFraming: MESSAGE_SPEC §2 omits a sequence-typed FIELD whose
// value equals its declared default, while a wrapper-array ELEMENT keeps its
// frame even when all-default (element presence carries a dynamic array's
// length — §5.1). Both rest on corelib-zig's hold-back framing: every sequence
// opens with writeSequenceBeginLazy and the CLOSER, chosen statically from the
// position in the schema, decides whether a contentless one survives —
// writeSequenceEnd drops it, writeSequenceEndKeep forces it onto the wire.
//
// The classification under test:
//
//	struct/union FIELD                       -> writeSequenceEnd
//	wrapper array FIELD (depth 0)            -> writeSequenceEnd
//	wrapper-array ELEMENT (struct/union)     -> writeSequenceEndKeep
//	wrapper-array ELEMENT (nested array row) -> writeSequenceEndKeep
//
// Getting a FIELD wrong costs two bytes; getting an ELEMENT wrong changes the
// decoded array LENGTH, so the element frames are the load-bearing assertions.
func TestZigLazySequenceFraming(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      s:    { id: 1, type: array, items: { type: string } }
      b:    { id: 2, type: array, items: { type: blob } }
      st:   { id: 3, type: array, items: { type: struct, fields: { a: { id: 0, type: u8 } } } }
      nest: { id: 4, type: array, items: { type: array, items: { type: string } } }
      nst:  { id: 5, type: array, items: { type: array, items: { type: struct, fields: { a: { id: 0, type: u8 } } } } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	// Every sequence opens lazily: the eager begin no longer exists in the corelib.
	if strings.Contains(m, "os.writeSequenceBegin(") {
		t.Error("eager writeSequenceBegin must not be emitted any more")
	}
	// 5 field wrappers + 4 element frames (st, nest row, nst row, nst struct).
	if n := strings.Count(m, "os.writeSequenceBeginLazy("); n != 9 {
		t.Errorf("want 9 lazy sequence opens, got %d:\n%s", n, m)
	}

	for _, want := range []string{
		// FIELD wrappers, one per array field: closed with the dropping end, so an
		// empty array is omitted and absence reconstructs it.
		// (the element loop runs over the narrowed run -- see elemTrimExpr)
		"try os.writeSequenceBeginLazy(1);\n        for (_trimSlices(u8, self.s), 0..) |_e0, _i0| {\n            if (_e0.len != 0) try os.writeString(@intCast(_i0), _e0);\n        }\n        try os.writeSequenceEnd();",
		"try os.writeSequenceBeginLazy(2);\n        for (_trimSlices(u8, self.b), 0..) |_e0, _i0| {\n            if (_e0.len != 0) try os.writeBlob(@intCast(_i0), _e0);\n        }\n        try os.writeSequenceEnd();",
		// A struct ELEMENT keeps its frame even with every child at its default.
		"            try os.writeSequenceBeginLazy(@intCast(_i0));\n            try _e0.marshal(os);\n            try os.writeSequenceEndKeep();\n        }\n        try os.writeSequenceEnd();",
		// A nested array row is an ELEMENT too: its own frame is kept, while the
		// field wrapper around the rows still closes with the dropping end.
		"try os.writeSequenceBeginLazy(4);\n        for (self.nest, 0..) |_e0, _i0| {\n            try os.writeSequenceBeginLazy(@intCast(_i0));",
		"                if (_e1.len != 0) try os.writeString(@intCast(_i1), _e1);\n            }\n            try os.writeSequenceEndKeep();\n        }\n        try os.writeSequenceEnd();",
		// Array of arrays of structs: row frame AND per-struct element frame kept.
		"                try os.writeSequenceBeginLazy(@intCast(_i1));\n                try _e1.marshal(os);\n                try os.writeSequenceEndKeep();\n            }\n            try os.writeSequenceEndKeep();\n        }\n        try os.writeSequenceEnd();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing lazy-framing shape %q:\n%s", want, m)
		}
	}

	// One keeping closer per wrapper-array ELEMENT site, one dropping closer per
	// array FIELD wrapper. ("...EndKeep();" is not a substring of "...End();".)
	if n := strings.Count(m, "os.writeSequenceEndKeep();"); n != 4 {
		t.Errorf("want 4 keeping closers (one per wrapper-array element site), got %d", n)
	}
	if n := strings.Count(m, "os.writeSequenceEnd();"); n != 5 {
		t.Errorf("want 5 dropping closers (one per array FIELD wrapper), got %d", n)
	}

	// A struct/union FIELD is a sequence too: lazily opened, dropping closer.
	sf := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      inner: { id: 1, type: struct, fields: { a: { id: 0, type: u8 } } }
`)
	sff, err := (&Backend{}).Generate(sf, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	sm := string(sff[0].Content)
	if !strings.Contains(sm, "try os.writeSequenceBeginLazy(1);\n        try self.inner.marshal(os);\n        try os.writeSequenceEnd();") {
		t.Errorf("a struct FIELD must be opened lazily and closed with the dropping end:\n%s", sm)
	}
	if strings.Contains(sm, "writeSequenceEndKeep") {
		t.Error("a struct FIELD must not keep an all-default frame")
	}
}

// A count:N wrapper array's canonical wire stops at M -- one past its last
// non-default element (MESSAGE_SPEC §3/§5.1, "even for sequence-form elements")
// -- and M == 0 leaves the whole wrapper omitted (§2). generator#248: the
// element loop used to run to the slice length, framing every trailing
// all-default element, so a decoder that accepted the non-canonical form
// re-encoded it unchanged instead of normalising. A DYNAMIC array has no N to
// refill from, so its trailing default element is significant and stays framed.
func TestZigFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	// The fixed array narrows to M before framing anything...
	if !strings.Contains(m, "for (_trimObjs(VecFixedElem, self.fixed), 0..) |*_e0, _i0| {") {
		t.Errorf("count:N struct array must loop to M, not to the slice length:\n%s", m)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(m, "for (self.dynamic, 0..) |*_e0, _i0| {") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", m)
	}
	// An interior all-default element is still framed: only the TRAILING run goes.
	if !strings.Contains(m, "            try os.writeSequenceBeginLazy(@intCast(_i0));\n            try _e0.marshal(os);\n            try os.writeSequenceEndKeep();\n") {
		t.Errorf("interior elements must keep the framing closer:\n%s", m)
	}

	// isDefault is the exact negation of what marshal writes, so it must narrow a
	// field exactly when the marshal loop does -- disagreeing would either omit a
	// field that is on the wire or keep one that is not.
	if !strings.Contains(m, "if (_trimObjs(VecFixedElem, self.fixed).len != 0) return false;") {
		t.Errorf("isDefault must narrow the fixed array like marshal does:\n%s", m)
	}
	if !strings.Contains(m, "if (self.dynamic.len != 0) return false;") {
		t.Errorf("isDefault must NOT narrow the dynamic array:\n%s", m)
	}
	if !strings.Contains(m, "if (_trimSlices(u8, self.fstrs).len != 0) return false;") {
		t.Errorf("isDefault for a string wrapper array must test the narrowed run:\n%s", m)
	}
	// The element predicate itself: the explicit form of the "no child was
	// written" test the lazy framing only encodes implicitly for a FIELD.
	if !strings.Contains(m, "pub fn isDefault(self: *const VecFixedElem) bool {\n        if (self.k != 0) return false;\n        return true;\n    }") {
		t.Errorf("every struct type must carry the all-default predicate:\n%s", m)
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at dest[id] after gap-filling -- never appended. Appending
// shortened the array by the size of any interior id gap and decoded a REOPENED
// id as a second element instead of merging into the first (§7.4). The leaf
// string/blob path next to it (sofab.arrays.setElem) always got this right.
//
// The N-fill when the sequence scope closes is what makes the §3/§5.1 trailing
// elision lossless: without it, re-encoding a decoded fixed array shortens it on
// every round trip.
func TestZigWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      dyn:  { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	for _, want := range []string{
		// placement, not append -- and the gap-fill that precedes it
		"                if (!sofab.arrays.grow(VecObjsElem, self.alloc, &(self.m.objs), @as(usize, id) + 1, .{})) break :blk .dead;\n                self.ei_root_objs = id;\n                break :blk .root_objs_e;",
		// the child stores address that element, never the last appended one
		"0 => _at(self.m.objs, self.ei_root_objs).k = @truncate(value),",
		// the cap bound still rejects an out-of-range element id, which also
		// bounds the gap-fill above
		"                if (id >= 4) { self.inv = true; break :blk .dead; }",
		// N-fill when the sequence scope closes
		"            .root_objs => _ = sofab.arrays.grow(VecObjsElem, self.alloc, &(self.m.objs), 4, .{}),",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The defect this replaced: appending ignored the id entirely.
	if strings.Contains(m, "self.m.objs.len + 1") || strings.Contains(m, "sofab.arrays.last(self.m.objs)") {
		t.Errorf("a wrapper element must not be appended id-blind:\n%s", m)
	}
	// A dynamic array has no N to refill from: its length is highest-present-id
	// + 1, so it is never filled.
	if strings.Contains(m, "&(self.m.dyn), 4") || strings.Contains(m, ".root_dyn => _ = sofab.arrays.grow") {
		t.Errorf("a dynamic wrapper array must never be default-filled:\n%s", m)
	}
}
