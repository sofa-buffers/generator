package dart

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
)

// LanguageVersion is the Dart language version the Format pass falls back to
// when nothing in reach declares one. It is NOT a free choice: `dart format`
// picks its style from the language version of the package the file belongs to
// — short style below 3.7, the tall style from 3.7 on — so formatting against a
// different version than the package declares produces a tree the user's own
// `dart format` immediately rewrites. It therefore tracks the `sdk:` constraint
// in `pubspec` (project.go), and format_test.go pins the two together.
//
// It is only the FALLBACK, because the package is usually knowable: see
// languageVersion below.
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
	lv := languageVersion(files, dir)
	out := make([]generator.File, len(files))
	copy(out, files)
	for _, i := range idx {
		content, err := runDartFormat(bin, dir, lv, out[i].Path, out[i].Content)
		if err != nil {
			return nil, "", fmt.Errorf("dart format refused generated %s: %w", out[i].Path, err)
		}
		out[i] = generator.File{Path: out[i].Path, Content: content}
	}
	return out, "", nil
}

// languageVersion is the Dart language version this run formats against: the
// `sdk:` lower bound of the package the generated files are going INTO.
//
// Getting it from the package rather than from a constant is the whole point.
// `dart format` chooses short style below 3.7 and the tall style from 3.7 on,
// so a constant 3.4 formats short — and in the DEFAULT emit mode (`sources`,
// no generated pubspec) those files land in the user's own package, which today
// is almost always ≥3.7. Their own `dart format` then rewrites every one of
// them: exactly the rewrite this pass exists to spare them, and silently, even
// under --format=require.
//
// Two places can declare it, in this order:
//
//	the generated pubspec, when the run scaffolds a project (`emit: project`).
//	It is not on disk yet — it is one of the files being written — so it is
//	read out of `files`.
//	the pubspec of the package `dir` sits in, walking up. That is the package
//	the files join under `emit: sources`, and its constraint is what the user's
//	own `dart format` over that tree resolves.
//
// With neither — generating into a bare directory that is not a package yet —
// there is nothing to match, and LanguageVersion is the documented fallback.
func languageVersion(files []generator.File, dir string) string {
	for _, f := range files {
		if filepath.Base(f.Path) == "pubspec.yaml" {
			if v := sdkLowerBound(f.Content); v != "" {
				return v
			}
		}
	}
	for d := dir; d != ""; {
		if b, err := os.ReadFile(filepath.Join(d, "pubspec.yaml")); err == nil {
			if v := sdkLowerBound(b); v != "" {
				return v
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return LanguageVersion
}

// sdkLowerBound pulls `major.minor` out of a pubspec's SDK constraint —
// `sdk: ^3.8.0`, `sdk: ">=3.8.0 <4.0.0"`, `sdk: '3.8.0'` all give "3.8". The
// LOWER bound, because that is the language version the package's code is
// written against, and it is what `dart format` itself resolves. An
// unparsable or absent constraint gives "", and the caller falls back.
func sdkLowerBound(pubspec []byte) string {
	for _, line := range strings.Split(string(pubspec), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "sdk:") {
			continue
		}
		return firstVersion(strings.TrimPrefix(t, "sdk:"))
	}
	return ""
}

// firstVersion reads the first `<digits>.<digits>` in s, ignoring whatever
// range syntax surrounds it.
func firstVersion(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j >= len(s) || s[j] != '.' {
			return ""
		}
		k := j + 1
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k == j+1 {
			return ""
		}
		return s[i:k]
	}
	return ""
}

// runDartFormat pipes one source file through `dart format` and returns what
// comes back. stdin/stdout rather than formatting in place: the files are not
// on disk yet, and a formatter must never be the reason a half-written tree is
// left behind.
//
// --language-version is passed explicitly rather than left to `dart format`'s
// own package resolution, which reads `.dart_tool/package_config.json` — a file
// `dart pub get` writes AFTER generation, and which is therefore usually absent
// exactly when this runs. Without it dart format falls back to the SDK's latest
// version, whatever the surrounding package asks for. The value is resolved by
// languageVersion above, from that package.
func runDartFormat(bin, dir, lv, path string, src []byte) ([]byte, error) {
	cmd := exec.Command(bin, "format",
		"--output=show", "--summary=none",
		"--language-version="+lv,
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
