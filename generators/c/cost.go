package c

import "github.com/sofa-buffers/generator/internal/ir"

// maxSize computes the worst-case serialized byte length of a message
// analytically from the IR (PLAN §5.5) — a language-independent upper bound,
// never by running the corelib. Returns (size, bounded); bounded is false if a
// reachable string/blob lacks maxlen (unbounded ⇒ streaming path).
func (g *gen) maxSize(fields []*ir.Field) (int64, bool) {
	var total int64
	seen := map[string]bool{}
	for _, f := range fields {
		c, ok := g.fieldCost(f, seen)
		if !ok {
			return 0, false
		}
		total += c
	}
	return total, true
}

// fieldCost bounds one field: header + worst-case payload.
func (g *gen) fieldCost(f *ir.Field, seen map[string]bool) (int64, bool) {
	hdr := varintLen(uint64(f.ID)<<3 | 7) // (id<<3)|wiretype, wiretype<=7
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return hdr + scalarVarintMax(f), true
	case ir.KindFP32:
		return hdr + 1 + 4, true // fixlen subheader + payload
	case ir.KindFP64:
		return hdr + 1 + 8, true
	case ir.KindString, ir.KindBlob:
		if !f.HasMaxlen {
			return 0, false
		}
		return hdr + varintLen(uint64(f.Maxlen)<<3) + f.Maxlen, true
	case ir.KindArray:
		switch f.Elem {
		case ir.KindString, ir.KindBlob:
			if !f.ElemMaxHas {
				return 0, false
			}
			// sequence framing + count * (string field cost) + seq_end
			elemHdr := varintLen(uint64(f.Count)<<3 | 7)
			per := elemHdr + varintLen(uint64(f.ElemMax)<<3) + f.ElemMax
			return hdr + 1 + f.Count*per + 1, true
		default:
			elem := arrayElemMax(f.Elem)
			return hdr + varintLen(uint64(f.Count)) + f.Count*elem, true
		}
	case ir.KindStruct, ir.KindUnion:
		// sequence_begin + sum(children) + sequence_end. Recurse; cap recursion
		// on cycles (a recursive struct is unbounded for a static max).
		if seen[f.Ref.Key] {
			return 0, false
		}
		seen[f.Ref.Key] = true
		var inner int64
		for _, c := range f.Ref.Target.Fields {
			cc, ok := g.fieldCost(c, seen)
			if !ok {
				delete(seen, f.Ref.Key)
				return 0, false
			}
			inner += cc
		}
		delete(seen, f.Ref.Key)
		return hdr + inner + 1, true
	}
	return hdr, true
}

func scalarVarintMax(f *ir.Field) int64 {
	switch f.Kind {
	case ir.KindBool:
		return 1
	case ir.KindU8, ir.KindI8:
		return 2
	case ir.KindU16, ir.KindI16:
		return 3
	case ir.KindU32, ir.KindI32, ir.KindEnum:
		return 5
	default: // u64/i64/bitfield
		return 10
	}
}

func arrayElemMax(k ir.Kind) int64 {
	switch k {
	case ir.KindU8, ir.KindI8:
		return 2
	case ir.KindU16, ir.KindI16:
		return 3
	case ir.KindU32, ir.KindI32:
		return 5
	case ir.KindFP32:
		return 4
	case ir.KindFP64:
		return 8
	default:
		return 10
	}
}

// varintLen returns the number of bytes in the LEB128 encoding of x.
func varintLen(x uint64) int64 {
	n := int64(1)
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}
