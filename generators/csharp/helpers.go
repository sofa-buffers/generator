package csharp

import (
	"encoding/base64"
	"fmt"
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

func (g *gen) csType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8:
		return "byte"
	case ir.KindU16:
		return "ushort"
	case ir.KindU32:
		return "uint"
	case ir.KindU64:
		return "ulong"
	case ir.KindI8:
		return "sbyte"
	case ir.KindI16:
		return "short"
	case ir.KindI32:
		return "int"
	case ir.KindI64:
		return "long"
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "byte[]"
	case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		switch f.Elem {
		case ir.KindString:
			return "List<string>"
		case ir.KindBlob:
			return "List<byte[]>"
		default:
			return "List<" + numCsType(f.Elem) + ">"
		}
	}
	return "object"
}

func numCsType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "byte"
	case ir.KindU16:
		return "ushort"
	case ir.KindU32:
		return "uint"
	case ir.KindU64:
		return "ulong"
	case ir.KindI8:
		return "sbyte"
	case ir.KindI16:
		return "short"
	case ir.KindI32:
		return "int"
	case ir.KindI64:
		return "long"
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	}
	return "byte"
}

// csInit returns the field initializer (" = ...") or "" for plain default.
func (g *gen) csInit(f *ir.Field) string {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return " = new()"
	case ir.KindArray:
		return " = new()"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf(" = %q", s)
		}
		return ` = ""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return fmt.Sprintf(" = new byte[]{%s}", byteList(raw))
			}
		}
		return " = Array.Empty<byte>()"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return " = true"
		}
		return ""
	case ir.KindU64:
		if f.Default != nil {
			return fmt.Sprintf(" = %sUL", scalarLit(f.Default))
		}
		return ""
	case ir.KindI64:
		if f.Default != nil {
			return fmt.Sprintf(" = %sL", scalarLit(f.Default))
		}
		return ""
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32:
		if f.Default != nil {
			return fmt.Sprintf(" = %s", scalarLit(f.Default))
		}
		return ""
	case ir.KindFP32:
		if f.Default != nil {
			return fmt.Sprintf(" = %sf", floatLit(f.Default))
		}
		return ""
	case ir.KindFP64:
		if f.Default != nil {
			return fmt.Sprintf(" = %s", floatLit(f.Default))
		}
		return ""
	case ir.KindEnum:
		if f.Default != nil {
			return fmt.Sprintf(" = (%s)%s", g.typeName(f.Ref.Key), scalarLit(f.Default))
		}
		return ""
	case ir.KindBitfield:
		if bits := g.bitfieldDefault(f); bits != 0 {
			return fmt.Sprintf(" = (%s)%d", g.typeName(f.Ref.Key), bits)
		}
		return ""
	}
	return ""
}

func (g *gen) bitfieldDefault(f *ir.Field) uint64 {
	var bits uint64
	for _, fl := range f.Ref.Target.Flags {
		if fl.HasDefault && fl.Default {
			bits |= 1 << uint(fl.Pos)
		}
	}
	return bits
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
		return "sbyte"
	case lo >= -32768 && hi <= 32767:
		return "short"
	default:
		return "int"
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
		return "byte"
	case max <= 15:
		return "ushort"
	case max <= 31:
		return "uint"
	default:
		return "ulong"
	}
}

func scalarLit(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func floatLit(v any) string {
	var fv float64
	switch x := v.(type) {
	case float64:
		fv = x
	case int:
		fv = float64(x)
	case int64:
		fv = float64(x)
	default:
		return "0.0"
	}
	s := fmt.Sprintf("%g", fv)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func byteList(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ", ")
}

// ---- max-size cost model ----

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
