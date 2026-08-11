package typescript

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// emitDecode generates the message's decode surface: a static decode(bytes)
// entry plus a monomorphic pull decoder decodeFrom(c: Cursor). One switch(id)
// reads each field straight off a corelib Cursor into `this` (PLAN §6.4). Every
// reader call site has a single caller (this per-type decoder), so V8 keeps it
// monomorphic and inlines the loop — unlike the former push/visitor path, whose
// shared call sites went megamorphic across the nested message types. A nested
// message recurses into its own decodeInto, which consumes through its matching
// SequenceEnd (readHeader() returns false there); an unknown id is consumed by
// skip() for forward/backward compatibility.
//
// The loop body lives in decodeInto(c, o) rather than decodeFrom so a re-opened
// nested scope can decode INTO the member an earlier opening already populated
// (MESSAGE_SPEC §7.4): last occurrence wins per field id, and children the
// earlier opening set whose ids do not recur are retained. decodeFrom keeps its
// signature as the fresh-object entry point.
func (g *gen) emitDecode(f *tsfile, name string, fields []*ir.Field) {
	f.line("  static decode(bytes: Uint8Array): %s {", name)
	f.line("    return %s.decodeFrom(new Cursor(bytes%s));", name, g.cursorLimits())
	f.line("  }")
	f.blank()
	f.line("  static decodeFrom(c: Cursor): %s {", name)
	f.line("    return %s.decodeInto(c, new %s());", name, name)
	f.line("  }")
	f.blank()
	f.line("  // Monomorphic pull decode: one switch(id) reads straight into this type's fields.")
	f.line("  // Decodes into `o` so a re-opened sequence continues its scope.")
	f.line("  static decodeInto(c: Cursor, o: %s): %s {", name, name)
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
	guard := fmt.Sprintf("if (%s) { c.skip(c.wire); break; } ", g.tsWireGuardCond(x))
	switch x.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		if cond := widthCond("_v", x.Kind); cond != "" {
			f.line("      case %d: { %sconst _v = Number(c.readUnsigned()); if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: value outside declared width %s\"); %s = _v; break; }", x.ID, guard, cond, x.Name, x.Kind, acc)
		} else {
			f.line("      case %d: %s%s = Number(c.readUnsigned()); break;", x.ID, guard, acc)
		}
	case ir.KindU64:
		if g.numberScalars() {
			f.line("      case %d: %s%s = Number(c.readUnsigned()); break;", x.ID, guard, acc)
		} else {
			f.line("      case %d: %s%s = c.readUnsigned() as bigint; break;", x.ID, guard, acc)
		}
	case ir.KindBool:
		f.line("      case %d: %s%s = Boolean(c.readUnsigned()); break;", x.ID, guard, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32:
		f.line("      case %d: { %sconst _v = Number(c.readSigned()); if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: value outside declared width %s\"); %s = _v; break; }", x.ID, guard, widthCond("_v", x.Kind), x.Name, x.Kind, acc)
	case ir.KindI64:
		if g.numberScalars() {
			f.line("      case %d: %s%s = Number(c.readSigned()); break;", x.ID, guard, acc)
		} else {
			f.line("      case %d: %s%s = c.readSigned() as bigint; break;", x.ID, guard, acc)
		}
	case ir.KindEnum:
		f.line("      case %d: %s%s = Number(c.readSigned()) as %s; break;", x.ID, guard, acc, g.typeName(x.Ref.Key))
	case ir.KindFP32:
		// Bit-exact fp32 decode (MESSAGE_SPEC §4.6, generator#235): read the four
		// wire bytes rather than the widened number, because widening an fp32
		// signaling NaN into a JS double quiets it (0x7F800001 -> 0x7FC00001) and
		// the field could then never be re-encoded bit-for-bit. The number is
		// derived from those same bytes for the value consumer; the bytes are kept
		// beside it only when the value is a NaN, the one case a double cannot
		// carry. The copy is required: readFp32Raw returns a view aliasing the
		// decoder's buffer, valid only until it is reused (same contract as
		// readBlob), and the object outlives one feed. The assignment is
		// unconditional so a re-opened field id (§7.4) drops the bits a previous
		// occurrence captured instead of re-emitting them under a new value.
		f.line("      case %d: { %sconst _r = c.readFp32Raw(); const _v = _fp32FromRaw(_r, 0); %s = _v; %s = Number.isNaN(_v) ? _r.slice() : null; break; }",
			x.ID, guard, acc, g.fp32RawStorage("o", x))
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
			// The schema maxlen is passed to readString so an over-maxlen string is
			// rejected as INVALID at the length word — before the payload take() can
			// report it truncated — so INVALID dominates a subsequent truncation
			// (generator#216 / F-0032, §5.2). The whole-string _utf8Len guard stays as
			// defense; it runs too late to beat truncation on its own.
			f.line("      case %d: { %sconst _s = c.readString(%d); if (_utf8Len(_s) > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: string byte length above schema maxlen %d\"); %s = _s; break; }",
				x.ID, guard, x.Maxlen, x.Maxlen, x.Name, x.Maxlen, acc)
		} else {
			f.line("      case %d: %s%s = c.readString(); break;", x.ID, guard, acc)
		}
	case ir.KindBlob:
		// A wire blob longer than its schema maxlen is malformed: reject, never
		// truncate (MESSAGE_SPEC §7.1). readBlob returns a Uint8Array view whose
		// .length is the exact wire byte length. An unbounded blob keeps the bare read.
		//
		// The view is COPIED into the destination: readBlob hands back a window into
		// the input buffer, and a decoded field owns its bytes, so the message
		// outlives the buffer it was decoded from (CORELIB_PLAN §5.1 read on the
		// decode side). The maxlen guard runs on the view, before the copy, so
		// nothing over-bound is ever duplicated.
		if x.HasMaxlen {
			// See the string case: the maxlen is passed to readBlob so an over-maxlen
			// blob is INVALID at the length word, dominating a truncated payload
			// (generator#216 / §5.2); the _b.length guard stays as defense.
			f.line("      case %d: { %sconst _b = c.readBlob(%d); if (_b.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: blob byte length above schema maxlen %d\"); %s = _b.slice(); break; }",
				x.ID, guard, x.Maxlen, x.Maxlen, x.Name, x.Maxlen, acc)
		} else {
			f.line("      case %d: %s%s = c.readBlob().slice(); break;", x.ID, guard, acc)
		}
	case ir.KindStruct, ir.KindUnion:
		// Decode INTO the existing member, never replace it: a field id repeating
		// within one scope re-opens that scope rather than starting a new one, so
		// children an earlier opening set whose ids do not recur must survive
		// (MESSAGE_SPEC §7.4). The member is always constructed (tsDefault emits
		// `new T()`), so there is nothing to allocate here. Assigning
		// decodeFrom(c)'s fresh object instead would discard the earlier opening.
		f.line("      case %d: %s%s.decodeInto(c, %s); break;", x.ID, guard, g.typeName(x.Ref.Key), acc)
	case ir.KindArray:
		if x.Elem == ir.KindFP32 {
			// Same §4.6 raw channel as the scalar above, one position deeper: every
			// element widens through a JS double, so readFp32Array would quiet a
			// signaling NaN element exactly as readFp32 quiets a scalar one
			// (generator#235). Only a native fp32 array (the field's own element kind)
			// is covered; an fp32 row nested inside a wrapper array still widens --
			// see docs/generator/typescript.md.
			g.emitFp32ArrayDecodeCase(f, x, guard, acc)
			return
		}
		if nativeArrayElem(x.Elem) {
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject the whole message, never keep-all
			// (generator#100). Count-less (dynamic) arrays have no bound.
			//
			// The wire count M IS the array's length (§3): the M elements that
			// arrived are the whole value, so they are taken as they come. A
			// declared `count: N` is a CAPACITY and bounds M; it never adds
			// elements, so there is nothing to fill in at [M, N).
			if x.HasCount {
				f.line("      case %d: { %sconst _a = %s; if (_a.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: array count above schema capacity %d\"); %s%s = _a; break; }",
					x.ID, guard, g.nativeArrayRead(x.Elem, x.ElemRef, fmt.Sprintf("%d", x.Count)), x.Count, x.Name, x.Count, widthScan("_a", x.Name, x.Elem), acc)
				return
			}
			if scan := widthScan("_a", x.Name, x.Elem); scan != "" {
				f.line("      case %d: { %sconst _a = %s; %s%s = _a; break; }", x.ID, guard, g.nativeArrayRead(x.Elem, x.ElemRef, ""), scan, acc)
				return
			}
			f.line("      case %d: %s%s = %s; break;", x.ID, guard, acc, g.nativeArrayRead(x.Elem, x.ElemRef, ""))
			return
		}
		// Composite array: a wrapper sequence whose elements arrive one per
		// readHeader. Loop until the sequence-end (readHeader() -> false).
		f.line("      case %d: {", x.ID)
		f.line("        if (%s) { c.skip(c.wire); break; }", g.tsWireGuardCond(x))
		f.line("        const arr: %s = [];", g.tsType(x))
		f.line("        while (c.readHeader()) { %s }", g.seqCollectBody("arr", x.Elem, x.ElemRef, x.ElemItems, capOf(x.HasCount, x.Count), x.ElemMaxHas, x.ElemMax))
		f.line("        %s = arr;", acc)
		f.line("        break;")
		f.line("      }")
	}
}

