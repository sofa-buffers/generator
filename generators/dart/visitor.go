package dart

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// ---- decode visitor -------------------------------------------------------

// emitVisitor emits the push child-visitor for an object's id scope. Scalars
// bind straight into a member; native arrays arrive whole and are copied
// (exactly as long as the wire made them); nested structs/unions and every
// wrapper-sequence array descend via onSequenceStart into a child visitor. A
// struct/union descent returns the EXISTING member's visitor, so a re-opened
// scope merges (MESSAGE_SPEC §7.4); an array wrapper clears its list first, so a
// re-opened wrapper is replaced. Unhandled ids fall through: a leaf id lands in
// an unarmed switch (no-op) and a sequence id returns null (skip), which is what
// makes a contradictory wire type evaporate structurally (MESSAGE_SPEC §7.3).
func (g *gen) emitVisitor(f *dfile, typeName string, fields []*ir.Field) {
	var uns, sig, f32, f32bits, f64, str, blob []string
	var uArr, sArr, f32Arr, f64Arr []string
	var seq []string
	// HeaderVisitor hooks (corelib-dart onArrayBegin/onFixlenHeader): schema-bound
	// rejects at the count/length word, BEFORE the corelib's truncation check, so a
	// field that is BOTH over-bound and truncated is INVALID, not INCOMPLETE
	// (generator#216 / F-0032, MESSAGE_SPEC §5.2). The whole-value guards below
	// (onUnsignedArray/onString len checks) fire only once every element/byte has
	// arrived, so a truncated over-bound field never reaches them — the header hook
	// is what makes the over-bound win the tie. tryDecode already reads the sticky
	// invalidate() latches the verdict inside the corelib, which stops there.
	//
	// Both hooks fire for ANY wire kind/subtype landing on a field id — the corelib
	// resolves what arrived but cannot know what was DECLARED — so both arms gate
	// their bound on the declared kind: a contradicting header is a §7.3 skip and
	// must never be measured against this field's bound (generator#224 for
	// onFixlenHeader, generator#259 / F-0042 for onArrayBegin).
	var arrBegin, fixHdr []string
	// onBytesDest / onArrayDest: the DESTINATION hooks, and the guard for the one
	// shape a receiver cap never covered.
	//
	// corelib-dart's defaults allocate a destination sized from the wire count or
	// length -- exactly right for a hand-written visitor that wants every field,
	// and wrong for a schema-bound scope. MESSAGE_SPEC §7.3 makes an id this scope
	// does not declare, or one whose wire kind contradicts what it declares, a
	// SKIPPED field, and CORELIB_PLAN §6.2.1 says a skipped field is never capped
	// *because* it allocates nothing. That is only true if the scope says so.
	//
	// So both are ALWAYS overridden, even by a scope with no array and no
	// string/blob field: an id with no arm returns null and nothing is
	// materialized at all -- not "at most N elements" but none, which is a
	// tighter bound than any cap and the only one this shape has now that the
	// decoder holds none (corelib-dart#88).
	var arrDest, bytesDest []string
	destArm := func(id int64, test, call string) string {
		return fmt.Sprintf("      case %d:\n        if (%s) return super.%s;\n        return null;", id, test, call)
	}
	// onArrayElemBound (corelib-dart): the declared width of a native integer
	// array's ELEMENTS, handed to the decoder so it can apply the bound while the
	// elements go past. arrayWidthGuard below scans the assembled list, which is
	// exact for an array that arrives — and never runs for one that does not, so
	// a message cut short after an out-of-width element reported INCOMPLETE where
	// §5.2 requires INVALID (generator#267, Crucible F-0043 width_elem_trunc).
	// Same shape as the header hooks one level down.
	var elemBound []string

	arm := func(id int64, body string) string {
		return fmt.Sprintf("      case %d:\n        %s\n        return;", id, body)
	}
	seqArm := func(id int64, body string) string {
		return fmt.Sprintf("      case %d:\n        %s", id, body)
	}

	for _, fld := range fields {
		acc := "o." + dartIdent(fld.Name)
		switch fld.Kind {
		case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
			uns = append(uns, arm(fld.ID, widthGuard(fld.Kind)+acc+" = value;"))
		case ir.KindBool:
			uns = append(uns, arm(fld.ID, acc+" = value != 0;"))
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
			sig = append(sig, arm(fld.ID, widthGuard(fld.Kind)+acc+" = value;"))
		case ir.KindFP32:
			// onFp32 fires for a non-NaN value: bind it and drop any bits a prior
			// (re-opened, §7.4) NaN occurrence captured. onFp32Bits fires for a NaN:
			// capture the exact wire bits and widen a display double for element access.
			bitsAcc := "o." + fp32BitsField(fld.Name)
			f32 = append(f32, arm(fld.ID, acc+" = value;\n        "+bitsAcc+" = null;"))
			f32bits = append(f32bits, arm(fld.ID, bitsAcc+" = bits;\n        "+acc+" = _f32FromBits(bits);"))
		case ir.KindFP64:
			f64 = append(f64, arm(fld.ID, acc+" = value;"))
		case ir.KindString:
			// The corelib delivers RAW wire bytes and does not validate them: its
			// cursor cannot tell a field this visitor binds from one it skips, and a
			// skipped payload must never be inspected. So the destination is
			// resolved first -- by reaching this arm at all -- and only then are the
			// bytes checked and transcoded (CORELIB_PLAN §6.4, generator#257).
			// Braced because Dart switch cases share one scope: two string fields
			// in the same visitor would otherwise redeclare `s`.
			body := "{\n          final s = sofab.decodeUtf8Strict(bytes);\n          " +
				"if (s == null) { invalidate(); return; }\n          " +
				acc + " = s;\n        }"
			if fld.HasMaxlen {
				// A wire byte length above the schema maxlen is malformed input
				// (MESSAGE_SPEC §7.1) — reject as INVALID, never truncate. The raw
				// bytes ARE the wire length, so this needs no re-encode.
				body = fmt.Sprintf("if (bytes.length > %d) { invalidate(); return; }\n        %s", fld.Maxlen, body)
			}
			if hdr := g.maxlenHdrGuard("string", fld); hdr != "" {
				fixHdr = append(fixHdr, arm(fld.ID, hdr))
			}
			bytesDest = append(bytesDest, destArm(fld.ID, "subtype == sofab.FixlenType.string", "onBytesDest(id, subtype, total)"))
			str = append(str, arm(fld.ID, body))
		case ir.KindBlob:
			// value aliases the decode buffer — copy what we keep.
			body := acc + " = Uint8List.fromList(value);"
			if fld.HasMaxlen {
				body = fmt.Sprintf("if (value.length > %d) { invalidate(); return; }\n        %s", fld.Maxlen, body)
			}
			if hdr := g.maxlenHdrGuard("blob", fld); hdr != "" {
				fixHdr = append(fixHdr, arm(fld.ID, hdr))
			}
			bytesDest = append(bytesDest, destArm(fld.ID, "subtype == sofab.FixlenType.blob", "onBytesDest(id, subtype, total)"))
			blob = append(blob, arm(fld.ID, body))
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, seqArm(fld.ID, fmt.Sprintf("return %s(%s);", visitorName(g.typeName(fld.Ref.Key)), acc)))
		case ir.KindArray:
			if nativeArrayElem(fld.Elem) {
				arrDest = append(arrDest, destArm(fld.ID,
					"kind == sofab.ArrayKind."+wireArrayKind(fld.Elem), "onArrayDest(id, kind, count)"))
			}
			g.emitArrayDecode(fld, acc, arm, seqArm, &uArr, &sArr, &f32Arr, &f64Arr, &seq, &arrBegin, &elemBound)
		}
	}

	f.line("class %s extends %s {", visitorName(typeName), visitorBase)
	f.line("  %s(this.o);", visitorName(typeName))
	f.line("  final %s o;", typeName)
	emitSwitch(f, "void onUnsigned(int id, int value)", uns)
	emitSwitch(f, "void onSigned(int id, int value)", sig)
	emitSwitch(f, "void onFp32(int id, double value)", f32)
	// onFp32Bits is delivered (instead of onFp32) for an fp32 field whose payload
	// is a NaN, carrying the raw 32 bits so a signaling/payload NaN survives §4.6.
	emitSwitch(f, "void onFp32Bits(int id, int bits)", f32bits)
	emitSwitch(f, "void onFp64(int id, double value)", f64)
	// A scope with no string destination emits NOTHING here and inherits the
	// no-op on sofab.VisitorBase, which is what makes an undeclared string a skip
	// rather than a validated payload (generator#265). It must never fall through
	// to sofab.MessageVisitor's validating default.
	emitSwitch(f, "void onStringBytes(int id, Uint8List bytes)", str)
	emitSwitch(f, "void onBlob(int id, Uint8List value)", blob)
	emitSwitch(f, "void onUnsignedArray(int id, Int64List values)", uArr)
	emitSwitch(f, "void onSignedArray(int id, Int64List values)", sArr)
	emitSwitch(f, "void onFp32Array(int id, Float32List values)", f32Arr)
	emitSwitch(f, "void onFp64Array(int id, Float64List values)", f64Arr)
	// Header hooks fire at the count/length word before the truncation check
	// (generator#216). Emitted only when a field declares a bound, so a type with
	// none does not override them and the corelib's max-speed path is unchanged.
	emitSwitch(f, "void onArrayBegin(int id, sofab.ArrayKind kind, int count)", arrBegin)
	emitSwitch(f, "void onFixlenHeader(int id, int subtype, int length)", fixHdr)
	// The element bound is asked once per array, at the count word, and applied
	// by the decoder per element — the position arrayWidthGuard cannot reach for
	// an array that never completes (generator#267).
	emitSwitchRet(f, "sofab.ElemRange? onArrayElemBound(int id, sofab.ArrayKind kind)", elemBound, "return null;")
	// Always emitted, arms or none: an id this scope does not bind gets NO
	// destination, so a §7.3-skipped array or payload is never materialized.
	emitDestSwitch(f, "Uint8List? onBytesDest(int id, int subtype, int total)", bytesDest)
	emitDestSwitch(f, "TypedData? onArrayDest(int id, sofab.ArrayKind kind, int count)", arrDest)
	// onSequenceStart is ALWAYS overridden: the base returns `this` (descend),
	// which would misread an unknown nested sequence as this object's fields.
	// Returning null skips any unhandled sequence (forward-compat + §7.3).
	f.line("  @override")
	f.line("  sofab.MessageVisitor? onSequenceStart(int id) {")
	if len(seq) > 0 {
		f.line("    switch (id) {")
		for _, a := range seq {
			f.line("%s", a)
		}
		f.line("    }")
	}
	f.line("    return null;")
	f.line("  }")
	f.line("}")
	f.blank()
}

