package golang

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reservedNames is every name on the Go member list, sorted, so the schema and
// the failure messages are stable. Each is spelled as the Go member it would be;
// exported() leaves such a name as it is.
func reservedNames() []string {
	var names []string
	for _, set := range []map[string]bool{goVisitorMembers, goStringCheckMembers, goMembers, unionReserved} {
		for n := range set {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// packageNames is every name on the type-level escape list (goPackageNames),
// sorted, as the schema name that spells it.
func packageNames() []string {
	var names []string
	for n := range goPackageNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// reservedYAML is a schema that uses every reserved member name as a field of a
// message, of a nested struct, and as an option of a union. Both types also
// carry a string field, so both embed sofab.StringCheck and the names it
// promotes are live -- including SetStringCheck, which the option
// `StringCheck`'s setter would hide (it is Set_StringCheck).
//
// Every name on the type-level escape list is a type too, each carrying the
// unbounded field that makes the package export the constant of that name:
// the first as a $defs struct, the others as messages.
func reservedYAML(names []string) string {
	var fields strings.Builder
	var opts []string
	for i, n := range names {
		fmt.Fprintf(&fields, "      %s: { id: %d, type: u8 }\n", n, i)
		opts = append(opts, fmt.Sprintf("%s: { id: %d, type: u8 }", n, i))
	}
	k := len(names)
	pkg := packageNames() // MaxDynArrayCount, MaxDynBlobLen, MaxDynStringLen
	return fmt.Sprintf(`version: 1
$defs:
  struct:
    Inner:
%s      text:  { id: %d, type: string, maxlen: 8 }
    %s:
      a: { id: 0, type: array, items: { type: u8 } }
messages:
  m:
    payload:
%s      text:  { id: %d, type: string, maxlen: 8 }
      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
      dyn:   { id: %d, type: struct, fields: { $ref: '#/$defs/struct/%s' } }
  %s:
    payload:
      b: { id: 0, type: blob }
  %s:
    payload:
      s: { id: 0, type: string }
`, fields.String(), k, pkg[0], fields.String(), k, k+1, k+2, strings.Join(opts, ", "), k+3, pkg[0], pkg[1], pkg[2])
}

// TestReservedNamesAreMangled: every name on the member list comes out with a
// trailing underscore as a struct field -- except the members only a union has,
// which a struct field keeps -- and the json tag keeps the schema name. Every
// name on the type-level list comes out as a type with a trailing underscore,
// beside the constant it would otherwise redeclare.
func TestReservedNamesAreMangled(t *testing.T) {
	names := reservedNames()
	var out string
	for _, content := range genGo(t, schemaFromYAMLString(t, reservedYAML(names)), nil) {
		out += content
	}
	for _, n := range names {
		want := n + "_"
		if unionReserved[n] {
			want = n
		}
		if !regexp.MustCompile(fmt.Sprintf("\t%s +uint8 +`json:%q`\n", regexp.QuoteMeta(want), n)).MatchString(out) {
			t.Errorf("field %q is not the Go field %s with json tag %q", n, want, n)
		}
	}
	for _, n := range packageNames() {
		if !strings.Contains(out, "type "+n+"_ struct {") {
			t.Errorf("type %q is not escaped to %s_", n, n)
		}
		if !regexp.MustCompile(`(?m)^\t` + n + ` += \d+$`).MatchString(out) {
			t.Errorf("the constant %s is not declared, so its escape is not exercised", n)
		}
	}
}

// TestReservedNamesBuild: the collision test. Every name on the list, used as a
// field, must give a package that vets, builds and round-trips every value
// under its schema name. The generator exits 0 on code that does not compile,
// so only building it catches a missing entry. Gated on SOFAB_GO_CORELIB.
func TestReservedNamesBuild(t *testing.T) {
	corelib := requireGoCorelib(t)
	names := reservedNames()
	bin, err := buildGoHarness(t, corelib, reservedYAML(names))
	if err != nil {
		t.Fatalf("every reserved name as a field does not build:\n%v", err)
	}
	top := map[string]any{"text": "hi"}
	inner := map[string]any{"text": "yo"}
	for i, n := range names {
		top[n] = i + 1
		inner[n] = i + 2
	}
	top["inner"] = inner
	top["dyn"] = map[string]any{"a": []int{1, 2}}
	in, _ := json.Marshal(top)

	var got map[string]any
	if err := json.Unmarshal([]byte(roundTrip(t, bin, "m", string(in))), &got); err != nil {
		t.Fatal(err)
	}
	gotInner, _ := got["inner"].(map[string]any)
	for i, n := range names {
		if got[n] != float64(i+1) || gotInner[n] != float64(i+2) {
			t.Errorf("field %q did not round-trip under its schema name: top %v, inner %v", n, got[n], gotInner[n])
		}
	}
	// Every option, one decode each: the setter and getter of each one is what
	// the round trip goes through.
	for i, n := range names {
		in, _ := json.Marshal(map[string]any{"u": map[string]any{n: i + 1}})
		var got map[string]any
		if err := json.Unmarshal([]byte(roundTrip(t, bin, "m", string(in))), &got); err != nil {
			t.Fatal(err)
		}
		if u, _ := got["u"].(map[string]any); u[n] != float64(i+1) {
			t.Errorf("union option %q did not round-trip: %v", n, got["u"])
		}
	}
	pkg := packageNames()
	if out := roundTrip(t, bin, pkg[1], `{"b":"AQI="}`); !strings.Contains(out, `"AQI="`) {
		t.Errorf("message %s did not round-trip: %s", pkg[1], out)
	}
	if out := roundTrip(t, bin, pkg[2], `{"s":"hi"}`); !strings.Contains(out, `"hi"`) {
		t.Errorf("message %s did not round-trip: %s", pkg[2], out)
	}
}

// TestFoldedNamesAreDistinct: the spellings that used to fold to one Go name
// and were refused -- a constant of one enum against one of another (`E.a_b`
// and `EA.b` were both EAB), likewise for bitfield flags -- are children of
// their type now (E_AB, EA_B), so they generate without an error.
func TestFoldedNamesAreDistinct(t *testing.T) {
	for _, tc := range []struct{ src, want1, want2 string }{
		{"version: 1\n$defs:\n  enum:\n    E: { a_b: 0 }\n    EA: { b: 0 }\nmessages:\n  m:\n    payload:\n      e: { id: 0, type: enum, enum: { $ref: '#/$defs/enum/E' } }\n      f: { id: 1, type: enum, enum: { $ref: '#/$defs/enum/EA' } }\n",
			"\tE_AB E = 0", "\tEA_B EA = 0"},
		{"version: 1\n$defs:\n  bitfield:\n    F: { a_b: { pos: 0 } }\n    FA: { b: { pos: 0 } }\nmessages:\n  m:\n    payload:\n      f: { id: 0, type: bitfield, bits: { $ref: '#/$defs/bitfield/F' } }\n      g: { id: 1, type: bitfield, bits: { $ref: '#/$defs/bitfield/FA' } }\n",
			"\tF_AB F = 1 << 0", "\tFA_B FA = 1 << 0"},
	} {
		types := genGo(t, schemaFromYAMLString(t, tc.src), nil)["sofab_types.go"]
		for _, w := range []string{tc.want1, tc.want2} {
			if !strings.Contains(types, w) {
				t.Errorf("missing %q in:\n%s", w, types)
			}
		}
	}
}
