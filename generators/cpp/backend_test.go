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
		"is.read(",                        // nested decode via is.read
		"float somefp32 = 0.0f;",          // valid float literal
		"is.readArray(someuintarray, 4);", // the over-count reject (generator#100) rides into readArray
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
		"std::vector<std::uint32_t> arr = {};",                          // unbounded native -> vector (was std::array<T,0>)
		"std::vector<bool> bl = {};",                                    // unbounded bool -> vector
		"std::array<std::uint32_t, 4> fixed = {};",                      // bounded native array unchanged
		"std::vector<std::vector<std::uint32_t>> matrix",                // matrix rows are dynamic vectors too
		"is.readArray(arr);",                                            // readArray sizes the vector to the wire count
		"if (arr != std::vector<std::uint32_t>{}) {",                    // whole-omit compares to an empty vector
		"std::size_t _count) noexcept override",                         // _count is named for the resize
		"sofabgen::WrapperSeq<std::vector<std::vector<std::uint32_t>>>", // matrix rows collected by the generated placer
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
		"{ sofab::StringSeq _r0{bs, 4, 16}; if (is.read(_r0)) { sofabgen::fillTo(bs, 4); } }", // bounded string -> cap 4, elem maxlen 16
		"{ sofab::BlobSeq _r0{bb, 3, 16}; if (is.read(_r0)) { sofabgen::fillTo(bb, 3); } }",   // bounded blob -> cap 3, elem maxlen 16
		"_r0.cap = 2;", // bounded struct -> placer cap 2
		"{ sofab::StringSeq _r0{ds, -1, -1}; is.read(_r0); }", // dynamic string -> unbounded cap + maxlen, and never refilled
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
		"{ sofab::StringSeq _r0{sa, 3, 5}; if (is.read(_r0)) { sofabgen::fillTo(sa, 3); } }", // wrapper string: cap 3, elem maxlen 5 handed to the corelib collector
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
		"std::array<std::uint32_t, 4> nums = {};",                                      // native array unchanged
		"sofab::InlineVector<sofab::FixedBytes<8>, 3> blobs = {{}, {}, {}};",           // blob sequence -> inline, at its count
		"sofab::InlineVector<sofab::FixedString<16>, 5> strs = {{}, {}, {}, {}, {}};",  // string sequence -> inline, at its count
		"sofab::InlineVector<MPtsElem",                                                 // struct sequence -> inline (prefix)
		"if (bl != sofab::FixedBytes<16>{}) {",                                         // blob default-compare typed
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
		"std::array<std::uint32_t, 4> a", "sofab::InlineVector<sofab::FixedString<5>, 2> t",
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
		"if (a != 7) { (void)os.write(0, a); }",               // scalar guard
		`if (s != "") {`,                                      // string guard (empty default)
		"if (bl != std::vector<std::uint8_t>{}) {",            // blob guard
		"std::array<std::int32_t, 3> nums = {1, 2, 3};",       // native array default materialized
		"if (nums != std::array<std::int32_t, 3>{1, 2, 3}) {", // native array whole-omit
		"(void)os.writeLazy(5, st);",                          // struct framed lazily (no guard)
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
		"is.readArray(arr, -1, SOFAB_MAX_DYN_ARRAY_COUNT);", // the cap rides into readArray
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

