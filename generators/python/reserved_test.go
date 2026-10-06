package python

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reservedNames is every name on the Python member list, sorted, so the schema
// and the failure messages are stable.
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

// typeReservedNames is every name on the type-level list, sorted.
func typeReservedNames() []string {
	var names []string
	for n := range pyTypeReserved {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// trie is a tree of name segments: a type-level reserved name with `_` in it
// (MAX_FIELD_SPAN) is reached by a PATH, message MAX > field FIELD > field SPAN,
// whose type identifier joins the segments with `_`.
type trie map[string]trie

func (t trie) add(segs []string) {
	if len(segs) == 0 {
		return
	}
	c, ok := t[segs[0]]
	if !ok {
		c = trie{}
		t[segs[0]] = c
	}
	c.add(segs[1:])
}

// yaml renders the node's payload: a u8 `x`, then one inline struct per child.
func (t trie) yaml() string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{"x: { id: 0, type: u8 }"}
	for i, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: { id: %d, type: struct, fields: { %s } }", k, i+1, t[k].yaml()))
	}
	return strings.Join(parts, ", ")
}

// typeReservedMessages renders one message per first segment of the type-level
// list, so every listed name is some type's identifier: a message (`Status`),
// or an inline struct down a path (`MAX_FIELD_SPAN`).
func typeReservedMessages() (string, []string) {
	root := trie{}
	for _, n := range typeReservedNames() {
		root.add(strings.Split(n, "_"))
	}
	keys := make([]string, 0, len(root))
	for k := range root {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: { payload: { %s } }\n", k, root[k].yaml())
	}
	return b.String(), keys
}

// reservedYAML is a schema that uses every reserved member name as a field of a
// message, of a nested struct, and as an option of a union; and every
// type-level reserved name as a type identifier. The array, struct and union
// fields carry the highest ids, so their `field(default_factory=...)` defaults
// are evaluated in the class body AFTER every reserved name has been bound there
// -- the order in which a field `field` or `list` breaks the class. The fields
// `Color` and `bytes` come before the enum and blob defaults the class body
// evaluates, which must not read either name.
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
	msgs, _ := typeReservedMessages()
	return fmt.Sprintf(`version: 1
$defs:
  enum:
    color: { red: 0, green: 1, blue: 2 }
  struct:
    Inner:
%s      arr:   { id: %d, type: array, items: { type: u8, count: 2 } }
messages:
  m:
    payload:
%s      arr:   { id: %d, type: array, items: { type: u8, count: 2 } }
      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
      Color: { id: %d, type: u8 }
      bytes: { id: %d, type: u8 }
      col:   { id: %d, type: enum, enum: { $ref: '#/$defs/enum/color' }, default: 2 }
      bl:    { id: %d, type: blob, default: "AQID" }
%s`, fields.String(), k, fields.String(), k, k+1, k+2, strings.Join(opts, ", "),
		k+3, k+4, k+5, k+6, msgs)
}

// TestReservedNamesAreMangled: every name on the member list comes out with a
// trailing underscore as a dataclass field -- except the members only a union
// has, which a dataclass field keeps -- and the JSON key keeps the schema name.
// Every name on the type-level list comes out as a class with a trailing
// underscore.
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
	for _, n := range typeReservedNames() {
		if !strings.Contains(mod, "\nclass "+n+"_:\n") {
			t.Errorf("type %s is not the class %s_", n, n)
		}
		if strings.Contains(mod, "\nclass "+n+":\n") {
			t.Errorf("type %s rebinds the module-level %s", n, n)
		}
	}
	// The class body reads the enum through its alias and the blob default as a
	// literal, so the fields `Color` and `bytes` before them cannot rebind them.
	for _, want := range []string{"    col: Color = _Color__Enum(2)\n", "    bl: bytes = b\"\\x01\\x02\\x03\"\n"} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// reservedDriver imports the generated module and round-trips one message whose
