package golang

import (
	goparser "go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	defparser "github.com/sofa-buffers/generator/internal/parser"
)

func exampleSchema(t *testing.T) *ir.Schema {
	t.Helper()
	return schemaFromYAMLFile(t, "../../examples/messages/example.yaml")
}

func schemaFromYAMLString(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := defparser.Parse([]byte(src), "vec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return analyzed(t, doc)
}

func schemaFromYAMLFile(t *testing.T, path string) *ir.Schema {
	t.Helper()
	doc, err := defparser.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return analyzed(t, doc)
}

func analyzed(t *testing.T, doc *defparser.Document) *ir.Schema {
	t.Helper()
	resolved, _ := doc.Resolve()
	if errs := defparser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func genGo(t *testing.T, s *ir.Schema, cfg map[string]any) map[string]string {
	t.Helper()
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

func TestGeneratedGoParses(t *testing.T) {
	files := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	fset := token.NewFileSet()
	for path, src := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if _, err := goparser.ParseFile(fset, path, []byte(src), goparser.AllErrors); err != nil {
			t.Errorf("generated %s is not valid Go: %v", path, err)
		}
	}
}

// TestGoOverIndexWrapperArray: a fixed-count wrapper array (string/blob/struct
// elements) threads its schema count N into the collector as cap, so an element
// id >= N is rejected as INVALID before the slice grows (issue #142 /
// MESSAGE_SPEC §5.1/§7). A dynamic wrapper array (no count) gets cap -1.
func TestGoOverIndexWrapperArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" +
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      ds: { id: 3, type: array, items: { type: string } }\n" +
		"      dp: { id: 4, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }\n"
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	msg := files["m.go"]
	for _, want := range []string{
		"&_strSeq{out: &m.Bs, cap: 4, emax: 16}",   // bounded string -> cap 4, maxlen 16
		"&_bytesSeq{out: &m.Bb, cap: 3, emax: 16}", // bounded blob   -> cap 3, maxlen 16
		"cap: 2}", // bounded struct -> _objSeq cap 2
		"&_strSeq{out: &m.Ds, cap: -1, emax: -1}", // dynamic string -> unbounded, no maxlen
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing %q:\n%s", want, msg)
		}
	}
	// The guards live in the shared prelude.
	prelude := files["sofab_visitor.go"]
	for _, want := range []string{
		"if s.cap >= 0 && int(id) >= s.cap {",
		"return sofab.ErrInvalidMsg",
	} {
		if !strings.Contains(prelude, want) {
			t.Errorf("sofab_visitor.go missing over-index guard %q", want)
		}
	}
}

// TestGoMaxlenReject verifies MESSAGE_SPEC §7.1: a bounded string/blob whose
// wire byte length exceeds its schema maxlen is rejected as INVALID (never
// truncated) — for scalar fields and wrapper-array elements alike. Unbounded
// fields carry no guard.
func TestGoMaxlenReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      b:  { id: 1, type: blob,   maxlen: 8 }\n" +
		"      u:  { id: 2, type: string }\n" +
		"      ws: { id: 3, type: array, items: { type: string, maxlen: 5 } }\n"
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	msg := files["m.go"]
	for _, want := range []string{
		"if len(v) > 8 {",                        // scalar string + blob guard
		"return sofab.ErrInvalidMsg",             // both scalar and wrapper reject with this
		"&_strSeq{out: &m.Ws, cap: -1, emax: 5}", // wrapper element maxlen threaded as emax
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing %q:\n%s", want, msg)
		}
	}
	// The scalar guard must fire for the bounded string (id 0) and blob (id 1) —
	// once in String and once in Bytes — but NOT for the unbounded string (id 2).
	if got := strings.Count(msg, "if len(v) > 8 {"); got != 2 {
		t.Errorf("expected exactly 2 scalar maxlen guards (string+blob), got %d:\n%s", got, msg)
	}
	// The unbounded string carries no maxlen guard -- but it does carry the UTF-8
	// check, which is not a bound: it fires wherever a string is MATERIALIZED
	// (generator#257), bounded or not.
	if !strings.Contains(msg, "case 2:\n\t\tif !sofab.Utf8Valid([]byte(v)) {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.U = v") {
		t.Errorf("m.go: unbounded string (id 2) must store without a maxlen guard:\n%s", msg)
	}
	// The wrapper-element guard lives in the shared prelude.
	prelude := files["sofab_visitor.go"]
	for _, want := range []string{
		"emax int",
		"if s.emax >= 0 && len(v) > s.emax {",
	} {
		if !strings.Contains(prelude, want) {
			t.Errorf("sofab_visitor.go missing wrapper maxlen guard %q", want)
		}
	}
}