// maxlenHdrGuard is the onFixlenHeader arm body rejecting a string/blob whose
// wire byte length exceeds the schema maxlen as INVALID, at the length word
// (generator#216). The bound is gated on the wire `subtype` matching the field's
// declared one: onFixlenHeader fires for ANY fixlen subtype at a field id (the
// corelib resolves the subtype but cannot know the DECLARED one — that is schema
// knowledge only the generated code has), and a fixlen value whose subtype
// contradicts the declaration must be SKIPPED, not measured against this field's
// maxlen (MESSAGE_SPEC §7.3, generator#224). Without the gate an fp64 (8 bytes)
// landing on a `blob` with `maxlen: 4` was rejected as INVALID instead of skipped.
// The payload callbacks (onString/onBlob) are already subtype-dispatched by the
// corelib, so only this pre-dispatch hook needs the explicit check.
// TWO bounds land here and they are mutually exclusive by rule: a field the
// schema bounds is governed by its own `maxlen` and is INVALID above it; a field
// the schema leaves unbounded is governed by the receiver's configured cap and
// is limitExceeded() above it (CORELIB_PLAN §6.2.1). The two categories must not
// be folded -- a cap rejects well-formed bytes that decode under a looser cap --
// and a cap must never reach a field the schema already bounds.
//
// The corelib holds no cap of its own to fall back on any more
// (corelib-dart#88): this arm is the whole receiver bound on a schema-unbounded
// scalar string or blob. "" when the field has neither bound to state.
func (g *gen) maxlenHdrGuard(sub string, fld *ir.Field) string {
	if fld.HasMaxlen {
		return fmt.Sprintf("if (subtype == sofab.FixlenType.%s && length > %d) invalidate();", sub, fld.Maxlen)
	}
	live := g.limits.stringHas
	if fld.Kind == ir.KindBlob {
		live = g.limits.blobHas
	}
	if !live {
		return ""
	}
	return fmt.Sprintf("if (subtype == sofab.FixlenType.%s && length > %s) limitExceeded();", sub, g.elemMaxExpr(fld.Kind))
}

