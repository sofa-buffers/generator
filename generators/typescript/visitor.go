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
	// Frame each field by the header wire type before reading (issue #160). The
	// schema-typed readers assume they are only called for their matching wire
	// type; a header whose wire type differs is skip()'d like an unknown id, which
	// keeps the cursor synced and lets the corelib reject malformed framing as
	// INVALID (or report truncation as INCOMPLETE) — the same framing every other
	// backend gets for free by driving the corelib's wire-type dispatch. Without
	// the guard a mismatched header (e.g. an array-fixlen header on a u8 field)
	// selects the wrong reader and desynchronizes the whole stream.
	ew := g.expectedWire(x)
	guard := fmt.Sprintf("if (c.wire !== %s) { c.skip(c.wire); break; } ", ew)
	switch x.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		f.line("      case %d: %s%s = Number(c.readUnsigned()); break;", x.ID, guard, acc)
	case ir.KindU64:
		if g.numberScalars() {
			f.line("      case %d: %s%s = Number(c.readUnsigned()); break;", x.ID, guard, acc)
		} else {
			f.line("      case %d: %s%s = c.readUnsigned() as bigint; break;", x.ID, guard, acc)
		}
	case ir.KindBool:
		f.line("      case %d: %s%s = Boolean(c.readUnsigned()); break;", x.ID, guard, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32:
		f.line("      case %d: %s%s = Number(c.readSigned()); break;", x.ID, guard, acc)
	case ir.KindI64:
		if g.numberScalars() {
			f.line("      case %d: %s%s = Number(c.readSigned()); break;", x.ID, guard, acc)
		} else {
			f.line("      case %d: %s%s = c.readSigned() as bigint; break;", x.ID, guard, acc)
		}
	case ir.KindEnum:
		f.line("      case %d: %s%s = Number(c.readSigned()) as %s; break;", x.ID, guard, acc, g.typeName(x.Ref.Key))
	case ir.KindFP32:
		f.line("      case %d: %s%s = c.readFp32(); break;", x.ID, guard, acc)
	case ir.KindFP64:
		f.line("      case %d: %s%s = c.readFp64(); break;", x.ID, guard, acc)
	case ir.KindString:
		// A wire string longer than its schema maxlen is malformed input: reject the
		// whole message rather than silently truncate (MESSAGE_SPEC §7.1). "Length"
		// is the UTF-8 BYTE length; the cursor hands back only the decoded string, so
		// count its bytes with the allocation-free _utf8Len (issue #153) rather than
		// re-encoding via TextEncoder in the hot loop. An unbounded string keeps the
		// bare read.
		if x.HasMaxlen {
			f.line("      case %d: { %sconst _s = c.readString(); if (_utf8Len(_s) > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: string byte length above schema maxlen %d\"); %s = _s; break; }",
				x.ID, guard, x.Maxlen, x.Name, x.Maxlen, acc)
		} else {
			f.line("      case %d: %s%s = c.readString(); break;", x.ID, guard, acc)
		}
	case ir.KindBlob:
		// A wire blob longer than its schema maxlen is malformed: reject, never
		// truncate (MESSAGE_SPEC §7.1). readBlob returns a Uint8Array view whose
		// .length is the exact wire byte length. An unbounded blob keeps the bare read.
		if x.HasMaxlen {
			f.line("      case %d: { %sconst _b = c.readBlob(); if (_b.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: blob byte length above schema maxlen %d\"); %s = _b; break; }",
				x.ID, guard, x.Maxlen, x.Name, x.Maxlen, acc)
		} else {
			f.line("      case %d: %s%s = c.readBlob(); break;", x.ID, guard, acc)
		}
	case ir.KindStruct, ir.KindUnion:
		f.line("      case %d: %s%s = %s.decodeFrom(c); break;", x.ID, guard, acc, g.typeName(x.Ref.Key))
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
				f.line("      case %d: { %sconst _a = %s; if (_a.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: array count above schema capacity %d\"); %s = _padTo(_a, %d, %s); break; }",
					x.ID, guard, g.nativeArrayRead(x.Elem, x.ElemRef), x.Count, x.Name, x.Count, acc, x.Count, g.elemZero(x))
				return
			}
			f.line("      case %d: %s%s = %s; break;", x.ID, guard, acc, g.nativeArrayRead(x.Elem, x.ElemRef))
			return
		}
		// Composite array: a wrapper sequence whose elements arrive one per
		// readHeader. Loop until the sequence-end (readHeader() -> false).
		f.line("      case %d: {", x.ID)
		f.line("        if (c.wire !== %s) { c.skip(c.wire); break; }", ew)
		f.line("        const arr: %s = [];", g.tsType(x))
		f.line("        while (c.readHeader()) { %s }", g.seqCollectBody("arr", x.Elem, x.ElemRef, x.ElemItems, capOf(x.HasCount, x.Count), x.ElemMaxHas, x.ElemMax))
		f.line("        %s = arr;", acc)
		f.line("        break;")
		f.line("      }")
	}
}

