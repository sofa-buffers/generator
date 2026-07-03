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
		"enum class MyfirstmessageSomeenum : std::int8_t {", // smallest signed backing
		"std::uint64_t someu64 = 18446744073709551615ULL;",
		"is.read(",               // nested decode via is.read
		"float somefp32 = 0.0f;", // valid float literal
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
// profile (containers: fixed, corelib: c-cpp) with the given extra config.
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
	cfg := map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp", "containers": "fixed"}
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

// TestCppFixedContainers: the opt-in fixed-capacity profile lowers blobs and
// struct/matrix/blob sequences to heap-free, schema-sized storage; strings and
// unbounded (allow_dynamic) fields stay dynamic. Wire bytes are unchanged (proven
// separately by the conformance run) — this asserts the emitted member types.
func TestCppFixedContainers(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bl: { id: 0, type: blob, maxlen: 16 }\n" +
		"      s: { id: 1, type: string, maxlen: 8 }\n" +
		"      nums: { id: 2, type: array, items: { type: u32, count: 4 } }\n" +
		"      blobs: { id: 3, type: array, items: { type: blob, count: 3, maxlen: 8 } }\n" +
		"      pts: { id: 4, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n"
	h, err := fixedHeader(t, src, "m.hpp", nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"FixedBytes<16> bl = {};",                              // scalar blob -> fixed
		"std::string s = \"\";",                                // string stays dynamic (deferred)
		"std::array<std::uint32_t, 4> nums = {};",              // native array unchanged
		"InlineVector<FixedBytes<8>, 3> blobs = {};",           // blob sequence -> inline
		"InlineVector<MPtsElem",                                // struct sequence -> inline (prefix)
		"struct FixedBytes {",                                  // prelude emitted
		"struct InlineVector {",                                // prelude emitted
		"if (bl != FixedBytes<16>{}) {",                        // blob default-compare typed
		"static _FixedBlobSeq<InlineVector<FixedBytes<8>, 3>>", // blob-seq collector
		"static _MsgSeqFixed<InlineVector<",                    // struct-seq collector
		"std::size_t encodeTo(std::uint8_t *dst",               // heap-free encode
	} {
		if !strings.Contains(h, want) {
			t.Errorf("fixed header missing %q", want)
		}
	}
	// No std::vector member for the blob or blob-sequence fields.
	if strings.Contains(h, "std::vector<std::uint8_t> bl") || strings.Contains(h, "std::vector<std::vector<std::uint8_t>> blobs") {
		t.Error("fixed profile must not emit std::vector for bounded blob/blob-array members")
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

// TestCppFixedRequiresClib: the fixed profile is only meaningful on corelib-c-cpp.
func TestCppFixedRequiresClib(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      a: { id: 0, type: u32 }\n"
	doc, _ := parser.Parse([]byte(src), "in.yaml")
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, _ := model.Build(doc)
	_ = analysis.Analyze(s)
	_, err := (&Backend{}).Generate(s, map[string]any{"containers": "fixed"}) // corelib defaults to cpp
	if err == nil || !strings.Contains(err.Error(), "requires corelib: c-cpp") {
		t.Errorf("expected corelib gate error, got %v", err)
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

// TestCppContainersDefault: corelib c-cpp (the embedded target) defaults to the
// fixed-capacity profile; the pure-cpp path stays dynamic; and either default can
// be overridden explicitly.
func TestCppContainersDefault(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      bl: { id: 0, type: blob, maxlen: 16 }\n"
	// c-cpp, no containers key -> fixed by default.
	h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp"})
	if err != nil {
		t.Fatalf("c-cpp default generate: %v", err)
	}
	if !strings.Contains(h, "FixedBytes<16> bl") {
		t.Error("c-cpp should default to containers: fixed (expected FixedBytes member)")
	}
	// c-cpp with explicit dynamic opt-out -> std::vector.
	h, err = genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp", "containers": "dynamic"})
	if err != nil {
		t.Fatalf("c-cpp dynamic generate: %v", err)
	}
	if !strings.Contains(h, "std::vector<std::uint8_t> bl") {
		t.Error("containers: dynamic should opt back out to std::vector")
	}
	// pure cpp -> dynamic by default (no corelib key).
	h, err = genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("cpp default generate: %v", err)
	}
	if !strings.Contains(h, "std::vector<std::uint8_t> bl") {
		t.Error("pure cpp should default to containers: dynamic")
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
