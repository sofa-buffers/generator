package java

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
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
		"public void sequenceBegin(int id)",                                             // flat-visitor nesting
		"public long someu64 = 0xFFFFFFFFFFFFFFFFL;",                                    // a u64 default is a compile-time constant, not a runtime parse (#479)
		"public int[] someuintarray = new int[]{0, 1, 1000, -1};",                       // primitive array (was List<Long>)
		"public float[] somefloatarray = new float[]{0.0f, -1.5f, 3.25f};",              // primitive fp array
		"public byte[] someenumarray = new byte[]{(byte) 2, (byte) 1, (byte) 0};",       // an enum array is backed by the width its declaration implies (§1): {0,1,2} is an i8
		"public byte[] somebitfieldarray = Seq.EMPTY_BYTES;",                            // and a bitfield by its highest `pos`: 1 is a u8
		"abulk = m.someenumarray = new byte[count];",                                    // the destination states i8, so the corelib's bulk offer carries the bound
		"os.writeArrayUnsigned(15, this.someuintarray);",                                // direct write, no box, no trim: the wire count IS the length
		"private static final int[] _arrdef_someuintarray = new int[]{0, 1, 1000, -1};", // omit-default hoisted to a static (#146)
		"if (!java.util.Arrays.equals(this.someuintarray, _arrdef_someuintarray)) {",    // guard reads the static -- no per-encode new long[] (#146)
		"m.someuintarray[ai++] = (int) value;",                                          // plain indexed store: arrayBegin sized the array at the checked count (§9.5 shape A)
		"case 15: if (kind != ArrayKind.UNSIGNED) break; if (count > 4) throw Sofab.invalid(\"someuintarray: array count above schema capacity 4\"); askip = 0; afill = count; atgt = 1; abulk = m.someuintarray = new int[count]; break;", // mis-typed header skipped before the bound (#254); over-count rejected (#100); the M that arrived is the whole value
		"OStream os = OStream.overScratch(MAX_SIZE);", // the corelib owns the scratch buffer; MAX_SIZE stays ours (§5.1)
		"return os.copyOfBytesUsed();",                // exact-size copy out of it
		"String _s = acc.string(total, offset, data, chunkOffset, chunkLength, Bound.SCHEMA_BOUNDED);", // reassembly, UTF-8 and the receiver cap, all the corelib's
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
// (issue #142 / MESSAGE_SPEC §5.1/§7). The comparison is the corelib's: the
// schema count travels into the Seq call as Bound.schema(N), and no literal index
// check is emitted. A dynamic array keeps every index the receiver cap allows.
func TestJavaOverIndexWrapperArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" +
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      ds: { id: 3, type: array, items: { type: string } }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// One Bound.schema constant per distinct count, built once.
		`private static final Bound SCHEMA_COUNT_2 = Bound.schema(2);`,
		`private static final Bound SCHEMA_COUNT_3 = Bound.schema(3);`,
		`private static final Bound SCHEMA_COUNT_4 = Bound.schema(4);`,
		// The count is the placement's argument: the corelib compares it before the
		// list grows, so a refused id leaves the container unextended (CORELIB_PLAN
		// §7.2 item 8), and answers INVALID_MSG past it.
		`case 1: Seq.placeElem(m.bs, id, "", _s, SCHEMA_COUNT_4); break;`,
		`Seq.placeElem(m.bb, id, Seq.EMPTY_BYTES, _b, SCHEMA_COUNT_3); break;`,
		// The struct-element arm reserves the slot by id (generator#247) through the
		// same corelib layer, and keeps only the routing that follows it.
		`: Seq.reserveElem(m.bp, id, MBpElem::new, SCHEMA_COUNT_2);`,
		// ...and the length word names the same bound, on its own.
		`Seq.checkIndex(id, SCHEMA_COUNT_4);`,
		`Seq.checkIndex(id, SCHEMA_COUNT_3);`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing over-index guard %q", want)
		}
	}
	// A dynamic array keeps every index the receiver cap allows, and the cap is the
	// call's argument rather than a guard in front of it -- so no schema verdict is
	// emitted for it at all.
	if !strings.Contains(m, `Seq.placeElem(m.ds, id, "", _s, CAP_DYN_ARRAY_COUNT); break;`) {
		t.Errorf("dynamic string array must place through the corelib with the receiver cap")
	}
	if strings.Contains(m, `"Root_ds element`) {
		t.Errorf("dynamic string array must not carry an over-index guard")
	}
	// The gap fill and the grow loop are the corelib's now: no generated wrapper
	// array re-implements them.
	if strings.Contains(m, `.add(new byte[0])`) || strings.Contains(m, `.size() <= id`) {
		t.Errorf("wrapper-array growth must not be emitted; it is Seq.placeElem/reserveElem's")
	}
	// Nor the index check: it rides the corelib call with the schema count.
	if strings.Contains(m, `if (id >= `) || strings.Contains(m, `array index above schema capacity`) {
		t.Errorf("a literal wrapper-array index check must not be emitted; the count travels as Bound.schema")
	}
	if regexp.MustCompile(`Seq\.\w+\([^;]*SCHEMA_BOUNDED`).MatchString(m) {
		t.Errorf("no index bound is Bound.SCHEMA_BOUNDED: it carries no count and the corelib refuses it")
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
		// Unbounded string: the cap is PASSED to the accumulator, which compares it
		// against the announced total before it buffers or materializes a byte
		// (CORELIB_PLAN §6.2.1). `s` is the only string destination and it is
		// unbounded, so the constant travels as a literal -- no guard, no `_lim`.
		"String _s = acc.string(total, offset, data, chunkOffset, chunkLength, CAP_DYN_STRING_LEN);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Dyn.java missing %q", want)
		}
	}
	if strings.Contains(m, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	// §6.2.1's "one implementation, wherever it runs": the length cap is either
	// a guard here or an argument to the corelib call, never both.
	if strings.Contains(m, "if (total > MAX_DYN_STRING_LEN)") {
		t.Errorf("the string cap must not ALSO be a generated guard:\n%s", m)
	}
	// This schema declares no blob at all, so there is no call for a cap to
	// travel on either: the callback body is empty and nothing is sized from the
	// wire for a payload nobody reads (generator#436). The blob twin of the
	// SCHEMA_BOUNDED literal is pinned in TestJavaSkippedBlobIsNotMaterialized,
	// whose blobs are all maxlen'd.
	if strings.Contains(m, "acc.blob(") {
		t.Errorf("a message with no blob field must not reach the blob accumulator:\n%s", m)
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
// elements alike. The comparison happens ONCE, in fixlenBegin at the length word
// (#594); the payload callbacks restate nothing. A schema-unbounded field
// carries no maxlen guard (it keeps only the generator#102 configured-limit
// behavior).
func TestJavaMaxlenReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:   { id: 0, type: string, maxlen: 8 }\n" +
		"      b:   { id: 1, type: blob,   maxlen: 8 }\n" +
		"      u:   { id: 2, type: string }\n" +
		"      arr: { id: 3, type: array, items: { type: string, maxlen: 5 } }\n"
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]
	for _, want := range []string{
		// Bounded scalar string: reject total > maxlen at its LENGTH WORD.
		`case 0: if (total > 8) throw Sofab.invalid("s: string length above schema maxlen 8"); break;`,
		// Bounded scalar blob: reject total > maxlen at its LENGTH WORD.
		`case 1: if (total > 8) throw Sofab.invalid("b: blob length above schema maxlen 8"); break;`,
		// Bounded wrapper string element: reject total > element maxlen.
		`if (total > 5) throw Sofab.invalid("arr element: string length above schema maxlen 5"); break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing maxlen reject %q", want)
		}
	}
	// All three sit in fixlenBegin and only there: three bounded destinations,
	// three comparisons (#594).
	if n := strings.Count(m, "above schema maxlen"); n != 3 {
		t.Errorf("expected 3 maxlen comparisons (one per bounded destination), got %d", n)
	}
	if i := strings.Index(m, "public void fixlenBegin("); i < 0 || strings.Index(m, "above schema maxlen") < i {
		t.Error("the maxlen comparison must live in fixlenBegin")
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
	// The blob cap stays inert: liveness is a property of the schema, not of the
	// configuration, and there is no unbounded blob.
	if strings.Contains(m, "MAX_DYN_BLOB_LEN") {
		t.Error("inert limit MAX_DYN_BLOB_LEN must not be emitted")
	}
	// The ARRAY cap is live, and `arr` is why. It is a WRAPPER array, so it
	// carries no count header for arrayBegin to check -- but its element INDEX is
	// its length, and that is what the cap binds (generator#387). The comparison
	// rides the corelib call that grows the list (generator#587), so what is
	// emitted is the Bound, not the rejection.
	if !strings.Contains(m, "private static final Bound CAP_DYN_ARRAY_COUNT = Bound.receiver(MAX_DYN_ARRAY_COUNT);") {
		t.Errorf("a dynamic wrapper array's element index must be capped:\n%s", m)
	}
	if !strings.Contains(m, `Seq.placeElem(m.arr, id, "", _s, CAP_DYN_ARRAY_COUNT);`) {
		t.Errorf("the array cap must ride the placement call:\n%s", m)
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
		// enum elements ride the SIGNED wire type, and they ride the bulk offer
		// like every other integer element: an enum is bound by the width its
		// DECLARATION implies (§1), {A:0, B:1} implies an i8, and a byte[]
		// destination is exactly how the offer states that bound.
		`case 4: if (kind != ArrayKind.SIGNED) break; if (count > 2) throw Sofab.invalid("ea: array count above schema capacity 2"); askip = 0; afill = count; atgt = 2; abulk = m.ea = new byte[count]; break;`,
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
	if !strings.Contains(m, "String _s = acc.string(total, offset, data, chunkOffset, chunkLength, CAP_DYN_STRING_LEN);") {
		t.Error("a string payload must be materialized through PayloadAcc.string, and carry its receiver cap")
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
		"public byte[] fe = Seq.EMPTY_BYTES;",  // enum -> the width its constants imply (i8)
		"public byte[] fbf = Seq.EMPTY_BYTES;", // bitfield -> the width its highest `pos` implies (u8)
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
		// placement, not append -- the reservation the corelib makes, and the
		// element-index register the routing that follows reads back
		"Seq.reserveElem(m.fixed, id, VecFixedElem::new, SCHEMA_COUNT_5); _ex_Root_fixed = id;",
		"Seq.reserveElem(m.dynamic, id, VecDynamicElem::new, CAP_DYN_ARRAY_COUNT); _ex_Root_dynamic = id;",
		// a child field of the element resolves through the PLACED index (the §7.1
		// width guard for the u32 destination precedes the store; see
		// TestJavaDeclaredWidthIsAValidityBound)
		"m.fixed.get(_ex_Root_fixed).k = value; break;",
		// leaf elements were always placed by id, and the placement is one call
		"Seq.placeElem(m.fstrs, id, \"\", _s, SCHEMA_COUNT_3); break;",
		"Seq.placeElem(m.dblbs, id, Seq.EMPTY_BYTES, _b, CAP_DYN_ARRAY_COUNT); break;",
		// the schema count bounds both the placement and the gap-fill; the corelib
		// takes it before it grows, so a refused id leaves the list unextended
		"private static final Bound SCHEMA_COUNT_5 = Bound.schema(5);",
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
	// And the gap fill it needed is the corelib's, not emitted per array.
	if strings.Contains(got, ".size() <= id") {
		t.Errorf("no wrapper array may emit its own grow loop:\n%s", got)
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
		// The row id is judged before the row's own count, by the corelib.
		`case 8: if (kind != ArrayKind.UNSIGNED) break; Seq.checkIndex(id, SCHEMA_COUNT_4); if (count > 3) throw Sofab.invalid("mat element: array count above schema capacity 3"); askip = 0; afill = count; atgt = 1; _arowInt = Seq.reserveRowInts(m.mat, id, count, SCHEMA_COUNT_4); _ex_Root_mat = id; break;`,
		// and the elements land in the row that id named -- through the cursor
		// arrayBegin parked, which is already exactly `count` long, so the store is
		// a plain indexed write with no growth and no write-back (§9.5 shape A)
		"_arowInt[ai++] = (int) value; return;",
		// wrapper rows: placed in sequenceBegin, same shape
		`case 9: Seq.reserveRow(m.smat, id, SCHEMA_COUNT_4); _ex_Root_smat = id; cur = 10; break;`,
		`Seq.placeElem(m.smat.get(_ex_Root_smat), id, "", _s, CAP_DYN_ARRAY_COUNT);`,
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
	// No schema-maxlen comparison is emitted in the payload callback at all
	// (#594): the one comparison is fixlenBegin's, at the length word, and its
	// throw ends the decode before string() is entered.
	if strings.Contains(fn, "above schema maxlen") {
		t.Errorf("string(): the schema maxlen must be compared only at the length word:\n%s", fn)
	}
}

// The blob twin of the test above, and the correction of an earlier reading of
// it: the guard was called a string-only concern because UTF-8 is the only thing
// a blob has nothing of. Validation was never all it bought. Without it a blob at
// an id this scope does not declare still reached acc.blob(), which sizes a
// byte[] from the wire `total` and copies the payload into it -- and only the
// switch below found no arm and dropped it. A 1 MiB blob at an unknown id cost
// 1 MiB of heap for a field nobody reads: a payload MATERIALIZED where
// MESSAGE_SPEC §7.3 says the bytes are walked over, and storage sized from the
// wire for a value never delivered (CORELIB_PLAN §6.2.1, §6.6, §6.7.2).
func TestJavaSkippedBlobIsNotMaterialized(t *testing.T) {
	files := genJavaFromYAML(t, `
version: 1
messages:
  m:
    payload:
      b:  { id: 0, type: blob, maxlen: 16 }
      n:
        id: 1
        type: struct
        fields:
          t: { id: 2, type: blob, maxlen: 8 }
      ba: { id: 3, type: array, items: { type: blob, count: 4, maxlen: 8 } }
`, map[string]any{})
	fn := javaMethod(t, files["src/main/java/message/M.java"], "    public void blob(int id,")

	const guardTail = "\n        default: return;\n        }"
	guardEnd := strings.Index(fn, guardTail)
	if guardEnd < 0 {
		t.Fatalf("blob() missing the §6.2.1 destination guard:\n%s", fn)
	}
	guard := fn[:guardEnd]
	for _, want := range []string{
		"case 0: switch (id) { case 0: break; default: return; } break;", // the scalar blob
		"case 1: switch (id) { case 2: break; default: return; } break;", // the nested struct's blob
		"case 2: break;", // the blob-array row: every id
	} {
		if !strings.Contains(guard, want) {
			t.Errorf("blob() missing destination arm %q:\n%s", want, fn)
		}
	}
	// The whole point: nothing is sized from the wire or copied before the gate.
	for _, after := range []string{"acc.blob(", "byte[] _b"} {
		if i := strings.Index(fn, after); i < 0 || guardEnd > i {
			t.Errorf("blob(): the destination guard must precede %q:\n%s", after, fn)
		}
	}
	// No schema-maxlen comparison in the payload callback (#594) -- the one
	// comparison is fixlenBegin's, at the length word.
	if strings.Contains(fn, "above schema maxlen") {
		t.Errorf("blob(): the schema maxlen must be compared only at the length word:\n%s", fn)
	}
	// Every blob here declares a maxlen, so no receiver cap governs any of them:
	// the accumulator is handed Bound.SCHEMA_BOUNDED, which names the rule that
	// applies rather than a number it must not compare (CORELIB_PLAN §6.2.1).
	if !strings.Contains(fn, "byte[] _b = acc.blob(total, offset, data, chunkOffset, chunkLength, Bound.SCHEMA_BOUNDED);") {
		t.Errorf("a fully schema-bounded blob set must pass Bound.SCHEMA_BOUNDED:\n%s", fn)
	}
}

// The blob twin of TestJavaStringFreeSchemaNeverDecodesAString: a message that
// declares NO blob still gets the callback (Visitor declares it, and the corelib
// still routes blob fields at unknown ids to it), but every blob reaching it is
// skipped by definition, so the body must be empty. A guard whose every arm
// returns is the same thing said longer -- and Java rejects the unreachable
// statements that would follow it.
func TestJavaBlobFreeSchemaNeverCopiesABlob(t *testing.T) {
	files := genJavaFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: string, maxlen: 8 }
`, map[string]any{})
	fn := javaMethod(t, files["src/main/java/message/M.java"], "    public void blob(int id,")
	for _, forbidden := range []string{"acc.blob(", "acc", "switch (cur)", "byte[] _b"} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("a blob-free schema must not %q in blob():\n%s", forbidden, fn)
		}
	}
	if !strings.Contains(files["src/main/java/message/M.java"], "public void blob(int id,") {
		t.Errorf("blob() must still be declared -- Visitor requires it:\n%s", files["src/main/java/message/M.java"])
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
	if !strings.Contains(m, "Seq.checkIndex(id, SCHEMA_COUNT_3); if (total > 6) throw") {
		t.Error("a wrapper element must latch over-index and element maxlen")
	}
	// Exactly one comparison per bound (#594): the length word's. The payload
	// callbacks restate none of them.
	if n := strings.Count(m, "total > 8"); n != 1 {
		t.Errorf("a scalar maxlen must be compared exactly once, got %d", n)
	}
	if n := strings.Count(m, "total > 6"); n != 1 {
		t.Errorf("an element maxlen must be compared exactly once, got %d", n)
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
		`Seq.checkIndex(id, SCHEMA_COUNT_3); if (count > 4) throw Sofab.invalid("mat element: array count above schema capacity 4");`,
		`Seq.reserveRowInts(m.mat, id, count, `,
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

// TestJavaWrapperIndexCap: a DYNAMIC wrapper array's element index is bounded by
// the receiver cap, checked before the container grows (ARCHITECTURE §9.5,
// generator#387).
//
// A wrapper array carries no count header, so `max_dyn_array_count` never
// reached it: its elements are keyed by an unbounded varint index and the
// collector grows to id + 1, which makes a ~9-byte message with a single
// over-index element an arbitrarily large allocation. Capping how many elements
// ARRIVED would not close that -- gap filling (§5.1) means two delivered
// elements at id 0 and id 16383 are a 16384-slot container, so the index IS the
// length and the index is what has to be bounded.
//
// The category is LIMIT_EXCEEDED, not INVALID_MSG: the bytes are well formed and
// the same message decodes under a looser cap (CORELIB_PLAN §6.2.1).
func TestJavaWrapperIndexCap(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      dstrs: { id: 0, type: array, items: { type: string } }
      dblbs: { id: 1, type: array, items: { type: blob } }
      dobjs: { id: 2, type: array, items: { type: struct, fields: { x: { id: 0, type: u32 } } } }
      dmat:  { id: 3, type: array, items: { type: array, items: { type: u32 } } }
      bstrs: { id: 4, type: array, items: { type: string, count: 4 } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]

	// The gap fill is the CORELIB's for every element kind (generator#587), so the
	// cap travels as that call's last argument -- compared before anything is
	// created and before the list grows to hold it -- and the LIMIT_EXCEEDED
	// rejection is built there, not here.
	for _, want := range []string{
		"private static final Bound CAP_DYN_ARRAY_COUNT = Bound.receiver(MAX_DYN_ARRAY_COUNT);",
		`Seq.placeElem(m.dstrs, id, "", _s, CAP_DYN_ARRAY_COUNT);`,
		`Seq.placeElem(m.dblbs, id, Seq.EMPTY_BYTES, _b, CAP_DYN_ARRAY_COUNT);`,
		`Seq.reserveElem(m.dobjs, id, MDobjsElem::new, CAP_DYN_ARRAY_COUNT);`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing wrapper index cap %q:\n%s", want, m)
		}
	}
	// One implementation, wherever it runs (§6.2.1): no element index is ALSO
	// guarded here.
	for _, gone := range []string{
		`"Root_dstrs element: array index above configured limit`,
		`"Root_dblbs element: array index above configured limit`,
		`"Root_dobjs element: array index above configured limit`,
	} {
		if strings.Contains(m, gone) {
			t.Errorf("an element index must not be capped twice (%s):\n%s", gone, m)
		}
	}
	// A native matrix ROW is placed by the CORELIB, so its index cap is that
	// call's last argument instead -- compared before the row is allocated and
	// before the outer list grows (CORELIB_PLAN §6.2.1). The row's own element
	// count is a different number and stays a guard beside it (#386).
	if !strings.Contains(m, `if (count > MAX_DYN_ARRAY_COUNT) throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "dmat: array count above configured limit 65536")); askip = 0; afill = count; atgt = 1; _arowInt = Seq.reserveRowInts(m.dmat, id, count, CAP_DYN_ARRAY_COUNT);`) {
		t.Errorf("a native matrix row must pass its index cap to Seq.reserveRow*:\n%s", m)
	}
	// One implementation, wherever it runs (§6.2.1): the index the corelib now
	// bounds must not ALSO be guarded here.
	if strings.Contains(m, `"Root_dmat element: array index above configured limit`) {
		t.Errorf("the row index must not be capped twice:\n%s", m)
	}
	// A SCHEMA-BOUNDED array keeps its own bound and its own category: the cap
	// governs only what the schema left unbounded (§9.5), so `bstrs` is handed
	// Bound.schema(4) -- INVALID at 4 -- and not the cap.
	if !strings.Contains(m, `case 6: Seq.placeElem(m.bstrs, id, "", _s, SCHEMA_COUNT_4);`) ||
		!strings.Contains(m, `private static final Bound SCHEMA_COUNT_4 = Bound.schema(4);`) {
		t.Errorf("a count:N wrapper array must keep its INVALID schema bound:\n%s", m)
	}
	if strings.Contains(m, `"Root_bstrs element: array index above configured limit`) {
		t.Errorf("a schema-bounded array must not also carry the receiver cap:\n%s", m)
	}
}

// TestJavaReceiverCapIsPassedNotGuarded: §6.2.1 fixes the PROVENANCE of a
// receiver cap (generated code, always) but not the SITE of the comparison --
// "A corelib MAY take a limit as an argument and perform the check itself, and a
// port that does is conformant". corelib-java 0.12.0 does, on the three calls
// this backend already made at the point each limit guards, so the numbers
// travel as arguments and the guards in front of them are gone.
//
// The rule that makes this a test rather than a refactor is "one implementation,
// wherever it runs": a cap is a guard here or an argument there, never both.
func TestJavaReceiverCapIsPassedNotGuarded(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      ds:  { id: 0, type: string }
      bs:  { id: 1, type: string, maxlen: 16 }
      db:  { id: 2, type: blob }
      bb:  { id: 3, type: blob, maxlen: 8 }
      dm:  { id: 4, type: array, items: { type: array, items: { type: u32 } } }
      sm:  { id: 5, type: array, items: { type: array, items: { type: string } } }
`
	m := genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"]

	// MIXED destinations: the cap depends on which field the payload is headed
	// for, so it rides on the destination switch already resolving exactly that
	// -- one store, no dispatch of its own -- and defaults to SCHEMA_BOUNDED.
	for _, want := range []string{
		"Bound _lim = Bound.SCHEMA_BOUNDED;",
		"case 0: switch (id) { case 0: _lim = CAP_DYN_STRING_LEN; break; case 1: break; default: return; } break;",
		"String _s = acc.string(total, offset, data, chunkOffset, chunkLength, _lim);",
		"byte[] _b = acc.blob(total, offset, data, chunkOffset, chunkLength, _lim);",
		// blob() resolves its cap on the SAME gate since generator#436 gave it one:
		// only the unbounded `db` raises `_lim`, the maxlen'd `bb` falls through at
		// SCHEMA_BOUNDED, and an undeclared id or a §7.3 contradiction returns
		// before the accumulator -- a skipped field is never capped, and is never
		// materialized either.
		"case 0: switch (id) { case 2: _lim = CAP_DYN_BLOB_LEN; break; case 3: break; default: return; } break;",
		// A wrapper matrix row goes through the corelib, uncapped by generated code.
		"Seq.reserveRow(m.sm, id, CAP_DYN_ARRAY_COUNT);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q:\n%s", want, m)
		}
	}
	// The three guards this replaces must be gone -- a removed guard with nothing
	// in its place reads identically in a diff, which is what these pin.
	for _, gone := range []string{
		"if (total > MAX_DYN_STRING_LEN)",
		"if (total > MAX_DYN_BLOB_LEN)",
		`"Root_dm element: array index above configured limit`,
		`"Root_sm element: array index above configured limit`,
	} {
		if strings.Contains(m, gone) {
			t.Errorf("the cap must not ALSO be a generated guard (%q):\n%s", gone, m)
		}
	}
	// A schema-bounded field is never handed a receiver cap: its maxlen governs,
	// and its breach is INVALID (§7.1), so the guard for it stays right here.
	for _, want := range []string{
		`if (total > 16) throw Sofab.invalid("bs: string length above schema maxlen 16");`,
		`if (total > 8) throw Sofab.invalid("bb: blob length above schema maxlen 8");`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing the schema maxlen reject %q:\n%s", want, m)
		}
	}
}