// expectedWire returns the WireType member a field's header must carry for its
// schema-typed reader to be the right one (issue #160). It mirrors the encode
// side (emitMarshal / marshalArray): unsigned integers, bool and bitfield ->
// Unsigned; signed integers and enum -> Signed; fp32/fp64, string and blob ->
// Fixlen; nested messages and composite arrays -> SequenceStart; native scalar
// arrays -> the matching Array* wire type. A header whose wire type differs is
// framed and skipped rather than misread.
func (g *gen) expectedWire(x *ir.Field) string {
	switch x.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBool, ir.KindBitfield:
		return "WireType.Unsigned"
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "WireType.Signed"
	case ir.KindFP32, ir.KindFP64, ir.KindString, ir.KindBlob:
		return "WireType.Fixlen"
	case ir.KindStruct, ir.KindUnion:
		return "WireType.SequenceStart"
	case ir.KindArray:
		if nativeArrayElem(x.Elem) {
			return arrayWire(x.Elem)
		}
		return "WireType.SequenceStart"
	}
	return "WireType.SequenceStart" // unreachable: keeps the switch total
}

// arrayWire returns the native scalar-array wire type for an element kind,
// mirroring marshalArray's writer choice: signed integers and enum ->
// ArraySigned, fp32/fp64 -> ArrayFixlen, everything else (unsigned integers,
// bool, bitfield) -> ArrayUnsigned.
func arrayWire(elem ir.Kind) string {
	switch elem {
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "WireType.ArraySigned"
	case ir.KindFP32, ir.KindFP64:
		return "WireType.ArrayFixlen"
	default: // u8/u16/u32/u64, bool, bitfield
		return "WireType.ArrayUnsigned"
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
			return "Long.ZERO"
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
			g.seqCollectBody("_r", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count), items.ElemMaxHas, items.ElemMax) + " } return _r; })()"
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
func (g *gen) seqCollectBody(arr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap int64, maxHas bool, maxVal int64) string {
	// Fixed-count wrapper array: an element id >= N is INVALID (MESSAGE_SPEC
	// §5.1/§7 — issue #142), rejected before the array grows, which also bounds an
	// over-index heap-amplification fill. A dynamic array keeps every index.
	guard := ""
	if cap >= 0 {
		guard = fmt.Sprintf(`if (c.id >= %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s: array index above schema capacity %d"); `, cap, arr, cap)
	}
	switch elem {
	case ir.KindString:
		// A bounded string element that overruns its schema maxlen is malformed:
		// reject, never truncate (MESSAGE_SPEC §7.1). "Length" is the UTF-8 byte
		// length, counted by the allocation-free _utf8Len rather than re-encoding the
		// decoded string via TextEncoder in the hot loop (issue #153).
		if maxHas {
			return guard + "const _id = c.id; while (" + arr + `.length <= _id) ` + arr + `.push(""); const _s = c.readString(); ` +
				fmt.Sprintf(`if (_utf8Len(_s) > %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s element: string byte length above schema maxlen %d"); `, maxVal, arr, maxVal) +
				arr + "[_id] = _s;"
		}
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + `.push(""); ` + arr + "[_id] = c.readString();"
	case ir.KindBlob:
		// A bounded blob element that overruns its schema maxlen is malformed:
		// reject, never truncate (MESSAGE_SPEC §7.1). readBlob's Uint8Array .length
		// is the exact wire byte length.
		if maxHas {
			return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(new Uint8Array()); const _b = c.readBlob(); " +
				fmt.Sprintf(`if (_b.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s element: blob byte length above schema maxlen %d"); `, maxVal, arr, maxVal) +
				arr + "[_id] = _b;"
		}
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

// utf8LenHelper counts a string's UTF-8 byte length without allocating — no
// TextEncoder, no throwaway Uint8Array — for the decode-side maxlen check on a
// bounded string field (MESSAGE_SPEC §7.1, issue #153). It is byte-for-byte
// identical to `new TextEncoder().encode(s).length`: an unpaired surrogate counts
// as the 3-byte U+FFFD replacement, matching what the corelib's TextDecoder
// produced, so validation semantics are unchanged. Emitted only when some bounded
// string field decodes (blob maxlen checks read the wire Uint8Array .length).
const utf8LenHelper = `// _utf8Len returns the UTF-8 byte length of s without allocating (mirrors what the
// encode path already does). Used to bound a decoded string against its schema
// maxlen on the hot decode path (issue #153).
function _utf8Len(s: string): number {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length && (s.charCodeAt(i + 1) & 0xfc00) === 0xdc00) { n += 4; i++; }
    else n += 3;
  }
  return n;
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
