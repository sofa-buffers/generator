package ir

import "testing"

// The fp32/fp64 array cases below are not hand-derived: they are the byte
// counts corelib-c-cpp actually emits for those arrays, captured from the
// encoder --
//
//	fp32[2] -> 05 02 20 00 00 80 3f 00 00 00 40                   (11 bytes)
//	fp64[2] -> 05 02 41 00 00 .. 3f 00 00 .. 40                   (19 bytes)
//	u32[2]  -> 03 02 01 02                                         (4 bytes)
//
// -- so the walk is checked against the wire, not against itself. The `20` /
// `41` byte is the per-array fixlen word that a fixlen array carries even when
// empty; an earlier per-backend model omitted it and under-counted by one.
func TestMaxFieldWireSize(t *testing.T) {
	tests := []struct {
		name  string
		field *Field
		want  int64
		ok    bool
	}{
		{"bool", &Field{ID: 0, Kind: KindBool}, 1 + 1, true},
		{"u8", &Field{ID: 0, Kind: KindU8}, 1 + 2, true},
		{"u16", &Field{ID: 0, Kind: KindU16}, 1 + 3, true},
		{"u32", &Field{ID: 0, Kind: KindU32}, 1 + 5, true},
		{"u64", &Field{ID: 0, Kind: KindU64}, 1 + 10, true},
		// ZigZag maps a signed value onto the same width as its unsigned peer.
		{"i32 costs the same as u32", &Field{ID: 0, Kind: KindI32}, 1 + 5, true},
		{"i64 costs the same as u64", &Field{ID: 0, Kind: KindI64}, 1 + 10, true},
		{"enum is a 32-bit value", &Field{ID: 0, Kind: KindEnum}, 1 + 5, true},
		{"bitfield is a 64-bit value", &Field{ID: 0, Kind: KindBitfield}, 1 + 10, true},
		{"fp32", &Field{ID: 0, Kind: KindFP32}, 1 + 1 + 4, true},
		{"fp64", &Field{ID: 0, Kind: KindFP64}, 1 + 1 + 8, true},

		// A wide id widens the header varint: (16<<3)|7 = 135 needs two bytes.
		{"two-byte header", &Field{ID: 16, Kind: KindU8}, 2 + 2, true},

		{"string with maxlen", &Field{ID: 3, Kind: KindString, HasMaxlen: true, Maxlen: 8}, 1 + 1 + 8, true},
		{"blob with maxlen", &Field{ID: 4, Kind: KindBlob, HasMaxlen: true, Maxlen: 8}, 1 + 1 + 8, true},
		{"string without maxlen is unbounded", &Field{ID: 3, Kind: KindString}, 0, false},
		{"blob without maxlen is unbounded", &Field{ID: 4, Kind: KindBlob}, 0, false},

		{
			"u32 array with count",
			&Field{ID: 5, Kind: KindArray, Elem: KindU32, HasCount: true, Count: 3},
			1 + 1 + 3*5, true,
		},
		{
			// The bug this walk exists to make unrepresentable: an array with no
			// count has no worst case, whatever its element type.
			"u32 array without count is unbounded",
			&Field{ID: 5, Kind: KindArray, Elem: KindU32},
			0, false,
		},
		{
			"u32[2] matches the emitted 4 bytes",
			&Field{ID: 0, Kind: KindArray, Elem: KindU32, HasCount: true, Count: 2},
			1 + 1 + 2*5, true, // worst case; the dump shows 4 for values 1,2
		},
		{
			"fp32[2] matches the emitted 11 bytes",
			&Field{ID: 0, Kind: KindArray, Elem: KindFP32, HasCount: true, Count: 2},
			11, true,
		},
		{
			"fp64[2] matches the emitted 19 bytes",
			&Field{ID: 0, Kind: KindArray, Elem: KindFP64, HasCount: true, Count: 2},
			19, true,
		},
		{
			"string array without element maxlen is unbounded",
			&Field{ID: 7, Kind: KindArray, Elem: KindString, HasCount: true, Count: 2},
			0, false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MaxFieldWireSize(tt.field, nil)
			if ok != tt.ok {
				t.Fatalf("bounded = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("size = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMaxWireSizeUnboundedPoisonsTheWholeMessage: one unbounded field makes the
// message unbounded even when every other field is tightly bound — the size of
// the bounded remainder is not a partial answer, it is no answer.
func TestMaxWireSizeUnboundedPoisonsTheWholeMessage(t *testing.T) {
	fields := []*Field{
		{Name: "a", ID: 0, Kind: KindU32},
		{Name: "b", ID: 1, Kind: KindArray, Elem: KindU32}, // no count
	}
	if _, ok := MaxWireSize(fields); ok {
		t.Fatal("message with an unbounded array reported as bounded")
	}
	fields[1].HasCount, fields[1].Count = true, 4
	got, ok := MaxWireSize(fields)
	if !ok {
		t.Fatal("message reported unbounded after the array was given a count")
	}
	if want := int64(6 + 1 + 1 + 4*5); got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
}

// A struct is a sequence: its field header opens it, a one-byte terminator
// closes it, and its members are charged inside.
func TestMaxWireSizeStruct(t *testing.T) {
	inner := &NamedType{Category: CatStruct, Name: "Inner", Key: "struct/Inner",
		Fields: []*Field{{Name: "x", ID: 0, Kind: KindI32}}}
	f := &Field{Name: "inner", ID: 8, Kind: KindStruct,
		Ref: &TypeRef{Key: inner.Key, Target: inner}}

	got, ok := MaxFieldWireSize(f, nil)
	if !ok {
		t.Fatal("struct reported unbounded")
	}
	if want := int64(1 + (1 + 5) + 1); got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
}

// A struct that reaches itself has no static worst case.
func TestMaxWireSizeRecursiveStructIsUnbounded(t *testing.T) {
	node := &NamedType{Category: CatStruct, Name: "Node", Key: "struct/Node"}
	node.Fields = []*Field{
		{Name: "v", ID: 0, Kind: KindU32},
		{Name: "next", ID: 1, Kind: KindStruct, Ref: &TypeRef{Key: node.Key, Target: node}},
	}
	f := &Field{Name: "root", ID: 0, Kind: KindStruct, Ref: &TypeRef{Key: node.Key, Target: node}}
	if _, ok := MaxFieldWireSize(f, nil); ok {
		t.Fatal("recursive struct reported as bounded")
	}
}
