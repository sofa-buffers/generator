package cpp

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

func cfgBool(cfg map[string]any, key string, dflt bool) bool {
	if v, ok := cfg[key].(bool); ok {
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

func (g *gen) cppType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8:
		return "std::uint8_t"
	case ir.KindU16:
		return "std::uint16_t"
	case ir.KindU32:
		return "std::uint32_t"
	case ir.KindU64:
		return "std::uint64_t"
	case ir.KindI8:
		return "std::int8_t"
	case ir.KindI16:
		return "std::int16_t"
	case ir.KindI32:
		return "std::int32_t"
	case ir.KindI64:
		return "std::int64_t"
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		// Fixed profile: a bounded string becomes sofab::FixedString<N> (heap-free
		// inline storage; the corelib-c-cpp wrapper fills it via the same
		// read_string_noterm path as std::string). Under allow_dynamic the same
		// bounded field lives in a std::string instead, sized to what the message
		// carries; the maxlen then rides into the decode path as an explicit
		// reject rather than as the container's capacity.
		if g.fixed && !g.allowDynamic && f.HasMaxlen {
			return fmt.Sprintf("sofab::FixedString<%d>", f.Maxlen)
		}
		return "std::string"
	case ir.KindBlob:
		// Fixed profile: a bounded blob becomes fixed-capacity inline storage
		// (no heap). The read(void*,size_t) blob overload already takes a raw
		// pointer, so decode needs no corelib change. allow_dynamic puts the same
		// bounded blob in a std::vector, as for strings above.
		if g.fixed && !g.allowDynamic && f.HasMaxlen {
			return fmt.Sprintf("sofab::FixedBytes<%d>", f.Maxlen)
		}
		return "std::vector<std::uint8_t>"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindBitfield:
		return bitfieldBacking(f.Ref.Target)
	case ir.KindArray:
		return g.cppArrayContainer(f.Elem, f.ElemRef, f.ElemItems, f.Count, f.ElemMaxHas, f.ElemMax)
	}
	return "void"
}

// isNativeArrayElem reports whether an array element lowers to a native array
// wire type (numeric/enum/boolean/bitfield): those are stored in a fixed
// std::array. String/blob/struct/union/nested-array elements lower to a wrapper
// sequence and are stored in a std::vector (decode appends).
// cppExpectedWire returns the sofab::Wire member a field's header must carry for
// its schema-typed read() to be the right one, mirroring the encode side:
// unsigned integers, bool and bitfield -> Unsigned; signed integers and enum ->
// Signed; fp32/fp64, string and blob -> Fixlen; nested messages and composite
// (wrapper) arrays -> SequenceStart; native scalar arrays -> the matching
// Array* wire type.
func cppExpectedWire(fld *ir.Field) string {
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBool, ir.KindBitfield:
		return "sofab::Wire::Unsigned"
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "sofab::Wire::Signed"
	case ir.KindFP32, ir.KindFP64, ir.KindString, ir.KindBlob:
		return "sofab::Wire::Fixlen"
	case ir.KindStruct, ir.KindUnion:
		return "sofab::Wire::SequenceStart"
	case ir.KindArray:
		if !isNativeArrayElem(fld.Elem) {
			return "sofab::Wire::SequenceStart"
		}
		switch fld.Elem {
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
			return "sofab::Wire::ArraySigned"
		case ir.KindFP32, ir.KindFP64:
			return "sofab::Wire::ArrayFixlen"
		default: // u8/u16/u32/u64, bool, bitfield
			return "sofab::Wire::ArrayUnsigned"
		}
	}
	return "sofab::Wire::SequenceStart" // unreachable: keeps the switch total
}

// cppFixSubtype returns the sofab::Fix member a fixlen-framed kind must carry,
// or "" for a kind the wire type alone already settles.
func cppFixSubtype(k ir.Kind) string {
	switch k {
	case ir.KindFP32:
		return "sofab::Fix::Fp32"
	case ir.KindFP64:
		return "sofab::Fix::Fp64"
	case ir.KindString:
		return "sofab::Fix::String"
	case ir.KindBlob:
		return "sofab::Fix::Blob"
	}
	return ""
}

