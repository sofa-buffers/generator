package matrix

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
)

// updateGolden rewrites the snapshots instead of comparing against them.
//
// The snapshots are a backend's `Generate` output, and the CLI is no longer a
// way to produce it: for the targets whose canonical formatter is an external
// program (rust, dart, python) `sofabgen` pipes the files through it before
// writing them (generator.Formatter, §8), so regenerating with `go run
// ./cmd/sofabgen` writes FORMATTED files that this test then rejects — on a box
// that has the formatter, and only there. Regenerating goes through the test
// itself, which calls the same Generate this compares.
var updateGolden = flag.Bool("update", false,
	"rewrite tests/matrix/testdata/golden from the backends' Generate output")

// TestGoldenOutput is the M8 reproducibility gate: regenerating scalars.yaml for
// every backend must be byte-identical to the committed golden snapshots under
// tests/matrix/golden/. A diff here means output drifted — regenerate with:
//
//	go test ./tests/matrix -run TestGoldenOutput -update
func TestGoldenOutput(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/scalars.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Use the same effective config the CLI applies (so goldens match `sofabgen`).
	empty := config.Empty()
	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		files, err := b.Generate(s, empty.Effective(lang))
		if err != nil {
			t.Fatalf("[%s] generate: %v", lang, err)
		}
		for _, f := range files {
			golden := filepath.Join("testdata", "golden", lang, f.Path)
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatalf("[%s] %s: %v", lang, golden, err)
				}
				if err := os.WriteFile(golden, f.Content, 0o644); err != nil {
					t.Fatalf("[%s] %s: %v", lang, golden, err)
				}
				continue
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Errorf("[%s] missing golden %s (regenerate)", lang, golden)
				continue
			}
			if string(f.Content) != string(want) {
				t.Errorf("[%s] %s drifted from golden (regenerate if intentional)", lang, f.Path)
			}
		}
	}
}