// TestGoHeaderVisitorReject verifies the generator#216 / F-0032 fix: a schema
// bound is rejected at the header word (sofab.HeaderVisitor) so INVALID dominates
// a subsequent truncation (MESSAGE_SPEC §5.2). ArrayBegin rejects an over-count
// native array at the count word, FixlenHeader an over-maxlen string/blob at the
// length word — both BEFORE the corelib's truncation check, which the whole-value
// len(v)>N guards run too late to beat. A type with no bound must implement
// neither method, so the decoder's max-speed path (no type assertion hit) is kept.
func TestGoHeaderVisitorReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      ua: { id: 0, type: array, items: { type: u32, count: 4 } }\n" +
		"      fa: { id: 1, type: array, items: { type: fp32, count: 3 } }\n" +
		"      s:  { id: 2, type: string, maxlen: 8 }\n" +
		"      b:  { id: 3, type: blob,   maxlen: 16 }\n" +
		"      da: { id: 4, type: array, items: { type: u32 } }\n" + // dynamic: no bound
		"      us: { id: 5, type: string }\n" + // unbounded string: no bound
		"      wa: { id: 6, type: array, items: { type: string, count: 5 } }\n" // wrapper array: no ArrayBegin arm
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	msg := files["m.go"]
	for _, want := range []string{
		"func (m *M) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {",
		"func (m *M) FixlenHeader(id sofab.ID, subtype int, length int) error {",
		// Each count guard is gated on the wire ArrayKind an array of the DECLARED
		// element type maps to: ArrayBegin fires for any array header at a field id,
		// and a contradicting kind must be skipped, not measured against this
		// field's capacity (§7.3, generator#259).
		"if kind == sofab.ArrayUnsigned && count > 4 {", // native u32 array (id 0)
		"if kind == sofab.ArrayFp32 && count > 3 {",     // fixlen fp32 array (id 1)
		// Each maxlen guard is gated on the DECLARED fixlen subtype (2 = string,
		// 3 = blob): FixlenHeader fires for any subtype at a field id, and a
		// contradicting one must be skipped, not measured against this field's
		// bound (§7.3, generator#224).
		"if subtype == 2 && length > 8 {",  // scalar string (id 2) maxlen
		"if subtype == 3 && length > 16 {", // scalar blob (id 3) maxlen
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing header-visitor guard %q:\n%s", want, msg)
		}
	}
	// Neither bound may ever be enforced on the wire count/length alone — an
	// un-gated compare is exactly the generator#224 (maxlen) and generator#259
	// (count) defect: a fixlen value or array whose subtype contradicts the
	// declaration was rejected as INVALID instead of skipped.
	for _, notWant := range []string{"if length > 8 {", "if length > 16 {", "if count > 4 {", "if count > 3 {"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("m.go: maxlen header guard %q is not gated on the fixlen subtype (generator#224):\n%s", notWant, msg)
		}
	}
	// The dynamic array (id 4) and unbounded string (id 5) declare no bound, so
	// they contribute no ArrayBegin/FixlenHeader arm. The wrapper-sequence array
	// (id 6) descends via BeginSequence and is bounded at the collector cap, not by
	// ArrayBegin — so ArrayBegin holds exactly the two native arrays' arms.
	if got := strings.Count(msg, "return sofab.ErrInvalidMsg"); got == 0 {
		t.Errorf("m.go: expected header-visitor rejects, found none:\n%s", msg)
	}
	// A message with no bounded field must NOT implement HeaderVisitor at all,
	// keeping the corelib's max-speed decode path (the once-per-scope type
	// assertion stays a miss).
	plain := genGo(t, schemaFromYAMLString(t,
		"version: 1\nmessages:\n  P:\n    payload:\n      x: { id: 0, type: u32 }\n      da: { id: 1, type: array, items: { type: u32 } }\n"),
		map[string]any{"package": "p"})["p.go"]
	for _, notWant := range []string{"ArrayBegin(id sofab.ID", "FixlenHeader(id sofab.ID"} {
		if strings.Contains(plain, notWant) {
			t.Errorf("p.go: an unbounded-only type must not implement HeaderVisitor (%q):\n%s", notWant, plain)
		}
	}
	// sofab.HeaderVisitor declares BOTH methods and the cursor reaches the hooks
	// through one `v.(HeaderVisitor)` assertion, so a type carrying only ONE kind of
	// bound must still implement both — emitting just the needed method leaves the
	// assertion failing and silently disables the header rejects entirely.
	for _, tc := range []struct{ name, src string }{
		{"maxlen only", "version: 1\nmessages:\n  Q:\n    payload:\n      s: { id: 0, type: string, maxlen: 8 }\n"},
		{"count only", "version: 1\nmessages:\n  Q:\n    payload:\n      a: { id: 0, type: array, items: { type: u32, count: 4 } }\n"},
	} {
		out := genGo(t, schemaFromYAMLString(t, tc.src), map[string]any{"package": "q"})["q.go"]
		for _, want := range []string{"func (m *Q) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {", "func (m *Q) FixlenHeader(id sofab.ID, subtype int, length int) error {"} {
			if !strings.Contains(out, want) {
				t.Errorf("q.go (%s): a bounded type must implement the whole HeaderVisitor, missing %q:\n%s", tc.name, want, out)
			}
		}
	}
}

