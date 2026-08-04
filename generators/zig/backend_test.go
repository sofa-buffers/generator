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
		"pub fn serialize(self: *const Myfirstmessage, os: *sofab.OStream) sofab.Error!void {",
		"pub fn encode(self: *const Myfirstmessage, alloc: std.mem.Allocator)",
		"pub const DecodeError = sofab.Error || error{IncompleteMessage};",
		"pub fn decode(alloc: std.mem.Allocator, data: []const u8) DecodeError!Myfirstmessage {",
		"const st = try sofab.decode(data, &v);",                 // corelib-zig feed(chunk)->Status: bind it (generator#120)
		"if (st == .incomplete) return error.IncompleteMessage;", // truncated input rejected, distinct from INVALID
		"const _dec_Myfirstmessage = struct {",                   // flat-visitor decoder
		"pub fn sequenceBegin(self: *_dec_Myfirstmessage",        // location-stack nesting
		"pub const MAX_SIZE: usize =",
		"someu64: u64 = 18446744073709551615,",                                                     // schema default in the declaration
		"someuintarray: FixedArray(u32, 4) = .{ .items = .{ 0, 1, 1000, 4294967295 }, .len = 4 },", // count:N native array: N of inline capacity plus the length
		"somefloatarray: FixedArray(f32, 3) =",                                                     // count:N fp array
		// a 3-element default is 3 long: `count` is a capacity, never padded to (§3)
		"someboolarray: FixedArray(bool, 8) = .{ .items = .{ true, true, false, false, false, false, false, false }, .len = 3 },",
		"somestring: []const u8 = \"\",",                                                     // zero-copy string storage
		"someblob: []const u8 = &.{ 72, 101, 108, 108, 111 },",                               // blob default bytes
		"somemap: []const MyfirstmessageSomemap",                                             // dynamic composite array -> slice
		"if (!std.mem.eql(u32, self.someuintarray.slice(), &.{ 0, 1, 1000, 4294967295 })) {", // omit-guard vs default, over the VALUE (items[0..len])
		"std.mem.sliceAsBytes",                                                               // bool array 0/1 lowering
		"sofab.arrays.putChecked(&self.m.someuintarray.items, &self.ai,",                     // capacity-checked indexed store (generator#100)
		"if (v.inv) return error.InvalidMessage;",                                            // over-count array rejected as INVALID (generator#100)
		"const chunk = self._reassemble(total, offset, _chunk) orelse return;",               // one contiguous payload, whatever the chunking
		// A reassembled payload leaves as its OWN allocation. `acc` is scratch the
		// next split payload clears and may reallocate, and destinations keep the
		// slice they are handed, so returning a view into it aliased earlier
		// wrapper-array elements onto the newest (generator#293 / F-0058).
		"return self.alloc.dupe(u8, self.acc.items[0..total]) catch { self.inv = true; return null; };",
		// The borrow is available to decode() only. On the streaming path a
		// delivered slice may point into the corelib's reused carry buffer, which
		// the next stitched item overwrites (generator#295).
		"if (!self.own) return chunk; // contiguous decode: borrow the caller's buffer",
		"return .{ .v = .{ .m = out, .alloc = alloc, .own = true } };",
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
	if !strings.Contains(m, "try os.writeSequenceBeginLazy(20);\n        try self.somestruct.serialize(os);\n        try os.writeSequenceEnd();") {
		t.Error("nested struct field must close with the dropping writeSequenceEnd")
	}
	// The eager begin is gone from the corelib; no call site may still use it.
	if strings.Contains(m, "os.writeSequenceBegin(") {
		t.Error("eager writeSequenceBegin must not be emitted any more")
	}
	// The shared scratch buffer must never reach a destination directly: that is
	// the exact shape of generator#293, and it reads as a harmless one-liner.
	if strings.Contains(m, "return self.acc.items;") {
		t.Error("_reassemble must hand out a copy, not a view into the shared acc buffer (generator#293)")
	}
	// No heap containers in the message type: storage is fixed arrays + slices.
	// The check is on the MESSAGE STRUCT, not the file: two pieces of codec
	// machinery legitimately hold a list and are not field storage -- the encode
	// sink's output buffer (_EncodeSink) and the decoder's chunk-reassembly
	// buffer (_dec_*.acc, which only a payload split across feed chunks reaches).
	if body, ok := structBody(m, "pub const Myfirstmessage = struct {"); !ok {
		t.Error("message.zig: could not locate the Myfirstmessage struct")
	} else if strings.Contains(body, "ArrayList(") {
		t.Errorf("message.zig uses a heap container for field storage:\n%s", body)
	}
}

