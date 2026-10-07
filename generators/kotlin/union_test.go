package kotlin

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

const unionDir = "src/main/kotlin/message/"

func genUnion(t *testing.T) map[string]string {
	t.Helper()
	return genFromYAML(t, unionSrc, map[string]any{"emit": "project"})
}

// memberBody returns the text of the class member whose declaration line starts
// with sig (at the class-member indent), up to its closing brace.
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

// A union is a tag plus one typed slot per option: every slot at the option's
// exact Kotlin type (no Any, no boxing), and only default_id's slot holds an
// object -- a fresh union allocates nothing for the struct, union and list
// options it does not hold.
func TestKotlinUnionStorage(t *testing.T) {
	u := genUnion(t)[unionDir+"M_U.kt"]
	mustContain(t, "M_U", u,
		"public const val NUM_ID: Int = 0",
		"public const val PT_ID: Int = 2",
		"    public var which: Int = PT_ID\n        private set\n",
		"    private var _num: UShort = 5u.toUShort()\n",
		"    private var _s: String = \"\"\n",
		"    private var _pt: M_U_Pt? = M_U_Pt()\n",
		"    private var _arr: UShortArray = Seq.EMPTY_USHORTS\n",
		"    private var _strs: MutableList<String>? = null\n",
		"    private var _bl: ByteArray = Seq.EMPTY_BYTES\n",
		"    private var _inner: M_U_Inner? = null\n",
		"    private var _f: Float = 1.5f\n",
		"    private var _flags: BooleanArray = Seq.EMPTY_BOOLEANS\n")
	mustNotContain(t, "M_U", u, "Any", "public var num: UShort =", "M_U_Inner()\n    private")
	// A scalar default_id's slot holds its default from the start.
	z := genUnion(t)[unionDir+"M_Z.kt"]
	mustContain(t, "M_Z", z, "    public var which: Int = A_ID\n", "    private var _a: UByte = 0u.toUByte()\n", "    private var _b: UByte = 3u.toUByte()\n")
}

// The property getter reads the slot only while its option is held and
// otherwise returns the option's own default, storing nothing; the setter
// selects; mutable<Opt>() selects at the default ONLY when another option is
// held, and never resets a held one.
func TestKotlinUnionAccessors(t *testing.T) {
	u := genUnion(t)[unionDir+"M_U.kt"]
	mustContain(t, "M_U", u,
		"    public var num: UShort\n        get() = if (which == NUM_ID) this._num else 5u.toUShort()\n        set(v) { which = NUM_ID; _num = v }\n",
		"public fun hasNum(): Boolean = which == NUM_ID",
		`get() = if (which == S_ID) this._s else ""`,
		"get() = if (which == PT_ID) this._pt!! else M_U_Pt()",
		"get() = if (which == ARR_ID) this._arr else Seq.EMPTY_USHORTS",
		"get() = if (which == STRS_ID) this._strs!! else mutableListOf()",
		"get() = if (which == BL_ID) this._bl else Seq.EMPTY_BYTES",
		"get() = if (which == F_ID) this._f else 1.5f",
		"set(v) { which = PT_ID; _pt = v }")
	mustContain(t, "M_U.mutablePt", memberBody(t, u, "public fun mutablePt(): M_U_Pt {"),
		"var s = _pt\n        if (s == null) { s = M_U_Pt(); _pt = s } else if (which != PT_ID) s.reset()\n        which = PT_ID\n        return s")
	mustContain(t, "M_U.mutableStrs", memberBody(t, u, "public fun mutableStrs(): MutableList<String> {"),
		"if (s == null) { s = mutableListOf(); _strs = s } else if (which != STRS_ID) s.clear()")
	mustContain(t, "M_U.mutableInner", u, "public fun mutableInner(): M_U_Inner {")
	// A primitive array (boolean included) is replaced whole through its
	// property: no mutable accessor.
	mustNotContain(t, "M_U", u, "mutableArr", "mutableNum", "mutableS(", "mutableFlags", "mutableBl")
	mustContain(t, "M_U.reset", memberBody(t, u, "public fun reset() {"),
		"which = PT_ID\n        val s = _pt; if (s == null) _pt = M_U_Pt() else s.reset()")
	mustContain(t, "M_Z.reset", memberBody(t, genUnion(t)[unionDir+"M_Z.kt"], "public fun reset() {"),
		"which = A_ID\n        _a = 0u.toUByte()")
}

