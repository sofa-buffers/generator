package java

import (
	"os"
	"sort"
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
		"public void serialize(OStream os) throws IOException",
		"public byte[] encode()",
		"public static Myfirstmessage decode(byte[] data)",
		"public static DecodeStatus tryDecode(byte[] data, Myfirstmessage out) throws SofabException", // status-surfacing decode (#105)
		"class MyfirstmessageVisitor implements Visitor {",
		"public void sequenceBegin(int id)", // flat-visitor nesting
		"public long someu64 = Long.parseUnsignedLong(\"18446744073709551615\");",
		"public int[] someuintarray = new int[]{0, 1, 1000, -1};",                                          // primitive array (was List<Long>)
		"public float[] somefloatarray = new float[]{0.0f, -1.5f, 3.25f};",                                 // primitive fp array
		"public long[] someenumarray = new long[]{2L, 1L, 0L};",                                            // declared default, NOT padded to count (count is a capacity)
		"os.writeArrayUnsigned(15, this.someuintarray);",                                                   // direct write, no box, no trim: the wire count IS the length
		"private static final int[] _arrdef_someuintarray = new int[]{0, 1, 1000, -1};",                    // omit-default hoisted to a static (#146)
		"if (!java.util.Arrays.equals(this.someuintarray, _arrdef_someuintarray)) {",                       // guard reads the static -- no per-encode new long[] (#146)
		"m.someuintarray[ai++] = (int) value;", // plain indexed store: arrayBegin sized the array at the checked count (§9.5 shape A)
		"case 15: if (kind != ArrayKind.UNSIGNED) break; if (count > 4) throw Sofab.invalid(\"someuintarray: array count above schema capacity 4\"); askip = 0; afill = count; atgt = 1; abulk = m.someuintarray = new int[count]; break;", // mis-typed header skipped before the bound (#254); over-count rejected (#100); the M that arrived is the whole value
		"OStream os = OStream.overScratch(MAX_SIZE);",                            // the corelib owns the scratch buffer; MAX_SIZE stays ours (§5.1)
		"return os.copyOfBytesUsed();",                                           // exact-size copy out of it
		"String _s = acc.string(total, offset, data, chunkOffset, chunkLength);", // reassembly + UTF-8, both the corelib's
		"private final PayloadAcc acc = new PayloadAcc();",
		"public List<Boolean> someboolarray", // boolean array stays boxed List
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Myfirstmessage.java missing %q", want)
		}
	}
	// The support layer is corelib-java's (generator#345 / corelib-java#97): none
	// of it may still be emitted, and no reference to a generated copy may remain.
	for _, gone := range []string{
		"class Sbuf", "Sbuf.",
		"private static long[] ensureCap", "private static float[] ensureCap",
		"private static String _utf8", "_utf8(",
		"ENC_BUF", "ThreadLocal",
		"ByteArrayOutputStream",
		"private static final int ARRAY_INIT_CAP",
	} {
		if strings.Contains(m, gone) {
			t.Errorf("Myfirstmessage.java must not still emit %q", gone)
		}
	}
	// The nested types are their own public classes in their own files now
	// (generator#305), so the message file must NOT declare them.
	if strings.Contains(m, "class MyfirstmessageSomestructNestedstruct {") {
		t.Error("a schema type must not be declared inside the message's file")
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
		`if (id >= 4) throw Sofab.invalid("Root_bs element: array index above schema capacity 4"); while (m.bs.size() <= id)`,
		`if (id >= 3) throw Sofab.invalid("Root_bb element: array index above schema capacity 3"); while (m.bb.size() <= id)`,
		// The struct-element arm gap-fills and PLACES by id (generator#247), so the
		// guard is now followed by the same grow-to-id loop the leaf arms use.
		`if (id >= 2) throw Sofab.invalid("Root_bp element: array index above schema capacity 2"); while (m.bp.size() <= id) m.bp.add(new`,
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
		// Unbounded array: the cap bounds the count, and the destination is then
		// allocated at exactly that count -- the check is what makes the exact
		// allocation safe (§9.5 shape A), and it is bulk-capable for the same reason.
		`case 1: if (kind != ArrayKind.UNSIGNED) break; if (count > MAX_DYN_ARRAY_COUNT) throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "arr: array count above configured limit 4")); askip = 0; afill = count; atgt = 1; abulk = m.arr = new long[count]; break;`,
		// Bounded array: only the generator#100 schema guard, never the cap. Both
		// bounds sit BEHIND the §7.3 kind test (generator#254).
		`case 2: if (kind != ArrayKind.SIGNED) break; if (count > 6) throw Sofab.invalid("barr: array count above schema capacity 6"); askip = 0; afill = count; atgt = 1; abulk = m.barr = new int[count]; break;`,
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

	// No keys configured -> the target's finite DEFAULTS, not "unlimited"
	// (§9.5, generator#385). Java is on the server tier.
	plain := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/Dyn.java"]
	for _, want := range []string{
		"static final long MAX_DYN_ARRAY_COUNT = 65536L;",
		"static final long MAX_DYN_STRING_LEN = 1048576L;",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("default limits missing %q", want)
		}
	}
	// Liveness is still a property of the schema, not of the configuration.
	if strings.Contains(plain, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
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
		`case 0: if (total > 8) throw Sofab.invalid("s: string length above schema maxlen 8"); break;`,
		// Bounded scalar blob: reject total > maxlen at the top of blob().
		`case 1: if (total > 8) throw Sofab.invalid("b: blob length above schema maxlen 8"); break;`,
		// Bounded wrapper string element: reject total > element maxlen.
		`if (total > 5) throw Sofab.invalid("arr element: string length above schema maxlen 5"); break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing maxlen reject %q", want)
		}
	}
	// The unbounded string `u` (id 2) gets no maxlen guard.
	if strings.Contains(m, `"u: string length above schema maxlen`) {
		t.Error("unbounded string must not carry a maxlen guard")
	}
	// The finite default cap (§9.5, generator#385) covers the unbounded string
	// `u`, alongside (never instead of) the schema maxlen guards.
	if !strings.Contains(m, "static final long MAX_DYN_STRING_LEN = 1048576L;") {
		t.Error("M.java missing the default string cap")
	}
	// The other two stay inert: liveness is a property of the schema, not of the
	// configuration. There is no unbounded blob, and `arr` is a WRAPPER array,
	// which carries no count header for arrayBegin to check (its element index is
	// what would have to be capped -- generator#387).
	for _, gone := range []string{"MAX_DYN_BLOB_LEN", "MAX_DYN_ARRAY_COUNT"} {
		if strings.Contains(m, gone) {
			t.Errorf("inert limit %s must not be emitted", gone)
		}
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
// under UNSIGNED/SIGNED, fp32 arrays under FP32 and fp64 arrays under FP64
// (generator#259: the fixlen kinds name their element subtype).
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
		// SKIPPING IS THE DEFAULT: arrayBegin arms the counter for every array it
		// is handed, and only a declared array at a matching kind disarms it. (It
		// used to be armed by a four-way `kind ==` chain ahead of a second, separate
		// (cur, id) walk; one switch does both.)
		"        askip = count;\n        afill = 0;\n        abulk = null;",
		// Each declared array disarms the skip AND arms the fill behind ITS OWN
		// kind test: the u32 array (id 2) under UNSIGNED, the i32 array (id 3)
		// under SIGNED, the fp32 array (id 4) under FP32. A header of any other
		// kind at that id falls out of the arm before the disarm.
		"case 2: if (kind != ArrayKind.UNSIGNED) break; if (count > 4) throw",
		"case 2: if (kind != ArrayKind.UNSIGNED) break; if (count > 4) throw Sofab.invalid(\"ua: array count above schema capacity 4\"); askip = 0; afill = count;",
		"case 3: if (kind != ArrayKind.SIGNED) break; if (count > 4) throw Sofab.invalid(\"ia: array count above schema capacity 4\"); askip = 0; afill = count;",
		"case 4: if (kind != ArrayKind.FP32) break; if (count > 3) throw Sofab.invalid(\"fa: array count above schema capacity 3\"); askip = 0; afill = count;",
		// Discarded at the top of every callback an array shares with a scalar,
		// behind the armed-fill arm (an armed fill and an armed skip are mutually
		// exclusive: arrayBegin sets exactly one).
		"    public void unsigned(int id, long value) {\n        // An element of the array arrayBegin armed",
		"    public void signed(int id, long value) {\n        // An element of the array arrayBegin armed",
		"    public void fp32(int id, float value) {\n        // An element of the array arrayBegin armed",
		"    public void fp64(int id, double value) {\n        // Drop an element of an array",
		"        if (askip > 0) { askip--; return; }",
		// The mirror guard (generator#188): a fill runs only while armed, and the
		// element count is what terminates it.
		"        if (afill != 0) {\n            afill--;\n            switch (atgt) {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing §7.3 array-skip guard %q", want)
		}
	}
	// This schema declares no fp64 array, so fp64() has no armed-fill arm at all
	// and an fp64 header at ANY id discards its elements.
	if strings.Contains(m, "    public void fp64(int id, double value) {\n        if (afill != 0)") {
		t.Error("fp64 has no declared array here; it must have no armed-fill arm")
	}
	// The fp32 array is armed behind an FP32 test, never grouped with the integer
	// arms — id 4 must not disarm under UNSIGNED/SIGNED.
	if strings.Contains(m, "case 2: case 3: case 4: askip = 0") {
		t.Error("an fp32 array must be armed under FP32, not the integer arm")
	}
	// The collapsed fixlen kind is gone from the ABI (generator#259): naming it
	// would not compile against the corelib.
	if strings.Contains(m, "ArrayKind.FIXLEN") {
		t.Error("ArrayKind.FIXLEN no longer exists; arrays must be keyed by FP32/FP64 (generator#259)")
	}
	// The unsigned- and signed-array kinds are NOT one case: an unsigned-declared
	// and a signed-declared array id must never disarm each other (generator#254).
	if strings.Contains(m, "ArrayKind.UNSIGNED || kind == ArrayKind.SIGNED") {
		t.Error("UNSIGNED and SIGNED must be separate arms (generator#254)")
	}
	if strings.Contains(m, "case 2: case 3: askip = 0") {
		t.Error("a u32 array and an i32 array must not disarm the same arm (generator#254)")
	}
}