// TestGoArrayElemBound covers generator#267's element position: an array element
// outside its DECLARED WIDTH is INVALID (§7.1) and, being established by its own
// bytes, dominates a truncation behind it (§5.2). The `for _, _x := range v` scan
// in the *Array arms decides an array that arrives and never runs for one that
// does not, so the bound also goes to the corelib as sofab.ElemBoundVisitor,
// which applies it while the elements go past.
func TestGoArrayElemBound(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      ua: { id: 0, type: array, items: { type: u8,  count: 4 } }\n" +
		"      sa: { id: 1, type: array, items: { type: i16, count: 4 } }\n" +
		"      wa: { id: 2, type: array, items: { type: u64, count: 4 } }\n" + // no narrower range
		"      da: { id: 3, type: array, items: { type: u32 } }\n" + // dynamic, still narrowed
		"      fa: { id: 4, type: array, items: { type: fp32, count: 4 } }\n" + // no width bound
		"      wr: { id: 5, type: array, items: { type: string, count: 4 } }\n" // wrapper array
	msg := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})["m.go"]
	for _, want := range []string{
		"func (m *M) ArrayElemBound(id sofab.ID, kind sofab.ArrayKind) (int64, int64, bool) {",
		// Gated on the wire kind an array of the DECLARED element type maps to,
		// for the reason ArrayBegin is: the hook is asked per field id, and an
		// array whose kind contradicts the declaration is a §7.3 skip whose
		// elements were never this field's value.
		"if kind == sofab.ArrayUnsigned {\n\t\t\treturn 0, 255, true\n\t\t}",
		"if kind == sofab.ArraySigned {\n\t\t\treturn -32768, 32767, true\n\t\t}",
		// The width is a property of the element TYPE, not of the array length,
		// so a count-less array carries it too.
		"return 0, 4294967295, true",
		"return 0, 0, false",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing element bound %q:\n%s", want, msg)
		}
	}
	// The scan over the assembled slice stays: it is what still bounds the
	// elements against a corelib that does not know the extension, and it costs
	// one pass over a slice already in hand.
	if !strings.Contains(msg, "for _, _x := range v {") {
		t.Errorf("m.go: the assembled-slice width scan must stay:\n%s", msg)
	}
	// u64 (id 2), fp32 (id 4) and the wrapper array (id 5) declare no element
	// width — the first because its range IS the callback parameter's, the others
	// because they are not integer elements at all. Read the ArrayElemBound body
	// alone: those ids DO carry arms in the neighbouring ArrayBegin switch.
	body := msg[strings.Index(msg, "func (m *M) ArrayElemBound("):]
	body = body[:strings.Index(body, "\n}\n")]
	for _, notWant := range []string{"case 2:", "case 4:", "case 5:"} {
		if strings.Contains(body, notWant) {
			t.Errorf("m.go: unexpected element bound %q:\n%s", notWant, body)
		}
	}
	// A type with no narrowed array element must not implement the interface at
	// all, so the corelib's assertion stays a miss.
	plain := genGo(t, schemaFromYAMLString(t,
		"version: 1\nmessages:\n  P:\n    payload:\n      a: { id: 0, type: array, items: { type: u64, count: 4 } }\n"),
		map[string]any{"package": "p"})["p.go"]
	if strings.Contains(plain, "ArrayElemBound(id sofab.ID") {
		t.Errorf("p.go: a type with no narrowed element must not implement ElemBoundVisitor:\n%s", plain)
	}
	// ElemBoundVisitor is its OWN interface, so a schema that declares an element
	// width but no count/maxlen gets it without HeaderVisitor coming along.
	only := genGo(t, schemaFromYAMLString(t,
		"version: 1\nmessages:\n  R:\n    payload:\n      a: { id: 0, type: array, items: { type: u8 } }\n"),
		map[string]any{"package": "r"})["r.go"]
	if !strings.Contains(only, "func (m *R) ArrayElemBound(") {
		t.Errorf("r.go: an element width alone must still be declared:\n%s", only)
	}
	if strings.Contains(only, "func (m *R) ArrayBegin(") {
		t.Errorf("r.go: an element width alone must not drag in HeaderVisitor:\n%s", only)
	}
}

// TestGoFixlenArrayKindPerSubtype pins generator#259 / Crucible F-0042: a fixlen
// array's element SUBTYPE decides whether the header is the declared field's
// value at all, so the schema count bound may only be applied to a header whose
// kind matches what the schema declared at that id. fp32 and fp64 are therefore
// separate kinds (the corelib dropped the collapsed FIXLEN kind and reports
// sofab.ArrayFp32 / sofab.ArrayFp64 after reading the fixlen_word), and each
// declared array's guard names exactly one of them.
//
// The defect this pins: an fp64 array of 8 elements arriving at a declared
// `array<fp32, count 5>` was rejected as INVALID by the un-gated `count > 5`,
// where MESSAGE_SPEC §7.3 requires it to be skipped and the field left alone.
func TestGoFixlenArrayKindPerSubtype(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      fa: { id: 0, type: array, items: { type: fp32, count: 5 } }\n" +
		"      da: { id: 1, type: array, items: { type: fp64, count: 7 } }\n" +
		"      ua: { id: 2, type: array, items: { type: u16,  count: 9 } }\n" +
		"      ia: { id: 3, type: array, items: { type: i16,  count: 9 } }\n" +
		"      ea: { id: 4, type: array, items: { type: enum, count: 2, enum: { A: 0, B: 1 } } }\n" +
		"      ba: { id: 5, type: array, items: { type: boolean, count: 2 } }\n"
	msg := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})["m.go"]
	for _, want := range []string{
		// A declared fp32 array is bounded only under ArrayFp32, a declared fp64
		// array only under ArrayFp64 — never the other way round, and never on a
		// count that arrived under the sibling subtype.
		"case 0:\n\t\tif kind == sofab.ArrayFp32 && count > 5 {",
		"case 1:\n\t\tif kind == sofab.ArrayFp64 && count > 7 {",
		// Integer arrays are keyed the same way, on the single wire kind their
		// element type maps to; enum rides the signed array wire type and
		// boolean the unsigned one.
		"case 2:\n\t\tif kind == sofab.ArrayUnsigned && count > 9 {",
		"case 3:\n\t\tif kind == sofab.ArraySigned && count > 9 {",
		"case 4:\n\t\tif kind == sofab.ArraySigned && count > 2 {",
		"case 5:\n\t\tif kind == sofab.ArrayUnsigned && count > 2 {",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing kind-keyed count bound %q:\n%s", want, msg)
		}
	}
	// The collapsed kind is gone from the corelib ABI: nothing may name it, and no
	// arm may accept both fixlen subtypes at one declared field.
	for _, notWant := range []string{
		"sofab.ArrayFixlen",
		"sofab.ArrayFp32 || kind == sofab.ArrayFp64",
		"sofab.ArrayFp64 || kind == sofab.ArrayFp32",
	} {
		if strings.Contains(msg, notWant) {
			t.Errorf("m.go: a fixlen array must be keyed by its own subtype alone, found %q:\n%s", notWant, msg)
		}
	}
	// Each declared fixlen array names its own kind and only its own: the fp32
	// field must not appear under ArrayFp64, nor the fp64 field under ArrayFp32.
	for _, tc := range []struct {
		kind string
		n    int
	}{{"sofab.ArrayFp32", 1}, {"sofab.ArrayFp64", 1}} {
		if got := strings.Count(msg, tc.kind); got != tc.n {
			t.Errorf("m.go: %s named %d times, want %d:\n%s", tc.kind, got, tc.n, msg)
		}
	}
	// And the hook still carries the kind — a stale signature would satisfy
	// sofab.HeaderVisitor structurally nowhere, but it would compile locally and
	// silently drop every header reject, so pin the exact one the corelib declares.
	if !strings.Contains(msg, "func (m *M) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {") {
		t.Errorf("m.go: ArrayBegin must take the wire ArrayKind:\n%s", msg)
	}
}

