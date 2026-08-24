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
	// msgLim is the `lim` flag's liveness for the decoder being emitted right
	// now, as msgLimitGuards decided it. The flag is declared per MESSAGE (a
	// message with no unbounded field must not carry one), so every guard that
	// sets it has to take the same decision, and the deepest of them --
	// overIndexCond -- has no path to the field list. Set by emitDecoder.
	msgLim bool
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
// resolved against the schema: every cap is always set — the target carries a
// finite default that the config key only overrides (§9.5, generator#385) — so
// an entry is active exactly when the schema has an unbounded field of that
// kind; otherwise the cap is inert and no plumbing is emitted. Enforcement is per-field in
// the generated decoder (schema-bounded fields keep only their generator#100
// guard), so the configured value is emitted as-is, never raised to a schema
// bound.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits resolves the max_dyn_* caps over the target's finite defaults
// and against the schema's bounds (see limitSet).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	d := generator.ServerDynLimits.Resolve(cfg)
	var l limitSet
	if b.HasDynArray {
		l.arrayCount, l.arrayHas = d.ArrayCount, true
	}
	if b.HasDynString {
		l.stringLen, l.stringHas = d.StringLen, true
	}
	if b.HasDynBlob {
		l.blobLen, l.blobHas = d.BlobLen, true
	}
	return l
}

type zfile struct{ b strings.Builder }

func (f *zfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *zfile) blank() { f.b.WriteByte('\n') }

// bytes closes the file on exactly one newline. Sections separate themselves
// with a trailing blank line, so whichever happens to come last would otherwise
// leave a blank line at EOF -- which `zig fmt` removes, and a `zig fmt --check`
// in a user's own tree then fails on.
func (f *zfile) bytes() []byte {
	return []byte(strings.TrimRight(f.b.String(), "\n") + "\n")
}

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
// zigStorage reads the storage back off the member type: a count-bounded native
// array lowers to sofab.FixedArray(T, N) (the capacity is in the type), while
// strings, blobs and wrapper arrays stay slices (it is not).
func zigStorage(zigType string) generator.FieldStorage {
	if strings.HasPrefix(zigType, "sofab.FixedArray(") {
		return generator.StorageFixed
	}
	return generator.StorageDynamic
}

func fieldDoc(fld *ir.Field, note string) string {
	var doc string
	switch {
	case fld.Description != "" && fld.Unit != "":
		doc = fld.Description + " (unit: " + fld.Unit + ")"
	case fld.Description != "":
		doc = fld.Description
	case fld.Unit != "":
		doc = "(unit: " + fld.Unit + ")"
	}
	doc = generator.AppendDoc(doc, note)
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
		typ := g.zigType(fld)
		f.emitDoc("    ", fieldDoc(fld, generator.BoundNote(fld, zigStorage(typ))))
		f.line("    %s: %s = %s,", zigIdent(fld.Name), typ, g.zigFieldDefault(fld))
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
	f.line("    pub fn serialize(self: *const %s, os: *sofab.OStream) sofab.Error!void {", name)
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
		f.line("        var sink: sofab.CollectingSink = .{ .alloc = alloc };")
		f.line("        defer sink.deinit();")
		f.line("        var scratch: [512]u8 = undefined;")
		f.line("        var os = sofab.OStream.initFlush(&scratch, 0, &sink, sofab.CollectingSink.push);")
		f.line("        try self.serialize(&os);")
		f.line("        _ = os.flush();")
		f.line("        return sink.toOwnedSlice();")
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
		g.emitStreamDecoder(f, name, fields)
	}
	f.line("};")
	f.blank()
}