// TestJavaMistypedArrayNotAllocated: MESSAGE_SPEC §7.3 — "A decoder ... MUST NOT
// decode its payload into the declared field." A native array field whose header
// carries the WRONG array kind (an array-signed header at a u8[]-declared id) is
// skipped like an unknown id, and skipping includes NOT RESIZING the declared
// field from the skipped header's count: the leak that generator#254 pins is the
// LENGTH, not the element — java re-encoded `a6 06 04 01 06 07` as
// `a6 06 03 01 00 07`, a one-element unsigned array the wire never carried.
//
// Two halves, both asserted here:
//  1. the skip arm arms the discard counter per array kind, so a mis-typed header
//     no longer disarms it (covered by the case-per-kind assertions below);
//  2. every arrayBegin allocation arm is fronted by the kind test — and the test
//     comes BEFORE the schema bound, so an over-count MIS-TYPED array is skipped
//     rather than rejected as a false INVALID (§7.3: "the schema bound applied
//     only to a field that survives it").
func TestJavaMistypedArrayNotAllocated(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    E: { A: 0, B: 1 }
messages:
  M:
    payload:
      ua: { id: 0, type: array, items: { type: u8, count: 5 } }
      ia: { id: 1, type: array, items: { type: i8, count: 5 } }
      fa: { id: 2, type: array, items: { type: fp32, count: 3 } }
      ba: { id: 3, type: array, items: { type: boolean, count: 2 } }
      ea: { id: 4, type: array, items: { type: enum, count: 2, enum: { $ref: "#/$defs/enum/E" } } }
      da: { id: 5, type: array, items: { type: u16 } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// The kind test fronts the disarm AND the allocation, and precedes the
		// schema bound. A bounded array reserves exactly `count` (the bound above
		// has just proved count <= N <= ARRAY_INIT_CAP); an unbounded one still
		// reserves the capped amount and grows, since its count is untrusted.
		`case 0: if (kind != ArrayKind.UNSIGNED) break; if (count > 5) throw Sofab.invalid("ua: array count above schema capacity 5"); askip = 0; afill = count; atgt = 1; abulk = m.ua = new byte[count]; break;`,
		`case 1: if (kind != ArrayKind.SIGNED) break; if (count > 5) throw Sofab.invalid("ia: array count above schema capacity 5"); askip = 0; afill = count; atgt = 1; abulk = m.ia = new byte[count]; break;`,
		`case 2: if (kind != ArrayKind.FP32) break; if (count > 3) throw Sofab.invalid("fa: array count above schema capacity 3"); askip = 0; afill = count; atgt = 1; m.fa = new float[count]; break;`,
		// A boolean array is a List: clearing it is decoding into it too, so the
		// kind test fronts the clear as well. boolean maps to the UNSIGNED kind.
		`case 3: if (kind != ArrayKind.UNSIGNED) break; if (count > 2) throw Sofab.invalid("ba: array count above schema capacity 2"); askip = 0; afill = count; atgt = 2; m.ba.clear(); break;`,
		// enum elements ride the SIGNED wire type.
		`case 4: if (kind != ArrayKind.SIGNED) break; if (count > 2) throw Sofab.invalid("ea: array count above schema capacity 2"); askip = 0; afill = count; atgt = 2; abulk = m.ea = new long[count]; break;`,
		// A count-less array has no schema bound, so the target's finite default
		// cap governs it (§9.5, generator#385) -- checked, like a schema bound,
		// BEHIND the kind test, and it is that check which lets the destination be
		// allocated at exactly the wire count (§9.5 shape A).
		`case 5: if (kind != ArrayKind.UNSIGNED) break; if (count > MAX_DYN_ARRAY_COUNT) throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "da: array count above configured limit 65536")); askip = 0; afill = count; atgt = 3; abulk = m.da = new short[count]; break;`,
		// Skipping is the default; only the arms above disarm it.
		"        askip = count;\n        afill = 0;\n        abulk = null;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing §7.3 mis-typed-array guard %q", want)
		}
	}
	// The bound must never precede the kind test: an over-count mis-typed array is
	// skipped, not a false INVALID.
	if strings.Contains(m, `case 0: if (count > 5)`) {
		t.Error("the schema bound must sit BEHIND the §7.3 kind test (generator#254)")
	}
}

