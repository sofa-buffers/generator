package dart

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// ---- decode visitor -------------------------------------------------------

// emitVisitor emits the push child-visitor for an object's id scope, against
// corelib-dart's one-call-per-field decode API (corelib-dart#96).
//
// Scalars arrive as values and bind straight into a member. Every aggregate --
// a string, a blob, a native array -- is announced ONCE, at its header, with its
// byte length or element count, and the arm answers with the member's `Inline…`
// destination: the codec sets its `length` there and writes the payload into its
// storage. Nothing is copied, no view is built, and nothing is called when the
// field is whole, so there is nothing to allocate per message either.
//
// The header call is also where every bound on the field is judged, and the only
// place: a schema `maxlen`/`count` is refused with invalidate(), a receiver cap
// on a schema-unbounded field with limitExceeded() (CORELIB_PLAN §6.2.1) -- both
// BEFORE the destination is handed over, which is the allocation they exist to
// prevent, and both ahead of a truncated payload (MESSAGE_SPEC §5.2,
// generator#216). The check precedes any sizing, so a hostile count can
// never size a destination.
//
// MESSAGE_SPEC §7.3 needs no code. Which call fires is decided by the WIRE kind:
// a field declared `array<u32>` that receives a signed array lands in
// onSignedArray, which has no arm for its id, answers null, and the field is
// skipped -- never measured against this field's bound (generator#224, #259 /
// F-0042), never materialized, and for a string never UTF-8-validated
// (generator#257, #265). A declared element width travels on the destination
// itself (`range:`, see rangeArg) and is applied by the codec per element.
//
// Nested structs/unions and every wrapper-sequence array descend via
// onSequenceStart into a child visitor. A struct/union descent returns the
// EXISTING member's visitor, so a re-opened scope merges (§7.4); an array
// wrapper clears its list first, so a re-opened wrapper is replaced.
func (g *gen) emitVisitor(f *dfile, typeName string, fields []*ir.Field) {
	var uns, sig, f32, f32bits, f64 []string
	var str, blob, uArr, sArr, f32Arr, f64Arr []string
	var seq []string

	arm := func(id int64, body string) string {
		return fmt.Sprintf("      case %d:\n        %s\n        return;", id, body)
	}
	retArm := func(id int64, body string) string {
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
			// The codec validates the bytes as UTF-8 once they are whole, and only
			// for a destination it was handed: a skipped string is never inspected
			// (CORELIB_PLAN §6.4, generator#257).
			str = append(str, retArm(fld.ID, g.destReturn(fld, acc, "length", g.limits.stringHas, g.elemMaxExpr(ir.KindString))))
		case ir.KindBlob:
			blob = append(blob, retArm(fld.ID, g.destReturn(fld, acc, "length", g.limits.blobHas, g.elemMaxExpr(ir.KindBlob))))
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, retArm(fld.ID, fmt.Sprintf("return %s(%s);", visitorName(g.typeName(fld.Ref.Key)), acc)))
		case ir.KindArray:
			if !nativeArrayElem(fld.Elem) {
				// Wrapper-sequence array: clear, then descend into a collector. The
				// clear is §7.4 -- a later occurrence of the field replaces it whole --
				// and keeps the list's backing store, where a fresh list would not.
				coll := g.collector(acc, fld.Elem, fld.ElemRef, fld.ElemItems, capOf(fld.HasCount, fld.Count), emaxOf(fld.ElemMaxHas, fld.ElemMax))
				seq = append(seq, retArm(fld.ID, fmt.Sprintf("%s.clear();\n        return %s;", acc, coll)))
				continue
			}
			a := retArm(fld.ID, g.destReturn(fld, acc, "count", g.limits.arrayHas, g.arrayCapExpr()))
			switch {
			case unsignedArrayElem(fld.Elem):
				uArr = append(uArr, a)
			case signedArrayElem(fld.Elem):
				sArr = append(sArr, a)
			case fld.Elem == ir.KindFP32:
				f32Arr = append(f32Arr, a)
			default:
				f64Arr = append(f64Arr, a)
			}
		}
	}

	f.line("class %s extends sofab.MessageVisitor {", visitorName(typeName))
	f.line("  %s(this.o);", visitorName(typeName))
	f.line("  final %s o;", typeName)
	emitSwitch(f, "void onUnsigned(int id, int value)", uns)
	emitSwitch(f, "void onSigned(int id, int value)", sig)
	emitSwitch(f, "void onFp32(int id, double value)", f32)
	// onFp32Bits is delivered (instead of onFp32) for an fp32 field whose payload
	// is a NaN, carrying the raw 32 bits so a signaling/payload NaN survives §4.6.
	emitSwitch(f, "void onFp32Bits(int id, int bits)", f32bits)
	emitSwitch(f, "void onFp64(int id, double value)", f64)
	// The aggregate header calls. A scope with no field of a kind emits nothing
	// and inherits the corelib's default, which answers null: the field is
	// skipped, so nothing is allocated for it and no bound applies (§6.2.1).
	emitSwitchRet(f, "sofab.InlineString? onString(int id, int length)", str, "return null;")
	emitSwitchRet(f, "sofab.InlineBytes? onBlob(int id, int length)", blob, "return null;")
	emitSwitchRet(f, "sofab.InlineInt64Array? onUnsignedArray(int id, int count)", uArr, "return null;")
	emitSwitchRet(f, "sofab.InlineInt64Array? onSignedArray(int id, int count)", sArr, "return null;")
	emitSwitchRet(f, "sofab.InlineFloat32Array? onFp32Array(int id, int count)", f32Arr, "return null;")
	emitSwitchRet(f, "sofab.InlineFloat64Array? onFp64Array(int id, int count)", f64Arr, "return null;")
	// The corelib's onSequenceStart skips by default, so a scope that binds no
	// sequence leaves it alone (forward-compat + §7.3).
	emitSwitchRet(f, "sofab.MessageVisitor? onSequenceStart(int id)", seq, "return null;")
	f.line("}")
	f.blank()
}

