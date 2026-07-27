package dart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// genFor parses + analyzes a definition file, generates with cfg, and returns all
// emitted files concatenated (path-delimited) for substring assertions.
func genFor(t *testing.T, def string, cfg map[string]any) string {
	t.Helper()
	data, err := os.ReadFile(def)
	if err != nil {
		t.Fatalf("read %s: %v", def, err)
	}
	doc, err := parser.Parse(data, def)
	if err != nil {
		t.Fatalf("parse %s: %v", def, err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid %s: %v", def, errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatalf("model %s: %v", def, err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatalf("analyze %s: %v", def, err)
	}
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var b strings.Builder
	for _, f := range files {
		b.WriteString("// === " + f.Path + " ===\n")
		b.Write(f.Content)
		b.WriteString("\n")
	}
	return b.String()
}

const exampleDef = "../../examples/messages/example.yaml"

func TestModuleShape(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		"import 'package:sofabuffers/sofabuffers.dart' as sofab;",
		"class Myfirstmessage {",
		"void marshal(sofab.Encoder e) {",
		"Uint8List encode() => sofab.Encoder.encodeToBytes(marshal);",
		"static sofab.DecodeStatus tryDecode(Uint8List data, Myfirstmessage out) {",
		"static Myfirstmessage decode(Uint8List data) {",
		"class _MyfirstmessageVisitor extends sofab.MessageVisitor {",
		"static const int maxSize =",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing %q", want)
		}
	}
}

func TestEnumBitfieldConstants(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	// enum/bitfield lower to an abstract-final class of static const int values.
	if !strings.Contains(out, "abstract final class MyfirstmessageSomeenum {") {
		t.Error("enum not lowered to an abstract final class")
	}
	if !strings.Contains(out, "static const int ") {
		t.Error("enum/bitfield constants not emitted as static const int")
	}
}

func TestKeywordAndTypeNameMangling(t *testing.T) {
	// A field named after a Dart keyword or core type is mangled with a trailing
	// underscore; the JSON/wire name is unaffected (id-keyed).
	out := genFor(t, "../../tests/matrix/corpus/defs/keywords.yaml", map[string]any{})
	if !strings.Contains(out, "int_") {
		t.Error("field named 'int' should mangle to int_ (would otherwise shadow the int type)")
	}
	if strings.Contains(out, " int int =") {
		t.Error("a field named 'int' must not be emitted unmangled")
	}
}

func TestU64DefaultLiteral(t *testing.T) {
	// A u64 default of 2^64-1 must not be emitted as a decimal literal (Dart's int
	// is signed 64-bit; the decimal form is a compile error). scalars.yaml has a
	// u64max field defaulting to 18446744073709551615.
	out := genFor(t, "../../tests/matrix/corpus/defs/scalars.yaml", map[string]any{})
	if strings.Contains(out, "18446744073709551615") {
		t.Error("u64 max default emitted as an out-of-range decimal literal")
	}
	if !strings.Contains(out, "= -1;") {
		t.Error("u64 max default should be emitted as its signed bit pattern -1")
	}
}

func TestSparseOmitGuards(t *testing.T) {
	out := genFor(t, "../../tests/matrix/corpus/defs/scalars.yaml", map[string]any{})
	// Every leaf write is guarded by a != default omit test (sparse canonical).
	if !strings.Contains(out, "if (u8max != 255) { e.writeUnsigned(1, u8max); }") {
		t.Error("scalar field not guarded by its != default omit test")
	}
}

// TestLazySequenceFraming locks MESSAGE_SPEC S2 framing: every sequence is opened
// with beginSequenceLazy, and the CLOSER is picked statically from the position in
// the schema. A struct/union FIELD and an array wrapper FIELD close with the
// dropping endSequence, so an all-default one is omitted instead of emitted as an
// empty frame; a wrapper-array ELEMENT closes with endSequenceKeep, because element
// presence is what carries a dynamic array's length (S5.1). example.yaml has a
// struct field (id 20), a union field (id 21), a struct-array (id 23) and a
// union-array (id 25).
func TestLazySequenceFraming(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		// FIELD: struct / union, opened lazily, dropped when no child was written.
		"e.beginSequenceLazy(20); somestruct.marshal(e); e.endSequence();",
		"e.beginSequenceLazy(21); someunion.marshal(e); e.endSequence();",
		// FIELD: the struct-array wrapper (id 23) -- also the dropping closer.
		"e.beginSequenceLazy(23);",
		// ELEMENT: a struct/union element keeps its frame even when all-default.
		"e.beginSequenceLazy(_i0); somestructarray[_i0].marshal(e); e.endSequenceKeep();",
		"e.beginSequenceLazy(_i0); someunionarray[_i0].marshal(e); e.endSequenceKeep();",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sequence framing missing %q", want)
		}
	}
	// The eager open is gone from corelib-dart, so emitting it would not compile.
	if strings.Contains(out, "e.beginSequence(") {
		t.Error("eager e.beginSequence( emitted; every sequence must open with beginSequenceLazy")
	}
	// The wrapper FIELD must close with the dropping end. Count both closers: the
	// keeping one appears exactly once per sequence-form element loop body.
	if got, want := strings.Count(out, "e.endSequenceKeep();"), strings.Count(out, ".marshal(e); e.endSequenceKeep();"); got != want {
		t.Errorf("endSequenceKeep used outside an array element body: %d keeping closers, %d element bodies", got, want)
	}
}

