package zig

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// frameKind classifies a sequence container reachable from a message.
type frameKind int

const (
	fkStruct       frameKind = iota // root / struct / union / struct-array element: named fields
	fkSeqArr                        // array of string/blob: elements placed in string()/blob()
	fkStructArr                     // array of struct/union: per-element sequence grows and descends
	fkNestedNative                  // array of native array: arrayBegin appends an inner slice
	fkArrArr                        // array of (string/blob/struct/nested) array: per-element sequence descends
)

// frame is one sequence container reachable from a message. loc is the _Loc
// variant; path is the Zig lvalue expression (e.g. "self.m.somestruct.deep" or
// "_last(self.m.points).tags").
type frame struct {
	loc      string
	path     string
	kind     frameKind
	fields   []*ir.Field // fkStruct
	elemLoc  string      // fkStructArr, fkArrArr: location to descend to on a per-element sequenceBegin
	elemKind ir.Kind     // fkSeqArr: string/blob element; fkNestedNative: inner native element kind
	elemRef  *ir.TypeRef // fkNestedNative: enum/bitfield backing type
	elemType string      // fkStructArr/fkArrArr/fkNestedNative: Zig type of one element (for _grow)
	elemFill string      // fkStructArr/fkArrArr/fkNestedNative: fill literal for _grow
}

// frames walks a message and returns every sequence container, root first.
func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walkFields func(loc, path string, fields []*ir.Field)
	var addArray func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)

	walkFields = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, kind: fkStruct, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				cl := loc + "_" + fld.Name
				walkFields(cl, path+"."+zigIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+zigIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems)
			}
		}
	}

	// addArray builds the frame(s) for a wrapper-sequence array whose slice is
	// at (loc, path) and whose element is (elem, ref, items).
	addArray = func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{loc: loc, path: path, kind: fkSeqArr, elemKind: elem})
		case ir.KindStruct, ir.KindUnion:
			el := loc + "_e"
			out = append(out, frame{
				loc: loc, path: path, kind: fkStructArr, elemLoc: el,
				elemType: g.typeName(ref.Key), elemFill: ".{}",
			})
			walkFields(el, "_last("+path+")", ref.Target.Fields)
		case ir.KindArray:
			// The element is an inner array (items). A native inner array is
			// handled by a single wrapper frame (arrayBegin appends a fresh
			// inner slice, elements land in the last one); a wrapper inner
			// array descends recursively.
			inner := g.zigArrayElem(items.Elem, items.ElemRef, items.ElemItems)
			if isNativeArrayElem(items.Elem) {
				out = append(out, frame{
					loc: loc, path: path, kind: fkNestedNative,
					elemKind: items.Elem, elemRef: items.ElemRef,
					elemType: "[]const " + inner, elemFill: "&.{}",
				})
			} else {
				el := loc + "_e"
				out = append(out, frame{
					loc: loc, path: path, kind: fkArrArr, elemLoc: el,
					elemType: "[]const " + inner, elemFill: "&.{}",
				})
				addArray(el, "_last("+path+").*", items.Elem, items.ElemRef, items.ElemItems)
			}
		}
	}

	walkFields("root", "self.m", m.Fields)
	return out
}

// visitorUse records which visitor callbacks a message actually needs; the
// corelib's comptime duck typing turns a missing method into an automatic
// skip, so only used callbacks are emitted.
type visitorUse struct {
	unsigned, signed, fp32, fp64, str, blob, scalarArray, sequence bool
}

