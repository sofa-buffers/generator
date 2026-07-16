package ir

import "sort"

// AlignRank returns a field's in-memory storage alignment in bytes (8/4/2/1).
// Backends use it to order struct members widest-first so the native compiler
// inserts less padding between fields. Variable-length and composite members
// (string, blob, array, struct, union) are treated as 8 — they are heap
// pointers / references in most targets, and where they are inlined (C arrays)
// over-ranking a low-alignment member is still padding-safe (it only moves it
// earlier, never introducing padding).
//
// Reordering member *declarations* never changes the wire bytes: encode iterates
// the schema field order and decode is keyed by field id, both independent of
// declaration order.
func AlignRank(f *Field) int {
	switch f.Kind {
	case KindU8, KindI8, KindBool:
		return 1
	case KindU16, KindI16:
		return 2
	case KindU32, KindI32, KindFP32:
		return 4
	case KindU64, KindI64, KindFP64:
		return 8
	case KindString, KindBlob, KindArray, KindStruct, KindUnion, KindMap:
		return 8
	case KindEnum:
		return enumAlign(f.Ref.Target)
	case KindBitfield:
		return bitfieldAlign(f.Ref.Target)
	}
	return 8
}

// enumAlign mirrors the per-backend enum backing-width choice (signed, smallest
// type that holds the constant range).
func enumAlign(nt *NamedType) int {
	var lo, hi int64
	for _, c := range nt.Consts {
		if c.Value < lo {
			lo = c.Value
		}
		if c.Value > hi {
			hi = c.Value
		}
	}
	switch {
	case lo >= -128 && hi <= 127:
		return 1
	case lo >= -32768 && hi <= 32767:
		return 2
	default:
		return 4
	}
}

// bitfieldAlign mirrors the per-backend bitfield backing-width choice (unsigned,
// smallest type that holds the highest flag position).
func bitfieldAlign(nt *NamedType) int {
	var max int64
	for _, fl := range nt.Flags {
		if fl.Pos > max {
			max = fl.Pos
		}
	}
	switch {
	case max <= 7:
		return 1
	case max <= 15:
		return 2
	case max <= 31:
		return 4
	default:
		return 8
	}
}

// SortedForLayout returns a copy of fields ordered by AlignRank descending
// (widest first), stable so fields of equal alignment keep their schema order.
// Use it ONLY for member-declaration emission, never for encode/descriptor order.
func SortedForLayout(fields []*Field) []*Field {
	out := make([]*Field, len(fields))
	copy(out, fields)
	sort.SliceStable(out, func(i, j int) bool {
		return AlignRank(out[i]) > AlignRank(out[j])
	})
	return out
}