// arrayCountHdrGuard is the onArrayBegin arm body rejecting a native array whose
// wire element count exceeds the schema `count` N as INVALID, at the array
// header (generator#100 for the bound, generator#216 for moving it to the
// header).
//
// The bound sits INSIDE the kind test, and that nesting is the whole point of
// generator#259 / Crucible F-0042. onArrayBegin fires for ANY array kind landing
// on this field id: the corelib reports the kind that arrived but cannot know
// the DECLARED one, which is schema knowledge only the generated code has. An
// array whose element kind contradicts the declaration was never this field's
// value (MESSAGE_SPEC §7.3) — it is a skipped field, so its element count is not
// this field's count and must not be measured against N. Bounding first would
// turn a skippable contradiction into INVALID: an fp64 array header announcing 8
// elements at a declared `fp32[5]` slot must be SKIPPED and the message
// ACCEPTED, not rejected as over-count.
//
// That is also why `fp32` and `fp64` are separate kinds rather than one
// "fixlen": a fixlen array's count word precedes its fixlen_word, so the hook
// has to fire past the subtype (CORELIB_PLAN §4.8) for this test to be able to
// distinguish them at all.
//
// The skip itself needs no code here. The whole-array callbacks
// (onUnsignedArray/onSignedArray/onFp32Array/onFp64Array) are already
// kind-dispatched by the corelib, so a contradicting array lands in a callback
// with no arm for this id and evaporates — which also leaves a correctly typed
// earlier occurrence of the same id intact (§7.4). This pre-dispatch hook is the
// one place the kind has to be tested explicitly.
// The receiver cap is the ELSE of that bound, in the same arm, inside the same
// kind gate, and in the other category (§6.2.1): a schema-bounded array answers
// INVALID and never sees a cap, a schema-unbounded one answers limitExceeded()
// and has no other bound at all -- the corelib holds none (corelib-dart#88).
func (g *gen) arrayCountHdrGuard(kind string, fld *ir.Field) string {
	if fld.HasCount {
		return fmt.Sprintf("if (kind == sofab.ArrayKind.%s && count > %d) invalidate();", kind, fld.Count)
	}
	if !g.limits.arrayHas {
		return ""
	}
	return fmt.Sprintf("if (kind == sofab.ArrayKind.%s && count > %s) limitExceeded();", kind, g.arrayCapExpr())
}

