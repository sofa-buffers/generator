package golang

import (
	"strings"
	"testing"
)

// Decode into a reused destination (generator#756): <Msg>__TryDecode(data, out)
// resets out and decodes into it, and the arrays and blobs out held are written
// into rather than allocated anew. These tests pin the SHAPE of what is emitted;
// reuse_corelib_test.go drives it against corelib-go, and the conformance suite
// runs tests/conformance/lib/check_reused_destination.py over it.

const reuseDef = `version: 1
messages:
  probe:
    payload:
      n:   { id: 0, type: u32, default: 5 }
      f:   { id: 1, type: fp32 }
      s:   { id: 2, type: string, maxlen: 8, default: "dflt" }
      b:   { id: 3, type: blob, maxlen: 8 }
      bd:  { id: 4, type: blob, maxlen: 8, default: "AQI=" }
      a:   { id: 6, type: array, items: { type: u16, count: 4 }, default: [1, 2] }
      ae:  { id: 7, type: array, items: { type: u16, count: 4 }, default: [] }
      an:  { id: 8, type: array, items: { type: i32, count: 4 } }
      sa:  { id: 9, type: array, items: { type: string, count: 4, maxlen: 8 } }
      st:
        id: 10
        type: struct
        fields:
          x: { id: 0, type: i32, default: -3 }
          xs: { id: 1, type: array, items: { type: u8, count: 4 } }
      u:
        id: 11
        type: union
        default_id: 0
        oneof:
          p: { id: 0, type: u8 }
          q: { id: 1, type: blob, maxlen: 4 }
          r: { id: 2, type: array, items: { type: u8, count: 4 } }
          w: { id: 3, type: array, items: { type: string, count: 2, maxlen: 4 } }
      e:   { id: 12, type: enum, enum: { A: 0, B: 1 }, default: 1 }
      old: { id: 13, type: u32, default: 9, deprecated: true }
`

func TestGoResetPutsEveryFieldBack(t *testing.T) {
	files := genGo(t, schemaFromYAMLString(t, reuseDef), map[string]any{})
	msg, types := files["probe.go"], files["sofab_types.go"]

	// One statement per field, in schema order: the schema's own defaults, a
	// nested struct through its Reset, a union through Clear, and every array and
	// blob handing the storage it held to its _spare member. A non-empty default
	// is appended into that storage; an empty one is the zero-sized literal and
	// never nil, which would read as absent.
	want := "func (m *Probe) Reset() {\n" +
		"\tm.N = 5\n" +
		"\tm.F = 0\n" +
		"\tm.S = \"dflt\"\n" +
		"\tm._spareB, m.B = m.B, nil\n" +
		"\tm._spareBd, m.Bd = m.Bd, append(m.Bd[:0], []byte{0x01, 0x02}...)\n" +
		"\tm._spareA, m.A = m.A, append(m.A[:0], []uint16{1, 2}...)\n" +
		"\tm._spareAe, m.Ae = m.Ae, []uint16{}\n" +
		"\tm._spareAn, m.An = m.An, nil\n" +
		"\tm._spareSa, m.Sa = m.Sa, nil\n" +
		"\tm.St.Reset()\n" +
		"\tm.U.Clear()\n" +
		"\tm.E = Probe_E(1)\n" +
		"\tm.Old = 9\n" +
		"}"
	if !strings.Contains(msg, want) {
		t.Errorf("Probe.Reset is not the schema's field list:\nwant\n%s\n\ngot\n%s", want, msg)
	}
	// The spare members: private, one per array or blob field, none for a
	// scalar or a string.
	for _, decl := range []string{"_spareB  []byte", "_spareA  []uint16", "_spareSa []string"} {
		if !strings.Contains(msg, decl) {
			t.Errorf("Probe is missing spare member %q", decl)
		}
	}
	for _, none := range []string{"_spareN", "_spareS ", "_spareSt", "_spareU "} {
		if strings.Contains(msg, none) {
			t.Errorf("Probe declares a spare for a field with no storage: %q", none)
		}
	}
	// A struct type gets its own Reset, which a parent's Reset calls.
	if !strings.Contains(types, "func (m *Probe_St) Reset() {\n\tm.X = -3\n\tm._spareXs, m.Xs = m.Xs, nil\n}") {
		t.Errorf("Probe_St.Reset missing or wrong:\n%s", types)
	}
	// A union keeps Clear and declares no Reset: two names for one operation
	// would only add a member an option can collide with.
	if strings.Contains(types, "func (m *Probe_U) Reset()") {
		t.Error("a union declares Reset beside Clear")
	}
	if strings.Contains(types, "_spareQ") || strings.Contains(types, "_spareR") {
		t.Error("a union option slot got a spare member: unions keep no storage across Clear")
	}
}

