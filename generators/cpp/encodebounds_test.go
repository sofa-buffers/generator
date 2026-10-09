package cpp

import (
	"strings"
	"testing"
)

// encodeBoundsSrc has one field of every bounded shape the encoder can be handed
// past its bound -- string and blob maxlen, a native array's count, a string /
// blob element's maxlen and its array's count, a nested row's count at both
// levels, a struct array's count, a union option's maxlen -- next to the shapes
// that need no check: an unbounded string, and scalars whose C++ type already
// is the declared width.
const encodeBoundsSrc = "version: 1\nmessages:\n  M:\n    payload:\n" +
	"      s:    { id: 0, type: string, maxlen: 4 }\n" +
	"      b:    { id: 1, type: blob, maxlen: 6 }\n" +
	"      au:   { id: 2, type: array, items: { type: u32, count: 3 } }\n" +
	"      as:   { id: 3, type: array, items: { type: string, count: 5, maxlen: 7 } }\n" +
	"      ab:   { id: 4, type: array, items: { type: blob, count: 2, maxlen: 9 } }\n" +
	"      rows: { id: 5, type: array, items: { type: array, count: 2, items: { type: u16, count: 11 } } }\n" +
	"      sts:  { id: 6, type: array, items: { type: struct, count: 4, fields: { t: { id: 0, type: string, maxlen: 12 } } } }\n" +
	"      un:   { id: 7, type: union, oneof: { i: { id: 0, type: i32 }, us: { id: 1, type: string, maxlen: 13 } } }\n" +
	"      u8:   { id: 8, type: u8 }\n" +
	"      i16:  { id: 9, type: i16 }\n" +
	"      e:    { id: 10, type: enum, enum: { A: 0, B: 1 } }\n" +
	"      f:    { id: 11, type: bitfield, bits: { x: { pos: 0 } } }\n"

// The guards encodeBoundsSrc gets in a growable (std::string / std::vector)
// member, by field: each refuses through the corelib's rejectArgument() before
// the value is written.
var encodeBoundsGuards = []string{
	"if (!s.empty()) { if (s.size() > 4) { return _os.rejectArgument(); } (void)_os.write(0, s); }",
	"if (!b.empty()) { if (b.size() > 6) { return _os.rejectArgument(); } (void)_os.write(1, b.data(),",
	"if (au.size() > 3) { return _os.rejectArgument(); }",
	"if (as.size() > 5) { return _os.rejectArgument(); }",
	"const auto &_e0 = as[_i0]; if (_e0.size() > 7) { return _os.rejectArgument(); }",
	"if (ab.size() > 2) { return _os.rejectArgument(); }",
	"const auto &_e0 = ab[_i0]; if (_e0.size() > 9) { return _os.rejectArgument(); }",
	"if (rows.size() > 2) { return _os.rejectArgument(); }",
	"if (_e0.size() > 11) { return _os.rejectArgument(); }",
	"if (sts.size() > 4) { return _os.rejectArgument(); }",
	"if (t.size() > 12) { return _os.rejectArgument(); }",
	"if ((*std::get_if<1>(&_opts)).size() > 13) { return _os.rejectArgument(); }",
}

// TestCppEncodeRefusesOverBoundOnGrowableStorage: under allow_dynamic (the
// corelib: cpp default) every bounded member is a std::string / std::vector,
// which can hold more than its bound, so serialize() refuses the value at encode
// -- one guard per bound, no opt-out switch on the maxspeed profile -- and only
// there: the unbounded string and the fixed-width scalars carry none.
func TestCppEncodeRefusesOverBoundOnGrowableStorage(t *testing.T) {
	h, err := genHeader(t, encodeBoundsSrc, "m.hpp", map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range encodeBoundsGuards {
		if !strings.Contains(h, want) {
			t.Errorf("missing encode guard %q", want)
		}
	}
	if got, want := strings.Count(h, "_os.rejectArgument()"), len(encodeBoundsGuards); got != want {
		t.Errorf("%d rejectArgument() calls, want exactly one per bound (%d)", got, want)
	}
	if strings.Contains(h, "ENCODE_BOUNDS") {
		t.Error("the maxspeed profile (corelib: cpp) has no encode-bounds opt-out")
	}
	for _, scalar := range []string{"u8 >", "i16 >", "i16 <", "e >", "f >"} {
		if strings.Contains(h, "if ("+scalar) {
			t.Errorf("a fixed-width scalar needs no encode guard, found %q", scalar)
		}
	}
	// The doc says what serialize refuses.
	if !strings.Contains(h, "is refused: the") || !strings.Contains(h, "stream latches InvalidArgument") {
		t.Error("serialize() doc does not say what it refuses")
	}
}

// TestCppEncodeBoundsSkipUnbounded: a field without a bound has nothing to check.
func TestCppEncodeBoundsSkipUnbounded(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string }\n" +
		"      a: { id: 1, type: array, items: { type: u32 } }\n" +
		"      t: { id: 2, type: array, items: { type: string } }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(h, "rejectArgument") {
		t.Errorf("an unbounded field got an encode guard:\n%s", h)
	}
}

