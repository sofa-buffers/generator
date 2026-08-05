package csharp

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
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
func fieldDoc(f *ir.Field, note string) string {
	var doc string
	switch {
	case f.Description != "" && f.Unit != "":
		doc = f.Description + " (unit: " + f.Unit + ")"
	case f.Description != "":
		doc = f.Description
	case f.Unit != "":
		doc = "(unit: " + f.Unit + ")"
	}
	doc = generator.AppendDoc(doc, note)
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

// csSeqElemDefault renders the element default of a WRAPPER (sequence) array:
// the value the decode-side gap-fill puts at an index no wire element reached —
// which is exactly the value the encoder omitted there (MESSAGE_SPEC §2).
// ("", false) for a native element, which is not a wrapper.
func (g *gen) csSeqElemDefault(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) (string, bool) {
	switch elem {
	case ir.KindString:
		return `""`, true
	case ir.KindBlob:
		return "Array.Empty<byte>()", true
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("new %s()", g.typeName(ref.Key)), true
	case ir.KindArray:
		return fmt.Sprintf("new List<%s>()", g.csArrayElemType(items.Elem, items.ElemRef, items.ElemItems)), true
	}
	return "", false
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
		// A NATIVE scalar array is a leaf field: materialize its default so an
		// omitted default array reconstructs correctly and marshal can compare
		// against it. A composite array is a wrapper sequence whose declared default
		// is not materialized, which is what makes its dropping closer correct (§2).
		if primArrayElem(f.Elem) {
			if lit, ok := g.csPrimArrayLiteral(f); ok {
				return " = " + lit
			}
			return " = Array.Empty<" + g.csArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">()"
		}
		if lit, ok := g.csNativeArrayLiteral(f); ok {
			return " = " + lit
		}
		// A WRAPPER array starts EMPTY, with or without a declared `count: N`.
		// `count` is a CAPACITY, not a length (MESSAGE_SPEC §3): it never reaches the
		// wire and never adds elements, so a fresh count:N array is the empty array —
		// which is also what its omit test compares against and what an absent field
		// decodes back to.
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
	// Not padded to a declared `count: N`: that is a capacity, not a length
	// (MESSAGE_SPEC §3), so the default stands exactly as written — and so does the
	// value it is compared against, which is what keeps a length-N all-zero array
	// distinct from the empty one.
	return fmt.Sprintf("new List<%s>{%s}", elemType, strings.Join(parts, ", ")), true
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
	// Not padded to a declared `count: N` (see csNativeArrayLiteral).
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

// fp32ArrayElem / fp64ArrayElem report whether an array element is delivered
// through Fp32() / Fp64() respectively — the two fixlen array wire subtypes.
// They are deliberately kept apart rather than folded into one "is fp" test:
// CORELIB_PLAN §4.8 makes the fixlen SUBTYPE part of the header, so an fp64
// header at an fp32-declared id is a MESSAGE_SPEC §7.3 wire-type contradiction
// that must be skipped, and a single predicate could not express that
// (generator#259 / Crucible F-0042).
func fp32ArrayElem(k ir.Kind) bool { return k == ir.KindFP32 }
func fp64ArrayElem(k ir.Kind) bool { return k == ir.KindFP64 }

// csArrayWireKind is the ArrayKind an array of `k` is encoded with (MESSAGE_SPEC
// §1/§3) — the only header kind that may decode into such a field. A fixlen
// array names its element subtype (Fp32/Fp64), never a collapsed "fixlen"
// category: the corelib's ArrayKind carries the subtype since CORELIB_PLAN §4.8
// moved the array-header hook past the fixlen_word (generator#259).
func csArrayWireKind(k ir.Kind) string {
	switch {
	case unsignedArrayElem(k):
		return "Unsigned"
	case signedArrayElem(k):
		return "Signed"
	case fp32ArrayElem(k):
		return "Fp32"
	case fp64ArrayElem(k):
		return "Fp64"
	}
	return ""
}

// arrayKindGuard is the leading clause of an ArrayBegin arm: leave the arm
// untouched unless the header's array kind is the one this element type maps to.
// A mis-typed header must be skipped exactly like an unknown id (S7.3), which
// includes not RESIZING the declared field from its count. Emitted BEFORE the
// schema-bound guard, so the bound applies only to a field that survives the kind
// test — an over-count mis-typed array is skipped, not a false InvalidMessage
// (generator#254).
func arrayKindGuard(k ir.Kind) string {
	wk := csArrayWireKind(k)
	if wk == "" {
		return ""
	}
	return "if (kind != ArrayKind." + wk + ") break; "
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

// ixVar names the visitor field holding the element index an array scope is
// currently decoding into. A wrapper element's id IS its array index
// (MESSAGE_SPEC §5.1 — generator#247), so the element scope must address
// `list[id]`, not "the last element"; the id is latched here by SequenceBegin
// (or, for a native inner row, by ArrayBegin) because the element's own
// callbacks arrive later, under the child scope. Each array scope has its own
// variable (scopes are a static, acyclic tree, so no scope is ever re-entered
// while active) and a nested scope's path composes off its parent's.
func ixVar(loc string) string { return "_ix" + loc }

// elemAt is the accessor for the element an array scope is decoding into.
func elemAt(list, loc string) string { return list + "[" + ixVar(loc) + "]" }
