package csharp

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// xmlEscape escapes the three XML-special characters so a description stays
// well-formed inside an XML doc comment. UTF-8 letters/symbols pass through
// byte-for-byte. Order matters: `&` must be escaped first.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// fieldDoc builds the doc text for a field from its Description and Unit:
// Description, with " (unit: <Unit>)" appended when a Unit is set; if only a
// Unit is present the doc is "(unit: <Unit>)". Empty when both are empty.
func fieldDoc(f *ir.Field) string {
	var doc string
	switch {
	case f.Description != "" && f.Unit != "":
		doc = f.Description + " (unit: " + f.Unit + ")"
	case f.Description != "":
		doc = f.Description
	case f.Unit != "":
		doc = "(unit: " + f.Unit + ")"
	}
	// A deprecated field carries the [Obsolete] attribute for tooling; the doc
	// generator (XML-doc) has no @deprecated tag, so keep a human "Deprecated."
	// note on its own doc line.
	if f.Deprecated {
		if doc != "" {
			doc += "\n"
		}
		doc += "Deprecated."
	}
	return doc
}

// flagDoc builds the doc text for a bitfield flag: its Description, with a
// " (default: true)" / " (default: false)" note appended when the flag declares
// a default. Empty when the flag has neither.
func flagDoc(fl *ir.BitfieldFlag) string {
	doc := fl.Description
	if fl.HasDefault {
		note := "(default: false)"
		if fl.Default {
			note = "(default: true)"
		}
		if doc != "" {
			doc += " " + note
		} else {
			doc = note
		}
	}
	return doc
}

// emitDoc writes an XML <summary> doc comment for text at the given indent.
// Empty text emits nothing. Multi-line text uses the docfx-friendly block form.
func emitDoc(f *cfile, indent, text string) {
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	f.line("%s/// <summary>", indent)
	for _, ln := range lines {
		f.line("%s/// %s", indent, xmlEscape(ln))
	}
	f.line("%s/// </summary>", indent)
}

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

// csDefaultValue is the value a field is compared against for omission (its
// init default, or the type-zero), matching the field initializer.
func (g *gen) csDefaultValue(f *ir.Field) string {
	if init := g.csInit(f); init != "" {
		return strings.TrimPrefix(init, " = ")
	}
	switch f.Kind {
	case ir.KindBool:
		return "false"
	case ir.KindString:
		return `""`
	default:
		return "0"
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
		if primArrayElem(f.Elem) {
			return g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + "[]"
		}
		return "List<" + g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">"
	case ir.KindMap:
		// map<K,V> -> Dictionary. Keys are sorted on encode for canonical bytes.
		return fmt.Sprintf("Dictionary<%s, %s>", g.csType(f.MapKey()), g.csType(f.MapValue()))
	}
	return "object"
}

// csArrayElemType is the C# type of an array element, recursing for nested
// arrays. Numeric elements map to their scalar type; enum/bitfield/struct/union
// to the named type; string/blob to string/byte[]; a nested array to List<...>.
func (g *gen) csArrayElemType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "byte[]"
	case ir.KindBool:
		return "bool"
	case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return "List<" + g.csArrayElemType(items.Elem, items.ElemRef, items.ElemItems) + ">"
	default:
		return numCsType(elem)
	}
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
	case ir.KindStruct, ir.KindUnion, ir.KindMap:
		return " = new()"
	case ir.KindArray:
		// A NATIVE scalar array is a leaf field: materialize its default so an
		// omitted default array reconstructs correctly and marshal can compare
		// against it. Composite arrays are wrapper sequences (always framed).
		if primArrayElem(f.Elem) {
			if lit, ok := g.csPrimArrayLiteral(f); ok {
				return " = " + lit
			}
			return " = Array.Empty<" + g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">()"
		}
		if lit, ok := g.csNativeArrayLiteral(f); ok {
			return " = " + lit
		}
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
			// parenthesize the value so a negative default casts correctly (CS0075).
			return fmt.Sprintf(" = (%s)(%s)", g.typeName(f.Ref.Key), scalarLit(f.Default))
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

