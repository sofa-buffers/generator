package typescript

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

func cfgBool(cfg map[string]any, key string) bool {
	b, _ := cfg[key].(bool)
	return b
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

func isBig(k ir.Kind) bool { return k == ir.KindU64 || k == ir.KindI64 }

func (g *gen) tsType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		return "bigint"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindBitfield, ir.KindFP32, ir.KindFP64:
		return "number"
	case ir.KindBool:
		return "boolean"
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "Uint8Array"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		switch f.Elem {
		case ir.KindString:
			return "string[]"
		case ir.KindBlob:
			return "Uint8Array[]"
		case ir.KindU64, ir.KindI64:
			return "bigint[]"
		default:
			return "number[]"
		}
	}
	return "unknown"
}

func (g *gen) tsDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		if f.Default != nil {
			return scalarLit(f.Default) + "n"
		}
		return "0n"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32:
		if f.Default != nil {
			return scalarLit(f.Default)
		}
		return "0"
	case ir.KindBitfield:
		return fmt.Sprintf("%d", g.bitfieldDefault(f))
	case ir.KindFP32, ir.KindFP64:
		if f.Default != nil {
			return fmt.Sprintf("%v", f.Default)
		}
		return "0"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", s)
		}
		return `""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return fmt.Sprintf("new Uint8Array(%s)", intListLit(raw))
			}
		}
		return "new Uint8Array()"
	case ir.KindEnum:
		tn := g.typeName(f.Ref.Key)
		if f.Default != nil {
			if name, ok := g.enumMember(f.Ref.Target, f.Default); ok {
				return tn + "." + name
			}
			return fmt.Sprintf("(%s as %s)", scalarLit(f.Default), tn)
		}
		return fmt.Sprintf("(0 as %s)", tn)
	case ir.KindStruct, ir.KindUnion:
		return "new " + g.typeName(f.Ref.Key) + "()"
	case ir.KindArray:
		return "[]"
	}
	return "undefined as never"
}

func (g *gen) enumMember(nt *ir.NamedType, def any) (string, bool) {
	v, ok := asInt(def)
	if !ok {
		return "", false
	}
	for _, c := range nt.Consts {
		if c.Value == v {
			return exported(c.Name), true
		}
	}
	return "", false
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

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

func scalarLit(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func intListLit(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ---- JSON (canonical: blob as number[], bigint as string for self round-trip) --

func (g *gen) emitJSON(f *tsfile, name string, fields []*ir.Field) {
	f.line("  toJSON(): Record<string, unknown> {")
	f.line("    return {")
	for _, fld := range fields {
		f.line("      %q: %s,", fld.Name, g.toJSONExpr(fld))
	}
	f.line("    };")
	f.line("  }")
	f.blank()
	f.line("  static fromJSON(d: Record<string, unknown>): %s {", name)
	f.line("    const o = new %s();", name)
	for _, fld := range fields {
		f.line("    if (%q in d) %s;", fld.Name, g.fromJSONStmt(fld))
	}
	f.line("    return o;")
	f.line("  }")
	f.blank()
}

func (g *gen) toJSONExpr(f *ir.Field) string {
	acc := "this." + f.Name
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		return acc + ".toString()"
	case ir.KindBlob:
		return "Array.from(" + acc + ")"
	case ir.KindStruct, ir.KindUnion:
		return acc + ".toJSON()"
	case ir.KindArray:
		switch f.Elem {
		case ir.KindU64, ir.KindI64:
			return acc + ".map((x) => x.toString())"
		case ir.KindBlob:
			return acc + ".map((x) => Array.from(x))"
		default:
			return acc
		}
	default:
		return acc
	}
}

func (g *gen) fromJSONStmt(f *ir.Field) string {
	acc := "o." + f.Name
	src := fmt.Sprintf("d[%q]", f.Name)
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		return fmt.Sprintf("%s = BigInt(%s as string | number)", acc, src)
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindBitfield, ir.KindFP32, ir.KindFP64:
		return fmt.Sprintf("%s = %s as number", acc, src)
	case ir.KindBool:
		return fmt.Sprintf("%s = %s as boolean", acc, src)
	case ir.KindString:
		return fmt.Sprintf("%s = %s as string", acc, src)
	case ir.KindBlob:
		return fmt.Sprintf("%s = new Uint8Array(%s as number[])", acc, src)
	case ir.KindEnum:
		return fmt.Sprintf("%s = %s as number", acc, src)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("%s = %s.fromJSON(%s as Record<string, unknown>)", acc, g.typeName(f.Ref.Key), src)
	case ir.KindArray:
		switch f.Elem {
		case ir.KindU64, ir.KindI64:
			return fmt.Sprintf("%s = (%s as (string | number)[]).map((x) => BigInt(x))", acc, src)
		case ir.KindBlob:
			return fmt.Sprintf("%s = (%s as number[][]).map((x) => new Uint8Array(x))", acc, src)
		case ir.KindString:
			return fmt.Sprintf("%s = %s as string[]", acc, src)
		default:
			return fmt.Sprintf("%s = %s as number[]", acc, src)
		}
	}
	return acc + " = undefined as never"
}