// emitArrayDecode appends the decode arm(s) for an array field to the right
// callback bucket. Native scalar arrays bind into the member (with an over-count
// INVALID guard); wrapper-sequence arrays clear their list and descend into a
// collector.
//
// The wire count M IS the array's length (MESSAGE_SPEC §3): the M elements that
// arrived are the whole value, so they are taken exactly as they come. A
// declared `count: N` is a CAPACITY -- it bounds M (the guard below) but never
// adds elements, so there is nothing to fill in at [M, N).
// widthGuard renders the §7.1 declared-width rejection for a scalar store
// (documentation#32): a `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination carrying a
// value outside its declared range is malformed input, INVALID — never masked to
// the width, never kept. "" for u64/i64. It rejects through the same
// maxlen and count rejects set.
//
// The `value < 0` term is not redundant on the unsigned side: Dart's int is a
// 64-bit SIGNED integer with no unsigned counterpart, so an unsigned wire value
// at or above 2^63 arrives negative and `value > 255` alone would wave through
// exactly the largest values. Every narrow maximum is below 2^63, so treating
// negative as out-of-range is right for all of them.
func widthGuard(k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	cond := fmt.Sprintf("value < 0 || value > %d", hi)
	if lo < 0 {
		cond = fmt.Sprintf("value < %d || value > %d", lo, hi)
	}
	return fmt.Sprintf("if (%s) { invalidate(); return; }\n        ", cond)
}

// arrayWidthGuard is the same bound for a native array's ELEMENTS. The corelib
// hands the whole array over as a List<int>, so the raw values are still visible
// and one scan decides the array.
func arrayWidthGuard(elem ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(elem)
	if !ok {
		return ""
	}
	cond := fmt.Sprintf("_v < 0 || _v > %d", hi)
	if lo < 0 {
		cond = fmt.Sprintf("_v < %d || _v > %d", lo, hi)
	}
	return fmt.Sprintf("for (final _v in values) { if (%s) { invalidate(); return; } }\n        ", cond)
}

