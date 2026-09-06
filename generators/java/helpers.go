package java

import (
	"encoding/base64"
	"fmt"
	"math"
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
// It is used both to materialize the field default and, in serialize, as the RHS to
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
		// Symmetry with javaPrimElemLit only, and nothing reaches it: primitiveArrayElem
		// claims u64, so a u64 array is a long[] and never this boxed List<Long>.
		// Kept in step all the same, so that moving u64 onto the boxed path cannot
		// silently reintroduce the per-instance parse (generator#479).
		return javaU64Lit(scalarLit(v)) // Long.valueOf(long) autoboxes the literal
	case ir.KindBitfield:
		// Symmetry with javaPrimElemLit only, and nothing reaches it: this boxed
		// path is boolean-only today, because primitiveArrayElem claims bitfield
		// and every caller tests that predicate first. Kept correct so that moving
		// a bitfield array onto the boxed List<Long> path cannot silently
		// reintroduce the decimal literal javac rejects at bit 63 (generator#477).
		if bits, err := strconv.ParseUint(scalarLit(v), 10, 64); err == nil {
			return javaMaskLit(bits)
		}
		return scalarLit(v) + "L"
	default: // integers, enum -> Long
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
// the narrowest one that holds the declared width's BITS, float/double for the fp
// kinds, and long for the kinds with no declared narrow width (u64/i64, and enum
// and bitfield, whose width is the named type's business).
//
// A SCALAR field still maps to `long` — Java has no unsigned types, and widening
// one value costs nothing. An ARRAY is the case where it costs: at 8 bytes an
// element a u8[1000] is eight kilobytes of which seven are sign bits. The corelib
// has taken `byte[]`/`short[]`/`int[]`/`long[]` on writeArrayUnsigned and
// writeArraySigned from the start, zero-extending each element on the way out
// (`elem & 0xFF`, `& 0xFFFF`, `& 0xFFFFFFFF`); this is the mapping those overloads
// were written for.
//
// For a SIGNED width the narrowing is exact — an i8 is a Java byte. For an
// UNSIGNED one the Java primitive holds the declared width's RAW BITS, so a
// `u8` element of 200 reads back as -56 and the value is recovered with
// Byte.toUnsignedInt / Short.toUnsignedInt / Integer.toUnsignedLong. That is the
// same bargain protobuf-java strikes for `uint32`, and the only alternative that
// stays value-preserving is to widen every unsigned array one step (u8 -> short,
// u32 -> long), which gives up most of what the change is for.
func primArrayBase(k ir.Kind) string {
	switch k {
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	case ir.KindU8, ir.KindI8:
		return "byte"
	case ir.KindU16, ir.KindI16:
		return "short"
	case ir.KindU32, ir.KindI32:
		return "int"
	default: // u64, i64, enum, bitfield
		return "long"
	}
}

// emptyPrimFor is the corelib's shared zero-length constant for a primitive
// element base, referenced by every field initializer, reset() and gap fill: an
// empty array has no state, so materializing one per field is pure waste.
func emptyPrimFor(base string) string {
	switch base {
	case "float":
		return "Seq.EMPTY_FLOATS"
	case "double":
		return "Seq.EMPTY_DOUBLES"
	case "byte":
		return "Seq.EMPTY_BYTES"
	case "short":
		return "Seq.EMPTY_SHORTS"
	case "int":
		return "Seq.EMPTY_INTS"
	default:
		return "Seq.EMPTY_LONGS"
	}
}

// primArrayCast is the cast a decoded `long` needs before it is stored into a
// primitive array of `k`, empty when the element base is already long. The §7.1
// width guard runs FIRST and rejects anything the cast would lose, so the cast
// only ever drops bits that were already proven to be sign extension (signed) or
// zero (unsigned).
func primArrayCast(k ir.Kind) string {
	switch primArrayBase(k) {
	case "byte":
		return "(byte) "
	case "short":
		return "(short) "
	case "int":
		return "(int) "
	}
	return ""
}

// primArrayWiden turns a stored primitive array element back into the VALUE it
// stands for: a no-op for a signed width (the narrowing was exact) and a mask for
// an unsigned one (the storage holds raw bits). Used wherever an element leaves
// the field as a number rather than as wire bytes -- the JSON writer.
func primArrayWiden(k ir.Kind, expr string) string {
	switch k {
	case ir.KindU8:
		return "(" + expr + " & 0xFFL)"
	case ir.KindU16:
		return "(" + expr + " & 0xFFFFL)"
	case ir.KindU32:
		return "(" + expr + " & 0xFFFFFFFFL)"
	}
	return expr
}

// javaPrimArrayLiteral renders a primitive array field's schema default as a
// `new byte[]{...}` / `new int[]{...}` / `new double[]{...}` literal (whichever
// primArrayBase gives the element), exactly as the schema wrote it.
// ("", false) when there is no default.
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
		parts = append(parts, javaPrimElemLit(f.Elem, v))
	}
	return fmt.Sprintf("new %s[]{%s}", base, strings.Join(parts, ", ")), true
}