// emitFp32ArrayDecodeCase reads a native fp32 array through the corelib's raw
// channel (Cursor.readFp32ArrayRaw), the array half of the §4.6 bit-exact decode
// (generator#235). It is the fp32-array twin of the scalar case in
// emitDecodeCase, and keeps every property of the plain reader it replaces:
//
//   - the schema `count` still goes into the reader call, so an over-count array
//     is INVALID at the count word, before a truncated payload could report
//     INCOMPLETE (generator#216 / §5.2); the whole-array `_n > N` reject then
//     stays as the defense it has always been (generator#100);
//   - the wire count M IS the array's length (§3), so the M elements that arrived
//     are the whole value — nothing is padded to N;
//   - the payload is copied, not aliased (readFp32ArrayRaw returns a view into
//     the decoder's buffer), and only when some element is a NaN — the one case a
//     JS number cannot carry. A re-opened field id (§7.4) overwrites both slots,
//     so an earlier occurrence's bits never leak into a later value.
func (g *gen) emitFp32ArrayDecodeCase(f *tsfile, x *ir.Field, guard, acc string) {
	cnt := ""
	if x.HasCount {
		cnt = fmt.Sprintf("%d", x.Count)
	}
	f.line("      case %d: {", x.ID)
	f.line("        %s", strings.TrimSpace(guard))
	f.line("        const _p = c.readFp32ArrayRaw(%s);", cnt)
	f.line("        const _n = _p.length >> 2;")
	if x.HasCount {
		f.line("        if (_n > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: array count above schema capacity %d\");", x.Count, x.Name, x.Count)
	}
	f.line("        const _dv = new DataView(_p.buffer, _p.byteOffset, _p.byteLength);")
	f.line("        const _a = new Array<number>(_n);")
	f.line("        let _nan = false;")
	f.line("        for (let _i = 0; _i < _n; _i++) { const _v = _dv.getFloat32(_i * 4, true); if (Number.isNaN(_v)) _nan = true; _a[_i] = _v; }")
	f.line("        %s = _a;", acc)
	f.line("        %s = _nan ? _p.slice() : null;", g.fp32RawStorage("o", x))
	f.line("        break;")
	f.line("      }")
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

// tsFixSub returns the FixlenSubtype member a fixlen kind must carry, or "" for a
// kind the wire type alone already settles. fp32/fp64/string/blob all share
// WireType.Fixlen, and the fp32/fp64 native arrays share WireType.ArrayFixlen, so
// the subtype is the only thing that separates them — checking the wire type
// without it lets a wrong-subtype header (e.g. a string word on an fp64 field)
// pass the guard and then throw from the wrong-typed reader (corelib-ts#58).
func tsFixSub(k ir.Kind) string {
	switch k {
	case ir.KindFP32:
		return "FixlenSubtype.Fp32"
	case ir.KindFP64:
		return "FixlenSubtype.Fp64"
	case ir.KindString:
		return "FixlenSubtype.String"
	case ir.KindBlob:
		return "FixlenSubtype.Blob"
	}
	return ""
}

// fieldHasFixlenGuard reports whether any guard emitted for x — tsWireGuardCond
// on the field itself, plus one tsElemWireGuardCond per level of the array
// element chain — names a FixlenSubtype member. A fixlen kind anywhere in that
// chain does it: as a native array element the containing array's guard carries
// the subtype (WireType.ArrayFixlen is ambiguous), and as a wrapper element the
// element's own guard does (WireType.Fixlen is). A struct/union element carries
// no subtype; its own fields are scanned separately through the named types.
func fieldHasFixlenGuard(x *ir.Field) bool {
	if tsFixSub(x.Kind) != "" {
		return true
	}
	if x.Kind != ir.KindArray {
		return false
	}
	for elem, items := x.Elem, x.ElemItems; ; elem, items = items.Elem, items.ElemItems {
		if tsFixSub(elem) != "" {
			return true
		}
		if elem != ir.KindArray || items == nil {
			return false
		}
	}
}

// tsWireGuardCond renders the §7.3 condition a field header must satisfy before
// its schema-typed reader may run: the wire type the declared type maps to, plus
// the fixlen subtype where the wire type alone is ambiguous. c.fixSub is the
// companion to c.wire that corelib-ts records at readHeader (corelib-ts#58); it
// is a non-consuming peek, so an INVALID/INCOMPLETE fixlen word still surfaces
// from the reader as before.
func (g *gen) tsWireGuardCond(x *ir.Field) string {
	cond := "c.wire !== " + g.expectedWire(x)
	sub := tsFixSub(x.Kind)
	if x.Kind == ir.KindArray && nativeArrayElem(x.Elem) {
		sub = tsFixSub(x.Elem)
	}
	if sub != "" {
		cond += " || c.fixSub !== " + sub
	}
	return cond
}

// tsElemWireGuardCond is tsWireGuardCond for a wrapper-sequence ELEMENT: the
// §7.3 condition the element header must satisfy before its schema-typed reader
// runs. A leaf element (string/blob/struct/union) maps by its own kind; a nested
// native array by its inner element kind (like a native-array field), and a
// nested composite array by SequenceStart. Built by reusing the field-level
// mapping through a synthetic field so the two stay in lockstep.
func (g *gen) tsElemWireGuardCond(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	x := &ir.Field{Kind: elem, Ref: ref}
	if elem == ir.KindArray {
		x.Elem = items.Elem
		x.ElemRef = items.ElemRef
	}
	// The wrapper loop is entered after the array field's own header guard, which
	// narrows c.wire to WireType.SequenceStart; corelib-ts's `wire` reads as a
	// readonly getter, so TS does not widen it back across c.readHeader() and would
	// flag `c.wire !== <element wire>` as a no-overlap comparison. `as WireType`
	// restores the full type — a compile-time-only widen; c.wire is a WireType at
	// runtime, so the check is unchanged.
	cond := "(c.wire as WireType) !== " + g.expectedWire(x)
	sub := tsFixSub(elem)
	if elem == ir.KindArray && nativeArrayElem(items.Elem) {
		sub = tsFixSub(items.Elem)
	}
	if sub != "" {
		cond += " || c.fixSub !== " + sub
	}
	return cond
}

// nativeArrayRead returns the expression reading a whole native scalar array off
// the cursor. u/i integer arrays read as number[] (u64/i64 as bigint[]); fp
// arrays have their own readers; bool arrays map to booleans and enum arrays cast
// each element to the enum type — the two conversions the number-first readers do
// not do inline (and that the reference decode patch's simpler schema never hit).
// The optional cnt is the schema `count`, passed straight into the corelib
// reader (readUnsignedArray(schemaCount), …) so an over-count array is rejected
// as INVALID at the count word — before the reader's own truncated-array
// INCOMPLETE — making INVALID dominate a subsequent truncation (generator#216 /
// F-0032, MESSAGE_SPEC §5.2 anti-folding). The whole-array `_a.length > N` guard
// at the call site only fires once every element has arrived, so it cannot beat a
// truncated over-count; the header argument is what does. cnt is "" for an
// unbounded array (today's behavior, no bound).
// widthCond renders the §7.1 out-of-declared-width test for the expression
// `v` against Kind k, or "" for the kinds whose range is not narrower than what
// the reader returns (documentation#32). TypeScript keeps a decoded integer in a
// `number`, so nothing masks an out-of-range value here — the defect was that it
// was KEPT — and the throw is the same InvalidMsg channel as the maxlen and
// count rejects.
func widthCond(v string, k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("%s < %d || %s > %d", v, lo, v, hi)
	}
	return fmt.Sprintf("%s > %d", v, hi)
}

