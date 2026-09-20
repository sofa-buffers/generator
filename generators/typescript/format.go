package typescript

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

// Swappable for the tests: they point these at a stub binary rather than at a
// real prettier, so the pass is testable on a box without one.
var (
	prettierBin      = "prettier"
	prettierLookPath = exec.LookPath
)

// localPrettier is where a JavaScript project keeps its own formatter. npm
// installs a package's binaries there, so this is the prettier a user's
// `npx prettier` or their package script resolves -- and, unlike anything on
// PATH, it is the version their own CI pins. It is tried first for that reason.
const localPrettier = "node_modules/.bin/prettier"

// Format runs prettier over every generated .ts file (generator.Formatter).
//
// Emitting prettier-clean TypeScript from the emitters instead was measured and
// rejected. Over the example plus the corpus, prettier rewrites all 54 generated
// .ts files -- 12.6k changed lines against 15.3k lines of source, i.e. most of
// the output. Part of that is mechanical (the import list, quoted object keys in
// toJSON, `if (!(x.isDefault()))`, one-line case arms), but the rest is
// width-driven and the width comes from SCHEMA content. A probe schema that
// differs from another only in the LENGTH of its message, field and type names
// makes prettier break a `static fromJSON(d: Record<string, unknown>): T`
// signature, a field initialiser after its `=`, a `new Uint8Array(T.MAX_SIZE)`
// call and an `if (...) return false;` body that the short-named twin keeps on
// one line each. Reproducing that in the emitter means reproducing prettier's
// line breaker, and anything less is a guarantee that holds for the corpus and
// quietly fails on a user's schema (the same conclusion rustfmt forced in
// generators/rust/format.go, and ruff in generators/python/format.go).
//
// What the emitters DO own is everything prettier does not have to be run to
// get right: the generated package.json, tsconfig.json and README.md come out
// prettier-clean from the emitter, at --format=off as much as anywhere else, so
// a user running `prettier --check .` over their tree only ever sees the .ts
// files this pass handles.
//
// prettier is not part of the TypeScript toolchain -- having node, npm and tsc
// says nothing about having it -- so, like ruff, this pass degrades instead of
// failing: a missing prettier returns the files unformatted with a note. That is
// not much of a loss. The users it matters to are the ones running
// `prettier --check` over their tree, generated files included; they have it.
//
// Nothing here runs unless the caller asked for it: the CLI spawns prettier only
// under --format=auto or --format=require, and the default is off.
func (*Backend) Format(files []generator.File, dir string) ([]generator.File, string, error) {
	var idx []int
	for i, f := range files {
		if strings.HasSuffix(f.Path, ".ts") {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return files, "", nil
	}
	// prettier resolves .prettierrc, .prettierignore and .editorconfig from the
	// file's own directory upwards, so running it in the output dir under the
	// file's real relative path is what makes the generated files come out the
	// way the user's own `prettier --write` over that tree would, house settings
	// included.
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			dir = ""
		}
	}
	bin, err := findPrettier(dir)
	if err != nil {
		return files, "prettier not found in " + filepath.Join(dir, localPrettier) + " or in PATH", nil
	}
	out := make([]generator.File, len(files))
	copy(out, files)
	for _, i := range idx {
		content, err := runPrettier(bin, dir, out[i].Path, out[i].Content)
		if err != nil {
			return nil, "", fmt.Errorf("prettier refused generated %s: %w", out[i].Path, err)
		}
		out[i] = generator.File{Path: out[i].Path, Content: content}
	}
	return out, "", nil
}

// findPrettier prefers the output tree's own node_modules/.bin/prettier over
// anything on PATH: a JavaScript project pins its formatter as a devDependency,
// and that pin is the one its CI checks against. A globally installed prettier
// is the fallback.
func findPrettier(dir string) (string, error) {
	if dir != "" {
		local := filepath.Join(dir, localPrettier)
		if st, err := os.Stat(local); err == nil && !st.IsDir() {
			// An absolute path, because exec resolves a relative one against the
			// PROCESS's working directory, not against cmd.Dir.
			if abs, err := filepath.Abs(local); err == nil {
				return abs, nil
			}
			return local, nil
		}
	}
	return prettierLookPath(prettierBin)
}

// runPrettier pipes one file through prettier and returns what comes back.
// stdin/stdout rather than formatting in place: the files are not on disk yet,
// and a formatter must never be the reason a half-written tree is left behind.
//
// --stdin-filepath is not cosmetic. It is how prettier picks the parser (a .ts
// file is not parsed like a .js one) and how it resolves the configuration and
// the ignore rules that apply to that path.
func runPrettier(bin, dir, path string, src []byte) ([]byte, error) {
	cmd := exec.Command(bin, "--stdin-filepath", path)
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
