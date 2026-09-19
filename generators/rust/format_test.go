package rust

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// stubFormatter installs a fake rustfmt for the duration of the test: a shell
// script that records the arguments it was handed and answers with `body` (or
// fails with `body` on stderr when `fail`). Testing against a real rustfmt
// would make this package's tests depend on a Rust toolchain -- the whole
// reason the pass lives outside Generate.
func stubFormatter(t *testing.T, body string, fail bool) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := filepath.Join(dir, "rustfmt-stub")
	sh := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argsFile + "\ncat > /dev/null\n"
	if fail {
		sh += "printf '%s' " + shQuote(body) + " >&2\nexit 1\n"
	} else {
		sh += "printf '%s' " + shQuote(body) + "\n"
	}
	if err := os.WriteFile(script, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldLook := rustfmtBin, lookPath
	rustfmtBin = script
	lookPath = exec.LookPath
	t.Cleanup(func() { rustfmtBin, lookPath = old, oldLook })
	return argsFile
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// The optional capability has to be visible through the interface, or the CLI
// never calls it and the whole pass is dead code.
func TestBackendImplementsFormatter(t *testing.T) {
	var b generator.Backend = &Backend{}
	if _, ok := b.(generator.Formatter); !ok {
		t.Fatal("rust Backend no longer satisfies generator.Formatter; the CLI will write unformatted Rust")
	}
}

func TestFormatRunsRustfmtOverEveryRustFile(t *testing.T) {
	args := stubFormatter(t, "FORMATTED\n", false)
	in := []generator.File{
		{Path: "src/message.rs", Content: []byte("fn  a(){}")},
		{Path: "Cargo.toml", Content: []byte("[package]\n")},
		{Path: "src/main.rs", Content: []byte("fn  b(){}")},
	}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if note != "" {
		t.Fatalf("unexpected note with a working formatter: %q", note)
	}
	if string(out[0].Content) != "FORMATTED\n" || string(out[2].Content) != "FORMATTED\n" {
		t.Errorf("a .rs file was not replaced by the formatter's output: %q / %q", out[0].Content, out[2].Content)
	}
	if string(out[1].Content) != "[package]\n" {
		t.Errorf("Cargo.toml went through rustfmt: %q", out[1].Content)
	}
	// Two .rs files, one invocation each, and each one carries the edition the
	// generated Cargo.toml declares -- rustfmt parses per edition, so a mismatch
	// silently formats against the wrong grammar.
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "--edition"); n != 2 {
		t.Errorf("rustfmt ran %d time(s) with --edition, want 2: %q", n, got)
	}
	if !strings.Contains(string(got), "--edition\n"+Edition+"\n") {
		t.Errorf("rustfmt was not told edition %s: %q", Edition, got)
	}
	for _, want := range []string{"--emit\nstdout\n", "--quiet\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing rustfmt argument %q in %q", want, got)
		}
	}
}

// Nothing to format means nothing to run: a backend must not spawn a process
// for a Cargo.toml-only file set.
func TestFormatSkipsWhenNoRustFile(t *testing.T) {
	args := stubFormatter(t, "FORMATTED\n", false)
	in := []generator.File{{Path: "Cargo.toml", Content: []byte("[package]\n")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil || note != "" {
		t.Fatalf("Format: err=%v note=%q", err, note)
	}
	if string(out[0].Content) != "[package]\n" {
		t.Errorf("content changed: %q", out[0].Content)
	}
	if _, err := os.Stat(args); err == nil {
		t.Error("rustfmt was invoked for a file set with no Rust in it")
	}
}

// A box without rustfmt still generates: the files come back untouched with a
// note. A generator that needs the target toolchain installed to emit code is
// not usable in the environments this one ships to.
func TestFormatWithoutRustfmtIsNotAnError(t *testing.T) {
	old, oldLook := rustfmtBin, lookPath
	rustfmtBin = "rustfmt"
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { rustfmtBin, lookPath = old, oldLook })

	in := []generator.File{{Path: "src/message.rs", Content: []byte("fn  a(){}")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("a missing rustfmt must not fail generation: %v", err)
	}
	if string(out[0].Content) != "fn  a(){}" {
		t.Errorf("content changed without a formatter: %q", out[0].Content)
	}
	if !strings.Contains(note, "rustfmt") {
		t.Errorf("no note naming rustfmt: %q", note)
	}
}

// A rustfmt that RUNS and refuses is the opposite case: it means the emitter
// produced Rust that does not parse, and that must not reach a file silently
// (the very fallback #579 called out in the Go backend).
func TestFormatSurfacesARefusal(t *testing.T) {
	stubFormatter(t, "error: this file contains an unclosed delimiter", true)
	in := []generator.File{
		{Path: "Cargo.toml", Content: []byte("x")},
		{Path: "src/message.rs", Content: []byte("fn a( {")},
	}
	out, _, err := (&Backend{}).Format(in, t.TempDir())
	if err == nil {
		t.Fatal("a rustfmt failure was swallowed")
	}
	if out != nil {
		t.Error("files were returned alongside the error; the caller could write them")
	}
	if !strings.Contains(err.Error(), "src/message.rs") {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "unclosed delimiter") {
		t.Errorf("rustfmt's own diagnosis was dropped: %v", err)
	}
}

// The real thing, when the box has it: the generated example must come back
// rustfmt-clean, and formatting must be idempotent (a second pass is a no-op).
// The corpus-wide gate is tests/conformance/rust/run.sh; this is the fast one.
func TestFormatRealRustfmtIsCleanAndIdempotent(t *testing.T) {
	if _, err := exec.LookPath("rustfmt"); err != nil {
		t.Skip("rustfmt not installed; tests/conformance/rust/run.sh is the gate")
	}
	s := exampleSchema(t)
	files, err := (&Backend{}).Generate(s, map[string]any{"corelib": "rs", "emit": "project"})
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
			t.Errorf("%s: rustfmt is not a fixed point on generated output", once[i].Path)
		}
	}
}

// The Edition constant is what the Format pass hands rustfmt; the Cargo.toml is
// what the crate is actually compiled and `cargo fmt`-ed under. They have to be
// the same string, in BOTH project shapes, or the generator formats against a
// grammar the crate does not use.
func TestEditionMatchesTheGeneratedCargoToml(t *testing.T) {
	for _, corelib := range []string{"rs", "rs-no-std"} {
		s := exampleSchema(t)
		if corelib == "rs-no-std" {
			s = exampleSchemaBounded(t)
		}
		files, err := (&Backend{}).Generate(s, map[string]any{"corelib": corelib, "emit": "project"})
		if err != nil {
			t.Fatalf("[%s] generate: %v", corelib, err)
		}
		var toml string
		for _, f := range files {
			if f.Path == "Cargo.toml" {
				toml = string(f.Content)
			}
		}
		if toml == "" {
			t.Fatalf("[%s] no Cargo.toml", corelib)
		}
		if want := "edition = \"" + Edition + "\"\n"; !strings.Contains(toml, want) {
			t.Errorf("[%s] Cargo.toml does not declare %s", corelib, want)
		}
	}
}
