package dart

import (
	"fmt"
	"strings"

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
			uns = append(uns, arm(fld.ID, widthGuard(fld.Kind, fld.Ref)+acc+" = value;"))
		case ir.KindBool:
			uns = append(uns, arm(fld.ID, acc+" = value != 0;"))
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
			sig = append(sig, arm(fld.ID, widthGuard(fld.Kind, fld.Ref)+acc+" = value;"))
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
// widthGuard renders the §7.1 rejection for a scalar store into a destination
// the schema declares with Kind k — and, for a composite kind, `ref` carries the
// rest of that declaration. "" when nothing reachable can breach the bound: the
// 64-bit kinds, whose range IS the callback parameter's own, and a bitfield
// declaring all 64 positions. It rejects through the same set as the maxlen and
// count rejects.
//
// What the schema declares is what binds, and the declaration takes two shapes.
// For an integer it is a WIDTH (documentation#32): a `u8`/`u16`/`u32`/`i8`/
// `i16`/`i32` destination carrying a value outside its declared range is
// malformed input, INVALID — never masked to the width, never kept. For an
// `enum` or a `bitfield` it is a SET — the declared constants, the mask of
// declared `pos` bits (MESSAGE_SPEC §1) — and closedCond answers for those.
//
// One clause serves four of the six positions a value lands in: this loop runs
// once per message, struct and union visitor, so the message field, the struct
// member, the struct-array element's member and the union member share it, and
// arrayWidthGuard carries the same clause to the two element positions
// (generator#516).
//
// The `value < 0` term is not redundant on the unsigned WIDTH side: Dart's int
// is a 64-bit SIGNED integer with no unsigned counterpart, so an unsigned wire
// value at or above 2^63 arrives negative and `value > 255` alone would wave
// through exactly the largest values. Every narrow maximum is below 2^63, so
// treating negative as out-of-range is right for all of them. A bitfield mask
// test needs no such term and must not have one — it reads the raw 64 bits, and
// for a bitfield declaring position 63 a negative value is a LEGAL one.
func widthGuard(k ir.Kind, ref *ir.TypeRef) string {
	cond := widthCond("value", k)
	if cond == "" {
		cond = closedCond("value", k, ref)
	}
	if cond == "" {
		return ""
	}
	return fmt.Sprintf("if (%s) { invalidate(); return; }\n        ", cond)
}

// widthCond is the declared-integer-width half of widthGuard's comparison.
func widthCond(v string, k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("%s < %d || %s > %d", v, lo, v, hi)
	}
	return fmt.Sprintf("%s < 0 || %s > %d", v, v, hi)
}

// closedCond is the reject comparison for the two CLOSED kinds, MESSAGE_SPEC §1:
// an `enum` is bound by the set of constants the schema declares, a `bitfield`
// by the mask of the positions it declares, so a value is valid exactly when
// `v & ~mask == 0`. It returns "" for every other kind (widthCond owns those)
// and for a bitfield whose declared positions cover all 64 bits, where the test
// is a tautology and the clause would be dead code.
//
// Neither bound is a width, and storage is never the bound. Dart holds both
// kinds in its own `int` whatever the schema declares, and a field whose
// declared positions are 0..3 does not become 0..2^64 valid because of it. The
// comparison runs on the value the corelib handed over, before anything is
// stored, and one clause covers both halves of the old reading at once: a mask
// test on the raw 64-bit word rejects an undeclared bit inside the byte the
// field would fit in and everything above it in the same expression, and a
// membership test does the same for an enum.
//
// This reverses generator#482, which kept an undeclared bit that fit the backing
// width on the argument that it is how a peer built from a newer schema carries
// a flag this one has not got yet. §1 answers that directly: adding a flag — or
// a constant — is a BREAKING schema change, and a receiver rejects the value
// rather than storing something its declared type cannot represent.
//
// The mask is rendered as a HEXADECIMAL literal: Dart accepts a hex literal
// anywhere in [0, 2^64) and reinterprets it as the signed 64-bit word, which is
// the only way to spell a mask with position 63 declared at all. Rendering the
// literal is the trap generator#470 already hit once.
func closedCond(v string, k ir.Kind, ref *ir.TypeRef) string {
	switch k {
	case ir.KindEnum:
		vals, ok := ir.EnumValues(ref)
		if !ok || len(vals) == 0 {
			return ""
		}
		if ir.EnumContiguous(ref) {
			return fmt.Sprintf("%s < %d || %s > %d", v, vals[0], v, vals[len(vals)-1])
		}
		terms := make([]string, len(vals))
		for i, x := range vals {
			terms[i] = fmt.Sprintf("%s != %d", v, x)
		}
		return strings.Join(terms, " && ")
	case ir.KindBitfield:
		mask, ok := ir.BitfieldMask(ref)
		if !ok || mask == ^uint64(0) {
			return ""
		}
		if mask == 0 {
			return fmt.Sprintf("%s != 0", v)
		}
		return fmt.Sprintf("(%s & ~0x%x) != 0", v, mask)
	}
	return ""
}

