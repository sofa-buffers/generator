package cpp

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleHeader(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(b, "example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "myfirstmessage.hpp" {
			return string(f.Content)
		}
	}
	t.Fatal("no header")
	return ""
}

func TestCppStructural(t *testing.T) {
	h := exampleHeader(t)
	for _, want := range []string{
		`#include "sofab/sofab.hpp"`,
		"static_assert(sofab::API_VERSION == 1,",
		"struct Myfirstmessage : sofab::Message {", // the pair, aliased in the corelib
		"sofab::OStreamImpl::Result serialize(sofab::OStreamImpl &os) const noexcept override",
		"void deserialize(sofab::IStreamImpl &is, sofab::id id,",
		"static constexpr std::size_t _maxSize =",
		"std::vector<std::uint8_t> encode() const",
		"static Myfirstmessage decode(",
		"static sofab::IStreamImpl::Result try_decode(const std::uint8_t *data, std::size_t len, Myfirstmessage &out)",
		"enum class MyfirstmessageSomeenum : std::int8_t {", // smallest signed backing
		"std::uint64_t someu64 = 18446744073709551615ULL;",
		"is.read(",               // nested decode via is.read
		"float somefp32 = 0.0f;", // valid float literal
		"is.readArray(someuintarray, 4, -1, sofab::ElemBound::of<std::uint32_t>());", // the over-count reject (generator#100) rides into readArray
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q", want)
		}
	}
	if strings.Contains(h, " 0f;") {
		t.Error("invalid C++ float literal '0f'")
	}
}

func TestCppDeterministic(t *testing.T) {
	if exampleHeader(t) != exampleHeader(t) {
		t.Fatal("C++ generation not deterministic")
	}
}

// headerFromYAML generates a single message header from an inline definition.
func headerFromYAML(t *testing.T, src, msgFile string) string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "in.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == msgFile {
			return string(f.Content)
		}
	}
	t.Fatalf("no header %s", msgFile)
	return ""
}

// fixedHeader generates a message header under the fixed-capacity (embedded)
// profile — i.e. corelib: c-cpp, which always uses fixed containers — with the
// given extra config.
func fixedHeader(t *testing.T, src, msgFile string, extra map[string]any) (string, error) {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "in.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp"}
	for k, v := range extra {
		cfg[k] = v
	}
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.Path == msgFile {
			return string(f.Content), nil
		}
	}
	t.Fatalf("no header %s", msgFile)
	return "", nil
}

// TestCppHeapUnboundedArray: on the heap (corelib: cpp) profile a schema-
// unbounded array (no count) must lower to a growable std::vector<T> — like the
// unbounded string->std::string and blob->std::vector<uint8_t> already do — not a
// fixed std::array<T, 0>, which cannot hold any element and silently drops the
// whole array on decode (#112). A bounded native array stays std::array<T, N>.
func TestCppHeapUnboundedArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      arr:    { id: 0, type: array, items: { type: u32 } }\n" + // unbounded native
		"      en:     { id: 1, type: array, items: { type: enum, enum: { a: 0, b: 1 } } }\n" + // unbounded enum
		"      bl:     { id: 2, type: array, items: { type: boolean } }\n" + // unbounded bool
		"      fixed:  { id: 3, type: array, items: { type: u32, count: 4 } }\n" + // bounded native
		"      matrix: { id: 4, type: array, items: { type: array, items: { type: u32 } } }\n" // matrix, unbounded rows
	h, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"std::vector<std::uint32_t> arr = {};",                              // unbounded native -> vector (was std::array<T,0>)
		"std::vector<bool> bl = {};",                                        // unbounded bool -> vector
		"std::vector<std::uint32_t> fixed = {};",                            // a bounded native array is length-carrying too
		"std::vector<std::vector<std::uint32_t>> matrix",                    // matrix rows are dynamic vectors too
		"is.readArray(arr, -1, -1, sofab::ElemBound::of<std::uint32_t>());", // readArray sizes the vector to the wire count
		"if (!arr.empty()) {",                                               // whole-omit: no declared default -> empty()
		"std::size_t _count) noexcept override",                             // _count is named for the resize
		"sofabgen::WrapperSeq<std::vector<std::vector<std::uint32_t>>>",     // matrix rows collected by the generated placer
	} {
		if !strings.Contains(h, want) {
			t.Errorf("heap header missing %q:\n%s", want, h)
		}
	}
	// The zero-length fixed array must never appear — that is the bug.
	if strings.Contains(h, "std::array<std::uint32_t, 0>") {
		t.Errorf("unbounded array must not lower to std::array<T, 0>:\n%s", h)
	}
	// enum vector: member is a vector of the scoped enum element type; readArray
	// sizes the temp to the wire count and the member follows it.
	if !strings.Contains(h, "std::vector<MEnElem> en = {};") {
		t.Errorf("unbounded enum array should be a std::vector of the enum element:\n%s", h)
	}
	if !strings.Contains(h, "en.resize(_t0.size());") {
		t.Errorf("unbounded enum array decode should follow the temp's size:\n%s", h)
	}
}

// TestCppOverIndexWrapperArray: a fixed-count wrapper array (string/blob/struct
// elements) rejects an element id >= N as INVALID before growing the heap
// container (issue #142 / MESSAGE_SPEC §5.1/§7). A dynamic wrapper array (no
// count) keeps every delivered index, so its collector cap is -1.
// The measure-phase descriptors are gone: header-first delivery reaches every
// §5.2 verdict through the reads themselves, so nothing is deposited in advance.
// TestCppWireTypeGuard pins that no generated header mentions sofab::schema.

func TestCppOverIndexWrapperArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" + // bounded string wrapper
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" + // bounded blob wrapper
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" + // bounded struct wrapper
		"      ds: { id: 3, type: array, items: { type: string } }\n" + // dynamic string wrapper
		"      dp: { id: 4, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }\n" // dynamic struct wrapper
	h, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The string/blob bounds are handed to the corelib's collectors, whose guards
	// live in corelib-cpp (sofab::StringSeq / BlobSeq); the object path collects
	// through the generated placer, which carries the same bound as `cap`. Either
	// way this asserts what the generator DECLARES rather than the check itself.
	for _, want := range []string{
		"{ sofab::StringSeq _r0{bs, 4, 16}; is.read(_r0); }", // bounded string -> cap 4, elem maxlen 16, never refilled
		"{ sofab::BlobSeq _r0{bb, 3, 16}; is.read(_r0); }",   // bounded blob -> cap 3, elem maxlen 16, never refilled
		"_r0.cap = 2;", // bounded struct -> placer cap 2
		"{ sofab::StringSeq _r0{ds, -1, -1}; is.read(_r0); }", // dynamic string -> unbounded cap + maxlen
		"_r0.cap = -1;", // dynamic struct -> unbounded
	} {
		if !strings.Contains(h, want) {
			t.Errorf("heap over-index bound missing %q:\n%s", want, h)
		}
	}
	// The leaf collectors are corelib types: a generated header must not define
	// its own copies of those on the pure path.
	for _, notWant := range []string{"struct _StrSeq", "struct _BlobSeq", "struct _MsgSeq"} {
		if strings.Contains(h, notWant) {
			t.Errorf("the pure path must use the corelib collector, not emit %q:\n%s", notWant, h)
		}
	}
}

// TestCppMaxlenReject: on the heap profile a bounded string/blob (scalar or
// wrapper-array element) rejects a wire byte length above its schema maxlen as
// INVALID before the read (MESSAGE_SPEC §7.1). Unbounded fields get no such guard.
func TestCppMaxlenReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" + // bounded scalar string
		"      b:  { id: 1, type: blob,   maxlen: 8 }\n" + // bounded scalar blob
		"      sa: { id: 2, type: array, items: { type: string, count: 3, maxlen: 5 } }\n" + // bounded wrapper string
		"      ds: { id: 3, type: string }\n" // unbounded string
	h, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		// The bound rides INTO the read. Header-first delivers a field before its
		// payload is known to be present, so a bound checked after the read would
		// fold an over-maxlen truncated field to INCOMPLETE (§5.2); inside the
		// read it is applied after the tag and before the payload, which is the
		// only order that satisfies both §7.3 and §5.2.
		"is.readString(s, 8);",
		"is.readBlob(b, 8);",
		"{ sofab::StringSeq _r0{sa, 3, 5}; is.read(_r0); }", // wrapper string: cap 3, elem maxlen 5 handed to the corelib collector
	} {
		if !strings.Contains(h, want) {
			t.Errorf("heap maxlen guard missing %q:\n%s", want, h)
		}
	}
	// The unbounded string field must not carry a maxlen guard.
	if strings.Contains(h, "is.read(ds)") && strings.Contains(h, "_size > -1") {
		t.Error("unbounded string must not carry a maxlen guard")
	}
}

// TestCppFixedContainers: corelib: c-cpp lowers bounded strings, blobs, and
// struct/matrix/string/blob sequences to heap-free, schema-sized storage. Wire
// bytes are unchanged (proven separately by the conformance run) — this asserts
// the emitted member types.
func TestCppFixedContainers(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bl: { id: 0, type: blob, maxlen: 16 }\n" +
		"      s: { id: 1, type: string, maxlen: 8 }\n" +
		"      nums: { id: 2, type: array, items: { type: u32, count: 4 } }\n" +
		"      blobs: { id: 3, type: array, items: { type: blob, count: 3, maxlen: 8 } }\n" +
		"      strs: { id: 4, type: array, items: { type: string, count: 5, maxlen: 16 } }\n" +
		"      pts: { id: 5, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n"
	h, err := fixedHeader(t, src, "m.hpp", nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"sofab::FixedBytes<16> bl = {};",                                               // scalar blob -> fixed
		"sofab::FixedString<8> s = \"\";",                                              // bounded string -> FixedString
		"sofab::InlineVector<std::uint32_t, 4> nums = {};",                             // native array is inline + length-carrying
		"sofab::InlineVector<sofab::FixedBytes<8>, 3> blobs = {};",                     // blob sequence -> inline, EMPTY (count is a capacity)
		"sofab::InlineVector<sofab::FixedString<16>, 5> strs = {};",                    // string sequence -> inline, EMPTY (count is a capacity)
		"sofab::InlineVector<MPtsElem",                                                 // struct sequence -> inline (prefix)
		"if (!bl.empty()) {",                                                           // blob, no declared default -> empty()
		"is.readString(s, _size, 8);",                                                  // FixedString decode, bound carried into the corelib read
		"is.readBlob(bl, _size, 16);",                                                  // FixedBytes decode, likewise (issue #95)
		"static sofab::FixedBlobSeq<sofab::InlineVector<sofab::FixedBytes<8>, 3>>",     // blob-seq collector
		"static sofab::FixedStringSeq<sofab::InlineVector<sofab::FixedString<16>, 5>>", // string-seq collector
		"static sofabgen::WrapperSeq<sofab::InlineVector<",                             // struct-seq collector (places by element index)
		// over-index element rejected INVALID (generator#149), no infinite loop (issue #126)
		"std::size_t encodeTo(std::uint8_t *dst", // heap-free encode
	} {
		if !strings.Contains(h, want) {
			t.Errorf("fixed header missing %q", want)
		}
	}
	// The clib wrapper emits no scalar over-count field guard: the C runtime itself
	// rejects a count/capacity mismatch with SOFAB_RET_E_INVALID_MSG (generator#100).
	// (String/blob fixed fields decode via the inline collectors, whose own
	// >= capacity() guard now rejects an over-index element via is.invalidate()
	// — the callback→decoder abort channel c-cpp gained in corelib-c-cpp#92
	// (generator#149) — instead of #126's silent drop.)
	if strings.Contains(h, "if (_count >") {
		t.Error("corelib: c-cpp must not emit a scalar over-count guard (C runtime already rejects over-count)")
	}
	// No std::string / std::vector member for the bounded string/blob fields.
	if strings.Contains(h, "std::string s ") || strings.Contains(h, "std::vector<std::uint8_t> bl") ||
		strings.Contains(h, "std::vector<std::string> strs") {
		t.Error("fixed profile must not emit std::string/std::vector for bounded string/blob members")
	}
	// The containers now live in the corelib (sofab::FixedBytes / sofab::InlineVector);
	// the generator references them and must no longer hand-roll the definitions.
	if strings.Contains(h, "struct FixedBytes {") || strings.Contains(h, "struct InlineVector {") {
		t.Error("fixed profile must not emit its own FixedBytes/InlineVector; they come from the corelib")
	}
	// FixedBytes decode must never feed the unclamped wire length to the raw
	// read(void*, size_t) overload — that overflows the inline N-byte buffer
	// (issue #95). The bounded form uses .size() (clamped by set_len).
	if strings.Contains(h, "is.read(bl.data(), _size);") {
		t.Error("FixedBytes decode uses unclamped _size — buffer overflow (issue #95)")
	}
}

