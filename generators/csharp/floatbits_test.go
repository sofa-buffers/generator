package csharp

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestCSharpFloatScalarComparesBits(t *testing.T) {
	for _, c := range []struct {
		f    *ir.Field
		op   string
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "!=", "global::System.BitConverter.SingleToInt32Bits(x) != 0"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "==", "global::System.BitConverter.SingleToInt32Bits(x) == 1069547520"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "!=", "global::System.BitConverter.DoubleToInt64Bits(x) != 4609434218613702656L"},
	} {
		if got := floatBitsCmp(c.f, "x", c.op); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
}