// TestCppFixedCountTrimsTrailingDefaultRun: a `count: N` native array is
// FIXED-LENGTH, so its canonical wire carries only elements [0, M') — M' being
// one past the last non-default element — and the decoder rebuilds [M', N) from
// the schema count (MESSAGE_SPEC §3, finding F-0010). Handing the whole
// std::array<T,N> to the corelib emits the trailing default run, because the
// span-based write takes .size() == N. Both corelibs take a std::span through
// the same templated OStream::write, so both profiles trim identically.
func TestCppFixedCountTrimsTrailingDefaultRun(t *testing.T) {
	for _, corelib := range []string{"cpp", "c-cpp"} {
		t.Run(corelib, func(t *testing.T) {
			h, err := genHeader(t, trimSrc, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": corelib})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			// The trim helper lives in corelib-cpp on the pure path (sofab::trimTail)
			// and is still emitted for the c-cpp wrapper, which links a different
			// corelib. Both compare the element's BYTE IMAGE, never == -- a trailing
			// -0.0 equals 0.0 but is not the default and must stay on the wire.
			trim := "sofab::trimTail"
			wants := []string{
				// Numeric + float fields trim in place.
				"(void)os.write(0, " + trim + "(u32s));",
				"(void)os.write(1, " + trim + "(f32s));",
				// Enum/bool value-convert through a native temp; the converted image is
				// trimmed (enum default 0 -> backing 0, false -> 0).
				"(void)os.write(2, " + trim + "(_t0)); }",
				"(void)os.write(3, " + trim + "(_t0)); }",
			}
			for _, want := range wants {
				if !strings.Contains(h, want) {
					t.Errorf("[%s] header missing %q:\n%s", corelib, want, h)
				}
			}
			// A nested matrix ROW is a wrapper-sequence element, not a `count: N`
			// field: the rule is scoped to fields, so a row's OWN elements are
			// never trimmed. The array OF rows still is, since it declares a count
			// of its own -- an all-zero fixed row is not "empty", so trimEmpty
			// leaves it in place, exactly as the Go reference does.
			if !strings.Contains(h, "(void)os.write(static_cast<sofab::id>(_i0), _e0);") {
				t.Errorf("[%s] nested array row must not be trimmed:\n%s", corelib, h)
			}
			// Decode is unchanged: the fixed std::array already materializes N
			// elements, zero-filled by the in-class initializer, so [M, N) is already
			// the element default. Over-count stays INVALID on the heap profile.
			if !strings.Contains(h, "std::array<std::uint32_t, 5> u32s = {};") {
				t.Errorf("[%s] fixed-count array must stay a zero-filled std::array:\n%s", corelib, h)
			}
			// Both corelibs read through readArray, which carries the bound and
			// performs the reset behind the tag match; the c-cpp signature also
			// takes the wire count, since it sizes a dynamic destination.
			wantRead := "is.readArray(u32s, 5);"
			if corelib == "c-cpp" {
				wantRead = "is.readArray(u32s, _count, 5);"
			}
			if !strings.Contains(h, wantRead) {
				t.Errorf("[%s] fixed-count decode must read the whole array (%s):\n%s", corelib, wantRead, h)
			}
		})
	}
}

// TestCppDynamicArrayNotTrimmed: a count-less (dynamic) array has no schema N to
// refill from at decode, so a trailing default element is SIGNIFICANT and must
// reach the wire. Only the heap profile has dynamic arrays (the fixed profile
// rejects an unbounded array in checkBounded).
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
		"(void)os.write(4, sofab::trimTail(fixed));", // the counted one still trims
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	for _, bad := range []string{"_trimTail(dyn)", "_trimTail(dynf)"} {
		if strings.Contains(h, bad) {
			t.Errorf("dynamic array must not be trimmed, found %q:\n%s", bad, h)
		}
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
			if !strings.Contains(h, "std::array<std::uint32_t, 5> c = {1, 2, 3};") {
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
				wantRead := "is.readArray(c, 5);"
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
	if !strings.Contains(h, "is.readArray(dyn);") {
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
		"is.readArray(i, 2);",
		"is.readArray(j, 2);",
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

// TestCppDynamicNativeArrayIsSizedToCount pins that a `count: N` array holds N
// elements after construction in BOTH storage modes.
//
// std::array<T,N> gets that for free from aggregate initialization; std::vector
// (the allow_dynamic storage) does not — `= {}` constructs it empty. A bounded
// array that starts empty is not merely a different representation of the same
// value: generated code indexes elements 0..N-1, so writes to every element are
// silently discarded. Found by the MAX_SIZE fill check, which encoded 137 bytes
// where the schema says 234 — exactly the four native arrays missing.
func TestCppDynamicNativeArrayIsSizedToCount(t *testing.T) {
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
		"std::vector<std::uint32_t> zeros = {0, 0, 0, 0};",
		"std::vector<std::uint32_t> partial = {10, 20, 0, 0};",
		"std::vector<float> floats = {0.0f, 0.0f, 0.0f};",
		"std::vector<bool> flags = {false, false};",
	} {
		if !strings.Contains(dyn, want) {
			t.Errorf("a bounded array in dynamic storage must construct with all %d elements, missing %q:\n%s",
				4, want, dyn)
		}
	}

	// The fixed profile keeps the idiomatic aggregate form — std::array fills the
	// tail itself, so spelling out the zeros would be noise.
	fixed, err := genHeader(t, src, "m.hpp", map[string]any{"corelib": "c-cpp"})
	if err != nil {
		t.Fatalf("generate fixed: %v", err)
	}
	for _, want := range []string{
		"std::array<std::uint32_t, 4> zeros = {};",
		"std::array<std::uint32_t, 4> partial = {10, 20};",
	} {
		if !strings.Contains(fixed, want) {
			t.Errorf("fixed storage should keep the aggregate form, missing %q:\n%s", want, fixed)
		}
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
		// reset and the wire-absent value cannot drift apart -- a `count: N`
		// wrapper array included, which is N elements long at rest just like the
		// native array below it.
		"        items = {{}, {}, {}, {}};",
		"        names = {{}, {}, {}, {}};",
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
		"        names = {{}, {}, {}, {}};",
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
		"{ sofab::StringSeq _r1{_e0, 3, 8}; if (is.read(_r1)) { sofabgen::fillTo(_e0, 3); } }",
		"_S0 _r0; _r0.out = &strrows; is.read(_r0);",
		// blob rows
		"{ sofab::BlobSeq _r1{_e0, 3, 8}; if (is.read(_r1)) { sofabgen::fillTo(_e0, 3); } }",
		// struct rows: MessageSeq over the ELEMENT type, not the row container
		"{ sofabgen::WrapperSeq<std::vector<MStructrowsElemElem>> _r1; _r1.out = &_e0; _r1.cap = 3; if (is.read(_r1)) { sofabgen::fillTo(_e0, 3); } }",
		// depth 3: the row collector nests, one level further
		"struct _S1 : sofab::IStreamMessage {",
		"{ sofab::StringSeq _r2{_e1, 3, 8}; if (is.read(_r2)) { sofabgen::fillTo(_e1, 3); } }",
		// §5.1 placement + over-index reject, §7.4 replace-whole
		"if (cap >= 0 && static_cast<std::size_t>(_id) >= static_cast<std::size_t>(cap)) { is.invalidate(); return; }",
		"while (out->size() <= static_cast<std::size_t>(_id)) out->emplace_back();",
		"void prepare() noexcept { if (out) out->clear(); }",
		// native rows keep the corelib collector
		"sofabgen::WrapperSeq<std::vector<std::array<std::uint32_t, 3>>> _r0; _r0.out = &urows;",
		"sofabgen::fillTo(urows, 2);",
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
		"is.readSequence(_r1, _e0); if (_p1) { sofabgen::fillTo(_e0, 3); }",
		"static _S0 _r0; is.readSequence(_r0, strrows);",
		"static sofab::FixedBlobSeq<sofab::InlineVector<sofab::FixedBytes<8>, 3>> _r1;",
		"static sofabgen::WrapperSeq<sofab::InlineVector<MStructrowsElemElem, 3>> _r1; _r1.cap = 3;",
		"struct _S1 : sofab::IStreamMessage {",
		"static sofab::FixedStringSeq<sofab::InlineVector<sofab::FixedString<8>, 3>> _r2;",
		"is.readSequence(_r2, _e1); if (_p2) { sofabgen::fillTo(_e1, 3); }",
		// native rows keep the corelib collector
		"static sofabgen::WrapperSeq<sofab::InlineVector<std::array<std::uint32_t, 3>, 2>> _r0; _r0.cap = 2;",
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

// TestCppWrapperElementsArePlacedByIDAndFilledToN: a wrapper array's element id
// IS the array index (MESSAGE_SPEC §5.1), so an element is PLACED at dest[id]
// after gap-filling -- never appended. Appending shortened the array by the size
// of any interior id gap and decoded a REOPENED id as a second element instead
// of merging into the first (§7.4). The leaf string/blob collectors in both
// corelibs always got this right; corelib-c-cpp's Fixed/MessageSeq did not, so
// the object path now collects through a generated placer on BOTH profiles
// (generator#247).
//
// The refill to N on top is what makes the §3/§5.1 trailing elision lossless:
// without it, re-encoding a decoded fixed array shortens it on every round trip.
func TestCppWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
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
				// the refill, and the schema count that feeds it
				"void fillTo(Container &out, std::size_t n) noexcept {",
				"sofabgen::fillTo(fixed, 5)",
				"sofabgen::fillTo(fstrs, 3)",
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
		})
	}
	// A dynamic array has no N to refill from: its length is highest-present-id
	// + 1, so filling it would invent elements the wire never carried.
	h, err := genHeader(t, wrapperSeqSrc, "vec.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(h, "_r0.cap = -1; is.read(_r0); }") {
		t.Errorf("a dynamic wrapper array must be read without a refill:\n%s", h)
	}
	if strings.Contains(h, "sofabgen::fillTo(dynamic") {
		t.Errorf("a dynamic wrapper array must never be refilled:\n%s", h)
	}
}