// TestCppFixedUnbounded: an unbounded field (array without count) is a hard error
// under the embedded profile in BOTH storage modes. allow_dynamic selects the
// container a bounded field lives in; it never makes a bound optional, so one
// schema stays valid for every c-cpp target.
func TestCppFixedUnbounded(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      m: { id: 0, type: array, items: { type: struct, fields: { k: { id: 0, type: i32 } } } }\n"
	for _, dyn := range []bool{false, true} {
		cfg := map[string]any{"allow_dynamic": dyn}
		if _, err := fixedHeader(t, src, "m.hpp", cfg); err == nil {
			t.Fatalf("allow_dynamic=%v: expected unbounded-field error", dyn)
		} else if !strings.Contains(err.Error(), "has no count") {
			t.Errorf("allow_dynamic=%v: unexpected error: %v", dyn, err)
		}
	}
	// Bounded, it generates in both modes — inline by default, heap under
	// allow_dynamic.
	bounded := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      m: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: i32 } } } }\n"
	h, err := fixedHeader(t, bounded, "m.hpp", nil)
	if err != nil {
		t.Fatalf("bounded should generate: %v", err)
	}
	if !strings.Contains(h, "sofab::InlineVector<") {
		t.Error("default storage should be inline")
	}
	h, err = fixedHeader(t, bounded, "m.hpp", map[string]any{"allow_dynamic": true})
	if err != nil {
		t.Fatalf("bounded under allow_dynamic should generate: %v", err)
	}
	if !strings.Contains(h, "std::vector<") || strings.Contains(h, "sofab::InlineVector<") {
		t.Error("allow_dynamic should put the bounded sequence in a std::vector")
	}
}

// TestCppFixedUnboundedNativeArray: a count-less NATIVE scalar array was the gap
// in checkBounded — its walkArray switch only covered string/blob/struct/union/
// nested-array elements, so a native scalar array slipped through and silently
// became std::array<T, 0> even under allow_dynamic: false (generator#104 pt 3).
// It must now be a hard error naming the field, exactly like the composite-array
// and string cases.
func TestCppFixedUnboundedNativeArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: u32 } }\n"
	if _, err := fixedHeader(t, src, "m.hpp", nil); err == nil {
		t.Fatal("expected unbounded native-array error under fixed profile")
	} else if !strings.Contains(err.Error(), "has no count") || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCppFixedUnboundedString: a string without maxlen is an unbounded field (no
// string exemption) and a hard error in both storage modes.
func TestCppFixedUnboundedString(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string }\n"
	for _, dyn := range []bool{false, true} {
		cfg := map[string]any{"allow_dynamic": dyn}
		if _, err := fixedHeader(t, src, "m.hpp", cfg); err == nil {
			t.Fatalf("allow_dynamic=%v: expected unbounded-string error", dyn)
		} else if !strings.Contains(err.Error(), "has no maxlen") {
			t.Errorf("allow_dynamic=%v: unexpected error: %v", dyn, err)
		}
	}
}

// TestCppDynamicStorage: with every bound in place, allow_dynamic swaps the
// storage and nothing else. The maxlen/count that were the inline container's
// capacity become explicit rejects on the decode path, so the same schema keeps
// the same wire contract on a target that has a heap.
func TestCppDynamicStorage(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string, maxlen: 12 }\n" +
		"      b: { id: 1, type: blob, maxlen: 8 }\n" +
		"      a: { id: 2, type: array, items: { type: u32, count: 4 } }\n" +
		"      t: { id: 3, type: array, items: { type: string, count: 2, maxlen: 5 } }\n"

	fix, err := fixedHeader(t, src, "m.hpp", nil)
	if err != nil {
		t.Fatalf("inline: %v", err)
	}
	for _, want := range []string{
		"sofab::FixedString<12> s", "sofab::FixedBytes<8> b",
		"sofab::InlineVector<std::uint32_t, 4> a", "sofab::InlineVector<sofab::FixedString<5>, 2> t",
	} {
		if !strings.Contains(fix, want) {
			t.Errorf("inline storage: missing %q", want)
		}
	}

	dyn, err := fixedHeader(t, src, "m.hpp", map[string]any{"allow_dynamic": true})
	if err != nil {
		t.Fatalf("dynamic: %v", err)
	}
	for _, want := range []string{
		"std::string s", "std::vector<std::uint8_t> b",
		"std::vector<std::uint32_t> a", "std::vector<std::string> t",
	} {
		if !strings.Contains(dyn, want) {
			t.Errorf("dynamic storage: missing %q", want)
		}
	}
	if strings.Contains(dyn, "sofab::Fixed") || strings.Contains(dyn, "sofab::InlineVector") {
		t.Error("dynamic storage should use no inline container")
	}
	// Each bound survives as a check — it rides into the corelib read, which
	// applies it after the §7.3 tag match and before it sizes the destination.
	// The two storage modes emit the same calls; only the members differ.
	for _, want := range []string{
		"is.readString(s, _size, 12);",
		"is.readBlob(b, _size, 8);",
		"is.readArray(a, _count, 4);",
		"_r0.cap = 2; _r0.elemMax = 5;",
	} {
		if !strings.Contains(dyn, want) {
			t.Errorf("dynamic decode: missing bound check %q", want)
		}
	}
}

// genHeader generates a single header with an explicit config (no defaults added
// beyond the backend's own), returning the header body.
func genHeader(t *testing.T, src, msgFile string, cfg map[string]any) (string, error) {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "in.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.Path == msgFile {
			return string(f.Content), nil
		}
	}
	t.Fatalf("no header %s", msgFile)
	return "", nil
}

// TestCppContainersByCorelib: the container representation is chosen solely by
// corelib — c-cpp (embedded) always uses fixed-capacity storage, pure cpp always
// uses dynamic std::vector/std::string. There is no separate knob.
func TestCppContainersByCorelib(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      bl: { id: 0, type: blob, maxlen: 16 }\n"
	// corelib: c-cpp -> fixed containers.
	h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp"})
	if err != nil {
		t.Fatalf("c-cpp generate: %v", err)
	}
	if !strings.Contains(h, "sofab::FixedBytes<16> bl") {
		t.Error("c-cpp should use fixed containers (expected sofab::FixedBytes member)")
	}
	// pure cpp (default) -> dynamic std::vector.
	h, err = genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("cpp generate: %v", err)
	}
	if !strings.Contains(h, "std::vector<std::uint8_t> bl") {
		t.Error("pure cpp should use dynamic std::vector")
	}
}

// TestCppSparse: the C++ serialize is always sparse-canonical (MESSAGE_SPEC S2),
// with no config toggle. A scalar/string/blob leaf is written under an
// "if (v != default)" guard; a native scalar array (leaf) is whole-omitted vs a
// materialized default; a struct/union field and a composite-array field are
// framed LAZILY, so an all-default one is omitted rather than emitted as an empty
// wrapper -- the corelib decides that from "was any child written".
func TestCppSparse(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: u32, default: 7 }\n" +
		"      s: { id: 1, type: string, maxlen: 8 }\n" +
		"      bl: { id: 2, type: blob, maxlen: 8 }\n" +
		"      nums: { id: 3, type: array, items: { type: i32, count: 3 }, default: [1, 2, 3] }\n" +
		"      strs: { id: 4, type: array, items: { type: string, count: 2, maxlen: 4 } }\n" +
		"      st: { id: 5, type: struct, fields: { x: { id: 0, type: i32 } } }\n"
	h := headerFromYAML(t, src, "m.hpp")
	for _, want := range []string{
		"if (a != 7) { (void)os.write(0, a); }",             // scalar guard
		"if (!s.empty()) {",                                 // string guard (empty default -> empty())
		"if (!bl.empty()) {",                                // blob guard (empty default -> empty())
		"std::vector<std::int32_t> nums = {1, 2, 3};",       // native array default materialized
		"if (nums != std::vector<std::int32_t>{1, 2, 3}) {", // native array whole-omit
		"(void)os.writeLazy(5, st);",                        // struct framed lazily (no guard)
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q", want)
		}
	}
	// A composite array carries no whole-omission guard in generated code: the
	// wrapper frame is opened lazily and the corelib drops it when no element was
	// written (S2). Its field declares no default, so the lazy form is correct.
	if strings.Contains(h, "if (strs !=") {
		t.Error("composite array must not carry a whole-omission guard")
	}
	if !strings.Contains(h, "(void)os.sequenceBeginLazy(4);") {
		t.Error("composite array field must be framed via sequenceBeginLazy")
	}
}

// TestCppDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated header on the pure-corelib-cpp
// path: guarded macros, per-field exceedLimit() guards on unbounded fields
// only, and the derived streaming reassembly cap passed as sofab::Limits into
// the one-shot decode entry points. Unset keys or the c-cpp profile emit none
// of it.
func TestCppDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      bs:   { id: 1, type: string, maxlen: 8000 }
      arr:  { id: 2, type: array, items: { type: u64 } }
      barr: { id: 3, type: array, items: { type: i32, count: 3 } }
