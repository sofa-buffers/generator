package cpp

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// namespaceDecl matches a namespace-level declaration of a generated header:
// a struct, a scoped or unscoped enum, and -- inside an unscoped enum, whose
// enumerators are namespace-level names -- each bitfield flag.
var (
	typeDecl = regexp.MustCompile(`(?m)^(?:struct|enum class|enum) (\w+) :`)
	flagDecl = regexp.MustCompile(`(?m)^enum \w+ : [\w:]+ \{\n((?:    \w+ = [^\n]*\n)*)\};`)
	flagName = regexp.MustCompile(`(?m)^    (\w+) = `)
)

// TestNamesSchemaDeclaresEachNameOnce generates the shared name-collision
// schema (tests/conformance/lib/names.yaml, ARCHITECTURE §8 "Naming") on all
// four profiles and checks, without a toolchain, what the conformance suite
// checks by building it: every namespace-level name the headers declare --
// types, split union variants, bitfield flags -- is declared exactly once
// across all of them (the harness includes every header into one translation
// unit), and no two files share a case-folded path.
func TestNamesSchemaDeclaresEachNameOnce(t *testing.T) {
	src, err := os.ReadFile("../../tests/conformance/lib/names.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range unionProfiles {
		files := unionFiles(t, string(src), p.cfg)
		seen := map[string]string{}
		paths := map[string]string{}
		for path, content := range files {
			k := strings.ToLower(path)
			if prev, dup := paths[k]; dup {
				t.Errorf("%s: files %q and %q share a case-folded path", p.name, prev, path)
			}
			paths[k] = path
			if !strings.HasSuffix(path, ".hpp") || strings.HasPrefix(path, "harness/") {
				continue
			}
			var names []string
			for _, m := range typeDecl.FindAllStringSubmatch(content, -1) {
				names = append(names, m[1])
			}
			for _, blk := range flagDecl.FindAllStringSubmatch(content, -1) {
				for _, m := range flagName.FindAllStringSubmatch(blk[1], -1) {
					names = append(names, m[1])
				}
			}
			for _, n := range names {
				if prev, dup := seen[n]; dup {
					t.Errorf("%s: %s is declared in %s and again in %s", p.name, n, prev, path)
				}
				seen[n] = path
			}
		}
		// Spot-check the spellings the contract fixes, so a regression that
		// merely renames everything consistently still fails here.
		for _, want := range []string{
			"M", "M_A", "MA", "M_Decoder", "MDecoder", "M_Visitor", "M_New", "M_Arr", "M_ArrElem", "M_U",
			"Point", "StructPoint", "Color", "ColorRed", "Flags", "Flags_On", "Flags_Id", "FlagsOn",
			"Shape_default_Pt", "Shape_default_Num", "ShapeDefaultPt", "ShapeDefault", "ShapeDefault_Pt",
			"A_BC", "AB_C", "Message_", "Sofab", "Std", "Stdint", "Main", "Json",
		} {
			if _, ok := seen[want]; !ok {
				t.Errorf("%s: no namespace-level declaration %s", p.name, want)
			}
		}
		if _, ok := files["harness/_json.hpp"]; !ok {
			t.Errorf("%s: no harness/_json.hpp", p.name)
		}
	}
}

// TestHeaderFileNames: a message's header is its name lower-cased, underscores
// kept -- vehicle_telemetry and vehicleTelemetry cannot both exist (their folds
// match), so lower-casing alone keeps the headers apart -- and a stem Windows
// reserves as a device name takes a trailing underscore, which no other
// message's stem can end with. The harness includes each header by that name.
func TestHeaderFileNames(t *testing.T) {
	src := "version: 1\nmessages:\n" +
		"  vehicle_telemetry:\n    payload:\n      a: { id: 0, type: u8 }\n" +
		"  Com_1:\n    payload:\n      a: { id: 0, type: u8 }\n" +
		"  CON:\n    payload:\n      a: { id: 0, type: u8 }\n" +
		"  lpt9:\n    payload:\n      a: { id: 0, type: u8 }\n" +
		"  console:\n    payload:\n      a: { id: 0, type: u8 }\n"
	files := unionFiles(t, src, nil)
	harness := files["harness/_json.hpp"]
	for _, want := range []string{"vehicle_telemetry.hpp", "com_1.hpp", "con_.hpp", "lpt9_.hpp", "console.hpp"} {
		if _, ok := files[want]; !ok {
			t.Errorf("no header %s", want)
		}
		if !strings.Contains(harness, `#include "`+want+`"`) {
			t.Errorf("the harness does not include %s", want)
		}
	}
	for _, bad := range []string{"vehicletelemetry.hpp", "com1.hpp", "con.hpp", "lpt9.hpp"} {
		if _, ok := files[bad]; ok {
			t.Errorf("unexpected header %s", bad)
		}
	}
}
