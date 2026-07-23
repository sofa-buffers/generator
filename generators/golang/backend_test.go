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
	if !strings.Contains(msg, "case 2:\n\t\tm.U = v") {
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
		"func (m *M) ArrayBegin(id sofab.ID, count int) error {",
		"func (m *M) FixlenHeader(id sofab.ID, subtype int, length int) error {",
		"if count > 4 {", // native u32 array (id 0) count bound
		"if count > 3 {", // fixlen fp32 array (id 1) count bound
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
	// The bound must never be enforced on length alone — an un-gated compare is
	// exactly the generator#224 defect (an fp64 landing on a `maxlen: 4` blob was
	// rejected as INVALID instead of skipped).
	for _, notWant := range []string{"if length > 8 {", "if length > 16 {"} {
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
		for _, want := range []string{"func (m *Q) ArrayBegin(id sofab.ID, count int) error {", "func (m *Q) FixlenHeader(id sofab.ID, subtype int, length int) error {"} {
			if !strings.Contains(out, want) {
				t.Errorf("q.go (%s): a bounded type must implement the whole HeaderVisitor, missing %q:\n%s", tc.name, want, out)
			}
		}
	}
}

func TestGoStructuralInvariants(t *testing.T) {
	files := genGo(t, exampleSchema(t), map[string]any{"package": "messages"})
	msg := files["myfirstmessage.go"]
	for _, want := range []string{
		"package messages",
		"func (m *Myfirstmessage) marshal(e *sofab.Encoder)",
		"_visitorBase", // struct embeds the no-op base
		"func (m *Myfirstmessage) Unsigned(id sofab.ID, v uint64) error", // visitor decode
		"func (m *Myfirstmessage) BeginSequence(id sofab.ID) (sofab.Visitor, error)",
		"func NewMyfirstmessage() *Myfirstmessage",
		"func DecodeMyfirstmessage(",
		"sofab.AcceptBytes(data, m)", // zero-copy cursor decode
		"e.WriteSequenceBegin(",      // nested struct/union framing (marshal unchanged)
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
	// The decode prelude (embedded no-op base + collectors) is emitted once.
	prelude := files["sofab_visitor.go"]
	for _, want := range []string{
		"type _visitorBase struct{}",
		"func _narrowU[T ~uint8 | ~uint16 | ~uint32 | ~uint64](v []uint64) []T",
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