// structBody returns the text between `header` and the matching closing brace at
// the same nesting depth, so an invariant about a type's own members is not
// fooled by machinery declared elsewhere in the file.
func structBody(src, header string) (string, bool) {
	i := strings.Index(src, header)
	if i < 0 {
		return "", false
	}
	rest := src[i+len(header):]
	depth := 1
	for j, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:j], true
			}
		}
	}
	return "", false
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

// TestZigNativeArrayCarriesItsLength: `count: N` is a CAPACITY, never a length
// (MESSAGE_SPEC §3, documentation af536c4). The wire count M IS a compact array's
// length, so nothing that carries the length may be elided: the trailing-default
// run stays on the wire for every native element kind, with or without a
// declared count, and there is no trim helper left in the emitted code.
//
// The storage follows: a count:N field is FixedArray(T, N) -- N of inline
// capacity PLUS the length -- because a bare `[N]T` can only ever BE N long and
// so cannot express M < N. The value it encodes is `.slice()`, its first `.len`
// elements.
func TestZigNativeArrayCarriesItsLength(t *testing.T) {
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

	// A count:N native array of every kind writes its VALUE whole -- the wire
	// count IS the length, so no trailing run may be dropped.
	for _, want := range []string{
		"fu: FixedArray(u32, 5) = .{},",
		"try os.writeArrayUnsigned(1, self.fu.slice());",
		"try os.writeArraySigned(2, self.fi.slice());",
		"try os.writeArrayFp32(3, self.ff.slice());",
		"try os.writeArrayFp64(4, self.fd.slice());",
		// bool lowers to its 0/1 byte image over the same value slice.
		"try os.writeArrayUnsigned(5, std.mem.sliceAsBytes(self.fb.slice()));",
		"try os.writeArraySigned(6, self.fe.slice());",
		"try os.writeArrayUnsigned(7, self.fbf.slice());",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q", want)
		}
	}

	// A dynamic (count-less) array is unchanged: it was never trimmed.
	for _, want := range []string{
		"try os.writeArrayUnsigned(8, self.dyn);",
		"try os.writeArrayFp32(9, self.dynf);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("dynamic array must write its value whole: missing %q", want)
		}
	}

	// A nested array row writes the loop variable straight through.
	if !strings.Contains(m, "try os.writeArrayUnsigned(@intCast(_i0), _e0);") {
		t.Error("nested array row must be written whole")
	}
	// The trailing-run trim is gone from every call site (the corelib still ships
	// sofab.arrays.trimTail; the generator simply stops calling it).
	if strings.Contains(m, "trimTail") {
		t.Errorf("no trailing-default-run trim may survive:\n%s", m)
	}
	// So is the fixed-count wrapper narrowing that went with it.
	for _, notWant := range []string{"_trimObjs", "_trimSlices"} {
		if strings.Contains(m, notWant) {
			t.Errorf("the count:N wrapper trim %q must be gone:\n%s", notWant, m)
		}
	}
}

