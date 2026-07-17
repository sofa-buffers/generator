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
		// read_string_noterm path as std::string). An unbounded string has no
		// maxlen, so it stays std::string — allowed only under allow_dynamic, else
		// checkBounded rejects it.
		if g.fixed && f.HasMaxlen {
			return fmt.Sprintf("sofab::FixedString<%d>", f.Maxlen)
		}
		return "std::string"
	case ir.KindBlob:
		// Fixed profile: a bounded blob becomes fixed-capacity inline storage
		// (no heap). The read(void*,size_t) blob overload already takes a raw
		// pointer, so decode needs no corelib change.
		if g.fixed && f.HasMaxlen {
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
func isNativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// dynNativeArray reports whether a native-element array lowers to a growable
// std::vector rather than a fixed std::array: the heap (corelib: cpp) profile
// with no schema count. Such an array carries no compile-time capacity, so it
// must be sized to the wire element count before the corelib's span-based
// read/write (#112). A bounded native array (count present) stays a fixed
// std::array; the fixed profile keeps std::array and rejects the count-less case
// in checkBounded.
func (g *gen) dynNativeArray(elem ir.Kind, count int64) bool {
	return !g.fixed && isNativeArrayElem(elem) && count <= 0
}

// cppArrayContainer is the C++ member type for an array with the given element.
// A bounded native element is a fixed std::array<T, count>; a count-less native
// element on the heap profile is a growable std::vector<T> (NOT std::array<T, 0>,
// which cannot hold any element — #112). For composite/dynamic elements the
// default profile uses std::vector<T>; the fixed profile lowers a bounded array
// (count present, and for blobs the element maxlen present) to an
// InlineVector<T, count> — fixed inline storage with a separate logical length,
// no heap. A string/blob element additionally needs its element maxlen to be
// sized; without it the array stays std::vector.
func (g *gen) cppArrayContainer(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool, elemMax int64) string {
	et := g.cppArrayElem(elem, ref, items, elemMaxHas, elemMax)
	if isNativeArrayElem(elem) {
		if g.dynNativeArray(elem, count) {
			return "std::vector<" + et + ">"
		}
		return fmt.Sprintf("std::array<%s, %d>", et, count)
	}
	if g.fixed && count > 0 {
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
		if g.fixed && elemMaxHas {
			return fmt.Sprintf("sofab::FixedString<%d>", elemMax)
		}
		return "std::string"
	case ir.KindBlob:
		if g.fixed && elemMaxHas {
			return fmt.Sprintf("sofab::FixedBytes<%d>", elemMax)
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

// cppFixedArrayNeedsReset reports whether a fixed native array field's decode
// must clear the member to the element default before the wire elements land.
//
// A `count: N` array decodes to exactly N elements: M from the wire, the ELEMENT
// default (zero) at [M,N) (MESSAGE_SPEC §3). The std::array<T,N> member starts at
// the field's *declaration* default, so with a non-zero SCHEMA default the tail
// the corelib's span read never touches would wrongly keep that schema default:
// with `default: [1,2,3]` on `count: 5`, a value of [1,2,0,0,0] encodes (trimmed)
// to the 2-element wire [1,2] and would decode back as [1,2,3,0,0] — a corrupted
// round-trip. Clearing first makes the tail the element default.
//
// The schema default is the value of an ABSENT field (sparse omission,
// MESSAGE_SPEC S2); it is reconstructed from the member's construction default
// and is untouched by this reset, which only runs once the field is PRESENT.
//
// A field with no schema default (or an all-zero one) already declares an
// all-zero array, so it needs no reset and its generated code is unchanged.
func (g *gen) cppFixedArrayNeedsReset(f *ir.Field) bool {
	if f.Kind != ir.KindArray || !isNativeArrayElem(f.Elem) {
		return false
	}
	// Only fixed storage: a count-less array lowers to a std::vector that decode
	// resizes to the wire count, so it has no stale tail to clear.
	if g.dynNativeArray(f.Elem, f.Count) || f.Count <= 0 {
		return false
	}
	vals, ok := f.Default.([]any)
	if !ok {
		return false
	}
	zero := g.cppArrayElemLit(f.Elem, f.ElemRef, 0)
	for _, v := range vals {
		if g.cppArrayElemLit(f.Elem, f.ElemRef, v) != zero {
			return true
		}
	}
	return false
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
    // Schema fixed-count bound N (-1 == dynamic/unbounded). An element id >= N is
    // a schema-bound violation (MESSAGE_SPEC S5.1/S7: an index at or past the
    // fixed count is INVALID, never grown-into) - reject before emplacing, which
    // also bounds the allocation against an over-index heap-amplification DoS.
    long cap = -1;
    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t, std::size_t _count) noexcept override {
        if (cap >= 0 && static_cast<std::size_t>(id) >= static_cast<std::size_t>(cap)) { is.invalidate(); return; }
        T &row = out->emplace_back();
        // A count-less native-array row (matrix with dynamic rows) is a std::vector
        // that the corelib's span read fills only up to its current size, so size it
        // to the row's wire count first. Struct/union rows are IStreamMessage
        // (no resize) and fixed std::array rows have no resize(), so both skip this.
        if constexpr (requires { row.resize(_count); } && !std::is_base_of_v<sofab::IStreamMessage, T>) {
            row.resize(_count);
        }
        is.read(row);
    }
};`

// cppFixedPrelude is emitted only for the fixed-capacity (embedded) path
// (corelib: c-cpp). The heap-free containers it decodes into —
// sofab::FixedBytes<N> (a blob) and sofab::InlineVector<T,N> (a sequence) —
// live in the corelib alongside sofab::FixedString<N>, so the generator only
// references them; this prelude supplies the element collectors that bridge the
// corelib's sequence-decode callbacks into that inline storage:
//
//   - _MsgSeqFixed / _FixedBlobSeq / _FixedStrSeq: per-element visitors that
//     emplace into the next inline slot instead of push_back/emplace_back onto
//     the heap. Inline storage never reallocates, so a bound-then-filled element
//     (the deferred corelib-c-cpp decoder) is address-stable.
const cppFixedPrelude = `template <typename Container>
struct _MsgSeqFixed : sofab::IStreamMessage {
    Container *out = nullptr;
    void deserialize(sofab::IStreamImpl &is, sofab::id, std::size_t, std::size_t) noexcept override {
        is.read(out->emplace_back());
    }
};
// _FixedBlobSeq / _FixedStrSeq place a blob / string element at its index id:
// a default (empty) element is omitted on the wire, so the
// inline vector is grown with empty-default slots up to id and the value is
// stored at that index rather than appended in arrival order. Inline storage
// never reallocates, so an earlier bound-then-filled element stays address-stable
// while later slots grow.
// An element index at or beyond the fixed capacity N has no inline slot:
// InlineVector::emplace_back() is a no-op once full (N never grows), so an
// unbounded fill loop would spin forever on such an index (issue #126). Drop the
// element instead — binding no destination leaves the corelib's target_ptr NULL,
// so the core skips its payload, mirroring how an unhandled field / over-capacity
// native-array element is dropped (MESSAGE_SPEC S5.1). Two open sequences at EOF
// then surface INCOMPLETE, matching the heap profile / C / Go / Rust.
template <typename Container>
struct _FixedBlobSeq : sofab::IStreamMessage {
    Container *out = nullptr;
    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t _size, std::size_t) noexcept override {
        if (static_cast<std::size_t>(id) >= out->capacity()) return;
        while (out->size() <= static_cast<std::size_t>(id)) out->emplace_back();
        auto &b = (*out)[id];
        b.set_len(_size);
        if (_size) is.read(b.data(), b.size());
    }
};
template <typename Container>
struct _FixedStrSeq : sofab::IStreamMessage {
    Container *out = nullptr;
    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t _size, std::size_t) noexcept override {
        if (static_cast<std::size_t>(id) >= out->capacity()) return;
        while (out->size() <= static_cast<std::size_t>(id)) out->emplace_back();
        auto &s = (*out)[id];
        s.set_len(_size);
        if (_size) is.read(s);
    }
};`

// cppTrimPrelude is emitted for BOTH corelibs. _trimTail returns a view of a's
// leading elements [0, M'), where M' is one past the last element that differs
// from the element default (0 / 0.0 / false for every native kind; M' == 0 when
// every element is the default). A `count: N` array is FIXED-LENGTH, so its
// canonical wire carries exactly those M' elements and the decoder rebuilds the
// trailing default run from the schema count (MESSAGE_SPEC §3) — handing the
// whole std::array to the corelib would emit that run instead.
//
// Elements compare by BIT PATTERN (memcmp against a value-initialized element),
// never by ==: -0.0 == 0.0 is true in C++, but -0.0 is a distinct value that
// must survive the round-trip rather than be silently trimmed to +0.0; a NaN
// likewise never matches the default. Every element type reaching this helper is
// an integer / float / enum backing type, none of which have padding bits, so a
// byte compare is exactly a value-with-sign compare.
//
// Non-owning and non-allocating (the span borrows the caller's storage), so the
// heap-free (corelib: c-cpp) profile uses the same helper. Unused template, so
// it costs nothing when a message has no fixed-count native array.
const cppTrimPrelude = `template <typename C>
std::span<const typename C::value_type> _trimTail(const C &_a) noexcept {
    using _T = typename C::value_type;
    const _T _z{};
    std::size_t _n = _a.size();
    while (_n > 0 && std::memcmp(&_a[_n - 1], &_z, sizeof(_T)) == 0) --_n;
    return std::span<const _T>(_a.data(), _n);
}`

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
const cppPrelude = `struct _StrSeq : sofab::IStreamMessage {
    std::vector<std::string> &out;
    long _cap;
    explicit _StrSeq(std::vector<std::string> &o, long cap = -1) : out(o), _cap(cap) {}
    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t, std::size_t) noexcept override {
        if (_cap >= 0 && static_cast<std::size_t>(id) >= static_cast<std::size_t>(_cap)) { is.invalidate(); return; }
        std::string _s; is.read(_s);
        while (out.size() <= static_cast<std::size_t>(id)) out.emplace_back();
        out[id] = std::move(_s);
    }
};
struct _BlobSeq : sofab::IStreamMessage {
    std::vector<std::vector<std::uint8_t>> &out;
    long _cap;
    explicit _BlobSeq(std::vector<std::vector<std::uint8_t>> &o, long cap = -1) : out(o), _cap(cap) {}
    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t, std::size_t) noexcept override {
        if (_cap >= 0 && static_cast<std::size_t>(id) >= static_cast<std::size_t>(_cap)) { is.invalidate(); return; }
        std::string _s; is.read(_s);
        while (out.size() <= static_cast<std::size_t>(id)) out.emplace_back();
        out[id].assign(_s.begin(), _s.end());
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
