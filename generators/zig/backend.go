// Package zig is the Zig backend (corelib-zig, the max-speed port): structs
// with schema defaults in the type declaration, marshal() over OStream, and a
// flat-visitor decode. The corelib's visitor is comptime duck typing
// (sequenceBegin/sequenceEnd events, no child visitors), so decode is a
// (location, id) state machine with a location stack -- the same shape as the
// Rust backend, monomorphized with zero dispatch overhead.
//
// The generated code follows the corelib's speed contract: encode() streams
// through a caller-owned scratch buffer into a growable list via the flush
// sink; decode() is zero-copy for strings and blobs (the decoded message
// borrows those bytes from the input buffer) and allocates array storage from
// the caller's allocator -- pass an arena and free everything at once.
package zig

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for Zig.
type Backend struct{}

func (*Backend) Lang() string { return "zig" }

// Generate emits src/message.zig; project mode adds build.zig + build.zig.zon
// and a JSON encode/decode harness (src/main.zig).
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{
		schema:  s,
		banner:  cfgString(cfg, "tool_banner", "sofabgen"),
		license: generator.LicenseID(cfg),
		limits:  resolveLimits(s, cfg),
		size:    generator.NewSizePolicy(cfg),
	}
	files := []generator.File{{Path: "src/message.zig", Content: g.module(s)}}
	if cfgString(cfg, "emit", "sources") == "project" {
		files = append(files, g.projectFiles(s)...)
	}
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	banner  string
	license string // SPDX id, "" to omit the header line
	limits  limitSet
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
}

// messageSize resolves a message's worst-case encoded size via the shared walk
// (ir.MaxWireSize), falling back to the configured max_message_size ceiling when
// a field is unbounded. The emit path has no error channel, so a violation of an
// explicitly configured ceiling is recorded here and surfaced by Generate.
func (g *gen) messageSize(name string, fields []*ir.Field) generator.MessageSize {
	ms, err := g.size.Resolve(name, fields)
	if err != nil && g.sizeErr == nil {
		g.sizeErr = err
	}
	return ms
}

// limitSet is the receiver-side decode-limit configuration (generator#102),
// resolved against the schema: an entry is active only when its key is
// configured AND the schema has an unbounded field of that kind — otherwise
// the option is inert and no plumbing is emitted. Enforcement is per-field in
// the generated decoder (schema-bounded fields keep only their generator#100
// guard), so the configured value is emitted as-is, never raised to a schema
// bound.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits reads the max_dyn_* config keys and resolves them against the
// schema's bounds (see limitSet).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	var l limitSet
	if v, ok := cfgLimit(cfg, "max_dyn_array_count"); ok && b.HasDynArray {
		l.arrayCount, l.arrayHas = v, true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_string_len"); ok && b.HasDynString {
		l.stringLen, l.stringHas = v, true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_blob_len"); ok && b.HasDynBlob {
		l.blobLen, l.blobHas = v, true
	}
	return l
}

type zfile struct{ b strings.Builder }

func (f *zfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *zfile) blank()        { f.b.WriteByte('\n') }
func (f *zfile) bytes() []byte { return []byte(f.b.String()) }

// emitDoc writes a Zig doc comment (`///`, one line per line of text) at the
// given indent. Empty text emits nothing, so it never leaves a dangling `///`.
func (f *zfile) emitDoc(indent, text string) {
	if text == "" {
		return
	}
	for _, ln := range strings.Split(text, "\n") {
		f.line("%s/// %s", indent, ln)
	}
}

// fieldDoc builds a field's doc-comment text from its Description and Unit.
// A deprecated field gets a trailing "Deprecated." note (Zig has no native
// deprecation attribute, so the doc line is the only marker).
func fieldDoc(fld *ir.Field) string {
	var doc string
	switch {
	case fld.Description != "" && fld.Unit != "":
		doc = fld.Description + " (unit: " + fld.Unit + ")"
	case fld.Description != "":
		doc = fld.Description
	case fld.Unit != "":
		doc = "(unit: " + fld.Unit + ")"
	}
	if fld.Deprecated {
		if doc != "" {
			doc += "\n"
		}
		doc += "Deprecated."
	}
	return doc
}

