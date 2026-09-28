package python

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A schema union is a dataclass holding exactly ONE option (MESSAGE_SPEC §4.2,
// §7.4.1; union.go). The tests below pin the emitted shape of every half -- the
// storage and accessors, the forced vs guarded encode arms, where the decode
// switch sits, that no union reaches a destination table, the gap fill and the
// JSON form -- and the name errors. check_union.py proves the behaviour on both
// engines; these make a regression loud at `go test` time.

const unionSrc = `
version: 1
messages:
  M:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8, default: "" }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 }, default: [] }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:    { id: 5, type: blob, maxlen: 4 }
          inner: { id: 6, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
          fa:    { id: 7, type: array, items: { type: fp32, count: 2 } }
          bo:    { id: 8, type: boolean, default: true }
      v:
        id: 1
        type: array
        items:
          type: union
          count: 4
          default_id: 1
          oneof:
            i: { id: 0, type: i32 }
            s: { id: 1, type: string, maxlen: 8 }
      a: { id: 2, type: u64 }
      b: { id: 3, type: u64 }
      c: { id: 4, type: u64 }
`

func unionModule(t *testing.T) string {
	t.Helper()
	return string(genPy(t, schema(t, unionSrc), map[string]any{})["message.py"])
}

// classBody returns the text of one generated class, up to the next top-level
// statement.
func classBody(t *testing.T, mod, class string) string {
	t.Helper()
	head := "\nclass " + class + ":\n"
	i := strings.Index(mod, head)
	if i < 0 {
		t.Fatalf("class %s not emitted:\n%s", class, mod)
	}
	body := mod[i+1:]
	for j := len(head) - 1; j < len(body); j++ {
		if body[j-1] == '\n' && body[j] != ' ' && body[j] != '\n' {
			return body[:j]
		}
	}
	return body
}

// TestPythonUnionStorage: two dataclass fields -- the held id and ONE value slot
// starting at default_id's own default -- and the id constants as ClassVars, so
// they stay out of the dataclass fields (and out of __init__/__eq__).
func TestPythonUnionStorage(t *testing.T) {
	mod := unionModule(t)
	u := classBody(t, mod, "MU")
	for _, want := range []string{
		"@dataclass\nclass MU:\n",
		"    _which: int = 2\n",
		"    _value: object = field(default_factory=lambda: MUPt())\n",
		"    NUM_ID: ClassVar[int] = 0\n",
		"    PT_ID: ClassVar[int] = 2\n",
		"    BO_ID: ClassVar[int] = 8\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("union storage missing %q", want)
		}
	}
	if !strings.Contains(mod, "from typing import ClassVar\n") {
		t.Error("ClassVar is used but not imported: dataclasses would make every id constant a field")
	}
	// No product type: no option is a dataclass field of its own.
	for _, bad := range []string{"    num: int = ", "    pt: MUPt = ", "    s: str = "} {
		if strings.Contains(u, bad) {
			t.Errorf("union still holds every option side by side (%q):\n%s", bad, u)
		}
	}
	// A union-free schema imports no typing.
	plain := string(genPy(t, schema(t, "version: 1\nmessages:\n  P:\n    payload:\n      x: { id: 0, type: u8 }\n"), map[string]any{})["message.py"])
	if strings.Contains(plain, "ClassVar") {
		t.Error("a union-free module imports ClassVar")
	}
}

// TestPythonUnionAccessors: the getter answers the option's default while another
// is held and stores nothing; the setter selects; mutable_<opt>() is SELECT IF NOT
// HELD -- it never resets an option already held, which is what lets decode call
// it once per occurrence and continue a held struct (§7.4); clear() is default_id
// at its own default.
func TestPythonUnionAccessors(t *testing.T) {
	u := classBody(t, unionModule(t), "MU")
	for _, want := range []string{
		"    @property\n    def which(self) -> int:\n",
		"        return self._which\n",
		"    @property\n    def num(self) -> int:\n        return self._value if self._which == 0 else 5\n",
		"    @num.setter\n    def num(self, v: int) -> None:\n        self._which = 0\n        self._value = v\n",
		"    def has_num(self) -> bool:\n        return self._which == 0\n",
		"        return self._value if self._which == 2 else MUPt()\n",
		"        return self._value if self._which == 5 else b\"\"\n",
		"        return self._value if self._which == 8 else True\n",
		"    def mutable_pt(self) -> MUPt:\n        if self._which != 2:\n            self._which = 2\n            self._value = MUPt()\n        return self._value\n",
		"    def mutable_inner(self) -> MUInner:\n        if self._which != 6:\n            self._which = 6\n            self._value = MUInner()\n        return self._value\n",
		"    def mutable_strs(self) -> list[str]:\n        if self._which != 4:\n            self._which = 4\n            self._value = []\n        return self._value\n",
		"    def clear(self) -> None:\n",
		"        self._which = 2\n        self._value = MUPt()\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("union accessor missing %q\n%s", want, u)
		}
	}
	// A scalar, string and blob are immutable: replaced through the setter only.
	for _, bad := range []string{"def mutable_num(", "def mutable_s(", "def mutable_bl(", "def mutable_bo("} {
		if strings.Contains(u, bad) {
			t.Errorf("an immutable option has a mutable accessor (%s)", bad)
		}
	}
}

