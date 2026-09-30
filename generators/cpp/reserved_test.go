package cpp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// reservedNames is every name on the C++ list, sorted, so the schema and the
// failure messages are stable.
func reservedNames() []string {
	var names []string
	for _, set := range []map[string]bool{cppKeywords, cppMembers, unionReserved} {
		for n := range set {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// reservedYAML is a schema that uses every reserved name as a field of a message,
// of a nested struct, and as an option of a union.
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
%s      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
`, fields.String(), fields.String(), k, k+1, strings.Join(opts, ", "))
}

// TestReservedNamesAreMangled: every name on the list comes out with a trailing
// underscore as a struct or message member -- except the members only a union
// has, which a struct member keeps.
func TestReservedNamesAreMangled(t *testing.T) {
	names := reservedNames()
	h := unionFiles(t, reservedYAML(names), nil)["m.hpp"]
	for _, n := range names {
		want := n + "_"
		if unionReserved[n] {
			want = n
		}
		if !strings.Contains(h, "    std::uint8_t "+want+" = 0;\n") {
			t.Errorf("field %q is not the member %s", n, want)
		}
	}
}

// TestReservedNamesCompile: the collision test. Every name on the list, used as
// a field, must give code that compiles -- types and project harness -- under
// -Wall -Wextra -Werror on all four profiles. The generator exits 0 on code that
// does not compile, so only compiling it catches a missing entry. Gated on the
// corelib checkouts (SOFAB_CPP_DIR, SOFAB_C_DIR), like the union compile gate.
func TestReservedNamesCompile(t *testing.T) {
	cpp, cc := os.Getenv("SOFAB_CPP_DIR"), os.Getenv("SOFAB_C_DIR")
	if cpp == "" || cc == "" {
		t.Skip("set SOFAB_CPP_DIR and SOFAB_C_DIR to corelib checkouts to run the compile gate")
	}
	gxx, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("g++ not found")
	}
	src := reservedYAML(reservedNames())
	for _, p := range unionProfiles {
		dir := t.TempDir()
		for path, content := range unionFiles(t, src, p.cfg) {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		inc := "-I" + filepath.Join(cpp, "include")
		if p.cfg["corelib"] == "c-cpp" {
			inc = "-I" + filepath.Join(cc, "src", "include")
		}
		args := []string{"-std=c++20", "-Wall", "-Wextra", "-Werror", "-fsyntax-only", inc,
			"-I" + filepath.Join(cc, "test", "shared"), "-I" + dir, filepath.Join(dir, "harness", "main.cpp")}
		if out, err := exec.Command(gxx, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: every reserved name as a field does not compile:\n%s", p.name, out)
		}
	}
}
