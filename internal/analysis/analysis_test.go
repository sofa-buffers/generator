package analysis

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// buildSchema parses + lowers a definition (no validation gate; these tests
// exercise analysis directly).
func buildSchema(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return s
}

func TestResolveSharedType(t *testing.T) {
	src := `version: 1
$defs:
  struct:
    Point: { x: {id: 0, type: i32}, y: {id: 1, type: i32} }
messages:
  M:
    payload:
      a: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/Point'}}
      b: {id: 1, type: struct, fields: {$ref: '#/$defs/struct/Point'}}
`
	s := buildSchema(t, src)
	if err := Analyze(s); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// Both fields must point at the SAME shared NamedType (not duplicated, §3.4).
	a, b := s.Messages[0].Fields[0], s.Messages[0].Fields[1]
	if a.Ref.Target == nil || a.Ref.Target != b.Ref.Target {
		t.Fatalf("expected both fields to share one Point type; a=%p b=%p", a.Ref.Target, b.Ref.Target)
	}
	if got := len(s.Named); got != 1 {
		t.Fatalf("expected exactly 1 named type, got %d", got)
	}
}

func TestDepthWithinLimitOK(t *testing.T) {
	src := `version: 1
messages:
  M:
    payload:
      a:
        id: 0
        type: struct
        fields:
          b:
            id: 0
            type: struct
            fields:
              c: {id: 0, type: u8}
`
	doc, _ := parser.Parse([]byte(src), "t.yaml")
	s, _ := model.Build(doc)
	if err := Analyze(s); err != nil {
		t.Fatalf("nested-but-shallow struct should pass, got: %v", err)
	}
	// every composite field should be resolved
	for _, m := range s.Messages {
		for _, f := range m.Fields {
			if f.Ref != nil && f.Ref.Target == nil {
				t.Fatalf("field %s left unresolved", f.Name)
			}
		}
	}
}

func TestRecursiveStructDoesNotLoop(t *testing.T) {
	// A self-referential struct via $defs must not send depth analysis into an
	// infinite loop (the back-edge is broken).
	src := `version: 1
$defs:
  struct:
    Node:
      val: {id: 0, type: u8}
      next: {id: 1, type: struct, fields: {$ref: '#/$defs/struct/Node'}}
messages:
  M:
    payload:
      root: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/Node'}}
`
	doc, _ := parser.Parse([]byte(src), "t.yaml")
	s, _ := model.Build(doc)
	if err := Analyze(s); err != nil {
		t.Fatalf("recursive struct should analyze without error, got: %v", err)
	}
}

func TestDanglingRefReported(t *testing.T) {
	// model.Build does not resolve; analysis should report the missing target.
	src := `version: 1
messages:
  M:
    payload:
      a: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/Missing'}}
`
	doc, _ := parser.Parse([]byte(src), "t.yaml")
	s, _ := model.Build(doc)
	err := Analyze(s)
	if err == nil || !strings.Contains(err.Error(), "unresolved type reference") {
		t.Fatalf("expected unresolved-ref error, got: %v", err)
	}
}

func defaultID(t *testing.T, what string, r *ir.TypeRef) int64 {
	t.Helper()
	if r == nil || r.Target == nil || r.Target.DefaultID == nil {
		t.Fatalf("%s: union type has no DefaultID", what)
	}
	return *r.Target.DefaultID
}

// Every union type carries its default after analysis: the site's default_id,
// else the lowest option id — for a field, an element and a nested element.
func TestUnionDefaultIDBound(t *testing.T) {
	src := `version: 1
messages:
  M:
    payload:
      f: {id: 0, type: union, default_id: 7, oneof: {a: {id: 3, type: u8}, b: {id: 7, type: u8}}}
      o: {id: 1, type: union, oneof: {b: {id: 9, type: u8}, a: {id: 4, type: u8}}}
      e: {id: 2, type: array, items: {type: union, count: 2, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}}
      g: {id: 3, type: array, items: {type: array, count: 2, items: {type: union, count: 2, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}}}
`
	s := buildSchema(t, src)
	if err := Analyze(s); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	fs := s.Messages[0].Fields
	for _, c := range []struct {
		what string
		ref  *ir.TypeRef
		want int64
		opt  string
	}{
		{"explicit", fs[0].Ref, 7, "b"},
		{"omitted -> lowest id", fs[1].Ref, 4, "a"},
		{"element", fs[2].ElemRef, 1, "b"},
		{"nested element", fs[3].ElemItems.ElemRef, 1, "b"},
	} {
		if got := defaultID(t, c.what, c.ref); got != c.want {
			t.Errorf("%s: DefaultID = %d, want %d", c.what, got, c.want)
		}
		if o := c.ref.Target.DefaultOption(); o == nil || o.Name != c.opt {
			t.Errorf("%s: DefaultOption = %v, want %q", c.what, o, c.opt)
		}
	}
}