// elemBoundArm is the onArrayElemBound arm body declaring the range an element
// of this array may take (MESSAGE_SPEC §7.1) — "" for u64/i64 and for
// enum/bitfield/bool elements, whose range is the callback parameter's own.
//
// Emitted exactly where arrayWidthGuard is: the two are one bound at two times.
// The guard scans the assembled list, which decides an array that ARRIVES; this
// is what the decoder applies to one that does not, where the whole-array
// callback never fires and the guard therefore never runs (generator#267).
//
// Gated on `kind` for the reason arrayCountHdrGuard is: the hook is asked per
// field id, and an array whose wire element kind contradicts the declared one is
// skipped under §7.3 — its elements were never this field's value.
//
// `const` so the range is a compile-time constant and the answer costs no
// allocation, as corelib-dart's doc asks.
func elemBoundArm(kind string, elem ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(elem)
	if !ok {
		return ""
	}
	return fmt.Sprintf("if (kind == sofab.ArrayKind.%s) {\n          return const sofab.ElemRange(%d, %d);\n        }", kind, lo, hi)
}

func (g *gen) emitArrayDecode(fld *ir.Field, acc string, arm func(int64, string) string, seqArm func(int64, string) string, uArr, sArr, f32Arr, f64Arr, seq, arrBegin, elemBound *[]string) {
	if nativeArrayElem(fld.Elem) {
		// Its own arm shape: the method answers with a value, so an arm that
		// declares no range for the kind that arrived falls through to `return
		// null` rather than to the bare `return;` the void callbacks use.
		if b := elemBoundArm(wireArrayKind(fld.Elem), fld.Elem); b != "" {
			*elemBound = append(*elemBound, fmt.Sprintf("      case %d:\n        %s\n        return null;", fld.ID, b))
		}
	}
	guard := ""
	if fld.HasCount {
		// A wire element count above the schema `count` is INVALID (MESSAGE_SPEC
		// §3+§7): reject, never clamp (generator#100).
		guard = fmt.Sprintf("if (values.length > %d) { invalidate(); return; }\n        ", fld.Count)
	}
	// Native arrays fire onArrayBegin at the array header; wrapper-sequence arrays
	// descend via onSequenceStart (no header hook) and are bounded on the
	// collector instead. So the header bound is only for the native kinds -- and
	// an unbounded one carries the receiver cap there, in the schema bound's place.
	if nativeArrayElem(fld.Elem) {
		if hdr := g.arrayCountHdrGuard(wireArrayKind(fld.Elem), fld); hdr != "" {
			*arrBegin = append(*arrBegin, arm(fld.ID, hdr))
		}
	}
	switch {
	case unsignedArrayElem(fld.Elem) && fld.Elem == ir.KindBool:
		*uArr = append(*uArr, arm(fld.ID, guard+acc+" = [for (final _v in values) _v != 0];"))
	case unsignedArrayElem(fld.Elem):
		*uArr = append(*uArr, arm(fld.ID, guard+arrayWidthGuard(fld.Elem)+acc+" = List<int>.from(values);"))
	case signedArrayElem(fld.Elem):
		*sArr = append(*sArr, arm(fld.ID, guard+arrayWidthGuard(fld.Elem)+acc+" = List<int>.from(values);"))
	case fld.Elem == ir.KindFP32:
		// Bit-exact copy into a fresh Float32List of the WIRE count: a per-element
		// widen through a double would quiet a signaling/payload NaN (MESSAGE_SPEC
		// S4.6). writeFp32Array re-emits a Float32List raw.
		*f32Arr = append(*f32Arr, arm(fld.ID, fmt.Sprintf("%s%s = sofab.copyFp32(values, values.length);", guard, acc)))
	case fld.Elem == ir.KindFP64:
		*f64Arr = append(*f64Arr, arm(fld.ID, guard+acc+" = List<double>.from(values);"))
	default: // wrapper-sequence array (string/blob/struct/union/nested)
		et := g.dartArrayElemType(fld.Elem, fld.ElemRef, fld.ElemItems)
		coll := g.collector(acc, fld.Elem, fld.ElemRef, fld.ElemItems, capOf(fld.HasCount, fld.Count), emaxOf(fld.ElemMaxHas, fld.ElemMax))
		*seq = append(*seq, seqArm(fld.ID, fmt.Sprintf("%s = <%s>[];\n        return %s;", acc, et, coll)))
	}
}

