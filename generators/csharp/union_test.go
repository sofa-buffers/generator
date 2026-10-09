package csharp

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// unionSrc has a union whose default option is a struct at a non-zero default
// and NOT its first option, one of every option kind the encode rule treats
// differently (scalar, string, compact array, wrapper array, boolean array, blob,
// union, fp), a union whose default option is a scalar, an array of unions whose
// default option is a struct, and a $defs union used with two default_ids (one
// type per default_id).
const unionSrc = `version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  M:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:    { id: 5, type: blob, maxlen: 4 }
          inner: { id: 6, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
          f:     { id: 7, type: fp32, default: 1.5 }
          flags: { id: 8, type: array, items: { type: boolean, count: 2 } }
      z: { id: 1, type: union, default_id: 0, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8, default: 3 } } }
      v: { id: 2, type: array, items: { type: union, count: 3, default_id: 1, oneof: { a: { id: 0, type: u8 }, p: { id: 1, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } } } } }
      pf: { id: 4, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      pe: { id: 5, type: array, items: { type: union, count: 3, oneof: { $ref: "#/$defs/union/Pick" } } }
`

// genCs generates src as a project and returns every file by path.
func genCs(t *testing.T, src string) map[string]string {
	t.Helper()
	files, err := genCsErr(t, src, map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return files
}

func genCsErr(t *testing.T, src string, cfg map[string]any) (map[string]string, error) {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "union.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out, nil
}

// classBody returns the text of `public sealed class <name> {` up to its closing
// brace at column 0.
func classBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "public sealed class "+name+" {")
	if i < 0 {
		t.Fatalf("no class %s in:\n%s", name, src)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+3]
	}
	return rest
}

// memberBody returns the member whose declaration line starts with sig (at the
// class-member indent), up to its closing brace.
func memberBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, "\n    "+sig)
	if i < 0 {
		t.Fatalf("no member %q in:\n%s", sig, src)
	}
	rest := src[i+1:]
	if j := strings.Index(rest, "\n    }\n"); j >= 0 {
		return rest[:j+7]
	}
	return rest
}

func mustContain(t *testing.T, what, src string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(src, w) {
			t.Errorf("%s: missing %q in:\n%s", what, w, src)
		}
	}
}

func mustNotContain(t *testing.T, what, src string, bads ...string) {
	t.Helper()
	for _, b := range bads {
		if strings.Contains(src, b) {
			t.Errorf("%s: must not contain %q in:\n%s", what, b, src)
		}
	}
}

// A union is a tag plus one typed slot per option: primitives unboxed in their
// own type, no object slot, and only default_id's slot constructed -- a fresh
// union allocates nothing for the options it does not hold.
func TestCsUnionStorage(t *testing.T) {
	mod := genCs(t, unionSrc)["Message.cs"]
	u := classBody(t, mod, "M_U")
	mustContain(t, "M_U", u,
		"    public const int Id_Num = 0;\n",
		"    public const int Id_Pt = 2;\n",
		"    private int _which = Id_Pt;\n",
		"    private ushort _num;\n",
		"    private string _s;\n",
		"    private M_U_Pt _pt = new();\n",
		"    private ushort[] _arr;\n",
		"    private global::System.Collections.Generic.List<string> _strs;\n",
		"    private byte[] _bl;\n",
		"    private M_U_Inner _inner;\n",
		"    private float _f;\n",
		"    private global::System.Collections.Generic.List<bool> _flags;\n",
		"    public int Which => _which;\n")
	mustNotContain(t, "M_U", u, "object ", "public ushort num", "private M_U_Inner _inner = new", "private ushort _num = 5")
	// A scalar default_id's slot holds its default from the start.
	z := classBody(t, mod, "M_Z")
	mustContain(t, "M_Z", z, "    private int _which = Id_A;\n", "    private byte _a;\n", "    private byte _b;\n")
	// The union member of the message is an ordinary constructed field.
	mustContain(t, "M", classBody(t, mod, "M"), "public M_U u = new();")
}