// cppNeedsWireGuard reports whether a field still needs a generated §7.3 guard.
//
// It never does any more. The comparison belongs in the corelib, where a typed
// read knows both the tag it declares and the one that was delivered, and both
// C++ corelibs now make it there: corelib-cpp inside every typed read (the seam,
// docs/models/type-reconciliation.md), and the corelib-c-cpp wrapper either in
// the C decoder — which unbinds a contradicting read and skips the field like an
// unknown id — or, where the arm has to touch its destination before binding it,
// in readString/readBlob/readArray/readSequence, which check the tag before that
// first side effect.
//
// Kept as a function rather than deleted so the reasoning above stays attached to
// the decision, and so a future shape that genuinely needs a generated guard has
// somewhere to say so.
func cppNeedsWireGuard(_ *ir.Field, _ bool) bool {
	return false
}

// cppWireGuard renders the §7.3 condition guarding one case arm: the wire type
// the declared type maps to, plus the fixlen subtype where the wire type alone
// is ambiguous (fp32/fp64/string/blob and the fp32/fp64 native arrays, which
// share Wire::Fixlen / Wire::ArrayFixlen). Returns "" when no guard is needed.
func cppWireGuard(fld *ir.Field) string {
	cond := "is.wire() != " + cppExpectedWire(fld)
	sub := cppFixSubtype(fld.Kind)
	if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
		sub = cppFixSubtype(fld.Elem)
	}
	if sub != "" {
		cond += " || is.fixType() != " + sub
	}
	return cond
}

func isNativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// dynNativeArray reports whether a native scalar array is stored in a growable
// std::vector rather than the corelib's heap-free sofab::InlineVector<T, N>.
//
// Since `count: N` became a CAPACITY and the wire count became the array's
// LENGTH (MESSAGE_SPEC §3, documentation#29), EVERY native array has to carry a
// logical length of its own: without one it cannot hold a value shorter than N,
// so [1, 2] on a `count: 4` field would encode as four elements on one profile
// and two on another — the same schema, two different wire images. So the only
// question left is WHERE the elements live, which is the heap-free decision and
// nothing else: inline storage on `c-cpp` without allow_dynamic, the heap
// everywhere else. std::array<T, N> is not an answer to either question and is
// no longer used for an array member.
func (g *gen) dynNativeArray(elem ir.Kind) bool {
	if !isNativeArrayElem(elem) {
		return false
	}
	return !(g.fixed && !g.allowDynamic)
}

// cppArrayContainer is the C++ member type for an array with the given element.
// Every array member is length-carrying: a growable std::vector<T>, or — on the
// heap-free c-cpp storage mode, for a bounded array — the corelib's
// InlineVector<T, count>, which is inline storage plus a separate logical
// length. That is uniform across element kinds now: a native scalar array is
// sized exactly like a wrapper array, because `count` is a capacity for both and
// the wire count is the length for both (MESSAGE_SPEC §3). A string/blob element
// additionally needs its element maxlen to be sized inline; without it the array
// stays std::vector.
func (g *gen) cppArrayContainer(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool, elemMax int64) string {
	et := g.cppArrayElem(elem, ref, items, elemMaxHas, elemMax)
	if isNativeArrayElem(elem) {
		if g.dynNativeArray(elem) {
			return "std::vector<" + et + ">"
		}
		return fmt.Sprintf("sofab::InlineVector<%s, %d>", et, count)
	}
	if g.fixed && !g.allowDynamic && count > 0 {
		switch elem {
		case ir.KindString, ir.KindBlob:
			if elemMaxHas {
				return fmt.Sprintf("sofab::InlineVector<%s, %d>", et, count)
			}
		case ir.KindStruct, ir.KindUnion, ir.KindArray:
			return fmt.Sprintf("sofab::InlineVector<%s, %d>", et, count)
		}
	}
	return "std::vector<" + et + ">"
}

