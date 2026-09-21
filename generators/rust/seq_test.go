package rust

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// seqConfigs are the four rust storage combinations: two corelibs times
// allow_dynamic, which is orthogonal to the corelib (ARCHITECTURE §9.5).
var seqConfigs = []map[string]any{
	{"corelib": "rs"},
	{"corelib": "rs", "allow_dynamic": false},
	{"corelib": "rs-no-std"},
	{"corelib": "rs-no-std", "allow_dynamic": true},
}

// seqSchemas are the definitions that between them reach every wrapper-array
// arm: leaf string/blob elements, struct/union elements, native matrix rows,
// wrapper rows, count-less arrays (std only) and repeated ids.
var seqSchemas = []string{
	"../../examples/messages/example.yaml",
	"../../tests/matrix/corpus/defs/named_members.yaml",
	"../../tests/bench/schema/unbounded_ingest.yaml",
	"../../tests/matrix/corpus/defs/seq_elements.yaml",
	"../../tests/matrix/corpus/defs/nested_rows.yaml",
}

// generateFile runs the backend on a definition file; ok is false when the
// schema is refused for this config (an unbounded field on rs-no-std).
func generateFile(t *testing.T, path string, cfg map[string]any) (files map[string]string, ok bool) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(src, filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("%s: invalid: %v", path, errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	out, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		return nil, false
	}
	files = map[string]string{}
	for _, f := range out {
		files[f.Path] = string(f.Content)
	}
	return files, true
}

// TestRustWrapperIndexBoundLivesInTheCorelib: every wrapper-array index check,
// gap fill and row reset goes through sofab::seq (generator#587, ARCHITECTURE
// §8). The generated code names the bound -- Bound::Schema(N) for a schema count,
// Bound::Cap(MAX_DYN_ARRAY_COUNT) for the receiver cap -- and compares nothing
// itself, on every corelib and storage mode. A literal index check or an inline
// growth loop coming back would be the emitted static helper this issue removed.
func TestRustWrapperIndexBoundLivesInTheCorelib(t *testing.T) {
	literal := regexp.MustCompile(`id as usize >=|\.len\(\) <= id as usize|push\(Default::default\(\)\)`)
	generated := 0
	for _, cfg := range seqConfigs {
		for _, path := range seqSchemas {
			files, ok := generateFile(t, path, cfg)
			if !ok {
				continue // an unbounded field on rs-no-std: refused by design
			}
			generated++
			m := files["src/message.rs"]
			if loc := literal.FindStringIndex(m); loc != nil {
				t.Errorf("%s (%v): an inline wrapper-array helper survived: %q", filepath.Base(path), cfg, m[loc[0]:loc[1]+60])
			}
			if !strings.Contains(m, "sofab::seq::") {
				t.Errorf("%s (%v): every schema here has a wrapper array, so it must call sofab::seq", filepath.Base(path), cfg)
			}
			if !strings.Contains(m, "fn refuse(&mut self, e: sofab::Error)") {
				t.Errorf("%s (%v): a seq verdict must be filed through V::refuse", filepath.Base(path), cfg)
			}
		}
	}
	if generated < len(seqConfigs)*3 {
		t.Fatalf("only %d schema/config pairs generated; the sweep lost its coverage", generated)
	}
}

// TestRustSeqCorelibFeatures: the SeqVec impl a crate's containers need is
// behind a corelib feature (heapless for static storage, alloc for rs-no-std's
// alloc fallback), and the generated Cargo.toml enables exactly that one. A
// missing feature fails the build loudly on the missing impl; an extra one on
// rs-no-std would pull alloc into a heap-free image.
func TestRustSeqCorelibFeatures(t *testing.T) {
	for _, tc := range []struct {
		cfg       map[string]any
		want, not string
	}{
		{map[string]any{"corelib": "rs"}, `package = "sofa-buffers-corelib", path = "${SOFAB_RS_CORELIB}" }`, "heapless"},
		{map[string]any{"corelib": "rs", "allow_dynamic": false}, `package = "sofa-buffers-corelib", path = "${SOFAB_RS_CORELIB}", features = ["heapless"] }`, `"alloc"`},
		{map[string]any{"corelib": "rs-no-std"}, `"value64", "heapless"]`, `"alloc"`},
		{map[string]any{"corelib": "rs-no-std", "allow_dynamic": true}, `"value64", "alloc"]`, `"heapless"]`},
	} {
		tc.cfg["emit"] = "project"
		files, ok := generateFile(t, "../../tests/matrix/corpus/defs/seq_elements.yaml", tc.cfg)
		if !ok {
			t.Fatalf("%v: generation failed", tc.cfg)
		}
		toml := files["Cargo.toml"]
		sofab := ""
		for _, line := range strings.Split(toml, "\n") {
			if strings.HasPrefix(line, "sofab = ") {
				sofab = line
			}
		}
		if !strings.Contains(sofab, tc.want) {
			t.Errorf("%v: corelib dependency must end %q, got %q", tc.cfg, tc.want, sofab)
		}
		if strings.Contains(sofab, tc.not) {
			t.Errorf("%v: corelib dependency must not name %q, got %q", tc.cfg, tc.not, sofab)
		}
	}
}
