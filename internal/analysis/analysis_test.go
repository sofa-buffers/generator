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
