package cpp

import "github.com/sofa-buffers/generator/internal/ir"

// The worst-case ENCODE size of a message is computed by ir.MaxWireSize, shared
// by every backend (internal/ir/wiresize.go) and reached here through
// generator.SizePolicy. This file holds only the other question C++ asks of the
// same walk: how large a single field can get on DECODE. That is a different
// question, and the one place the receiver-side max_dyn_* caps legitimately
// apply — they bound what this peer accepts, which is exactly what a decode
// window must accommodate.

// maxFieldSpan returns the largest worst-case byte span of a single top-level
// field across every message, with the configured max_dyn_* caps substituted for
// missing schema bounds. ok is false when some field stays unbounded even with
// the caps applied.
func (g *gen) maxFieldSpan(s *ir.Schema) (int64, bool) {
	caps := &ir.DynCaps{
		ArrayCount: g.limArr, HasArray: g.limArrHas,
		StringLen: g.limStr, HasString: g.limStrHas,
		BlobLen: g.limBlob, HasBlob: g.limBlobHas,
	}
	var worst int64
	for _, m := range s.Messages {
		for _, f := range m.Fields {
			c, ok := ir.MaxFieldDecodeSpan(f, map[string]bool{}, caps)
			if !ok {
				return 0, false
			}
			worst = max(worst, c)
		}
	}
	return worst, worst > 0
}