func TestGoStructuralInvariants(t *testing.T) {
	files := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	msg := files["myfirstmessage.go"]
	for _, want := range []string{
		"package messages",
		"func (m *Myfirstmessage) Serialize(e *sofab.Encoder)",
		"_visitorBase", // struct embeds the no-op base
		"func (m *Myfirstmessage) Unsigned(id sofab.ID, v uint64) error", // visitor decode
		"func (m *Myfirstmessage) BeginSequence(id sofab.ID) (sofab.Visitor, error)",
		"func NewMyfirstmessage() *Myfirstmessage",
		"func DecodeMyfirstmessage(",
		"sofab.AcceptBytes(data, m)", // zero-copy cursor decode
		"e.WriteSequenceBeginLazy(",  // nested struct/union framing (MESSAGE_SPEC S2)
		"e.WriteSequenceEndKeep()",   // ... and an array ELEMENT keeps its frame
		"`json:\"somei8\"`",          // canonical json tags
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("myfirstmessage.go missing %q", want)
		}
	}
	for _, notWant := range []string{
		"func (m *Myfirstmessage) unmarshal(d *sofab.Decoder)", // pull-parser removed
		"d.Next()",
	} {
		if strings.Contains(msg, notWant) {
			t.Errorf("myfirstmessage.go should no longer contain %q (pull-parser replaced by visitor)", notWant)
		}
	}
	// Every sequence is opened lazily: the eager begin no longer exists in the
	// corelib, and emitting it would not even compile (MESSAGE_SPEC S2).
	if strings.Contains(strings.ReplaceAll(msg, "e.WriteSequenceBeginLazy(", ""), "e.WriteSequenceBegin(") {
		t.Errorf("myfirstmessage.go must open every sequence with WriteSequenceBeginLazy:\n%s", msg)
	}
	// The decode prelude (embedded no-op base + collectors) is emitted once.
	prelude := files["sofab_visitor.go"]
	for _, want := range []string{
		"type _visitorBase struct{}",
		"type _strSeq struct {",
	} {
		if !strings.Contains(prelude, want) {
			t.Errorf("sofab_visitor.go missing %q", want)
		}
	}
	types := files["types.go"]
	if !strings.Contains(types, "type MyfirstmessageSomeenum int8") {
		t.Errorf("enum backing type missing/incorrect:\n%s", firstLines(types, 12))
	}
}

// MESSAGE_SPEC §2: every sequence opens with the lazy begin, so the CLOSER alone
// decides whether a contentless one survives -- and where it is chosen from is the
// whole of the element rule. A sequence-typed FIELD (a struct/union field, an
// array wrapper) is decided by the SCHEMA: it always closes with the dropping
// WriteSequenceEnd, so an all-default one is omitted. A wrapper-array ELEMENT (a
// struct element, a nested row) is decided by its position in the VALUE, at run
// time: the dropping closer in the array's interior, where an all-default element
// vanishes into an id gap, and WriteSequenceEndKeep at the last index, where its
// presence is what fixes the decoded length (§5.1).
func TestGoSequenceCloserIsPositional(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  vec:
    payload:
      nested: { id: 0, type: struct, fields: { x: { id: 0, type: i32 } } }
      strs:   { id: 1, type: array, items: { type: string } }
      structs: { id: 2, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }
      rows:   { id: 3, type: array, items: { type: array, items: { type: string } } }
`)
	got := genGo(t, s, map[string]any{"package": "messages"})["vec.go"]
	for _, want := range []string{
		// struct FIELD: lazy begin, dropping end -- no run-time choice at all
		"\te.WriteSequenceBeginLazy(0)\n\tm.Nested.Serialize(e)\n\te.WriteSequenceEnd()\n",
		// string-array wrapper FIELD (id 1): dropping end
		"\te.WriteSequenceBeginLazy(1)\n",
		// struct-array wrapper FIELD (id 2) holding ELEMENT frames whose closer is
		// picked from the element's index in the value
		"\te.WriteSequenceBeginLazy(2)\n\tfor _i0, _e0 := range m.Structs {\n\t\te.WriteSequenceBeginLazy(sofab.ID(_i0))\n\t\t_e0.Serialize(e)\n\t\tif _i0 == len(m.Structs)-1 {\n\t\t\te.WriteSequenceEndKeep()\n\t\t} else {\n\t\t\te.WriteSequenceEnd()\n\t\t}\n\t}\n\te.WriteSequenceEnd()\n",
		// nested array: the outer wrapper is a FIELD (end), each row an ELEMENT
		// (kept only at the last index)
		"\te.WriteSequenceBeginLazy(3)\n\tfor _i0, _e0 := range m.Rows {\n\t\te.WriteSequenceBeginLazy(sofab.ID(_i0))\n",
		"\t\tif _i0 == len(m.Rows)-1 {\n\t\t\te.WriteSequenceEndKeep()\n\t\t} else {\n\t\t\te.WriteSequenceEnd()\n\t\t}\n\t}\n\te.WriteSequenceEnd()\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("vec.go missing %q:\n%s", want, got)
		}
	}
	// An ELEMENT must never take the keeping closer unconditionally: that framed
	// every all-default interior element, which the sparse rule now omits.
	if strings.Contains(got, "_e0.Serialize(e)\n\t\te.WriteSequenceEndKeep()\n") {
		t.Errorf("an array element must not close unconditionally with the keeping end:\n%s", got)
	}
	// The string-array FIELD must close with the dropping end, never the keeping
	// one -- getting this backwards costs two bytes per all-default array field.
	if !strings.Contains(got, "\t\te.WriteString(sofab.ID(_i0), _e0)\n\t\t}\n\t}\n\te.WriteSequenceEnd()\n") {
		t.Errorf("string-array wrapper FIELD must close with WriteSequenceEnd:\n%s", got)
	}
	// The eager begin no longer exists in corelib-go; emitting it would not compile.
	if strings.Contains(strings.ReplaceAll(got, "WriteSequenceBeginLazy(", ""), "WriteSequenceBegin(") {
		t.Errorf("no sequence may use the eager begin:\n%s", got)
	}
}

// A blob field with no schema default omits via the idiomatic len()==0 test,
// matching the array/string/scalar omit-checks and touching neither bytes.Equal
// nor the bytes import (#113). bytes.Equal(x, nil) is true iff len(x)==0, so the
// emitted check is exactly equivalent.
func TestGoNestedBlobOmitUsesLen(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  outer:
    payload:
      nested:
        id: 0
        type: struct
        fields:
          bytes_field:
            id: 3
            type: blob
`)
	files := genGo(t, s, map[string]any{"package": "messages"})
	types := files["types.go"]
	if !strings.Contains(types, "if len(m.BytesField) != 0 {") {
		t.Errorf("expected nested blob marshal to omit via len() in types.go:\n%s", firstLines(types, 20))
	}
	if strings.Contains(types, "bytes.Equal") {
		t.Errorf("default-less blob should not use bytes.Equal:\n%s", firstLines(types, 20))
	}
	if strings.Contains(types, `"bytes"`) {
		t.Errorf("types.go should not import bytes for a default-less blob:\n%s", firstLines(types, 20))
	}
}

