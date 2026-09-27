package zig

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// unionSrc covers every union site and option shape the backend lowers
// differently: a field union whose default_id is a STRUCT option that is not the
// first, scalar / string / blob / compact-array / wrapper-array / struct / union
// options, a keyword option (`error`) and one on the union's own API (`which`);
// an array of unions whose default_id is its string option; a $defs union used
// with two default_ids (split into two types); and a single-option union.
const unionSrc = `
version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  m:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          box:   { id: 5, type: struct, fields: { z: { id: 0, type: u8 } } }
          which: { id: 6, type: boolean }
          error: { id: 7, type: blob, maxlen: 4 }
          inner: { id: 8, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
      v: { id: 1, type: array, items: { type: union, count: 4, default_id: 1, oneof: { i: { id: 0, type: i32 }, s: { id: 1, type: string, maxlen: 8 } } } }
      pf: { id: 2, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      po: { id: 3, type: union, oneof: { $ref: "#/$defs/union/Pick" } }
      one: { id: 4, type: union, oneof: { only: { id: 0, type: u8 } } }
`

// generateYAML runs the whole front end over src and hands back Generate's
// result, error included.
func generateYAML(t *testing.T, src string, cfg map[string]any) ([]generator.File, error) {
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
	return (&Backend{}).Generate(s, cfg)
}

// unionFiles generates unionSrc as a project: message.zig and the harness.
func unionFiles(t *testing.T) (msg, harness string) {
	t.Helper()
	files, err := generateYAML(t, unionSrc, map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		switch f.Path {
		case "src/message.zig":
			msg = string(f.Content)
		case "src/main.zig":
			harness = string(f.Content)
		}
	}
	return msg, harness
}

func wantCode(t *testing.T, got, why string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !containsCode(got, w) {
			t.Errorf("%s, missing %q:\n%s", why, w, got)
		}
	}
}

// section returns the text from head up to (not including) the next line that
// starts at column 0 with `pub const ` or `const ` -- one top-level declaration.
func section(t *testing.T, m, head string) string {
	t.Helper()
	i := strings.Index(m, head)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", head, m)
	}
	rest := m[i+len(head):]
	end := len(rest)
	for _, next := range []string{"\npub const ", "\nconst "} {
		if j := strings.Index(rest, next); j >= 0 && j < end {
			end = j
		}
	}
	return head + rest[:end]
}

// A union is a native tagged union holding one option; its `init` is
// default_id at that option's own default, per (union, default_id) type, and a
// union field and a union array's gap fill both start from it.
func TestZigUnionIsTaggedUnion(t *testing.T) {
	m, _ := unionFiles(t)
	u := section(t, m, "pub const MU = union(enum) {")
	wantCode(t, u, "the union is a tagged union with one field per option",
		"num: u16,", "s: []const u8,", "pt: MUPt,", "arr: sofab.FixedArray(u16, 4),",
		"strs: []const []const u8,", "box: MUBox,", "which_: bool,", `@"error": []const u8,`, "inner: MUInner,",
		// D is `pt` (id 2), not the first option.
		"pub const init: MU = .{ .pt = .{} };")
	wantCode(t, m, "a union field starts from its type's init", "u: MU = .init,")
	// A union option that is itself a union starts from ITS init (b = -2).
	wantCode(t, m, "a nested union's init is its own default_id",
		"pub const init: MUInner = .{ .b = -2 };",
		"if (self.* != .inner) self.* = .{ .inner = .init };")
	// The $defs union splits per default_id (§1.1); each split type bakes its own D.
	wantCode(t, m, "split $defs types carry their own default",
		"pub const init: UnionPickDefaultT = .{ .t = .{} };",
		"pub const init: UnionPickDefaultN = .{ .n = 6 };",
		"pf: UnionPickDefaultT = .init,", "po: UnionPickDefaultN = .init,")
	// The element gap fill is the element type's init: D = `s` at "".
	wantCode(t, m, "an array of unions fills its gaps with the element type's init",
		"pub const init: MVElem = .{ .s = \"\" };",
		"sofab.arrays.reserveElem(MVElem, .{ .schema = 4 }, self.alloc, &(self.m.v), id, .init)")
	if containsCode(m, "MU = .{}") || containsCode(m, "reserveElem(MVElem, .{ .schema = 4 }, self.alloc, &(self.m.v), id, .{})") {
		t.Errorf("a union must never be built as a product-type `.{}`:\n%s", m)
	}
}