// TestJavaFixlenArrayKindPerSubtype: generator#259 / Crucible F-0042. A fixlen
// array header carries a second word (the fixlen_word) naming its element
// subtype, and CORELIB_PLAN §4.8 fixes the decode order so the array is announced
// only AFTER that word has been read — count under the format ceiling first, then
// the subtype, then MESSAGE_SPEC §7.3, and only then a schema bound. corelib-java
// therefore dropped the collapsed ArrayKind.FIXLEN and reports FP32 / FP64.
//
// Codegen has to key the arms by subtype to match. Two things are pinned:
//  1. a declared fp32[N] appears ONLY under the FP32 arm and a declared fp64[N]
//     ONLY under FP64, so an fp64 header at the fp32 slot leaves the discard
//     counter armed (its elements are dropped) and never touches the field;
//  2. the schema `count > N` bound stays INSIDE the matched arm, behind the kind
//     test, so an over-count header of the OTHER subtype is skipped rather than
//     rejected as a false INVALID.
func TestJavaFixlenArrayKindPerSubtype(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      fa: { id: 0, type: array, items: { type: fp32, count: 3 } }\n" +
		"      da: { id: 1, type: array, items: { type: fp64, count: 4 } }\n" +
		"      ua: { id: 2, type: array, items: { type: u32, count: 5 } }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// One arm per subtype, each arming the discard counter for every id that
		// does not declare an array of exactly that subtype.
		"        askip = count;\n        afill = 0;\n        abulk = null;",
		// The kind test fronts the allocation and the schema bound sits behind it.
		`case 0: if (kind != ArrayKind.FP32) break; if (count > 3) throw Sofab.invalid("fa: array count above schema capacity 3"); askip = 0; afill = count; atgt = 1; m.fa = new float[count]; break;`,
		`case 1: if (kind != ArrayKind.FP64) break; if (count > 4) throw Sofab.invalid("da: array count above schema capacity 4"); askip = 0; afill = count; atgt = 1; m.da = new double[count]; break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing fixlen subtype arm %q", want)
		}
	}
	// The two fixlen ids must never share an arm: grouping them is exactly the bug
	// the collapsed FIXLEN kind caused — an fp64 header sizing a declared float[].
	if strings.Contains(m, "case 0: case 1: askip = 0") {
		t.Error("an fp32 and an fp64 array must not disarm the same arm (generator#259)")
	}
	if strings.Contains(m, "ArrayKind.FP32 || kind == ArrayKind.FP64") {
		t.Error("FP32 and FP64 must be separate arms (generator#259)")
	}
	// Nothing may still name the removed collapsed kind.
	if strings.Contains(m, "ArrayKind.FIXLEN") {
		t.Error("ArrayKind.FIXLEN was removed from the corelib ABI (generator#259)")
	}
	// The bound must not float ahead of the kind test on either fixlen path.
	if strings.Contains(m, `case 0: if (count > 3)`) || strings.Contains(m, `case 1: if (count > 4)`) {
		t.Error("the schema count bound must sit INSIDE the matched kind arm (generator#259)")
	}
}

// A `count: N` array is FIXED-LENGTH (MESSAGE_SPEC §3, finding F-0010): the
// encoder elides the trailing run of default elements and the decoder rebuilds
// it from the schema count, so the decoded value always has exactly N elements.
// A dynamic (count-less) array has no N to refill from — a trailing default
// element is significant there and must survive untouched.
// Strict UTF-8 (#85) is the corelib's: a `string` payload is materialized through
// PayloadAcc.string, which validates the reassembled bytes and then converts them,
// so the generated file carries no validator and no conversion of its own. The
// range bug this test was written for -- passing a LENGTH where Utf8.valid wants
// an exclusive end index, which made the scan a no-op for any field not first in
// the buffer -- is now unreachable from generated code, and corelib-java#97 owns
// the split-payload coverage that pins it.
func TestJavaStringGoesThroughTheCorelibAccumulator(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      s: { id: 0, type: string }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	if !strings.Contains(m, "String _s = acc.string(total, offset, data, chunkOffset, chunkLength);") {
		t.Error("a string payload must be materialized through PayloadAcc.string")
	}
	if !strings.Contains(m, "if (_s == null) return;") {
		t.Error("an incomplete payload must return, not fall through with a null value")
	}
	for _, gone := range []string{"Utf8.valid(", "new String(b, off, len", "_utf8"} {
		if strings.Contains(m, gone) {
			t.Errorf("generated code must not still carry its own UTF-8 path (%q)", gone)
		}
	}
}

// documentation#29: `count: N` is a CAPACITY, never a length. The wire count M IS
// a compact array's length, so nothing that carries it may be elided -- the
// trim-on-encode / fill-on-decode pair this backend shipped for a `count: N`
// native array was correct only under the superseded fixed-length reading and is
// gone. [1,2,0,0] and [1,2] are different values with different bytes, and a
// count:N array decodes to exactly the M elements that arrived.
func TestJavaCountIsCapacityNativeArrays(t *testing.T) {
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

	for _, want := range []string{
		// --- encode: every element the value holds is written, count or no count.
		"os.writeArrayUnsigned(0, this.fu);",
		"os.writeArraySigned(1, this.fi);",
		"os.writeArrayFp32(2, this.ff32);",
		"os.writeArrayFp64(3, this.ff64);",
		"os.writeArrayUnsigned(4, Seq.boolsToLongs(this.fb));",
		"os.writeArraySigned(5, this.fe);",    // enum -> signed
		"os.writeArrayUnsigned(6, this.fbf);", // bitfield -> unsigned
		// --- decode: a count:N array is filled exactly like a count-less one, from
		// the M elements that arrived; the schema count only bounds M.
		"abulk = m.fu = new int[count]",
		"m.ff32 = new float[count]",
		"m.ff64 = new double[count]",
		"m.fb.clear()",
		"m.fb.add(value != 0);",
		// --- the over-count guard (#100) still rejects M > N.
		`if (count > 5) throw Sofab.invalid("fu: array count above schema capacity 5");`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	for _, unwanted := range []string{
		// The whole trim-on-encode / fill-on-decode pair is gone.
		"trimTail", "fillFalse", "padTo",
		"acap = 5;", // no materialization at the schema count
		"m.fu = new long[5]", "m.ff32 = new float[5]", "m.ff64 = new double[5]",
		"m.fb.set(ai++", // a boolean array is grown, never overwritten by index
	} {
		if strings.Contains(m, unwanted) {
			t.Errorf("M.java must not contain %q (count is a capacity, not a length)", unwanted)
		}
	}

	// Dynamic arrays keep the same encode side, and decode into an exactly-sized
	// destination now that their count is checked against the cap first.
	for _, want := range []string{
		"os.writeArrayUnsigned(7, this.du);",
		"os.writeArrayFp32(8, this.df32);",
		"os.writeArrayUnsigned(9, Seq.boolsToLongs(this.db));",
		"abulk = m.du = new int[count]",
		"m.db.clear()",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing (dynamic, unchanged) %q", want)
		}
	}

	// A native ROW of a matrix carries no frame of its own, so the §2 element rule
	// lands on the write: an interior row equal to the element default (the empty
	// row) is not written at all, and the last row always is. A primitive row is a
	// primitive array, so "empty" is a length test and the row goes to the wire
	// unboxed.
	if !strings.Contains(m, "if (_e0.length != 0 || _i0 == _t1.size() - 1) {") {
		t.Errorf("a native matrix row must take the interior/last write guard:\n%s", m)
	}
}

// A `count: N` array's VALUE is bounded by N, never sized to it (MESSAGE_SPEC §3,
// documentation#29): a fresh count:N array is EMPTY, a declared default shorter
// than N stands exactly as written, and an all-zero N-element value is a length-N
// array that differs from the empty one and stays on the wire. Padding either side
// to N is what used to make [0,0,0,0] indistinguishable from "no value".
func TestJavaCountIsCapacityDefaultShape(t *testing.T) {
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
      # count: N, NO schema default -> the EMPTY array.
      fu:   { id: 0, type: array, items: { type: u32, count: 5 } }
      ff32: { id: 1, type: array, items: { type: fp32, count: 4 } }
      ff64: { id: 2, type: array, items: { type: fp64, count: 2 } }
      fb:   { id: 3, type: array, items: { type: boolean, count: 3 } }
      fe:   { id: 4, type: array, items: { type: enum, count: 3, enum: { $ref: "#/$defs/enum/Color" } } }
      fbf:  { id: 5, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Flags" } } }
      # count: N with a SHORT schema default -> exactly as written, not padded.
      pu:   { id: 6, type: array, items: { type: u32, count: 4 }, default: [1, 2] }
      pb:   { id: 7, type: array, items: { type: boolean, count: 4 }, default: [true, true] }
      pf32: { id: 8, type: array, items: { type: fp32, count: 3 }, default: [1.5] }
      # wrapper elements: a count:N one starts empty just like a count-less one.
      fstr: { id: 9, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fobj: { id: 10, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }
      # dynamic -> unchanged, shared zero-length default.
      du:   { id: 11, type: array, items: { type: u32 } }
      df32: { id: 12, type: array, items: { type: fp32 } }
      db:   { id: 13, type: array, items: { type: boolean } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]

	for _, want := range []string{
		// --- count:N, no schema default: the empty array, exactly like a dynamic one.
		"public int[] fu = Seq.EMPTY_INTS;",
		"public float[] ff32 = Seq.EMPTY_FLOATS;",
		"public double[] ff64 = Seq.EMPTY_DOUBLES;",
		"public List<Boolean> fb = new ArrayList<>();",
		"public long[] fe = Seq.EMPTY_LONGS;",  // enum -> long[]
		"public long[] fbf = Seq.EMPTY_LONGS;", // bitfield -> long[]
		// --- count:N with a short schema default: as written, no tail padding.
		"public int[] pu = new int[]{1, 2};",
		"public List<Boolean> pb = new ArrayList<>(List.of(true, true));",
		"public float[] pf32 = new float[]{1.5f};",
		// --- count:N wrapper arrays start empty too.
		"public List<String> fstr = new ArrayList<>();",
		"public List<MFobjElem> fobj = new ArrayList<>();",
		// --- dynamic: unchanged.
		"public int[] du = Seq.EMPTY_INTS;",
		"public List<Boolean> db = new ArrayList<>();",
		// --- with no declared default the omit guard is plain emptiness, so an
		// all-zero N-element value is NOT default and stays on the wire.
		"if (this.fu != null && this.fu.length != 0) {",
		"if (this.fb != null && !this.fb.isEmpty()) {",
		// --- a declared default is still hoisted to a static and compared whole (#146).
		"private static final int[] _arrdef_pu = new int[]{1, 2};",
		"if (!java.util.Arrays.equals(this.pu, _arrdef_pu)) {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	// No array of any shape may be padded out to its schema count.
	for _, unwanted := range []string{
		"public long[] fu = new long[5]",
		"public long[] pu = new long[]{1L, 2L, 0, 0}",
		"public float[] pf32 = new float[]{1.5f, 0.0f, 0.0f}",
		"List.of(true, true, false, false)",
		"List.of(false, false, false)",
		"_seqdef_", // the count:N wrapper-array filler is gone entirely
	} {
		if strings.Contains(m, unwanted) {
			t.Errorf("M.java must not contain %q (count is a capacity, not a length)", unwanted)
		}
	}
}

// TestJavaLazySequenceFraming: MESSAGE_SPEC §2 omits a sequence-typed FIELD whose
// value equals its declared default instead of framing it empty. Every sequence is
// therefore opened with the corelib's hold-back begin (writeSequenceBeginLazy) and
// the CLOSER is what encodes the distinction — writeSequenceEnd drops a
// contentless frame, writeSequenceEndKeep forces it out.
//
// documentation#29 made that choice POSITIONAL for a sequence-form array ELEMENT,
// read off the value at run time rather than off the schema: the dropping closer
// in the array's interior, where an all-default element vanishes into an id gap
// like any other default value, and the keeping one at the LAST index, whose
// presence is what carries the array's length (§5.1). A sequence-typed FIELD still
// always drops.
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
		"os.writeSequenceBeginLazy(0); (this.st == null ? new MSt() : this.st).serialize(os); os.writeSequenceEnd();",
		// A wrapper-array FIELD (string/blob elements): same -- depth 0 drops.
		"os.writeSequenceBeginLazy(1);",
		"os.writeSequenceBeginLazy(2);",
		// A struct ELEMENT chooses its closer from its position in the VALUE.
		"os.writeSequenceBeginLazy(3);",
		"os.writeSequenceBeginLazy(_i0); (_t2.get(_i0) == null ? new MObjsElem() : _t2.get(_i0)).serialize(os); if (_i0 == _t2.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
		// A nested wrapper ROW is an element too, and takes the same choice.
		"os.writeSequenceBeginLazy(4);",
		"            if (_i0 == _t3.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	// The eager begin is gone from the corelib; emitting it would not compile.
	if strings.Contains(m, "os.writeSequenceBegin(") {
		t.Error("M.java: every sequence must be opened with writeSequenceBeginLazy")
	}
	// Two element positions (the objs struct element, the mat row), each a
	// keep/drop pair; five sequence-typed FIELDS, each an unconditional drop.
	if got := strings.Count(m, "writeSequenceEndKeep()"); got != 2 {
		t.Errorf("expected 2 keeping closes (struct element + nested row), got %d", got)
	}
	if got := strings.Count(m, "else os.writeSequenceEnd();"); got != 2 {
		t.Errorf("expected 2 positional closers (struct element + nested row), got %d", got)
	}
	if got := strings.Count(m, "os.writeSequenceEnd();"); got != 7 {
		t.Errorf("expected 7 dropping closes (5 fields + 2 element interiors), got %d", got)
	}
	// An element is NEVER framed unconditionally any more: an all-default one in
	// the interior must vanish into an id gap (§2).
	if strings.Contains(m, ".serialize(os); os.writeSequenceEndKeep();") {
		t.Error("M.java: a sequence-form element must not take the keeping closer unconditionally")
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
	files := genJavaFromYAML(t, src, map[string]any{})
	m := files["src/main/java/message/M.java"]
	// The nested struct is its own public class in its own file (generator#305),
	// so its half of the reset contract is asserted there.
	st := files["src/main/java/message/MSt.java"]
	for _, want := range []string{
		// Public, so a caller driving the Visitor by hand can re-arm too.
		"    public void reset() {",
		// Scalars and strings go back to the declared default.
		"        this.lead = 3L;",
		`        this.name = "dflt";`,
		// Containers are emptied in place — the point of taking a destination.
		"        this.strs = Seq.reset(this.strs);",
		"        this.dyn = Seq.EMPTY_INTS;",
		// A fixed-count array is refilled from the shared default without allocating.
		"        if (this.fixd != null && this.fixd.length == _arrdef_fixd.length) System.arraycopy(_arrdef_fixd, 0, this.fixd, 0, _arrdef_fixd.length);",
		"        else this.fixd = _arrdef_fixd.clone();",
		"        this.bools = Seq.reset(this.bools);\n        this.bools.addAll(_arrdef_bools);",
		// A nested object recurses instead of being re-allocated.
		"        if (this.st == null) this.st = new MSt(); else this.st.reset();",
		// The reuse entry point re-arms before feeding.
		"    public static DecodeStatus tryDecode(byte[] data, M out) throws SofabException {\n        out.reset();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}
	// The struct's own reset(), in the struct's own file: it empties its
	// container in place, exactly as the message does.
	for _, want := range []string{
		"public class MSt {",
		"    public void reset() {",
		"        this.inner = Seq.reset(this.inner);",
	} {
		if !strings.Contains(st, want) {
			t.Errorf("MSt.java missing %q", want)
		}
	}
	// One reset() per class, and one class per file.
	if got := strings.Count(m, "public void reset() {"); got != 1 {
		t.Errorf("expected one reset() in M.java, got %d", got)
	}
	if got := strings.Count(st, "public void reset() {"); got != 1 {
		t.Errorf("expected one reset() in MSt.java, got %d", got)
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

// TestJavaNoSupportClassEmitted: the whole support layer is corelib-java's
// (generator#345 / corelib-java#97). A schema generates only its own classes --
// there is no Sbuf.java beside them any more, for any schema shape.
func TestJavaNoSupportClassEmitted(t *testing.T) {
	files := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})
	if _, ok := files["src/main/java/message/Sbuf.java"]; ok {
		t.Error("Sbuf.java is still emitted")
	}
	for path, body := range files {
		if strings.Contains(body, "class Sbuf") || strings.Contains(body, "Sbuf.") {
			t.Errorf("%s still names the generated support class", path)
		}
	}
}

// wrapperArraySrc is the schema the wrapper-array regression tests below run
// against: a count:N struct array next to a count-less one of the same element
// shape, count:N and count-less leaf arrays, and both matrix flavours (native
// rows and wrapper rows).
const wrapperArraySrc = `
version: 1
messages:
  Vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      dstrs:   { id: 3, type: array, items: { type: string, maxlen: 8 } }
      dblbs:   { id: 4, type: array, items: { type: blob, maxlen: 8 } }
      mat:     { id: 5, type: array, items: { type: array, count: 4, items: { type: u32, count: 3 } } }
      smat:    { id: 6, type: array, items: { type: array, count: 4, items: { type: string, maxlen: 8 } } }
`

// documentation#29 leaves ONE sparse rule for both element kinds, the same with
// or without a declared count: an element BEFORE the last one that equals its
// element default is omitted and leaves an id GAP, while the LAST element is
// always written -- as its value for a leaf, as an empty frame for a
// struct/union/nested-array element. Nothing is narrowed over the whole array any
// more: the wire count IS a compact array's length and the highest wrapper id IS
// its last index, so a trailing-run elision would SHORTEN the value, not re-shape
// it.
func TestJavaWrapperArrayInteriorSparseLastAlwaysWritten(t *testing.T) {
	files := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})
	got := files["src/main/java/message/Vec.java"]

	for _, want := range []string{
		// The loop runs over the value as written -- only a null is absorbed --
		// with or without a count.
		"List<VecFixedElem> _t0 = Seq.orEmpty(this.fixed);",
		"List<VecDynamicElem> _t1 = Seq.orEmpty(this.dynamic);",
		"List<String> _t2 = Seq.orEmpty(this.fstrs);",
		"for (int _i0 = 0; _i0 < _t0.size(); _i0++) { os.writeSequenceBeginLazy(_i0);",
		// A sequence-form element takes the POSITIONAL closer: dropping in the
		// interior (where an all-default element becomes an id gap), keeping at the
		// last index. Identical for the count:N and the count-less array.
		"(_t0.get(_i0) == null ? new VecFixedElem() : _t0.get(_i0)).serialize(os); if (_i0 == _t0.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
		"(_t1.get(_i0) == null ? new VecDynamicElem() : _t1.get(_i0)).serialize(os); if (_i0 == _t1.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
		// A leaf element: the same rule, unconditional now rather than count-gated.
		`String _e0 = _t2.get(_i0); if (_e0 == null) _e0 = ""; if (!_e0.isEmpty() || _i0 == _t2.size() - 1) os.writeString(_i0, _e0);`,
		`String _e0 = _t3.get(_i0); if (_e0 == null) _e0 = ""; if (!_e0.isEmpty() || _i0 == _t3.size() - 1) os.writeString(_i0, _e0);`,
		`byte[] _e0 = _t4.get(_i0); if (_e0 == null) _e0 = Seq.EMPTY_BYTES; if (_e0.length != 0 || _i0 == _t4.size() - 1) os.writeBlob(_i0, _e0);`,
		// A NATIVE row has no frame of its own, so the rule lands on the write; a
		// primitive one is a long[], written with no box/unbox temporary.
		"if (_e0.length != 0 || _i0 == _t5.size() - 1) {",
		"os.writeArrayUnsigned(_i0, _e0);",
		// A WRAPPER row has one, so it takes the closer -- like the struct element.
		"if (_i0 == _t6.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Vec.java missing %q:\n%s", want, got)
		}
	}

	// isDefault is the exact mirror of what marshal writes: the writer emits a
	// child for every element it holds (the last one whatever its value), so "no
	// child is written" is exactly "the array is empty" -- for every element kind
	// and whether or not a count is declared.
	for _, want := range []string{
		"if (!Seq.orEmpty(this.fixed).isEmpty()) return false;",
		"if (!Seq.orEmpty(this.dynamic).isEmpty()) return false;",
		"if (!Seq.orEmpty(this.fstrs).isEmpty()) return false;",
		"if (!Seq.orEmpty(this.mat).isEmpty()) return false;",
		"if (!Seq.orEmpty(this.smat).isEmpty()) return false;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, got)
		}
	}

	// The superseded narrowing is gone from the generated code.
	for _, gone := range []string{"trimTailObjs", "trimTailStrings", "trimTailBlobs", "trimTailRows"} {
		if strings.Contains(got, gone) {
			t.Errorf("Vec.java must not still narrow with %q:\n%s", gone, got)
		}
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at dest[id] after gap-filling -- never appended. Interior
// sparsity (documentation#29) is what makes an interior gap reachable at all, so
// this now matters for every element kind, matrix rows included.
//
// The other half: `count: N` is a CAPACITY, so it bounds the element id and
// nothing more -- sequenceEnd fills NOTHING back in, because the elements that
// arrived are the whole value.
func TestJavaWrapperElementsArePlacedByID(t *testing.T) {
	got := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})["src/main/java/message/Vec.java"]

	for _, want := range []string{
		// placement, not append -- and the gap-fill that precedes it
		"while (m.fixed.size() <= id) m.fixed.add(new VecFixedElem()); _ex_Root_fixed = id;",
		"while (m.dynamic.size() <= id) m.dynamic.add(new VecDynamicElem()); _ex_Root_dynamic = id;",
		// a child field of the element resolves through the PLACED index (the §7.1
		// width guard for the u32 destination precedes the store; see
		// TestJavaDeclaredWidthIsAValidityBound)
		"m.fixed.get(_ex_Root_fixed).k = value; break;",
		// leaf elements were always placed by id
		"while (m.fstrs.size() <= id) m.fstrs.add(\"\"); m.fstrs.set(id, _s); break;",
		"while (m.dblbs.size() <= id) m.dblbs.add(new byte[0]); m.dblbs.set(id, _b); break;",
		// the over-index guard bounds both the placement and the gap-fill
		`if (id >= 5) throw Sofab.invalid("Root_fixed element: array index above schema capacity 5");`,
		// sequenceEnd is a bare pop: a capacity adds no elements.
		"public void sequenceEnd() { cur = sp > 0 ? stk[--sp] : 0; }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Vec.java missing %q:\n%s", want, got)
		}
	}

	// The defect this replaced: appending ignored the id entirely.
	if strings.Contains(got, "m.fixed.add(new VecFixedElem()); cur =") {
		t.Errorf("struct-array elements must not be appended id-blind:\n%s", got)
	}
	// No wrapper array may be refilled to a schema count on sequenceEnd any more.
	for _, gone := range []string{
		"while (m.fixed.size() < 5)",
		"while (m.fstrs.size() < 3)",
		"Refill the closing wrapper array",
	} {
		if strings.Contains(got, gone) {
			t.Errorf("a capacity must never add elements, found %q:\n%s", gone, got)
		}
	}
}

// The row collectors of a matrix (native inner rows) and of an array-of-wrapper-
// arrays used to APPEND, ignoring the row's element id. That was unreachable while
// every row was written; interior sparsity (documentation#29) makes an interior
// gap ordinary, and an appending collector then shifts every later row down by
// one. Rows are placed at out[id] like every other element kind, bounded by the
// outer array's count -- which also closes the over-index hole those collectors
// had.
func TestJavaMatrixRowsArePlacedByID(t *testing.T) {
	got := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})["src/main/java/message/Vec.java"]

	for _, want := range []string{
		// native rows: placed in arrayBegin, bounded by the OUTER array's count --
		// behind the §7.3 kind test, so a mis-typed row is skipped, never placed
		// and never bound-checked (generator#254).
		`case 8: if (kind != ArrayKind.UNSIGNED) break; if (id >= 4) throw Sofab.invalid("Root_mat element: array index above schema capacity 4"); if (count > 3) throw Sofab.invalid("mat element: array count above schema capacity 3"); askip = 0; afill = count; atgt = 1; _arowInt = Seq.reserveRowInts(m.mat, id, count); _ex_Root_mat = id; break;`,
		// and the elements land in the row that id named -- through the cursor
		// arrayBegin parked, which is already exactly `count` long, so the store is
		// a plain indexed write with no growth and no write-back (§9.5 shape A)
		"_arowInt[ai++] = (int) value; return;",
		// wrapper rows: placed in sequenceBegin, same shape
		`case 9: if (id >= 4) throw Sofab.invalid("Root_smat element: array index above schema capacity 4"); Seq.reserveRow(m.smat, id); _ex_Root_smat = id; cur = 10; break;`,
		"while (m.smat.get(_ex_Root_smat).size() <= id) m.smat.get(_ex_Root_smat).add(\"\");",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Vec.java missing %q:\n%s", want, got)
		}
	}

	// The defect: an id-blind append, and a row accessor reaching for the last
	// appended row instead of the one the id named.
	for _, gone := range []string{
		"m.mat.add(new ArrayList<>())",
		"m.smat.add(new ArrayList<>())",
		"m.mat.get(m.mat.size()-1)",
		"m.smat.get(m.smat.size()-1)",
	} {
		if strings.Contains(got, gone) {
			t.Errorf("matrix rows must not be collected id-blind, found %q:\n%s", gone, got)
		}
	}

	// The placement itself -- grow with empty rows, then empty the row AT the id in
	// place, because an array wrapper IS the array's value (§7.4) -- is Seq.reserveRow
	// in corelib-java, which owns its tests. What is pinned here is that the
	// generated code reaches it and does not re-implement it.
	if strings.Contains(got, "while (m.smat.size() < id)") {
		t.Errorf("row placement must be Seq.reserveRow, not re-emitted:\n%s", got)
	}
}

// A `count: N` wrapper array is NOT materialized to N elements anywhere:
// `count` is a capacity, so a fresh one is empty, reset() leaves it empty, and an
// absent field decodes back to empty -- which is exactly what a count-less one
// does. The filler factory that used to add N element defaults is gone with the
// fill-to-N it existed to match.
func TestJavaCountNWrapperArrayNotMaterialized(t *testing.T) {
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
		// Construction: empty, exactly like the count-less array next to them.
		"public List<String> strs = new ArrayList<>();",
		"public int[] nums = Seq.EMPTY_INTS;",
		"public List<byte[]> blobs = new ArrayList<>();",
		"public List<MObjsElem> objs = new ArrayList<>();",
		"public List<String> dyn = new ArrayList<>();",
		// reset() re-arms to the same value, in place, and adds nothing.
		"        this.strs = Seq.reset(this.strs);\n        this.nums = Seq.EMPTY_INTS;",
		"        this.objs = Seq.reset(this.objs);\n        this.dyn = Seq.reset(this.dyn);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q:\n%s", want, m)
		}
	}
	// The N-element filler and every trace of it are gone.
	for _, gone := range []string{"_seqdef_", "for (int i = 0; i < 3; i++) l.add", "new ArrayList<>(3)"} {
		if strings.Contains(m, gone) {
			t.Errorf("a count:N wrapper array must not be materialized, found %q:\n%s", gone, m)
		}
	}
}

// TestJavaSkippedStringIsNotValidated: a `string` payload the visitor will not
// materialize must be skipped whole — its bytes jumped over, never inspected
// (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038). corelib-java hands EVERY
// fixlen-string field to the generated string() callback, unknown ids and §7.3
// wire-type contradictions included, so the callback itself is what decides
// whether a payload is read. It used to materialize the string first and dispatch
// on (cur, id) second, so a lone continuation byte at an id the scope does not
// declare threw INVALID_MSG out of an otherwise valid message.
//
// The fix is order: resolve the destination first and return when nothing
// matches, so no byte is decoded or written into the shared `acc`.
func TestJavaSkippedStringIsNotValidated(t *testing.T) {
	files := genJavaFromYAML(t, `
version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 16 }
      n:
        id: 1
        type: struct
        fields:
          t: { id: 2, type: string, maxlen: 8 }
      sa: { id: 3, type: array, items: { type: string, count: 4, maxlen: 8 } }
