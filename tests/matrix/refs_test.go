package matrix

import (
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// TestLocalRefSharedOnce: a $defs type referenced N times resolves to a single
// shared NamedType, and every referencing field points at it.
func TestLocalRefSharedOnce(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/multi_ref.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := count(s, ir.CatStruct); got != 1 {
		t.Fatalf("expected exactly 1 shared struct (Point), got %d", got)
	}
	q := s.Messages[0]
	var target *ir.NamedType
	for i := 0; i < 4; i++ { // a,b,c,d all reference Point
		f := q.Fields[i]
		if f.Ref == nil || f.Ref.Target == nil {
			t.Fatalf("field %s unresolved", f.Name)
		}
		if target == nil {
			target = f.Ref.Target
		} else if f.Ref.Target != target {
			t.Errorf("field %s points at a different Point instance (not shared)", f.Name)
		}
	}
	// Color enum referenced twice -> one enum.
	if got := count(s, ir.CatEnum); got != 1 {
		t.Errorf("expected 1 shared enum, got %d", got)
	}
}

// TestCrossFileRefSharedAndTransitive: a cross-file $ref pulls a def plus its
// same-file dependency, and the dependency (Vec3) is shared across uses.
func TestCrossFileRefSharedAndTransitive(t *testing.T) {
	s, err := buildIR(t, "corpus/defs/cross_file.yaml")
	if err != nil {
		t.Fatalf("cross-file ref should resolve: %v", err)
	}
	// Vec3 (transitive dep of Bounds) and Bounds both present, each once.
	for _, key := range []string{"struct/Vec3", "struct/Bounds", "enum/Unit", "bitfield/Caps"} {
		if _, ok := s.Named[key]; !ok {
			t.Errorf("expected merged named type %q in graph (have %v)", key, s.NamedOrder)
		}
	}
	// Vec3 is exactly one shared instance even though Bounds.min, Bounds.max and
	// the velocity field all use it.
	vec3 := s.Named["struct/Vec3"]
	bounds := s.Named["struct/Bounds"]
	if bounds.Fields[0].Ref.Target != vec3 || bounds.Fields[1].Ref.Target != vec3 {
		t.Error("Bounds.min/max should share the single Vec3 instance")
	}
	if s.Messages[0].Fields[1].Ref.Target != vec3 { // velocity
		t.Error("velocity should share the same Vec3 instance")
	}
}

func count(s *ir.Schema, cat ir.Category) int {
	n := 0
	for _, key := range s.NamedOrder {
		if s.Named[key].Category == cat {
			n++
		}
	}
	return n
}
