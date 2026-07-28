package java

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func exampleFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(b, "example.yaml")
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
	files, err := (&Backend{}).Generate(s, map[string]any{"package": "messages"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "Myfirstmessage.java") {
			return string(f.Content)
		}
	}
	t.Fatal("no message file")
	return ""
}

func TestJavaStructural(t *testing.T) {
	m := exampleFile(t)
	for _, want := range []string{
		"package messages;",
		"import org.sofabuffers.sofab.*;",
		"public class Myfirstmessage {",
		"public void marshal(OStream os) throws IOException",
		"public byte[] encode()",
		"public static Myfirstmessage decode(byte[] data)",
		"public static DecodeStatus tryDecode(byte[] data, Myfirstmessage out) throws SofabException", // status-surfacing decode (#105)
		"class MyfirstmessageVisitor implements Visitor {",
		"public void sequenceBegin(int id)", // flat-visitor nesting
		"public long someu64 = Long.parseUnsignedLong(\"18446744073709551615\");",
		"class MyfirstmessageSomestructNestedstruct {",                                                // nested types in file
		"public long[] someuintarray = new long[]{0L, 1L, 1000L, 4294967295L};",                       // primitive array (was List<Long>)
		"public float[] somefloatarray = new float[]{0.0f, -1.5f, 3.25f};",                            // primitive fp array
		"public long[] someenumarray = new long[]{2L, 1L, 0L, 0};",                                    // short default tail-padded to count
		"os.writeArrayUnsigned(15, Sbuf.trimTail(this.someuintarray));",                               // direct write, no Sbuf box; count: 4 -> trailing default run elided (#136)
		"private static final long[] _arrdef_someuintarray = new long[]{0L, 1L, 1000L, 4294967295L};", // omit-default hoisted to a static (#146)
		"if (!java.util.Arrays.equals(this.someuintarray, _arrdef_someuintarray)) {",                  // guard reads the static -- no per-encode new long[] (#146)
		"m.someuintarray = ensureCap(m.someuintarray, ai, acap); m.someuintarray[ai++] = value;",      // grow-on-demand indexed decode (#96)
		"case 15: if (count > 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"someuintarray: array count above schema capacity 4\")); acap = 4; m.someuintarray = new long[4]; break;", // over-count rejected (#100); fixed count -> materialize exactly N, zero tail (#136)
		"private static long[] ensureCap(long[] a, int i, int cap) {",   // lazy-growth helper
		"private static float[] ensureCap(float[] a, int i, int cap) {", // fp32 overload
		"if (offset == 0 && chunkLength >= total) {",                    // string/blob single-shot
		"public List<Boolean> someboolarray",                            // boolean array stays boxed List
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Myfirstmessage.java missing %q", want)
		}
	}
}

func TestJavaDeterministic(t *testing.T) {
	if exampleFile(t) != exampleFile(t) {
		t.Fatal("Java generation not deterministic")
	}
}

// TestJavaDeprecatedField: a deprecated field carries both the native
// @Deprecated annotation and a Javadoc @deprecated tag (with its original
// description preserved). Java lowers enum/bitfield fields to raw long, so no
// enum/flag symbols are emitted to annotate.
func TestJavaDeprecatedField(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode:
      Off: { value: 0, description: "Powered down." }
  bitfield:
    Flags:
      ready: { pos: 0, default: true, description: "Initialized." }
messages:
  Telemetry:
    payload:
      legacyId: { id: 1, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 2, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 3, type: bitfield, bits: { $ref: "#/$defs/bitfield/Flags" } }
`
	m := genJavaFromYAML(t, src, map[string]any{"package": "messages"})["src/main/java/messages/Telemetry.java"]
	for _, want := range []string{
		// Description preserved, @deprecated tag appended, native annotation emitted.
		"     * Old identifier retained for backward compatibility.",
		"     * @deprecated This field is deprecated and may be removed in a future version.",
		"    @Deprecated\n    public long legacyId;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Telemetry.java missing %q", want)
		}
	}
	// Java lowers enum/bitfield to long: no enum/flag type or symbol is emitted.
	if strings.Contains(m, "enum Mode") || strings.Contains(m, "enum Flags") {
		t.Error("Java must lower enum/bitfield to long, not emit enum types")
	}
	if !strings.Contains(m, "public long mode;") || !strings.Contains(m, "public long status") {
		t.Error("enum/bitfield fields must be lowered to long")
	}
}

// genJavaFromYAML generates from an inline definition and returns the emitted
// files keyed by path.
// TestJavaOverIndexWrapperArray: a fixed-count wrapper array (string/blob/struct
// elements) throws INVALID_MSG for an element id >= N before the List grows
// (issue #142 / MESSAGE_SPEC §5.1/§7). A dynamic array keeps every index.
func TestJavaOverIndexWrapperArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" +
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      ds: { id: 3, type: array, items: { type: string } }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		`if (id >= 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_bs element: array index above schema capacity 4")); while (m.bs.size() <= id)`,
		`if (id >= 3) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_bb element: array index above schema capacity 3")); while (m.bb.size() <= id)`,
		// The struct-element arm gap-fills and PLACES by id (generator#247), so the
		// guard is now followed by the same grow-to-id loop the leaf arms use.
		`if (id >= 2) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_bp element: array index above schema capacity 2")); while (m.bp.size() <= id) m.bp.add(new`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing over-index guard %q", want)
		}
	}
	// Dynamic string array keeps every index (bare grow).
	if !strings.Contains(m, `while (m.ds.size() <= id) m.ds.add(""); m.ds.set(id, _s); break;`) ||
		strings.Contains(m, `array index above schema capacity`+" ds") {
		// ensure ds arm has no guard prefix
		if strings.Contains(m, `INVALID_MSG, "Root_ds element`) {
			t.Errorf("dynamic string array must not carry an over-index guard")
		}
	}
}