`
	cfg := map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob -> inert
	}
	h, err := genHeader(t, src, "dyn.hpp", cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"#define SOFAB_MAX_DYN_ARRAY_COUNT 65536",
		"#define SOFAB_MAX_DYN_STRING_LEN 4096",
		// Derived cap: the largest BYTE span one top-level field can legitimately
		// reach (#228). Here that is arr — 65536 elements (the configured count
		// cap) at up to 10 varint bytes each, plus header and count word — not the
		// 8000-byte bs, and emphatically not the count 65536 itself: a count is an
		// element count, never a byte budget.
		"#define SOFAB_MAX_DYN_BUFFERED_FIELD 655364",
		"if (_size > SOFAB_MAX_DYN_STRING_LEN) { is.exceedLimit(); return; }",
		"is.readArray(arr, -1, SOFAB_MAX_DYN_ARRAY_COUNT, sofab::ElemBound::of<std::uint64_t>());", // the cap rides into readArray
		"sofab::IStreamObject<Dyn> in{sofab::Limits{SOFAB_MAX_DYN_BUFFERED_FIELD}};",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("dyn.hpp missing %q", want)
		}
	}
	if strings.Contains(h, "SOFAB_MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	// The bounded string (maxlen 8000) must NOT get a limit guard: exactly one
	// string guard (for the unbounded s), governed otherwise by its schema bound.
	if n := strings.Count(h, "SOFAB_MAX_DYN_STRING_LEN) { is.exceedLimit()"); n != 1 {
		t.Errorf("want exactly 1 string limit guard (unbounded field only), got %d", n)
	}

	// No limits configured -> no plumbing at all.
	plain, err := genHeader(t, src, "dyn.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate plain: %v", err)
	}
	if strings.Contains(plain, "SOFAB_MAX_DYN") || strings.Contains(plain, "exceedLimit") {
		t.Error("unset limits must emit no limit plumbing")
	}
}

// TestCppBufferedFieldCapIsBytes pins the dimension of the derived reassembly cap
// (#228): SOFAB_MAX_DYN_BUFFERED_FIELD is a BYTE budget, so it must be derived
// from the worst-case byte SPAN of a field, never from an element count. Deriving
// it from counts made the corelib's exceedsBuffer reject messages the per-field
// guards accept — a valid at-cap array, and even a fully schema-bounded wrapper
// array — through try_decode, where a bare feed accepted them.
func TestCppBufferedFieldCapIsBytes(t *testing.T) {
	// A fully schema-bounded wrapper array: 5 string elements of maxlen 16 span 97
	// bytes on the wire (1 + 5*(1 id + 2 length word + 16) + 1). The cap must
	// cover it. Deriving from the count gave 50 (count 5 * 10) and rejected it.
	bounded := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      sa: { id: 0, type: array, items: { type: string, count: 5, maxlen: 16 } }\n" +
		"      s:  { id: 1, type: string }\n"
	h, err := genHeader(t, bounded, "m.hpp", map[string]any{"max_dyn_string_len": 16})
	if err != nil {
		t.Fatalf("generate bounded: %v", err)
	}
	// 97, not 98: the field's own header IS the sequence_begin, so a wrapper
	// array costs `elements + terminator`, not `begin + elements + terminator`.
	// The assertion previously pinned the surplus byte the per-backend cost
	// models all carried; the comment above computed the right number all along.
	if !strings.Contains(h, "#define SOFAB_MAX_DYN_BUFFERED_FIELD 97") {
		t.Errorf("the cap must cover a fully bounded wrapper array's 97-byte span:\n%s", h)
	}

	// A dynamic array capped at 8 elements: the cap must charge 8 * the widest
	// varint element plus framing (82), not the count 8. This config sets only
	// max_dyn_array_count, which previously derived no cap at all — so the array
	// caps now buy reassembly protection they did not before.
	dyn := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: u32 } }\n"
	h, err = genHeader(t, dyn, "m.hpp", map[string]any{"max_dyn_array_count": 8})
	if err != nil {
		t.Fatalf("generate dyn: %v", err)
	}
	for _, want := range []string{
		"#define SOFAB_MAX_DYN_BUFFERED_FIELD 82",
		"sofab::IStreamObject<M> in{sofab::Limits{SOFAB_MAX_DYN_BUFFERED_FIELD}};",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("dynamic-array cap missing %q:\n%s", want, h)
		}
	}

	// A field kind left uncapped has no legitimate maximum, and the reassembly cap
	// is one number for the whole stream — so none is emitted rather than one that
	// would reject valid traffic. The per-field policy guard still stands.
	mixed := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      s: { id: 0, type: string }\n" +
		"      b: { id: 1, type: blob }\n" // unbounded AND uncapped
	h, err = genHeader(t, mixed, "m.hpp", map[string]any{"max_dyn_string_len": 16})
	if err != nil {
		t.Fatalf("generate mixed: %v", err)
	}
	if !strings.Contains(h, "if (_size > SOFAB_MAX_DYN_STRING_LEN) { is.exceedLimit(); return; }") {
		t.Errorf("the configured per-field string guard must still be emitted:\n%s", h)
	}
	for _, notWant := range []string{"SOFAB_MAX_DYN_BUFFERED_FIELD", "sofab::Limits{"} {
		if strings.Contains(h, notWant) {
			t.Errorf("an uncapped dynamic field must leave reassembly uncapped, got %q:\n%s", notWant, h)
		}
	}
}

// TestCppMetadataDocs verifies the metadata doc-comment contract: enum-constant
// descriptions, bitfield-flag descriptions plus their default note, and a
// deprecated field's [[deprecated]] attribute, @deprecated doc note, and the
// warning-suppression pragma that keeps the generated encode/decode clean.
func TestCppMetadataDocs(t *testing.T) {
	src := "version: 1\n" +
		"$defs:\n" +
		"  enum:\n" +
		"    Mode:\n" +
		"      Off:    { value: 0, description: \"Node is powered down.\" }\n" +
		"      Active: { value: 1, description: \"Node is sampling and transmitting.\" }\n" +
		"      Fault:  { value: 2, description: \"Node detected an unrecoverable fault.\" }\n" +
		"  bitfield:\n" +
		"    StatusFlags:\n" +
		"      ready:      { pos: 0, default: true, description: \"Node has completed initialization.\" }\n" +
		"      overheated: { pos: 1, description: \"Core temperature exceeded the safe threshold.\" }\n" +
		"messages:\n" +
		"  Telemetry:\n" +
		"    payload:\n" +
		"      legacyId: { id: 1, type: u32, description: \"Old identifier retained for backward compatibility.\", deprecated: true }\n" +
		"      mode:     { id: 2, type: enum, enum: { $ref: \"#/$defs/enum/Mode\" }, description: \"Current operating mode.\" }\n" +
		"      status:   { id: 3, type: bitfield, bits: { $ref: \"#/$defs/bitfield/StatusFlags\" }, description: \"Health flags for this sample.\" }\n"
	h := headerFromYAML(t, src, "telemetry.hpp")
	for _, want := range []string{
		// enum-constant descriptions
		"Off = 0,  ///< Node is powered down.",
		"Active = 1,  ///< Node is sampling and transmitting.",
		"Fault = 2,  ///< Node detected an unrecoverable fault.",
		// bitfield-flag descriptions + default note
		"BitfieldStatusFlagsReady = 1,  ///< Node has completed initialization. (default: true)",
		"BitfieldStatusFlagsOverheated = 2,  ///< Core temperature exceeded the safe threshold.",
		// deprecated field: native attribute + doc note
		"[[deprecated]] std::uint32_t legacyId = 0;  ///< Old identifier retained for backward compatibility. @deprecated",
		// warning-suppression pragma around the generated member functions
		"#pragma GCC diagnostic push",
		"#pragma GCC diagnostic ignored \"-Wdeprecated-declarations\"",
		"#pragma GCC diagnostic pop",
		// the default constructor is explicitly defaulted inside the suppressed
		// span so its use of the deprecated member's initializer never warns
		"Telemetry() = default;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	// A flag without a default must NOT get a default note.
	if strings.Contains(h, "safe threshold. (default:") {
		t.Errorf("flag without a schema default must not carry a default note:\n%s", h)
	}
}

// trimSrc is a def covering every native element family behind a `count: N`
// (trimmed) plus a nested matrix row and, on the heap profile, a count-less
// array (both untrimmed).
const trimSrc = "version: 1\nmessages:\n  M:\n    payload:\n" +
	"      u32s: { id: 0, type: array, items: { type: u32, count: 5 } }\n" +
	"      f32s: { id: 1, type: array, items: { type: fp32, count: 3 } }\n" +
	"      ens: { id: 2, type: array, items: { type: enum, count: 3, enum: { a: 0, b: 1 } } }\n" +
	"      bls: { id: 3, type: array, items: { type: boolean, count: 4 } }\n" +
	"      matrix: { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }\n"

// TestCppNativeArrayWritesEveryElement: `count: N` is a CAPACITY, not a length
// (MESSAGE_SPEC §3 as tightened by documentation#29): the wire count M IS the
// array's length, so nothing that carries it may be elided. The
// trailing-default-run trim this backend used to apply (sofab::trimTail) was
// correct only under the superseded fixed-length reading and is gone -- with it
// [1,2,3,0,0] would encode exactly like [1,2,3] and decode two elements short.
// Both corelibs write through the same templated span overload, so both profiles
// hand over the whole container.
func TestCppNativeArrayWritesEveryElement(t *testing.T) {
	for _, corelib := range []string{"cpp", "c-cpp"} {
		t.Run(corelib, func(t *testing.T) {
			h, err := genHeader(t, trimSrc, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": corelib})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			wants := []string{
				// Numeric + float fields hand the container over whole.
				"(void)os.write(0, u32s);",
				"(void)os.write(1, f32s);",
				// An enum value-converts through a native temp, element for element.
				"(void)os.write(2, _t0); }",
			}
			// A boolean array's element is bool on the corelib-cpp leg (converted
			// through a temp) and std::uint8_t on the c-cpp leg, where the member
			// already holds the wire bytes and is handed over whole.
			if corelib == "c-cpp" {
				wants = append(wants, "(void)os.write(3, bls);")
			} else {
				wants = append(wants, "(void)os.write(3, _t0); }")
			}
			for _, want := range wants {
				if !strings.Contains(h, want) {
					t.Errorf("[%s] header missing %q:\n%s", corelib, want, h)
				}
			}
			// The superseded helper must not be called at all any more. The corelibs
			// still ship it (marked deprecated) so older generated code compiles;
			// this generator must not reach for it.
			if strings.Contains(h, "trimTail") {
				t.Errorf("[%s] a native array must not be trimmed (count is a capacity):\n%s", corelib, h)
			}
			// A nested matrix ROW is an array ELEMENT, so it takes the positional
			// rule: an interior row equal to the element default is not written and
			// leaves an id gap; the last row always is. The row has no frame of its
			// own (it is one count-prefixed value), so the rule lands on the write.
			rowType := "std::vector<std::uint32_t>"
			if corelib == "c-cpp" {
				rowType = "sofab::InlineVector<std::uint32_t, 3>"
			}
			if !strings.Contains(h, "if (_e0 != "+rowType+"{} || _i0 + 1 == _n0) {") {
				t.Errorf("[%s] a matrix row must be omitted in the interior when default:\n%s", corelib, h)
			}
			if !strings.Contains(h, "(void)os.write(static_cast<sofab::id>(_i0), _e0);") {
				t.Errorf("[%s] a matrix row must still be written whole:\n%s", corelib, h)
			}
			// The member carries a LENGTH: a `count: 5` array with no default is
			// EMPTY, not five zeros, so the profiles cannot disagree about what
			// [1, 2] means on this field.
			wantMember := "std::vector<std::uint32_t> u32s = {};"
			if corelib == "c-cpp" {
				wantMember = "sofab::InlineVector<std::uint32_t, 5> u32s = {};"
			}
			if !strings.Contains(h, wantMember) {
				t.Errorf("[%s] a count:N array must be length-carrying (%s):\n%s", corelib, wantMember, h)
			}
			// Both corelibs read through readArray, which carries the bound and
			// performs the reset behind the tag match; the c-cpp signature also
			// takes the wire count, since it sizes a dynamic destination. Nothing
			// follows it: there is no fill-back to N (§3).
			wantRead := "is.readArray(u32s, 5, -1, sofab::ElemBound::of<std::uint32_t>());"
			if corelib == "c-cpp" {
				wantRead = "is.readArray(u32s, _count, 5);"
			}
			if !strings.Contains(h, wantRead) {
				t.Errorf("[%s] fixed-count decode must read the whole array (%s):\n%s", corelib, wantRead, h)
			}
			if strings.Contains(h, "fillTo") {
				t.Errorf("[%s] decode must not refill a count:N array (count is a capacity):\n%s", corelib, h)
			}
		})
	}
}

// TestCppDynamicArrayNotTrimmed: neither a count-less nor a `count: N` array is
// narrowed on encode -- since documentation#29 the two are the same rule, so the
// counted one lost its carve-out too.
func TestCppDynamicArrayNotTrimmed(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      dyn: { id: 0, type: array, items: { type: u32 } }\n" +
		"      dynf: { id: 1, type: array, items: { type: fp32 } }\n" +
		"      dynen: { id: 2, type: array, items: { type: enum, enum: { a: 0, b: 1 } } }\n" +
		"      dynbl: { id: 3, type: array, items: { type: boolean } }\n" +
		"      fixed: { id: 4, type: array, items: { type: u32, count: 4 } }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": "cpp"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"(void)os.write(0, dyn);",
		"(void)os.write(1, dynf);",
		"(void)os.write(2, _t0); }",
		"(void)os.write(3, _t0); }",
		"(void)os.write(4, fixed);", // the counted one no longer trims either
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "trimTail") {
		t.Errorf("no array may be trimmed any more:\n%s", h)
	}
}

// TestCppFixedCountResetsSchemaDefaultTail: a `count: N` array decodes to N
// elements — M from the wire, the ELEMENT default (zero) at [M,N) (MESSAGE_SPEC
// §3). The std::array member starts at the field's *declaration* default, so a
// non-zero SCHEMA default would leak into the tail the corelib's span read never
// touches: `default: [1,2,3]` on `count: 5` decoding a 2-element wire [1,2] would
// yield [1,2,3,0,0] instead of [1,2,0,0,0]. The encode trim (F-0010) is what
// makes that short wire reachable, so the reset ships with it.
//
// The reset is gated on a non-zero schema default: every other schema's decode
// stays byte-identical.
func TestCppFixedCountResetsSchemaDefaultTail(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: u32, count: 5 } }\n" + // no default
		"      b: { id: 1, type: array, items: { type: u32, count: 3 }, default: [0, 0, 0] }\n" + // all-zero default
		"      c: { id: 2, type: array, items: { type: u32, count: 5 }, default: [1, 2, 3] }\n" + // non-zero default
		"      d: { id: 3, type: array, items: { type: fp32, count: 3 }, default: [1.5, 0.0] }\n" + // non-zero fp default
		"      e: { id: 4, type: array, items: { type: boolean, count: 3 }, default: [true, false] }\n" // non-zero bool default
	for _, corelib := range []string{"cpp", "c-cpp"} {
		t.Run(corelib, func(t *testing.T) {
			h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": corelib})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			// The member still declares the schema default: an ABSENT field must
			// reconstruct to it (sparse-omission contract, MESSAGE_SPEC S2).
			wantC := "std::vector<std::uint32_t> c = {1, 2, 3};"
			if corelib == "c-cpp" {
				wantC = "sofab::InlineVector<std::uint32_t, 5> c = {1, 2, 3};"
			}
			if !strings.Contains(h, wantC) {
				t.Errorf("[%s] schema default must stay the member's declaration default:\n%s", corelib, h)
			}
			// A non-zero schema default must be reset on decode so the elements the
			// encoder trimmed off the tail come back as the ELEMENT default, not as
			// that schema default (MESSAGE_SPEC S3). Both corelibs now do it inside
			// readArray — behind the tag match and the bound, which is the order
			// that makes a §7.3-skipped or rejected occurrence leave the target
			// alone. Neither profile resets in the arm any more.
			{
				for _, bad := range []string{"\n            c = {};", "\n            d = {};", "\n            e = {};"} {
					if strings.Contains(h, bad) {
						t.Errorf("[%s] the arm must not reset %q — readArray does it behind the bound:\n%s", corelib, bad, h)
					}
				}
				wantRead := "is.readArray(c, 5, -1, sofab::ElemBound::of<std::uint32_t>());"
				if corelib == "c-cpp" {
					wantRead = "is.readArray(c, _count, 5);"
				}
				if !strings.Contains(h, wantRead) {
					t.Errorf("[%s] the bound must ride into readArray (%s):\n%s", corelib, wantRead, h)
				}
			}
			// A field with no schema default, or an all-zero one, already declares an
			// all-zero array: no reset, generated code unchanged.
			for _, bad := range []string{"\n            a = {};", "\n            b = {};"} {
				if strings.Contains(h, bad) {
					t.Errorf("[%s] zero/absent-default array must not emit a reset, found %q:\n%s", corelib, bad, h)
				}
			}
		})
	}
}

// TestCppDynamicArrayNoReset: a count-less array lowers to a std::vector that
// decode resizes to the wire count, so it has no stale tail and needs no reset.
func TestCppDynamicArrayNoReset(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      dyn: { id: 0, type: array, items: { type: u32 }, default: [1, 2, 3] }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": "cpp"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(h, "\n            dyn = {};") {
		t.Errorf("dynamic array must not emit a reset:\n%s", h)
	}
	if !strings.Contains(h, "is.readArray(dyn, -1, -1, sofab::ElemBound::of<std::uint32_t>());") {
		t.Errorf("dynamic array should read through readArray (which sizes it):\n%s", h)
	}
}

// g_containsInOrder reports whether the needles appear in s in the given order.
func g_containsInOrder(s string, needles ...string) bool {
	for _, n := range needles {
		i := strings.Index(s, n)
		if i < 0 {
			return false
		}
		s = s[i+len(n):]
	}
	return true
}

// TestCppWireTypeGuard pins where the MESSAGE_SPEC §7.3 decision lives on the
// pure-corelib-cpp path. It used to be a generated compare in every case arm
// (generator#174); the corelib now makes it inside the typed read itself, which
// compares the delivered field's whole wire tag against the one the read declares
// and leaves a contradicting field unconsumed for the driver to skip (the seam,
// docs/models/type-reconciliation.md).
//
// So a scalar, fixlen or struct/union arm carries NO guard, and the fixlen kinds
// state their subtype by calling readString/readBlob rather than a bare read().
// Array arms keep theirs: the arm resets its destination before the read, and that
// reset must not run for a field §7.3 skips (§7.4 — a skipped occurrence is not an
// occurrence).
func TestCppWireTypeGuard(t *testing.T) {
	h := headerFromYAML(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: i32 }
      c: { id: 2, type: boolean }
      d: { id: 3, type: fp32 }
      e: { id: 4, type: fp64 }
      f: { id: 5, type: string, maxlen: 8 }
      g: { id: 6, type: blob, maxlen: 8 }
      h: { id: 7, type: struct, fields: { x: { id: 0, type: u8 } } }
      i: { id: 8, type: array, items: { type: u32, count: 2 } }
      j: { id: 9, type: array, items: { type: i32, count: 2 } }
      k: { id: 10, type: array, items: { type: fp64, count: 2 } }
      l: { id: 11, type: array, items: { type: string, count: 2, maxlen: 4 } }
`, "m.hpp")
	// Every kind states its expectation through the call it makes, and the corelib
	// compares the tag: readString/readBlob name the fixlen subtype, readArray the
	// array kind (and carries the bounds), read() the rest.
	for _, want := range []string{
		"is.readString(f, 8);",
		"is.readBlob(g, 8);",
		"is.readArray(i, 2, -1, sofab::ElemBound::of<std::uint32_t>());",
		"is.readArray(j, 2, -1, sofab::ElemBound::of<std::int32_t>());",
		"is.readArray(k, 2);",
		"is.read(h);", // nested struct
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.hpp missing %q\n%s", want, h)
		}
	}
	// NOTHING in a pure-corelib-cpp deserialize compares a wire type any more —
	// that is the point of the seam. A single leftover comparison is a regression.
	//
	// Scoped to the message types: the shared wrapper-array collector emitted
	// ahead of the namespace does compare one, because an ELEMENT skipped for a
	// contradicting wire type must leave the container untouched and only the
	// collector can see that (there is no typed read to fold the decision into).
	body := h
	if i := strings.Index(body, "#endif // SOFABGEN_WRAPPER_SEQ_HELPERS"); i >= 0 {
		body = body[i:]
	}
	if strings.Contains(body, "is.wire() !=") || strings.Contains(body, "is.fixType() !=") {
		t.Errorf("the pure-cpp deserialize must carry no wire comparison at all\n%s", h)
	}
}

