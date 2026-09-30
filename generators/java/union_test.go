package java

import (
	"strings"
	"testing"
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

const unionDir = "src/main/java/message/"

func genUnion(t *testing.T) map[string]string {
	t.Helper()
	return genJavaFromYAML(t, unionSrc, map[string]any{"emit": "project"})
}

// methodBody returns the text of the method whose declaration line starts with
// sig (at the class-member indent), up to its closing brace.
func methodBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, "\n    "+sig)
	if i < 0 {
		t.Fatalf("no method %q in:\n%s", sig, src)
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
// own type, no Object slot, and only default_id's slot constructed -- a fresh
// union allocates nothing for the options it does not hold.
func TestJavaUnionStorage(t *testing.T) {
	u := genUnion(t)[unionDir+"M_U.java"]
	mustContain(t, "MU", u,
		"public static final int NUM_ID = 0;",
		"public static final int PT_ID = 2;",
		"    private int which = PT_ID;\n",
		"    private long _num;\n",
		"    private String _s;\n",
		"    private M_U_Pt _pt = new M_U_Pt();\n",
		"    private short[] _arr;\n",
		"    private List<String> _strs;\n",
		"    private byte[] _bl;\n",
		"    private M_U_Inner _inner;\n",
		"    private float _f;\n",
		"    private List<Boolean> _flags;\n",
		"public int which() { return which; }")
	mustNotContain(t, "MU", u, "Object ", "public long num", "new M_U_Inner();\n")
	// A scalar default_id's slot holds its default from the start.
	z := genUnion(t)[unionDir+"M_Z.java"]
	mustContain(t, "MZ", z, "    private int which = A_ID;\n", "    private long _a;\n", "    private long _b;\n")
}

// A getter reads the slot only while its option is held and otherwise returns
// the option's own default, storing nothing; a setter selects; mutable<Opt>()
// selects at the default ONLY when another option is held.
func TestJavaUnionAccessors(t *testing.T) {
	u := genUnion(t)[unionDir+"M_U.java"]
	mustContain(t, "MU", u,
		"public long getNum() { return which == NUM_ID ? _num : 5L; }",
		"public boolean hasNum() { return which == NUM_ID; }",
		"public void setNum(long v) { which = NUM_ID; this._num = v; }",
		`public String getS() { return which == S_ID ? _s : ""; }`,
		"public M_U_Pt getPt() { return which == PT_ID ? _pt : new M_U_Pt(); }",
		"public short[] getArr() { return which == ARR_ID ? _arr : Seq.EMPTY_SHORTS; }",
		"public List<String> getStrs() { return which == STRS_ID ? _strs : new ArrayList<>(); }",
		"public byte[] getBl() { return which == BL_ID ? _bl : Seq.EMPTY_BYTES; }",
		"public float getF() { return which == F_ID ? _f : 1.5f; }")
	mustContain(t, "MU.mutablePt", methodBody(t, u, "public M_U_Pt mutablePt() {"),
		"if (which != PT_ID || _pt == null) {\n            if (_pt == null) _pt = new M_U_Pt(); else _pt.reset();\n            which = PT_ID;\n        }\n        return _pt;")
	mustContain(t, "MU.mutableStrs", methodBody(t, u, "public List<String> mutableStrs() {"),
		"if (which != STRS_ID || _strs == null) {\n            _strs = Seq.reset(_strs);\n            which = STRS_ID;")
	mustContain(t, "MU.mutableFlags", u, "public List<Boolean> mutableFlags() {")
	// A primitive array is replaced whole through its setter: no mutable accessor.
	mustNotContain(t, "MU", u, "mutableArr", "mutableNum", "mutableS(")
	mustContain(t, "MU.reset", methodBody(t, u, "public void reset() {"),
		"which = PT_ID;\n        if (_pt == null) _pt = new M_U_Pt(); else _pt.reset();")
	mustContain(t, "MZ.reset", methodBody(t, genUnion(t)[unionDir+"M_Z.java"], "public void reset() {"),
		"which = A_ID;\n        _a = 0L;")
}

// Encode: one arm per held option. default_id is written like an ordinary field
// (omitted at its default, a struct closed with the DROPPING end); every other
// option is forced -- unguarded, a compact array as its count, a struct, union or
// wrapper-array option closed with the KEEPING end.
func TestJavaUnionEncodeArms(t *testing.T) {
	u := genUnion(t)[unionDir+"M_U.java"]
	ser := methodBody(t, u, "public void serialize(OStream os) throws IOException {")
	mustContain(t, "MU.serialize", ser,
		"switch (which) {",
		"case NUM_ID: {\n            os.writeUnsigned(0, this._num);\n            break;",
		`os.writeString(1, this._s == null ? "" : this._s);`,
		"os.writeSequenceBeginLazy(2); (this._pt == null ? new M_U_Pt() : this._pt).serialize(os); os.writeSequenceEnd();",
		"os.writeArrayUnsigned(3, (this._arr == null ? Seq.EMPTY_SHORTS : this._arr));",
		"os.writeBlob(5, this._bl == null ? Seq.EMPTY_BYTES : this._bl);",
		"os.writeSequenceBeginLazy(6); (this._inner == null ? new M_U_Inner() : this._inner).serialize(os); os.writeSequenceEndKeep();",
		"case F_ID: {\n            os.writeFp32(7, this._f);",
		"os.writeArrayUnsigned(8, Seq.boolsToLongs(Seq.orEmpty(this._flags)));")
	strs := ser[strings.Index(ser, "case STRS_ID:"):strings.Index(ser, "case BL_ID:")]
	mustContain(t, "MU.serialize strs", strs, "os.writeSequenceBeginLazy(4);", "os.writeSequenceEndKeep();")
	// No ≠-default guard on any forced option.
	mustNotContain(t, "MU.serialize", ser, "if (this.num", "if (this.f", "!this._s.isEmpty()", "this._arr.length != 0", "if (this.bl")
	mustContain(t, "MU.isDefault", methodBody(t, u, "boolean isDefault() {"),
		"return which == PT_ID && !(this._pt != null && !this._pt.isDefault());")
	z := genUnion(t)[unionDir+"M_Z.java"]
	mustContain(t, "MZ", z,
		"case A_ID: {\n            if (this._a != 0L) { os.writeUnsigned(0, this._a); }",
		"case B_ID: {\n            os.writeUnsigned(1, this._b);",
		"return which == A_ID && !(this._a != 0L);")
}

// Decode: every store into a union option is its setter, and every path into a
// struct/union/List option is its mutable accessor -- both only past the §7.3
// gate of that option's kind. Nothing is selected in fixlenBegin, which fires
// before the payload and for whatever fixlen subtype arrived.
func TestJavaUnionDecodeSwitch(t *testing.T) {
	m := genUnion(t)[unionDir+"M.java"]
	vis := m[strings.Index(m, "class _M__Visitor"):]
	mustContain(t, "scalar option", vis,
		`if (value < 0 || value > 65535L) throw Sofab.invalid("num: value outside declared width u16"); m.u.setNum(value); break;`,
		"case 7: m.u.setF(value); break;",
		// A member below a struct option goes through the option's mutable accessor.
		`throw Sofab.invalid("x: value outside declared width i32"); m.u.mutablePt().x = value; break;`,
		`throw Sofab.invalid("b: value outside declared width i8"); m.u.mutableInner().setB(value); break;`)
	fixlen := methodBody(t, vis, "public void fixlenBegin(")
	mustNotContain(t, "fixlenBegin", fixlen, "setS", "setBl", "mutable")
	str := methodBody(t, vis, "public void string(")
	acc := strings.Index(str, "acc.string(")
	sel := strings.Index(str, "m.u.setS(_s)")
	if acc < 0 || sel < acc {
		t.Errorf("a string option must be selected at the completion store, after the accumulator:\n%s", str)
	}
	mustContain(t, "blob option", methodBody(t, vis, "public void blob("), "case 5: m.u.setBl(_b); break;")
	mustContain(t, "wrapper-array element", str, "Seq.placeElem(m.u.mutableStrs(), id, \"\", _s,")
	ab := methodBody(t, vis, "public void arrayBegin(")
	mustContain(t, "compact array option", ab,
		`case 3: if (kind != ArrayKind.UNSIGNED) break; if (count > 4) throw Sofab.invalid("arr: array count above schema capacity 4"); askip = 0; afill = count; atgt = 1; m.u.setArr(new short[count]); abulk = m.u.getArr(); break;`,
		`case 8: if (kind != ArrayKind.UNSIGNED) break; if (count > 2) throw Sofab.invalid("flags: array count above schema capacity 2"); askip = 0; afill = count; atgt = 2; m.u.mutableFlags().clear(); break;`)
	mustContain(t, "array fill", vis,
		"m.u.getArr()[ai++] = (short) value; return;",
		"m.u.mutableFlags().add(value != 0); return;")
	sb := methodBody(t, vis, "public void sequenceBegin(")
	mustContain(t, "sequence options", sb,
		"case 2: m.u.mutablePt(); cur = ",
		"case 4: m.u.mutableStrs().clear(); cur = ",
		"case 6: m.u.mutableInner(); cur = ",
		// An array of unions fills its gaps from the element type's constructor --
		// per type, so each $defs split fills with its own default option.
		"Seq.reserveElem(m.v, id, M_V::new, SCHEMA_COUNT_3);",
		"Seq.reserveElem(m.pe, id, Pick__DefaultN::new, SCHEMA_COUNT_3);",
		"m.v.get(_ex_Root_v).mutableP(); cur = ",
		"case 1: m.pf.mutableT(); cur = ")
	mustNotContain(t, "sequence options", sb, "setPt(", "setInner(", "new M_U_Pt()")
}

// One type per (union, default_id): the $defs union used with default_id 1 and
// with the omitted (lowest) one are two classes, each constructing its own
// default option.
func TestJavaUnionSplitDefaults(t *testing.T) {
	out := genUnion(t)
	mustContain(t, "Pick_default_t", out[unionDir+"Pick__DefaultT.java"],
		"private int which = T_ID;", "private Pick_T _t = new Pick_T();")
	mustContain(t, "Pick_default_n", out[unionDir+"Pick__DefaultN.java"],
		"private int which = N_ID;", "private long _n = 6L;", "private Pick_T _t;\n")
	mustContain(t, "M", out[unionDir+"M.java"],
		"public Pick__DefaultT pf = new Pick__DefaultT();")
}

// The JSON harness renders exactly the held option and selects what it reads.
func TestJavaUnionJSONHarness(t *testing.T) {
	j := genUnion(t)[unionDir+"harness/Json.java"]
	to := methodBody(t, j, "static void to(message.M_U o, StringBuilder b) {")
	mustContain(t, "Json.to(MU)", to,
		"switch (o.which()) {",
		"case message.M_U.NUM_ID: {\n        b.append(\"\\\"num\\\":\");\n        b.append(o.getNum());",
		"to(o.getPt(), b);")
	from := methodBody(t, j, "static void from(JsonObject j, message.M_U o) {")
	mustContain(t, "Json.from(MU)", from,
		"for (Map.Entry<String, JsonElement> me : j.entrySet()) {",
		`case "num": {`+"\n            o.setNum(e.getAsLong());",
		"from(e.getAsJsonObject(), o.mutablePt());",
		"o.setArr(_u);",
		"List<String> _u = o.mutableStrs();",
		"o.setBl(Json.toBytes(e.getAsJsonArray()));")
}

// An option named after Object's final getClass is mangled; an option named
// after the tag, or after another option's id constant, is kept apart by the
// slot channel: every slot is `_` + the option name, which no tag, constant or
// other slot spells. Nothing is refused.
func TestJavaUnionNames(t *testing.T) {
	out := genJavaFromYAML(t, `version: 1
messages:
  N:
    payload:
      u: { id: 0, type: union, oneof: { class: { id: 0, type: u8 }, which: { id: 1, type: string, maxlen: 4 }, a: { id: 2, type: u8 }, A_ID: { id: 3, type: u8 } } }
`, map[string]any{})
	mustContain(t, "N_U", out[unionDir+"N_U.java"],
		"public long getClass_() { return which == CLASS_ID ? _class : 0L; }",
		"public void setClass_(long v) { which = CLASS_ID; this._class = v; }",
		"private String _which;",
		"public String getWhich() { return which == WHICH_ID ? _which : \"\"; }",
		"public static final int A_ID = 2;",
		"public static final int A_ID_ID = 3;",
		"private long _a;",
		"private long _A_ID;",
		"public long getAID() { return which == A_ID_ID ? _A_ID : 0L; }")
}