// TestPythonUnionEncodeArms: serialize writes the HELD option only. default_id is
// written exactly as a field of its kind (a struct D closes with the dropping
// end, so a union at its default vanishes); every other option is FORCED -- no
// ≠-default guard, the empty string/blob/array as its empty payload, a struct,
// union or wrapper option as a present frame closed with the keeping end.
func TestPythonUnionEncodeArms(t *testing.T) {
	u := classBody(t, unionModule(t), "MU")
	for _, want := range []string{
		"        _w = self._which\n        _v = self._value\n",
		"        if _w == 0:\n            e.write_unsigned(0, int(_v))\n",
		"        elif _w == 1:\n            e.write_string(1, _v)\n",
		"        elif _w == 2:\n            e.write_sequence_begin_lazy(2)\n            _v.serialize(e)\n            e.write_sequence_end()\n",
		"        elif _w == 3:\n            e.write_unsigned_array(3, _v)\n",
		"        elif _w == 4:\n            e.write_sequence_begin_lazy(4)\n",
		"                    e.write_string(_i0, _e0)\n            e.write_sequence_end_keep()\n",
		"        elif _w == 5:\n            e.write_bytes(5, bytes(_v))\n",
		"        elif _w == 6:\n            e.write_sequence_begin_lazy(6)\n            _v.serialize(e)\n            e.write_sequence_end_keep()\n",
		"        elif _w == 7:\n            e.write_float32_array(7, _v)\n",
		"        elif _w == 8:\n            e.write_bool(8, _v)\n",
		// isDefault agrees with the writer: only default_id at its default.
		"    def _is_default(self) -> bool:\n        return self._which == 2 and self._value._is_default()\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("union encode arm missing %q\n%s", want, u)
		}
	}
	// The product-type guards must not come back: a non-D option at its own
	// default would be omitted and read back as default_id.
	for _, bad := range []string{"if _v != 5:", "if _v != \"\":", "if len(_v) != 0:", "if bytes(_v) != ", "if _v != True:"} {
		if strings.Contains(u, bad) {
			t.Errorf("a non-default_id option is guarded (%q): it would vanish at its own default", bad)
		}
	}
	// The union FIELD keeps its lazily framed, dropping close.
	if !strings.Contains(unionModule(t), "        e.write_sequence_begin_lazy(0)\n        self.u.serialize(e)\n        e.write_sequence_end()\n") {
		t.Error("the union field is no longer lazily framed with the dropping end")
	}
}

