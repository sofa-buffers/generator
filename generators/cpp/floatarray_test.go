package cpp

import (
	"strings"
	"testing"
)

const floatArrayYAML = `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: array, items: { type: fp32, count: 3 }, default: [0.0, 1.5] }
      d: { id: 1, type: array, items: { type: fp64, count: 3 }, default: [0.0, 2.5] }
      e: { id: 2, type: array, items: { type: fp32, count: 3 } }
      i: { id: 3, type: array, items: { type: u8, count: 3 }, default: [0, 7] }
      n: { id: 4, type: struct, fields: { b: { id: 0, type: array, items: { type: fp32, count: 2 }, default: [1.5, 0.0] } } }
`

// A float array with a declared default is compared with it by bit pattern
// through sofab::bitsEqual, in the write guard and in _isDefault alike, on every
// storage profile; no IEEE container compare is left (generator#636).
func TestCppFloatArrayDefaultComparesBits(t *testing.T) {
	for _, p := range unionProfiles {
		h := unionFiles(t, floatArrayYAML, p.cfg)["m.hpp"]
		for _, want := range []string{
			"if (!sofab::bitsEqual(a, std::initializer_list<float>{0.0f, 1.5f})) {",
			"if (!sofab::bitsEqual(d, std::initializer_list<double>{0.0, 2.5})) {",
			"if (!sofab::bitsEqual(b, std::initializer_list<float>{1.5f, 0.0f})) {",
			"if (!(sofab::bitsEqual(a, std::initializer_list<float>{0.0f, 1.5f}))) { return false; }",
		} {
			if !strings.Contains(h, want) {
				t.Errorf("%s: missing %q in:\n%s", p.name, want, h)
			}
		}
		for _, bad := range []string{
			"a != std::vector<float>", "a == std::vector<float>",
			"a != sofab::InlineVector<float", "a == sofab::InlineVector<float",
			"d != std::vector<double>", "d != sofab::InlineVector<double",
			"b != std::vector<float>", "b != sofab::InlineVector<float",
		} {
			if strings.Contains(h, bad) {
				t.Errorf("%s: IEEE array compare %q left in:\n%s", p.name, bad, h)
			}
		}
		// An empty default stays empty(); an integer array keeps its container compare.
		if !strings.Contains(h, "!e.empty()") {
			t.Errorf("%s: empty-default fp array lost its empty() guard", p.name)
		}
		if !strings.Contains(h, "i != ") {
			t.Errorf("%s: integer array compare changed", p.name)
		}
	}
}
