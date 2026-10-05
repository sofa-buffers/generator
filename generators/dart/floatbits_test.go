package dart

import (
	"strings"
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

// A float array default is held by the corelib's Float32ArrayDefault /
// Float64ArrayDefault and compared by bit pattern through its `matches`, never
// by `_prefixEq` (whose `!=` on a double is IEEE): the
// omission test of fp32 and fp64 arrays, at top level and inside a nested struct.
func TestDartFloatArrayDefaultUsesCorelibBitCompare(t *testing.T) {
	const def = "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: fp32, count: 3 }, default: [0.0, 1.5] }\n" +
		"      b: { id: 1, type: array, items: { type: fp64 }, default: [0.0, 1.5] }\n" +
		"      n:\n        id: 2\n        type: struct\n        fields:\n" +
		"          c: { id: 0, type: array, items: { type: fp32 }, default: [0.0] }\n"
	out := genFor(t, writeDef(t, def), map[string]any{})
	for _, want := range []string{
		"static final sofab.Float32ArrayDefault _aDefault = sofab.Float32ArrayDefault(",
		"static final sofab.Float64ArrayDefault _bDefault = sofab.Float64ArrayDefault(",
		"..assign(_aDefault.list)",
		"if (!_aDefault.matches(a.storage, a.length)) {",
		"if (!_bDefault.matches(b.storage, b.length)) {",
		"if (!_cDefault.matches(c.storage, c.length)) {",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "_prefixEq") {
		t.Errorf("an IEEE array compare is left:\n%s", out)
	}
}
