package golang

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/testschema"
)

// TestMaxDepthMatchesSequenceCap: at the last schema the analysis accepts for
// each nesting shape, the emitted <Msg>__MaxDepth is the analysis' own count
// (never skipped for exceeding the wire cap).
func TestMaxDepthMatchesSequenceCap(t *testing.T) {
	for _, shape := range testschema.Shapes {
		b := testschema.Boundary[shape]
		t.Run(shape, func(t *testing.T) {
			s := schemaFromYAMLString(t, testschema.Deep(shape, b.Last))
			if d, ok := ir.SeqDepth(s.Messages[0].Fields, map[string]bool{}); !ok || d != b.Depth || d > ir.MaxSeqDepth {
				t.Fatalf("SeqDepth = %d (ok=%v), want %d", d, ok, b.Depth)
			}
			want := fmt.Sprintf("const M__MaxDepth = %d\n", b.Depth)
			for _, src := range genGo(t, s, map[string]any{"package": "messages"}) {
				if strings.Contains(src, want) {
					return
				}
			}
			t.Fatalf("no %q emitted", strings.TrimSpace(want))
		})
	}
}