`, map[string]any{})
	fn := javaMethod(t, files["src/main/java/message/M.java"], "    public void string(int id,")

	// The outer `default: return;` closing the (cur) switch — the inner per-id
	// switches carry one too, so match on the outer indentation.
	const guardTail = "\n        default: return;\n        }"
	guardEnd := strings.Index(fn, guardTail)
	if guardEnd < 0 {
		t.Fatalf("string() missing the §6.4 destination guard:\n%s", fn)
	}
	guard := fn[:guardEnd]
	for _, want := range []string{
		"case 0: switch (id) { case 0: break; default: return; } break;", // the scalar string
		"case 1: switch (id) { case 2: break; default: return; } break;", // the nested struct's string
		"case 2: break;", // the string-array row: every id
	} {
		if !strings.Contains(guard, want) {
			t.Errorf("string() missing destination arm %q:\n%s", want, fn)
		}
	}
	// The guard precedes the UTF-8 decode and the accumulator alike, so a skipped
	// payload is neither validated nor able to leave bytes behind for a later
	// declared field to inherit.
	for _, after := range []string{"acc.string(", "String _s"} {
		if i := strings.Index(fn, after); i < 0 || guardEnd > i {
			t.Errorf("string(): the destination guard must precede %q:\n%s", after, fn)
		}
	}
	// The maxlen reject stays destination-scoped behind it.
	if i := strings.Index(fn, "above schema maxlen"); i < 0 || guardEnd > i {
		t.Errorf("string(): the maxlen reject must survive behind the guard:\n%s", fn)
	}
	// A blob carries no encoding, so blob() keeps the plain shape.
	if strings.Contains(javaMethod(t, files["src/main/java/message/M.java"], "    public void blob(int id,"), "default: return;") {
		t.Errorf("blob() must not carry a destination guard:\n%s", files["src/main/java/message/M.java"])
	}
}

// A message that declares NO string still gets a string() callback (Visitor
// declares it, and the corelib still routes string fields at unknown ids to it),
// but every string reaching it is skipped by definition — so the body must be
// empty. Decoding one only to drop it is the same §6.4 violation, just with
// every string skipped instead of some.
func TestJavaStringFreeSchemaNeverDecodesAString(t *testing.T) {
	files := genJavaFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
      b: { id: 1, type: blob, maxlen: 8 }
`, map[string]any{})
	src := files["src/main/java/message/M.java"]
	fn := javaMethod(t, src, "    public void string(int id,")
	for _, forbidden := range []string{"acc.string(", "acc", "switch (cur)", "String _s"} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("a string-free schema must not %q in string():\n%s", forbidden, fn)
		}
	}
	// The callback is still declared -- Visitor requires it.
	if !strings.Contains(src, "public void string(int id,") {
		t.Errorf("string() must still be declared:\n%s", src)
	}
}

