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
		"struct Myfirstmessage : sofab::OStreamMessage, sofab::IStreamMessage {",
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
		"if (_count > 4) { is.invalidate(); return; }", // over-count scalar array rejected as INVALID (generator#100)
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
		"sofab::FixedBytes<16> bl = {};",                                      // scalar blob -> fixed
		"sofab::FixedString<8> s = \"\";",                                     // bounded string -> FixedString
		"std::array<std::uint32_t, 4> nums = {};",                             // native array unchanged
		"sofab::InlineVector<sofab::FixedBytes<8>, 3> blobs = {};",            // blob sequence -> inline
		"sofab::InlineVector<sofab::FixedString<16>, 5> strs = {};",           // string sequence -> inline
		"sofab::InlineVector<MPtsElem",                                        // struct sequence -> inline (prefix)
		"if (bl != sofab::FixedBytes<16>{}) {",                                // blob default-compare typed
		"s.set_len(_size); if (_size) is.read(s);",                            // FixedString decode
		"bl.set_len(_size); is.read(bl.data(), bl.size());",                   // FixedBytes decode: clamped size, not raw _size (issue #95)
		"static _FixedBlobSeq<sofab::InlineVector<sofab::FixedBytes<8>, 3>>",  // blob-seq collector
		"static _FixedStrSeq<sofab::InlineVector<sofab::FixedString<16>, 5>>", // string-seq collector
		"static _MsgSeqFixed<sofab::InlineVector<",                            // struct-seq collector
		"std::size_t encodeTo(std::uint8_t *dst",                              // heap-free encode
	} {
		if !strings.Contains(h, want) {
			t.Errorf("fixed header missing %q", want)
		}
	}
	// The clib wrapper emits no over-count guard: the C runtime itself rejects a
	// count/capacity mismatch with SOFAB_RET_E_INVALID_MSG (generator#100).
	if strings.Contains(h, "is.invalidate()") {
		t.Error("corelib: c-cpp must not emit is.invalidate() (C runtime already rejects over-count)")
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
// under the fixed profile, unless allow_dynamic keeps a std::vector fallback.
func TestCppFixedUnbounded(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      m: { id: 0, type: array, items: { type: struct, fields: { k: { id: 0, type: i32 } } } }\n"
	if _, err := fixedHeader(t, src, "m.hpp", nil); err == nil {
		t.Fatal("expected unbounded-field error under fixed profile")
	} else if !strings.Contains(err.Error(), "has no count") {
		t.Errorf("unexpected error: %v", err)
	}
	// allow_dynamic keeps a std::vector fallback and generates cleanly.
	h, err := fixedHeader(t, src, "m.hpp", map[string]any{"allow_dynamic": true})
	if err != nil {
		t.Fatalf("allow_dynamic should generate: %v", err)
	}
	if !strings.Contains(h, "std::vector<MMElem") && !strings.Contains(h, "std::vector<") {
		t.Error("allow_dynamic should keep a std::vector fallback for the unbounded field")
	}
}

// TestCppFixedUnboundedString: a string without maxlen is now an unbounded field
// (no more string exemption) — a hard error, unless allow_dynamic keeps a
// std::string fallback. A bounded string still becomes FixedString even under
// allow_dynamic.
func TestCppFixedUnboundedString(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string }\n"
	if _, err := fixedHeader(t, src, "m.hpp", nil); err == nil {
		t.Fatal("expected unbounded-string error under fixed profile")
	} else if !strings.Contains(err.Error(), "has no maxlen") {
		t.Errorf("unexpected error: %v", err)
	}
	// allow_dynamic keeps the unbounded string as std::string, but a bounded one
	// still becomes FixedString.
	src2 := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string }\n" +
		"      t: { id: 1, type: string, maxlen: 12 }\n"
	h, err := fixedHeader(t, src2, "m.hpp", map[string]any{"allow_dynamic": true})
	if err != nil {
		t.Fatalf("allow_dynamic should generate: %v", err)
	}
	if !strings.Contains(h, "std::string s = \"\";") {
		t.Error("unbounded string under allow_dynamic should stay std::string")
	}
	if !strings.Contains(h, "sofab::FixedString<12> t = \"\";") {
		t.Error("bounded string should still be FixedString even under allow_dynamic")
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
// materialized default; a struct/union and a composite array stay ALWAYS framed.
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
		"(void)os.write(5, st);",                              // struct ALWAYS framed (no guard)
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q", want)
		}
	}
	// A composite array is always framed: emitted via sequenceBegin, never guarded
	// by an "if (strs != ...)" whole-omission.
	if strings.Contains(h, "if (strs !=") {
		t.Error("composite array must be always framed (no whole-omission guard)")
	}
	if !strings.Contains(h, "(void)os.sequenceBegin(4);") {
		t.Error("composite array must be framed via sequenceBegin")
	}
}