// A $defs union referenced with different default_ids is split into one type per
// default_id, at the original's position; an omitted default_id and an explicit
// one naming the same option share a type.
func TestUnionSplitByDefaultID(t *testing.T) {
	src := `version: 1
$defs:
  struct:
    A: { x: {id: 0, type: u8} }
  union:
    Shape:
      num: {id: 0, type: u16, default: 5}
      pt:  {id: 2, type: struct, fields: { x: {id: 0, type: i32} }}
  enum:
    Z: { Q: 0 }
messages:
  M:
    payload:
      first:  {id: 0, type: union, default_id: 2, oneof: {$ref: '#/$defs/union/Shape'}}
      second: {id: 1, type: union, default_id: 0, oneof: {$ref: '#/$defs/union/Shape'}}
      third:  {id: 2, type: union, oneof: {$ref: '#/$defs/union/Shape'}}
      list:   {id: 3, type: array, items: {type: union, count: 2, default_id: 2, oneof: {$ref: '#/$defs/union/Shape'}}}
`
	s := buildSchema(t, src)
	if err := Analyze(s); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	fs := s.Messages[0].Fields
	for _, c := range []struct {
		what, key string
		ref       *ir.TypeRef
		want      int64
	}{
		{"first", "union/Shape~pt", fs[0].Ref, 2},
		{"second", "union/Shape~num", fs[1].Ref, 0},
		{"third (omitted)", "union/Shape~num", fs[2].Ref, 0},
		{"list element", "union/Shape~pt", fs[3].ElemRef, 2},
	} {
		if c.ref.Key != c.key || c.ref.Target == nil || c.ref.Target.Key != c.key || s.Named[c.key] != c.ref.Target {
			t.Errorf("%s: repointed to %q (target %v), want %q", c.what, c.ref.Key, c.ref.Target, c.key)
		}
		if got := defaultID(t, c.what, c.ref); got != c.want {
			t.Errorf("%s: DefaultID = %d, want %d", c.what, got, c.want)
		}
	}
	if fs[1].Ref.Target != fs[2].Ref.Target {
		t.Error("an omitted default_id and an explicit one naming the same option must share one type")
	}
	if v := s.Named["union/Shape~pt"]; v.Name != "Shape" || v.Variant != "pt" || strings.Join(v.Path, ".") != "Shape" {
		t.Errorf("variant Name/Variant/Path = %q/%q/%v, want Shape/pt/[Shape]", v.Name, v.Variant, v.Path)
	}
	if _, ok := s.Named["union/Shape"]; ok {
		t.Error("the split original must be removed from Named")
	}
	want := []string{"struct/A", "union/Shape~num", "union/Shape~pt", "union/Shape.pt", "enum/Z"}
	if strings.Join(s.NamedOrder, ",") != strings.Join(want, ",") {
		t.Errorf("NamedOrder = %v, want %v", s.NamedOrder, want)
	}
}

// A $defs union used with a single default_id (explicit at one site, omitted at
// another, both naming the lowest option) is not split.
func TestUnionNoSplitForOneDefault(t *testing.T) {
	src := `version: 1
$defs:
  union:
    U: { a: {id: 0, type: u8}, b: {id: 1, type: u8} }
messages:
  M:
    payload:
      x: {id: 0, type: union, default_id: 0, oneof: {$ref: '#/$defs/union/U'}}
      y: {id: 1, type: union, oneof: {$ref: '#/$defs/union/U'}}
`
	s := buildSchema(t, src)
	if err := Analyze(s); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	fs := s.Messages[0].Fields
	if fs[0].Ref.Key != "union/U" || fs[1].Ref.Target != fs[0].Ref.Target || defaultID(t, "U", fs[0].Ref) != 0 {
		t.Errorf("want one unsplit union/U with DefaultID 0, got %q / %q", fs[0].Ref.Key, fs[1].Ref.Key)
	}
}

// A split variant can meet no other type: its key carries "~", which no name
// or other key contains, and backends name it through Variant, in a channel
// no path segment can produce. These schemas used to be refused because the
// variant was named <Name>_default_<option>, which a $defs union spelled that
// way, or the inline option `pt` of a union `ShapeDefault`, derived as well.
func TestUnionSplitMeetsNoOtherType(t *testing.T) {
	for _, other := range []string{
		"Shape_default_pt: { z: {id: 0, type: u8} }",
		"ShapeDefault: { pt: {id: 0, type: struct, fields: { x: {id: 0, type: u8} }} }",
	} {
		src := `version: 1
$defs:
  union:
    Shape: { num: {id: 0, type: u8}, pt: {id: 2, type: u8} }
    ` + other + `
messages:
  M:
    payload:
      a: {id: 0, type: union, default_id: 0, oneof: {$ref: '#/$defs/union/Shape'}}
      b: {id: 1, type: union, default_id: 2, oneof: {$ref: '#/$defs/union/Shape'}}
`
		s := buildSchema(t, src)
		if err := Analyze(s); err != nil {
			t.Fatalf("%s: analyze: %v", other, err)
		}
		fs := s.Messages[0].Fields
		if fs[0].Ref.Key != "union/Shape~num" || fs[1].Ref.Key != "union/Shape~pt" {
			t.Errorf("%s: sites point at %q and %q, want the two variants", other, fs[0].Ref.Key, fs[1].Ref.Key)
		}
		if len(s.Named) != len(s.NamedOrder) {
			t.Errorf("%s: Named has %d types, NamedOrder %d", other, len(s.Named), len(s.NamedOrder))
		}
	}
}