// javaMethod returns the generated method body starting at `head` up to the next
// top-level `    public ` line, so an ordering assertion inside one callback
// cannot accidentally match text from a neighbouring one.
func javaMethod(t *testing.T, src, head string) string {
	t.Helper()
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", head, src)
	}
	rest := src[i+len(head):]
	if j := strings.Index(rest, "\n    public "); j >= 0 {
		return src[i : i+len(head)+j]
	}
	return src[i:]
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound, rejected through the
// same unchecked INVALID_MSG channel as the maxlen guard.
//
// The `value < 0` term on the unsigned side is load-bearing, not noise: the
// corelib delivers an unsigned wire value as a Java long, so a u64 at or above
// 2^63 arrives with its sign bit set and `value > 255` alone would let exactly
// the largest values through.
func TestJavaDeclaredWidthIsAValidityBound(t *testing.T) {
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
	got := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/W.java"]
	for _, want := range []string{
		`case 0: if (value < 0 || value > 255L) throw Sofab.invalid("a_u8: value outside declared width u8"); m.a_u8 = value; break;`,
		`case 2: if (value < 0 || value > 4294967295L) throw Sofab.invalid("c_u32: value outside declared width u32"); m.c_u32 = value; break;`,
		`case 4: if (value < -128L || value > 127L) throw Sofab.invalid("e_i8: value outside declared width i8"); m.e_i8 = value; break;`,
		`case 6: if (value < -2147483648L || value > 2147483647L) throw Sofab.invalid("g_i32: value outside declared width i32"); m.g_i32 = value; break;`,
		// An array element carries the same bound, guarded AFTER the fill guard so a
		// §7.3-skipped bare scalar at the array id is not turned into an INVALID.
		`case 1: if (value < 0 || value > 255L) throw Sofab.invalid("arr_u8 element: value outside declared width u8");`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("W.java missing width guard %q:\n%s", want, got)
		}
	}
	for _, want := range []string{"case 3: m.d_u64 = value; break;", "case 7: m.h_i64 = value; break;"} {
		if !strings.Contains(got, want) {
			t.Errorf("W.java: a 64-bit destination must store unguarded (%q):\n%s", want, got)
		}
	}
}

