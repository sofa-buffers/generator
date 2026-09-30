package c

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// declPatterns find every name a generated file declares at file scope: a
// typedef, a macro, a descriptor or table, a function definition, a harness
// static. Prototypes and forward declarations end in ";" and are not matched,
// so a name declared in a header and defined in its source counts once.
var declPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^#define (\w+)`),
	regexp.MustCompile(`(?m)^\}\s*(\w+);$`),                                 // typedef struct { ... } NAME;
	regexp.MustCompile(`(?m)^typedef [^\n]*\}\s*(\w+);$`),                   // one-line typedef struct
	regexp.MustCompile(`(?m)^typedef char (\w+)\[`),                         // compile-time assertion
	regexp.MustCompile(`(?m)^(?:static )?const [\w *]*?\b(\w+)(?:\[\])? =`), // descriptor, table, image
	regexp.MustCompile(`(?m)^(?:static )?[\w ]+?[ *](\w+)\([^;\n]*\) \{$`),  // function definition
	regexp.MustCompile(`(?m)^static [\w ]+? (\w+)(?:\[[^\]\n]*\])?;$`),      // harness state
}

// TestNamesSchemaDeclaresEachNameOnce generates the shared name-collision
// schema (tests/conformance/lib/names.yaml, ARCHITECTURE §8 "Naming") as a
// project and asserts that every name any generated file declares at file
// scope is declared exactly once across all of them: C has one namespace for
// every typedef, macro, function and external object a translation unit or the
// link sees. This is the toolchain-free half of the guarantee; the conformance
// suite builds the same schema against the real corelib.
func TestNamesSchemaDeclaresEachNameOnce(t *testing.T) {
	def := filepath.Join("..", "..", "tests", "conformance", "lib", "names.yaml")
	doc, err := parser.Load(def)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("names.yaml must validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"message_", "sofab_"} {
		files, err := (&Backend{}).Generate(s, map[string]any{"emit": "project", "symbol_prefix": prefix})
		if err != nil {
			t.Fatalf("prefix %s: names.yaml must generate: %v", prefix, err)
		}
		where := map[string][]string{}
		for _, f := range files {
			if !strings.HasSuffix(f.Path, ".c") && !strings.HasSuffix(f.Path, ".h") {
				continue
			}
			for _, re := range declPatterns {
				for _, m := range re.FindAllStringSubmatch(string(f.Content), -1) {
					where[m[1]] = append(where[m[1]], f.Path)
				}
			}
		}
		if len(where) < 1000 {
			t.Fatalf("prefix %s: found only %d declarations; the patterns no longer match the output", prefix, len(where))
		}
		var dups []string
		for name, at := range where {
			if len(at) != 1 {
				dups = append(dups, name+" in "+strings.Join(at, ", "))
			}
		}
		sort.Strings(dups)
		for _, d := range dups {
			t.Errorf("prefix %s: declared more than once: %s", prefix, d)
		}
		// The ones names.yaml exists for.
		for _, want := range []string{
			prefix + "m___a_t", prefix + "m_a_t", // inline m.a beside message m_a
			prefix + "m___decoder_t", prefix + "m__decoder_t", prefix + "m_decoder_t",
			prefix + "m___arr_t", prefix + "m___arr__elems_t", prefix + "m___arr_elem_t",
			prefix + "a___b_c_t", prefix + "a_b___c_t",
			prefix + "point_t", prefix + "struct_point_t",
			prefix + "shape__default_pt_t", prefix + "shape__default_num_t", prefix + "shape_default___pt_t", prefix + "shape_default_pt_t",
			strings.ToUpper(prefix) + "FLAGS___ON", strings.ToUpper(prefix) + "FLAGS_ON__H",
		} {
			if len(where[want]) != 1 {
				t.Errorf("prefix %s: %s declared %d times, want once", prefix, want, len(where[want]))
			}
		}
	}
}
