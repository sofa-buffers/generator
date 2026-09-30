package c

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// reservedNames is every name on the C list, sorted.
func reservedNames() []string {
	var names []string
	for _, set := range []map[string]bool{cKeywords, cHeaderMacros} {
		for n := range set {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// reservedYAML is a schema that uses every reserved name as a field of a message,
// of a nested struct, and as an option of a union. A blob gives the message a
// length companion too. Names are quoted: `true`/`false` are YAML booleans.
func reservedYAML(names []string) string {
	var fields strings.Builder
	opts := make([]string, len(names))
	for i, n := range names {
		fmt.Fprintf(&fields, "      %q: { id: %d, type: u8 }\n", n, i)
		opts[i] = fmt.Sprintf("%q: { id: %d, type: u8 }", n, i)
	}
	k := len(names)
	return fmt.Sprintf(`version: 1
$defs:
  struct:
    Inner:
%smessages:
  m:
    payload:
%s      b:     { id: %d, type: blob, maxlen: 4 }
      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
`, fields.String(), fields.String(), k, k+1, k+2, strings.Join(opts, ", "))
}

// TestReservedNamesAreMangled: every name on the list is a member with a
// trailing underscore, in the message and in the nested struct.
func TestReservedNamesAreMangled(t *testing.T) {
	names := reservedNames()
	h := genCFromYAMLCfg(t, reservedYAML(names), map[string]any{})["m.h"]
	for _, n := range names {
		// Anchored on the 4-space indent: the union's options sit at 8.
		if got := strings.Count(h, "\n    uint8_t "+n+"_;\n"); got != 2 {
			t.Errorf("field %q is the member %s_ in %d of the 2 structs", n, n, got)
		}
	}
}

// TestMemberNameCollision: two fields that derive one member -- a mangled
// keyword landing on a field of that name, or a field named after a sibling's
// length companion -- are a generation error naming both; so is a field named
// after a macro the backend defines.
func TestMemberNameCollision(t *testing.T) {
	const dup, mac = "both generate the member", "also the generated macro"
	for _, c := range []struct{ a, b, want string }{
		{"int: { id: 0, type: u8 }", "int_: { id: 1, type: u8 }", dup},
		{"b: { id: 0, type: blob, maxlen: 4 }", "b_len: { id: 1, type: u8 }", dup},
		{"a: { id: 0, type: array, items: { type: u8, count: 2 } }", "a_len: { id: 1, type: u8 }", dup},
		// the backend's own macros: MAX_SIZE, the include guard, an option id
		{"MESSAGE_M_MAX_SIZE: { id: 0, type: u8 }", "x: { id: 1, type: u8 }", mac},
		{"MESSAGE_M_H: { id: 0, type: u8 }", "x: { id: 1, type: u8 }", mac},
		{"u: { id: 0, type: union, oneof: { p: { id: 0, type: u8 }, q: { id: 1, type: u8 } } }", "MESSAGE_M_U_P_ID: { id: 1, type: u8 }", mac},
		// ...and an option landing on one
		{"u: { id: 0, type: union, oneof: { MESSAGE_M_H: { id: 0, type: u8 } } }", "x: { id: 1, type: u8 }", mac},
	} {
		src := fmt.Sprintf("version: 1\nmessages:\n  m:\n    payload:\n      %s\n      %s\n", c.a, c.b)
		_, err := (&Backend{}).Generate(schemaFromYAML(t, src), map[string]any{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s / %s: want an error containing %q, got %v", c.a, c.b, c.want, err)
		}
	}
}

// TestReservedNamesBuild: the collision test. Every name on the list, used as a
// field, must give a project that builds under the strict warning set, compiles
// as C23 and as gcc's default gnu17 too, and round-trips every value under its
// schema name.
// Gated on SOFAB_C_CORELIB + make + gcc.
func TestReservedNamesBuild(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to run the reserved-name build gate")
	}
	for _, tool := range []string{"make", "gcc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	names := reservedNames()
	dir := t.TempDir()
	for path, content := range genCFromYAMLCfg(t, reservedYAML(names), map[string]any{"emit": "project"}) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, strictMakeVar).CombinedOutput(); err != nil {
		t.Fatalf("every reserved name as a field does not build:\n%s", out)
	}
	// C23 makes `true`, `nullptr`, `typeof` keywords and adds the *_WIDTH
	// macros; gnu17 (gcc's default) predefines `linux`/`unix` and lets glibc
	// add its own. The project's Makefile builds as C99, which has neither.
	for _, std := range []string{"c2x", "gnu17"} {
		cc := exec.Command("gcc", "-std="+std, "-fsyntax-only", "-Wall", "-Wextra", "-Werror",
			"-I"+filepath.Join(corelib, "src", "include"), "-I"+filepath.Join(dir, "generated"), filepath.Join(dir, "generated", "m.c"))
		if out, err := cc.CombinedOutput(); err != nil {
			t.Fatalf("every reserved name as a field does not compile as -std=%s:\n%s", std, out)
		}
	}

	top := map[string]any{"b": []int{1, 2}}
	inner := map[string]any{}
	for i, n := range names {
		top[n] = i + 1
		inner[n] = i + 2
	}
	top["inner"] = inner
	last := names[len(names)-1]
	top["u"] = map[string]any{last: 7}
	in, _ := json.Marshal(top)
	harness := filepath.Join(dir, "harness", "harness")
	enc := exec.Command(harness, "encode")
	enc.Stdin = strings.NewReader(string(in))
	wire, err := enc.Output()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := exec.Command(harness, "decode")
	dec.Stdin = strings.NewReader(string(wire))
	out, err := dec.Output()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode output is not JSON: %v\n%s", err, out)
	}
	gotInner, _ := got["inner"].(map[string]any)
	for i, n := range names {
		if got[n] != float64(i+1) || gotInner[n] != float64(i+2) {
			t.Errorf("field %q did not round-trip under its schema name: top %v, inner %v", n, got[n], gotInner[n])
		}
	}
	if u, _ := got["u"].(map[string]any); u[last] != float64(7) {
		t.Errorf("union option %q did not round-trip under its schema name: %v", last, got["u"])
	}
	if b, _ := json.Marshal(got["b"]); string(b) != "[1,2]" {
		t.Errorf("blob b did not round-trip: %s", b)
	}
}