// A blob field with a schema default still compares against that default via
// bytes.Equal, and lands in types.go (not the message file) when nested.
// Regression for #84: any file that references bytes. must import "bytes"
// itself rather than relying on the message file's own import. go/parser only
// parses, so it never caught this — the failure is an undefined identifier at
// compile time. Here we assert every generated file that references bytes. also
// imports it.
func TestGoNestedDefaultedBlobImportsBytes(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  outer:
    payload:
      nested:
        id: 0
        type: struct
        fields:
          bytes_field:
            id: 3
            type: blob
            default: "AAEC"
`)
	files := genGo(t, s, map[string]any{"package": "messages"})
	types := files["types.go"]
	if !strings.Contains(types, "bytes.Equal") {
		t.Fatalf("expected defaulted nested blob marshal to use bytes.Equal in types.go:\n%s", firstLines(types, 20))
	}
	for path, src := range files {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if strings.Contains(src, "bytes.") && !strings.Contains(src, `"bytes"`) {
			t.Errorf("%s references bytes. but does not import \"bytes\":\n%s", path, firstLines(src, 12))
		}
	}
}

// TestGoMetadataDocComments checks that field/enum/bitfield metadata renders as
// idiomatic godoc: a deprecated field carries a leading doc block with a
// "Deprecated:" paragraph (Go's only deprecation marker) while keeping its
// description; enum constants keep their trailing description; bitfield flags
// keep their description and gain a "(default: true/false)" note when defaulted.
func TestGoMetadataDocComments(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode:
      Off:    { value: 0, description: "Node is powered down." }
      Active: { value: 1, description: "Node is sampling and transmitting." }
  bitfield:
    StatusFlags:
      ready:      { pos: 0, default: true, description: "Node has completed initialization." }
      overheated: { pos: 1, description: "Core temperature exceeded the safe threshold." }
messages:
  Telemetry:
    summary: Periodic telemetry sample from a sensor node.
    payload:
      temp:     { id: 0, type: i16, description: "Ambient temperature.", unit: degC, default: 20 }
      legacyId: { id: 1, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 2, type: enum, enum: { $ref: "#/$defs/enum/Mode" }, description: "Current operating mode." }
      status:   { id: 3, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" }, description: "Health flags for this sample." }
`
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "messages"})
	msg, types := files["telemetry.go"], files["types.go"]

	// Deprecated field: leading godoc block, description kept, Deprecated: line,
	// and no trailing description comment on the field line itself.
	for _, want := range []string{
		"// LegacyId Old identifier retained for backward compatibility.",
		"// Deprecated: retained for backward compatibility only; do not use in new code.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("telemetry.go missing deprecated doc %q:\n%s", want, firstLines(msg, 20)) //nolint
		}
	}
	if !regexp.MustCompile(`// Deprecated:[^\n]*\n\tLegacyId\s+uint32`).MatchString(msg) {
		t.Errorf("Deprecated: line must directly precede the LegacyId field:\n%s", firstLines(msg, 20))
	}

	// Enum constant descriptions (trailing, unchanged; gofmt aligns columns).
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`EnumModeOff\s+EnumMode = 0 // Node is powered down\.`),
		regexp.MustCompile(`EnumModeActive\s+EnumMode = 1 // Node is sampling and transmitting\.`),
	} {
		if !want.MatchString(types) {
			t.Errorf("types.go missing enum const doc %v", want)
		}
	}

	// Bitfield flag descriptions + default note.
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`BitfieldStatusFlagsReady\s+BitfieldStatusFlags = 1 << 0 // Node has completed initialization\. \(default: true\)`),
		regexp.MustCompile(`BitfieldStatusFlagsOverheated\s+BitfieldStatusFlags = 1 << 1 // Core temperature exceeded the safe threshold\.`),
	} {
		if !want.MatchString(types) {
			t.Errorf("types.go missing bitfield flag doc %v:\n%s", want, firstLines(types, 20))
		}
	}
	// A defaulted flag with no description would still carry the note.
	if strings.Contains(types, "(default: true) (default: true)") {
		t.Error("default note duplicated")
	}
}