// generator#268 (Crucible F-0044) and #272 (F-0047): sequenceBegin's dispatch had
// no default arm, so a sequence the schema does not declare at this position was
// ENTERED and its children bound into the ENCLOSING scope — an unknown sequence
// id carrying a child id 3 set the ROOT's field 3 (#268), and a sequence opened
// at a string-array element position bound its string as that element (#272).
//
// Both are one missing default: an undeclared (scope, id) moves to _DEAD, which
// no callback case matches, so the whole subtree is discarded. The stack alone
// restores the live scope at the matching end.
func TestJavaUnknownSequenceIsSkippedWhole(t *testing.T) {
	const src = `
version: 1
messages:
  Probe:
    payload:
      a:            { id: 3, type: i16 }
      known:        { id: 10, type: struct, fields: { k: { id: 0, type: u32 } } }
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
`
	got := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/Probe.java"]
	for _, want := range []string{
		"private static final int _DEAD = -1;",
		// The declared position still descends ...
		"case 10: cur = 1; break;",
		// ... an undeclared id in a scope that HAS sequences is skipped ...
		"default: cur = _DEAD; break;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Probe.java missing %q:\n%s", want, got)
		}
	}
	// ... and so is any id in a scope that declares none (the string-array element
	// scope of #272), which used to fall straight through the outer switch.
	if strings.Count(got, "cur = _DEAD; break;") < 2 {
		t.Errorf("every scope must skip an undeclared sequence id (#272):\n%s", got)
	}
}

