package java

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

// javaOmitCond is the condition under which to write a field (value differs from
// its default): sparse encoding is canonical (MESSAGE_SPEC S2). Strings use
// Objects.equals (content compare).
func (g *gen) javaOmitCond(f *ir.Field) string {
	acc := "this." + javaIdent(f.Name)
	def := g.javaDefaultValue(f)
	if f.Kind == ir.KindString {
		return fmt.Sprintf("!java.util.Objects.equals(%s, %s)", acc, def)
	}
	return fmt.Sprintf("%s != %s", acc, def)
}

func (g *gen) javaDefaultValue(f *ir.Field) string {
	if init := g.javaInit(f); init != "" {
		return strings.TrimPrefix(init, " = ")
	}
	switch f.Kind {
	case ir.KindBool:
		return "false"
	case ir.KindString:
		return `""`
	case ir.KindFP32:
		return "0f"
	case ir.KindFP64:
		return "0"
	default:
		return "0L"
	}
}

// javaNativeArrayLiteral renders a native scalar array's schema default as an
// immutable-List expression (List.of(...)); ("", false) when there is no default.
// It is used both to materialize the field default and, in marshal, as the RHS to
// compare against for whole-array omission.
func (g *gen) javaNativeArrayLiteral(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = g.javaArrayElemLit(f.Elem, v)
	}
	return "List.of(" + strings.Join(parts, ", ") + ")", true
}

// javaArrayElemLit renders one native array element default as a boxed Java
// literal (Long/Float/Double/Boolean), matching the List<...> member type.
func (g *gen) javaArrayElemLit(elem ir.Kind, v any) string {
	switch elem {
	case ir.KindBool:
		return fmt.Sprintf("%v", v)
	case ir.KindFP32:
		return floatLit(v) + "f"
	case ir.KindFP64:
		return floatLit(v)
	case ir.KindU64:
		return fmt.Sprintf("Long.parseUnsignedLong(%q)", scalarLit(v))
	default: // integers, enum, bitfield -> Long
		return scalarLit(v) + "L"
	}
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

// javaType: all integers map to long (Java has no unsigned); arrays use List.
func (g *gen) javaType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum, ir.KindBitfield:
		return "long"
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	case ir.KindBool:
		return "boolean"
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "byte[]"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return "List<" + g.javaArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">"
	}
	return "Object"
}

// javaArrayElemType is the boxed element type stored in an array's List<...>.
// Integers/enum/bitfield box to Long, boolean to Boolean, fp to Float/Double;
// struct/union use the class type; nested arrays recurse into List<...>.
func (g *gen) javaArrayElemType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "byte[]"
	case ir.KindFP32:
		return "Float"
	case ir.KindFP64:
		return "Double"
	case ir.KindBool:
		return "Boolean"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return "List<" + g.javaArrayElemType(items.Elem, items.ElemRef, items.ElemItems) + ">"
	default: // integers, enum, bitfield
		return "Long"
	}
}

