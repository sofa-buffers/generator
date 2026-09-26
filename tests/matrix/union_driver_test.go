package matrix

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// TestUnionDriverSchema keeps tests/conformance/lib/check_union.py honest from
// the core suite, before any language wires it in (generator#608): its builders
// match their hand-written hex (`--self-test`), and the document it prints
// validates, splits its $defs union into one type per default_id, and
// generates for every registered backend.
func TestUnionDriverSchema(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	driver := filepath.Join("..", "conformance", "lib", "check_union.py")
	if out, err := exec.Command(py, driver, "--self-test").CombinedOutput(); err != nil {
		t.Fatalf("check_union.py --self-test: %v\n%s", err, out)
	}
	schema, err := exec.Command(py, driver, "--emit-schema").Output()
	if err != nil {
		t.Fatalf("check_union.py --emit-schema: %v", err)
	}
	path := filepath.Join(t.TempDir(), "union.yaml")
	if err := os.WriteFile(path, schema, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := buildIR(t, path)
	if err != nil {
		t.Fatalf("the driver's schema must validate: %v", err)
	}
	for key, want := range map[string]int64{"union/Pick_default_n": 0, "union/Pick_default_t": 1} {
		nt, ok := s.Named[key]
		if !ok || nt.DefaultID == nil || *nt.DefaultID != want {
			t.Errorf("want split type %s with default_id %d, got %+v", key, want, nt)
		}
	}
	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		if _, err := b.Generate(s, map[string]any{}); err != nil {
			t.Errorf("[%s] generate: %v", lang, err)
		}
	}
}
