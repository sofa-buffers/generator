package golang

import (
	"strconv"
	"strings"
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

// A short float array default is compared by a generated function of its own,
// length first and then one statement per element against a bit-pattern
// constant (see floatArrayIsDefaultFunc for why not one condition). A longer one
// goes through the corelib's sofab.BitsEqual against a package-level copy, so the
// default is not rebuilt on every call. An integer array keeps slices.Equal.
func TestGoFloatArrayComparesBits(t *testing.T) {
	fld := func(elem ir.Kind, def ...any) *ir.Field {
		return &ir.Field{Name: "a", Kind: ir.KindArray, Elem: elem, Default: def}
	}
	long := make([]any, floatArrayUnrollMax+1)
	for i := range long {
		long[i] = 0.5
	}
	for _, c := range []struct {
		fld  *ir.Field
		want string // the expression
		decl string // a fragment of the queued declaration
	}{
		{fld(ir.KindFP32, 0.0, 1.5), "_M__AIsDefault(x)",
			"func _M__AIsDefault(a []float32) bool {\n\tif len(a) != 2 {\n\t\treturn false\n\t}\n" +
				"\tif math.Float32bits(a[0]) != 0x0 {\n\t\treturn false\n\t}\n" +
				"\tif math.Float32bits(a[1]) != 0x3fc00000 {\n\t\treturn false\n\t}\n\treturn true\n}"},
		// One element: a single return, which inlines without a materialised bool.
		{fld(ir.KindFP64, 1.5), "_M__AIsDefault(x)",
			"func _M__AIsDefault(a []float64) bool {\n\treturn len(a) == 1 && math.Float64bits(a[0]) == 0x3ff8000000000000\n}"},
		// A -0.0 default is the Go constant 0 (the member is +0.0), so its bits are 0.
		{fld(ir.KindFP32, -0.0*-1, 1.5), "_M__AIsDefault(x)", "if math.Float32bits(a[0]) != 0x0 {"},
		{fld(ir.KindFP32), "_M__AIsDefault(x)", "return len(a) == 0\n}"},
		{fld(ir.KindFP32, long[:floatArrayUnrollMax]...), "_M__AIsDefault(x)", "if math.Float32bits(a[15]) != 0x3f000000 {"},
		{fld(ir.KindFP32, long...), "sofab.BitsEqual(x, _M__ADefault)", "var _M__ADefault = []float32{0.5, "},
	} {
		g := &gen{owner: "M"}
		f := newGoFile("p")
		if got := g.fieldIsDefaultExprAt(f, c.fld, "x"); got != c.want {
			t.Errorf("%d defaults: got %q, want %q", len(c.fld.Default.([]any)), got, c.want)
		}
		if len(g.defDecls) != 1 || !strings.Contains(g.defDecls[0], c.decl) {
			t.Errorf("%d defaults: queued %q, want it to contain %q", len(c.fld.Default.([]any)), g.defDecls, c.decl)
		}
		if strings.Contains(c.want, "slices.Equal") || strings.Contains(strings.Join(g.defDecls, ""), "slices.Equal") {
			t.Errorf("%d defaults: IEEE compare", len(c.fld.Default.([]any)))
		}
		// The write guard and isDefault name the same function, queued once.
		_ = g.fieldIsDefaultExprAt(f, c.fld, "m.A")
		if len(g.defDecls) != 1 {
			t.Errorf("declared twice: %q", g.defDecls)
		}
	}

	// An integer array keeps slices.Equal.
	g := &gen{owner: "M"}
	u := &ir.Field{Name: "a", Kind: ir.KindArray, Elem: ir.KindU32, Default: []any{1, 2}}
	if got := g.fieldIsDefaultExprAt(newGoFile("p"), u, "x"); got != "slices.Equal(x, []uint32{1, 2})" || len(g.defDecls) != 0 {
		t.Errorf("integer array: %q", got)
	}
}

// floatArrayDefaultSrc declares float array defaults on both sides of the inline
// bound (floatArrayUnrollMax): 1, 2 and 16 elements are compared by a generated
// function, 17 by sofab.BitsEqual.
const floatArrayDefaultSrc = `version: 1
messages:
  M:
    payload:
      a1:  { id: 0, type: array, items: { type: fp64, count: 4 }, default: [1.5] }
      a2:  { id: 1, type: array, items: { type: fp32, count: 4 }, default: [0.0, 1.5] }
      a16: { id: 2, type: array, items: { type: fp64, count: 20 }, default: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15] }
      a17: { id: 3, type: array, items: { type: fp32, count: 20 }, default: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16] }
`

// Behaviour of both shapes against the real corelib: a value at its default is
// omitted, and -0.0, a changed last element and a different length are each a
// value that reaches the wire and survives a decode.
func TestGoFloatArrayDefaultBehavior(t *testing.T) {
	corelib := requireGoCorelib(t)
	bin := buildGoHarnessCfg(t, corelib, floatArrayDefaultSrc, nil)
	seq := func(n int, neg0 bool) string {
		var parts []string
		for i := 0; i < n; i++ {
			switch {
			case i == 0 && neg0:
				parts = append(parts, "-0.0")
			case i == 0:
				parts = append(parts, "0")
			default:
				parts = append(parts, strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(i), 'f', 1, 64), "0"), "."))
			}
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	for _, c := range []struct {
		field   string
		def     string // JSON of the default
		n       int
		changed string // JSON of the same length, last element different
	}{
		{"a1", "[1.5]", 1, "[2.5]"},
		{"a2", "[0, 1.5]", 2, "[0, 2.5]"},
		{"a16", seq(16, false), 16, strings.Replace(seq(16, false), "15", "9", 1)},
		{"a17", seq(17, false), 17, strings.Replace(seq(17, false), "16", "9", 1)},
	} {
		if got := encHex(t, bin, "M", `{"`+c.field+`":`+c.def+`}`); got != "" {
			t.Errorf("%s at its default: wrote %s", c.field, got)
		}
		neg := strings.Replace(c.def, "0", "-0.0", 1)
		if c.field == "a1" {
			neg = "[-1.5]"
		}
		for name, v := range map[string]string{
			"sign of the first element": neg,
			"last element":              c.changed,
			"shorter":                   "[1.5]",
		} {
			if c.n == 1 && name == "shorter" {
				v = "[]"
			}
			in := `{"` + c.field + `":` + v + `}`
			got := encHex(t, bin, "M", in)
			if got == "" {
				t.Errorf("%s, %s: %s omitted as the default", c.field, name, in)
				continue
			}
			if rt := decJSON(t, bin, "M", got); !strings.Contains(rt, `"`+c.field+`"`) {
				t.Errorf("%s, %s: round trip lost the field: %s", c.field, name, rt)
			}
		}
	}
}
