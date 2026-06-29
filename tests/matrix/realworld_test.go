package matrix

import (
	goparser "go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// TestRealWorldExample guards examples/realworld: the multi-file, cross-file
// $ref schema must validate, resolve its shared-type graph correctly, and
// generate for every backend (Go output is parse-checked). Hermetic.
func TestRealWorldExample(t *testing.T) {
	s, err := buildIR(t, "../../examples/realworld/vehicle_telemetry.yaml")
	if err != nil {
		t.Fatalf("real-world example should validate: %v", err)
	}

	// Cross-file types from both files are merged into one graph.
	for _, key := range []string{
		"struct/Vector3", "struct/GeoPoint", "struct/Timestamp",
		"struct/DiagnosticCode", "enum/Gear", "bitfield/FaultFlags",
	} {
		if _, ok := s.Named[key]; !ok {
			t.Errorf("expected merged type %q (have %v)", key, s.NamedOrder)
		}
	}
	// velocity and acceleration share the single Vector3 instance.
	msg := s.Messages[0]
	var velocity, accel *ir.Field
	for _, f := range msg.Fields {
		switch f.Name {
		case "velocity":
			velocity = f
		case "acceleration":
			accel = f
		}
	}
	if velocity == nil || accel == nil || velocity.Ref.Target == nil ||
		velocity.Ref.Target != accel.Ref.Target {
		t.Error("velocity and acceleration should share one Vector3 type")
	}

	// Generates for every backend; Go output parses.
	for _, lang := range generator.Registered() {
		b, _ := generator.Lookup(lang)
		files, err := b.Generate(s, map[string]any{})
		if err != nil {
			t.Errorf("[%s] generate: %v", lang, err)
			continue
		}
		for _, f := range files {
			if lang == "go" && strings.HasSuffix(f.Path, ".go") {
				if _, perr := goparser.ParseFile(token.NewFileSet(), f.Path, f.Content, goparser.AllErrors); perr != nil {
					t.Errorf("[go] %s does not parse: %v", f.Path, perr)
				}
			}
		}
	}
}
