// Package cpp is the max-speed C++ backend (PLAN §6.3): header-only output
// against corelib-cpp. Each object is a struct deriving OStreamMessage +
// IStreamMessage with serialize()/deserialize(); nested struct/union decode via
// is.read(child), encode via os.write(id, child). 64-bit-safe; no heap on the
// hot path (OStreamInline<_maxSize>).
//
// The `corelib` config key selects the C++ corelib: "cpp" (default, pure
// corelib-cpp) or "c-cpp" (the C++ wrapper over the C library in corelib-c-cpp).
// Both expose the same sofab:: interface and identical encode output. They
// differ on decode of variable-length fields: corelib-cpp resizes targets for
// you, while the corelib-c-cpp wrapper binds a target by address and fills it
// after the field callback. The "c-cpp" path therefore pre-sizes strings/blobs
// from the field length, reads blobs/sequences via the wrapper's native
// read() overloads, and reads enums into their underlying-typed storage (a
// local temp would dangle under the deferred decoder). The project Makefile for
// "c-cpp" compiles and links corelib-c-cpp's C sources.
package cpp

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for max-speed C++.
type Backend struct{}

func (*Backend) Lang() string { return "cpp" }

// Generate emits one header-only .hpp per message (with its reachable types).
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	clib := cfgString(cfg, "corelib", "cpp") == "c-cpp"
	// corelib-c-cpp targets embedded devices, so every field is schema-bounded
	// (checkBounded) and the default storage is heap-free: strings ->
	// sofab::FixedString<N>, blobs -> sofab::FixedBytes<N>, sequences ->
	// sofab::InlineVector<T,N>, native arrays -> std::array<T,N>, all sized from
	// the schema. allow_dynamic keeps the same bounds but puts those fields in
	// std::string / std::vector, for a target that has a heap and would rather
	// allocate what a message carries than its declared worst case.
	fixed := clib
	g := &gen{schema: s, ns: cfgString(cfg, "namespace", "message"), banner: cfgString(cfg, "tool_banner", "sofabgen"), license: generator.LicenseID(cfg), clib: clib, fixed: fixed, allowDynamic: cfgBool(cfg, "allow_dynamic", false), size: generator.NewSizePolicy(cfg)}
	if fixed {
		if err := g.checkBounded(s); err != nil {
			return nil, err
		}
	}
	g.resolveLimits(s, cfg)
	var files []generator.File
	for _, m := range s.Messages {
		files = append(files, generator.File{Path: strings.ToLower(m.Name) + ".hpp", Content: g.header(m)})
	}
	if cfgString(cfg, "emit", "sources") == "project" {
		files = append(files, g.projectFiles(s, cfg)...)
	}
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	ns      string
	banner  string
	license string // SPDX id, "" to omit the header line
	// clib selects the corelib-c-cpp C++ wrapper (corelib: c-cpp) instead of the
	// pure corelib-cpp. The wrapper's read(std::string&) fills the string's
	// existing buffer rather than resizing it, so generated decode must pre-size
	// variable-length fields from the field length the deserialize callback gets.
	clib bool
	// fixed is the fixed-capacity (embedded) representation; it is always equal to
	// clib (corelib-c-cpp always uses fixed containers, corelib-cpp always dynamic).
	// Bounded strings become sofab::FixedString<N>, blobs sofab::FixedBytes<N>, and
	// string/blob/struct/union/matrix sequence arrays sofab::InlineVector<T,N>, all
	// sized from the schema — no heap on the message path.
	fixed bool
	// allowDynamic selects heap containers (std::string / std::vector) for the
	// c-cpp profile's bounded fields instead of inline storage, for targets that
	// have a heap and the C++ stdlib: a field then allocates what the message
	// actually carries rather than its declared worst case, and moves instead of
	// copying. It does NOT relax any bound — maxlen/count stay mandatory in both
	// modes (checkBounded), so one schema stays valid for every c-cpp target and
	// the generator turns each bound into an explicit check on the decode path.
	allowDynamic bool
	// Receiver-side decode limits (generator#102), pure-corelib-cpp path only
	// (the c-cpp wrapper is statically schema-bounded). Each is active when its
	// max_dyn_* config key is set AND the schema has an unbounded field of that
	// kind; the generated deserialize then guards those fields per-field
	// (is.exceedLimit() -> Error::LimitExceeded) before any read. limBuffered
	// additionally caps the corelib's streaming reassembly buffer
	// (sofab::Limits{max_buffered_field}) — derived, not its own config key: the
	// largest BYTE span any single top-level field can legitimately reach, from
	// the same worst-case cost walk that sizes _maxSize, with each configured
	// max_dyn_* cap substituted for the missing schema bound (#228). A count is
	// an element count, never a byte budget: the span of an array is its count
	// times the worst-case element size, plus framing. 0 when the cap would be
	// unsound (see resolveLimits).
	limArr, limStr, limBlob          int64
	limArrHas, limStrHas, limBlobHas bool
	limBuffered                      int64
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
}

// messageSize resolves a message's worst-case encoded size via the shared walk,
// deferring a max_message_size violation to Generate.
func (g *gen) messageSize(name string, fields []*ir.Field) generator.MessageSize {
	ms, err := g.size.Resolve(name, fields)
	if err != nil && g.sizeErr == nil {
		g.sizeErr = err
	}
	return ms
}

func (g *gen) anyLimit() bool { return g.limArrHas || g.limStrHas || g.limBlobHas }

// istreamLimits renders the IStreamObject constructor argument carrying the
// derived streaming reassembly cap ("" when no length limit is configured).
func (g *gen) istreamLimits() string {
	if g.limBuffered <= 0 {
		return ""
	}
	return "{sofab::Limits{SOFAB_MAX_DYN_BUFFERED_FIELD}}"
}

// istreamInlineLimits is the same cap as a trailing constructor argument, for
// IStreamInline — whose first parameter is the field callback. Empty in the
// fixed profile: its containers are statically bounded, so no cap is derived and
// the corelib-c-cpp IStreamInline takes the callback alone.
func (g *gen) istreamInlineLimits() string {
	if g.limBuffered <= 0 {
		return ""
	}
	return ", sofab::Limits{SOFAB_MAX_DYN_BUFFERED_FIELD}"
}

// resolveLimits fills the gen's limit fields from the max_dyn_* config keys and
// the schema's bounds (see the gen field comment).
func (g *gen) resolveLimits(s *ir.Schema, cfg map[string]any) {
	if g.clib {
		return // statically bounded (fixed containers); keys are inert
	}
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	if v, ok := cfgLimit(cfg, "max_dyn_array_count"); ok && b.HasDynArray {
		g.limArr, g.limArrHas = v, true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_string_len"); ok && b.HasDynString {
		g.limStr, g.limStrHas = v, true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_blob_len"); ok && b.HasDynBlob {
		g.limBlob, g.limBlobHas = v, true
	}
	// The reassembly cap is the largest byte span a single top-level field can
	// legitimately reach, so no message the per-field guards accept can trip it
	// (#228). Derived from the same cost walk as _maxSize, with the configured
	// caps standing in for the missing schema bounds. A field that is neither
	// schema-bounded nor covered by a configured cap has no legitimate maximum,
	// and the cap is one number for the whole stream — so rather than pick a
	// value that would reject valid traffic, none is emitted and reassembly stays
	// uncapped (as it is with no limits configured at all). Capping every dynamic
	// field kind the schema uses is what buys the bound.
	if g.anyLimit() {
		if v, ok := g.maxFieldSpan(s); ok {
			g.limBuffered = v
		}
	}
}

type hfile struct{ b strings.Builder }

func (f *hfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *hfile) blank()        { f.b.WriteByte('\n') }
func (f *hfile) bytes() []byte { return []byte(f.b.String()) }

func (g *gen) header(m *ir.Message) []byte {
	// reachable named types in post-order (children before parents)
	order := g.reachable(m)

	f := &hfile{}
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	f.line("#pragma once")
	f.line("#include <cstdint>")
	f.line("#include <string>")
	f.line("#include <vector>")
	f.line("#include <array>")
	// <span>/<cstring>/<cstddef> back the _trimTail fixed-count encode helper.
	f.line("#include <span>")
	f.line("#include <cstring>")
	f.line("#include <cstddef>")
	if g.needsSeqHelpers(m) {
		// <type_traits> backs the sofabgen:: wrapper-sequence helpers below.
		f.line("#include <type_traits>")
	}
	f.line("#include %q", "sofab/sofab.hpp")
	f.blank()
	f.line("static_assert(sofab::API_VERSION == 1,")
	f.line("    \"SofaBuffers: generated against C++ API v1, but the linked corelib differs.\");")
	f.blank()
	// Scalar-array and leaf helpers live in the corelib on BOTH C++ paths --
	// sofab::StringSeq / BlobSeq / trimTail in corelib-cpp, sofab::FixedStringSeq /
	// FixedBlobSeq / trimTail in corelib-c-cpp. The WRAPPER-ARRAY element helpers
	// do not: the schema `count` N is what decides placement, refill and the
	// trailing-run trim, and N is known here (CORELIB_PLAN §7 split), so they are
	// generated.
	g.emitRawArrayHelper(f, m)
	g.emitSeqHelpers(f, m)
	f.line("namespace %s {", g.ns)
	f.blank()

	// Receiver-side decode limits (generator#102), baked from the sofabgen config.
	// Macros (not inline constexpr) so multiple generated headers agree in one TU;
	// they govern only fields the schema left unbounded — exceeding one fails the
	// decode with sofab::Error::LimitExceeded (a policy cap, not INVALID).
	if g.anyLimit() {
		if g.limArrHas {
			f.line("#ifndef SOFAB_MAX_DYN_ARRAY_COUNT")
			f.line("#define SOFAB_MAX_DYN_ARRAY_COUNT %d", g.limArr)
			f.line("#endif")
		}
		if g.limStrHas {
			f.line("#ifndef SOFAB_MAX_DYN_STRING_LEN")
			f.line("#define SOFAB_MAX_DYN_STRING_LEN %d", g.limStr)
			f.line("#endif")
		}
		if g.limBlobHas {
			f.line("#ifndef SOFAB_MAX_DYN_BLOB_LEN")
			f.line("#define SOFAB_MAX_DYN_BLOB_LEN %d", g.limBlob)
			f.line("#endif")
		}
		if g.limBuffered > 0 {
			f.line("#ifndef SOFAB_MAX_DYN_BUFFERED_FIELD")
			f.line("#define SOFAB_MAX_DYN_BUFFERED_FIELD %d", g.limBuffered)
			f.line("#endif")
		}
		f.blank()
	}

	for _, key := range order {
		nt := g.schema.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfield(f, nt)
		}
	}
	for _, key := range order {
		nt := g.schema.Named[key]
		if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			g.emitStruct(f, g.typeName(key), nt.Summary, nt.Fields, false)
		}
	}
	g.emitStruct(f, exported(m.Name), m.Summary, m.Fields, true)
	f.line("} // namespace %s", g.ns)
	return f.bytes()
}