func (g *gen) module(s *ir.Schema) []byte {
	f := &zfile{}
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	f.line("//! SofaBuffers message types over the `sofab` Zig corelib (max-speed build).")
	f.line("//! decode() borrows string/blob bytes from the input buffer (zero-copy) and")
	f.line("//! allocates array storage from the caller's allocator; pass an arena and")
	f.line("//! free the whole message at once.")
	f.blank()
	f.line("const std = @import(\"std\");")
	f.line("const sofab = @import(\"sofab\");")
	f.blank()
	f.line("/// Error set of the one-shot decode() wrappers: the corelib baseline plus")
	f.line("/// IncompleteMessage. The corelib reports INCOMPLETE as a")
	f.line("/// non-error decode Status -- the caller owns end-of-input -- and for a")
	f.line("/// one-shot decode over a whole buffer, end-of-input is here: a trailing")
	f.line("/// .incomplete means the message was truncated. Kept distinct from")
	f.line("/// error.InvalidMessage so INCOMPLETE and INVALID never collapse.")
	f.line("pub const DecodeError = sofab.Error || error{IncompleteMessage};")
	f.blank()

	if g.limits.any() {
		f.line("// Receiver-side decode limits, baked from the sofabgen config")
		f.line("// (max_dyn_*): they govern only schema-unbounded fields (an array without")
		f.line("// `count`, a string/blob without `maxlen`) and are checked at the count or")
		f.line("// length header, before the field's storage is allocated. Exceeding a cap")
		f.line("// fails decode() with error.LimitExceeded -- a policy rejection, never a clamp.")
		if g.limits.arrayHas {
			f.line("const max_dyn_array_count: usize = %d;", g.limits.arrayCount)
		}
		if g.limits.stringHas {
			f.line("const max_dyn_string_len: usize = %d;", g.limits.stringLen)
		}
		if g.limits.blobHas {
			f.line("const max_dyn_blob_len: usize = %d;", g.limits.blobLen)
		}
		f.blank()
	}

	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfieldConsts(f, nt)
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			g.emitStruct(f, g.typeName(key), nt.Fields, false, "")
		}
	}
	for _, m := range s.Messages {
		g.emitStruct(f, exported(m.Name), m.Fields, true, m.Summary)
	}
	for _, m := range s.Messages {
		g.emitDecoder(f, exported(m.Name), m.Fields)
	}
	g.emitSupport(f, g.dynAllocUse(s))
	return f.bytes()
}

func (g *gen) emitEnum(f *zfile, nt *ir.NamedType) {
	backing := enumBacking(nt)
	f.line("pub const %s = struct {", strings.ToLower(g.typeName(nt.Key)))
	for _, c := range nt.Consts {
		f.emitDoc("    ", c.Description)
		f.line("    pub const %s: %s = %d;", strings.ToUpper(c.Name), backing, c.Value)
	}
	f.line("};")
	f.blank()
}

func (g *gen) emitBitfieldConsts(f *zfile, nt *ir.NamedType) {
	backing := bitfieldBacking(nt)
	f.line("pub const %s = struct {", strings.ToLower(g.typeName(nt.Key)))
	for _, fl := range nt.Flags {
		doc := fl.Description
		if fl.HasDefault {
			if doc != "" {
				doc += " "
			}
			if fl.Default {
				doc += "(default: true)"
			} else {
				doc += "(default: false)"
			}
		}
		f.emitDoc("    ", doc)
		f.line("    pub const %s: %s = 1 << %d;", strings.ToUpper(fl.Name), backing, fl.Pos)
	}
	f.line("};")
	f.blank()
}