// destReturn is the body of an aggregate header arm: judge the announced length
// or count `n`, then hand over the destination `acc`.
//
// TWO bounds land here and they are mutually exclusive by rule (§6.2.1): a field
// the schema bounds is governed by its own `maxlen`/`count` and is INVALID above
// it (§7.1); a field the schema leaves unbounded is governed by the receiver's
// configured cap and is limitExceeded() above it. The two categories must not be
// folded -- a cap rejects well-formed bytes that decode under a looser cap -- and
// a cap never reaches a field the schema already bounds.
//
// A destination sized to its bound at construction (eagerDest) is handed over as
// it is. Any other gets storage of exactly the count when what it holds is
// short -- allocated once, from a count that has just been checked, and never
// grown element by element (ARCHITECTURE §9.5). The old contents are not kept:
// the codec overwrites all `n` elements, so copying them over would be waste.
func (g *gen) destReturn(fld *ir.Field, acc, n string, capLive bool, capExpr string) string {
	size := fmt.Sprintf("if (%s.capacity < %s) %s.storage = %s(%s);\n        ", acc, n, acc, storageType(fld), n)
	if bound, ok := destBound(fld); ok {
		if eagerDest(fld) {
			size = ""
		}
		return fmt.Sprintf("if (%s > %d) invalidate();\n        %sreturn %s;", n, bound, size, acc)
	}
	guard := ""
	if capLive {
		guard = fmt.Sprintf("if (%s > %s) limitExceeded();\n        ", n, capExpr)
	}
	return fmt.Sprintf("%s%sreturn %s;", guard, size, acc)
}

// storageType is the typed list a destination field's storage is.
func storageType(fld *ir.Field) string {
	if fld.Kind != ir.KindArray {
		return "Uint8List"
	}
	switch fld.Elem {
	case ir.KindFP32:
		return "Float32List"
	case ir.KindFP64:
		return "Float64List"
	}
	return "Int64List"
}

