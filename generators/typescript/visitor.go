package typescript

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

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
	f.line("    return %s.decodeFrom(new Cursor(bytes%s));", name, g.cursorLimits())
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
	// A Long-backed array decodes into the private backing field directly: the
	// readers produce canonical Long[], so the setter's fromValue pass (and its
	// array copy) would be pure overhead on the hot path.
	acc := g.storage("o", x)
	switch x.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		f.line("      case %d: %s = Number(c.readUnsigned()); break;", x.ID, acc)
	case ir.KindU64:
		if g.numberScalars() {
			f.line("      case %d: %s = Number(c.readUnsigned()); break;", x.ID, acc)
		} else {
			f.line("      case %d: %s = c.readUnsigned() as bigint; break;", x.ID, acc)
		}
	case ir.KindBool:
		f.line("      case %d: %s = Boolean(c.readUnsigned()); break;", x.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32:
		f.line("      case %d: %s = Number(c.readSigned()); break;", x.ID, acc)
	case ir.KindI64:
		if g.numberScalars() {
			f.line("      case %d: %s = Number(c.readSigned()); break;", x.ID, acc)
		} else {
			f.line("      case %d: %s = c.readSigned() as bigint; break;", x.ID, acc)
		}
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
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject the whole message, never keep-all
			// (generator#100). Count-less (dynamic) arrays have no bound.
			// A `count: N` array is fixed-length: a wire count M < N means the
			// elements at [M, N) are the element default, which the encoder does
			// not transmit. Materialize them so the decoded field always has
			// exactly N elements (MESSAGE_SPEC §3).
			if x.HasCount {
				f.line("      case %d: { const _a = %s; if (_a.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: array count above schema capacity %d\"); %s = _padTo(_a, %d, %s); break; }",
					x.ID, g.nativeArrayRead(x.Elem, x.ElemRef), x.Count, x.Name, x.Count, acc, x.Count, g.elemZero(x))
				return
			}
			f.line("      case %d: %s = %s; break;", x.ID, acc, g.nativeArrayRead(x.Elem, x.ElemRef))
			return
		}
		// Composite array: a wrapper sequence whose elements arrive one per
		// readHeader. Loop until the sequence-end (readHeader() -> false).
		f.line("      case %d: {", x.ID)
		f.line("        const arr: %s = [];", g.tsType(x))
		f.line("        while (c.readHeader()) { %s }", g.seqCollectBody("arr", x.Elem, x.ElemRef, x.ElemItems, capOf(x.HasCount, x.Count)))
		f.line("        %s = arr;", acc)
		f.line("        break;")
		f.line("      }")
	}
}

// elemZero renders a native array field's element default — zero for every
// native element kind — as the value _padTo fills a fixed-count array's elided
// trailing run with. A Long is immutable, so one shared zero instance is safe
// across the padded slots.
func (g *gen) elemZero(x *ir.Field) string {
	switch x.Elem {
	case ir.KindU64, ir.KindI64:
		if g.longArrays() {
			return "Long.fromValue(0)"
		}
		return "0n"
	case ir.KindBool:
		return "false"
	case ir.KindEnum:
		return "0 as " + g.typeName(x.ElemRef.Key)
	default: // u8/u16/u32, i8/i16/i32, fp32/fp64, bitfield
		return "0"
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
		if g.longArrays() {
			return "c.readUnsignedArrayLong()"
		}
		return "c.readUnsignedArray() as bigint[]"
	case ir.KindI64:
		if g.longArrays() {
			return "c.readSignedArrayLong()"
		}
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
			g.seqCollectBody("_r", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count)) + " } return _r; })()"
	}
	return "undefined as never"
}

