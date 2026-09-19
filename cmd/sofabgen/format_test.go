package main

import (
	"errors"
	"os"
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
	files, err := formatFiles("testfmtlang", out, []generator.File{{Path: "a.txt", Content: []byte("raw")}}, os.Stderr, &noted)
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
	files, err := formatFiles("go", t.TempDir(), in, os.Stderr, &noted)
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
	b.note = "note: rustfmt not found"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	in := []generator.File{{Path: "a.txt", Content: []byte("raw")}}
	for i := 0; i < 3; i++ {
		if _, err := formatFiles("testfmtlang", t.TempDir(), in, w, &noted); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if n := strings.Count(sb.String(), "note: rustfmt not found"); n != 1 {
		t.Errorf("note printed %d times over three definitions, want 1: %q", n, sb.String())
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
	files, err := formatFiles("testfmtlang", t.TempDir(), in, os.Stderr, &noted)
	if err == nil {
		t.Fatal("a formatter failure was swallowed")
	}
	if files != nil {
		t.Error("files were handed back for writing after the formatter failed")
	}
}
