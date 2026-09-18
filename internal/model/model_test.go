package model_test

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// An array field's `unit` and `decimals` describe its leaf element and must reach
// the IR exactly as they do on a scalar field: backends read Field.Unit and
// Field.Decimals without a kind check.
func TestArrayFieldCarriesUnitAndDecimals(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      v: {id: 0, type: array, unit: mV, decimals: 2, items: {type: array, items: {type: fp32, count: 4}}}\n" +
		"      s: {id: 1, type: fp64, unit: m, decimals: 7}\n"
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		unit     string
		decimals int
	}{"v": {"mV", 2}, "s": {"m", 7}}
	for _, f := range s.Messages[0].Fields {
		w := want[f.Name]
		if f.Unit != w.unit {
			t.Errorf("%s: Unit = %q, want %q", f.Name, f.Unit, w.unit)
		}
		if f.Decimals == nil || *f.Decimals != w.decimals {
			t.Errorf("%s: Decimals = %v, want %d", f.Name, f.Decimals, w.decimals)
		}
	}
}