// collector returns the Dart expression constructing the MessageVisitor that
// gathers a wrapper-sequence array's elements into the (freshly-cleared) list
// `out`. It recurses for nested arrays.
//
// Every collector is handed BOTH bounds of every axis it has: the schema pair
// (cap, emax / rowCount) and the receiver pair beside it (rcap, relemMax /
// rowCap). A wrapper array carries no count header -- its elements are keyed by
// an unbounded varint INDEX and the list is grown to fit, so the index IS the
// length -- and neither that index nor an element's length word ever reaches the
// generated visitor. The collector is therefore where this shape's receiver
// bounds land, and corelib-dart keeps each pair exclusive per §6.2.1: where the
// schema declares a `count`/`maxlen` the cap beside it is inert and the
// violation is INVALID, where it does not the cap governs and the violation is
// limitExceeded.
//
// The receiver arguments are REQUIRED by corelib-dart and emitted here
// unconditionally, including where the schema sibling makes them inert: §6.2.1
// gives that library no number to invent, so there is no default to leave one
// out in favour of.
func (g *gen) collector(out string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap, emax int64) string {
	rcap := ", rcap: " + g.arrayCapExpr()
	switch elem {
	case ir.KindString:
		return fmt.Sprintf("sofab.StringSeq(%s, %d, %d%s, relemMax: %s)", out, cap, emax, rcap, g.elemMaxExpr(ir.KindString))
	case ir.KindBlob:
		return fmt.Sprintf("sofab.BlobSeq(%s, %d, %d%s, relemMax: %s)", out, cap, emax, rcap, g.elemMaxExpr(ir.KindBlob))
	case ir.KindStruct, ir.KindUnion:
		t := g.typeName(ref.Key)
		return fmt.Sprintf("sofab.MessageSeq<%s>(%s, %d, () => %s(), (x) => %s(x)%s)", t, out, cap, t, visitorName(t), rcap)
	case ir.KindArray:
		// The row collectors take the OUTER array's cap: a row's element id is its
		// index in this array (§5.1), so cap is what bounds it -- and so is the
		// receiver cap beside it, for the same reason.
		if nativeArrayElem(items.Elem) {
			// A matrix has TWO axes and therefore four bounds. cap/rcap bound the ROW
			// ID; rowCount/rowCap bound a row's OWN element count, which the row
			// announces as a real count header because a row IS a native array -- and
			// which nothing bounded before: the inner `count:` was dropped on the
			// floor here, and the decoder-wide cap that stood in for it is gone.
			rows := fmt.Sprintf("%s, rowCount: %d, rowCap: %s", rcap, capOf(items.HasCount, items.Count), g.arrayCapExpr())
			switch {
			case items.Elem == ir.KindBool:
				return fmt.Sprintf("sofab.BoolMatrixSeq(%s, %d%s)", out, cap, rows)
			case items.Elem == ir.KindFP32 || items.Elem == ir.KindFP64:
				return fmt.Sprintf("sofab.DoubleMatrixSeq(%s, %d, %v%s)", out, cap, items.Elem == ir.KindFP64, rows)
			default:
				_lo, _hi, _ := ir.NarrowRange(items.Elem)
				return fmt.Sprintf("sofab.IntMatrixSeq(%s, %d, %v, %d, %d%s)", out, cap, signedArrayElem(items.Elem), _lo, _hi, rows)
			}
		}
		// Array of wrapper arrays: each element opens a sequence collected into the
		// inner list its element id names, by a recursively-built collector. The
		// inner one takes its OWN index cap: the row's schema `count` bounds the
		// row's elements, and where the row declares none the receiver cap does.
		innerT := g.dartArrayElemType(items.Elem, items.ElemRef, items.ElemItems)
		inner := g.collector("p", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count), emaxOf(items.ElemMaxHas, items.ElemMax))
		return fmt.Sprintf("sofab.NestedSeq<%s>(%s, %d, (p) => %s%s)", innerT, out, cap, inner, rcap)
	}
	return "null"
}

