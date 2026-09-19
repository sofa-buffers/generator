package dart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// stubDartFormat installs a fake `dart` for the duration of the test: a shell
// script that records the arguments it was handed and answers with `body` (or
// fails with `body` on stderr when `fail`). Testing against a real Dart SDK
// would make this package's tests depend on that SDK -- the whole reason the
// pass lives outside Generate.
func stubDartFormat(t *testing.T, body string, fail bool) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := filepath.Join(dir, "dart-stub")
	sh := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argsFile + "\ncat > /dev/null\n"
	if fail {
		sh += "printf '%s' " + dartShQuote(body) + " >&2\nexit 65\n"
	} else {
		sh += "printf '%s' " + dartShQuote(body) + "\n"
	}
	if err := os.WriteFile(script, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldLook := dartBin, dartLookPath
	dartBin = script
	dartLookPath = exec.LookPath
	t.Cleanup(func() { dartBin, dartLookPath = old, oldLook })
	return argsFile
}

func dartShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// The optional capability has to be visible through the interface, or the CLI
// never calls it and the whole pass is dead code.
func TestBackendImplementsFormatter(t *testing.T) {
	var b generator.Backend = &Backend{}
	if _, ok := b.(generator.Formatter); !ok {
		t.Fatal("dart Backend no longer satisfies generator.Formatter; the CLI will write unformatted Dart")
	}
}

func TestFormatRunsDartFormatOverEveryDartFile(t *testing.T) {
	args := stubDartFormat(t, "FORMATTED\n", false)
	in := []generator.File{
		{Path: "lib/message.dart", Content: []byte("void  a(){}")},
		{Path: "pubspec.yaml", Content: []byte("name: harness\n")},
		{Path: "bin/harness.dart", Content: []byte("void  b(){}")},
	}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if note != "" {
		t.Fatalf("unexpected note with a working formatter: %q", note)
	}
	if string(out[0].Content) != "FORMATTED\n" || string(out[2].Content) != "FORMATTED\n" {
		t.Errorf("a .dart file was not replaced by the formatter's output: %q / %q", out[0].Content, out[2].Content)
	}
	if string(out[1].Content) != "name: harness\n" {
		t.Errorf("pubspec.yaml went through dart format: %q", out[1].Content)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "format\n"); n != 2 {
		t.Errorf("dart format ran %d time(s), want 2: %q", n, got)
	}
	// The language version is what decides the STYLE (short below 3.7, tall from
	// 3.7 on), so a pass that leaves it out formats against whichever version the
	// SDK defaults to -- and the generated pubspec asks for another one.
	if !strings.Contains(string(got), "--language-version="+LanguageVersion+"\n") {
		t.Errorf("dart format was not told language version %s: %q", LanguageVersion, got)
	}
	for _, want := range []string{"--output=show\n", "--summary=none\n", "--stdin-name=lib/message.dart\n", "--stdin-name=bin/harness.dart\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing dart format argument %q in %q", want, got)
		}
	}
}

// Nothing to format means nothing to run: a backend must not spawn a process
// for a pubspec-only file set.
func TestFormatSkipsWhenNoDartFile(t *testing.T) {
	args := stubDartFormat(t, "FORMATTED\n", false)
	in := []generator.File{{Path: "pubspec.yaml", Content: []byte("name: harness\n")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil || note != "" {
		t.Fatalf("Format: err=%v note=%q", err, note)
	}
	if string(out[0].Content) != "name: harness\n" {
		t.Errorf("content changed: %q", out[0].Content)
	}
	if _, err := os.Stat(args); err == nil {
		t.Error("dart was invoked for a file set with no Dart in it")
	}
}

// A box without the Dart SDK still generates: the files come back untouched
// with a note. A generator that needs the target toolchain installed to emit
// code is not usable in the environments this one ships to.
func TestFormatWithoutDartIsNotAnError(t *testing.T) {
	old, oldLook := dartBin, dartLookPath
	dartBin = "dart"
	dartLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { dartBin, dartLookPath = old, oldLook })

	in := []generator.File{{Path: "lib/message.dart", Content: []byte("void  a(){}")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("a missing dart must not fail generation: %v", err)
	}
	if string(out[0].Content) != "void  a(){}" {
		t.Errorf("content changed without a formatter: %q", out[0].Content)
	}
	if !strings.Contains(note, "dart") {
		t.Errorf("no note naming dart: %q", note)
	}
}

// A `dart format` that RUNS and refuses is the opposite case: it means the
// emitter produced Dart that does not parse, and that must not reach a file
// silently (the very fallback #579 called out in the Go backend).
func TestFormatSurfacesARefusal(t *testing.T) {
	stubDartFormat(t, "Could not format because the source could not be parsed", true)
	in := []generator.File{
		{Path: "pubspec.yaml", Content: []byte("x")},
		{Path: "lib/message.dart", Content: []byte("void a( {")},
	}
	out, _, err := (&Backend{}).Format(in, t.TempDir())
	if err == nil {
		t.Fatal("a dart format failure was swallowed")
	}
	if out != nil {
		t.Error("files were returned alongside the error; the caller could write them")
	}
	if !strings.Contains(err.Error(), "lib/message.dart") {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "could not be parsed") {
		t.Errorf("dart format's own diagnosis was dropped: %v", err)
	}
}

// The real thing, when the box has it: the generated example must come back
// dart-format-clean, and formatting must be idempotent (a second pass is a
// no-op). The corpus-wide gate is tests/conformance/dart/run.sh; this is the
// fast one.
func TestFormatRealDartFormatIsCleanAndIdempotent(t *testing.T) {
	if _, err := exec.LookPath("dart"); err != nil {
		t.Skip("dart not installed; tests/conformance/dart/run.sh is the gate")
	}
	files, err := (&Backend{}).Generate(schemaFor(t, exampleDef), map[string]any{"emit": "project"})
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
			t.Errorf("%s: dart format is not a fixed point on generated output", once[i].Path)
		}
	}
}

// LanguageVersion is what the Format pass hands `dart format`; the pubspec is
// what the package is actually resolved and formatted under by the user's own
// `dart format`. They have to agree, or the generator formats in one style and
// the user's check demands the other.
func TestLanguageVersionMatchesTheGeneratedPubspec(t *testing.T) {
	files, err := (&Backend{}).Generate(schemaFor(t, exampleDef), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatal(err)
	}
	var spec string
	for _, f := range files {
		if f.Path == "pubspec.yaml" {
			spec = string(f.Content)
		}
	}
	if spec == "" {
		t.Fatal("no pubspec.yaml in the generated project")
	}
	if want := "sdk: ^" + LanguageVersion + "."; !strings.Contains(spec, want) {
		t.Errorf("pubspec does not declare an SDK constraint starting at %s (want %q):\n%s", LanguageVersion, want, spec)
	}
}