// TestJavaNamedTypesArePublicAndOwnTheirFile pins generator#305.
//
// Java allows one public top-level class per file and the message owns that
// slot, so a schema struct emitted INTO the message's file could only be
// package-private — which made the message's own public field unusable: a
// caller outside the generated package could neither touch `msg.inner.x` nor
// name the type. Every other target exports these types.
//
// The same emission had a harder failure behind it: a type reached from two
// messages was written into both files, i.e. declared twice in one package,
// which javac rejects as a duplicate class. So the schema below shares one
// struct between two messages, and the type must appear exactly once.
func TestJavaNamedTypesArePublicAndOwnTheirFile(t *testing.T) {
	const src = `
version: 1
$defs:
  struct:
    Point:
      x: { id: 0, type: i32 }
messages:
  first:
    payload:
      p: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Point' } }
  second:
    payload:
      q: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Point' } }
`
	files := genJavaFromYAML(t, src, map[string]any{"package": "com.example.two"})

	const dir = "src/main/java/com/example/two/"
	pointFile, ok := files[dir+"StructPoint.java"]
	if !ok {
		var have []string
		for p := range files {
			have = append(have, p)
		}
		sort.Strings(have)
		t.Fatalf("the shared struct has no file of its own; emitted: %v", have)
	}

	if !strings.Contains(pointFile, "public class StructPoint {") {
		t.Error("a schema struct must be public — a caller outside the package has to name it")
	}
	if !strings.Contains(pointFile, "package com.example.two;") {
		t.Error("the type's file is missing the package declaration")
	}

	// Declared once, in its own file and nowhere else: two declarations in one
	// package do not compile.
	for path, body := range files {
		if path == dir+"StructPoint.java" {
			continue
		}
		if strings.Contains(body, "class StructPoint") {
			t.Errorf("%s also declares StructPoint — duplicate class in one package", path)
		}
	}

	// The generated plumbing stays package-private: it is not schema surface.
	msg := files[dir+"First.java"]
	if !strings.Contains(msg, "class FirstVisitor implements Visitor {") ||
		strings.Contains(msg, "public class FirstVisitor") {
		t.Error("the decode visitor must stay package-private")
	}

}