// The RECEIVER CAP is latched at that same length word, and with its own
// category (CORELIB_PLAN §6.2.1 "Enforcement point", ARCHITECTURE §9.5).
//
// The schema-maxlen half above was already latched there. The cap half was not:
// it travelled only as an argument to acc.string/.blob, and those fire once a
// payload byte exists. So `02 a2 06` -- a length word declaring 100 bytes on a
// field capped at 8, then end of input -- reached no callback and answered
// INCOMPLETE, which §6.3 makes the wrong category (the refusal is terminal) and
// which invites a streaming caller to feed more of a stream this receiver has
// already refused.
//
// The comparison is still the corelib's -- PayloadAcc.checkStringLength is what
// acc.string runs at the top of itself -- so this is one implementation of the
// rule applied at two points, not a second copy in the generated layer.
func TestJavaFixlenBeginLatchesTheReceiverCapAtTheLengthWord(t *testing.T) {
	files := genJavaFromYAML(t, `version: 1
messages:
  m:
    payload:
      ds: { id: 0, type: string }
      bs: { id: 1, type: string, maxlen: 32 }
      db: { id: 2, type: blob }
      sa: { id: 3, type: array, items: { type: string } }
`, map[string]any{"max_dyn_string_len": 8, "max_dyn_blob_len": 8, "max_dyn_array_count": 4})
	var m string
	for path, src := range files {
		if strings.HasSuffix(path, "M.java") {
			m = src
		}
	}
	if m == "" {
		t.Fatal("no M.java")
	}
	fx := m[strings.Index(m, "public void fixlenBegin("):strings.Index(m, "public void string(")]
	if fx == "" {
		t.Fatal("no fixlenBegin implementation")
	}
	for _, want := range []string{
		// A schema-unbounded string and blob: the corelib's own check, at the header.
		"case 0: PayloadAcc.checkStringLength(total, CAP_DYN_STRING_LEN); break;",
		"case 2: PayloadAcc.checkBlobLength(total, CAP_DYN_BLOB_LEN); break;",
		// A schema-bounded one keeps INVALID and its own number: §6.2.1 forbids the
		// cap on a field the schema bounds, even a maxlen far above the cap.
		`case 1: if (total > 32) throw Sofab.invalid("bs: string length above schema maxlen 32"); break;`,
		// ...all of it behind the §7.3 declared-subtype gate.
		"if (subtype == FixlenType.STRING) {",
		"if (subtype == FixlenType.BLOB) {",
	} {
		if !strings.Contains(fx, want) {
			t.Errorf("fixlenBegin missing %q\ngot:\n%s", want, fx)
		}
	}
	// The cap is never re-implemented here: no bare comparison against the cap
	// constant, which would be the second implementation §6.2.1 forbids.
	if strings.Contains(fx, "total > MAX_DYN_STRING_LEN") || strings.Contains(fx, "total > MAX_DYN_BLOB_LEN") {
		t.Error("the cap comparison belongs to the corelib call, not to a guard emitted beside it (§6.2.1)")
	}
	// A wrapper element's length is capped at the header too.
	if !strings.Contains(fx, "PayloadAcc.checkStringLength(total, CAP_DYN_STRING_LEN); break;") {
		t.Error("a schema-unbounded wrapper element must be capped at the length word")
	}
	// A message whose every string the schema bounds meets no cap anywhere: the
	// exclusivity rule leaves nothing for the cap to govern, so no call is made
	// even though the project configures one.
	bounded := genJavaFromYAML(t, `version: 1
messages:
  m:
    payload:
      s: { id: 0, type: string, maxlen: 8 }
      b: { id: 1, type: blob, maxlen: 8 }
`, map[string]any{"max_dyn_string_len": 8, "max_dyn_blob_len": 8})
	for path, src := range bounded {
		if strings.HasSuffix(path, "M.java") &&
			(strings.Contains(src, "checkStringLength") || strings.Contains(src, "checkBlobLength")) {
			t.Error("a schema-bounded field must not meet the receiver cap (§6.2.1)")
		}
	}
}

