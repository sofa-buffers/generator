package rust

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
)

// Edition is the Rust edition every generated crate declares (projectFiles'
// Cargo.toml). rustfmt parses per edition, so the Format pass below has to be
// told the same one; project_test.go pins the two together.
const Edition = "2021"

// Swappable for the tests: they point these at a stub binary rather than at a
// real rustfmt, so the pass is testable on a box with no Rust toolchain.
var (
	rustfmtBin = "rustfmt"
	lookPath   = exec.LookPath
)

// Format runs rustfmt over every generated .rs file (generator.Formatter).
//
// Unlike Go, where go/format is a library call inside the backend, Rust's
// canonical formatter is a program. Emitting rustfmt-clean Rust from the
// emitters instead was measured and rejected: rustfmt's line breaking is
// width-driven (max_width, fn_call_width, struct_lit_width, chain_width), and
// the widths are set by SCHEMA content — a long enough field name or default
// value makes rustfmt break a condition, a field-access chain or a struct
// literal that a shorter one keeps on a single line. Reproducing that in the
// emitter means reproducing rustfmt, and anything less is a guarantee that
// holds for the corpus and quietly fails on a user's schema.
//
// rustfmt ships with every rustup toolchain, so anyone who can BUILD the
// generated crate already has it. Anyone who cannot is not blocked: a missing
// rustfmt returns the files unformatted with a note, never an error.
//
// Nothing here runs unless the caller asked for it: the CLI spawns rustfmt only
// under --format=auto or --format=require, and the default is off.
func (*Backend) Format(files []generator.File, dir string) ([]generator.File, string, error) {
	var idx []int
	for i, f := range files {
		if strings.HasSuffix(f.Path, ".rs") {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return files, "", nil
	}
	bin, err := lookPath(rustfmtBin)
	if err != nil {
		return files, "rustfmt not found in PATH", nil
	}
	// rustfmt resolves rustfmt.toml from its working directory upwards. Running
	// it in the output dir is what makes the generated files come out the way the
	// user's own `cargo fmt` over that tree would, house style included.
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			dir = ""
		}
	}
	out := make([]generator.File, len(files))
	copy(out, files)
	for _, i := range idx {
		content, err := runRustfmt(bin, dir, out[i].Content)
		if err != nil {
			return nil, "", fmt.Errorf("rustfmt refused generated %s: %w", out[i].Path, err)
		}
		out[i] = generator.File{Path: out[i].Path, Content: content}
	}
	return out, "", nil
}

// runRustfmt pipes one source file through rustfmt and returns what comes back.
// stdin/stdout rather than formatting in place: the files are not on disk yet,
// and a formatter must never be the reason a half-written tree is left behind.
func runRustfmt(bin, dir string, src []byte) ([]byte, error) {
	cmd := exec.Command(bin, "--edition", Edition, "--emit", "stdout", "--quiet")
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