// The API: an id constant and a select-if-not-held accessor per option, and
// which(). Names that land on the union's own declarations are mangled; a
// keyword option is a quoted identifier.
func TestZigUnionAccessors(t *testing.T) {
	m, _ := unionFiles(t)
	u := section(t, m, "pub const MU = union(enum) {")
	wantCode(t, u, "the union API",
		"pub const num_id: sofab.Id = 0;", "pub const error_id: sofab.Id = 7;", "pub const which_id: sofab.Id = 6;",
		"pub fn which(self: *const MU) sofab.Id { return switch (self.*) { .num => num_id,",
		".which_ => which_id,", `.@"error" => error_id,`,
		// select if not held: the option is (re)built only when another one is held.
		"pub fn ptMut(self: *MU) *MUPt { if (self.* != .pt) self.* = .{ .pt = .{} }; return &self.pt; }",
		"pub fn numMut(self: *MU) *u16 { if (self.* != .num) self.* = .{ .num = 5 }; return &self.num; }",
		"pub fn whichMut(self: *MU) *bool { if (self.* != .which_) self.* = .{ .which_ = false }; return &self.which_; }",
		`pub fn errorMut(self: *MU) *[]const u8 { if (self.* != .@"error") self.* = .{ .@"error" = "" }; return &self.@"error"; }`)
	// A single-option union holds that option always: no tag test.
	one := section(t, m, "pub const MOne = union(enum) {")
	wantCode(t, one, "a single-option accessor", "pub fn onlyMut(self: *MOne) *u8 { return &self.only; }")
}

// Encode (§0 / MESSAGE_SPEC §4.2): default_id is guarded and closed with the
// dropping end like an ordinary field; every other option is written
// unguarded, a sequence-framed one (struct, union, wrapper array) closed with
// the keeping end; isDefault is "D held and D at its default".
func TestZigUnionEncodeArms(t *testing.T) {
	m, _ := unionFiles(t)
	u := section(t, m, "pub const MU = union(enum) {")
	wantCode(t, u, "encode arms",
		".num => { try os.writeUnsigned(0, self.num); },",
		".s => { try os.writeString(1, self.s); },",
		".pt => { try os.writeSequenceBeginLazy(2); try self.pt.serialize(os); try os.writeSequenceEnd(); },",
		".arr => { try os.writeArrayUnsigned(3, self.arr.slice()); },",
		"try os.writeString(@intCast(_i0), _e0); } try os.writeSequenceEndKeep(); },",
		".box => { try os.writeSequenceBeginLazy(5); try self.box.serialize(os); try os.writeSequenceEndKeep(); },",
		".which_ => { try os.writeBoolean(6, self.which_); },",
		`.@"error" => { try os.writeBlob(7, self.@"error"); },`,
		".inner => { try os.writeSequenceBeginLazy(8); try self.inner.serialize(os); try os.writeSequenceEndKeep(); },",
		"pub fn isDefault(self: *const MU) bool { return self.* == .pt and self.pt.isDefault(); }")
	// A leaf D keeps the ordinary field's ≠-default guard.
	inner := section(t, m, "pub const MUInner = union(enum) {")
	wantCode(t, inner, "a leaf default_id is guarded; the other option is not",
		".a => { try os.writeUnsigned(0, self.a); },",
		".b => { if (self.b != -2) try os.writeSigned(1, self.b); },",
		"return self.* == .b and !(self.b != -2);")
	// The product-type shape: every option guarded and all written side by side.
	for _, bad := range []string{"if (self.num != 5)", "if (self.s.len != 0)", "if (self.arr.len() != 0)"} {
		if containsCode(u, bad) {
			t.Errorf("a non-default option must be written even at its own default, found guard %q:\n%s", bad, u)
		}
	}
	// The union FIELD keeps the ordinary lazy frame with the dropping end.
	wantCode(t, m, "a union field", "try os.writeSequenceBeginLazy(0); try self.u.serialize(os); try os.writeSequenceEnd();")
}