func genJavaFromYAML(t *testing.T, src string, cfg map[string]any) map[string]string {
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

// TestJavaDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated visitor — named constants plus a
// LIMIT_EXCEEDED guard on every schema-unbounded field, checked at the wire
// count / total header before any allocation. Schema-bounded fields keep only
// their generator#100 INVALID_MSG guard; an unset key (or a key whose kind has
// no unbounded field) emits nothing, keeping the output byte-identical.
func TestJavaDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 6 } }
`
	cfg := map[string]any{
		"max_dyn_array_count": 4,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	}
	m := genJavaFromYAML(t, src, cfg)["src/main/java/message/Dyn.java"]
	for _, want := range []string{
		"static final long MAX_DYN_ARRAY_COUNT = 4L;",
		"static final long MAX_DYN_STRING_LEN = 4096L;",
		// Unbounded array: count checked against the cap before the (lazy) reservation.
		`case 1: if (count > MAX_DYN_ARRAY_COUNT) throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "arr: array count above configured limit 4")); m.arr = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// Bounded array: only the generator#100 schema guard, never the cap.
		`case 2: if (count > 6) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "barr: array count above schema capacity 6")); acap = 6; m.barr = new long[6]; break;`,
		// Unbounded string: total checked at the top of string(), before accumulation.
		"if (total > MAX_DYN_STRING_LEN) {",
		`case 0: throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "s: string length above configured limit 4096"));`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Dyn.java missing %q", want)
		}
	}
	if strings.Contains(m, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}

	// No limits configured -> no limit plumbing at all.
	plain := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/Dyn.java"]
	if strings.Contains(plain, "MAX_DYN") || strings.Contains(plain, "LIMIT_EXCEEDED") {
		t.Error("unset limits must emit no limit plumbing")
	}
}

// TestJavaMaxlenReject: a bounded string/blob (schema maxlen) whose wire byte
// length exceeds its maxlen is malformed input (MESSAGE_SPEC §7.1) and must be
// rejected as INVALID_MSG at the length header, before any byte accumulates --
// never truncated. This covers scalar fields and wrapper-array string/blob
// elements alike. A schema-unbounded field carries no maxlen guard (it keeps
// only the generator#102 configured-limit behavior).
func TestJavaMaxlenReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:   { id: 0, type: string, maxlen: 8 }\n" +
		"      b:   { id: 1, type: blob,   maxlen: 8 }\n" +
		"      u:   { id: 2, type: string }\n" +
		"      arr: { id: 3, type: array, items: { type: string, maxlen: 5 } }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// Bounded scalar string: reject total > maxlen at the top of string().
		`case 0: if (total > 8) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "s: string length above schema maxlen 8")); break;`,
		// Bounded scalar blob: reject total > maxlen at the top of blob().
		`case 1: if (total > 8) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "b: blob length above schema maxlen 8")); break;`,
		// Bounded wrapper string element: reject total > element maxlen.
		`if (total > 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "arr element: string length above schema maxlen 5")); break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing maxlen reject %q", want)
		}
	}
	// The unbounded string `u` (id 2) gets no maxlen guard.
	if strings.Contains(m, `"u: string length above schema maxlen`) {
		t.Error("unbounded string must not carry a maxlen guard")
	}
	// No config limits set -> no configured-limit plumbing, only the maxlen guards.
	if strings.Contains(m, "MAX_DYN") || strings.Contains(m, "LIMIT_EXCEEDED") {
		t.Error("unset limits must emit no configured-limit plumbing")
	}
}