func (g *gen) emitStruct(f *zfile, name string, fields []*ir.Field, isMessage bool, summary string) {
	f.emitDoc("", summary)
	f.line("pub const %s = struct {", name)
	// Field defaults ARE the schema defaults: a plain `.{}` message carries
	// them, and sparse-canonical decode (MESSAGE_SPEC S2) reconstructs an
	// omitted field simply by leaving the default in place.
	for _, fld := range fields {
		f.emitDoc("    ", fieldDoc(fld))
		f.line("    %s: %s = %s,", zigIdent(fld.Name), g.zigType(fld), g.zigFieldDefault(fld))
	}
	f.blank()
	if isMessage {
		ms := g.messageSize(name, fields)
		if !ms.Bounded {
			f.line("    /// Configured ceiling (max_message_size): an unbounded field means this")
			f.line("    /// size is imposed, not derived from the schema.")
			f.line("    pub const MAX_SIZE_LIMIT: usize = %d;", ms.Size)
			f.line("    pub const MAX_SIZE: usize = MAX_SIZE_LIMIT;")
		} else {
			f.line("    /// Worst-case encoded size of this message, derived from the schema.")
			f.line("    pub const MAX_SIZE: usize = %d;", ms.Size)
		}
		f.blank()
	}

	// marshal: sparse-canonical (MESSAGE_SPEC S2) -- a leaf equal to its
	// default is omitted; a sequence is opened lazily and, at field level, closed
	// with the dropping end (MESSAGE_SPEC §2).
	f.line("    /// Write this value's fields to `os` (sparse-canonical encoding).")
	f.line("    pub fn marshal(self: *const %s, os: *sofab.OStream) sofab.Error!void {", name)
	needsSelf := len(fields) > 0
	if !needsSelf {
		f.line("        _ = self;")
		f.line("        _ = os;")
	}
	for _, fld := range fields {
		g.emitMarshal(f, fld)
	}
	f.line("    }")

	g.emitIsDefault(f, name, fields)

	if isMessage {
		f.blank()
		f.line("    /// Encode into a fresh buffer allocated from `alloc`.")
		f.line("    pub fn encode(self: *const %s, alloc: std.mem.Allocator) (sofab.Error || std.mem.Allocator.Error)![]u8 {", name)
		f.line("        var sink: _EncodeSink = .{ .alloc = alloc };")
		f.line("        var scratch: [512]u8 = undefined;")
		f.line("        var os = sofab.OStream.initFlush(&scratch, 0, &sink, _EncodeSink.push);")
		f.line("        try self.marshal(&os);")
		f.line("        _ = os.flush();")
		f.line("        if (sink.failed) return error.OutOfMemory;")
		f.line("        return sink.list.toOwnedSlice(alloc);")
		f.line("    }")
		f.blank()
		f.line("    /// Decode a complete message. Zero-copy: the result borrows string and")
		f.line("    /// blob bytes from `data` (keep it alive as long as the message); array")
		f.line("    /// storage comes from `alloc` (an arena frees everything at once).")
		f.line("    /// Truncated input (the corelib's .incomplete decode Status) fails with")
		f.line("    /// error.IncompleteMessage; malformed input with error.InvalidMessage.")
		f.line("    pub fn decode(alloc: std.mem.Allocator, data: []const u8) DecodeError!%s {", name)
		f.line("        var m: %s = .{};", name)
		f.line("        var v: _dec_%s = .{ .m = &m, .alloc = alloc };", name)
		f.line("        const st = try sofab.decode(data, &v);")
		f.line("        // A scalar array over its schema count, or a wrapper-array element")
		f.line("        // id at/beyond the schema count: an index above the schema capacity")
		f.line("        // is invalid and is rejected, never clamped.")
		f.line("        if (v.inv) return error.InvalidMessage;")
		if g.msgLimitGuards(fields) {
			f.line("        // An unbounded field exceeded a receiver-configured decode limit")
			f.line("        // (max_dyn_*): a policy rejection, never a clamp.")
			f.line("        if (v.lim) return error.LimitExceeded;")
		}
		f.line("        // The bytes end inside a field or an open sequence: INCOMPLETE.")
		f.line("        // This wrapper decodes a whole buffer, so a trailing .incomplete")
		f.line("        // is a truncated message.")
		f.line("        if (st == .incomplete) return error.IncompleteMessage;")
		f.line("        return m;")
		f.line("    }")
	}
	f.line("};")
	f.blank()
}

