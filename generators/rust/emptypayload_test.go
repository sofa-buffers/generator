package rust

import (
	"strings"
	"testing"
)

// emptyPayloadSrc: a message whose payload declares no fields. A legal schema --
// MESSAGE_SPEC §2 makes the all-default message the empty byte string, so a
// field-less message is the ping -- which generated code the user's build warned
// about twice (generator#738).
const emptyPayloadSrc = "version: 1\nmessages:\n  ping:\n    payload: {}\n"

// TestRustEmptyPayloadIsWarningFree: serialize writes nothing, so the stream is
// `_os`, and the visitor binds nothing, so the destination borrow it must still
// hold says why it is unread. Both are warnings in `cargo build` otherwise.
func TestRustEmptyPayloadIsWarningFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
	}{
		{"rs", map[string]any{"corelib": "rs"}},
		{"rs-static", map[string]any{"corelib": "rs", "allow_dynamic": false}},
		{"rs-no-std", map[string]any{"corelib": "rs-no-std"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var src string
			for _, f := range filesFromYAML(t, emptyPayloadSrc, tc.cfg) {
				if strings.HasSuffix(f.Path, "message.rs") {
					src = string(f.Content)
				}
			}
			if src == "" {
				t.Fatal("no message.rs")
			}
			for _, want := range []string{
				"pub fn serialize<_F: sofab::Flush>(&self, _os: &mut sofab::OStream<'_, _F>)",
				"#[allow(dead_code)] // a type with no fields binds nothing",
			} {
				if !strings.Contains(src, want) {
					t.Errorf("missing %q:\n%s", want, src)
				}
			}
		})
	}

	// A message WITH a field keeps the plain name and no allowance: the rule is
	// "unused", not "always".
	var src string
	for _, f := range filesFromYAML(t, "version: 1\nmessages:\n  one:\n    payload:\n"+
		"      n: { id: 0, type: u32 }\n", map[string]any{"corelib": "rs"}) {
		if strings.HasSuffix(f.Path, "message.rs") {
			src = string(f.Content)
		}
	}
	if !strings.Contains(src, "pub fn serialize<_F: sofab::Flush>(&self, os: &mut sofab::OStream<'_, _F>)") {
		t.Errorf("a message with a field must name the stream `os`:\n%s", src)
	}
	if strings.Contains(src, "#[allow(dead_code)] // a type with no fields binds nothing") {
		t.Errorf("the dead_code allowance belongs to the field-less case only:\n%s", src)
	}
}
