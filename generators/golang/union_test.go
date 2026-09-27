package golang

import (
	"regexp"
	"strings"
	"testing"
)

// unionSrc has a union whose default option is a struct at a non-zero default
// and NOT its first option (so the tag is stored XOR its id), one of every option
// kind the encode rule treats differently, an array of unions whose default
// option needs seeding, an array of unions whose default option does not, and a
// $defs union used with two default_ids (one type per default_id).
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
      z: { id: 1, type: union, default_id: 0, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8, default: 3 } } }
      v: { id: 2, type: array, items: { type: union, count: 3, default_id: 1, oneof: { a: { id: 0, type: u8 }, p: { id: 1, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } } } } }
      w: { id: 3, type: array, items: { type: union, count: 3, default_id: 0, oneof: { i: { id: 0, type: i32 }, s: { id: 1, type: string, maxlen: 8 } } } }
      pf: { id: 4, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      po: { id: 5, type: union, oneof: { $ref: "#/$defs/union/Pick" } }
`

func genUnion(t *testing.T) string {
	t.Helper()
	return genGo(t, schemaFromYAMLString(t, unionSrc), map[string]any{"package": "m"})["types.go"]
}

// funcBody returns the text of the method `func (m *T) Name(` (or the value
// receiver form) up to its closing brace at column 0.
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("missing %q in:\n%s", sig, src)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("unterminated %q", sig)
	}
	return src[i : i+j+2]
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

// A union is one tag plus one unexported typed slot per option -- no exported
// option field a caller could set beside another, no interface (one allocation
// per set), no pointer (one allocation per switch).
func TestGoUnionStorage(t *testing.T) {
	types := genUnion(t)
	// gofmt aligns the member types; compare with the alignment folded away.
	decl := regexp.MustCompile(`[ \t]+`).ReplaceAllString(funcBody(t, types, "type MU struct {"), " ")
	mustContain(t, "MU", decl,
		" sofab.VisitorBase\n", " sofab.StringCheck\n", " which sofab.ID\n",
		" optNum uint16\n", " optS string\n", " optPt MUPt\n", " optArr []uint16\n",
		" optStrs []string\n", " optBl []byte\n", " optInner MUInner\n", " optF float32\n",
		" _acc sofab.PayloadAcc\n")
	mustNotContain(t, "MU", decl, " Num ", " Pt ", "*MUPt", "any", "interface")
	// No string/blob option: no UTF-8 policy and no accumulator.
	mustNotContain(t, "MZ", funcBody(t, types, "type MZ struct {"), "StringCheck", "_acc")

	mustContain(t, "id constants", types,
		"MUNumID   sofab.ID = 0\n", "MUPtID    sofab.ID = 2\n", "MUFID     sofab.ID = 7\n")
}

// The tag is stored relative to default_id, so the zero value holds it; for
// default_id 0 the XOR is not emitted at all.
func TestGoUnionTagIsRelativeToDefault(t *testing.T) {
	types := genUnion(t)
	mustContain(t, "default_id 2", types,
		"func (m *MU) Which() sofab.ID { return m.which ^ MUPtID }",
		"func (m *MU) HasPt() bool { return m.which == 0 }",
		"func (m *MU) HasNum() bool { return m.which == MUNumID^MUPtID }",
		"\tm.which = MUNumID ^ MUPtID\n\tm.optNum = v\n")
	mustContain(t, "default_id 0", types,
		"func (m *MZ) Which() sofab.ID { return m.which }",
		"func (m *MZ) HasA() bool { return m.which == 0 }",
		"func (m *MZ) HasB() bool { return m.which == MZBID }")
	mustNotContain(t, "default_id 0", types, "^ MZAID", "^MZAID")
}

// setDefaults exists only where the default option's own default is not Go's
// zero value, and it seeds that option alone. New<Msg> calls it, and an array of
// unions hands it to NewMessageSeqInit -- per TYPE, so each split $defs variant
// fills its gaps with its own default option.
func TestGoUnionDefaults(t *testing.T) {
	files := genGo(t, schemaFromYAMLString(t, unionSrc), map[string]any{"package": "m"})
	types, msg := files["types.go"], files["m.go"]

	// u: default pt (struct, x = 7): seeded through the struct.
	mustContain(t, "MU.setDefaults", funcBody(t, types, "func (m *MU) setDefaults() {"), "\tm.optPt.setDefaults()\n")
	// num's default 5 is NOT seeded: num is not the default option.
	mustNotContain(t, "MU.setDefaults", funcBody(t, types, "func (m *MU) setDefaults() {"), "optNum")
	// z: default a has no default, b's 3 does not matter -> no method at all.
	mustNotContain(t, "MZ", types, "func (m *MZ) setDefaults()")
	mustContain(t, "MZ.Clear", funcBody(t, types, "func (m *MZ) Clear() {"), "\t*m = MZ{}\n")
	mustNotContain(t, "MZ.Clear", funcBody(t, types, "func (m *MZ) Clear() {"), "setDefaults")
	mustContain(t, "MU.Clear", funcBody(t, types, "func (m *MU) Clear() {"), "\t*m = MU{}\n\tm.setDefaults()\n")

	// The split $defs variants: one type per default_id, each seeding its own.
	mustContain(t, "Pick_default_t", funcBody(t, types, "func (m *UnionPickDefaultT) setDefaults() {"), "\tm.optT.setDefaults()\n")
	mustContain(t, "Pick_default_n", funcBody(t, types, "func (m *UnionPickDefaultN) setDefaults() {"), "\tm.optN = 6\n")
	mustContain(t, "split tags", types,
		"func (m *UnionPickDefaultT) Which() sofab.ID { return m.which ^ UnionPickDefaultTTID }",
		"func (m *UnionPickDefaultN) Which() sofab.ID { return m.which }")

	newM := funcBody(t, msg, "func NewM() *M {")
	mustContain(t, "NewM", newM, "m.U.setDefaults()", "m.Pf.setDefaults()", "m.Po.setDefaults()")
	mustNotContain(t, "NewM", newM, "m.Z.setDefaults()")

	// Element gap fill: v's default option p has q = 9, w's i is zero.
	mustContain(t, "element fill", msg,
		"sofab.NewMessageSeqInit[MVElem, *MVElem](&m.V, sofab.Bounds{Count: 3}, _caps, (*MVElem).setDefaults)",
		"sofab.NewMessageSeq[MWElem, *MWElem](&m.W, sofab.Bounds{Count: 3}, _caps)")
}

// Encode, MESSAGE_SPEC §4.2: the default option keeps its ordinary guarded write
// (and a struct default its dropping closer); every other option is written
// unguarded, and a sequence-framed one closes with WriteSequenceEndKeep, so a
// held option at its own default is a present value or a present empty frame.
func TestGoUnionEncodeArms(t *testing.T) {
	types := genUnion(t)
	ser := funcBody(t, types, "func (m *MU) Serialize(e *sofab.Encoder) {")
	mustContain(t, "MU.Serialize", ser,
		"\tswitch m.Which() {\n",
		"\tcase MUPtID:\n\t\te.WriteSequenceBeginLazy(2)\n\t\tm.optPt.Serialize(e)\n\t\te.WriteSequenceEnd()\n",
		"\tcase MUNumID:\n\t\te.WriteUnsigned(0, uint64(m.optNum))\n",
		"\tcase MUSID:\n\t\te.WriteString(1, m.optS)\n",
		"\tcase MUArrID:\n\t\tsofab.WriteUnsignedArray(e, 3, m.optArr)\n",
		"\tcase MUBlID:\n\t\te.WriteBytes(5, m.optBl)\n",
		"\tcase MUInnerID:\n\t\te.WriteSequenceBeginLazy(6)\n\t\tm.optInner.Serialize(e)\n\t\te.WriteSequenceEndKeep()\n",
		"\tcase MUFID:\n\t\te.WriteFloat32(7, m.optF)\n",
	)
	strs := ser[strings.Index(ser, "case MUStrsID:"):strings.Index(ser, "case MUBlID:")]
	mustContain(t, "wrapper option", strs, "e.WriteSequenceBeginLazy(4)", "\t\te.WriteSequenceEndKeep()\n")
	mustNotContain(t, "wrapper option", strs, "e.WriteSequenceEnd()\n")
	// No guard on any non-default option: the product-type shape was one ≠-default
	// test per option.
	mustNotContain(t, "MU.Serialize", ser, "m.optNum != 5", "m.optS != \"\"", "len(m.optArr) != 0", "len(m.optBl) != 0", "m.optF != 1.5")

	// z's default a is a leaf: guarded exactly like a field.
	mustContain(t, "MZ.Serialize", funcBody(t, types, "func (m *MZ) Serialize(e *sofab.Encoder) {"),
		"\tcase MZAID:\n\t\tif m.optA != 0 {\n\t\t\te.WriteUnsigned(0, uint64(m.optA))\n\t\t}\n",
		"\tcase MZBID:\n\t\te.WriteUnsigned(1, uint64(m.optB))\n")

	// isDefault agrees with Serialize: default option held AND at its default.
	mustContain(t, "isDefault", types,
		"func (m *MU) isDefault() bool {\n\treturn m.which == 0 && m.optPt.isDefault()\n}",
		"func (m *MZ) isDefault() bool {\n\treturn m.which == 0 && m.optA == 0\n}")
}

// Decode, MESSAGE_SPEC §7.4.1: the switch sits behind every §7.3 gate and is
// "select if not held" -- nothing re-selects, resets or re-binds an option that is
// already held, so a repeated or resumed hook cannot wipe what was decoded.
func TestGoUnionDecodeSwitch(t *testing.T) {
	types := genUnion(t)
	// FixlenBegin also fires for a subtype the option does not declare: it bounds,
	// it never selects.
	mustNotContain(t, "FixlenBegin", funcBody(t, types, "func (m *MU) FixlenBegin("), "which", "Set")
	// A string/blob selects at completion, through the setter.
	mustContain(t, "String", funcBody(t, types, "func (m *MU) String("),
		"_b, _done := m._acc.Take(total, offset, chunk)\n\t\tif !_done {\n\t\t\treturn nil\n\t\t}\n\t\tif !m.UTF8Valid(_b) {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.SetS(string(_b))")
	mustContain(t, "Bytes", funcBody(t, types, "func (m *MU) Bytes("), "\t\tm.SetBl(append([]byte(nil), _b...))")
	// A scalar selects after its width bound.
	mustContain(t, "Unsigned", funcBody(t, types, "func (m *MU) Unsigned("),
		"case 0:\n\t\tif v > 65535 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.SetNum(uint16(v))")
	// A compact array selects after the kind gate and the count bound.
	mustContain(t, "ArrayBegin", funcBody(t, types, "func (m *MU) ArrayBegin("),
		"case 3:\n\t\tif kind != sofab.ArrayUnsigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 4 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.which = MUArrID ^ MUPtID\n\t\tm.optArr = make([]uint16, 0, count)")
	// Struct/union options descend through Mut<Opt>(); a wrapper option selects and
	// then takes the §7.4 replace it always had.
	seq := funcBody(t, types, "func (m *MU) BeginSequence(")
	mustContain(t, "BeginSequence", seq,
		"case 2:\n\t\treturn m.MutPt(), nil",
		"case 6:\n\t\treturn m.MutInner(), nil",
		"case 4:\n\t\tm.which = MUStrsID ^ MUPtID\n\t\tm.optStrs = m.optStrs[:0]\n\t\treturn sofab.NewStringSeq(&m.optStrs,")
	// Mut<Opt>() resets only when another option is held.
	mustContain(t, "MutPt", funcBody(t, types, "func (m *MU) MutPt() *MUPt {"),
		"\tif m.which != 0 {\n\t\tm.which = 0\n\t\tm.optPt = MUPt{}\n\t\tm.optPt.setDefaults()\n\t}\n\treturn &m.optPt\n")
	mustContain(t, "MutInner", funcBody(t, types, "func (m *MU) MutInner() *MUInner {"),
		"\tif m.which != MUInnerID^MUPtID {\n\t\tm.which = MUInnerID ^ MUPtID\n\t\tm.optInner = MUInner{}\n\t\tm.optInner.setDefaults()\n\t}\n\treturn &m.optInner\n")
}

// The read side: a getter of an option that is not held returns that option's
// default and stores nothing; Mut<Opt> only for the kinds edited in place.
func TestGoUnionAccessors(t *testing.T) {
	types := genUnion(t)
	mustContain(t, "getters", types,
		"func (m *MU) Num() uint16 {\n\tif m.which != MUNumID^MUPtID {\n\t\treturn 5\n\t}\n\treturn m.optNum\n}",
		"func (m *MU) F() float32 {\n\tif m.which != MUFID^MUPtID {\n\t\treturn 1.5\n\t}\n\treturn m.optF\n}",
		"func (m *MU) Arr() []uint16 {\n\tif m.which != MUArrID^MUPtID {\n\t\treturn nil\n\t}\n\treturn m.optArr\n}",
		"func (m *MU) Inner() MUInner {\n\tif m.which != MUInnerID^MUPtID {\n\t\tvar d MUInner\n\t\td.setDefaults()\n\t\treturn d\n\t}\n\treturn m.optInner\n}",
		"func (m *MU) SetPt(v MUPt) {\n\tm.which = 0\n\tm.optPt = v\n}",
		"func (m *MU) MutArr() *[]uint16 {", "func (m *MU) MutStrs() *[]string {")
	mustNotContain(t, "Mut on a leaf", types, "MutNum", "MutS(", "MutBl", "MutF(")
}

// JSON: exactly one member, the held option -- a value-receiver MarshalJSON, so a
// union inside a struct value or a slice renders the same way, and an
// UnmarshalJSON that starts from the union's default.
func TestGoUnionJSON(t *testing.T) {
	types := genUnion(t)
	mj := funcBody(t, types, "func (m MU) MarshalJSON() ([]byte, error) {")
	mustContain(t, "MarshalJSON", mj, "\tswitch m.Which() {\n", "\tcase MUPtID:\n\t\treturn json.Marshal(struct {\n\t\t\tV MUPt `json:\"pt\"`\n\t\t}{m.optPt})\n")
	if n := strings.Count(mj, "json.Marshal("); n != 8 {
		t.Errorf("MarshalJSON: want one arm per option (8), got %d", n)
	}
	uj := funcBody(t, types, "func (m *MU) UnmarshalJSON(b []byte) error {")
	mustContain(t, "UnmarshalJSON", uj,
		"\tif len(one) != 1 {\n", "\tm.Clear()\n",
		"\t\tcase \"pt\":\n\t\t\treturn json.Unmarshal(v, m.MutPt())\n",
		"\t\tcase \"num\":\n\t\t\tvar x uint16\n\t\t\tif err := json.Unmarshal(v, &x); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t\tm.SetNum(x)\n")
}