// A getter reads the slot only while its option is held and otherwise returns
// the option's own default, storing nothing; a setter selects; Mutable<Opt>()
// selects at the default ONLY when another option is held.
func TestCsUnionAccessors(t *testing.T) {
	u := classBody(t, genCs(t, unionSrc)["Message.cs"], "M_U")
	mustContain(t, "M_U", u,
		"    public ushort Num {\n        get { if (_which == Id_Num) return _num; return 5; }\n        set { _which = Id_Num; _num = value; }\n    }\n",
		"    public bool Has_Num => _which == Id_Num;\n",
		`get { if (_which == Id_S) return _s; return ""; }`,
		"get { if (_which == Id_Pt) return _pt; return new M_U_Pt(); }",
		"get { if (_which == Id_Arr) return _arr; return global::System.Array.Empty<ushort>(); }",
		"get { if (_which == Id_Strs) return _strs; return new global::System.Collections.Generic.List<string>(); }",
		"get { if (_which == Id_Bl) return _bl; return global::System.Array.Empty<byte>(); }",
		"get { if (_which == Id_F) return _f; return 1.5f; }",
		"set { _which = Id_Pt; _pt = value; }")
	mustContain(t, "M_U.Mutable_Pt", memberBody(t, u, "public M_U_Pt Mutable_Pt() {"),
		"if (_which != Id_Pt || _pt == null) { _pt = new M_U_Pt(); _which = Id_Pt; }\n        return _pt;")
	mustContain(t, "M_U.Mutable_Strs", memberBody(t, u, "public global::System.Collections.Generic.List<string> Mutable_Strs() {"),
		"if (_which != Id_Strs || _strs == null) { _strs = new global::System.Collections.Generic.List<string>(); _which = Id_Strs; }")
	mustContain(t, "M_U.Mutable_Flags", u, "public global::System.Collections.Generic.List<bool> Mutable_Flags() {")
	mustContain(t, "M_U.Mutable_Inner", u, "public M_U_Inner Mutable_Inner() {")
	// A primitive array is replaced whole through its setter: no mutable accessor.
	mustNotContain(t, "M_U", u, "Mutable_Arr", "Mutable_Num", "Mutable_S(", "Mutable_Bl")
	mustContain(t, "M_U.Clear", memberBody(t, u, "public void Clear() {"),
		"_which = Id_Pt;\n        _pt = new M_U_Pt();")
	mustContain(t, "M_Z.Clear", memberBody(t, classBody(t, genCs(t, unionSrc)["Message.cs"], "M_Z"), "public void Clear() {"),
		"_which = Id_A;\n        _a = 0;")
}

// Encode: one arm per held option. default_id is written like an ordinary field
// (omitted at its default, a struct closed with the DROPPING end); every other
// option is forced -- unguarded, a compact array as its count, a struct, union or
// wrapper-array option closed with the KEEPING end.
func TestCsUnionEncodeArms(t *testing.T) {
	mod := genCs(t, unionSrc)["Message.cs"]
	u := classBody(t, mod, "M_U")
	ser := memberBody(t, u, "public void Serialize(global::sofab.OStream os) {")
	mustContain(t, "M_U.Serialize", ser,
		"switch (_which) {",
		"case Id_Num: {\n            os.WriteUnsigned(0, (ulong)this._num);\n            break;",
		`os.WriteString(1, this._s ?? "", 8);`,
		"os.WriteSequenceBeginLazy(2); (this._pt ?? new M_U_Pt()).Serialize(os); os.WriteSequenceEnd();",
		"os.WriteArrayUnsigned(3, (this._arr ?? global::System.Array.Empty<ushort>()));",
		"os.WriteBlob(5, this._bl ?? global::System.Array.Empty<byte>());",
		"os.WriteSequenceBeginLazy(6); (this._inner ?? new M_U_Inner()).Serialize(os); os.WriteSequenceEndKeep();",
		"case Id_F: {\n            os.WriteFp32(7, this._f);",
		"var _o = this._flags ?? new global::System.Collections.Generic.List<bool>();\n"+
			"            if (_o.Count > 2) throw new global::sofab.SofabException(global::sofab.SofabError.Argument, \"flags: array count above schema capacity 2\");\n"+
			"            os.WriteArrayUnsigned(8, global::System.Array.ConvertAll(_o.ToArray(), _x => _x ? (byte)1 : (byte)0));")
	strs := ser[strings.Index(ser, "case Id_Strs:"):strings.Index(ser, "case Id_Bl:")]
	mustContain(t, "M_U.Serialize strs", strs, "var _o = this._strs ?? new global::System.Collections.Generic.List<string>();", "os.WriteSequenceBeginLazy(4);", "os.WriteSequenceEndKeep();")
	// No ≠-default guard on any forced option.
	mustNotContain(t, "M_U.Serialize", ser, "if (this._num", "if (this._f", "if (this._s", "this._arr.Length != 0", "if (this._bl", "_o.Count != 0")
	mustContain(t, "M_U.IsDefault", u,
		"public bool IsDefault() => _which == Id_Pt && ((this._pt ?? new M_U_Pt()).IsDefault());")
	z := classBody(t, mod, "M_Z")
	mustContain(t, "M_Z", z,
		"case Id_A: {\n            if (this._a != 0) { os.WriteUnsigned(0, (ulong)this._a); }",
		"case Id_B: {\n            os.WriteUnsigned(1, (ulong)this._b);",
		"public bool IsDefault() => _which == Id_A && (this._a == 0);")
}

