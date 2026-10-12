package cpp

import (
	"strings"
	"testing"
)

// TestCppEncodeReportsItsVerdict is generator#736.
//
// encode() answered a refusal with an empty vector and encodeTo() with 0 --
// which is also what the ALL-DEFAULT message produces, whose encoding is
// legitimately zero bytes (MESSAGE_SPEC §2). Both surfaces read _os.ok() and
// then discarded what they learned, so the refusal generator#656 made reachable
// from an ordinary setter was unobservable at the documented entry points; the
// doc comment made it worse by sending the caller to encodeTo(), which could not
// tell them apart either. CORELIB_PLAN §5.1.2: "an encoder that could not write
// what it was asked to write reports it, and a one-shot helper that ignores that
// report is non-conformant."
//
// The fix keeps the §6.1.1 name set closed -- there is no try_encode to put the
// verdict in, the way try_decode holds the decode one -- so the status became
// the return value and the destination the out-parameter, on both surfaces, in
// all four profiles.
func TestCppEncodeReportsItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]any
	}{
		{"cpp", map[string]any{"namespace": "sofabuffers"}},
		{"cpp-static", map[string]any{"namespace": "sofabuffers", "allow_dynamic": false}},
		{"c-cpp", map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp", "allow_dynamic": true}},
		{"c-cpp-static", map[string]any{"namespace": "sofabuffers", "corelib": "c-cpp", "allow_dynamic": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := genHeader(t, encodeBoundsSrc, "m.hpp", tc.opts)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			for _, want := range []string{
				// The status is the return value, the bytes the out-parameter, and
				// discarding it is a compile error in a -Werror build.
				"[[nodiscard]] sofab::Error encode(std::vector<std::uint8_t> &_out) const {",
				"[[nodiscard]] sofab::Error encodeTo(std::uint8_t *_dst, std::size_t _cap, std::size_t &_used) const noexcept {",
				// The code the stream latched, not a bool: BufferFull and
				// InvalidArgument are the two failures a caller acts on differently.
				"if (!_os.ok()) { _out.clear(); return _os.error(); }",
				"if (!_os.ok()) { return _os.error(); }",
				"return sofab::Error::None;",
				// A failed encode hands back no partial message: the destination is
				// emptied and the byte count zeroed before the writes start.
				"_used = 0;",
				// The doc comment names the collision instead of pointing past it.
				"legitimately does.",
			} {
				if !strings.Contains(h, want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, bad := range []string{
				// The old silent surfaces, and the advice that sent a caller to one.
				"std::vector<std::uint8_t> encode() const",
				"std::size_t encodeTo(std::uint8_t *_dst, std::size_t _cap) const",
				"use encodeTo() when the",
				"if (!_os.ok()) { return {}; }",
				"if (!_os.ok()) { return 0; }",
				// §6.1.1 closes the name set: no second spelling of either entry point.
				"try_encode",
			} {
				if strings.Contains(h, bad) {
					t.Errorf("still emits %q:\n%s", bad, h)
				}
			}
		})
	}
}

// TestCppUnboundedEncodeReportsItsVerdict: the unbounded arm reports too. It
// drains a scratch through a flush sink, so its failure is not the bounded arm's
// -- the configured ceiling never binds it -- and it has its own return path.
func TestCppUnboundedEncodeReportsItsVerdict(t *testing.T) {
	h := headerFromYAML(t, "version: 1\nmessages:\n  U:\n    payload:\n"+
		"      s: { id: 1, type: string }\n", "u.hpp")
	for _, want := range []string{
		"[[nodiscard]] sofab::Error encode(std::vector<std::uint8_t> &_out) const {",
		"_out.clear();",
		"_os.flush();",
		"if (!_os.ok()) { _out.clear(); return _os.error(); }",
		"return sofab::Error::None;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("unbounded encode missing %q:\n%s", want, h)
		}
	}
}
