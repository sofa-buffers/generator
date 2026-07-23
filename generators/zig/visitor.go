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

	// Schema-unbounded element markers, for the receiver-side decode limits
	// (generator#102): only unbounded fields are guarded.
	elemDynLen   bool // fkSeqArr: element string/blob has no schema maxlen
	elemDynCount bool // fkNestedNative: inner native array has no schema count

	// cap is the wrapper array's schema fixed-count bound N (-1 == dynamic/no
	// count): an element id >= N is a schema-bound violation (MESSAGE_SPEC
	// §5.1/§7), rejected as INVALID (self.inv) before the slice grows — which also
	// bounds an over-index heap-amplification fill. Set on fkSeqArr / fkStructArr /
	// fkArrArr.
	cap int64

	// emax is the fkSeqArr string/blob element's schema maxlen L (-1 == no bound):
	// a wire byte length above L is malformed input, rejected as INVALID
	// (self.inv) before the value is stored, never truncated (MESSAGE_SPEC §7.1) —
	// the wrapper-element twin of the scalar-field maxlen reject.
	emax int64
}

// capOf maps a schema fixed-count bound to a frame's cap: N when the array
// declares a count, -1 (dynamic/unbounded) otherwise.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// boundOf maps a schema maxlen presence+value to a frame's emax bound: L when
// the element declares a maxlen, -1 (unbounded) otherwise.
func boundOf(has bool, v int64) int64 {
	if has {
		return v
	}
	return -1
}

// frames walks a message and returns every sequence container, root first.
func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walkFields func(loc, path string, fields []*ir.Field)
	var addArray func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64, cap int64)

	walkFields = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, kind: fkStruct, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				cl := loc + "_" + fld.Name
				walkFields(cl, path+"."+zigIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+zigIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, fld.ElemMaxHas, fld.ElemMax, capOf(fld.HasCount, fld.Count))
			}
		}
	}

	// addArray builds the frame(s) for a wrapper-sequence array whose slice is
	// at (loc, path) and whose element is (elem, ref, items); elemMaxHas is the
	// element's schema maxlen presence (string/blob elements only); cap is the
	// array's schema fixed-count bound (-1 == dynamic).
	addArray = func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64, cap int64) {
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{loc: loc, path: path, kind: fkSeqArr, elemKind: elem, elemDynLen: !elemMaxHas, cap: cap, emax: boundOf(elemMaxHas, elemMax)})
		case ir.KindStruct, ir.KindUnion:
			el := loc + "_e"
			out = append(out, frame{
				loc: loc, path: path, kind: fkStructArr, elemLoc: el,
				elemType: g.typeName(ref.Key), elemFill: ".{}", cap: cap,
			})
			walkFields(el, "_last("+path+")", ref.Target.Fields)
		case ir.KindArray:
			// The element is an inner array (items). A native inner array is
			// handled by a single wrapper frame (arrayBegin appends a fresh
			// inner slice, elements land in the last one); a wrapper inner
			// array descends recursively with its own inner count bound.
			inner := g.zigArrayElem(items.Elem, items.ElemRef, items.ElemItems)
			if isNativeArrayElem(items.Elem) {
				out = append(out, frame{
					loc: loc, path: path, kind: fkNestedNative,
					elemKind: items.Elem, elemRef: items.ElemRef,
					elemType: "[]const " + inner, elemFill: "&.{}",
					elemDynCount: !items.HasCount, cap: cap,
				})
			} else {
				el := loc + "_e"
				out = append(out, frame{
					loc: loc, path: path, kind: fkArrArr, elemLoc: el,
					elemType: "[]const " + inner, elemFill: "&.{}", cap: cap,
				})
				addArray(el, "_last("+path+").*", items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax, capOf(items.HasCount, items.Count))
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
	// dynAlloc: the message decodes at least one slice-backed native array (a
	// count-less direct field or a nested native element array), i.e. it
	// allocates array storage from an untrusted wire count and needs the
	// hardened _allocN/_put pair plus the announced-count register `an`.
	dynAlloc bool
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
			u.dynAlloc = true
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
			if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) && !fld.HasCount {
				u.dynAlloc = true
			}
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
// array, which needs an arrayBegin allocation driven by the wire count.
func (g *gen) dynNativeArray(f *ir.Field) bool {
	return f.Kind == ir.KindArray && isNativeArrayElem(f.Elem) && !f.HasCount
}