// TestCppWireTypeGuardOnCCpp: corelib-c-cpp now exposes IStreamImpl::wire() /
// fixType() with the same sofab::Wire/sofab::Fix surface as corelib-cpp
// (corelib-c-cpp#104), so the §7.3 guard is emitted for the c-cpp profile too —
// the same shape as the pure-C++ path. Emitting it is also what makes the §7.4
// interaction rule hold there: the guard's `break` precedes the array-wrapper
// clear, so a mis-typed later occurrence cannot wipe a valid earlier array.
func TestCppWireTypeGuardOnCCpp(t *testing.T) {
	h, err := fixedHeader(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: string, maxlen: 8 }
`, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// No profile emits a wire comparison any more. The c-cpp wrapper was the last
	// one to: its C decoder now unbinds a contradicting read and skips the field,
	// and the arms that must touch their destination before binding it go through
	// readString/readBlob/readArray/readSequence, which settle the tag first.
	for _, bad := range []string{"is.wire()", "is.fixType()"} {
		if strings.Contains(h, bad) {
			t.Errorf("c-cpp deserialize must carry no %q comparison:\n%s", bad, h)
		}
	}
	for _, want := range []string{"is.read(a);", "is.readString(b, _size, 8);"} {
		if !strings.Contains(h, want) {
			t.Errorf("c-cpp profile must emit %q\n%s", want, h)
		}
	}
}

// TestCppWrapperArrayCleared pins the MESSAGE_SPEC §7.4 array-wrapper half
// (generator#175): an array wrapper IS the array's value (§5), so a repeated
// field id replaces it whole. The collectors place by element index / emplace in
// arrival order and never reset, so the case arm must clear the target first —
// otherwise a second opening merges into the first one's elements. Native scalar
// arrays read the whole array in one call and already replace, so they get no
// clear.
func TestCppWrapperArrayCleared(t *testing.T) {
	h := headerFromYAML(t, `
version: 1
messages:
  M:
    payload:
      strs:   { id: 0, type: array, items: { type: string, count: 3, maxlen: 4 } }
      blobs:  { id: 1, type: array, items: { type: blob, count: 3, maxlen: 4 } }
      msgs:   { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: u8 } } } }
      native: { id: 3, type: array, items: { type: u32, count: 3 } }
