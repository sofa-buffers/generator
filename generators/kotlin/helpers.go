package kotlin

import (
	"encoding/base64"
	"fmt"
	"strconv"
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

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

// ktHardKeywords are the Kotlin *hard* keywords: the ones that can never appear
// where an identifier is expected. Kotlin has a real escape (backticks), so a
// colliding field name is ESCAPED rather than mangled -- the identifier stays
// the schema's, which keeps the JSON key and the generated member spelled the
// same (ARCHITECTURE §8, "escape where the language allows"). Soft keywords
// (`by`, `where`, `data`, ...) are legal identifiers already and are left alone.
var ktHardKeywords = map[string]bool{
	"as": true, "break": true, "class": true, "continue": true, "do": true,
	"else": true, "false": true, "for": true, "fun": true, "if": true,
	"in": true, "interface": true, "is": true, "null": true, "object": true,
	"package": true, "return": true, "super": true, "this": true, "throw": true,
	"true": true, "try": true, "typealias": true, "typeof": true, "val": true,
	"var": true, "when": true, "while": true,
}

// ktReservedMembers are the names the generated class already gives a member.
// A schema field with one of these names would redeclare it, so it is mangled
// with a trailing underscore -- a backtick escape cannot help here, because the
// clash is with another DECLARATION rather than with the grammar. The JSON key
// keeps the schema name (see the harness, which emits fld.Name).
var ktReservedMembers = map[string]bool{
	"serialize": true, "isDefault": true, "reset": true, "encode": true,
	"encodeTo": true, "decode": true, "tryDecode": true, "decoder": true,
	"MAX_SIZE": true, "MAX_SIZE_LIMIT": true,
}

// ktIdent renders a schema field name as a Kotlin member identifier: escaped
// with backticks when it is a hard keyword, suffixed when it would collide with
// a generated member, and otherwise passed through unchanged. The wire is
// unaffected (fields are keyed by id) and the JSON name stays the schema's.
func ktIdent(name string) string {
	if ktReservedMembers[name] {
		return name + "_"
	}
	if ktHardKeywords[name] {
		return "`" + name + "`"
	}
	return name
}

// exported upper-camels a schema name for a generated type.
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

// typeName is the generated Kotlin type name for a shared IR type key
// ("struct/Point" -> "Point", "message_field_elem" -> "MessageFieldElem").
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

// ---------------------------------------------------------------------------
// Type mapping
// ---------------------------------------------------------------------------

// ktScalarType is the Kotlin type a leaf kind lowers to.
//
// Every integer maps to its exact declared width, unsigned included
// (ARCHITECTURE §8, "pick the narrowest correct type"): `u8` is a `UByte`, not a
// widened `Long` the caller has to mask. That is the C# position rather than
// Java's, and Java's reason for widening -- no unsigned type -- does not apply
// here. Kotlin's unsigned types are inline value classes, so a scalar member
// costs the same one machine word its signed peer does and nothing is boxed on
// the hot path; see arrayWriteCall for what that buys at the corelib boundary.
//
// enum and bitfield are the two kinds whose width belongs to the NAMED TYPE
// rather than to the field, and both are pinned at the width that cannot lose a
// legal value rather than at the narrowest one that fits the declared members:
//
//   - `enum` -> `Int`. MESSAGE_SPEC §1 bounds an enum by the SIGNED 32-BIT
//     range, so `Int` holds every value the schema can declare, exactly. A
//     narrower backing (a `Byte` for a two-member enum) would have to truncate a
//     wire value outside it, and silent truncation is the one answer §7.1 rules
//     out everywhere it speaks.
//   - `bitfield` -> `ULong`. Flag positions run 0..63 and the wire word is an
//     unsigned varint, so `ULong` IS the domain; there is no value to lose.
func ktScalarType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "UByte"
	case ir.KindU16:
		return "UShort"
	case ir.KindU32:
		return "UInt"
	case ir.KindU64:
		return "ULong"
	case ir.KindI8:
		return "Byte"
	case ir.KindI16:
		return "Short"
	case ir.KindI32:
		return "Int"
	case ir.KindI64:
		return "Long"
	case ir.KindFP32:
		return "Float"
	case ir.KindFP64:
		return "Double"
	case ir.KindBool:
		return "Boolean"
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "ByteArray"
	case ir.KindEnum:
		return "Int"
	case ir.KindBitfield:
		return "ULong"
	}
	return "Any"
}