// widthScan renders the element scan for a native array of narrow-integer
// elements: one out-of-range element makes the whole message INVALID.
func widthScan(arr, name string, elem ir.Kind) string {
	cond := widthCond("_e", elem)
	if cond == "" {
		return ""
	}
	return fmt.Sprintf("for (const _e of %s) if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s element: value outside declared width %s\"); ", arr, cond, name, elem)
}

// elemArgs renders the extra corelib arguments that bound each element to its
// declared width — `, max` for unsigned, `, min, max` for signed — or "" for the
// kinds whose range is not narrower than what the reader returns.
//
// Same reasoning as the `cnt` argument above, one level down: the whole-array
// scan at the call site (widthScan) only fires once EVERY element has arrived, so
// a message truncated right after an out-of-range element loses the verdict to
// the reader's own INCOMPLETE. Handing the bound to the reader is what latches it
// at the element that carries the value (§5.2 anti-folding, generator#267).
//
// `cnt` is "" for an unbounded array, and a positional argument cannot be
// skipped, so the count slot is filled with `undefined` when a bound follows it.
func elemArgs(elem ir.Kind, cnt string) string {
	lo, hi, ok := ir.NarrowRange(elem)
	if !ok {
		return cnt
	}
	if cnt == "" {
		cnt = "undefined"
	}
	if lo < 0 {
		return fmt.Sprintf("%s, %d, %d", cnt, lo, hi)
	}
	return fmt.Sprintf("%s, %d", cnt, hi)
}

