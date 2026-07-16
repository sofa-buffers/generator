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

func TestGoMapField(t *testing.T) {
	s := schemaFromYAMLString(t, `
version: 1
messages:
  M:
    payload:
      counts: { type: map, id: 1, key: { type: string, maxlen: 32 }, value: { type: u32 }, count: 128 }
      nested:
        type: map
        id: 2
        key: { type: u32 }
        value: { type: map, key: { type: u32 }, value: { type: u8 } }
`)
	files := genGo(t, s, map[string]any{})
	msg := files["m.go"]
	for _, want := range []string{
		"Counts map[string]uint32",              // surface container
		"Nested map[uint32]map[uint32]uint8",    // nested map value
		"sort.Slice(_keys",                      // canonical-order encode
		"_entry := MCountsEntry{Key: _k, Value: m.Counts[_k]}", // entry-struct reuse on marshal
		"_mapSeq[string, uint32, MCountsEntry, *MCountsEntry]", // decode collector
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("m.go missing %q", want)
		}
	}
	// The shared collector type is emitted once in the prelude.
	if !strings.Contains(files["sofab_visitor.go"], "type _mapSeq[") {
		t.Error("prelude missing _mapSeq collector")
	}
	// Everything must be valid Go.
	fset := token.NewFileSet()
	for path, src := range files {
		if strings.HasSuffix(path, ".go") {
			if _, err := goparser.ParseFile(fset, path, []byte(src), goparser.AllErrors); err != nil {
				t.Errorf("generated %s is not valid Go: %v", path, err)
			}
		}
	}
}
