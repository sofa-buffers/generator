package dart

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// ---- decode visitor -------------------------------------------------------

// emitVisitor emits the push child-visitor for an object's id scope. Scalars
// bind straight into a member; native arrays arrive whole and are copied (and,
// for a fixed-count array, padded to N); nested structs/unions and every
// wrapper-sequence array descend via onSequenceStart into a child visitor. A
// struct/union descent returns the EXISTING member's visitor, so a re-opened
// scope merges (MESSAGE_SPEC §7.4); an array wrapper clears its list first, so a
// re-opened wrapper is replaced. Unhandled ids fall through: a leaf id lands in
// an unarmed switch (no-op) and a sequence id returns null (skip), which is what
// makes a contradictory wire type evaporate structurally (MESSAGE_SPEC §7.3).
func (g *gen) emitVisitor(f *dfile, typeName string, fields []*ir.Field) {
	var uns, sig, f32, f64, str, blob []string
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
	var arrBegin, fixHdr []string

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
			uns = append(uns, arm(fld.ID, acc+" = value;"))
		case ir.KindBool:
			uns = append(uns, arm(fld.ID, acc+" = value != 0;"))
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
			sig = append(sig, arm(fld.ID, acc+" = value;"))
		case ir.KindFP32:
			f32 = append(f32, arm(fld.ID, acc+" = value;"))
		case ir.KindFP64:
			f64 = append(f64, arm(fld.ID, acc+" = value;"))
		case ir.KindString:
			body := acc + " = value;"
			if fld.HasMaxlen {
				// A wire byte length above the schema maxlen is malformed input
				// (MESSAGE_SPEC §7.1) — reject as INVALID, never truncate.
				body = fmt.Sprintf("if (_u8len(value) > %d) { e.inv = true; return; }\n        %s", fld.Maxlen, body)
				fixHdr = append(fixHdr, arm(fld.ID, fmt.Sprintf("if (length > %d) e.inv = true;", fld.Maxlen)))
			}
			str = append(str, arm(fld.ID, body))
		case ir.KindBlob:
			// value aliases the decode buffer — copy what we keep.
			body := acc + " = Uint8List.fromList(value);"
			if fld.HasMaxlen {
				body = fmt.Sprintf("if (value.length > %d) { e.inv = true; return; }\n        %s", fld.Maxlen, body)
				fixHdr = append(fixHdr, arm(fld.ID, fmt.Sprintf("if (length > %d) e.inv = true;", fld.Maxlen)))
			}
			blob = append(blob, arm(fld.ID, body))
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, seqArm(fld.ID, fmt.Sprintf("return %s(%s, e);", visitorName(g.typeName(fld.Ref.Key)), acc)))
		case ir.KindArray:
			g.emitArrayDecode(fld, acc, arm, seqArm, &uArr, &sArr, &f32Arr, &f64Arr, &seq, &arrBegin)
		}
	}

	f.line("class %s extends sofab.MessageVisitor {", visitorName(typeName))
	f.line("  %s(this.o, this.e);", visitorName(typeName))
	f.line("  final %s o;", typeName)
	f.line("  final _Dec e;")
	emitSwitch(f, "void onUnsigned(int id, int value)", uns)
	emitSwitch(f, "void onSigned(int id, int value)", sig)
	emitSwitch(f, "void onFp32(int id, double value)", f32)
	emitSwitch(f, "void onFp64(int id, double value)", f64)
	emitSwitch(f, "void onString(int id, String value)", str)
	emitSwitch(f, "void onBlob(int id, Uint8List value)", blob)
	emitSwitch(f, "void onUnsignedArray(int id, Int64List values)", uArr)
	emitSwitch(f, "void onSignedArray(int id, Int64List values)", sArr)
	emitSwitch(f, "void onFp32Array(int id, Float32List values)", f32Arr)
	emitSwitch(f, "void onFp64Array(int id, Float64List values)", f64Arr)
	// Header hooks fire at the count/length word before the truncation check
	// (generator#216). Emitted only when a field declares a bound, so a type with
	// none does not override them and the corelib's max-speed path is unchanged.
	emitSwitch(f, "void onArrayBegin(int id, int count)", arrBegin)
	emitSwitch(f, "void onFixlenHeader(int id, int subtype, int length)", fixHdr)
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

