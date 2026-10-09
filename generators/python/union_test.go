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
// switch sits, which unions reach a destination table (a one-of table, struct and
// union options included) and which stay on the visitor, the gap fill and the
// JSON form --
// and the name errors. check_union.py proves the behaviour on both
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
	u := classBody(t, mod, "M_U")
	for _, want := range []string{
		"@dataclass\nclass M_U:\n",
		"    _which: int = 2\n",
		"    _value: object = field(default_factory=lambda: M_U_Pt())\n",
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
	for _, bad := range []string{"    num: int = ", "    pt: M_U_Pt = ", "    s: str = "} {
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
	u := classBody(t, unionModule(t), "M_U")
	for _, want := range []string{
		"    @property\n    def which(self) -> int:\n",
		"        return self._which\n",
		"    @property\n    def num(self) -> int:\n        return self._value if self._which == 0 else 5\n",
		"    @num.setter\n    def num(self, v: int) -> None:\n        self._which = 0\n        self._value = v\n",
		"    def has_num(self) -> bool:\n        return self._which == 0\n",
		"        return self._value if self._which == 2 else M_U_Pt()\n",
		"        return self._value if self._which == 5 else b\"\"\n",
		"        return self._value if self._which == 8 else True\n",
		"    def mutable_pt(self) -> M_U_Pt:\n        if self._which != 2:\n            self._which = 2\n            self._value = M_U_Pt()\n        return self._value\n",
		"    def mutable_inner(self) -> M_U_Inner:\n        if self._which != 6:\n            self._which = 6\n            self._value = M_U_Inner()\n        return self._value\n",
		"    def mutable_strs(self) -> list[str]:\n        if self._which != 4:\n            self._which = 4\n            self._value = []\n        return self._value\n",
		"    def clear(self) -> None:\n",
		"        self._which = 2\n        self._value = M_U_Pt()\n",
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
	u := classBody(t, unionModule(t), "M_U")
	for _, want := range []string{
		"        _w = self._which\n        _v = self._value\n",
		"        if _w == 0:\n            e.write_u16(0, int(_v))\n",
		"        elif _w == 1:\n            e.write_string_bounded(1, _v, 8)\n",
		"        elif _w == 2:\n            e.write_sequence_begin_lazy(2)\n            _v.serialize(e)\n            e.write_sequence_end()\n",
		"        elif _w == 3:\n            e.write_u16_array(3, _v, 4)\n",
		"        elif _w == 4:\n            _n0 = len(_v)\n            if _n0 > 3:\n                raise SofaArgumentError(\"strs: array over count 3\")\n            e.write_sequence_begin_lazy(4)\n",
		"                    e.write_string_bounded(_i0, _e0, 4)\n            e.write_sequence_end_keep()\n",
		"        elif _w == 5:\n            e.write_bytes_bounded(5, bytes(_v), 4)\n",
		"        elif _w == 6:\n            e.write_sequence_begin_lazy(6)\n            _v.serialize(e)\n            e.write_sequence_end_keep()\n",
		"        elif _w == 7:\n            e.write_float32_array_bounded(7, _v, 2)\n",
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
	u := classBody(t, string(genPy(t, schema(t, src), map[string]any{})["message.py"]), "M_Q")
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
	vis := mod[strings.Index(mod, "class _M__Visitor(Visitor):"):]
	for _, want := range []string{
		" c == _M__Loc__u:\n            if fid == 0:\n                if value > 65535:\n" +
			"                    raise SofaDecodeError(\"num: value outside declared width u16\")\n" +
			"                _u = self._o.u\n                _u._which = 0\n                _u._value = value\n",
		"            elif fid == 8:\n                _u = self._o.u\n                _u._which = 8\n                _u._value = bool(value)\n",
		// on_sequence_begin: select if not held / select and replace.
		"            if fid == 2:\n                self._o.u.mutable_pt()\n                self._s.append(c)\n                self._c = _M__Loc__u__pt\n",
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

// TestPythonLeafUnionBindsAsOneOfTable: a union whose options are all leaves --
// scalar, string, blob, bounded native array -- is a ONE-OF table
// (corelib-py#165): closed, `which_at` naming its own words slot, one row per
// option, reached from the parent by a `sequence` row. The which slot is seeded
// with default_id in the prefill, and the union's scope leaves the visitor
// entirely -- no location, no on_sequence_begin arm, no typed-hook arm.
func TestPythonLeafUnionBindsAsOneOfTable(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      b: { id: 1, type: u64 }
      u:
        id: 2
        type: union
        default_id: 3
        oneof:
          n:  { id: 0, type: u16, default: 5 }
          s:  { id: 1, type: string, maxlen: 8 }
          bl: { id: 2, type: blob, maxlen: 4 }
          ar: { id: 3, type: array, items: { type: i8, count: 2 } }
          f:  { id: 4, type: fp64 }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"_M__Bind__u = (Binding(closed=True, which_at=4)\n" +
			"    .unsigned(0, at=5, count_at=6, max_value=65535)\n" +
			"    .string(1, at=0, maxlen=8, count_at=7)\n" +
			"    .bytes(2, at=1, maxlen=4, count_at=8)\n" +
			"    .signed_array(3, at=9, cap=2, count_at=11, elem_min=-128, elem_max=127)\n" +
			"    .float64(4, at=12, count_at=13)\n)\n",
		"    .sequence(2, child=_M__Bind__u)\n",
		// The which slot starts at default_id -- not at the all-ones every other
		// slot starts at, which is no option id at all.
		"_M__Fill = bytearray(b\"\\xff\" * (_M__Words * 8))\n",
		"memoryview(_M__Fill).cast(\"Q\")[4] = 3  # m.u\n_M__Fill = bytes(_M__Fill)\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("leaf union one-of table missing %q\n%s", want, mod)
		}
	}
	// Every id of M is now on a closed table, so M is table-only: no hook at all,
	// which is also what lets the decoder skip on_sequence_begin for the whole
	// message type (corelib-py's _wants_seq_begin is false for it).
	vis := mod[strings.Index(mod, "class _M__Visitor(Visitor):"):]
	if !strings.Contains(vis, "a destination table and nothing else") {
		t.Errorf("M should be table-only once its union is bound:\n%s", vis)
	}
	for _, bad := range []string{"_M__Loc__u", "def on_sequence_begin", "def on_unsigned", "def on_field"} {
		if strings.Contains(vis, bad) {
			t.Errorf("the bound union's scope still reaches the visitor (%q)", bad)
		}
	}
}

// TestPythonOneOfScatterReadsWhichFirst: the scatter consults the which slot and
// then only the held option's slots. Every arm is gated on `_x == <id>`, and the
// which slot is read before any option slot -- a reader that went straight to an
// option's slot would return a discarded option's stale value.
func TestPythonOneOfScatterReadsWhichFirst(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      u: { id: 1, type: union, default_id: 1, oneof: { x: { id: 0, type: u32 }, y: { id: 1, type: i32, default: -3 }, z: { id: 2, type: boolean } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	const want = "        _u = m.u\n" +
		"        _x = U[2]\n" +
		"        if _x == 0 and U[4] != _ABSENT:\n" +
		"            _u._which = 0\n" +
		"            _u._value = U[3]\n" +
		"        elif _x == 1 and U[6] != _ABSENT:\n" +
		"            _u._which = 1\n" +
		"            _u._value = S[5]\n" +
		"        elif _x == 2 and U[8] != _ABSENT:\n" +
		"            _u._which = 2\n" +
		"            _u._value = U[7] != 0\n"
	if !strings.Contains(mod, want) {
		t.Fatalf("one-of scatter missing:\n%s\n---\n%s", want, mod)
	}
	if !strings.Contains(mod, "memoryview(_M__Fill).cast(\"Q\")[2] = 1  # m.u\n") {
		t.Error("the which slot does not start at default_id")
	}
	// No option slot is read outside its arm.
	sc := mod[strings.Index(mod, "    def scatter(self) -> None:"):]
	sc = sc[:strings.Index(sc, "        _u = m.u\n")]
	for _, slot := range []string{"U[3]", "S[5]", "U[7]"} {
		if strings.Contains(sc, slot) {
			t.Errorf("an option slot (%s) is read before the which slot", slot)
		}
	}
}

// TestPythonUnionWithSequenceOptionStaysOnVisitor: what still keeps a union off
// the table now that a struct/union option binds (corelib-py#167). An option
// whose SHAPE the table has no entry for -- a wrapper array, an unbounded array,
// a struct option holding a wrapper array, a nested union with a wrapper option
// -- and a struct option MEMBER with a non-empty string/blob/array default,
// which the corelib's option reset would restart empty instead of at that
// default (rule 4). Each such union's scope stays open, and so does every scope
// holding it.
func TestPythonUnionWithSequenceOptionStaysOnVisitor(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      b: { id: 1, type: u64 }
      c: { id: 2, type: u64 }
      uw: { id: 3, type: union, oneof: { x: { id: 0, type: u64 }, w: { id: 1, type: array, items: { type: string, count: 2, maxlen: 4 } } } }
      ud: { id: 4, type: union, oneof: { x: { id: 0, type: u64 }, d: { id: 1, type: array, items: { type: u8 } } } }
      ui: { id: 5, type: union, oneof: { x: { id: 0, type: u64 }, i: { id: 1, type: union, oneof: { q: { id: 0, type: u8 }, w: { id: 1, type: array, items: { type: string, count: 2, maxlen: 4 } } } } } }
      st: { id: 6, type: struct, fields: { k: { id: 0, type: u64 }, w: { id: 1, type: union, oneof: { p: { id: 0, type: struct, fields: { z: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } } } } } } } }
      us: { id: 7, type: union, oneof: { x: { id: 0, type: u64 }, p: { id: 1, type: struct, fields: { s: { id: 0, type: string, maxlen: 4, default: "ab" } } } } }
      ua: { id: 8, type: union, oneof: { x: { id: 0, type: u64 }, p: { id: 1, type: struct, fields: { r: { id: 0, type: array, items: { type: u8, count: 2 }, default: [1] } } } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	if !strings.Contains(mod, "_M__Bind = (Binding()\n    .unsigned(0, at=0, count_at=1)") {
		t.Fatalf("the message's scalars should still be on its (open) table:\n%s", mod)
	}
	for _, bad := range []string{"which_at=", ".sequence(", "_M__Bind__u", "_M__Bind__st"} {
		if strings.Contains(mod, bad) {
			t.Errorf("a union the table cannot carry (or a scope holding one) is on a destination table (%q)", bad)
		}
	}
	for _, want := range []string{
		"            if fid == 3:\n                self._s.append(c)\n                self._c = _M__Loc__uw\n",
		"            elif fid == 4:\n                self._s.append(c)\n                self._c = _M__Loc__ud\n",
		"            elif fid == 5:\n                self._s.append(c)\n                self._c = _M__Loc__ui\n",
		"            elif fid == 6:\n                self._s.append(c)\n                self._c = _M__Loc__st\n",
		"            elif fid == 7:\n                self._s.append(c)\n                self._c = _M__Loc__us\n",
		"            elif fid == 8:\n                self._s.append(c)\n                self._c = _M__Loc__ua\n",
		"                self._o.us.mutable_p()\n",
		"                self._o.ua.mutable_p()\n",
		"                self._o.ui.mutable_i()\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("union visitor decode missing %q", want)
		}
	}
	if strings.Contains(mod, "a destination table and nothing else") {
		t.Error("a class decodes through a table alone although a union in it needs the visitor")
	}
	// The same struct option with EMPTY defaults binds: it is the default, not
	// the kind, that keeps `us`/`ua` off.
	const ok = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      us: { id: 1, type: union, oneof: { x: { id: 0, type: u64 }, p: { id: 1, type: struct, fields: { s: { id: 0, type: string, maxlen: 4, default: "" }, r: { id: 1, type: array, items: { type: u8, count: 2 }, default: [] } } } } }
`
	okMod := string(genPy(t, schema(t, ok), map[string]any{})["message.py"])
	if !strings.Contains(okMod, "    .sequence(1, child=_M__Bind__us)\n") {
		t.Errorf("a struct option whose string/array members default to empty should bind:\n%s", okMod)
	}
}

// TestPythonUnionWithStructOptionBinds: a union with a struct option and a
// nested union option is a one-of table (corelib-py#167). The struct option's
// table is a `sequence` row of the one-of table; every scalar row inside an
// option's subtree states its non-zero schema default (a boolean as 1, an enum
// or a bitfield as its integer, an fp32 as written), a union nested in an option
// states its default_id; and nothing outside an option -- neither a top-level
// member nor the top-level union's own leaf options -- carries a `default=`.
func TestPythonUnionWithStructOptionBinds(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64, default: 3 }
      b: { id: 1, type: u64 }
      u:
        id: 2
        type: union
        default_id: 1
        oneof:
          n:  { id: 0, type: u16, default: 5 }
          pt:
            id: 1
            type: struct
            fields:
              x:  { id: 0, type: i32, default: -7 }
              y:  { id: 1, type: i32 }
              bo: { id: 2, type: boolean, default: true }
              f:  { id: 3, type: fp32, default: 0.1 }
              e:  { id: 4, type: enum, enum: { A: 0, B: 1, C: 2 }, default: 2 }
              fl: { id: 5, type: bitfield, bits: { r: { pos: 0 }, w: { pos: 1, default: true } } }
              nz: { id: 6, type: fp64, default: -0.0 }
              in: { id: 7, type: struct, fields: { k: { id: 0, type: u8, default: 9 } } }
          nu: { id: 2, type: union, default_id: 1, oneof: { p: { id: 0, type: u8 }, q: { id: 1, type: u8, default: 4 } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"_M__Bind__u__pt__in = (Binding(closed=True)\n    .unsigned(0, at=21, count_at=22, max_value=255, default=9)\n)\n",
		"_M__Bind__u__pt = (Binding(closed=True)\n" +
			"    .signed(0, at=7, count_at=8, min_value=-2147483648, max_value=2147483647, default=-7)\n" +
			"    .signed(1, at=9, count_at=10, min_value=-2147483648, max_value=2147483647)\n" +
			"    .boolean(2, at=11, count_at=12, default=1)\n" +
			"    .float32(3, at=13, count_at=14, default=0.1)\n" +
			"    .signed(4, at=15, count_at=16, min_value=-128, max_value=127, default=2)\n" +
			"    .unsigned(5, at=17, count_at=18, max_value=255, default=2)\n" +
			"    .float64(6, at=19, count_at=20, default=-0.0)\n" +
			"    .sequence(7, child=_M__Bind__u__pt__in)\n)\n",
		// A nested union states its default_id, and its own options their
		// defaults: the corelib resets it to that option at that default.
		"_M__Bind__u__nu = (Binding(closed=True, which_at=23, default_id=1)\n" +
			"    .unsigned(0, at=24, count_at=25, max_value=255)\n" +
			"    .unsigned(1, at=26, count_at=27, max_value=255, default=4)\n)\n",
		// The top-level union: its leaf option has no default= (its own arrival
		// writes it whole), and no default_id= (the prefill seeds it).
		"_M__Bind__u = (Binding(closed=True, which_at=4)\n" +
			"    .unsigned(0, at=5, count_at=6, max_value=65535)\n" +
			"    .sequence(1, child=_M__Bind__u__pt)\n" +
			"    .sequence(2, child=_M__Bind__u__nu)\n)\n",
		"_M__Bind = (Binding(closed=True)\n    .unsigned(0, at=0, count_at=1)\n",
		"memoryview(_M__Fill).cast(\"Q\")[4] = 1  # m.u\n",
		"memoryview(_M__Fill).cast(\"Q\")[23] = 1  # m.u.nu\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("struct-option one-of table missing %q\n%s", want, mod)
		}
	}
	// Outside an option a default is never stated: the corelib would ignore it,
	// and the dataclass default is what an absent member reads as.
	if strings.Contains(mod, ".unsigned(0, at=0, count_at=1, default=") {
		t.Error("a top-level member states default=: only a union option's subtree reads it")
	}
	// Everything is on the table, so M is table-only.
	vis := mod[strings.Index(mod, "class _M__Visitor(Visitor):"):]
	if !strings.Contains(vis, "a destination table and nothing else") {
		t.Errorf("M should be table-only once its union is bound:\n%s", vis)
	}
	for _, bad := range []string{"_M__Loc__u", "def on_sequence_begin", "mutable_pt()\n                self._s"} {
		if strings.Contains(vis, bad) {
			t.Errorf("the bound union still reaches the visitor (%q)", bad)
		}
	}
}

// TestPythonOneOfScatterStructOption: the scatter reads the which slot first
// and reaches a struct option's slots -- and a nested union's which slot -- only
// inside that option's arm. A struct/union arm has no arrival test of its own:
// mutable_<opt>() selects it (the held object on a fresh message), and its
// members follow with theirs, onto that object. A nested union's locals are
// suffixed so they never clobber the enclosing option's.
func TestPythonOneOfScatterStructOption(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      u:
        id: 1
        type: union
        default_id: 0
        oneof:
          n:  { id: 0, type: u16 }
          pt: { id: 1, type: struct, fields: { x: { id: 0, type: i32 }, w: { id: 1, type: union, oneof: { p: { id: 0, type: u8 }, s: { id: 1, type: struct, fields: { z: { id: 0, type: u8 } } } } }, k: { id: 2, type: u8 } } }
          nu: { id: 2, type: union, default_id: 1, oneof: { p: { id: 0, type: u8 }, q: { id: 1, type: u8, default: 4 } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	sc := mod[strings.Index(mod, "class _M__Visitor(Visitor):"):]
	sc = sc[strings.Index(sc, "        m = self._o\n"):]
	sc = sc[:strings.Index(sc, "\n\n")]
	const want = "        _u = m.u\n" +
		"        _x = U[2]\n" +
		"        if _x == 0 and U[4] != _ABSENT:\n" +
		"            _u._which = 0\n" +
		"            _u._value = U[3]\n" +
		"        elif _x == 1:\n" +
		"            _v = _u.mutable_pt()\n" +
		"            if U[6] != _ABSENT: _v.x = S[5]\n" +
		"            if U[13] != _ABSENT: _v.k = U[12]\n" +
		"            _u1 = _v.w\n" +
		"            _x1 = U[7]\n" +
		"            if _x1 == 0 and U[9] != _ABSENT:\n" +
		"                _u1._which = 0\n" +
		"                _u1._value = U[8]\n" +
		"            elif _x1 == 1:\n" +
		"                _v1 = _u1.mutable_s()\n" +
		"                if U[11] != _ABSENT: _v1.z = U[10]\n" +
		"        elif _x == 2:\n" +
		"            _u1 = _u.mutable_nu()\n" +
		"            _x1 = U[14]\n" +
		"            if _x1 == 0 and U[16] != _ABSENT:\n" +
		"                _u1._which = 0\n" +
		"                _u1._value = U[15]\n" +
		"            elif _x1 == 1 and U[18] != _ABSENT:\n" +
		"                _u1._which = 1\n" +
		"                _u1._value = U[17]"
	if !strings.HasSuffix(sc, want) {
		t.Fatalf("struct-option scatter:\nwant suffix\n%s\n---\ngot\n%s", want, sc)
	}
	// No option slot is read before the which slot.
	pre := sc[:strings.Index(sc, "        _x = U[2]\n")]
	for _, slot := range []string{"U[3]", "S[5]", "U[7]", "U[14]"} {
		if strings.Contains(pre, slot) {
			t.Errorf("an option slot (%s) is read before the which slot", slot)
		}
	}
}

// TestPythonMixedUnionsOneBoundOneNot: one message, one union that binds (a
// struct option, a one-of table) and one the table cannot carry (a wrapper
// option: the visitor). Each takes its own path and neither leaks into the
// other: the bound one has no location and no hook arm, the other no table; the
// root table stays open, because one scope in it still needs the visitor.
func TestPythonMixedUnionsOneBoundOneNot(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      lf: { id: 1, type: union, default_id: 1, oneof: { x: { id: 0, type: u32 }, p: { id: 1, type: struct, fields: { k: { id: 0, type: u8 } } } } }
      sq: { id: 2, type: union, oneof: { x: { id: 0, type: u32 }, w: { id: 1, type: array, items: { type: string, count: 2, maxlen: 4 } } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"_M__Bind__lf = (Binding(closed=True, which_at=",
		"    .sequence(1, child=_M__Bind__lf)\n",
		"_M__Bind = (Binding()\n",
		"        _u = m.lf\n",
		"            _v = _u.mutable_p()\n",
		"            if fid == 2:\n                self._s.append(c)\n                self._c = _M__Loc__sq\n",
		"                _u = self._o.sq\n                _u._which = 0\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("mixed unions missing %q", want)
		}
	}
	for _, bad := range []string{"_M__Loc__lf", "_u = self._o.lf", "self._o.lf.mutable_p()", "_M__Bind__sq", ".sequence(2,"} {
		if strings.Contains(mod, bad) {
			t.Errorf("mixed unions: one union took the other's path (%q)", bad)
		}
	}
}

// TestPythonScopeHoldingBoundUnionCloses: a struct whose only non-scalar member
// is a leaf union is now bindable whole, so its table is CLOSED and the parent
// descends into it -- an unknown id inside it is skipped by the codec instead of
// reaching on_field (corelib-py#132).
func TestPythonScopeHoldingBoundUnionCloses(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      b: { id: 1, type: u64 }
      st: { id: 2, type: struct, fields: { k: { id: 0, type: u64 }, w: { id: 1, type: union, oneof: { p: { id: 0, type: u8 }, q: { id: 1, type: fp32 } } } } }
      ws: { id: 3, type: array, items: { type: string, count: 2, maxlen: 4 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"_M__Bind__st__w = (Binding(closed=True, which_at=",
		"_M__Bind__st = (Binding(closed=True)\n",
		"    .sequence(1, child=_M__Bind__st__w)\n",
		"    .sequence(2, child=_M__Bind__st)\n",
		// The wrapper array keeps the root open, so the visitor still exists and
		// declines what it does not enter.
		"_M__Bind = (Binding()\n",
		"    def on_sequence_begin(self, fid: int) -> bool:\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("scope holding a bound union missing %q\n%s", want, mod)
		}
	}
	for _, bad := range []string{"_M__Loc__st ", "_M__Loc__st__w", "c == _M__Loc__st"} {
		if strings.Contains(mod, bad) {
			t.Errorf("the scope holding a bound union still reaches the visitor (%q)", bad)
		}
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
		"class Pick__DefaultT:\n",
		"class Pick__DefaultN:\n",
		"    pf: Pick__DefaultT = field(default_factory=lambda: Pick__DefaultT())\n",
		"reserve_elem(self._o.pe, fid, Pick__DefaultN, 3, ",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("per-type union default missing %q", want)
		}
	}
	if !strings.Contains(classBody(t, mod, "Pick__DefaultT"), "    _which: int = 1\n") ||
		!strings.Contains(classBody(t, mod, "Pick__DefaultN"), "    _which: int = 0\n") {
		t.Error("the split union types do not start at their own default_id")
	}
}

// TestPythonUnionJSON: exactly the held option, `{"<option>": value}`, printed
// even when it is default_id at its default; from_jsonable selects through the
// setters.
func TestPythonUnionJSON(t *testing.T) {
	u := classBody(t, unionModule(t), "M_U")
	for _, want := range []string{
		"        if _w == 0:\n            return {\"num\": _v}\n",
		"        if _w == 5:\n            return {\"bl\": list(_v)}\n",
		"        if _w == 6:\n            return {\"inner\": _v.to_jsonable()}\n",
		"        return {\"pt\": _v.to_jsonable()}\n",
		"        if \"pt\" in d:\n            o.pt = M_U_Pt.from_jsonable(d[\"pt\"])\n",
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
	u := classBody(t, string(genPy(t, schema(t, ok), map[string]any{})["message.py"]), "M_U")
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
	// An option spelled like a member the union derives from another option
	// takes the trailing underscore by its shape alone, so the pair generates:
	// no two options of a valid schema derive one member, and nothing is refused.
	for _, c := range []struct {
		name, src string
		want      []string
	}{
		{"id constant vs option", `{ a: { id: 0, type: u8 }, A_ID: { id: 1, type: u8 } }`,
			[]string{"    A_ID: ClassVar[int] = 0\n", "    def A_ID_(self) -> int:\n", "    def has_A_ID(self) -> bool:\n"}},
		{"has_ vs option", `{ x: { id: 0, type: u8 }, has_x: { id: 1, type: u8 } }`,
			[]string{"    def has_x(self) -> bool:\n", "    def has_x_(self) -> int:\n", "    def has_has_x(self) -> bool:\n"}},
		{"mutable_ vs option", `{ y: { id: 0, type: struct, fields: { z: { id: 0, type: u8 } } }, mutable_y: { id: 1, type: u8 } }`,
			[]string{"    def mutable_y(self) -> M_U_Y:\n", "    def mutable_y_(self) -> int:\n"}},
		{"id constant vs fixed member", `{ MAX_SIZE_ID: { id: 0, type: u8 }, max_size: { id: 1, type: u8 } }`,
			[]string{"    MAX_SIZE_ID: ClassVar[int] = 1\n", "    def MAX_SIZE_ID_(self) -> int:\n"}},
	} {
		src := "version: 1\nmessages:\n  M:\n    payload:\n      u: { id: 0, type: union, oneof: " + c.src + " }\n"
		files, err := (&Backend{}).Generate(schema(t, src), map[string]any{})
		if err != nil {
			t.Errorf("%s: a valid schema must generate: %v", c.name, err)
			continue
		}
		u := classBody(t, string(files[0].Content), "M_U")
		for _, want := range c.want {
			if !strings.Contains(u, want) {
				t.Errorf("%s: missing %q\n%s", c.name, want, u)
			}
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
	u := classBody(t, string(files["message.py"]), "M_U")
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
from message import M, M_U

for name, oid, v in (("property_", 0, 9), ("b", 1, 0), ("classmethod_", 2, 0), ("c", 3, 4)):
    m = M()
    setattr(m.u, name, v)
    got = M.decode(m.encode())
    assert got.u.which == oid, (name, got.u.which)
    assert getattr(got.u, name) == v, (name, getattr(got.u, name))
    assert M_U.from_jsonable(m.u.to_jsonable()) == m.u, name
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
