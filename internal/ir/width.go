package ir

// The declared integer width is a normative validity bound, not a storage hint
// (MESSAGE_SPEC §1/§7.1, documentation#32). A wire value outside the range of
// the width the schema declares is a schema-bound INVALID, in the same class as
// `M > N` and `maxlen`: it MUST NOT be masked to the width and MUST NOT be kept.
//
// The bound cannot live in the corelib. CORELIB_PLAN §4.1 accumulates every
// integer into a ≥64-bit accumulator and hands that over; only the schema knows
// the destination was declared `u8`. Per MESSAGE_SPEC §7 — "the corelib cannot
// know the schema, so schema-bound violations are detected, and reported, by
// generated code" — the check belongs in the emitted store arm, beside the
// `count`, wrapper-element-id and `maxlen` guards the backends already carry.
//
// NarrowRange returns the inclusive range a declared integer Kind may carry and
// whether that range is narrower than the accumulator the value arrives in. The
// 64-bit kinds return ok == false: their range IS the delivery type's, so no
// reachable value can breach it and a backend must emit no guard for them.
//
// Enum and bitfield kinds are deliberately not covered, because a width is the
// wrong question for them: MESSAGE_SPEC §1 binds both to the SET the schema
// declares, not to the range of whatever integer a target stores them in. Their
// bound lives in closed.go — EnumValues and BitfieldMask — and a backend emits
// that check in the same store arm, ahead of the same narrowing cast.
func NarrowRange(k Kind) (lo, hi int64, ok bool) {
	switch k {
	case KindU8:
		return 0, 255, true
	case KindU16:
		return 0, 65535, true
	case KindU32:
		return 0, 4294967295, true
	case KindI8:
		return -128, 127, true
	case KindI16:
		return -32768, 32767, true
	case KindI32:
		return -2147483648, 2147483647, true
	}
	return 0, 0, false
}

// IsNarrow reports whether Kind k carries a declared width narrower than the
// accumulator it is delivered in — i.e. whether a store of k needs the §7.1
// over-width guard at all.
func IsNarrow(k Kind) bool {
	_, _, ok := NarrowRange(k)
	return ok
}

// An `enum` and a `bitfield` are bound by the WIDTH their declaration implies
// (MESSAGE_SPEC §1, documentation PR #95, doc `382159e`) — for an enum the
// smallest SIGNED type that holds every declared constant, for a bitfield the
// smallest UNSIGNED type that holds its highest declared `pos`. A wire value
// inside that width is valid even when the schema names no constant for it and
// even when it carries an undeclared bit; a value outside it is malformed input
// and MUST be reported INVALID (§7.1), exactly as an over-width `i8` is.
//
// This REPLACES the set/mask bound of closed.go, which implemented the reading
// MESSAGE_SPEC carried for six days (PR #89, doc `a50db95`) and PR #95 withdrew.
// Both bounds are now ordinary intervals, which is what §1 gives as the reason
// for the change: an array's elements are consumed inside the corelib loop, so a
// bound must cross that channel as an interval, and a width fits where a set
// does not.
//
// ok is false where no reachable value can breach the bound — an enum needing
// the full i64 and a bitfield needing the full u64 — because the value arrives
// in a 64-bit accumulator (CORELIB_PLAN §4.1) and the guard would be dead code.

// EnumWidthRange returns the inclusive range of the smallest signed integer type
// holding every constant the enum behind ref declares, and whether that range is
// narrower than the 64-bit accumulator the value is delivered in.
//
// The width is SIGNED whatever the constants are, because the wire type is
// (CORELIB_PLAN §4.5): deriving it from their sign would make an enum's wire type
// a function of its constants, so adding a negative constant later would change
// the encoding rather than only the range.
//
// An enum declaring no constant at all yields the narrowest signed width: there
// is no constant to widen it, and i8 is what the declaration implies.
func EnumWidthRange(ref *TypeRef) (lo, hi int64, ok bool) {
	vals, isEnum := EnumValues(ref)
	if !isEnum {
		return 0, 0, false
	}
	var min, max int64
	if len(vals) > 0 {
		min, max = vals[0], vals[len(vals)-1] // EnumValues sorts ascending
	}
	for _, k := range []Kind{KindI8, KindI16, KindI32} {
		wlo, whi, _ := NarrowRange(k)
		if min >= wlo && max <= whi {
			return wlo, whi, true
		}
	}
	return 0, 0, false // i64: the accumulator's own range
}

// BitfieldWidthMax returns the maximum value of the smallest unsigned integer
// type holding the highest `pos` the bitfield behind ref declares, and whether
// that width is narrower than the 64-bit accumulator.
//
// A bitfield declaring positions 0, 1 and 3 is bounded as a u8: 0..255. Every
// value inside that range is valid whichever bits it carries, undeclared ones
// included — masking them away would turn malformed-looking input into a
// DECLARED combination and report it Ok (§1). A bitfield declaring no flag
// yields u8 by the same reasoning EnumWidthRange gives for an enum with no
// constant.
func BitfieldWidthMax(ref *TypeRef) (hi uint64, ok bool) {
	if ref == nil || ref.Target == nil || ref.Target.Category != CatBitfield {
		return 0, false
	}
	maxPos := int64(-1)
	for _, fl := range ref.Target.Flags {
		if fl.Pos > maxPos {
			maxPos = fl.Pos
		}
	}
	switch {
	case maxPos < 8:
		return 0xFF, true
	case maxPos < 16:
		return 0xFFFF, true
	case maxPos < 32:
		return 0xFFFFFFFF, true
	}
	return 0, false // u64: the accumulator's own range
}