func capOf(has bool, count int64) int64 {
	if has {
		return count
	}
	return -1
}

func emaxOf(has bool, max int64) int64 {
	if has {
		return max
	}
	return -1
}

// emitSwitch emits a callback override with an id switch, or nothing when the
// object has no field for it (the base no-op then applies).
func emitSwitch(f *dfile, sig string, arms []string) {
	emitSwitchRet(f, sig, arms, "")
}

// emitDestSwitch is emitSwitchRet for the two DESTINATION hooks, with one
// difference that is the whole point of it: it emits the override even when
// there are no arms. A scope that binds no array and no payload must still
// DECLINE every array and every payload, or corelib-dart's allocating default
// stands and a skipped field is materialized from the wire (§6.2.1, §7.3).
func emitDestSwitch(f *dfile, sig string, arms []string) {
	f.line("  @override")
	f.line("  %s {", sig)
	if len(arms) > 0 {
		f.line("    switch (id) {")
		for _, a := range arms {
			f.line("%s", a)
		}
		f.line("    }")
	}
	f.line("    return null;")
	f.line("  }")
}

// emitSwitchRet is emitSwitch for a callback that answers with a value: `tail`
// is what an id with no arm falls through to. "" for the void callbacks, whose
// arms return on their own.
func emitSwitchRet(f *dfile, sig string, arms []string, tail string) {
	if len(arms) == 0 {
		return
	}
	f.line("  @override")
	f.line("  %s {", sig)
	f.line("    switch (id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("    }")
	if tail != "" {
		f.line("    %s", tail)
	}
	f.line("  }")
}

// ---- shared prelude (helpers + collectors) --------------------------------

// needs records which prelude helpers and collector classes a schema actually
// uses, so only those are emitted (clean output; nothing unused).
type needs struct {
	dec     bool
	f32bits bool
}

func (g *gen) computeNeeds(s *ir.Schema) needs {
	var n needs
	scan := func(fields []*ir.Field) {
		n.dec = true
		for _, fld := range fields {
			g.scanField(fld, &n)
		}
	}
	for _, key := range s.NamedOrder {
		if nt := s.Named[key]; nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			scan(nt.Fields)
		}
	}
	for _, m := range s.Messages {
		scan(m.Fields)
	}
	return n
}

func (g *gen) scanField(fld *ir.Field, n *needs) {
	switch fld.Kind {
	case ir.KindFP32:
		n.f32bits = true
	case ir.KindArray:
		if nativeArrayElem(fld.Elem) {
			return
		}
		g.scanArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems, n)
	}
}

// scanArrayElem descends a wrapper array's element type. It records nothing any
// more -- which collector each level needs is the corelib's business since
// corelib-dart#74 -- but the walk stays: an element that is itself an array can
// bottom out at an fp32 scalar, and that still decides `f32bits`.
func (g *gen) scanArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, n *needs) {
	if elem != ir.KindArray {
		return
	}
	if nativeArrayElem(items.Elem) {
		return
	}
	g.scanArrayElem(items.Elem, items.ElemRef, items.ElemItems, n)
}

// visitorBase is what every generated visitor extends. The corelib hosts it
// (corelib-dart#65): the class flips two sofab.MessageVisitor defaults that are
// right for a hand-written visitor and wrong for a schema-bound one -- an id
// this scope does not declare is skipped, not inspected, and a sub-sequence it
// does not bind is skipped whole. Neither decision has a schema in it, so the
// base is written once there rather than emitted into every module.
const visitorBase = "sofab.VisitorBase"

func (g *gen) emitPrelude(f *dfile, s *ir.Schema) {
	n := g.computeNeeds(s)
	if !n.dec && !g.limits.any() {
		return
	}
	if n.dec {
	}
	if n.f32bits {
		f.line("// Widen the 32 raw wire bits of an fp32 NaN to a display double for element")
		f.line("// access; the exact bits are kept alongside for a bit-for-bit re-encode.")
		f.line("double _f32FromBits(int bits) =>")
		f.line("    (ByteData(4)..setUint32(0, bits, Endian.little)).getFloat32(0, Endian.little);")
		f.blank()
	}
}