// dynAllocUse reports whether any message decodes a slice-backed native array
// (a count-less direct field or a nested native element array) — i.e. whether
// the hardened _allocN/_put pair is referenced (see emitSupport).
func (g *gen) dynAllocUse(s *ir.Schema) bool {
	for _, m := range s.Messages {
		if visitorUseOf(g.frames(m)).dynAlloc {
			return true
		}
	}
	return false
}

// msgLimitGuards reports whether the message's decoder emits at least one
// decode-limit guard (generator#102) — i.e. whether it needs the sticky `lim`
// flag and the decode() LimitExceeded check. It mirrors the guard emission
// exactly: an active limit only guards fields the schema left unbounded.
func (g *gen) msgLimitGuards(fields []*ir.Field) bool {
	if !g.limits.any() {
		return false
	}
	for _, fr := range g.frames(&ir.Message{Fields: fields}) {
		switch fr.kind {
		case fkNestedNative:
			if g.limits.arrayHas && fr.elemDynCount {
				return true
			}
		case fkSeqArr:
			if fr.elemDynLen && ((fr.elemKind == ir.KindString && g.limits.stringHas) ||
				(fr.elemKind == ir.KindBlob && g.limits.blobHas)) {
				return true
			}
		case fkStruct:
			for _, fld := range fr.fields {
				switch {
				case g.limits.arrayHas && g.dynNativeArray(fld):
					return true
				case g.limits.stringHas && fld.Kind == ir.KindString && !fld.HasMaxlen:
					return true
				case g.limits.blobHas && fld.Kind == ir.KindBlob && !fld.HasMaxlen:
					return true
				}
			}
		}
	}
	return false
}