// emitIsDefault emits the object's all-default predicate. It is the exact
// negation of what marshal writes: the object is default iff marshal would emit
// no child at all, evaluated per field and recursively (MESSAGE_SPEC §2). Each
// term IS the field's marshal write guard (fieldNeExpr), so the predicate and
// the writer cannot drift apart: one that disagrees either drops a non-default
// element or keeps a default one.
func (g *gen) emitIsDefault(f *zfile, name string, fields []*ir.Field) {
	f.blank()
	f.line("    /// True when every field equals its declared default, compared per field")
	f.line("    /// and recursively -- i.e. when serialize would write no child at all (S2).")
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
// default", shared by emitMarshalArray and emitIsDefault so the write guard and
// the predicate are ONE expression.
//
// A declared `count: N` takes no part in the test. `count` is a CAPACITY, never
// a length (MESSAGE_SPEC §3), so the value is compared against the declared
// default exactly as written -- neither side padded to N -- and against the
// empty collection when no default is declared. A count:N array is therefore
// default only when it is EMPTY: an all-zero N-element value is a length-N
// array, which differs from the empty one and stays on the wire.
func (g *gen) arrayNeExpr(fld *ir.Field, acc string) string {
	if isNativeArrayElem(fld.Elem) {
		if parts, ok := g.zigNativeArrayParts(fld); ok {
			elem := g.zigArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems)
			return fmt.Sprintf("!std.mem.eql(%s, %s, &.{ %s })", elem, g.arrayValExpr(fld, acc), parts)
		}
		// A count:N field keeps its length behind an accessor -- the inline
		// capacity past it is not part of the value; a dynamic one is a slice and
		// carries its own `.len`.
		if _, _, ok := g.fixedNativeArray(fld); ok {
			return fmt.Sprintf("%s.len() != 0", acc)
		}
		return fmt.Sprintf("%s.len != 0", acc)
	}
	// Wrapper array: the writer emits a child for every element it holds, because
	// the LAST element is written whatever its value (§2) -- so "no child is
	// written" is exactly "the array is empty", and the two cannot drift apart.
	return fmt.Sprintf("%s.len != 0", acc)
}

