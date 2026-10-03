package zig

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestZigFloatScalarComparesBits(t *testing.T) {
	g := &gen{}
	for _, c := range []struct {
		f    *ir.Field
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "@as(u32, @bitCast(x)) != 0x0"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "@as(u32, @bitCast(x)) != 0x3fc00000"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "@as(u64, @bitCast(x)) != 0x3ff8000000000000"},
	} {
		if got := g.zigLeafNe("x", c.f); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
}