// csNativeArrayLiteral renders a native scalar array's schema default as a C#
// list literal (new List<T>{...}); ("", false) when the element is not a native
// scalar or there is no default. enum/bitfield elements are cast (nonzero ints
// have no implicit enum conversion); fp32 elements take the float suffix.
func (g *gen) csNativeArrayLiteral(f *ir.Field) (string, bool) {
	if !nativeArrayElem(f.Elem) {
		return "", false
	}
	vals, ok := f.Default.([]any)
	if !ok {
		// A `count: N` native array is fixed-length even with no schema default:
		// its value is N element defaults, so materialize them. Without this a
		// fresh (or all-default, hence omitted-on-the-wire) array would decode to
		// an empty list on this growable backend while the fixed-storage camp
		// yields N zeros — the same MESSAGE_SPEC §3 divergence as the trailing
		// default run, reached through the omission path. Constructing from a
		// zeroed T[N] keeps the emitted source O(1) for a large N.
		if f.Default == nil && f.HasCount {
			et := g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems)
			return fmt.Sprintf("new List<%s>(new %s[%d])", et, et, f.Count), true
		}
		return "", false
	}
	elemType := g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems)
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch f.Elem {
		case ir.KindBool:
			if b, ok := v.(bool); ok && b {
				parts[i] = "true"
			} else {
				parts[i] = "false"
			}
		case ir.KindFP32:
			parts[i] = floatLit(v) + "f"
		case ir.KindFP64:
			parts[i] = floatLit(v)
		case ir.KindEnum, ir.KindBitfield:
			parts[i] = fmt.Sprintf("(%s)(%s)", elemType, scalarLit(v))
		default: // numeric: an in-range integer constant converts implicitly
			parts[i] = scalarLit(v)
		}
	}
	parts = g.tailPadLiteral(f, parts, elemType)
	return fmt.Sprintf("new List<%s>{%s}", elemType, strings.Join(parts, ", ")), true
}

// tailPadLiteral extends a `count: N` array's schema default to exactly N
// elements with the element default: the array is fixed-length, so a shorter
// schema default leaves the trailing elements at the element default, and this
// backend's initial value must match the fixed-storage camp's zero-filled
// `[T; N]` / `std::array<T, N>` (MESSAGE_SPEC §3). Dynamic arrays keep the
// default exactly as written.
func (g *gen) tailPadLiteral(f *ir.Field, parts []string, elemType string) []string {
	if !f.HasCount {
		return parts
	}
	zero := "0"
	switch f.Elem {
	case ir.KindBool:
		zero = "false"
	case ir.KindFP32:
		zero = "0f"
	case ir.KindEnum, ir.KindBitfield:
		zero = "(" + elemType + ")(0)"
	}
	for int64(len(parts)) < f.Count {
		parts = append(parts, zero)
	}
	return parts
}

// csPrimArrayLiteral renders a primitive (numeric/fp) array field's schema
// default as a `new T[]{...}` literal; ("", false) when there is no default.
// Element rendering matches csNativeArrayLiteral so the marshal omit-compare
// sees identical values.
func (g *gen) csPrimArrayLiteral(f *ir.Field) (string, bool) {
	if !primArrayElem(f.Elem) {
		return "", false
	}
	vals, ok := f.Default.([]any)
	if !ok {
		// A `count: N` array with no schema default is N element defaults, not an
		// empty array (see csNativeArrayLiteral). `new T[N]` is zero-filled and
		// keeps the emitted source O(1) for a large N.
		if f.Default == nil && f.HasCount {
			return fmt.Sprintf("new %s[%d]", g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems), f.Count), true
		}
		return "", false
	}
	elemType := g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems)
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch f.Elem {
		case ir.KindFP32:
			parts[i] = floatLit(v) + "f"
		case ir.KindFP64:
			parts[i] = floatLit(v)
		default: // numeric: an in-range integer constant converts implicitly
			parts[i] = scalarLit(v)
		}
	}
	parts = g.tailPadLiteral(f, parts, elemType)
	return fmt.Sprintf("new %s[]{%s}", elemType, strings.Join(parts, ", ")), true
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
		case ir.KindStruct, ir.KindUnion, ir.KindArray:
			// Composite/nested-array elements are not statically bounded here;
			// fall back to the whole-message size cap.
			return 0, false
		default:
			// Numeric/enum/boolean/bitfield elements use the native array wire.
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
	case ir.KindMap:
		// Unbounded wrapper sequence: fall back to the analytic MAX_SIZE cap.
		return 0, false
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

