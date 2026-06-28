package rust

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

func exported(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	if b.Len() == 0 {
		return "X"
	}
	return b.String()
}

func (g *gen) typeName(key string) string {
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

func (g *gen) rustType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return numRustType(f.Kind)
	case ir.KindFP32:
		return "f32"
	case ir.KindFP64:
		return "f64"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "Vec<u8>"
	case ir.KindEnum:
		return enumBacking(f.Ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(f.Ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		switch f.Elem {
		case ir.KindString:
			return "Vec<String>"
		case ir.KindBlob:
			return "Vec<Vec<u8>>"
		default:
			return "Vec<" + numRustType(f.Elem) + ">"
		}
	}
	return "()"
}

func numRustType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "u8"
	case ir.KindU16:
		return "u16"
	case ir.KindU32:
		return "u32"
	case ir.KindU64:
		return "u64"
	case ir.KindI8:
		return "i8"
	case ir.KindI16:
		return "i16"
	case ir.KindI32:
		return "i32"
	case ir.KindI64:
		return "i64"
	case ir.KindFP32:
		return "f32"
	case ir.KindFP64:
		return "f64"
	}
	return "u8"
}

func enumBacking(nt *ir.NamedType) string {
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
		return "i8"
	case lo >= -32768 && hi <= 32767:
		return "i16"
	default:
		return "i32"
	}
}

func bitfieldBacking(nt *ir.NamedType) string {
	var max int64
	for _, fl := range nt.Flags {
		if fl.Pos > max {
			max = fl.Pos
		}
	}
	switch {
	case max <= 7:
		return "u8"
	case max <= 15:
		return "u16"
	case max <= 31:
		return "u32"
	default:
		return "u64"
	}
}

// capabilities returns the sofab features the schema needs, for require!() and
// the generated Cargo.toml.
func (g *gen) capabilities(s *ir.Schema) []string {
	caps := map[string]bool{}
	var walk func(fields []*ir.Field)
	seen := map[string]bool{}
	walk = func(fields []*ir.Field) {
		for _, f := range fields {
			switch f.Kind {
			case ir.KindString, ir.KindBlob, ir.KindFP32:
				caps["fixlen"] = true
			case ir.KindFP64:
				caps["fixlen"] = true
				caps["fp64"] = true
			case ir.KindU64, ir.KindI64:
				caps["value64"] = true
			case ir.KindStruct, ir.KindUnion:
				caps["sequence"] = true
				if !seen[f.Ref.Key] {
					seen[f.Ref.Key] = true
					walk(f.Ref.Target.Fields)
				}
			case ir.KindArray:
				switch f.Elem {
				case ir.KindString, ir.KindBlob:
					caps["sequence"] = true
					caps["fixlen"] = true
				case ir.KindFP64:
					caps["array"] = true
					caps["fixlen"] = true
					caps["fp64"] = true
				case ir.KindFP32:
					caps["array"] = true
					caps["fixlen"] = true
				case ir.KindU64, ir.KindI64:
					caps["array"] = true
					caps["value64"] = true
				default:
					caps["array"] = true
				}
			}
		}
	}
	for _, m := range s.Messages {
		walk(m.Fields)
	}
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ---- max-size cost model (PLAN §5.5) ----

func (g *gen) maxSize(fields []*ir.Field) (int64, bool) {
	var total int64
	seen := map[string]bool{}
	for _, f := range fields {
		c, ok := g.fieldCost(f, seen)
		if !ok {
			return 8192, true
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
		switch f.Elem {
		case ir.KindString, ir.KindBlob:
			if !f.ElemMaxHas {
				return 0, false
			}
			per := varintLen(uint64(f.Count)<<3|7) + varintLen(uint64(f.ElemMax)<<3) + f.ElemMax
			return hdr + 1 + f.Count*per + 1, true
		default:
			return hdr + varintLen(uint64(f.Count)) + f.Count*10, true
		}
	case ir.KindStruct, ir.KindUnion:
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

func varintLen(x uint64) int64 {
	n := int64(1)
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}

var _ = fmt.Sprintf
