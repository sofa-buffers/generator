package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// fakeFormatterBackend stands in for a real backend so the CLI wiring can be
// tested without a language toolchain: it is the seam under test, not rustfmt.
type fakeFormatterBackend struct {
	note    string
	err     error
	gotDir  string
	formats int
}

func (*fakeFormatterBackend) Lang() string { return "testfmtlang" }

func (*fakeFormatterBackend) Generate(*ir.Schema, map[string]any) ([]generator.File, error) {
	return []generator.File{{Path: "a.txt", Content: []byte("raw")}}, nil
}

func (b *fakeFormatterBackend) Format(files []generator.File, dir string) ([]generator.File, string, error) {
	b.formats++
	b.gotDir = dir
	if b.err != nil {
		return nil, "", b.err
	}
	out := []generator.File{{Path: files[0].Path, Content: []byte("formatted")}}
	return out, b.note, nil
}

func init() { generator.Register(&fakeFormatterBackend{}) }

func backendUnderTest(t *testing.T) *fakeFormatterBackend {
	t.Helper()
	b, ok := generator.Lookup("testfmtlang")
	if !ok {
		t.Fatal("test backend not registered")
	}
	f := b.(*fakeFormatterBackend)
	f.note, f.err, f.gotDir, f.formats = "", nil, "", 0
	return f
}

// The CLI must run the optional formatter over what Generate produced, and
// write THAT -- the tree the user receives is the one their `cargo fmt --check`
// will run over.
func TestFormatFilesAppliesTheBackendFormatter(t *testing.T) {
	b := backendUnderTest(t)
	out := filepath.Join(t.TempDir(), "gen")
	noted := false
	files, err := formatFiles(formatAuto, "testfmtlang", out, []generator.File{{Path: "a.txt", Content: []byte("raw")}}, os.Stderr, &noted)
	if err != nil {
		t.Fatal(err)
	}
	if string(files[0].Content) != "formatted" {
		t.Errorf("unformatted content reached the writer: %q", files[0].Content)
	}
	// The formatter runs IN the output dir, which must therefore already exist:
	// that is how it picks up the project's own formatter configuration.
	if b.gotDir != out {
		t.Errorf("formatter ran in %q, want the output dir %q", b.gotDir, out)
	}
	if st, err := os.Stat(out); err != nil || !st.IsDir() {
		t.Errorf("output dir was not created before formatting: %v", err)
	}
}

// A backend without the optional capability is left alone -- no formatter, no
// error, no change.
func TestFormatFilesLeavesAPlainBackendAlone(t *testing.T) {
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	files, err := formatFiles(formatAuto, "go", t.TempDir(), in, os.Stderr, &noted)
	if err != nil {
		t.Fatal(err)
	}
	if string(files[0].Content) != "raw" {
		t.Errorf("content changed: %q", files[0].Content)
	}
}