func (g *gen) nativeArrayRead(elem ir.Kind, ref *ir.TypeRef, cnt string) string {
	switch elem {
	case ir.KindU64:
		if g.longArrays() {
			return "c.readUnsignedArrayLong(" + cnt + ")"
		}
		return "c.readUnsignedArray(" + cnt + ") as bigint[]"
	case ir.KindI64:
		if g.longArrays() {
			return "c.readSignedArrayLong(" + cnt + ")"
		}
		return "c.readSignedArray(" + cnt + ") as bigint[]"
	case ir.KindI8, ir.KindI16, ir.KindI32:
		return "c.readSignedArray(" + elemArgs(elem, cnt) + ") as number[]"
	case ir.KindFP32:
		return "c.readFp32Array(" + cnt + ")"
	case ir.KindFP64:
		return "c.readFp64Array(" + cnt + ")"
	case ir.KindBool:
		return "(c.readUnsignedArray(" + cnt + ") as number[]).map((_e) => Boolean(_e))"
	case ir.KindEnum:
		return "(c.readSignedArray(" + cnt + ") as number[]).map((_e) => _e as " + g.typeName(ref.Key) + ")"
	default: // u8/u16/u32, bitfield
		return "c.readUnsignedArray(" + elemArgs(elem, cnt) + ") as number[]"
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
		// Copied, like every other blob destination: the read is a window into the
		// input buffer and a decoded field owns its bytes. seqCollectBody handles a
		// blob element itself today, so this arm is not reached — which is exactly
		// why it must not be the one place the rule is missing.
		return "c.readBlob().slice()"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key) + ".decodeFrom(c)"
	case ir.KindArray:
		if nativeArrayElem(items.Elem) {
			cnt := ""
			if items.HasCount {
				cnt = fmt.Sprintf("%d", items.Count)
			}
			return g.nativeArrayRead(items.Elem, items.ElemRef, cnt)
		}
		// tsArrayType already answers with the CONTAINER type for the level it is
		// handed: given the row's element kind (plus the row's own ElemItems for a
		// deeper row) it returns exactly the row's type — string[] for a row of
		// strings, string[][] for a row of rows of strings. Appending another "[]"
		// declared the collector with the container type of the level ABOVE while
		// its body collected the row's LEAF elements, so a string[][] accumulator
		// was pushed "" and assigned a string: TS2345/TS2322 on every
		// array<array<string|blob|struct>> and one level deeper (generator#250's
		// TypeScript analogue). The row collector's type must be the row's.
		rowT := g.tsArrayType(items.Elem, items.ElemRef, items.ElemItems)
		return "((): " + rowT + " => { const _r: " + rowT + " = []; while (c.readHeader()) { " +
			g.seqCollectBody("_r", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count), items.ElemMaxHas, items.ElemMax) + " } return _r; })()"
	}
	return "undefined as never"
}

