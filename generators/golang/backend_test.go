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
// MESSAGE_SPEC §5.1/§7). A dynamic wrapper array (no count) gets cap -1 and is
// bounded by the RECEIVER cap beside it instead — a wrapper array's element ids
// never reach the generated visitor, so the collector is where that shape's
// receiver bounds land (CORELIB_PLAN §6.2.1, corelib-go#132).
//
// Both receiver fields are emitted on EVERY collector, including where the
// schema bound beside them makes them inert: corelib-go falls back to the format
// ceiling for a missing one, which bounds nothing the format does not already
// reject, so omitting one is no receiver bound at all rather than a default.
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
		// bounded string -> Cap 4, maxlen 16; the caps beside them are inert.
		"&sofab.StringSeq{Out: &m.Bs, Cap: 4, ElemMax: 16, RCap: MaxDynArrayCount, RElemMax: MaxDynStringLen}",
		// bounded blob -> Cap 3, maxlen 16. No unbounded blob in the schema, so
		// MaxDynBlobLen is not exported and the configured number goes in as a
		// literal rather than the package growing a constant nothing reads.
		"&sofab.BlobSeq{Out: &m.Bb, Cap: 3, ElemMax: 16, RCap: MaxDynArrayCount, RElemMax: 4194304}",
		// bounded struct -> sofab.MessageSeq Cap 2, index cap inert beside it
		"Cap: 2, RCap: MaxDynArrayCount}",
		// dynamic string -> unbounded, no maxlen: here BOTH receiver caps govern,
		// and they are the only bound this shape has.
		"&sofab.StringSeq{Out: &m.Ds, Cap: -1, ElemMax: -1, RCap: MaxDynArrayCount, RElemMax: MaxDynStringLen}",
		"&sofab.MessageSeq[MDpElem, *MDpElem]{Out: &m.Dp, Cap: -1, RCap: MaxDynArrayCount}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing %q:\n%s", want, msg)
		}
	}
	// The guard itself is the corelib collector's, so the generator emits none of
	// it -- only the bound that parameterizes it, asserted above.
	if strings.Contains(files["sofab_visitor.go"], "int(id) >=") {
		t.Errorf("the over-index guard must be corelib-go's, not emitted:\n%s", files["sofab_visitor.go"])
	}
}

// TestGoMaxlenReject verifies MESSAGE_SPEC §7.1: a bounded string/blob whose
// wire byte length exceeds its schema maxlen is rejected as INVALID (never
// truncated) — for scalar fields and wrapper-array elements alike. Unbounded
// fields carry no guard.
//
// The bound has exactly ONE site now: FixlenBegin, at the length word. The
// whole-value guard that used to sit beside it in the payload arm went with the
// callback that carried a whole value (corelib-go#130): a string arrives in
// pieces, so a guard on the assembled result is the one that cannot fire for a
// field the message truncates — which is the §5.2 tie the header hook exists to
// win.
func TestGoMaxlenReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      b:  { id: 1, type: blob,   maxlen: 8 }\n" +
		"      u:  { id: 2, type: string }\n" +
		"      ws: { id: 3, type: array, items: { type: string, maxlen: 5 } }\n"
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	msg := files["m.go"]
	for _, want := range []string{
		"func (m *M) FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, total int) error {",
		// Both bounds sit behind the DECLARED subtype: FixlenBegin fires for any
		// fixlen subtype at a field id, and a value contradicting the declaration
		// is a §7.3 skip, never this field's length (generator#224).
		"case 0:\n\t\tif sub != sofab.FixlenStr {\n\t\t\treturn nil\n\t\t}\n\t\tif total > 8 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}",
		"case 1:\n\t\tif sub != sofab.FixlenBlob {\n\t\t\treturn nil\n\t\t}\n\t\tif total > 8 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}",
		// wrapper element maxlen threaded as ElemMax; RElemMax beside it is inert
		// (§6.2.1 keeps a cap off a field the schema bounds), RCap is not — the
		// array itself declares no count.
		"&sofab.StringSeq{Out: &m.Ws, Cap: -1, ElemMax: 5, RCap: MaxDynArrayCount, RElemMax: MaxDynStringLen}",
		// The unbounded string (id 2) is bounded at the very same word, by the
		// receiver cap and in the other category (§6.2.1: a policy rejection of
		// well-formed bytes, never folded into INVALID).
		"case 2:\n\t\tif sub != sofab.FixlenStr {\n\t\t\treturn nil\n\t\t}\n\t\tif total > MaxDynStringLen {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing %q:\n%s", want, msg)
		}
	}
	// Exactly two length-word guards: the bounded string (id 0) and blob (id 1),
	// and nothing for the unbounded string (id 2).
	if got := strings.Count(msg, "if total > 8 {"); got != 2 {
		t.Errorf("expected exactly 2 length-word maxlen guards (string+blob), got %d:\n%s", got, msg)
	}
	// The payload arms measure NOTHING. A bound on the assembled value would be a
	// second enforcement point that disagrees with the first on exactly the
	// messages this design is about — the truncated ones.
	if strings.Contains(msg, "len(_b) >") {
		t.Errorf("the assembled payload must carry no bound of its own:\n%s", msg)
	}
	// The unbounded string carries no maxlen guard in the PAYLOAD arm (its cap
	// lives at the length word, asserted above) -- but it does carry the UTF-8
	// check, which is not a bound: it fires wherever a string is MATERIALIZED
	// (generator#257), bounded or not. m.UTF8Valid, not the package-level
	// primitive, so WithStrictUTF8 reaches the destination (§6.4).
	if !strings.Contains(msg, "case 2:\n\t\t_b, _done := m._acc.Take(total, offset, chunk)\n\t\tif !_done {\n\t\t\treturn nil\n\t\t}\n\t\tif !m.UTF8Valid(_b) {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.U = string(_b)") {
		t.Errorf("m.go: unbounded string (id 2) must store without a maxlen guard:\n%s", msg)
	}
	// The wrapper-element guard is sofab.StringSeq's own; the generator hands it
	// the bound and emits no comparison of its own.
	if strings.Contains(files["sofab_visitor.go"], "len(v) >") {
		t.Errorf("the wrapper maxlen guard must be corelib-go's, not emitted:\n%s", files["sofab_visitor.go"])
	}
}