// arrayValExpr is the array field's VALUE as a slice expression. A dynamic array
// is already one; a count:N native array is inline sofab.FixedArray(T, N) storage,
// whose value is its first `.len` elements -- `count` is a capacity, so the
// spare tail is not part of the value and never reaches the wire (§3).
func (g *gen) arrayValExpr(fld *ir.Field, acc string) string {
	if _, _, ok := g.fixedNativeArray(fld); ok {
		return acc + ".slice()"
	}
	return acc
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
		f.line("        try %s.serialize(os);", acc)
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
	// element drops the frame, so an empty array is omitted (MESSAGE_SPEC §2).
	//
	// A declared `count: N` takes no part in either test, and nothing is elided
	// from either form: `count` is a capacity, never a length (§3), so the wire
	// count IS the array's length.
	if isNativeArrayElem(fld.Elem) {
		// One expression, shared with isDefault (see arrayNeExpr).
		f.line("        if (%s) {", g.arrayNeExpr(fld, acc))
		g.marshalArray(f, "            ", fmt.Sprintf("%d", fld.ID), g.arrayValExpr(fld, acc), fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
		f.line("        }")
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today -- the
	// generated field initializer is the empty collection with or without a
	// `count` -- so absent and explicitly-empty denote the same value. If that gap
	// is ever closed, this call needs a guard -- `if (value != default) { ...
	// writeSequenceEndKeep(); }` -- so that a value differing from a non-empty
	// default still reaches the wire as the empty wrapper, the only encoding of
	// "explicitly empty" (MESSAGE_SPEC §2, §3).
	g.marshalArray(f, "        ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over the value val.
//
// It is the whole of the positional half of MESSAGE_SPEC §2's element rule. A
// wrapper array carries no length field: its decoded length is *highest present
// id + 1* (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length, and nothing that carries the length may be
// elided. Everything before it may be: an interior element equal to the element
// default is indistinguishable from an absent one, because the decoder restores
// an absent id from that same default. Hence: interior sparse, last always
// written.
//
// A declared `count: N` changes nothing here. N is a capacity, not a length
// (§3), so it can never restore an elided tail -- the same test applies with or
// without one.
//
// val is the very slice the loop runs over and the test is only ever evaluated
// from inside the loop body (len >= 1), so `.len - 1` cannot underflow.
func lastElemExpr(iv, val string) string {
	return fmt.Sprintf("%s == %s.len - 1", iv, val)
}

// emitSeqEnd closes the wrapper sequence opened at ind, choosing between the two
// closers the corelib offers. Every sequence is opened LAZILY (the corelib holds
// the header back until a child is written), so the closer alone decides whether
// a contentless one survives: writeSequenceEnd drops it, writeSequenceEndKeep
// forces the empty frame out.
//
// keepIf is the condition under which an empty frame must survive:
//   - "" -- never. A sequence-typed FIELD (a struct/union field, an array
//     wrapper): an all-default one is omitted and absence reconstructs it (§2).
//   - a lastElemExpr -- a sequence-form array ELEMENT, kept only at the array's
//     last index. In the interior it is dropped and leaves an id GAP, which is
//     what makes an all-default element sparse like any other default value.
//     Note this is decided from the position in the VALUE, at run time; the
//     schema cannot answer it.
func emitSeqEnd(f *zfile, ind, keepIf string) {
	if keepIf == "" {
		f.line("%stry os.writeSequenceEnd();", ind)
		return
	}
	f.line("%sif (%s) {", ind, keepIf)
	f.line("%s    try os.writeSequenceEndKeep();", ind)
	f.line("%s} else {", ind)
	f.line("%s    try os.writeSequenceEnd();", ind)
	f.line("%s}", ind)
}

// marshalArray writes the array val (a slice-like expression) as field idExpr.
// Numeric/enum/bitfield elements use the native array wire type (numeric/enum
// by signedness, bitfield -> unsigned); boolean lowers to a 0/1 unsigned array
// via its byte representation; string/blob/struct/union/array elements lower
// to a wrapper sequence whose child ids are the 0-based index (MESSAGE_SPEC
// §5.1). Recurses for nested arrays, depth-suffixing loop vars.
//
// Every element the value holds is written -- no trailing run is elided, of
// either element kind, because the wire count IS the array's length (§3) and the
// highest wrapper id IS its last index (§5.1). What the interior may drop is a
// value that is indistinguishable from absence, and only that.
//
// keepIf is the closer this call's own wrapper takes (see emitSeqEnd); the
// native element kinds open no sequence and ignore it.
func (g *gen) marshalArray(f *zfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int, keepIf string) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		// bitfield backing is an unsigned int, so it writes directly.
		f.line("%stry os.writeArrayUnsigned(%s, %s);", ind, idExpr, val)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		// enum backing is a signed int, so it writes directly.
		f.line("%stry os.writeArraySigned(%s, %s);", ind, idExpr, val)
	case ir.KindBool:
		// bool has no array wire type; it lowers to a 0/1 unsigned array. A
		// Zig bool is one byte holding exactly 0 or 1, so the slice's byte
		// view is already the element list -- no temporary, no allocator.
		f.line("%stry os.writeArrayUnsigned(%s, std.mem.sliceAsBytes(%s));", ind, idExpr, val)
	case ir.KindFP32:
		f.line("%stry os.writeArrayFp32(%s, %s);", ind, idExpr, val)
	case ir.KindFP64:
		f.line("%stry os.writeArrayFp64(%s, %s);", ind, idExpr, val)
	case ir.KindString:
		// A string element is a leaf: in the array's INTERIOR it is omitted when it
		// equals the element default (empty), leaving an id gap the decoder
		// restores from that same default -- the ordinary sparse-field rule of
		// MESSAGE_SPEC §2, applied to an element. At the LAST index it is written
		// whatever its value: see lastElemExpr.
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |%s, %s| {", ind, val, ev, iv)
		f.line("%s    if (%s.len != 0 or %s) try os.writeString(@intCast(%s), %s);", ind, ev, lastElemExpr(iv, val), iv, ev)
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindBlob:
		// A blob element is a leaf, exactly like the string element above.
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |%s, %s| {", ind, val, ev, iv)
		f.line("%s    if (%s.len != 0 or %s) try os.writeBlob(@intCast(%s), %s);", ind, ev, lastElemExpr(iv, val), iv, ev)
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above --
		// one rule for both kinds -- and the lazily-held frame is where it is
		// applied. The nested marshal writes no child exactly when the element
		// equals its declared default, so the CLOSER alone decides: the dropping
		// one in the interior, where an all-default element vanishes into an id
		// gap; the keeping one at the last index, where it survives as an empty
		// frame because that presence is what fixes the array's length.
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |*%s, %s| {", ind, val, ev, iv)
		f.line("%s    try os.writeSequenceBeginLazy(@intCast(%s));", ind, iv)
		f.line("%s    try %s.serialize(os);", ind, ev)
		emitSeqEnd(f, ind+"    ", lastElemExpr(iv, val))
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindArray:
		f.line("%stry os.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (%s, 0..) |%s, %s| {", ind, val, ev, iv)
		if isNativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its
			// own, so the rule lands on the WRITE rather than on a closer: an
			// interior row equal to the element default (the empty row) is not
			// written at all, and the last row always is.
			f.line("%s    if (%s.len != 0 or %s) {", ind, ev, lastElemExpr(iv, val))
			g.marshalArray(f, ind+"        ", fmt.Sprintf("@intCast(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, "")
			f.line("%s    }", ind)
		} else {
			// A wrapper row has its own frame, so it takes the closer instead -- the
			// same interior/last choice, expressed the same way as for a struct
			// element above.
			g.marshalArray(f, ind+"    ", fmt.Sprintf("@intCast(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, lastElemExpr(iv, val))
		}
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	}
}

// emitStreamDecoder writes the public incremental decoder: a handle on the
// corelib's resumable IStream plus the visitor state that has to survive
// between chunks. The corelib already suspends and resumes at any byte
// boundary; what was missing was a public handle, since `_dec_<Msg>` is not
// `pub` (PLAN S5.6).
//
// The DESTINATION is the caller's, not a field of the Decoder. A Decoder that
// owned its message would have to point its visitor at its own field, and Zig
// moves structs by value -- returning one from a factory would leave that
// pointer dangling. Taking `out: *Msg` keeps the decoder trivially movable.
func (g *gen) emitStreamDecoder(f *zfile, name string, fields []*ir.Field) {
	f.blank()
	f.line("    /// Incremental decoder: hold one and feed the message as bytes arrive,")
	f.line("    /// instead of buffering it whole first.")
	f.line("    ///")
	f.line("    /// The wire format has no end marker at the top level -- a message ends")
	f.line("    /// where its bytes end -- so a feed cannot report that the MESSAGE is")
	f.line("    /// complete, only that the bytes handed in ended on a field boundary")
	f.line("    /// (.complete) or mid-field (.incomplete). Neither is a failure")
	f.line("    /// mid-stream; the caller's own framing decides when the input is over,")
	f.line("    /// and `finish` then gives the verdict for the message as a whole.")
	f.line("    ///")
	f.line("    /// STORAGE: unlike decode(), this path COPIES every string and blob into")
	f.line("    /// `alloc` -- a fed chunk does not have to outlive the message, and the")
	f.line("    /// decoded value never points into a buffer the caller may reuse.")
	f.line("    ///")
	f.line("    /// Borrowing is not available here, and not merely declined: a payload")
	f.line("    /// stitched across a chunk boundary is completed inside the corelib's own")
	f.line("    /// reusable carry buffer, and the delivered slice then points THERE, not")
	f.line("    /// into any chunk the caller passed. Nothing in the callback distinguishes")
	f.line("    /// that slice from one in the caller's buffer, and the next stitched item")
	f.line("    /// overwrites it. decode() is unaffected -- it hands the corelib one whole")
	f.line("    /// buffer, which is never stitched, so it keeps borrowing.")
	f.line("    pub const Decoder = struct {")
	f.line("        is: sofab.IStream = sofab.IStream.init(),")
	f.line("        v: _dec_%s,", name)
	f.blank()
	f.line("        /// Feed the next chunk, of any size. `.complete` means the bytes")
	f.line("        /// ended on a field boundary, `.incomplete` mid-field -- neither")
	f.line("        /// answers whether the MESSAGE is done.")
	f.line("        pub fn feed(self: *Decoder, chunk: []const u8) DecodeError!sofab.Status {")
	f.line("            const st = try self.is.feed(chunk, &self.v);")
	f.line("            if (self.v.inv) return error.InvalidMessage;")
	if g.msgLimitGuards(fields) {
		f.line("            if (self.v.lim) return error.LimitExceeded;")
	}
	f.line("            return st;")
	f.line("        }")
	f.blank()
	f.line("        /// The outcome for everything fed so far, without feeding more.")
	f.line("        pub fn status(self: *const Decoder) sofab.Status {")
	f.line("            return self.is.status();")
	f.line("        }")
	f.blank()
	f.line("        /// Declare end-of-input. Fails a stream that ended mid-field rather")
	f.line("        /// than leaving the destination half-filled; the destination is the")
	f.line("        /// caller's either way.")
	f.line("        pub fn finish(self: *const Decoder) DecodeError!void {")
	f.line("            if (self.is.status() == .incomplete) return error.IncompleteMessage;")
	f.line("        }")
	f.line("    };")
	f.blank()
	f.line("    /// An incremental decoder filling `out`: hold it and feed chunks as they")
	f.line("    /// arrive, instead of buffering the whole message first.")
	f.line("    pub fn decoder(out: *%s, alloc: std.mem.Allocator) Decoder {", name)
	// `.own = true` is what separates this path from decode(): every payload is
	// copied, because a slice the corelib delivers may point into its carry
	// buffer rather than into the caller's chunk (generator#295).
	f.line("        return .{ .v = .{ .m = out, .alloc = alloc, .own = true } };")
	f.line("    }")
}
