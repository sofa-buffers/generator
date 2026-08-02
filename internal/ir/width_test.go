package ir

import "testing"

func TestNarrowRange(t *testing.T) {
	cases := []struct {
		k      Kind
		lo, hi int64
		ok     bool
	}{
		{KindU8, 0, 255, true},
		{KindU16, 0, 65535, true},
		{KindU32, 0, 4294967295, true},
		{KindI8, -128, 127, true},
		{KindI16, -32768, 32767, true},
		{KindI32, -2147483648, 2147483647, true},
		// The 64-bit kinds span their own delivery accumulator, so no value can
		// breach them and backends must emit no guard.
		{KindU64, 0, 0, false},
		{KindI64, 0, 0, false},
		// Non-integer kinds are not width-bounded at all.
		{KindFP32, 0, 0, false},
		{KindFP64, 0, 0, false},
		{KindBool, 0, 0, false},
		{KindString, 0, 0, false},
		{KindBlob, 0, 0, false},
		{KindArray, 0, 0, false},
		{KindEnum, 0, 0, false},
		{KindBitfield, 0, 0, false},
		{KindStruct, 0, 0, false},
		{KindUnion, 0, 0, false},
		{KindInvalid, 0, 0, false},
	}
	for _, c := range cases {
		lo, hi, ok := NarrowRange(c.k)
		if lo != c.lo || hi != c.hi || ok != c.ok {
			t.Errorf("NarrowRange(%v) = (%d, %d, %v), want (%d, %d, %v)",
				c.k, lo, hi, ok, c.lo, c.hi, c.ok)
		}
		if got := IsNarrow(c.k); got != c.ok {
			t.Errorf("IsNarrow(%v) = %v, want %v", c.k, got, c.ok)
		}
	}
}

// The reproducers from the issue: a u8 destination must reject 256 and 16383 but
// keep 255, and a u16 destination must reject 70000.
func TestNarrowRangeReproducers(t *testing.T) {
	_, u8hi, _ := NarrowRange(KindU8)
	for _, v := range []int64{256, 16383} {
		if v <= u8hi {
			t.Errorf("u8 must reject %d", v)
		}
	}
	if 255 > u8hi {
		t.Errorf("u8 must accept the in-range control 255")
	}
	_, u16hi, _ := NarrowRange(KindU16)
	if 70000 <= u16hi {
		t.Errorf("u16 must reject 70000")
	}
}