// arrayWidthGuard is the same bound for a native array's ELEMENTS. The corelib
// hands the whole array over as a List<int>, so the raw values are still visible
// and one scan decides the array.
//
// For a declared width the scan is the second of two statements of one bound —
// elemBoundArm below states it at the element, for the array that never
// completes. For a CLOSED kind the scan is the EXACT one and the hook below can
// only carry the hull, so this is where a gap value is refused; see there.
func arrayWidthGuard(elem ir.Kind, ref *ir.TypeRef) string {
	cond := widthCond("_v", elem)
	if cond == "" {
		cond = closedCond("_v", elem, ref)
	}
	if cond == "" {
		return ""
	}
	return fmt.Sprintf("for (final _v in values) { if (%s) { invalidate(); return; } }\n        ", cond)
}

// elemBoundArm is the onArrayElemBound arm body declaring the range an element
// of this array may take (MESSAGE_SPEC §7.1) — "" for u64/i64 and bool, whose
// range is the callback parameter's own.
//
// Emitted exactly where arrayWidthGuard is: the two are one bound at two times.
// The guard scans the assembled list, which decides an array that ARRIVES; this
// is what the decoder applies to one that does not, where the whole-array
// callback never fires and the guard therefore never runs (generator#267).
//
// For a declared WIDTH the two say the same thing. For the two CLOSED kinds they
// do not, and cannot: sofab.ElemRange is an INTERVAL and a declared set is not
// one — the constants are gapped in general, and a mask is not a range at all —
// so what is stated here is the HULL (elemRange), the most of the bound this
// hook can carry. It refuses every value outside the declared range whether the
// array completes or is cut short behind the offending element, and
// arrayWidthGuard's scan closes the gap for an array that arrives. What stays
// unenforced is exactly one case, reported rather than papered over: a value
// inside the hull that the set does not declare, in an array a truncation cuts
// short behind it. Closing it needs a set/mask channel on corelib-dart's
// onArrayElemBound.
//
// Gated on `kind` for the reason arrayCountHdrGuard is: the hook is asked per
// field id, and an array whose wire element kind contradicts the declared one is
// skipped under §7.3 — its elements were never this field's value.
//
// `const` so the range is a compile-time constant and the answer costs no
// allocation, as corelib-dart's doc asks.
func elemBoundArm(kind string, elem ir.Kind, ref *ir.TypeRef) string {
	lo, hi, ok := elemRange(elem, ref)
	if !ok {
		return ""
	}
	return fmt.Sprintf("if (kind == sofab.ArrayKind.%s) {\n          return const sofab.ElemRange(%d, %d);\n        }", kind, lo, hi)
}