// TestJavaArrayAtScalarIdSkipped: MESSAGE_SPEC §7.3 — a field whose header wire
// type is not the one its declared type maps to is SKIPPED like an unknown id.
// corelib-java delivers array elements one-by-one through the same
// unsigned()/signed()/fp32()/fp64() callbacks a lone scalar uses, so the id
// dispatch alone cannot tell an array element from a scalar; arrayBegin must arm
// a discard counter with the announced count and those callbacks must drop
// exactly that many (generator#183 for integers, #193 for fp). Ids that genuinely
// declare a native array of the matching element kind disarm it — integer arrays
// under UNSIGNED/SIGNED, fp arrays under FIXLEN.
func TestJavaArrayAtScalarIdSkipped(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      u:  { id: 0, type: u8, default: 7 }\n" +
		"      i:  { id: 1, type: i8, default: 10 }\n" +
		"      ua: { id: 2, type: array, items: { type: u32, count: 4 } }\n" +
		"      ia: { id: 3, type: array, items: { type: i32, count: 4 } }\n" +
		"      fa: { id: 4, type: array, items: { type: fp32, count: 3 } }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// The counters themselves (askip: generator#183; afill: generator#188).
		"private int askip = 0;",
		"private int afill = 0;",
		// Armed at the top of arrayBegin, integer array kinds.
		"        askip = 0;\n        afill = 0;\n        if (kind == ArrayKind.UNSIGNED || kind == ArrayKind.SIGNED) {\n            askip = count;\n            switch (cur) {",
		// The two declared integer arrays (ids 2, 3) disarm the skip AND arm the
		// fill; the fp32 array (id 4) is armed the same way in the FIXLEN branch.
		"                case 2: case 3: askip = 0; afill = count; break;",
		"        else if (kind == ArrayKind.FIXLEN) {\n            askip = count;\n            switch (cur) {",
		"                case 4: askip = 0; afill = count; break;",
		// Discarded at the top of every callback an array shares with a scalar.
		"    public void unsigned(int id, long value) {\n        // Drop an element of an array",
		"    public void signed(int id, long value) {\n        // Drop an element of an array",
		"    public void fp32(int id, float value) {\n        // Drop an element of an array",
		"    public void fp64(int id, double value) {\n        // Drop an element of an array",
		"        if (askip > 0) { askip--; return; }",
		// The mirror guard (generator#188) fronts every native-array fill arm.
		"if (afill == 0) break; afill--; ",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing §7.3 array-skip guard %q", want)
		}
	}
	// The fp32 array is armed in the FIXLEN branch, never grouped with the integer
	// arm — id 4 must not appear alongside ids 2/3 under UNSIGNED/SIGNED.
	if strings.Contains(m, "case 2: case 3: case 4: askip = 0") {
		t.Error("an fp32 array must be armed under FIXLEN, not the integer arm")
	}
}

// A `count: N` array is FIXED-LENGTH (MESSAGE_SPEC §3, finding F-0010): the
// encoder elides the trailing run of default elements and the decoder rebuilds
// it from the schema count, so the decoded value always has exactly N elements.
// A dynamic (count-less) array has no N to refill from — a trailing default
// element is significant there and must survive untouched.
// The UTF-8 validator takes an EXCLUSIVE END index (`_utf8ok(b, i, end)`) while
// its caller `_utf8` takes an (offset, length) pair, so the call must convert:
// `off + len`, never `len`. Passing `len` scans the wrong range, and in the
// single-shot decode path — `_utf8(data, chunkOffset, total)`, where
// `chunkOffset` is non-zero for any field that is not first in the buffer —
// `chunkOffset >= total` makes the loop body never run, so the validator
// returns true for every input and strict UTF-8 (#85) is silently bypassed.
func TestJavaUtf8ValidatorRange(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      s: { id: 0, type: string }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	if !strings.Contains(m, "if (Utf8.valid(b, off, off + len))") {
		t.Error("_utf8 must pass an exclusive end index (off + len) to Utf8.valid")
	}
	if strings.Contains(m, "Utf8.valid(b, off, len)") {
		t.Error("Utf8.valid called with a length where an exclusive end index is required")
	}
}

