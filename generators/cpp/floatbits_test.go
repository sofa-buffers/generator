package cpp

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestCppFloatScalarComparesBits(t *testing.T) {
	g := &gen{}
	for _, c := range []struct {
		f    *ir.Field
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "std::bit_cast<std::uint32_t>(x) != 0x0u"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "std::bit_cast<std::uint32_t>(x) != 0x3fc00000u"},
		{&ir.Field{Kind: ir.KindFP64}, "std::bit_cast<std::uint64_t>(x) != 0x0ull"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "std::bit_cast<std::uint64_t>(x) != 0x3ff8000000000000ull"},
	} {
		if got := g.fieldIsNotDefaultExprAt(c.f, "x"); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
	if got := g.fieldIsDefaultExprAt(&ir.Field{Kind: ir.KindFP32}, "x"); got != "std::bit_cast<std::uint32_t>(x) == 0x0u" {
		t.Errorf("isDefault: got %q", got)
	}
}