// capOf maps a schema count bound to a wrapper array's cap: N when the array
// declares a count, -1 (unbounded) otherwise. N is a CAPACITY: the decoder uses it
// only to reject an out-of-range element id, never to size the result.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// elemDefaultNew renders a FRESH wrapper-array element default, the value the
// placement gap-fill grows the array with. Every composite element is mutable, so
// each slot gets its own instance — sharing one would let a later decode into slot
// i show up in slot j.
func (g *gen) elemDefaultNew(elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindString:
		return `""`
	case ir.KindBlob:
		return "new Uint8Array()"
	case ir.KindStruct, ir.KindUnion:
		return "new " + g.typeName(ref.Key) + "()"
	}
	return "[]" // a nested row: the empty array is the row default
}

// seqCollectBody returns the body of a `while (c.readHeader()) { ... }` loop that
// places one decoded element into arr. EVERY element kind is keyed by its wire id
// (MESSAGE_SPEC §5.1: the element id IS the array index): an INTERIOR element
// equal to the element default is omitted on the wire, so we grow arr with the
// element default and place the value at its id, restoring any gap. The array's
// LAST element is always on the wire, so the decoded length — highest present id
// + 1 — is exact.
func (g *gen) seqCollectBody(arr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap int64, maxHas bool, maxVal int64) string {
	// §5.1 makes every wrapper element a normal field with its own (id, type)
	// header, so §7.3 applies to it: an element whose wire type (or fixlen subtype)
	// is not the one its declared type maps to is skipped like an unknown id, NOT
	// fed to the schema-typed reader (which would throw). #174/#160 added this
	// framing to struct-field dispatch; this is the same guard one position deeper,
	// on the wrapper-element loop (issue #189). It runs before the over-index check
	// so a mis-typed element is skipped, not rejected for its id.
	guard := fmt.Sprintf("if (%s) { c.skip(c.wire); continue; } ", g.tsElemWireGuardCond(elem, ref, items))
	// Fixed-count wrapper array: an element id >= N is INVALID (MESSAGE_SPEC
	// §5.1/§7 — issue #142), rejected before the array grows, which also bounds an
	// over-index heap-amplification fill. A dynamic array keeps every index.
	if cap >= 0 {
		guard += fmt.Sprintf(`if (c.id >= %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s: array index above schema capacity %d"); `, cap, arr, cap)
	}
	switch elem {
	case ir.KindString:
		// A bounded string element that overruns its schema maxlen is malformed:
		// reject, never truncate (MESSAGE_SPEC §7.1). "Length" is the UTF-8 byte
		// length, counted by the allocation-free _utf8Len rather than re-encoding the
		// decoded string via TextEncoder in the hot loop (issue #153).
		if maxHas {
			// The bound goes INTO the reader, as it already does for a scalar
			// string. readString() reads the payload first, so a message truncated
			// inside an over-maxlen element raises INCOMPLETE and the check below
			// never runs -- while §5.2 makes INVALID dominate, the violation being
			// established by the length word alone. The post-read check stays: it is
			// the only bound left for a consumer on a corelib whose reader ignores
			// the argument, and it costs one length compare on a decoded string.
			return guard + "const _id = c.id; while (" + arr + `.length <= _id) ` + arr + `.push(""); ` +
				fmt.Sprintf("const _s = c.readString(%d); ", maxVal) +
				fmt.Sprintf(`if (_utf8Len(_s) > %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s element: string byte length above schema maxlen %d"); `, maxVal, arr, maxVal) +
				arr + "[_id] = _s;"
		}
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + `.push(""); ` + arr + "[_id] = c.readString();"
	case ir.KindBlob:
		// A bounded blob element that overruns its schema maxlen is malformed:
		// reject, never truncate (MESSAGE_SPEC §7.1). readBlob's Uint8Array .length
		// is the exact wire byte length.
		//
		// The element is COPIED for the same reason the scalar blob field is: the
		// read hands back a window into the input buffer, and a decoded field owns
		// its bytes. The maxlen guard still runs on the view, before the copy.
		if maxHas {
			// Same as the string element above: the bound travels with the read so
			// it is decided at the length word, not after the payload.
			return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(new Uint8Array()); " +
				fmt.Sprintf("const _b = c.readBlob(%d); ", maxVal) +
				fmt.Sprintf(`if (_b.length > %d) throw new SofabError(SofabErrorCode.InvalidMsg, "%s element: blob byte length above schema maxlen %d"); `, maxVal, arr, maxVal) +
				arr + "[_id] = _b.slice();"
		}
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(new Uint8Array()); " + arr + "[_id] = c.readBlob().slice();"
	case ir.KindStruct, ir.KindUnion:
		// The element id IS the array index (§5.1), exactly as for the string/blob
		// leaf paths above, so the element is PLACED at arr[id] after gap-filling
		// with default elements — never appended. Appending would shorten the array
		// by the size of any interior id gap, and would decode a REOPENED element id
		// as a second element instead of merging into the first (§7.4) — which
		// placement gives for free, because decodeInto continues the element an
		// earlier opening already populated. The over-index guard above rejects an
		// element id >= N, which also bounds the gap-fill.
		t := g.typeName(ref.Key)
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(new " + t + "()); " +
			t + ".decodeInto(c, " + arr + "[_id]!);"
	default:
		// A nested-array element (a matrix row, native or wrapper) is placed at the
		// index its element id names, growing arr with empty rows so an id GAP decodes
		// as an empty row instead of shifting every later row down by one. Rows were
		// pushed id-blind here: unreachable while every row was written, but an
		// interior row equal to the element default (the empty row) is now omitted
		// (§2), and only the LAST row is guaranteed present — which is what makes the
		// decoded length, highest present id + 1, exact. The over-index guard above
		// rejects a row id >= N, which also bounds the gap-fill. A REPEATED row id
		// replaces the row rather than appending a second one, which is what §7.4
		// asks of an array wrapper.
		return guard + "const _id = c.id; while (" + arr + ".length <= _id) " + arr + ".push(" + g.elemDefaultNew(elem, ref) + "); " +
			arr + "[_id] = " + g.elemDecode(elem, ref, items) + ";"
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
const arrEqHelper = `// arrEq is an element-wise equality check used by the sparse-canonical serialize to
// decide whether a leaf blob or native scalar array equals its default (and may
// thus be omitted). Works for Uint8Array and number/bigint/boolean arrays.
function arrEq(a: ArrayLike<unknown>, b: ArrayLike<unknown>): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
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
// maxlen on the hot decode path.
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

// fp32RawHelper is the scalar half of the fp32 raw-bits channel (MESSAGE_SPEC
// §4.6, generator#235). A JS number is a 64-bit double, and widening an fp32
// SIGNALING NaN into one quiets it (0x7F800001 -> 0x7FC00001), so the number
// alone can never re-encode that field bit-for-bit. Decode therefore reads the
// four wire bytes (Cursor.readFp32Raw) and widens them here for the value
// consumer, keeping a copy of the bytes only when the value is a NaN. Every
// non-NaN fp32 narrows back to its own bits exactly, so nothing else needs them.
// Emitted only when the schema has an fp32 scalar field.
const fp32RawHelper = `// Shared 4-byte scratch word for the fp32 raw-bytes path below: widening an
// fp32's wire bytes allocates nothing per decode.
const _fp32Buf = new ArrayBuffer(4);
const _fp32Bytes = new Uint8Array(_fp32Buf);
const _fp32View = new DataView(_fp32Buf);

// _fp32FromRaw widens the four little-endian wire bytes at raw[off] to a JS
// number. A signaling NaN quiets in the widening (0x7F800001 -> 0x7FC00001),
// which is exactly why the caller keeps the bytes beside the number.
function _fp32FromRaw(raw: Uint8Array, off: number): number {
  _fp32Bytes[0] = raw[off]!;
  _fp32Bytes[1] = raw[off + 1]!;
  _fp32Bytes[2] = raw[off + 2]!;
  _fp32Bytes[3] = raw[off + 3]!;
  return _fp32View.getFloat32(0, true);
}`

// fp32ArrayRawHelper is the array half of the same channel: it renders an fp32
// array's wire payload from the value, substituting the captured wire bytes ONLY
// for an element that is still the NaN it decoded as. An element the caller has
// changed since -- and any element whose captured bytes are not themselves a NaN
// -- re-renders from its number, so a hand-set value is never overwritten by a
// stale capture. Emitted only when the schema has a native fp32 array field.
const fp32ArrayRawHelper = `// _fp32ArrayRaw renders an fp32 array's wire payload (count * 4 little-endian
// bytes) from vals, keeping the captured wire bits of every element that is
// STILL the NaN it decoded as. An element the caller has changed since
// re-renders from its number, so a hand-set value never loses to a stale
// capture: only the bits a JS number cannot carry come from ` + "`raw`" + `.
function _fp32ArrayRaw(vals: readonly number[], raw: Uint8Array): Uint8Array {
  const out = new Uint8Array(vals.length * 4);
  const odv = new DataView(out.buffer);
  const rdv = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
  for (let i = 0, o = 0; i < vals.length; i++, o += 4) {
    const v = vals[i]!;
    if (Number.isNaN(v) && o + 4 <= raw.length && Number.isNaN(rdv.getFloat32(o, true))) {
      out[o] = raw[o]!;
      out[o + 1] = raw[o + 1]!;
      out[o + 2] = raw[o + 2]!;
      out[o + 3] = raw[o + 3]!;
    } else {
      odv.setFloat32(o, v, true);
    }
  }
  return out;
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
