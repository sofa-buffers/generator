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
		"std::vector<std::uint32_t> arr = {};",           // unbounded native -> vector (was std::array<T,0>)
		"std::vector<bool> bl = {};",                     // unbounded bool -> vector
		"std::array<std::uint32_t, 4> fixed = {};",       // bounded native array unchanged
		"std::vector<std::vector<std::uint32_t>> matrix", // matrix rows are dynamic vectors too
		"arr.resize(_count); is.read(arr);",              // decode sizes the vector to the wire count
		"if (arr != std::vector<std::uint32_t>{}) {",     // whole-omit compares to an empty vector
		"std::size_t _count) noexcept override",          // _count is named for the resize
		"if constexpr (requires { row.resize(_count); }", // _MsgSeq sizes dynamic matrix rows
	} {
		if !strings.Contains(h, want) {
			t.Errorf("heap header missing %q:\n%s", want, h)
		}
	}
	// The zero-length fixed array must never appear — that is the bug.
	if strings.Contains(h, "std::array<std::uint32_t, 0>") {
		t.Errorf("unbounded array must not lower to std::array<T, 0>:\n%s", h)
	}
	// enum vector: member is a vector of the scoped enum element type, and decode
	// sizes it to _count before the value-narrowing read.
	if !strings.Contains(h, "std::vector<MEnElem> en = {};") {
		t.Errorf("unbounded enum array should be a std::vector of the enum element:\n%s", h)
	}
	if !strings.Contains(h, "en.resize(_count);") {
		t.Errorf("unbounded enum array decode should resize to _count:\n%s", h)
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
		"if (static_cast<std::size_t>(id) >= out->capacity()) return;",        // over-capacity element dropped, no infinite loop (issue #126)
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
		// derived cap: max(cfg string 4096, cfg blob unset, schema maxlen 8000, count*10 30)
		"#define SOFAB_MAX_DYN_BUFFERED_FIELD 8000",
		"if (_size > SOFAB_MAX_DYN_STRING_LEN) { is.exceedLimit(); return; }",
		"if (_count > SOFAB_MAX_DYN_ARRAY_COUNT) { is.exceedLimit(); return; }",
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
			for _, want := range []string{
				// The helper is emitted for both corelibs, inside the shared prelude
				// guard, and returns a non-owning span (heap-free).
				"std::span<const typename C::value_type> _trimTail(const C &_a) noexcept {",
				// Bit-pattern compare, never ==: a trailing -0.0 (== 0.0) must survive.
				"std::memcmp(&_a[_n - 1], &_z, sizeof(_T)) == 0",
				// Numeric + float fields trim in place.
				"(void)os.write(0, _trimTail(u32s));",
				"(void)os.write(1, _trimTail(f32s));",
				// Enum/bool value-convert through a native temp; the converted image is
				// trimmed (enum default 0 -> backing 0, false -> 0).
				"(void)os.write(2, _trimTail(_t0)); }",
				"(void)os.write(3, _trimTail(_t0)); }",
			} {
				if !strings.Contains(h, want) {
					t.Errorf("[%s] header missing %q:\n%s", corelib, want, h)
				}
			}
			// A nested matrix row is a wrapper-sequence element, not a `count: N`
			// field: the rule is scoped to fields, so rows are never trimmed.
			if !strings.Contains(h, "(void)os.write(_i0++, _e0);") {
				t.Errorf("[%s] nested array row must not be trimmed:\n%s", corelib, h)
			}
			// Decode is unchanged: the fixed std::array already materializes N
			// elements, zero-filled by the in-class initializer, so [M, N) is already
			// the element default. Over-count stays INVALID on the heap profile.
			if !strings.Contains(h, "std::array<std::uint32_t, 5> u32s = {};") {
				t.Errorf("[%s] fixed-count array must stay a zero-filled std::array:\n%s", corelib, h)
			}
			if !strings.Contains(h, "is.read(u32s);") {
				t.Errorf("[%s] fixed-count decode must read the whole array:\n%s", corelib, h)
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
		"(void)os.write(4, _trimTail(fixed));", // the counted one still trims
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
			// A non-zero schema default resets on decode, after the over-count guard
			// (a rejected message must not mutate the target) and before the read.
			for _, want := range []string{"\n            c = {};", "\n            d = {};", "\n            e = {};"} {
				if !strings.Contains(h, want) {
					t.Errorf("[%s] missing fixed-array reset %q:\n%s", corelib, want, h)
				}
			}
			if !g_containsInOrder(h, "if (_count > 5) { is.invalidate(); return; }", "\n            c = {};", "is.read(c);") && corelib == "cpp" {
				t.Errorf("[%s] reset must sit between the over-count guard and the read:\n%s", corelib, h)
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
	if !strings.Contains(h, "dyn.resize(_count); is.read(dyn);") {
		t.Errorf("dynamic array should resize to the wire count:\n%s", h)
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

const cppMapSchema = `
version: 1
messages:
  M:
    payload:
      counts: { type: map, id: 1, key: { type: string, maxlen: 32 }, value: { type: u32 }, count: 128 }
      nested:
        type: map
        id: 2
        key: { type: u32 }
        value: { type: map, key: { type: u32 }, value: { type: u8 } }
`

func TestCppMapField(t *testing.T) {
	// corelib: cpp (default): std::map surface + _MapSeq child-visitor decode.
	h := headerFromYAML(t, cppMapSchema, "m.hpp")
	for _, want := range []string{
		"#include <map>", // include emitted only when a map is present
		"std::map<std::string, std::uint32_t> counts",                           // surface container
		"std::map<std::uint32_t, std::map<std::uint32_t, std::uint8_t>> nested", // nested map value
		"struct _MapSeq",                 // shared collector prelude
		"for (const auto &_kv : counts)", // sorted (std::map) canonical-order encode
		"MCountsEntry _e; _e.key = _kv.first; _e.value = _kv.second;", // entry-struct reuse on serialize
		"_MapSeq<std::map<std::string, std::uint32_t>, MCountsEntry>", // decode collector
	} {
		if !strings.Contains(h, want) {
			t.Errorf("m.hpp (cpp) missing %q", want)
		}
	}
}

func TestCppMapCcppRejected(t *testing.T) {
	_, err := fixedHeader(t, cppMapSchema, "m.hpp", nil)
	if err == nil || !strings.Contains(err.Error(), "not yet supported for corelib: c-cpp") {
		t.Fatalf("expected c-cpp map rejection, got %v", err)
	}
}
