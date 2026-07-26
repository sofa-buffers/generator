package matrix

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// maxSizePattern matches the worst-case size constant in every target's syntax.
// A backend that emits one under a new spelling must be added here rather than
// silently dropping out of the agreement check.
var maxSizePattern = regexp.MustCompile(
	`(?:_MAX_SIZE\s+(\d+))` + // C:      #define X_MAX_SIZE 49
		`|(?:_maxSize = (\d+))` + // C++:    static constexpr … _maxSize = 49
		`|(?:MAX_SIZE: usize = (\d+))` + // Rust:   pub const MAX_SIZE: usize = 49
		`|(?:MAX_SIZE: usize = (\d+))` + // Zig:    pub const MAX_SIZE: usize = 49
		`|(?:MaxSize = (\d+))` + // C#:     public const int MaxSize = 49
		`|(?:MAX_SIZE = (\d+))` + // Java:   public static final int MAX_SIZE = 49
		`|(?:maxSize = (\d+))`, // Dart:   static const int maxSize = 49
)

// TestMaxSizeAgreesAcrossTargets is the guard the per-backend cost models never
// had: the wire format is language-agnostic, so a message's worst-case encoded
// size is a property of the SCHEMA and every target that emits it must emit the
// SAME number.
//
// Seven backends each carried their own copy of the walk, and they disagreed —
// six charged every integer the full 64-bit varint width (10 bytes) where the
// schema said u32 (5), and all seven charged a surplus framing byte per wrapper
// array. One of the seven also sized an unbounded array at zero bytes. None of
// that was visible while each backend was only ever compared against itself.
func TestMaxSizeAgreesAcrossTargets(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/scalars.yaml")
	if err != nil {
		t.Fatal(err)
	}
	empty := config.Empty()

	sizes := map[string]int64{}
	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		files, err := b.Generate(s, empty.Effective(lang))
		if err != nil {
			t.Fatalf("[%s] generate: %v", lang, err)
		}
		for _, f := range files {
			for _, m := range maxSizePattern.FindAllStringSubmatch(string(f.Content), -1) {
				for _, g := range m[1:] {
					if g == "" {
						continue
					}
					v, err := strconv.ParseInt(g, 10, 64)
					if err != nil {
						t.Fatalf("[%s] %s: unparsable size %q", lang, f.Path, g)
					}
					if prev, seen := sizes[lang]; seen && prev != v {
						t.Errorf("[%s] emits two different sizes: %d and %d", lang, prev, v)
					}
					sizes[lang] = v
				}
			}
		}
	}
	if len(sizes) < 2 {
		t.Fatalf("expected several targets to emit a size constant, got %v", sizes)
	}

	// The schema is the authority: every target must match the shared walk.
	want, bounded := ir.MaxWireSize(s.Messages[0].Fields)
	if !bounded {
		t.Fatal("corpus message unexpectedly unbounded")
	}
	for lang, got := range sizes {
		if got != want {
			t.Errorf("[%s] MAX_SIZE = %d, but ir.MaxWireSize says %d — a backend is "+
				"computing the wire size itself instead of using the shared walk", lang, got, want)
		}
	}
}

// TestMaxMessageSizeCeiling covers the two roles of the max_message_size key:
// filling in for a schema that cannot be bounded, and refusing a schema that
// does not fit a budget the user actually declared.
func TestMaxMessageSizeCeiling(t *testing.T) {
	// An unbounded array has no worst case; the ceiling stands in for it, and the
	// generated code says so by naming the constant MAX_SIZE_LIMIT.
	unbounded, err := buildIRFromSource(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      a: { id: 0, type: array, items: { type: u32 } }\n")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generator.Lookup("rust")
	files, err := b.Generate(unbounded, map[string]any{"corelib": "rs", "max_message_size": 512})
	if err != nil {
		t.Fatalf("generate unbounded: %v", err)
	}
	var src string
	for _, f := range files {
		src += string(f.Content)
	}
	for _, want := range []string{
		"pub const MAX_SIZE_LIMIT: usize = 512;",
		"pub const MAX_SIZE: usize = Self::MAX_SIZE_LIMIT;",
	} {
		if !contains(src, want) {
			t.Errorf("an unbounded message must name its imposed ceiling, missing %q:\n%s", want, src)
		}
	}

	// A bounded schema keeps its computed size — the ceiling does not replace a
	// number the schema can supply.
	bounded, err := buildIRFromSource(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      a: { id: 0, type: array, items: { type: u32, count: 4 } }\n")
	if err != nil {
		t.Fatal(err)
	}
	files, err = b.Generate(bounded, map[string]any{"corelib": "rs", "max_message_size": 512})
	if err != nil {
		t.Fatalf("generate bounded: %v", err)
	}
	src = ""
	for _, f := range files {
		src += string(f.Content)
	}
	if !contains(src, "pub const MAX_SIZE: usize = 22;") {
		t.Errorf("a bounded message must keep its computed size, not the ceiling:\n%s", src)
	}
	if contains(src, "MAX_SIZE_LIMIT") {
		t.Error("a bounded message must not emit an imposed-ceiling constant")
	}

	// A schema that outgrows an EXPLICIT budget is a generate-time error, not a
	// runtime surprise.
	if _, err := b.Generate(bounded, map[string]any{"corelib": "rs", "max_message_size": 8}); err == nil {
		t.Error("a worst case above the configured max_message_size must fail generation")
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }

// buildIRFromSource builds the IR from an inline schema, so a size case can be
// stated in three lines instead of a corpus file.
func buildIRFromSource(t *testing.T, src string) (*ir.Schema, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return nil, err
	}
	return buildIR(t, path)
}