// cppArrayElem is the C++ type of a single array element, recursing for nested
// arrays. Enum/bitfield map to their backing/underlying type only where the
// element is stored raw; enum keeps its scoped type so JSON stays value-typed.
func (g *gen) cppArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64) string {
	switch elem {
	case ir.KindString:
		// Same rule as a scalar string field: inline storage by default, a
		// std::string under allow_dynamic, with the element maxlen still declared
		// and still enforced — on the decode path rather than as a capacity.
		if g.fixed && !g.allowDynamic && elemMaxHas {
			return fmt.Sprintf("sofab::FixedString<%d>", elemMax)
		}
		return "std::string"
	case ir.KindBlob:
		if g.fixed && !g.allowDynamic && elemMaxHas {
			return fmt.Sprintf("sofab::FixedBytes<%d>", elemMax)
		}
		return "std::vector<std::uint8_t>"
	case ir.KindBool:
		// On the c-cpp leg a boolean array's element is the wire's own
		// std::uint8_t, not bool. corelib-c-cpp's decoder is DEFERRED: read()
		// records the destination's ADDRESS and the C runtime writes the element
		// bytes after the field callback has returned, so the destination must be
		// the member itself and must have one addressable byte per element.
		// std::vector<bool> is the bit-packed specialisation -- no data(), no byte
		// per element -- so it cannot be a decode destination at all, and the
		// std::array<bool, N> leg was only ever reached through a
		// reinterpret_cast. One element type for both c-cpp storage modes keeps
		// the two legs' generated API identical, which is the profile promise.
		// corelib-cpp decodes synchronously through a temporary, so it keeps bool.
		if g.fixed {
			return "std::uint8_t"
		}
		return "bool"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindBitfield:
		return bitfieldBacking(ref.Target)
	case ir.KindArray:
		return g.cppArrayContainer(items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas, items.ElemMax)
	default:
		return numCppType(elem)
	}
}

func numCppType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "std::uint8_t"
	case ir.KindU16:
		return "std::uint16_t"
	case ir.KindU32:
		return "std::uint32_t"
	case ir.KindU64:
		return "std::uint64_t"
	case ir.KindI8:
		return "std::int8_t"
	case ir.KindI16:
		return "std::int16_t"
	case ir.KindI32:
		return "std::int32_t"
	case ir.KindI64:
		return "std::int64_t"
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	}
	return "std::uint8_t"
}

