package golang

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The encode half of a schema bound (ARCHITECTURE §9.6): a string or blob past
// its maxlen, an array past its count -- at every level a bound is declared --
// is refused at encode, before any byte is handed back as a message. The bound
// lives in the generated per-field guard only; the corelib is handed nothing but
// the verdict (sofab.Encoder.RejectArgument, ErrArgument).

const encodeBoundsSchema = "version: 1\nmessages:\n  M:\n    payload:\n" +
	"      s:   { id: 0, type: string, maxlen: 4 }\n" +
	"      sd:  { id: 1, type: string, maxlen: 6, default: \"abc\" }\n" +
	"      b:   { id: 2, type: blob, maxlen: 5 }\n" +
	"      au:  { id: 3, type: array, items: { type: u32, count: 3 } }\n" +
	"      as:  { id: 4, type: array, items: { type: string, count: 2, maxlen: 7 } }\n" +
	"      ab:  { id: 5, type: array, items: { type: blob, count: 9, maxlen: 8 } }\n" +
	"      mx:  { id: 6, type: array, items: { type: array, count: 2, items: { type: u16, count: 11 } } }\n" +
	"      ms:  { id: 7, type: array, items: { type: array, count: 12, items: { type: string, count: 13, maxlen: 14 } } }\n" +
	"      ab2: { id: 8, type: array, items: { type: boolean, count: 15 } }\n" +
	"      u8:  { id: 9, type: u8 }\n" +
	"      e:   { id: 10, type: enum, enum: { A: 0, B: 1 } }\n" +
	"      us:  { id: 11, type: string }\n" +
	"      ub:  { id: 12, type: blob }\n" +
	"      ua:  { id: 13, type: array, items: { type: u32 } }\n" +
	"      un:\n" +
	"        id: 14\n" +
	"        type: union\n" +
	"        default_id: 0\n" +
	"        oneof:\n" +
	"          o1: { id: 0, type: u16 }\n" +
	"          o2: { id: 1, type: string, maxlen: 16 }\n"

// guard is the exact refusal emitted at indent ind for cond.
func guard(ind, cond string) string {
	return ind + "if " + cond + " {\n" + ind + "\te.RejectArgument()\n" + ind + "\treturn\n" + ind + "}\n"
}

func TestGoEncodeRefusesOverBoundValues(t *testing.T) {
	files := genGo(t, schemaFromYAMLString(t, encodeBoundsSchema), map[string]any{"package": "m"})
	ser := between(files["m.go"], "func (m *M) Serialize(", "\n}\n")
	if ser == "" {
		t.Fatalf("no Serialize in m.go:\n%s", files["m.go"])
	}
	for _, want := range []string{
		// A leaf guard sits INSIDE the write branch: a field at its default is
		// not written and pays nothing.
		"\tif m.S != \"\" {\n" + guard("\t\t", "len(m.S) > 4") + "\t\te.WriteString(0, m.S)",
		"\tif m.Sd != \"abc\" {\n" + guard("\t\t", "len(m.Sd) > 6") + "\t\te.WriteString(1, m.Sd)",
		"\tif len(m.B) != 0 {\n" + guard("\t\t", "len(m.B) > 5") + "\t\te.WriteBytes(2, m.B)",
		"\tif len(m.Au) != 0 {\n" + guard("\t\t", "len(m.Au) > 3") + "\t\tsofab.WriteUnsignedArray(e, 3, m.Au)",
		// A wrapper array: the count before its frame opens, the element maxlen
		// inside the element's write branch.
		guard("\t", "len(m.As) > 2") + "\te.WriteSequenceBeginLazy(4)",
		"\t\tif _e0 != \"\" || _i0 == len(m.As)-1 {\n" + guard("\t\t\t", "len(_e0) > 7") + "\t\t\te.WriteString(sofab.ID(_i0), _e0)",
		guard("\t", "len(m.Ab) > 9") + "\te.WriteSequenceBeginLazy(5)",
		"\t\tif len(_e0) != 0 || _i0 == len(m.Ab)-1 {\n" + guard("\t\t\t", "len(_e0) > 8") + "\t\t\te.WriteBytes(sofab.ID(_i0), _e0)",
		// Nested arrays: every level's own count, and the inner element maxlen.
		guard("\t", "len(m.Mx) > 2") + "\te.WriteSequenceBeginLazy(6)",
		guard("\t\t\t", "len(_e0) > 11") + "\t\t\tsofab.WriteUnsignedArray(e, sofab.ID(_i0), _e0)",
		guard("\t", "len(m.Ms) > 12") + "\te.WriteSequenceBeginLazy(7)",
		guard("\t\t", "len(_e0) > 13") + "\t\te.WriteSequenceBeginLazy(sofab.ID(_i0))",
		guard("\t\t\t\t", "len(_e1) > 14") + "\t\t\t\te.WriteString(sofab.ID(_i1), _e1)",
		// A boolean array is counted before its 0/1 copy is built.
		"\tif len(m.Ab2) != 0 {\n" + guard("\t\t", "len(m.Ab2) > 15") + "\t\t{\n\t\t\t_b0 := make([]uint8, len(m.Ab2))",
	} {
		if !strings.Contains(ser, want) {
			t.Errorf("Serialize missing\n%s\n--- got:\n%s", want, ser)
		}
	}
	// Exactly one guard per declared bound, and nothing for the undeclared ones
	// (us/ub/ua) or for a scalar: the Go type of u8 and the enum IS the declared
	// width, so no value past it can be held.
	if got, want := strings.Count(ser, "e.RejectArgument()"), 14; got != want {
		t.Errorf("Serialize has %d guards, want %d (one per declared bound):\n%s", got, want, ser)
	}
	for _, unwanted := range []string{"len(m.Us) >", "len(m.Ub) >", "len(m.Ua) >", "m.U8 >", "m.E >", "m.E <"} {
		if strings.Contains(ser, unwanted) {
			t.Errorf("Serialize guards %q, which declares no bound:\n%s", unwanted, ser)
		}
	}
	// A held union option is written whatever its value, so its guard has no
	// write branch to sit in and precedes the write.
	un := files["sofab_types.go"]
	if !strings.Contains(un, guard("\t\t", "len(m.optO2) > 16")+"\t\te.WriteString(1, m.optO2)") {
		t.Errorf("union string option missing its maxlen guard:\n%s", un)
	}
}