// elemRange is the inclusive INTERVAL an element may take: a declared width
// exactly, or a closed kind's hull — an enum's lowest and highest constant,
// `0..mask` for a bitfield. It is what onArrayElemBound is handed, the one
// corelib hook here that carries an interval and can carry nothing else, and it
// is a WEAKER bound than a closed declaration: nothing may use it where
// closedCond can state the set itself.
//
// ok is false where no interval can be stated, and each case is a real one:
//   - u64/i64 and bool, whose range IS the callback parameter's own;
//   - an enum declaring no constants, and a bitfield whose mask is 0 — the value
//     domain is a single value, which `lo == hi` disarms both hooks on;
//   - a bitfield declaring position 63. Its mask has no positive Dart `int` to
//     state as a maximum, and both hooks compare an unsigned element as
//     `v < 0 || v > max`, so stating anything at all would refuse the very value
//     that bit is.
func elemRange(k ir.Kind, ref *ir.TypeRef) (lo, hi int64, ok bool) {
	if lo, hi, ok = ir.NarrowRange(k); ok {
		return lo, hi, true
	}
	switch k {
	case ir.KindEnum:
		lo, hi, ok = ir.EnumHull(ref)
		return lo, hi, ok && lo != hi
	case ir.KindBitfield:
		mask, isBf := ir.BitfieldMask(ref)
		if !isBf || mask == 0 || mask >= uint64(1)<<63 {
			return 0, 0, false
		}
		return 0, int64(mask), true
	}
	return 0, 0, false
}