// emitIsDefault emits the object's all-default predicate. It is the exact
// negation of what marshal writes: the object is default iff marshal would emit
// no child at all, evaluated per field and recursively (MESSAGE_SPEC S2). Every
// generated struct/union carries it so that a `count: N` wrapper array can find
// M -- one past its last non-default element -- BEFORE opening the element loop
// (S3/S5.1), which the implicit "no child was written" test cannot answer in
// time. Each term IS the field's marshal write guard (fieldNeExpr), so the
// predicate and the writer cannot drift apart: one that disagrees either drops a
// non-default element or keeps a default one.
func (g *gen) emitIsDefault(f *zfile, name string, fields []*ir.Field) {
	f.blank()
	f.line("    /// True when every field equals its declared default, compared per field")
	f.line("    /// and recursively -- i.e. when marshal would write no child at all")
	f.line("    /// (S2). A `count: N` array of this type trims its trailing")
	f.line("    /// run of default elements with it (S3/S5.1).")
	f.line("    pub fn isDefault(self: *const %s) bool {", name)
	if len(fields) == 0 {
		f.line("        _ = self;")
		f.line("        return true;")
		f.line("    }")
		return
	}
	for _, fld := range fields {
		f.line("        if (%s) return false;", g.fieldNeExpr(fld, "self."+zigIdent(fld.Name)))
	}
	f.line("        return true;")
	f.line("    }")
}

// fieldNeExpr is the boolean "this field differs from its declared default" --
// the very guard emitMarshal writes the field under. A nested sequence is framed
// lazily, so its frame survives iff the nested marshal wrote a child, which is
// exactly "the nested object is not default".
func (g *gen) fieldNeExpr(fld *ir.Field, acc string) string {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("!%s.isDefault()", acc)
	case ir.KindArray:
		return g.arrayNeExpr(fld, acc)
	}
	return g.zigLeafNe(acc, fld)
}

// arrayNeExpr is the boolean "this array field differs from its declared
// default": a native array compares against its materialized default literal, a
// wrapper array is default iff it would write no child -- i.e. iff narrowing it
// to M (elemTrimExpr) leaves nothing. Shared by emitMarshalArray and
// emitIsDefault so the write guard and the predicate are ONE expression.
func (g *gen) arrayNeExpr(fld *ir.Field, acc string) string {
	if isNativeArrayElem(fld.Elem) {
		elem := g.zigArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems)
		if _, _, ok := g.fixedNativeArray(fld); ok {
			if _, ok := g.zigNativeArrayParts(fld); ok {
				// Compare against the full (tail-padded) default literal.
				return fmt.Sprintf("!std.mem.eql(%s, %s[0..], &%s)", elem, acc, g.zigFieldDefault(fld))
			}
			// No schema default: the fixed array's default is all element zeros
			// (@splat has no inferable type inside eql).
			return fmt.Sprintf("!std.mem.allEqual(%s, %s[0..], %s)", elem, acc, zigElemZero(fld.Elem))
		}
		if parts, ok := g.zigNativeArrayParts(fld); ok {
			return fmt.Sprintf("!std.mem.eql(%s, %s, &.{ %s })", elem, acc, parts)
		}
		return fmt.Sprintf("%s.len != 0", acc)
	}
	// Wrapper array: no child is written iff every element equals the element
	// default. fld.HasCount must be threaded through unchanged -- narrowing here
	// but not in the marshal loop (or the reverse) is exactly the drift this
	// shared helper exists to prevent: it would call a dynamic [.{}] "default"
	// while the writer still frames its one empty element, omitting a field that
	// is on the wire.
	return fmt.Sprintf("%s.len != 0", g.elemTrimExpr(acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.HasCount))
}

