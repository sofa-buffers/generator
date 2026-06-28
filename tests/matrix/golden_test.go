package matrix

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
)

// TestGoldenOutput is the M8 reproducibility gate: regenerating scalars.yaml for
// every backend must be byte-identical to the committed golden snapshots under
// tests/matrix/golden/. A diff here means output drifted — regenerate with:
//
//	for l in c cpp go python typescript rust csharp java; do \
//	  go run ./cmd/sbufgen --lang $l --in tests/matrix/corpus/defs/scalars.yaml \
//	    --out tests/matrix/golden/$l; done
func TestGoldenOutput(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/scalars.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Use the same effective config the CLI applies (so goldens match `sbufgen`).
	empty := config.Empty()
	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		files, err := b.Generate(s, empty.Effective(lang))
		if err != nil {
			t.Fatalf("[%s] generate: %v", lang, err)
		}
		for _, f := range files {
			golden := filepath.Join("testdata", "golden", lang, f.Path)
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
