package golang

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reservedNames is every name on the Go list, sorted, so the schema and the
// failure messages are stable. Each is spelled as the Go member it would be;
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

// reservedYAML is a schema that uses every reserved name as a field of a message,
// of a nested struct, and as an option of a union. Both types also carry a string
// field, so both embed sofab.StringCheck and the names it promotes are live.
//
// A union option is left out where a DERIVED accessor lands on the list
// (`StringCheck` derives SetStringCheck): only the getter is mangled, so that
// option is a generation error by design (checkUnionNames).
func reservedYAML(names []string) string {
	var fields strings.Builder
	var opts []string
	for i, n := range names {
		fmt.Fprintf(&fields, "      %s: { id: %d, type: u8 }\n", n, i)
		if !goReserved("Set"+n) && !goReserved("Has"+n) {
			opts = append(opts, fmt.Sprintf("%s: { id: %d, type: u8 }", n, i))
		}
	}
	k := len(names)
	return fmt.Sprintf(`version: 1
$defs:
  struct:
    Inner:
%s      text:  { id: %d, type: string, maxlen: 8 }
messages:
  m:
    payload:
%s      text:  { id: %d, type: string, maxlen: 8 }
      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
`, fields.String(), k, fields.String(), k, k+1, k+2, strings.Join(opts, ", "))
}

// TestReservedNamesAreMangled: every name on the list comes out with a trailing
// underscore as a struct field -- except the members only a union has, which a
// struct field keeps -- and the json tag keeps the schema name.
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
}

// TestFieldNameFoldingCollision: exported() folds underscores into camel case,
// so two schema names can derive one Go field -- directly (`a_b`, `aB`) or
// through the mangling (`encode_` lands on the mangled `encode`). That is a
// generation error naming both fields, not a duplicate field in the output.
func TestFieldNameFoldingCollision(t *testing.T) {
	for _, pair := range [][2]string{{"a_b", "aB"}, {"encode", "encode_"}} {
		src := fmt.Sprintf("version: 1\nmessages:\n  m:\n    payload:\n      %s: { id: 0, type: u8 }\n      %s: { id: 1, type: u8 }\n", pair[0], pair[1])
		_, err := (&Backend{}).Generate(schemaFromYAMLString(t, src), nil)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("fields %q and %q", pair[0], pair[1])) {
			t.Errorf("%v: want a collision error naming both fields, got %v", pair, err)
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
	last := names[len(names)-1]
	top["u"] = map[string]any{last: 9}
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
	if u, _ := got["u"].(map[string]any); u[last] != float64(9) {
		t.Errorf("union option %q did not round-trip: %v", last, got["u"])
	}
}

// TestConstNameCollision: two enum constants or bitfield flags that give one
// generated name (`a_b` and `aB` are both AB) are a generation error naming both.
func TestConstNameCollision(t *testing.T) {
	for _, src := range []string{
		"version: 1\nmessages:\n  m:\n    payload:\n      e: { id: 0, type: enum, enum: { a_b: 0, aB: 1 } }\n",
		"version: 1\n$defs:\n  bitfield:\n    F: { x_y: { pos: 0 }, xY: { pos: 1 } }\nmessages:\n  m:\n    payload:\n      f: { id: 0, type: bitfield, bits: { $ref: '#/$defs/bitfield/F' } }\n",
	} {
		_, err := (&Backend{}).Generate(schemaFromYAMLString(t, src), nil)
		if err == nil || !strings.Contains(err.Error(), "both generate") {
			t.Errorf("want a collision error for:\n%s\ngot %v", src, err)
		}
	}
}

// TestCrossTypeConstNameCollision: enum constants and bitfield flags are prefixed with their type and
// live package-wide, so a constant of one type and one of another can spell one
// name (`E.a_b` and `EA.b`, `F.a_b` and `FA.b`) -- a generation error.
func TestCrossTypeConstNameCollision(t *testing.T) {
	for _, src := range []string{
		"version: 1\n$defs:\n  enum:\n    E: { a_b: 0 }\n    EA: { b: 0 }\nmessages:\n  m:\n    payload:\n      e: { id: 0, type: enum, enum: { $ref: '#/$defs/enum/E' } }\n      f: { id: 1, type: enum, enum: { $ref: '#/$defs/enum/EA' } }\n",
		"version: 1\n$defs:\n  bitfield:\n    F: { a_b: { pos: 0 } }\n    FA: { b: { pos: 0 } }\nmessages:\n  m:\n    payload:\n      f: { id: 0, type: bitfield, bits: { $ref: '#/$defs/bitfield/F' } }\n      g: { id: 1, type: bitfield, bits: { $ref: '#/$defs/bitfield/FA' } }\n",
	} {
		_, err := (&Backend{}).Generate(schemaFromYAMLString(t, src), nil)
		if err == nil || !strings.Contains(err.Error(), "both generate") {
			t.Errorf("want a collision error for:\n%s\ngot %v", src, err)
		}
	}
}
