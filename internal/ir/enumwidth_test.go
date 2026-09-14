package ir

import "testing"

// The width an enum declares is the smallest SIGNED type holding every constant
// (MESSAGE_SPEC §1). The examples are the spec's own: {RED=1, GREEN=2, BLUE=3}
// is bounded as an i8, {1, 2, 300} as an i16.
func TestEnumWidthRange(t *testing.T) {
	cases := []struct {
		name   string
		ref    *TypeRef
		lo, hi int64
		ok     bool
	}{
		{"spec example {1,2,3} is i8", enumRef(1, 2, 3), -128, 127, true},
		{"spec example {1,2,300} is i16", enumRef(1, 2, 300), -32768, 32767, true},
		{"a gap does not widen", enumRef(0, 1, 2, 10), -128, 127, true},
		{"negative constants stay signed", enumRef(-5, 1), -128, 127, true},
		{"i8 lower edge", enumRef(-128, 127), -128, 127, true},
		{"one past the i8 edge widens", enumRef(-129), -32768, 32767, true},
		{"one past the i16 edge widens", enumRef(32768), -2147483648, 2147483647, true},
		{"i32 edge still bounds", enumRef(2147483647), -2147483648, 2147483647, true},
		// Past i32 the width is i64, which IS the accumulator the value arrives
		// in, so nothing reachable can breach it and a backend emits no guard.
		{"beyond i32 is the accumulator itself", enumRef(2147483648), 0, 0, false},
		// An enum declaring nothing implies the narrowest signed width; there is
		// no constant to widen it.
		{"no constant implies i8", enumRef(), -128, 127, true},
		{"a bitfield is not an enum", bitRef(0, 1), 0, 0, false},
		{"nil is not an enum", nil, 0, 0, false},
	}
	for _, c := range cases {
		lo, hi, ok := EnumWidthRange(c.ref)
		if lo != c.lo || hi != c.hi || ok != c.ok {
			t.Errorf("%s: EnumWidthRange = (%d, %d, %v), want (%d, %d, %v)",
				c.name, lo, hi, ok, c.lo, c.hi, c.ok)
		}
	}
}

// A bitfield's width is the smallest UNSIGNED type holding its highest declared
// `pos` — NOT the mask its flags form. Positions 0, 1 and 3 give 0..255, so the
// undeclared bit 2 (value 4) is inside the bound and valid.
func TestBitfieldWidthMax(t *testing.T) {
	cases := []struct {
		name string
		ref  *TypeRef
		hi   uint64
		ok   bool
	}{
		{"spec example pos{0,1,3} is u8", bitRef(0, 1, 3), 0xFF, true},
		{"the highest pos decides, not the count", bitRef(7), 0xFF, true},
		{"pos 8 widens to u16", bitRef(0, 8), 0xFFFF, true},
		{"pos 15 is still u16", bitRef(15), 0xFFFF, true},
		{"pos 16 widens to u32", bitRef(16), 0xFFFFFFFF, true},
		{"pos 31 is still u32", bitRef(31), 0xFFFFFFFF, true},
		// u64 IS the accumulator, so no guard is emitted for it.
		{"pos 32 reaches the accumulator", bitRef(32), 0, false},
		{"pos 63 reaches the accumulator", bitRef(63), 0, false},
		{"no flag implies u8", bitRef(), 0xFF, true},
		{"an enum is not a bitfield", enumRef(1, 2), 0, false},
		{"nil is not a bitfield", nil, 0, false},
	}
	for _, c := range cases {
		hi, ok := BitfieldWidthMax(c.ref)
		if hi != c.hi || ok != c.ok {
			t.Errorf("%s: BitfieldWidthMax = (%#x, %v), want (%#x, %v)", c.name, hi, ok, c.hi, c.ok)
		}
	}
}

// The values the OLD closed-set rule rejected and the new width rule admits —
// the behavioural difference PR #95 made, pinned so a regression is loud.
func TestWidthAdmitsWhatTheSetRejected(t *testing.T) {
	lo, hi, ok := EnumWidthRange(enumRef(0, 1, 2, 10))
	if !ok || 5 < lo || 5 > hi {
		t.Errorf("enum {0,1,2,10}: 5 must be INSIDE the declared width, got [%d, %d] ok=%v", lo, hi, ok)
	}
	max, ok := BitfieldWidthMax(bitRef(0, 1, 3))
	if !ok || 4 > max {
		t.Errorf("bitfield pos{0,1,3}: 4 must be INSIDE the declared width, got max=%d ok=%v", max, ok)
	}
	// ...and what stays out: one past the width is still INVALID.
	if _, hi, _ := EnumWidthRange(enumRef(1, 2, 3)); hi >= 200 {
		t.Errorf("enum {1,2,3}: 200 must be OUTSIDE the declared width, got hi=%d", hi)
	}
	if max, _ := BitfieldWidthMax(bitRef(0, 1, 3)); max >= 256 {
		t.Errorf("bitfield pos{0,1,3}: 256 must be OUTSIDE the declared width, got max=%d", max)
	}
}