// primArrayType is the Kotlin primitive-array type a NATIVE array element kind
// lowers to. Every native element kind has one -- including `boolean`
// (`BooleanArray`) -- so a native array never boxes, which is the whole reason
// this target maps them at all.
func primArrayType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "UByteArray"
	case ir.KindU16:
		return "UShortArray"
	case ir.KindU32:
		return "UIntArray"
	case ir.KindU64:
		return "ULongArray"
	case ir.KindI8:
		return "ByteArray"
	case ir.KindI16:
		return "ShortArray"
	case ir.KindI32:
		return "IntArray"
	case ir.KindI64:
		return "LongArray"
	case ir.KindFP32:
		return "FloatArray"
	case ir.KindFP64:
		return "DoubleArray"
	case ir.KindBool:
		return "BooleanArray"
	case ir.KindEnum:
		return "IntArray"
	case ir.KindBitfield:
		return "ULongArray"
	}
	return ""
}

// primBaseOrder is every primitive array a field can be backed by, in a fixed
// order so the emitted row cursors come out stable.
var primBaseOrder = []string{
	"UByteArray", "UShortArray", "UIntArray", "ULongArray",
	"ByteArray", "ShortArray", "IntArray", "LongArray",
	"FloatArray", "DoubleArray", "BooleanArray",
}

// baseSuffix is the disambiguating suffix a per-array-type name carries.
// Kotlin's unsigned arrays are inline classes over their signed peers, so
// `UByteArray` and `ByteArray` share a JVM erasure and two same-named members
// would clash; naming them apart is what lets the corelib's Seq offer one per
// element type at all, and it keeps the emitted row cursors readable.
func baseSuffix(arrType string) string { return strings.TrimSuffix(arrType, "Array") }

// seqSuffix is the element-name suffix Seq's per-array-type members carry. The
// corelib spells them plural -- `reserveRowBytes`, `EMPTY_BYTES` -- because
// each names a row or an array of that element rather than one of it.
func seqSuffix(arrType string) string { return baseSuffix(arrType) + "s" }

// emptyArrayExpr is the corelib's shared zero-length constant for a primitive
// array type.
func emptyArrayExpr(arrType string) string {
	return "Seq.EMPTY_" + strings.ToUpper(seqSuffix(arrType))
}

// nativeArrayElem reports whether an array element is carried by the native
// array wire type (numeric/enum/boolean/bitfield) rather than a wrapper
// sequence.
func nativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindEnum, ir.KindBool, ir.KindBitfield:
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

// ktType is the declared type of a field's generated member.
func (g *gen) ktType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return g.ktArrayType(f.Elem, f.ElemRef, f.ElemItems)
	}
	return ktScalarType(f.Kind)
}

// ktArrayType is the container an array lowers to: a Kotlin primitive array for
// a native element kind, a MutableList for a wrapper-sequence one.
func (g *gen) ktArrayType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	if nativeArrayElem(elem) {
		return primArrayType(elem)
	}
	return "MutableList<" + g.ktArrayElemType(elem, ref, items) + ">"
}

// ktArrayElemType is the element type stored in a wrapper array's MutableList.
// A nested array whose OWN elements are native is a primitive array, not a list
// of boxed values -- the same rule ktArrayType applies one level up, and for the
// same reason (the boxed form is the hot allocator).
func (g *gen) ktArrayElemType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "ByteArray"
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return g.ktArrayType(items.Elem, items.ElemRef, items.ElemItems)
	}
	return ktScalarType(elem)
}

// ---------------------------------------------------------------------------
// Conversions between the corelib's delivery/argument types and the members
// ---------------------------------------------------------------------------