// javaPrimElemLit renders one element of such a literal: an fp value as written, a
// long-backed integer as a Java long literal, and a NARROWED integer as the
// declared width's bits in the narrower type. The schema states the value; what
// goes in the array is its representation, which for an unsigned width is the low
// bits (a u8 default of 200 is written `(byte) -56`, and reads back as 200 through
// Byte.toUnsignedInt) -- so the literal is emitted already reduced rather than as
// a cast expression a reader would have to evaluate.
func javaPrimElemLit(elem ir.Kind, v any) string {
	switch primArrayBase(elem) {
	case "float":
		return floatLit(v) + "f"
	case "double":
		return floatLit(v)
	case "long":
		if elem == ir.KindU64 {
			// Doubly worth a literal here: this initializer is per-INSTANCE, so the
			// parse it used to emit was a static call per element per object
			// constructed (generator#479) -- the same argument the bitfield arm
			// below already made for itself.
			return javaU64Lit(scalarLit(v))
		}
		if elem == ir.KindBitfield {
			// The same hole as the scalar default, one level in: an element mask
			// with bit 63 set has no decimal long literal, so
			// `new long[]{1L, 9223372036854775808L}` is "integer number too large"
			// and the class does not compile at all. Hex is doubly right here --
			// this initializer is per-INSTANCE, so Long.parseUnsignedLong would be
			// a static call per element per object constructed.
			if bits, err := strconv.ParseUint(scalarLit(v), 10, 64); err == nil {
				return javaMaskLit(bits)
			}
			// Not a mask the validator would have accepted; fall through and let
			// the compiler complain rather than emitting something invented.
		}
		return scalarLit(v) + "L"
	}
	// A narrowed width: reduce the schema's value to the bits the field holds.
	n, err := strconv.ParseInt(scalarLit(v), 10, 64)
	if err != nil {
		// Not an integer literal the schema validator would have accepted; leave it
		// to the compiler to complain rather than emitting something invented.
		return scalarLit(v)
	}
	switch primArrayBase(elem) {
	case "byte":
		return fmt.Sprintf("(byte) %d", int8(n))
	case "short":
		return fmt.Sprintf("(short) %d", int16(n))
	default:
		return fmt.Sprintf("%d", int32(n))
	}
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

// javaArrayElemType is the element type stored in an array's List<...>.
// Integers/enum/bitfield box to Long, boolean to Boolean, fp to Float/Double;
// struct/union use the class type; nested arrays recurse.
//
// A nested array whose OWN elements are primitive is a primitive array, not a
// List: `array<array<u16>>` is `List<long[]>`, not `List<List<Long>>`. This is
// the same rule primitiveArrayElem applies to a top-level array field, applied
// one level in -- and for the same reason, which is that the boxed form is the
// hot allocator. A row of N integers costs N Long objects on decode plus an
// unboxing temporary on encode; measured on vehicle_telemetry (four
// 8-element rows) that pair is ~9 % of decode and ~8 % of encode. A boolean row
// stays List<Boolean>: it has no primitive OStream overload, so it would have to
// be converted for the write either way.
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
		if primitiveArrayElem(items.Elem) {
			return primArrayBase(items.Elem) + "[]"
		}
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
		// an omitted default array reconstructs correctly and serialize can compare
		// against it. A composite array is a wrapper sequence whose declared default
		// is not materialized, which is what makes its dropping closer correct (§2).
		if primitiveArrayElem(f.Elem) {
			if lit, ok := g.javaPrimArrayLiteral(f); ok {
				return " = " + lit
			}
			return " = " + emptyPrimFor(primArrayBase(f.Elem))
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
		return " = Seq.EMPTY_BYTES"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return " = true"
		}
		return ""
	case ir.KindU64:
		// A u64 is carried in a SIGNED Java long, so the default has to be spelled
		// as a literal that survives the reuse of this one string at five sites, two
		// of them compares in the encode path -- see javaU64Lit. Decimal as written
		// up to 2^63-1, hex past it.
		if f.Default != nil {
			return " = " + javaU64Lit(scalarLit(f.Default))
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
		// A bitfield is an unsigned 64-bit mask carried in a SIGNED Java long, so
		// a mask with bit 63 set has no DECIMAL long literal: `9223372036854775808L`
		// is "integer number too large" and the class does not compile. A HEX long
		// literal has no such hole -- it spells every uint64 bit pattern, up to
		// 0xFFFFFFFFFFFFFFFFL -- and it stays a compile-time constant. That matters
		// because this one string is reused four times, twice of them in a hot path:
		// the field initializer, the `!= default` omission compare in serialize, the
		// same compare in isDefault, and reset(). Hex is also simply the right
		// spelling for a mask assembled from flag POSITIONS -- it shows which bits
		// are set, which no decimal does. The u64 arm above reaches the same place by
		// a different route (javaU64Lit): there the author DID write a decimal, so it
		// stands as written wherever javac accepts it, and only a value past 2^63-1
		// is re-spelled in hex.
		if bits := g.bitfieldDefault(f); bits != 0 {
			return " = " + javaMaskLit(bits)
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

// javaMaskLit spells a bitfield mask as a Java `long` literal. It is the one
// place that decision lives: javaInit renders a scalar bitfield default with it
// and javaPrimElemLit renders an array element with it, so a third site cannot
// drift from the first two -- generator#477 was exactly that drift, the scalar
// arm spelled the mask correctly while the array element in the same file did
// not.
//
// Hex is the spelling, for the reasons javaInit's arm sets out: it is the only
// one that covers every uint64 pattern in a SIGNED carrier, it stays a
// compile-time constant on a maxspeed target, and it shows which bits a mask
// assembled from flag POSITIONS actually sets.
//
// Every mask is re-spelled in hex, including an ARRAY element the author did
// write as a decimal (bitfields.yaml's `masks` row spells one element `1` and
// another as the quoted decimal "18446744073709551615"), and none of them gets
// the javadoc line javaWideU64Note gives a re-spelled u64. That is deliberate,
// and it is the one place this file re-spells without a note: hex is the RIGHT
// reading of a mask whatever base it was written in -- the value's meaning is
// which bits are set -- so the hex literal is not hiding the author's number,
// it is showing it better. A u64 default is the opposite case: there the
// decimal IS the meaning (a count, a limit, an id), so re-spelling it loses
// something and the javadoc pays it back.
func javaMaskLit(bits uint64) string {
	return fmt.Sprintf("0x%XL", bits)
}

// javaU64Lit spells a `u64` schema default as a Java `long` literal, and is the
// one place that decision lives -- javaInit renders the scalar default with it,
// javaPrimElemLit an element of a `long[]` default, javaArrayElemLit a boxed one.
//
// The whole point is that the result is a COMPILE-TIME CONSTANT, because the one
// string javaInit returns is reused at five sites, two of them compares and one of
// those in the encode path: the field initializer, the `!= default` omission
// compare in serialize(), the same compare in isDefault(), reset(), and -- for an
// array -- once per element in the per-instance `new long[]{...}`. The
// Long.parseUnsignedLong this used to emit is none of those things; javac renders
// it `ldc` + `invokestatic` where a literal is a bare `ldc2_w`, and C2 does not
// fold it away either (generator#479, measured at ~20 ns/op against ~0.9 for a
// literal, JDK 25.0.3).
//
// Two spellings, split at the top of a decimal long literal:
//
//   - Up to 2^63-1 the value is emitted as its own DECIMAL, which for every
//     well-formed schema default is the author's text unchanged (`default: 42`
//     gives `42L`). That is the common case, it is already what every other
//     integer kind emits one arm up in javaInit, and it keeps the author's number
//     visible in the generated source.
//   - Past it there IS no decimal long literal -- 9223372036854775808L is javac's
//     "integer number too large" -- so the value is spelled in HEX, through
//     javaMaskLit, which reads a 64-bit two's-complement pattern and so covers
//     every uint64 up to 0xFFFFFFFFFFFFFFFFL. Only here is the author's spelling
//     re-written, and javaWideU64Note puts the decimal back in the field's javadoc
//     so the generated source still says what the schema said.
//
// The decimal is RE-RENDERED from the parsed value (strconv.FormatUint), never
// echoed as the schema's raw text, and that is load-bearing rather than tidiness:
// Java reads a leading-zero integer literal as OCTAL, and `010L` is 8 to javac
// while `09L` is not a legal literal at all; Long.parseUnsignedLong("010") is 10,
// so echoing the schema's text would be a silent value change at the omission
// compare, isDefault and reset. FormatUint is free for every other input: `42` is
// still `42L`.
//
// Nothing VALIDATED can hand this function a non-decimal spelling any more.
// generator#484 closed both routes: checkArrayElem's u64/i64 arm used to accept
// any string at all with no format check (that is how "010" reached here, where
// the scalar path's decIntRe had always rejected it), and the validator used to
// accept an integral float64, so `default: 1e10` on a u64 arrived as "1e+10" from
// scalarLit's %v and the emitted `Long.parseUnsignedLong("1e+10")` compiled and
// then threw NumberFormatException in the constructor. Both are now refused in
// internal/parser, which is where deciding what a valid default is belongs -- it
// was never this function's business, and the same schema used to give cpp
// `std::uint64_t expo = 1e+10ULL;`, which does not compile either.
//
// The fallback to `Long.parseUnsignedLong(<raw>)` stays all the same, and the
// test that pins it now builds its IR through a validation-skipping helper. It is
// what keeps this function from inventing a literal for text it cannot parse, and
// it is the guard that made the "010" defect a wrong VALUE rather than a build
// failure for however long the validator was letting it through.
func javaU64Lit(dec string) string {
	lit, _ := javaU64Spelling(dec)
	return lit
}

// javaU64Spelling is the single owner of the 2^63-1 split: it returns the Java
// literal for a u64 schema default and whether that literal is the HEX form, i.e.
// whether the author's decimal has been re-spelled and so needs javaWideU64Note to
// put it back in the javadoc.
//
// Both halves come from here on purpose. The javadoc note used to re-derive the
// threshold with its own ParseUint comparison, which is the drift javaMaskLit was
// extracted to prevent in #477 one function up: move the split and the literal and
// the sentence describing it would disagree, and no test would notice, because
// each is asserted against its own hardcoded string.
func javaU64Spelling(dec string) (lit string, hex bool) {
	n, err := strconv.ParseUint(dec, 10, 64)
	switch {
	case err != nil:
		return fmt.Sprintf("Long.parseUnsignedLong(%q)", dec), false
	case n > math.MaxInt64:
		return javaMaskLit(n), true
	default:
		return strconv.FormatUint(n, 10) + "L", false
	}
}

// javaWideU64Note is the javadoc line that carries a re-spelled u64 default's
// DECIMAL value, and the empty string when nothing was re-spelled -- which is the
// usual case, since only a value past 2^63-1 is re-spelled at all.
//
// It exists because the hex literal is the only record of the default in the
// generated source (the field javadoc carries the description and the schema
// bound, never the default), and hex is not how the author wrote a u64 quantity.
// A trailing `//` comment cannot serve: javaDefaultValue hands the same one string
// to two INLINE compares, where a line comment would swallow the rest of the
// expression. The javadoc is where a reader asks "what is this field's default"
// anyway.
//
// It never decides for itself WHICH defaults were re-spelled: javaU64Spelling
// hands back that flag alongside the literal, and the sentence interpolates the
// literal it is actually describing rather than recomputing it.
//
// An array of BITFIELD is re-spelled in hex too and deliberately gets no note --
// see javaMaskLit, where hex is the right reading of a mask however it was
// written.
func javaWideU64Note(f *ir.Field) string {
	switch {
	case f.Kind == ir.KindU64 && f.Default != nil:
		dec := scalarLit(f.Default)
		lit, hex := javaU64Spelling(dec)
		if !hex {
			return ""
		}
		return fmt.Sprintf("Default %s: past 2^63-1, so it is spelled below as the hex long literal %s.", dec, lit)
	case f.Kind == ir.KindArray && f.Elem == ir.KindU64:
		vals, ok := f.Default.([]any)
		if !ok {
			return ""
		}
		anyHex := false
		decs := make([]string, len(vals))
		for i, v := range vals {
			decs[i] = scalarLit(v)
			_, hex := javaU64Spelling(decs[i])
			anyHex = anyHex || hex
		}
		if !anyHex {
			return ""
		}
		return fmt.Sprintf("Default [%s]: the elements past 2^63-1 are spelled below as hex long literals.",
			strings.Join(decs, ", "))
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
