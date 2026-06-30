package csharp

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleModule(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(b, "example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, map[string]any{"namespace": "Sofabuffers"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "Messages.cs" {
			return string(f.Content)
		}
	}
	t.Fatal("no module")
	return ""
}

func TestCsStructural(t *testing.T) {
	m := exampleModule(t)
	for _, want := range []string{
		"using sofab;",
		"namespace Sofabuffers;",
		"public sealed class Myfirstmessage {",
		"public void Marshal(OStream os)",
		"public byte[] Encode()",
		"public static Myfirstmessage Decode(byte[] data)",
		"internal sealed class MyfirstmessageVisitor : IVisitor {",
		"public void SequenceBegin(int id)", // flat-visitor nesting
		"public ulong someu64 = 18446744073709551615UL;",
		"public enum MyfirstmessageSomeenum : sbyte {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Messages.cs missing %q", want)
		}
	}
}

func TestCsDeterministic(t *testing.T) {
	if exampleModule(t) != exampleModule(t) {
		t.Fatal("C# generation not deterministic")
	}
}
