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
		"public long[] someuintarray = new long[]{0L, 1L, 1000L, 4294967295L};",                       // primitive array (was List<Long>)
		"public float[] somefloatarray = new float[]{0.0f, -1.5f, 3.25f};",                            // primitive fp array
		"public long[] someenumarray = new long[]{2L, 1L, 0L};",                                       // declared default, NOT padded to count (count is a capacity)
		"os.writeArrayUnsigned(15, this.someuintarray);",                                              // direct write, no Sbuf box, no trim: the wire count IS the length
		"private static final long[] _arrdef_someuintarray = new long[]{0L, 1L, 1000L, 4294967295L};", // omit-default hoisted to a static (#146)
		"if (!java.util.Arrays.equals(this.someuintarray, _arrdef_someuintarray)) {",                  // guard reads the static -- no per-encode new long[] (#146)
		"m.someuintarray = ensureCap(m.someuintarray, ai, acap); m.someuintarray[ai++] = value;",      // grow-on-demand indexed decode (#96)
		"case 15: if (kind != ArrayKind.UNSIGNED) break; if (count > 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"someuintarray: array count above schema capacity 4\")); m.someuintarray = new long[Math.min(count, ARRAY_INIT_CAP)]; break;", // mis-typed header skipped before the bound (#254); over-count rejected (#100); the M that arrived is the whole value
		"private static long[] ensureCap(long[] a, int i, int cap) {",   // lazy-growth helper
		"private static float[] ensureCap(float[] a, int i, int cap) {", // fp32 overload
		"if (offset == 0 && chunkLength >= total) {",                    // string/blob single-shot
		"public List<Boolean> someboolarray",                            // boolean array stays boxed List
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Myfirstmessage.java missing %q", want)
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
		`case 1: if (kind != ArrayKind.UNSIGNED) break; if (count > MAX_DYN_ARRAY_COUNT) throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, "arr: array count above configured limit 4")); m.arr = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// Bounded array: only the generator#100 schema guard, never the cap. Both
		// bounds sit BEHIND the §7.3 kind test (generator#254).
		`case 2: if (kind != ArrayKind.SIGNED) break; if (count > 6) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "barr: array count above schema capacity 6")); m.barr = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
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
		// Armed at the top of arrayBegin, one arm per wire array kind (#254).
		"        askip = 0;\n        afill = 0;\n        if (kind == ArrayKind.UNSIGNED) {\n            askip = count;\n            switch (cur) {",
		"        else if (kind == ArrayKind.SIGNED) {\n            askip = count;\n            switch (cur) {",
		// Each declared array disarms the skip AND arms the fill under ITS OWN kind:
		// the u32 array (id 2) under UNSIGNED, the i32 array (id 3) under SIGNED,
		// the fp32 array (id 4) under FP32.
		"                case 2: askip = 0; afill = count; break;",
		"                case 3: askip = 0; afill = count; break;",
		"        else if (kind == ArrayKind.FP32) {\n            askip = count;\n            switch (cur) {",
		"                case 4: askip = 0; afill = count; break;",
		// The FP64 arm exists too and arms the counter unconditionally: this schema
		// declares no fp64 array, so an fp64 header at ANY id discards its elements.
		"        else if (kind == ArrayKind.FP64) {\n            askip = count;\n            switch (cur) {",
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
	// The fp32 array is armed in the FP32 branch, never grouped with the integer
	// arms — id 4 must not appear alongside ids 2/3 under UNSIGNED/SIGNED.
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
		// The kind test fronts the allocation AND precedes the schema bound.
		`case 0: if (kind != ArrayKind.UNSIGNED) break; if (count > 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "ua: array count above schema capacity 5")); m.ua = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		`case 1: if (kind != ArrayKind.SIGNED) break; if (count > 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "ia: array count above schema capacity 5")); m.ia = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		`case 2: if (kind != ArrayKind.FP32) break; if (count > 3) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "fa: array count above schema capacity 3")); m.fa = new float[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// A boolean array is a List: clearing it is decoding into it too, so the
		// kind test fronts the clear as well. boolean maps to the UNSIGNED kind.
		`case 3: if (kind != ArrayKind.UNSIGNED) break; if (count > 2) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "ba: array count above schema capacity 2")); m.ba.clear(); break;`,
		// enum elements ride the SIGNED wire type.
		`case 4: if (kind != ArrayKind.SIGNED) break; if (count > 2) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "ea: array count above schema capacity 2")); m.ea = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// A count-less array has no schema bound, but still gets the kind test.
		`case 5: if (kind != ArrayKind.UNSIGNED) break; m.da = new long[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		// The skip counter is armed per kind: each id disarms under its own kind only.
		"        if (kind == ArrayKind.UNSIGNED) {\n            askip = count;",
		"        else if (kind == ArrayKind.SIGNED) {\n            askip = count;",
		"                case 0: case 3: case 5: askip = 0; afill = count; break;",
		"                case 1: case 4: askip = 0; afill = count; break;",
		"                case 2: askip = 0; afill = count; break;",
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
		"        else if (kind == ArrayKind.FP32) {\n            askip = count;\n            switch (cur) {",
		"        else if (kind == ArrayKind.FP64) {\n            askip = count;\n            switch (cur) {",
		// The kind test fronts the allocation and the schema bound sits behind it.
		`case 0: if (kind != ArrayKind.FP32) break; if (count > 3) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "fa: array count above schema capacity 3")); m.fa = new float[Math.min(count, ARRAY_INIT_CAP)]; break;`,
		`case 1: if (kind != ArrayKind.FP64) break; if (count > 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "da: array count above schema capacity 4")); m.da = new double[Math.min(count, ARRAY_INIT_CAP)]; break;`,
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
	files := genJavaFromYAML(t, src, map[string]any{})
	m := files["src/main/java/message/M.java"]
	sbuf := files["src/main/java/message/Sbuf.java"]

	for _, want := range []string{
		// --- encode: every element the value holds is written, count or no count.
		"os.writeArrayUnsigned(0, this.fu);",
		"os.writeArraySigned(1, this.fi);",
		"os.writeArrayFp32(2, this.ff32);",
		"os.writeArrayFp64(3, this.ff64);",
		"os.writeArrayUnsigned(4, Sbuf.boolToLongArray(this.fb));",
		"os.writeArraySigned(5, this.fe);",    // enum -> signed
		"os.writeArrayUnsigned(6, this.fbf);", // bitfield -> unsigned
		// --- decode: a count:N array is filled exactly like a count-less one, from
		// the M elements that arrived; the schema count only bounds M.
		"m.fu = new long[Math.min(count, ARRAY_INIT_CAP)]",
		"m.ff32 = new float[Math.min(count, ARRAY_INIT_CAP)]",
		"m.ff64 = new double[Math.min(count, ARRAY_INIT_CAP)]",
		"m.fb.clear()",
		"m.fb.add(value != 0);",
		// --- the over-count guard (#100) still rejects M > N.
		`if (count > 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "fu: array count above schema capacity 5"));`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("M.java missing %q", want)
		}
	}

	for _, unwanted := range []string{
		// The whole trim-on-encode / fill-on-decode pair is gone.
		"Sbuf.trimTail", "Sbuf.fillFalse", "Sbuf.padTo",
		"acap = 5;", // no materialization at the schema count
		"m.fu = new long[5]", "m.ff32 = new float[5]", "m.ff64 = new double[5]",
		"m.fb.set(ai++", // a boolean array is grown, never overwritten by index
	} {
		if strings.Contains(m, unwanted) {
			t.Errorf("M.java must not contain %q (count is a capacity, not a length)", unwanted)
		}
	}

	// Dynamic arrays were already right and must be untouched.
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

	// A native ROW of a matrix carries no frame of its own, so the §2 element rule
	// lands on the write: an interior row equal to the element default (the empty
	// row) is not written at all, and the last row always is.
	if !strings.Contains(m, "if (!_e0.isEmpty() || _i0 == _t1.size() - 1) {") {
		t.Errorf("a native matrix row must take the interior/last write guard:\n%s", m)
	}

	// The generated support class ships neither trim nor fill any more.
	for _, gone := range []string{"trimTail", "fillFalse", "padTo"} {
		if strings.Contains(sbuf, gone) {
			t.Errorf("Sbuf.java must not still ship %q", gone)
		}
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
		"public long[] fu = Sbuf.EMPTY_LONGS;",
		"public float[] ff32 = Sbuf.EMPTY_FLOATS;",
		"public double[] ff64 = Sbuf.EMPTY_DOUBLES;",
		"public List<Boolean> fb = new ArrayList<>();",
		"public long[] fe = Sbuf.EMPTY_LONGS;",  // enum -> long[]
		"public long[] fbf = Sbuf.EMPTY_LONGS;", // bitfield -> long[]
		// --- count:N with a short schema default: as written, no tail padding.
		"public long[] pu = new long[]{1L, 2L};",
		"public List<Boolean> pb = new ArrayList<>(List.of(true, true));",
		"public float[] pf32 = new float[]{1.5f};",
		// --- count:N wrapper arrays start empty too.
		"public List<String> fstr = new ArrayList<>();",
		"public List<MFobjElem> fobj = new ArrayList<>();",
		// --- dynamic: unchanged.
		"public long[] du = Sbuf.EMPTY_LONGS;",
		"public List<Boolean> db = new ArrayList<>();",
		// --- with no declared default the omit guard is plain emptiness, so an
		// all-zero N-element value is NOT default and stays on the wire.
		"if (this.fu != null && this.fu.length != 0) {",
		"if (this.fb != null && !this.fb.isEmpty()) {",
		// --- a declared default is still hoisted to a static and compared whole (#146).
		"private static final long[] _arrdef_pu = new long[]{1L, 2L};",
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
		"        this.strs = Sbuf.resetList(this.strs);",
		"        this.dyn = Sbuf.EMPTY_LONGS;",
		// A fixed-count array is refilled from the shared default without allocating.
		"        if (this.fixd != null && this.fixd.length == _arrdef_fixd.length) System.arraycopy(_arrdef_fixd, 0, this.fixd, 0, _arrdef_fixd.length);",
		"        else this.fixd = _arrdef_fixd.clone();",
		"        this.bools = Sbuf.resetList(this.bools);\n        this.bools.addAll(_arrdef_bools);",
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
		"        this.inner = Sbuf.resetList(this.inner);",
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
		"List<VecFixedElem> _t0 = Sbuf.orEmpty(this.fixed);",
		"List<VecDynamicElem> _t1 = Sbuf.orEmpty(this.dynamic);",
		"List<String> _t2 = Sbuf.orEmpty(this.fstrs);",
		"for (int _i0 = 0; _i0 < _t0.size(); _i0++) { os.writeSequenceBeginLazy(_i0);",
		// A sequence-form element takes the POSITIONAL closer: dropping in the
		// interior (where an all-default element becomes an id gap), keeping at the
		// last index. Identical for the count:N and the count-less array.
		"(_t0.get(_i0) == null ? new VecFixedElem() : _t0.get(_i0)).serialize(os); if (_i0 == _t0.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
		"(_t1.get(_i0) == null ? new VecDynamicElem() : _t1.get(_i0)).serialize(os); if (_i0 == _t1.size() - 1) os.writeSequenceEndKeep(); else os.writeSequenceEnd();",
		// A leaf element: the same rule, unconditional now rather than count-gated.
		`String _e0 = _t2.get(_i0); if (_e0 == null) _e0 = ""; if (!_e0.isEmpty() || _i0 == _t2.size() - 1) os.writeString(_i0, _e0);`,
		`String _e0 = _t3.get(_i0); if (_e0 == null) _e0 = ""; if (!_e0.isEmpty() || _i0 == _t3.size() - 1) os.writeString(_i0, _e0);`,
		`byte[] _e0 = _t4.get(_i0); if (_e0 == null) _e0 = Sbuf.EMPTY_BYTES; if (_e0.length != 0 || _i0 == _t4.size() - 1) os.writeBlob(_i0, _e0);`,
		// A NATIVE row has no frame of its own, so the rule lands on the write.
		"if (!_e0.isEmpty() || _i0 == _t5.size() - 1) {",
		"os.writeArrayUnsigned(_i0, Sbuf.toLongArray(_e0));",
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
		"if (!Sbuf.orEmpty(this.fixed).isEmpty()) return false;",
		"if (!Sbuf.orEmpty(this.dynamic).isEmpty()) return false;",
		"if (!Sbuf.orEmpty(this.fstrs).isEmpty()) return false;",
		"if (!Sbuf.orEmpty(this.mat).isEmpty()) return false;",
		"if (!Sbuf.orEmpty(this.smat).isEmpty()) return false;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, got)
		}
	}

	// The superseded narrowing is gone from the generated code and from Sbuf.
	for _, gone := range []string{"trimTailObjs", "trimTailStrings", "trimTailBlobs", "trimTailRows"} {
		if strings.Contains(got, gone) {
			t.Errorf("Vec.java must not still narrow with %q:\n%s", gone, got)
		}
		if strings.Contains(files["src/main/java/message/Sbuf.java"], gone) {
			t.Errorf("Sbuf.java must not still ship %q", gone)
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
		`if (id >= 5) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_fixed element: array index above schema capacity 5"));`,
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
		`case 8: if (kind != ArrayKind.UNSIGNED) break; if (id >= 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_mat element: array index above schema capacity 4")); Sbuf.placeRow(m.mat, id); _ex_Root_mat = id; break;`,
		// and the elements land in the row that id named
		"m.mat.get(_ex_Root_mat).add(value); break;",
		// wrapper rows: placed in sequenceBegin, same shape
		`case 9: if (id >= 4) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "Root_smat element: array index above schema capacity 4")); Sbuf.placeRow(m.smat, id); _ex_Root_smat = id; cur = 10; break;`,
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

	// The helper itself: grow with empty rows, then REPLACE at the id (an array
	// wrapper is the array's value, §7.4).
	sbuf := genJavaFromYAML(t, wrapperArraySrc, map[string]any{})["src/main/java/message/Sbuf.java"]
	for _, want := range []string{
		"static <T> void placeRow(List<List<T>> l, int id) {",
		"while (l.size() <= id) l.add(new java.util.ArrayList<>());",
		"l.set(id, new java.util.ArrayList<>());",
	} {
		if !strings.Contains(sbuf, want) {
			t.Errorf("Sbuf.java missing %q:\n%s", want, sbuf)
		}
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
		"public long[] nums = Sbuf.EMPTY_LONGS;",
		"public List<byte[]> blobs = new ArrayList<>();",
		"public List<MObjsElem> objs = new ArrayList<>();",
		"public List<String> dyn = new ArrayList<>();",
		// reset() re-arms to the same value, in place, and adds nothing.
		"        this.strs = Sbuf.resetList(this.strs);\n        this.nums = Sbuf.EMPTY_LONGS;",
		"        this.objs = Sbuf.resetList(this.objs);\n        this.dyn = Sbuf.resetList(this.dyn);",
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
// whether a payload is read. It used to call _utf8() first and dispatch on
// (cur, id) second, so a lone continuation byte at an id the scope does not
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
	for _, after := range []string{"_utf8(", "acc"} {
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
	for _, forbidden := range []string{"_utf8(", "acc", "switch (cur)", "String _s;"} {
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
		`case 0: if (value < 0 || value > 255L) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "a_u8: value outside declared width u8")); m.a_u8 = value; break;`,
		`case 2: if (value < 0 || value > 4294967295L) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "c_u32: value outside declared width u32")); m.c_u32 = value; break;`,
		`case 4: if (value < -128L || value > 127L) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "e_i8: value outside declared width i8")); m.e_i8 = value; break;`,
		`case 6: if (value < -2147483648L || value > 2147483647L) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "g_i32: value outside declared width i32")); m.g_i32 = value; break;`,
		// An array element carries the same bound, guarded AFTER the fill guard so a
		// §7.3-skipped bare scalar at the array id is not turned into an INVALID.
		`case 8: if (afill == 0) break; afill--; if (value < 0 || value > 255L) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, "arr_u8 element: value outside declared width u8"));`,
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
	if sbuf := files[dir+"Sbuf.java"]; !strings.Contains(sbuf, "final class Sbuf {") ||
		strings.Contains(sbuf, "public final class Sbuf") {
		t.Error("Sbuf must stay package-private")
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
