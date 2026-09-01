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
	fkNestedNative                  // array of native array: arrayBegin places an inner slice at the row's id
	fkArrArr                        // array of (string/blob/struct/nested) array: per-element sequence descends
)

// frame is one sequence container reachable from a message. loc is the _Loc
// variant; path is the Zig lvalue expression (e.g. "self.m.somestruct.deep" or
// "sofab.arrays.at(self.m.points, self.ei_root_points).tags").
type frame struct {
	loc      string
	path     string
	kind     frameKind
	fields   []*ir.Field // fkStruct
	elemLoc  string      // fkStructArr, fkArrArr: location to descend to on a per-element sequenceBegin
	elemKind ir.Kind     // fkSeqArr: string/blob element; fkNestedNative: inner native element kind
	elemRef  *ir.TypeRef // fkNestedNative: enum/bitfield backing type
	elemType string      // fkStructArr/fkArrArr/fkNestedNative: Zig type of one element (for _grow)
	elemFill string      // fkStructArr/fkArrArr/fkNestedNative: fill literal for the grow helper

	// idx is the decoder register holding the element index this frame is
	// currently decoding into (fkStructArr/fkArrArr/fkNestedNative). The element
	// id IS the array index (MESSAGE_SPEC §5.1), so sequenceBegin (arrayBegin for
	// a native row) places at that index and the child stores address it through
	// sofab.arrays.at(path, self.<idx>) — never through the last appended element.
	idx string

	// Schema-unbounded element markers, for the receiver-side decode limits
	// (generator#102): only unbounded fields are guarded.
	elemDynLen   bool // fkSeqArr: element string/blob has no schema maxlen
	elemDynCount bool // fkNestedNative: inner native array has no schema count
	// elemCap is the inner native array's own schema count N (-1 == none) -- the
	// bound on a ROW's element count, which `cap` below does not give: cap bounds
	// the row's ID against the outer array's capacity. Both are needed, for
	// different reasons: the id bound stops an over-index gap-fill, this one stops
	// a row header claiming more elements than the schema allows it (§7.1).
	elemCap int64

	// cap is the wrapper array's schema count bound N (-1 == no count). N is a
	// CAPACITY, not a length: it never reaches the wire and never adds elements the
	// wire did not carry. All it does here is bound the array — an element id >= N
	// is a schema-bound violation (MESSAGE_SPEC §5.1/§7), rejected as INVALID
	// (self.inv) before the slice grows, which also bounds an over-index
	// heap-amplification fill. Set on fkSeqArr / fkStructArr / fkArrArr /
	// fkNestedNative.
	cap int64

	// emax is the fkSeqArr string/blob element's schema maxlen L (-1 == no bound):
	// a wire byte length above L is malformed input, rejected as INVALID
	// (self.inv) before the value is stored, never truncated (MESSAGE_SPEC §7.1) —
	// the wrapper-element twin of the scalar-field maxlen reject.
	emax int64
}

