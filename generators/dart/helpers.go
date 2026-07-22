package dart

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// ---- config helpers -------------------------------------------------------

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

// ---- identifiers ----------------------------------------------------------

// dartKeywords are Dart reserved words: used as a field/member name they are a
// hard error. Dart has no verbatim-identifier escape (no C# `@`), so a collision
// is mangled with a trailing `_` (the C/Java/Python convention). The wire is
// keyed by id, and the JSON name stays the original (the harness maps the raw
// name), so mangling is source-only.
var dartKeywords = map[string]bool{
	"assert": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "default": true, "do": true, "else": true,
	"enum": true, "extends": true, "false": true, "final": true, "finally": true,
	"for": true, "if": true, "in": true, "is": true, "new": true, "null": true,
	"rethrow": true, "return": true, "super": true, "switch": true, "this": true,
	"throw": true, "true": true, "try": true, "var": true, "void": true,
	"while": true, "with": true,
	// Contextual/built-in identifiers that are unsafe as a member name.
	"await": true, "yield": true, "dynamic": true,
	// Core type names: a field named `int` would shadow the `int` type the
	// generated code references, so these are mangled too. (A field named after a
	// generated class is not escaped; the schema identifier space makes that rare
	// and the wire is id-keyed regardless.)
	"int": true, "double": true, "bool": true, "num": true, "String": true,
	"List": true, "Map": true, "Set": true, "Object": true, "Iterable": true,
	"Null": true, "Never": true, "Function": true, "Uint8List": true,
	"Symbol": true, "Type": true, "Enum": true, "Record": true,
}

// dartIdent mangles a field name that is a Dart reserved word with a trailing
// underscore. It also guards a leading digit / empty name defensively (the
// schema identifier pattern already forbids those).
func dartIdent(name string) string {
	if dartKeywords[name] {
		return name + "_"
	}
	return name
}

// typeName renders a graph key ("struct/Point", "enum/Colour", or an inline
// synthetic like "msg_field") as a PascalCase Dart type name.
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
	if b.Len() == 0 {
		return "X"
	}
	return b.String()
}

// exported PascalCases a message name into its Dart class name.
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

// ---- doc comments ---------------------------------------------------------

// fieldDoc builds the dartdoc text for a field: its Description, with a
// " (unit: <Unit>)" suffix when a Unit is set, and a "Deprecated." note when the
// field is deprecated (the @Deprecated annotation is emitted separately).
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
	if f.Deprecated {
		if doc != "" {
			doc += "\n"
		}
		doc += "Deprecated."
	}
	return doc
}

// flagDoc builds the dartdoc text for a bitfield flag: its Description plus a
// "(default: true|false)" note when the flag declares a default.
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

// emitDoc writes a `///` dartdoc comment for text at the given indent. Empty
// text emits nothing. Multi-line text is written one `///` line per line. The
// text passes through verbatim (UTF-8 preserved); only a comment terminator is
// neutralised (dartdoc has no `*/` hazard for `///` line comments).
func emitDoc(f *dfile, indent, text string) {
	if text == "" {
		return
	}
	for _, ln := range strings.Split(text, "\n") {
		f.line("%s/// %s", indent, ln)
	}
}

// ---- type mapping ---------------------------------------------------------

// dartType is the Dart storage type of a field. All integer widths map to `int`
// (Dart has one 64-bit int), floats to `double`, blob to `Uint8List`, arrays to
// `List<...>`, and composites to their generated class.
func (g *gen) dartType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum, ir.KindBitfield:
		return "int"
	case ir.KindFP32, ir.KindFP64:
		return "double"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "Uint8List"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return "List<" + g.dartArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">"
	}
	return "Object?"
}