// emitArrayDecode appends the decode arm(s) for an array field to the right
// callback bucket. Native scalar arrays bind into the member (with an over-count
// INVALID guard and a fixed-count pad); wrapper-sequence arrays clear their list
// and descend into a collector.
func (g *gen) emitArrayDecode(fld *ir.Field, acc string, arm func(int64, string) string, seqArm func(int64, string) string, uArr, sArr, f32Arr, f64Arr, seq, arrBegin *[]string) {
	guard := ""
	if fld.HasCount {
		// A wire element count above the schema `count` is INVALID (MESSAGE_SPEC
		// §3+§7): reject, never clamp (generator#100).
		guard = fmt.Sprintf("if (values.length > %d) { e.inv = true; return; }\n        ", fld.Count)
		// Native arrays fire onArrayBegin at the count word; wrapper-sequence arrays
		// descend via onSequenceStart (no header hook) and are bounded at the
		// collector cap instead. So the header reject is only for the native kinds.
		if nativeArrayElem(fld.Elem) {
			*arrBegin = append(*arrBegin, arm(fld.ID, fmt.Sprintf("if (count > %d) e.inv = true;", fld.Count)))
		}
	}
	pad := ""
	if fld.HasCount {
		// A `count: N` array is fixed-length: positions [M, N) are the element
		// default. A growable list must materialize them (MESSAGE_SPEC S3).
		pad = fmt.Sprintf("\n        _padTo(%s, %d, %s);", acc, fld.Count, elemZero(fld.Elem))
	}
	switch {
	case unsignedArrayElem(fld.Elem) && fld.Elem == ir.KindBool:
		*uArr = append(*uArr, arm(fld.ID, guard+acc+" = [for (final _v in values) _v != 0];"+pad))
	case unsignedArrayElem(fld.Elem):
		*uArr = append(*uArr, arm(fld.ID, guard+acc+" = List<int>.from(values);"+pad))
	case signedArrayElem(fld.Elem):
		*sArr = append(*sArr, arm(fld.ID, guard+acc+" = List<int>.from(values);"+pad))
	case fld.Elem == ir.KindFP32:
		*f32Arr = append(*f32Arr, arm(fld.ID, guard+acc+" = List<double>.from(values);"+pad))
	case fld.Elem == ir.KindFP64:
		*f64Arr = append(*f64Arr, arm(fld.ID, guard+acc+" = List<double>.from(values);"+pad))
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
		if nativeArrayElem(items.Elem) {
			switch {
			case items.Elem == ir.KindBool:
				return fmt.Sprintf("_BoolMat(%s, e)", out)
			case items.Elem == ir.KindFP32 || items.Elem == ir.KindFP64:
				return fmt.Sprintf("_DblMat(%s, %v, e)", out, items.Elem == ir.KindFP64)
			default:
				return fmt.Sprintf("_IntMat(%s, %v, e)", out, signedArrayElem(items.Elem))
			}
		}
		// Array of wrapper arrays: each element opens a sequence collected into a
		// fresh inner list by a recursively-built collector.
		innerT := g.dartArrayElemType(items.Elem, items.ElemRef, items.ElemItems)
		inner := g.collector("p", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count), emaxOf(items.ElemMaxHas, items.ElemMax))
		return fmt.Sprintf("_SeqSeq<%s>(%s, e, (p) => %s)", innerT, out, inner)
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
	f.line("  }")
}

// ---- shared prelude (helpers + collectors) --------------------------------

// needs records which prelude helpers and collector classes a schema actually
// uses, so only those are emitted (clean output; nothing unused).
type needs struct {
	dec                             bool
	bytesEq, listEq, u8len          bool
	trimInt, trimF32, trimF64, pad  bool
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
	// _StrSeq references _u8len unconditionally (the emax guard is runtime-gated
	// but the call must still resolve), so a string wrapper array needs it emitted.
	if n.strSeq {
		n.u8len = true
	}
	return n
}