// TestPythonUnionScalarDefaultArm: a scalar default_id is guarded like a scalar
// field, and is the one arm that is.
func TestPythonUnionScalarDefaultArm(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      q: { id: 0, type: union, default_id: 1, oneof: { big: { id: 0, type: u64 }, sig: { id: 1, type: i64 } } }
`
	u := classBody(t, string(genPy(t, schema(t, src), map[string]any{})["message.py"]), "MQ")
	for _, want := range []string{
		"        if _w == 0:\n            e.write_unsigned(0, int(_v))\n",
		"        elif _w == 1:\n            if _v != 0:\n                e.write_signed(1, int(_v))\n",
		"        return self._which == 1 and self._value == 0\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("scalar default_id arm missing %q\n%s", want, u)
		}
	}
}

// TestPythonUnionDecodeSwitch: the switch sits ONLY where the §7.3 gate has
// already passed and the hook fires once per occurrence -- the typed value hook
// (width check first, then the tag and the value) and on_sequence_begin, where a
// struct/union option is selected if not held and a wrapper option is selected
// and replaced (§7.4). Never in on_field, on_schema_bound or on_array_begin,
// which decide before the value is read. Paths below an option go through the
// one `_value` slot.
func TestPythonUnionDecodeSwitch(t *testing.T) {
	mod := unionModule(t)
	vis := mod[strings.Index(mod, "class _MVisitor(Visitor):"):]
	for _, want := range []string{
		" c == _L_M_u:\n            if fid == 0:\n                if value > 65535:\n" +
			"                    raise SofaDecodeError(\"num: value outside declared width u16\")\n" +
			"                _u = self._o.u\n                _u._which = 0\n                _u._value = value\n",
		"            elif fid == 8:\n                _u = self._o.u\n                _u._which = 8\n                _u._value = bool(value)\n",
		// on_sequence_begin: select if not held / select and replace.
		"            if fid == 2:\n                self._o.u.mutable_pt()\n                self._s.append(c)\n                self._c = _L_M_u_pt\n",
		"            elif fid == 4:\n                self._o.u.strs = []\n                self._s.append(c)\n",
		"            elif fid == 6:\n                self._o.u.mutable_inner()\n",
		// below an option: the one value slot, for a struct member and a union
		// option alike.
		"                self._o.u._value.y = value\n",
		"                _u = self._o.u._value\n                _u._which = 0\n",
		// an array of unions: a string option at the element.
		"                _u = self._o.v[self._ix",
	} {
		if !strings.Contains(vis, want) {
			t.Errorf("union decode store missing %q", want)
		}
	}
	for _, hook := range []string{"on_field", "on_schema_bound", "on_array_begin"} {
		i := strings.Index(vis, "    def "+hook+"(")
		if i < 0 {
			continue
		}
		body := vis[i+5:]
		if j := strings.Index(body, "\n    def "); j >= 0 {
			body = body[:j]
		}
		if strings.Contains(body, "_which") || strings.Contains(body, "mutable_") {
			t.Errorf("%s switches a union option; it fires before the value (and may be replayed):\n%s", hook, body)
		}
	}
}

// TestPythonUnionNeverBound: a union is not bindable at any depth. A destination
// table maps each id to a fixed slot, so two options in one frame would both
// land; the union's scope stays on the visitor, its on_sequence_begin arm is
// emitted, and a union class carries no table of its own.
func TestPythonUnionNeverBound(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      b: { id: 1, type: u64 }
      c: { id: 2, type: u64 }
      u: { id: 3, type: union, oneof: { x: { id: 0, type: u64 }, y: { id: 1, type: u64 }, z: { id: 2, type: u64 } } }
      st: { id: 4, type: struct, fields: { k: { id: 0, type: u64 }, w: { id: 1, type: union, oneof: { p: { id: 0, type: u8 } } } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	if !strings.Contains(mod, "    .unsigned(0, at=0, count_at=1)") {
		t.Fatalf("the message's scalars should still be on its table:\n%s", mod)
	}
	for _, bad := range []string{".sequence(3,", ".sequence(4,", "_BIND_MU", "_BIND_M_u ", "_BIND_M_st"} {
		if strings.Contains(mod, bad) {
			t.Errorf("a union (or a scope holding one) is on a destination table (%q)", bad)
		}
	}
	for _, want := range []string{
		"            if fid == 3:\n                self._s.append(c)\n                self._c = _L_M_u\n",
		"            elif fid == 4:\n                self._s.append(c)\n                self._c = _L_M_st\n",
		"                _u = self._o.u\n                _u._which = 2\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("union decode missing %q", want)
		}
	}
	// The union class's own visitor is a visitor, never a table-only class.
	if strings.Contains(mod, "class _MUVisitor(Visitor):\n    \"\"\"Decode handler for :class:`MU`: a destination table") {
		t.Error("the union class decodes through a destination table")
	}
}

// TestPythonUnionGapFillPerType: an array of unions grows through the element
// class, whose constructor holds that type's default_id -- so a $defs union split
// per default_id fills each site's gaps with its own default option.
func TestPythonUnionGapFillPerType(t *testing.T) {
	const src = `
version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  M:
    payload:
      pf: { id: 0, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      pe: { id: 1, type: array, items: { type: union, count: 3, default_id: 0, oneof: { $ref: "#/$defs/union/Pick" } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"class UnionPickDefaultT:\n",
		"class UnionPickDefaultN:\n",
		"    pf: UnionPickDefaultT = field(default_factory=lambda: UnionPickDefaultT())\n",
		"reserve_elem(self._o.pe, fid, UnionPickDefaultN, 3, ",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("per-type union default missing %q", want)
		}
	}
	if !strings.Contains(classBody(t, mod, "UnionPickDefaultT"), "    _which: int = 1\n") ||
		!strings.Contains(classBody(t, mod, "UnionPickDefaultN"), "    _which: int = 0\n") {
		t.Error("the split union types do not start at their own default_id")
	}
}

// TestPythonUnionJSON: exactly the held option, `{"<option>": value}`, printed
// even when it is default_id at its default; from_jsonable selects through the
// setters.
func TestPythonUnionJSON(t *testing.T) {
	u := classBody(t, unionModule(t), "MU")
	for _, want := range []string{
		"        if _w == 0:\n            return {\"num\": _v}\n",
		"        if _w == 5:\n            return {\"bl\": list(_v)}\n",
		"        if _w == 6:\n            return {\"inner\": _v.to_jsonable()}\n",
		"        return {\"pt\": _v.to_jsonable()}\n",
		"        if \"pt\" in d:\n            o.pt = MUPt.from_jsonable(d[\"pt\"])\n",
		"        if \"bl\" in d:\n            o.bl = bytes(d[\"bl\"])\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("union JSON missing %q\n%s", want, u)
		}
	}
}

