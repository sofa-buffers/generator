package typescript

import "github.com/sofa-buffers/generator/internal/ir"

// emitDecode generates the message's decode surface: a static decode(bytes)
// entry plus a monomorphic pull decoder decodeFrom(c: Cursor). One switch(id)
// reads each field straight off a corelib Cursor into `this` (PLAN §6.4). Every
// reader call site has a single caller (this per-type decoder), so V8 keeps it
// monomorphic and inlines the loop — unlike the former push/visitor path, whose
// shared call sites went megamorphic across the nested message types. A nested
// message recurses into its own decodeFrom, which consumes through its matching
// SequenceEnd (readHeader() returns false there); an unknown id is consumed by
// skip() for forward/backward compatibility.
func (g *gen) emitDecode(f *tsfile, name string, fields []*ir.Field) {
	f.line("  static decode(bytes: Uint8Array): %s {", name)
	f.line("    return %s.decodeFrom(new Cursor(bytes));", name)
	f.line("  }")
	f.blank()
	f.line("  // Monomorphic pull decode: one switch(id) reads straight into this type's fields.")
	f.line("  static decodeFrom(c: Cursor): %s {", name)
	f.line("    const o = new %s();", name)
	f.line("    while (c.readHeader()) {")
	f.line("      switch (c.id) {")
	for _, x := range fields {
		g.emitDecodeCase(f, x)
	}
	f.line("      default: c.skip(c.wire); break;")
	f.line("      }")
	f.line("    }")
	f.line("    return o;")
	f.line("  }")
}

// emitDecodeCase emits the switch case reading one field off the cursor. Scalars
// read a single value (number-first for u64/i64); nested messages recurse into
// decodeFrom; native scalar arrays read the whole array in one call; composite
// (string/blob/message/nested-array) arrays loop readHeader over their wrapper
// sequence. The `as number[]`/`as bigint[]` casts bridge the reader's
// number-first (number|bigint)[] to the field's declared element type; the
// runtime values are byte-for-byte what the old visitor produced.
func (g *gen) emitDecodeCase(f *tsfile, x *ir.Field) {
	acc := "o." + x.Name
	switch x.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		f.line("      case %d: %s = Number(c.readUnsigned()); break;", x.ID, acc)
	case ir.KindU64:
		f.line("      case %d: %s = c.readUnsigned() as bigint; break;", x.ID, acc)
	case ir.KindBool:
		f.line("      case %d: %s = Boolean(c.readUnsigned()); break;", x.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32:
		f.line("      case %d: %s = Number(c.readSigned()); break;", x.ID, acc)
	case ir.KindI64:
		f.line("      case %d: %s = c.readSigned() as bigint; break;", x.ID, acc)
	case ir.KindEnum:
		f.line("      case %d: %s = Number(c.readSigned()) as %s; break;", x.ID, acc, g.typeName(x.Ref.Key))
	case ir.KindFP32:
		f.line("      case %d: %s = c.readFp32(); break;", x.ID, acc)
	case ir.KindFP64:
		f.line("      case %d: %s = c.readFp64(); break;", x.ID, acc)
	case ir.KindString:
		f.line("      case %d: %s = c.readString(); break;", x.ID, acc)
	case ir.KindBlob:
		f.line("      case %d: %s = c.readBlob(); break;", x.ID, acc)
	case ir.KindStruct, ir.KindUnion:
		f.line("      case %d: %s = %s.decodeFrom(c); break;", x.ID, acc, g.typeName(x.Ref.Key))
	case ir.KindArray:
		if nativeArrayElem(x.Elem) {
			f.line("      case %d: %s = %s; break;", x.ID, acc, g.nativeArrayRead(x.Elem, x.ElemRef))
			return
		}
		// Composite array: a wrapper sequence whose elements arrive one per
		// readHeader. Loop until the sequence-end (readHeader() -> false).
		f.line("      case %d: {", x.ID)
		f.line("        const arr: %s = [];", g.tsType(x))
		f.line("        while (c.readHeader()) { %s }", g.seqCollectBody("arr", x.Elem, x.ElemRef, x.ElemItems))
		f.line("        %s = arr;", acc)
		f.line("        break;")
		f.line("      }")
	}
}