// A schema bound that the fixlen LENGTH WORD already decides must be latched at
// that word, not once payload bytes arrive (CORELIB_PLAN §5.2, generator#267).
// The guards lived in the PAYLOAD callback, which never fires for a message
// truncated immediately after the length word -- so that reported INCOMPLETE
// where the same bytes read whole are INVALID.
//
// Pinned here: the hook exists, both bounds are inside it, and every guard sits
// behind the DECLARED-subtype test (a contradicting subtype is a §7.3 skip, not
// this field's length).
func TestJavaFixlenBeginLatchesBoundsAtTheLengthWord(t *testing.T) {
	files := genJavaFromYAML(t, `version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 8 }
      b:  { id: 1, type: blob, maxlen: 4 }
      sa: { id: 2, type: array, items: { type: string, count: 3, maxlen: 6 } }
`, map[string]any{})
	var m string
	for path, src := range files {
		if strings.HasSuffix(path, "M.java") {
			m = src
		}
	}
	if m == "" {
		t.Fatal("no M.java")
	}

	if !strings.Contains(m, "public void fixlenBegin(int id, FixlenType subtype, int total)") {
		t.Fatal("no fixlenBegin override")
	}
	if !strings.Contains(m, "if (subtype == FixlenType.STRING) {") ||
		!strings.Contains(m, "case 0: if (total > 8)") {
		t.Error("a scalar string maxlen must be latched under FixlenType.STRING")
	}
	if !strings.Contains(m, "if (subtype == FixlenType.BLOB) {") ||
		!strings.Contains(m, "case 1: if (total > 4)") {
		t.Error("a scalar blob maxlen must be latched under FixlenType.BLOB")
	}
	// Over-index first, then the element maxlen.
	if !strings.Contains(m, "if (id >= 3) throw") || !strings.Contains(m, "if (total > 6) throw") {
		t.Error("a wrapper element must latch over-index and element maxlen")
	}
	// The payload-side guard stays as defense for an older corelib.
	if strings.Count(m, "total > 8") < 2 {
		t.Error("the payload-side maxlen guard must remain")
	}
}

// TestJavaAShapeCheckThenAllocate: an array whose size arrives BEFORE its payload
// -- a native integer or fp array, and a native matrix row -- is bounded at the
// count header and then allocated at exactly that count, once (ARCHITECTURE §9.5,
// shape A, generator#386).
//
// What this replaced was the #96/#98 shape: reserve Seq.ARRAY_INIT_CAP elements
// and grow toward the count with Seq.ensureCap. That was the heap-exhaustion
// mitigation for an untrusted wire count, written the day before the config caps
// of #102 existed; once the count is checked against a finite bound before the
// allocation, allocating it exactly is both safe and cheaper. The check is what
// makes it safe, so the two assertions belong together and this test keeps them
// in one place: no arm may allocate without a bound in front of it.
func TestJavaAShapeCheckThenAllocate(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      dyn: { id: 0, type: array, items: { type: u32 } }
      bnd: { id: 1, type: array, items: { type: u32, count: 8 } }
      fps: { id: 2, type: array, items: { type: fp32 } }
      mat: { id: 3, type: array, items: { type: array, count: 3, items: { type: u32, count: 4 } } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]

	for _, want := range []string{
		// Schema-unbounded: the cap decides, then the exact allocation.
		`if (count > MAX_DYN_ARRAY_COUNT)`,
		`abulk = m.dyn = new int[count]`,
		// Schema-bounded: its own bound decides, then the exact allocation.
		`if (count > 8) throw Sofab.invalid("bnd: array count above schema capacity 8"); askip = 0; afill = count; atgt = 2; abulk = m.bnd = new int[count]`,
		// An fp array is not bulk-capable (the offer is integer-only) but is sized
		// the same way -- the shape is about the allocation, not about the offer.
		`m.fps = new float[count]`,
		// A ROW's own element count is bounded by the INNER schema count, which is
		// not the same bound as the outer array's capacity beside it: that one
		// bounds the row's id. Both, in that order.
		`if (id >= 3) throw Sofab.invalid("Root_mat element: array index above schema capacity 3"); if (count > 4) throw Sofab.invalid("mat element: array count above schema capacity 4");`,
		`Seq.reserveRowInts(m.mat, id, count)`,
		// The stores are plain indexed writes: nothing grows, so nothing is
		// re-assigned into the message object per element.
		`m.dyn[ai++] = (int) value`,
		`_arowInt[ai++] = (int) value`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q:\n%s", want, m)
		}
	}
	// The growth machinery must be gone, not merely unused: Seq.ensureCap and
	// Seq.ARRAY_INIT_CAP are corelib API with no call site left here, and `acap`
	// was the per-array growth ceiling that fed them.
	for _, gone := range []string{"Seq.ensureCap", "Seq.ARRAY_INIT_CAP", "Math.min(count", "acap"} {
		if strings.Contains(m, gone) {
			t.Errorf("M.java must not still grow into an array (%q):\n%s", gone, m)
		}
	}
}
