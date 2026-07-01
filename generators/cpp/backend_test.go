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