// putCall renders the element store for a direct native array field: the
// capacity-checked _putc for a fixed [N]T — an over-count element flags the
// message INVALID per MESSAGE_SPEC 3+7 (generator#100) — or the growing _put
// for a dynamic (count-less) slice, which keeps every wire element up to the
// announced count while never trusting that count for the eager allocation.
func (g *gen) putCall(fr frame, fld *ir.Field, val string) string {
	acc := fr.path + "." + zigIdent(fld.Name)
	var inner string
	if _, _, ok := g.fixedNativeArray(fld); ok {
		inner = fmt.Sprintf("_putc(&%s, &self.ai, %s, &self.inv)", acc, val)
	} else {
		inner = fmt.Sprintf("_put(&%s, self.alloc, &self.ai, self.an, %s)", acc, val)
	}
	// §7.3 fill guard (generator#188): only fill while arrayBegin has this array
	// armed; a bare scalar at this id (afill == 0) falls through and is skipped.
	return fmt.Sprintf("{ if (self.afill != 0) { self.afill -= 1; %s; } }", inner)
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
	f.line("    inv: bool = false, // a scalar array over its schema count, or a wrapper element id >= count -> INVALID")
	// Sticky decode-limit flag (generator#102): an unbounded field exceeded a
	// configured max_dyn_* cap; decode() then rejects with error.LimitExceeded.
	if g.msgLimitGuards(fields) {
		f.line("    lim: bool = false, // an unbounded field exceeded a configured decode limit")
	}
	if use.scalarArray {
		f.line("    ai: usize = 0, // index into the native array currently being filled")
	}
	if use.dynAlloc {
		f.line("    an: usize = 0, // announced wire count of that array (untrusted until its elements arrive)")
	}
	// §7.3 array-vs-scalar skip counter (generator#183 for integers, #193 for fp):
	// corelib-zig streams an array element-by-element through the same
	// unsigned()/signed()/fp32()/fp64() callbacks a lone scalar uses, so a
	// SCALAR-declared id that receives an ARRAY header would otherwise store the
	// elements. arrayBegin arms this with the announced count and the callbacks
	// discard exactly that many.
	arrSkip := use.unsigned || use.signed || use.fp32 || use.fp64
	if arrSkip {
		f.line("    askip: usize = 0, // elements left to discard from a S7.3-contradictory array")
	}
	// §7.3 mirror (generator#188): a bare scalar delivered at a native-array id
	// would otherwise land in that array's fill arm as element 0. arrayBegin arms
	// this with the announced count at legitimate native-array positions; a fill
	// runs only while it is positive, so an unarmed bare scalar (afill == 0) is
	// skipped like an unknown id.
	if use.scalarArray {
		f.line("    afill: usize = 0, // elements still expected by an armed native-array fill (S7.3)")
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
	// arrayBegin is emitted for its own array-target work, and additionally
	// whenever the §7.3 guard needs a place to arm itself. The corelib calls it
	// through @hasDecl, so emitting it for the guard alone is enough.
	if use.scalarArray || arrSkip {
		g.emitArrayBegin(f, fs, name, arrSkip)
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
	// §7.3 fill guard (generator#188), plus the empty-case guard: if the
	// per-element allocation failed the outer slice may have no last element.
	return fmt.Sprintf("{ if (self.afill != 0) { self.afill -= 1; if (%s.len != 0) _put(_last(%s), self.alloc, &self.ai, self.an, %s); } }", fr.path, fr.path, cast)
}

// emitArraySkipArm arms the §7.3 discard counter in arrayBegin (generator#183,
// extended to fp by generator#193). Every array kind whose elements land in a
// callback a scalar shares is armed: integers under .unsigned/.signed, fp under
// .fixlen. Every (scope, id) that genuinely declares a native array of the
// matching element kind disarms it (=> 0), so a legitimate array stores normally;
// everything else — a scalar-declared id, an unknown id — discards exactly
// `count` elements, after which a real scalar at the same id still decodes.
// Mirrors emitArrayFillArm.

// arraySkipUsesID reports whether the §7.3 skip arm switches on `id`, i.e.
// whether the message declares any native-element array (integer or fp) to
// disarm for. Zig rejects an unused function parameter, so arrayBegin's signature
// has to know this before the body is emitted.
func arraySkipUsesID(fs []frame) bool {
	for _, fr := range fs {
		if fr.kind != fkStruct {
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
				return true
			}
		}
	}
	return false
}

// arrayFillUsesID reports whether the §7.3 fill arm switches on `id`, i.e. the
// message declares any native-element array in a struct scope to arm for.
func arrayFillUsesID(fs []frame) bool {
	for _, fr := range fs {
		if fr.kind != fkStruct {
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
				return true
			}
		}
	}
	return false
}

// emitArrayFillArm arms the §7.3 fill counter in arrayBegin (generator#188), the
// mirror of emitArraySkipArm. It is armed at a legitimate native-array position
// matching the wire array kind — integer arrays under .unsigned/.signed, fp
// arrays under .fixlen — and 0 elsewhere, so a bare scalar at an array id
// (afill == 0) falls through its fill arm and is skipped.
func (g *gen) emitArrayFillArm(f *zfile, fs []frame, fillArm bool) {
	if !fillArm {
		return
	}
	emit := func(kinds string, want func(ir.Kind) bool) {
		f.line("            %s => switch (self.cur) {", kinds)
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				var arms []string
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && want(fld.Elem) {
						arms = append(arms, fmt.Sprintf("%d => count,", fld.ID))
					}
				}
				if len(arms) > 0 {
					f.line("                .%s => switch (id) {", fr.loc)
					for _, a := range arms {
						f.line("                    %s", a)
					}
					f.line("                    else => 0,")
					f.line("                },")
				}
			case fkNestedNative:
				if want(fr.elemKind) {
					f.line("                .%s => count,", fr.loc)
				}
			}
		}
		f.line("                else => 0,")
		f.line("            },")
	}
	// ArrayKind is exactly {unsigned, signed, fixlen}; listing all three leaves no
	// room for an else prong (Zig rejects an unreachable one).
	f.line("        self.afill = switch (kind) {")
	emit(".unsigned, .signed", func(k ir.Kind) bool { return isNativeArrayElem(k) && k != ir.KindFP32 && k != ir.KindFP64 })
	emit(".fixlen", func(k ir.Kind) bool { return k == ir.KindFP32 || k == ir.KindFP64 })
	f.line("        };")
}

