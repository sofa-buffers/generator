package python

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar at a zero default is compared by bit pattern: the sign of the
// zero is read with math.copysign (-0.0 is not the default 0). A non-zero default
// keeps the plain compare.
func TestPyFloatZeroDefaultComparesSign(t *testing.T) {
	for _, c := range []struct {
		f       *ir.Field
		differs bool
		want    string
		ok      bool
	}{
		{&ir.Field{Kind: ir.KindFP32}, true, "x != 0.0 or math.copysign(1.0, x) < 0.0", true},
		{&ir.Field{Kind: ir.KindFP64}, false, "x == 0.0 and math.copysign(1.0, x) > 0.0", true},
		{&ir.Field{Kind: ir.KindFP64, Default: 0.0}, true, "x != 0.0 or math.copysign(1.0, x) < 0.0", true},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, true, "", false},
		{&ir.Field{Kind: ir.KindU32}, true, "", false},
	} {
		got, ok := floatZeroCmp(c.f, "x", c.differs)
		if got != c.want || ok != c.ok {
			t.Errorf("%v default %v: got %q,%v want %q,%v", c.f.Kind, c.f.Default, got, ok, c.want, c.ok)
		}
	}
}
