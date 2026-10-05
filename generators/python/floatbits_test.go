package python

import (
	"math"
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

// A float array is compared with its default by bit pattern. The list `==` runs
// at C speed and already tells every value but a zero's sign apart, so a default
// with no zero keeps it, and a default with zeros adds one math.copysign read per
// zero position behind it, in the write guard and in _is_default alike. A default
// with more zeros than maxInlineZeroSigns is compared by the corelib's
// float_array_bits_equal instead, and the integer array keeps its plain compare.
func TestPyFloatArrayDefaultCompare(t *testing.T) {
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
      d:
        id: 3
        type: array
        items: { type: fp64 }
        default: [1.5, 2.5]
      e:
        id: 4
        type: array
        items: { type: fp64 }
        default: [-0.0, 1.5, 0.0]
      f:
        id: 5
        type: array
        items: { type: fp32 }
        default: [0.0, 0.0, 0.0, 0.0, 0.0, 1.0]
`)
	src := string(genPy(t, s, map[string]any{})["message.py"])
	for _, want := range []string{
		// one zero: the list compare, then the sign of that zero
		"if (_a := self.a) != _M__Def__a or copysign(1.0, _a[0]) < 0.0:",
		"if not ((_a := self.a) == _M__Def__a and copysign(1.0, _a[0]) > 0.0):",
		"if (_a := self.b) != _M__Def__b or copysign(1.0, _a[0]) < 0.0:",
		"if not ((_a := self.b) == _M__Def__b and copysign(1.0, _a[0]) > 0.0):",
		// no zero: exactly the plain compare
		"if self.d != _M__Def__d:",
		"if not (self.d == _M__Def__d):",
		// a -0.0 and a +0.0 in one default: one sign read each, each its own sign
		"if (_a := self.e) != _M__Def__e or copysign(1.0, _a[0]) > 0.0 or copysign(1.0, _a[2]) < 0.0:",
		"if not ((_a := self.e) == _M__Def__e and copysign(1.0, _a[0]) < 0.0 and copysign(1.0, _a[2]) > 0.0):",
		// more zeros than are told apart inline: the corelib helper
		"if not float_array_bits_equal(self.f, _M__Def__f):",
		"if not (float_array_bits_equal(self.f, _M__Def__f)):",
		"if self.c != [1, 2]:",
		"from math import copysign\n",
		// each float array default is built once, after its class, and named by it
		"\n_M__Def__a = [0.0, 1.5]\n", "\n_M__Def__b = [0.0]\n", "\n_M__Def__d = [1.5, 2.5]\n",
		"\n_M__Def__e = [-0.0, 1.5, 0.0]\n", "\n_M__Def__f = [0.0, 0.0, 0.0, 0.0, 0.0, 1.0]\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated module lacks %q", want)
		}
	}
	if !strings.Contains(src, "float_array_bits_equal, ") && !strings.Contains(src, ", float_array_bits_equal") {
		t.Errorf("float_array_bits_equal is not imported from sofab")
	}
	// No list literal is rebuilt per compare, and no integer array gets a constant.
	for _, bad := range []string{"self.a != [", "self.a == [", "self.b != [", "self.b == [", "_M__Def__c"} {
		if strings.Contains(src, bad) {
			t.Errorf("generated module still has %q", bad)
		}
	}
}

// A schema whose float array defaults all take the inline form calls no helper,
// so the generated module does not import it (pyflakes F401).
func TestPyFloatArrayInlineFormImportsNoHelper(t *testing.T) {
	s := schema(t, `version: 1
messages:
  m:
    payload:
      a:
        id: 0
        type: array
        items: { type: fp32 }
        default: [0.0, 1.5]
`)
	src := string(genPy(t, s, map[string]any{})["message.py"])
	if strings.Contains(src, "float_array_bits_equal") {
		t.Errorf("an inline float array compare still imports or calls the helper:\n%s", src)
	}
}

func TestPyFloatArrayDefaultZeros(t *testing.T) {
	zeros, neg, helper := floatArrayDefaultZeros([]any{0.0, 1.5, math.Copysign(0, -1), 0})
	if len(zeros) != 3 || zeros[0] != 0 || zeros[1] != 2 || zeros[2] != 3 || neg[0] || !neg[1] || neg[2] || helper {
		t.Errorf("zeros %v neg %v helper %v", zeros, neg, helper)
	}
	if _, _, h := floatArrayDefaultZeros([]any{1.5, math.NaN()}); !h {
		t.Errorf("a NaN default must use the helper")
	}
	if _, _, h := floatArrayDefaultZeros([]any{0.0, 0.0, 0.0, 0.0, 0.0}); !h {
		t.Errorf("more than %d zeros must use the helper", maxInlineZeroSigns)
	}
	if z, _, h := floatArrayDefaultZeros([]any{nil, "x", 1.5}); len(z) != 0 || h {
		t.Errorf("non-numeric elements are not zeros: %v %v", z, h)
	}
}

// A negative zero inside a float array default keeps its sign: "%v" would print
// -0, the integer 0, and the bit compare would then run against +0.0.
func TestPyFloatArrayDefaultKeepsNegativeZero(t *testing.T) {
	s := schema(t, `version: 1
messages:
  m:
    payload:
      b:
        id: 1
        type: array
        items: { type: fp64 }
        default: [-0.0, 2.0]
`)
	src := string(genPy(t, s, map[string]any{})["message.py"])
	for _, want := range []string{
		"lambda: [-0.0, 2]",
		"(_a := self.b) != _M__Def__b or copysign(1.0, _a[0]) > 0.0",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
}

// A float array default is held by a constant named after the class that owns
// the field, so two classes with a field of the same name cannot share one. (A
// union option cannot declare a non-empty array default, so none gets one.)
func TestPyFloatArrayDefaultConstantIsPerClass(t *testing.T) {
	s := schema(t, `version: 1
messages:
  m:
    payload:
      p: { id: 0, type: struct, fields: { v: { id: 0, type: array, items: { type: fp64 }, default: [0.0, 1.0] } } }
      q: { id: 1, type: struct, fields: { v: { id: 0, type: array, items: { type: fp64 }, default: [0.0, 2.0] } } }
`)
	src := string(genPy(t, s, map[string]any{})["message.py"])
	n := 0
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "__Def__v = ") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("want one __Def__v constant per owning class, got %d in:\n%s", n, src)
	}
	for _, want := range []string{"[0.0, 1.0]", "[0.0, 2.0]"} {
		if !strings.Contains(src, want) {
			t.Errorf("module lacks %q", want)
		}
	}
}