func TestGoDeterministic(t *testing.T) {
	a := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	b := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	if a["myfirstmessage.go"] != b["myfirstmessage.go"] || a["types.go"] != b["types.go"] {
		t.Fatal("Go generation is not deterministic")
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestGoDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated package — constants in the prelude
// plus sofab.WithMax* options on every AcceptBytes call. The cap is raised to
// the largest schema bound of its kind (escape hatch: schema-bounded fields
// stay governed by their own bound), an unset key emits nothing, and a key
// whose kind has no unbounded field is inert.
func TestGoDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
`
	s := schemaFromYAMLString(t, src)
	files := genGo(t, s, map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	})
	prelude, msg := files["sofab_visitor.go"], files["dyn.go"]
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`MaxDynArrayCount\s+= 100000`), // raised to the schema count of barr
		regexp.MustCompile(`MaxDynStringLen\s+= 4096`),
	} {
		if !want.MatchString(prelude) {
			t.Errorf("prelude missing %v", want)
		}
	}
	if strings.Contains(prelude, "MaxDynBlobLen") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	if !strings.Contains(msg, "sofab.AcceptBytes(data, m, sofab.WithMaxArrayCount(MaxDynArrayCount), sofab.WithMaxStringLen(MaxDynStringLen))") {
		t.Error("Decode must pass the active limits into AcceptBytes")
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := genGo(t, s, map[string]any{})
	if strings.Contains(plain["sofab_visitor.go"], "MaxDyn") || strings.Contains(plain["dyn.go"], "WithMax") {
		t.Error("unset limits must emit no limit plumbing")
	}
}

// `count: N` is a CAPACITY, not a length (MESSAGE_SPEC §3): it never reaches the
// wire, the wire count M IS the array's length, and nothing that carries that
// length may be elided. So the whole trim-on-encode / fill-on-decode pair is gone
// -- from both array forms -- and a count:N array is generated exactly like a
// count-less one except for the bound it still enforces.
func TestGoArrayCountIsCapacityNotLength(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fnums:   { id: 3, type: array, items: { type: u32, count: 4 } }
      fdef:    { id: 4, type: array, items: { type: u32, count: 4 }, default: [1, 2] }
`)
	files := genGo(t, s, map[string]any{"package": "messages"})
	got, prelude := files["vec.go"], files["sofab_visitor.go"]

	// Both array kinds loop over the value itself: there is no M to narrow to.
	for _, want := range []string{
		"for _i0, _e0 := range m.Fixed {",
		"for _i0, _e0 := range m.Dynamic {",
		"for _i0, _e0 := range m.Fstrs {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("vec.go missing %q:\n%s", want, got)
		}
	}
	// A count:N native array is emitted whole, and its omit test compares the value
	// as it stands -- not a trailing-trimmed image of it.
	if !strings.Contains(got, "\tif len(m.Fnums) != 0 {\n\t\tsofab.WriteUnsignedArray(e, 3, m.Fnums)\n\t}") {
		t.Errorf("a count:N native array must be written whole:\n%s", got)
	}
	// A `default` shorter than the count stands for itself: it is NOT padded to N,
	// on either side of the comparison.
	if !strings.Contains(got, "if !slices.Equal(m.Fdef, []uint32{1, 2}) {") {
		t.Errorf("a short count:N default must not be padded to N:\n%s", got)
	}
	// isDefault has to reach the same verdict as the writer, or it omits a field
	// that is on the wire (or keeps one that is not). Both are now the plain value.
	for _, want := range []string{
		"len(m.Fixed) == 0",
		"len(m.Dynamic) == 0",
		"len(m.Fstrs) == 0",
		"len(m.Fnums) == 0",
		"slices.Equal(m.Fdef, []uint32{1, 2})",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("isDefault must test the plain value, missing %q:\n%s", want, got)
		}
	}
	// Nothing anywhere may trim on encode or refill on decode.
	for _, banned := range []string{
		"_trimObjs", "_trimStrs", "_trimBlobs", "_trimRows",
		"sofab.TrimTail", "sofab.PadTo", "_fillTo",
	} {
		for name, src := range map[string]string{"vec.go": got, "sofab_visitor.go": prelude} {
			if strings.Contains(src, banned) {
				t.Errorf("%s still uses the superseded fixed-length machinery %q:\n%s", name, banned, src)
			}
		}
	}
	// A fresh count:N array is the EMPTY array, not N element defaults: the
	// constructor materializes a declared default and nothing else.
	if strings.Contains(got, "m.Fixed = make(") || strings.Contains(got, "m.Fstrs = make(") {
		t.Errorf("a count:N wrapper array must not be materialized to N:\n%s", got)
	}
	if !strings.Contains(got, "m.Fdef = []uint32{1, 2}\n") {
		t.Errorf("a declared array default must still be materialized:\n%s", got)
	}
	if strings.Contains(got, "m.Fnums = []uint32{0, 0, 0, 0}") {
		t.Errorf("a count:N native array with no default must stay empty:\n%s", got)
	}
	// The bound itself is untouched -- that is all `count` still does.
	if !strings.Contains(got, "cap: 5") || !strings.Contains(got, "if len(v) > 4 {") {
		t.Errorf("the count bound must still be enforced:\n%s", got)
	}
}

