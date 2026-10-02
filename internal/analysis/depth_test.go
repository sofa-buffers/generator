package analysis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/parser"
	"github.com/sofa-buffers/generator/internal/testschema"
)

// TestSequenceDepthBoundary: for every nesting shape the cap sits where the wire
// opens its 255th sequence -- the last accepted N passes, N+1 is rejected with
// the real sequence count and the field path.
func TestSequenceDepthBoundary(t *testing.T) {
	for _, shape := range testschema.Shapes {
		b := testschema.Boundary[shape]
		t.Run(shape, func(t *testing.T) {
			s := buildSchema(t, testschema.Deep(shape, b.Last))
			if err := Analyze(s); err != nil {
				t.Fatalf("N=%d must be accepted: %v", b.Last, err)
			}
			d, ok := ir.SeqDepth(s.Messages[0].Fields, map[string]bool{})
			if !ok || d != b.Depth {
				t.Fatalf("N=%d: SeqDepth = %d (ok=%v), want %d", b.Last, d, ok, b.Depth)
			}

			s = buildSchema(t, testschema.Deep(shape, b.Last+1))
			err := Analyze(s)
			if err == nil {
				t.Fatalf("N=%d must be rejected", b.Last+1)
			}
			d, _ = ir.SeqDepth(s.Messages[0].Fields, map[string]bool{})
			if want := fmt.Sprintf("sequence depth %d exceeds MAX_DEPTH (%d)", d, ir.MaxSeqDepth); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
			if !strings.Contains(err.Error(), "messages/M/a/a") {
				t.Errorf("error %q does not name the field path", err)
			}
		})
	}
}

// TestSequenceDepthCircularStillRejected: a recursive $ref never reaches the
// depth walk -- the parser refuses it as circular.
func TestSequenceDepthCircularStillRejected(t *testing.T) {
	src := `version: 1
$defs:
  struct:
    Node:
      next: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Node' } }
messages:
  M:
    payload:
      n: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Node' } }
`
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err == nil {
		_, err = doc.Resolve()
	}
	if err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("recursive $ref must be rejected as circular, got %v", err)
	}
}