// Decode: every store into a union option is its setter, and every path into a
// struct/union/List option is its mutable accessor -- both only past the §7.3
// gate of that option's kind. Nothing is selected in FixlenBegin, which fires
// before the payload and for whatever fixlen subtype arrived.
func TestCsUnionDecodeSwitch(t *testing.T) {
	mod := genCs(t, unionSrc)["Message.cs"]
	vis := mod[strings.Index(mod, "internal sealed class _M__Visitor"):]
	mustContain(t, "scalar option", vis,
		`case (Root_u, 0): if (value > 65535) throw new global::sofab.SofabException(global::sofab.SofabError.InvalidMessage, "num: value outside declared width u16"); m.u.Num = (ushort)value; break;`,
		"case (Root_u, 7): m.u.F = value; break;",
		// A member below a struct option goes through the option's mutable accessor.
		`"x: value outside declared width i32"); m.u.Mutable_Pt().x = (int)value; break;`,
		`"b: value outside declared width i8"); m.u.Mutable_Inner().B = (sbyte)value; break;`)
	fixlen := memberBody(t, vis, "public void FixlenBegin(")
	mustNotContain(t, "FixlenBegin", fixlen, "m.u.S", "m.u.Bl", "Mutable")
	str := memberBody(t, vis, "public void String(")
	acc := strings.Index(str, "if (_s == null) return;")
	sel := strings.Index(str, "m.u.S = _s;")
	if acc < 0 || sel < acc {
		t.Errorf("a string option must be selected at the completion store, after the payload is whole:\n%s", str)
	}
	mustContain(t, "blob option", memberBody(t, vis, "public void Blob("), "case (Root_u, 5): m.u.Bl = _b; break;")
	mustContain(t, "wrapper-array element", str, `global::sofab.Seq.PlaceElem(m.u.Mutable_Strs(), id, "", _s, 3, MaxDynArrayCount);`)
	ab := memberBody(t, vis, "public void ArrayBegin(")
	mustContain(t, "compact array option", ab,
		`case (Root_u, 3): if (kind != global::sofab.ArrayKind.Unsigned) break; if (count > 4) throw new global::sofab.SofabException(global::sofab.SofabError.InvalidMessage, "arr: array count above schema capacity 4"); m.u.Arr = new ushort[count]; break;`,
		`case (Root_u, 8): if (kind != global::sofab.ArrayKind.Unsigned) break; if (count > 2) throw new global::sofab.SofabException(global::sofab.SofabError.InvalidMessage, "flags: array count above schema capacity 2"); m.u.Mutable_Flags().Clear(); break;`)
	mustContain(t, "array fill", vis,
		"m.u.Arr[ai++] = (ushort)value; break;",
		"m.u.Flags.Add(value != 0); break;")
	sb := memberBody(t, vis, "public void SequenceBegin(")
	mustContain(t, "sequence options", sb,
		"case (Root_u, 2): m.u.Mutable_Pt(); cur = Root_u_pt; break;",
		"case (Root_u, 4): m.u.Mutable_Strs().Clear(); cur = Root_u_strs; break;",
		"case (Root_u, 6): m.u.Mutable_Inner(); cur = Root_u_inner; break;",
		// An array of unions fills its gaps from the element type's constructor --
		// per type, so each $defs split fills with its own default option.
		"global::sofab.Seq.ReserveElem(m.v, id, static () => new M_V(), 3, MaxDynArrayCount);",
		"global::sofab.Seq.ReserveElem(m.pe, id, static () => new Pick__DefaultN(), 3, MaxDynArrayCount);",
		"m.v[_ixRoot_v].Mutable_P(); cur = Root_v_e_p;",
		"case (Root_pf, 1): m.pf.Mutable_T(); cur = Root_pf_t; break;")
	mustNotContain(t, "sequence options", sb, "m.u.Pt =", "m.u.Inner =", "new M_U_Pt()")
}

