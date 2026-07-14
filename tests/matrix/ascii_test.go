package matrix

import (
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// TestGeneratedOutputIsASCII guards the "generated code is pure ASCII" invariant:
// no backend may emit a non-ASCII byte (>= 0x80) into generated code. Banners,
// doc comments, Makefiles, and READMEs must use ASCII punctuation (e.g. "-" not
// the em-dash). It sweeps every registered backend over the whole corpus plus the
// example, in BOTH sources and project mode (project mode adds the Makefiles and
// READMEs that source mode omits). Hermetic — no toolchains.
func TestGeneratedOutputIsASCII(t *testing.T) {
	defs, _ := filepath.Glob("corpus/defs/*.yaml")
	defs = append(defs, filepath.Join("..", "..", "examples", "messages", "example.yaml"))
	if len(defs) < 2 {
		t.Fatal("no defs found")
	}
	modes := []map[string]any{
		{"emit": "sources"},
		{"emit": "project", "timestamp": false},
	}
	for _, def := range defs {
		s, err := buildIR(t, def)
		if err != nil {
			t.Fatalf("%s should validate: %v", def, err)
		}
		for _, lang := range generator.Registered() {
			if fixedOnlyTarget(lang) && hasUnboundedField(s) {
				continue // heapless target cannot size an unbounded field (generator#104)
			}
			b, _ := generator.Lookup(lang)
			for _, cfg := range modes {
				files, err := b.Generate(s, cfg)
				if err != nil {
					t.Errorf("[%s] generate %s: %v", lang, filepath.Base(def), err)
					continue
				}
				for _, f := range files {
					for i, c := range f.Content {
						if c >= 0x80 {
							t.Errorf("[%s] %s (%s, %s): non-ASCII byte 0x%02x at offset %d",
								lang, f.Path, filepath.Base(def), cfg["emit"], c, i)
							break
						}
					}
				}
			}
		}
	}
}
