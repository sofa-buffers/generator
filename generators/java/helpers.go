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

// cfgLimit reads an integer decode-limit key (generator#102). YAML/JSON decode
// integers into different Go types depending on the path, so all are accepted.
func cfgLimit(cfg map[string]any, key string) (int64, bool) {
	switch v := cfg[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// javaOmitCond is the condition under which to write a field (value differs from
// its default): sparse encoding is canonical (MESSAGE_SPEC S2). Strings use
// Objects.equals (content compare).
func (g *gen) javaOmitCond(f *ir.Field) string {
	acc := "this." + javaIdent(f.Name)
	def := g.javaDefaultValue(f)
	if f.Kind == ir.KindString {
		// Same truth table as !Objects.equals(acc, def) for a non-null literal
		// default, without the static-call indirection; an empty default is
		// just an isEmpty check.
		if def == `""` {
			return fmt.Sprintf("(%s == null || !%s.isEmpty())", acc, acc)
		}
		return fmt.Sprintf("!%s.equals(%s)", def, acc)
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
//
// A declared `count: N` takes no part in it. `count` is a CAPACITY, never a
// length (MESSAGE_SPEC §3): it never reaches the wire, so a default shorter than
// N stands exactly as written rather than being tail-padded to N, and a count:N
// array with no declared default is simply the EMPTY array.
func (g *gen) javaNativeArrayLiteral(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false // no (or non-array) default: no literal
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

// primitiveArrayElem reports whether an array element lowers to a Java primitive
// array (`long[]`/`float[]`/`double[]`) instead of a boxed `List<...>`: integers,
// enum and bitfield (all long-backed, delivered via signed/unsigned) and fp. It
// is the hot allocator — boxing every element and unboxing on encode. Boolean
// arrays stay `List<Boolean>` (no primitive OStream overload for them), and
// string/blob/struct/union/nested arrays are wrapper sequences (not native).
func primitiveArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindEnum, ir.KindBitfield, ir.KindFP32, ir.KindFP64:
		return true
	}
	return false
}

// primArrayBase is the Java primitive element type backing a primitive array:
// long for every integer width (the corelib widens to 64-bit), float/double for
// the fp kinds.
func primArrayBase(k ir.Kind) string {
	switch k {
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	default:
		return "long"
	}
}

// javaPrimArrayLiteral renders a primitive array field's schema default as a
// `new long[]{...}` / `new float[]{...}` / `new double[]{...}` literal, exactly
// as the schema wrote it. ("", false) when there is no default.
//
// A declared `count: N` contributes nothing: `count` is a CAPACITY, not a length
// (MESSAGE_SPEC §3), so a short default is NOT tail-padded to N and a count:N
// array with no default has no literal at all — its value is the empty array,
// which is also what an absent field decodes back to. Padding here would make an
// all-zero N-element array compare equal to "no value" and vanish from the wire,
// where §3 says [1,2,3,0,0] and [1,2,3] are different values.
func (g *gen) javaPrimArrayLiteral(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	base := primArrayBase(f.Elem)
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, g.javaArrayElemLit(f.Elem, v))
	}
	return fmt.Sprintf("new %s[]{%s}", base, strings.Join(parts, ", ")), true
}

// javaType: all integers map to long (Java has no unsigned); native numeric/fp
// arrays are primitive arrays (long[]/float[]/double[]), other arrays use List.
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
		if primitiveArrayElem(f.Elem) {
			return primArrayBase(f.Elem) + "[]"
		}
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
		// against it. A composite array is a wrapper sequence whose declared default
		// is not materialized, which is what makes its dropping closer correct (§2).
		if primitiveArrayElem(f.Elem) {
			if lit, ok := g.javaPrimArrayLiteral(f); ok {
				return " = " + lit
			}
			switch primArrayBase(f.Elem) {
			case "float":
				return " = Sbuf.EMPTY_FLOATS"
			case "double":
				return " = Sbuf.EMPTY_DOUBLES"
			default:
				return " = Sbuf.EMPTY_LONGS"
			}
		}
		if nativeArrayElem(f.Elem) { // boolean array (stays boxed List<Boolean>)
			if lit, ok := g.javaNativeArrayLiteral(f); ok {
				return " = new ArrayList<>(" + lit + ")"
			}
		}
		// A wrapper array starts EMPTY, with or without a declared `count: N`:
		// `count` is a capacity, not a length (MESSAGE_SPEC §3), so a fresh count:N
		// array holds no elements and an absent field decodes back to none.
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
		return " = Sbuf.EMPTY_BYTES"
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
    // Shared zero-length defaults: field initializers reference these instead of
    // allocating a fresh empty array per instance (decode replaces them anyway).
    static final long[] EMPTY_LONGS = {};
    static final float[] EMPTY_FLOATS = {};
    static final double[] EMPTY_DOUBLES = {};
    static final byte[] EMPTY_BYTES = {};

    static long[] toLongArray(List<Long> l) { long[] a = new long[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i); return a; }
    static long[] boolToLongArray(List<Boolean> l) { long[] a = new long[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i) ? 1 : 0; return a; }
    static float[] toFloatArray(List<Float> l) { float[] a = new float[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i); return a; }
    static double[] toDoubleArray(List<Double> l) { double[] a = new double[l.size()]; for (int i = 0; i < a.length; i++) a[i] = l.get(i); return a; }

    // placeRow stores a FRESH empty row of a matrix (an array whose elements are
    // themselves arrays) at the index its element id names, growing the outer list
    // with empty rows so an id GAP decodes as an empty row instead of shifting
    // every later row down by one. Gaps are ordinary: an interior row equal to the
    // element default (the empty row) is omitted by a conformant encoder (S2), and
    // only the LAST row is guaranteed present -- which is what makes the decoded
    // length, highest present id + 1, exact. The row is replaced rather than merged
    // into, because an array wrapper IS the array's value (S7.4). The caller's
    // over-index guard bounds the id against the outer array's schema capacity
    // before this grows anything.
    static <T> void placeRow(List<List<T>> l, int id) {
        while (l.size() <= id) l.add(new java.util.ArrayList<>());
        l.set(id, new java.util.ArrayList<>());
    }

    // resetList empties a list IN PLACE, keeping its capacity, and materializes one
    // only when the field is null. The generated reset() uses it so re-arming a
    // reused decode destination costs no allocation -- which is the whole point of
    // taking a destination from the caller.
    static <T> List<T> resetList(List<T> l) { if (l == null) return new java.util.ArrayList<>(); l.clear(); return l; }

    // orEmpty is the null-absorbing identity the marshal loop and the all-default
    // predicate both run a WRAPPER array through. No narrowing happens here and
    // none may: the wire count IS a compact array's length and the highest wrapper
    // id IS its last index (S3/S5.1), so dropping a trailing default element would
    // not re-shape the bytes, it would SHORTEN the value. What the interior may
    // drop -- a leaf equal to the element default, an all-default sequence element
    // -- is decided per element inside the loop, never here.
    static <T> List<T> orEmpty(List<T> a) { return a == null ? java.util.Collections.emptyList() : a; }
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
