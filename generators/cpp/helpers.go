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
		return "std::string"
	case ir.KindBlob:
		return "std::vector<std::uint8_t>"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindBitfield:
		return bitfieldBacking(f.Ref.Target)
	case ir.KindArray:
		return g.cppArrayContainer(f.Elem, f.ElemRef, f.ElemItems, f.Count)
	}
	return "void"
}

// isNativeArrayElem reports whether an array element lowers to a native array
// wire type (numeric/enum/boolean/bitfield): those are stored in a fixed
// std::array. String/blob/struct/union/nested-array elements lower to a wrapper
// sequence and are stored in a std::vector (decode appends).
func isNativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// cppArrayContainer is the C++ member type for an array with the given element:
// std::array<T, count> for native elements, std::vector<T> otherwise.
func (g *gen) cppArrayContainer(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64) string {
	et := g.cppArrayElem(elem, ref, items)
	if isNativeArrayElem(elem) {
		return fmt.Sprintf("std::array<%s, %d>", et, count)
	}
	return "std::vector<" + et + ">"
}

// cppArrayElem is the C++ type of a single array element, recursing for nested
// arrays. Enum/bitfield map to their backing/underlying type only where the
// element is stored raw; enum keeps its scoped type so JSON stays value-typed.
func (g *gen) cppArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "std::string"
	case ir.KindBlob:
		return "std::vector<std::uint8_t>"
	case ir.KindBool:
		return "bool"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindBitfield:
		return bitfieldBacking(ref.Target)
	case ir.KindArray:
		return g.cppArrayContainer(items.Elem, items.ElemRef, items.ElemItems, items.Count)
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
			return scalarLit(f.Default) + "LL"
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
		return "{}"
	}
	return "{}"
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

// cppMsgSeqPrelude is emitted for BOTH corelibs (the _StrSeq/_BlobSeq prelude is
// pure-corelib-cpp only). _MsgSeq decodes a wrapper sequence of struct/union
// elements, or of nested (native) arrays, into a std::vector: one element is
// emplaced and read per child. is.read descends into a struct/union element's
// own sub-sequence, or reads a nested array element, exactly as a scalar field
// would. The target is held by pointer (not a bound reference) so the same
// instance can be reused: the corelib-c-cpp decoder is deferred and dereferences
// the visitor after deserialize returns, so on that path the visitor is given
// static storage (a bound stack local would be a use-after-return). Unused
// template, so it costs nothing when a message has no such array.
const cppMsgSeqPrelude = `template <typename T>
struct _MsgSeq : sofab::IStreamMessage {
    std::vector<T> *out = nullptr;
    void deserialize(sofab::IStreamImpl &is, sofab::id, std::size_t, std::size_t) noexcept override {
        out->emplace_back();
        is.read(out->back());
    }
};`

const cppPrelude = `struct _StrSeq : sofab::IStreamMessage {
    std::vector<std::string> &out;
    explicit _StrSeq(std::vector<std::string> &o) : out(o) {}
    void deserialize(sofab::IStreamImpl &is, sofab::id, std::size_t, std::size_t) noexcept override {
        std::string _s; is.read(_s); out.push_back(std::move(_s));
    }
};
struct _BlobSeq : sofab::IStreamMessage {
    std::vector<std::vector<std::uint8_t>> &out;
    explicit _BlobSeq(std::vector<std::vector<std::uint8_t>> &o) : out(o) {}
    void deserialize(sofab::IStreamImpl &is, sofab::id, std::size_t, std::size_t) noexcept override {
        std::string _s; is.read(_s); out.emplace_back(_s.begin(), _s.end());
    }
};`

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
