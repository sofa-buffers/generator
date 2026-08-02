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
// Enum and bitfield kinds are deliberately not covered. Their backing width is a
// property of the named type rather than of the field, and an out-of-range enum
// value is a different question (unknown-variant handling) from an over-width
// integer; both stay with the existing per-backend treatment.
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