// needsSeqHelpers reports whether this header contains a wrapper array -- a
// string/blob/struct/union/nested-array array, which lowers to a sequence whose
// child ids are the element indexes. Only such a header needs the sofabgen::
// helper block; a schema of scalars and native arrays never names it.
func (g *gen) needsSeqHelpers(m *ir.Message) bool {
	has := func(fields []*ir.Field) bool {
		for _, fld := range fields {
			if fld.Kind == ir.KindArray && !isNativeArrayElem(fld.Elem) {
				return true
			}
		}
		return false
	}
	if has(m.Fields) {
		return true
	}
	for _, key := range g.reachable(m) {
		nt := g.schema.Named[key]
		if (nt.Category == ir.CatStruct || nt.Category == ir.CatUnion) && has(nt.Fields) {
			return true
		}
	}
	return false
}

// needsRawArray reports whether this header decodes an ENUM array through the
// corelib-c-cpp wrapper, which is the one case that needs the sofabgen::RawArray
// element view: the member's element type is the scoped enum (so JSON and the
// generated API stay value-typed) while the wire element is its backing integer.
// Every other native array's member element already IS the wire element type.
func (g *gen) needsRawArray(m *ir.Message) bool {
	if !g.clib {
		return false
	}
	has := func(fields []*ir.Field) bool {
		for _, fld := range fields {
			if fld.Kind == ir.KindArray && fld.Elem == ir.KindEnum {
				return true
			}
		}
		return false
	}
	if has(m.Fields) {
		return true
	}
	for _, key := range g.reachable(m) {
		nt := g.schema.Named[key]
		if (nt.Category == ir.CatStruct || nt.Category == ir.CatUnion) && has(nt.Fields) {
			return true
		}
	}
	return false
}

// emitRawArrayHelper writes sofabgen::RawArray, the element-level view an enum
// array's decode destination takes on the corelib-c-cpp leg.
//
// corelib-c-cpp is a DEFERRED decoder: is.read()/readArray() record the
// destination's ADDRESS and the C runtime writes the bytes after the field
// callback returns. So the destination must be the member itself; a temporary of
// the wire element type -- the shape the synchronous corelib-cpp leg uses -- would
// dangle. But the member's element type is the scoped enum, which the corelib's
// span read does not know how to tag.
//
// RawArray closes exactly that gap and nothing else: it reinterprets the
// ELEMENTS (a std::vector<Color>'s bytes ARE an array of Color's backing type),
// forwards resize()/size() to the member, and hands the whole thing to
// readArray, which keeps ownership of the tag check, the schema-bound check, the
// reset and the bind, in that order. It is emphatically NOT a cast of the
// CONTAINER: reinterpreting a std::vector as a std::array of its elements makes
// the vector's begin/end/capacity words the first elements, so wire bytes
// overwrite the begin pointer and the destructor frees a pointer partly
// assembled from the message.
func (g *gen) emitRawArrayHelper(f *hfile, m *ir.Message) {
	if !g.needsRawArray(m) {
		return
	}
	f.line("#ifndef SOFABGEN_RAW_ARRAY_HELPER")
	f.line("#define SOFABGEN_RAW_ARRAY_HELPER")
	f.line("/// Native-array element view shared by every sofabgen-generated header.")
	f.line("namespace sofabgen {")
	f.blank()
	f.line("/**")
	f.line(" * @brief Views a native array member as a sequence of its WIRE element type.")
	f.line(" *")
	f.line(" * An enum array's member elements are the scoped enum; the wire elements are")
	f.line(" * the enum's backing integer. The two have the same size and the same object")
	f.line(" * representation, so the member's own storage IS a valid destination -- which")
	f.line(" * matters because corelib-c-cpp binds a destination by ADDRESS and fills it")
	f.line(" * after the field callback returns, so a temporary would dangle.")
	f.line(" *")
	f.line(" * The view forwards `resize()`/`size()` to the member and exposes `data()`")
	f.line(" * as the wire element type, which is all `IStreamImpl::readArray` needs: it")
	f.line(" * keeps the tag check, the schema-count check, the reset and the bind, in")
	f.line(" * that order. The view itself is never used after readArray returns -- only")
	f.line(" * the member's storage stays bound.")
	f.line(" *")
	f.line(" * @tparam Container Destination container (the member).")
	f.line(" * @tparam Wire      Wire element type (the enum's backing integer).")
	f.line(" */")
	f.line("template <typename Container, typename Wire>")
	f.line("struct RawArray {")
	f.line("    using value_type = Wire;")
	f.line("    Container *out;  ///< The member this view writes through.")
	f.blank()
	f.line("    /** @brief Prepare the member for @p n elements, as readArray would itself. */")
	f.line("    void resize(std::size_t n) noexcept {")
	f.line("        if constexpr (requires { out->resize(n); }) { out->resize(n); }")
	f.line("        else { *out = Container{}; (void)n; }")
	f.line("    }")
	f.line("    std::size_t size() const noexcept { return out->size(); }")
	f.line("    Wire *data() noexcept { return reinterpret_cast<Wire *>(out->data()); }")
	f.line("    const Wire *data() const noexcept { return reinterpret_cast<const Wire *>(out->data()); }")
	f.line("    Wire *begin() noexcept { return data(); }")
	f.line("    Wire *end() noexcept { return data() + size(); }")
	f.line("    const Wire *begin() const noexcept { return data(); }")
	f.line("    const Wire *end() const noexcept { return data() + size(); }")
	f.line("};")
	f.blank()
	f.line("} // namespace sofabgen")
	f.line("#endif // SOFABGEN_RAW_ARRAY_HELPER")
	f.blank()
}

// emitSeqHelpers writes the wrapper-array element collector shared by every
// generated header.
//
// It is generated rather than taken from the corelib because the schema `count`
// N is what bounds the element index, and only the generator knows N
// (CORELIB_PLAN §7): corelib-c-cpp's MessageSeq/FixedMessageSeq append in
// arrival order because the C wrapper never learns the index means anything,
// which an id GAP -- ordinary since §2 made an interior all-default element
// sparse -- would turn into every later element shifted down by one.
//
// The block is emitted at global scope in its own `sofabgen` namespace and is
// macro-guarded, since several generated headers -- possibly with different
// `namespace` settings -- can be included into one translation unit.
func (g *gen) emitSeqHelpers(f *hfile, m *ir.Message) {
	if !g.needsSeqHelpers(m) {
		return
	}
	f.line("#ifndef SOFABGEN_WRAPPER_SEQ_HELPERS")
	f.line("#define SOFABGEN_WRAPPER_SEQ_HELPERS")
	f.line("/// Wrapper-array element helpers shared by every sofabgen-generated header.")
	f.line("namespace sofabgen {")
	f.blank()
	f.line("/**")
	f.line(" * @brief Collects an array of strings, blobs, structs, unions or rows, placing")
	f.line(" *        each element at the index its own id names.")
	f.line(" *")
	f.line(" * Such an array travels as a sequence whose child id IS the element's index, so")
	f.line(" * an element is stored at `dest[id]` -- never appended. The ids may have gaps:")
	f.line(" * an INTERIOR element equal to the element default is left off the wire, and the")
	f.line(" * gap it leaves is filled with that default. Appending instead would shorten the")
	f.line(" * array by the size of every gap, and would turn a repeated element id into a")
	f.line(" * second element instead of continuing the first one. The array's LAST element")
	f.line(" * is always on the wire, so the decoded length -- highest present id + 1 -- is")
	f.line(" * exact.")
	f.line(" *")
	f.line(" * The schema `count` N is a CAPACITY, not a length: it bounds the array -- an")
	f.line(" * index at or past N is rejected as a malformed message, before the container")
	f.line(" * grows, which also bounds the gap fill against an over-index amplification --")
	f.line(" * but it never adds an element the wire did not carry.")
	f.line(" *")
	f.line(" * @tparam Container Destination container; its `value_type` is the element type.")
	f.line(" */")
	f.line("template <typename Container>")
	f.line("struct WrapperSeq : sofab::IStreamMessage {")
	f.line("    using Elem = typename Container::value_type;")
	f.line("    Container *out = nullptr;  ///< Destination, bound by the generated read.")
	f.line("    long cap = -1;             ///< Schema `count`, or -1 when the array has none.")
	f.blank()
	f.line("    /**")
	f.line("     * @brief Empty the destination before collecting into it.")
	f.line("     *")
	f.line("     * The sequence IS the array's value, so a field id occurring twice replaces")
	f.line("     * the array rather than extending it. This runs only once the field is known")
	f.line("     * to be a sequence, so an occurrence skipped for a contradicting wire type")
	f.line("     * cannot wipe a valid earlier one.")
	f.line("     */")
	f.line("    void prepare() noexcept { if (out != nullptr) { out->clear(); } }")
	f.blank()
	f.line("    void deserialize(sofab::IStreamImpl &is, sofab::id id, std::size_t, std::size_t count) noexcept override {")
	f.line("        /* The two corelibs put the wire tag enum in different scopes; decltype")
	f.line("         * names neither. */")
	f.line("        using Tag = decltype(is.wire());")
	f.line("        if constexpr (std::is_base_of_v<sofab::IStreamMessage, Elem>) {")
	f.line("            /* An element whose wire type contradicts the declared one is skipped")
	f.line("             * exactly like an unknown id -- which means it must leave the")
	f.line("             * container untouched, so the decision comes before the fill below. */")
	f.line("            if (is.wire() != Tag::SequenceStart) { return; }")
	f.line("        }")
	f.line("        if (cap >= 0 && static_cast<long>(id) >= cap) { is.invalidate(); return; }")
	f.line("        while (out->size() <= static_cast<std::size_t>(id)) { (void)out->emplace_back(); }")
	f.line("        Elem &row = (*out)[static_cast<std::size_t>(id)];")
	f.line("        /* A row that is itself a count-less array is filled only up to its")
	f.line("         * current size, so size it to the row's element count first. Struct,")
	f.line("         * union and fixed-length rows have no resize(). */")
	f.line("        if constexpr (requires { row.resize(count); } && !std::is_base_of_v<sofab::IStreamMessage, Elem>) {")
	f.line("            row.resize(count);")
	f.line("        }")
	f.line("        is.read(row);")
	f.line("    }")
	f.line("};")
	f.blank()
	f.line("} // namespace sofabgen")
	f.line("#endif // SOFABGEN_WRAPPER_SEQ_HELPERS")
	f.blank()
}

