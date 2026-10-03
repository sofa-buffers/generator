package kotlin

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestKotlinFloatScalarComparesBits(t *testing.T) {
	for _, c := range []struct {
		f    *ir.Field
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "x.toRawBits() != 0"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "x.toRawBits() != 1069547520"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "x.toRawBits() != 4609434218613702656L"},
	} {
		if got := floatBitsNe(c.f, "x"); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
}
