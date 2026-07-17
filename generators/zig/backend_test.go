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
		"_putc(&self.m.someuintarray, &self.ai,",                                              // capacity-checked indexed store (generator#100)
		"if (v.inv) return error.InvalidMessage;",                                             // over-count array rejected as INVALID (generator#100)
		"if (offset != 0) return;",                                                            // single-shot payload guard
		"self.m.somestring = chunk,",                                                          // zero-copy string decode
		"/// Unsigned 8-bit integer",                                                          // descriptions as doc comments
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q", want)
		}
	}
	// Sequences are always framed (never omit-guarded); the struct field write
	// must be unconditional.
	if !strings.Contains(m, "try os.writeSequenceBegin(20);") {
		t.Error("nested struct must be framed unconditionally")
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
		`.root_bs => if (id >= 4) { self.inv = true; } else { _setElem`,         // bounded string
		`.root_bb => if (id >= 3) { self.inv = true; } else { _setElem`,         // bounded blob
		`.root_bp => blk: { if (id >= 2) self.inv = true; break :blk if (_grow`, // bounded struct
		`if (v.inv) return error.InvalidMessage;`,                               // surfaced as INVALID
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing over-index guard %q", want)
		}
	}
	// The dynamic string array keeps every index — its arm is a bare _setElem.
	if !strings.Contains(m, `.root_ds => _setElem([]const u8, self.alloc, &(self.m.ds), id, "", chunk),`) {
		t.Errorf("dynamic string array must not carry an over-index guard:\n%s", m)
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
		"try os.writeArrayUnsigned(1, _trimTail(self.fu[0..]));",
		"try os.writeArraySigned(2, _trimTail(self.fi[0..]));",
		"try os.writeArrayFp32(3, _trimTail(self.ff[0..]));",
		"try os.writeArrayFp64(4, _trimTail(self.fd[0..]));",
		// bool lowers to its 0/1 byte image; trimming that image is equivalent.
		"try os.writeArrayUnsigned(5, _trimTail(std.mem.sliceAsBytes(self.fb[0..])));",
		"try os.writeArraySigned(6, _trimTail(self.fe[0..]));",
		"try os.writeArrayUnsigned(7, _trimTail(self.fbf[0..]));",
		// The helper compares the element BYTE IMAGE, never ==: a trailing -0.0
		// (which == 0.0) must survive, and a NaN is never a default.
		"fn _trimTail(a: anytype) []const std.meta.Elem(@TypeOf(a)) {",
		"while (n > 0 and std.mem.allEqual(u8, std.mem.asBytes(&a[n - 1]), 0)) : (n -= 1) {}",
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
		"_trimTail(self.dyn)",
		"_trimTail(self.dynf)",
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
	if strings.Contains(m, "_trimTail(_e0)") {
		t.Error("nested array row must not be trimmed, found _trimTail(_e0)")
	}
	// A string-element array is a wrapper sequence: no native array to trim.
	if strings.Contains(m, "_trimTail(self.strs") {
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
		// A non-zero schema default is cleared to the element default first.
		"1 => self.m.d = @splat(0),",
		"4 => self.m.f = @splat(0.0),",
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
		"0 => if (total > max_dyn_string_len) { self.lim = true; } else { self.m.s = chunk; },",
		// InvalidMessage (generator#100) takes precedence over LimitExceeded.
		"if (v.inv) return error.InvalidMessage;",
		"if (v.lim) return error.LimitExceeded;",
		// The schema-bounded array keeps only its generator#100 guard.
		"2 => _putc(&self.m.barr, &self.ai, @truncate(value), &self.inv),",
		// Hardened eager allocation: cap the untrusted wire count...
		"const s = a.alloc(T, @min(n, 1024)) catch return &.{};",
		// ...and grow as elements actually arrive, never past the announced count.
		"const new = a.alloc(T, @min(@max(s.*.len * 2, i.* + 1), n)) catch return;",
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