// TestResetRestoresDefaults: MESSAGE_SPEC S2 omits a sequence-typed field equal to
// its default, so an absent field fires NO decode callback and the S7.4
// sequence-start clear cannot run for it. Decoding into a REUSED destination
// therefore has to start from the defaults, which is what the generated reset()
// gives tryDecode. Every field kind must be covered, in place where it can be.
func TestResetRestoresDefaults(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		"  void reset() {",
		// tryDecode resets the caller's destination; decode's fresh instance does not
		// pay for it twice.
		"    out.reset();\n    return _decodeInto(data, out);",
		"    final m = Myfirstmessage();\n    _decodeInto(data, m);",
		// Scalars/strings/blobs are values: assignment is the reset.
		"    someu8 = 7;",
		"    somestring = '';",
		"    someblob = Uint8List.fromList(<int>[72, 101, 108, 108, 111]);",
		// fp32 drops the captured NaN wire bits with the value (S4.6).
		"    somefp32 = 0.0;\n    _somefp32Fp32Bits = null;",
		// A nested struct/union is reset in place, recursively -- the nested case:
		// an all-default struct in the next message is omitted entirely.
		"    somestruct.reset();",
		"    someunion.reset();",
		// Wrapper-sequence arrays (string/blob/struct/union/nested) clear.
		"    somestringarray.clear();",
		"    somestructarray.clear();",
		"    somematrix.clear();",
		// A native array with a materialized default is cleared and refilled from a
		// const literal: no reallocation of the list, none of the default either.
		"    someuintarray..clear()..addAll(const <int>[0, 1, 1000, 4294967295]);",
		"    someenumarray..clear()..addAll(const <int>[2, 1, 0, 0]);",
		// fp32 arrays are the one exception: decode installs a fixed-length
		// Float32List (bit-exact NaN copy), which cannot be cleared.
		"    somefloatarray = <double>[0.0, -1.5, 3.25];",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reset() missing %q", want)
		}
	}
	// Every generated object class carries one, message and named type alike.
	if got, want := strings.Count(out, "  void reset() {"), strings.Count(out, "  void marshal(sofab.Encoder e) {"); got != want {
		t.Errorf("reset() on %d classes, marshal on %d: every object class needs both", got, want)
	}
	// The S7.4 replace-on-reopen clear stays where it was.
	if !strings.Contains(out, "        o.somestringarray = <String>[];") {
		t.Error("the S7.4 sequence-start clear must remain in the visitor")
	}
}

// TestResetIsInPlaceForReuse: reset must not hand the field a fresh container, or
// the reuse entry point reallocates everything it was meant to recycle. A blob and
// an fp32 array are the documented exceptions (both fixed-length in Dart).
func TestResetIsInPlaceForReuse(t *testing.T) {
	def := filepath.Join(t.TempDir(), "reuse.yaml")
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      names: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }\n" +
		"      nums: { id: 1, type: array, items: { type: u32, count: 3 }, default: [1, 2, 3] }\n" +
		"      dyn: { id: 2, type: array, items: { type: i16 } }\n"
	if err := os.WriteFile(def, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := genFor(t, def, map[string]any{})
	body := out[strings.Index(out, "  void reset() {"):]
	body = body[:strings.Index(body, "\n  }")]
	for _, want := range []string{
		"    names.clear();",
		"    nums..clear()..addAll(const <int>[1, 2, 3]);",
		"    dyn.clear();",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reset() body missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "names = <") || strings.Contains(body, "nums = <") || strings.Contains(body, "dyn = <") {
		t.Errorf("reset() reallocated a list instead of clearing it:\n%s", body)
	}
}

