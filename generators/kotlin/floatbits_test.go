package kotlin

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A float scalar is compared with its default by bit pattern, against an integer
// literal the generator computes (-0.0 is not the default 0).
func TestKotlinFloatScalarComparesBits(t *testing.T) {
	for _, c := range []struct {
		f    *ir.Field
		want string
	}{
		{&ir.Field{Kind: ir.KindFP32}, "x.toRawBits() != 0"},
		{&ir.Field{Kind: ir.KindFP32, Default: 1.5}, "x.toRawBits() != 1069547520"},
		{&ir.Field{Kind: ir.KindFP64, Default: 1.5}, "x.toRawBits() != 4609434218613702656L"},
	} {
		if got := floatBitsNe(c.f, "x"); got != c.want {
			t.Errorf("%v default %v: got %q, want %q", c.f.Kind, c.f.Default, got, c.want)
		}
	}
}

// An fp32 position is taken from its raw wire bits (generator#670): on Kotlin/JS
// a Float is a double and widening quiets a signaling NaN. A scalar keeps the
// bits beside its value while it is a NaN and serialize hands both to the
// corelib; an array element goes through the corelib's raw-bits view; a field
// whose own name ends in Fp32Bits is escaped so it cannot meet a companion.
func TestKotlinFp32TakesRawBits(t *testing.T) {
	out := genFromYAML(t, `version: 1
messages:
  M:
    payload:
      f: { id: 0, type: fp32 }
      fFp32Bits: { id: 1, type: u8 }
      a: { id: 2, type: array, items: { type: fp32, count: 4 } }
      rows: { id: 3, type: array, items: { type: array, count: 2, items: { type: fp32, count: 3 } } }
`, map[string]any{})
	src := out["src/main/kotlin/message/M.kt"]
	if src == "" {
		for p := range out {
			t.Logf("emitted %s", p)
		}
		t.Fatal("no M.kt")
	}
	for _, want := range []string{
		"    public var f: Float = 0.0f\n",
		"    public var fFp32Bits: Int? = null\n",
		"    public var fFp32Bits_: UByte = ",
		"os.writeFp32(0, this.f, this.fFp32Bits)",
		"        this.fFp32Bits = null\n",
		"    override fun fp32Bits(id: Int, bits: Int) {",
		"0 -> { m.f = Float.fromBits(bits); m.fFp32Bits = Seq.fp32NaNBits(bits) }",
		"m.a = FloatArray(count); afv = Seq.fp32BitsView(m.a)",
		"Seq.putFp32Bits(m.a, afv, ai, bits); ai++",
		"; afv = Seq.fp32BitsView(_arowFloat)",
		"Seq.putFp32Bits(_arowFloat, afv, ai, bits); ai++",
		"    private var afv: IntArray? = null",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"override fun fp32(id: Int, value: Float)", "os.writeFp32(0, this.f)\n"} {
		if strings.Contains(src, bad) {
			t.Errorf("still emits %q", bad)
		}
	}
}