`, "m.hpp")
	// The replace-whole reset moved out of the case arm entirely: it is prepare()
	// on the collectors (corelib sofab::StringSeq / BlobSeq for the leaves, the
	// generated sofabgen::WrapperSeq for the object path), which
	// IStreamImpl::read calls once the SequenceStart tag matched — so it can no
	// longer wipe a valid earlier occurrence on a §7.3-skipped one. The arms must
	// therefore carry no clear at all, and must name a collector.
	for _, want := range []string{
		"sofab::StringSeq _r0{strs,",
		"sofab::BlobSeq _r0{blobs,",
		"sofabgen::WrapperSeq<",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.hpp missing corelib collector %q\n%s", want, h)
		}
	}
	for _, notWant := range []string{"strs.clear();", "blobs.clear();", "msgs.clear();", "native.clear();"} {
		if strings.Contains(h, notWant) {
			t.Errorf("the arm must not clear %q — prepare() does it behind the tag match:\n%s", notWant, h)
		}
	}
}

// TestCppNativeArrayIsNotPaddedToCount pins the construction value of a
// `count: N` native array in BOTH storage modes, now that `count` is a CAPACITY
// rather than a length (MESSAGE_SPEC §3 / documentation#29).
//
// The growable storage (allow_dynamic) carries its own length, so it expresses
// 0..N: a fresh array is EMPTY and a declared `default` shorter than N is
// materialized exactly as written, never tail-padded. That is what keeps
// [10, 20] a two-element value distinct from [10, 20, 0, 0].
//
// The fixed std::array<T,N> has no logical length at all, so its value is always
// N elements: the aggregate initializer zero-fills whatever the default leaves
// out, and a shorter value is simply not representable there. That divergence is
// storage, not spec — see docs/generator/cpp.md.
func TestCppNativeArrayIsNotPaddedToCount(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      zeros:   { id: 0, type: array, items: { type: u32, count: 4 } }\n" +
		"      partial: { id: 1, type: array, items: { type: u32, count: 4 }, default: [10, 20] }\n" +
		"      floats:  { id: 2, type: array, items: { type: fp32, count: 3 } }\n" +
		"      flags:   { id: 3, type: array, items: { type: boolean, count: 2 } }\n"

	dyn, err := genHeader(t, src, "m.hpp", map[string]any{"corelib": "c-cpp", "allow_dynamic": true})
	if err != nil {
		t.Fatalf("generate allow_dynamic: %v", err)
	}
	for _, want := range []string{
		"std::vector<std::uint32_t> zeros = {};",
		"std::vector<std::uint32_t> partial = {10, 20};",
		"std::vector<float> floats = {};",
		// A boolean array's element is std::uint8_t on the c-cpp leg: the C
		// decoder writes into the destination's own bytes and std::vector<bool>
		// is the bit-packed specialisation, which has none.
		"std::vector<std::uint8_t> flags = {};",
	} {
		if !strings.Contains(dyn, want) {
			t.Errorf("a bounded array in dynamic storage must not be padded to its count, missing %q:\n%s", want, dyn)
		}
	}
	// The JSON harness index-assigns into the container, so a growable one has to
	// be sized to the input first -- otherwise an empty member swallows every
	// element (the same MAX_SIZE fill symptom the padding used to hide).
	dynJSON, err := genHeader(t, src, "harness/json.hpp", map[string]any{"corelib": "c-cpp", "allow_dynamic": true, "emit": "project"})
	if err != nil {
		t.Fatalf("generate allow_dynamic project: %v", err)
	}
	if !strings.Contains(dynJSON, "o.zeros.resize(sofab_json_array_size(c));") {
		t.Errorf("a growable native array must be sized from the JSON input:\n%s", dynJSON)
	}

	// The heap-free storage mode expresses the SAME values, in sofab::InlineVector
	// -- inline slots plus a logical length. That is the whole point: without a
	// length it could only ever hold N elements, so [10, 20] on a count: 4 field
	// would be a different value (and a different wire image) here than under
	// allow_dynamic, for one and the same schema.
	fixed, err := genHeader(t, src, "m.hpp", map[string]any{"corelib": "c-cpp"})
	if err != nil {
		t.Fatalf("generate fixed: %v", err)
	}
	for _, want := range []string{
		"sofab::InlineVector<std::uint32_t, 4> zeros = {};",
		"sofab::InlineVector<std::uint32_t, 4> partial = {10, 20};",
	} {
		if !strings.Contains(fixed, want) {
			t.Errorf("heap-free storage must carry a length, missing %q:\n%s", want, fixed)
		}
	}
	if strings.Contains(fixed, "std::array<std::uint32_t, 4>") {
		t.Errorf("no native array member may be a bare std::array any more:\n%s", fixed)
	}
	// The JSON path sizes BOTH containers now: InlineVector has a resize() too
	// (corelib-c-cpp), so neither leg index-assigns into a container that is the
	// wrong length.
	fixedJSON, err := genHeader(t, src, "harness/json.hpp", map[string]any{"corelib": "c-cpp", "emit": "project"})
	if err != nil {
		t.Fatalf("generate fixed project: %v", err)
	}
	if !strings.Contains(fixedJSON, "o.zeros.resize(sofab_json_array_size(c));") {
		t.Errorf("an inline native array must be sized from the JSON input too:\n%s", fixedJSON)
	}
}

// TestCppDeprecatedSuppressionSpansTheClass: the deprecation-suppression pragma
// must enclose the whole class definition, not just its member functions.
//
// A [[deprecated]] member is touched by the implicitly-defined special member
// functions — destructor, copy/move constructor, assignment — and those
// definitions are located AT THE CLASS. With the region starting after the
// member declarations, a consumer that merely declared a message value got a
// deprecation warning for a field it never named, pointing at a header line it
// cannot edit. The attribute then fires for everyone instead of for the one
// caller still using the field, which is the opposite of what marking it
// deprecated is for.
//
// The attribute itself stays on the member: a consumer writing msg.oldField
// must still be warned, at its own line.
func TestCppDeprecatedSuppressionSpansTheClass(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      keep: { id: 0, type: u32 }\n" +
		"      old:  { id: 1, type: u32, deprecated: true }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	push := strings.Index(h, "#pragma GCC diagnostic ignored \"-Wdeprecated-declarations\"")
	open := strings.Index(h, "struct M : sofab::Message {")
	pop := strings.Index(h, "#pragma GCC diagnostic pop")
	if push < 0 || open < 0 || pop < 0 {
		t.Fatalf("expected a suppressed span around the class:\n%s", h)
	}
	if push > open {
		t.Errorf("the suppression must start BEFORE the class, or the implicit "+
			"destructor/copy members warn at every consumer:\n%s", h)
	}
	if pop < strings.LastIndex(h[:pop+1], "};") {
		t.Errorf("the suppression must end after the class closes:\n%s", h)
	}
	// The attribute survives, so a consumer touching the field is still warned.
	if !strings.Contains(h, "[[deprecated]] std::uint32_t old") {
		t.Errorf("the member must keep its [[deprecated]] attribute:\n%s", h)
	}
	// A message with nothing deprecated emits no pragma at all.
	plain, err := genHeader(t, "version: 1\nmessages:\n  m:\n    payload:\n      keep: { id: 0, type: u32 }\n", "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate plain: %v", err)
	}
	if strings.Contains(plain, "diagnostic") {
		t.Errorf("no deprecated field: no pragma should be emitted:\n%s", plain)
	}
}

// TestCppResetPutsEveryFieldBackInPlace pins the generated reset() and the reuse
// contract it exists for.
//
// A field whose value equals its declared default is omitted from the encoded
// bytes entirely - a sequence-typed one included. An omitted field delivers no
// deserialize() callback, so nothing on the callback side can clear a reused
// destination: the array-replaces-whole clear inside the wrapper collectors
// hangs off the sequence header, which an absent field never sends. The clear
// therefore has to happen at the START of the decode, which is what reset() is.
func TestCppResetPutsEveryFieldBackInPlace(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      items:  { id: 0, type: array, items: { type: struct, count: 4, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      names:  { id: 1, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      nums:   { id: 2, type: array, items: { type: u16, count: 3 }, default: [7, 8, 9] }\n" +
		"      nested: { id: 3, type: struct, fields: { a: { id: 0, type: i32, default: -4 }, s: { id: 1, type: string, maxlen: 8, default: \"hi\" } } }\n" +
		"      tag:    { id: 4, type: i32, default: 3 }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"    void reset() noexcept {",
		// Every member goes back to the value its DECLARATION carries, so the
		// reset and the wire-absent value cannot drift apart. A `count: N` array
		// is EMPTY at rest -- `count` is a capacity, not a length (MESSAGE_SPEC
		// §3) -- and a declared default shorter than the count stands as written.
		"        items = {};",
		"        names = {};",
		"        nums = {7, 8, 9};",
		"        tag = 3;",
		// A struct member recurses instead of being assigned a fresh temporary:
		// in place, so the nested containers keep their capacity too.
		"        nested.reset();",
		// The nested type carries its own reset, with its own declared defaults.
		"        a = -4;",
		"        s = \"hi\";",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("reset() missing %q:\n%s", want, h)
		}
	}
	// Public: a caller driving the stream itself owns its destination and needs
	// the same call. It must not be tucked below a private:.
	if strings.Contains(h, "private:") {
		t.Errorf("reset() must stay public - generated messages have no private section:\n%s", h)
	}
	// The reuse entry point resets the caller's destination and then decodes
	// straight into it. Staging in a second instance and copying across would
	// mask the staleness but throw away every buffer `out` already holds, which
	// is the only reason to pass a destination in.
	for _, want := range []string{
		"static sofab::IStreamImpl::Result try_decode(const std::uint8_t *data, std::size_t len, M &out) {",
		"        out.reset();",
		"        sofab::IStreamInline _is{[&out, &_isp](sofab::id _id, std::size_t _size, std::size_t _count) {",
		"            out.deserialize(*_isp, _id, _size, _count);",
		"        return _is.feed(data, len);",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("try_decode missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "out = *in;") {
		t.Errorf("try_decode must decode into the caller's destination, not copy into it:\n%s", h)
	}
	// The reset runs before the bytes are fed - after would undo the decode.
	if strings.Index(h, "out.reset();") > strings.Index(h, "return _is.feed(data, len);") {
		t.Errorf("out.reset() must precede the feed:\n%s", h)
	}
	// The sequence-start clear is untouched: a re-opened wrapper still replaces
	// the array whole rather than merging into the earlier occurrence.
	if !strings.Contains(h, "sofabgen::WrapperSeq<std::vector<MItemsElem>> _r0; _r0.out = &items;") {
		t.Errorf("the wrapper collector (and its replace-whole clear) must be unchanged:\n%s", h)
	}
}

// TestCppTryDecodeCarriesTheDerivedReassemblyCap: IStreamInline takes the field
// callback first, so a configured cap rides in as a TRAILING argument - the
// IStreamObject spelling (the cap as the only argument) does not compile there.
func TestCppTryDecodeCarriesTheDerivedReassemblyCap(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: u32 } }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{"max_dyn_array_count": 8})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(h, "}, sofab::Limits{SOFAB_MAX_DYN_BUFFERED_FIELD}};") {
		t.Errorf("try_decode's stream must carry the cap after the callback:\n%s", h)
	}
	// The uncapped case passes the callback alone (the fixed profile's
	// IStreamInline has no limits parameter at all).
	plain, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate plain: %v", err)
	}
	if strings.Contains(plain, "sofab::Limits") {
		t.Errorf("no cap configured: no Limits argument:\n%s", plain)
	}
}

// TestCppFixedProfileKeepsItsDecodeShape: the heap-free profile still emits
// reset(), but try_decode keeps decoding into a fresh instance and copying it
// over the destination.
//
// There is nothing to reuse there - the containers are inline, so the copy is a
// memcpy with no allocation to hand back - and the destination it decodes into
// starts at the declared defaults every time, so it cannot go stale. Routing it
// through a callback instead would put std::function in .text on the targets
// that have the least of it: this profile's IStreamObject dispatches through a
// C-ABI function pointer, IStreamInline does not.
func TestCppFixedProfileKeepsItsDecodeShape(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      names: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      tag:   { id: 1, type: i32, default: 3 }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{"corelib": "c-cpp"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"    void reset() noexcept {",
		"        names = {};",
		"        tag = 3;",
		"        sofab::IStreamObject<M> in;",
		"        if (r.ok()) { out = *in; }",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("fixed profile missing %q:\n%s", want, h)
		}
	}
	// No callback plumbing: that is the whole point of keeping this shape.
	// (reset()'s doc comment names IStreamInline as the thing a caller might be
	// driving; what must not appear is a stream instantiated here.)
	if strings.Contains(h, "sofab::IStreamInline _is") {
		t.Errorf("the fixed profile must not route try_decode through a std::function:\n%s", h)
	}
}

// nestedWrapperRowsSrc is the shape of generator#250: a nested array whose ROW
// is itself a wrapper sequence (string / blob / struct elements), at depth 2 and
// depth 3, alongside a native-row control that must keep its existing lowering.
const nestedWrapperRowsSrc = "version: 1\nmessages:\n  M:\n    payload:\n" +
	"      strrows:    { id: 0, type: array, items: { type: array, count: 2, items: { type: string, count: 3, maxlen: 8 } } }\n" +
	"      blobrows:   { id: 1, type: array, items: { type: array, count: 2, items: { type: blob,   count: 3, maxlen: 8 } } }\n" +
	"      structrows: { id: 2, type: array, items: { type: array, count: 2, items: { type: struct, count: 3, fields: { a: { id: 0, type: u32 } } } } }\n" +
	"      deep:       { id: 3, type: array, items: { type: array, count: 2, items: { type: array, count: 2, items: { type: string, count: 3, maxlen: 8 } } } }\n" +
	"      urows:      { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }\n"

// TestCppNestedWrapperRowsHeap: a row of strings/blobs/structs is a wrapper
// SEQUENCE — neither a span of scalars nor an IStreamMessage — so handing the
// ROW CONTAINER to sofab::MessageSeq<T> makes its is.read(row) fail the
// corelib's "Unsupported span element type in IStream::read()" static_assert and
// the whole header stops compiling (generator#250). Such a row gets a generated
// collector that places the row at its element id and reads it with the SAME
// emission the first level uses (StringSeq / BlobSeq / MessageSeq over the
// ELEMENT type). A row of native scalars is unaffected: the corelib collector
// reads it directly.
func TestCppNestedWrapperRowsHeap(t *testing.T) {
	h, err := genHeader(t, nestedWrapperRowsSrc, "m.hpp", map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		// string rows: outer collector with the schema count as its cap, inner read
		// through the corelib's string collector over the ROW.
		"struct _S0 : sofab::IStreamMessage {",
		"std::vector<std::vector<std::string>> *out = nullptr;",
		"long cap = 2;",
		"{ sofab::StringSeq _r1{_e0, 3, 8}; is.read(_r1); }",
		"_S0 _r0; _r0.out = &strrows; is.read(_r0);",
		// blob rows
		"{ sofab::BlobSeq _r1{_e0, 3, 8}; is.read(_r1); }",
		// struct rows: MessageSeq over the ELEMENT type, not the row container
		"{ sofabgen::WrapperSeq<std::vector<MStructrowsElemElem>> _r1; _r1.out = &_e0; _r1.cap = 3; is.read(_r1); }",
		// depth 3: the row collector nests, one level further
		"struct _S1 : sofab::IStreamMessage {",
		"{ sofab::StringSeq _r2{_e1, 3, 8}; is.read(_r2); }",
		// §5.1 placement + over-index reject, §7.4 replace-whole
		"if (cap >= 0 && static_cast<std::size_t>(_id) >= static_cast<std::size_t>(cap)) { is.invalidate(); return; }",
		"while (out->size() <= static_cast<std::size_t>(_id)) out->emplace_back();",
		"void prepare() noexcept { if (out) out->clear(); }",
		// native rows keep the corelib collector
		"sofabgen::WrapperSeq<std::vector<std::vector<std::uint32_t>>> _r0; _r0.out = &urows;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("nested wrapper rows missing %q:\n%s", want, h)
		}
	}
	// The bug itself: a row CONTAINER handed to MessageSeq<T> is the
	// non-compiling adapter. None of these may appear.
	for _, notWant := range []string{
		"sofab::MessageSeq<std::vector<std::string>>",
		"sofab::MessageSeq<std::vector<std::vector<std::uint8_t>>>",
		"sofab::MessageSeq<std::vector<MStructrowsElemElem>>",
		"sofab::MessageSeq<std::vector<std::vector<std::string>>>",
	} {
		if strings.Contains(h, notWant) {
			t.Errorf("wrapper row must not be read as %q (static_assert in IStream::read):\n%s", notWant, h)
		}
	}
}

// TestCppNestedWrapperRowsFixed: the same shape on the footprint leg
// (corelib: c-cpp). The collector is static — the C decoder dereferences it
// after the callback returns — reads through readSequence (which clears the
// destination for §7.4), and takes its §5.1 over-index bound from the inline
// container's capacity, which also stops InlineVector's saturating
// emplace_back() from spinning on an over-index id (issue #126).
func TestCppNestedWrapperRowsFixed(t *testing.T) {
	h, err := fixedHeader(t, nestedWrapperRowsSrc, "m.hpp", nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"struct _S0 : sofab::IStreamMessage {",
		"sofab::InlineVector<sofab::InlineVector<sofab::FixedString<8>, 3>, 2> *out = nullptr;",
		"if (static_cast<std::size_t>(_id) >= out->capacity()) { is.invalidate(); return; }",
		"static sofab::FixedStringSeq<sofab::InlineVector<sofab::FixedString<8>, 3>> _r1;",
		"static sofab::FixedStringSeq<sofab::InlineVector<sofab::FixedString<8>, 3>> _r1; is.readSequence(_r1, _e0); }",
		"static _S0 _r0; is.readSequence(_r0, strrows);",
		"static sofab::FixedBlobSeq<sofab::InlineVector<sofab::FixedBytes<8>, 3>> _r1;",
		"static sofabgen::WrapperSeq<sofab::InlineVector<MStructrowsElemElem, 3>> _r1; _r1.cap = 3;",
		"struct _S1 : sofab::IStreamMessage {",
		"static sofab::FixedStringSeq<sofab::InlineVector<sofab::FixedString<8>, 3>> _r2;",
		"static sofab::FixedStringSeq<sofab::InlineVector<sofab::FixedString<8>, 3>> _r2; is.readSequence(_r2, _e1); }",
		// native rows keep the corelib collector
		"static sofabgen::WrapperSeq<sofab::InlineVector<sofab::InlineVector<std::uint32_t, 3>, 2>> _r0; _r0.cap = 2;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("fixed nested wrapper rows missing %q:\n%s", want, h)
		}
	}
	for _, notWant := range []string{
		"sofab::FixedMessageSeq<sofab::InlineVector<sofab::InlineVector<sofab::FixedString<8>, 3>, 2>>",
		"sofab::FixedMessageSeq<sofab::InlineVector<sofab::InlineVector<sofab::FixedBytes<8>, 3>, 2>>",
		"sofab::FixedMessageSeq<sofab::InlineVector<sofab::InlineVector<MStructrowsElemElem, 3>, 2>>",
	} {
		if strings.Contains(h, notWant) {
			t.Errorf("wrapper row must not be read as %q (static_assert in IStream::read):\n%s", notWant, h)
		}
	}
	// The heap storage mode of the same leg: the bound is no longer a container
	// capacity, so it rides in as `cap`, and the row vector is reserved so
	// placing a later row never moves a still-bound earlier one.
	d, err := fixedHeader(t, nestedWrapperRowsSrc, "m.hpp", map[string]any{"allow_dynamic": true})
	if err != nil {
		t.Fatalf("generate dynamic: %v", err)
	}
	for _, want := range []string{
		"long cap = 2;",
		"if (cap >= 0 && static_cast<std::size_t>(_id) >= static_cast<std::size_t>(cap)) { is.invalidate(); return; }",
		"strrows.reserve(2);",
		"static _S0 _r0; is.readSequence(_r0, strrows);",
		"static sofab::StringSeq _r1; _r1.cap = 3; _r1.elemMax = 8;",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("c-cpp allow_dynamic nested wrapper rows missing %q:\n%s", want, d)
		}
	}
}

// wrapperSeqSrc: a `count: N` struct array next to a dynamic one and a
// `count: N` string array, so one header shows every narrowing decision.
const wrapperSeqSrc = "version: 1\nmessages:\n  vec:\n    payload:\n" +
	"      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }\n" +
	"      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }\n" +
	"      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }\n"

// TestCppWrapperElementsArePlacedByID: a wrapper array's element id IS the array
// index (MESSAGE_SPEC §5.1), so an element is PLACED at dest[id] after
// gap-filling -- never appended. Appending shortens the array by the size of any
// interior id gap and decodes a REOPENED id as a second element instead of
// merging into the first (§7.4). The leaf string/blob collectors in both
// corelibs always got this right; corelib-c-cpp's Fixed/MessageSeq did not, so
// the object path collects through a generated placer on BOTH profiles
// (generator#247).
//
// Interior id gaps used to be unreachable -- every element was framed -- which
// is what made the appending shape survive as long as it did. Since
// documentation#29 an interior all-default element is omitted, so a gap is
// ordinary input and placement is load-bearing.
func TestCppWrapperElementsArePlacedByID(t *testing.T) {
	for _, corelib := range []string{"cpp", "c-cpp"} {
		t.Run(corelib, func(t *testing.T) {
			cfg := map[string]any{}
			src := wrapperSeqSrc
			if corelib == "c-cpp" {
				cfg["corelib"] = "c-cpp"
				cfg["allow_dynamic"] = true
				// The embedded profile requires a count on every array.
				src = strings.Replace(src, "items: { type: struct, fields:", "items: { type: struct, count: 2, fields:", 1)
			}
			h, err := genHeader(t, src, "vec.hpp", cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			for _, want := range []string{
				// placement, not append -- and the gap-fill that precedes it
				"while (out->size() <= static_cast<std::size_t>(id)) { (void)out->emplace_back(); }",
				"Elem &row = (*out)[static_cast<std::size_t>(id)];",
				// the over-index reject, which also bounds the gap-fill
				"if (cap >= 0 && static_cast<long>(id) >= cap) { is.invalidate(); return; }",
				// the schema count rides in as that bound, and only as that bound
				"_r0.cap = 5;",
			} {
				if !strings.Contains(h, want) {
					t.Errorf("[%s] vec.hpp missing %q:\n%s", corelib, want, h)
				}
			}
			// The corelib collectors that append id-blind must be gone from the
			// object path on both profiles -- that is the defect being replaced.
			for _, notWant := range []string{"sofab::FixedMessageSeq<", "sofab::MessageSeq<"} {
				if strings.Contains(h, notWant) {
					t.Errorf("[%s] the appending collector %q must not be used any more:\n%s", corelib, notWant, h)
				}
			}
			// `count: N` is a CAPACITY: it bounds the element id and nothing else.
			// The refill to N that used to follow every wrapper read turned ["a"]
			// into ["a", "", ""] on a count: 3 field -- a different value, since
			// the length is highest present id + 1 (§3/§5.1).
			if strings.Contains(h, "fillTo") {
				t.Errorf("[%s] a decoded wrapper array must never be refilled to its count:\n%s", corelib, h)
			}
		})
	}
	h, err := genHeader(t, wrapperSeqSrc, "vec.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(h, "_r0.cap = -1; is.read(_r0); }") {
		t.Errorf("a count-less wrapper array must read with an unbounded cap:\n%s", h)
	}
}

// TestCppWrapperArrayInteriorIsSparseLastElementKept: one rule for both element
// kinds, positional in the VALUE (MESSAGE_SPEC §2 as tightened by
// documentation#29). An element before the last one that equals its element
// default is omitted and leaves an id GAP -- a string leaf simply not written, a
// struct element not framed either -- while the LAST element is always written,
// as its value or as an empty frame.
//
// The superseded reading trimmed the trailing default run of a `count: N` array
// and framed every interior sequence element unconditionally. Both are gone, and
// a declared count changes nothing: the counted and count-less arrays below emit
// the identical loop.
func TestCppWrapperArrayInteriorIsSparseLastElementKept(t *testing.T) {
	h, err := genHeader(t, wrapperSeqSrc, "vec.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// A struct element: the CLOSER is what carries the rule. writeLazy drops a
	// contentless interior frame; write keeps it at the last index.
	for _, want := range []string{
		"const std::size_t _n0 = fixed.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { if (_i0 + 1 == _n0) { (void)os.write(static_cast<sofab::id>(_i0), fixed[_i0]); } else { (void)os.writeLazy(static_cast<sofab::id>(_i0), fixed[_i0]); } }",
		"const std::size_t _n0 = dynamic.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { if (_i0 + 1 == _n0) { (void)os.write(static_cast<sofab::id>(_i0), dynamic[_i0]); } else { (void)os.writeLazy(static_cast<sofab::id>(_i0), dynamic[_i0]); } }",
		// A string element: the same rule on the write itself, since the leaf has
		// no frame. The counted array gets the same guard as a count-less one.
		"const std::size_t _n0 = fstrs.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { const auto &_e0 = fstrs[_i0]; if (!_e0.empty() || _i0 + 1 == _n0) {",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("element rule missing %q:\n%s", want, h)
		}
	}
	// The FIELD wrapper still closes with the dropping end: an EMPTY array is
	// omitted and absence reconstructs it (§2).
	if !strings.Contains(h, "(void)os.sequenceBeginLazy(0);") || !strings.Contains(h, "(void)os.sequenceEnd();") {
		t.Errorf("the field wrapper must stay lazily opened and dropped by its closer:\n%s", h)
	}
	// _isDefault is the exact negation of what serialize writes. The writer emits
	// a child for every element it holds, so "no child written" is exactly "the
	// array is empty" -- with or without a count. Narrowing on one side only is
	// the drift that omits a field which is on the wire.
	for _, want := range []string{
		"if (!(fixed.size() == 0)) { return false; }",
		"if (!(dynamic.size() == 0)) { return false; }",
		"if (!(fstrs.size() == 0)) { return false; }",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("_isDefault must be computed from the same expression as serialize, missing %q:\n%s", want, h)
		}
	}
	// The superseded narrowing helpers must not be emitted at all.
	for _, bad := range []string{"trimObjs", "trimEmpty", "trimTail", "fillTo"} {
		if strings.Contains(h, bad) {
			t.Errorf("the superseded helper %q must be gone:\n%s", bad, h)
		}
	}
}

// TestCppMistypedWrapperElementLeavesNoPhantom: a child inside a wrapper array
// whose wire type contradicts the declared element is skipped exactly like an
// unknown id (MESSAGE_SPEC §7.3) -- and a skip must leave the container
// byte-for-byte as it was. Both C++ profiles used to grow the container first
// and only then discover the mismatch, leaving a phantom default-initialised
// element behind (generator#249).
//
// The assertion is on ORDER: the wire-type decision has to precede every
// mutation, which is the only shape that cannot leave the phantom.
func TestCppMistypedWrapperElementLeavesNoPhantom(t *testing.T) {
	for _, corelib := range []string{"cpp", "c-cpp"} {
		t.Run(corelib, func(t *testing.T) {
			cfg := map[string]any{}
			src := wrapperSeqSrc
			if corelib == "c-cpp" {
				cfg["corelib"] = "c-cpp"
				cfg["allow_dynamic"] = true
				src = strings.Replace(src, "items: { type: struct, fields:", "items: { type: struct, count: 2, fields:", 1)
			}
			h, err := genHeader(t, src, "vec.hpp", cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			gate := "if (is.wire() != Tag::SequenceStart) { return; }"
			grow := "while (out->size() <= static_cast<std::size_t>(id)) { (void)out->emplace_back(); }"
			gi, wi := strings.Index(h, gate), strings.Index(h, grow)
			if gi < 0 {
				t.Fatalf("[%s] the element wire-type gate is missing:\n%s", corelib, h)
			}
			if wi < 0 {
				t.Fatalf("[%s] the placement fill is missing:\n%s", corelib, h)
			}
			if gi > wi {
				t.Errorf("[%s] the wire-type gate must precede any container growth, or a skipped child leaves a phantom element:\n%s", corelib, h)
			}
			// The gate is only for element kinds read as their own sequence; a
			// nested-array row is not one, hence the compile-time guard.
			if !strings.Contains(h, "if constexpr (std::is_base_of_v<sofab::IStreamMessage, Elem>) {") {
				t.Errorf("[%s] the gate must be scoped to sequence-shaped elements:\n%s", corelib, h)
			}
		})
	}
}

// TestCppCountIsACapacityNotALength pins the length a `count: N` array has at
// rest, in all three storage kinds.
//
// documentation#29 settles it on the schema's side: `count` is a CAPACITY. It
// never reaches the wire, it bounds the array, it lets fixed-storage targets
// pre-size -- and it never adds an element. A fresh count: N wrapper array is
// therefore EMPTY, not N element defaults, in every storage kind that can say so:
// the heap profile's std::vector, the c-cpp allow_dynamic std::vector, and the
// c-cpp inline sofab::InlineVector, whose logical length is separate from its N
// inline slots.
//
// The one container that cannot say so is std::array<T,N>, which the native
// arrays keep: it has no logical length, so its value is always N elements. That
// divergence is storage, not spec (docs/generator/cpp.md).
func TestCppCountIsACapacityNotALength(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      strs:  { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }\n" +
		"      blobs: { id: 1, type: array, items: { type: blob, count: 2, maxlen: 4 } }\n" +
		"      pts:   { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      nums:  { id: 3, type: array, items: { type: u32, count: 3 } }\n"
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{"heap", map[string]any{}, []string{
			"std::vector<std::string> strs = {};",
			"std::vector<std::vector<std::uint8_t>> blobs = {};",
			"std::vector<std::uint32_t> nums = {};",
		}},
		{"c-cpp-inline", map[string]any{"corelib": "c-cpp"}, []string{
			"sofab::InlineVector<sofab::FixedString<8>, 3> strs = {};",
			"sofab::InlineVector<sofab::FixedBytes<4>, 2> blobs = {};",
			"sofab::InlineVector<std::uint32_t, 3> nums = {};",
		}},
		{"c-cpp-dynamic", map[string]any{"corelib": "c-cpp", "allow_dynamic": true}, []string{
			"std::vector<std::string> strs = {};",
			"std::vector<std::vector<std::uint8_t>> blobs = {};",
			"std::vector<std::uint32_t> nums = {};",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := genHeader(t, src, "m.hpp", tc.cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			// A struct element's member type is a generated row name, so only its
			// initializer is pinned; reset() must agree with the declaration.
			want := append(tc.want, " pts = {};", "        strs = {};")
			for _, w := range want {
				if !strings.Contains(h, w) {
					t.Errorf("[%s] missing %q:\n%s", tc.name, w, h)
				}
			}
			// The construction default is what an ABSENT field reconstructs to, so
			// materializing N elements here would make absence mean the length-N
			// all-default array rather than the empty one.
			for _, bad := range []string{"strs = {{}, {}, {}};", "blobs = {{}, {}};", "pts = {{}, {}};"} {
				if strings.Contains(h, bad) {
					t.Errorf("[%s] a count:N wrapper array must be constructed EMPTY, found %q:\n%s", tc.name, bad, h)
				}
			}
			// §2 is untouched: an empty array is omitted whole -- and since every
			// element the value holds is written, empty is exactly "no child".
			if !strings.Contains(h, "if (!(strs.size() == 0)) { return false; }") {
				t.Errorf("[%s] the omission predicate must be the array's emptiness:\n%s", tc.name, h)
			}
		})
	}

	// A count-less array behaves identically -- which is the point: one rule.
	dyn, err := genHeader(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      strs: { id: 0, type: array, items: { type: string } }\n", "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate dynamic: %v", err)
	}
	if !strings.Contains(dyn, "std::vector<std::string> strs = {};") {
		t.Errorf("a count-less wrapper array must stay empty:\n%s", dyn)
	}
}

// The last element of a wrapper array is always written, whatever its value, and
// a declared `count: N` makes no difference (MESSAGE_SPEC §2, documentation#29).
// Such an array recovers its length as highest-present-id + 1 (§5.1), so the
// element at the highest index is the only one whose PRESENCE carries the
// length: dropping a trailing default leaf encodes ["a", ""] exactly like ["a"]
// and decodes one element short, and ["", ""] as nothing at all. The counted
// array used to be exempt -- it elided its whole trailing run and the decoder
// refilled to N -- which `count` being a capacity makes non-conformant.
//
// Only the heap profile (corelib: cpp) can carry a count-less field at all: the
// embedded profile requires a `count` on every array in BOTH storage modes --
// asserted at the end -- which is exactly why the counted array has to obey the
// same rule for the two profiles to agree on the bytes.
func TestCppWrapperArrayAlwaysWritesLastElement(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }\n" +
		"      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }\n" +
		"      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }\n"
	h, err := genHeader(t, src, "vec.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		// Dynamic: the element loop runs over the whole container, and the last
		// index escapes the omit test.
		"const std::size_t _n0 = dynstr.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { const auto &_e0 = dynstr[_i0]; if (!_e0.empty() || _i0 + 1 == _n0) {",
		"const std::size_t _n0 = dynblob.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { const auto &_e0 = dynblob[_i0]; if (!_e0.empty() || _i0 + 1 == _n0) {",
		// Counted: the SAME loop and the same guard -- no carve-out left.
		"const std::size_t _n0 = fixedstr.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { const auto &_e0 = fixedstr[_i0]; if (!_e0.empty() || _i0 + 1 == _n0) {",
		// The all-default predicate has to follow the writer: a dynamic [""] now
		// puts an element on the wire, so the field is NOT default and must not be
		// omitted. Narrowing it here would drop a field the serialize loop writes.
		"if (!(dynstr.size() == 0)) { return false; }",
		"if (!(dynblob.size() == 0)) { return false; }",
		// The counted one is measured the same way.
		"if (!(fixedstr.size() == 0)) { return false; }",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("vec.hpp missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "trimEmpty") {
		t.Errorf("no leaf array may be narrowed any more:\n%s", h)
	}

	for _, cfg := range []map[string]any{
		{"corelib": "c-cpp"},
		{"corelib": "c-cpp", "allow_dynamic": true},
	} {
		if _, err := genHeader(t, src, "vec.hpp", cfg); err == nil {
			t.Errorf("cfg %v: the embedded profile must reject a count-less array", cfg)
		}
	}
}

// TestCppClibEnumBoolArrayNeverCastsTheContainer is the regression guard for the
// memory corruption the c-cpp enum/boolean array decode used to emit.
//
// It was:
//
//	std::vector<Color> cols;                                   // three pointers
//	is.read(reinterpret_cast<std::array<std::int8_t, 3> &>(cols));
//
// The cast reinterprets the vector's OWN begin/end/capacity words as its first N
// elements, so a received message's bytes overwrite the begin pointer and the
// destructor then frees a pointer partly assembled from the wire. It is reachable
// from any message that fills an enum or boolean array on
// `corelib: c-cpp` + `allow_dynamic: true`.
//
// The two element kinds are fixed differently, because the corelib-c-cpp decoder
// is DEFERRED (it records the destination's address and fills it after the field
// callback returns), so the corelib-cpp shape -- read into a temporary of the
// wire type, convert afterwards -- would bind a dangling pointer:
//
//   - boolean: the member's element type BECOMES the wire's std::uint8_t, so the
//     member is a native destination like any other and readArray binds it
//     directly. std::vector<bool> could never have been a destination anyway: it
//     is the bit-packed specialisation and has no data().
//   - enum: the member keeps its scoped enum element (the generated API and JSON
//     stay value-typed) and readArray binds it through sofabgen::RawArray, which
//     reinterprets the ELEMENTS -- the same narrow cast the scalar enum arm makes
//     -- and forwards resize()/size() so readArray still owns the tag check, the
//     schema-bound check and the reset, in that order.
func TestCppClibEnumBoolArrayNeverCastsTheContainer(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      cols: { id: 0, type: array, items: { type: enum, count: 3, enum: { RED: 0, GREEN: 1 } } }\n" +
		"      flags: { id: 1, type: array, items: { type: boolean, count: 4 } }\n"

	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{
			name: "allow_dynamic",
			cfg:  map[string]any{"corelib": "c-cpp", "allow_dynamic": true},
			want: []string{
				"std::vector<std::uint8_t> flags = {};",
				"is.readArray(flags, _count, 4);",
				"{ sofabgen::RawArray<std::vector<MColsElem>, std::int8_t> _t0{&cols}; is.readArray(_t0, _count, 3); }",
			},
		},
		{
			name: "inline",
			cfg:  map[string]any{"corelib": "c-cpp"},
			want: []string{
				"is.readArray(flags, _count, 4);",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := genHeader(t, src, "m.hpp", tc.cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			// The whole point: no decode destination is ever reached through a cast
			// of its CONTAINER.
			if strings.Contains(h, "reinterpret_cast<std::array<") {
				t.Errorf("an array member must never be decoded through a container cast:\n%s", h)
			}
			for _, want := range tc.want {
				if !strings.Contains(h, want) {
					t.Errorf("missing %q:\n%s", want, h)
				}
			}
			// RawArray reinterprets the elements, never the container, and it never
			// takes ownership of the ordering readArray documents.
			if strings.Contains(h, "RawArray") && !strings.Contains(h, "reinterpret_cast<Wire *>(out->data())") {
				t.Errorf("RawArray must view the member's ELEMENTS:\n%s", h)
			}
		})
	}

	// corelib-cpp is synchronous, so it keeps bool elements and the temporary.
	pure, err := genHeader(t, src, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate cpp: %v", err)
	}
	if !strings.Contains(pure, "std::vector<bool> flags") {
		t.Errorf("the corelib-cpp leg keeps bool elements:\n%s", pure)
	}
	if strings.Contains(pure, "sofabgen::RawArray") {
		t.Errorf("the corelib-cpp leg needs no element view:\n%s", pure)
	}
}

// TestCppNativeCountArrayCarriesALength pins the container decision for a
// `count: N` array of NATIVE scalars across all three profiles.
//
// Since `count` became a CAPACITY and the wire count became the array's LENGTH
// (MESSAGE_SPEC §3, documentation#29), an array member that cannot be shorter
// than N cannot represent what the wire can say. std::array<T, N> is exactly
// such a member, and it was what two of the three profiles used -- so the same
// schema had two wire images and, worse, two DECODE results: `7b 02 01 02`
// (wire count 2) came back as [1, 2] on one leg and [1, 2, 0, 0] on the other.
// That is the byte-identity promise in docs/generator/cpp.md and in
// checkBounded -- "the storage switch never changes the wire" -- broken.
//
// So every native array member is length-carrying now: std::vector<T> on the
// heap profiles, sofab::InlineVector<T, N> (inline slots + len_) on the
// heap-free c-cpp storage mode. std::array is gone from array members entirely.
func TestCppNativeCountArrayCarriesALength(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      nums: { id: 0, type: array, items: { type: u32, count: 4 } }\n" +
		"      part: { id: 1, type: array, items: { type: i32, count: 4 }, default: [10, 20] }\n" +
		"      rows: { id: 2, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }\n"

	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{
			// maxspeed: the decision that changed here. A bounded native array was
			// std::array<T, N> on this profile too.
			name: "cpp-maxspeed",
			cfg:  map[string]any{},
			want: []string{
				"std::vector<std::uint32_t> nums = {};",
				"std::vector<std::int32_t> part = {10, 20};",
				"std::vector<std::vector<std::uint32_t>> rows = {};",
				"is.readArray(nums, 4, -1, sofab::ElemBound::of<std::uint32_t>());",
			},
		},
		{
			// heap-free: inline storage, but with a logical length.
			name: "c-cpp-inline",
			cfg:  map[string]any{"corelib": "c-cpp"},
			want: []string{
				"sofab::InlineVector<std::uint32_t, 4> nums = {};",
				"sofab::InlineVector<std::int32_t, 4> part = {10, 20};",
				"sofab::InlineVector<sofab::InlineVector<std::uint32_t, 3>, 2> rows = {};",
				"is.readArray(nums, _count, 4);",
			},
		},
		{
			// unchanged by this commit -- pinned so the three legs cannot drift.
			name: "c-cpp-dynamic",
			cfg:  map[string]any{"corelib": "c-cpp", "allow_dynamic": true},
			want: []string{
				"std::vector<std::uint32_t> nums = {};",
				"std::vector<std::int32_t> part = {10, 20};",
				"std::vector<std::vector<std::uint32_t>> rows = {};",
				"is.readArray(nums, _count, 4);",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := genHeader(t, src, "m.hpp", tc.cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(h, want) {
					t.Errorf("missing %q:\n%s", want, h)
				}
			}
			// The whole point: no array member is a fixed-extent container any more.
			// A std::array<T,N> member is precisely the thing that cannot express a
			// value shorter than N.
			if strings.Contains(h, "std::array<std::uint32_t, 4>") ||
				strings.Contains(h, "std::array<std::int32_t, 4>") ||
				strings.Contains(h, "std::array<std::uint32_t, 3>") {
				t.Errorf("a count:N native array must not be a fixed-extent std::array:\n%s", h)
			}
			// A declared default shorter than N stays shorter than N -- on every
			// profile. Under std::array the initializer was zero-filled out to N, so
			// [10, 20] and [10, 20, 0, 0] named the same construction value.
			if strings.Contains(h, "part = {10, 20, 0, 0}") {
				t.Errorf("a short default must not be padded to the count:\n%s", h)
			}
		})
	}

	// The encode temp an enum array converts through must not touch a heap on the
	// heap-free profile, and must be the VALUE's length -- never the schema count,
	// which would put N elements on the wire for a shorter value.
	enumSrc := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      cols: { id: 0, type: array, items: { type: enum, count: 3, enum: { A: 0, B: 1 } } }\n"
	fixedEnum, err := genHeader(t, enumSrc, "m.hpp", map[string]any{"corelib": "c-cpp"})
	if err != nil {
		t.Fatalf("generate enum fixed: %v", err)
	}
	if !strings.Contains(fixedEnum, "{ sofab::InlineVector<std::int8_t, 3> _t0; _t0.resize(cols.size());") {
		t.Errorf("the heap-free enum encode temp must be inline and value-length:\n%s", fixedEnum)
	}
	heapEnum, err := genHeader(t, enumSrc, "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate enum heap: %v", err)
	}
	if !strings.Contains(heapEnum, "{ std::vector<std::int8_t> _t0; _t0.resize(cols.size());") {
		t.Errorf("the heap enum encode temp must be value-length:\n%s", heapEnum)
	}
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound.
//
// corelib-cpp's typed read() ends in `value = static_cast<T>(raw)` — the mask
// §7.1 forbids, applied where generated code cannot see the raw value. So a
// narrow destination reads through a 64-bit temporary and range-checks before the
// store. §7.3 is unaffected: read() picks its expected wire type from signedness
// alone, so u8 and u64 frame identically and a contradicting tag still returns
// false and stores nothing.
func TestCppDeclaredWidthIsAValidityBound(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      a_u8:   { id: 0, type: u8 }
      c_u32:  { id: 2, type: u32 }
      d_u64:  { id: 3, type: u64 }
      e_i8:   { id: 4, type: i8 }
      g_i32:  { id: 6, type: i32 }
      h_i64:  { id: 7, type: i64 }
`
	got := headerFromYAML(t, src, "w.hpp")
	for _, want := range []string{
		"{ std::uint64_t _v; if (is.read(_v)) { if (_v > 255) { is.invalidate(); return; } a_u8 = static_cast<std::uint8_t>(_v); } }",
		"{ std::uint64_t _v; if (is.read(_v)) { if (_v > 4294967295) { is.invalidate(); return; } c_u32 = static_cast<std::uint32_t>(_v); } }",
		"{ std::int64_t _v; if (is.read(_v)) { if (_v < -128 || _v > 127) { is.invalidate(); return; } e_i8 = static_cast<std::int8_t>(_v); } }",
		"{ std::int64_t _v; if (is.read(_v)) { if (_v < -2147483648 || _v > 2147483647) { is.invalidate(); return; } g_i32 = static_cast<std::int32_t>(_v); } }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("w.hpp missing width guard %q:\n%s", want, got)
		}
	}
	// 64-bit destinations keep the direct typed read: nothing to bound.
	for _, want := range []string{"is.read(d_u64);", "is.read(h_i64);"} {
		if !strings.Contains(got, want) {
			t.Errorf("w.hpp: a 64-bit destination must keep its direct read (%q):\n%s", want, got)
		}
	}
}