// Decode (§0 / MESSAGE_SPEC §7.4.1): a scalar store assigns the tagged union
// after its width guard; a string/blob option only at the completion store; a
// compact array option in arrayBegin behind the kind gate and the over-count
// reject; a struct/union/wrapper option at its sequence begin, through the
// select-if-not-held accessor. Nothing re-emplaces a held option.
func TestZigUnionDecodeSwitch(t *testing.T) {
	m, _ := unionFiles(t)
	dec := section(t, m, "const _dec_M = struct {")
	wantCode(t, dec, "the decode switch",
		// scalar: switch and store are one assignment, after the width guard.
		"0 => { if (value > 65535) { self.inv = true; return; } self.m.u = .{ .num = @intCast(value) }; },",
		"6 => self.m.u = .{ .which_ = value != 0 },",
		// string / blob: the completion store, after the bind returned the whole payload.
		"1 => { const chunk = self._takeStr(total, offset, _chunk) orelse return; self.m.u = .{ .s = chunk }; },",
		`7 => { const chunk = self._take(total, offset, _chunk) orelse return; self.m.u = .{ .@"error" = chunk }; },`,
		// compact array: behind the kind gate and the over-count reject.
		"3 => if (kind == .unsigned) { if (count > 4) { self.inv = true; return; } self.m.u.arrMut().*.clear(); },",
		// struct / union option: selected at its sequence begin, kept when held.
		"2 => blk: { _ = self.m.u.ptMut(); break :blk .root_u_pt; },",
		"8 => blk: { _ = self.m.u.innerMut(); break :blk .root_u_inner; },",
		// wrapper option: selected, then replaced whole (§7.4).
		"4 => blk: { self.m.u.strsMut().* = &.{}; break :blk .root_u_strs; },",
		// every path below an option goes through its accessor.
		"self.m.u.ptMut().*.x = @intCast(value);",
		"self.m.u.innerMut().* = .{ .a = @intCast(value) };",
		"sofab.arrays.placeElem([]const u8, .{ .schema = 3 }, self.alloc, &(self.m.u.strsMut().*), id, \"\", chunk)",
		// an element union: a leaf option replaces the element.
		"sofab.arrays.at(self.m.v, self.ei_root_v).* = .{ .i = @intCast(value) };",
		"sofab.arrays.at(self.m.v, self.ei_root_v).* = .{ .s = chunk };",
		// the split types: each site decodes into its own type.
		"self.m.pf.tMut().*.k = @intCast(value);", "self.m.po = .{ .n = @intCast(value) };")
	// Nothing in the decoder rebuilds a struct/union option or a whole union at
	// its default: that would wipe a held option on a repeated frame (§7.4) or
	// on a resumed feed.
	for _, bad := range []string{".{ .pt = .{} }", ".{ .inner = .init }", "self.m.u = .init", "self.m.u.pt.", "self.m.u.num ="} {
		if containsCode(dec, bad) {
			t.Errorf("the decoder must reach an option only through its select-if-not-held accessor, found %q:\n%s", bad, dec)
		}
	}
	// The length word decides bounds only: fixlenBegin never selects.
	fb := dec[strings.Index(dec, "pub fn fixlenBegin"):]
	fb = fb[:strings.Index(fb, "\n    }\n")]
	if strings.Contains(fb, "self.m.u =") || strings.Contains(fb, "Mut()") {
		t.Errorf("fixlenBegin must not switch the union (the payload is not complete yet):\n%s", fb)
	}
}

// JSON (harness): exactly one member, the option held; fromJson starts from
// init and selects through the accessor.
func TestZigUnionJSON(t *testing.T) {
	_, h := unionFiles(t)
	wantCode(t, h, "the union JSON form",
		"fn toJson_MU(o: *const message.MU, w: *std.Io.Writer) std.Io.Writer.Error!void { switch (o.*) {",
		`.num => { try w.writeAll("{\"num\":"); try w.print("{d}", .{o.num}); try w.writeByte('}'); },`,
		`.pt => { try w.writeAll("{\"pt\":"); try toJson_MUPt(&o.pt, w); try w.writeByte('}'); },`,
		"var o: message.MU = .init;",
		`if (obj.get("num")) |x| o.numMut().* = @intCast(jsonU64(x));`,
		`if (obj.get("pt")) |x| o.ptMut().* = fromJson_MUPt(alloc, x);`,
		`o.arrMut().*.set(t0[0..n0]);`)
}

// Options that derive the same Zig member fail generation, naming both.
func TestZigUnionNameClash(t *testing.T) {
	for _, c := range []struct{ a, b, member string }{
		{"a", "aMut", "aMut"},
		{"a", "a_id", "a_id"},
		{"init", "init_", "init_"},
	} {
		src := "version: 1\nmessages:\n  m:\n    payload:\n      u: { id: 0, type: union, oneof: { " +
			c.a + ": { id: 0, type: u8 }, " + c.b + ": { id: 1, type: u8 } } }\n"
		_, err := generateYAML(t, src, map[string]any{})
		if err == nil {
			t.Errorf("options %q and %q must fail generation", c.a, c.b)
			continue
		}
		for _, w := range []string{c.a, c.b, c.member} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("clash error for %q/%q does not name %q: %v", c.a, c.b, w, err)
			}
		}
	}
}
