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
	// e.inv before returning the incomplete status, so the flag alone suffices.
	//
	// Both hooks fire for ANY wire kind/subtype landing on a field id — the corelib
	// resolves what arrived but cannot know what was DECLARED — so both arms gate
	// their bound on the declared kind: a contradicting header is a §7.3 skip and
	// must never be measured against this field's bound (generator#224 for
	// onFixlenHeader, generator#259 / F-0042 for onArrayBegin).
	var arrBegin, fixHdr []string
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
				"if (s == null) { e.inv = true; return; }\n          " +
				acc + " = s;\n        }"
			if fld.HasMaxlen {
				// A wire byte length above the schema maxlen is malformed input
				// (MESSAGE_SPEC §7.1) — reject as INVALID, never truncate. The raw
				// bytes ARE the wire length, so this needs no re-encode.
				body = fmt.Sprintf("if (bytes.length > %d) { e.inv = true; return; }\n        %s", fld.Maxlen, body)
				fixHdr = append(fixHdr, arm(fld.ID, maxlenHdrGuard("string", fld.Maxlen)))
			}
			str = append(str, arm(fld.ID, body))
		case ir.KindBlob:
			// value aliases the decode buffer — copy what we keep.
			body := acc + " = Uint8List.fromList(value);"
			if fld.HasMaxlen {
				body = fmt.Sprintf("if (value.length > %d) { e.inv = true; return; }\n        %s", fld.Maxlen, body)
				fixHdr = append(fixHdr, arm(fld.ID, maxlenHdrGuard("blob", fld.Maxlen)))
			}
			blob = append(blob, arm(fld.ID, body))
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, seqArm(fld.ID, fmt.Sprintf("return %s(%s, e);", visitorName(g.typeName(fld.Ref.Key)), acc)))
		case ir.KindArray:
			g.emitArrayDecode(fld, acc, arm, seqArm, &uArr, &sArr, &f32Arr, &f64Arr, &seq, &arrBegin, &elemBound)
		}
	}

	f.line("class %s extends %s {", visitorName(typeName), visitorBase)
	f.line("  %s(this.o, this.e);", visitorName(typeName))
	f.line("  final %s o;", typeName)
	f.line("  final _Dec e;")
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
func maxlenHdrGuard(sub string, n int64) string {
	return fmt.Sprintf("if (subtype == sofab.FixlenType.%s && length > %d) e.inv = true;", sub, n)
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
func arrayCountHdrGuard(kind string, n int64) string {
	return fmt.Sprintf("if (kind == sofab.ArrayKind.%s && count > %d) e.inv = true;", kind, n)
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
// the width, never kept. "" for u64/i64. `e.inv` is the same sticky flag the
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
	return fmt.Sprintf("if (%s) { e.inv = true; return; }\n        ", cond)
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
	return fmt.Sprintf("for (final _v in values) { if (%s) { e.inv = true; return; } }\n        ", cond)
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
		guard = fmt.Sprintf("if (values.length > %d) { e.inv = true; return; }\n        ", fld.Count)
		// Native arrays fire onArrayBegin at the array header; wrapper-sequence arrays
		// descend via onSequenceStart (no header hook) and are bounded at the
		// collector cap instead. So the header reject is only for the native kinds.
		if nativeArrayElem(fld.Elem) {
			*arrBegin = append(*arrBegin, arm(fld.ID, arrayCountHdrGuard(wireArrayKind(fld.Elem), fld.Count)))
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
		*f32Arr = append(*f32Arr, arm(fld.ID, fmt.Sprintf("%s%s = _f32copy(values, values.length);", guard, acc)))
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
func (g *gen) collector(out string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap, emax int64) string {
	switch elem {
	case ir.KindString:
		return fmt.Sprintf("_StrSeq(%s, %d, %d, e)", out, cap, emax)
	case ir.KindBlob:
		return fmt.Sprintf("_BlobSeq(%s, %d, %d, e)", out, cap, emax)
	case ir.KindStruct, ir.KindUnion:
		t := g.typeName(ref.Key)
		return fmt.Sprintf("_ObjSeq<%s>(%s, %d, e, () => %s(), (x) => %s(x, e))", t, out, cap, t, visitorName(t))
	case ir.KindArray:
		// The row collectors take the OUTER array's cap: a row's element id is its
		// index in this array (§5.1), so cap is what bounds it.
		if nativeArrayElem(items.Elem) {
			switch {
			case items.Elem == ir.KindBool:
				return fmt.Sprintf("_BoolMat(%s, %d, e)", out, cap)
			case items.Elem == ir.KindFP32 || items.Elem == ir.KindFP64:
				return fmt.Sprintf("_DblMat(%s, %d, %v, e)", out, cap, items.Elem == ir.KindFP64)
			default:
				_lo, _hi, _ := ir.NarrowRange(items.Elem)
				return fmt.Sprintf("_IntMat(%s, %d, %v, %d, %d, e)", out, cap, signedArrayElem(items.Elem), _lo, _hi)
			}
		}
		// Array of wrapper arrays: each element opens a sequence collected into the
		// inner list its element id names, by a recursively-built collector.
		innerT := g.dartArrayElemType(items.Elem, items.ElemRef, items.ElemItems)
		inner := g.collector("p", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count), emaxOf(items.ElemMaxHas, items.ElemMax))
		return fmt.Sprintf("_SeqSeq<%s>(%s, %d, e, (p) => %s)", innerT, out, cap, inner)
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
	dec                             bool
	f32copy, f32bits                bool
	strSeq, blobSeq, objSeq         bool
	intMat, dblMat, boolMat, seqSeq bool
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
	// _DblMat's onFp32Array branch calls _f32copy even for an fp64-only matrix (the
	// call must resolve), so a native-array matrix always needs it emitted.
	if n.dblMat {
		n.f32copy = true
	}
	return n
}

func (g *gen) scanField(fld *ir.Field, n *needs) {
	switch fld.Kind {
	case ir.KindFP32:
		n.f32bits = true
	case ir.KindArray:
		if nativeArrayElem(fld.Elem) {
			if fld.Elem == ir.KindFP32 {
				// fp32 arrays bind through _f32copy (bit-exact).
				n.f32copy = true
			}
			return
		}
		g.scanArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems, n)
	}
}

func (g *gen) scanArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, n *needs) {
	switch elem {
	case ir.KindString:
		n.strSeq = true
	case ir.KindBlob:
		n.blobSeq = true
	case ir.KindStruct, ir.KindUnion:
		n.objSeq = true
	case ir.KindArray:
		if nativeArrayElem(items.Elem) {
			switch {
			case items.Elem == ir.KindBool:
				n.boolMat = true
			case items.Elem == ir.KindFP32 || items.Elem == ir.KindFP64:
				n.dblMat = true
			default:
				n.intMat = true
			}
			return
		}
		n.seqSeq = true
		g.scanArrayElem(items.Elem, items.ElemRef, items.ElemItems, n)
	}
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
		f.line("// A sticky INVALID flag shared across all visitors of one decode. The corelib")
		f.line("// visitor callbacks return void, so a schema-bound violation (over-count,")
		f.line("// over-index, over-maxlen) sets this and the generated decode converts it to a")
		f.line("// terminal INVALID after the corelib returns.")
		f.line("class _Dec {")
		f.line("  bool inv = false;")
		f.line("}")
		f.blank()
	}
	if n.f32bits {
		f.line("// Widen the 32 raw wire bits of an fp32 NaN to a display double for element")
		f.line("// access; the exact bits are kept alongside for a bit-for-bit re-encode.")
		f.line("double _f32FromBits(int bits) =>")
		f.line("    (ByteData(4)..setUint32(0, bits, Endian.little)).getFloat32(0, Endian.little);")
		f.blank()
	}
	if n.f32copy {
		f.line("// Bit-exact fp32 array copy into a fresh Float32List of length [n] (>= the")
		f.line("// source length). A raw byte copy preserves a signaling/payload NaN that a")
		f.line("// per-element widen through a Dart double would quiet; writeFp32Array")
		f.line("// re-emits a Float32List's bytes verbatim.")
		f.line("Float32List _f32copy(Float32List v, int n) {")
		f.line("  final out = Float32List(n < v.length ? v.length : n);")
		f.line("  Uint8List.sublistView(out).setRange(0, v.length * 4, Uint8List.sublistView(v));")
		f.line("  return out;")
		f.line("}")
		f.blank()
	}
	g.emitCollectors(f, n)
}