// capOf maps a schema fixed-count bound to a wrapper array's cap: N when the
// array declares a count, -1 (dynamic/unbounded) otherwise.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// seqCollectBody returns the body of a `while (c.readHeader()) { ... }` loop that
// places one decoded element into arr. String/blob leaf elements are keyed by
// their wire id (MESSAGE_SPEC S2): a default (empty) element is omitted on the
// wire, so we grow arr with the element default ("" / empty bytes) and place the
// value at its id, restoring any gap. Composite elements (struct/union/nested-
// array) are always framed, never omitted, so they push in arrival order.
func (g *gen) seqCollectBody(arr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap int64) string {
	// Fixed-count wrapper array: an element id >= N is INVALID (MESSAGE_SPEC
	// §5.1/§7 — issue #142), rejected before the array grows, which also bounds an
	// over-index heap-amplification fill. A dynamic array keeps every index.
	guard := ""
	if cap >= 0 {
		guard = fmt.Sprintf(`if (c.id >= %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s: array index above schema capacity %d"); `, cap, arr, cap)
	}
	switch elem {
	case ir.KindString:
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + `.push(""); ` + arr + "[_id] = c.readString();"
	case ir.KindBlob:
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(new Uint8Array()); " + arr + "[_id] = c.readBlob();"
	default:
		return guard + arr + ".push(" + g.elemDecode(elem, ref, items) + ");"
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

// trimTailHelper is the encode-side trailing-default-run trim a fixed-count
// (`count: N`) native array's canonical wire requires: only the elements up to
// the last non-default one are emitted, and the decoder rebuilds the rest from
// the schema count. Elements compare by BIT PATTERN (Object.is), never by ===:
// -0 === 0 is true, so === would silently trim a trailing -0 to +0; Object.is
// also keeps a trailing NaN (never the default) on the wire.
const trimTailHelper = `// _trimTail returns a's leading run up to and including the last element that
// differs from the element default, i.e. it drops the trailing default run. Used
// only for fixed-count arrays, whose declared count lets the decoder rebuild the
// dropped run. Elements compare with Object.is (bit pattern), so a trailing -0 or
// NaN is not a default and survives the round-trip.
function _trimTail<T>(a: readonly T[], zero: T): readonly T[] {
  let n = a.length;
  while (n > 0 && Object.is(a[n - 1], zero)) n--;
  return n === a.length ? a : a.slice(0, n);
}`

// trimTailLongHelper is the Long[] flavour of the trim: a Long is an object
// identity, so the element default is tested by (low, high) word pair (Object.is
// would compare references and never match).
const trimTailLongHelper = `// _trimTailLong is _trimTail for Long[]: the element default (zero) is tested by
// (low, high) word pair, since Long objects are identities.
function _trimTailLong(a: readonly Long[]): readonly Long[] {
  let n = a.length;
  while (n > 0 && a[n - 1]!.low === 0 && a[n - 1]!.high === 0) n--;
  return n === a.length ? a : a.slice(0, n);
}`

// padToHelper is the decode-side counterpart of the trim: a fixed-count array
// decodes to exactly its schema count, so the trailing default run the encoder
// elided is materialized back. The corelib readers hand back a freshly allocated
// plain array, so the grow is in place.
const padToHelper = `// _padTo grows a to exactly n elements with the element default. A fixed-count
// array always decodes to its schema count: a wire count M < N means the elements
// at [M, N) are the element default, which the encoder does not transmit.
function _padTo<T>(a: T[], n: number, zero: T): T[] {
  while (a.length < n) a.push(zero);
  return a;
}`

// longArrEqHelper is the Long[] flavour of arrEq: Long elements are object
// identities, so the sparse-omission default compare goes by (low, high) word
// pairs instead of element !==. Emitted only when some Long-backed 64-bit
// array carries a non-empty schema default (see scanHelpers).
const longArrEqHelper = `// longArrEq is arrEq for Long[]: element-wise compare by (low, high) word pair
// (Long objects are identities, so !== would never match a default literal).
function longArrEq(a: readonly Long[], b: readonly Long[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i]!.low !== b[i]!.low || a[i]!.high !== b[i]!.high) return false;
  return true;
}`
