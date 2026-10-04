package rust

import (
	"strings"
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

// A float array with a declared default is compared with it by bit pattern
// through the corelib helper, never with an IEEE slice compare: [-0.0, 1.5] is
// not the default [0.0, 1.5]. Nested structs reach the same guard through their
// own serialize.
func TestRustFloatArrayComparesBitsThroughCorelib(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: array, items: { type: fp32, count: 4 }, default: [0.0, 1.5] }
      d: { id: 1, type: array, items: { type: fp64, count: 4 }, default: [0.0, 1.5] }
      u: { id: 2, type: array, items: { type: u32, count: 4 }, default: [1, 2] }
      n: { id: 3, type: array, items: { type: fp32, count: 4 } }
      s:
        id: 4
        type: struct
        fields:
          a: { id: 0, type: array, items: { type: fp32, count: 4 }, default: [0.0, 1.5] }
`
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{"std", map[string]any{}, []string{
			"if !sofab::float_bits::bits_equal(&self.a[..], &[0.0, 1.5][..]) {",
			"if !sofab::float_bits::bits_equal(&self.d[..], &[0.0, 1.5][..]) {",
		}},
		{"no-std", map[string]any{"corelib": "rs-no-std"}, []string{
			"if !sofab::floats::bits_equal_f32(&self.a[..], &[0.0, 1.5][..]) {",
			"if !sofab::floats::bits_equal_f64(&self.d[..], &[0.0, 1.5][..]) {",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := moduleFromYAML(t, src, tc.cfg)
			for _, w := range tc.want {
				if !strings.Contains(m, w) {
					t.Errorf("missing %q:\n%s", w, m)
				}
			}
			// Both inner.a and m.a use the helper; no float array keeps the IEEE compare.
			if n := strings.Count(m, "bits_equal"); n < 3 {
				t.Errorf("want the helper in m and the nested struct (3 sites), got %d", n)
			}
			for _, bad := range []string{"self.a[..] != [0.0", "self.d[..] != [0.0"} {
				if strings.Contains(m, bad) {
					t.Errorf("IEEE array compare left: %q", bad)
				}
			}
			if !strings.Contains(m, "if self.u[..] != [1, 2][..] {") {
				t.Errorf("an integer array keeps the slice compare")
			}
			if !strings.Contains(m, "if !self.n.is_empty() {") {
				t.Errorf("a default-less float array is omitted only when empty")
			}
		})
	}
}
