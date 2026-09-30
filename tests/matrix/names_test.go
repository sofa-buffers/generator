package matrix

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// TestGeneratedFilesAreDistinct generates the shared name-collision schema
// (tests/conformance/lib/names.yaml, ARCHITECTURE §8 "Naming") for every
// target, in sources and project mode, and fails when generation refuses it or
// when two output files share a path once case is folded: the second would
// overwrite the first on write, or on a case-insensitive filesystem. Whether
// the files also compile is the conformance suites' part — they build the same
// schema against the real corelib.
func TestGeneratedFilesAreDistinct(t *testing.T) {
	def := filepath.Join("..", "conformance", "lib", "names.yaml")
	s, err := buildIR(t, def)
	if err != nil {
		t.Fatalf("names.yaml must validate: %v", err)
	}
	for _, lang := range generator.Registered() {
		t.Run(lang, func(t *testing.T) {
			b, _ := generator.Lookup(lang)
			for _, cfg := range []map[string]any{
				{"emit": "sources"},
				{"emit": "project", "timestamp": false},
			} {
				files, err := b.Generate(s, cfg)
				if err != nil {
					t.Errorf("%s: generate: %v", cfg["emit"], err)
					continue
				}
				seen := map[string]string{}
				for _, f := range files {
					k := strings.ToLower(filepath.ToSlash(f.Path))
					if prev, dup := seen[k]; dup {
						t.Errorf("%s: %q and %q are one file on a case-insensitive filesystem (or the same file)", cfg["emit"], prev, f.Path)
						continue
					}
					seen[k] = f.Path
				}
			}
		})
	}
}
