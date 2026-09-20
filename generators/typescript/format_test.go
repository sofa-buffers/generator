package typescript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// stubPrettier installs a fake prettier for the duration of the test: a shell
// script that records the arguments it was handed and answers with `body` (or
// fails with `body` on stderr when `fail`). Testing against a real prettier
// would make this package's tests depend on a tool the box may not have -- the
// whole reason the pass lives outside Generate.
func stubPrettier(t *testing.T, body string, fail bool) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := filepath.Join(dir, "prettier-stub")
	sh := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argsFile + "\ncat > /dev/null\n"
	if fail {
		sh += "printf '%s' " + tsShQuote(body) + " >&2\nexit 2\n"
	} else {
		sh += "printf '%s' " + tsShQuote(body) + "\n"
	}
	if err := os.WriteFile(script, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldLook := prettierBin, prettierLookPath
	prettierBin = script
	prettierLookPath = exec.LookPath
	t.Cleanup(func() { prettierBin, prettierLookPath = old, oldLook })
	return argsFile
}

func tsShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// The optional capability has to be visible through the interface, or the CLI
// never calls it and the whole pass is dead code.
func TestBackendImplementsFormatter(t *testing.T) {
	var b generator.Backend = &Backend{}
	if _, ok := b.(generator.Formatter); !ok {
		t.Fatal("typescript Backend no longer satisfies generator.Formatter; the CLI will write unformatted TypeScript")
	}
}

func TestFormatRunsPrettierOverEveryTypeScriptFile(t *testing.T) {
	args := stubPrettier(t, "FORMATTED\n", false)
	in := []generator.File{
		{Path: "message.ts", Content: []byte("const x   =1")},
		// package.json and README.md are prettier-formattable too, but they come
		// out of the emitter already clean (project.go), so no process is spawned
		// for them.
		{Path: "package.json", Content: []byte("{}\n")},
		{Path: "README.md", Content: []byte("# gen\n")},
		{Path: "harness.ts", Content: []byte("const y   =2")},
	}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if note != "" {
		t.Fatalf("unexpected note with a working formatter: %q", note)
	}
	if string(out[0].Content) != "FORMATTED\n" || string(out[3].Content) != "FORMATTED\n" {
		t.Errorf("a .ts file was not replaced by the formatter's output: %q / %q", out[0].Content, out[3].Content)
	}
	if string(out[1].Content) != "{}\n" || string(out[2].Content) != "# gen\n" {
		t.Errorf("a non-.ts file went through prettier: %q / %q", out[1].Content, out[2].Content)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	// --stdin-filepath is what picks the TypeScript parser and what resolves the
	// user's .prettierrc/.prettierignore for that path; without it prettier
	// cannot even tell which language it was handed.
	for _, want := range []string{"--stdin-filepath\n", "message.ts\n", "harness.ts\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing prettier argument %q in %q", want, got)
		}
	}
	if n := strings.Count(string(got), "--stdin-filepath\n"); n != 2 {
		t.Errorf("prettier ran %d time(s), want 2: %q", n, got)
	}
}