// MESSAGE_SPEC §2 governs both element kinds with ONE rule, and the rule is
// positional: an element before the last one that equals its element default is
// omitted -- a string/blob leaf simply not written, a sequence element not framed
// either -- while the element at the LAST index is always written, as its value or
// as an empty frame. Only the last index carries the array's length (§5.1); an
// interior gap is restored from the element default and is therefore free.
//
// This holds with or without a declared `count`: a capacity can never restore an
// elided tail.
func TestGoElementSparsityIsPositional(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  vec:
    payload:
      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fixedobj: { id: 3, type: array, items: { type: struct, count: 3, fields: { k: { id: 0, type: u32 } } } }
      mat:      { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
`)
	got := genGo(t, s, map[string]any{"package": "messages"})["vec.go"]

	for _, want := range []string{
		// leaf elements: the last index escapes the omit test -- count or no count
		"\t\tif _e0 != \"\" || _i0 == len(m.Dynstr)-1 {",
		"\t\tif len(_e0) != 0 || _i0 == len(m.Dynblob)-1 {",
		"\t\tif _e0 != \"\" || _i0 == len(m.Fixedstr)-1 {",
		// sequence elements: the same rule, applied through the closer
		"\t\tif _i0 == len(m.Fixedobj)-1 {\n\t\t\te.WriteSequenceEndKeep()\n\t\t} else {\n\t\t\te.WriteSequenceEnd()\n\t\t}",
		// a native row carries no frame of its own, so the rule lands on the write
		"\t\tif len(_e0) != 0 || _i0 == len(m.Mat)-1 {\n\t\t\tsofab.WriteUnsignedArray(e, sofab.ID(_i0), _e0)\n\t\t}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("vec.go missing %q:\n%s", want, got)
		}
	}
	// The predicate follows the writer: every non-empty array now puts at least one
	// element on the wire, so "default" is exactly "empty" for every element kind.
	for _, want := range []string{"len(m.Dynstr) == 0", "len(m.Fixedstr) == 0", "len(m.Fixedobj) == 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("isDefault must be plain emptiness, missing %q:\n%s", want, got)
		}
	}
}

// A wrapper array's element id IS the array index (§5.1), so an element is PLACED
// at dest[id] after gap-filling from the element default -- never appended. That
// is what restores an interior element the sparse rule omitted; appending would
// shorten the array by the size of every gap and would decode a REOPENED id as a
// second element instead of merging into the first (§7.4).
//
// The decoded length is highest present id + 1, exact because the last element is
// never elided. Nothing is filled in beyond it: a schema `count` is a capacity, so
// it bounds the id but never adds an element the wire did not carry.
func TestGoWrapperElementsArePlacedByID(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      mat:  { id: 1, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      rows: { id: 2, type: array, items: { type: array, count: 2, items: { type: string, maxlen: 4 } } }
`)
	files := genGo(t, s, map[string]any{"package": "messages"})
	prelude := files["sofab_visitor.go"]

	for _, want := range []string{
		// struct elements: place, not append -- and the gap-fill that precedes it
		"\tfor len(*s.out) <= int(id) {\n\t\t*s.out = append(*s.out, zero)\n\t}\n\treturn PT(&(*s.out)[id]), nil",
		// native matrix rows: same, through the shared _placeRow
		"func _placeRow[T any](out *[][]T, cap int, id sofab.ID, row []T) error {",
		"\tfor len(*out) <= int(id) {\n\t\t*out = append(*out, nil)\n\t}\n\t(*out)[id] = row",
		"func (s *_uMatSeq[T]) UnsignedArray(id sofab.ID, v []uint64) error {\n\treturn _placeRow(s.out, s.cap, id, sofab.NarrowUnsigned[T](v))",
		// wrapper rows: same again
		"func (s *_seqSeq[T]) BeginSequence(id sofab.ID) (sofab.Visitor, error) {",
		"\tfor len(*s.out) <= int(id) {\n\t\t*s.out = append(*s.out, nil)\n\t}\n\treturn s.mk(&(*s.out)[id]), nil",
		// the over-index guard bounds every id-keyed fill
		"if s.cap >= 0 && int(id) >= s.cap {",
		"\tif cap >= 0 && int(id) >= cap {\n\t\treturn sofab.ErrInvalidMsg\n\t}",
	} {
		if !strings.Contains(prelude, want) {
			t.Errorf("sofab_visitor.go missing %q:\n%s", want, prelude)
		}
	}
	// The defects this replaced: appending ignored the id entirely.
	for _, banned := range []string{
		"*s.out = append(*s.out, zero)\n\treturn PT(&(*s.out)[len(*s.out)-1]), nil",
		"*s.out = append(*s.out, sofab.NarrowUnsigned[T](v))",
		"return s.mk(&(*s.out)[len(*s.out)-1]), nil",
		// ...and the refill that made the superseded trailing elision lossless.
		"EndSequence() error { return _fillTo",
		"func (s *_objSeq[T, PT]) EndSequence() error {",
	} {
		if strings.Contains(prelude, banned) {
			t.Errorf("sofab_visitor.go must no longer contain %q:\n%s", banned, prelude)
		}
	}
	// Every collector carries the outer array's count bound, matrices included.
	for _, want := range []string{"cap: 4", "&_uMatSeq[uint32]{out: &m.Mat, cap: 2}", "&_seqSeq[string]{out: &m.Rows, cap: 2,"} {
		if !strings.Contains(files["vec.go"], want) {
			t.Errorf("vec.go must pass the schema count as the collector cap, missing %q:\n%s", want, files["vec.go"])
		}
	}
}