func (g *gen) emitEnum(f *hfile, nt *ir.NamedType) {
	f.line("enum class %s : %s {", g.typeName(nt.Key), enumBacking(nt))
	for _, c := range nt.Consts {
		if doc := oneLineDoc(c.Description); doc != "" {
			f.line("    %s = %d,  ///< %s", exported(c.Name), c.Value, doc)
		} else {
			f.line("    %s = %d,", exported(c.Name), c.Value)
		}
	}
	f.line("};")
	f.blank()
}

func (g *gen) emitBitfield(f *hfile, nt *ir.NamedType) {
	f.line("enum %s : %s {", g.typeName(nt.Key), bitfieldBacking(nt))
	for _, fl := range nt.Flags {
		if doc := flagDoc(fl); doc != "" {
			f.line("    %s%s = %d,  ///< %s", g.typeName(nt.Key), exported(fl.Name), uint64(1)<<uint(fl.Pos), doc)
		} else {
			f.line("    %s%s = %d,", g.typeName(nt.Key), exported(fl.Name), uint64(1)<<uint(fl.Pos))
		}
	}
	f.line("};")
	f.blank()
}

// oneLineDoc collapses a possibly multi-line description to a single line (joined
// with spaces) for a trailing ///< comment, which cannot span lines.
func oneLineDoc(s string) string {
	return strings.Join(strings.Split(s, "\n"), " ")
}

// flagDoc builds a bitfield flag's trailing-doc text: its description, plus a
// "(default: true/false)" note when the flag carries a schema default. Returns
// "" when the flag has neither.
func flagDoc(fl *ir.BitfieldFlag) string {
	desc := oneLineDoc(fl.Description)
	if !fl.HasDefault {
		return desc
	}
	note := "(default: false)"
	if fl.Default {
		note = "(default: true)"
	}
	if desc != "" {
		return desc + " " + note
	}
	return note
}

// emitReset writes reset(): every field back to its declared default, in place.
//
// MESSAGE_SPEC §2 omits a field whose value equals its default — a sequence-typed
// one included — so an absent field delivers no deserialize() callback at all and
// leaves whatever the previous message put in that member. Decoding into a REUSED
// destination therefore has to clear it where absence is still observable: at the
// start of the decode. It cannot be done from the callback side — the §7.4
// "a later occurrence replaces the array whole" clear lives in the wrapper
// collectors (StringSeq/BlobSeq/MessageSeq::prepare) and hangs off the
// SequenceStart tag, which an omitted field never sends. That clear stays exactly
// as it is; this is the other half.
//
// Each member is assigned its DECLARATION default, which is by construction the
// value an absent field decodes to, so the two cannot drift apart.
//
// In place on purpose: std::string/std::vector assignment reuses the buffer the
// destination already holds (and the fixed profile's FixedString/FixedBytes/
// InlineVector have no buffer to hand back at all), whereas `out = P{}` would
// return every allocation only to take it again on the next message — which is
// the whole point of a reuse API. Struct and union members recurse into their own
// reset() rather than being assigned a fresh temporary, for the same reason.
//
// Public because a caller driving the visitor itself (sofab::IStreamInline, or a
// bare sofab::IStreamImpl) owns its destinations and needs the same call between
// messages; corelib-cpp exposes the decoder-state half as IStreamImpl::reset().
func (g *gen) emitReset(f *hfile, fields []*ir.Field, isMessage bool) {
	f.line("    /**")
	f.line("     * @brief Put every field back to its declared default, in place.")
	f.line("     *")
	f.line("     * A field whose value equals its default is absent from the encoded")
	f.line("     * bytes, so nothing runs for it on decode and a destination decoded into")
	f.line("     * twice keeps the earlier message's value - the elements of an array")
	f.line("     * field included, since an array is only cleared when its sequence is")
	f.line("     * actually present. The clear therefore has to happen before the bytes")
	f.line("     * are fed, not from a callback an absent field never fires.")
	f.line("     *")
	// The reference only resolves on a type that HAS a try_decode, and only says
	// something true on the profile whose try_decode decodes into the caller's
	// destination; the fixed profile's stages in a fresh instance instead.
	if isMessage && !g.clib {
		f.line("     * @ref try_decode calls this for you. Drive a stream yourself (e.g.")
		f.line("     * @c sofab::IStreamInline) and it is yours to call between messages,")
		f.line("     * alongside the stream's own reset for the decoder state.")
	} else {
		f.line("     * Drive a stream yourself (e.g. @c sofab::IStreamInline) and this is")
		f.line("     * yours to call between messages, alongside the stream's own reset for")
		f.line("     * the decoder state.")
	}
	f.line("     *")
	f.line("     * Containers are cleared, not reallocated: the capacity a reused")
	f.line("     * destination has already paid for is kept.")
	f.line("     */")
	f.line("    void reset() noexcept {")
	for _, fld := range fields {
		acc := cppIdent(fld.Name)
		switch fld.Kind {
		case ir.KindStruct, ir.KindUnion:
			// Recurse: the nested default is that type's own declaration state, and
			// reaching it in place keeps the nested containers' capacity too.
			f.line("        %s.reset();", acc)
		default:
			f.line("        %s = %s;", acc, g.cppDefault(fld))
		}
	}
	f.line("    }")
	f.blank()
}