// TestJavaDecoderAsksTheStreamForItsVerdict pins issue #541: the generated
// Decoder remembers NOTHING about the outcome. The stream already holds it — a
// refusal is terminal (CORELIB_PLAN §5.2 for malformed bytes, §6.3 for a receiver
// limit) and IStream.feed latches it on BOTH carriers, the bare SofabException
// and the UncheckedIOException a Visitor guard has to wrap in, re-throwing the
// very code it was refused with from every later call.
//
// #461 had this layer keep a second copy in `st`, with two catches mapping a
// refusal onto it. That copy could only restate what the stream held, and it
// flattened LIMIT_EXCEEDED into an INCOMPLETE that says something untrue about
// the wire. finish therefore ASKS, with a zero-length feed.
func TestJavaDecoderAsksTheStreamForItsVerdict(t *testing.T) {
	m := exampleFile(t)
	for _, want := range []string{
		// The one-shot needs no memory: feed's return IS the answer.
		"        return is.feed(data, new MyfirstmessageVisitor(out));",
		// Feed forwards and nothing more: no assignment, no catch.
		"            return feed(chunk, 0, chunk.length);",
		"            return is.feed(chunk, off, len, v);",
		// finish asks the stream and judges what it answers. It can now surface
		// the refusal's own code, so it declares the checked exception.
		"        public Myfirstmessage finish() throws SofabException {",
		"            DecodeStatus st = is.feed(new byte[0], 0, 0, v);",
		"            if (st != DecodeStatus.COMPLETE) {",
		"                    \"Myfirstmessage: stream ended mid-field (\" + st + \")\");",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Myfirstmessage.java missing %q (generator#541):\n%s", want, m)
		}
	}
	// Nothing remembers a status, and neither carrier maps a refusal onto one.
	// Each of these is a way the deleted latch could come back.
	for _, gone := range []string{
		"private DecodeStatus st =",
		"catch (SofabException e) {",
		"catch (java.io.UncheckedIOException e) {",
		"st = DecodeStatus.INVALID;",
		"st = DecodeStatus.INCOMPLETE;",
		"public DecodeStatus status()",
	} {
		if strings.Contains(m, gone) {
			t.Errorf("Myfirstmessage.java still carries the removed status latch %q (generator#541):\n%s", gone, m)
		}
	}
	// The accessor is gone from the corelib; asking the stream a second time
	// must not come back in any form (generator#461).
	if strings.Contains(m, "is.status()") {
		t.Errorf("Myfirstmessage.java still calls the removed IStream.status() (generator#461):\n%s", m)
	}
}

