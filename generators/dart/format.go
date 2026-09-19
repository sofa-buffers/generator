package dart

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
)

// LanguageVersion is the Dart language version the Format pass below formats
// against. It is NOT a free choice: `dart format` picks its style from the
// language version of the package the file belongs to — short style below 3.7,
// the tall style from 3.7 on — so formatting against a different version than
// the generated pubspec declares produces a tree the user's own `dart format`
// immediately rewrites. It therefore tracks the `sdk:` constraint in `pubspec`
// (project.go), and format_test.go pins the two together.
const LanguageVersion = "3.4"

// Swappable for the tests: they point these at a stub binary rather than at a
// real Dart SDK, so the pass is testable on a box with no Dart toolchain.
var (
	dartBin      = "dart"
	dartLookPath = exec.LookPath
)

// Format runs `dart format` over every generated .dart file
// (generator.Formatter).
//
// Emitting dart-format-clean Dart from the emitters instead was measured and
// rejected: over the example plus the corpus, `dart format` rewrites all 54
// generated files, ~9k diff lines in the short style and ~10k in the tall one,
// and the rewrites are width-driven in both — a long enough field name, type
// name or default value is what decides whether an argument list, a condition
// or a cascade stays on one line. A probe schema with long names moves the
// splits, so reproducing them in the emitter means reproducing dart_style, and
// anything less is a guarantee that holds for the corpus and quietly fails on a
// user's schema.
//
// `dart format` is part of the Dart SDK: anyone who can BUILD the generated
// code already has it, which is the condition #579 put on invoking a formatter
// from the generator. Anyone who does not is not blocked either — a missing
// `dart` returns the files unformatted with a note, never an error.
//
// Nothing here runs unless the caller asked for it: the CLI spawns `dart format`
// only under --format=auto or --format=require, and the default is off.
func (*Backend) Format(files []generator.File, dir string) ([]generator.File, string, error) {
	var idx []int
	for i, f := range files {
		if strings.HasSuffix(f.Path, ".dart") {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return files, "", nil
	}
	bin, err := dartLookPath(dartBin)
	if err != nil {
		return files, "dart not found in PATH", nil
	}
	// The formatter runs in the output dir, the way the user's own `dart format`
	// over that tree would.
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			dir = ""
		}
	}
	out := make([]generator.File, len(files))
	copy(out, files)
	for _, i := range idx {
		content, err := runDartFormat(bin, dir, out[i].Path, out[i].Content)
		if err != nil {
			return nil, "", fmt.Errorf("dart format refused generated %s: %w", out[i].Path, err)
		}
		out[i] = generator.File{Path: out[i].Path, Content: content}
	}
	return out, "", nil
}

// runDartFormat pipes one source file through `dart format` and returns what
// comes back. stdin/stdout rather than formatting in place: the files are not
// on disk yet, and a formatter must never be the reason a half-written tree is
// left behind.
//
// --language-version is passed explicitly rather than left to the surrounding
// package config, which does not exist yet at generation time (it is written by
// `dart pub get`, after this) and would silently fall back to the SDK's latest
// version — a different style from the one the generated pubspec asks for.
func runDartFormat(bin, dir, path string, src []byte) ([]byte, error) {
	cmd := exec.Command(bin, "format",
		"--output=show", "--summary=none",
		"--language-version="+LanguageVersion,
		// Only used for diagnostics: with --language-version given, dart format
		// does not go looking for a surrounding package.
		"--stdin-name="+path)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(src)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}
