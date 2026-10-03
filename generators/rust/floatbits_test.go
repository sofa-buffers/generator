package rust

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestRustFloatScalarComparesBits(t *testing.T) {
	g := &gen{}
	for _, c := range []struct {
		f    *ir.Field
		acc  string
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "self.f", "self.f.to_bits() != 0x0"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "self.f", "self.f.to_bits() != 0x3fc00000"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "self.d", "self.d.to_bits() != 0x3ff8000000000000"},
		{&ir.Field{Kind: ir.KindFP32}, "*v", "(*v).to_bits() != 0x0"},
	} {
		if got := g.rustLeafNe(c.acc, c.f); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
}
