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
		// Strings stay std::string even in the fixed profile: a FixedString<N>
		// decode overload is blocked on a corelib-c-cpp addition (the scalar string
		// read is hard-gated on std::is_same_v<T,std::string>, and IStreamImpl::ctx_
		// is protected/unreachable from generated code, so the interim bridge is not
		// writable). See docs/generator/cpp.md.
		return "std::string"
	case ir.KindBlob:
		// Fixed profile: a bounded blob becomes fixed-capacity inline storage
		// (no heap). The read(void*,size_t) blob overload already takes a raw
		// pointer, so decode needs no corelib change.
		if g.fixed && f.HasMaxlen {
			return fmt.Sprintf("FixedBytes<%d>", f.Maxlen)
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
func isNativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// cppArrayContainer is the C++ member type for an array with the given element.
// Native elements are always a fixed std::array<T, count>. For composite/dynamic
// elements the default profile uses std::vector<T>; the fixed profile lowers a
// bounded array (count present, and for blobs the element maxlen present) to an
// InlineVector<T, count> — fixed inline storage with a separate logical length,
// no heap. Strings remain std::vector<std::string> in either profile (the fixed
// string element type is blocked on a corelib-c-cpp decode overload).
func (g *gen) cppArrayContainer(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool, elemMax int64) string {
	et := g.cppArrayElem(elem, ref, items, elemMaxHas, elemMax)
	if isNativeArrayElem(elem) {
		return fmt.Sprintf("std::array<%s, %d>", et, count)
	}
	if g.fixed && count > 0 {
		switch elem {
		case ir.KindBlob:
			if elemMaxHas {
				return fmt.Sprintf("InlineVector<%s, %d>", et, count)
			}
		case ir.KindStruct, ir.KindUnion, ir.KindArray:
			return fmt.Sprintf("InlineVector<%s, %d>", et, count)
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
		return "std::string"
	case ir.KindBlob:
		if g.fixed && elemMaxHas {
			return fmt.Sprintf("FixedBytes<%d>", elemMax)
		}
		return "std::vector<std::uint8_t>"
	case ir.KindBool:
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
		// A native scalar array is a leaf: materialize its schema default at
		// construction (zero-filled when none) so an omitted default array
		// reconstructs correctly and serialize can compare against it. A
		// composite/dynamic-element array is a wrapper sequence (always framed) and
		// is left empty.
		if isNativeArrayElem(f.Elem) {
			return g.cppNativeArrayBraces(f)
		}
		return "{}"
	}
	return "{}"
}

// cppNativeArrayBraces renders a native scalar array's schema default as a braced
// initializer ({v0, v1, ...}); "{}" (zero-filled) when there is no default.
func (g *gen) cppNativeArrayBraces(f *ir.Field) string {
	vals, ok := f.Default.([]any)
	if !ok {
		return "{}"
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = g.cppArrayElemLit(f.Elem, f.ElemRef, v)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// cppArrayElemLit renders one native-array element default as a C++ literal typed
// for the array's element type (u64/i64 get width suffixes; fp a decimal point;
// enum its scoped member/cast; bool true/false).
func (g *gen) cppArrayElemLit(elem ir.Kind, ref *ir.TypeRef, v any) string {
	switch elem {
	case ir.KindU64:
		return scalarLit(v) + "ULL"
	case ir.KindI64:
		return scalarLit(v) + "LL"
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

// cppFixedPrelude is emitted only for the fixed-capacity (embedded) path
// (corelib: c-cpp). It provides heap-free, schema-sized
// storage that presents the same .data()/.size() surface the encode/decode paths
// already use, so the emitted wire bytes are unchanged.
//
//   - FixedBytes<N>: a blob of at most N bytes, std::array<uint8_t,N> + length.
//     Encode uses .data()/.size() (unchanged); decode uses the corelib's
//     read(void*,size_t) blob overload, so no corelib change is needed.
//   - InlineVector<T,N>: a fixed-capacity sequence (std::array<T,N> + length)
//     exposing exactly what serialize and the _MsgSeq* visitors use. Inline
//     storage never reallocates, so a bound-then-filled element (the deferred
//     corelib-c-cpp decoder) is address-stable — strictly safer than the
//     std::vector + reserve() it replaces.
//   - _MsgSeqFixed / _FixedBlobSeq: element collectors that write into the next
//     inline slot instead of push_back/emplace_back onto the heap.
const cppFixedPrelude = `template <std::size_t N>
struct FixedBytes {
    std::array<std::uint8_t, N> buf{};
    std::size_t len_ = 0;
    FixedBytes() = default;
    FixedBytes(std::initializer_list<std::uint8_t> init) noexcept {
        for (auto b : init) { if (len_ >= N) break; buf[len_++] = b; }
    }
    std::uint8_t *data() noexcept { return buf.data(); }
    const std::uint8_t *data() const noexcept { return buf.data(); }
    std::size_t size() const noexcept { return len_; }
    void set_len(std::size_t n) noexcept { len_ = n < N ? n : N; }
    void clear() noexcept { len_ = 0; }
    void push_back(std::uint8_t b) noexcept { if (len_ < N) buf[len_++] = b; }
    bool operator==(const FixedBytes &o) const noexcept {
        if (len_ != o.len_) return false;
        for (std::size_t i = 0; i < len_; ++i) { if (buf[i] != o.buf[i]) return false; }
        return true;
    }
    bool operator!=(const FixedBytes &o) const noexcept { return !(*this == o); }
};
template <typename T, std::size_t N>
struct InlineVector {
    std::array<T, N> buf{};
    std::size_t len_ = 0;
    std::size_t size() const noexcept { return len_; }
    static constexpr std::size_t capacity() noexcept { return N; }
    void reserve(std::size_t) noexcept {}
    void clear() noexcept { len_ = 0; }
    T &emplace_back() noexcept {
        std::size_t i = len_ < N ? len_++ : N - 1;
        buf[i] = T{};
        return buf[i];
    }
    void push_back(const T &v) noexcept { emplace_back() = v; }
    void push_back(T &&v) noexcept { emplace_back() = static_cast<T &&>(v); }
    T &back() noexcept { return buf[len_ - 1]; }
    T &operator[](std::size_t i) noexcept { return buf[i]; }
    const T &operator[](std::size_t i) const noexcept { return buf[i]; }
    T *data() noexcept { return buf.data(); }
    const T *data() const noexcept { return buf.data(); }
    T *begin() noexcept { return buf.data(); }
    T *end() noexcept { return buf.data() + len_; }
    const T *begin() const noexcept { return buf.data(); }
    const T *end() const noexcept { return buf.data() + len_; }
    bool operator==(const InlineVector &o) const noexcept {
        if (len_ != o.len_) return false;
        for (std::size_t i = 0; i < len_; ++i) { if (!(buf[i] == o.buf[i])) return false; }
        return true;
    }
    bool operator!=(const InlineVector &o) const noexcept { return !(*this == o); }
};
template <typename Container>
struct _MsgSeqFixed : sofab::IStreamMessage {
    Container *out = nullptr;
    void deserialize(sofab::IStreamImpl &is, sofab::id, std::size_t, std::size_t) noexcept override {
        is.read(out->emplace_back());
    }
};
template <typename Container>
struct _FixedBlobSeq : sofab::IStreamMessage {
    Container *out = nullptr;
    void deserialize(sofab::IStreamImpl &is, sofab::id, std::size_t _size, std::size_t) noexcept override {
        auto &b = out->emplace_back();
        b.set_len(_size);
        if (_size) is.read(b.data(), _size);
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