func (g *gen) emitMarshal(f *zfile, fld *ir.Field) {
	acc := "self." + zigIdent(fld.Name)
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		write = fmt.Sprintf("try os.writeUnsigned(%d, %s);", fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		write = fmt.Sprintf("try os.writeSigned(%d, %s);", fld.ID, acc)
	case ir.KindBool:
		write = fmt.Sprintf("try os.writeBoolean(%d, %s);", fld.ID, acc)
	case ir.KindFP32:
		write = fmt.Sprintf("try os.writeFp32(%d, %s);", fld.ID, acc)
	case ir.KindFP64:
		write = fmt.Sprintf("try os.writeFp64(%d, %s);", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("try os.writeString(%d, %s);", fld.ID, acc)
	case ir.KindBlob:
		write = fmt.Sprintf("try os.writeBlob(%d, %s);", fld.ID, acc)
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence-typed
		// FIELD is no exception, so the frame is opened LAZILY -- the corelib
		// holds the header back until a child field actually appears. The nested
		// marshal omits every child that equals its default, so "no child was
		// written" IS "the object equals its declared default", evaluated per
		// field and recursively. writeSequenceEnd therefore drops an all-default
		// nested object entirely instead of emitting an empty wrapper.
		f.line("        try os.writeSequenceBeginLazy(%d);", fld.ID)
		f.line("        try %s.marshal(os);", acc)
		f.line("        try os.writeSequenceEnd();")
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Scalar/string/blob/enum/bitfield leaf: omit when equal to the default;
	// sparse encoding is canonical (MESSAGE_SPEC S2) and the decoder
	// reconstructs the omitted field from its default.
	f.line("        if (%s) %s", g.zigLeafNe(acc, fld), write)
}

func (g *gen) emitMarshalArray(f *zfile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized in the field initializer), else when empty. A composite/
	// dynamic-element array is a wrapper sequence, opened lazily: writing no
	// element drops the frame, so an empty array is omitted (MESSAGE_SPEC S2).
	if isNativeArrayElem(fld.Elem) {
		val := acc
		if _, _, ok := g.fixedNativeArray(fld); ok {
			val = acc + "[0..]"
		}
		// One expression, shared with isDefault (see arrayNeExpr).
		f.line("        if (%s) {", g.arrayNeExpr(fld, acc))
		g.marshalArray(f, "            ", fmt.Sprintf("%d", fld.ID), val, fld.Elem, fld.ElemRef, fld.ElemItems, fld.HasCount, 0)
		f.line("        }")
		return
	}
	// A `count: N` wrapper array's canonical wire stops at M, one past its last
	// non-default element (MESSAGE_SPEC S3/S5.1, "even for sequence-form
	// elements"), so fld.HasCount is threaded into the element loop, which
	// narrows to M before framing anything (see elemTrimExpr).
	//
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today: the generated
	// field initializer is the empty collection for a dynamic array and the N
	// ELEMENT defaults for a `count: N` one (which sequenceEnd's fill-to-N
	// reproduces), so absent and explicitly-empty denote the same value either
	// way. If that gap is ever closed, this call needs a guard
	// -- `if (value != default) { ... writeSequenceEndKeep(); }` -- so that a
	// value differing from a non-empty default still reaches the wire as the
	// empty wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC S2, S3).
	g.marshalArray(f, "        ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.HasCount, 0)
}

// elemTrimExpr narrows a wrapper array to the M its canonical wire carries: one
// past the last element differing from the element default (MESSAGE_SPEC S3/S5.1,
// which says "even for sequence-form elements"). Only a declared `count: N` array
// is fixed-length and may be narrowed -- a dynamic array has no N to refill from,
// so a trailing default ELEMENT is significant and stays framed. Interior
// all-default elements are never dropped by this: element presence carries the
// length, so only the trailing run goes. Both the marshal loop and isDefault run
// off this one expression, so the writer and the predicate cannot disagree.
//
// A string/blob element is a leaf the writer omits individually when it equals
// the element default, so narrowing a FIXED array's run does not change the
// bytes -- it exists so the predicate is computed from the very expression the
// writer loops over. On a DYNAMIC array the trim is dropped instead: its final
// element is always written (see lastElemGuard), so a trailing default leaf is
// on the wire and trimming here would make isDefault call a dynamic [""]
// "default" and omit a field the marshal loop writes.
func (g *gen) elemTrimExpr(val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, fixed bool) string {
	switch elem {
	case ir.KindString, ir.KindBlob:
		if !fixed {
			return val
		}
		// Both are []const u8 in Zig, so one slice-shaped trim covers them.
		return fmt.Sprintf("_trimSlices(u8, %s)", val)
	case ir.KindStruct, ir.KindUnion:
		if !fixed {
			return val
		}
		return fmt.Sprintf("_trimObjs(%s, %s)", g.typeName(ref.Key), val)
	case ir.KindArray:
		if !fixed {
			return val
		}
		return fmt.Sprintf("_trimSlices(%s, %s)", g.zigArrayElem(items.Elem, items.ElemRef, items.ElemItems), val)
	}
	return val
}

// lastElemGuard is the "or this is the last element" disjunct that keeps a
// DYNAMIC wrapper array's final element on the wire whatever its value
// (MESSAGE_SPEC S2, "the last element of a dynamic array is always present").
// Such an array recovers its length as highest-present-id + 1 (S5.1), so the
// element at the highest index is the only one whose PRESENCE carries the
// length: dropping it would encode ["a", ""] exactly like ["a"] and decode one
// element short. Sequence-form elements never needed this -- they are framed
// unconditionally -- so this closes the gap on the leaf side and holds both
// element kinds to one standard. A fixed-count array needs none of it: its
// length is N whatever the wire carries, which is why it elides the entire
// trailing default run instead (S3/S5.1), so the guard is omitted there and the
// trailing run collapses as before.
//
// val is the very slice the loop runs over, so `.len - 1` cannot underflow --
// the guard is only ever evaluated from inside the loop body, i.e. len >= 1.
func lastElemGuard(iv, val string, fixed bool) string {
	if fixed {
		return ""
	}
	return fmt.Sprintf(" or %s == %s.len - 1", iv, val)
}

// trimExpr wraps a native array expression in the trailing-default-run trim that
// a fixed-count array's canonical encoding requires (MESSAGE_SPEC S3): only the
// elements up to the last non-default one are emitted, and the decoder rebuilds
// the trailing default run from the schema count. Only a declared `count: N`
// array is fixed-length; a dynamic (count-less) array has no N to refill from,
// so a trailing default element is significant and stays.
func trimExpr(val string, fixed bool) string {
	if !fixed {
		return val
	}
	return fmt.Sprintf("sofab.arrays.trimTail(%s)", val)
}

// marshalArray writes the array val (a slice-like expression) as field idExpr.
// Numeric/enum/bitfield elements use the native array wire type (numeric/enum
// by signedness, bitfield -> unsigned); boolean lowers to a 0/1 unsigned array
// via its byte representation; string/blob/struct/union/array elements lower
// to a wrapper sequence whose child ids are the 0-based index (MESSAGE_SPEC
// S5.1). Recurses for nested arrays, depth-suffixing loop vars.
//
// fixed marks val as a `count: N` array FIELD, whose canonical wire drops the
// trailing run of default elements (MESSAGE_SPEC S3, see trimExpr).
func (g *gen) marshalArray(f *zfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, fixed bool, depth int) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	// MESSAGE_SPEC S2: every sequence is opened lazily; the CLOSER decides whether
	// a contentless one survives, and it is a static property of the position in
	// the schema, never of the value. A wrapper array is a sequence-typed FIELD, so
	// at depth 0 it closes with the dropping writeSequenceEnd -- an empty array is
	// omitted and absence reconstructs it. A nested row (depth > 0) is an array
	// ELEMENT, and element presence is what carries a dynamic array's length
	// (S5.1), so it closes with writeSequenceEndKeep: dropping an all-default row
	// would change the decoded length, not merely the bytes.
	seqEnd := "writeSequenceEnd"
	if depth > 0 {
		seqEnd = "writeSequenceEndKeep"
	}
	// A wrapper element loop runs to M, not to len (see elemTrimExpr); a native
	// array trims its own trailing default run at the wire level (trimExpr).
	elemTrim := g.elemTrimExpr(val, elem, ref, items, fixed)
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		// bitfield backing is an unsigned int, so it writes directly.
		f.line("%stry os.writeArrayUnsigned(%s, %s);", ind, idExpr, trimExpr(val, fixed))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		// enum backing is a signed int, so it writes directly.
		f.line("%stry os.writeArraySigned(%s, %s);", ind, idExpr, trimExpr(val, fixed))
	case ir.KindBool:
		// bool has no array wire type; it lowers to a 0/1 unsigned array. A
		// Zig bool is one byte holding exactly 0 or 1, so the slice's byte
		// view is already the element list -- no temporary, no allocator.
		// Trimming the 0/1 byte image is equivalent to trimming the bools
		// (false <-> 0).
		f.line("%stry os.writeArrayUnsigned(%s, %s);", ind, idExpr, trimExpr(fmt.Sprintf("std.mem.sliceAsBytes(%s)", val), fixed))
	case ir.KindFP32:
		f.line("%stry os.writeArrayFp32(%s, %s);", ind, idExpr, trimExpr(val, fixed))
	case ir.KindFP64:
		f.line("%stry os.writeArrayFp64(%s, %s);", ind, idExpr, trimExpr(val, fixed))
	case ir.KindString:
		// A string element is a leaf: omit it when equal to the element
		// default (empty), leaving an id gap the decoder restores -- except at
		// the one position whose presence carries the length, see lastElemGuard.
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |%s, %s| {", ind, elemTrim, ev, iv)
		f.line("%s    if (%s.len != 0%s) try os.writeString(@intCast(%s), %s);", ind, ev, lastElemGuard(iv, elemTrim, fixed), iv, ev)
		f.line("%s}", ind)
		f.line("%stry os.%s();", ind, seqEnd)
	case ir.KindBlob:
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |%s, %s| {", ind, elemTrim, ev, iv)
		f.line("%s    if (%s.len != 0%s) try os.writeBlob(@intCast(%s), %s);", ind, ev, lastElemGuard(iv, elemTrim, fixed), iv, ev)
		f.line("%s}", ind)
		f.line("%stry os.%s();", ind, seqEnd)
	case ir.KindStruct, ir.KindUnion:
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |*%s, %s| {", ind, elemTrim, ev, iv)
		// An INTERIOR element is framed unconditionally: dropping it would leave an
		// id gap and change the decoded length, not just the bytes (S5.1). The
		// TRAILING all-default run is already gone -- the loop runs to M, not to
		// len (S3/S5.1) -- and M == 0 writes no child at all, so the lazily-opened
		// wrapper is dropped and the field is omitted (S2).
		f.line("%s    try os.writeSequenceBeginLazy(@intCast(%s));", ind, iv)
		f.line("%s    try %s.marshal(os);", ind, ev)
		f.line("%s    try os.writeSequenceEndKeep();", ind)
		f.line("%s}", ind)
		f.line("%stry os.%s();", ind, seqEnd)
	case ir.KindArray:
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |%s, %s| {", ind, elemTrim, ev, iv)
		// A nested row is a wrapper-sequence element, not a `count: N` field:
		// the trailing-default-run rule is scoped to fields (MESSAGE_SPEC S3),
		// so rows are never trimmed. (A nested array is always a slice anyway --
		// only a direct field lowers to a fixed [N]T.)
		g.marshalArray(f, ind+"    ", fmt.Sprintf("@intCast(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, false, depth+1)
		f.line("%s}", ind)
		f.line("%stry os.%s();", ind, seqEnd)
	}
}

// emitSupport writes the module-level helpers shared by every message: the
// encode flush sink and the small generic decode stores. Zig analyzes private
// declarations lazily, so helpers a given schema never references cost
// nothing.
//
// dynAlloc selects the initial-allocation strategy for a slice-backed native
// array. The wire count is untrusted, so when any message decodes one the eager
// allocation is capped here and grown as elements actually arrive
// (sofab.arrays.putGrowing); otherwise the exact count is allocated up front.
func (g *gen) emitSupport(f *zfile, dynAlloc bool) {
	f.line("// --- shared encode/decode support -------------------------------------------")
	f.blank()
	f.line("/// Flush sink behind encode(): drains the OStream scratch buffer into a")
	f.line("/// growable byte list.")
	f.line("const _EncodeSink = struct {")
	f.line("    alloc: std.mem.Allocator,")
	f.line("    list: std.ArrayList(u8) = .empty,")
	f.line("    failed: bool = false,")
	f.line("    fn push(ctx: ?*anyopaque, data: []const u8) void {")
	f.line("        const self: *_EncodeSink = @ptrCast(@alignCast(ctx.?));")
	f.line("        self.list.appendSlice(self.alloc, data) catch {")
	f.line("            self.failed = true;")
	f.line("        };")
	f.line("    }")
	f.line("};")
	f.blank()
	f.line("/// Mutable pointer to element `i` of a decode-allocated wrapper array.")
	f.line("///")
	f.line("/// The element id IS the array index (S5.1), so sequenceBegin")
	f.line("/// grows the destination to id + 1 -- default-filling the gaps left by omitted")
	f.line("/// elements -- records the id, and every child store then lands HERE, at that")
	f.line("/// index. Appending instead would shorten the array by the size of any interior")
	f.line("/// id gap, and would decode a REOPENED element id as a second element instead of")
	f.line("/// merging into the first (S7.4). It is the object-element twin of what")
	f.line("/// sofab.arrays.setElem does for a string/blob element.")
	f.line("fn _at(s: anytype, i: usize) *std.meta.Elem(@TypeOf(s)) {")
	f.line("    return @constCast(&s[i]);")
	f.line("}")
	f.blank()
	f.line("/// Narrow a `count: N` wrapper array of struct/union elements to M -- one past")
	f.line("/// the last element differing from the element default -- which is what its")
	f.line("/// canonical wire carries (S3/S5.1, \"even for sequence-form")
	f.line("/// elements\"). Only the TRAILING run is dropped: an interior all-default element")
	f.line("/// keeps its frame, because element presence is what carries the array's length.")
	f.line("/// M == 0 writes no child at all, so the lazily-opened wrapper is dropped by")
	f.line("/// writeSequenceEnd and the whole field is omitted (S2). A dynamic (count-less)")
	f.line("/// array has no N to refill from and is never narrowed.")
	f.line("fn _trimObjs(comptime T: type, a: []const T) []const T {")
	f.line("    var m = a.len;")
	f.line("    while (m > 0 and a[m - 1].isDefault()) : (m -= 1) {}")
	f.line("    return a[0..m];")
	f.line("}")
	f.blank()
	f.line("/// _trimObjs for the slice-shaped element kinds -- string, blob and nested rows")
	f.line("/// -- whose element default is the empty slice. A string/blob element is a leaf")
	f.line("/// the writer already omits individually, so narrowing its trailing run does not")
	f.line("/// change the bytes: it exists so the all-default predicate is computed from the")
	f.line("/// very expression the writer loops over, and cannot drift away from it.")
	f.line("fn _trimSlices(comptime T: type, a: []const []const T) []const []const T {")
	f.line("    var m = a.len;")
	f.line("    while (m > 0 and a[m - 1].len == 0) : (m -= 1) {}")
	f.line("    return a[0..m];")
	f.line("}")
	if dynAlloc {
		f.blank()
		f.line("/// Initial storage for a native array announcing n wire elements. The count")
		f.line("/// is untrusted until the elements actually arrive, so the eager allocation")
		f.line("/// is capped here and sofab.arrays.putGrowing extends it on demand -- a")
		f.line("/// lying count cannot force a huge allocation. On allocation failure the")
		f.line("/// array decodes as empty.")
		f.line("fn _allocN(comptime T: type, a: std.mem.Allocator, n: usize) []const T {")
		f.line("    return sofab.arrays.allocN(T, a, @min(n, 1024));")
		f.line("}")
	} else {
		f.blank()
		f.line("/// Native-array destination of exactly the announced wire count.")
		f.line("fn _allocN(comptime T: type, a: std.mem.Allocator, n: usize) []const T {")
		f.line("    return sofab.arrays.allocN(T, a, n);")
		f.line("}")
	}
}
