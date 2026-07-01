package cpp

import "github.com/sofa-buffers/generator/internal/ir"

// maxSize analytically bounds the worst-case serialized length (PLAN §5.5),
// used for OStreamInline<_maxSize>. Returns (size, bounded).
func (g *gen) maxSize(fields []*ir.Field) (int64, bool) {
	var total int64
	seen := map[string]bool{}
	for _, f := range fields {
		c, ok := g.fieldCost(f, seen)
		if !ok {
			return 4096, true // fallback bound for unbounded fields
		}
		total += c
	}
	if total < 64 {
		total = 64
	}
	return total, true
}

func (g *gen) fieldCost(f *ir.Field, seen map[string]bool) (int64, bool) {
	hdr := varintLen(uint64(f.ID)<<3 | 7)
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return hdr + 10, true
	case ir.KindFP32:
		return hdr + 1 + 4, true
	case ir.KindFP64:
		return hdr + 1 + 8, true
	case ir.KindString, ir.KindBlob:
		if !f.HasMaxlen {
			return 0, false
		}
		return hdr + varintLen(uint64(f.Maxlen)<<3) + f.Maxlen, true
	case ir.KindArray:
		body, ok := g.arrayCost(f.Elem, f.ElemRef, f.ElemItems, f.Count, f.ElemMaxHas, f.ElemMax, seen)
		if !ok {
			return 0, false
		}
		return hdr + body, true
	case ir.KindStruct, ir.KindUnion:
		inner, ok := g.structInner(f.Ref, seen)
		if !ok {
			return 0, false
		}
		return hdr + inner + 1, true
	}
	return hdr, true
}

// structInner sums the worst-case cost of a struct/union's fields (a union is
// bounded by the sum of all options, a safe over-estimate). Guards recursion via
// the shared seen set.
func (g *gen) structInner(ref *ir.TypeRef, seen map[string]bool) (int64, bool) {
	if seen[ref.Key] {
		return 0, false
	}
	seen[ref.Key] = true
	var inner int64
	for _, c := range ref.Target.Fields {
		cc, ok := g.fieldCost(c, seen)
		if !ok {
			delete(seen, ref.Key)
			return 0, false
		}
		inner += cc
	}
	delete(seen, ref.Key)
	return inner, true
}

// arrayCost bounds an array's payload (excluding the field's own header, which
// the caller adds). A dynamic array (no fixed count) is unbounded. Numeric/enum/
// boolean/bitfield elements use the native count-prefixed array; string/blob/
// struct/union/nested-array elements lower to a wrapper sequence framed by a
// per-index header plus a one-byte sequence end.
func (g *gen) arrayCost(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool, elemMax int64, seen map[string]bool) (int64, bool) {
	if count <= 0 {
		return 0, false // dynamic length -> unbounded
	}
	idHdr := varintLen(uint64(count)<<3 | 7)
	switch elem {
	case ir.KindString, ir.KindBlob:
		if !elemMaxHas {
			return 0, false
		}
		per := idHdr + varintLen(uint64(elemMax)<<3) + elemMax
		return 1 + count*per + 1, true
	case ir.KindStruct, ir.KindUnion:
		inner, ok := g.structInner(ref, seen)
		if !ok {
			return 0, false
		}
		return count*(idHdr+inner+1) + 1, true
	case ir.KindArray:
		ic, ok := g.arrayCost(items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas, items.ElemMax, seen)
		if !ok {
			return 0, false
		}
		return count*(idHdr+ic) + 1, true
	default: // numeric / enum / boolean / bitfield -> native array
		return varintLen(uint64(count)) + count*10, true
	}
}

func varintLen(x uint64) int64 {
	n := int64(1)
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}