// TestGoSkippedStringIsNotValidated: UTF-8 validation belongs where a `string`
// is MATERIALIZED — read into a declared destination — never on a payload the
// decoder is skipping (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038).
//
// corelib-go used to validate inside the cursor, which cannot tell a field this
// visitor binds from one it skips, so an unknown id carrying invalid UTF-8 failed
// the whole decode. The corelib dropped that check and exports `sofab.Utf8Valid`
// instead; the generated destination arms are now what validate. A skipped field
// reaches no arm, so it is never inspected — which is the whole point.
func TestGoSkippedStringIsNotValidated(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      u:  { id: 1, type: string }\n" +
		"      n:  { id: 2, type: struct, fields: { t: { id: 0, type: string } } }\n" +
		"      sa: { id: 3, type: array, items: { type: string, count: 4 } }\n"
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	msg := files["m.go"]

	// Every materializing destination validates: the bounded scalar, the
	// unbounded scalar (both on the message type) and the nested struct's field
	// (its own type, emitted alongside).
	all := ""
	for _, f := range files {
		all += f
	}
	if got := strings.Count(all, "if !sofab.Utf8Valid([]byte(v)) {"); got != 4 {
		t.Errorf("want a UTF-8 check at each string destination + the collector, got %d:\n%s", got, all)
	}
	// It sits behind the maxlen guard, which decides on the wire length alone.
	if !strings.Contains(msg, "if len(v) > 8 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tif !sofab.Utf8Valid([]byte(v)) {") {
		t.Errorf("the maxlen reject must stay ahead of the UTF-8 check:\n%s", msg)
	}
	// The wrapper-array element path validates in the shared collector.
	prelude := files["sofab_visitor.go"]
	if !strings.Contains(prelude, "if !sofab.Utf8Valid([]byte(v)) {") {
		t.Errorf("_strSeq must validate the element it materializes:\n%s", prelude)
	}
	// A blob carries no encoding, so its arms must not grow a check.
	if strings.Contains(msg, "Bytes(id sofab.ID, v []byte) error") &&
		strings.Contains(msg, "Utf8Valid(v)") {
		t.Errorf("blob must never be UTF-8-validated:\n%s", msg)
	}
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound. An out-of-range value
// is INVALID — never masked by the uint8(v) conversion, never kept. u64/i64 span
// the visitor parameter's own range and get no guard.
func TestGoDeclaredWidthIsAValidityBound(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      a_u8:   { id: 0, type: u8 }
      c_u32:  { id: 2, type: u32 }
      d_u64:  { id: 3, type: u64 }
      e_i8:   { id: 4, type: i8 }
      g_i32:  { id: 6, type: i32 }
      h_i64:  { id: 7, type: i64 }
      arr_u8: { id: 8, type: array, items: { type: u8, count: 4 } }
`
	got := genGo(t, schemaFromYAMLString(t, src), map[string]any{})["w.go"]
	for _, want := range []string{
		"if v > 255 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.AU8 = uint8(v)",
		"if v > 4294967295 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.CU32 = uint32(v)",
		"if v < -128 || v > 127 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.EI8 = int8(v)",
		"if v < -2147483648 || v > 2147483647 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.GI32 = int32(v)",
		// The array's ELEMENTS carry the same bound: the corelib hands the whole
		// array over as []uint64, so one scan precedes the narrowing conversion.
		"for _, _x := range v {\n\t\t\tif _x > 255 {\n\t\t\t\treturn sofab.ErrInvalidMsg\n\t\t\t}\n\t\t}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("w.go missing width guard %q:\n%s", want, got)
		}
	}
	// u64/i64 store bare.
	for _, want := range []string{"m.DU64 = uint64(v)", "m.HI64 = int64(v)"} {
		if !strings.Contains(got, want) {
			t.Errorf("w.go missing unguarded 64-bit store %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "if v > 18446744073709551615") || strings.Contains(got, "if v > 9223372036854775807") {
		t.Errorf("a 64-bit destination must not be width-guarded:\n%s", got)
	}
}

// CORELIB_PLAN §5.6 asks generated code to process a message in small chunks,
// not only whole-buffer-at-once. `AcceptBytes` cannot: it takes a `[]byte` and
// therefore requires the whole wire image resident by construction, and
// `Decoder.Accept` only moves that requirement inside the corelib (it slurps the
// reader before dispatching). `AcceptStream` is the reader-driven entry point
// that actually bounds memory by the largest single field (corelib-go#71/#72,
// generator#312).
//
// The assertion is on the emitted CALL, not on the presence of a `From` helper:
// a streaming-shaped signature over `Decoder.Accept` would satisfy "takes an
// io.Reader" while still slurping, which is exactly the state this replaces.
func TestGoStreamingDecodeUsesAcceptStream(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  vec:
    payload:
      a: { id: 0, type: u64 }
      s: { id: 1, type: string }
`)
	msg := genGo(t, s, map[string]any{})["vec.go"]

	if !strings.Contains(msg, "func DecodeVecFrom(r io.Reader) (*Vec, error)") {
		t.Error("missing the io.Reader-driven decode entry point")
	}
	if !strings.Contains(msg, "sofab.NewDecoder(r).AcceptStream(m)") {
		t.Error("streaming decode must go through AcceptStream")
	}
	// The slurping entry points must not be what the reader path is built on.
	if strings.Contains(msg, "NewDecoder(r).Accept(m)") {
		t.Error("Decoder.Accept slurps the reader — it does not bound memory")
	}
	// ...and the in-memory path is unchanged: this is an addition.
	if !strings.Contains(msg, "sofab.AcceptBytes(data, m)") {
		t.Error("the []byte path must stay AcceptBytes")
	}

	// Receiver-side decode limits (generator#102) bind BOTH entry points. They
	// reach AcceptBytes as trailing arguments and NewDecoder as its options, so
	// the same renderer serves both only because the two signatures happen to be
	// variadic in the same position — worth pinning, since a limit enforced on
	// one path and not the other is a silent asymmetry.
	lim := genGo(t, s, map[string]any{"max_dyn_string_len": 4096})["vec.go"]
	if !strings.Contains(lim, "sofab.NewDecoder(r, sofab.WithMaxStringLen(MaxDynStringLen)).AcceptStream(m)") {
		t.Error("streaming decode must carry the active decode limits")
	}
}