func TestGoTryDecodeIsResetThenDecode(t *testing.T) {
	msg := genGo(t, schemaFromYAMLString(t, reuseDef), map[string]any{})["probe.go"]
	// The whole body: Reset in place of New, on the same AcceptBytes Decode
	// runs, so the verdict is Decode's on the same bytes and nothing else is a
	// second decode machine.
	want := "func Probe__TryDecode(data []byte, out *Probe) error {\n\tout.Reset()\n\treturn sofab.AcceptBytes(data, out)\n}"
	if !strings.Contains(msg, want) {
		t.Errorf("Probe__TryDecode missing or wrong:\n%s", msg)
	}
	// The contract a caller needs: what is overwritten, and what a refusal leaves.
	for _, doc := range []string{
		"is overwritten by it. Copy what you keep.",
		"// decoded before the error and the defaults for the rest, never a value of",
	} {
		if !strings.Contains(msg, doc) {
			t.Errorf("Probe__TryDecode doc lacks %q", doc)
		}
	}
}

func TestGoReuseArmsWriteIntoTheSpare(t *testing.T) {
	files := genGo(t, schemaFromYAMLString(t, reuseDef), map[string]any{})
	msg, types := files["probe.go"], files["sofab_types.go"]
	for _, arm := range []string{
		// A native array opens over the spare, after the kind gate and the bound.
		"\t\tif count > 4 {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\tm.An = sofab.ReuseSlice(m._spareAn, count)",
		// A blob is copied into the spare's storage.
		"\t\tm.B = sofab.ReuseBytes(m._spareB, _b)",
		// A wrapper array hands its collector the spare's storage, never nil.
		"\t\tm.Sa = sofab.ReuseSlice(m._spareSa, 0)\n\t\treturn sofab.NewStringSeq(&m.Sa,",
	} {
		if !strings.Contains(msg, arm) {
			t.Errorf("Probe lacks the reusing arm %q", arm)
		}
	}
	if strings.Contains(msg, ", 0, count)") || strings.Contains(msg, "append([]byte(nil), _b...)") {
		t.Errorf("a struct arm still allocates fresh storage:\n%s", msg)
	}
	// A union option has no spare: it allocates as it did, and a wrapper option
	// reuses its slot through the same never-nil call.
	for _, arm := range []string{
		"m.SetQ(append([]byte(nil), _b...))",
		"m.optR = make([]uint8, 0, count)",
		"m.optW = sofab.ReuseSlice(m.optW, 0)",
	} {
		if !strings.Contains(types, arm) {
			t.Errorf("Probe_U lacks its unchanged arm %q", arm)
		}
	}
}

// Reset is a member every struct and message declares, so a field named
// `reset` takes the escape (Reset_), exactly as one named `encode` does. A union
// declares Clear and no Reset, so an option `reset` keeps its plain getter.
func TestGoResetNameIsEscaped(t *testing.T) {
	const def = `version: 1
messages:
  m:
    payload:
      reset: { id: 0, type: u32, default: 3 }
      s: { id: 1, type: struct, fields: { reset: { id: 0, type: blob, maxlen: 4 } } }
      u:
        id: 2
        type: union
        default_id: 0
        oneof:
          reset: { id: 0, type: u8 }
          other: { id: 1, type: u16 }
`
	files := genGo(t, schemaFromYAMLString(t, def), map[string]any{})
	msg, types := files["m.go"], files["sofab_types.go"]
	for _, s := range []string{"Reset_ uint32 `json:\"reset\"`", "\tm.Reset_ = 3\n"} {
		if !strings.Contains(msg, s) {
			t.Errorf("message field `reset` is not escaped: missing %q", s)
		}
	}
	for _, s := range []string{"Reset_ []byte `json:\"reset\"`", "\tm._spareReset_, m.Reset_ = m.Reset_, nil\n"} {
		if !strings.Contains(types, s) {
			t.Errorf("struct field `reset` is not escaped: missing %q", s)
		}
	}
	if !strings.Contains(types, "func (m *M_U) Reset() uint8 {") {
		t.Error("union option `reset` lost its plain getter Reset")
	}
}

func TestGoHarnessReuseVerbAndBench(t *testing.T) {
	files := genGo(t, schemaFromYAMLString(t, reuseDef), map[string]any{"emit": "project"})
	h := files["harness/main.go"]
	for _, s := range []string{
		// The verb check_reused_destination.py drives: the held message through
		// the plain Decode, the next one through TryDecode into it.
		"} else if mode == \"reusedecode\" {",
		"obj, err := message.Probe__Decode(hb)",
		"if err := message.Probe__TryDecode(in, obj); errors.Is(err, sofab.ErrIncomplete) {",
		// The bench decode op: TryDecode into a destination its warmup already
		// decoded the same wire into -- the receive-loop steady state.
		"func run_decode_probe() {\n\tif err := message.Probe__TryDecode(benchProbeWire, benchProbeOut); err != nil {",
		"benchProbeOut = message.Probe__New()\n\t\t\twarmup_decode_probe()",
	} {
		if !strings.Contains(h, s) {
			t.Errorf("harness lacks %q", s)
		}
	}
}
