package ir

import "testing"

func TestBounds(t *testing.T) {
	nested := &NamedType{Name: "Inner", Fields: []*Field{
		{Name: "s", Kind: KindString, HasMaxlen: true, Maxlen: 32},
		{Name: "b", Kind: KindBlob}, // dynamic blob
	}}
	fields := []*Field{
		{Name: "u", Kind: KindU32},
		{Name: "s", Kind: KindString, HasMaxlen: true, Maxlen: 50},
		{Name: "dynArr", Kind: KindArray, Elem: KindU64}, // no count
		{Name: "arr", Kind: KindArray, Elem: KindI32, HasCount: true, Count: 5},
		{Name: "strArr", Kind: KindArray, Elem: KindString, HasCount: true, Count: 3, ElemMaxHas: true, ElemMax: 64},
		{Name: "obj", Kind: KindStruct, Ref: &TypeRef{Key: "struct/Inner", Target: nested}},
	}
	b := Bounds(fields)
	if !b.HasDynArray || b.HasDynString || !b.HasDynBlob {
		t.Fatalf("dyn flags wrong: %+v", b)
	}
	if !b.HasDyn() {
		t.Fatal("HasDyn should be true")
	}
	if b.MaxCount != 5 || b.MaxStringLen != 64 || b.MaxBlobLen != 0 {
		t.Fatalf("maxima wrong: %+v", b)
	}

	all := Bounds([]*Field{{Name: "s", Kind: KindString, HasMaxlen: true, Maxlen: 8}})
	if all.HasDyn() {
		t.Fatalf("fully bounded fields must report no dyn: %+v", all)
	}
}