// The generated guard behaves: an over-bound value fails Encode and EncodeTo with
// ErrArgument (exit 1 from the harness), a value at the bound round-trips.
// Gated on SOFAB_GO_CORELIB like the other wire tests.
func TestGoEncodeBoundsWire(t *testing.T) {
	corelib := requireGoCorelib(t)
	bin := buildGoHarnessCfg(t, corelib, encodeBoundsSchema, nil)
	run := func(in string) ([]byte, error) {
		cmd := exec.Command(bin, "encode", "M")
		cmd.Stdin = strings.NewReader(in)
		return cmd.Output()
	}
	for _, over := range []string{
		`{"s":"xxxxx"}`, `{"s":"xxxé"}`, `{"sd":"abcdefg"}`, `{"b":"AAAAAAAA"}`,
		`{"au":[1,2,3,4]}`, `{"as":["a","b","c"]}`, `{"as":["","12345678"]}`,
		`{"ab":["AAAAAAAAAAAA"]}`, `{"mx":[[1],[2],[3]]}`, `{"mx":[[1,2,3,4,5,6,7,8,9,10,11,12]]}`,
		`{"ms":[["123456789012345"]]}`, `{"ab2":[true,true,true,true,true,true,true,true,true,true,true,true,true,true,true,true]}`,
		`{"un":{"o2":"12345678901234567"}}`,
	} {
		if out, err := run(over); err == nil {
			t.Errorf("encode %s succeeded with %x, want a refusal", over, out)
		} else if ee, ok := err.(*exec.ExitError); ok && !strings.Contains(string(ee.Stderr), "invalid argument") {
			t.Errorf("encode %s refused with %q, want ErrArgument", over, ee.Stderr)
		}
	}
	at := `{"s":"xxxx","sd":"abcdef","b":"AAAAAAA=","au":[1,2,3],"as":["","1234567"],` +
		`"mx":[[1,2,3,4,5,6,7,8,9,10,11],[]],"ms":[["12345678901234"]],"un":{"o2":"1234567890123456"}}`
	wire, err := run(at)
	if err != nil {
		t.Fatalf("encode at the bound failed: %v", err)
	}
	dec := exec.Command(bin, "decode", "M")
	dec.Stdin = strings.NewReader(string(wire))
	out, err := dec.Output()
	if err != nil {
		t.Fatalf("decode at the bound failed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode printed no JSON: %s", out)
	}
	if got["s"] != "xxxx" || got["sd"] != "abcdef" {
		t.Errorf("at-bound values changed: %s", out)
	}
}