// The "formatter is not installed" note is printed once per run, not once per
// definition file: a folder of twenty schemas must not print it twenty times.
func TestFormatFilesNotesOncePerRun(t *testing.T) {
	b := backendUnderTest(t)
	b.note = "rustfmt not found in PATH"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	for i := 0; i < 3; i++ {
		if _, err := formatFiles(formatAuto, "testfmtlang", t.TempDir(), in, w, &noted); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	out := readAll(t, r)
	if n := strings.Count(out, "rustfmt not found in PATH"); n != 1 {
		t.Errorf("note printed %d times over three definitions, want 1: %q", n, out)
	}
	if b.formats != 3 {
		t.Errorf("formatter ran %d times, want 3", b.formats)
	}
}

// A formatter that refuses must abort the run for that definition rather than
// let unformatted -- possibly unparsable -- code reach a file.
func TestFormatFilesSurfacesAFailure(t *testing.T) {
	b := backendUnderTest(t)
	b.err = errors.New("rustfmt refused generated src/message.rs")
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	files, err := formatFiles(formatAuto, "testfmtlang", t.TempDir(), in, os.Stderr, &noted)
	if err == nil {
		t.Fatal("a formatter failure was swallowed")
	}
	if files != nil {
		t.Error("files were handed back for writing after the formatter failed")
	}
}

// off is the DEFAULT, and off means nothing happens: no backend lookup, no
// process, and the bytes Generate produced reach the writer untouched. This is
// what makes a run reproducible across machines.
func TestFormatFilesOffRunsNothing(t *testing.T) {
	b := backendUnderTest(t)
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	files, err := formatFiles(formatOff, "testfmtlang", t.TempDir(), in, os.Stderr, &noted)
	if err != nil {
		t.Fatal(err)
	}
	if b.formats != 0 {
		t.Errorf("the formatter ran %d times under --format=off, want 0", b.formats)
	}
	if string(files[0].Content) != "raw" {
		t.Errorf("content changed under --format=off: %q", files[0].Content)
	}
}

// off must not print the "not installed" note either: a run that never intended
// to format has nothing to report.
func TestFormatFilesOffIsSilent(t *testing.T) {
	b := backendUnderTest(t)
	b.note = "rustfmt not found in PATH"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	if _, err := formatFiles(formatOff, "testfmtlang", t.TempDir(), in, w, &noted); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if got := readAll(t, r); got != "" {
		t.Errorf("--format=off printed %q, want nothing", got)
	}
}

// require is the mode for a build that must not ship unformatted code: a
// missing tool fails the run instead of degrading quietly, and no file is
// handed to the writer.
func TestFormatFilesRequireFailsOnAMissingFormatter(t *testing.T) {
	b := backendUnderTest(t)
	b.note = "rustfmt not found in PATH"
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	files, err := formatFiles(formatRequire, "testfmtlang", t.TempDir(), in, os.Stderr, &noted)
	if err == nil {
		t.Fatal("--format=require accepted a missing formatter")
	}
	if !strings.Contains(err.Error(), "rustfmt not found in PATH") {
		t.Errorf("the error does not name the reason: %v", err)
	}
	if files != nil {
		t.Error("files were handed back for writing although nothing was formatted")
	}
}

// A formatter that RUNS and refuses is an error under auto too: the backend
// emitted something that does not parse, and no switch turns that into a file.
func TestFormatFilesAutoStillFailsOnARefusal(t *testing.T) {
	b := backendUnderTest(t)
	b.err = errors.New("rustfmt refused generated src/message.rs")
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	if _, err := formatFiles(formatAuto, "testfmtlang", t.TempDir(), in, os.Stderr, &noted); err == nil {
		t.Fatal("--format=auto swallowed a refusal")
	}
}

func TestParseFormatMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want formatMode
	}{{"off", formatOff}, {"auto", formatAuto}, {"require", formatRequire}} {
		got, err := parseFormatMode(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseFormatMode(%q) = %q, %v", tc.in, got, err)
		}
	}
	// A typo must not fall back to a default -- it must name the three values.
	_, err := parseFormatMode("requre")
	if err == nil {
		t.Fatal("a misspelled mode was accepted")
	}
	for _, want := range []string{"off", "auto", "require", "requre"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, err := parseFormatMode(""); err == nil {
		t.Error("an empty mode was accepted")
	}
}

func readAll(t *testing.T, r *os.File) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// exampleDef is the showcase schema, used here only as a definition the rust
// backend certainly generates from.
const exampleDef = "../../examples/messages/example.yaml"

// stubRustfmtOnPath installs a fake `rustfmt` as the ONLY thing on PATH and
// returns the marker file it touches when it is spawned. The stub copies stdin
// to stdout, so the generated bytes are the same whether it ran or not: the
// marker, not the output, is what the switch is observed by.
func stubRustfmtOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	// cat by absolute path: PATH is about to hold nothing but this stub.
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("no cat to build a stub formatter from: %v", err)
	}
	script := "#!/bin/sh\necho ran >> " + marker + "\nexec " + cat + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rustfmt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return marker
}

// emptyPath removes every program from the run's reach, which is how a box
// without the target's formatter is reproduced.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func spawned(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	return err == nil
}