func TestJavaFixedCountTrailingDefaultRun(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Color: { RED: 0, GREEN: 1 }
  bitfield:
    Flags:
      a: { pos: 0 }
      b: { pos: 1 }
messages:
  m:
    payload:
      fu:   { id: 0, type: array, items: { type: u32, count: 5 } }
      fi:   { id: 1, type: array, items: { type: i32, count: 5 } }
      ff32: { id: 2, type: array, items: { type: fp32, count: 5 } }
      ff64: { id: 3, type: array, items: { type: fp64, count: 5 } }
      fb:   { id: 4, type: array, items: { type: boolean, count: 5 } }
      fe:   { id: 5, type: array, items: { type: enum, count: 5, enum: { $ref: "#/$defs/enum/Color" } } }
      fbf:  { id: 6, type: array, items: { type: bitfield, count: 5, bits: { $ref: "#/$defs/bitfield/Flags" } } }
      du:   { id: 7, type: array, items: { type: u32 } }
      df32: { id: 8, type: array, items: { type: fp32 } }
      db:   { id: 9, type: array, items: { type: boolean } }
      ds:   { id: 10, type: array, items: { type: string } }
      mat:  { id: 11, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	sbuf := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/Sbuf.java"]

	for _, want := range []string{
		// --- encode: a fixed-count native array trims its trailing default run.
		"os.writeArrayUnsigned(0, Sbuf.trimTail(this.fu));",
		"os.writeArraySigned(1, Sbuf.trimTail(this.fi));",
		"os.writeArrayFp32(2, Sbuf.trimTailF32(this.ff32));", // bit-pattern compare: -0.0 must NOT trim
		"os.writeArrayFp64(3, Sbuf.trimTailF64(this.ff64));", // bit-pattern compare: -0.0 must NOT trim
		"os.writeArrayUnsigned(4, Sbuf.trimTail(Sbuf.boolToLongArray(this.fb)));",
		"os.writeArraySigned(5, Sbuf.trimTail(this.fe));",    // enum -> signed
		"os.writeArrayUnsigned(6, Sbuf.trimTail(this.fbf));", // bitfield -> unsigned

		// --- decode: materialize exactly N, defaults at [M, N).
		"acap = 5; m.fu = new long[5]",
		"acap = 5; m.ff32 = new float[5]",
		"acap = 5; m.ff64 = new double[5]",
		"Sbuf.fillFalse(m.fb, 5)",
		"m.fb.set(ai++, value != 0);",

		// --- the over-count guard (#100) still rejects M > N.
		`if (count > 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "fu: array count above schema capacity 5"));`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	for _, unwanted := range []string{
		// --- encode: a DYNAMIC array is never trimmed (trailing default is significant).
		"Sbuf.trimTail(this.du)",
		"Sbuf.trimTailF32(this.df32)",
		"Sbuf.trimTail(Sbuf.boolToLongArray(this.db))",
		// --- decode: a dynamic array keeps the lazy reservation and .add/.clear.
		"acap = 5; m.du =",
		"Sbuf.fillFalse(m.db",
		"m.db.set(ai++",
	} {
		if strings.Contains(m, unwanted) {
			t.Errorf("M.java must not contain %q (dynamic arrays are unchanged)", unwanted)
		}
	}

	// Dynamic arrays keep their original emission verbatim.
	for _, want := range []string{
		"os.writeArrayUnsigned(7, this.du);",
		"os.writeArrayFp32(8, this.df32);",
		"os.writeArrayUnsigned(9, Sbuf.boolToLongArray(this.db));",
		"m.du = new long[Math.min(count, ARRAY_INIT_CAP)]",
		"m.db.clear()",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing (dynamic, unchanged) %q", want)
		}
	}

	// A nested array-of-array row is NOT a top-level fixed field: rows stay
	// untrimmed even though both the outer and inner carry a `count`.
	if strings.Contains(m, "Sbuf.trimTail(Sbuf.toLongArray(") {
		t.Error("nested array rows must not be trimmed")
	}

	// The trim helpers compare by BIT PATTERN, never by == (-0.0 == 0.0 in Java).
	for _, want := range []string{
		"Float.floatToRawIntBits(a[n - 1]) == 0",
		"Double.doubleToRawLongBits(a[n - 1]) == 0L",
		"static void fillFalse(List<Boolean> l, int n)",
	} {
		if !strings.Contains(sbuf, want) {
			t.Errorf("Sbuf.java missing %q", want)
		}
	}
}

// A `count: N` array is fixed-length, so its VALUE is always exactly N elements —
// including before anything touches the wire (MESSAGE_SPEC §3, finding F-0010).
// With no schema default that is N element defaults; with a short schema default
// the unlisted trailing elements are the element default. An all-default array is
// omitted entirely by the sparse rule, so it never reaches arrayBegin on decode:
// without an N-element constructor default it would decode back as length 0 here
// while the fixed-storage backends yield N zeros. Dynamic arrays have no N and
// keep the shared zero-length default.
func TestJavaFixedCountDefaultShape(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Color: { RED: 0, GREEN: 1 }
  bitfield:
    Flags:
      a: { pos: 0 }
      b: { pos: 1 }
messages:
  m:
    payload:
      # count: N, NO schema default -> N element defaults.
      fu:   { id: 0, type: array, items: { type: u32, count: 5 } }
      ff32: { id: 1, type: array, items: { type: fp32, count: 4 } }
      ff64: { id: 2, type: array, items: { type: fp64, count: 2 } }
      fb:   { id: 3, type: array, items: { type: boolean, count: 3 } }
      fe:   { id: 4, type: array, items: { type: enum, count: 3, enum: { $ref: "#/$defs/enum/Color" } } }
      fbf:  { id: 5, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Flags" } } }
      # count: N with a SHORT schema default -> tail-padded to N.
      pu:   { id: 6, type: array, items: { type: u32, count: 4 }, default: [1, 2] }
      pb:   { id: 7, type: array, items: { type: boolean, count: 4 }, default: [true, true] }
      pf32: { id: 8, type: array, items: { type: fp32, count: 3 }, default: [1.5] }
      # dynamic -> unchanged, shared zero-length default.
      du:   { id: 9, type: array, items: { type: u32 } }
      df32: { id: 10, type: array, items: { type: fp32 } }
      db:   { id: 11, type: array, items: { type: boolean } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]

	for _, want := range []string{
		// --- fixed, no schema default: exactly N element defaults.
		"public long[] fu = new long[5];",
		"public float[] ff32 = new float[4];",
		"public double[] ff64 = new double[2];",
		"public List<Boolean> fb = new ArrayList<>(List.of(false, false, false));",
		"public long[] fe = new long[3];",  // enum -> long[]
		"public long[] fbf = new long[2];", // bitfield -> long[]

		// --- fixed, short schema default: tail-padded to N.
		"public long[] pu = new long[]{1L, 2L, 0, 0};",
		"public List<Boolean> pb = new ArrayList<>(List.of(true, true, false, false));",
		"public float[] pf32 = new float[]{1.5f, 0.0f, 0.0f};",

		// --- dynamic: shared zero-length default, unchanged.
		"public long[] du = Sbuf.EMPTY_LONGS;",
		"public float[] df32 = Sbuf.EMPTY_FLOATS;",
		"public List<Boolean> db = new ArrayList<>();",

		// --- the synthesized default doubles as the whole-field omission guard, so
		// an all-default fixed array is omitted entirely (encodes to no bytes). The
		// default is hoisted to a static (#146) so the guard allocates nothing.
		"private static final long[] _arrdef_fu = new long[5];",
		"if (!java.util.Arrays.equals(this.fu, _arrdef_fu)) {",
		"private static final List<Boolean> _arrdef_fb = List.of(false, false, false);",
		"if (!_arrdef_fb.equals(this.fb)) {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	// A dynamic array must never gain a synthesized N-element default nor an
	// Arrays.equals omission guard (it has no N to refill from).
	for _, unwanted := range []string{
		"public long[] du = new long[",
		"public List<Boolean> db = new ArrayList<>(List.of(",
		"java.util.Arrays.equals(this.du",
		// #146: the omit guard must not allocate a throwaway array per encode --
		// no `new T[...]` literal inside an Arrays.equals / List.of compare.
		"Arrays.equals(this.fu, new long[",
		"List.of(false, false, false).equals(this.fb)",
	} {
		if strings.Contains(m, unwanted) {
			t.Errorf("M.java must not contain %q (dynamic arrays keep the empty default)", unwanted)
		}
	}

	// Dynamic arrays keep the plain emptiness guard.
	if !strings.Contains(m, "if (this.du != null && this.du.length != 0) {") {
		t.Error("M.java: dynamic array must keep its length!=0 omission guard")
	}
}

// TestJavaLazySequenceFraming: MESSAGE_SPEC §2 omits a sequence-typed FIELD whose
// value equals its declared default instead of framing it empty, while a
// wrapper-array ELEMENT keeps its frame — element presence is what carries a
// dynamic array's length (§5.1). Every sequence is therefore opened with the
// corelib's hold-back begin (writeSequenceBeginLazy); the CLOSER is what encodes
// the distinction and is chosen statically from the position in the schema:
// writeSequenceEnd (drops a contentless frame) for a struct/union field and for a
// wrapper-array field, writeSequenceEndKeep (forces it out) for an element and for
// a nested array row.
func TestJavaLazySequenceFraming(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      st:   { id: 0, type: struct, fields: { x: { id: 0, type: i32 } } }
      strs: { id: 1, type: array, items: { type: string, maxlen: 8 } }
      blbs: { id: 2, type: array, items: { type: blob, maxlen: 8 } }
      objs: { id: 3, type: array, items: { type: struct, fields: { y: { id: 0, type: i32 } } } }
      mat:  { id: 4, type: array, items: { type: array, items: { type: string, maxlen: 8 } } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]

	for _, want := range []string{
		// A struct FIELD: opened lazily, closed with the dropping end, so an
		// all-default nested object vanishes instead of becoming an empty wrapper.
		"os.writeSequenceBeginLazy(0); (this.st == null ? new MSt() : this.st).marshal(os); os.writeSequenceEnd();",
		// A wrapper-array FIELD (string/blob elements): same -- depth 0 drops.
		"os.writeSequenceBeginLazy(1);",
		"os.writeSequenceBeginLazy(2);",
		// A struct ELEMENT keeps its frame: its id counts toward the array length.
		"os.writeSequenceBeginLazy(3);",
		"os.writeSequenceBeginLazy(_i0); (this.objs.get(_i0) == null ? new MObjsElem() : this.objs.get(_i0)).marshal(os); os.writeSequenceEndKeep();",
		// A nested array ROW is an element too: the inner row keeps, the outer
		// wrapper field drops.
		"os.writeSequenceBeginLazy(4);",
		"            os.writeSequenceEndKeep();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	// The eager begin is gone from the corelib; emitting it would not compile.
	if strings.Contains(m, "os.writeSequenceBegin(") {
		t.Error("M.java: every sequence must be opened with writeSequenceBeginLazy")
	}
	// Two keeping closes (the objs struct element, the mat row) and five dropping
	// ones (st, strs, blbs, the objs wrapper, the mat wrapper) -- one per
	// sequence-typed FIELD.
	if got := strings.Count(m, "writeSequenceEndKeep()"); got != 2 {
		t.Errorf("expected 2 keeping closes (struct element + nested row), got %d", got)
	}
	if got := strings.Count(m, "os.writeSequenceEnd();"); got != 5 {
		t.Errorf("expected 5 dropping closes (one per sequence-typed field), got %d", got)
	}
	// A wrapper array carries no whole-omission guard in generated code: the frame
	// is opened lazily and the corelib drops it when no element was written.
	if strings.Contains(m, "if (this.strs !=") || strings.Contains(m, "if (this.objs !=") {
		t.Error("M.java: a wrapper array must not carry a whole-omission guard")
	}
}

// TestJavaResetForReuse: MESSAGE_SPEC §2 made ABSENCE the encoding of an
// all-default field, and an absent field fires no callback — so a destination
// supplied by the caller must be re-armed before the feed, not from
// sequenceBegin/arrayBegin. Every class gets a public reset() that restores its
// declared defaults IN PLACE, and tryDecode calls it first. Without this a reused
// destination keeps the previous decode's array elements: data that is not in the
// message.
func TestJavaResetForReuse(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      lead: { id: 0, type: u32, default: 3 }
      name: { id: 1, type: string, maxlen: 8, default: "dflt" }
      strs: { id: 2, type: array, items: { type: string, maxlen: 8 } }
      dyn:  { id: 3, type: array, items: { type: u32 } }
      fixd: { id: 4, type: array, items: { type: u32, count: 3 }, default: [7, 8, 9] }
      bools: { id: 5, type: array, items: { type: boolean, count: 2 }, default: [true, false] }
      st:   { id: 6, type: struct, fields: { inner: { id: 0, type: array, items: { type: string, maxlen: 8 } } } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// Public, so a caller driving the Visitor by hand can re-arm too.
		"    public void reset() {",
		// Scalars and strings go back to the declared default.
		"        this.lead = 3L;",
		`        this.name = "dflt";`,
		// Containers are emptied in place — the point of taking a destination.
		"        this.strs = Sbuf.resetList(this.strs);",
		"        this.dyn = Sbuf.EMPTY_LONGS;",
		// A fixed-count array is refilled from the shared default without allocating.
		"        if (this.fixd != null && this.fixd.length == _arrdef_fixd.length) System.arraycopy(_arrdef_fixd, 0, this.fixd, 0, _arrdef_fixd.length);",
		"        else this.fixd = _arrdef_fixd.clone();",
		"        this.bools = Sbuf.resetList(this.bools);\n        this.bools.addAll(_arrdef_bools);",
		// A nested object recurses instead of being re-allocated.
		"        if (this.st == null) this.st = new MSt(); else this.st.reset();",
		"        this.inner = Sbuf.resetList(this.inner);",
		// The reuse entry point re-arms before feeding.
		"    public static DecodeStatus tryDecode(byte[] data, M out) throws SofabException {\n        out.reset();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}
	// Every emitted class (M plus its one struct) carries a reset().
	if got := strings.Count(m, "public void reset() {"); got != 2 {
		t.Errorf("expected one reset() per class (2), got %d", got)
	}
	// decode(byte[]) builds a fresh instance, so it must not pay for a reset.
	if strings.Contains(m, "M m = new M();\n        m.reset();") {
		t.Error("decode(byte[]) constructs a fresh instance and must not call reset()")
	}
	// §7.4 is unchanged: a re-opened wrapper still replaces the array whole.
	if !strings.Contains(m, "case 2: m.strs.clear(); cur = 1; break;") {
		t.Error("M.java: the §7.4 sequence-start clear must stay")
	}
}

// TestJavaSbufResetList pins the in-place list reset the generated reset() leans
// on: clearing keeps the backing capacity, so re-arming a reused destination
// allocates nothing.
func TestJavaSbufResetList(t *testing.T) {
	s := string(genJavaFromYAML(t, "version: 1\nmessages:\n  M:\n    payload:\n      a: { id: 0, type: u32 }\n",
		map[string]any{})["src/main/java/message/Sbuf.java"])
	if !strings.Contains(s, "static <T> List<T> resetList(List<T> l) { if (l == null) return new java.util.ArrayList<>(); l.clear(); return l; }") {
		t.Error("Sbuf.java missing the in-place resetList helper")
	}
}

// wrapperArraySrc is the schema both wrapper-array regression tests below run
// against: a count:N struct array, a count-less one of the same element shape as
// the control, and a count:N string array for the leaf-element path.
const wrapperArraySrc = `
version: 1
messages:
  Vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`

// A count:N wrapper array's canonical wire stops at M -- one past its last
// non-default element (MESSAGE_SPEC §3/§5.1, "even for sequence-form elements")
// -- and M == 0 leaves the whole wrapper omitted (§2). generator#248: the element
// loop used to run to size(), framing every trailing all-default element, so a
// decoder that accepted the non-canonical form re-encoded it unchanged instead of
// normalising. A DYNAMIC array has no N to refill from, so its trailing default
// element is significant and must still be framed.
func TestJavaFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	files := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})
	got := files["src/main/java/message/Vec.java"]

	// The fixed array narrows to M before framing anything...
	if !strings.Contains(got, "List<VecFixedElem> _t0 = Sbuf.trimTailObjs(this.fixed, VecFixedElem::isDefault);") ||
		!strings.Contains(got, "for (int _i0 = 0; _i0 < _t0.size(); _i0++) { os.writeSequenceBeginLazy(_i0);") {
		t.Errorf("count:N struct array must loop to M, not size():\n%s", got)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(got, "for (int _i0 = 0; _i0 < this.dynamic.size(); _i0++) { os.writeSequenceBeginLazy(_i0);") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", got)
	}
	// An interior all-default element is still framed: only the TRAILING run goes.
	if !strings.Contains(got, ".marshal(os); os.writeSequenceEndKeep(); }") {
		t.Errorf("interior elements must keep the framing closer:\n%s", got)
	}
	// The leaf string array shares the mechanism.
	if !strings.Contains(got, "List<String> _t1 = Sbuf.trimTailStrings(this.fstrs);") {
		t.Errorf("count:N string array must loop over the trimmed run:\n%s", got)
	}

	// isDefault is the exact mirror of what marshal writes, so it must narrow a
	// field exactly when the marshal loop does -- disagreeing would either omit a
	// field that is on the wire or keep one that is not.
	if !strings.Contains(got, "if (!Sbuf.trimTailObjs(this.fixed, VecFixedElem::isDefault).isEmpty()) return false;") {
		t.Errorf("isDefault must narrow the fixed array like marshal does:\n%s", got)
	}
	if !strings.Contains(got, "if (!this.dynamic.isEmpty()) return false;") {
		t.Errorf("isDefault must NOT narrow the dynamic array:\n%s", got)
	}
	if !strings.Contains(got, "if (!Sbuf.trimTailStrings(this.fstrs).isEmpty()) return false;") {
		t.Errorf("isDefault for a string wrapper array must test the trimmed run:\n%s", got)
	}

	// The narrowing helpers themselves, and the fact that they return a VIEW.
	sbuf := files["src/main/java/message/Sbuf.java"]
	for _, want := range []string{
		"static <T> List<T> trimTailObjs(List<T> a, java.util.function.Predicate<? super T> isDefault) {",
		"static List<String> trimTailStrings(List<String> a) {",
		"return n == a.size() ? a : a.subList(0, n);",
	} {
		if !strings.Contains(sbuf, want) {
			t.Errorf("Sbuf.java missing %q:\n%s", want, sbuf)
		}
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at dest[id] after gap-filling -- never appended. Appending
// shortened the array by the size of any interior id gap and decoded a REOPENED
// id as a second element instead of merging into the first (§7.4). The leaf
// string/blob element paths next to it always got this right.
//
// The N-fill on sequenceEnd is what makes the §3/§5.1 trailing elision lossless:
// without it, re-encoding a decoded fixed array shortens it on every round trip.
func TestJavaWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
	got := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})["src/main/java/message/Vec.java"]

	for _, want := range []string{
		// placement, not append -- and the gap-fill that precedes it
		"while (m.fixed.size() <= id) m.fixed.add(new VecFixedElem()); _ex_Root_fixed = id;",
		// a child field of the element resolves through the PLACED index, not
		// through the last-appended element
		"case 0: m.fixed.get(_ex_Root_fixed).k = value; break;",
		// N-fill when the sequence scope closes
		"case 1: while (m.fixed.size() < 5) m.fixed.add(new VecFixedElem()); break;",
		"case 5: while (m.fstrs.size() < 3) m.fstrs.add(\"\"); break;",
		// the over-index guard still bounds both the placement and the gap-fill
		`if (id >= 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_fixed element: array index above schema capacity 5"));`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Vec.java missing %q:\n%s", want, got)
		}
	}

	// The defect this replaced: appending ignored the id entirely.
	if strings.Contains(got, "m.fixed.add(new VecFixedElem()); cur =") {
		t.Errorf("struct-array elements must not be appended id-blind:\n%s", got)
	}
	if strings.Contains(got, "m.fixed.get(m.fixed.size()-1)") {
		t.Errorf("element child fields must not resolve through the last-appended element:\n%s", got)
	}
	// A DYNAMIC array is placed by id too, but its length is highest-id + 1, so it
	// is never filled to any N.
	if !strings.Contains(got, "while (m.dynamic.size() <= id) m.dynamic.add(new VecDynamicElem()); _ex_Root_dynamic = id;") {
		t.Errorf("dynamic struct array must also place by id:\n%s", got)
	}
	// A fill arm ends right after the add (`... .add(new X()); break;`), where the
	// placement arm above continues into `_ex_... = id; cur = ...`. Neither the
	// dynamic struct array nor any other count-less wrapper may get one.
	if strings.Contains(got, "m.dynamic.add(new VecDynamicElem()); break;") {
		t.Errorf("a dynamic array has no N and must never be default-filled:\n%s", got)
	}
}