// TestZigCountIsCapacityStorage: `count: N` is a capacity, so a count:N native
// array is FixedArray(T, N) -- N of inline storage plus the length -- and its
// declared `default` stands exactly as written, never padded out to N.
//
// Decode follows: the array header resets the length (the wire count IS the
// length, §3 -- an explicitly empty array decodes to the EMPTY array, not to N
// element defaults and not to the previous value), and the element stores
// advance it. The reset is gated on the announced wire kind, so a header whose
// kind contradicts the declared element type is skipped like an unknown id and
// leaves a correctly typed earlier occurrence intact (§7.3/§7.4).
func TestZigCountIsCapacityStorage(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      d:     { id: 1, type: array, items: { type: u32, count: 5 }, default: [1, 2, 3] }
      zeros: { id: 2, type: array, items: { type: u32, count: 5 }, default: [0, 0] }
      plain: { id: 3, type: array, items: { type: u32, count: 5 } }
      f:     { id: 4, type: array, items: { type: fp32, count: 3 }, default: [1.5] }
      e:     { id: 5, type: array, items: { type: i16, count: 2 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	for _, want := range []string{
		// The declared default is the VALUE: `.len` is its own element count, and
		// only the inline storage is N wide. A 3-element default on count:5 is a
		// 3-element array, not a 5-element one.
		"d: FixedArray(u32, 5) = .{ .items = .{ 1, 2, 3, 0, 0 }, .len = 3 },",
		"f: FixedArray(f32, 3) = .{ .items = .{ 1.5, 0.0, 0.0 }, .len = 1 },",
		"zeros: FixedArray(u32, 5) = .{ .items = .{ 0, 0, 0, 0, 0 }, .len = 2 },",
		// No default at all: the EMPTY array, which is what a fresh count:N array
		// now is (it used to be N element defaults).
		"plain: FixedArray(u32, 5) = .{},",
		// The over-count reject stays at the count header (generator#216), now
		// followed by the length reset and gated on the wire kind.
		"1 => if (kind == .unsigned) { if (count > 5) { self.inv = true; return; } self.m.d.len = 0; },",
		"4 => if (kind == .fp32) { if (count > 3) { self.inv = true; return; } self.m.f.len = 0; },",
		"5 => if (kind == .signed) { if (count > 2) { self.inv = true; return; } self.m.e.len = 0; },",
		// The store advances the length: M elements arrive, M is the length. The
		// §7.1 width guard for the u32 element sits inside the fill guard and ahead
		// of the store, and the cast is @intCast because the value provably fits by
		// then (see TestZigDeclaredWidthIsAValidityBound).
		"1 => { if (self.afill != 0) { self.afill -= 1; if (value > 4294967295) { self.inv = true; return; } sofab.arrays.putChecked(&self.m.d.items, &self.ai, @intCast(value), &self.inv); self.m.d.len = self.ai; } },",
		// The storage type itself.
		"pub fn FixedArray(comptime T: type, comptime N: usize) type {",
		"        pub fn slice(self: *const Self) []const T {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// Nothing refills the tail any more: the [M, N) slots are spare capacity.
	if strings.Contains(m, "@splat(") {
		t.Errorf("a count:N array must not be splat-reset (that was the fill-to-N):\n%s", m)
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
		"1 => if (kind == .unsigned) { if (count > max_dyn_array_count) { self.lim = true; self.an = 0; } else { self.m.arr = _allocN(u64, self.alloc, count); } },",
		"0 => if (total > max_dyn_string_len) { self.lim = true; } else { if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { self.m.s = chunk; } },",
		// InvalidMessage (generator#100) takes precedence over LimitExceeded.
		"if (v.inv) return error.InvalidMessage;",
		"if (v.lim) return error.LimitExceeded;",
		// The schema-bounded array keeps its generator#100 guard, now behind the
		// generator#188 fill guard (a bare scalar at this array id is skipped).
		"2 => { if (self.afill != 0) { self.afill -= 1; if (value < -2147483648 or value > 2147483647) { self.inv = true; return; } sofab.arrays.putChecked(&self.m.barr.items, &self.ai, @intCast(value), &self.inv); self.m.barr.len = self.ai; } },",
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
// arrays under .unsigned/.signed, fp arrays under the prong for their own element
// subtype (.fp32 / .fp64, generator#259) — and a message with no native array at
// all still emits arrayBegin purely to arm the guard, with `_` for the unused id,
// which Zig requires.
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
		"            .unsigned => switch (self.cur) {",
		"            .fp32 => switch (self.cur) {",
		"            .fp64 => switch (self.cur) {",
		"                    2 => 0,", // declared u32 array disarms under .unsigned/.signed
		"                    3 => 0,", // declared i32 array disarms
		"                    4 => 0,", // declared fp32 array disarms under .fp32 (#193, #259)
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

// TestZigFixlenArrayKindBySubtype: a fixlen array's `count` word precedes its
// `fixlen_word`, so a receiver cannot know whether the array that arrived IS the
// declared field's value until the element subtype is in hand. CORELIB_PLAN §4.8
// therefore fires arrayBegin past the fixlen_word and reports the SUBTYPE —
// ArrayKind is {unsigned = 0, signed = 1, fp32 = 2, fp64 = 3}, the collapsed
// `fixlen` member is gone (generator#259 / Crucible F-0042).
//
// What the generated arms must do with that:
//
//   - a declared fp32[N] is listed ONLY under .fp32, a declared fp64[N] ONLY
//     under .fp64 — never under both, which is what a collapsed kind forced;
//   - the schema `count > N` bound sits INSIDE the kind test, so an fp64 header
//     at the fp32 slot is never measured against the fp32 field's N. Bounding
//     first would turn a skippable wire-type contradiction (§7.3) into INVALID;
//   - under the non-matching kind the id disarms nothing: it takes
//     `else => count`, so the elements are discarded, and the declared field is
//     not sized, cleared or otherwise touched — a correctly typed earlier
//     occurrence survives (§7.4).
func TestZigFixlenArrayKindBySubtype(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      s32: { id: 0, type: fp32 }
      s64: { id: 1, type: fp64 }
      a32: { id: 2, type: array, items: { type: fp32, count: 5 } }
      a64: { id: 3, type: array, items: { type: fp64, count: 7 } }
      au:  { id: 4, type: array, items: { type: u32, count: 3 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	for _, want := range []string{
		// The bound lives behind the kind test, keyed by the field's OWN subtype.
		"2 => if (kind == .fp32) { if (count > 5) { self.inv = true; return; } self.m.a32.len = 0; },",
		"3 => if (kind == .fp64) { if (count > 7) { self.inv = true; return; } self.m.a64.len = 0; },",
		// Integer arrays are untouched by all of this: there is no second word on
		// that path, so .unsigned/.signed stay one prong.
		"4 => if (kind == .unsigned) { if (count > 3) { self.inv = true; return; } self.m.au.len = 0; },",
		// Three prongs cover all four ArrayKind members, so no else prong (Zig
		// rejects an unreachable one).
		"        self.askip = switch (kind) {",
		"        self.afill = switch (kind) {",
		"            .unsigned => switch (self.cur) {",
		"            .fp32 => switch (self.cur) {",
		"            .fp64 => switch (self.cur) {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The collapsed kind is gone from the emitted code entirely.
	if strings.Contains(m, ".fixlen") {
		t.Errorf("ArrayKind.fixlen no longer exists in corelib-zig; emitted code must not name it:\n%s", m)
	}

	// Each fp arm lists its own id and only its own id. Slice out the .fp32 and
	// .fp64 prongs of the skip arm and check membership in both directions: id 2
	// (fp32[5]) disarms under .fp32 and NOT under .fp64, id 3 the mirror. That is
	// the whole fix — under the other prong the id falls to `else => count` and
	// the array is discarded like one at an unknown id.
	skip := between(t, m, "self.askip = switch (kind) {", "self.afill = switch (kind) {")
	fp32Arm := between(t, skip, ".fp32 => switch (self.cur) {", ".fp64 => switch (self.cur) {")
	fp64Arm := skip[strings.Index(skip, ".fp64 => switch (self.cur) {"):]
	for _, tc := range []struct {
		arm, name string
		want      string // the id that must disarm here
		notWant   string // the id that must NOT
	}{
		{fp32Arm, ".fp32", "2 => 0,", "3 => 0,"},
		{fp64Arm, ".fp64", "3 => 0,", "2 => 0,"},
	} {
		if !strings.Contains(tc.arm, tc.want) {
			t.Errorf("the %s skip arm must disarm %q:\n%s", tc.name, tc.want, tc.arm)
		}
		if strings.Contains(tc.arm, tc.notWant) {
			t.Errorf("the %s skip arm must NOT disarm %q (that field's subtype is the other one):\n%s", tc.name, tc.notWant, tc.arm)
		}
		if !strings.Contains(tc.arm, "else => count,") {
			t.Errorf("the %s skip arm must discard every other id:\n%s", tc.name, tc.arm)
		}
	}
}

// between returns the slice of s strictly between the first occurrence of from
// and the first occurrence of to after it, failing the test if either is absent.
func between(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("missing %q in:\n%s", from, s)
	}
	rest := s[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("missing %q after %q in:\n%s", to, from, s)
	}
	return rest[:j]
}

// TestZigLazySequenceFraming: MESSAGE_SPEC §2 omits a sequence-typed FIELD whose
// value equals its declared default; a wrapper-array ELEMENT follows the same
// rule POSITIONALLY — the interior is sparse (an all-default element is not
// framed and leaves an id gap), the LAST element is always written, as an empty
// frame if that is all it is. All of it rests on corelib-zig's hold-back
// framing: every sequence opens with writeSequenceBeginLazy and the CLOSER
// decides whether a contentless one survives — writeSequenceEnd drops it,
// writeSequenceEndKeep forces it onto the wire.
//
// The classification under test:
//
//	struct/union FIELD                       -> writeSequenceEnd
//	wrapper array FIELD (depth 0)            -> writeSequenceEnd
//	wrapper-array ELEMENT (struct/union)     -> Keep at the last index, End before
//	wrapper-array ELEMENT (nested array row) -> Keep at the last index, End before
//
// Getting a FIELD wrong costs two bytes; getting an ELEMENT wrong changes the
// decoded array LENGTH, so the element frames are the load-bearing assertions.
// The element choice is made from the position in the VALUE, at run time — the
// schema cannot answer it, which is why it is an emitted `if`, not a constant.
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
      mat:  { id: 6, type: array, items: { type: array, count: 4, items: { type: u32, count: 4 } } }
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
	// 6 field wrappers + 4 element frames (st, nest row, nst row, nst struct); a
	// native `mat` row has no frame of its own.
	if n := strings.Count(m, "os.writeSequenceBeginLazy("); n != 10 {
		t.Errorf("want 10 lazy sequence opens, got %d:\n%s", n, m)
	}

	for _, want := range []string{
		// FIELD wrappers, one per array field: closed with the dropping end, so an
		// empty array is omitted and absence reconstructs it. A leaf element is
		// omitted in the interior when it equals the element default, and written
		// at the last index whatever its value.
		"try os.writeSequenceBeginLazy(1);\n        for (self.s, 0..) |_e0, _i0| {\n            if (_e0.len != 0 or _i0 == self.s.len - 1) try os.writeString(@intCast(_i0), _e0);\n        }\n        try os.writeSequenceEnd();",
		"try os.writeSequenceBeginLazy(2);\n        for (self.b, 0..) |_e0, _i0| {\n            if (_e0.len != 0 or _i0 == self.b.len - 1) try os.writeBlob(@intCast(_i0), _e0);\n        }\n        try os.writeSequenceEnd();",
		// A struct ELEMENT takes the SAME rule through its closer: the keeping one
		// at the last index, the dropping one (an id gap) in the interior.
		"            try os.writeSequenceBeginLazy(@intCast(_i0));\n            try _e0.serialize(os);\n            if (_i0 == self.st.len - 1) {\n                try os.writeSequenceEndKeep();\n            } else {\n                try os.writeSequenceEnd();\n            }\n        }\n        try os.writeSequenceEnd();",
		// A wrapper nested row is an ELEMENT too, with its own frame and the same
		// positional closer; the field wrapper around the rows still drops.
		"try os.writeSequenceBeginLazy(4);\n        for (self.nest, 0..) |_e0, _i0| {\n            try os.writeSequenceBeginLazy(@intCast(_i0));",
		"                if (_e1.len != 0 or _i1 == _e0.len - 1) try os.writeString(@intCast(_i1), _e1);\n            }\n            if (_i0 == self.nest.len - 1) {\n                try os.writeSequenceEndKeep();\n            } else {\n                try os.writeSequenceEnd();\n            }\n        }\n        try os.writeSequenceEnd();",
		// Array of arrays of structs: the rule applies independently at both depths.
		"                try os.writeSequenceBeginLazy(@intCast(_i1));\n                try _e1.serialize(os);\n                if (_i1 == _e0.len - 1) {\n                    try os.writeSequenceEndKeep();\n                } else {\n                    try os.writeSequenceEnd();\n                }\n            }\n            if (_i0 == self.nst.len - 1) {",
		// A NATIVE row has no frame of its own, so the rule lands on the write: an
		// interior empty row is not written at all, the last one always is.
		"        for (self.mat, 0..) |_e0, _i0| {\n            if (_e0.len != 0 or _i0 == self.mat.len - 1) {\n                try os.writeArrayUnsigned(@intCast(_i0), _e0);\n            }\n        }",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing lazy-framing shape %q:\n%s", want, m)
		}
	}

	// One keeping closer per wrapper-array ELEMENT site (st, nest row, nst row,
	// nst struct), and one dropping closer per element site PLUS one per array
	// FIELD wrapper -- the element sites now emit both arms of the positional
	// choice. ("...EndKeep();" is not a substring of "...End();".)
	if n := strings.Count(m, "os.writeSequenceEndKeep();"); n != 4 {
		t.Errorf("want 4 keeping closers (one per wrapper-array element site), got %d", n)
	}
	if n := strings.Count(m, "os.writeSequenceEnd();"); n != 10 {
		t.Errorf("want 10 dropping closers (6 field wrappers + 4 element interiors), got %d", n)
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
	if !strings.Contains(sm, "try os.writeSequenceBeginLazy(1);\n        try self.inner.serialize(os);\n        try os.writeSequenceEnd();") {
		t.Errorf("a struct FIELD must be opened lazily and closed with the dropping end:\n%s", sm)
	}
	if strings.Contains(sm, "writeSequenceEndKeep") {
		t.Error("a struct FIELD must not keep an all-default frame")
	}
}

// One sparse rule for both element kinds, with or without a declared `count`
// (MESSAGE_SPEC §2, documentation af536c4): the element loop runs over every
// element the value holds -- no trailing run is narrowed away, because the
// highest wrapper id IS the array's last index (§5.1) -- and what the INTERIOR
// may drop is the value that is indistinguishable from absence.
func TestZigWrapperArrayIsInteriorSparse(t *testing.T) {
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

	// Both arrays loop over the value itself: a `count: N` is a capacity, so it
	// narrows nothing.
	for _, want := range []string{
		"for (self.fixed, 0..) |*_e0, _i0| {",
		"for (self.dynamic, 0..) |*_e0, _i0| {",
		"for (self.fstrs, 0..) |_e0, _i0| {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the element loop must run over the whole value: missing %q:\n%s", want, m)
		}
	}
	// The count:N struct array's closer is positional, exactly like the dynamic
	// one's: dropped in the interior (an id gap), kept at the last index.
	if !strings.Contains(m, "            try os.writeSequenceBeginLazy(@intCast(_i0));\n            try _e0.serialize(os);\n            if (_i0 == self.fixed.len - 1) {\n                try os.writeSequenceEndKeep();\n            } else {\n                try os.writeSequenceEnd();\n            }\n") {
		t.Errorf("a count:N struct element must take the positional closer:\n%s", m)
	}
	// A count:N string element gets the same last-index escape as a dynamic one.
	if !strings.Contains(m, "if (_e0.len != 0 or _i0 == self.fstrs.len - 1) try os.writeString(@intCast(_i0), _e0);") {
		t.Errorf("a count:N string element must keep the last-index write:\n%s", m)
	}

	// isDefault is the exact negation of what marshal writes. The writer emits a
	// child for every element it holds (the last one whatever its value), so "no
	// child is written" is exactly "the array is empty" -- for either kind, with
	// or without a count. The two cannot drift apart any more.
	for _, want := range []string{
		"if (self.fixed.len != 0) return false;",
		"if (self.dynamic.len != 0) return false;",
		"if (self.fstrs.len != 0) return false;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("isDefault must test emptiness alone: missing %q:\n%s", want, m)
		}
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
// Nothing is filled in on the way out: the highest present id + 1 IS the decoded
// length (§5.1), and a declared `count: N` is a capacity that bounds the ids and
// adds no elements (§3).
func TestZigWrapperElementsArePlacedByID(t *testing.T) {
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
		"_at(self.m.objs, self.ei_root_objs).k = @intCast(value); },",
		// the cap bound still rejects an out-of-range element id, which also
		// bounds the gap-fill above
		"                if (id >= 4) { self.inv = true; break :blk .dead; }",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The defect this replaced: appending ignored the id entirely.
	if strings.Contains(m, "self.m.objs.len + 1") || strings.Contains(m, "sofab.arrays.last(self.m.objs)") {
		t.Errorf("a wrapper element must not be appended id-blind:\n%s", m)
	}
	// No fill-to-N survives, for either kind: `count` never adds elements.
	if strings.Contains(m, "&(self.m.objs), 4") || strings.Contains(m, "=> _ = sofab.arrays.grow") {
		t.Errorf("a count:N wrapper array must not be default-filled to N:\n%s", m)
	}
}

// TestZigMatrixRowsArePlacedByID: an array whose elements are NATIVE arrays
// collected its rows through arrayBegin, which appended one per header and
// ignored the element id. That was unreachable while every row was framed; §2's
// interior-sparse rule makes an omitted all-default (empty) row reachable, and an
// appending collector then shifts every later row down by one. Rows are placed at
// out[id] like every other element kind, bounded by the outer array's `count`.
func TestZigMatrixRowsArePlacedByID(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  vec:
    payload:
      mat: { id: 0, type: array, items: { type: array, count: 4, items: { type: u32, count: 4 } } }
      dyn: { id: 1, type: array, items: { type: array, items: { type: fp32 } } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	for _, want := range []string{
		// The row is placed at its element id, after the gap-fill, and the index is
		// recorded so the element stores address THAT row.
		".root_mat => if (kind == .unsigned) if (id >= 4) { self.inv = true; } else { self.ei_root_mat = id; if (sofab.arrays.grow([]const u32, self.alloc, &(self.m.mat), @as(usize, id) + 1, &.{})) { _at(self.m.mat, id).* = _allocN(u32, self.alloc, count); } },",
		"self.ei_root_mat < self.m.mat.len) sofab.arrays.putGrowing(_at(self.m.mat, self.ei_root_mat), self.alloc, &self.ai, self.an,",
		// The fp row collector is the same shape.
		"if (self.ei_root_dyn < self.m.dyn.len) sofab.arrays.putGrowing(_at(self.m.dyn, self.ei_root_dyn), self.alloc, &self.ai, self.an, value)",
		// The index registers exist, one per collecting frame.
		"    ei_root_mat: usize = 0,",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The defect: appending at the end of the outer slice, id unread.
	if strings.Contains(m, "self.m.mat.len + 1") || strings.Contains(m, "sofab.arrays.last(self.m.mat)") {
		t.Errorf("a matrix row must not be appended id-blind:\n%s", m)
	}
}

// A `count: N` array is not materialized to N elements anywhere: `count` is a
// capacity, not a length (MESSAGE_SPEC §3), so a fresh one -- native or wrapper
// -- is the EMPTY array. It used to be N element defaults, to match a
// fixed-length reading of `count` that the spec no longer takes.
func TestZigCountNArrayIsNotMaterialized(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  vec:
    payload:
      strs:  { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }
      blobs: { id: 1, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      objs:  { id: 2, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }
      rows:  { id: 3, type: array, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }
      nums:  { id: 4, type: array, items: { type: u32, count: 3 } }
      dstrs: { id: 5, type: array, items: { type: string, maxlen: 8 } }
      dobjs: { id: 6, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	for _, want := range []string{
		// A count:N wrapper array is the empty slice, exactly like a count-less one.
		`    strs: []const []const u8 = &.{},`,
		`    blobs: []const []const u8 = &.{},`,
		"    objs: []const VecObjsElem = &.{},",
		"    rows: []const []const []const u8 = &.{},",
		"    dstrs: []const []const u8 = &.{},",
		"    dobjs: []const VecDobjsElem = &.{},",
		// The native twin agrees: N of inline capacity, length 0.
		"    nums: FixedArray(u32, 3) = .{},",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The `**` repetition literal was the materialization; it must be gone.
	if strings.Contains(m, "** 3)") || strings.Contains(m, "** 2)") {
		t.Errorf("no count:N array may be materialized to N element defaults:\n%s", m)
	}

	// sequenceBegin still resets the field first: an explicit empty wrapper must
	// override a non-empty value (§7.4, the array wrapper is REPLACED whole).
	for _, want := range []string{
		"0 => blk: { self.m.strs = &.{}; break :blk .root_strs; },",
		"2 => blk: { self.m.objs = &.{}; break :blk .root_objs; },",
		"3 => blk: { self.m.rows = &.{}; break :blk .root_rows; },",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing the sequenceBegin reset %q:\n%s", want, m)
		}
	}
}

// The LAST element of a wrapper array is always written, whatever its value, and
// with or without a declared `count` (MESSAGE_SPEC §2, documentation af536c4).
// An array recovers its length as highest-present-id + 1 (§5.1), so the element
// at the highest index is the only one whose PRESENCE carries the length:
// dropping it encodes ["a", ""] exactly like ["a"] and decodes one element short.
// A `count: N` is no exemption -- N is a capacity and can never restore an
// elided tail.
func TestZigArrayAlwaysWritesLastElement(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  vec:
    payload:
      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)

	for _, want := range []string{
		"for (self.dynstr, 0..) |_e0, _i0| {\n            if (_e0.len != 0 or _i0 == self.dynstr.len - 1) try os.writeString(@intCast(_i0), _e0);",
		"for (self.dynblob, 0..) |_e0, _i0| {\n            if (_e0.len != 0 or _i0 == self.dynblob.len - 1) try os.writeBlob(@intCast(_i0), _e0);",
		// the count:N array takes the very same guard -- one rule, one shape
		"for (self.fixedstr, 0..) |_e0, _i0| {\n            if (_e0.len != 0 or _i0 == self.fixedstr.len - 1) try os.writeString(@intCast(_i0), _e0);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The all-default predicate has to follow the writer: [""] puts an element on
	// the wire, so the field is NOT default and must not be omitted.
	for _, want := range []string{
		"if (self.dynstr.len != 0) return false;",
		"if (self.dynblob.len != 0) return false;",
		"if (self.fixedstr.len != 0) return false;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("isDefault must test emptiness alone: missing %q:\n%s", want, m)
		}
	}
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound. This is the backend
// where the defect was written down as intent — storeCast used to @truncate with
// the comment "the declared width is a storage hint" — so the test also pins the
// cast: @intCast, reached only for a value the guard has already let through.
func TestZigDeclaredWidthIsAValidityBound(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  W:
    payload:
      a_u8:   { id: 0, type: u8 }
      c_u32:  { id: 2, type: u32 }
      d_u64:  { id: 3, type: u64 }
      e_i8:   { id: 4, type: i8 }
      g_i32:  { id: 6, type: i32 }
      h_i64:  { id: 7, type: i64 }
      arr_u8: { id: 8, type: array, items: { type: u8, count: 4 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		"0 => { if (value > 255) { self.inv = true; return; } self.m.a_u8 = @intCast(value); },",
		"2 => { if (value > 4294967295) { self.inv = true; return; } self.m.c_u32 = @intCast(value); },",
		"4 => { if (value < -128 or value > 127) { self.inv = true; return; } self.m.e_i8 = @intCast(value); },",
		"6 => { if (value < -2147483648 or value > 2147483647) { self.inv = true; return; } self.m.g_i32 = @intCast(value); },",
		// Array element: guard inside the fill guard, so a §7.3 skip stays a skip.
		"8 => { if (self.afill != 0) { self.afill -= 1; if (value > 255) { self.inv = true; return; }",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing width guard %q:\n%s", want, m)
		}
	}
	// 64-bit destinations pass through with neither guard nor cast.
	for _, want := range []string{"3 => self.m.d_u64 = value,", "7 => self.m.h_i64 = value,"} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig: a 64-bit destination must store unguarded (%q):\n%s", want, m)
		}
	}
	// The masking cast is gone: nothing may @truncate a decoded scalar any more.
	if strings.Contains(m, "@truncate(value)") {
		t.Errorf("a decoded value must never be masked to the declared width (§7.1):\n%s", m)
	}
}

// generator#268 (Crucible F-0044), #270 (F-0045) and #272 (F-0047) are the zig
// half of the skip family. Two causes:
//
//   - arrayBegin armed its counters on the kind FAMILY (`.unsigned, .signed` in
//     one arm), so an .unsigned header at a declared `i8[]` was skipped but left
//     the fill counter armed and the NEXT bare scalar was absorbed (#270).
//   - sequenceBegin's default arms were `else => self.cur` ("stay put"), so an
//     unknown sequence id (#268) and a §7.3-mistyped element sequence (#272)
//     were ENTERED and their children bound into the enclosing scope.
func TestZigSkippedSubtreeAndArrayKindKeying(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  probe:
    payload:
      a: { id: 3, type: i16 }
      arrays:
        id: 100
        type: struct
        fields:
          u8s: { id: 0, type: array, items: { type: u8, count: 5 } }
          i8s: { id: 1, type: array, items: { type: i8, count: 5 } }
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		// One arm per wire kind (#270).
		"            .unsigned => switch (self.cur) {",
		"            .signed => switch (self.cur) {",
		// The declared position still descends...
		"                100 => .root_arrays,",
		// ... everything else is discarded whole (#268/#272).
		"                else => .dead,",
		"            else => .dead,",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q:\n%s", want, m)
		}
	}
	// The defects themselves.
	if strings.Contains(m, ".unsigned, .signed =>") {
		t.Errorf("the integer kinds must not share one arrayBegin arm (#270):\n%s", m)
	}
	if strings.Contains(m, "else => self.cur,") {
		t.Errorf("`else => self.cur` lets a skipped subtree's children bind into the enclosing scope (#268/#272):\n%s", m)
	}
}

// The same skip must exist for a message that declares NO sequence of its own.
// corelib-zig only checks @hasDecl for the callback (istream.zig
// T_SEQUENCE_START) — it does not skip the subtree on its own — so a visitor
// without sequenceBegin would let an unknown sequence's children arrive with
// `cur` still on root and bind there.
func TestZigScalarOnlyMessageStillSkipsAnUnknownSequence(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  probe:
    payload:
      a: { id: 3, type: i16 }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	want := "    pub fn sequenceBegin(self: *_dec_Probe, _: sofab.Id) void {\n" +
		"        if (self.sp < self.stack.len) {\n" +
		"            self.stack[self.sp] = self.cur;\n" +
		"            self.sp += 1;\n" +
		"        }\n" +
		"        self.cur = .dead;\n" +
		"    }"
	if !strings.Contains(m, want) {
		t.Errorf("a scalar-only message must still override sequenceBegin to skip:\n%s", m)
	}
	if !strings.Contains(m, "pub fn sequenceEnd(") {
		t.Errorf("sequenceEnd must accompany it, or the stack never unwinds:\n%s", m)
	}
}
