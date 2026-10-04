package python

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar at a zero default is compared by bit pattern: the sign of the
// zero is read with math.copysign (-0.0 is not the default 0). A non-zero default
// keeps the plain compare.
func TestPyFloatZeroDefaultComparesSign(t *testing.T) {
	for _, c := range []struct {
		f       *ir.Field
		differs bool
		want    string
		ok      bool
	}{
		{&ir.Field{Kind: ir.KindFP32}, true, "x != 0.0 or math.copysign(1.0, x) < 0.0", true},
		{&ir.Field{Kind: ir.KindFP64}, false, "x == 0.0 and math.copysign(1.0, x) > 0.0", true},
		{&ir.Field{Kind: ir.KindFP64, Default: 0.0}, true, "x != 0.0 or math.copysign(1.0, x) < 0.0", true},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, true, "", false},
		{&ir.Field{Kind: ir.KindU32}, true, "", false},
	} {
		got, ok := floatZeroCmp(c.f, "x", c.differs)
		if got != c.want || ok != c.ok {
			t.Errorf("%v default %v: got %q,%v want %q,%v", c.f.Kind, c.f.Default, got, ok, c.want, c.ok)
		}
	}
}

// A float array is compared with its default by bit pattern, through the
// corelib's float_array_bits_equal, in the write guard and in _is_default; the
// IEEE list compare is gone for it, and the integer array keeps its plain one.
func TestPyFloatArrayDefaultUsesCorelibHelper(t *testing.T) {
	s := schema(t, `version: 1
messages:
  m:
    payload:
      a:
        id: 0
        type: array
        items: { type: fp32, count: 3 }
        default: [0.0, 1.5]
      b:
        id: 1
        type: array
        items: { type: fp64 }
        default: [0.0]
      c:
        id: 2
        type: array
        items: { type: u8 }
        default: [1, 2]
`)
	src := string(genPy(t, s, map[string]any{})["message.py"])
	for _, want := range []string{
		"if not float_array_bits_equal(self.a, [0, 1.5]):",
		"if not float_array_bits_equal(self.b, [0]):",
		"if not (float_array_bits_equal(self.b, [0])):",
		"if not (float_array_bits_equal(self.a, [0, 1.5])):",
		"if self.c != [1, 2]:",
		"from sofab import ", "float_array_bits_equal",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated module lacks %q", want)
		}
	}
	for _, bad := range []string{"self.a != [", "self.a == [", "self.b != [", "self.b == ["} {
		if strings.Contains(src, bad) {
			t.Errorf("generated module still compares a float array with IEEE equality: %q", bad)
		}
	}
	if !strings.Contains(src, "float_array_bits_equal, ") && !strings.Contains(src, ", float_array_bits_equal") {
		t.Errorf("float_array_bits_equal is not imported from sofab")
	}
}
