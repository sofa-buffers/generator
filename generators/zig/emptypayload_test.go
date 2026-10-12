package zig

import (
	"strings"
	"testing"
)

// emptyPayloadSrc: a message whose payload declares no fields -- a legal schema
// (MESSAGE_SPEC §2 makes the all-default message the empty byte string), whose
// generated harness did not compile: Zig rejects an unused parameter and a local
// that is never mutated (generator#738).
const emptyPayloadSrc = "version: 1\nmessages:\n  ping:\n    payload: {}\n"

// TestZigEmptyPayloadHarnessCompiles pins both discards the JSON pair needs for a
// field-less type, beside the `alloc`/`obj` ones it already emitted.
func TestZigEmptyPayloadHarnessCompiles(t *testing.T) {
	files, err := generateYAML(t, emptyPayloadSrc, map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var main string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "src/main.zig") {
			main = string(f.Content)
		}
	}
	if main == "" {
		t.Fatal("no src/main.zig")
	}
	for _, want := range []string{
		"fn toJson_Ping(o: *const message.Ping, w: *std.Io.Writer) std.Io.Writer.Error!void {\n    _ = o;",
		"const o: message.Ping = .{};", // never mutated: `var` is an error
		"    _ = alloc;",
		"    _ = obj;",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("harness missing %q:\n%s", want, main)
		}
	}

	// A message WITH a field keeps `var` and reads `o`, so neither is blanket.
	files, err = generateYAML(t, "version: 1\nmessages:\n  one:\n    payload:\n"+
		"      n: { id: 0, type: u32 }\n", map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "src/main.zig") {
			got := string(f.Content)
			if !strings.Contains(got, "var o: message.One = .{};") {
				t.Errorf("a message with a field must keep `var`:\n%s", got)
			}
			if strings.Contains(got, "Writer.Error!void {\n    _ = o;") {
				t.Errorf("the discard belongs to the field-less case only:\n%s", got)
			}
		}
	}
}