// Nothing to format means nothing to run: a backend must not spawn a process
// for a file set with no TypeScript in it.
func TestFormatSkipsWhenNoTypeScriptFile(t *testing.T) {
	args := stubPrettier(t, "FORMATTED\n", false)
	in := []generator.File{{Path: "README.md", Content: []byte("# gen\n")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil || note != "" {
		t.Fatalf("Format: err=%v note=%q", err, note)
	}
	if string(out[0].Content) != "# gen\n" {
		t.Errorf("content changed: %q", out[0].Content)
	}
	if _, err := os.Stat(args); err == nil {
		t.Error("prettier was invoked for a file set with no TypeScript in it")
	}
}

// A JavaScript project pins prettier as a devDependency, so the formatter that
// answers for the generated tree must be the tree's own -- not whatever version
// happens to be installed globally on the machine that runs sofabgen.
func TestFormatPrefersTheProjectsOwnPrettier(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, localPrettier)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat > /dev/null\nprintf 'LOCAL\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A global prettier that would answer differently, so the test can tell the
	// two apart rather than just observing that something ran.
	stubPrettier(t, "GLOBAL\n", false)

	out, note, err := (&Backend{}).Format([]generator.File{{Path: "message.ts", Content: []byte("x")}}, dir)
	if err != nil || note != "" {
		t.Fatalf("Format: err=%v note=%q", err, note)
	}
	if string(out[0].Content) != "LOCAL\n" {
		t.Errorf("the project's own node_modules/.bin/prettier was not preferred: %q", out[0].Content)
	}
}

// A box without prettier still generates -- and, as with ruff, this is the
// common case: prettier is not part of the TypeScript toolchain. The files come
// back untouched with a note.
func TestFormatWithoutPrettierIsNotAnError(t *testing.T) {
	old, oldLook := prettierBin, prettierLookPath
	prettierBin = "prettier"
	prettierLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { prettierBin, prettierLookPath = old, oldLook })

	in := []generator.File{{Path: "message.ts", Content: []byte("const x   =1")}}
	out, note, err := (&Backend{}).Format(in, t.TempDir())
	if err != nil {
		t.Fatalf("a missing prettier must not fail generation: %v", err)
	}
	if string(out[0].Content) != "const x   =1" {
		t.Errorf("content changed without a formatter: %q", out[0].Content)
	}
	if !strings.Contains(note, "prettier") {
		t.Errorf("no note naming prettier: %q", note)
	}
}

// A prettier that RUNS and refuses is the opposite case: it means the emitter
// produced TypeScript that does not parse, and that must not reach a file
// silently (the very fallback #579 called out in the Go backend).
func TestFormatSurfacesARefusal(t *testing.T) {
	stubPrettier(t, "[error] message.ts: SyntaxError: Unexpected token (1:7)", true)
	in := []generator.File{
		{Path: "README.md", Content: []byte("x")},
		{Path: "message.ts", Content: []byte("class {")},
	}
	out, _, err := (&Backend{}).Format(in, t.TempDir())
	if err == nil {
		t.Fatal("a prettier failure was swallowed")
	}
	if out != nil {
		t.Error("files were returned alongside the error; the caller could write them")
	}
	if !strings.Contains(err.Error(), "message.ts") {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "SyntaxError") {
		t.Errorf("prettier's own diagnosis was dropped: %v", err)
	}
}

// The real thing, when the box has it: the generated example must come back
// prettier-clean, and formatting must be idempotent (a second pass is a no-op).
// The corpus-wide gate is tests/conformance/typescript/run.sh; this is the fast
// one.
func TestFormatRealPrettierIsCleanAndIdempotent(t *testing.T) {
	if _, err := exec.LookPath("prettier"); err != nil {
		t.Skip("prettier not installed; tests/conformance/typescript/run.sh is the gate")
	}
	files, err := (&Backend{}).Generate(exampleSchema(t), map[string]any{"emit": "project"})
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
			t.Errorf("%s: prettier is not a fixed point on generated output", once[i].Path)
		}
	}
}

// The files the emitters own outright: package.json, tsconfig.json and
// README.md are written prettier-clean (project.go), so a user's
// `prettier --check .` stays quiet about them even at --format=off, where no
// formatter runs at all. Unlike the .ts files, nothing reformats these after the
// fact -- this test is the only thing holding them to it.
func TestNonTypeScriptProjectFilesAreEmittedPrettierClean(t *testing.T) {
	bin, err := exec.LookPath("prettier")
	if err != nil {
		t.Skip("prettier not installed; tests/conformance/typescript/run.sh is the gate")
	}
	files, err := (&Backend{}).Generate(exampleSchema(t), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".ts") {
			continue
		}
		seen++
		got, err := runPrettier(bin, t.TempDir(), f.Path, f.Content)
		if err != nil {
			t.Errorf("%s: prettier refused it: %v", f.Path, err)
			continue
		}
		if string(got) != string(f.Content) {
			t.Errorf("%s is not emitted prettier-clean; prettier wants:\n%s", f.Path, got)
		}
	}
	if seen == 0 {
		t.Fatal("no non-TypeScript project file was checked; a check over nothing proves nothing")
	}
}

func exampleSchema(t *testing.T) *ir.Schema {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return schema(t, string(b))
}