// TestJavaFixedCountWrapperArrayMaterialized: MESSAGE_SPEC §5.1 makes a
// `count: N` array's value N elements long whether or not the field ever reaches
// the wire -- the length "is N for every target". The NATIVE fixed-count arrays
// have always been materialized at construction from their padded default
// literal (`new long[3]`, `new ArrayList<>(List.of(false, false))`); the WRAPPER
// ones were left as the empty List, so the same schema disagreed with itself:
//
//	absent field             -> length 0     (wrong)
//	one element on the wire  -> length N     (right, sequenceEnd refills)
//	explicitly-empty wrapper -> length N     (right)
//
// sequenceEnd can only refill a sequence that was actually OPENED, so the absent
// case has to be covered where the native arrays already cover it: at
// construction, and in the reset() that re-arms a caller-supplied destination.
// A DYNAMIC (count-less) array has no N -- its length is highest-present-id + 1
// -- and must stay empty.
func TestJavaFixedCountWrapperArrayMaterialized(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      strs:  { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }
      nums:  { id: 1, type: array, items: { type: u32, count: 3 } }
      blobs: { id: 2, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      objs:  { id: 3, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }
      dyn:   { id: 4, type: array, items: { type: string, maxlen: 8 } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// Construction: the wrapper arrays are materialized right where the native
		// one next to them is.
		"public List<String> strs = _seqdef_strs(new ArrayList<>(3));",
		"public long[] nums = new long[3];",
		"public List<byte[]> blobs = _seqdef_blobs(new ArrayList<>(2));",
		"public List<MObjsElem> objs = _seqdef_objs(new ArrayList<>(2));",
		// The filler adds N ELEMENT defaults -- the same values the collectors
		// gap-fill an id hole with.
		"        for (int i = 0; i < 3; i++) l.add(\"\");",
		"        for (int i = 0; i < 2; i++) l.add(new byte[0]);",
		"        for (int i = 0; i < 2; i++) l.add(new MObjsElem());",
		// reset() re-arms to the same value, in place.
		"        this.strs = Sbuf.resetList(this.strs);\n        _seqdef_strs(this.strs);",
		"        this.objs = Sbuf.resetList(this.objs);\n        _seqdef_objs(this.objs);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q:\n%s", want, m)
		}
	}
	// A dynamic wrapper array has no N: it starts empty and reset() leaves it empty.
	if !strings.Contains(m, "public List<String> dyn = new ArrayList<>();") {
		t.Errorf("a count-less wrapper array must start empty:\n%s", m)
	}
	if strings.Contains(m, "_seqdef_dyn") {
		t.Errorf("a count-less wrapper array must never be default-filled:\n%s", m)
	}
	// Materializing the elements must not make the field encode: an all-default
	// wrapper array is still dropped whole (§2), which is what keeps the empty
	// message empty.
	if !strings.Contains(m, "if (!Sbuf.trimTailStrings(this.strs).isEmpty()) return false;") ||
		strings.Contains(m, "if (this.strs != null && !this.strs.isEmpty())") {
		t.Errorf("a materialized wrapper array must not gain a whole-omission guard:\n%s", m)
	}
}