func (g *gen) emitArrayDecode(fld *ir.Field, acc string, arm func(int64, string) string, seqArm func(int64, string) string, uArr, sArr, f32Arr, f64Arr, seq, arrBegin, elemBound *[]string) {
	if nativeArrayElem(fld.Elem) {
		// Its own arm shape: the method answers with a value, so an arm that
		// declares no range for the kind that arrived falls through to `return
		// null` rather than to the bare `return;` the void callbacks use.
		if b := elemBoundArm(wireArrayKind(fld.Elem), fld.Elem, fld.ElemRef); b != "" {
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
		*uArr = append(*uArr, arm(fld.ID, guard+arrayWidthGuard(fld.Elem, fld.ElemRef)+acc+" = List<int>.from(values);"))
	case signedArrayElem(fld.Elem):
		*sArr = append(*sArr, arm(fld.ID, guard+arrayWidthGuard(fld.Elem, fld.ElemRef)+acc+" = List<int>.from(values);"))
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
				// lo/hi are the DECLARED WIDTH and nothing else. For the two CLOSED
				// kinds they stay 0, 0 — which the collector reads as "no bound" —
				// because the wrapper below is the whole bound there, and arming a
				// hull beside it would put a second, weaker interval on the same
				// values that disagrees with it on exactly the gap.
				_lo, _hi, _ := ir.NarrowRange(items.Elem)
				seq := fmt.Sprintf("sofab.IntMatrixSeq(%s, %d, %v, %d, %d%s)", out, cap, signedArrayElem(items.Elem), _lo, _hi, rows)
				if gn := g.rowGuardName(items.Elem, items.ElemRef); gn != "" {
					seq = fmt.Sprintf("%s(%s, %d, %v, %d, %d%s)", gn, out, cap, signedArrayElem(items.Elem), _lo, _hi, rows)
				}
				return seq
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

// ---- closed matrix-row elements -------------------------------------------

// matrixRowClosed keys, by named type, every `enum` and `bitfield` this schema
// uses as a MATRIX ROW ELEMENT (`array<array<enum>>`) and whose declared set
// bounds anything -- i.e. the row collectors that need a guard subclass.
//
// It is a scan and not a flag set during emission because the module is one
// Dart library: the subclass has to exist before the first expression naming it
// and may be declared exactly once however many messages share the type.
func (g *gen) matrixRowClosed(s *ir.Schema) map[string]*ir.TypeRef {
	out := map[string]*ir.TypeRef{}
	var walk func(elem ir.Kind, items *ir.ArrayElem)
	walk = func(elem ir.Kind, items *ir.ArrayElem) {
		// The shape `collector` builds an IntMatrixSeq for: an array whose ELEMENT
		// is itself an array of natives. Anything deeper recurses the way the
		// nested wrapper-array collector does.
		if elem != ir.KindArray || items == nil {
			return
		}
		if !nativeArrayElem(items.Elem) {
			walk(items.Elem, items.ElemItems)
			return
		}
		if n := g.rowGuardName(items.Elem, items.ElemRef); n != "" {
			out[items.ElemRef.Key] = items.ElemRef
		}
	}
	scan := func(fields []*ir.Field) {
		for _, fld := range fields {
			if fld.Kind == ir.KindArray {
				walk(fld.Elem, fld.ElemItems)
			}
		}
	}
	for _, m := range s.Messages {
		scan(m.Fields)
	}
	for _, key := range s.NamedOrder {
		scan(s.Named[key].Fields)
	}
	return out
}

// rowGuardName is the generated subclass that closes a matrix row element's
// declared set, or "" for an element kind that needs none. Library-private, so
// it cannot collide with a generated type name.
func (g *gen) rowGuardName(elem ir.Kind, ref *ir.TypeRef) string {
	if ref == nil || closedCond("_v", elem, ref) == "" {
		return ""
	}
	return "_Row" + g.typeName(ref.Key)
}

// emitRowGuard emits the matrix-row subclass for one closed named type: it
// overrides the single element callback its declared wire kind arrives on,
// closes the set, and delegates the row to the corelib collector.
//
// It exists because a matrix row's elements never reach the generated visitor --
// sofab.IntMatrixSeq gathers them and places the finished row -- and the only
// bound the collector carries of its own is the `lo`/`hi` INTERVAL, which states
// neither a gapped constant set nor a bitfield mask (MESSAGE_SPEC §1).
// Subclassing is a purely generated-side fix, the same one the Go backend makes
// with an embedded collector: the row id, the row count and the placing stay the
// corelib's, and only the value test is ours.
//
// The scan is unconditional because the callback it overrides is the one the
// collector was constructed for: `signed` is fixed by the schema, and a row of
// the OTHER wire kind arrives on the other callback, which this subclass does
// not touch and the collector already skips under §7.3.
//
// It decides an array that ARRIVES. A row cut short behind an undeclared element
// reports INCOMPLETE where §5.2 wants INVALID, and closing that needs a
// per-element channel on corelib-dart's collector -- the same limit the flat
// array position has, where onArrayElemBound at least carries the hull.
func (g *gen) emitRowGuard(f *dfile, nt *ir.NamedType, ref *ir.TypeRef) {
	kind := ir.KindBitfield
	cb := "onUnsignedArray"
	if nt.Category == ir.CatEnum {
		kind, cb = ir.KindEnum, "onSignedArray"
	}
	name := g.rowGuardName(kind, ref)
	f.line("/// Closes [%s] at a matrix row element (MESSAGE_SPEC §1). The row's", g.typeName(nt.Key))
	f.line("/// values never reach the generated visitor -- the collector gathers them and")
	f.line("/// places the finished row -- and the only bound it carries of its own is an")
	f.line("/// interval, which states neither a gapped constant set nor a bitfield mask.")
	f.line("/// So the value test is here and everything else stays the corelib's.")
	f.line("class %s extends sofab.IntMatrixSeq {", name)
	f.line("  %s(super.out, super.cap, super.signed, super.lo, super.hi,", name)
	f.line("      {required super.rcap, required super.rowCount, required super.rowCap});")
	f.blank()
	f.line("  @override")
	f.line("  void %s(int id, Int64List values) {", cb)
	f.line("    for (final _v in values) { if (%s) { invalidate(); return; } }", closedCond("_v", kind, ref))
	f.line("    super.%s(id, values);", cb)
	f.line("  }")
	f.line("}")
	f.blank()
}