func (g *gen) scanField(fld *ir.Field, n *needs) {
	switch fld.Kind {
	case ir.KindBlob:
		if _, ok := g.blobDefaultLit(fld); ok {
			n.bytesEq = true
		}
	case ir.KindString:
		if fld.HasMaxlen {
			n.u8len = true
		}
	case ir.KindArray:
		if nativeArrayElem(fld.Elem) {
			if _, ok := fld.Default.([]any); ok {
				n.listEq = true
			}
			if fld.HasCount {
				n.pad = true
				switch fld.Elem {
				case ir.KindFP32:
					n.trimF32 = true
				case ir.KindFP64:
					n.trimF64 = true
				default:
					n.trimInt = true
				}
			}
			return
		}
		g.scanArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems, fld.ElemMaxHas, n)
	}
}

func (g *gen) scanArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, n *needs) {
	switch elem {
	case ir.KindString:
		n.strSeq = true
		if elemMaxHas {
			n.u8len = true
		}
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
		g.scanArrayElem(items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, n)
	}
}

func (g *gen) emitPrelude(f *dfile, s *ir.Schema) {
	n := g.computeNeeds(s)
	if !n.dec && !g.limits.any() {
		return
	}
	if n.dec {
		f.line("// A sticky INVALID flag shared across all visitors of one decode. corelib-dart")
		f.line("// visitor callbacks return void, so a schema-bound violation (over-count,")
		f.line("// over-index, over-maxlen) sets this and the generated decode converts it to a")
		f.line("// terminal INVALID after the corelib returns (the Rust/Zig sticky-flag model).")
		f.line("class _Dec {")
		f.line("  bool inv = false;")
		f.line("}")
		f.blank()
	}
	if n.bytesEq {
		f.line("bool _bytesEq(Uint8List a, Uint8List b) {")
		f.line("  if (a.length != b.length) return false;")
		f.line("  for (var i = 0; i < a.length; i++) { if (a[i] != b[i]) return false; }")
		f.line("  return true;")
		f.line("}")
		f.blank()
	}
	if n.listEq {
		f.line("bool _listEq<T>(List<T> a, List<T> b) {")
		f.line("  if (a.length != b.length) return false;")
		f.line("  for (var i = 0; i < a.length; i++) { if (a[i] != b[i]) return false; }")
		f.line("  return true;")
		f.line("}")
		f.blank()
	}
	if n.u8len {
		f.line("// Exact UTF-8 byte length of [s] without allocating a transcode buffer.")
		f.line("int _u8len(String s) {")
		f.line("  var n = 0;")
		f.line("  for (final r in s.runes) {")
		f.line("    if (r < 0x80) {")
		f.line("      n += 1;")
		f.line("    } else if (r < 0x800) {")
		f.line("      n += 2;")
		f.line("    } else if (r < 0x10000) {")
		f.line("      n += 3;")
		f.line("    } else {")
		f.line("      n += 4;")
		f.line("    }")
		f.line("  }")
		f.line("  return n;")
		f.line("}")
		f.blank()
	}
	if n.trimInt {
		f.line("// Trailing-default-run trim for a fixed-count int array (MESSAGE_SPEC S3):")
		f.line("// drop the trailing run of element-default (0) values the canonical wire elides.")
		f.line("List<int> _trimInt(List<int> a) {")
		f.line("  var n = a.length;")
		f.line("  while (n > 0 && a[n - 1] == 0) { n--; }")
		f.line("  return n == a.length ? a : a.sublist(0, n);")
		f.line("}")
		f.blank()
	}
	if n.trimF32 || n.trimF64 {
		f.line("// Float trims compare by BIT PATTERN, so a trailing -0.0 (== 0.0) survives the")
		f.line("// round-trip and a NaN is never mistaken for the default (MESSAGE_SPEC S3).")
	}
	if n.trimF32 {
		f.line("List<double> _trimF32(List<double> a) {")
		f.line("  final bd = ByteData(4);")
		f.line("  var n = a.length;")
		f.line("  while (n > 0) {")
		f.line("    bd.setFloat32(0, a[n - 1]);")
		f.line("    if (bd.getInt32(0) != 0) break;")
		f.line("    n--;")
		f.line("  }")
		f.line("  return n == a.length ? a : a.sublist(0, n);")
		f.line("}")
		f.blank()
	}
	if n.trimF64 {
		f.line("List<double> _trimF64(List<double> a) {")
		f.line("  final bd = ByteData(8);")
		f.line("  var n = a.length;")
		f.line("  while (n > 0) {")
		f.line("    bd.setFloat64(0, a[n - 1]);")
		f.line("    if (bd.getInt64(0) != 0) break;")
		f.line("    n--;")
		f.line("  }")
		f.line("  return n == a.length ? a : a.sublist(0, n);")
		f.line("}")
		f.blank()
	}
	if n.pad {
		f.line("// Grow [a] to exactly [n] elements with the element default [zero]: a decoded")
		f.line("// fixed-count array has exactly its schema count elements (MESSAGE_SPEC S3).")
		f.line("void _padTo<T>(List<T> a, int n, T zero) {")
		f.line("  while (a.length < n) { a.add(zero); }")
		f.line("}")
		f.blank()
	}
	g.emitCollectors(f, n)
}

