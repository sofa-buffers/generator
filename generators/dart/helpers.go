package dart

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// ---- config helpers -------------------------------------------------------

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
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
	// The encoder parameter of serialize/encodeTo: a field named `e` would shadow
	// it inside those bodies (the field's own read then names the encoder).
	"e": true,
}

// fp32BitsField is the companion `int?` holding the raw 32 wire bits of an fp32
// SCALAR field whose decoded value is a NaN, so a signaling/payload NaN
// re-encodes bit-for-bit — a Dart `double` cannot carry an fp32 NaN payload
// (MESSAGE_SPEC §4.6). null means "no captured bits; derive from the double".
//
// PUBLIC, and that is the whole point (generator#275 / Crucible F-0049).
// CORELIB_PLAN §6.5 requires a double-only target to provide the raw-wire path
// "for bit-exact CONSUMERS" — a transcoder, a comparator, a materialized walk —
// not merely for the type's own re-encode. Dart privacy is per LIBRARY, so a
// leading underscore put the bits out of reach of every consumer outside the
// generated file: the round-trip stayed bit-exact (which is why a
// round-trip-only test never saw it) while any external walk got the widened
// double, whose quiet bit is already set and whose signaling NaN is therefore
// unrecoverable.
//
// The typescript backend — same language class, same corelib support — has
// always exposed its equivalent (`<name>Fp32Raw`) as a public field. Matching
// that also keeps the ENCODE side reachable: a caller who wants to emit a
// signaling NaN has no other way to say so, since the double cannot carry it.
func fp32BitsField(name string) string { return dartIdent(name) + "Fp32Bits" }

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
// (Dart has one 64-bit int), floats to `double`, composites to their generated
// class. Every field whose payload the codec writes in place -- a string, a blob,
// a native array -- is one of corelib-dart's `Inline…` destinations: storage of a
// fixed capacity plus the length in use (CORELIB_PLAN §6.6.3), which the codec
// fills at the field header without a copy, a view or a per-message allocation.
// A wrapper array is a List of its elements' types.
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
		return "sofab.InlineString"
	case ir.KindBlob:
		return "sofab.InlineBytes"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		if nativeArrayElem(f.Elem) {
			return inlineArrayType(f.Elem)
		}
		return "List<" + g.dartArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">"
	}
	return "Object?"
}

// isDest reports whether a field is held in an `Inline…` destination: a
// string, a blob or a native array. Such a field is `final` -- the codec writes
// into its storage, and a caller changes the value through `assign` rather than
// by replacing the object.
func isDest(f *ir.Field) bool {
	return f.Kind == ir.KindString || f.Kind == ir.KindBlob ||
		(f.Kind == ir.KindArray && nativeArrayElem(f.Elem))
}

// inlineArrayType is the destination type of a native array (or matrix row) of
// `elem`: every integer kind -- bool, enum and bitfield included -- decodes to
// 64-bit elements, which is what both integer array wire types carry (§4.7).
func inlineArrayType(elem ir.Kind) string {
	switch elem {
	case ir.KindFP32:
		return "sofab.InlineFloat32Array"
	case ir.KindFP64:
		return "sofab.InlineFloat64Array"
	}
	return "sofab.InlineInt64Array"
}

// dartArrayElemType is the Dart type of a WRAPPER array's element, recursing for
// nested arrays. A string/blob element and a native matrix row are destinations
// the corelib collectors decode in place.
func (g *gen) dartArrayElemType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "sofab.InlineString"
	case ir.KindBlob:
		return "sofab.InlineBytes"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		if nativeArrayElem(items.Elem) {
			return inlineArrayType(items.Elem)
		}
		return "List<" + g.dartArrayElemType(items.Elem, items.ElemRef, items.ElemItems) + ">"
	}
	return inlineArrayType(elem)
}

// ---- field initializers (Dart requires non-nullable fields be initialized) --