// TestNestedRowKeepsItsFrame: a nested array row is an ELEMENT (depth > 0), so its
// wrapper closes with endSequenceKeep even though the identically-shaped field-level
// wrapper (depth 0) closes with the dropping endSequence.
func TestNestedRowKeepsItsFrame(t *testing.T) {
	def := filepath.Join(t.TempDir(), "matrix.yaml")
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      rows: { id: 0, type: array, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }\n"
	if err := os.WriteFile(def, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := genFor(t, def, map[string]any{})
	if !strings.Contains(out, "e.beginSequenceLazy(0);") {
		t.Error("array FIELD wrapper not opened lazily")
	}
	// depth 0 (the field) drops, depth 1 (the row element) keeps.
	if !strings.Contains(out, "e.endSequenceKeep();") {
		t.Error("nested row (an array ELEMENT) must close with endSequenceKeep")
	}
	if strings.Count(out, "e.endSequence();") != 1 {
		t.Errorf("expected exactly one dropping closer (the field wrapper), got %d", strings.Count(out, "e.endSequence();"))
	}
}

// TestFp32SignalingNaNPreserved asserts the codegen shape that keeps an fp32
// signaling/payload NaN bit-for-bit through decode -> re-encode (issue #226): a
// Dart `double` quiets the NaN, so the generated code must route through
// corelib-dart's raw-bits API (onFp32Bits / writeFp32Bits) for the scalar and a
// bit-exact Float32List copy for the array. example.yaml has a scalar `somefp32`
// (id 8) and a fixed-count fp32 array `somefloatarray` (id 17).
func TestFp32SignalingNaNPreserved(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		// Scalar: a private companion bits slot, captured in onFp32Bits and cleared
		// in onFp32, and re-emitted via writeFp32Bits when the value is a NaN.
		"int? _somefp32Fp32Bits;",
		"void onFp32Bits(int id, int bits) {",
		"o._somefp32Fp32Bits = bits;",
		"o.somefp32 = _f32FromBits(bits);",
		"o._somefp32Fp32Bits = null;",
		"if (somefp32.isNaN && _somefp32Fp32Bits != null) { e.writeFp32Bits(8, _somefp32Fp32Bits!); }",
		// Array: a bit-exact Float32List copy, never a widening List<double>.from.
		"Float32List _f32copy(Float32List v, int n) {",
		"o.somefloatarray = _f32copy(values, 3);",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fp32 sNaN codegen missing %q", want)
		}
	}
	// The widening path the bug rode on must be gone for fp32 arrays.
	if strings.Contains(out, "somefloatarray = List<double>.from(values)") {
		t.Error("fp32 array still decoded via List<double>.from (quiets a signaling NaN)")
	}
}

func TestProjectFiles(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{"emit": "project"})
	for _, want := range []string{
		"// === pubspec.yaml ===",
		"// === lib/message.dart ===",
		"// === bin/harness.dart ===",
		"path: ${SOFAB_DART_CORELIB}",
		"void main(List<String> args) {",
		"exit(1);", // decode-mode failure sets a non-zero process exit
	} {
		if !strings.Contains(out, want) {
			t.Errorf("project output missing %q", want)
		}
	}
}

// TestDartHeaderVisitorReject verifies the generator#216 / F-0032 fix: a schema
// bound is rejected at the header word via the corelib-dart HeaderVisitor hooks
// (onArrayBegin at the count word, onFixlenHeader at the length word), so a field
// that is BOTH over-bound and truncated is INVALID, not INCOMPLETE (MESSAGE_SPEC
// §5.2). The example's someuintarray (count 4), somestring (maxlen 50) and someblob
// (maxlen 16) exercise both hooks; the sticky e.inv the guard sets is read by
// tryDecode before the incomplete status, so the flag alone makes INVALID dominate.
func TestDartHeaderVisitorReject(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		"void onArrayBegin(int id, int count) {",
		"void onFixlenHeader(int id, int subtype, int length) {",
		"if (count > 4) e.inv = true;", // someuintarray, count 4
		// Each maxlen guard is gated on the DECLARED fixlen subtype: onFixlenHeader
		// fires for any subtype at a field id, and a contradicting one must be
		// skipped, not measured against this field's bound (§7.3, generator#224).
		"if (subtype == sofab.FixlenType.string && length > 50) e.inv = true;", // somestring
		"if (subtype == sofab.FixlenType.blob && length > 16) e.inv = true;",   // someblob
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing header-visitor guard %q", want)
		}
	}
	// The bound must never be enforced on length alone — an un-gated compare is
	// exactly the generator#224 defect (an fp64 landing on a `maxlen: 4` blob was
	// rejected as INVALID instead of skipped).
	for _, notWant := range []string{
		"if (length > 50) e.inv = true;",
		"if (length > 16) e.inv = true;",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("maxlen header guard %q is not gated on the fixlen subtype (generator#224)", notWant)
		}
	}
	// A message with no bounded field must NOT override the header hooks, keeping
	// the corelib's max-speed decode path (no per-scope dispatch cost). scalars.yaml
	// is all fixed-width scalars — no count, no maxlen.
	plain := genFor(t, "../../tests/matrix/corpus/defs/scalars.yaml", map[string]any{})
	for _, notWant := range []string{"void onArrayBegin(", "void onFixlenHeader("} {
		if strings.Contains(plain, notWant) {
			t.Errorf("a bound-free message must not override %q", notWant)
		}
	}
}

