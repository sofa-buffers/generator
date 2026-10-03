package golang

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestGoFloatScalarComparesBits(t *testing.T) {
	g := &gen{}
	f := newGoFile("p")
	for _, c := range []struct {
		f    *ir.Field
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "math.Float32bits(x) != 0x0"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "math.Float32bits(x) != 0x3fc00000"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "math.Float64bits(x) != 0x3ff8000000000000"},
		// -0 is the Go constant 0, so the member is +0.0 and so are the bits.
		{&ir.Field{Kind: ir.KindFP64, Default: -0.0 * -1}, "math.Float64bits(x) != 0x0"},
	} {
		if got := g.floatBitsCmp(f, c.f, "x", "!="); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
	if got := g.fieldIsDefaultExprAt(f, &ir.Field{Kind: ir.KindFP32}, "x"); got != "math.Float32bits(x) == 0x0" {
		t.Errorf("isDefault: got %q", got)
	}
}