// TestGoHeaderBoundsAtTheWord verifies the generator#216 / F-0032 fix: a schema
// bound is rejected at the header word so INVALID dominates a subsequent
// truncation (MESSAGE_SPEC §5.2). ArrayBegin rejects an over-count native array
// at the count word, FixlenBegin an over-maxlen string/blob at the length word —
// both before a byte of the value is read.
//
// Both are ORDINARY Visitor methods since corelib-go#130. The optional
// HeaderVisitor they used to reach the decoder through is gone, and with it the
// trap it carried: the cursor found both hooks through one interface assertion,
// so a type emitting only the method it needed left that assertion failing and
// silently disabled the header rejects altogether. Each method now stands alone
// and a type with no bound of that kind simply keeps sofab.VisitorBase's no-op.
func TestGoHeaderBoundsAtTheWord(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      ua: { id: 0, type: array, items: { type: u32, count: 4 } }\n" +
		"      fa: { id: 1, type: array, items: { type: fp32, count: 3 } }\n" +
		"      s:  { id: 2, type: string, maxlen: 8 }\n" +
		"      b:  { id: 3, type: blob,   maxlen: 16 }\n" +
		"      da: { id: 4, type: array, items: { type: u32 } }\n" + // dynamic: no count bound
		"      us: { id: 5, type: string }\n" + // unbounded string: no length bound
		"      wa: { id: 6, type: array, items: { type: string, count: 5 } }\n" // wrapper array: no ArrayBegin arm
	files := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})
	msg := files["m.go"]
	for _, want := range []string{
		"func (m *M) ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error {",
		"func (m *M) FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, total int) error {",
		// Each arm opens on the wire kind an array of the DECLARED element type
		// maps to, and leaves on a mismatch: ArrayBegin fires for any array header
		// at a field id, and a contradicting kind was never this field's value —
		// so neither its count nor its elements may touch the field (§7.3,
		// generator#259).
		"case 0:\n\t\tif kind != sofab.ArrayUnsigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 4 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}", // native u32 array
		"case 1:\n\t\tif kind != sofab.ArrayFp32 {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 3 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}",     // fixlen fp32 array
		// Same shape for the two length bounds, on the declared fixlen subtype.
		"case 2:\n\t\tif sub != sofab.FixlenStr {\n\t\t\treturn nil\n\t\t}\n\t\tif total > 8 {",
		"case 3:\n\t\tif sub != sofab.FixlenBlob {\n\t\t\treturn nil\n\t\t}\n\t\tif total > 16 {",
		// The destination is opened at the header, sized from the count the line
		// above has just bounded -- and by make, never by reslicing, so an array
		// that arrives EMPTY decodes as the empty array and not as a nil slice.
		"m.Ua = make([]uint32, 0, count)",
		// An unbounded array is bounded in the very same arm, one line up, by the
		// receiver cap that exists to bound exactly this allocation -- and in the
		// other category, the cap being a policy rejection of well-formed bytes
		// (§6.2.1). The corelib holds no cap of its own to fall back on any more
		// (corelib-go#133), so this arm is the whole bound on this shape.
		"case 4:\n\t\tif kind != sofab.ArrayUnsigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > MaxDynArrayCount {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}\n\t\tm.Da = make([]uint32, 0, count)",
		// ...and the unbounded string likewise, at its length word.
		"case 5:\n\t\tif sub != sofab.FixlenStr {\n\t\t\treturn nil\n\t\t}\n\t\tif total > MaxDynStringLen {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing header guard %q:\n%s", want, msg)
		}
	}
	// Neither bound may ever be enforced on the wire count/length alone — an
	// un-gated compare is exactly the generator#224 (maxlen) and generator#259
	// (count) defect: a fixlen value or array whose subtype contradicts the
	// declaration was rejected as INVALID instead of skipped.
	for _, notWant := range []string{"case 2:\n\t\tif total >", "case 0:\n\t\tif count >"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("m.go: a header bound %q is not gated on the declared kind/subtype:\n%s", notWant, msg)
		}
	}
	// A CAP never reaches a field the schema already bounds (§6.2.1): the four
	// bounded fields answer INVALID and nothing else, so no MaxDyn* constant may
	// appear in their arms.
	for _, notWant := range []string{
		"if count > 4 {\n\t\t\treturn sofab.ErrLimitExceeded",
		"if total > 8 {\n\t\t\treturn sofab.ErrLimitExceeded",
		"if count > MaxDynArrayCount {\n\t\t\treturn sofab.ErrInvalidMsg",
	} {
		if strings.Contains(msg, notWant) {
			t.Errorf("m.go: the two categories must not be folded (%q):\n%s", notWant, msg)
		}
	}
	// Exactly two arms carry the array cap: the dynamic native array (id 4). The
	// wrapper-sequence array (id 6) descends via BeginSequence -- no count header
	// exists for it at all -- so it is bounded on its collector instead and never
	// names a header switch.
	if got := strings.Count(switchBody(t, msg, "func (m *M) ArrayBegin("), "MaxDynArrayCount"); got != 1 {
		t.Errorf("expected exactly 1 array cap in ArrayBegin, got %d:\n%s", got, msg)
	}
	if strings.Contains(switchBody(t, msg, "func (m *M) FixlenBegin("), "case 6:") ||
		strings.Contains(switchBody(t, msg, "func (m *M) ArrayBegin("), "case 6:") {
		t.Errorf("m.go: the wrapper array must not carry a header bound:\n%s", msg)
	}
	// A type with no bound of a kind does not override that method at all: the
	// embedded sofab.VisitorBase no-op stands, and the decode pays no call. With
	// the caps enforced per field, "no bound" now means neither a schema bound nor
	// a live cap -- a scalar-only payload.
	plain := genGo(t, schemaFromYAMLString(t,
		"version: 1\nmessages:\n  P:\n    payload:\n      x: { id: 0, type: u32 }\n      y: { id: 1, type: i32 }\n"),
		map[string]any{"package": "p"})["p.go"]
	for _, notWant := range []string{"ArrayBegin(id sofab.ID", "FixlenBegin(id sofab.ID"} {
		if strings.Contains(plain, notWant) {
			t.Errorf("p.go: a type with no header bound must not override the hook (%q):\n%s", notWant, plain)
		}
	}
	// An unbounded string alone is enough to make FixlenBegin exist -- that is
	// where its cap goes -- and still drags no ArrayBegin along.
	capped := genGo(t, schemaFromYAMLString(t,
		"version: 1\nmessages:\n  R:\n    payload:\n      x: { id: 0, type: u32 }\n      s: { id: 1, type: string }\n"),
		map[string]any{"package": "r"})["r.go"]
	if !strings.Contains(capped, "if total > MaxDynStringLen {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}") {
		t.Errorf("r.go: an unbounded string must carry its receiver cap at the length word:\n%s", capped)
	}
	if strings.Contains(capped, "ArrayBegin(id sofab.ID") {
		t.Errorf("r.go: a string cap must not drag ArrayBegin along:\n%s", capped)
	}
	// ...and one kind of bound no longer drags the other's method along, which is
	// what the retired HeaderVisitor assertion forced.
	for _, tc := range []struct{ name, src, want, notWant string }{
		{"maxlen only", "version: 1\nmessages:\n  Q:\n    payload:\n      s: { id: 0, type: string, maxlen: 8 }\n",
			"func (m *Q) FixlenBegin(", "func (m *Q) ArrayBegin("},
		{"count only", "version: 1\nmessages:\n  Q:\n    payload:\n      a: { id: 0, type: array, items: { type: u32, count: 4 } }\n",
			"func (m *Q) ArrayBegin(", "func (m *Q) FixlenBegin("},
	} {
		out := genGo(t, schemaFromYAMLString(t, tc.src), map[string]any{"package": "q"})["q.go"]
		if !strings.Contains(out, tc.want) {
			t.Errorf("q.go (%s): missing %q:\n%s", tc.name, tc.want, out)
		}
		if strings.Contains(out, tc.notWant) {
			t.Errorf("q.go (%s): must not emit %q:\n%s", tc.name, tc.notWant, out)
		}
	}
}