func (g *gen) cppDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU64:
		if f.Default != nil {
			return scalarLit(f.Default) + "ULL"
		}
		return "0"
	case ir.KindI64:
		if f.Default != nil {
			return cppI64Lit(f.Default)
		}
		return "0"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32:
		if f.Default != nil {
			return scalarLit(f.Default)
		}
		return "0"
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
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", s)
		}
		return `""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return fmt.Sprintf("{%s}", byteList(raw))
			}
		}
		return "{}"
	case ir.KindEnum:
		tn := g.typeName(f.Ref.Key)
		if f.Default != nil {
			if name, ok := g.enumMember(f.Ref.Target, f.Default); ok {
				return tn + "::" + name
			}
			return fmt.Sprintf("static_cast<%s>(%s)", tn, scalarLit(f.Default))
		}
		return fmt.Sprintf("static_cast<%s>(0)", tn)
	case ir.KindBitfield:
		return fmt.Sprintf("%d", g.bitfieldDefault(f))
	case ir.KindStruct, ir.KindUnion:
		return "{}"
	case ir.KindArray:
		// A native scalar array is a leaf: materialize its schema default at
		// construction (zero-filled when none) so an omitted default array
		// reconstructs correctly and serialize can compare against it.
		if isNativeArrayElem(f.Elem) {
			return g.cppNativeArrayBraces(f)
		}
		// A composite-element array is a wrapper sequence, and its construction
		// value is the EMPTY array — with or without a declared `count: N`.
		//
		// `count` is a CAPACITY, not a length (MESSAGE_SPEC §3): it never reaches
		// the wire and never adds an element, so a fresh count: N array holds
		// nothing, exactly like a count-less one. Both std::vector and
		// sofab::InlineVector<T,N> carry their own logical length, so both express
		// that directly — which is also what makes 0..N all representable here,
		// and what "the wire count M IS the length" needs of the storage.
		//
		// The declared `default` is still not materialized (§2); absent and
		// explicitly-empty therefore denote the same value, which is what lets
		// emitSerializeArray close the field wrapper with the dropping end.
		return "{}"
	}
	return "{}"
}

// cppNativeArrayBraces renders a native scalar array's schema default as a braced
// initializer ({v0, v1, ...}).
//
// It is NOT padded out to a declared `count: N`: that is a capacity, not a length
// (MESSAGE_SPEC §3), so the default stands exactly as written — and so does the
// value it is compared against, which is what keeps a shorter array distinct from
// the padded one.
//
// The storage no longer has a say in it: both containers a native array can land
// in — std::vector<T> and sofab::InlineVector<T, N> — take the initializer
// verbatim and carry the resulting length themselves, so a default shorter than
// N stays shorter than N on every profile. That is what the container change
// bought: previously a std::array<T,N> zero-filled whatever the initializer left
// out, and the same schema then had two different construction values (and two
// different wire images). See docs/generator/cpp.md.
func (g *gen) cppNativeArrayBraces(f *ir.Field) string {
	vals, _ := f.Default.([]any)
	return "{" + strings.Join(g.cppArrayElemLits(f, vals), ", ") + "}"
}

func (g *gen) cppArrayElemLits(f *ir.Field, vals []any) []string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = g.cppArrayElemLit(f.Elem, f.ElemRef, v)
	}
	return parts
}

// cppArrayElemLit renders one native-array element default as a C++ literal typed
// for the array's element type (u64/i64 get width suffixes; fp a decimal point;
// enum its scoped member/cast; bool true/false).
func (g *gen) cppArrayElemLit(elem ir.Kind, ref *ir.TypeRef, v any) string {
	switch elem {
	case ir.KindU64:
		return scalarLit(v) + "ULL"
	case ir.KindI64:
		return cppI64Lit(v)
	case ir.KindFP32:
		return floatLit(v) + "f"
	case ir.KindFP64:
		return floatLit(v)
	case ir.KindBool:
		if b, ok := v.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindEnum:
		tn := g.typeName(ref.Key)
		if name, ok := g.enumMember(ref.Target, v); ok {
			return tn + "::" + name
		}
		return fmt.Sprintf("static_cast<%s>(%s)", tn, scalarLit(v))
	default: // u8..i32, bitfield
		return scalarLit(v)
	}
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
		return "std::int8_t"
	case lo >= -32768 && hi <= 32767:
		return "std::int16_t"
	default:
		return "std::int32_t"
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
		return "std::uint8_t"
	case max <= 15:
		return "std::uint16_t"
	case max <= 31:
		return "std::uint32_t"
	default:
		return "std::uint64_t"
	}
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

// cppI64Lit renders an i64 default. INT64_MIN needs a form of its own: C++ has
// no negative literals, so -9223372036854775808LL parses as the unary minus of
// 9223372036854775808LL, and that magnitude does not fit a signed 64-bit type.
// The compiler gives it an unsigned type and warns, which breaks any consumer
// building generated headers with -Wall -Werror. Writing it as (min+1)-1 keeps
// every intermediate value in range.
func cppI64Lit(v any) string {
	// The IR carries an i64 default as int64 or, when the schema wrote it as a
	// string (which is how a value at the edge of the range is expressed safely
	// in YAML/JSON), as that string. Both reach here.
	if scalarLit(v) == "-9223372036854775808" {
		return "(-9223372036854775807LL - 1)"
	}
	return scalarLit(v) + "LL"
}

func scalarLit(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// floatLit renders a numeric default as a C++ floating literal (always with a
// decimal point so "0" becomes "0.0", which is a valid float when suffixed).
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
		parts[i] = fmt.Sprintf("0x%02x", x)
	}
	return strings.Join(parts, ", ")
}

// _StrSeq / _BlobSeq collect the elements of a string / blob wrapper-sequence
// array. Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
// element is omitted on the wire, so each value is placed at its id and any gap
// is grown with the element default ("" / empty blob) rather than appended in
// arrival order.
//
// cap is the schema fixed-count bound N (-1 == dynamic/unbounded): an element id
// >= N is a schema-bound violation (MESSAGE_SPEC S5.1/S7 — an index at or past
// the fixed count is INVALID, never grown-into), rejected before the container
// grows, which also caps the allocation against an over-index heap-amplification
// DoS. A dynamic array (no schema count) keeps every delivered index.
// _emax is the element's schema maxlen (-1 == no bound): an element whose wire
// byte length exceeds it is INVALID (MESSAGE_SPEC S7.1), rejected before the
// read, never truncated -- the wrapper-element analogue of the scalar maxlen
// reject.

// cppKeywords are C++ reserved words (superset of C). No identifier escape, so a
// field with such a name is mangled (trailing underscore); JSON keys (emitted as
// string literals) keep the original name.
var cppKeywords = map[string]bool{
	"alignas": true, "alignof": true, "and": true, "and_eq": true, "asm": true,
	"auto": true, "bitand": true, "bitor": true, "bool": true, "break": true,
	"case": true, "catch": true, "char": true, "char8_t": true, "char16_t": true,
	"char32_t": true, "class": true, "compl": true, "concept": true, "const": true,
	"consteval": true, "constexpr": true, "constinit": true, "const_cast": true,
	"continue": true, "co_await": true, "co_return": true, "co_yield": true,
	"decltype": true, "default": true, "delete": true, "do": true, "double": true,
	"dynamic_cast": true, "else": true, "enum": true, "explicit": true, "export": true,
	"extern": true, "false": true, "float": true, "for": true, "friend": true,
	"goto": true, "if": true, "inline": true, "int": true, "long": true,
	"mutable": true, "namespace": true, "new": true, "noexcept": true, "not": true,
	"not_eq": true, "nullptr": true, "operator": true, "or": true, "or_eq": true,
	"private": true, "protected": true, "public": true, "register": true,
	"reinterpret_cast": true, "requires": true, "return": true, "short": true,
	"signed": true, "sizeof": true, "static": true, "static_assert": true,
	"static_cast": true, "struct": true, "switch": true, "template": true, "this": true,
	"thread_local": true, "throw": true, "true": true, "try": true, "typedef": true,
	"typeid": true, "typename": true, "union": true, "unsigned": true, "using": true,
	"virtual": true, "void": true, "volatile": true, "wchar_t": true, "while": true,
	"xor": true, "xor_eq": true,
}

// cppIdent mangles a field name that is a C++ keyword (trailing underscore).
func cppIdent(name string) string {
	if cppKeywords[name] {
		return name + "_"
	}
	return name
}

// cppArrayBounds renders the trailing readArray() arguments carrying the two
// receiver-side bounds: the schema `count` (INVALID when exceeded) and the
// configured max_dyn_array_count policy cap (LimitExceeded). Both are omitted
// when absent, so a plain bounded array reads `is.readArray(x, 4)` and an
// unbounded one with no cap configured reads `is.readArray(x)`.
// cppElemBound renders the readArray() element-width argument for a NUMERIC
// array element, or "" where no bound applies (generator#279 / Crucible F-0052).
//
// MESSAGE_SPEC §1/§7.1 makes the declared element width a validity bound, and
// corelib-cpp enforces it -- but only when the argument is armed. Left at its
// default the unbounded decode runs and an over-width element is masked to the
// width and kept: 5208 into a `u8` array came back as 88. The scalar position was
// already correct (generated code checks that one inline, generator#266); this is
// the array half, and it lives in the corelib because readArray converts the
// elements itself.
//
// ElemBound::of<E>() is the corelib's own helper and is documented as safe to
// hand in unconditionally: it comes back UNARMED for a 64-bit element, whose
// range is the accumulator's own, so a u64/i64 array pays nothing.
//
// Floating-point elements are excluded, and not merely as an optimization.
// ElemBound::of<float>() would cast std::numeric_limits<float>::max() to int64_t
// inside a constexpr function -- out of range, so a hard compile error rather
// than a wrong bound. The corelib ignores the argument for a non-integral element
// anyway (its bounded path sits behind `if constexpr (std::is_integral_v<Elem>)`),
// so there is nothing to express here.
func (g *gen) cppElemBound(elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return fmt.Sprintf("sofab::ElemBound::of<%s>()", numCppType(elem))
	case ir.KindBitfield:
		if ref != nil && ref.Target != nil {
			return fmt.Sprintf("sofab::ElemBound::of<%s>()", bitfieldBacking(ref.Target))
		}
	}
	return ""
}

// cppArrayArgs renders the trailing readArray() arguments WITH the element-width
// bound. The bound is the fourth parameter, so the schema count and the policy cap
// have to be spelled out even where they are the defaults -- which is why this
// cannot simply append to cppArrayBounds.
func (g *gen) cppArrayArgs(count int64, hasCount bool, elemBound string) string {
	if elemBound == "" {
		return g.cppArrayBounds(count, hasCount)
	}
	schema := int64(-1)
	if hasCount {
		schema = count
	}
	dyn := "-1"
	if !hasCount && g.limArrHas {
		dyn = "SOFAB_MAX_DYN_ARRAY_COUNT"
	}
	return fmt.Sprintf(", %d, %s, %s", schema, dyn, elemBound)
}

func (g *gen) cppArrayBounds(count int64, hasCount bool) string {
	schema := int64(-1)
	if hasCount {
		schema = count
	}
	if !hasCount && g.limArrHas {
		return fmt.Sprintf(", -1, SOFAB_MAX_DYN_ARRAY_COUNT")
	}
	if schema < 0 {
		return ""
	}
	return fmt.Sprintf(", %d", schema)
}
