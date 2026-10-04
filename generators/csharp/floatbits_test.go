package csharp

import (
	"strings"
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

// A float array is compared with its default through the corelib's FloatBits, in
// the write guard and in IsDefault, and no IEEE-based array compare is left for it;
// an integer array keeps the element-wise compare.
func TestCSharpFloatArrayComparesBitsThroughCorelib(t *testing.T) {
	src := `version: 1
messages:
  M:
    payload:
      a: { id: 0, type: array, items: { type: fp32, count: 2 }, default: [0.0, 1.5] }
      d: { id: 1, type: array, items: { type: fp64, count: 2 }, default: [0.0, 1.5] }
      n: { id: 2, type: array, items: { type: u16, count: 2 }, default: [1, 2] }
`
	var all strings.Builder
	for _, c := range genCs(t, src) {
		all.WriteString(c)
	}
	out := all.String()
	for _, want := range []string{
		"global::sofab.FloatBits.BitsEqual(this.a, _arrdef_a)",
		"global::sofab.FloatBits.BitsEqual(this.d, _arrdef_d)",
		"SequenceEqual(this.n, _arrdef_n)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"SequenceEqual(this.a,", "SequenceEqual(this.d,"} {
		if strings.Contains(out, bad) {
			t.Errorf("IEEE array compare left: %q", bad)
		}
	}
}
