package zig

import (
	"strings"
	"testing"
)

// TestZigDecoderResetsDestination pins the reset decoder(out, alloc) owes its
// caller-owned destination: an absent field fires no callback, so a destination
// that still holds an older message must be put back to the schema defaults
// before the first chunk, or it keeps that message's value for every field the
// new one omits (generator#784). The reset is `.{}` -- the field initializers
// carry the defaults -- so no per-schema reset() is emitted beside it.
// tests/conformance/zig/run.sh proves the values through
// check_reused_destination.py and the harness's `reusedecode` verb.
func TestZigDecoderResetsDestination(t *testing.T) {
	files, err := generateYAML(t, "version: 1\nmessages:\n  one:\n    payload:\n"+
		"      n: { id: 0, type: u32, default: 7 }\n"+
		"      s: { id: 1, type: string, maxlen: 8, default: \"x\" }\n",
		map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var msg, main string
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Path, "src/message.zig"):
			msg = string(f.Content)
		case strings.HasSuffix(f.Path, "src/main.zig"):
			main = string(f.Content)
		}
	}
	if msg == "" || main == "" {
		t.Fatal("no src/message.zig or src/main.zig")
	}
	want := "    pub fn decoder(out: *One, alloc: std.mem.Allocator) Decoder {\n" +
		"        out.* = .{};\n" +
		"        return .{ .v = .{ .m = out, .alloc = alloc } };\n"
	if !strings.Contains(msg, want) {
		t.Errorf("decoder() must reset out before binding it; want\n%s\nin:\n%s", want, msg)
	}
	if strings.Contains(msg, "fn reset(") {
		t.Errorf("no reset() is emitted: `.{}` already is the reset")
	}
	if !strings.Contains(main, `std.mem.eql(u8, mode, "reusedecode")`) {
		t.Errorf("harness has no reusedecode verb:\n%s", main)
	}
}
