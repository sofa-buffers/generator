package csharp

import (
	"strings"
	"testing"
)

// resetSchema has one field of every shape Reset treats differently: a scalar
// with a default, a deprecated one, a string and a blob with defaults, a nested
// struct (itself holding a struct), a union, a primitive array with a default,
// a boolean array with a default (a List refilled from the hoisted default), a
// wrapper array presized by its count and one with no count.
const resetSchema = `version: 1
$defs:
  struct:
    Deep: { d: { id: 0, type: i32, default: -5 } }
    Inner:
      x: { id: 0, type: u8, default: 3 }
      deep: { id: 1, type: struct, fields: { $ref: '#/$defs/struct/Deep' } }
messages:
  m:
    payload:
      n:     { id: 0, type: u16, default: 7 }
      old:   { id: 1, type: u8, default: 2, deprecated: true }
      s:     { id: 2, type: string, default: "hi" }
      b:     { id: 3, type: blob, default: "AQI=" }
      inner: { id: 4, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:
        id: 5
        type: union
        default_id: 0
        oneof:
          a: { id: 0, type: u8 }
          t: { id: 1, type: string }
      nums:  { id: 6, type: array, items: { type: u16, count: 4 }, default: [1, 2] }
      flags: { id: 7, type: array, items: { type: boolean, count: 4 }, default: [true, false] }
      names: { id: 8, type: array, items: { type: string, count: 4 } }
      rows:  { id: 9, type: array, items: { type: struct, fields: { $ref: '#/$defs/struct/Deep' } } }
`

// TestCsResetRestoresEveryFieldInPlace: Reset() puts every field back to the
// value a fresh object holds. Where a decode writes INTO the member -- a nested
// struct, a union, a list -- the member is reset in place, so a reference taken
// from the previous decode reads the defaults too; a union keeps its own
// spelling, Clear(). A list with a declared default is refilled from the hoisted
// static, and a list keeps its initializer's capacity when it was null.
// Everything a decode replaces whole is assigned its initializer.
func TestCsResetRestoresEveryFieldInPlace(t *testing.T) {
	src := buildModule(t, []byte(resetSchema), "reset.yaml", map[string]any{})
	msg := csMethod(t, classBody(t, src, "M"), "    public void Reset() {")
	want := []string{
		"#pragma warning disable 612 // internal access to a member marked [Obsolete] (CS0612)",
		"        this.n = 7;",
		"        this.old = 2;",
		"        this.s = \"hi\";",
		"        this.b = new byte[]{1, 2};",
		"        (this.inner ??= new()).Reset();",
		"        (this.u ??= new()).Clear();",
		"        this.nums = new ushort[]{1, 2};",
		"        (this.flags ??= new()).Clear(); this.flags.AddRange(_arrdef_flags);",
		"        (this.names ??= new(4)).Clear();",
		"        (this.rows ??= new()).Clear();",
		"#pragma warning restore 612",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(msg[at:], w+"\n")
		if i < 0 {
			t.Fatalf("message Reset(): missing %q (in order) in:\n%s", w, msg)
		}
		at += i + len(w)
	}
	// One statement per field and nothing else: no helper call, no loop.
	if got := strings.Count(msg, ";\n"); got != 10 {
		t.Errorf("message Reset() has %d statements, want one per field (10):\n%s", got, msg)
	}
	// The struct classes get their own Reset, recursing into theirs.
	inner := csMethod(t, classBody(t, src, "Inner"), "    public void Reset() {")
	for _, w := range []string{"        this.x = 3;\n", "        (this.deep ??= new()).Reset();\n"} {
		if !strings.Contains(inner, w) {
			t.Errorf("struct Reset(): missing %q in:\n%s", w, inner)
		}
	}
	if deep := csMethod(t, classBody(t, src, "Deep"), "    public void Reset() {"); !strings.Contains(deep, "        this.d = -5;\n") {
		t.Errorf("struct Reset(): missing the default -5 in:\n%s", deep)
	}
	// The union keeps Clear() and declares no Reset of its own.
	if u := classBody(t, src, "M_U"); strings.Contains(u, "Reset()") || !strings.Contains(u, "public void Clear() {") {
		t.Errorf("union: want Clear() and no Reset() in:\n%s", u)
	}
	// A struct with a deprecated field needs no pragma when it has none.
	if strings.Contains(inner, "#pragma") {
		t.Errorf("struct Reset() of a class with no deprecated field carries a pragma:\n%s", inner)
	}
}