func TestDecodeLimitsPlumbing(t *testing.T) {
	// An unbounded string + a configured cap wires a DecoderLimits (no_maxlen.yaml
	// has an unbounded string `s` and blob `b`).
	out := genFor(t, "../../tests/matrix/corpus/defs/no_maxlen.yaml", map[string]any{"max_dyn_string_len": 8})
	if !strings.Contains(out, "sofab.DecoderLimits(") {
		t.Error("configured max_dyn_string_len should bake a DecoderLimits")
	}
	if !strings.Contains(out, ", limits: _limits)") {
		t.Error("DecoderLimits should be passed to Decoder.decode")
	}
}

func TestGeneratedIsASCII(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{"emit": "project"})
	for i := 0; i < len(out); i++ {
		if out[i] >= 0x80 {
			t.Fatalf("non-ASCII byte 0x%02x at offset %d", out[i], i)
		}
	}
}

// TestConformance runs the full generate -> dart build -> round-trip ->
// shared-vector harness. Gated on SOFAB_DART_CORELIB (a corelib-dart checkout)
// and the `dart` toolchain; skipped otherwise, so the hermetic core CI job stays
// toolchain-free (the lang-dart job runs the harness directly).
func TestConformance(t *testing.T) {
	corelib := os.Getenv("SOFAB_DART_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_DART_CORELIB to a corelib-dart checkout to run the Dart conformance harness")
	}
	if _, err := exec.LookPath("dart"); err != nil {
		t.Skip("dart toolchain not on PATH")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "tests", "conformance", "dart", "run.sh"), corelib)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("conformance harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("conformance harness did not report PASS:\n%s", out)
	}
}

// writeDef writes a schema source to a temp file and returns its path.
func writeDef(t *testing.T, src string) string {
	t.Helper()
	def := filepath.Join(t.TempDir(), "def.yaml")
	if err := os.WriteFile(def, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return def
}

const trimDef = "version: 1\nmessages:\n  vec:\n    payload:\n" +
	"      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }\n" +
	"      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }\n" +
	"      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }\n"

// A count:N wrapper array's canonical wire stops at M -- one past its last
// non-default element (MESSAGE_SPEC S3/S5.1, "even for sequence-form elements")
// -- and M == 0 leaves the whole wrapper omitted (S2). generator#248: the
// element loop used to run to length, framing every trailing all-default
// element, so a decoder that accepted the non-canonical form re-encoded it
// unchanged instead of normalising. A DYNAMIC array has no N to refill from, so
// its trailing default element is significant and must still be framed.
func TestDartFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	out := genFor(t, writeDef(t, trimDef), map[string]any{})

	// The fixed array narrows to M before framing anything...
	if !strings.Contains(out, "for (var _i0 = 0, _n0 = _trimLen(fixed, (x) => x._isDefault); _i0 < _n0; _i0++) {") {
		t.Errorf("count:N struct array must loop to M, not length:\n%s", out)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(out, "for (var _i0 = 0, _n0 = dynamic_.length; _i0 < _n0; _i0++) {") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", out)
	}
	// An interior all-default element is still framed: only the TRAILING run goes.
	if !strings.Contains(out, "e.beginSequenceLazy(_i0); fixed[_i0].marshal(e); e.endSequenceKeep();") {
		t.Errorf("interior elements must keep the framing closer:\n%s", out)
	}

	// _isDefault is the exact negation of what marshal writes, so it must narrow a
	// field exactly when the marshal loop does -- disagreeing would either omit a
	// field that is on the wire or keep one that is not.
	if !strings.Contains(out, "if (!(_trimLen(fixed, (x) => x._isDefault) == 0)) return false;") {
		t.Errorf("_isDefault must narrow the fixed array like marshal does:\n%s", out)
	}
	if !strings.Contains(out, "if (!(dynamic_.length == 0)) return false;") {
		t.Errorf("_isDefault must NOT narrow the dynamic array:\n%s", out)
	}
	if !strings.Contains(out, "if (!(_trimLen(fstrs, (x) => x.isEmpty) == 0)) return false;") {
		t.Errorf("_isDefault for a string wrapper array must test the trimmed run:\n%s", out)
	}
	// The all-default predicate itself: emitted for every class, per field and
	// recursively, so an ELEMENT can be judged before the element loop opens.
	if !strings.Contains(out, "bool get _isDefault {") {
		t.Errorf("every generated class must carry the all-default predicate:\n%s", out)
	}
	if !strings.Contains(out, "int _trimLen<T>(List<T> a, bool Function(T) isDef) {") {
		t.Errorf("the M helper must be emitted:\n%s", out)
	}
}