// every reserved-name field, nested one and union option holds a non-default
// value. Importing is the check that matters: a rebound `classmethod` or `field`
// fails at import, a rebound `list` at construction, and a field the method of
// the same name shadows (`encode`) at the encode call. Then every type-level
// class round-trips, and the module's own names are still the module's.
const reservedDriver = `
import json, sys
sys.path.insert(0, sys.argv[1])
import message, sofab
attrs = json.loads(sys.argv[2])
m = message.M()
for i, a in enumerate(attrs):
    setattr(m, a, i + 1)
    setattr(m.inner, a, i + 1)
m.arr = [1, 2]
m.inner.arr = [3, 4]
m.u.` + "%s" + ` = 9
m.Color = 5
m.bytes = 6
m.col = message.Color.RED
m.bl = b"x"
back = message.M.decode(m.encode())
assert back.to_jsonable() == m.to_jsonable(), (back.to_jsonable(), m.to_jsonable())
assert message.M().col == message.Color.BLUE and message.M().bl == b"\x01\x02\x03"
for cls in json.loads(sys.argv[3]):
    c = getattr(message, cls)
    o = c.from_jsonable({"x": 3})
    got = c.decode(o.encode())
    assert got.to_jsonable() == o.to_jsonable(), (cls, got.to_jsonable())
    d = c.decoder()
    assert d.feed(o.encode()) is sofab.Status.COMPLETE, cls
for n in ("Decoder", "Encoder", "Status", "Visitor", "SofaDecodeError", "SofaIncompleteError"):
    assert getattr(message, n) is getattr(sofab, n), n
assert message.REASSEMBLY > message.MAX_FIELD_SPAN > 0
print("ok")
`

// TestReservedNamesImport: the collision test. Every name on the lists, used as
// a field or as a type, must give a module that imports and round-trips. The
// generator exits 0 on a broken module, so only running it catches one. Gated
// on SOFAB_PY_CORELIB.
func TestReservedNamesImport(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	pyEngine(t, corelib)
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
	_, tops := typeReservedMessages()
	classes := make([]string, len(tops))
	for i, n := range tops {
		classes[i] = escapeType(n)
	}
	cs, _ := json.Marshal(classes)
	// The last option (by id) holds a value, so the union round-trips non-default.
	driver := fmt.Sprintf(reservedDriver, unionOptProp(names[len(names)-1]))
	cmd := exec.Command(py, "-W", "error", "-c", driver, dir, string(js), string(cs))
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(corelib, "src"))
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("module with every reserved name as a field or type does not import/round-trip: %v\n%s", err, out)
	}
}

var (
	importStmt = regexp.MustCompile(`(?m)^from \S+ import (.+)$`)
	upperConst = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*) = `)
)

// TestTypeReservedCoversImports: the type-level list is complete -- every
// upper-case name a generated module imports or states as a module constant is
// on it, so no type can be spelled like one and rebind it. Swept over the
// example, the shared names schema, the reserved schema and a schema with an
// unbounded string, blob and wrapper array (the three MAX_DYN_* constants are
// emitted only for an unbounded field of their kind). The `from sofab import`
// line is also checked through sofabImports on a decode text that takes every
// branch, so a name no schema reaches today (SofaLimitError) is held too.
func TestTypeReservedCoversImports(t *testing.T) {
	for _, s := range []string{"../../examples/messages/example.yaml", "../../tests/conformance/lib/names.yaml"} {
		checkTypeReservedCovers(t, s, string(genPy(t, schemaFile(t, s), map[string]any{})["message.py"]))
	}
	checkTypeReservedCovers(t, "reserved", string(genPy(t, schema(t, reservedYAML(reservedNames())), map[string]any{})["message.py"]))
	unbounded := string(genPy(t, schema(t, unboundedYAML), map[string]any{})["message.py"])
	for _, c := range []string{"MAX_DYN_ARRAY_COUNT = ", "MAX_DYN_STRING_LEN = ", "MAX_DYN_BLOB_LEN = "} {
		if !strings.Contains(unbounded, "\n"+c) {
			t.Errorf("unbounded schema: no %q line -- the sweep no longer reaches it", c)
		}
	}
	checkTypeReservedCovers(t, "unbounded", unbounded)

	every := sofabImports("raise SofaLimitError(x)\nreserve_elem(a, b, UNBOUNDED, c)\nreserve_leaf(\nreserve_row(\n"+
		"t = (Binding(closed=True),)\ndef on_field(self, fld: Field):\nWireType.FIXLEN, FixlenSubtype.FP32\n",
		"_M__Def__a = FloatArrayDefault([0.0])\n")
	if want := []string{"Binding", "Decoder", "Encoder", "Field", "FixlenSubtype", "FloatArrayDefault", "SofaDecodeError", "SofaIncompleteError",
		"SofaLimitError", "Status", "UNBOUNDED", "Visitor", "WireType", "reserve_elem", "reserve_leaf", "reserve_row"}; strings.Join(every, " ") != strings.Join(want, " ") {
		t.Errorf("sofabImports on every trigger = %v, want %v (extend the trigger text with the new branch)", every, want)
	}
	checkTypeReservedCovers(t, "every sofab import", "from sofab import "+strings.Join(every, ", ")+"\n")
}

// unboundedYAML has one unbounded field of each receiver-capped kind.
const unboundedYAML = `version: 1
messages:
  m:
    payload:
      s: { id: 0, type: string }
      b: { id: 1, type: blob }
      arr: { id: 2, type: array, items: { type: string } }
