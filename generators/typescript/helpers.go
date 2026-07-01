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

// emitDoc writes a TSDoc/JSDoc `/** ... */` block immediately before the
// declaration it documents, at the given indent. Single-line text becomes
// `/** text */`; multi-line text becomes a starred block. Any `*/` inside the
// text is defanged to `* /` so it cannot close the comment early. Empty text
// emits nothing, so it never leaves a dangling comment.
func (f *tsfile) emitDoc(indent, text string) {
	if text == "" {
		return
	}
	text = strings.ReplaceAll(text, "*/", "* /")
	lines := strings.Split(text, "\n")
	if len(lines) == 1 {
		f.line("%s/** %s */", indent, lines[0])
		return
	}
	f.line("%s/**", indent)
	for _, ln := range lines {
		if ln == "" {
			f.line("%s *", indent)
		} else {
			f.line("%s * %s", indent, ln)
		}
	}
	f.line("%s */", indent)
}

// fieldDoc builds a field's TSDoc text from its Description and Unit: the
// description with " (unit: <Unit>)" appended when a unit is set, or just
// "(unit: <Unit>)" when only a unit is present. Empty when both are empty.
func fieldDoc(fld *ir.Field) string {
	switch {
	case fld.Description != "" && fld.Unit != "":
		return fld.Description + " (unit: " + fld.Unit + ")"
	case fld.Description != "":
		return fld.Description
	case fld.Unit != "":
		return "(unit: " + fld.Unit + ")"
	default:
		return ""
	}
}

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
		return g.tsArrayType(f.Elem, f.ElemRef, f.ElemItems)
	}
	return "unknown"
}

// tsArrayType returns the `T[]` member type for an array element, recursing for
// nested arrays (array-of-array -> T[][]).
func (g *gen) tsArrayType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "string[]"
	case ir.KindBlob:
		return "Uint8Array[]"
	case ir.KindU64, ir.KindI64:
		return "bigint[]"
	case ir.KindBool:
		return "boolean[]"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key) + "[]"
	case ir.KindArray:
		return g.tsArrayType(items.Elem, items.ElemRef, items.ElemItems) + "[]"
	default: // integers, bitfield
		return "number[]"
	}
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
		return g.tsArrayToJSON(acc, f.Elem, f.ElemRef, f.ElemItems, 0)
	default:
		return acc
	}
}

// tsArrayToJSON builds a JSON-able expression for an array value: u64/i64 -> string,
// blob -> number[], struct/union -> toJSON(); recurses for nested arrays. enum/
// bool/bitfield/numeric/string are already JSON-native (identity).
func (g *gen) tsArrayToJSON(val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) string {
	x := fmt.Sprintf("_x%d", depth)
	switch elem {
	case ir.KindU64, ir.KindI64:
		return fmt.Sprintf("%s.map((%s) => %s.toString())", val, x, x)
	case ir.KindBlob:
		return fmt.Sprintf("%s.map((%s) => Array.from(%s))", val, x, x)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("%s.map((%s) => %s.toJSON())", val, x, x)
	case ir.KindArray:
		return fmt.Sprintf("%s.map((%s) => %s)", val, x, g.tsArrayToJSON(x, items.Elem, items.ElemRef, items.ElemItems, depth+1))
	default:
		return val
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
		return fmt.Sprintf("%s = %s", acc, g.tsArrayFromJSON(src, f.Elem, f.ElemRef, f.ElemItems, 0))
	}
	return acc + " = undefined as never"
}

// tsArrayFromJSON rebuilds an array from JSON: u64/i64 -> bigint, blob -> Uint8Array,
// struct/union -> fromJSON(); recurses for nested arrays. enum/bool/bitfield/numeric/
// string are plain casts.
func (g *gen) tsArrayFromJSON(src string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) string {
	x := fmt.Sprintf("_x%d", depth)
	switch elem {
	case ir.KindU64, ir.KindI64:
		return fmt.Sprintf("(%s as (string | number)[]).map((%s) => BigInt(%s))", src, x, x)
	case ir.KindBlob:
		return fmt.Sprintf("(%s as number[][]).map((%s) => new Uint8Array(%s))", src, x, x)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("(%s as Record<string, unknown>[]).map((%s) => %s.fromJSON(%s))", src, x, g.typeName(ref.Key), x)
	case ir.KindEnum:
		return fmt.Sprintf("%s as %s[]", src, g.typeName(ref.Key))
	case ir.KindArray:
		return fmt.Sprintf("(%s as unknown[]).map((%s) => %s)", src, x, g.tsArrayFromJSON(x, items.Elem, items.ElemRef, items.ElemItems, depth+1))
	case ir.KindBool:
		return fmt.Sprintf("%s as boolean[]", src)
	case ir.KindString:
		return fmt.Sprintf("%s as string[]", src)
	default:
		return fmt.Sprintf("%s as number[]", src)
	}
}
