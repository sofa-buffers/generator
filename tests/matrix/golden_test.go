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
// The snapshots are a backend's `Generate` output, and regenerating goes
// through this test rather than through the CLI, which calls the same Generate
// this compares. `go run ./cmd/sofabgen` matches it only as long as the run
// formats nothing: for the targets whose canonical formatter is an external
// program (rust, dart, python) a run with --format=auto or --format=require
// pipes the files through it before writing them (generator.Formatter, §8) and
// the result is FORMATTED files this test rejects — on a box that has the
// formatter, and only there. That is one of the reasons --format defaults to
// off: the bytes a plain run writes are the bytes pinned here.
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

// stubFormattersOnPath makes PATH hold nothing but a stub for every external
// formatter a backend could reach for, each of which mangles what it is given
// and records that it ran. Anything that spawns one is then visible twice: in
// the bytes and in the marker file whose path is returned.
func stubFormattersOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	// Every binary a generator.Formatter implementor could reach for
	// (ARCHITECTURE §8 names them), plus npx, which is how a prettier is
	// commonly resolved. A formatter added to a backend belongs in this list
	// the same day, or this test cannot see it spawn.
	for _, tool := range []string{"rustfmt", "dart", "ruff", "prettier", "npx", "gofmt", "zig", "clang-format"} {
		script := "#!/bin/sh\necho " + tool + " >> " + marker + "\necho MANGLED\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return marker
}

// TestGoldensDoNotDependOnAnyFormatter pins what the snapshots are worth.
//
// A golden is only a regression detector as long as the same inputs produce the
// same bytes everywhere and forever. An external formatter is neither: rustfmt,
// `dart format` and ruff each change their layout between releases, and none of
// them is installed on every machine. If the snapshots were formatter output, a
// formatter release — or a colleague without the tool — would "move" a golden
// that no backend touched.
//
// They are not: `Generate` is a pure function of (IR, config) and spawns
// nothing, the optional formatter pass lives in the CLI behind `--format`
// (default `off`, §8/§12 gate 10), and `-update` above writes exactly the bytes
// this compares. This test holds that line: with every formatter on PATH
// replaced by a stub that would mangle whatever reached it, every backend's
// output still equals the committed golden, and no stub ran.
func TestGoldensDoNotDependOnAnyFormatter(t *testing.T) {
	marker := stubFormattersOnPath(t)
	s, err := buildIR(t, "corpus/defs/scalars.yaml")
	if err != nil {
		t.Fatal(err)
	}
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
				t.Errorf("[%s] %s differs from its golden with formatters stubbed out: "+
					"Generate must not depend on an external tool", lang, f.Path)
			}
		}
	}
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("Generate spawned an external formatter: %s", ran)
	}
}