// TestCppFixedWrapperArrayTrimsTrailingDefaultRun: a `count: N` wrapper array's
// canonical wire stops at M -- one past its last non-default element
// (MESSAGE_SPEC §3/§5.1, "even for sequence-form elements") -- and M == 0 leaves
// the whole wrapper omitted (§2). generator#248: the element loop used to run to
// the container's size, framing every trailing all-default element, so a decoder
// that accepted the non-canonical form re-encoded it unchanged instead of
// normalising. A DYNAMIC array has no N to refill from, so its trailing default
// element is significant and must still be framed.
func TestCppFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	h, err := genHeader(t, wrapperSeqSrc, "vec.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The fixed array narrows to M before framing anything...
	if !strings.Contains(h, "const std::size_t _n0 = sofabgen::trimObjs(fixed); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { (void)os.write(static_cast<sofab::id>(_i0), fixed[_i0]); }") {
		t.Errorf("count:N struct array must loop to M, not to size():\n%s", h)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(h, "const std::size_t _n0 = dynamic.size(); for (std::size_t _i0 = 0; _i0 < _n0; ++_i0) { (void)os.write(static_cast<sofab::id>(_i0), dynamic[_i0]); }") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", h)
	}
	// An INTERIOR all-default element is still framed: the write keeps its own
	// frame, and the field's lazy closer is what drops an M == 0 wrapper.
	if !strings.Contains(h, "(void)os.sequenceBeginLazy(0);") || !strings.Contains(h, "(void)os.sequenceEnd();") {
		t.Errorf("the wrapper must stay lazily opened and dropped by its closer:\n%s", h)
	}
	// _isDefault is the exact negation of what serialize writes, so it must
	// narrow a field exactly when the serialize loop does -- disagreeing would
	// either omit a field that is on the wire or keep one that is not.
	for _, want := range []string{
		"if (!(sofabgen::trimObjs(fixed) == 0)) { return false; }",
		"if (!(dynamic.size() == 0)) { return false; }",
		"if (!(sofabgen::trimEmpty(fstrs) == 0)) { return false; }",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("_isDefault must be computed from the same expression as serialize, missing %q:\n%s", want, h)
		}
	}
	// The trim itself: one past the last element that is not the element default.
	if !strings.Contains(h, "while (m > 0 && a[m - 1]._isDefault()) { --m; }") {
		t.Errorf("trimObjs must stop one past the last non-default element:\n%s", h)
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

// TestCppFixedCountWrapperArrayIsMaterializedToN pins the length a `count: N`
// wrapper array has at rest.
//
// MESSAGE_SPEC §5.1 makes that length N "for every target", and it is not a wire
// property: it holds whether or not the field ever reaches the wire. The native
// `count: N` array next to it has always had it — its member declaration carries
// the padded default literal — but the wrapper one was constructed empty, so one
// schema had two different answers to the same question:
//
//	absent field             -> length 0   (wrong)
//	one element on the wire   -> length N  (sofabgen::fillTo, after the sequence)
//	explicitly-empty wrapper  -> length N  (same)
//
// The refill can only run once the sequence is actually opened, so closing the
// gap has to happen at construction. All three storage kinds are checked: the
// heap profile's std::vector, the c-cpp allow_dynamic std::vector, and the c-cpp
// inline sofab::InlineVector — whose LOGICAL length starts at 0 even though its
// inline buffer already has N slots, so it needs the same treatment.
//
// A DYNAMIC (count-less) array has no N and must stay empty; the §2 omission of
// an all-default array is unaffected, since the encoder measures elements
// (trimEmpty), not the container's size.
func TestCppFixedCountWrapperArrayIsMaterializedToN(t *testing.T) {
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
			"std::vector<std::string> strs = {{}, {}, {}};",
			"std::vector<std::vector<std::uint8_t>> blobs = {{}, {}};",
			"std::array<std::uint32_t, 3> nums = {};",
		}},
		{"c-cpp-inline", map[string]any{"corelib": "c-cpp"}, []string{
			"sofab::InlineVector<sofab::FixedString<8>, 3> strs = {{}, {}, {}};",
			"sofab::InlineVector<sofab::FixedBytes<4>, 2> blobs = {{}, {}};",
			"std::array<std::uint32_t, 3> nums = {};",
		}},
		// allow_dynamic puts the native array in a std::vector too — and that one
		// has always been written out to all N elements, which is precisely the
		// precedent the wrapper arrays above now follow.
		{"c-cpp-dynamic", map[string]any{"corelib": "c-cpp", "allow_dynamic": true}, []string{
			"std::vector<std::string> strs = {{}, {}, {}};",
			"std::vector<std::vector<std::uint8_t>> blobs = {{}, {}};",
			"std::vector<std::uint32_t> nums = {0, 0, 0};",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := genHeader(t, src, "m.hpp", tc.cfg)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			// A struct element's member type is a generated row name, so only its
			// initializer is pinned; reset() must agree with the declaration.
			want := append(tc.want, " pts = {{}, {}};", "        strs = {{}, {}, {}};")
			for _, w := range want {
				if !strings.Contains(h, w) {
					t.Errorf("[%s] missing %q:\n%s", tc.name, w, h)
				}
			}
			// The construction default is what an ABSENT field reconstructs to, so
			// an empty container here IS the length-0 answer.
			for _, bad := range []string{"strs = {};", "blobs = {};", "pts = {};"} {
				if strings.Contains(h, bad) {
					t.Errorf("[%s] a count:N wrapper array must not be constructed empty, found %q:\n%s", tc.name, bad, h)
				}
			}
			// §2 is untouched: an all-default array is still omitted whole, because
			// the encoder measures ELEMENTS rather than the container's size.
			if !strings.Contains(h, "if (!(sofabgen::trimEmpty(strs) == 0)) { return false; }") {
				t.Errorf("[%s] the omission predicate must still count elements:\n%s", tc.name, h)
			}
		})
	}

	// A count-less array has no N to materialize and stays empty. Only the heap
	// profile accepts one — the c-cpp profile requires every array to be sized.
	dyn, err := genHeader(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      strs: { id: 0, type: array, items: { type: string } }\n", "m.hpp", map[string]any{})
	if err != nil {
		t.Fatalf("generate dynamic: %v", err)
	}
	if !strings.Contains(dyn, "std::vector<std::string> strs = {};") {
		t.Errorf("a count-less wrapper array has no N and must stay empty:\n%s", dyn)
	}
}
