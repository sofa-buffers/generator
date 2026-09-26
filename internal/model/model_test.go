package model_test

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
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

// A union site's default_id travels on its TypeRef — for a field, an array's
// union element and a union element two array levels down alike — so analysis
// can bind it to the union type. An omitted default_id stays nil here.
func TestUnionDefaultIDReachesEveryTypeRef(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      f: {id: 0, type: union, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}\n" +
		"      e: {id: 1, type: array, items: {type: union, count: 2, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}}\n" +
		"      g: {id: 2, type: array, items: {type: array, count: 2, items: {type: union, count: 2, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}}}\n" +
		"      o: {id: 3, type: union, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}\n"
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	fs := map[string]*ir.Field{}
	for _, f := range s.Messages[0].Fields {
		fs[f.Name] = f
	}
	for name, ref := range map[string]*ir.TypeRef{
		"field":          fs["f"].Ref,
		"element":        fs["e"].ElemRef,
		"nested element": fs["g"].ElemItems.ElemRef,
	} {
		if ref == nil || ref.DefaultID == nil || *ref.DefaultID != 1 {
			t.Errorf("%s: TypeRef.DefaultID = %v, want 1", name, ref)
		}
	}
	if d := fs["o"].Ref.DefaultID; d != nil {
		t.Errorf("omitted default_id: TypeRef.DefaultID = %d, want nil", *d)
	}
	if fs["f"].Default != int64(1) {
		t.Errorf("union field Default = %v, want the raw default_id 1 (read by the docs target)", fs["f"].Default)
	}
}
