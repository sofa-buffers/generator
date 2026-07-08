package zig

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleSchema(t *testing.T) *ir.Schema {
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
	return s
}

func exampleFiles(t *testing.T, cfg map[string]any) map[string]string {
	t.Helper()
	s := exampleSchema(t)
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

func TestZigStructural(t *testing.T) {
	m := exampleFiles(t, map[string]any{})["src/message.zig"]
	if m == "" {
		t.Fatal("no src/message.zig")
	}
	for _, want := range []string{
		"const sofab = @import(\"sofab\");",
		"pub const Myfirstmessage = struct {",
		"pub fn marshal(self: *const Myfirstmessage, os: *sofab.OStream) sofab.Error!void {",
		"pub fn encode(self: *const Myfirstmessage, alloc: std.mem.Allocator)",
		"pub fn decode(alloc: std.mem.Allocator, data: []const u8) sofab.Error!Myfirstmessage {",
		"const _dec_Myfirstmessage = struct {",            // flat-visitor decoder
		"pub fn sequenceBegin(self: *_dec_Myfirstmessage", // location-stack nesting
		"pub const MAX_SIZE: usize =",
		"someu64: u64 = 18446744073709551615,",                                                // schema default in the declaration
		"someuintarray: [4]u32 = .{ 0, 1, 1000, 4294967295 },",                                // fixed native array
		"somefloatarray: [3]f32 =",                                                            // fixed fp array
		"someboolarray: [8]bool = .{ true, true, false, false, false, false, false, false },", // tail-padded default
		"somestring: []const u8 = \"\",",                                                      // zero-copy string storage
		"someblob: []const u8 = &.{ 72, 101, 108, 108, 111 },",                                // blob default bytes
		"somemap: []const MyfirstmessageSomemap",                                              // dynamic composite array -> slice
		"if (!std.mem.eql(u32, self.someuintarray[0..], &.{ 0, 1, 1000, 4294967295 })) {",     // omit-guard vs default
		"std.mem.sliceAsBytes",                                                                // bool array 0/1 lowering
		"_put(&self.m.someuintarray, &self.ai,",                                               // bounds-checked indexed store
		"if (offset != 0) return;",                                                            // single-shot payload guard
		"self.m.somestring = chunk,",                                                          // zero-copy string decode
		"/// Unsigned 8-bit integer",                                                          // descriptions as doc comments
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.zig missing %q", want)
		}
	}
	// Sequences are always framed (never omit-guarded); the struct field write
	// must be unconditional.
	if !strings.Contains(m, "try os.writeSequenceBegin(20);") {
		t.Error("nested struct must be framed unconditionally")
	}
	// No heap containers in the message type: storage is fixed arrays + slices.
	for _, notWant := range []string{
		"ArrayList(", // only the encode sink may use a list, and only via _EncodeSink
	} {
		if strings.Count(m, notWant) > 1 { // once inside _EncodeSink
			t.Errorf("message.zig should not use %q for field storage", notWant)
		}
	}
}

func TestZigProjectMode(t *testing.T) {
	files := exampleFiles(t, map[string]any{"emit": "project"})
	for _, path := range []string{"src/message.zig", "src/main.zig", "build.zig", "build.zig.zon", "README.md"} {
		if files[path] == "" {
			t.Errorf("project mode missing %s", path)
		}
	}
	if !strings.Contains(files["build.zig.zon"], "${SOFAB_ZIG_CORELIB}") {
		t.Error("build.zig.zon must carry the corelib path placeholder")
	}
	if !strings.Contains(files["build.zig.zon"], ".name = .sofabuffers_generated") {
		t.Error("build.zig.zon must pin the fixed package name (its fingerprint depends on it)")
	}
	h := files["src/main.zig"]
	for _, want := range []string{
		"fromJson_Myfirstmessage(alloc, v)",
		"toJson_Myfirstmessage(&obj, out)",
		".number_string => |s| return std.fmt.parseInt(u64, s, 10) catch 0,", // u64 > 2^53 stays exact
		"std.json.Stringify.encodeJsonString",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("main.zig missing %q", want)
		}
	}
	// Sources mode emits no project scaffolding.
	src := exampleFiles(t, map[string]any{})
	if len(src) != 1 {
		t.Errorf("sources mode should emit only src/message.zig, got %d files", len(src))
	}
}

func TestZigKeywordEscaping(t *testing.T) {
	b, err := os.ReadFile("../../tests/matrix/corpus/defs/keywords.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(b, "keywords.yaml")
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
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		"@\"const\": u32 = 0,", // Zig keyword -> quoted identifier
		"@\"fn\": u32 = 0,",
		"@\"switch\": u32 = 0,",
		"type: u32 = 0,", // primitive-type names are legal field names
	} {
		if !strings.Contains(m, want) {
			t.Errorf("keywords message.zig missing %q", want)
		}
	}
}

func TestZigDeterministic(t *testing.T) {
	a := exampleFiles(t, map[string]any{"emit": "project"})
	b := exampleFiles(t, map[string]any{"emit": "project"})
	for path, content := range a {
		if b[path] != content {
			t.Fatalf("Zig generation not deterministic (%s)", path)
		}
	}
}