// emitCollectors emits the wrapper-sequence collector visitors the schema uses.
// Each is keyed by the 0-based element index id; the cap (schema count N, or -1
// for a dynamic array) rejects an over-index element as INVALID before the list
// grows (MESSAGE_SPEC §5.1/§7 — also bounding an over-index amplification DoS),
// and emax (element maxlen, or -1) rejects an over-length string/blob element.
func (g *gen) emitCollectors(f *dfile, n needs) {
	if n.strSeq {
		f.line("class _StrSeq extends sofab.MessageVisitor {")
		f.line("  _StrSeq(this.out, this.cap, this.emax, this.e);")
		f.line("  final List<String> out;")
		f.line("  final int cap;")
		f.line("  final int emax;")
		f.line("  final _Dec e;")
		f.line("  @override")
		f.line("  void onString(int id, String value) {")
		f.line("    if (cap >= 0 && id >= cap) { e.inv = true; return; }")
		f.line("    if (emax >= 0 && _u8len(value) > emax) { e.inv = true; return; }")
		f.line("    while (out.length <= id) { out.add(''); }")
		f.line("    out[id] = value;")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if n.blobSeq {
		f.line("class _BlobSeq extends sofab.MessageVisitor {")
		f.line("  _BlobSeq(this.out, this.cap, this.emax, this.e);")
		f.line("  final List<Uint8List> out;")
		f.line("  final int cap;")
		f.line("  final int emax;")
		f.line("  final _Dec e;")
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
		f.line("class _ObjSeq<T> extends sofab.MessageVisitor {")
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
	if n.intMat {
		f.line("class _IntMat extends sofab.MessageVisitor {")
		f.line("  _IntMat(this.out, this.signed, this.e);")
		f.line("  final List<List<int>> out;")
		f.line("  final bool signed;")
		f.line("  final _Dec e;")
		f.line("  void _row(int id, Int64List v) {")
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
		f.line("class _DblMat extends sofab.MessageVisitor {")
		f.line("  _DblMat(this.out, this.f64, this.e);")
		f.line("  final List<List<double>> out;")
		f.line("  final bool f64;")
		f.line("  final _Dec e;")
		f.line("  void _row(int id, List<double> v) {")
		f.line("    while (out.length <= id) { out.add(<double>[]); }")
		f.line("    out[id] = List<double>.from(v);")
		f.line("  }")
		f.line("  @override")
		f.line("  void onFp32Array(int id, Float32List values) { if (!f64) _row(id, values); }")
		f.line("  @override")
		f.line("  void onFp64Array(int id, Float64List values) { if (f64) _row(id, values); }")
		f.line("}")
		f.blank()
	}
	if n.boolMat {
		f.line("class _BoolMat extends sofab.MessageVisitor {")
		f.line("  _BoolMat(this.out, this.e);")
		f.line("  final List<List<bool>> out;")
		f.line("  final _Dec e;")
		f.line("  @override")
		f.line("  void onUnsignedArray(int id, Int64List values) {")
		f.line("    while (out.length <= id) { out.add(<bool>[]); }")
		f.line("    out[id] = [for (final v in values) v != 0];")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if n.seqSeq {
		f.line("class _SeqSeq<T> extends sofab.MessageVisitor {")
		f.line("  _SeqSeq(this.out, this.e, this.make);")
		f.line("  final List<List<T>> out;")
		f.line("  final _Dec e;")
		f.line("  final sofab.MessageVisitor Function(List<T>) make;")
		f.line("  @override")
		f.line("  sofab.MessageVisitor? onSequenceStart(int id) {")
		f.line("    while (out.length <= id) { out.add(<T>[]); }")
		f.line("    return make(out[id]);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
}