// TestCppStaticStorageClampsInsteadOfRefusing: a sofab::FixedString / FixedBytes
// / InlineVector carries its bound as its capacity and clamps on assignment (a
// documented contract), so the static profiles emit no encode guard -- except
// where a bounded level still falls back to a growable container: a count-ed
// array of maxlen-less strings is a std::vector, and its count is checked.
func TestCppStaticStorageClampsInsteadOfRefusing(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"namespace": "sofabuffers", "allow_dynamic": false},
		{"namespace": "sofabuffers", "corelib": "c-cpp"},
	} {
		h, err := genHeader(t, encodeBoundsSrc, "m.hpp", cfg)
		if err != nil {
			t.Fatalf("generate %v: %v", cfg, err)
		}
		if strings.Contains(h, "rejectArgument") {
			t.Errorf("%v: fixed-capacity storage got an encode guard", cfg)
		}
		if !strings.Contains(h, "clamps on assignment instead") {
			t.Errorf("%v: serialize() doc does not state the clamp contract", cfg)
		}
	}
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      t: { id: 0, type: array, items: { type: string, count: 3 } }\n"
	h, err := genHeader(t, src, "m.hpp", map[string]any{"namespace": "sofabuffers", "allow_dynamic": false})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(h, "std::vector<std::string> t") ||
		!strings.Contains(h, "if (t.size() > 3) { return _os.rejectArgument(); }") {
		t.Errorf("a count-ed array that falls back to std::vector keeps its count guard:\n%s", h)
	}
}

// TestCppCCppEncodeBoundsOptOut: on corelib: c-cpp (the footprint profile) each
// guard starts with sofab::ENCODE_BOUNDS, a corelib constant that
// SOFAB_DISABLE_ENCODE_BOUNDS turns false, so the switch compiles every check
// away; the check itself is the same one the maxspeed profile emits.
func TestCppCCppEncodeBoundsOptOut(t *testing.T) {
	h, err := genHeader(t, encodeBoundsSrc, "m.hpp", map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp", "allow_dynamic": true})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"if (!s.empty()) { if (sofab::ENCODE_BOUNDS && s.size() > 4) { return _os.rejectArgument(); } (void)_os.write(0, s); }",
		"if (sofab::ENCODE_BOUNDS && au.size() > 3) { return _os.rejectArgument(); }",
		"const auto &_e0 = as[_i0]; if (sofab::ENCODE_BOUNDS && _e0.size() > 7) { return _os.rejectArgument(); }",
		"Defining SOFAB_DISABLE_ENCODE_BOUNDS compiles the refusals out.",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q", want)
		}
	}
	if got, gated := strings.Count(h, "_os.rejectArgument()"), strings.Count(h, "if (sofab::ENCODE_BOUNDS && "); got != gated || got == 0 {
		t.Errorf("%d guards, %d behind sofab::ENCODE_BOUNDS: every c-cpp guard must be switchable", got, gated)
	}
}

// TestCppHarnessReportsAnEncodeRefusal: encode() answers a refusal with no bytes,
// which is also what an all-default message encodes to; the harness tells them
// apart with _isDefault() and exits non-zero on the refusal, so the conformance
// driver sees it.
func TestCppHarnessReportsAnEncodeRefusal(t *testing.T) {
	main, err := genHeader(t, encodeBoundsSrc, "harness/main.cpp", map[string]any{"namespace": "sofabuffers", "emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(main, `if (bytes.empty() && !obj._isDefault()) { std::cerr << "encode refused\n"; return 1; }`) {
		t.Error("the harness encode verb does not report a refused encode")
	}
	js, err := genHeader(t, encodeBoundsSrc, "harness/_json.hpp", map[string]any{"namespace": "sofabuffers", "emit": "project", "allow_dynamic": false})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// push_back, which an InlineVector drops past its capacity, not emplace_back,
	// which would overwrite the last element kept.
	if strings.Contains(js, "emplace_back") || !strings.Contains(js, "o.as.push_back(std::move(_v0));") {
		t.Errorf("a string element is not appended through push_back:\n%s", js)
	}
}