// The last element of a DYNAMIC wrapper array is always written, whatever its
// value (MESSAGE_SPEC §2, tightened by documentation#29). Such an array recovers
// its length as highest-present-id + 1 (§5.1), so the element at the highest
// index is the only one whose PRESENCE carries the length: dropping a trailing
// default leaf encoded ["a", ""] exactly like ["a"] and decoded one element
// short, and ["", ""] encoded to nothing and decoded as []. Sequence-form
// elements never had the problem -- they are framed unconditionally by the
// keeping closer -- so this holds both element kinds to one standard. A count:N
// array is exempt: its length is N whatever the wire carries, so it still elides
// the whole trailing run.
func TestJavaDynamicArrayAlwaysWritesLastElement(t *testing.T) {
	got := genJavaFromYAML(t, `
version: 1
messages:
  Vec:
    payload:
      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`, map[string]any{})["src/main/java/message/Vec.java"]

	for _, want := range []string{
		// Dynamic: the last index escapes the omit test. A null element is the
		// element default, so it is normalized before the guard can write it.
		`List<String> _t0 = Sbuf.orEmpty(this.dynstr);`,
		`String _e0 = _t0.get(_i0); if (_e0 == null) _e0 = ""; if (!_e0.isEmpty() || _i0 == _t0.size() - 1) os.writeString(_i0, _e0);`,
		`List<byte[]> _t1 = Sbuf.orEmpty(this.dynblob);`,
		`byte[] _e0 = _t1.get(_i0); if (_e0 == null) _e0 = Sbuf.EMPTY_BYTES; if (_e0.length != 0 || _i0 == _t1.size() - 1) os.writeBlob(_i0, _e0);`,
		// Fixed: no guard -- the trailing run still collapses and the decoder
		// refills to N.
		`List<String> _t2 = Sbuf.trimTailStrings(this.fixedstr);`,
		`String _e0 = _t2.get(_i0); if (_e0 == null) _e0 = ""; if (!_e0.isEmpty()) os.writeString(_i0, _e0);`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Vec.java missing %q:\n%s", want, got)
		}
	}
	// The all-default predicate has to follow the writer: a dynamic [""] now puts
	// an element on the wire, so the field is NOT default and must not be omitted.
	// Trimming it here would drop a field the marshal loop writes.
	for _, want := range []string{
		"if (!Sbuf.orEmpty(this.dynstr).isEmpty()) return false;",
		"if (!Sbuf.orEmpty(this.dynblob).isEmpty()) return false;",
		// The fixed one keeps its trim on both sides.
		"if (!Sbuf.trimTailStrings(this.fixedstr).isEmpty()) return false;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "trimTailStrings(this.dynstr)") || strings.Contains(got, "trimTailBlobs(this.dynblob)") {
		t.Errorf("a dynamic leaf array must never be trimmed:\n%s", got)
	}
}
