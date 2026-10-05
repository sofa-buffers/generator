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

// A float ARRAY default is compared by bit pattern through the corelib helper of
// its width, against a module-level typed constant built once, in the omit guard
// and in isDefault, in every int64 mode; an integer array next to it keeps
// elementsEqual against its literal, and no IEEE array compare is left for the
// floats.
func TestTSFloatArrayDefaultUsesBitsHelper(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: fp32 }, default: [0, 1.5] }\n" +
		"      d: { id: 1, type: array, items: { type: fp64 }, default: [0, 2.5] }\n" +
		"      n: { id: 2, type: array, items: { type: u8 }, default: [1, 2] }\n" +
		"      b: { id: 3, type: array, items: { type: fp32 }, default: [0, 1.5] }\n"
	for _, mode := range []string{"bigint", "long", "number"} {
		mod := genTSWith(t, src, map[string]any{"int64": mode})
		for _, want := range []string{
			"const _DEF_0 = new Float32Array([0, 1.5]);",
			"const _DEF_1 = new Float64Array([0, 2.5]);",
			"if (!fp32ArrayBitsEqual(this.a, _DEF_0)) {",
			"if (!fp64ArrayBitsEqual(this.d, _DEF_1)) {",
			"if (!(fp32ArrayBitsEqual(this.a, _DEF_0))) return false;",
			"if (!fp32ArrayBitsEqual(this.b, _DEF_0)) {",
			"if (!elementsEqual(this.n, [1, 2])) {",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("%s: missing %q", mode, want)
			}
		}
		// Equal defaults of one carrier share one constant, and nothing is built
		// at the compare site.
		if strings.Contains(mod, "_DEF_2") {
			t.Errorf("%s: a third default constant for two equal defaults", mode)
		}
		for _, bad := range []string{"elementsEqual(this.a,", "elementsEqual(this.d,", "floatArrayBitsEqual", "BitsEqual(this.a, [", "BitsEqual(this.d, ["} {
			if strings.Contains(mod, bad) {
				t.Errorf("%s: %q left in the output", mode, bad)
			}
		}
		for _, name := range []string{"fp32ArrayBitsEqual", "fp64ArrayBitsEqual"} {
			if !strings.Contains(mod, name+", ") && !strings.Contains(mod, ", "+name) {
				t.Errorf("%s: %s not imported", mode, name)
			}
		}
	}
}

// A schema with no float array default declares no default constants, and an
// import list that names no float helper.
func TestTSNoFloatDefaultNoConstants(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      n: { id: 0, type: array, items: { type: u8 }, default: [1, 2] }\n" +
		"      f: { id: 1, type: array, items: { type: fp32 } }\n"
	mod := genTSWith(t, src, map[string]any{})
	for _, bad := range []string{"_DEF_", "ArrayBitsEqual"} {
		if strings.Contains(mod, bad) {
			t.Errorf("%q in the output of a schema with no float array default", bad)
		}
	}
}
