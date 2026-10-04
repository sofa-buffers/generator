package zig

import (
	"strings"
	"testing"
)

// A float array is compared with its declared default through the corelib's
// bit-pattern helper, in the write guard and in isDefault alike, never an IEEE
// std.mem.eql; an integer array keeps std.mem.eql.
func TestZigFloatArrayDefaultComparesBitsThroughCorelib(t *testing.T) {
	s := buildSchema(t, `
version: 1
messages:
  m:
    payload:
      f:  { id: 1, type: array, items: { type: fp32, count: 3 }, default: [0.0, 1.5] }
      d:  { id: 2, type: array, items: { type: fp64 }, default: [0.0, 1.5] }
      u:  { id: 3, type: array, items: { type: u32 }, default: [1, 2] }
`)
	files, err := (&Backend{}).Generate(s, map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m := string(files[0].Content)
	for _, want := range []string{
		"!sofab.floats.bitsEqual(f32, self.f.slice(), &.{ 0.0, 1.5 })",
		"!sofab.floats.bitsEqual(f64, self.d, &.{ 0.0, 1.5 })",
		"!std.mem.eql(u32, self.u, &.{ 1, 2 })",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q:\n%s", want, m)
		}
	}
	for _, bad := range []string{"std.mem.eql(f32", "std.mem.eql(f64"} {
		if strings.Contains(m, bad) {
			t.Errorf("IEEE array compare left: %q", bad)
		}
	}
}