func visitorUseOf(fs []frame) visitorUse {
	u := visitorUse{}
	if len(fs) > 1 { // any nested struct/union or wrapper-array frame
		u.sequence = true
	}
	for _, fr := range fs {
		switch fr.kind {
		case fkSeqArr:
			u.str = u.str || fr.elemKind == ir.KindString
			u.blob = u.blob || fr.elemKind == ir.KindBlob
		case fkNestedNative:
			u.scalarArray = true
			switch {
			case fr.elemKind == ir.KindFP32:
				u.fp32 = true
			case fr.elemKind == ir.KindFP64:
				u.fp64 = true
			case isSignedElem(fr.elemKind) || fr.elemKind == ir.KindEnum:
				u.signed = true
			default: // unsigned numeric, bool, bitfield
				u.unsigned = true
			}
		}
		for _, fld := range fr.fields {
			switch fld.Kind {
			case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBool, ir.KindBitfield:
				u.unsigned = true
			case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
				u.signed = true
			case ir.KindFP32:
				u.fp32 = true
			case ir.KindFP64:
				u.fp64 = true
			case ir.KindString:
				u.str = true
			case ir.KindBlob:
				u.blob = true
			case ir.KindArray:
				switch {
				case fld.Elem == ir.KindString:
					u.str = true
				case fld.Elem == ir.KindBlob:
					u.blob = true
				case fld.Elem == ir.KindFP32:
					u.fp32, u.scalarArray = true, true
				case fld.Elem == ir.KindFP64:
					u.fp64, u.scalarArray = true, true
				case fld.Elem == ir.KindStruct || fld.Elem == ir.KindUnion || fld.Elem == ir.KindArray:
					// wrapper element -- handled by its own sub-frame
				case isSignedElem(fld.Elem) || fld.Elem == ir.KindEnum:
					u.signed, u.scalarArray = true, true
				default: // unsigned numeric, bool, bitfield
					u.unsigned, u.scalarArray = true, true
				}
			}
		}
	}
	return u
}

// dynNativeArray reports whether a field is a dynamic (count-less) native
// array, which needs an arrayBegin allocation of exactly the wire count.
func (g *gen) dynNativeArray(f *ir.Field) bool {
	return f.Kind == ir.KindArray && isNativeArrayElem(f.Elem) && !f.HasCount
}

// putTarget is the _put destination for a native array field: a pointer for a
// fixed [N]T (mutable through the message), the slice value for a dynamic
// decode-allocated array (_put const-casts the elements).
func (g *gen) putTarget(fr frame, fld *ir.Field) string {
	acc := fr.path + "." + zigIdent(fld.Name)
	if _, _, ok := g.fixedNativeArray(fld); ok {
		return "&" + acc
	}
	return acc
}

// putCall renders the element store for a direct native array field: the
// capacity-checked _putc for a fixed [N]T — an over-count element flags the
// message INVALID per MESSAGE_SPEC 3+7 (generator#100) — or plain _put for a
// dynamic (count-less) slice, which keeps every wire element.
func (g *gen) putCall(fr frame, fld *ir.Field, val string) string {
	if _, _, ok := g.fixedNativeArray(fld); ok {
		return fmt.Sprintf("_putc(%s, &self.ai, %s, &self.inv)", g.putTarget(fr, fld), val)
	}
	return fmt.Sprintf("_put(%s, &self.ai, %s)", g.putTarget(fr, fld), val)
}

// storeCast renders the visitor value expression for a numeric destination
// type: u64/i64 pass through, narrower integers truncate (the declared width
// is a storage hint; the wire value is a single varint).
func storeCast(dest string, value string) string {
	if dest == "u64" || dest == "i64" {
		return value
	}
	return "@truncate(" + value + ")"
}

func (g *gen) emitDecoder(f *zfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	use := visitorUseOf(fs)

	f.line("/// Flat-visitor decoder for %s: a (location, id) state machine over the", name)
	f.line("/// corelib's streaming callbacks, with a bounded location stack.")
	f.line("const _dec_%s = struct {", name)
	f.line("    m: *%s,", name)
	f.line("    alloc: std.mem.Allocator,")
	if use.sequence {
		// The corelib rejects nesting deeper than MAX_DEPTH (255), so 256
		// slots always suffice -- no heap, no overflow handling.
		f.line("    stack: [256]_Loc = undefined,")
		f.line("    sp: usize = 0,")
	}
	f.line("    cur: _Loc = .root,")
	// Sticky malformed-message flag: a fixed native array received more
	// elements than its schema count (generator#100); decode() then rejects
	// with error.InvalidMessage. Always present so decode() can check it.
	f.line("    inv: bool = false, // a scalar array overflowed its schema count -> INVALID")
	if use.scalarArray {
		f.line("    ai: usize = 0, // index into the native array currently being filled")
	}
	f.blank()
	f.line("    const _Loc = enum {")
	for _, fr := range fs {
		f.line("        %s,", fr.loc)
	}
	f.line("        dead, // a per-element allocation failed; ignore the subtree")
	f.line("    };")

	if use.unsigned {
		g.emitIntVisit(f, fs, name, false)
	}
	if use.signed {
		g.emitIntVisit(f, fs, name, true)
	}
	if use.fp32 {
		g.emitFloatVisit(f, fs, name, ir.KindFP32, "fp32", "f32")
	}
	if use.fp64 {
		g.emitFloatVisit(f, fs, name, ir.KindFP64, "fp64", "f64")
	}
	if use.str {
		g.emitPayloadVisit(f, fs, name, ir.KindString, "string")
	}
	if use.blob {
		g.emitPayloadVisit(f, fs, name, ir.KindBlob, "blob")
	}
	if use.scalarArray {
		g.emitArrayBegin(f, fs, name)
	}
	if use.sequence {
		g.emitSequence(f, fs, name)
	}
	f.line("};")
	f.blank()
}