func (g *gen) emitStruct(f *hfile, name, summary string, fields []*ir.Field, isMessage bool) {
	emitStructDoc(f, summary)

	// A [[deprecated]] member is touched by the implicitly-defined special member
	// functions — destructor, copy/move constructor, assignment — as well as by
	// the generated serialize/deserialize. Those implicit definitions are located
	// AT THE CLASS, so a consumer that merely declares a message value gets a
	// deprecation warning for a field it never named, from a header line it
	// cannot edit. That devalues the attribute: it fires for everyone instead of
	// for the one caller still using the field.
	//
	// So the suppression spans the whole class definition. The attribute stays on
	// the member, so `msg.oldField` in a consumer's code still warns — at the
	// consumer's own line, which is the point of marking it deprecated.
	hasDeprecated := false
	for _, fld := range fields {
		if fld.Deprecated {
			hasDeprecated = true
			break
		}
	}
	if hasDeprecated {
		f.line("#pragma GCC diagnostic push")
		f.line("#pragma GCC diagnostic ignored \"-Wdeprecated-declarations\"")
	}
	// sofab::Message is exactly the OStreamMessage + IStreamMessage pair (an empty
	// intermediate base, same layout). Both corelibs define it, so both profiles
	// use it.
	f.line("struct %s : sofab::Message {", name)
	// Declare members widest-first to minimise padding; encode/decode below stay
	// in schema/id order, so the wire bytes are unchanged.
	for _, fld := range ir.SortedForLayout(fields) {
		attr := ""
		if fld.Deprecated {
			attr = "[[deprecated]] "
		}
		doc := fieldDoc(fld)
		if fld.Deprecated {
			if doc != "" {
				doc += " @deprecated"
			} else {
				doc = "@deprecated"
			}
		}
		if doc != "" {
			f.line("    %s%s %s = %s;  ///< %s", attr, g.cppType(fld), cppIdent(fld.Name), g.cppDefault(fld), doc)
		} else {
			f.line("    %s%s %s = %s;", attr, g.cppType(fld), cppIdent(fld.Name), g.cppDefault(fld))
		}
	}
	if isMessage {
		ms := g.messageSize(name, fields)
		if !ms.Bounded {
			f.line("    /// Configured ceiling (max_message_size): an unbounded field means this")
			f.line("    /// size is imposed, not derived from the schema.")
			f.line("    static constexpr std::size_t _maxSizeLimit = %d;", ms.Size)
			f.line("    static constexpr std::size_t _maxSize = _maxSizeLimit;")
		} else {
			f.line("    static constexpr std::size_t _maxSize = %d;", ms.Size)
		}
	}
	f.blank()

	// The default constructor is defaulted explicitly so its definition is located
	// inside this class — an implicit one first instantiated inside a corelib
	// template would be diagnosed there, out of reach of any pragma here.
	if hasDeprecated {
		f.line("    %s() = default;", name)
		f.blank()
	}

	g.emitReset(f, fields, isMessage)

	// per-message encode()/decode() (members so multiple messages don't clash).
	if isMessage {
		// encode(): one allocation, no copy. The vector is created at the
		// schema's worst case, serialized into directly, then shrunk to what was
		// actually written — resize() downwards never reallocates, so the bytes
		// stay put and the vector is returned by move. Staging in an
		// OStreamInline<_maxSize> first would put the worst case on the stack as
		// well and then copy it across.
		f.line("    /**")
		f.line("     * @brief Encode this message into a new byte vector.")
		f.line("     * @return The encoded bytes (empty if the message encodes to nothing).")
		f.line("     */")
		f.line("    std::vector<std::uint8_t> encode() const {")
		f.line("        std::vector<std::uint8_t> out(_maxSize);")
		f.line("        sofab::OStreamView os{out.data(), out.size()};")
		f.line("        serialize(os);")
		f.line("        out.resize(os.bytesUsed());")
		f.line("        return out;")
		f.line("    }")
		// encodeTo(): the same, into storage the caller already has — no
		// allocation at all. Returns 0 if the message does not fit in cap, in
		// which case dst holds however much was written before that was found out.
		f.line("    /**")
		f.line("     * @brief Encode this message into caller-provided storage (no allocation).")
		f.line("     * @param dst Destination buffer.")
		f.line("     * @param cap Capacity of @p dst in bytes.")
		f.line("     * @return Bytes written, or 0 if the message does not fit in @p cap;")
		f.line("     *         in which case @p dst holds however much was written first.")
		f.line("     */")
		f.line("    std::size_t encodeTo(std::uint8_t *dst, std::size_t cap) const noexcept {")
		f.line("        sofab::OStreamView os{dst, cap};")
		f.line("        serialize(os);")
		f.line("        if (!os.ok()) { return 0; }")
		f.line("        return os.bytesUsed();")
		f.line("    }")
		// Infallible, best-effort decode: kept for back-compat. It discards feed's
		// Result and always returns a value, so it can never reject malformed input
		// — prefer try_decode when the accept/reject verdict matters.
		f.line("    /**")
		f.line("     * @brief Decode a message, best effort.")
		f.line("     *")
		f.line("     * Never reports failure: malformed input yields whatever was decoded")
		f.line("     * before the error. Use @ref try_decode when the verdict matters.")
		f.line("     *")
		f.line("     * @param data Encoded bytes.")
		f.line("     * @param len  Number of bytes at @p data.")
		f.line("     * @return The decoded message.")
		f.line("     */")
		f.line("    static %s decode(const std::uint8_t *data, std::size_t len) {", name)
		f.line("        sofab::IStreamObject<%s> in%s;", name, g.istreamLimits())
		f.line("        in.feed(data, len);")
		f.line("        return *in;")
		f.line("    }")
		f.blank()
		// Fallible decode: surfaces the corelib's accept/reject decision. feed()
		// detects malformed input and returns Error::InvalidMessage, but the
		// infallible decode above drops it, so the public C++ API could otherwise
		// never reject (generator#83).
		//
		// Two shapes, because the destination means something different in each
		// profile — and MESSAGE_SPEC §2 is what makes the difference matter. An
		// all-default field is now absent from the bytes, so a destination that is
		// DECODED INTO keeps the previous message's value for every field these
		// bytes do not carry, a wrapper array included (its collector clears on the
		// sequence header, which an absent field never sends). Only the start of the
		// decode can clear that, which is what reset() is for.
		//
		//   heap profile (corelib-cpp): `out` IS the destination. An IStreamInline
		//   binds this message's deserialize, so nothing is staged in a second
		//   instance and copied across — which is the point of passing a destination
		//   in (the bench decode row and the JSON harness both feed one in a loop),
		//   and why the reset has to be here. The stream reaches the callback through
		//   a pointer filled in right after construction: IStreamInline takes its
		//   callback by value, so the lambda cannot capture the object it is being
		//   handed to.
		//
		//   fixed profile (corelib-c-cpp): decode into a freshly constructed
		//   IStreamObject and copy the result over `out`. That is a memcpy of inline
		//   storage — no allocation to hand back, nothing to reuse — and it cannot go
		//   stale, because the destination it decodes into starts at the declared
		//   defaults every time. It stays this way for footprint: this profile's
		//   IStreamObject dispatches through a C-ABI function pointer, whereas
		//   IStreamInline holds a std::function, so routing the decode through a
		//   callback here would put that machinery in .text on the targets that have
		//   the least of it. reset() is emitted all the same, for a caller driving
		//   the stream itself.
		f.line("    /**")
		if g.clib {
			f.line("     * @brief Decode a message, reporting whether the input was acceptable.")
			f.line("     *")
			f.line("     * Decodes into a fresh instance and copies it over @p out on success,")
			f.line("     * so @p out never carries anything over from an earlier message. To")
			f.line("     * decode into a destination directly, drive the stream yourself and")
			f.line("     * call @ref reset on it between messages.")
			f.line("     *")
			f.line("     * @param data Encoded bytes.")
			f.line("     * @param len  Number of bytes at @p data.")
			f.line("     * @param out  Receives the message on success; untouched otherwise.")
			f.line("     * @return The decode result; check @c ok() before reading @p out.")
			f.line("     */")
			f.line("    static sofab::IStreamImpl::Result try_decode(const std::uint8_t *data, std::size_t len, %s &out) {", name)
			f.line("        sofab::IStreamObject<%s> in%s;", name, g.istreamLimits())
			f.line("        sofab::IStreamImpl::Result r = in.feed(data, len);")
			f.line("        if (r.ok()) { out = *in; }")
			f.line("        return r;")
			f.line("    }")
		} else {
			f.line("     * @brief Decode a message into @p out, reporting whether the input was")
			f.line("     *        acceptable.")
			f.line("     *")
			f.line("     * @p out is put back to its declared defaults first (@ref reset) and")
			f.line("     * then decoded into directly, so it may be reused across messages")
			f.line("     * without carrying anything over and without giving its buffers back.")
			f.line("     *")
			f.line("     * @param data Encoded bytes.")
			f.line("     * @param len  Number of bytes at @p data.")
			f.line("     * @param out  Receives the message; on a rejected input it holds the")
			f.line("     *             fields decoded before the error, never an older message's.")
			f.line("     * @return The decode result; check @c ok() before reading @p out.")
			f.line("     */")
			f.line("    static sofab::IStreamImpl::Result try_decode(const std::uint8_t *data, std::size_t len, %s &out) {", name)
			f.line("        out.reset();")
			f.line("        sofab::IStreamInline *_isp = nullptr;")
			f.line("        sofab::IStreamInline _is{[&out, &_isp](sofab::id _id, std::size_t _size, std::size_t _count) {")
			f.line("            out.deserialize(*_isp, _id, _size, _count);")
			f.line("        }%s};", g.istreamInlineLimits())
			f.line("        _isp = &_is;")
			f.line("        return _is.feed(data, len);")
			f.line("    }")
		}
		f.blank()
	}

	g.emitIsDefault(f, fields)

	// serialize: each write is a statement (Result is non-assignable, used only
	// for chaining); return a no-op writeIf so the signature is satisfied.
	f.line("    /**")
	f.line("     * @brief Write this message's fields to an output stream.")
	f.line("     *")
	f.line("     * Called by @ref encode / @ref encodeTo, and directly when writing into a")
	f.line("     * stream you own. Fields equal to their default are omitted.")
	f.line("     *")
	f.line("     * @param os Stream to write to.")
	f.line("     * @return The result of the writes.")
	f.line("     */")
	f.line("    sofab::OStreamImpl::Result serialize(sofab::OStreamImpl &os) const noexcept override {")
	for _, fld := range fields {
		g.emitSerialize(f, fld)
	}
	// No-op write purely to return a Result; bool is never feature-gated (avoid a
	// 64-bit literal so the body compiles under SOFAB_DISABLE_INT64_SUPPORT).
	f.line("        return os.writeIf(0, false, false);")
	f.line("    }")
	f.blank()

	// deserialize: unhandled ids are auto-skipped by the driver (no-op default).
	// In clib mode the field length (_size) pre-sizes variable-length targets;
	// the pure path needs it for the unbounded string/blob limit guards (#102).
	sizeParam := "std::size_t"
	if g.clib {
		sizeParam = "std::size_t _size"
	} else {
		for _, fld := range fields {
			// _size is read by the unbounded #102 limit guard (no maxlen + a
			// configured cap) and by the bounded schema-maxlen reject (MESSAGE_SPEC
			// S7.1) — both string and blob.
			if (fld.Kind == ir.KindString || fld.Kind == ir.KindBlob) &&
				(fld.HasMaxlen || (!fld.HasMaxlen && (g.limStrHas && fld.Kind == ir.KindString || g.limBlobHas && fld.Kind == ir.KindBlob))) {
				sizeParam = "std::size_t _size"
				break
			}
		}
	}
	// The wire element count (_count) is needed for the over-count guard on
	// count-bearing native arrays (generator#100), the limit guard on count-less
	// ones (#102), and to size a native vector before its span read (#112). With
	// inline storage the C runtime rejects a count/capacity mismatch itself, so
	// the parameter stays unnamed; with dynamic storage the vector has no
	// capacity to mismatch, so the generator checks the count and sizes to it.
	countParam := "std::size_t"
	for _, fld := range fields {
		if fld.Kind != ir.KindArray || !isNativeArrayElem(fld.Elem) {
			continue
		}
		if g.clib {
			// readArray takes the wire count in both storage modes: it bounds the
			// count before a dynamic resize, and is what a fixed array is filled to.
			countParam = "std::size_t _count"
			break
		}
		if fld.HasCount || g.limArrHas || g.dynNativeArray(fld.Elem) {
			countParam = "std::size_t _count"
			break
		}
	}
	f.line("    /**")
	f.line("     * @brief Bind one decoded field to its member.")
	f.line("     *")
	f.line("     * Called once per field as the stream is fed. An id this message does")
	f.line("     * not know, or one whose wire type contradicts the member's, binds")
	f.line("     * nothing and is skipped.")
	f.line("     *")
	f.line("     * @param is Stream delivering the field.")
	f.line("     * @param id Field identifier.")
	f.line("     */")
	f.line("    void deserialize(sofab::IStreamImpl &is, sofab::id id, %s, %s) noexcept override {", sizeParam, countParam)
	f.line("        switch (id) {")
	for _, fld := range fields {
		f.line("        case %d:", fld.ID)
		// Frame each field by its header wire type before reading (MESSAGE_SPEC
		// §7.3): a contradicting field is skipped, exactly like an unknown id.
		//
		// On the pure-corelib-cpp path the corelib now decides this inside the typed
		// read itself (the seam, docs/models/type-reconciliation.md), so no guard is
		// emitted for a scalar, fixlen or struct/union field. It is still emitted
		// where the decision has to precede a side effect the arm performs — see
		// cppNeedsWireGuard.
		if cppNeedsWireGuard(fld, g.clib) {
			f.line("            if (%s) break;", cppWireGuard(fld))
		}
		g.emitDeserialize(f, fld)
		f.line("            break;")
	}
	f.line("        default: break;")
	f.line("        }")
	f.line("    }")
	f.line("};")
	if hasDeprecated {
		f.line("#pragma GCC diagnostic pop")
	}
	f.blank()
}

// emitStructDoc writes a Doxygen @brief block before a struct, when the summary
// is non-empty. Single-line summaries become a one-line /** @brief ... */;
// multi-line summaries expand to a starred block, one comment line per source
// line. UTF-8 passes through byte-for-byte.
func emitStructDoc(f *hfile, summary string) {
	if summary == "" {
		return
	}
	// Neutralise a comment terminator so a summary containing "*/" cannot close
	// the /** ... */ block early (the trailing member ///< form is a line comment
	// and needs no such guard).
	summary = strings.ReplaceAll(summary, "*/", "* /")
	lines := strings.Split(summary, "\n")
	if len(lines) == 1 {
		f.line("/** @brief %s */", lines[0])
		return
	}
	f.line("/**")
	f.line(" * @brief %s", lines[0])
	for _, ln := range lines[1:] {
		f.line(" * %s", ln)
	}
	f.line(" */")
}

// fieldDoc builds the trailing member-doc text from a field's Description and
// Unit. Multi-line descriptions collapse to one line (joined with spaces) since
// a trailing ///< comment cannot span lines. Returns "" when both are empty.
func fieldDoc(fld *ir.Field) string {
	desc := strings.Join(strings.Split(fld.Description, "\n"), " ")
	switch {
	case desc != "" && fld.Unit != "":
		return fmt.Sprintf("%s (unit: %s)", desc, fld.Unit)
	case desc != "":
		return desc
	case fld.Unit != "":
		return fmt.Sprintf("(unit: %s)", fld.Unit)
	default:
		return ""
	}
}