// dartArrayElemType is the Dart type of an array element, recursing for nested
// arrays.
func (g *gen) dartArrayElemType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "Uint8List"
	case ir.KindBool:
		return "bool"
	case ir.KindFP32, ir.KindFP64:
		return "double"
	case ir.KindEnum, ir.KindBitfield:
		return "int"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return "List<" + g.dartArrayElemType(items.Elem, items.ElemRef, items.ElemItems) + ">"
	default: // numeric integer
		return "int"
	}
}

// ---- field initializers (Dart requires non-nullable fields be initialized) --

// dartInit returns the " = <expr>" initializer for a field, materializing its
// schema default (or the type-zero). Every field is initialized so a decoded-
// from-omitted field reconstructs its default (sparse-canonical, MESSAGE_SPEC S2)
// and marshal can compare against the same value.
func (g *gen) dartInit(f *ir.Field) string {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return " = " + g.typeName(f.Ref.Key) + "()"
	case ir.KindArray:
		if lit, ok := g.dartArrayLiteral(f); ok {
			return " = " + lit
		}
		return " = <" + g.dartArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">[]"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return " = " + dartStringLit(s)
		}
		return ` = ''`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil && len(raw) > 0 {
				return fmt.Sprintf(" = Uint8List.fromList(<int>[%s])", byteList(raw))
			}
		}
		return " = Uint8List(0)"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return " = true"
		}
		return " = false"
	case ir.KindFP32, ir.KindFP64:
		if f.Default != nil {
			return " = " + floatLit(f.Default)
		}
		return " = 0.0"
	case ir.KindEnum:
		if f.Default != nil {
			return " = " + scalarLit(f.Default)
		}
		return " = 0"
	case ir.KindBitfield:
		if bits := g.bitfieldDefault(f); bits != 0 {
			return " = " + intLitFromU64(bits)
		}
		return " = 0"
	default: // integers
		if f.Default != nil {
			return " = " + scalarLit(f.Default)
		}
		return " = 0"
	}
}

// dartDefaultValue is the value a scalar/string/enum/bitfield field is compared
// against for omission on marshal — exactly its initializer's RHS.
func (g *gen) dartDefaultValue(f *ir.Field) string {
	init := g.dartInit(f)
	return strings.TrimPrefix(init, " = ")
}

// dartArrayLiteral renders a native scalar array field's schema default (padded
// to `count` with the element default for a fixed-count array) as a Dart list
// literal; ("", false) for a wrapper-sequence array or a count-less array with
// no default. A `count: N` array is fixed-length, so with no schema default it
// still materializes N element defaults (MESSAGE_SPEC S3), matching the
// fixed-storage camp's zero-filled [T; N].
func (g *gen) dartArrayLiteral(f *ir.Field) (string, bool) {
	if !nativeArrayElem(f.Elem) {
		return "", false
	}
	et := g.dartArrayElemType(f.Elem, f.ElemRef, f.ElemItems)
	vals, ok := f.Default.([]any)
	if !ok {
		if f.Default == nil && f.HasCount {
			zero := elemZero(f.Elem)
			parts := make([]string, f.Count)
			for i := range parts {
				parts[i] = zero
			}
			return fmt.Sprintf("<%s>[%s]", et, strings.Join(parts, ", ")), true
		}
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = g.elemLit(f.Elem, v)
	}
	parts = g.tailPadLiteral(f, parts)
	return fmt.Sprintf("<%s>[%s]", et, strings.Join(parts, ", ")), true
}

// tailPadLiteral extends a `count: N` array's schema default to exactly N
// elements with the element default (MESSAGE_SPEC S3). Dynamic arrays keep the
// default exactly as written.
func (g *gen) tailPadLiteral(f *ir.Field, parts []string) []string {
	if !f.HasCount {
		return parts
	}
	zero := elemZero(f.Elem)
	for int64(len(parts)) < f.Count {
		parts = append(parts, zero)
	}
	return parts
}

// elemLit renders one native-array element value as a Dart literal.
func (g *gen) elemLit(elem ir.Kind, v any) string {
	switch elem {
	case ir.KindBool:
		if b, ok := v.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindFP32, ir.KindFP64:
		return floatLit(v)
	default: // integer / enum / bitfield
		return scalarLit(v)
	}
}