// dartInit returns the " = <expr>" initializer for a field, materializing its
// schema default (or the type-zero). Every field is initialized so a decoded-
// from-omitted field reconstructs its default (sparse-canonical, MESSAGE_SPEC S2)
// and marshal can compare against the same value.
//
// A destination is sized ONCE, here, to the schema bound -- the storage every
// later decode into this object reuses (see initialCap for the exceptions).
func (g *gen) dartInit(f *ir.Field) string {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return " = " + g.typeName(f.Ref.Key) + "()"
	case ir.KindArray:
		if nativeArrayElem(f.Elem) {
			ctor := fmt.Sprintf("%s(%d%s)", inlineArrayType(f.Elem), initialCap(f), g.rangeArg(f.Elem, f.ElemRef))
			if def, ok := defaultRef(f); ok {
				return fmt.Sprintf(" = %s..assign(%s)", ctor, def)
			}
			return " = " + ctor
		}
		return " = <" + g.dartArrayElemType(f.Elem, f.ElemRef, f.ElemItems) + ">[]"
	case ir.KindString, ir.KindBlob:
		ctor := fmt.Sprintf("%s(%d)", g.dartType(f), initialCap(f))
		if def, ok := defaultRef(f); ok {
			return fmt.Sprintf(" = %s..assign(%s)", ctor, def)
		}
		return " = " + ctor
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

// eagerDestBytes is the largest destination sized to its schema bound when the
// object is built. Up to it, the one allocation at construction is the whole
// cost: the header call just hands the storage over, and a reused object never
// allocates again. Past it, sizing up front would make every fresh object --
// every one-shot decode() -- zero the schema MAXIMUM whether the field is on the
// wire or not (an `array<i32, count: 100000>` is 800 KB), so such a field starts
// empty like an unbounded one and is grown to the count at its header.
const eagerDestBytes = 1024

// destBound is a destination field's schema bound -- `maxlen` for a string or
// blob, `count` for a native array -- and whether the schema declares one.
func destBound(f *ir.Field) (int64, bool) {
	if f.Kind == ir.KindArray {
		return f.Count, f.HasCount
	}
	return f.Maxlen, f.HasMaxlen
}

// destElemBytes is the storage one element of a destination field takes.
func destElemBytes(f *ir.Field) int64 {
	switch {
	case f.Kind != ir.KindArray: // string / blob bytes
		return 1
	case f.Elem == ir.KindFP32:
		return 4
	}
	return 8 // Int64List / Float64List
}

// eagerDest reports whether a destination is sized to its schema bound at
// construction (see eagerDestBytes); false for an unbounded field and for one
// whose bound is too large to pay for on every fresh object.
func eagerDest(f *ir.Field) bool {
	n, ok := destBound(f)
	return ok && n*destElemBytes(f) <= eagerDestBytes
}

// initialCap is a destination's capacity at construction: its schema bound when
// eagerDest, else 0 -- grown at the header, once the bound has been checked.
func initialCap(f *ir.Field) int64 {
	if eagerDest(f) {
		n, _ := destBound(f)
		return n
	}
	return 0
}

// rangeArg is the `range:` argument of an integer array destination: the
// declared element width (or the one an enum/bitfield implies, MESSAGE_SPEC §1),
// which the codec applies to every element as it is decoded -- on both decode
// surfaces, and ahead of a truncated tail (§5.2, generator#267). "" where no
// interval narrows the 64-bit element.
func (g *gen) rangeArg(elem ir.Kind, ref *ir.TypeRef) string {
	lo, hi, ok := elemRange(elem, ref)
	if !ok {
		return ""
	}
	return fmt.Sprintf(", range: const sofab.ElemRange(%d, %d)", lo, hi)
}

// dartDefaultValue is the value a scalar/enum/bitfield field is compared
// against for omission on marshal -- exactly its initializer's RHS.
func (g *gen) dartDefaultValue(f *ir.Field) string {
	init := g.dartInit(f)
	return strings.TrimPrefix(init, " = ")
}

// defaultRef names the class-level typed list (defaultDecl) holding the declared
// default of a destination field -- the one value its storage is filled from at
// construction and on reset(), and compared against for omission. ("", false)
// when no non-empty default is declared: an empty default is the destination's
// zero state already.
//
// A typed list rather than a `const <int>[]` literal, because that is what makes
// the fill cheap: `assign` copies it with setRange, which is a memmove between
// two typed lists of one element type and an element-by-element walk from a
// plain List -- once per defaulted field of every object built, the bench row's
// struct-array elements included.
func defaultRef(f *ir.Field) (string, bool) {
	if !hasDestDefault(f) {
		return "", false
	}
	return "_" + dartIdent(f.Name) + "Default", true
}

// defaultDecl is the static declaration defaultRef names.
func (g *gen) defaultDecl(f *ir.Field) string {
	ref, _ := defaultRef(f)
	lit, _ := g.defaultLit(f)
	t := storageType(f)
	return fmt.Sprintf("static final %s %s = %s.fromList(%s);", t, ref, t, lit)
}

// hasDestDefault reports whether a destination field declares a non-empty
// default (see defaultLit).
func hasDestDefault(f *ir.Field) bool {
	switch f.Kind {
	case ir.KindString:
		s, ok := f.Default.(string)
		return ok && s != ""
	case ir.KindBlob:
		s, ok := f.Default.(string)
		if !ok {
			return false
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		return err == nil && len(raw) > 0
	case ir.KindArray:
		vals, ok := f.Default.([]any)
		return nativeArrayElem(f.Elem) && ok && len(vals) > 0
	}
	return false
}

// defaultLit renders the declared default of a destination field -- a string, a
// blob or a native array -- as the element literal defaultDecl builds its typed
// list from: the UTF-8 bytes of a string, the bytes of a blob, the elements of an
// array (a bool array's as 0/1, the integers it is stored as). ("", false)
// exactly where hasDestDefault is false.
//
// It is NOT padded to a declared `count: N`: that is a capacity, not a length
// (MESSAGE_SPEC §3), so the default stands exactly as written -- and so does the
// value it is compared against, which is what keeps a length-N all-zero array
// distinct from the empty one.
//
// An fp32 array's elements are written as the exact double of their fp32
// rounding (fp32Lit), so the literal says precisely what the Float32List built
// from it -- and every element decoded into the field -- holds.
func (g *gen) defaultLit(f *ir.Field) (string, bool) {
	switch f.Kind {
	case ir.KindString:
		s, ok := f.Default.(string)
		if !ok || s == "" {
			return "", false
		}
		return fmt.Sprintf("const <int>[%s]", byteList([]byte(s))), true
	case ir.KindBlob:
		s, ok := f.Default.(string)
		if !ok {
			return "", false
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil || len(raw) == 0 {
			return "", false
		}
		return fmt.Sprintf("const <int>[%s]", byteList(raw)), true
	case ir.KindArray:
		if !nativeArrayElem(f.Elem) {
			return "", false
		}
		vals, ok := f.Default.([]any)
		if !ok || len(vals) == 0 {
			return "", false
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = g.elemLit(f.Elem, v)
		}
		if f.Elem == ir.KindFP32 || f.Elem == ir.KindFP64 {
			return fmt.Sprintf("const <double>[%s]", strings.Join(parts, ", ")), true
		}
		return fmt.Sprintf("const <int>[%s]", strings.Join(parts, ", ")), true
	}
	return "", false
}

// elemLit renders one native-array element value as a Dart literal, as the
// destination stores it: a bool as 0/1.
func (g *gen) elemLit(elem ir.Kind, v any) string {
	switch elem {
	case ir.KindBool:
		if b, ok := v.(bool); ok && b {
			return "1"
		}
		return "0"
	case ir.KindFP32:
		return fp32Lit(v)
	case ir.KindFP64:
		return floatLit(v)
	default: // integer / enum / bitfield
		return scalarLit(v)
	}
}

// fp32Lit renders v rounded to fp32, as the exact double that rounding yields:
// what an InlineFloat32Array element reads back as, so the literal and the
// stored element compare equal.
func fp32Lit(v any) string {
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
	s := strconv.FormatFloat(float64(float32(fv)), 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
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
