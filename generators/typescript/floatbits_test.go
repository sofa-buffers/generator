package typescript

import (
	"strings"
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

// A float ARRAY default is compared by bit pattern through the corelib helper,
// in the omit guard and in isDefault, in every int64 mode; an integer array next
// to it keeps elementsEqual, and no IEEE array compare is left for the floats.
func TestTSFloatArrayDefaultUsesBitsHelper(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: fp32 }, default: [0, 1.5] }\n" +
		"      d: { id: 1, type: array, items: { type: fp64 }, default: [0, 2.5] }\n" +
		"      n: { id: 2, type: array, items: { type: u8 }, default: [1, 2] }\n"
	for _, mode := range []string{"bigint", "long", "number"} {
		mod := genTSWith(t, src, map[string]any{"int64": mode})
		for _, want := range []string{
			"if (!floatArrayBitsEqual(this.a, [0, 1.5])) {",
			"if (!floatArrayBitsEqual(this.d, [0, 2.5])) {",
			"if (!(floatArrayBitsEqual(this.a, [0, 1.5]))) return false;",
			"if (!elementsEqual(this.n, [1, 2])) {",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("%s: missing %q", mode, want)
			}
		}
		if strings.Contains(mod, "elementsEqual(this.a,") || strings.Contains(mod, "elementsEqual(this.d,") {
			t.Errorf("%s: IEEE array compare left for a float array", mode)
		}
		if !strings.Contains(mod, "floatArrayBitsEqual, ") && !strings.Contains(mod, ", floatArrayBitsEqual") {
			t.Errorf("%s: floatArrayBitsEqual not imported", mode)
		}
	}
}
