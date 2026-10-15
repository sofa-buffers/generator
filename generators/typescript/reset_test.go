package typescript

import (
	"regexp"
	"strings"
	"testing"
)

// resetSrc has a field of every shape reset() handles differently: a defaulted
// scalar, a 64-bit integer, an fp32 with its raw-bytes companion, a string and a
// blob with defaults, a native array with a default and one without, a wrapper
// array, a struct, a union -- and a field-less struct.
const resetSrc = `version: 1
messages:
  m:
    payload:
      n:  { id: 0, type: u32, default: 5 }
      i:  { id: 1, type: i64 }
      f:  { id: 2, type: fp32 }
      s:  { id: 3, type: string, maxlen: 8, default: "d" }
      bd: { id: 4, type: blob, maxlen: 8, default: "AQI=" }
      a:  { id: 5, type: array, items: { type: u16, count: 4 }, default: [1, 2] }
      e:  { id: 6, type: array, items: { type: u8 } }
      sa: { id: 7, type: array, items: { type: string, count: 2, maxlen: 4 } }
      st: { id: 8, type: struct, fields: { x: { id: 0, type: i32, default: -3 } } }
      u:  { id: 9, type: union, default_id: 0, oneof: { p: { id: 0, type: u8 }, q: { id: 1, type: string, maxlen: 4 } } }
      z:  { id: 10, type: struct, fields: {} }
`

// classBody is the text of `export class <name> {` up to its closing brace.
func classBody(t *testing.T, mod, name string) string {
	t.Helper()
	start := strings.Index(mod, "export class "+name+" {")
	if start < 0 {
		t.Fatalf("no class %s:\n%s", name, mod)
	}
	end := strings.Index(mod[start:], "\n}\n")
	return mod[start : start+end]
}

// TestTSResetRestoresEveryDefault: every message and struct class carries a
// public reset() that puts each field back to the value its declaration
// initialises it with -- a nested struct reset in place, a union back to its
// default option through its own clear(), an fp32's raw bytes dropped.
func TestTSResetRestoresEveryDefault(t *testing.T) {
	for mode, i := range map[string]string{
		"bigint": "this.i = 0n;",
		"long":   "this._i = Long.ZERO;",
		"number": "this.i = 0;",
	} {
		mod := genTSWith(t, resetSrc, map[string]any{"int64": mode})
		want := "  reset(): void {\n" +
			"    this.n = 5;\n" +
			"    " + i + "\n" +
			"    this.f = 0;\n" +
			"    this.fFp32Raw = null;\n" +
			"    this.s = \"d\";\n" +
			"    this.bd = new Uint8Array([1, 2]);\n" +
			"    this.a = new Uint16Array([1, 2]);\n" +
			"    this.e = _E_Uint8Array;\n" +
			"    this.sa = [];\n" +
			"    this.st.reset();\n" +
			"    this.u.clear();\n" +
			"    this.z.reset();\n" +
			"  }\n"
		if got := classBody(t, mod, "M"); !strings.Contains(got, want) {
			t.Errorf("int64: %s -- message reset() missing\n%s\nin:\n%s", mode, want, got)
		}
		if got := classBody(t, mod, "M_St"); !strings.Contains(got, "  reset(): void {\n    this.x = -3;\n  }\n") {
			t.Errorf("int64: %s -- struct reset() missing:\n%s", mode, got)
		}
		// A struct with no field still has one: the message's reset() calls it.
		if got := classBody(t, mod, "M_Z"); !strings.Contains(got, "  reset(): void {\n  }\n") {
			t.Errorf("int64: %s -- a field-less struct needs an empty reset():\n%s", mode, got)
		}
		// The union keeps its one spelling of the same thing.
		if got := classBody(t, mod, "M_U"); strings.Contains(got, "reset(") || !strings.Contains(got, "clear(): void {") {
			t.Errorf("int64: %s -- a union is reset by clear() alone:\n%s", mode, got)
		}
	}
}

// TestTSDecoderResetsItsDestination: the incremental decoder resets a destination
// it is handed before a byte reaches it -- before the visitor that fills it is
// built -- and says so, rejected input included.
func TestTSDecoderResetsItsDestination(t *testing.T) {
	mod := genTSWith(t, resetSrc, map[string]any{})
	want := "  constructor(out?: M) {\n" +
		"    out?.reset();\n" +
		"    this.out = out ?? new M();\n" +
		"    this.is = new IStream(new _M__Visitor(this.out, new PayloadAcc()));\n" +
		"  }\n"
	if !strings.Contains(mod, want) {
		t.Errorf("decoder must reset its destination first; missing\n%s\nin:\n%s", want, mod)
	}
	for _, doc := range []string{
		" * Pass a destination to decode into it: the constructor resets it to the",
		" * the destination holds the fields decoded before the error, never an older",
	} {
		if !strings.Contains(mod, doc) {
			t.Errorf("decoder doc missing %q", doc)
		}
	}
}

// TestTSResetFieldIsEscaped: reset() is a member of every message and struct, so
// a schema field named `reset` takes the backend's escape -- in the declaration,
// in the reset() body that assigns it, and nowhere in the JSON key.
func TestTSResetFieldIsEscaped(t *testing.T) {
	const src = "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      reset: { id: 0, type: u8, default: 4 }\n" +
		"      st: { id: 1, type: struct, fields: { reset: { id: 0, type: u8 } } }\n"
	if tsIdent("reset") != "reset_" {
		t.Fatalf("tsIdent(reset) = %q, want reset_", tsIdent("reset"))
	}
	mod := genTSWith(t, src, map[string]any{})
	for _, want := range []string{
		"  reset_: number = 4;",
		"  reset(): void {\n    this.reset_ = 4;\n    this.st.reset();\n  }",
		"  reset(): void {\n    this.reset_ = 0;\n  }",
		`"reset": this.reset_`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q:\n%s", want, mod)
		}
	}
	if regexp.MustCompile(`(?m)^  reset: `).MatchString(mod) {
		t.Errorf("a field must not be declared as the member reset:\n%s", mod)
	}
}