func (g *gen) emitArraySkipArm(f *zfile, fs []frame, arrSkip bool) {
	if !arrSkip {
		return
	}
	emit := func(kinds string, want func(ir.Kind) bool) {
		f.line("            %s => switch (self.cur) {", kinds)
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				var arms []string
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && want(fld.Elem) {
						arms = append(arms, fmt.Sprintf("%d => 0,", fld.ID))
					}
				}
				if len(arms) > 0 {
					f.line("                .%s => switch (id) {", fr.loc)
					for _, a := range arms {
						f.line("                    %s", a)
					}
					f.line("                    else => count,")
					f.line("                },")
				}
			case fkNestedNative:
				if want(fr.elemKind) {
					f.line("                .%s => 0,", fr.loc)
				}
			}
		}
		f.line("                else => count,")
		f.line("            },")
	}
	// ArrayKind is exactly {unsigned, signed, fixlen}; listing all three leaves no
	// room for an else prong (Zig rejects an unreachable one), mirroring
	// emitArrayFillArm.
	f.line("        self.askip = switch (kind) {")
	emit(".unsigned, .signed", func(k ir.Kind) bool { return isNativeArrayElem(k) && k != ir.KindFP32 && k != ir.KindFP64 })
	emit(".fixlen", func(k ir.Kind) bool { return k == ir.KindFP32 || k == ir.KindFP64 })
	f.line("        };")
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
	// §7.3 (generator#183): discard the elements of an integer array delivered to
	// a scalar-declared id. arrayBegin armed the count; this self-terminates
	// without an array-end callback and survives feed chunk boundaries.
	f.line("        if (self.askip > 0) { self.askip -= 1; return; }")
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
			body := fmt.Sprintf("if (%s.len != 0) _put(_last(%s), self.alloc, &self.ai, self.an, value)", fr.path, fr.path)
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
	// §7.3 (generator#193): discard the elements of an fp array delivered to a
	// scalar-declared id. arrayBegin armed the count; this self-terminates without
	// an array-end callback and survives feed chunk boundaries. Always present:
	// use.fp32/fp64 implies arrSkip, so the askip field always exists here.
	f.line("        if (self.askip > 0) { self.askip -= 1; return; }")
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
//
// With an active max_dyn_string_len / max_dyn_blob_len (generator#102) every
// schema-unbounded field checks the header-announced total length before the
// value is taken: the borrow never allocates, but the cap is a policy bound,
// so an over-limit payload flags `lim` and decode() fails with LimitExceeded.
func (g *gen) emitPayloadVisit(f *zfile, fs []frame, name string, kind ir.Kind, cb string) {
	active, capName := g.limits.stringHas, "max_dyn_string_len"
	if kind == ir.KindBlob {
		active, capName = g.limits.blobHas, "max_dyn_blob_len"
	}
	// Collect arms first: the total-length parameter is named only when some
	// limit guard reads it (Zig rejects unused parameters).
	type frameArms struct {
		fr   frame
		arms []string // fkStruct: "id => body" lines
		body string   // fkSeqArr: single body
	}
	var all []frameArms
	totalUsed := false
	// Strict UTF-8 (MESSAGE_SPEC §8 / CORELIB_PLAN §6.4): a `string` payload is
	// UTF-8. Zig's string is a borrowed byte slice (byte-container), so the corelib
	// exposes `utf8_valid(bytes)` and generated code emits an UNCONDITIONAL call to
	// it at the materialization site — the SOFAB_STRICT_UTF8 gate lives inside the
	// primitive (folds to true when compiled off), so this code is identical across
	// build configs. Invalid UTF-8 is the INVALID outcome (self.inv). `blob` is
	// opaque bytes and is stored verbatim. Skipped fields hit the switch `else`
	// arms and are never validated (§6.4). mat() wraps only the materialization.
	mat := func(store string) string {
		if kind != ir.KindString {
			return store
		}
		return "if (!sofab.utf8_valid(chunk)) { self.inv = true; } else { " + store + " }"
	}
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind {
			set := fmt.Sprintf("_setElem([]const u8, self.alloc, &(%s), id, \"\", chunk)", fr.path)
			// stmt is the placement as a single statement (trailing ;), for use
			// inside an { ... } block; body is the raw arm expression. For a string
			// element the materialization is UTF-8-validated (mat); blob is verbatim.
			stmt := mat(set + ";")
			body := set
			if kind == ir.KindString {
				body = stmt
			}
			if active && fr.elemDynLen {
				totalUsed = true
				body = fmt.Sprintf("if (total > %s) { self.lim = true; } else { %s }", capName, stmt)
				stmt = body
			}
			// Bounded element (schema maxlen): a wire byte length above the maxlen
			// is malformed input, rejected as INVALID before the value is stored,
			// never truncated (MESSAGE_SPEC §7.1). Mutually exclusive with the #102
			// limit guard above, which only fires on an unbounded element.
			if fr.emax >= 0 {
				totalUsed = true
				body = fmt.Sprintf("if (total > %d) { self.inv = true; } else { %s }", fr.emax, stmt)
				stmt = body
			}
			// Fixed-count wrapper array: reject an element id >= N as INVALID
			// (MESSAGE_SPEC §5.1/§7 — issue #142) before _setElem grows the slice,
			// which also bounds an over-index heap-amplification fill.
			if fr.cap >= 0 {
				body = fmt.Sprintf("if (id >= %d) { self.inv = true; } else { %s }", fr.cap, stmt)
			}
			all = append(all, frameArms{fr: fr, body: body})
		}
		if fr.kind != fkStruct {
			continue
		}
		fa := frameArms{fr: fr}
		for _, fld := range fr.fields {
			if fld.Kind != kind {
				continue
			}
			acc := fr.path + "." + zigIdent(fld.Name)
			store := acc + " = chunk;"
			switch {
			case fld.HasMaxlen:
				// Bounded scalar string/blob: a wire byte length above the schema
				// maxlen is malformed input, rejected as INVALID before the value
				// is stored, never truncated (MESSAGE_SPEC §7.1). A string is then
				// UTF-8-validated at the store (mat); blob is stored verbatim.
				totalUsed = true
				fa.arms = append(fa.arms, fmt.Sprintf("%d => if (total > %d) { self.inv = true; } else { %s },", fld.ID, fld.Maxlen, mat(store)))
			case active:
				// Unbounded scalar: keep the configured #102 decode-limit behavior.
				totalUsed = true
				fa.arms = append(fa.arms, fmt.Sprintf("%d => if (total > %s) { self.lim = true; } else { %s },", fld.ID, capName, mat(store)))
			default:
				if kind == ir.KindString {
					fa.arms = append(fa.arms, fmt.Sprintf("%d => %s,", fld.ID, mat(store)))
				} else {
					fa.arms = append(fa.arms, fmt.Sprintf("%d => %s = chunk,", fld.ID, acc))
				}
			}
		}
		if len(fa.arms) > 0 {
			all = append(all, fa)
		}
	}
	totalParam := "_"
	if totalUsed {
		totalParam = "total"
	}
	f.blank()
	f.line("    pub fn %s(self: *_dec_%s, id: sofab.Id, %s: usize, offset: usize, chunk: []const u8) void {", cb, name, totalParam)
	f.line("        if (offset != 0) return; // decode() is single-shot; a split payload means truncated input")
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