// generator#247 (already correct in Dart -- pinned as a regression): a wrapper
// array's element id IS the array index (S5.1), so an element is PLACED at
// out[id] after gap-filling -- never appended. Appending would shorten the array
// by the size of any interior id gap and would decode a REOPENED id as a second
// element instead of merging into the first (S7.4).
//
// The N-fill on onSequenceEnd is what makes the S3/S5.1 trailing elision
// lossless: without it, re-encoding a decoded fixed array shortens it on every
// round trip. That is the prerequisite generator#248 could not land without.
func TestDartWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
	out := genFor(t, writeDef(t, trimDef), map[string]any{})

	for _, want := range []string{
		// placement, not append -- and the gap-fill that precedes it
		"    while (out.length <= id) { out.add(make()); }\n    return vis(out[id]);",
		// N-fill when the sequence scope closes, for every wrapper collector
		"  void onSequenceEnd() {\n    if (cap < 0) return;\n    while (out.length < cap) { out.add(make()); }\n  }",
		"  void onSequenceEnd() {\n    if (cap < 0) return;\n    while (out.length < cap) { out.add(''); }\n  }",
		// the over-index guard still bounds both the placement and the gap-fill
		"    if (cap >= 0 && id >= cap) { e.inv = true; return null; }",
		// the schema count reaches the collector as its cap; the dynamic one is -1
		"_ObjSeq<VecFixedElem>(o.fixed, 5, e,",
		"_ObjSeq<VecDynamicElem>(o.dynamic_, -1, e,",
		"_StrSeq(o.fstrs, 3, 8, e)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	// The defect the placement replaced: appending ignores the id entirely.
	if strings.Contains(out, "out.add(make());\n    return vis(out[out.length - 1]);") {
		t.Errorf("_ObjSeq must not append id-blind:\n%s", out)
	}
}

// A blob wrapper array gets the same N-fill, and an emax-bounded element keeps
// its guard: the fill must not disturb the over-index / over-maxlen rejects.
func TestDartBlobWrapperArrayFillsToN(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      blobs: { id: 0, type: array, items: { type: blob, count: 4, maxlen: 8 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	for _, want := range []string{
		"  void onSequenceEnd() {\n    if (cap < 0) return;\n    while (out.length < cap) { out.add(Uint8List(0)); }\n  }",
		"    if (emax >= 0 && value.length > emax) { e.inv = true; return; }",
		"_BlobSeq(o.blobs, 4, 8, e)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("blob wrapper array missing %q:\n%s", want, out)
		}
	}
}

// A DYNAMIC wrapper array must be left alone end to end: no trim on the marshal
// side (a trailing default element is significant when there is no N to refill
// from) and no fill on the decode side (its length is highest-present-id + 1).
func TestDartDynamicWrapperArrayIsNeitherTrimmedNorFilled(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      strs: { id: 0, type: array, items: { type: string, maxlen: 8 } }\n" +
		"      objs: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	if strings.Contains(out, "_trimLen(") {
		t.Errorf("a schema with no count:N wrapper array must not emit the M helper:\n%s", out)
	}
	if !strings.Contains(out, "_StrSeq(o.strs, -1, 8, e)") || !strings.Contains(out, "_ObjSeq<VecObjsElem>(o.objs, -1, e,") {
		t.Errorf("dynamic wrapper arrays must pass cap -1:\n%s", out)
	}
	// cap -1 short-circuits the fill at runtime; the guard must be present.
	if !strings.Contains(out, "    if (cap < 0) return;") {
		t.Errorf("the N-fill must short-circuit for a dynamic array:\n%s", out)
	}
}
