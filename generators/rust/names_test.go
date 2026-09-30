package rust

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// moduleDecl matches a declaration at the top level of src/message.rs (column
// 0), in either namespace: a type, a module, a constant, a re-export's name.
var moduleDecl = regexp.MustCompile(`(?m)^(?:pub )?(?:struct|enum|mod|const|static|type|trait|fn) (\w+)|^pub use \S+ as (\w+);`)

// declScopes splits src/message.rs into its top level ("") and the body of
// each per-message decoder module, whose items sit at column 0 as well. A
// module ends where its braces balance; generated string literals hold
// balanced braces only (`{:?}`).
func declScopes(text string) map[string]string {
	out := map[string]string{}
	var cur string
	depth := 0
	start := regexp.MustCompile(`^mod (_\w+__Decode) \{$`)
	for _, ln := range strings.Split(text, "\n") {
		if cur == "" {
			if m := start.FindStringSubmatch(ln); m != nil {
				cur, depth = m[1], 1
				out[""] += ln + "\n"
				continue
			}
			out[""] += ln + "\n"
			continue
		}
		depth += strings.Count(ln, "{") - strings.Count(ln, "}")
		if depth == 0 {
			cur = ""
			continue
		}
		out[cur] += ln + "\n"
	}
	return out
}

// harnessDecl matches a top-level function or import of the harness main.rs.
var harnessDecl = regexp.MustCompile(`(?m)^(?:pub )?fn (\w+)|^use \S+ as (\w+);`)

// TestNamesSchemaDeclaresEachNameOnce generates the shared name-collision
// schema (tests/conformance/lib/names.yaml, ARCHITECTURE §8 "Naming") on every
// profile and checks, without a toolchain, that every name the module and the
// harness declare at their top level is declared exactly once, and that every
// type the schema asks for is there. The conformance suite builds the same
// schema against the real corelibs.
func TestNamesSchemaDeclaresEachNameOnce(t *testing.T) {
	src, err := os.ReadFile("../../tests/conformance/lib/names.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []map[string]any{
		{"corelib": "rs", "emit": "project"},
		{"corelib": "rs", "allow_dynamic": false, "emit": "project"},
		{"corelib": "rs-no-std", "emit": "project"},
		{"corelib": "rs-no-std", "allow_dynamic": true, "emit": "project"},
	} {
		files, err := generateYAML(t, string(src), cfg)
		if err != nil {
			t.Fatalf("%v: generate: %v", cfg, err)
		}
		var mod, main string
		for _, f := range files {
			switch f.Path {
			case "src/message.rs":
				mod = string(f.Content)
			case "src/main.rs":
				main = string(f.Content)
			}
		}
		scopes := map[string]struct {
			text string
			re   *regexp.Regexp
		}{"main.rs": {main, harnessDecl}}
		for name, text := range declScopes(mod) {
			if name == "" {
				name = "message.rs"
			}
			scopes[name] = struct {
				text string
				re   *regexp.Regexp
			}{text, moduleDecl}
		}
		if len(scopes) < 3 {
			t.Fatalf("%v: no decoder module found", cfg)
		}
		for what, pair := range scopes {
			seen := map[string]int{}
			for _, m := range pair.re.FindAllStringSubmatch(pair.text, -1) {
				n := m[1] + m[2]
				seen[n]++
				if seen[n] == 2 {
					t.Errorf("%v: %s declares %s twice", cfg, what, n)
				}
			}
			if strings.HasPrefix(what, "_") {
				// A decoder module brings the schema's types in with
				// `use super::*`, which loses to a local: every local must
				// therefore be one no type identifier can spell.
				for n := range seen {
					if !strings.HasPrefix(n, "_") && n != "decode" && n != "try_decode" {
						t.Errorf("%v: %s declares %s, which a schema type could spell", cfg, what, n)
					}
				}
				continue
			}
			if what != "message.rs" {
				continue
			}
			for _, want := range []string{
				// path clashes
				"M", "M_A", "MA", "M_Decoder", "M__Decoder", "MDecoder", "MDecoder__Decoder",
				"M_Visitor", "M_New", "M_Arr", "M_ArrElem", "A_BC", "AB_C",
				"Point", "StructPoint", "Shape__DefaultNum", "Shape__DefaultPt", "ShapeDefaultPt",
				"Shape_Pt", "ShapeDefault", "ShapeDefault_Pt", "Color", "ColorRed", "Flags", "FlagsOn",
				// escapes
				"DecodeError", "DecodeError_", "Vec_", "Option_", "Result_", "String_", "Box_", "Self_",
				// corelib and harness names are plain types: nothing is imported
				"IStream", "OStream", "Visitor", "ArrayKind", "Serialize", "Read", "Std", "Sofab",
				// private channel
				"_M__Decode", "_DecodeError__Decode",
			} {
				if seen[want] == 0 {
					t.Errorf("%v: message.rs does not declare %s", cfg, want)
				}
			}
		}
	}
}