// Encode: one arm per held option. default_id is written like an ordinary field
// (omitted at its default, a struct closed with the DROPPING end); every other
// option is forced -- unguarded, a compact array as its count, a struct, union or
// wrapper-array option closed with the KEEPING end.
func TestKotlinUnionEncodeArms(t *testing.T) {
	u := genUnion(t)[unionDir+"M_U.kt"]
	ser := memberBody(t, u, "public fun serialize(os: OStream) {")
	mustContain(t, "M_U.serialize", ser,
		"when (which) {",
		"NUM_ID -> {\n                os.writeUnsigned(0, this._num.toLong())\n            }",
		"S_ID -> {\n                os.writeString(1, this._s)\n            }",
		"PT_ID -> {\n                os.writeSequenceBeginLazy(2); this._pt!!.serialize(os); os.writeSequenceEnd()\n            }",
		"ARR_ID -> {\n                os.writeArrayUnsigned(3, this._arr.asShortArray())\n            }",
		"BL_ID -> {\n                os.writeBlob(5, this._bl)\n            }",
		"os.writeSequenceBeginLazy(6); this._inner!!.serialize(os); os.writeSequenceEndKeep()",
		"F_ID -> {\n                os.writeFp32(7, this._f, this.fFp32Bits)\n            }",
		"FLAGS_ID -> {\n                os.writeArrayUnsigned(8, Seq.boolsToBytes(this._flags))\n            }")
	strs := ser[strings.Index(ser, "STRS_ID ->"):strings.Index(ser, "BL_ID ->")]
	mustContain(t, "M_U.serialize strs", strs, "= this._strs!!", "os.writeSequenceBeginLazy(4)", "os.writeSequenceEndKeep()")
	// No ≠-default guard on any forced option.
	mustNotContain(t, "M_U.serialize", ser, "if (this._num", "if (this._f", "this._s.isNotEmpty()", "this._arr.isNotEmpty()", "this._bl.isNotEmpty()", "this._flags.isNotEmpty()")
	mustContain(t, "M_U.isDefault", u,
		"internal fun isDefault(): Boolean = which == PT_ID && this._pt!!.isDefault()")
	z := genUnion(t)[unionDir+"M_Z.kt"]
	mustContain(t, "M_Z", z,
		"A_ID -> {\n                if (this._a != 0u.toUByte()) os.writeUnsigned(0, this._a.toLong())",
		"B_ID -> {\n                os.writeUnsigned(1, this._b.toLong())",
		"internal fun isDefault(): Boolean = which == A_ID && !(this._a != 0u.toUByte())")
}

// Decode: every store into a union option is its property setter, and every
// path into a struct/union/list option is its mutable accessor -- both only past
// the §7.3 gate of that option's kind. Nothing is selected in fixlenBegin, which
// fires before the payload and for whatever fixlen subtype arrived.
func TestKotlinUnionDecodeSwitch(t *testing.T) {
	m := genUnion(t)[unionDir+"M.kt"]
	vis := m[strings.Index(m, "internal class _M__Visitor"):]
	mustContain(t, "scalar option", vis,
		`0 -> { if (value < 0L || value > 65535L) throw SofabException(SofabError.INVALID_MSG, "num: value outside declared width u16"); m.u.num = value.toUShort() }`,
		"7 -> { m.u.f = Float.fromBits(bits); m.u.fFp32Bits = Seq.fp32NaNBits(bits) }",
		// A member below a struct/union option goes through the option's mutable
		// accessor.
		`throw SofabException(SofabError.INVALID_MSG, "x: value outside declared width i32"); m.u.mutablePt().x = value.toInt() }`,
		`throw SofabException(SofabError.INVALID_MSG, "b: value outside declared width i8"); m.u.mutableInner().b = value.toByte() }`)
	fixlen := memberBody(t, vis, "override fun fixlenBegin(")
	mustNotContain(t, "fixlenBegin", fixlen, "m.u.s =", "m.u.bl =", "mutable")
	str := memberBody(t, vis, "override fun string(")
	acc := strings.Index(str, "acc.string(")
	sel := strings.Index(str, "m.u.s = s")
	if acc < 0 || sel < acc {
		t.Errorf("a string option must be selected at the completion store, after the accumulator:\n%s", str)
	}
	mustContain(t, "blob option", memberBody(t, vis, "override fun blob("), "5 -> m.u.bl = b")
	mustContain(t, "wrapper-array element", str, "Seq.placeElem(m.u.mutableStrs(), id, \"\", s,")
	ab := memberBody(t, vis, "override fun arrayBegin(")
	mustContain(t, "compact array option", ab,
		`3 -> if (kind == ArrayKind.UNSIGNED) { if (count > 4) throw SofabException(SofabError.INVALID_MSG, "arr: array count above schema capacity 4"); askip = 0; afill = count; atgt = 1; m.u.arr = UShortArray(count); abulk = m.u.arr.asShortArray() }`,
		`8 -> if (kind == ArrayKind.UNSIGNED) { if (count > 2) throw SofabException(SofabError.INVALID_MSG, "flags: array count above schema capacity 2"); askip = 0; afill = count; atgt = 2; m.u.flags = BooleanArray(count) }`)
	mustContain(t, "array fill", vis,
		"m.u.arr[ai] = value.toUShort(); ai++",
		"m.u.flags[ai] = value != 0L; ai++")
	sb := memberBody(t, vis, "override fun sequenceBegin(")
	mustContain(t, "sequence options", sb,
		"2 -> { m.u.mutablePt(); cur = ",
		"4 -> { m.u.mutableStrs().clear(); cur = ",
		"6 -> { m.u.mutableInner(); cur = ",
		// An array of unions fills its gaps from the element type's constructor --
		// per type, so each $defs split fills with its own default option.
		"Seq.reserveElem(m.v, id, 3, MAX_DYN_ARRAY_COUNT) { M_V() }",
		"Seq.reserveElem(m.pe, id, 3, MAX_DYN_ARRAY_COUNT) { Pick__DefaultN() }",
		"1 -> { m.v[_ex_Root_v].mutableP(); cur = ",
		"1 -> { m.pf.mutableT(); cur = ")
	mustNotContain(t, "sequence options", sb, "m.u.pt =", "m.u.inner =", "M_U_Pt()")
}