// emitIsDefault writes the object's all-default predicate. It is the exact
// negation of what serialize writes: the object is default iff serialize would
// emit no child at all, evaluated per field and recursively (MESSAGE_SPEC §2).
//
// It is the explicit form of the predicate lazy framing applies implicitly
// ("not one child was written"), generated from the very same per-field
// expressions the writer uses so the two cannot drift apart. Generated code no
// longer needs it -- an INTERIOR sequence element is now dropped by its closer,
// which answers the same question after the fact -- but it stays part of the
// generated surface: it is the one place a caller can ask "would this object
// reach the wire at all?", and to_json/from_json round trips lean on it.
//
// Keep this in lockstep with emitSerialize: a predicate that disagrees with the
// writer either drops a non-default element or keeps a default one.
func (g *gen) emitIsDefault(f *hfile, fields []*ir.Field) {
	f.line("    /**")
	f.line("     * @brief True when every field still holds its declared default.")
	f.line("     *")
	f.line("     * The exact negation of @ref serialize: an object is default when")
	f.line("     * serialize would write nothing at all for it, tested per field and")
	f.line("     * recursively. Used to find the last non-default element of an array")
	f.line("     * declared with a `count`, whose encoding stops one past it.")
	f.line("     */")
	f.line("    bool _isDefault() const noexcept {")
	for _, fld := range fields {
		f.line("        if (!(%s)) { return false; }", g.fieldIsDefaultExpr(fld))
	}
	f.line("        return true;")
	f.line("    }")
	f.blank()
}

// fieldIsDefaultExpr is the boolean expression "this field equals its default",
// i.e. the negation of emitSerialize's write guard for the same field.
func (g *gen) fieldIsDefaultExpr(fld *ir.Field) string {
	acc := cppIdent(fld.Name)
	switch fld.Kind {
	case ir.KindBlob:
		return fmt.Sprintf("%s == %s%s", acc, g.cppType(fld), g.cppDefault(fld))
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed: the frame survives iff the nested serialize wrote a
		// child, which is exactly "the nested object is not default".
		return fmt.Sprintf("%s._isDefault()", acc)
	case ir.KindArray:
		// A declared `count: N` takes no part in either test. `count` is a
		// CAPACITY, never a length (MESSAGE_SPEC §3): it never reaches the wire,
		// so the value is compared against the declared default exactly as
		// written, with neither side padded out to N.
		if isNativeArrayElem(fld.Elem) {
			// The same expression emitSerializeArray guards the whole field with.
			// Note a std::array<T, N> is not a length-carrying container: its
			// value IS N elements, so its default is the N-element default array
			// and comparing against it is the whole test.
			def := g.cppArrayContainer(fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.ElemMaxHas, fld.ElemMax) + g.cppDefault(fld)
			return fmt.Sprintf("%s == %s", acc, def)
		}
		// Wrapper array: the writer emits a child for every element it holds,
		// because the LAST element is written whatever its value (§2) -- so "no
		// child is written" is exactly "the array is empty", and the predicate
		// and the writer cannot drift apart.
		return fmt.Sprintf("%s.size() == 0", acc)
	}
	return fmt.Sprintf("%s == %s", acc, g.cppDefault(fld))
}