func (g *gen) javaInit(f *ir.Field) string {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return " = new " + g.typeName(f.Ref.Key) + "()"
	case ir.KindArray:
		// A native scalar array is a leaf field: materialize its schema default so
		// an omitted default array reconstructs correctly and marshal can compare
		// against it. Composite arrays are wrapper sequences (always framed).
		if nativeArrayElem(f.Elem) {
			if lit, ok := g.javaNativeArrayLiteral(f); ok {
				return " = new ArrayList<>(" + lit + ")"
			}
		}
		return " = new ArrayList<>()"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf(" = %q", s)
		}
		return ` = ""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return fmt.Sprintf(" = new byte[]{%s}", javaBytes(raw))
			}
		}
		return " = new byte[0]"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return " = true"
		}
		return ""
	case ir.KindU64:
		if f.Default != nil {
			return fmt.Sprintf(" = Long.parseUnsignedLong(%q)", scalarLit(f.Default))
		}
		return ""
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		if f.Default != nil {
			return fmt.Sprintf(" = %sL", scalarLit(f.Default))
		}
		return ""
	case ir.KindEnum:
		if f.Default != nil {
			return fmt.Sprintf(" = %sL", scalarLit(f.Default))
		}
		return ""
	case ir.KindBitfield:
		if bits := g.bitfieldDefault(f); bits != 0 {
			return fmt.Sprintf(" = %dL", bits)
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

func javaBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("(byte)%d", x)
	}
	return strings.Join(parts, ", ")
}

// reachable returns named-type keys used by m in post-order (children first).
// Both scalar refs (f.Ref) and composite array element refs (f.ElemRef / nested
// f.ElemItems) are followed so array-of-struct/union/enum element classes are
// discovered and emitted.
func (g *gen) reachable(m *ir.Message) []string {
	var order []string
	seen := map[string]bool{}
	var addRef func(ref *ir.TypeRef)
	var visit func(fields []*ir.Field)
	var visitElem func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)
	addRef = func(ref *ir.TypeRef) {
		if ref == nil || seen[ref.Key] {
			return
		}
		seen[ref.Key] = true
		t := ref.Target
		if t.Category == ir.CatStruct || t.Category == ir.CatUnion {
			visit(t.Fields)
		}
		order = append(order, ref.Key)
	}
	visitElem = func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		switch elem {
		case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
			addRef(ref)
		case ir.KindArray:
			visitElem(items.Elem, items.ElemRef, items.ElemItems)
		}
	}
	visit = func(fields []*ir.Field) {
		for _, f := range fields {
			if f.Ref != nil {
				addRef(f.Ref)
			}
			if f.Kind == ir.KindArray {
				visitElem(f.Elem, f.ElemRef, f.ElemItems)
			}
		}
	}
	visit(m.Fields)
	return order
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
		body, ok := g.arrayBodyCost(f.Count, f.Elem, f.ElemRef, f.ElemItems, f.ElemMaxHas, f.ElemMax, seen)
		if !ok {
			return 0, false
		}
		return hdr + body, true
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

// arrayBodyCost is an upper bound (in bytes) for an array's on-wire payload,
// excluding the outer field header. Native numeric/enum/boolean/bitfield elements
// use the native array wire type; string/blob/struct/union/nested-array elements
// lower to a wrapper sequence. A dynamic (count 0) array or an unbounded
// string/blob element makes the size unbounded (ok=false).
func (g *gen) arrayBodyCost(count int64, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64, seen map[string]bool) (int64, bool) {
	if count <= 0 {
		return 0, false
	}
	idxHdr := varintLen(uint64(count)<<3 | 7) // generous per-element header
	switch elem {
	case ir.KindString, ir.KindBlob:
		if !elemMaxHas {
			return 0, false
		}
		per := idxHdr + varintLen(uint64(elemMax)<<3) + elemMax
		return 1 + count*per + 1, true
	case ir.KindStruct, ir.KindUnion:
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
		per := idxHdr + inner + 1
		return 1 + count*per + 1, true
	case ir.KindArray:
		innerBody, ok := g.arrayBodyCost(items.Count, items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax, seen)
		if !ok {
			return 0, false
		}
		per := idxHdr + innerBody
		return 1 + count*per + 1, true
	default: // numeric / enum / boolean / bitfield -> native array
		return 1 + count*10, true
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

// sbufSupport is the shared array-conversion helper class.
func (g *gen) sbufSupport() []byte {
	spdx := ""
	if g.license != "" {
		spdx = fmt.Sprintf("// SPDX-License-Identifier: %s\n", g.license)
	}
	return []byte(fmt.Sprintf(`// Code generated by %s; DO NOT EDIT.
%spackage %s;
import java.util.List;

final class Sbuf {
    static long[] toLongArray(List<Long> l) { long[] a = new long[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i); return a; }
    static long[] boolToLongArray(List<Boolean> l) { long[] a = new long[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i) ? 1 : 0; return a; }
    static float[] toFloatArray(List<Float> l) { float[] a = new float[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i); return a; }
    static double[] toDoubleArray(List<Double> l) { double[] a = new double[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i); return a; }
}
`, g.banner, spdx, g.pkg))
}

// javaKeywords are Java reserved words. Java has no raw-identifier escape, so a
// field with such a name is mangled (trailing underscore); the JSON key keeps the
// original name (emitted as a separate string literal).
var javaKeywords = map[string]bool{
	"abstract": true, "assert": true, "boolean": true, "break": true, "byte": true,
	"case": true, "catch": true, "char": true, "class": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extends": true, "final": true, "finally": true, "float": true,
	"for": true, "goto": true, "if": true, "implements": true, "import": true,
	"instanceof": true, "int": true, "interface": true, "long": true, "native": true,
	"new": true, "package": true, "private": true, "protected": true, "public": true,
	"return": true, "short": true, "static": true, "strictfp": true, "super": true,
	"switch": true, "synchronized": true, "this": true, "throw": true, "throws": true,
	"transient": true, "try": true, "void": true, "volatile": true, "while": true,
	"true": true, "false": true, "null": true, "var": true, "record": true, "yield": true,
}

// javaIdent mangles a field name that is a Java keyword (trailing underscore).
func javaIdent(name string) string {
	if javaKeywords[name] {
		return name + "_"
	}
	return name
}
