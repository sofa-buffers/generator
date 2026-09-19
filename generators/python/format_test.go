package python

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// stubRuff installs a fake ruff for the duration of the test: a shell script
// that records the arguments it was handed and answers with `body` (or fails
// with `body` on stderr when `fail`). Testing against a real ruff would make
// this package's tests depend on a tool the box may not have -- the whole
// reason the pass lives outside Generate.
func stubRuff(t *testing.T, body string, fail bool) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := filepath.Join(dir, "ruff-stub")
	sh := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argsFile + "\ncat > /dev/null\n"
	if fail {
		sh += "printf '%s' " + pyShQuote(body) + " >&2\nexit 2\n"
	} else {
		sh += "printf '%s' " + pyShQuote(body) + "\n"
	}
	if err := os.WriteFile(script, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldLook := ruffBin, ruffLookPath
	ruffBin = script
	ruffLookPath = exec.LookPath
	t.Cleanup(func() { ruffBin, ruffLookPath = old, oldLook })
	return argsFile
}

func pyShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// The optional capability has to be visible through the interface, or the CLI
// never calls it and the whole pass is dead code.
func TestBackendImplementsFormatter(t *testing.T) {
	var b generator.Backend = &Backend{}
	if _, ok := b.(generator.Formatter); !ok {
		t.Fatal("python Backend no longer satisfies generator.Formatter; the CLI will write unformatted Python")
	}
}

func TestFormatRunsRuffOverEveryPythonFile(t *testing.T) {
	args := stubRuff(t, "FORMATTED\n", false)
	in := []generator.File{
		{Path: "message.py", Content: []byte("x=[1,2]")},
		{Path: "README.md", Content: []byte("# gen\n")},
		{Path: "harness.py", Content: []byte("y=[3,4]")},
	}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if note != "" {
		t.Fatalf("unexpected note with a working formatter: %q", note)
	}
	if string(out[0].Content) != "FORMATTED\n" || string(out[2].Content) != "FORMATTED\n" {
		t.Errorf("a .py file was not replaced by the formatter's output: %q / %q", out[0].Content, out[2].Content)
	}
	if string(out[1].Content) != "# gen\n" {
		t.Errorf("README.md went through ruff: %q", out[1].Content)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "format\n"); n != 2 {
		t.Errorf("ruff format ran %d time(s), want 2: %q", n, got)
	}
	// The file name is what ruff resolves the user's pyproject.toml/ruff.toml
	// against, and `-` is what makes it read the module off stdin rather than
	// off a disk it has not been written to yet.
	for _, want := range []string{"--stdin-filename=message.py\n", "--stdin-filename=harness.py\n", "-\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing ruff argument %q in %q", want, got)
		}
	}
}

// Nothing to format means nothing to run: a backend must not spawn a process
// for a file set with no Python in it.
func TestFormatSkipsWhenNoPythonFile(t *testing.T) {
	args := stubRuff(t, "FORMATTED\n", false)
	in := []generator.File{{Path: "README.md", Content: []byte("# gen\n")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil || note != "" {
		t.Fatalf("Format: err=%v note=%q", err, note)
	}
	if string(out[0].Content) != "# gen\n" {
		t.Errorf("content changed: %q", out[0].Content)
	}
	if _, err := os.Stat(args); err == nil {
		t.Error("ruff was invoked for a file set with no Python in it")
	}
}

// A box without ruff still generates -- and unlike rustfmt, which ships with
// every rustup toolchain, this is the common case: ruff is not part of the
// Python toolchain. The files come back untouched with a note.
func TestFormatWithoutRuffIsNotAnError(t *testing.T) {
	old, oldLook := ruffBin, ruffLookPath
	ruffBin = "ruff"
	ruffLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { ruffBin, ruffLookPath = old, oldLook })

	in := []generator.File{{Path: "message.py", Content: []byte("x=[1,2]")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("a missing ruff must not fail generation: %v", err)
	}
	if string(out[0].Content) != "x=[1,2]" {
		t.Errorf("content changed without a formatter: %q", out[0].Content)
	}
	if !strings.Contains(note, "ruff") {
		t.Errorf("no note naming ruff: %q", note)
	}
}

// A ruff that RUNS and refuses is the opposite case: it means the emitter
// produced Python that does not parse, and that must not reach a file silently
// (the very fallback #579 called out in the Go backend).
func TestFormatSurfacesARefusal(t *testing.T) {
	stubRuff(t, "error: Failed to parse message.py:1:5: Expected an identifier", true)
	in := []generator.File{
		{Path: "README.md", Content: []byte("x")},
		{Path: "message.py", Content: []byte("def (")},
	}
	out, _, err := (&Backend{}).Format(in, t.TempDir())
	if err == nil {
		t.Fatal("a ruff failure was swallowed")
	}
	if out != nil {
		t.Error("files were returned alongside the error; the caller could write them")
	}
	if !strings.Contains(err.Error(), "message.py") {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "Failed to parse") {
		t.Errorf("ruff's own diagnosis was dropped: %v", err)
	}
}

// The real thing, when the box has it: the generated example must come back
// ruff-format-clean, and formatting must be idempotent (a second pass is a
// no-op). The corpus-wide gate is tests/conformance/python/run.sh; this is the
// fast one.
func TestFormatRealRuffIsCleanAndIdempotent(t *testing.T) {
	if _, err := exec.LookPath("ruff"); err != nil {
		t.Skip("ruff not installed; tests/conformance/python/run.sh is the gate")
	}
	s := schemaFile(t, "../../examples/messages/example.yaml")
	files, err := (&Backend{}).Generate(s, map[string]any{"emit": "project"})
	if err != nil {
		t.Fatal(err)
	}
	once, _, err := (&Backend{}).Format(files, t.TempDir())
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	twice, _, err := (&Backend{}).Format(once, t.TempDir())
	if err != nil {
		t.Fatalf("Format (second pass): %v", err)
	}
	for i := range once {
		if string(once[i].Content) != string(twice[i].Content) {
			t.Errorf("%s: ruff format is not a fixed point on generated output", once[i].Path)
		}
	}
}