// TestCsTryDecodeIntoDestination: the by-value TryDecode overload resets the
// destination and feeds a visitor over it -- the same feed the `out` overload
// runs, which stays as it was and still constructs its message. The contract is
// in its doc comment.
func TestCsTryDecodeIntoDestination(t *testing.T) {
	src := buildModule(t, []byte(resetSchema), "reset.yaml", map[string]any{})
	into := "    public static global::sofab.DecodeStatus TryDecode(byte[] data, M msg) {\n" +
		"        msg.Reset();\n" +
		"        return new global::sofab.IStream().Feed(data, 0, data.Length, new _M__Visitor(msg));\n" +
		"    }\n"
	if !strings.Contains(src, into) {
		t.Errorf("missing the TryDecode(data, msg) overload:\n%s", into)
	}
	out := "    public static global::sofab.DecodeStatus TryDecode(byte[] data, out M msg) {\n" +
		"        msg = new M();\n" +
		"        return new global::sofab.IStream().Feed(data, 0, data.Length, new _M__Visitor(msg));\n" +
		"    }\n"
	if !strings.Contains(src, out) {
		t.Errorf("the TryDecode(data, out msg) overload changed; want:\n%s", out)
	}
	if got := strings.Count(src, "public static global::sofab.DecodeStatus TryDecode("); got != 2 {
		t.Errorf("%d TryDecode overloads, want 2", got)
	}
	for _, c := range []string{
		"holds the fields decoded before the",
		"error, never an older message's.",
		"into it overwrites that object",
	} {
		if !strings.Contains(src, c) {
			t.Errorf("the TryDecode(data, msg) doc comment does not state %q", c)
		}
	}
	// Decode() is untouched: it builds its own message and never resets one.
	if dec := csMethod(t, src, "    public static M Decode(byte[] data) {"); strings.Contains(dec, "Reset") {
		t.Errorf("Decode() must not reset:\n%s", dec)
	}
}

// TestCsResetNameIsEscaped: Reset is a member of every struct and message class,
// so a schema field `Reset` takes the member escape (Reset_) in both, and a struct
// type named Reset takes the type escape (CS0542: a member may not be named like
// its class). C# is case-sensitive: a field
// `reset` clashes with nothing and keeps its name.
func TestCsResetNameIsEscaped(t *testing.T) {
	const schema = `version: 1
$defs:
  struct:
    Reset: { x: { id: 0, type: u8 } }
    Holder: { Reset: { id: 0, type: u8 } }
messages:
  m:
    payload:
      Reset: { id: 0, type: u8 }
      r: { id: 1, type: struct, fields: { $ref: '#/$defs/struct/Reset' } }
      h: { id: 2, type: struct, fields: { $ref: '#/$defs/struct/Holder' } }
  lower:
    payload:
      reset: { id: 0, type: u8 }
`
	src := buildModule(t, []byte(schema), "reset.yaml", map[string]any{})
	for _, cls := range []string{"M", "Holder"} {
		body := classBody(t, src, cls)
		for _, w := range []string{
			"    [global::System.Text.Json.Serialization.JsonPropertyName(\"Reset\")]\n    public byte Reset_;",
			"        this.Reset_ = 0;",
		} {
			if !strings.Contains(body, w) {
				t.Errorf("class %s: missing %q in:\n%s", cls, w, body)
			}
		}
	}
	if !strings.Contains(src, "public sealed class Reset_ {") || !strings.Contains(src, "    public Reset_ r = new();") {
		t.Errorf("the field of struct type Reset must be declared with the escaped type Reset_")
	}
	if body := classBody(t, src, "Lower"); !strings.Contains(body, "    public byte reset;") || !strings.Contains(body, "        this.reset = 0;") {
		t.Errorf("a field `reset` clashes with nothing and keeps its name:\n%s", body)
	}
}

// TestCsHarnessReuseVerb: the harness's reusedecode verb decodes the file it is
// given and then stdin into ONE destination through TryDecode(data, msg), and
// prints the verdict, the JSON and the re-encode a refusal included
// (tests/conformance/lib/check_reused_destination.py).
func TestCsHarnessReuseVerb(t *testing.T) {
	prog := csProgram(t, resetSchema)
	for _, w := range []string{
		"} else if (mode == \"reusedecode\") {",
		"global::Message.M.TryDecode(File.ReadAllBytes(args[2]), obj);",
		"try { st = global::Message.M.TryDecode(input, obj).ToString().ToUpperInvariant(); }",
		"catch (SofabException e) { st = e.Error.ToString(); }",
		"try { hex = Convert.ToHexString(obj.Encode()).ToLowerInvariant(); }",
	} {
		if !strings.Contains(prog, w) {
			t.Errorf("Program.cs: missing %q", w)
		}
	}
}