// jsonHelperFor generates the `emit: project` harness for the definition file
// def and returns Json.java, the generated JSON codec the conformance runner
// drives. The front-end pipeline itself is genJavaFromYAML's, so it is spelled
// out once in this file.
func jsonHelperFor(t *testing.T, def string) string {
	t.Helper()
	b, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range genJavaFromYAML(t, string(b), map[string]any{"package": "messages", "emit": "project"}) {
		if strings.HasSuffix(p, "Json.java") {
			return c
		}
	}
	t.Fatal("no Json.java in the project emit")
	return ""
}

// TestJavaBitfieldIsJSONUnsigned: the generated harness writes and reads a
// bitfield the way it writes and reads a u64, because that is what a bitfield is
// -- an unsigned 64-bit mask carried in a SIGNED Java `long` (generator#475).
//
// The signed arm it used to share with the small integers gets both halves
// wrong. Writing, `b.append(o.flags)` prints a mask with bit 63 set as -1, where
// python, go, rust, C#, kotlin, typescript and dart all print
// 18446744073709551615. Reading, `e.getAsLong()` ACCEPTS a JSON value at or
// above 2^64 and wraps it into a legal-looking mask -- Gson's
// LazilyParsedNumber.longValue() falls back to asBigDecimal().longValue(), which
// silently keeps the low 64 bits (measured on gson 2.11.0: a bare 2^64 read back
// as 0) -- where C# and kotlin reject it. The same arm THREW on the QUOTED
// spelling dart and typescript write for a wide mask, so the two halves of the
// asymmetry could not even meet.
//
// The dart twin is TestDartBitfieldReadsJSONAsUnsigned.
func TestJavaBitfieldIsJSONUnsigned(t *testing.T) {
	// bitfields.yaml declares LOW at pos 0 and HIGH at pos 63, so `flags` is a
	// mask that reaches the sign bit of its Java carrier.
	out := jsonHelperFor(t, "../../tests/matrix/corpus/defs/bitfields.yaml")
	for _, want := range []string{
		// WRITE: unsigned decimal, so bit 63 prints as 18446744073709551615.
		"b.append(Long.toUnsignedString(o.flags));",
		// READ: parseUnsignedLong REJECTS >= 2^64 with a NumberFormatException
		// instead of wrapping it, and still accepts a bare JSON number --
		// Gson's getAsString() on a numeric primitive returns the literal's own
		// spelling, so an unquoted `"flags":2` parses unchanged.
		"o.flags = Long.parseUnsignedLong(e.getAsString());",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Json.java missing %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{
		"b.append(o.flags);",       // signed write: a bit-63 mask prints -1
		"o.flags = e.getAsLong();", // signed read: >= 2^64 wraps instead of throwing
	} {
		if strings.Contains(out, gone) {
			t.Errorf("Json.java must not treat a bitfield as a SIGNED long: %q:\n%s", gone, out)
		}
	}

	// The same treatment on the ARRAY arms, and the narrow bitfield the
	// hand-written round-trip inputs in this tree spell (`"somebitfield":2`).
	// Java prints the unsigned spelling as a bare JSON number, so a mask that
	// fits stays byte-identical: Long.toUnsignedString(2L) is "2".
	ex := jsonHelperFor(t, "../../examples/messages/example.yaml")
	for _, want := range []string{
		"b.append(Long.toUnsignedString(o.somebitfield));",
		"o.somebitfield = Long.parseUnsignedLong(e.getAsString());",
		// The array is backed by the width the declaration implies (flagA/flagB at
		// pos 0 and 1, i.e. a u8), so the element is stored as RAW BITS and the
		// JSON writer masks it back to the value first -- exactly what an
		// array<u8> does, and the reason the mask may not be dropped: a u8-wide
		// bitfield carrying bit 7 is stored negative and denotes 128.
		"b.append(Long.toUnsignedString((o.somebitfieldarray[_i0] & 0xFFL)));",
		"o.somebitfieldarray[_k0] = (byte) Long.parseUnsignedLong(_a0.get(_k0).getAsString());",
	} {
		if !strings.Contains(ex, want) {
			t.Errorf("Json.java missing %q:\n%s", want, ex)
		}
	}
	// A bitfield's JSON stays an unquoted number on both sides -- the unsigned
	// spelling is appended raw, never through the string writer.
	if strings.Contains(ex, "Json.str(b, o.somebitfield") {
		t.Error("a bitfield must stay a bare JSON number, not a quoted string")
	}
	for _, gone := range []string{
		"b.append(o.somebitfield);",
		"o.somebitfield = e.getAsLong();",
		"b.append(o.somebitfieldarray[_i0]);",
		"o.somebitfieldarray[_k0] = _a0.get(_k0).getAsLong();",
	} {
		if strings.Contains(ex, gone) {
			t.Errorf("Json.java must not treat a bitfield as a SIGNED long: %q:\n%s", gone, ex)
		}
	}
}

// javaLongLiteralBits applies javac's own range rule to one Java `long` literal
// and returns the 64 bits it denotes. It is the whole point of
// TestJavaBitfieldDefaultIsALegalLongLiteral: a test that merely grepped for the
// emitted substring would pass just as happily on the decimal spelling javac
// rejects.
//
// JLS 3.10.1: a DECIMAL long literal denotes a value in [0, 2^63-1] -- only the
// unary minus that may precede it reaches -2^63 -- so 9223372036854775808L is
// the compile error "integer number too large". A HEX (or binary/octal) long
// literal is read as a 64-bit two's-complement PATTERN instead, so it spells
// every value up to 0xFFFFFFFFFFFFFFFFL.
func javaLongLiteralBits(lit string) (uint64, error) {
	body := strings.TrimSuffix(strings.TrimSuffix(lit, "L"), "l")
	switch {
	case strings.HasPrefix(body, "0x"), strings.HasPrefix(body, "0X"):
		return strconv.ParseUint(strings.ReplaceAll(body[2:], "_", ""), 16, 64)
	case strings.HasPrefix(body, "0b"), strings.HasPrefix(body, "0B"):
		return strconv.ParseUint(strings.ReplaceAll(body[2:], "_", ""), 2, 64)
	default:
		// Decimal: bounded by int64, exactly as javac bounds it.
		v, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
		return uint64(v), err
	}
}

// TestJavaBitfieldDefaultIsALegalLongLiteral: a bitfield default whose highest
// defaulted flag sits at bit 63 must still be SPELLABLE (generator#477).
//
// A bitfield is an unsigned 64-bit mask carried in a signed Java `long`. The
// backend used to render its default with `fmt.Sprintf(" = %dL", bits)` over a
// uint64, so a pos-63 `default: true` produced ` = 9223372036854775808L` -- past
// the top of a decimal long literal. Measured on javac 25.0.3, the emitted class
// did not compile at all, with one "integer number too large" per site:
//
//	Bf.java:8: error: integer number too large
//	    public long flags = 9223372036854775808L;
//
// There are FOUR such sites, because javaDefaultValue's one string is reused in
// the field initializer, the `!= default` omission compare in serialize(), the
// same compare in isDefault(), and reset() -- which is also why the fix is a hex
// literal rather than the Long.parseUnsignedLong the u64 arm used to emit: hex is
// a compile-time constant, so the two compares stay a bare lcmp against the
// constant pool instead of an invokestatic per call on a maxspeed target. The u64
// arm has since been held to the same rule; see
// TestJavaU64DefaultIsACompileTimeConstant (generator#479).
func TestJavaBitfieldDefaultIsALegalLongLiteral(t *testing.T) {
	const src = `
version: 1
messages:
  Bf:
    payload:
      flags: { id: 0, type: bitfield, bits: { LOW: { pos: 0, default: true }, HIGH: { pos: 63, default: true } } }
`
	const want = uint64(1)<<63 | 1 // HIGH | LOW

	var out string
	for p, c := range genJavaFromYAML(t, src, map[string]any{"package": "messages"}) {
		if strings.HasSuffix(p, "Bf.java") {
			out = c
		}
	}
	if out == "" {
		t.Fatal("no Bf.java generated")
	}

	// Every literal the default is spelled as, at all four sites.
	lits := regexp.MustCompile(`(?m)flags (?:= |!= )([0-9A-Fa-fxX_]+L)`).FindAllStringSubmatch(out, -1)
	if len(lits) != 4 {
		t.Fatalf("expected the default at 4 sites (initializer, serialize compare, isDefault compare, reset), got %d:\n%s",
			len(lits), out)
	}
	for _, m := range lits {
		bits, err := javaLongLiteralBits(m[1])
		if err != nil {
			t.Errorf("%q is not a long literal javac accepts (%v) -- the generated class does not compile", m[1], err)
			continue
		}
		if bits != want {
			t.Errorf("%q denotes 0x%X, want the declared mask 0x%X", m[1], bits, want)
		}
	}

	// The spelling itself, so the constant-fold argument above cannot be
	// silently traded away for a runtime parse.
	if !strings.Contains(out, "public long flags = 0x8000000000000001L;") {
		t.Errorf("Bf.java does not initialize the mask with a hex long literal:\n%s", out)
	}

	// A mask that FITS keeps a compact hex spelling and the same value; nothing
	// about the narrow case regressed into a wide one.
	const narrow = `
version: 1
messages:
  Bf:
    payload:
      flags: { id: 0, type: bitfield, bits: { LOW: { pos: 1, default: true }, HIGH: { pos: 63 } } }
`
	for p, c := range genJavaFromYAML(t, narrow, map[string]any{"package": "messages"}) {
		if strings.HasSuffix(p, "Bf.java") && !strings.Contains(c, "public long flags = 0x2L;") {
			t.Errorf("a narrow bitfield default should stay a plain hex literal:\n%s", c)
		}
	}
}

// TestJavaBitfieldArrayDefaultIsALegalLongLiteral: the same rule one level in.
//
// An ARRAY of a bitfield declaring pos 63 lowers to a Java `long[]`: the width
// its declaration implies is u64, which is the accumulator's own. Its default is
// rendered element by element by
// javaPrimElemLit -- which special-cased only ir.KindU64 and let a bitfield
// element fall through to a bare decimal. A default element with bit 63 set
// therefore emitted `new long[]{0x1L, 9223372036854775808L}`, which javac 25.0.3
// refused twice (the per-instance initializer and the hoisted _arrdef_ constant),
// so the class did not compile at all -- the same total failure the scalar arm
// had. Both now go through javaMaskLit.
func TestJavaBitfieldArrayDefaultIsALegalLongLiteral(t *testing.T) {
	const src = `
version: 1
messages:
  Bf2:
    payload:
      masks: { id: 0, type: array, items: { type: bitfield, bits: { LOW: { pos: 0 }, HIGH: { pos: 63 } } }, default: [1, 9223372036854775808] }
`
	want := []uint64{1, 1 << 63}

	var out string
	for p, c := range genJavaFromYAML(t, src, map[string]any{"package": "messages"}) {
		if strings.HasSuffix(p, "Bf2.java") {
			out = c
		}
	}
	if out == "" {
		t.Fatal("no Bf2.java generated")
	}

	// Every `new long[]{...}` the default is spelled as: the field initializer and
	// the hoisted _arrdef_ constant serialize/isDefault/reset compare against.
	inits := regexp.MustCompile(`new long\[\]\{([^}]*)\}`).FindAllStringSubmatch(out, -1)
	if len(inits) != 2 {
		t.Fatalf("expected the array default at 2 sites (field initializer, _arrdef_ constant), got %d:\n%s",
			len(inits), out)
	}
	for _, in := range inits {
		lits := strings.Split(in[1], ", ")
		if len(lits) != len(want) {
			t.Errorf("%q has %d elements, want %d", in[0], len(lits), len(want))
			continue
		}
		for i, lit := range lits {
			// The range rule javac itself applies -- a substring assertion would
			// have passed just as happily on the decimal that does not compile.
			bits, err := javaLongLiteralBits(lit)
			if err != nil {
				t.Errorf("element %d of %q is not a long literal javac accepts (%v) -- the generated class does not compile",
					i, in[0], err)
				continue
			}
			if bits != want[i] {
				t.Errorf("element %d of %q denotes 0x%X, want 0x%X", i, in[0], bits, want[i])
			}
		}
	}

	// And the spelling, so the per-instance initializer cannot be traded for a
	// Long.parseUnsignedLong call per element per object constructed.
	if !strings.Contains(out, "new long[]{0x1L, 0x8000000000000000L}") {
		t.Errorf("Bf2.java does not spell the array default in hex:\n%s", out)
	}
}

// TestJavaU64DefaultIsACompileTimeConstant: a `u64` default must be spelled as a
// long LITERAL at every site, not as a Long.parseUnsignedLong call (generator#479).
//
// javaDefaultValue hands javaInit's one string to five sites -- the field
// initializer, the `!= default` omission compare in serialize(), the same compare
// in isDefault(), reset(), and (for an array) once per element in the per-instance
// `new long[]{...}`. Two of those are compares and one of THOSE is the encode
// path, so on this maxspeed target the spelling has to be a compile-time constant.
// The parse is not one at any tier: javac emits `ldc` + `invokestatic
// Long.parseUnsignedLong` + `lcmp` where a literal is a bare `ldc2_w` + `lcmp`,
// and C2 does not fold it away either -- measured at 19.7-20.0 ns/op against
// 0.6-1.0 for the literal (200M iterations after 100M warmup, JDK 25.0.3).
//
// The assertions below are on the SHAPE of each emitted literal, via javac's own
// range rule; a substring check would pass just as happily on the parse call this
// replaced, and on the decimal that does not compile past 2^63-1.
func TestJavaU64DefaultIsACompileTimeConstant(t *testing.T) {
	const src = `
version: 1
messages:
  Wide:
    payload:
      big:    { id: 0, type: u64, default: 18446744073709551615 }
      small:  { id: 1, type: u64, default: 42 }
      bigarr: { id: 2, type: array, items: { type: u64, count: 3 }, default: ["18446744073709551615", 1, "9223372036854775808"] }
`
	var out string
	for p, c := range genJavaFromYAML(t, src, map[string]any{"package": "messages"}) {
		if strings.HasSuffix(p, "Wide.java") {
			out = c
		}
	}
	if out == "" {
		t.Fatal("no Wide.java generated")
	}

	// Nothing anywhere in the class may parse a default at run time -- not the
	// scalar sites, not an array element, not the hoisted _arrdef_ constant.
	if strings.Contains(out, "Long.parseUnsignedLong(") {
		t.Errorf("a u64 default is still parsed at run time:\n%s", out)
	}

	// Both scalar defaults, at all four of their sites, checked by value rather
	// than by spelling: a wide one has no decimal long literal and must be hex, a
	// narrow one keeps the schema's own decimal.
	for _, tc := range []struct {
		field string
		want  uint64
	}{
		{"big", math.MaxUint64},
		{"small", 42},
	} {
		re := regexp.MustCompile(`(?m)\b` + tc.field + ` (?:= |!= )([0-9A-Fa-fxX_]+L)`)
		lits := re.FindAllStringSubmatch(out, -1)
		if len(lits) != 4 {
			t.Errorf("expected %s at 4 sites (initializer, serialize compare, isDefault compare, reset), got %d:\n%s",
				tc.field, len(lits), out)
			continue
		}
		for _, m := range lits {
			bits, err := javaLongLiteralBits(m[1])
			if err != nil {
				t.Errorf("%q is not a long literal javac accepts (%v)", m[1], err)
				continue
			}
			if bits != tc.want {
				t.Errorf("%s spelled %q denotes 0x%X, want 0x%X", tc.field, m[1], bits, tc.want)
			}
		}
	}

	// The narrow default keeps the author's decimal, and the wide one -- which has
	// no decimal spelling at all -- goes to hex. Split at exactly 2^63-1.
	if !strings.Contains(out, "public long small = 42L;") {
		t.Errorf("a u64 default that fits in a signed long must keep the schema's decimal:\n%s", out)
	}
	if !strings.Contains(out, "public long big = 0xFFFFFFFFFFFFFFFFL;") {
		t.Errorf("a u64 default past 2^63-1 must be spelled in hex:\n%s", out)
	}

	// Both `new long[]{...}` sites: the per-instance field initializer and the
	// hoisted _arrdef_ constant. Per element, per object constructed, is where the
	// parse hurt most.
	wantElems := []uint64{math.MaxUint64, 1, 1 << 63}
	inits := regexp.MustCompile(`new long\[\]\{([^}]*)\}`).FindAllStringSubmatch(out, -1)
	if len(inits) != 2 {
		t.Fatalf("expected the array default at 2 sites (field initializer, _arrdef_ constant), got %d:\n%s",
			len(inits), out)
	}
	for _, in := range inits {
		lits := strings.Split(in[1], ", ")
		if len(lits) != len(wantElems) {
			t.Errorf("%q has %d elements, want %d", in[0], len(lits), len(wantElems))
			continue
		}
		for i, lit := range lits {
			bits, err := javaLongLiteralBits(lit)
			if err != nil {
				t.Errorf("element %d of %q is not a long literal javac accepts (%v)", i, in[0], err)
				continue
			}
			if bits != wantElems[i] {
				t.Errorf("element %d of %q denotes 0x%X, want 0x%X", i, in[0], bits, wantElems[i])
			}
		}
	}
	if !strings.Contains(out, "new long[]{0xFFFFFFFFFFFFFFFFL, 1L, 0x8000000000000000L}") {
		t.Errorf("the u64 array default is not spelled element-for-element as literals:\n%s", out)
	}

	// Re-spelling a value in hex is the one thing that loses what the schema said,
	// so the decimal is put back in the field's javadoc -- and only there, where a
	// line comment cannot break the two inline compares.
	for _, note := range []string{
		"Default 18446744073709551615: past 2^63-1, so it is spelled below as the hex long literal 0xFFFFFFFFFFFFFFFFL.",
		"Default [18446744073709551615, 1, 9223372036854775808]: the elements past 2^63-1 are spelled below as hex long literals.",
	} {
		if !strings.Contains(out, note) {
			t.Errorf("the javadoc does not carry the re-spelled default: %q\n%s", note, out)
		}
	}
	// A default that was NOT re-spelled gets no such note: nothing was hidden.
	if strings.Contains(out, "Default 42") {
		t.Errorf("a u64 default that keeps its decimal needs no javadoc note:\n%s", out)
	}
}

// genJavaFromYAMLNoValidate is genJavaFromYAML with parser.Validate SKIPPED.
// Exactly one test may use it, and only because #484 closed the front door on the
// shape it needs: a quoted non-decimal u64 array element no longer reaches any
// backend from a validated definition, and the renderer that must not echo one is
// still worth pinning -- the same reason generators/java/helpers.go keeps its two
// boxed arms correct although nothing reaches them either. Any other backend test
// that skipped validation would be asserting on a definition the tool refuses.
func genJavaFromYAMLNoValidate(t *testing.T, src string, cfg map[string]any) map[string]string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "dyn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Resolve(); err != nil {
		t.Fatal(err)
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

// TestJavaU64LeadingZeroDefaultIsNotOctal: a u64 default is emitted as the value
// the generator PARSED, never as the schema's raw text, because Java reads a
// leading-zero integer literal as OCTAL.
//
// The spelling that carries it is an ARRAY element. Until generator#484,
// internal/parser/validate.go's checkArrayElem accepted ANY string for a `u64`
// element with no format check at all, so "010" was schema-legal in exactly the
// place javaPrimElemLit renders per instance. #484 gave that arm the field-level
// rule, so this definition no longer validates -- which is why the test builds it
// through genJavaFromYAMLNoValidate. The renderer is kept honest all the same:
// this is defence in depth behind the validator, not a reachable shape.
//
// Echoing the text there would have been a silent value change, and #479 nearly
// was one: Long.parseUnsignedLong("010") is 10, while `010L` is 8 to javac
// (verified with javac 25.0.3 -- `System.out.println(010L)` prints 8) and `09L`
// is "illegal digit in an octal literal", which does not compile at all. The
// omission compare in serialize(), isDefault() and reset() would every one of
// them have read the wrong constant.
func TestJavaU64LeadingZeroDefaultIsNotOctal(t *testing.T) {
	const src = `
version: 1
messages:
  Oct:
    payload:
      arr: { id: 0, type: array, items: { type: u64, count: 3 }, default: ["010", "09", 1] }
`
	var out string
	for p, c := range genJavaFromYAMLNoValidate(t, src, map[string]any{"package": "messages"}) {
		if strings.HasSuffix(p, "Oct.java") {
			out = c
		}
	}
	if out == "" {
		t.Fatal("no Oct.java generated")
	}

	// By VALUE, through javac's own literal rule -- which is the only assertion
	// that catches this: `010L` is a perfectly legal long literal, so every shape
	// check in the test above passes just as happily on the broken output.
	wantElems := []uint64{10, 9, 1}
	inits := regexp.MustCompile(`new long\[\]\{([^}]*)\}`).FindAllStringSubmatch(out, -1)
	if len(inits) != 2 {
		t.Fatalf("expected the array default at 2 sites (field initializer, _arrdef_ constant), got %d:\n%s",
			len(inits), out)
	}
	for _, in := range inits {
		lits := strings.Split(in[1], ", ")
		if len(lits) != len(wantElems) {
			t.Errorf("%q has %d elements, want %d", in[0], len(lits), len(wantElems))
			continue
		}
		for i, lit := range lits {
			bits, err := javaLongLiteralBits(lit)
			if err != nil {
				t.Errorf("element %d of %q is not a long literal javac accepts (%v)", i, in[0], err)
				continue
			}
			if bits != wantElems[i] {
				t.Errorf("element %d of %q denotes %d, want %d -- the raw text was echoed and javac read it as octal",
					i, in[0], bits, wantElems[i])
			}
		}
	}
	// And the spelling, so the leading zero cannot come back by another route.
	if !strings.Contains(out, "new long[]{10L, 9L, 1L}") {
		t.Errorf("the leading zeros were not normalized away:\n%s", out)
	}
	if strings.Contains(out, "010L") || strings.Contains(out, "09L") {
		t.Errorf("an octal-looking literal survived into the generated class:\n%s", out)
	}
}

// widthSixSrc declares an `enum` and a `bitfield` at all SIX positions a value
// can land in, and both definitions are GAPPED on purpose: the enum declares
// {0, 1, 2, 10}, so 5 sits inside the implied width and is not a constant, and
// the bitfield declares positions 0, 1 and 3, so 4 sets a bit no flag declares.
// Both of those values are VALID under the width rule and were INVALID under the
// withdrawn closed-set one, which is what makes a gapped definition the shape
// that tells the two rules apart.
const widthSixSrc = `
version: 1
messages:
  Closed:
    payload:
      en:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
      bf:  { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      ea:  { id: 2, type: array, items: { type: enum, count: 4, enum: { A: 0, B: 1, C: 2, Z: 10 } } }
      bfa: { id: 3, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } } }
      st:
        id: 4
        type: struct
        fields:
          se:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
          sbf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      sa:
        id: 5
        type: array
        items:
          type: struct
          count: 2
          fields:
            se:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
            sbf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      un:
        id: 6
        type: union
        default_id: 0
        oneof:
          ue:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
          ubf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      mat: { id: 7, type: array, items: { type: array, count: 2, items: { type: enum, count: 3, enum: { A: 0, B: 1, C: 2, Z: 10 } } } }
      mbf: { id: 8, type: array, items: { type: array, count: 2, items: { type: bitfield, count: 3, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } } } }
`

// MESSAGE_SPEC §1 binds an `enum` to the width of the smallest SIGNED type
// holding every declared constant and a `bitfield` to the width of the smallest
// UNSIGNED type holding its highest declared `pos`. Here that is i8 (−128..127)
// for {0, 1, 2, 10} and u8 (0..255) for positions 0, 1 and 3.
//
// The bound is not the integer the target stores the field in. Java keeps every
// SCALAR-family position in a `long`, which is §1's fourth consequence — a
// receiver that cannot hold the field at exactly the declared width holds it
// wider and MUST enforce the width as an explicit check, because nothing about
// its storage will. The two ARRAY positions do hold it at the declared width
// (`byte[]` here), and the guard is emitted there all the same: it is the
// fallback for a corelib that declines the bulk offer, and a store that happens
// to be narrow enough is never what satisfies the rule.
//
// All six positions are pinned by name — scalar, native array element, struct
// member, struct-array element member, union member, matrix row element — for
// both kinds. Four of them share one emitted arm per kind, which is exactly why
// "the arm is shared" is not worth trusting after the next refactor.
func TestJavaEnumAndBitfieldWidthBoundAtEverySixPositions(t *testing.T) {
	m := genJavaFromYAML(t, widthSixSrc, map[string]any{})["src/main/java/message/Closed.java"]
	const enRej = `if (value < -128L || value > 127L) throw Sofab.invalid(`
	const bfRej = `if ((value & ~0xffL) != 0) throw Sofab.invalid(`
	for _, want := range []string{
		// 1. scalar
		`case 0: ` + enRej + `"en: value outside declared enum width"); m.en = value; break;`,
		`case 1: ` + bfRej + `"bf: value outside declared bitfield width"); m.bf = value; break;`,
		// 2. native array element — in the armed-fill arm, which is only reached
		// while arrayBegin has this array armed, so a bare scalar at an array id
		// stays a §7.3 skip rather than becoming a spurious INVALID.
		enRej + `"ea element: value outside declared enum width"); m.ea[ai++] = (byte) value;`,
		bfRej + `"bfa element: value outside declared bitfield width"); m.bfa[ai++] = (byte) value;`,
		// 3. struct member
		`case 0: ` + enRej + `"se: value outside declared enum width"); m.st.se = value; break;`,
		`case 1: ` + bfRej + `"sbf: value outside declared bitfield width"); m.st.sbf = value; break;`,
		// 4. struct-array element member
		`case 0: ` + enRej + `"se: value outside declared enum width"); m.sa.get(_ex_Root_sa).se = value; break;`,
		`case 1: ` + bfRej + `"sbf: value outside declared bitfield width"); m.sa.get(_ex_Root_sa).sbf = value; break;`,
		// 5. union member
		`case 0: ` + enRej + `"ue: value outside declared enum width"); m.un.ue = value; break;`,
		`case 1: ` + bfRej + `"ubf: value outside declared bitfield width"); m.un.ubf = value; break;`,
		// 6. matrix row element — the row cursor, not a field.
		enRej + `"Root_mat element: value outside declared enum width"); _arowByte[ai++] = (byte) value;`,
		bfRej + `"Root_mbf element: value outside declared bitfield width"); _arowByte[ai++] = (byte) value;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Closed.java: an enum/bitfield position stores without its §1 width bound, missing %q:\n%s", want, m)
		}
	}
	// The two ARRAY positions also ride the bulk offer, and that is not a hole in
	// the twelve stores above: the destination is a `byte[]`, which states the i8
	// and u8 the declarations imply, and the corelib refuses an element that does
	// not fit it (IStream.narrowI8 / narrowU8). The guards above stay put as the
	// fallback for a corelib that declines the offer.
	if !strings.Contains(m, "abulk = m.ea = new byte[count]; break;") {
		t.Errorf("an enum array must offer its narrow destination in bulk:\n%s", m)
	}
	if !strings.Contains(m, "abulk = m.bfa = new byte[count]; break;") {
		t.Errorf("a bitfield array must offer its narrow destination in bulk:\n%s", m)
	}
	// A matrix ROW is never the bulk destination -- it is a row cursor, not a
	// field -- so its elements keep coming through the guarded arm above.
	if strings.Contains(m, "abulk = _arowByte") {
		t.Errorf("a matrix row must not be offered in bulk:\n%s", m)
	}
	// No bare store may remain on any of the twelve paths.
	for _, bad := range []string{
		"case 0: m.en = value; break;",
		"case 1: m.bf = value; break;",
		"case 0: m.st.se = value; break;",
		"case 1: m.un.ubf = value; break;",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Closed.java still stores an enum/bitfield unguarded (%q):\n%s", bad, m)
		}
	}
}

// The elisions under the width rule: a guard is emitted only where the implied
// width is NARROWER than the 64-bit accumulator the value arrives in. A bitfield
// whose highest declared position is 63 implies u64, and an enum needing more
// than i32 implies i64 — in both cases nothing reachable can breach the bound and
// the clause would be dead code.
//
// Note what is NOT an elision any more: whether an enum's constants are
// contiguous no longer matters at all. The width is derived from the extremes, so
// {0,1,2} and {0,1,2,10} produce the identical i8 guard, and the membership chain
// a gapped set used to need is gone.
func TestJavaEnumBitfieldWidthElisions(t *testing.T) {
	var bits []string
	for i := 0; i < 64; i++ {
		bits = append(bits, fmt.Sprintf("F%d: { pos: %d }", i, i))
	}
	m := genJavaFromYAML(t, "version: 1\nmessages:\n  W:\n    payload:\n"+
		"      f: { id: 0, type: bitfield, bits: { "+strings.Join(bits, ", ")+" } }\n"+
		"      e: { id: 1, type: enum, enum: { R: 0, G: 1, B: 2 }, default: 0 }\n",
		map[string]any{})["src/main/java/message/W.java"]
	if !strings.Contains(m, "case 0: m.f = value; break;") {
		t.Errorf("an all-bits-declared bitfield must store unguarded:\n%s", m)
	}
	if strings.Contains(m, "0xffffffffffffffff") {
		t.Errorf("a tautological mask guard was emitted:\n%s", m)
	}
	// {R:0, G:1, B:2} implies i8, NOT the 0..2 hull of its constants: 5 is a
	// valid wire value for this field and must decode.
	if !strings.Contains(m, `case 1: if (value < -128L || value > 127L) throw Sofab.invalid("e: value outside declared enum width"); m.e = value; break;`) {
		t.Errorf("a contiguous enum must take the implied i8 width, not its constant hull:\n%s", m)
	}
}

// A bitfield declaring position 63 implies u64 — the accumulator's own width —
// so under the width rule it carries NO guard at all. Under the withdrawn
// closed-set rule the same declaration emitted a `~0x8000000000000001L` mask,
// which is the literal-rendering trap generator#470 hit once; deriving the bound
// from the highest position instead removes both the trap and the comparison.
func TestJavaBitfieldSpanningBit63IsUnguarded(t *testing.T) {
	m := genJavaFromYAML(t, "version: 1\nmessages:\n  W:\n    payload:\n"+
		"      g: { id: 0, type: bitfield, bits: { LOW: { pos: 0 }, HIGH: { pos: 63 } } }\n",
		map[string]any{})["src/main/java/message/W.java"]
	if !strings.Contains(m, "case 0: m.g = value; break;") {
		t.Errorf("a bitfield implying the full u64 width must store unguarded:\n%s", m)
	}
	if strings.Contains(m, "0x8000000000000001") {
		t.Errorf("the withdrawn flag-mask guard was emitted:\n%s", m)
	}
}

// The behavioural difference the width rule makes, stated as the values
// themselves: a gapped enum admits a value between its constants, and a bitfield
// admits an undeclared bit — both INVALID under the withdrawn closed-set rule.
// Pinned on the emitted bound so a silent reversion is loud.
func TestJavaWidthAdmitsUndeclaredValues(t *testing.T) {
	m := genJavaFromYAML(t, widthSixSrc, map[string]any{})["src/main/java/message/Closed.java"]
	// enum {0,1,2,10}: the guard must admit 5 — i.e. be the i8 interval, never a
	// membership chain over the constants.
	if strings.Contains(m, "value != 10L") {
		t.Errorf("the withdrawn membership chain over enum constants was emitted:\n%s", m)
	}
	// bitfield pos{0,1,3}: the guard must admit 4 — i.e. mask the WIDTH (0xff),
	// never the flag mask (0xb).
	if strings.Contains(m, "~0xbL") {
		t.Errorf("the withdrawn flag-mask guard was emitted:\n%s", m)
	}
	if !strings.Contains(m, "~0xffL") {
		t.Errorf("the bitfield width mask is missing:\n%s", m)
	}
}

// An enum/bitfield ARRAY is backed by the Java primitive whose width its
// DECLARATION implies (MESSAGE_SPEC §1), exactly as an array<i8> is backed by a
// byte[] and an array<u8> by the same byte[] holding raw bits. Under the
// withdrawn closed-set rule neither kind had a declared width, so both sat on
// `long[]` — eight bytes for an element the schema bounds at one.
//
// Pinned per implied width, on both sides of every step, because the mapping is
// derived rather than named: an enum by the smallest SIGNED type holding every
// constant, a bitfield by the smallest UNSIGNED type holding its highest `pos`.
// The schema validator caps enum constants at signed 32 bits, so i64 is
// unreachable from a declaration and the enum table stops at int.
//
// The bulk decision is pinned in the same table, because it follows from exactly
// this mapping: the offer's only bound is the destination's width, so it may be
// taken wherever that width IS the declared one — which, now that the two kinds
// have one, is every integer element.
func TestJavaEnumBitfieldArrayElementWidth(t *testing.T) {
	for _, tc := range []struct {
		name, decl, want string
		bulk             bool
	}{
		// enum: the extremes decide, so one constant at the edge of a width is
		// enough to take the array up to it.
		{"enum_i8", `{ type: enum, count: 4, enum: { A: -128, B: 127 } }`, "byte", true},
		{"enum_i16", `{ type: enum, count: 4, enum: { A: 0, B: 128 } }`, "short", true},
		{"enum_i16_low", `{ type: enum, count: 4, enum: { A: -129, B: 0 } }`, "short", true},
		{"enum_i32", `{ type: enum, count: 4, enum: { A: 0, B: 32768 } }`, "int", true},
		{"enum_i32_max", `{ type: enum, count: 4, enum: { A: 0, B: 2147483647 } }`, "int", true},
		// bitfield: the highest declared position decides.
		{"bf_u8", `{ type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 7 } } }`, "byte", true},
		{"bf_u16", `{ type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 8 } } }`, "short", true},
		{"bf_u32", `{ type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 16 } } }`, "int", true},
		// u64 is the accumulator's own width: `long[]` states it, and there is no
		// bound left for the offer to lose — the position array<u64> is in.
		{"bf_u64", `{ type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 32 } } }`, "long", true},
		{"bf_u64_top", `{ type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 63 } } }`, "long", true},
		// The references the derivation has to match, narrow and wide.
		{"i8_ref", `{ type: i8, count: 4 }`, "byte", true},
		{"u8_ref", `{ type: u8, count: 4 }`, "byte", true},
		{"u64_ref", `{ type: u64, count: 4 }`, "long", true},
		// fp is the one integer-array offer still declined: those elements arrive
		// through the decoder's fixlen loop, which the offer does not cover.
		{"fp32_ref", `{ type: fp32, count: 4 }`, "float", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := genJavaFromYAML(t, "version: 1\nmessages:\n  W:\n    payload:\n"+
				"      a: { id: 0, type: array, items: "+tc.decl+" }\n",
				map[string]any{})["src/main/java/message/W.java"]
			if want := "public " + tc.want + "[] a = "; !strings.Contains(m, want) {
				t.Errorf("the field is not backed by %s[]; missing %q:\n%s", tc.want, want, m)
			}
			alloc := "m.a = new " + tc.want + "[count]"
			if tc.bulk {
				if !strings.Contains(m, "abulk = "+alloc) {
					t.Errorf("a %s[] destination states the declared width, so the bulk offer must be taken:\n%s", tc.want, m)
				}
			} else {
				if strings.Contains(m, "abulk") {
					t.Errorf("the bulk offer must not be made for a %s[] destination:\n%s", tc.want, m)
				}
				if !strings.Contains(m, alloc) {
					t.Errorf("declining the offer must not change how the destination is sized:\n%s", m)
				}
			}
		})
	}
}

// The unsigned bargain, one level in: a narrowed BITFIELD array holds the
// declared width's RAW BITS, so an element with the top bit of that width set
// reads back negative and has to be zero-extended wherever it leaves the field
// as a VALUE. That is exactly what an array<u8> already does, and an ENUM array
// is the signed counterpart that needs nothing, exactly like an array<i8>.
func TestJavaNarrowBitfieldArrayIsWidenedForJSON(t *testing.T) {
	files := genJavaFromYAML(t, "version: 1\nmessages:\n  W:\n    payload:\n"+
		"      bf:   { id: 0, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 7 } } } }\n"+
		"      bf16: { id: 1, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 8 } } } }\n"+
		"      wide: { id: 2, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, H: { pos: 63 } } } }\n"+
		"      en:   { id: 3, type: array, items: { type: enum, count: 4, enum: { A: 0, B: 1 } } }\n"+
		"      u8:   { id: 4, type: array, items: { type: u8, count: 4 } }\n",
		map[string]any{"emit": "project"})
	j := files["src/main/java/message/Json.java"]
	if j == "" {
		t.Fatal("no Json.java generated")
	}
	for _, want := range []string{
		// A u8-wide bitfield element of 200 is stored as -56 and denotes 200.
		"b.append(Long.toUnsignedString((o.bf[_i0] & 0xFFL)));",
		"b.append(Long.toUnsignedString((o.bf16[_i0] & 0xFFFFL)));",
		// The u8 array this is modelled on, so the comparison is on the record.
		"b.append(Long.toUnsignedString((o.u8[_i0] & 0xFFL)));",
		// A long-backed bitfield already holds the value's own bits.
		"b.append(Long.toUnsignedString(o.wide[_i0]));",
		// An enum width is SIGNED: the narrowing was exact, so nothing is masked.
		"b.append(o.en[_i0]);",
		// And back in: the JSON carries the value, the field holds the bits.
		"o.bf[_k0] = (byte) Long.parseUnsignedLong(_a0.get(_k0).getAsString());",
		"o.en[_k0] = (byte) _a0.get(_k0).getAsLong();",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("Json.java missing %q:\n%s", want, j)
		}
	}
	// The mask may not leak onto the signed side: that would turn an enum's -1
	// into 4294967295 on the way out.
	if strings.Contains(j, "o.en[_i0] & 0x") {
		t.Errorf("an enum array element must not be masked -- its width is signed:\n%s", j)
	}
}

// TestJavaDeprecationIsSuppressedOnlyWhereItIsRead: the generated code builds
// under javac -Xlint:all -Werror. A @Deprecated field is written by the visitor
// (a separate top-level class) and round-tripped by the JSON harness, both of
// which javac flags as [deprecation]; the suppression sits on exactly those
// sites, and only when a field they touch is deprecated -- nested scopes
// included. The bench sink never picks a deprecated field and spells a keyword
// field through javaIdent. The generated pom compiles with -Xlint:all.
func TestJavaDeprecationIsSuppressedOnlyWhereItIsRead(t *testing.T) {
	const src = `