// The c-cpp (footprint) profile was ALREADY conformant — its deferred descriptor
// carries the declared width to the corelib, which rejects there — so it must not
// grow a second, generator-side guard.
func TestCppCCppKeepsItsOwnWidthReject(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      a_u8:  { id: 0, type: u8 }
      c_u32: { id: 2, type: u32 }
`
	got, err := fixedHeader(t, src, "w.hpp", map[string]any{"corelib": "c-cpp", "allow_dynamic": true})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, gone := range []string{"std::uint64_t _v;", "is.invalidate(); return; } a_u8"} {
		if strings.Contains(got, gone) {
			t.Errorf("c-cpp must not gain a generator-side width guard (%q):\n%s", gone, got)
		}
	}
}

// generator#279 (Crucible F-0052): MESSAGE_SPEC §1/§7.1 makes the declared
// element width a validity bound, and corelib-cpp enforces it inside readArray —
// but only once the fourth argument is ARMED. Left at its default the unbounded
// decode ran and an over-width element was masked to the width and kept: 5208
// into a `u8` array came back as 88.
//
// The scalar position was already correct (generated code checks that one inline,
// generator#266). This is the array half, which lives in the corelib because
// readArray converts the elements itself.
func TestCppReadArrayArmsTheElementWidthBound(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      u8s:  { id: 0, type: array, items: { type: u8,   count: 5 } }
      i8s:  { id: 1, type: array, items: { type: i8,   count: 5 } }
      u64s: { id: 2, type: array, items: { type: u64,  count: 5 } }
      f32s: { id: 3, type: array, items: { type: fp32, count: 5 } }
`
	got := headerFromYAML(t, src, "w.hpp")
	for _, want := range []string{
		// A narrow element carries its bound...
		"is.readArray(u8s, 5, -1, sofab::ElemBound::of<std::uint8_t>());",
		"is.readArray(i8s, 5, -1, sofab::ElemBound::of<std::int8_t>());",
		// ...and so does a 64-bit one: ElemBound::of comes back UNARMED there, so
		// the corelib's own helper decides, not an emission-time special case.
		"is.readArray(u64s, 5, -1, sofab::ElemBound::of<std::uint64_t>());",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("w.hpp missing armed element bound %q:\n%s", want, got)
		}
	}
	// A FLOAT element must NOT get one. This is not an optimization:
	// ElemBound::of<float>() would cast numeric_limits<float>::max() to int64_t in
	// a constexpr function — out of range, so a hard compile error. The corelib
	// ignores the argument for a non-integral element anyway.
	if strings.Contains(got, "ElemBound::of<float>") || strings.Contains(got, "ElemBound::of<double>") {
		t.Errorf("a floating-point element must not be given an ElemBound:\n%s", got)
	}
	if !strings.Contains(got, "is.readArray(f32s, 5);") {
		t.Errorf("an fp32 array must keep the unbounded read:\n%s", got)
	}
}

// c-cpp was already conformant — it rejects an over-width element through its own
// descriptor path — and its readArray has a different signature, so it must not
// gain the argument.
func TestCppCCppReadArrayKeepsItsOwnShape(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      u8s: { id: 0, type: array, items: { type: u8, count: 5 } }
`
	got, err := fixedHeader(t, src, "w.hpp", map[string]any{"corelib": "c-cpp", "allow_dynamic": true})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(got, "ElemBound") {
		t.Errorf("c-cpp must not gain the corelib-cpp element bound:\n%s", got)
	}
}