`

func checkTypeReservedCovers(t *testing.T, label, mod string) {
	t.Helper()
	var names []string
	for _, m := range importStmt.FindAllStringSubmatch(mod, -1) {
		for _, n := range strings.Split(m[1], ",") {
			names = append(names, strings.TrimSpace(n))
		}
	}
	for _, m := range upperConst.FindAllStringSubmatch(mod, -1) {
		names = append(names, m[1])
	}
	for _, n := range names {
		if n == "" || n[0] < 'A' || n[0] > 'Z' {
			continue // a type identifier starts upper-case
		}
		if _, ok := pyTypeReserved[n]; !ok {
			t.Errorf("%s: the module-level name %s is not on pyTypeReserved: a type spelled %s would rebind it", label, n, n)
		}
	}
}

var (
	classDecl  = regexp.MustCompile(`(?m)^class ([A-Za-z0-9_]+)[(:]`)
	assignDecl = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*) = `)
	defDecl    = regexp.MustCompile(`(?m)^def ([A-Za-z0-9_]+)\(`)
)

// moduleDecls counts every name a module declares at namespace level: its
// imports, classes, functions and assigned names. A name assigned more than once
// counts once when every assignment is the same kind -- the prefill is built in
// steps (`_M__Fill = bytearray(...)`, then `_M__Fill = bytes(_M__Fill)`).
func moduleDecls(mod string) map[string]int {
	n := map[string]int{}
	for _, m := range importStmt.FindAllStringSubmatch(mod, -1) {
		for _, x := range strings.Split(m[1], ",") {
			n[strings.TrimSpace(x)]++
		}
	}
	for _, re := range []*regexp.Regexp{classDecl, defDecl} {
		for _, m := range re.FindAllStringSubmatch(mod, -1) {
			n[m[1]]++
		}
	}
	assigned := map[string]bool{}
	for _, m := range assignDecl.FindAllStringSubmatch(mod, -1) {
		assigned[m[1]] = true
	}
	for a := range assigned {
		n[a]++
	}
	return n
}

// TestNamesSchemaDeclaresOnce generates the shared name-collision schema
// (tests/conformance/lib/names.yaml, ARCHITECTURE §8 "Naming") and requires
// every namespace-level name of the module -- import, class, enum alias,
// location constant, table, prefill -- and of the harness to be declared
// exactly once. Python rebinds a name silently, so a second declaration is not
// an error anywhere but at run time; the conformance suite runs the module,
// this checks the guarantee without a toolchain.
func TestNamesSchemaDeclaresOnce(t *testing.T) {
	s := schemaFile(t, "../../tests/conformance/lib/names.yaml")
	files := genPy(t, s, map[string]any{"emit": "project"})
	for _, path := range []string{"message.py", "harness.py"} {
		for name, n := range moduleDecls(string(files[path])) {
			if n != 1 {
				t.Errorf("%s: %s is declared %d times", path, name, n)
			}
		}
	}
	mod := string(files["message.py"])
	// Every message is a class of its own, and the harness names each once.
	for _, m := range s.Messages {
		if !strings.Contains(mod, "\nclass "+msgIdent(m)+":\n") {
			t.Errorf("message %s: class %s missing", m.Name, msgIdent(m))
		}
	}
	h := string(files["harness.py"])
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?m)^    "([^"]+)": message\.`), regexp.MustCompile(`if w in \('(encode_[^']+)'`)} {
		seen := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(h, -1) {
			if seen[m[1]] {
				t.Errorf("harness.py: %q is keyed twice", m[1])
			}
			seen[m[1]] = true
		}
		if len(seen) != len(s.Messages) {
			t.Errorf("harness.py: %d keys for %d messages (%s)", len(seen), len(s.Messages), re)
		}
	}
}
