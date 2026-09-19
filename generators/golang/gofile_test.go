package golang

import (
	"errors"
	"go/format"
	"strings"
	"testing"
)

// Source go/format cannot parse is an error from gofile.bytes, never the raw
// text: emitting it would hand the user a file that fails `gofmt -l`.
func TestGoFileUnparsableIsAnError(t *testing.T) {
	f := newGoFile("message")
	f.line("func broken( {")
	out, err := f.bytes("sofabgen", "")
	if err == nil {
		t.Fatalf("unparsable body formatted without an error:\n%s", out)
	}
	if out != nil {
		t.Fatalf("an error must come with no output, got:\n%s", out)
	}
}

// ...and Generate returns that error, naming the file, instead of any files.
// formatSource is swapped so the failure is driven through a real schema; the
// real go/format never rejects what the backend emits today.
func TestGoGenerateSurfacesFormatFailure(t *testing.T) {
	boom := errors.New("simulated go/format failure")
	formatSource = func([]byte) ([]byte, error) { return nil, boom }
	defer func() { formatSource = format.Source }()

	for _, emit := range []string{"sources", "project"} {
		files, err := (&Backend{}).Generate(exampleSchema(t), map[string]any{"emit": emit})
		if !errors.Is(err, boom) {
			t.Fatalf("emit=%s: Generate err = %v, want the format failure", emit, err)
		}
		if !strings.Contains(err.Error(), ".go") {
			t.Errorf("emit=%s: error does not name the file: %v", emit, err)
		}
		if files != nil {
			t.Errorf("emit=%s: Generate returned %d files alongside the error", emit, len(files))
		}
	}
}