// switchBody returns the body of the method whose declaration starts with decl,
// so a "this id carries no arm" assertion reads that switch alone: the same id
// legitimately appears in the neighbouring callbacks.
func switchBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		return ""
	}
	body := src[i:]
	if j := strings.Index(body, "\n}\n"); j >= 0 {
		body = body[:j]
	}
	return body
}

// TestGoArrayElementWidth covers generator#267's element position: an array
// element outside its DECLARED WIDTH is INVALID (§7.1) and, being established by
// its own bytes, dominates a truncation behind it (§5.2).
//
// corelib-go#130 is what makes that reachable in generated code. The elements
// used to arrive as one assembled slice, so the guard was a scan that decided an
// array which ARRIVES and never ran for one that does not — the bound therefore
// had to be handed to the decoder through sofab.ElemBoundVisitor to be applied
// in time. They arrive one at a time now, so the check sits in the element arm,
// where it fires as the element lands, and the extension is gone.
func TestGoArrayElementWidth(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      ua: { id: 0, type: array, items: { type: u8,  count: 4 } }\n" +
		"      sa: { id: 1, type: array, items: { type: i16, count: 4 } }\n" +
		"      wa: { id: 2, type: array, items: { type: u64, count: 4 } }\n" + // no narrower range
		"      da: { id: 3, type: array, items: { type: u32 } }\n" + // dynamic, still narrowed
		"      fa: { id: 4, type: array, items: { type: fp32, count: 4 } }\n" + // no width bound
		"      wr: { id: 5, type: array, items: { type: string, count: 4 } }\n" // wrapper array
	msg := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})["m.go"]
	for _, want := range []string{
		"func (m *M) ArrayUnsigned(id sofab.ID, _ int, v uint64) error {",
		"func (m *M) ArraySigned(id sofab.ID, _ int, v int64) error {",
		// The guard precedes the conversion, which is the whole point: the
		// `uint8(v)` below it IS the mask §7.1 forbids, so a value outside the
		// declared width has to be refused before it.
		"case 0:\n\t\tif v > 255 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.Ua = append(m.Ua, uint8(v))",
		"case 1:\n\t\tif v < -32768 || v > 32767 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.Sa = append(m.Sa, int16(v))",
		// The width is a property of the element TYPE, not of the array length,
		// so a count-less array carries it too.
		"case 3:\n\t\tif v > 4294967295 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.Da = append(m.Da, uint32(v))",
		// u64 spans the callback parameter's own range: nothing to bound, and the
		// element is stored as delivered.
		"case 2:\n\t\tm.Wa = append(m.Wa, v)",
		// fp32 has no integer width at all.
		"case 4:\n\t\tm.Fa = append(m.Fa, v)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing element width guard %q:\n%s", want, msg)
		}
	}
	// The scan over an assembled slice is gone with the slice.
	if strings.Contains(msg, "for _, _x := range v {") {
		t.Errorf("m.go: the assembled-slice width scan must not survive:\n%s", msg)
	}
	// ...and so is the extension that carried the bound to the decoder.
	for _, notWant := range []string{"ArrayElemBound", "sofab.NarrowUnsigned", "sofab.NarrowSigned"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("m.go: %q was retired with the whole-slice callback:\n%s", notWant, msg)
		}
	}
	// The wrapper array (id 5) has no element callback at all: it descends via
	// BeginSequence into a collector that owns its elements.
	if strings.Contains(switchBody(t, msg, "func (m *M) ArrayUnsigned("), "case 5:") {
		t.Errorf("m.go: a wrapper array must not carry an element arm:\n%s", msg)
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
		"case 0:\n\t\tif kind != sofab.ArrayFp32 {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 5 {",
		"case 1:\n\t\tif kind != sofab.ArrayFp64 {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 7 {",
		// Integer arrays are keyed the same way, on the single wire kind their
		// element type maps to; enum rides the signed array wire type and
		// boolean the unsigned one.
		"case 2:\n\t\tif kind != sofab.ArrayUnsigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 9 {",
		"case 3:\n\t\tif kind != sofab.ArraySigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 9 {",
		"case 4:\n\t\tif kind != sofab.ArraySigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 2 {",
		"case 5:\n\t\tif kind != sofab.ArrayUnsigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 2 {",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing kind-keyed count bound %q:\n%s", want, msg)
		}
	}
	// The collapsed kind is gone from the corelib ABI: nothing may name it, and no
	// arm may accept both fixlen subtypes at one declared field.
	for _, notWant := range []string{
		"sofab.ArrayFixlen",
		"sofab.ArrayFp32 && kind != sofab.ArrayFp64",
		"sofab.ArrayFp64 && kind != sofab.ArrayFp32",
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
	// And the hook still carries the kind — a stale signature compiles locally
	// and simply never overrides sofab.VisitorBase's no-op, silently dropping
	// every header reject, so pin the exact one the corelib declares.
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
		"sofab.VisitorBase", // struct embeds the corelib no-op base
		"func (m *Myfirstmessage) Unsigned(id sofab.ID, v uint64) error", // visitor decode
		"func (m *Myfirstmessage) BeginSequence(id sofab.ID) (sofab.Visitor, error)",
		"func NewMyfirstmessage() *Myfirstmessage",
		"func DecodeMyfirstmessage(",
		"sofab.AcceptBytes(data, m", // zero-copy cursor decode (limit options may follow)
		"e.WriteSequenceBeginLazy(", // nested struct/union framing (MESSAGE_SPEC S2)
		"e.WriteSequenceEndKeep()",  // ... and an array ELEMENT keeps its frame
		"`json:\"somei8\"`",         // canonical json tags
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
	// The no-op base and the collectors are corelib-go's (generator#345), so the
	// prelude holds only what names a GENERATED symbol.
	prelude := files["sofab_visitor.go"]
	if !strings.Contains(prelude, "type _isDefaulter interface{ isDefault() bool }") {
		t.Errorf("sofab_visitor.go missing the isDefault contract:\n%s", prelude)
	}
	for _, banned := range []string{"_visitorBase", "_strSeq", "_bytesSeq", "_objSeq", "_seqSeq", "MatSeq", "_placeRow"} {
		if strings.Contains(prelude, banned) {
			t.Errorf("sofab_visitor.go must no longer emit %q:\n%s", banned, prelude)
		}
	}
	if !strings.Contains(msg, "sofab.VisitorBase") {
		t.Errorf("the object must embed the corelib no-op base:\n%s", firstLines(msg, 20))
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
// limits (generator#102) into the generated package as constants, and generated
// code enforces them ITSELF, per field, at that field's own count/length header.
//
// Nothing is passed into the corelib any more: CORELIB_PLAN §6.2.1 puts the
// numbers with the layer that knows the schema, and corelib-go#133 removed the
// sofab.WithMax* options entirely. Two properties follow, and both are asserted
// here:
//
//   - the caps travel AS CONFIGURED. They used to be raised to the largest schema
//     bound of their kind, because a decoder-level option binds every field alike
//     and would otherwise reject a schema-bounded sibling §6.2.1 forbids it to
//     touch. The raise bought that at the price of loosening the cap for the
//     UNBOUNDED fields -- the ones it exists to protect.
//   - a cap never reaches a schema-bounded field, so the two can now disagree
//     freely: max_dyn_array_count 65536 coexists with a sibling's count: 100000.
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
		regexp.MustCompile(`MaxDynArrayCount\s+= 65536`), // AS CONFIGURED, below barr's count: 100000
		regexp.MustCompile(`MaxDynStringLen\s+= 4096`),
	} {
		if !want.MatchString(prelude) {
			t.Errorf("prelude missing %v", want)
		}
	}
	if strings.Contains(prelude, "MaxDynBlobLen") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	// The corelib is handed no cap at all -- neither entry point takes one.
	for _, notWant := range []string{"sofab.WithMax", "AcceptBytes(data, m,", "NewDecoder(m,"} {
		if strings.Contains(msg, notWant) {
			t.Errorf("the corelib must be handed no receiver cap (%q):\n%s", notWant, msg)
		}
	}
	if !strings.Contains(msg, "sofab.AcceptBytes(data, m)") {
		t.Errorf("Decode must drive the corelib with the visitor alone:\n%s", msg)
	}
	// Enforced per field, in the header hooks, in the right category each time.
	for _, want := range []string{
		// the unbounded string: policy, at the length word
		"case 0:\n\t\tif sub != sofab.FixlenStr {\n\t\t\treturn nil\n\t\t}\n\t\tif total > MaxDynStringLen {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}",
		// the unbounded array: policy, at the count word
		"case 1:\n\t\tif kind != sofab.ArrayUnsigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > MaxDynArrayCount {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}",
		// the schema-bounded array: its OWN bound, INVALID, and no cap beside it --
		// a wire count of 70000 decodes here and is refused at id 1, which the
		// raised decoder-level cap made impossible to express.
		"case 2:\n\t\tif kind != sofab.ArraySigned {\n\t\t\treturn nil\n\t\t}\n\t\tif count > 100000 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("dyn.go missing per-field bound %q:\n%s", want, msg)
		}
	}

	// No keys configured -> the target's finite DEFAULTS, not "unlimited"
	// (§9.5, generator#385). The blob cap stays inert either way: liveness is a
	// property of the schema, not of the configuration.
	plain := genGo(t, s, map[string]any{})
	if !regexp.MustCompile(`MaxDynArrayCount\s+= 65536`).MatchString(plain["sofab_visitor.go"]) {
		t.Error("default array cap must be emitted, unraised")
	}
	if !regexp.MustCompile(`MaxDynStringLen\s+= 1048576`).MatchString(plain["sofab_visitor.go"]) {
		t.Error("default string cap must be emitted")
	}
	if strings.Contains(plain["sofab_visitor.go"], "MaxDynBlobLen") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
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
	// The bound itself is untouched -- that is all `count` still does. It is taken
	// at the count word now (ArrayBegin), which is where §5.2 wants it and the
	// only place left: the elements arrive one at a time, so there is no
	// assembled slice to measure (corelib-go#130).
	if !strings.Contains(got, "Cap: 5") || !strings.Contains(got, "if count > 4 {") {
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

	// The placement itself is corelib-go's (sofab.MessageSeq / sofab.PlaceRow /
	// sofab.NestedSeq): identical for every schema, so the generator only names
	// the collector and hands it the bounds that differ.
	// Every collector also carries the RECEIVER caps beside the schema bounds it
	// is exclusive with (§6.2.1) -- for a matrix, on both axes: RCap for the row
	// id and RowCount/RowCap for the row's OWN element count, which nothing
	// bounded before (the inner `count:` was dropped on the floor here, and the
	// codec cap that stood in for it is gone -- corelib-go#132/#133).
	for _, want := range []string{
		"&sofab.MessageSeq[VecObjsElem, *VecObjsElem]{Out: &m.Objs, Cap: 4, RCap: MaxDynArrayCount}",
		"&sofab.UnsignedMatrixSeq[uint32]{Out: &m.Mat, Cap: 2, RCap: MaxDynArrayCount, RowCount: 3, RowCap: MaxDynArrayCount, Hi: 4294967295}",
		"&sofab.NestedSeq[string]{Out: &m.Rows, Cap: 2, RCap: MaxDynArrayCount,",
		"&sofab.StringSeq{Out: p, Cap: -1, ElemMax: 4, RCap: MaxDynArrayCount, RElemMax: 1048576}",
	} {
		if !strings.Contains(files["vec.go"], want) {
			t.Errorf("vec.go must bind the corelib collector, missing %q:\n%s", want, files["vec.go"])
		}
	}
	// And emits none of the machinery itself -- neither the collectors nor the
	// append-instead-of-place defect they replaced.
	all := ""
	for _, f := range files {
		all += f
	}
	for _, banned := range []string{
		"_objSeq", "_seqSeq", "_placeRow", "_uMatSeq", "_visitorBase{}, nil",
		"append(*s.out",
	} {
		if strings.Contains(all, banned) {
			t.Errorf("generated output must no longer contain %q:\n%s", banned, all)
		}
	}
}

