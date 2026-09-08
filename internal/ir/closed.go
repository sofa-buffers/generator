package ir

import "sort"

// An `enum` and a `bitfield` are CLOSED types (MESSAGE_SPEC §1). Their bound is
// not a width but the set the schema declares: for an enum the set of constant
// values, for a bitfield the mask of declared `pos` bits. A wire value outside
// that set — an enum value that is not a constant, a bitfield value carrying a
// bit no flag declares — is malformed input and MUST be reported INVALID (§7.1),
// exactly as an over-width integer is.
//
// The two facts below are what every backend needs to emit that check, and they
// are shared here for the same reason NarrowRange is: only the schema knows
// them, the corelib accumulates into a ≥64-bit carrier and hands the raw value
// over, so the check belongs in the generated store arm.
//
// Storage is a separate question and is never the bound. A target MAY hold the
// field in the smallest integer that covers the declared constants/positions
// (§1, a MAY), and a footprint target does — but a field whose declared
// positions are 0..3 does not become 0..255 valid because it is held in a byte.

// EnumValues returns the values the enum behind ref declares, sorted ascending
// and de-duplicated, and whether ref names an enum at all. The result is the
// field's complete validity bound: a value it does not contain is INVALID.
func EnumValues(ref *TypeRef) ([]int64, bool) {
	if ref == nil || ref.Target == nil || ref.Target.Category != CatEnum {
		return nil, false
	}
	vals := make([]int64, 0, len(ref.Target.Consts))
	for _, c := range ref.Target.Consts {
		vals = append(vals, c.Value)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	out := vals[:0]
	for i, x := range vals {
		if i == 0 || x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out, true
}

// EnumHull returns the smallest interval containing every declared constant.
// It is a WEAKER bound than the set — it admits the gaps — and exists only for
// the corelib hooks that can carry an interval and nothing else. A backend that
// can emit the set MUST emit the set; whatever the hull lets through has to be
// reported as unenforced, never as the rule.
//
// ok is false for a ref that is not an enum, and for an enum with no constants:
// there is no interval to state, and no value is valid.
func EnumHull(ref *TypeRef) (lo, hi int64, ok bool) {
	vals, isEnum := EnumValues(ref)
	if !isEnum || len(vals) == 0 {
		return 0, 0, false
	}
	return vals[0], vals[len(vals)-1], true
}

// EnumContiguous reports whether the declared constants run lo..hi with no gap,
// i.e. whether the hull is the set. A backend may then emit the cheaper
// two-sided comparison instead of a membership test.
func EnumContiguous(ref *TypeRef) bool {
	vals, ok := EnumValues(ref)
	if !ok || len(vals) == 0 {
		return false
	}
	return uint64(vals[len(vals)-1]-vals[0]) == uint64(len(vals)-1)
}

// BitfieldMask returns the mask of the positions the bitfield behind ref
// declares — one bit set per `pos` — and whether ref names a bitfield. A wire
// value v is valid exactly when v&^mask == 0, which admits every combination of
// declared flags including none of them, and rejects every other bit. Because
// the test runs on the raw 64-bit carrier it also rejects everything above the
// highest declared position, so no separate width term is needed.
//
// A bitfield declaring no flag has mask 0: only the zero value is valid.
func BitfieldMask(ref *TypeRef) (uint64, bool) {
	if ref == nil || ref.Target == nil || ref.Target.Category != CatBitfield {
		return 0, false
	}
	var mask uint64
	for _, fl := range ref.Target.Flags {
		if fl.Pos >= 0 && fl.Pos < 64 {
			mask |= uint64(1) << uint(fl.Pos)
		}
	}
	return mask, true
}

// BitfieldMaskIsTotal reports whether the declared positions cover all 64 bits.
// The mask check is then a tautology, and a backend elides the guard rather than
// emit dead code a linter will flag.
func BitfieldMaskIsTotal(ref *TypeRef) bool {
	mask, ok := BitfieldMask(ref)
	return ok && mask == ^uint64(0)
}