// No generic union helper (CLAUDE.md, "Generated code stays thin"): every
// function the union code adds is a method of its union type, made of that
// union's own option arms -- no package-level select/reset/JSON helper shared by
// the unions of a schema.
func TestGoUnionNoStaticHelper(t *testing.T) {
	types := genUnion(t)
	re := regexp.MustCompile(`(?m)^func (\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)`)
	for _, m := range re.FindAllStringSubmatch(types, -1) {
		if m[1] == "" {
			t.Errorf("types.go emits a package-level function %q; union code must be methods of the union", m[2])
		}
	}
}

func genErr(t *testing.T, src string) error {
	t.Helper()
	_, err := (&Backend{}).Generate(schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	return err
}

// Option accessors that clash -- with each other or with a method the union type
// already has -- fail generation naming both; a getter landing on a fixed method
// takes the trailing underscore instead.
func TestGoUnionNames(t *testing.T) {
	src := func(opts string) string {
		return "version: 1\nmessages:\n  M:\n    payload:\n      u: { id: 0, type: union, default_id: 0, oneof: { " + opts + " } }\n"
	}
	err := genErr(t, src(`foo: { id: 0, type: u8 }, set_foo: { id: 1, type: u8 }`))
	if err == nil || !strings.Contains(err.Error(), `options "foo" and "set_foo" both generate the method SetFoo`) {
		t.Errorf("foo/set_foo: want a located clash error, got %v", err)
	}
	err = genErr(t, src(`s: { id: 0, type: string }, string_check: { id: 1, type: u8 }`))
	if err == nil || !strings.Contains(err.Error(), `option "string_check" generates the method SetStringCheck`) {
		t.Errorf("string_check: want a located clash with the promoted SetStringCheck, got %v", err)
	}
	types := genGo(t, schemaFromYAMLString(t, src(`string: { id: 0, type: string }, which: { id: 1, type: u8 }, clear: { id: 2, type: u8 }`)), map[string]any{"package": "m"})["types.go"]
	mustContain(t, "mangled getters", types,
		"func (m *MU) String_() string {", "func (m *MU) SetString(v string) {", "func (m *MU) HasString() bool {",
		"func (m *MU) Which_() uint8 {", "func (m *MU) SetWhich(v uint8) {",
		"func (m *MU) Clear_() uint8 {", "func (m *MU) Clear() {")

	// Package scope: an option-id constant against another package-level name.
	err = genErr(t, "version: 1\nmessages:\n  U:\n    payload:\n      shape: { id: 0, type: union, default_id: 0, oneof: { num: { id: 0, type: u8 } } }\n"+
		"  UShapeNumID:\n    payload:\n      x: { id: 0, type: u8 }\n")
	if err == nil || !strings.Contains(err.Error(), `option "num"'s id constant UShapeNumID collides with the message type UShapeNumID`) {
		t.Errorf("package-scope clash: want a located error, got %v", err)
	}
}