// TestGoSkippedStringIsNotValidated: UTF-8 validation belongs where a `string`
// is MATERIALIZED — read into a declared destination — never on a payload the
// decoder is skipping (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038).
//
// corelib-go used to validate inside the cursor, which cannot tell a field this
// visitor binds from one it skips, so an unknown id carrying invalid UTF-8 failed
// the whole decode. The corelib dropped that check and exports `sofab.UTF8Valid`
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
	// m.UTF8Valid rather than the package-level sofab.UTF8Valid: the object embeds
	// sofab.StringCheck, so the decode's own WithStrictUTF8 policy reaches the
	// destination and not only the build tag (§6.4).
	if got := strings.Count(all, "if !m.UTF8Valid(_b) {"); got != 3 {
		t.Errorf("want a UTF-8 check at each string destination, got %d:\n%s", got, all)
	}
	if strings.Contains(all, "sofab.UTF8Valid(") {
		t.Errorf("a destination inside a decode must read the decode's policy:\n%s", all)
	}
	// It runs once the payload is whole -- on the assembled bytes, never on a
	// piece: a chunk boundary must not turn a valid string invalid (§6.4).
	if !strings.Contains(msg, "_b, _done := m._acc.Take(total, offset, chunk)\n\t\tif !_done {\n\t\t\treturn nil\n\t\t}\n\t\tif !m.UTF8Valid(_b) {") {
		t.Errorf("the UTF-8 check must run on the assembled payload:\n%s", msg)
	}
	// The maxlen reject is one callback earlier, at the length word, so it wins
	// the §5.2 tie against a truncation the UTF-8 check would never see.
	if !strings.Contains(msg, "case 0:\n\t\tif sub != sofab.FixlenStr {\n\t\t\treturn nil\n\t\t}\n\t\tif total > 8 {") {
		t.Errorf("the maxlen reject must stay at the length word:\n%s", msg)
	}
	// The wrapper-array element is materialized by sofab.StringSeq, which
	// validates it there (and, embedding sofab.StringCheck, sees the decode's own
	// WithStrictUTF8 policy rather than only the build tag). Binding the collector
	// is all the generator has to emit for it.
	if !strings.Contains(msg, "&sofab.StringSeq{Out: &m.Sa, Cap: 4, ElemMax: -1, RCap: 65536, RElemMax: MaxDynStringLen}") {
		t.Errorf("the string array must bind the validating collector:\n%s", msg)
	}
	// A blob carries no encoding, so its arms must not grow a check.
	if strings.Contains(switchBody(t, msg, "func (m *M) Bytes("), "UTF8Valid") {
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
		// The array's ELEMENTS carry the same bound, at the element: they arrive
		// one at a time (corelib-go#130), so the guard precedes the narrowing
		// conversion of each and an over-width element is INVALID where it lands
		// rather than after the array completes.
		"if v > 255 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.ArrU8 = append(m.ArrU8, uint8(v))",
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
// therefore requires the whole wire image resident by construction. `FeedFrom`
// drains the reader into a scratch buffer and feeds the decoder chunk by chunk,
// so peak memory is that buffer plus the largest single field (generator#312,
// corelib-go#130).
//
// The assertion is on the emitted CALL, not on the presence of a `From` helper:
// a streaming-shaped signature over a slurping call would satisfy "takes an
// io.Reader" while holding the whole message, which is what this replaces.
func TestGoStreamingDecodeFeedsInChunks(t *testing.T) {
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
	if !strings.Contains(msg, "sofab.NewDecoder(m).FeedFrom(r, scratch)") {
		t.Error("streaming decode must feed the decoder in chunks")
	}
	// The scratch buffer is the CALLER's by contract (§6.6: the corelib sizes no
	// buffer from a stream), so generated code is what allocates it.
	if !strings.Contains(msg, "scratch := make([]byte, 4096)") {
		t.Error("the chunk buffer must be the generated layer's")
	}
	// A reader that stops inside a field is INCOMPLETE, and the outcome — not the
	// error — is what says so: FeedFrom returns a nil error there, so a caller
	// that only checked err would accept a truncated message as whole.
	if !strings.Contains(msg, "if out != sofab.Complete {\n\t\treturn nil, sofab.ErrIncomplete\n\t}") {
		t.Error("an INCOMPLETE stream must not be returned as a decoded message")
	}
	// The retired pull surface must not be what the reader path is built on.
	for _, gone := range []string{"AcceptStream", "NewDecoder(r"} {
		if strings.Contains(msg, gone) {
			t.Errorf("%q is gone with the pull API (corelib-go#130):\n%s", gone, msg)
		}
	}
	// ...and the in-memory path is unchanged: this is an addition.
	if !strings.Contains(msg, "sofab.AcceptBytes(data, m)") {
		t.Error("the []byte path must stay AcceptBytes")
	}

	// The schema's `s` is a schema-unbounded string, so a receiver cap is live —
	// and it reaches BOTH entry points by reaching NEITHER. The cap is enforced by
	// the visitor, which is the one argument the two calls share, so a limit
	// applied on one path and not the other is no longer expressible: the old
	// asymmetry risk was that the caps rode in as per-call options.
	lim := genGo(t, s, map[string]any{"max_dyn_string_len": 4096})["vec.go"]
	if strings.Contains(lim, "sofab.WithMax") {
		t.Errorf("no receiver cap may reach the corelib (corelib-go#133):\n%s", lim)
	}
	if !strings.Contains(lim, "if total > MaxDynStringLen {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}") {
		t.Errorf("the cap must be enforced by the visitor both paths drive:\n%s", lim)
	}
	if !strings.Contains(lim, "sofab.NewDecoder(m).FeedFrom(r, scratch)") ||
		!strings.Contains(lim, "sofab.AcceptBytes(data, m)") {
		t.Errorf("both entry points must drive that same visitor:\n%s", lim)
	}
}

// The encode entry points must hand the corelib storage the CALLER owns
// (CORELIB_PLAN §5.1): a corelib allocates nothing and grows nothing, so the
// buffer is generated code's to size and to allocate. The two arms differ
// because the schema does:
//
//   - bounded — every field carries a count/maxlen, so the worst case is known
//     and one exactly-sized buffer always holds the message: NewEncoderBuffer,
//     no sink, no minimum.
//   - unbounded — no worst case exists, and the configured max_message_size
//     ceiling is a policy number, not a size the message cannot exceed. Sizing
//     the buffer from it would refuse a larger message the caller legitimately
//     built, so a fixed scratch drains into caller-owned storage instead.
func TestGoEncodeUsesCallerOwnedBuffers(t *testing.T) {
	bounded := genGo(t, schemaFromYAMLString(t, `
version: 1
messages:
  b:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: string, maxlen: 8 }
`), map[string]any{})["b.go"]
	for _, want := range []string{
		"const BMaxSize = ",
		"buf := make([]byte, BMaxSize)",
		"e, err := sofab.NewEncoderBuffer(buf, 0)",
		"return e.Bytes(), nil",
	} {
		if !strings.Contains(bounded, want) {
			t.Errorf("bounded encode missing %q:\n%s", want, bounded)
		}
	}
	// A derived worst case is emitted as MAX_SIZE alone — a MAX_SIZE_LIMIT here
	// would tell the reader the number is imposed when it is not.
	if strings.Contains(bounded, "BMaxSizeLimit") {
		t.Error("a schema-derived worst case must not be spelled as a configured ceiling")
	}

	unbounded := genGo(t, schemaFromYAMLString(t, `
version: 1
messages:
  u:
    payload:
      s: { id: 0, type: string }
`), map[string]any{})["u.go"]
	for _, want := range []string{
		"UMaxSizeLimit = 4096",
		"UMaxSize      = UMaxSizeLimit",
		"var scratch [512]byte",
		"e, err := sofab.NewEncoderSink(scratch[:], 0, func(_ *sofab.Encoder, b []byte) error {",
		"out = append(out, b...)",
	} {
		if !strings.Contains(unbounded, want) {
			t.Errorf("unbounded encode missing %q:\n%s", want, unbounded)
		}
	}
	// The ceiling must never size the buffer: that is the truncation this arm
	// exists to avoid.
	if strings.Contains(unbounded, "make([]byte, UMaxSize)") {
		t.Error("the configured ceiling must not size an encode buffer")
	}
	// An explicitly configured ceiling a bounded schema cannot fit is a config
	// error, surfaced by Generate rather than emitted as a too-small buffer.
	if _, err := (&Backend{}).Generate(schemaFromYAMLString(t, `
version: 1
messages:
  big:
    payload:
      a: { id: 0, type: array, items: { type: u64, count: 1000 } }
`), map[string]any{"max_message_size": 16}); err == nil {
		t.Error("a worst case above an explicit max_message_size must be reported")
	}

	// EncodeTo is the same caller-owned shape for both arms: io.Writer.Write may
	// not retain what it is handed, so w is a copying sink and returns without
	// taking the buffer.
	for label, src := range map[string]string{"bounded": bounded, "unbounded": unbounded} {
		if !strings.Contains(src, "_, werr := w.Write(b)") {
			t.Errorf("%s EncodeTo must drain the caller's scratch into w:\n%s", label, src)
		}
		// The compatibility constructor allocates a window of its own and grows
		// it — a negative assertion, so it cannot creep back in either arm.
		if strings.Contains(src, "sofab.NewEncoder(") {
			t.Errorf("%s still uses the corelib-allocating NewEncoder", label)
		}
		if strings.Contains(src, "bytes.Buffer") {
			t.Errorf("%s still buffers the whole message a second time", label)
		}
	}
}

// TestGoNestedRowElemWidth is generator#330: a NESTED native row
// (array<array<u8>>) got no element-width guard at all, so an over-width element
// was silently masked by sofab.NarrowUnsigned — a conversion, not a check, as its
// own corelib doc says. MESSAGE_SPEC §7.1 makes that INVALID, never a truncation.
//
// Unlike #267 this is an ABSENT bound rather than a late one, so it shows on a
// COMPLETE message: no truncation is needed to reach it, which is why the
// differential corpus never caught it.
func TestGoNestedRowElemWidth(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      urows: { id: 1, type: array, items: { type: array, count: 2, items: { type: u8,  count: 3 } } }\n" +
		"      srows: { id: 2, type: array, items: { type: array, count: 2, items: { type: i16, count: 3 } } }\n" +
		"      wide:  { id: 3, type: array, items: { type: array, count: 2, items: { type: u64, count: 3 } } }\n"
	msg := genGo(t, schemaFromYAMLString(t, src), map[string]any{"package": "m"})["m.go"]
	// The bound travels with the collector, so the scan can run before Narrow* masks.
	for _, want := range []string{
		"&sofab.UnsignedMatrixSeq[uint8]{Out: &m.Urows, Cap: 2, RCap: 65536, RowCount: 3, RowCap: 65536, Hi: 255}",
		"&sofab.SignedMatrixSeq[int16]{Out: &m.Srows, Cap: 2, RCap: 65536, RowCount: 3, RowCap: 65536, Lo: -32768, Hi: 32767}",
		// u64 spans the callback parameter's own range: nothing can fall outside,
		// so the zero bound switches the scan off rather than emitting a dead one.
		"&sofab.UnsignedMatrixSeq[uint64]{Out: &m.Wide, Cap: 2, RCap: 65536, RowCount: 3, RowCap: 65536, Hi: 0}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}