// intArm renders one match arm body for an unsigned/signed store, or "" when
// the field does not belong to this callback.
func (g *gen) intArm(fr frame, fld *ir.Field, signed bool) string {
	acc := fr.path + "." + zigIdent(fld.Name)
	if signed {
		switch {
		case isSignedElem(fld.Kind):
			return fmt.Sprintf("%s = %s", acc, storeCast(numZigType(fld.Kind), "value"))
		case fld.Kind == ir.KindEnum:
			return fmt.Sprintf("%s = %s", acc, storeCast(enumBacking(fld.Ref.Target), "value"))
		case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
			return g.putCall(fr, fld, storeCast(numZigType(fld.Elem), "value"))
		case fld.Kind == ir.KindArray && fld.Elem == ir.KindEnum:
			return g.putCall(fr, fld, storeCast(enumBacking(fld.ElemRef.Target), "value"))
		}
		return ""
	}
	switch {
	case isUnsignedElem(fld.Kind):
		return fmt.Sprintf("%s = %s", acc, storeCast(numZigType(fld.Kind), "value"))
	case fld.Kind == ir.KindBool:
		return fmt.Sprintf("%s = value != 0", acc)
	case fld.Kind == ir.KindBitfield:
		return fmt.Sprintf("%s = %s", acc, storeCast(bitfieldBacking(fld.Ref.Target), "value"))
	case fld.Kind == ir.KindArray && isUnsignedElem(fld.Elem):
		return g.putCall(fr, fld, storeCast(numZigType(fld.Elem), "value"))
	case fld.Kind == ir.KindArray && fld.Elem == ir.KindBool:
		return g.putCall(fr, fld, "value != 0")
	case fld.Kind == ir.KindArray && fld.Elem == ir.KindBitfield:
		return g.putCall(fr, fld, storeCast(bitfieldBacking(fld.ElemRef.Target), "value"))
	}
	return ""
}

// nestedNativeArm renders the store into the innermost slice of a nested
// native array frame ("" when the element kind belongs to another callback).
func (g *gen) nestedNativeArm(fr frame, signed bool) string {
	var cast string
	if signed {
		switch {
		case isSignedElem(fr.elemKind):
			cast = storeCast(numZigType(fr.elemKind), "value")
		case fr.elemKind == ir.KindEnum:
			cast = storeCast(enumBacking(fr.elemRef.Target), "value")
		default:
			return ""
		}
	} else {
		switch {
		case isUnsignedElem(fr.elemKind):
			cast = storeCast(numZigType(fr.elemKind), "value")
		case fr.elemKind == ir.KindBool:
			cast = "value != 0"
		case fr.elemKind == ir.KindBitfield:
			cast = storeCast(bitfieldBacking(fr.elemRef.Target), "value")
		default:
			return ""
		}
	}
	// Guard the empty case: if the per-element allocation failed the outer
	// slice may have no last element to fill.
	return fmt.Sprintf("if (%s.len != 0) _put(_last(%s).*, &self.ai, %s)", fr.path, fr.path, cast)
}

func (g *gen) emitIntVisit(f *zfile, fs []frame, name string, signed bool) {
	cb, vt := "unsigned", "sofab.Unsigned"
	if signed {
		cb, vt = "signed", "sofab.Signed"
	}
	// Collect arms first: parameter names depend on whether any arm switches
	// on the field id (Zig rejects unused parameters).
	type frameArms struct {
		fr   frame
		arms []string // fkStruct: "id => body" lines
		body string   // fkNestedNative: single body
	}
	var all []frameArms
	idUsed := false
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			fa := frameArms{fr: fr}
			for _, fld := range fr.fields {
				if body := g.intArm(fr, fld, signed); body != "" {
					fa.arms = append(fa.arms, fmt.Sprintf("%d => %s,", fld.ID, body))
				}
			}
			if len(fa.arms) > 0 {
				idUsed = true
				all = append(all, fa)
			}
		case fkNestedNative:
			if body := g.nestedNativeArm(fr, signed); body != "" {
				all = append(all, frameArms{fr: fr, body: body})
			}
		}
	}
	idParam := "id"
	if !idUsed {
		idParam = "_"
	}
	f.blank()
	f.line("    pub fn %s(self: *_dec_%s, %s: sofab.Id, value: %s) void {", cb, name, idParam, vt)
	f.line("        switch (self.cur) {")
	for _, fa := range all {
		if fa.fr.kind == fkNestedNative {
			f.line("            .%s => %s,", fa.fr.loc, fa.body)
			continue
		}
		f.line("            .%s => switch (id) {", fa.fr.loc)
		for _, arm := range fa.arms {
			f.line("                %s", arm)
		}
		f.line("                else => {},")
		f.line("            },")
	}
	f.line("            else => {},")
	f.line("        }")
	f.line("    }")
}