// emitArrayBegin emits the arrayBegin callback: reset the element fill index,
// allocate a dynamic native array from the wire count (capped eagerly, grown
// by _put as elements actually arrive), and append a fresh inner slice for a
// nested native array element.
//
// With an active max_dyn_array_count (generator#102) every schema-unbounded
// array checks the announced count first: an over-limit count flags `lim` and
// skips the field, and decode() then fails with error.LimitExceeded.
func (g *gen) emitArrayBegin(f *zfile, fs []frame, name string, arrSkip bool) {
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
				if _, n, ok := g.fixedNativeArray(fld); ok {
					// Over-count reject at the count header (generator#216 / F-0032):
					// a wire element count above the schema `count` N is INVALID
					// (MESSAGE_SPEC 3+7), and setting the sticky `inv` HERE — before
					// the elements are read — makes INVALID dominate a truncated tail
					// (§5.2), since decode() reads `inv` before surfacing `.incomplete`.
					// The store-side _putc bound only fires when the N+1th element
					// actually arrives, which a truncated over-count array never reaches.
					guard := fmt.Sprintf("if (count > %d) { self.inv = true; return; }", n)
					// A fixed [N]T whose declaration default is the schema default also
					// needs a reset here: clear it so the elements the encoder trimmed
					// off the tail decode as the element default, not that schema default
					// (MESSAGE_SPEC S3 -- see zigFixedArrayNeedsReset).
					reset := ""
					if g.zigFixedArrayNeedsReset(fld) {
						reset = fmt.Sprintf(" %s.%s = @splat(%s);", fr.path, zigIdent(fld.Name), zigElemZero(fld.Elem))
					}
					fa.arms = append(fa.arms, fmt.Sprintf("%d => { %s%s },", fld.ID, guard, reset))
					// The guard reads the wire count; the switch reads id.
					idUsed, countUsed = true, true
					continue
				}
				if g.dynNativeArray(fld) {
					idUsed, countUsed = true, true
					elem := g.zigArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems)
					body := fmt.Sprintf("%s.%s = _allocN(%s, self.alloc, count)", fr.path, zigIdent(fld.Name), elem)
					if g.limits.arrayHas {
						// A count-less array is always unbounded, so every
						// direct dynamic native array gets the guard. an = 0
						// drops the rejected array's elements: a field over
						// the cap never allocates (generator#102).
						fa.arms = append(fa.arms, fmt.Sprintf("%d => if (count > max_dyn_array_count) { self.lim = true; self.an = 0; } else { %s; },", fld.ID, body))
					} else {
						fa.arms = append(fa.arms, fmt.Sprintf("%d => %s,", fld.ID, body))
					}
				}
			}
			if len(fa.arms) > 0 {
				all = append(all, fa)
			}
		case fkNestedNative:
			inner := strings.TrimPrefix(fr.elemType, "[]const ")
			body := fmt.Sprintf("if (_grow(%s, self.alloc, &(%s), %s.len + 1, &.{})) { _last(%s).* = _allocN(%s, self.alloc, count); }",
				fr.elemType, fr.path, fr.path, fr.path, inner)
			if g.limits.arrayHas && fr.elemDynCount {
				body = fmt.Sprintf("if (count > max_dyn_array_count) { self.lim = true; self.an = 0; } else %s", body)
			}
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
	// The §7.3 guard always reads kind and count, and reads id only when the
	// message has a native-element array (integer or fp) to disarm for (Zig rejects
	// an unused parameter, so a message without one keeps `_`).
	if arrSkip {
		kindParam, countParam = "kind", "count"
		if arraySkipUsesID(fs) {
			idParam = "id"
		}
	}
	// The §7.3 fill arm (generator#188) reads kind and count, and id whenever the
	// message has any native-element array to arm for (integer or fp).
	fillArm := visitorUseOf(fs).scalarArray
	if fillArm {
		kindParam, countParam = "kind", "count"
		if arrayFillUsesID(fs) {
			idParam = "id"
		}
	}
	f.blank()
	f.line("    pub fn arrayBegin(self: *_dec_%s, %s: sofab.Id, %s: sofab.ArrayKind, %s: usize) void {", name, idParam, kindParam, countParam)
	if visitorUseOf(fs).scalarArray {
		f.line("        self.ai = 0;")
	}
	g.emitArraySkipArm(f, fs, arrSkip)
	g.emitArrayFillArm(f, fs, fillArm)
	if visitorUseOf(fs).dynAlloc {
		f.line("        self.an = count;")
	}
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
			grow := fmt.Sprintf("if (_grow(%s, self.alloc, &(%s), %s.len + 1, %s)) .%s else .dead",
				fr.elemType, fr.path, fr.path, fr.elemFill, fr.elemLoc)
			body := grow
			// Fixed-count wrapper array: a struct/union/nested-array element arrives
			// in dense arrival order, so id == the appended index; an id >= N marks
			// the decode INVALID (MESSAGE_SPEC §5.1/§7 — issue #142). The element is
			// still appended (bounded by the real wire elements, no amplification) so
			// the descended element location stays valid.
			if fr.cap >= 0 {
				idUsed = true
				body = fmt.Sprintf("blk: { if (id >= %d) self.inv = true; break :blk %s; }", fr.cap, grow)
			}
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