// One type per (union, default_id): the $defs union used with default_id 1 and
// with the omitted (lowest) one are two classes, each constructing its own
// default option.
func TestKotlinUnionSplitDefaults(t *testing.T) {
	out := genUnion(t)
	mustContain(t, "Pick_default_t", out[unionDir+"Pick__DefaultT.kt"],
		"public var which: Int = T_ID", "private var _t: Pick_T? = Pick_T()")
	mustContain(t, "Pick_default_n", out[unionDir+"Pick__DefaultN.kt"],
		"public var which: Int = N_ID", "private var _n: UShort = 6u.toUShort()", "private var _t: Pick_T? = null\n")
	mustContain(t, "M", out[unionDir+"M.kt"],
		"public var pf: Pick__DefaultT = Pick__DefaultT()")
}

// The JSON harness renders exactly the held option and selects what it reads.
func TestKotlinUnionJSONHarness(t *testing.T) {
	j := genUnion(t)[unionDir+"_Json.kt"]
	to := memberBody(t, j, "internal fun to(o: M_U, b: kotlin.text.StringBuilder) {")
	mustContain(t, "Json.to(M_U)", to,
		"when (o.which) {",
		"M_U.NUM_ID -> {\n                b.append(\"\\\"num\\\":\")\n                b.append(o.num)",
		"to(o.pt, b)")
	// One member only: no separator between members, as a product type had.
	mustNotContain(t, "Json.to(M_U)", to, "\n        b.append(',')\n")
	from := memberBody(t, j, "internal fun from(j: kotlin.collections.Map<String, _JsonValue>, o: M_U) {")
	mustContain(t, "Json.from(M_U)", from,
		"for ((k, e) in j) {",
		"\"num\" -> {\n                    o.num = e.uint().toUShort()",
		"from(e.obj(), o.mutablePt())",
		"o.arr = UShortArray(_a0.size)",
		"val _u = o.mutableStrs()",
		"o.bl = _Json.toBytes(e.arr())",
		"o.flags = BooleanArray(_a0.size)")
}

func genKotlinErr(t *testing.T, src string) error {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "dyn.yaml")
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
	_, err = (&Backend{}).Generate(s, map[string]any{})
	return err
}

// A keyword option is backtick-escaped, an option named after the tag or a
// generated member takes the trailing underscore, and so does one spelled like
// an id constant. No valid pair of options is refused (ARCHITECTURE §8): a
// property beside a same-named function compiles, and an `is` option beside the
// one its setter would share a JVM name with gets its own setter name.
func TestKotlinUnionNames(t *testing.T) {
	out := genFromYAML(t, `version: 1
messages:
  N:
    payload:
      u: { id: 0, type: union, oneof: { class: { id: 0, type: u8 }, which: { id: 1, type: string, maxlen: 4 }, reset: { id: 2, type: u8 } } }
`, map[string]any{})
	mustContain(t, "N_U", out[unionDir+"N_U.kt"],
		"    public var `class`: UByte\n",
		"public fun hasClass(): Boolean = which == CLASS_ID",
		"    private var _which: String = \"\"\n",
		"    public var which_: String\n        get() = if (which == WHICH_ID) this._which else \"\"",
		"    public var reset_: UByte\n")
	mustContain(t, "N visitor", out[unionDir+"N.kt"], "m.u.`class` = value.toUByte()", "m.u.which_ = s", "m.u.reset_ = value.toUByte()")

	out = genFromYAML(t, `version: 1
messages:
  N:
    payload:
      u: { id: 0, type: union, oneof: { a: { id: 0, type: u8 }, A_ID: { id: 1, type: u8 }, foo: { id: 2, type: u8 }, hasFoo: { id: 3, type: u8 }, open: { id: 4, type: u8 }, isOpen: { id: 5, type: u8 } } }
`, map[string]any{})
	mustContain(t, "N_U", out[unionDir+"N_U.kt"],
		"public const val A_ID: Int = 0",
		"public const val A_ID_ID: Int = 1",
		"    public var A_ID_: UByte\n",
		"public fun hasFoo(): Boolean = which == FOO_ID",
		"    public var hasFoo: UByte\n",
		"    @set:kotlin.jvm.JvmName(\"setIsOpen__\")\n    public var isOpen: UByte\n",
		"    public var open: UByte\n")
	mustContain(t, "N visitor", out[unionDir+"N.kt"], "m.u.A_ID_ = value.toUByte()")
}