func (g *gen) emitFloatVisit(f *zfile, fs []frame, name string, kind ir.Kind, cb, ztype string) {
	type frameArms struct {
		fr   frame
		arms []string
		body string
	}
	var all []frameArms
	idUsed := false
	for _, fr := range fs {
		if fr.kind == fkNestedNative && fr.elemKind == kind {
			body := fmt.Sprintf("if (%s.len != 0) _put(_last(%s).*, &self.ai, value)", fr.path, fr.path)
			all = append(all, frameArms{fr: fr, body: body})
			continue
		}
		fa := frameArms{fr: fr}
		for _, fld := range fr.fields {
			acc := fr.path + "." + zigIdent(fld.Name)
			switch {
			case fld.Kind == kind:
				fa.arms = append(fa.arms, fmt.Sprintf("%d => %s = value,", fld.ID, acc))
			case fld.Kind == ir.KindArray && fld.Elem == kind:
				fa.arms = append(fa.arms, fmt.Sprintf("%d => %s,", fld.ID, g.putCall(fr, fld, "value")))
			}
		}
		if len(fa.arms) > 0 {
			idUsed = true
			all = append(all, fa)
		}
	}
	idParam := "id"
	if !idUsed {
		idParam = "_"
	}
	f.blank()
	f.line("    pub fn %s(self: *_dec_%s, %s: sofab.Id, value: %s) void {", cb, name, idParam, ztype)
	f.line("        switch (self.cur) {")
	for _, fa := range all {
		if fa.body != "" {
			f.line("            .%s => %s,", fa.fr.loc, fa.body)
			continue
		}
		f.line("            .%s => switch (id) {", fa.fr.loc)
		for _, arm := range fa.arms {
			f.line("                %s", arm)
		}
		f.line("                else => {},")
		f.line("            },")
	}
	f.line("            else => {},")
	f.line("        }")
	f.line("    }")
}

// emitPayloadVisit emits the string or blob callback. The generated decode()
// feeds the whole buffer at once, so payloads always arrive single-shot
// (offset 0, whole chunk) and the borrowed chunk IS the value -- zero-copy.
func (g *gen) emitPayloadVisit(f *zfile, fs []frame, name string, kind ir.Kind, cb string) {
	f.blank()
	f.line("    pub fn %s(self: *_dec_%s, id: sofab.Id, _: usize, offset: usize, chunk: []const u8) void {", cb, name)
	f.line("        if (offset != 0) return; // decode() is single-shot; a split payload means truncated input")
	f.line("        switch (self.cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind {
			f.line("            .%s => _setElem([]const u8, self.alloc, &(%s), id, \"\", chunk),", fr.loc, fr.path)
		}
		if fr.kind != fkStruct {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == kind {
				arms = append(arms, fmt.Sprintf("%d => %s.%s = chunk,", fld.ID, fr.path, zigIdent(fld.Name)))
			}
		}
		if len(arms) == 0 {
			continue
		}
		f.line("            .%s => switch (id) {", fr.loc)
		for _, arm := range arms {
			f.line("                %s", arm)
		}
		f.line("                else => {},")
		f.line("            },")
	}
	f.line("            else => {},")
	f.line("        }")
	f.line("    }")
}

