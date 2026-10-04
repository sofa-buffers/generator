package typescript

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar at a zero default is compared by bit pattern: the sign of the
// zero is read from 1 / x, which only runs for a zero (-0 === 0 is true). A
// non-zero default keeps the plain compare.
func TestTSFloatZeroDefaultComparesSign(t *testing.T) {
	g := &gen{}
	for _, c := range []struct {
		f       *ir.Field
		differs bool
		want    string
		ok      bool
	}{
		{&ir.Field{Kind: ir.KindFP32}, true, "x !== 0 || 1 / x < 0", true},
		{&ir.Field{Kind: ir.KindFP64}, false, "x === 0 && 1 / x > 0", true},
		{&ir.Field{Kind: ir.KindFP64, Default: 0.0}, true, "x !== 0 || 1 / x < 0", true},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, true, "", false},
		{&ir.Field{Kind: ir.KindU32}, true, "", false},
	} {
		got, ok := g.floatZeroCmp(c.f, "x", c.differs)
		if got != c.want || ok != c.ok {
			t.Errorf("%v default %v: got %q,%v want %q,%v", c.f.Kind, c.f.Default, got, ok, c.want, c.ok)
		}
	}
}