// csKeywords are C# reserved words; used as an identifier they need the
// verbatim-identifier escape `@name`. System.Text.Json serialises `@int` under
// the name "int", so JSON/wire names are unchanged.
var csKeywords = map[string]bool{
	"abstract": true, "as": true, "base": true, "bool": true, "break": true,
	"byte": true, "case": true, "catch": true, "char": true, "checked": true,
	"class": true, "const": true, "continue": true, "decimal": true, "default": true,
	"delegate": true, "do": true, "double": true, "else": true, "enum": true,
	"event": true, "explicit": true, "extern": true, "false": true, "finally": true,
	"fixed": true, "float": true, "for": true, "foreach": true, "goto": true,
	"if": true, "implicit": true, "in": true, "int": true, "interface": true,
	"internal": true, "is": true, "lock": true, "long": true, "namespace": true,
	"new": true, "null": true, "object": true, "operator": true, "out": true,
	"override": true, "params": true, "private": true, "protected": true, "public": true,
	"readonly": true, "ref": true, "return": true, "sbyte": true, "sealed": true,
	"short": true, "sizeof": true, "stackalloc": true, "static": true, "string": true,
	"struct": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "uint": true, "ulong": true, "unchecked": true,
	"unsafe": true, "ushort": true, "using": true, "virtual": true, "void": true,
	"volatile": true, "while": true,
}

// csIdent escapes a field name that is a C# keyword as a verbatim identifier.
func csIdent(name string) string {
	if csKeywords[name] {
		return "@" + name
	}
	return name
}

// ---- array element classification ----------------------------------------

// unsignedArrayElem reports whether an array element is delivered through the
// Unsigned callback (native unsigned wire type): u*/boolean/bitfield.
func unsignedArrayElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64 ||
		k == ir.KindBool || k == ir.KindBitfield
}

// signedArrayElem reports whether an array element is delivered through the
// Signed callback (native signed wire type): i*/enum.
func signedArrayElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64 ||
		k == ir.KindEnum
}

// nativeArrayElem reports whether an array element encodes as a native array
// wire type (numeric/enum/boolean/bitfield) rather than a wrapper sequence.
func nativeArrayElem(k ir.Kind) bool {
	return unsignedArrayElem(k) || signedArrayElem(k) || k == ir.KindFP32 || k == ir.KindFP64
}

// primArrayElem reports whether an array element lowers to a C# primitive
// array (`byte[]`/`int[]`/`float[]`/...) instead of a boxed-growth `List<T>`:
// the pure numeric and fp kinds. It is the hot allocator — a List field costs
// a `.ToArray()` temporary on every encode and Add-growth on every decode.
// Bool/enum/bitfield arrays stay `List<T>` (they value-convert element-wise),
// and string/blob/struct/union/nested arrays are wrapper sequences.
func primArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64:
		return true
	}
	return false
}

// seqArrayElem reports whether an array element lowers to a wrapper sequence
// (string/blob/struct/union, or a nested array).
func seqArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindStruct, ir.KindUnion, ir.KindArray:
		return true
	}
	return false
}

// arrayElemAddRHS converts a decoded native array element `v` to the member
// element type before appending: bool becomes a comparison, enum/bitfield cast
// to the named type, floats pass through, and integers narrow to their width.
func (g *gen) arrayElemAddRHS(elem ir.Kind, ref *ir.TypeRef, v string) string {
	switch elem {
	case ir.KindBool:
		return v + " != 0"
	case ir.KindEnum, ir.KindBitfield:
		return "(" + g.typeName(ref.Key) + ")" + v
	case ir.KindFP32, ir.KindFP64:
		return v
	default: // numeric
		return "(" + numCsType(elem) + ")" + v
	}
}

// lastElem is the accessor for the most-recently-added element of List `list`,
// used as the target when decoding into an array element in-place.
func lastElem(list string) string {
	return list + "[" + list + ".Count - 1]"
}