// elemZero is the Dart zero literal for a native-array element kind.
func elemZero(elem ir.Kind) string {
	switch elem {
	case ir.KindBool:
		return "false"
	case ir.KindFP32, ir.KindFP64:
		return "0.0"
	default:
		return "0"
	}
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

// ---- literals -------------------------------------------------------------

// scalarLit renders an integer/enum/bitfield default as a valid Dart int
// literal. Dart's `int` is signed 64-bit and a decimal literal outside
// [-(2^63-1), 2^63-1] is a compile error (both a u64 >= 2^63 and int64 min), so
// a value is emitted as its 64-bit bit pattern: the signed-decimal form (a u64
// like 2^64-1 becomes -1, which writeUnsigned re-expands to the same bits), or a
// hex literal for int64 min, which no decimal form can express.
func scalarLit(v any) string {
	if s, ok := v.(string); ok {
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			if _, err2 := strconv.ParseUint(s, 10, 64); err2 != nil {
				return s // non-numeric fallback (defensive; enum values are ints)
			}
		}
	}
	return intLitFromU64(toBits(v))
}

// intLitFromU64 renders a 64-bit bit pattern as a Dart int literal (see scalarLit).
func intLitFromU64(bits uint64) string {
	i := int64(bits)
	if i == math.MinInt64 {
		return "0x8000000000000000"
	}
	return strconv.FormatInt(i, 10)
}

func toBits(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case int64:
		return uint64(x)
	case int:
		return uint64(int64(x))
	case float64:
		return uint64(int64(x))
	case string:
		if i, err := strconv.ParseInt(x, 10, 64); err == nil {
			return uint64(i)
		}
		if u, err := strconv.ParseUint(x, 10, 64); err == nil {
			return u
		}
	}
	return 0
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

// dartStringLit renders a Go string as a single-quoted Dart string literal,
// escaping the characters that would break it. UTF-8 passes through verbatim.
func dartStringLit(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '$':
			b.WriteString(`\$`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func byteList(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ", ")
}

// ---- enum / bitfield backing (for constant values only) -------------------

// ---- array element classification -----------------------------------------

// unsignedArrayElem: array elements delivered through the unsigned wire type.
func unsignedArrayElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64 ||
		k == ir.KindBool || k == ir.KindBitfield
}

// signedArrayElem: array elements delivered through the signed wire type.
func signedArrayElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64 ||
		k == ir.KindEnum
}

// nativeArrayElem: an array element that encodes as a native array wire type
// (numeric/enum/boolean/bitfield) rather than a wrapper sequence.
func nativeArrayElem(k ir.Kind) bool {
	return unsignedArrayElem(k) || signedArrayElem(k) || k == ir.KindFP32 || k == ir.KindFP64
}

// seqArrayElem: an array element that lowers to a wrapper sequence
// (string/blob/struct/union, or a nested array).
func seqArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindStruct, ir.KindUnion, ir.KindArray:
		return true
	}
	return false
}

// ---- max-size cost model (mirrors the C#/Go analytic bound) ---------------

func (g *gen) maxSize(fields []*ir.Field) int64 {
	var total int64
	seen := map[string]bool{}
	for _, f := range fields {
		c, ok := g.fieldCost(f, seen)
		if !ok {
			return 8192
		}
		total += c
	}
	if total < 64 {
		total = 64
	}
	return total
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
			if !f.ElemMaxHas || !f.HasCount {
				return 0, false
			}
			per := varintLen(uint64(f.Count)<<3|7) + varintLen(uint64(f.ElemMax)<<3) + f.ElemMax
			return hdr + 1 + f.Count*per + 1, true
		case ir.KindStruct, ir.KindUnion, ir.KindArray:
			return 0, false
		default:
			if !f.HasCount {
				return 0, false
			}
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