// capOf maps a schema count bound to a frame's cap: N when the array declares a
// count, -1 (unbounded) otherwise. N is a CAPACITY: a frame uses it only to
// reject an out-of-range element id, never to size the result.
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
	// array's schema count bound (-1 == no count).
	addArray = func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64, cap int64) {
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{loc: loc, path: path, kind: fkSeqArr, elemKind: elem, elemDynLen: !elemMaxHas, cap: cap, emax: boundOf(elemMaxHas, elemMax)})
		case ir.KindStruct, ir.KindUnion:
			el := loc + "_e"
			idx := "ei_" + loc
			out = append(out, frame{
				loc: loc, path: path, kind: fkStructArr, elemLoc: el, idx: idx,
				elemType: g.typeName(ref.Key), elemFill: ".{}", cap: cap,
			})
			walkFields(el, "sofab.arrays.at("+path+", self."+idx+")", ref.Target.Fields)
		case ir.KindArray:
			// The element is an inner array (items). A native inner array is
			// handled by a single wrapper frame (arrayBegin places a fresh inner
			// slice at the row's element id, elements land in that one); a
			// wrapper inner array descends recursively with its own count bound.
			inner := g.zigArrayElem(items.Elem, items.ElemRef, items.ElemItems)
			if isNativeArrayElem(items.Elem) {
				out = append(out, frame{
					loc: loc, path: path, kind: fkNestedNative, idx: "ei_" + loc,
					elemKind: items.Elem, elemRef: items.ElemRef,
					elemType: "[]const " + inner, elemFill: "&.{}",
					elemDynCount: !items.HasCount, elemCap: capOf(items.HasCount, items.Count), cap: cap,
				})
			} else {
				el := loc + "_e"
				idx := "ei_" + loc
				out = append(out, frame{
					loc: loc, path: path, kind: fkArrArr, elemLoc: el, idx: idx,
					elemType: "[]const " + inner, elemFill: "&.{}", cap: cap,
				})
				addArray(el, "sofab.arrays.at("+path+", self."+idx+").*", items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax, capOf(items.HasCount, items.Count))
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
	// capped sofab.arrays.allocCapped plus putGrowing, and the announced-count
	// register `an` and the fill index `ai`.
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

// msgLimitGuards reports whether the message's decoder emits at least one
// decode-limit guard (generator#102) — i.e. whether it needs the sticky `lim`
// flag and the decode() LimitExceeded check. It mirrors the guard emission
// exactly: an active limit only guards fields the schema left unbounded.
func (g *gen) msgLimitGuards(fields []*ir.Field) bool {
	if !g.limits.any() {
		return false
	}
	for _, fr := range g.frames(&ir.Message{Fields: fields}) {
		// Any wrapper array the schema leaves uncounted needs the flag: since
		// generator#387 the array cap bounds that array's element INDEX, which is
		// the array's length (§9.5). Checked ahead of the per-kind conditions
		// below, and separately from them -- a message whose only unbounded array
		// is a string wrapper has no count header anywhere, so none of them fires
		// while the index guard still sets self.lim.
		if g.limits.arrayHas && fr.kind != fkStruct && fr.cap < 0 {
			return true
		}
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
// capacity-checked push into a count:N field's inline storage -- an element
// past the capacity flags the message INVALID -- or sofab.arrays.putGrowing
// into a dynamic (count-less) slice.
//
// putGrowing no longer grows anything here, and cannot: arrayBegin sizes that
// slice at exactly the announced count `n` after bounding it against the cap
// (ARCHITECTURE §9.5, shape A), so `i >= s.len` implies `i >= n` and the helper
// has already returned on its first line. It stays the call because it is still
// the corelib's store for this destination shape; reducing it to the bounded put
// it has become is corelib-zig's half of #386.
//
// A count:N field carries its own index, so its store needs none of the
// visitor's: the wire count M IS the array's length (MESSAGE_SPEC §3), so the
// length follows the elements that actually arrive, counting up from the
// `clear` arrayBegin issued. `count` bounds them but adds none, and nothing is
// filled in at [M, N).
// `guard` is the element's §7.1 declared-width rejection (widthGuard), placed
// INSIDE the fill guard: an over-width scalar arriving at this id with no
// arrayBegin in front of it (afill == 0) is a §7.3 skip and must not be rejected.
func (g *gen) putCall(fr frame, fld *ir.Field, guard, val string) string {
	acc := fr.path + "." + zigIdent(fld.Name)
	var inner string
	if _, _, ok := g.fixedNativeArray(fld); ok {
		inner = fmt.Sprintf("%s.push(%s, &self.inv)", acc, val)
	} else {
		inner = fmt.Sprintf("sofab.arrays.putGrowing(&%s, self.alloc, &self.ai, self.an, %s)", acc, val)
	}
	// §7.3 fill guard (generator#188): only fill while arrayBegin has this array
	// armed; a bare scalar at this id (afill == 0) falls through and is skipped.
	return fmt.Sprintf("{ if (self.afill != 0) { self.afill -= 1; %s%s; } }", guard, inner)
}

// storeCast renders the visitor value expression for a numeric destination
// type: u64/i64 pass through, narrower integers are cast down.
//
// The cast is only ever reached for a value that FITS. The declared width is a
// normative validity bound, not a storage hint (MESSAGE_SPEC §1/§7.1,
// documentation#32), so widthGuard rejects an out-of-range value as INVALID
// before the store — `@truncate` here would otherwise be exactly the masking §7.1
// forbids. `@intCast` rather than `@truncate` makes that contract explicit: it
// is checked in safe build modes, so a guard that ever failed to precede a store
// is a panic in Debug/ReleaseSafe rather than a silently masked value.
func storeCast(dest string, value string) string {
	if dest == "u64" || dest == "i64" {
		return value
	}
	return "@intCast(" + value + ")"
}

// widthGuard renders the §7.1 declared-width rejection for a store into a
// destination of Kind k, or "" for the 64-bit kinds (whose range IS the
// accumulator the value arrives in). `self.inv` is the same sticky INVALID flag
// the over-count and over-index guards set, surfaced by decode() as
// error.InvalidMessage.
//
// The unsigned side needs no negative term: sofab.Unsigned is a u64.
//
// guardedStore wraps a scalar store arm in the block a guard needs. Zig prong
// bodies are expressions, so a guarded store becomes `{ if (...) {...} store; }`
// while an unguarded one stays the bare expression it was.
func guardedStore(guard, stmt string) string {
	if guard == "" {
		return stmt
	}
	return "{ " + guard + stmt + "; }"
}

func widthGuard(k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("if (value < %d or value > %d) { self.inv = true; return; } ", lo, hi)
	}
	return fmt.Sprintf("if (value > %d) { self.inv = true; return; } ", hi)
}

func (g *gen) emitDecoder(f *zfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	use := visitorUseOf(fs)
	g.msgLim = g.msgLimitGuards(fields) // for overIndexCond, which cannot reach fields

	f.line("/// Flat-visitor decoder for %s: a (location, id) state machine over the", name)
	f.line("/// corelib's streaming callbacks, with a bounded location stack.")
	f.line("const _dec_%s = struct {", name)
	f.line("    m: *%s,", name)
	f.line("    alloc: std.mem.Allocator,")
	// Always present, even for a message that declares no sequence of its own:
	// sequenceBegin is emitted unconditionally so an unknown sequence's CHILDREN
	// are skipped rather than bound into the enclosing scope (generator#268), and
	// it needs somewhere to push. The corelib rejects nesting deeper than
	// MAX_DEPTH (255), so 256 slots always suffice -- no heap, no overflow
	// handling.
	f.line("    stack: [256]_Loc = undefined,")
	f.line("    sp: usize = 0,")
	f.line("    cur: _Loc = .root,")
	// Reassembly buffer for a string/blob payload split across feed chunks.
	// Untouched -- and never allocated -- on the contiguous path and on every
	// streaming payload that happens to arrive whole in one chunk, which is what
	// keeps the zero-copy borrow the common case rather than the exception.
	f.line("    acc: sofab.PayloadAcc = .{}, // only a payload split across feed chunks lands here")
	// Set by decoder(), left false by decode(). See _reassemble: on the streaming
	// path a delivered slice may live in the corelib's reusable carry buffer
	// rather than in the caller's chunk, and nothing in the callback tells the
	// two apart -- so that path owns every payload instead of borrowing.
	f.line("    own: bool = false, // copy every payload instead of borrowing (streaming path)")
	// Sticky malformed-message flag: a fixed native array received more
	// elements than its schema count (generator#100); decode() then rejects
	// with error.InvalidMessage. Always present so decode() can check it.
	f.line("    inv: bool = false, // a scalar array over its schema count, or a wrapper element id >= count -> INVALID")
	// Sticky decode-limit flag (generator#102): an unbounded field exceeded a
	// configured max_dyn_* cap; decode() then rejects with error.LimitExceeded.
	if g.msgLimitGuards(fields) {
		f.line("    lim: bool = false, // an unbounded field exceeded a configured decode limit")
	}
	// Only a slice-backed array needs the visitor to carry the fill index: a
	// count:N field keeps its own length, and pushes into it.
	if use.dynAlloc {
		f.line("    ai: usize = 0, // index into the native array currently being filled")
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
		f.line("    askip: usize = 0, // elements left to discard from a wire-type-contradictory array")
	}
	// §7.3 mirror (generator#188): a bare scalar delivered at a native-array id
	// would otherwise land in that array's fill arm as element 0. arrayBegin arms
	// this with the announced count at legitimate native-array positions; a fill
	// runs only while it is positive, so an unarmed bare scalar (afill == 0) is
	// skipped like an unknown id.
	if use.scalarArray {
		f.line("    afill: usize = 0, // elements still expected by an armed native-array fill (S7.3)")
	}
	// One element-index register per struct/nested-array wrapper frame: the
	// element id IS the array index (MESSAGE_SPEC §5.1), so sequenceBegin records
	// the id it placed at and the child stores address that element through
	// sofab.arrays.at(path, self.<idx>). Nesting needs no stack — a frame's register is only
	// read while that frame's element scope is open.
	for _, fr := range fs {
		if fr.idx != "" {
			f.line("    %s: usize = 0, // index of the element %s is decoding into (S5.1)", fr.idx, fr.loc)
		}
	}
	f.blank()
	f.line("    const _Loc = enum {")
	for _, fr := range fs {
		f.line("        %s,", fr.loc)
	}
	f.line("        dead, // skipped subtree: an undeclared sequence id, a S7.3 wire-type mismatch, or a failed per-element allocation")
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
	// Every schema bound the LENGTH WORD already decides, latched at that word.
	g.emitFixlenBegin(f, fs, name)

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
	// Unconditional: corelib-zig only checks @hasDecl for the callback, it does NOT
	// skip the subtree on its own (istream.zig T_SEQUENCE_START), so a visitor
	// without sequenceBegin would let an unknown sequence's children arrive with
	// `cur` still on the enclosing scope and bind there (generator#268 / F-0044).
	g.emitSequence(f, fs, name)
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
			return guardedStore(widthGuard(fld.Kind), fmt.Sprintf("%s = %s", acc, storeCast(numZigType(fld.Kind), "value")))
		case fld.Kind == ir.KindEnum:
			return fmt.Sprintf("%s = %s", acc, storeCast(enumBacking(fld.Ref.Target), "value"))
		case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
			return g.putCall(fr, fld, widthGuard(fld.Elem), storeCast(numZigType(fld.Elem), "value"))
		case fld.Kind == ir.KindArray && fld.Elem == ir.KindEnum:
			return g.putCall(fr, fld, "", storeCast(enumBacking(fld.ElemRef.Target), "value"))
		}
		return ""
	}
	switch {
	case isUnsignedElem(fld.Kind):
		return guardedStore(widthGuard(fld.Kind), fmt.Sprintf("%s = %s", acc, storeCast(numZigType(fld.Kind), "value")))
	case fld.Kind == ir.KindBool:
		return fmt.Sprintf("%s = value != 0", acc)
	case fld.Kind == ir.KindBitfield:
		return fmt.Sprintf("%s = %s", acc, storeCast(bitfieldBacking(fld.Ref.Target), "value"))
	case fld.Kind == ir.KindArray && isUnsignedElem(fld.Elem):
		return g.putCall(fr, fld, widthGuard(fld.Elem), storeCast(numZigType(fld.Elem), "value"))
	case fld.Kind == ir.KindArray && fld.Elem == ir.KindBool:
		return g.putCall(fr, fld, "", "value != 0")
	case fld.Kind == ir.KindArray && fld.Elem == ir.KindBitfield:
		return g.putCall(fr, fld, "", storeCast(bitfieldBacking(fld.ElemRef.Target), "value"))
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
	// §7.3 fill guard (generator#188), plus the placement guard: the row lives at
	// the index arrayBegin recorded, not at the end of the outer slice, and that
	// index is only addressable if the row's allocation succeeded. The §7.1 width
	// guard sits inside the fill guard for the same reason it does in putCall.
	return fmt.Sprintf("{ if (self.afill != 0) { self.afill -= 1; %sif (self.%s < %s.len) sofab.arrays.putGrowing(sofab.arrays.at(%s, self.%s), self.alloc, &self.ai, self.an, %s); } }", widthGuard(fr.elemKind), fr.idx, fr.path, fr.path, fr.idx, cast)
}

// emitArraySkipArm arms the §7.3 discard counter in arrayBegin (generator#183,
// extended to fp by generator#193). Every array kind whose elements land in a
// callback a scalar shares is armed: integers under .unsigned/.signed, fp under
// .fp32/.fp64 by element subtype (generator#259 / F-0042). Every (scope, id) that
// genuinely declares a native array of the matching element kind disarms it
// (=> 0), so a legitimate array stores normally; everything else — a
// scalar-declared id, an unknown id, an fp64 array at a declared fp32 id —
// discards exactly `count` elements, after which a real scalar at the same id
// still decodes. Mirrors emitArrayFillArm.

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
// arrays under the prong for their own element subtype, .fp32 or .fp64
// (generator#259) — and 0 elsewhere, so a bare scalar at an array id
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
	// ArrayKind is exactly {unsigned, signed, fp32, fp64}; the three prongs below
	// cover all four, leaving no room for an else prong (Zig rejects an
	// unreachable one). The fp prongs are keyed by ELEMENT SUBTYPE
	// (generator#259): a declared fp32[N] arms its fill only under .fp32, so an
	// fp64 header at that id falls to `else => 0` and its elements are discarded
	// by the skip counter instead of being stored into the wrong field.
	f.line("        self.afill = switch (kind) {")
	// One arm per wire kind, never a collapsed integer family: a declared `i8[]`
	// must disarm only for .signed, so an .unsigned header at that id is skipped
	// AND leaves the fill counter at 0 -- otherwise the NEXT bare scalar is
	// absorbed into the array (generator#270 / Crucible F-0045).
	emit(".unsigned", wantUnsignedArrayElem)
	emit(".signed", wantSignedArrayElem)
	emit(".fp32", func(k ir.Kind) bool { return k == ir.KindFP32 })
	emit(".fp64", func(k ir.Kind) bool { return k == ir.KindFP64 })
	f.line("        };")
}

