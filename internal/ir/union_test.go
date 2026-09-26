package ir

import "testing"

func TestUnionDefaultOption(t *testing.T) {
	a := &Field{Name: "a", ID: 3}
	b := &Field{Name: "b", ID: 7}
	id := int64(7)
	u := &NamedType{Category: CatUnion, Fields: []*Field{a, b}, DefaultID: &id}

	if got := u.DefaultOption(); got != b {
		t.Fatalf("DefaultOption = %v, want option b", got)
	}
	if !u.IsDefaultOption(b) || u.IsDefaultOption(a) || u.IsDefaultOption(nil) {
		t.Error("IsDefaultOption must hold for b only")
	}

	// A split variant shares the original's Fields slice: its default is its own.
	lo := int64(3)
	v := *u
	v.DefaultID = &lo
	if !v.IsDefaultOption(a) || v.IsDefaultOption(b) {
		t.Error("a variant with DefaultID 3 must report option a")
	}

	for name, nt := range map[string]*NamedType{
		"unbound union": {Category: CatUnion, Fields: []*Field{a}},
		"struct":        {Category: CatStruct, Fields: []*Field{a}, DefaultID: &lo},
		"nil":           nil,
	} {
		if nt.DefaultOption() != nil || nt.IsDefaultOption(a) {
			t.Errorf("%s: want no default option", name)
		}
	}
}
