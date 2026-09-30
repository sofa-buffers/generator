package python

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

// reservedNames is every name on the Python list, sorted, so the schema and the
// failure messages are stable.
func reservedNames() []string {
	var names []string
	for _, set := range []map[string]bool{pyKeywords, pyMembers, pyEvaluated, unionReserved} {
		for n := range set {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// reservedYAML is a schema that uses every reserved name as a field of a message,
// of a nested struct, and as an option of a union. The array, struct and union
// fields carry the highest ids, so their `field(default_factory=...)` defaults are
// evaluated in the class body AFTER every reserved name has been bound there --
// the order in which a field `field` or `list` breaks the class.
func reservedYAML(names []string) string {
	var fields strings.Builder
	for i, n := range names {
		fmt.Fprintf(&fields, "      %q: { id: %d, type: u8 }\n", n, i)
	}
	opts := make([]string, len(names))
	for i, n := range names {
		opts[i] = fmt.Sprintf("%q: { id: %d, type: u8 }", n, i)
	}
	k := len(names)
	return fmt.Sprintf(`version: 1
$defs:
  struct:
    Inner:
%s      arr:   { id: %d, type: array, items: { type: u8, count: 2 } }
messages:
  m:
    payload:
%s      arr:   { id: %d, type: array, items: { type: u8, count: 2 } }
      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
`, fields.String(), k, fields.String(), k, k+1, k+2, strings.Join(opts, ", "))
}

// TestReservedNamesAreMangled: every name on the list comes out with a trailing
// underscore as a dataclass field -- except the members only a union has, which
// a dataclass field keeps -- and the JSON key keeps the schema name.
func TestReservedNamesAreMangled(t *testing.T) {
	names := reservedNames()
	mod := string(genPy(t, schema(t, reservedYAML(names)), map[string]any{})["message.py"])
	for _, n := range names {
		want := n + "_"
		if unionReserved[n] {
			want = n
		}
		if !strings.Contains(mod, "\n    "+want+": int = 0\n") {
			t.Errorf("field %q is not the member %s", n, want)
		}
		if !strings.Contains(mod, fmt.Sprintf("%q: ", n)) {
			t.Errorf("JSON key %q is gone", n)
		}
	}
}

// reservedDriver imports the generated module and round-trips one message whose
// every reserved-name field, nested one and union option holds a non-default
// value. Importing is the check that matters: a rebound `classmethod` or `field`
// fails at import, a rebound `list` at construction, and a field the method of
// the same name shadows (`encode`) at the encode call.
const reservedDriver = `
import json, sys
sys.path.insert(0, sys.argv[1])
import message
attrs = json.loads(sys.argv[2])
m = message.M()
for i, a in enumerate(attrs):
    setattr(m, a, i + 1)
    setattr(m.inner, a, i + 1)
m.arr = [1, 2]
m.inner.arr = [3, 4]
m.u.` + "%s" + ` = 9
back = message.M.decode(m.encode())
assert back.to_jsonable() == m.to_jsonable(), (back.to_jsonable(), m.to_jsonable())
print("ok")
`

// TestReservedNamesImport: the collision test. Every name on the list, used as a
// field, must give a module that imports and round-trips. The generator exits 0
// on a broken module, so only running it catches one. Gated on SOFAB_PY_CORELIB.
func TestReservedNamesImport(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	names := reservedNames()
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, reservedYAML(names)), map[string]any{}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	attrs := make([]string, len(names))
	for i, n := range names {
		attrs[i] = pyIdent(n)
	}
	js, _ := json.Marshal(attrs)
	// The last option (by id) holds a value, so the union round-trips non-default.
	driver := fmt.Sprintf(reservedDriver, unionOptProp(names[len(names)-1]))
	cmd := exec.Command(py, "-c", driver, dir, string(js))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(corelib, "src"))
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("module with every reserved name as a field does not import/round-trip: %v\n%s", err, out)
	}
}