func (g *gen) emitSerialize(f *hfile, fld *ir.Field) {
	acc := cppIdent(fld.Name)
	var write string
	switch fld.Kind {
	// Write each integer at its natural width (not a forced 64-bit cast): the
	// varint output is value-based so the bytes are identical, and it lets the
	// corelib-c-cpp wrapper compile with SOFAB_DISABLE_INT64_SUPPORT for messages
	// that have no u64/i64 field (a u64/i64 field still requires INT64, correctly).
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		write = fmt.Sprintf("(void)os.write(%d, %s);", fld.ID, acc)
	case ir.KindBool:
		write = fmt.Sprintf("(void)os.write(%d, %s);", fld.ID, acc)
	case ir.KindFP32, ir.KindFP64:
		write = fmt.Sprintf("(void)os.write(%d, %s);", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("(void)os.write(%d, %s);", fld.ID, acc)
	case ir.KindEnum:
		write = fmt.Sprintf("(void)os.write(%d, static_cast<%s>(%s));", fld.ID, enumBacking(fld.Ref.Target), acc)
	case ir.KindBitfield:
		write = fmt.Sprintf("(void)os.write(%d, %s);", fld.ID, acc)
	case ir.KindBlob:
		// A blob is a leaf: sparse-canonical encoding (MESSAGE_SPEC S2) omits it
		// when it equals its default (empty if none). The decoder reconstructs the
		// omitted blob from the member's construction default.
		f.line("        if (%s != %s%s) { (void)os.write(%d, %s.data(), static_cast<std::int32_t>(%s.size())); }", acc, g.cppType(fld), g.cppDefault(fld), fld.ID, acc, acc)
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so writeLazy() opens the frame lazily -- the corelib writes the
		// header only once a child field appears. The nested serialize omits each
		// child that equals its default, so "no child was written" IS "the object
		// equals its declared default", per field and recursively. An all-default
		// nested object is dropped, not emitted as an empty wrapper.
		f.line("        (void)os.writeLazy(%d, %s);", fld.ID, acc)
		return
	case ir.KindArray:
		g.emitSerializeArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield leaf: always omit when equal to the default.
	// Sparse encoding is canonical (MESSAGE_SPEC S2); the decoder reconstructs the
	// omitted field from its member construction default.
	f.line("        if (%s != %s) { %s }", acc, g.cppDefault(fld), write)
}

func (g *gen) emitSerializeArray(f *hfile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf: omit the whole field when it equals its
	// default (materialized at construction). A composite/dynamic-element array is
	// a wrapper sequence, opened lazily and closed with the dropping end
	// (MESSAGE_SPEC §2), so an EMPTY one is omitted rather than framed empty.
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written -- neither side padded out
	// to N -- and against the empty collection when no default is declared.
	if isNativeArrayElem(fld.Elem) {
		def := g.cppArrayContainer(fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.ElemMaxHas, fld.ElemMax) + g.cppDefault(fld)
		f.line("        if (%s != %s) {", acc, def)
		g.serializeArray(f, "            ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, 0, "")
		f.line("        }")
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today, so absent and
	// explicitly-empty denote the same value. If that gap is ever closed, this call
	// needs a guard -- `if (value != default) { ... sequenceEndKeep(); }` -- so that
	// a value differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC S2, S3).
	g.serializeArray(f, "        ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, 0, "")
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over an array of nv elements.
//
// It is the whole of the positional half of MESSAGE_SPEC §2's element rule. A
// wrapper array carries no length field: its decoded length is *highest present
// id + 1* (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length, and nothing that carries the length may be
// elided. Everything before it may be: an interior element equal to the element
// default is indistinguishable from an absent one, because the decoder restores
// an absent id from that same default. Hence: interior sparse, last always
// written.
//
// A declared `count: N` changes nothing here. N is a capacity, not a length
// (§3), so it can never restore an elided tail -- the same test applies with or
// without one.
//
// The loop body only runs with nv >= 1, but the comparison is written additively
// so the unsigned nv - 1 never appears.
func lastElemExpr(iv, nv string) string {
	return fmt.Sprintf("%s + 1 == %s", iv, nv)
}

// emitSeqEnd closes the wrapper sequence opened at ind, choosing between the two
// closers the corelib offers. Every sequence is opened LAZILY (the corelib holds
// the header back until a child is written), so the closer alone decides whether
// a contentless one survives: sequenceEnd drops it, sequenceEndKeep forces the
// empty frame out.
//
// keepIf is the condition under which an empty frame must survive:
//   - "" -- never. A sequence-typed FIELD (an array wrapper): an all-default one
//     is omitted and absence reconstructs it (§2).
//   - a lastElemExpr -- a sequence-form array ELEMENT, kept only at the array's
//     last index. In the interior it is dropped and leaves an id GAP, which is
//     what makes an all-default element sparse like any other default value.
//     Note this is decided from the position in the VALUE, at run time; the
//     schema cannot answer it.
func emitSeqEnd(f *hfile, ind, keepIf string) {
	if keepIf == "" {
		f.line("%s(void)os.sequenceEnd();", ind)
		return
	}
	f.line("%sif (%s) { (void)os.sequenceEndKeep(); } else { (void)os.sequenceEnd(); }", ind, keepIf)
}

// serializeArray writes an array value as field idExpr, mirroring the Go/Python
// backends: numeric/bitfield elements use the native array wire type directly;
// enum (->signed) and boolean (->0/1 unsigned) are value-converted through a
// temporary native array; string/blob/struct/union/nested-array elements lower
// to a wrapper sequence whose child ids are the 0-based index. Recurses for
// nested arrays, depth-suffixing loop vars to avoid collisions.
//
// Every element the value holds is written -- no trailing run is elided, of
// either element kind, because the wire count IS a compact array's length (§3)
// and the highest wrapper id IS its last index (§5.1). What the interior may
// drop is a value that is indistinguishable from absence, and only that.
//
// keepIf is the closer this call's own wrapper takes (see emitSeqEnd); the
// native element kinds open no sequence and ignore it.
func (g *gen) serializeArray(f *hfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, depth int, keepIf string) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	tv := fmt.Sprintf("_t%d", depth)
	nv := fmt.Sprintf("_n%d", depth)
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBitfield:
		f.line("%s(void)os.write(%s, %s);", ind, idExpr, val)
	case ir.KindEnum:
		// The enum values are converted through a native-typed temporary before the
		// write, element for element. The temp is the value's LENGTH long, never
		// the schema `count`: `count` is a capacity and the wire count is the
		// length (§3), so padding the temp out to N would put N elements on the
		// wire for a shorter value. The temp takes the member's own container form,
		// so the heap-free profile stays heap-free here too.
		f.line("%s{ %s %s; %s.resize(%s.size()); for (std::size_t %s = 0; %s < %s.size(); ++%s) %s[%s] = static_cast<%s>(%s[%s]); (void)os.write(%s, %s); }",
			ind, g.nativeTemp(enumBacking(ref.Target), count), tv, tv, val, iv, iv, val, iv, tv, iv, enumBacking(ref.Target), val, iv, idExpr, tv)
	case ir.KindBool:
		if g.clib {
			// On the c-cpp leg the element already IS the wire's std::uint8_t (see
			// cppArrayElem), so there is nothing to convert: the member is written
			// directly, exactly like a numeric array. The bytes are the same ones
			// the bool->0/1 temporary produced.
			f.line("%s(void)os.write(%s, %s);", ind, idExpr, val)
		} else {
			f.line("%s{ std::vector<std::uint8_t> %s(%s.size()); for (std::size_t %s = 0; %s < %s.size(); ++%s) %s[%s] = %s[%s] ? 1 : 0; (void)os.write(%s, %s); }",
				ind, tv, val, iv, iv, val, iv, tv, iv, val, iv, idExpr, tv)
		}
	case ir.KindBlob:
		// A blob element is a leaf: in the array's INTERIOR it is omitted when it
		// equals the element default (empty), leaving an id gap the decoder
		// restores from that same default -- the ordinary sparse-field rule of
		// MESSAGE_SPEC §2, applied to an element. The index still advances on an
		// omitted element, so the surviving ids stay aligned. At the LAST index it
		// is written whatever its value: see lastElemExpr.
		f.line("%s(void)os.sequenceBeginLazy(%s);", ind, idExpr)
		f.line("%s{ const std::size_t %s = %s.size(); for (std::size_t %s = 0; %s < %s; ++%s) { const auto &%s = %s[%s]; if (!%s.empty() || %s) { (void)os.write(static_cast<sofab::id>(%s), %s.data(), static_cast<std::int32_t>(%s.size())); } } }",
			ind, nv, val, iv, iv, nv, iv, ev, val, iv, ev, lastElemExpr(iv, nv), iv, ev, ev)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindString:
		// A string element is a leaf, exactly like the blob element above.
		f.line("%s(void)os.sequenceBeginLazy(%s);", ind, idExpr)
		f.line("%s{ const std::size_t %s = %s.size(); for (std::size_t %s = 0; %s < %s; ++%s) { const auto &%s = %s[%s]; if (!%s.empty() || %s) { (void)os.write(static_cast<sofab::id>(%s), %s); } } }",
			ind, nv, val, iv, iv, nv, iv, ev, val, iv, ev, lastElemExpr(iv, nv), iv, ev)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above --
		// one rule for both kinds -- and the lazily-held frame is where it is
		// applied. Both corelib calls run the nested serialize, which omits every
		// child equal to its default, so "no child was written" IS "the element
		// equals its declared default"; only the CLOSER differs. writeLazy is the
		// dropping one, taken in the interior, where an all-default element
		// vanishes into an id gap; write is the keeping one, taken at the last
		// index, where it survives as an empty frame because that presence is what
		// fixes the array's length.
		f.line("%s(void)os.sequenceBeginLazy(%s);", ind, idExpr)
		f.line("%s{ const std::size_t %s = %s.size(); for (std::size_t %s = 0; %s < %s; ++%s) { if (%s) { (void)os.write(static_cast<sofab::id>(%s), %s[%s]); } else { (void)os.writeLazy(static_cast<sofab::id>(%s), %s[%s]); } } }",
			ind, nv, val, iv, iv, nv, iv, lastElemExpr(iv, nv), iv, val, iv, iv, val, iv)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindArray:
		f.line("%s(void)os.sequenceBeginLazy(%s);", ind, idExpr)
		f.line("%s{ const std::size_t %s = %s.size(); for (std::size_t %s = 0; %s < %s; ++%s) { const auto &%s = %s[%s];",
			ind, nv, val, iv, iv, nv, iv, ev, val, iv)
		if isNativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its
			// own, so the rule lands on the WRITE rather than on a closer: an
			// interior row equal to the element default is not written at all, and
			// the last row always is. The element default is the row container's own
			// default value -- empty for a std::vector row, and the N element
			// defaults for a fixed std::array row, which has no shorter value.
			inner := g.cppArrayContainer(items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas, items.ElemMax)
			f.line("%s    if (%s != %s{} || %s) {", ind, ev, inner, lastElemExpr(iv, nv))
			g.serializeArray(f, ind+"        ", fmt.Sprintf("static_cast<sofab::id>(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, items.Count, depth+1, "")
			f.line("%s    }", ind)
		} else {
			// A wrapper row has its own frame, so it takes the closer instead -- the
			// same interior/last choice, expressed the same way as for a struct
			// element above.
			g.serializeArray(f, ind+"    ", fmt.Sprintf("static_cast<sofab::id>(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, items.Count, depth+1, lastElemExpr(iv, nv))
		}
		f.line("%s} }", ind)
		emitSeqEnd(f, ind, keepIf)
	}
}

func (g *gen) emitDeserialize(f *hfile, fld *ir.Field) {
	acc := cppIdent(fld.Name)
	switch fld.Kind {
	case ir.KindString:
		// corelib-c-cpp's read() fills the existing buffer, so pre-size from the
		// field length; a zero-length string binds no target (read_field asserts
		// varlen > 0), so skip the read and leave it empty. corelib-cpp resizes
		// for us, so an empty string is fine there.
		// Unbounded string under a configured receiver-side cap: reject the
		// claimed length before the read — a policy rejection, LimitExceeded,
		// never INVALID and never a truncating read (generator#102).
		if !g.clib && !fld.HasMaxlen && g.limStrHas {
			f.line("            if (_size > SOFAB_MAX_DYN_STRING_LEN) { is.exceedLimit(); return; }")
		}
		if g.fixed {
			// Both storage modes emit the same call: readString establishes the
			// delivered type before it touches the destination (§7.3) and rejects
			// past the maxlen before sizing it (§7.1) — so the arm is identical
			// whether the destination is a FixedString or a std::string, and the
			// bound is enforced the same way in both.
			f.line("            is.readString(%s, _size, %d);", acc, maxlenOr(fld.HasMaxlen, fld.Maxlen))
		} else if g.clib {
			f.line("            %s.assign(_size, '\\0'); if (_size) is.read(%s);", acc, acc)
		} else if fld.HasMaxlen {
			// readString declares the fixlen SUBTYPE, so a contradicting one (a blob,
			// an fp64) is skipped by the corelib and never measured against this
			// field's maxlen — the maxlen reject (MESSAGE_SPEC S7.1: INVALID, never a
			// truncating read) therefore hangs off a successful read. Checking _size
			// first would resurrect generator#224/#229 on the deliver path.
			f.line("            is.readString(%s, %d);", acc, fld.Maxlen)
		} else {
			f.line("            is.readString(%s);", acc)
		}
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindBool, ir.KindFP32, ir.KindFP64, ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC §7.1: the declared integer width is a validity bound, so an
		// over-width value is INVALID — never masked to the width. corelib-cpp's
		// typed read() ends in `value = static_cast<T>(raw)`, which IS that mask and
		// leaves the raw value invisible to us, so a narrow destination is read
		// through a 64-bit temporary instead and range-checked before the store.
		//
		// The §7.3 behaviour is unchanged by the wider temporary: read() picks its
		// expected wire type from the signedness alone (Wire::Unsigned for every u*,
		// Wire::Signed for every i*), so u64 and u8 frame identically. A
		// contradicting tag still returns false and the arm stores nothing — the
		// skip, not a reject.
		//
		// c-cpp is deliberately excluded: it is already conformant (its deferred
		// descriptor carries the declared width to the corelib, which rejects there),
		// and it has no such read() to route around.
		if lo, hi, narrow := ir.NarrowRange(fld.Kind); narrow && !g.clib {
			tmp := "std::uint64_t"
			cond := fmt.Sprintf("_v > %d", hi)
			if lo < 0 {
				tmp = "std::int64_t"
				cond = fmt.Sprintf("_v < %d || _v > %d", lo, hi)
			}
			f.line("            { %s _v; if (is.read(_v)) { if (%s) { is.invalidate(); return; } %s = static_cast<%s>(_v); } }", tmp, cond, acc, g.cppType(fld))
		} else {
			f.line("            is.read(%s);", acc)
		}
	case ir.KindBlob:
		// corelib-c-cpp binds blobs with the BLOB tag via its read(void*, size_t)
		// overload into the address-stable vector buffer; corelib-cpp reads a
		// length-prefixed blob into a std::string.
		// Unbounded blob cap: same policy guard as the string case above (#102).
		if !g.clib && !fld.HasMaxlen && g.limBlobHas {
			f.line("            if (_size > SOFAB_MAX_DYN_BLOB_LEN) { is.exceedLimit(); return; }")
		}
		if g.fixed {
			// As for the string arm above.
			f.line("            is.readBlob(%s, _size, %d);", acc, maxlenOr(fld.HasMaxlen, fld.Maxlen))
		} else if g.clib {
			f.line("            %s.resize(_size); is.read(%s.data(), _size);", acc, acc)
		} else if fld.HasMaxlen {
			// readBlob declares Fix::Blob and reads straight into the byte container
			// (no std::string round-trip); the maxlen reject hangs off the successful
			// read for the same S7.3 reason as the string case above.
			f.line("            is.readBlob(%s, %d);", acc, fld.Maxlen)
		} else {
			f.line("            is.readBlob(%s);", acc)
		}
	case ir.KindEnum:
		// corelib-c-cpp's read binds a target by address and fills it after the
		// callback, so a local temp would dangle; read straight into the enum's
		// underlying-typed storage instead. corelib-cpp copies in place, so the
		// temp is safe there.
		if g.clib {
			f.line("            is.read(reinterpret_cast<%s &>(%s));", enumBacking(fld.Ref.Target), acc)
		} else {
			f.line("            { std::int64_t _v = 0; is.read(_v); %s = static_cast<%s>(_v); }", acc, g.typeName(fld.Ref.Key))
		}
	case ir.KindBitfield:
		// The bitfield member is an integral type, so corelib-c-cpp can fill it
		// directly (no dangling temp).
		if g.clib {
			f.line("            is.read(%s);", acc)
		} else {
			f.line("            { std::uint64_t _v = 0; is.read(_v); %s = static_cast<%s>(_v); }", acc, g.cppType(fld))
		}
	case ir.KindArray:
		// A wire element count above the schema `count` capacity is INVALID per
		// MESSAGE_SPEC §3+§7 — poison the stream so feed()/try_decode report
		// Error::InvalidMessage instead of the corelib read clamping the excess
		// (generator#100). The clib wrapper needs no guard: the C runtime
		// rejects a count/capacity mismatch on its own (SOFAB_RET_E_INVALID_MSG).
		// On the pure path both the schema `count` and the configured policy cap
		// ride into readArray(), which applies them AFTER the tag match — so
		// neither can be measured against a field §7.3 skips (the deliver-path
		// shape of generator#224/#229). The clib wrapper emitted neither: its C
		// runtime rejects a count/capacity mismatch on its own.
		//
		// The wire count M IS the array's length (MESSAGE_SPEC §3): the M elements
		// that arrived are the whole value, taken as they come. A declared
		// `count: N` is a capacity and bounds M; it never adds elements, so there
		// is nothing to fill in at [M, N). The one exception is forced by C++
		// storage rather than by the spec: a fixed std::array<T, N> has no logical
		// length, so its value is always N elements and readArray value-initialises
		// it before the M that arrived land on top -- see docs/generator/cpp.md.

		// A composite array's wrapper sequence IS the array's value (MESSAGE_SPEC
		// §5), so a field id repeating within one scope REPLACES it whole — unlike a
		// struct or union, whose re-opened scope continues (§7.4). The collectors
		// (_StrSeq/_BlobSeq/_MsgSeq and their fixed variants) place by element index
		// or emplace in arrival order and never reset the target, so without this
		// clear a second opening merges into the first one's elements. Native scalar
		// arrays already replace: they read the whole array in one call.
		// On the pure path the collectors declare prepare(), which read() calls once
		// the SequenceStart tag matched — so the replace-whole reset happens behind
		// the §7.3 decision instead of in front of it.

		g.deserializeArray(f, "            ", acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, fld.ElemMaxHas, fld.ElemMax, 0)
	}
}

// deserializeArray reads an array into target, mirroring serializeArray. Native
// numeric/bitfield arrays read directly; enum/boolean arrays value-convert;
// string/blob use the native/_StrSeq helpers; struct/union and nested arrays use
// the _MsgSeq sequence visitor. The corelib-c-cpp wrapper binds targets by
// address for a deferred pass, so its enum/boolean arrays read in place (a temp
// would dangle) and the target vector is reserved so an emplace never moves a
// still-unfilled bound element.
// elemMaxOr returns a string/blob wrapper element's schema maxlen bound (L) when
// present, else -1 (no bound), for the _StrSeq/_BlobSeq collectors' _emax guard.
func maxlenOr(has bool, m int64) int64 {
	if has {
		return m
	}
	return -1
}

func elemMaxOr(has bool, m int64) int64 {
	if has {
		return m
	}
	return -1
}

// nativeTemp is the container an enum array's encode temporary takes: the same
// form the member itself takes, so a heap-free profile converts without touching
// a heap. Both forms carry their own length, which is what the temp needs — the
// wire count IS the length (MESSAGE_SPEC §3), so the temp must be the value's
// length and not the schema `count`.
func (g *gen) nativeTemp(elemType string, count int64) string {
	if g.fixed && !g.allowDynamic {
		return fmt.Sprintf("sofab::InlineVector<%s, %d>", elemType, count)
	}
	return "std::vector<" + elemType + ">"
}

// nativeArrayRead emits the read for an array whose MEMBER element type is
// already the wire element type -- every numeric/bitfield array, and a boolean
// array on the c-cpp leg, where the element is std::uint8_t. No conversion, no
// temporary, no cast: the corelib binds the member itself.
func (g *gen) nativeArrayRead(f *hfile, ind, target string, count int64, hasCount bool, cap int64, depth int) {
	if g.clib && depth == 0 {
		// readArray settles the tag before it prepares the destination, checks
		// the wire count against the schema bound before any resize, and only
		// then binds — so inline and dynamic storage emit the same call and the
		// arm needs no guard and no reset of its own.
		f.line("%sis.readArray(%s, _count, %d);", ind, target, cap)
		return
	}
	if g.clib {
		f.line("%sis.read(%s);", ind, target)
		return
	}
	// readArray carries the tag, the schema count, the configured policy cap
	// and the reset — see IStreamImpl::readArray for the order they must run
	// in. A count-less array sizes to the wire count inside it, so it can no
	// longer decode empty (#112).
	f.line("%sis.readArray(%s%s);", ind, target, g.cppArrayBounds(count, hasCount))
}

func (g *gen) deserializeArray(f *hfile, ind, target string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, hasCount, elemMaxHas bool, elemMax int64, depth int) {
	tv := fmt.Sprintf("_t%d", depth)
	iv := fmt.Sprintf("_i%d", depth)
	rv := fmt.Sprintf("_r%d", depth)
	// Fixed-count wrapper array: an element id >= N is INVALID (MESSAGE_SPEC
	// S5.1/S7). cap is that bound N handed to the collector; -1 == dynamic (no
	// schema count), which keeps every delivered index.
	cap := int64(-1)
	if hasCount {
		cap = count
	}
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBitfield:
		g.nativeArrayRead(f, ind, target, count, hasCount, cap, depth)
	case ir.KindEnum:
		bk := enumBacking(ref.Target)
		et := g.cppArrayElem(elem, ref, items, elemMaxHas, elemMax)
		if g.clib {
			// corelib-c-cpp's decoder is DEFERRED: readArray records the
			// destination's ADDRESS and the C runtime writes the element bytes after
			// this callback has returned, so the destination has to be the member
			// itself -- a temporary would dangle, which is why this arm cannot take
			// the corelib-cpp shape below. What it must NOT do is reinterpret the
			// CONTAINER: casting a std::vector<E> to a std::array<int8, N> made the
			// vector's own begin/end/capacity words the first N elements, so wire
			// bytes overwrote the begin pointer and the destructor freed a pointer
			// assembled from the message. RawArray reinterprets the ELEMENTS
			// instead -- exactly what the scalar enum arm does -- and forwards
			// resize/size, so readArray keeps ownership of the tag/bound/reset
			// order and the bytes land in the member's own storage.
			f.line("%s{ sofabgen::RawArray<%s, %s> %s{&%s}; is.readArray(%s, _count, %d); }",
				ind, g.cppArrayContainer(elem, ref, items, count, elemMaxHas, elemMax), bk, tv, target, tv, cap)
		} else {
			// corelib-cpp decodes synchronously, so the temp is safe here: it carries
			// the read (and with it the tag/bound/reset decision) and the member is
			// only resized and filled once that succeeded, so a §7.3-skipped
			// occurrence leaves it untouched. The member is sized to the TEMP's
			// length, which is the wire count -- the array's length (§3) -- and never
			// to the schema `count`.
			f.line("%s{ std::vector<%s> %s; if (is.readArray(%s%s)) { %s.resize(%s.size()); for (std::size_t %s = 0; %s < %s.size(); ++%s) %s[%s] = static_cast<%s>(%s[%s]); } }",
				ind, bk, tv, tv, g.cppArrayBounds(count, hasCount), target, tv, iv, iv, tv, iv, target, iv, et, tv, iv)
		}
	case ir.KindBool:
		if g.clib {
			// On the c-cpp leg the element already IS the wire's std::uint8_t
			// (cppArrayElem), so the member is a native destination like any other
			// and takes the numeric arm verbatim -- no conversion, and above all no
			// reinterpret_cast of the container, which is what corrupted a
			// std::vector<bool>'s control words under allow_dynamic.
			g.nativeArrayRead(f, ind, target, count, hasCount, cap, depth)
		} else {
			f.line("%s{ std::vector<std::uint8_t> %s; if (is.readArray(%s%s)) { %s.resize(%s.size()); for (std::size_t %s = 0; %s < %s.size(); ++%s) %s[%s] = %s[%s] != 0; } }",
				ind, tv, tv, g.cppArrayBounds(count, hasCount), target, tv, iv, iv, tv, iv, target, iv, tv, iv)
		}
	case ir.KindString:
		cont := g.cppArrayContainer(elem, ref, items, count, elemMaxHas, elemMax)
		// The leaf collectors stay in the corelib: they already PLACE each element
		// at its element id (MESSAGE_SPEC §5.1), which is the half of generator#247
		// the object path was missing, and the schema count and element maxlen ride
		// in as bounds. Nothing follows the read -- the length is what the ids
		// established, and `count` never adds to it (§3).
		if strings.HasPrefix(cont, "sofab::InlineVector") {
			// Fixed string sequence: fill fixed inline FixedString slots by the
			// element size via the scalar FixedString read, no heap. Static for the
			// same deferred-decoder reason as the other fixed collectors.
			g.emitSeqRead(f, ind, fmt.Sprintf("static sofab::FixedStringSeq<%s> %s;", cont, rv),
				fmt.Sprintf("is.readSequence(%s, %s)", rv, target))
		} else if g.clib {
			// Static for the same deferred-decoder reason as the fixed collectors:
			// the C decoder dereferences it after this returns.
			g.emitSeqRead(f, ind, fmt.Sprintf("static sofab::StringSeq %s; %s.cap = %d; %s.elemMax = %d;", rv, rv, cap, rv, elemMaxOr(elemMaxHas, elemMax)),
				fmt.Sprintf("is.readSequence(%s, %s)", rv, target))
		} else {
			g.emitSeqRead(f, ind, fmt.Sprintf("sofab::StringSeq %s{%s, %d, %d};", rv, target, cap, elemMaxOr(elemMaxHas, elemMax)),
				fmt.Sprintf("is.read(%s)", rv))
		}
	case ir.KindBlob:
		cont := g.cppArrayContainer(elem, ref, items, count, elemMaxHas, elemMax)
		if strings.HasPrefix(cont, "sofab::InlineVector") {
			// Fixed blob sequence: fill fixed inline slots by the element size (the
			// read(void*,size_t) blob overload), no heap. The collector is static
			// because the corelib-c-cpp decoder dereferences it after this returns.
			g.emitSeqRead(f, ind, fmt.Sprintf("static sofab::FixedBlobSeq<%s> %s;", cont, rv),
				fmt.Sprintf("is.readSequence(%s, %s)", rv, target))
		} else if g.clib {
			g.emitSeqRead(f, ind, fmt.Sprintf("static sofab::BlobSeq %s; %s.cap = %d; %s.elemMax = %d;", rv, rv, cap, rv, elemMaxOr(elemMaxHas, elemMax)),
				fmt.Sprintf("is.readSequence(%s, %s)", rv, target))
		} else {
			g.emitSeqRead(f, ind, fmt.Sprintf("sofab::BlobSeq %s{%s, %d, %d};", rv, target, cap, elemMaxOr(elemMaxHas, elemMax)),
				fmt.Sprintf("is.read(%s)", rv))
		}
	case ir.KindStruct, ir.KindUnion:
		cont := g.cppArrayContainer(elem, ref, items, count, elemMaxHas, elemMax)
		g.deserializeSeqInto(f, ind, target, g.typeName(ref.Key), count, cap, rv, cont)
	case ir.KindArray:
		cont := g.cppArrayContainer(elem, ref, items, count, elemMaxHas, elemMax)
		// A row of native scalars IS readable by the corelib: MessageSeq/
		// FixedMessageSeq places the row and hands it to is.read(), whose span
		// overload fills a contiguous container of trivially-copyable elements.
		// A row whose elements are strings, blobs, structs/unions or arrays is
		// itself a wrapper SEQUENCE, which is not a span of scalars and not an
		// IStreamMessage either -- handing it to is.read() is the static_assert
		// "Unsupported span element type" (generator#250). Such a row needs its
		// own collector, one level down.
		if isNativeArrayElem(items.Elem) {
			inner := g.cppArrayContainer(items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas, items.ElemMax)
			g.deserializeSeqInto(f, ind, target, inner, count, cap, rv, cont)
			return
		}
		g.deserializeRowSeq(f, ind, target, items, count, cap, rv, cont, depth)
	}
}

// deserializeRowSeq reads an array of wrapper ROWS -- array<array<string>>,
// array<array<blob>>, array<array<struct|union>> and deeper -- into target.
//
// The corelib's MessageSeq/FixedMessageSeq is the collector for a sequence whose
// ELEMENTS the stream can read on its own: a struct/union element (an
// IStreamMessage) or a native-scalar row (a span of trivially-copyable values).
// A row that is itself a wrapper sequence is neither, so it needs a collector of
// its own, and the corelib cannot ship one: what a row costs to read is the
// schema's business (element bounds, element type), not the wire format's.
//
// So the row collector is generated, right where it is used, and the row read it
// wraps is the SAME array emission one level down -- which is what makes this
// recursive rather than three special cases: depth 3 wraps a depth-2 collector,
// and a struct/blob/string row lands on the corelib collector the first-level
// path already uses (sofab::StringSeq/BlobSeq/MessageSeq, or their Fixed*
// counterparts on the c-cpp leg).
//
// The collector carries the same two spec rules as the corelib ones:
//   - §5.1 an element id IS its index, so a row is PLACED at that index (gaps are
//     legal and stay at the element default), and an id at or past the schema
//     `count` is INVALID -- the fixed profile reads that bound off the inline
//     container's capacity, which also stops an over-index emplace_back that
//     InlineVector would otherwise no-op forever on (#126).
//   - §7.4 a repeated field id replaces the array whole, via prepare() on the
//     pure path (read() calls it once the SequenceStart tag matched, so a §7.3
//     skip cannot wipe an earlier value) and via readSequence() on the c-cpp leg,
//     which clears the destination itself.
//
// Storage follows deserializeSeqInto: the c-cpp decoder is deferred and uses the
// collector after this call returns, so it gets static storage (one instance per
// nesting level is enough -- rows are decoded one at a time, in stream order),
// and a heap row vector is reserved up front so placing a later row never moves
// a still-bound earlier one.
func (g *gen) deserializeRowSeq(f *hfile, ind, target string, items *ir.ArrayElem, count, cap int64, rv, container string, depth int) {
	sv := fmt.Sprintf("_S%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	inlineRows := strings.HasPrefix(container, "sofab::InlineVector")
	in2 := ind + "    "
	in3 := in2 + "    "
	in4 := in3 + "    "
	f.line("%s{", ind)
	f.line("%sstruct %s : sofab::IStreamMessage {", in2, sv)
	f.line("%s%s *out = nullptr;", in3, container)
	if !inlineRows {
		f.line("%slong cap = %d;", in3, cap)
	}
	if !g.clib {
		// Declaring prepare() is how a collector asks read() for the §7.4
		// replace-whole reset; readSequence() on the c-cpp leg clears for us.
		f.line("%svoid prepare() noexcept { if (out) out->clear(); }", in3)
	}
	f.line("%svoid deserialize(sofab::IStreamImpl &is, sofab::id _id, std::size_t, std::size_t) noexcept override {", in3)
	if inlineRows {
		f.line("%sif (static_cast<std::size_t>(_id) >= out->capacity()) { is.invalidate(); return; }", in4)
	} else {
		f.line("%sif (cap >= 0 && static_cast<std::size_t>(_id) >= static_cast<std::size_t>(cap)) { is.invalidate(); return; }", in4)
	}
	f.line("%swhile (out->size() <= static_cast<std::size_t>(_id)) out->emplace_back();", in4)
	f.line("%sauto &%s = (*out)[_id];", in4, ev)
	g.deserializeArray(f, in4, ev, items.Elem, items.ElemRef, items.ElemItems, items.Count, items.HasCount, items.ElemMaxHas, items.ElemMax, depth+1)
	f.line("%s}", in3)
	f.line("%s};", in2)
	if g.clib {
		if count > 0 && !inlineRows {
			f.line("%s%s.reserve(%d);", in2, target, count)
		}
		f.line("%sstatic %s %s; is.readSequence(%s, %s);", in2, sv, rv, rv, target)
	} else {
		f.line("%s%s %s; %s.out = &%s; is.read(%s);", in2, sv, rv, rv, target, rv)
	}
	f.line("%s}", ind)
}

// deserializeSeqInto reads a wrapper sequence of struct/union elements or
// nested-array rows into target through sofabgen::WrapperSeq, which places each
// element at its element id (MESSAGE_SPEC §5.1) instead of appending it. The
// decoded length is highest present id + 1 and nothing is added past it: the
// schema count N is a capacity that bounds the id, never a length (§3).
//
// The collector is generated rather than taken from the corelib because
// corelib-c-cpp's Fixed/MessageSeq APPEND in arrival order -- the C wrapper
// never learns that the element id means anything -- which an interior id gap,
// ordinary since §2 made an all-default interior element sparse, would turn into
// every later element shifted down by one.
//
// corelib-cpp decodes synchronously, so a plain stack-local collector is fine
// and read() reports whether the SequenceStart tag matched. The corelib-c-cpp
// wrapper is a DEFERRED decoder that uses the collector after this call returns,
// so its collector gets static storage, the wrapper's own presence is decided
// from the field tag before readSequence, and a fixed count reserves the target
// up front so a later placement never reallocates a still-bound element.
func (g *gen) deserializeSeqInto(f *hfile, ind, target, elemType string, count, cap int64, rv, container string) {
	seq := fmt.Sprintf("sofabgen::WrapperSeq<%s>", container)
	if g.clib {
		reserve := ""
		if count > 0 && !strings.HasPrefix(container, "sofab::InlineVector") {
			// A dynamic destination must not reallocate while an element the
			// deferred decoder still has to fill is bound into it.
			reserve = fmt.Sprintf(" %s.reserve(%d);", target, count)
		}
		g.emitSeqRead(f, ind, fmt.Sprintf("static %s %s; %s.cap = %d;%s", seq, rv, rv, cap, reserve),
			fmt.Sprintf("is.readSequence(%s, %s)", rv, target))
		return
	}
	g.emitSeqRead(f, ind, fmt.Sprintf("%s %s; %s.out = &%s; %s.cap = %d;", seq, rv, rv, target, rv, cap),
		fmt.Sprintf("is.read(%s)", rv))
}

// emitSeqRead writes one wrapper-array read: the collector declaration and the
// read itself.
//
// Nothing follows the read any more. A declared `count: N` is a CAPACITY, not a
// length (MESSAGE_SPEC §3): the array's length is *highest present id + 1*
// (§5.1), which the collector's placement already establishes, and N never adds
// an element the wire did not carry. The refill this used to emit was the decode
// half of the superseded trim/fill pair — it turned ["a"] into ["a", "", ""] on
// a count: 3 field, which is a different value.
func (g *gen) emitSeqRead(f *hfile, ind, decl, readCall string) {
	f.line("%s{ %s %s; }", ind, decl, readCall)
}

// checkBounded enforces the fixed-capacity (embedded) profile's unbounded-field
// policy (plan §9): every field that the profile lowers to fixed storage must be
// sized by the schema. A string or blob needs a maxlen; a
// string/blob/struct/union/nested-array sequence needs a count (and a string/blob
// element needs an element maxlen). An unbounded such field is a hard error unless
// The bound is mandatory in BOTH storage modes: allow_dynamic selects the
// container a bounded field lives in, never whether it needs a bound. That is
// what keeps one schema valid for every c-cpp target — the same maxlen/count,
// the same wire bytes, only the storage differs — so the switch can be flipped
// per device without touching the schema.
func (g *gen) checkBounded(s *ir.Schema) error {
	seen := map[string]bool{}
	var walkFields func(owner string, fields []*ir.Field) error
	var walkArray func(owner, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool) error
	walkArray = func(owner, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool) error {
		// Every array level needs a count — including a native scalar array, which
		// this switch previously did not cover: a count-less native array slipped
		// through and silently became std::array<T, 0> even under allow_dynamic:
		// false (generator#104 point 3).
		if count <= 0 {
			return unboundedErr(owner, path, "count")
		}
		switch elem {
		case ir.KindString, ir.KindBlob:
			if !elemMaxHas {
				return unboundedErr(owner, path, "element maxlen")
			}
		case ir.KindStruct, ir.KindUnion:
			return walkFields(g.typeName(ref.Key), ref.Target.Fields)
		case ir.KindArray:
			return walkArray(owner, path+"[]", items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas)
		}
		return nil
	}
	walkFields = func(owner string, fields []*ir.Field) error {
		if seen[owner] {
			return nil
		}
		seen[owner] = true
		for _, fld := range fields {
			switch fld.Kind {
			case ir.KindString, ir.KindBlob:
				if !fld.HasMaxlen {
					return unboundedErr(owner, fld.Name, "maxlen")
				}
			case ir.KindStruct, ir.KindUnion:
				if err := walkFields(g.typeName(fld.Ref.Key), fld.Ref.Target.Fields); err != nil {
					return err
				}
			case ir.KindArray:
				if err := walkArray(owner, fld.Name, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.ElemMaxHas); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, m := range s.Messages {
		if err := walkFields(exported(m.Name), m.Fields); err != nil {
			return err
		}
	}
	return nil
}

func unboundedErr(owner, path, missing string) error {
	return fmt.Errorf("cpp: field %q in %q has no %s; the embedded (corelib: c-cpp) profile requires count on every array and maxlen on every string/blob, in both storage modes — allow_dynamic chooses the container, not whether a bound is needed (use corelib: cpp for genuinely unbounded fields)", path, owner, missing)
}

// --- Measure-phase schema descriptors (generator#216 / MESSAGE_SPEC S5.2) -----
// corelib-cpp is a measure-then-deliver decoder: it measures a whole top-level
// field for completeness BEFORE delivering it to the generated deserialize, where
// the bound guards live. A field that is both over-bound and truncated would thus
// be reported INCOMPLETE, where S5.2 requires INVALID to dominate (anti-folding).
// sofab::schema (corelib-cpp#50) lets the measure walk reject at the deciding word
// instead: the generator emits a static SeqNode tree per message and installs it
// with setSchema. Pure-corelib-cpp only (the c-cpp wrapper has no measure phase).

// reachable returns named-type keys used by m in post-order (children first).
// Composite array elements (enum/bitfield/struct/union) and nested-array element
// composites are collected too, so an array-of-struct element type is emitted.
func (g *gen) reachable(m *ir.Message) []string {
	var order []string
	seen := map[string]bool{}
	var visit func(fields []*ir.Field)
	var addRef func(ref *ir.TypeRef)
	var visitArray func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)
	addRef = func(ref *ir.TypeRef) {
		if ref == nil {
			return
		}
		key := ref.Key
		if seen[key] {
			return
		}
		seen[key] = true
		t := ref.Target
		if t.Category == ir.CatStruct || t.Category == ir.CatUnion {
			visit(t.Fields)
		}
		order = append(order, key)
	}
	visitArray = func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		switch elem {
		case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
			addRef(ref)
		case ir.KindArray:
			visitArray(items.Elem, items.ElemRef, items.ElemItems)
		}
	}
	visit = func(fields []*ir.Field) {
		for _, f := range fields {
			if f.Kind == ir.KindArray {
				visitArray(f.Elem, f.ElemRef, f.ElemItems)
				continue
			}
			addRef(f.Ref)
		}
	}
	visit(m.Fields)
	return order
}