version: 1
$defs:
  struct:
    Inner: { old: { id: 0, type: u8, deprecated: true } }
messages:
  Outer:
    payload:
      inner: { id: 0, type: struct, fields: { $ref: "#/$defs/struct/Inner" } }
  Plain:
    payload:
      gone: { id: 0, type: u16, deprecated: true }
      int:  { id: 1, type: u32 }
  Clean:
    payload:
      a: { id: 0, type: u8 }
`
	files := genJavaFromYAML(t, src, map[string]any{"package": "p", "emit": "project"})
	const ann = `@SuppressWarnings("deprecation")`
	for _, name := range []string{"Outer", "Plain"} {
		src := files["src/main/java/p/"+name+".java"]
		if !strings.Contains(src, ann+" // decode must still fill deprecated fields\nclass "+name+"Visitor") {
			t.Errorf("%sVisitor writes a deprecated field but carries no deprecation suppression", name)
		}
	}
	if c := files["src/main/java/p/Clean.java"]; strings.Contains(c, "SuppressWarnings") {
		t.Error("Clean touches no deprecated field and must carry no suppression")
	}
	js := files["src/main/java/p/Json.java"]
	sup := "    " + ann + " // the harness round-trips deprecated fields too\n"
	for _, head := range []string{"static void to(StructInner ", "static void from(JsonObject j, StructInner ", "static void to(Plain ", "static void from(JsonObject j, Plain "} {
		if !strings.Contains(js, sup+"    "+head) {
			t.Errorf("Json.java: %q is not preceded by the deprecation suppression", head)
		}
	}
	for _, head := range []string{"static void to(Outer ", "static void to(Clean "} {
		if strings.Contains(js, sup+"    "+head) {
			t.Errorf("Json.java: %q touches no deprecated field directly and must not be suppressed", head)
		}
	}
	main := files["src/main/java/p/Main.java"]
	if !strings.Contains(main, "benchSink ^= Plain.decode(wire).int_;") {
		t.Errorf("bench sink must skip the deprecated field and spell the keyword field via javaIdent:\n%s", javaMethod(t, main, "private static void benchOp_plain("))
	}
	if !strings.Contains(files["pom.xml"], "<arg>-Xlint:all</arg>") {
		t.Error("pom.xml must compile with -Xlint:all")
	}
}