// emitCollectors emits the wrapper-sequence collector visitors the schema uses.
// Each is keyed by the 0-based element index id: an element is PLACED at
// out[id] after gap-filling with element defaults, never appended, because an
// INTERIOR element equal to the element default is omitted on the wire and
// leaves an id GAP (MESSAGE_SPEC §2). The array's LAST element is always on the
// wire, so the decoded length -- highest present id + 1 -- is exact, and nothing
// is filled in afterwards.
//
// cap is the schema count N, or -1 for a count-less array. N is a CAPACITY, not
// a length (§3): it never reaches the wire and never adds elements the wire did
// not carry. All it does here is bound the array -- an element id >= N is a
// schema-bound violation (INVALID, never grown-into) -- rejected before the list
// grows, which also bounds the id-keyed gap-fill against an over-index
// amplification DoS. emax (element maxlen, or -1) rejects an over-length
// string/blob element the same way.
func (g *gen) emitCollectors(f *dfile, n needs) {
	if n.strSeq {
		f.line("class _StrSeq extends %s {", visitorBase)
		f.line("  _StrSeq(this.out, this.cap, this.emax, this.e);")
		f.line("  final List<String> out;")
		f.line("  final int cap;")
		f.line("  final int emax;")
		f.line("  final _Dec e;")
		// The element's schema bounds are decided at the LENGTH WORD, before a byte
		// of payload is buffered. MESSAGE_SPEC S5.2 makes INVALID dominate
		// INCOMPLETE, so a message truncated right after the word that carries the
		// violating number must still be INVALID -- deciding it in onStringBytes,
		// which never fires for such a message, reported INCOMPLETE instead
		// (generator#267 / Crucible F-0043).
		//
		// Both bounds sit inside the declared-subtype test for the same reason the
		// scalar header guard does: onFixlenHeader fires for ANY fixlen subtype at
		// this id, and an element whose subtype contradicts the declaration was
		// never this array's value (S7.3) -- so neither its id nor its length may be
		// measured against this array's bounds.
		f.line("  @override")
		f.line("  void onFixlenHeader(int id, int subtype, int length) {")
		f.line("    if (subtype != sofab.FixlenType.string) return;")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    if (emax >= 0 && length > emax) { e.inv = true; return; }")
		f.line("  }")
		f.line("  @override")
		f.line("  void onStringBytes(int id, Uint8List bytes) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    if (emax >= 0 && bytes.length > emax) { e.inv = true; return; }")
		f.line("    // The element is being materialized, so this is where its UTF-8 is")
		f.line("    // checked. A skipped payload never reaches a collector at all.")
		f.line("    final s = sofab.decodeUtf8Strict(bytes);")
		f.line("    if (s == null) { e.inv = true; return; }")
		f.line("    while (out.length <= id) { out.add(''); }")
		f.line("    out[id] = s;")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if n.blobSeq {
		f.line("class _BlobSeq extends %s {", visitorBase)
		f.line("  _BlobSeq(this.out, this.cap, this.emax, this.e);")
		f.line("  final List<Uint8List> out;")
		f.line("  final int cap;")
		f.line("  final int emax;")
		f.line("  final _Dec e;")
		// The blob twin of the string collector above: bounds latched at the length
		// word (generator#267), gated on the declared subtype (S7.3).
		f.line("  @override")
		f.line("  void onFixlenHeader(int id, int subtype, int length) {")
		f.line("    if (subtype != sofab.FixlenType.blob) return;")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    if (emax >= 0 && length > emax) { e.inv = true; return; }")
		f.line("  }")
		f.line("  @override")
		f.line("  void onBlob(int id, Uint8List value) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    if (emax >= 0 && value.length > emax) { e.inv = true; return; }")
		f.line("    while (out.length <= id) { out.add(Uint8List(0)); }")
		f.line("    out[id] = Uint8List.fromList(value);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if n.objSeq {
		// The element id IS the array index (§5.1), so an element is PLACED at
		// out[id] after gap-filling with default elements -- never appended.
		// Appending would shorten the array by the size of any interior id gap --
		// and an omitted all-default interior element is exactly such a gap -- and
		// would decode a REOPENED id as a second element instead of merging into the
		// first (§7.4, which placement gives for free).
		f.line("class _ObjSeq<T> extends %s {", visitorBase)
		f.line("  _ObjSeq(this.out, this.cap, this.e, this.make, this.vis);")
		f.line("  final List<T> out;")
		f.line("  final int cap;")
		f.line("  final _Dec e;")
		f.line("  final T Function() make;")
		f.line("  final sofab.MessageVisitor Function(T) vis;")
		f.line("  @override")
		f.line("  sofab.MessageVisitor? onSequenceStart(int id) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return null; }")
		f.line("    while (out.length <= id) { out.add(make()); }")
		f.line("    return vis(out[id]);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	// The matrix / nested-row collectors below place a row at the index its element
	// id names, exactly like the leaf and object collectors above: an interior row
	// equal to the element default (the empty row) is omitted by a conformant
	// encoder (§2), and appending would shift every later row down by one. cap is
	// the OUTER array's schema count, which bounds that id (§5.1/§7) and with it
	// the gap-fill.
	if n.intMat {
		f.line("class _IntMat extends %s {", visitorBase)
		f.line("  _IntMat(this.out, this.cap, this.signed, this.lo, this.hi, this.e);")
		f.line("  final List<List<int>> out;")
		f.line("  final int cap;")
		f.line("  final bool signed;")
		// The row element's DECLARED width. Without it an over-width element was
		// stored as-is, which MESSAGE_SPEC 7.1 forbids -- it is INVALID, never a
		// silent store. lo == hi means "no bound": u64/i64 and enum/bitfield span
		// the callback parameter's own range, exactly as the flat guard omits
		// itself there (generator#330).
		f.line("  final int lo;")
		f.line("  final int hi;")
		f.line("  final _Dec e;")
		f.line("  void _row(int id, Int64List v) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    if (lo != hi) { for (final _v in v) { if (_v < lo || _v > hi) { e.inv = true; return; } } }")
		f.line("    while (out.length <= id) { out.add(<int>[]); }")
		f.line("    out[id] = List<int>.from(v);")
		f.line("  }")
		f.line("  @override")
		f.line("  void onUnsignedArray(int id, Int64List values) { if (!signed) _row(id, values); }")
		f.line("  @override")
		f.line("  void onSignedArray(int id, Int64List values) { if (signed) _row(id, values); }")
		f.line("}")
		f.blank()
	}
	if n.dblMat {
		f.line("class _DblMat extends %s {", visitorBase)
		f.line("  _DblMat(this.out, this.cap, this.f64, this.e);")
		f.line("  final List<List<double>> out;")
		f.line("  final int cap;")
		f.line("  final bool f64;")
		f.line("  final _Dec e;")
		f.line("  @override")
		f.line("  void onFp32Array(int id, Float32List values) {")
		f.line("    if (f64) return;")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    while (out.length <= id) { out.add(<double>[]); }")
		f.line("    out[id] = _f32copy(values, values.length); // bit-exact: keep an fp32 NaN's bits")
		f.line("  }")
		f.line("  @override")
		f.line("  void onFp64Array(int id, Float64List values) {")
		f.line("    if (!f64) return;")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    while (out.length <= id) { out.add(<double>[]); }")
		f.line("    out[id] = List<double>.from(values);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if n.boolMat {
		f.line("class _BoolMat extends %s {", visitorBase)
		f.line("  _BoolMat(this.out, this.cap, this.e);")
		f.line("  final List<List<bool>> out;")
		f.line("  final int cap;")
		f.line("  final _Dec e;")
		f.line("  @override")
		f.line("  void onUnsignedArray(int id, Int64List values) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    while (out.length <= id) { out.add(<bool>[]); }")
		f.line("    out[id] = [for (final v in values) v != 0];")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if n.seqSeq {
		f.line("class _SeqSeq<T> extends %s {", visitorBase)
		f.line("  _SeqSeq(this.out, this.cap, this.e, this.make);")
		f.line("  final List<List<T>> out;")
		f.line("  final int cap;")
		f.line("  final _Dec e;")
		f.line("  final sofab.MessageVisitor Function(List<T>) make;")
		f.line("  @override")
		f.line("  sofab.MessageVisitor? onSequenceStart(int id) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return null; }")
		f.line("    while (out.length <= id) { out.add(<T>[]); }")
		f.line("    return make(out[id]);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
}
