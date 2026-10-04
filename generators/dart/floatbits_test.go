package dart

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar at a zero default is compared by bit pattern: the sign is read
// with isNegative (-0.0 is not the default 0). A non-zero default keeps `!=`.
func TestDartFloatDiffersComparesSign(t *testing.T) {
	g := &gen{}
	for _, c := range []struct {
		f    *ir.Field
		want string
	}{
		{&ir.Field{Kind: ir.KindFP64}, "x != 0.0 || x.isNegative"},
		{&ir.Field{Kind: ir.KindFP64, Default: 0.0}, "x != 0.0 || x.isNegative"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "x != 1.5"},
	} {
		if got := g.floatDiffers(c.f, "x"); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
}