// toWireUnsigned renders the expression handed to OStream.writeUnsigned for a
// member of kind k. u64 and bitfield take the ULong overload; the narrower
// unsigned widths zero-extend into a Long, which is exact.
func toWireUnsigned(k ir.Kind, expr string) string {
	switch k {
	case ir.KindU64, ir.KindBitfield:
		return expr // the ULong overload
	}
	return expr + ".toLong()"
}

// toWireSigned renders the expression handed to OStream.writeSigned.
func toWireSigned(k ir.Kind, expr string) string {
	if k == ir.KindI64 {
		return expr
	}
	return expr + ".toLong()"
}

// fromWire narrows a value the corelib delivered (a Long for the integer
// callbacks) into the member's declared type. The §7.1 width guard runs FIRST
// and rejects anything this conversion would lose, so it only ever drops bits
// already proven to be sign extension or zero.
func fromWire(k ir.Kind, expr string) string {
	switch k {
	case ir.KindU8:
		return expr + ".toUByte()"
	case ir.KindU16:
		return expr + ".toUShort()"
	case ir.KindU32:
		return expr + ".toUInt()"
	case ir.KindU64, ir.KindBitfield:
		return expr + ".toULong()"
	case ir.KindI8:
		return expr + ".toByte()"
	case ir.KindI16:
		return expr + ".toShort()"
	case ir.KindI32, ir.KindEnum:
		return expr + ".toInt()"
	case ir.KindI64:
		return expr
	case ir.KindBool:
		return expr + " != 0L"
	}
	return expr
}