// One type per (union, default_id): the $defs union used with default_id 1 and
// with the omitted (lowest) one are two classes, each constructing its own
// default option.
func TestCsUnionSplitDefaults(t *testing.T) {
	mod := genCs(t, unionSrc)["Message.cs"]
	mustContain(t, "Pick_default_t", classBody(t, mod, "Pick__DefaultT"),
		"private int _which = Id_T;", "private Pick_T _t = new();")
	mustContain(t, "Pick_default_n", classBody(t, mod, "Pick__DefaultN"),
		"private int _which = Id_N;", "private ushort _n = 6;", "private Pick_T _t;\n")
	mustContain(t, "M", classBody(t, mod, "M"), "public Pick__DefaultT pf = new();")
}

// The JSON harness renders exactly the held option and selects what it reads,
// through a converter registered for every union type. It imports no generated
// namespace: every generated type is global::<ns>.<T>, and its own classes are
// private names (`_` first).
func TestCsUnionJSONHarness(t *testing.T) {
	p := genCs(t, unionSrc)["Program.cs"]
	mustContain(t, "Program.cs", p,
		"sealed class _M_U__JsonConverter : JsonConverter<global::Message.M_U> {",
		"switch (v.Which) {",
		`case global::Message.M_U.Id_Num: w.WritePropertyName("num"); JsonSerializer.Serialize(w, v.Num, o); break;`,
		`case global::Message.M_U.Id_Pt: w.WritePropertyName("pt"); JsonSerializer.Serialize(w, v.Pt, o); break;`,
		`case "num": u.Num = JsonSerializer.Deserialize<ushort>(ref r, o); break;`,
		`case "strs": u.Strs = JsonSerializer.Deserialize<global::System.Collections.Generic.List<string>>(ref r, o); break;`,
		"sealed class _Pick__DefaultN__JsonConverter : JsonConverter<global::Message.Pick__DefaultN> {",
		"Converters = { new _ByteArrayConverter(), new _Pick__DefaultN__JsonConverter(), new _Pick__DefaultT__JsonConverter(), new _M_U__JsonConverter(), new _M_U_Inner__JsonConverter(), new _M_V__JsonConverter(), new _M_Z__JsonConverter() }")
	mustNotContain(t, "Program.cs", p, "using Message;")
	// The library itself stays JSON-free.
	mustNotContain(t, "Message.cs", genCs(t, unionSrc)["Message.cs"], "Json")
}

// An option landing on one of the union's own members takes the trailing
// underscore; the derived members are a word, `_` and the unescaped option
// name, so options the old spelling folded together (`a`'s AId and `a_id`,
// `x`'s HasX and `has_x`) are distinct members now, and nothing is refused.
func TestCsUnionNames(t *testing.T) {
	mod := genCs(t, `version: 1
messages:
  N:
    payload:
      u: { id: 0, type: union, oneof: { which: { id: 0, type: u8 }, clear: { id: 1, type: string, maxlen: 4 }, n_u: { id: 2, type: u8 }, a: { id: 3, type: struct, fields: { v: { id: 0, type: u8 } } }, a_id: { id: 4, type: u8 }, has_a: { id: 5, type: u8 }, mutable_a: { id: 6, type: u8 } } }
`)["Message.cs"]
	u := classBody(t, mod, "N_U")
	mustContain(t, "N_U", u,
		"public const int Id_Which = 0;",
		"private byte _which_;",
		"public byte Which_ {",
		"public bool Has_Which => _which == Id_Which;",
		"public string Clear_ {",
		"public byte NU {",
		"public const int Id_A = 3;", "public const int Id_AId = 4;",
		"public byte AId {", "public bool Has_A =>", "public byte HasA {",
		"public N_U_A Mutable_A() {", "public byte MutableA {")
}

// A union type named like one of its own members (CS0542) takes the type
// escape; its members keep their names. The inline union of field `x` in
// message `has` is Has_X, and its option `x` declares the member Has_X.
func TestCsUnionTypeNamedLikeItsMember(t *testing.T) {
	mod := genCs(t, `version: 1
messages:
  has:
    payload:
      x: { id: 0, type: union, oneof: { x: { id: 0, type: u8 }, y: { id: 1, type: u8 } } }
`)["Message.cs"]
	u := classBody(t, mod, "Has_X_")
	mustContain(t, "Has_X_", u, "public bool Has_X => _which == Id_X;")
	mustContain(t, "Has", classBody(t, mod, "Has"), "public Has_X_ x = new();")
}