// wantUnsignedArrayElem / wantSignedArrayElem split the integer element kinds by
// the wire ARRAY KIND they map to (§1): signed integers and enum travel as
// .signed, unsigned integers, bool and bitfield as .unsigned. Keeping them apart
// is what makes the §7.3 kind check decide before the counters are armed
// (generator#270).
func wantUnsignedArrayElem(k ir.Kind) bool {
	return isNativeArrayElem(k) && !wantSignedArrayElem(k) && k != ir.KindFP32 && k != ir.KindFP64
}

func wantSignedArrayElem(k ir.Kind) bool {
	return isSignedElem(k) || k == ir.KindEnum
}

func (g *gen) emitArraySkipArm(f *zfile, fs []frame, arrSkip bool) {
	if !arrSkip {
		return
	}
	use := visitorUseOf(fs)
	emit := func(kinds string, drained bool, want func(ir.Kind) bool) {
		// A wire kind whose element callback this visitor does not declare is
		// never delivered at all: corelib-zig guards every element call on
		// @hasDecl, so those elements go nowhere and there is nothing to
		// discard. Arming the counter for such a kind is not merely redundant,
		// it is WRONG -- nothing can drain it, so the next field of a kind that
		// IS declared drains it and is swallowed. Silent data loss on a fully
		// bounded schema: a signed array at an `array<u32>` id armed askip, no
		// signed() existed to spend it, and the u32 scalar that followed was
		// eaten, decoding COMPLETE with the wrong value (audit 2026-09-01).
		if !drained {
			f.line("            %s => 0,", kinds)
			return
		}
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
	// ArrayKind is exactly {unsigned, signed, fp32, fp64}; the three prongs below
	// cover all four, leaving no room for an else prong (Zig rejects an
	// unreachable one), mirroring emitArrayFillArm. Keying the fp prongs by
	// element subtype (generator#259) is what arms the discard counter for an
	// fp64 array arriving at a declared fp32 field: that id disarms only under
	// .fp32, so under .fp64 it takes `else => count` and the array is skipped
	// whole, exactly like one at an unknown id.
	f.line("        self.askip = switch (kind) {")
	// One arm per wire kind, never a collapsed integer family: a declared `i8[]`
	// must disarm only for .signed, so an .unsigned header at that id is skipped
	// AND leaves the fill counter at 0 -- otherwise the NEXT bare scalar is
	// absorbed into the array (generator#270 / Crucible F-0045).
	emit(".unsigned", use.unsigned, wantUnsignedArrayElem)
	emit(".signed", use.signed, wantSignedArrayElem)
	emit(".fp32", use.fp32, func(k ir.Kind) bool { return k == ir.KindFP32 })
	emit(".fp64", use.fp64, func(k ir.Kind) bool { return k == ir.KindFP64 })
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
			body := fmt.Sprintf("if (self.%s < %s.len) sofab.arrays.putGrowing(sofab.arrays.at(%s, self.%s), self.alloc, &self.ai, self.an, value)", fr.idx, fr.path, fr.path, fr.idx)
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
				fa.arms = append(fa.arms, fmt.Sprintf("%d => %s,", fld.ID, g.putCall(fr, fld, "", "value")))
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

// emitFixlenBegin latches every schema bound a fixlen field's LENGTH WORD already
// decides, at that word (CORELIB_PLAN §5.2, generator#267).
//
// The bounds are not new -- a scalar/element `maxlen` and a wrapper element's
// `id >= count` were both already rejected -- but in the payload callback, which
// only fires once payload bytes arrive. A message truncated immediately after the
// length word never reached them and reported INCOMPLETE, while the same bytes
// read whole are INVALID; §5.2 makes INVALID dominate INCOMPLETE because the
// violation is already established by the bytes seen. corelib-zig#37 added the
// hook.
//
// Zig's hook is the only one in the family that RETURNS AN ERROR rather than
// setting a sticky flag, so the reject is `return error.InvalidMessage` here
// instead of `self.inv = true`. Both surface as INVALID; the corelib fails the
// field on a raised error, which is exactly the latch this needs.
//
// Everything sits inside the DECLARED-subtype switch: the hook fires for whatever
// subtype arrived at a field id, and a contradicting one is a §7.3 skip rather
// than this field's length (#224/#259, one position over).
//
// The payload-side guards stay -- unreachable for a message that gets this far,
// and the only thing still bounding a consumer built against an older corelib.
func (g *gen) emitFixlenBegin(f *zfile, fs []frame, name string) {
	str := g.fixlenBeginArms(fs, ir.KindString)
	blob := g.fixlenBeginArms(fs, ir.KindBlob)
	if len(str) == 0 && len(blob) == 0 {
		return
	}
	// Zig rejects an unused function parameter, and not every schema uses both:
	// an array with a `count` but no element `maxlen` reads `id` and never
	// `total`. Name each parameter only when some arm below actually reads it --
	// the same rule emitPayloadVisit follows for its own `total`.
	idP, totalP := "_", "_"
	for _, a := range append(append([]string{}, str...), blob...) {
		if strings.Contains(a, "id ") || strings.Contains(a, "(id)") {
			idP = "id"
		}
		if strings.Contains(a, "total ") {
			totalP = "total"
		}
	}
	f.blank()
	f.line("    /// Latch a schema bound at the fixlen LENGTH WORD, before any payload byte.")
	f.line("    /// S5.2 makes INVALID dominate INCOMPLETE, so truncating right after this")
	f.line("    /// word must not downgrade the verdict. The subtype switch is S7.3 -- a")
	f.line("    /// contradicting fixlen kind at this id is a SKIPPED field, not this")
	f.line("    /// field's length.")
	f.line("    pub fn fixlenBegin(self: *_dec_%s, %s: sofab.Id, subtype: sofab.FixlenType, %s: usize) sofab.Error!void {", name, idP, totalP)
	f.line("        switch (subtype) {")
	for _, a := range []struct {
		variant string
		arms    []string
	}{{"string", str}, {"blob", blob}} {
		if len(a.arms) == 0 {
			continue
		}
		f.line("            .%s => switch (self.cur) {", a.variant)
		for _, arm := range a.arms {
			f.line("%s", arm)
		}
		f.line("                else => {},")
		f.line("            },")
	}
	f.line("            else => {},")
	f.line("        }")
	f.line("    }")
}

// overIndexCond is the test that bounds a wrapper array's element INDEX, and the
// verdict that test carries.
//
// A wrapper array carries no count HEADER: its elements are keyed by an
// unbounded varint index and the destination grows to id + 1, so the index IS
// the array's length (MESSAGE_SPEC §5.1 -- two elements at id 0 and id 16383 are
// a 16384-slot slice). A single over-index element is therefore an amplification
// vector by itself, and it is the INDEX that has to be bounded: capping how many
// elements arrived would not bound the allocation, because a sparse array
// allocates by its highest id.
//
// Which bound applies depends on whether the schema counts the array, and the
// two differ only in that and in what the failure is called (ARCHITECTURE §9.5):
// `count: N` makes id >= N INVALID (the bytes contradict the agreed schema,
// issue #142), no count makes id >= max_dyn_array_count LimitExceeded (the bytes
// are well formed and the same message decodes under a looser cap, issue #387 --
// folding the two together is forbidden by CORELIB_PLAN §6.2.1).
//
// Only ONE of the two is ever emitted here, and which one decides where the
// comparison runs. A schema `count` is generated code's: the caller emits this
// test in front of the corelib call, and callers differ in how they REFUSE -- a
// sticky flag, an error return, a break to the dead scope -- so this returns the
// test and the category and lets each spell its own refusal. A receiver cap is
// corelib-zig's: `overLimit` true means the caller emits NO test and passes
// max_dyn_array_count to the capped helper (`growCapped` / `setElemCapped`)
// instead, which compares it before it extends the destination and answers
// error.LimitExceeded. CORELIB_PLAN §6.2.1 permits that site and requires it be
// the only one -- a caller that emitted this condition too would be the "two
// routes to one rule" the section forbids.
//
// ok is false only when the array is dynamic AND no cap is live for this schema.
func (g *gen) overIndexCond(cap int64) (cond string, overLimit, ok bool) {
	if cap >= 0 {
		return fmt.Sprintf("id >= %d", cap), false, true
	}
	if !g.msgLim {
		return "", false, false
	}
	return "id >= max_dyn_array_count", true, true
}

// fixlenBeginArms builds the per-scope arms for one fixlen subtype. A wrapper
// element carries its array's SCHEMA over-index bound AND its element length
// bound, in that order: an element that is not this array's element at all must
// not have its length measured against the element bound. A scalar field carries
// its own length bound, keyed by field id inside its scope.
//
// The array's RECEIVER cap on the index is not here: it is passed to the
// corelib call that grows the destination (`setElemCapped`) and compared there,
// so this hook emits no test for it (CORELIB_PLAN §6.2.1, overIndexCond).
//
// The length bound is the schema `maxlen` where the schema declares one
// (INVALID, MESSAGE_SPEC §7.1) and the configured receiver cap where it does not
// (LimitExceeded, CORELIB_PLAN §6.2.1). The two are mutually exclusive by
// construction -- §6.2.1 forbids applying a cap to a field the schema already
// bounds -- and lenBound returns whichever governs.
func (g *gen) fixlenBeginArms(fs []frame, kind ir.Kind) []string {
	// lenBound: the test a fixlen LENGTH WORD faces at this field/element, and
	// the error it raises. ok is false when neither a schema maxlen nor a live
	// receiver cap governs the length.
	capActive, capName := g.limits.stringHas, "max_dyn_string_len"
	if kind == ir.KindBlob {
		capActive, capName = g.limits.blobHas, "max_dyn_blob_len"
	}
	lenBound := func(maxlen int64, dynLen bool) (stmt string, ok bool) {
		if maxlen >= 0 {
			return fmt.Sprintf("if (total > %d) return sofab.Error.InvalidMessage;", maxlen), true
		}
		if capActive && dynLen {
			return fmt.Sprintf("if (total > %s) return sofab.Error.LimitExceeded;", capName), true
		}
		return "", false
	}
	var arms []string
	for _, fr := range fs {
		idCond, idLim, idOK := g.overIndexCond(fr.cap)
		elemLen, elemLenOK := lenBound(fr.emax, fr.elemDynLen)
		// Only the SCHEMA count is latched here. A receiver cap on an unbounded
		// array is the corelib's comparison now, made inside the setElemCapped
		// this element's payload callback issues (CORELIB_PLAN §6.2.1), and
		// §6.2.1's "one implementation, wherever it runs" forbids emitting it a
		// second time in generated code. It still lands before the destination is
		// sized, which is the enforcement point §6.2.1 fixes for a wrapper array:
		// "the element index, checked before the container it indexes into is
		// extended". The schema bound cannot move with it — MESSAGE_SPEC §7.1 is a
		// validity verdict, and §5.2.3 wants it decided at the length word so a
		// message truncated right after it is INVALID, not INCOMPLETE.
		//
		// The element LENGTH bound is untouched by that move and stays here for
		// both flavours (#438): its receiver cap has no corelib call of its own to
		// ride, and deciding it at the length word is what keeps a truncated
		// over-cap payload LimitExceeded rather than INCOMPLETE.
		latchIdx := idOK && !idLim
		if fr.kind == fkSeqArr && fr.elemKind == kind && (latchIdx || elemLenOK) {
			body := ""
			if latchIdx {
				body += fmt.Sprintf("if (%s) return sofab.Error.InvalidMessage; ", idCond)
			}
			if elemLenOK {
				body += elemLen + " "
			}
			arms = append(arms, fmt.Sprintf("                .%s => { %s},", fr.loc, body))
			continue
		}
		if fr.kind != fkStruct {
			continue
		}
		var inner []string
		for _, fld := range fr.fields {
			if fld.Kind != kind {
				continue
			}
			maxlen := int64(-1)
			if fld.HasMaxlen {
				maxlen = fld.Maxlen
			}
			// A scalar string/blob the schema leaves unbounded is exactly the
			// field a receiver cap governs, so it is dyn-length by definition.
			if stmt, ok := lenBound(maxlen, true); ok {
				inner = append(inner, fmt.Sprintf("%d => %s", fld.ID, strings.TrimSuffix(stmt, ";")+","))
			}
		}
		if len(inner) > 0 {
			arm := fmt.Sprintf("                .%s => switch (id) { ", fr.loc)
			arm += strings.Join(inner, " ")
			arm += " else => {}, },"
			arms = append(arms, arm)
		}
	}
	return arms
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
	// exposes `utf8Valid(bytes)` and generated code emits an UNCONDITIONAL call to
	// it at the materialization site — the SOFAB_STRICT_UTF8 gate lives inside the
	// primitive (folds to true when compiled off), so this code is identical across
	// build configs. Invalid UTF-8 is the INVALID outcome (self.inv). `blob` is
	// opaque bytes and is stored verbatim. Skipped fields hit the switch `else`
	// arms and are never validated (§6.4). mat() wraps only the materialization.
	mat := func(store string) string {
		if kind != ir.KindString {
			return store
		}
		return "if (!sofab.utf8Valid(chunk)) { self.inv = true; } else { " + store + " }"
	}
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind {
			// The element INDEX bound. A SCHEMA `count` is a validity statement and
			// stays a generated INVALID guard (below); the receiver cap on an
			// unbounded array rides the placement call itself — setElemCapped
			// compares `id` against it and refuses before it grows the destination
			// (CORELIB_PLAN §6.2.1, "a corelib MAY take a limit as an argument and
			// perform the check itself"). One implementation, in one place: with the
			// cap passed in, generated code emits no index guard for this array at
			// all, here or at the element's length word (fixlenBeginArms).
			idCond, idLim, idOK := g.overIndexCond(fr.cap)
			capped := idOK && idLim
			set := fmt.Sprintf("sofab.arrays.setElem([]const u8, self.alloc, &(%s), id, \"\", chunk)", fr.path)
			if capped {
				set = fmt.Sprintf("sofab.arrays.setElemCapped([]const u8, self.alloc, &(%s), id, \"\", chunk, max_dyn_array_count) catch { self.lim = true; }", fr.path)
			}
			// stmt is the placement as a single statement (trailing ;), for use
			// inside an { ... } block; body is the raw arm expression. For a string
			// element the materialization is UTF-8-validated (mat); blob is verbatim.
			stmt := mat(set + ";")
			body := set
			if kind == ir.KindString {
				body = stmt
			} else if capped {
				// `x catch { ... }` is a statement, not an arm expression: brace it.
				body = "{ " + stmt + " }"
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
			// The schema `count`: an element id at or past it is INVALID
			// (MESSAGE_SPEC §7.1), decided here, before setElem grows the slice —
			// the index IS the array's length, so this is what bounds an
			// over-index heap amplification (see overIndexCond). The receiver-cap
			// case took the setElemCapped route above and emits nothing here.
			if idOK && !idLim {
				body = fmt.Sprintf("if (%s) { self.inv = true; } else { %s }", idCond, stmt)
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
	// `total` is always bound now: the reassembly preamble needs it to tell a
	// whole payload from the first piece of a split one, whether or not any arm
	// compares it against a maxlen.
	_ = totalUsed
	f.blank()
	f.line("    pub fn %s(self: *_dec_%s, id: sofab.Id, total: usize, offset: usize, _chunk: []const u8) void {", cb, name)
	// The corelib delivers a payload in as many pieces as the feed chunks split
	// it into. Rebind `chunk` to the WHOLE payload so every arm below sees one
	// contiguous slice and none of them has to know about chunking.
	f.line("        const chunk = self._reassemble(total, offset, _chunk) orelse return;")
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
// as elements actually arrive), and append a fresh inner slice for a
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
					// The store-side push bound only fires when the N+1th element
					// actually arrives, which a truncated over-count array never reaches.
					guard := fmt.Sprintf("if (count > %d) { self.inv = true; return; }", n)
					// Then the value is cleared: the wire count M IS the array's length
					// (§3), so the value is exactly what this array header delivers --
					// an explicitly empty one (count 0) decodes to the EMPTY array, not
					// to the previous value and not to N element defaults. The stores
					// refill it from here (see putCall); the spare capacity past the
					// length is not part of the value, so it needs no clearing.
					//
					// Gated on the announced wire KIND, so a header whose kind
					// contradicts the declared element type is skipped like an unknown
					// id and a correctly typed earlier occurrence survives it (§7.3,
					// §7.4) -- this arm must not clear an array it will not refill.
					//
					// For a fixlen array the kind names the ELEMENT SUBTYPE (.fp32 /
					// .fp64), not just "fixlen" (CORELIB_PLAN §4.8, generator#259).
					// That matters for where the over-count guard sits: it is INSIDE
					// the kind test, so an fp64 header at a declared fp32[N] slot is
					// never measured against N. Bounding first would turn a skippable
					// contradiction into INVALID -- §7.3 decides the subtype BEFORE
					// any schema bound, and only a field that survives that test is
					// bounded at all.
					arm := fmt.Sprintf("if (kind == .%s) { %s %s.%s.clear(); }",
						wireArrayKind(fld.Elem), guard, fr.path, zigIdent(fld.Name))
					fa.arms = append(fa.arms, fmt.Sprintf("%d => %s,", fld.ID, arm))
					// The guard reads the wire count; the switch reads id.
					idUsed, countUsed = true, true
					continue
				}
				if g.dynNativeArray(fld) {
					idUsed, countUsed = true, true
					elem := g.zigArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems)
					if !g.limits.arrayHas {
						panic("zig: count-less native array with no cap -- every target has a finite default (§9.5)")
					}
					// A count-less array is always unbounded, so every direct
					// dynamic native array is capped. The cap is passed INTO the
					// allocation it exists to prevent: allocNCapped compares the
					// wire count against it and refuses before it allocates
					// (CORELIB_PLAN §6.2.1 -- "a corelib MAY take a limit as an
					// argument and perform the check itself"), so generated code
					// emits no guard of its own and the rule has one implementation.
					// The number is still ours: it is a constant of this file,
					// passed per call and not retained anywhere.
					//
					// an = 0 drops the rejected array's elements, and the
					// destination keeps the value it had -- rejected, never
					// clamped, and never half-filled.
					//
					// A count that clears the cap is allocated at EXACTLY that
					// count, once (ARCHITECTURE §9.5, shape A). The capped
					// reservation this replaces -- sofab.arrays.allocCapped, grown
					// by putGrowing -- existed because nothing had bounded the
					// count yet; the cap bounds it, so the reservation only added
					// doubling and copies.
					body := fmt.Sprintf("%s.%s = sofab.arrays.allocNCapped(%s, self.alloc, count, max_dyn_array_count) catch { self.lim = true; self.an = 0; return; };",
						fr.path, zigIdent(fld.Name), elem)
					// A count-less array has no schema bound to misapply, but it
					// still must not ALLOCATE from a header that is being skipped
					// (CORELIB_PLAN §4.8 / MESSAGE_SPEC §7.3, generator#259). The
					// wire count of a contradicting header is not this field's
					// length, so sizing from it is an eager allocation for elements
					// that will never arrive -- the same hazard the receiver-limit
					// work closed for legitimate headers. Gate on the declared
					// element kind, exactly like the counted arm above.
					fa.arms = append(fa.arms, fmt.Sprintf("%d => if (kind == .%s) { %s },", fld.ID, wireArrayKind(fld.Elem), body))
				}
			}
			if len(fa.arms) > 0 {
				all = append(all, fa)
			}
		case fkNestedNative:
			// A row's element id IS its index in the outer array (MESSAGE_SPEC
			// §5.1), so the row is PLACED at that index — never appended. Appending
			// was unreachable while every row was framed; the §2 interior-sparse
			// rule makes an omitted all-default row reachable, and an appending
			// collector then shifts every later row down by one. The outer array's
			// `count` bounds the id (an id >= N is INVALID, §5.1/§7), which also
			// bounds the id-keyed gap-fill against an over-index amplification.
			inner := strings.TrimPrefix(fr.elemType, "[]const ")
			// A ROW's own element count needs its own bound -- fr.cap bounds the
			// row's ID, never how many elements the row claims. A row the schema
			// counts is bounded by that count (INVALID above it, §7.1), decided by
			// generated code before it calls; one the schema leaves unbounded is
			// bounded by the receiver cap, which is passed INTO allocNCapped and
			// compared there (CORELIB_PLAN §6.2.1). Either way the row is sized at
			// exactly the announced count only once that count has been bounded
			// (§9.5, shape A).
			rowAlloc := fmt.Sprintf("sofab.arrays.at(%s, id).* = sofab.arrays.allocN(%s, self.alloc, count);", fr.path, inner)
			if fr.elemCap < 0 {
				if !g.limits.arrayHas {
					panic("zig: unbounded native row with no cap -- every target has a finite default (§9.5)")
				}
				rowAlloc = fmt.Sprintf("sofab.arrays.at(%s, id).* = sofab.arrays.allocNCapped(%s, self.alloc, count, max_dyn_array_count) catch { self.lim = true; self.an = 0; return; };",
					fr.path, inner)
			}
			// The row's ID, bounded before the outer slice grows to hold it. A
			// schema `count` is again generated code's (INVALID); the receiver cap
			// rides growCapped, which refuses the index before it extends the
			// destination -- §6.2.1's enforcement point for an array with no count
			// header, and the site the cap now has ONE implementation at.
			idCond, idLim, idOK := g.overIndexCond(fr.cap)
			grow := fmt.Sprintf("sofab.arrays.grow(%s, self.alloc, &(%s), @as(usize, id) + 1, &.{})", fr.elemType, fr.path)
			if idOK && idLim {
				grow = fmt.Sprintf("sofab.arrays.growCapped(%s, self.alloc, &(%s), @as(usize, id) + 1, &.{}, max_dyn_array_count) catch { self.lim = true; self.an = 0; return; }",
					fr.elemType, fr.path)
			}
			body := fmt.Sprintf("{ self.%s = id; if (%s) { %s } }", fr.idx, grow, rowAlloc)
			if fr.elemCap >= 0 {
				body = fmt.Sprintf("if (count > %d) { self.inv = true; self.an = 0; } else %s", fr.elemCap, body)
			}
			if idOK && !idLim {
				body = fmt.Sprintf("if (%s) { self.inv = true; self.an = 0; } else %s", idCond, body)
			}
			// Same rule as the leaf arms: a row is grown and sized only for a
			// header whose element kind matches the one this row declares. A
			// contradicting header is skipped whole, so it must not grow the outer
			// slice nor size the row it would land in (generator#259).
			body = fmt.Sprintf("if (kind == .%s) %s", wireArrayKind(fr.elemKind), body)
			idUsed, countUsed = true, true
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
	if visitorUseOf(fs).dynAlloc {
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

// wireArrayKind is the corelib ArrayKind a native array element type is
// delivered under: integers (and bool, enum, bitfield) as .unsigned / .signed by
// signedness of the backing type, fp as .fp32 / .fp64 by element width.
//
// The fp kinds name the ELEMENT SUBTYPE, not merely "a fixlen array"
// (CORELIB_PLAN §4.8, generator#259 / Crucible F-0042). A fixlen array's count
// word precedes its fixlen_word, so a receiver that only learns "fixlen" cannot
// tell a contradicting fp64 array from a declared fp32 array's value, and would
// apply the declared field's schema bound to an array it must instead skip
// (MESSAGE_SPEC §7.3). corelib-zig fires arrayBegin past the fixlen_word and
// reports .fp32 / .fp64, so the id arm can key on the subtype.
func wireArrayKind(elem ir.Kind) string {
	switch {
	case elem == ir.KindFP32:
		return "fp32"
	case elem == ir.KindFP64:
		return "fp64"
	case isSignedElem(elem) || elem == ir.KindEnum:
		return "signed"
	default: // unsigned numeric, bool, bitfield
		return "unsigned"
	}
}

// emitSequence emits sequenceBegin/sequenceEnd: push the current location and
// descend. Wrapper-array fields reset their slice on entry (an explicit empty
// wrapper must override a non-empty value); struct/nested-array element frames
// grow their slice to id + 1 and descend into the element that id names.
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
			// The element id IS the array index (MESSAGE_SPEC §5.1), exactly as for
			// the string/blob leaf elements sofab.arrays.setElem places: grow to
			// id + 1 — default-filling the gaps left by omitted elements — record
			// the index, and descend INTO that element. Appending would shorten the
			// array by the size of any interior id gap and would decode a REOPENED
			// id as a second element instead of merging into the first (§7.4), which
			// placement gives for free. A failed allocation drops the subtree.
			idUsed = true
			var b strings.Builder
			b.WriteString("blk: {\n")
			// Bound the element INDEX before the destination grows, which is what
			// bounds the gap-fill against an over-index heap amplification (see
			// overIndexCond). A schema `count` is a validity bound and is decided
			// here (INVALID, MESSAGE_SPEC §7.1); the receiver cap on an unbounded
			// array is handed to growCapped instead, which compares it and refuses
			// before it extends anything -- the enforcement point §6.2.1 names for
			// an array with no count header, and the rule's single implementation.
			idCond, idLim, idOK := g.overIndexCond(fr.cap)
			if idOK && !idLim {
				fmt.Fprintf(&b, "                if (%s) { self.inv = true; break :blk .dead; }\n", idCond)
			}
			if idOK && idLim {
				fmt.Fprintf(&b, "                if (!(sofab.arrays.growCapped(%s, self.alloc, &(%s), @as(usize, id) + 1, %s, max_dyn_array_count) catch { self.lim = true; break :blk .dead; })) break :blk .dead;\n",
					fr.elemType, fr.path, fr.elemFill)
			} else {
				fmt.Fprintf(&b, "                if (!sofab.arrays.grow(%s, self.alloc, &(%s), @as(usize, id) + 1, %s)) break :blk .dead;\n",
					fr.elemType, fr.path, fr.elemFill)
			}
			fmt.Fprintf(&b, "                self.%s = id;\n", fr.idx)
			fmt.Fprintf(&b, "                break :blk .%s;\n", fr.elemLoc)
			b.WriteString("            }")
			all = append(all, frameArms{fr: fr, body: b.String()})
		}
	}
	// The string/blob reassembly helper. Emitted next to the callbacks that use
	// it so the whole chunk-boundary story sits in one place.
	f.blank()
	f.line("    /// Give the string/blob callbacks ONE contiguous payload, whatever the")
	f.line("    /// feed chunking was. Returns null while the payload is still incomplete.")
	f.line("    ///")
	f.line("    /// On the contiguous decode() path a payload always arrives whole and is")
	f.line("    /// returned as-is: the destination borrows the caller's buffer and nothing")
	f.line("    /// is copied. That buffer is the one the caller handed to decode(), so the")
	f.line("    /// borrow is sound for exactly as long as the documented contract says.")
	f.line("    ///")
	f.line("    /// The streaming path (`own`) borrows NOTHING, whether or not the payload")
	f.line("    /// arrived whole. A payload stitched across a chunk boundary is completed")
	f.line("    /// inside the corelib's fixed, REUSED carry buffer, and the slice handed")
	f.line("    /// over then points into the decoder itself -- indistinguishable in the")
	f.line("    /// callback from a slice into the caller's chunk, and overwritten by the")
	f.line("    /// next stitched item. Borrowing on that path aliased earlier fields and")
	f.line("    /// elements onto later ones at particular chunk sizes.")
	f.line("    ///")
	f.line("    /// A split payload is stitched by sofab.PayloadAcc, which hands it back")
	f.line("    /// as its own allocation -- a destination KEEPS the slice it is given.")
	f.line("    fn _reassemble(self: *_dec_%s, total: usize, offset: usize, chunk: []const u8) ?[]const u8 {", name)
	f.line("        if (offset == 0 and chunk.len >= total) {")
	f.line("            if (!self.own) return chunk; // contiguous decode: borrow the caller's buffer")
	f.line("            return self.alloc.dupe(u8, chunk[0..total]) catch { self.inv = true; return null; };")
	f.line("        }")
	// generator#295: the branch above used to borrow on BOTH paths. corelib-zig
	// stitches an item that straddles a feed boundary into a fixed `carry` buffer
	// and parses out of it, so a payload completing inside that stitch is handed
	// over as a slice into the IStream -- which the next stitched item overwrites.
	// Measured with pointer instrumentation: at chunk size 4 two of four elements
	// were delivered from the carry buffer at the SAME address. Copying on the
	// streaming path is what makes the destination independent of both the carry
	// buffer and the caller's chunk lifetime.
	//
	// The split path below owns its result for the same reason, plus the one
	// PayloadAcc documents (generator#293 / Crucible F-0058): its scratch is
	// reused by every following payload.
	f.line("        return self.acc.push(self.alloc, total, offset, chunk) catch { self.inv = true; return null; };")
	f.line("    }")

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
	if len(all) == 0 {
		// Nothing is declared as a sequence anywhere in this message, so every
		// sequence that arrives is unknown; the switch would carry only its else.
		f.line("        self.cur = .dead;")
		f.line("    }")
		f.blank()
		g.emitSequenceEnd(f, name)
		return
	}
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
		f.line("                else => .dead,")
		f.line("            },")
	}
	// The default arms are a SKIP, not "stay where you are". A sequence id the
	// schema does not declare in this scope -- an unknown id (§5.2/§4.9) or one
	// landing on a position declared as something else (§7.3) -- must be discarded
	// WHOLE, children included. Staying put let those children bind into the
	// enclosing scope (generator#268 / F-0044, generator#272 / F-0047). No depth
	// counter is needed: every begin pushes and every end pops, and a nested
	// sequence inside a dead subtree matches no arm either, so it stays .dead.
	f.line("            else => .dead,")
	f.line("        };")
	f.line("    }")
	f.blank()
	g.emitSequenceEnd(f, name)
}

func (g *gen) emitSequenceEnd(f *zfile, name string) {
	f.line("    pub fn sequenceEnd(self: *_dec_%s) void {", name)
	// Nothing to fill in on the way out: the wire count M IS a compact array's
	// length and the highest present element id IS a wrapper array's last index
	// (MESSAGE_SPEC §3/§5.1), so what arrived is the whole value. A declared
	// `count: N` is a capacity — it bounds the elements (see the id guards in
	// sequenceBegin) and never adds any.
	f.line("        if (self.sp > 0) {")
	f.line("            self.sp -= 1;")
	f.line("            self.cur = self.stack[self.sp];")
	f.line("        } else {")
	f.line("            self.cur = .root;")
	f.line("        }")
	f.line("    }")
}