// arrayWriteCall renders the OStream call that writes a native array `val` at
// `idExpr`.
//
// This is where holding the exact declared width comes out free rather than
// merely correct: Kotlin's unsigned arrays are inline classes over their signed
// peers, so `asByteArray()` and friends are a reinterpretation, not a
// conversion. The corelib's `writeArrayUnsigned(ByteArray)` receives the very
// same backing array a `UByteArray` field holds -- no copy, no per-element
// widening -- and the bulk decode offer hands that same view back as the
// destination, so neither direction pays for the narrow type.
func arrayWriteCall(elem ir.Kind, idExpr, val string) string {
	switch elem {
	case ir.KindU8:
		return fmt.Sprintf("os.writeArrayUnsigned(%s, %s.asByteArray())", idExpr, val)
	case ir.KindU16:
		return fmt.Sprintf("os.writeArrayUnsigned(%s, %s.asShortArray())", idExpr, val)
	case ir.KindU32:
		return fmt.Sprintf("os.writeArrayUnsigned(%s, %s.asIntArray())", idExpr, val)
	case ir.KindU64, ir.KindBitfield:
		return fmt.Sprintf("os.writeArrayUnsigned(%s, %s.asLongArray())", idExpr, val)
	case ir.KindI8:
		return fmt.Sprintf("os.writeArraySigned(%s, %s)", idExpr, val)
	case ir.KindI16:
		return fmt.Sprintf("os.writeArraySigned(%s, %s)", idExpr, val)
	case ir.KindI32, ir.KindEnum:
		return fmt.Sprintf("os.writeArraySigned(%s, %s)", idExpr, val)
	case ir.KindI64:
		return fmt.Sprintf("os.writeArraySigned(%s, %s)", idExpr, val)
	case ir.KindFP32:
		return fmt.Sprintf("os.writeArrayFp32(%s, %s)", idExpr, val)
	case ir.KindFP64:
		return fmt.Sprintf("os.writeArrayFp64(%s, %s)", idExpr, val)
	case ir.KindBool:
		// The one native element with no OStream overload of its own: a boolean
		// array is a `0`/`1` unsigned array on the wire (MESSAGE_SPEC §3), so it
		// is materialised as bytes for the write.
		return fmt.Sprintf("os.writeArrayUnsigned(%s, Seq.boolsToBytes(%s))", idExpr, val)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// ktDefaultValue is the literal a field's member is initialised to, and the very
// expression its omit-compare tests against. A field left untouched therefore
// always compares equal and is omitted (MESSAGE_SPEC §2).
func (g *gen) ktDefaultValue(f *ir.Field) string {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key) + "()"
	case ir.KindArray:
		if nativeArrayElem(f.Elem) {
			if lit, ok := g.ktPrimArrayLiteral(f); ok {
				return lit
			}
			return emptyArrayExpr(primArrayType(f.Elem))
		}
		// A wrapper array starts EMPTY, with or without a declared `count: N`:
		// `count` is a capacity, not a length (MESSAGE_SPEC §3), so a fresh
		// count:N array holds no elements and an absent field decodes back to
		// none.
		return "mutableListOf()"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return ktStringLit(s)
		}
		return `""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil && len(raw) > 0 {
				return "byteArrayOf(" + ktBytes(raw) + ")"
			}
		}
		return "Seq.EMPTY_BYTES"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindFP32:
		if f.Default != nil {
			return floatLit(f.Default) + "f"
		}
		return "0.0f"
	case ir.KindFP64:
		if f.Default != nil {
			return floatLit(f.Default)
		}
		return "0.0"
	case ir.KindBitfield:
		return fmt.Sprintf("%duL", g.bitfieldDefault(f))
	}
	// Integers and enum.
	if f.Default != nil {
		return ktIntLit(f.Kind, scalarLit(f.Default))
	}
	return ktIntLit(f.Kind, "0")
}

// ktIntLit renders an integer literal at the member's declared Kotlin type: the
// `u` suffix for an unsigned width, `L` for the 64-bit ones, and a plain literal
// where the declared type already is `Int`. A narrow width takes an explicit
// conversion, because Kotlin has no implicit narrowing from an `Int` literal to
// a `Byte`/`UByte` member.
func ktIntLit(k ir.Kind, v string) string {
	switch k {
	case ir.KindU8:
		return v + "u.toUByte()"
	case ir.KindU16:
		return v + "u.toUShort()"
	case ir.KindU32:
		return v + "u"
	case ir.KindU64:
		return v + "uL"
	case ir.KindI8:
		return "(" + v + ").toByte()"
	case ir.KindI16:
		return "(" + v + ").toShort()"
	case ir.KindI32, ir.KindEnum:
		return v
	case ir.KindI64:
		// Kotlin has no literal for Long.MIN_VALUE: the digits alone are one
		// past Long.MAX and the unary minus is applied afterwards, so the
		// literal is rejected before the sign reaches it.
		if v == "-9223372036854775808" {
			return "Long.MIN_VALUE"
		}
		return v + "L"
	case ir.KindBitfield:
		return v + "uL"
	}
	return v
}

// ktPrimArrayLiteral renders a native array field's schema default as the
// matching Kotlin primitive-array literal, exactly as the schema wrote it.
// ("", false) when there is no default.
//
// A declared `count: N` contributes nothing: `count` is a CAPACITY, not a
// length (MESSAGE_SPEC §3), so a short default is NOT tail-padded to N and a
// count:N array with no default has no literal at all -- its value is the empty
// array, which is also what an absent field decodes back to.
func (g *gen) ktPrimArrayLiteral(f *ir.Field) (string, bool) {
	if !nativeArrayElem(f.Elem) {
		return "", false
	}
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	if len(vals) == 0 {
		return emptyArrayExpr(primArrayType(f.Elem)), true
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = ktArrayElemLit(f.Elem, v)
	}
	return primArrayCtor(f.Elem) + "(" + strings.Join(parts, ", ") + ")", true
}

// primArrayCtor is the `xArrayOf` factory for a native element kind. Kotlin
// spells the unsigned ones all-lowercase (`ubyteArrayOf`, not `uByteArrayOf`),
// so the whole element prefix is lowered rather than just its first letter.
func primArrayCtor(k ir.Kind) string {
	t := primArrayType(k)
	return strings.ToLower(baseSuffix(t)) + "ArrayOf"
}

// ktArrayElemLit renders one native array element default at the array's
// element type.
func ktArrayElemLit(elem ir.Kind, v any) string {
	switch elem {
	case ir.KindBool:
		if b, ok := v.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindFP32:
		return floatLit(v) + "f"
	case ir.KindFP64:
		return floatLit(v)
	}
	return ktIntLit(elem, scalarLit(v))
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
	s := strconv.FormatFloat(fv, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// ktBytes renders a blob default as Kotlin byte literals. A byte above 127 is
// written as its signed two's-complement value, because Kotlin's `Byte` is
// signed and an out-of-range literal is a compile error.
func ktBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%d", int8(x))
	}
	return strings.Join(parts, ", ")
}

// ktStringLit renders a Kotlin string literal. `$` starts a template in Kotlin,
// so it is escaped alongside the usual suspects; every other byte (UTF-8
// included) passes through verbatim, which is what keeps a user's default text
// byte-for-byte what the schema wrote.
func ktStringLit(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '$':
			b.WriteString(`\$`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------------------
// Omit conditions (MESSAGE_SPEC §2)
// ---------------------------------------------------------------------------

// ktWritesExpr is the boolean expression "serialize would put this field on the
// wire" -- literally the write guard emitMarshal emits for the same field.
// isDefault() is built from it rather than from a hand-written "equals its
// default" twin so the two cannot state different truth tables: the object is
// default exactly when no arm fires.
func (g *gen) ktWritesExpr(f *ir.Field) string {
	acc := "this." + ktIdent(f.Name)
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed, so the frame survives iff the nested serialize wrote a
		// child -- exactly "the nested object is not default".
		return "!" + acc + ".isDefault()"
	case ir.KindArray:
		if nativeArrayElem(f.Elem) {
			if _, ok := g.arrayCompareDefault(f); ok {
				return "!" + acc + ".contentEquals(" + arrDefName(f) + ")"
			}
			// With no declared default the compare degenerates to a length test,
			// sparing a contentEquals against a shared empty array per call.
			return acc + ".isNotEmpty()"
		}
		// A wrapper array puts something on the wire iff it holds an element at
		// all: the LAST element is written whatever its value (§2), so "no child
		// is written" is exactly "the array is empty".
		return acc + ".isNotEmpty()"
	case ir.KindString:
		if def := g.ktDefaultValue(f); def == `""` {
			return acc + ".isNotEmpty()"
		}
		return acc + " != " + g.ktDefaultValue(f)
	case ir.KindBlob:
		if _, ok := g.arrayCompareDefault(f); !ok {
			return acc + ".isNotEmpty()"
		}
		return "!" + acc + ".contentEquals(" + arrDefName(f) + ")"
	}
	return acc + " != " + g.ktDefaultValue(f)
}

// arrDefName is the companion constant holding a field's omit-compare default.
// Hoisting matters because the comparison only ever READS the value --
// rebuilding the literal on every serialize() call would allocate per encode.
func arrDefName(f *ir.Field) string { return "_arrdef_" + f.Name }

// arrayCompareDefault is the literal a field's value is compared against for
// whole-field omission, and ("", false) when the field has no materialised
// default (a dynamic array with no schema default, or a wrapper-sequence array,
// which is never whole-omitted).
func (g *gen) arrayCompareDefault(f *ir.Field) (string, bool) {
	if f.Kind == ir.KindBlob {
		def := g.ktDefaultValue(f)
		if def == "Seq.EMPTY_BYTES" {
			return "", false
		}
		return def, true
	}
	if f.Kind != ir.KindArray || !nativeArrayElem(f.Elem) {
		return "", false
	}
	lit, ok := g.ktPrimArrayLiteral(f)
	if !ok || lit == emptyArrayExpr(primArrayType(f.Elem)) {
		return "", false
	}
	return lit, true
}

// ---------------------------------------------------------------------------
// Reachability
// ---------------------------------------------------------------------------

// reachable returns named-type keys used by m in post-order (children first).
// Both scalar refs (f.Ref) and composite array element refs (f.ElemRef / nested
// f.ElemItems) are followed so array-of-struct/union/enum element types are
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

func itoa(i int) string     { return strconv.Itoa(i) }
func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
