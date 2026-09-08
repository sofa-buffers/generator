package ir

import "testing"

func enumRef(vals ...int64) *TypeRef {
	nt := &NamedType{Category: CatEnum, Name: "E", Key: "enum/E"}
	for _, v := range vals {
		nt.Consts = append(nt.Consts, &EnumConst{Name: "C", Value: v})
	}
	return &TypeRef{Key: nt.Key, Target: nt}
}

func bitRef(pos ...int64) *TypeRef {
	nt := &NamedType{Category: CatBitfield, Name: "F", Key: "bitfield/F"}
	for _, p := range pos {
		nt.Flags = append(nt.Flags, &BitfieldFlag{Name: "A", Pos: p})
	}
	return &TypeRef{Key: nt.Key, Target: nt}
}

// The declared set is the bound, so it has to come back sorted, de-duplicated
// and complete — a backend renders it straight into a match/switch.
func TestEnumValuesSortsAndDedups(t *testing.T) {
	got, ok := EnumValues(enumRef(10, 0, 2, 1, 2))
	if !ok {
		t.Fatalf("an enum ref must answer")
	}
	want := []int64{0, 1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if _, ok := EnumValues(bitRef(0)); ok {
		t.Errorf("a bitfield ref must not answer as an enum")
	}
	if _, ok := EnumValues(nil); ok {
		t.Errorf("a nil ref must not answer")
	}
}

// The hull is the WEAKER bound the interval-only corelib hooks can carry, and
// the point of EnumContiguous is to say when it is weaker at all.
func TestEnumHullAndContiguity(t *testing.T) {
	lo, hi, ok := EnumHull(enumRef(-100, 2, 33))
	if !ok || lo != -100 || hi != 33 {
		t.Fatalf("hull = %d..%d ok=%v, want -100..33", lo, hi, ok)
	}
	if EnumContiguous(enumRef(0, 1, 2, 10)) {
		t.Errorf("{0,1,2,10} has a gap: 5 is inside the hull and not a constant")
	}
	if !EnumContiguous(enumRef(-2, -1, 0, 1)) {
		t.Errorf("-2..1 runs with no gap")
	}
	if !EnumContiguous(enumRef(7)) {
		t.Errorf("a single constant is its own hull")
	}
	if _, _, ok := EnumHull(enumRef()); ok {
		t.Errorf("an enum with no constant has no interval to state")
	}
}

// The mask is one bit per declared pos, and explicitly NOT every bit up to the
// highest one: positions 0, 1 and 3 give 0b1011, so bit 2 is undeclared and a
// wire value of 4 is INVALID (MESSAGE_SPEC §1).
func TestBitfieldMaskIsTheDeclaredPositions(t *testing.T) {
	mask, ok := BitfieldMask(bitRef(0, 1, 3))
	if !ok || mask != 0xb {
		t.Fatalf("mask = %#x ok=%v, want 0xb", mask, ok)
	}
	if mask&4 != 0 {
		t.Errorf("bit 2 is undeclared and must not be in the mask")
	}
	if m, _ := BitfieldMask(bitRef(0, 63)); m != 0x8000000000000001 {
		t.Errorf("mask = %#x, want 0x8000000000000001", m)
	}
	// No flag at all: only the zero value is valid.
	if m, ok := BitfieldMask(bitRef()); !ok || m != 0 {
		t.Errorf("mask = %#x ok=%v, want 0", m, ok)
	}
	if _, ok := BitfieldMask(enumRef(0)); ok {
		t.Errorf("an enum ref must not answer as a bitfield")
	}
}

// A bitfield declaring all 64 positions admits every value, so the mask test is
// a tautology and a backend elides the guard rather than emit dead code.
func TestBitfieldMaskIsTotal(t *testing.T) {
	var all []int64
	for i := int64(0); i < 64; i++ {
		all = append(all, i)
	}
	if !BitfieldMaskIsTotal(bitRef(all...)) {
		t.Errorf("64 declared positions cover the whole carrier")
	}
	if BitfieldMaskIsTotal(bitRef(0, 63)) {
		t.Errorf("pos 0 and 63 leave 62 bits undeclared; the guard is live")
	}
}