// widthGuard renders the §7.1 rejection for a scalar store into a destination
// the schema declares with Kind k — and, for a composite kind, `ref` carries the
// rest of that declaration. "" when nothing reachable can breach the bound: the
// 64-bit kinds, whose range IS the callback parameter's own, an enum
// whose constants need the full i64 and a bitfield whose highest declared `pos`
// needs the full u64. It rejects through the same set as the maxlen and count
// rejects.
//
// What the schema declares is what binds, and every declaration binds a WIDTH.
// For an integer it is the declared one (documentation#32): a `u8`/`u16`/`u32`/
// `i8`/`i16`/`i32` destination carrying a value outside its declared range is
// malformed input, INVALID — never masked to the width, never kept. An `enum`
// and a `bitfield` bind the width their declaration IMPLIES (MESSAGE_SPEC §1) —
// the smallest signed type holding every constant, the smallest unsigned type
// holding the highest `pos` — and declaredWidthCond answers for those.
//
// One clause serves four of the six positions a value lands in: this loop runs
// once per message, struct and union visitor, so the message field, the struct
// member, the struct-array element's member and the union member share it. The
// two element positions -- a native array and a matrix row -- carry the same
// interval on their destination instead (elemRange), where the codec applies it
// (generator#516).
//
// The `value < 0` term is not redundant on the unsigned WIDTH side: Dart's int
// is a 64-bit SIGNED integer with no unsigned counterpart, so an unsigned wire
// value at or above 2^63 arrives negative and `value > 255` alone would wave
// through exactly the largest values. Every narrow maximum is below 2^63, so
// treating negative as out-of-range is right for all of them. A bitfield needs
// no such term and gets the same answer for free: its guard masks the implied
// WIDTH off the raw 64 bits, and a value arriving negative has bits set above
// every width this can bound.
func widthGuard(k ir.Kind, ref *ir.TypeRef) string {
	cond := widthCond("value", k)
	if cond == "" {
		cond = declaredWidthCond("value", k, ref)
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

// declaredWidthCond is the reject comparison for an `enum` and a `bitfield`,
// MESSAGE_SPEC §1: each is bound by the WIDTH its declaration implies — for an
// enum the smallest SIGNED type holding every declared constant, for a bitfield
// the smallest UNSIGNED type holding its highest declared `pos`. It returns ""
// for every other kind (widthCond owns those) and wherever the implied width is
// the 64-bit accumulator itself, where the test is a tautology and the clause
// would be dead code.
//
// A value INSIDE that width is valid even when the schema names no constant for
// it and even when it carries an undeclared bit; only a value outside it is
// malformed input. So an enum declaring {0, 1, 2, 10} admits 5 and refuses 200,
// and a bitfield declaring positions 0, 1 and 3 admits 4 — the undeclared bit 2
// — and refuses 256. An undeclared bit is NOT masked away: masking would turn
// malformed-looking input into a DECLARED combination and report it Ok.
//
// This replaces the set/mask bound of generator#530, which implemented the
// closed-type reading MESSAGE_SPEC carried for six days (doc PR #89, `a50db95`)
// and doc PR #95 (`382159e`) withdrew. Both bounds are ordinary intervals now,
// which is §1's own reason for the change: an array's elements are consumed
// inside the corelib loop, so a bound must cross that channel as an interval,
// and a width fits where a set does not. That is why an array destination's
// `range:` carries the whole bound, and why no generated code has to state it
// per element.
//
// Storage is still never the bound. Dart holds both kinds in its own `int`
// whatever the schema declares, which is §1's fourth consequence directly: a
// receiver that cannot hold the field at exactly the declared width holds it
// wider and MUST then enforce the width as an explicit check, because nothing
// about its storage will. So the comparison runs on the value the corelib handed
// over, before anything is stored.
func declaredWidthCond(v string, k ir.Kind, ref *ir.TypeRef) string {
	switch k {
	case ir.KindEnum:
		lo, hi, ok := ir.EnumWidthRange(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf("%s < %d || %s > %d", v, lo, v, hi)
	case ir.KindBitfield:
		// Spelled as a MASK of the width rather than `v < 0 || v > hi`: Dart's int
		// is a 64-bit signed word with no unsigned counterpart, so an unsigned wire
		// value at or above 2^63 arrives negative, and one mask on the raw bits
		// refuses both an over-width value and that sign-bit case in a single
		// operation. The widest mask a width can produce is `~0xffffffff`, so the
		// literal-rendering trap of generator#470 — a mask with bit 63 set, which
		// has no decimal spelling in Dart — is not reachable from here any more.
		hi, ok := ir.BitfieldWidthMax(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf("(%s & ~0x%x) != 0", v, hi)
	}
	return ""
}

// elemRange is the inclusive INTERVAL an element may take: the declared width,
// or the width an `enum`/`bitfield` declaration implies (MESSAGE_SPEC §1). It is
// what an integer array destination's `range:` and the matrix-row collector
// are handed -- the two places an element's bound crosses into the corelib,
// which applies it to every element as it is decoded, on both decode surfaces
// and ahead of a truncated tail (MESSAGE_SPEC §5.2, generator#267).
//
// ok is false where no interval narrows anything, and each case is a real one:
// u64/i64 and bool, whose range IS the callback parameter's own, and an enum
// whose constants need the full i64 or a bitfield whose highest declared `pos`
// needs the full u64, for the same reason.
//
// The widest maximum this returns is 0xffffffff — a bitfield implying u32 — so
// it always fits a positive Dart `int`. Both hooks compare an unsigned element
// as `v < 0 || v > max`, and the bit-63 mask that had no positive maximum to
// state (generator#470) is not reachable from a width.
func elemRange(k ir.Kind, ref *ir.TypeRef) (lo, hi int64, ok bool) {
	if lo, hi, ok = ir.NarrowRange(k); ok {
		return lo, hi, true
	}
	switch k {
	case ir.KindEnum:
		return ir.EnumWidthRange(ref)
	case ir.KindBitfield:
		max, isBf := ir.BitfieldWidthMax(ref)
		if !isBf {
			return 0, 0, false
		}
		return 0, int64(max), true
	}
	return 0, 0, false
}

// collector returns the Dart expression constructing the MessageVisitor that
// gathers a wrapper-sequence array's elements into the (freshly-cleared) list
// `out`. It recurses for nested arrays. The string/blob and matrix collectors
// decode each element straight into its slot of `out` -- an `Inline…`
// destination, reused where it already holds enough storage.
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
			// announces as a real count header because a row IS a native array.
			rows := fmt.Sprintf("%s, rowCount: %d, rowCap: %s", rcap, capOf(items.HasCount, items.Count), g.arrayCapExpr())
			switch items.Elem {
			case ir.KindFP32:
				return fmt.Sprintf("sofab.Float32MatrixSeq(%s, %d%s)", out, cap, rows)
			case ir.KindFP64:
				return fmt.Sprintf("sofab.Float64MatrixSeq(%s, %d%s)", out, cap, rows)
			}
			// lo/hi bound a ROW's elements, and this pair is the ONLY bound the
			// position has: a row's values never reach the generated visitor, so
			// there is no store here to guard. Under the width rule it carries the
			// whole bound for an `enum` and a `bitfield` too (MESSAGE_SPEC §1). A
			// bool row has none -- any non-zero element is `true` (§4.4) -- and
			// neither does a 64-bit one: equal lo/hi mean "nothing to check".
			lo, hi, _ := elemRange(items.Elem, items.ElemRef)
			return fmt.Sprintf("sofab.IntMatrixSeq(%s, %d, %v, %d, %d%s)", out, cap, signedArrayElem(items.Elem), lo, hi, rows)
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

// ---- shared prelude (helpers) ----------------------------------------------

// needs records which prelude helpers a schema actually uses, so only those are
// emitted: `dart analyze --fatal-infos` is this backend's build gate and
// rejects an unreferenced declaration.
type needs struct {
	f32bits bool
	// bools: some bool array or bool matrix row is written, through _bools01.
	bools bool
	// boolDefault: some bool array declares a default, compared by _boolsEq.
	boolDefault bool
	// prefixEq: some string, blob or non-bool native array declares a default,
	// compared by _prefixEq.
	prefixEq bool
}

func (g *gen) computeNeeds(s *ir.Schema) needs {
	var n needs
	scan := func(fields []*ir.Field) {
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
	if _, ok := g.defaultLit(fld); ok {
		if fld.Kind == ir.KindArray && fld.Elem == ir.KindBool {
			n.boolDefault = true
		} else {
			n.prefixEq = true
		}
	}
	switch fld.Kind {
	case ir.KindFP32:
		n.f32bits = true
	case ir.KindArray:
		if fld.Elem == ir.KindBool {
			n.bools = true
			return
		}
		if !nativeArrayElem(fld.Elem) {
			scanArrayElem(fld.Elem, fld.ElemItems, n)
		}
	}
}

// scanArrayElem descends a wrapper array's element type to the bool matrix row
// it may bottom out at.
func scanArrayElem(elem ir.Kind, items *ir.ArrayElem, n *needs) {
	if elem != ir.KindArray {
		return
	}
	if items.Elem == ir.KindBool {
		n.bools = true
		return
	}
	scanArrayElem(items.Elem, items.ElemItems, n)
}

func (g *gen) emitPrelude(f *dfile, s *ir.Schema) {
	n := g.computeNeeds(s)
	if n.f32bits {
		f.line("// Widen the 32 raw wire bits of an fp32 NaN to a display double for element")
		f.line("// access; the exact bits are kept alongside for a bit-for-bit re-encode.")
		f.line("double _f32FromBits(int bits) =>")
		f.line("    (ByteData(4)..setUint32(0, bits, Endian.little)).getFloat32(0, Endian.little);")
		f.blank()
	}
	if n.prefixEq {
		// A destination's storage is its CAPACITY, so a default compare reads the
		// first `length` elements -- no view, no copy.
		f.line("// Whether the first [n] elements of [s] are exactly [d]: the default test of")
		f.line("// a destination field, whose storage is sized to its capacity.")
		f.line("bool _prefixEq<T>(List<T> s, int n, List<T> d) {")
		f.line("  if (n != d.length) return false;")
		f.line("  for (var i = 0; i < n; i++) {")
		f.line("    if (s[i] != d[i]) return false;")
		f.line("  }")
		f.line("  return true;")
		f.line("}")
		f.blank()
	}
	if n.boolDefault {
		f.line("// The default test of a bool array: its elements compared as booleans, since")
		f.line("// any non-zero element decodes as `true`.")
		f.line("bool _boolsEq(Int64List s, int n, List<int> d) {")
		f.line("  if (n != d.length) return false;")
		f.line("  for (var i = 0; i < n; i++) {")
		f.line("    if ((s[i] != 0) != (d[i] != 0)) return false;")
		f.line("  }")
		f.line("  return true;")
		f.line("}")
		f.blank()
	}
	if n.bools {
		// A bool array decodes into the 64-bit elements the wire carries, and any
		// non-zero one is `true` (§4.4) -- so a decoded 5 is a `true` whose
		// canonical encoding is 1. Normalizing in place keeps the value and makes
		// the re-encode canonical without a copy.
		f.line("// Normalizes a bool array to its canonical 0/1 elements in place (any")
		f.line("// non-zero element is `true`) and returns its storage.")
		f.line("Int64List _bools01(sofab.InlineInt64Array a) {")
		f.line("  final s = a.storage;")
		f.line("  for (var i = 0; i < a.length; i++) {")
		f.line("    if (s[i] != 0) s[i] = 1;")
		f.line("  }")
		f.line("  return s;")
		f.line("}")
		f.blank()
	}
}