// TestPythonUnionNames: an option landing on a union member takes the trailing
// underscore; two options deriving one member -- the property, has_/mutable_
// method or id constant share the class's single namespace -- fail generation,
// naming the union and both options.
func TestPythonUnionNames(t *testing.T) {
	const ok = `
version: 1
messages:
  M:
    payload:
      u: { id: 0, type: union, oneof: { which: { id: 0, type: u8 }, clear: { id: 1, type: u8 }, class: { id: 2, type: u8 } } }
`
	u := classBody(t, string(genPy(t, schema(t, ok), map[string]any{})["message.py"]), "MU")
	for _, want := range []string{
		"    def which_(self) -> int:\n",
		"    def clear_(self) -> int:\n",
		"    def class_(self) -> int:\n",
		"    def has_which(self) -> bool:\n",
		"    WHICH_ID: ClassVar[int] = 0\n",
		"        if \"which\" in d:\n            o.which_ = d[\"which\"]\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("mangled union name missing %q\n%s", want, u)
		}
	}
	for _, c := range []struct{ name, src, want string }{
		{"id constant vs option", `{ a: { id: 0, type: u8 }, A_ID: { id: 1, type: u8 } }`, `options "a" and "A_ID" both generate the member A_ID`},
		{"has_ vs option", `{ x: { id: 0, type: u8 }, has_x: { id: 1, type: u8 } }`, `options "x" and "has_x" both generate the member has_x`},
		{"id constant vs fixed member", `{ MAX_SIZE_ID: { id: 0, type: u8 }, max_size: { id: 1, type: u8 } }`, `both generate the member MAX_SIZE_ID`},
	} {
		src := "version: 1\nmessages:\n  M:\n    payload:\n      u: { id: 0, type: union, oneof: " + c.src + " }\n"
		_, err := (&Backend{}).Generate(schema(t, src), map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "python backend: union ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error containing %q, got %v", c.name, c.want, err)
		}
	}
}

// TestPythonUnionBuiltinOptionNames: an option named after a builtin the class
// body evaluates while the class is defined -- the `@property` and
// `@classmethod` decorators -- takes the trailing underscore. Unmangled, the
// option property `property` rebinds the decorator the next option's
// `@property` evaluates, and `classmethod` the one on from_jsonable: generation
// succeeds and the module fails at import. The second half imports the module
// and round-trips every option on both engines.
func TestPythonUnionBuiltinOptionNames(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      u: { id: 0, type: union, oneof: { property: { id: 0, type: u8 }, b: { id: 1, type: u8 }, classmethod: { id: 2, type: u8 }, c: { id: 3, type: u8 } } }
`
	files := genPy(t, schema(t, src), map[string]any{})
	u := classBody(t, string(files["message.py"]), "MU")
	for _, want := range []string{
		"    def property_(self) -> int:\n",
		"    @property_.setter\n",
		"    def classmethod_(self) -> int:\n",
		"    def has_property(self) -> bool:\n",
		"    PROPERTY_ID: ClassVar[int] = 0\n",
		"    CLASSMETHOD_ID: ClassVar[int] = 2\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("builtin-named option not mangled, missing %q\n%s", want, u)
		}
	}
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout for the import half")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const driver = `
import sofab
from message import M, MU

for name, oid, v in (("property_", 0, 9), ("b", 1, 0), ("classmethod_", 2, 0), ("c", 3, 4)):
    m = M()
    setattr(m.u, name, v)
    got = M.decode(m.encode())
    assert got.u.which == oid, (name, got.u.which)
    assert getattr(got.u, name) == v, (name, getattr(got.u, name))
    assert MU.from_jsonable(m.u.to_jsonable()) == m.u, name
print("%s ok" % sofab.IMPL)
`
	if err := os.WriteFile(filepath.Join(dir, "driver.py"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pure := range []string{"", "1"} {
		cmd := exec.Command(py, filepath.Join(dir, "driver.py"))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PYTHONPATH="+filepath.Join(corelib, "src")+string(os.PathListSeparator)+dir,
			"SOFAB_PUREPYTHON="+pure)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("SOFAB_PUREPYTHON=%q: %v\n%s", pure, err, out)
		}
		t.Logf("SOFAB_PUREPYTHON=%q: %s", pure, strings.TrimSpace(string(out)))
	}
}