// runCLI drives the real CLI entry point and captures stderr.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	code := run(args, devnull, w)
	w.Close()
	return code, readAll(t, r)
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func generatedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// The switch, end to end through the real CLI and the real rust backend. Each
// case states what a user typed and what sofabgen is then allowed to do.
func TestFormatSwitchEndToEnd(t *testing.T) {
	// THE DEFAULT. sofabgen runs no external tool it was not asked to run, so
	// the same version writes the same bytes on a box that has rustfmt and on
	// one that does not.
	t.Run("default_is_off", func(t *testing.T) {
		marker := stubRustfmtOnPath(t)
		out := t.TempDir()
		code, errOut := runCLI(t, "--lang", "rust", "--in", exampleDef, "--out", out)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if spawned(t, marker) {
			t.Error("the default run spawned rustfmt")
		}
		if errOut != "" {
			t.Errorf("the default run wrote to stderr: %q", errOut)
		}
	})

	t.Run("auto_formats_when_the_tool_is_there", func(t *testing.T) {
		marker := stubRustfmtOnPath(t)
		out := t.TempDir()
		code, errOut := runCLI(t, "--lang", "rust", "--in", exampleDef, "--out", out, "--format=auto")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !spawned(t, marker) {
			t.Error("--format=auto did not spawn rustfmt although it was on PATH")
		}
	})

	// The config key is the second half of the switch: a project that always
	// wants formatted output says so once, in its config.
	t.Run("config_key_turns_it_on", func(t *testing.T) {
		marker := stubRustfmtOnPath(t)
		cfg := writeConfig(t, "generic:\n  run_formatter: auto\n")
		out := t.TempDir()
		code, errOut := runCLI(t, "--config", cfg, "--lang", "rust", "--in", exampleDef, "--out", out)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !spawned(t, marker) {
			t.Error("generic.run_formatter: auto did not reach the format pass")
		}
	})

	t.Run("flag_overrides_the_config_key", func(t *testing.T) {
		marker := stubRustfmtOnPath(t)
		cfg := writeConfig(t, "generic:\n  run_formatter: auto\n")
		out := t.TempDir()
		code, errOut := runCLI(t, "--config", cfg, "--lang", "rust", "--in", exampleDef, "--out", out, "--format=off")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if spawned(t, marker) {
			t.Error("--format=off did not override generic.run_formatter: auto")
		}
	})

	// require bites: with the tool gone the run FAILS and writes nothing, which
	// is the whole point of the mode -- a build that must not ship unformatted
	// code finds out at generate time.
	t.Run("require_fails_without_the_tool", func(t *testing.T) {
		emptyPath(t)
		out := t.TempDir()
		code, errOut := runCLI(t, "--lang", "rust", "--in", exampleDef, "--out", out, "--format=require")
		if code == 0 {
			t.Fatal("--format=require succeeded with no rustfmt on PATH")
		}
		if !strings.Contains(errOut, "rustfmt not found in PATH") {
			t.Errorf("the failure does not name the missing tool: %q", errOut)
		}
		if files := generatedFiles(t, out); len(files) != 0 {
			t.Errorf("unformatted files were written anyway: %v", files)
		}
	})

	// auto degrades: same box, same run, but the user asked for best effort.
	t.Run("auto_degrades_without_the_tool", func(t *testing.T) {
		emptyPath(t)
		out := t.TempDir()
		code, errOut := runCLI(t, "--lang", "rust", "--in", exampleDef, "--out", out, "--format=auto")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !strings.Contains(errOut, "rustfmt not found in PATH") {
			t.Errorf("--format=auto did not report the missing tool: %q", errOut)
		}
		if files := generatedFiles(t, out); len(files) == 0 {
			t.Error("--format=auto wrote nothing")
		}
	})

	t.Run("an_unknown_value_is_a_startup_error", func(t *testing.T) {
		code, errOut := runCLI(t, "--lang", "rust", "--in", exampleDef, "--out", t.TempDir(), "--format=requre")
		if code == 0 {
			t.Fatal("a misspelled --format value was accepted")
		}
		for _, want := range []string{"off", "auto", "require"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("the error does not name %q: %q", want, errOut)
			}
		}
	})

	// The config key goes through the closed config schema, so a bad value there
	// is refused at load time, before any generation.
	t.Run("an_unknown_config_value_is_refused", func(t *testing.T) {
		cfg := writeConfig(t, "generic:\n  run_formatter: sometimes\n")
		code, errOut := runCLI(t, "--config", cfg, "--lang", "rust", "--in", exampleDef, "--out", t.TempDir())
		if code == 0 {
			t.Fatal("generic.run_formatter: sometimes was accepted")
		}
		if !strings.Contains(errOut, "run_formatter") {
			t.Errorf("the error does not name the key: %q", errOut)
		}
	})
}
