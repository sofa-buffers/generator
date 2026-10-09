package kotlin

import (
	"fmt"
	"math"

	"github.com/sofa-buffers/generator/internal/ir"
)

// Encode-side schema bounds (ARCHITECTURE §9.6).
//
// The decoder refuses every value past its schema bound (MESSAGE_SPEC §7.1); the
// encoder is the other half. A value past its bound -- a string or blob over
// `maxlen` bytes, an array over `count` (nested rows and string/blob elements
// included), an enum or bitfield outside its declared width -- is REFUSED with
// SofabError.ARGUMENT, the corelib's category for a bad encode call
// (CORELIB_PLAN §6.3), before the field writes a byte. It is never clamped.
//
// Only the generated code knows a bound, so each guard is a per-field compare of
// a length, a count or a value against the schema literal. The one bound that is
// not a plain compare is a string's: its UTF-8 byte length is known only inside
// the corelib's measuring pass, so the bound is handed to
// `OStream.writeString(id, text, maxlen)` as an argument and judged there, in
// that same pass. The corelib holds no bound of its own.
//
// What the Kotlin type already guarantees gets no guard: every integer kind maps
// to its exact declared width (`u8` is a `UByte`, `i16` a `Short`), and an enum
// or bitfield ARRAY element is held at its declared width (primArrayType). The
// scalar enum (`Int`) and bitfield (`ULong`) are held wider than the width their
// declaration implies, so they carry the explicit check -- the same comparison
// the decode side emits for them (declaredWidthCond), on the member.

// arrBound is the bound set of one array level: its own `count` and, for a
// string/blob element, the element `maxlen`. name labels the refusal.
type arrBound struct {
	name       string
	hasCount   bool
	count      int64
	elemMaxHas bool
	elemMax    int64
}

// rowBound is the bound set of a nested array's row, read from its ArrayElem.
func rowBound(parent arrBound, items *ir.ArrayElem) arrBound {
	return arrBound{
		name:       parent.name + " row",
		hasCount:   items.HasCount,
		count:      items.Count,
		elemMaxHas: items.ElemMaxHas,
		elemMax:    items.ElemMax,
	}
}

// fitsInt says a bound is one a Kotlin `Int` size or length can exceed. A bound
// at or past Int.MAX_VALUE cannot be breached by a `size`, so it gets no guard.
func fitsInt(n int64) bool { return n < math.MaxInt32 }

// overThrow renders `if (cond) throw` with the ARGUMENT category.
func overThrow(cond, name, detail string) string {
	return fmt.Sprintf("if (%s) throw SofabException(SofabError.ARGUMENT, %s)",
		cond, ktStringLit(name+": "+detail))
}

// countGuard is the refusal of an array value holding more than its `count`
// elements; "" for an array with no count. val must be cheap to re-read (a
// member access or a local).
func countGuard(b arrBound, val string) string {
	if !b.hasCount || !fitsInt(b.count) {
		return ""
	}
	return overThrow(fmt.Sprintf("%s.size > %d", val, b.count), b.name,
		fmt.Sprintf("more than count %d elements", b.count))
}

// stringWriteCall is the write of a string value: the bounded corelib overload
// when the schema declares a `maxlen`, which refuses an over-long value in the
// pass that measures it, before any byte.
func stringWriteCall(idExpr, val string, hasMax bool, max int64) string {
	if hasMax && fitsInt(max) {
		return fmt.Sprintf("os.writeString(%s, %s, %d)", idExpr, val, max)
	}
	return fmt.Sprintf("os.writeString(%s, %s)", idExpr, val)
}

// encodeGuard is the refusal a leaf field needs in front of its write, or "":
// a blob over `maxlen`, and the scalar enum/bitfield outside its declared width.
// A string's bound rides on its write call (stringWriteCall).
func encodeGuard(fld *ir.Field, acc string) string {
	switch fld.Kind {
	case ir.KindBlob:
		if fld.HasMaxlen && fitsInt(fld.Maxlen) {
			return overThrow(fmt.Sprintf("%s.size > %d", acc, fld.Maxlen), fld.Name,
				fmt.Sprintf("longer than maxlen %d bytes", fld.Maxlen))
		}
	case ir.KindEnum:
		// Held in an `Int`: only a width narrower than 32 bits needs the check.
		lo, hi, ok := ir.EnumWidthRange(fld.Ref)
		if !ok || (lo <= math.MinInt32 && hi >= math.MaxInt32) {
			return ""
		}
		return overThrow(fmt.Sprintf("%s < %d || %s > %d", acc, lo, acc, hi), fld.Name,
			"value outside declared enum width")
	case ir.KindBitfield:
		// Held in a `ULong`: a 64-bit implied width is the type itself. The mask
		// is the decode side's comparison, on the unsigned member.
		hi, ok := ir.BitfieldWidthMax(fld.Ref)
		if !ok || hi == math.MaxUint64 {
			return ""
		}
		return overThrow(fmt.Sprintf("(%s and 0x%xUL.inv()) != 0UL", acc, hi), fld.Name,
			"value outside declared bitfield width")
	}
	return ""
}