// nativeArrayRead returns the expression reading a whole native scalar array off
// the cursor. u/i integer arrays read as number[] (u64/i64 as bigint[]); fp
// arrays have their own readers; bool arrays map to booleans and enum arrays cast
// each element to the enum type — the two conversions the number-first readers do
// not do inline (and that the reference decode patch's simpler schema never hit).
func (g *gen) nativeArrayRead(elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindU64:
		return "c.readUnsignedArray() as bigint[]"
	case ir.KindI64:
		return "c.readSignedArray() as bigint[]"
	case ir.KindI8, ir.KindI16, ir.KindI32:
		return "c.readSignedArray() as number[]"
	case ir.KindFP32:
		return "c.readFp32Array()"
	case ir.KindFP64:
		return "c.readFp64Array()"
	case ir.KindBool:
		return "(c.readUnsignedArray() as number[]).map((_e) => Boolean(_e))"
	case ir.KindEnum:
		return "(c.readSignedArray() as number[]).map((_e) => _e as " + g.typeName(ref.Key) + ")"
	default: // u8/u16/u32, bitfield
		return "c.readUnsignedArray() as number[]"
	}
}

// elemDecode returns the expression decoding ONE element of a composite wrapper
// sequence whose header readHeader() has just accepted. Leaf string/blob elements
// read a value; message elements recurse into decodeFrom (their opening
// SequenceStart was the header just read, and decodeFrom consumes to the matching
// SequenceEnd). A nested-array element is itself a row: a native inner array reads
// in one call, a composite inner array recurses via an inline IIFE loop.
func (g *gen) elemDecode(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "c.readString()"
	case ir.KindBlob:
		return "c.readBlob()"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key) + ".decodeFrom(c)"
	case ir.KindArray:
		if nativeArrayElem(items.Elem) {
			return g.nativeArrayRead(items.Elem, items.ElemRef)
		}
		rowT := g.tsArrayType(items.Elem, items.ElemRef, items.ElemItems)
		return "((): " + rowT + "[] => { const _r: " + rowT + "[] = []; while (c.readHeader()) { " +
			g.seqCollectBody("_r", items.Elem, items.ElemRef, items.ElemItems) + " } return _r; })()"
	}
	return "undefined as never"
}

// seqCollectBody returns the body of a `while (c.readHeader()) { ... }` loop that
// places one decoded element into arr. String/blob leaf elements are keyed by
// their wire id (MESSAGE_SPEC S2): a default (empty) element is omitted on the
// wire, so we grow arr with the element default ("" / empty bytes) and place the
// value at its id, restoring any gap. Composite elements (struct/union/nested-
// array) are always framed, never omitted, so they push in arrival order.
func (g *gen) seqCollectBody(arr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "const _id = c.id; while (" + arr + ".length <= _id) " + arr + `.push(""); ` + arr + "[_id] = c.readString();"
	case ir.KindBlob:
		return "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(new Uint8Array()); " + arr + "[_id] = c.readBlob();"
	default:
		return arr + ".push(" + g.elemDecode(elem, ref, items) + ");"
	}
}

// nativeArrayElem reports whether an array element is encoded as a native array
// wire type (numeric/enum/boolean/bitfield) rather than a wrapper sequence.
func nativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindEnum, ir.KindBool, ir.KindBitfield:
		return true
	}
	return false
}

// arrEqHelper is the element-wise equality helper the sparse-canonical marshal
// uses to decide whether a leaf blob or native scalar array equals a non-empty
// default (and may thus be omitted). It is emitted only when some field actually
// has such a value default (see usesArrEq); empty defaults use a `.length !== 0`
// guard instead, which needs no helper and no per-encode comparison allocation.
const arrEqHelper = `// arrEq is an element-wise equality check used by the sparse-canonical marshal to
// decide whether a leaf blob or native scalar array equals its default (and may
// thus be omitted). Works for Uint8Array and number/bigint/boolean arrays.
function arrEq(a: ArrayLike<unknown>, b: ArrayLike<unknown>): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}`
