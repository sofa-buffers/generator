package python

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
)

// Swappable for the tests: they point these at a stub binary rather than at a
// real ruff, so the pass is testable on a box without one.
var (
	ruffBin      = "ruff"
	ruffLookPath = exec.LookPath
)

// Format runs `ruff format` over every generated .py file
// (generator.Formatter).
//
// Emitting ruff-format-clean Python from the emitters instead was measured and
// rejected. Over the example plus the corpus, `ruff format` rewrites all 27
// generated modules (~6.3k diff lines); a large part of that is mechanical and
// could be emitted (blank lines around definitions, one-line `if cond: stmt`
// bodies, the `from sofab import ...` line), but the rest is width-driven and
// the width comes from SCHEMA content. A probe schema with long field names
// makes ruff split a dataclass field default, a `def` whose return annotation
// no longer fits, a dict literal and a comparison that shorter names keep on one
// line. Reproducing that in the emitter means reproducing the Black line
// breaker, and anything less is a guarantee that holds for the corpus and
// quietly fails on a user's schema (the same conclusion rustfmt forced in
// generators/rust/format.go).
//
// ruff is not part of the Python toolchain the way rustfmt is part of rustup,
// so this pass is the one that most often finds no formatter — and that is
// exactly why it degrades instead of failing: a missing ruff returns the files
// unformatted with a note. It is also why the missing case is not a real loss.
// The users this matters to are the ones running `ruff format --check` over
// their tree, generated files included; they have ruff. For anyone else the
// generated module is as usable unformatted.
//
// Nothing here runs unless the caller asked for it: the CLI spawns ruff only
// under --format=auto or --format=require, and the default is off.
func (*Backend) Format(files []generator.File, dir string) ([]generator.File, string, error) {
	var idx []int
	for i, f := range files {
		if strings.HasSuffix(f.Path, ".py") {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return files, "", nil
	}
	bin, err := ruffLookPath(ruffBin)
	if err != nil {
		return files, "ruff not found in PATH", nil
	}
	// ruff resolves pyproject.toml/ruff.toml from the file's own directory
	// upwards, so running it in the output dir under the file's real relative
	// path is what makes the generated files come out the way the user's own
	// `ruff format` over that tree would, house settings included.
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			dir = ""
		}
	}
	out := make([]generator.File, len(files))
	copy(out, files)
	for _, i := range idx {
		content, err := runRuffFormat(bin, dir, out[i].Path, out[i].Content)
		if err != nil {
			return nil, "", fmt.Errorf("ruff format refused generated %s: %w", out[i].Path, err)
		}
		out[i] = generator.File{Path: out[i].Path, Content: content}
	}
	return out, "", nil
}

// runRuffFormat pipes one module through `ruff format` and returns what comes
// back. stdin/stdout rather than formatting in place: the files are not on disk
// yet, and a formatter must never be the reason a half-written tree is left
// behind.
func runRuffFormat(bin, dir, path string, src []byte) ([]byte, error) {
	cmd := exec.Command(bin, "format", "--stdin-filename="+path, "-")
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