// emitArrayBegin emits the arrayBegin callback: reset the element fill index,
// allocate a dynamic native array to exactly the wire count, and append a
// fresh inner slice for a nested native array element.
func (g *gen) emitArrayBegin(f *zfile, fs []frame, name string) {
	type frameArms struct {
		fr   frame
		arms []string
		body string
	}
	var all []frameArms
	idUsed, countUsed := false, false
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			fa := frameArms{fr: fr}
			for _, fld := range fr.fields {
				if g.dynNativeArray(fld) {
					elem := g.zigArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems)
					fa.arms = append(fa.arms, fmt.Sprintf("%d => %s.%s = _allocN(%s, self.alloc, count),",
						fld.ID, fr.path, zigIdent(fld.Name), elem))
				}
			}
			if len(fa.arms) > 0 {
				idUsed, countUsed = true, true
				all = append(all, fa)
			}
		case fkNestedNative:
			inner := strings.TrimPrefix(fr.elemType, "[]const ")
			body := fmt.Sprintf("if (_grow(%s, self.alloc, &(%s), %s.len + 1, &.{})) { _last(%s).* = _allocN(%s, self.alloc, count); }",
				fr.elemType, fr.path, fr.path, fr.path, inner)
			countUsed = true
			all = append(all, frameArms{fr: fr, body: body})
		}
	}
	idParam, kindParam, countParam := "_", "_", "_"
	if idUsed {
		idParam = "id"
	}
	if countUsed {
		countParam = "count"
	}
	f.blank()
	f.line("    pub fn arrayBegin(self: *_dec_%s, %s: sofab.Id, %s: sofab.ArrayKind, %s: usize) void {", name, idParam, kindParam, countParam)
	f.line("        self.ai = 0;")
	if len(all) > 0 {
		f.line("        switch (self.cur) {")
		for _, fa := range all {
			if fa.body != "" {
				f.line("            .%s => %s,", fa.fr.loc, fa.body)
				continue
			}
			f.line("            .%s => switch (id) {", fa.fr.loc)
			for _, arm := range fa.arms {
				f.line("                %s", arm)
			}
			f.line("                else => {},")
			f.line("            },")
		}
		f.line("            else => {},")
		f.line("        }")
	}
	f.line("    }")
}

// emitSequence emits sequenceBegin/sequenceEnd: push the current location and
// descend. Wrapper-array fields reset their slice on entry (an explicit empty
// wrapper must override a non-empty value); struct/nested-array element frames
// grow their slice by one default element and descend into it.
func (g *gen) emitSequence(f *zfile, fs []frame, name string) {
	type frameArms struct {
		fr   frame
		arms []string
		body string
	}
	var all []frameArms
	idUsed := false
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			fa := frameArms{fr: fr}
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
					fa.arms = append(fa.arms, fmt.Sprintf("%d => .%s,", fld.ID, fr.loc+"_"+fld.Name))
				case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
					acc := fr.path + "." + zigIdent(fld.Name)
					fa.arms = append(fa.arms, fmt.Sprintf("%d => blk: { %s = &.{}; break :blk .%s; },", fld.ID, acc, fr.loc+"_"+fld.Name))
				}
			}
			if len(fa.arms) > 0 {
				idUsed = true
				all = append(all, fa)
			}
		case fkStructArr, fkArrArr:
			body := fmt.Sprintf("if (_grow(%s, self.alloc, &(%s), %s.len + 1, %s)) .%s else .dead",
				fr.elemType, fr.path, fr.path, fr.elemFill, fr.elemLoc)
			all = append(all, frameArms{fr: fr, body: body})
		}
	}
	idParam := "_"
	if idUsed {
		idParam = "id"
	}
	f.blank()
	f.line("    pub fn sequenceBegin(self: *_dec_%s, %s: sofab.Id) void {", name, idParam)
	f.line("        if (self.sp < self.stack.len) {")
	f.line("            self.stack[self.sp] = self.cur;")
	f.line("            self.sp += 1;")
	f.line("        }")
	f.line("        self.cur = switch (self.cur) {")
	for _, fa := range all {
		if fa.body != "" {
			f.line("            .%s => %s,", fa.fr.loc, fa.body)
			continue
		}
		f.line("            .%s => switch (id) {", fa.fr.loc)
		for _, arm := range fa.arms {
			f.line("                %s", arm)
		}
		f.line("                else => self.cur,")
		f.line("            },")
	}
	f.line("            else => self.cur,")
	f.line("        };")
	f.line("    }")
	f.blank()
	f.line("    pub fn sequenceEnd(self: *_dec_%s) void {", name)
	f.line("        if (self.sp > 0) {")
	f.line("            self.sp -= 1;")
	f.line("            self.cur = self.stack[self.sp];")
	f.line("        } else {")
	f.line("            self.cur = .root;")
	f.line("        }")
	f.line("    }")
}
