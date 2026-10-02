package c

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/testschema"
)

var decSlots = regexp.MustCompile(`sofab_object_decoder_t dec\[(\d+)\];`)

// TestDecoderSlotsAtSequenceCap: the decoder stack holds one slot per open
// sequence plus the root, and the 8-bit depth seeded from it
// ((uint8_t)(slots-1)) must not wrap -- so for the deepest schema analysis
// accepts the slot count is at most 256.
func TestDecoderSlotsAtSequenceCap(t *testing.T) {
	for _, shape := range testschema.Shapes {
		b := testschema.Boundary[shape]
		t.Run(shape, func(t *testing.T) {
			files := genCFromYAML(t, testschema.Deep(shape, b.Last))
			for path, src := range files {
				if !strings.HasSuffix(path, ".h") {
					continue
				}
				m := decSlots.FindStringSubmatch(src)
				if m == nil {
					continue
				}
				slots, _ := strconv.Atoi(m[1])
				if slots != b.Depth+1 || slots > ir.MaxSeqDepth+1 {
					t.Fatalf("%s: dec[%d], want dec[%d] (<= %d)", path, slots, b.Depth+1, ir.MaxSeqDepth+1)
				}
				return
			}
			t.Fatal(fmt.Sprint("no decoder slot array emitted for ", shape))
		})
	}
}
