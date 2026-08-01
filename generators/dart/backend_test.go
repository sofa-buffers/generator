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

// TestLazySequenceFraming locks MESSAGE_SPEC §2 framing: every sequence is opened
// with beginSequenceLazy, and the CLOSER is what decides whether a contentless one
// survives. A struct/union FIELD and an array wrapper FIELD close with the dropping
// endSequence, so an all-default one is omitted instead of emitted as an empty
// frame. A sequence-form array ELEMENT chooses POSITIONALLY, from the index in the
// value at run time: the keeping closer at the array's last index (its presence is
// what carries the length, §5.1), the dropping one in the interior, where an
// all-default element vanishes into an id gap. example.yaml has a struct field
// (id 20), a union field (id 21), a struct-array (id 23) and a union-array (id 25).
func TestLazySequenceFraming(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		// FIELD: struct / union, opened lazily, dropped when no child was written.
		"e.beginSequenceLazy(20); somestruct.marshal(e); e.endSequence();",
		"e.beginSequenceLazy(21); someunion.marshal(e); e.endSequence();",
		// FIELD: the struct-array wrapper (id 23) -- also the dropping closer.
		"e.beginSequenceLazy(23);",
		// ELEMENT: the closer is chosen from the position in the VALUE.
		"e.beginSequenceLazy(_i0); somestructarray[_i0].marshal(e);\n" +
			"      if (_i0 == somestructarray.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }",
		"e.beginSequenceLazy(_i0); someunionarray[_i0].marshal(e);\n" +
			"      if (_i0 == someunionarray.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sequence framing missing %q", want)
		}
	}
	// The eager open is gone from corelib-dart, so emitting it would not compile.
	if strings.Contains(out, "e.beginSequence(") {
		t.Error("eager e.beginSequence( emitted; every sequence must open with beginSequenceLazy")
	}
	// The keeping closer must never appear unconditionally: it is only ever reached
	// through the last-element test.
	if got, want := strings.Count(out, "e.endSequenceKeep();"), strings.Count(out, ".length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }"); got != want {
		t.Errorf("endSequenceKeep emitted unconditionally: %d keeping closers, %d positional choices", got, want)
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
		// Wrapper-sequence arrays (string/blob/struct/union/nested) reset to EMPTY,
		// their declared `count: N` notwithstanding: N is a capacity, not a length
		// (§3), so a fresh array holds no elements -- which is also what an absent
		// field decodes back to.
		"    somestringarray.clear();",
		"    somestructarray.clear();",
		"    somematrix.clear();",
		// A native array with a declared default is cleared and refilled from a
		// const literal: no reallocation of the list, none of the default either.
		// The literal is the default EXACTLY as written, never padded out to N --
		// someenumarray declares count: 4 with a 3-element default.
		"    someuintarray..clear()..addAll(const <int>[0, 1, 1000, 4294967295]);",
		"    someenumarray..clear()..addAll(const <int>[2, 1, 0]);",
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
		// `names` is count:2 with no declared default, so it resets EMPTY -- a
		// capacity adds no elements (§3). `nums` refills from its declared default,
		// in place. `dyn` is count-less and has none: the bare clear.
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

// TestNestedRowClosesPositionally: a nested wrapper row is an ELEMENT (depth > 0),
// so its closer is chosen from its index in the value -- keeping at the last row,
// dropping in the interior, where an empty row becomes an id gap. The identically-
// shaped field-level wrapper (depth 0) always drops.
func TestNestedRowClosesPositionally(t *testing.T) {
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
	if !strings.Contains(out, "      if (_i0 == rows.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }") {
		t.Errorf("a nested wrapper row must choose its closer positionally:\n%s", out)
	}
	// The FIELD wrapper is the only unconditional dropping closer.
	if got := strings.Count(out, "    e.endSequence();\n"); got != 1 {
		t.Errorf("expected exactly one unconditional dropping closer (the field wrapper), got %d:\n%s", got, out)
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
		// exactly the wire count: `count: 3` is a capacity and adds no elements (§3)
		"o.somefloatarray = _f32copy(values, values.length);",
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
		"void onArrayBegin(int id, sofab.ArrayKind kind, int count) {",
		"void onFixlenHeader(int id, int subtype, int length) {",
		// Gated on the DECLARED element kind, exactly like the maxlen guard below
		// (§7.3, generator#259) -- see TestDartArrayHeaderBoundIsKeyedByElementKind.
		"if (kind == sofab.ArrayKind.unsigned && count > 4) e.inv = true;", // someuintarray, count 4
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
		// ...and the array count bound is the same defect one hook over
		// (generator#259 / F-0042): an un-gated compare measures a contradicting
		// array kind against this field's N.
		"if (count > 4) e.inv = true;",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("header guard %q is not gated on the declared kind/subtype (generator#224, generator#259)", notWant)
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

// capDef exercises every array shape the `count`-is-a-capacity rule touches:
// a count:N and a count-less struct array, count:N string/blob wrapper arrays, a
// count:N native array with and without a declared default, a native matrix and a
// wrapper-row matrix.
const capDef = "version: 1\nmessages:\n  vec:\n    payload:\n" +
	"      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }\n" +
	"      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }\n" +
	"      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }\n" +
	"      fblobs:  { id: 3, type: array, items: { type: blob, count: 4, maxlen: 8 } }\n" +
	"      fnums:   { id: 4, type: array, items: { type: u32, count: 4 } }\n" +
	"      withdef: { id: 5, type: array, items: { type: u32, count: 4 }, default: [1, 2] }\n" +
	"      rows:    { id: 6, type: array, items: { type: array, count: 3, items: { type: u32, count: 3 } } }\n" +
	"      srows:   { id: 7, type: array, items: { type: array, count: 3, items: { type: string, maxlen: 4 } } }\n"

// A schema `count: N` is a CAPACITY, never a length (MESSAGE_SPEC §3, af536c4):
// it never reaches the wire, it bounds the array, and it never adds an element the
// value does not hold. So a count:N array starts and resets EMPTY unless a default
// is declared, a short declared default stands exactly as written, and the field's
// omit test is the ordinary `!= default` compare with nothing padded to N on
// either side. An all-zero length-N value differs from the empty one and stays on
// the wire.
func TestDartCountIsACapacityNotALength(t *testing.T) {
	out := genFor(t, writeDef(t, capDef), map[string]any{})

	for _, want := range []string{
		// Initializers: empty for every count:N array, wrapper and native alike.
		"  List<VecFixedElem> fixed = <VecFixedElem>[];",
		"  List<String> fstrs = <String>[];",
		"  List<Uint8List> fblobs = <Uint8List>[];",
		"  List<int> fnums = <int>[];",
		// A declared default is materialized EXACTLY as written -- count: 4 with a
		// 2-element default stays 2 elements long.
		"  List<int> withdef = <int>[1, 2];",
		// reset() restores the same thing, in place.
		"    fixed.clear();",
		"    fstrs.clear();",
		"    fblobs.clear();",
		"    fnums.clear();",
		"    withdef..clear()..addAll(const <int>[1, 2]);",
		// The field omit test: emptiness, or an exact compare against the declared
		// default -- neither side padded to N.
		"    if (fnums.isNotEmpty) { e.writeUnsignedArray(4, fnums); }",
		"    if (!_listEq(withdef, <int>[1, 2])) { e.writeUnsignedArray(5, withdef); }",
		// ...and _isDefault is the exact negation of it.
		"    if (!(fnums.isEmpty)) return false;",
		"    if (!(_listEq(withdef, <int>[1, 2]))) return false;",
		// A wrapper array writes a child for every element it holds (the last one
		// unconditionally), so "no child written" IS "empty" -- for count:N and
		// count-less alike, no narrowing on either side.
		"    if (!(fixed.isEmpty)) return false;",
		"    if (!(dynamic_.isEmpty)) return false;",
		"    if (!(fstrs.isEmpty)) return false;",
		"    if (!(rows.isEmpty)) return false;",
		"    if (!(srows.isEmpty)) return false;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}

	// The superseded fixed-length reading, in every form it took: a count:N array
	// materialized to N element defaults, a short default tail-padded to N, the
	// trailing-run trim on encode and the fill-to-N on decode.
	for _, notWant := range []string{
		"<VecFixedElem>[VecFixedElem(),",
		"<String>['', '',",
		"<Uint8List>[Uint8List(0),",
		"<int>[0, 0, 0, 0]",
		"<int>[1, 2, 0, 0]",
		"_trimLen(", "_trimInt(", "_trimF32(", "_trimF64(", "_padTo(",
		"void onSequenceEnd()",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("`count` is a capacity: %q must not be generated:\n%s", notWant, out)
		}
	}
}

// One sparse rule, both element kinds, with or without a declared count
// (MESSAGE_SPEC §2, af536c4): an element BEFORE the last one that equals its
// element default is omitted, leaving an id GAP -- a string/blob leaf is not
// written, a struct/union/nested-array element is not framed either. The LAST
// element is always written: a leaf as its value, a sequence element as an empty
// frame. The choice is positional, from the index in the VALUE at run time; the
// schema cannot answer it.
func TestDartArrayElementSparsityIsPositional(t *testing.T) {
	out := genFor(t, writeDef(t, capDef), map[string]any{})

	for _, want := range []string{
		// Leaf elements: the omit test escapes at the last index. Unconditional now
		// -- the count:N carve-out ("its length is N whatever the wire carries") is
		// gone, so fstrs/fblobs carry the very same guard a count-less array does.
		"for (var _i0 = 0; _i0 < fstrs.length; _i0++) { if (fstrs[_i0].isNotEmpty || _i0 == fstrs.length - 1) e.writeString(_i0, fstrs[_i0]); }",
		"for (var _i0 = 0; _i0 < fblobs.length; _i0++) { if (fblobs[_i0].isNotEmpty || _i0 == fblobs.length - 1) e.writeBlob(_i0, fblobs[_i0]); }",
		// Sequence-form elements: the loop runs to length (no trailing elision) and
		// the CLOSER decides -- dropping in the interior, keeping at the last index.
		"    for (var _i0 = 0; _i0 < fixed.length; _i0++) {\n" +
			"      e.beginSequenceLazy(_i0); fixed[_i0].marshal(e);\n" +
			"      if (_i0 == fixed.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }\n" +
			"    }",
		"    for (var _i0 = 0; _i0 < dynamic_.length; _i0++) {\n" +
			"      e.beginSequenceLazy(_i0); dynamic_[_i0].marshal(e);\n" +
			"      if (_i0 == dynamic_.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }\n" +
			"    }",
		// A NATIVE row has no frame of its own, so the rule lands on the write.
		"      if (rows[_i0].isNotEmpty || _i0 == rows.length - 1) e.writeUnsignedArray(_i0, rows[_i0]);",
		// A WRAPPER row has one, so it takes the closer -- and its own elements obey
		// the same rule one level down.
		"      if (_i0 == srows.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }",
		"if (srows[_i0][_i1].isNotEmpty || _i1 == srows[_i0].length - 1) e.writeString(_i1, srows[_i0][_i1]);",
		// A sequence-typed FIELD still always drops.
		"    e.endSequence();",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}

	// The superseded shapes: an unconditional keeping closer on an element (the
	// old "sequence elements are framed unconditionally" carve-out), and a leaf
	// omit test with no last-element escape (the old fixed-count trailing elision).
	for _, notWant := range []string{
		"marshal(e); e.endSequenceKeep();",
		"if (fstrs[_i0].isNotEmpty) e.writeString",
		"if (fblobs[_i0].isNotEmpty) e.writeBlob",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("superseded element rule still generated (%q):\n%s", notWant, out)
		}
	}
}

// Decode: a wrapper element's id IS its array index (§5.1), so EVERY collector
// places at out[id] after gap-filling -- never appends. Interior sparsity makes an
// interior gap reachable for the first time, and an appending collector would shift
// every later element down by one. The row collectors (_IntMat / _DblMat /
// _BoolMat / _SeqSeq) were the ones appending id-blind in this backend's siblings;
// here they already placed by id but carried no bound, so they also gain the outer
// array's cap -- which rejects an over-index row and bounds the gap-fill against an
// over-index amplification DoS.
func TestDartCollectorsPlaceByIDAndAreBounded(t *testing.T) {
	out := genFor(t, writeDef(t, capDef), map[string]any{})

	for _, want := range []string{
		// leaf / object collectors: guard, gap-fill, place
		// The string collector takes RAW wire bytes now: the destination (this
		// collector, at this id) is resolved before the payload is validated or
		// transcoded, so a skipped string is never inspected (generator#257).
		"    if (cap >= 0 && id >= cap) { e.inv = true; return; }\n" +
			"    if (emax >= 0 && bytes.length > emax) { e.inv = true; return; }\n",
		"    if (!sofab.utf8Valid(bytes)) { e.inv = true; return; }\n" +
			"    while (out.length <= id) { out.add(''); }\n" +
			"    out[id] = utf8.decode(bytes);",
		"    if (cap >= 0 && id >= cap) { e.inv = true; return null; }\n" +
			"    while (out.length <= id) { out.add(make()); }\n" +
			"    return vis(out[id]);",
		// native-row collector: bounded placement, not an append
		"  void _row(int id, Int64List v) {\n" +
			"    if (cap >= 0 && id >= cap) { e.inv = true; return; }\n" +
			"    while (out.length <= id) { out.add(<int>[]); }\n" +
			"    out[id] = List<int>.from(v);\n" +
			"  }",
		// wrapper-row collector: same
		"    if (cap >= 0 && id >= cap) { e.inv = true; return null; }\n" +
			"    while (out.length <= id) { out.add(<T>[]); }\n" +
			"    return make(out[id]);",
		// the schema count reaches every collector as its cap; count-less is -1, and
		// the ROW collectors take the OUTER array's cap (a row id is its index there)
		"_ObjSeq<VecFixedElem>(o.fixed, 5, e,",
		"_ObjSeq<VecDynamicElem>(o.dynamic_, -1, e,",
		"_StrSeq(o.fstrs, 3, 8, e)",
		"_BlobSeq(o.fblobs, 4, 8, e)",
		"_IntMat(o.rows, 3, false, e)",
		"_SeqSeq<String>(o.srows, 3, e, (p) => _StrSeq(p, -1, 4, e))",
		// M elements arrived, M is the length: the count word is bounded, nothing
		// is filled in behind it.
		"        if (values.length > 4) { e.inv = true; return; }\n        o.fnums = List<int>.from(values);\n        return;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}

	// The id-blind append the reference backend had to fix, in both row shapes.
	for _, notWant := range []string{
		"out.add(List<int>.from(v));",
		"out.add(<T>[]);\n    return make(out[out.length - 1]);",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("a row collector must not append id-blind (%q):\n%s", notWant, out)
		}
	}
}

// An fp32 array binds through the bit-exact _f32copy, and its length is the WIRE
// count -- a `count: N` is a capacity and pre-allocates nothing (§3). Pinned
// separately because the fp32 path used to pass the schema N here, which was the
// fill-to-N in disguise.
func TestDartFp32ArrayTakesTheWireLength(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      f32s: { id: 0, type: array, items: { type: fp32, count: 3 } }\n" +
		"      f64s: { id: 1, type: array, items: { type: fp64, count: 3 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	for _, want := range []string{
		"o.f32s = _f32copy(values, values.length);",
		"o.f64s = List<double>.from(values);",
		"  List<double> f32s = <double>[];",
		"  List<double> f64s = <double>[];",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "_f32copy(values, 3)") || strings.Contains(out, "_padTo(") {
		t.Errorf("an fp32/fp64 count:N array must not be pre-sized to N:\n%s", out)
	}
}

// The array header hook carries the element KIND, and the schema `count` bound
// sits INSIDE the test for the field's own declared kind (generator#259 /
// Crucible F-0042, CORELIB_PLAN §4.8).
//
// onArrayBegin fires for whatever array kind lands on a field id -- the corelib
// reports what arrived but only the generated code knows what was DECLARED -- and
// an array whose element kind contradicts the declaration was never this field's
// value (MESSAGE_SPEC §7.3). It is a skipped field, so its element count is not
// this field's count and must not be measured against N. The two fp kinds are
// therefore kept apart on the wire hook: a fixlen array's count word precedes its
// fixlen_word, so a collapsed "fixlen" kind could not tell an fp64 header at a
// declared fp32[N] slot from a real one, and the over-count bound would reject a
// message that must be ACCEPTED (the driver: an fp64 array announcing 8 elements
// arriving at `f32s`, declared `count: 3`).
//
// The skip needs no emitted code: the whole-array callbacks are kind-dispatched
// by the corelib, so the contradicting array lands in a callback with no arm for
// this id and evaporates -- leaving any correctly typed earlier occurrence of the
// same id intact (§7.4).
func TestDartArrayHeaderBoundIsKeyedByElementKind(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      f32s: { id: 0, type: array, items: { type: fp32, count: 3 } }\n" +
		"      f64s: { id: 1, type: array, items: { type: fp64, count: 5 } }\n" +
		"      us:   { id: 2, type: array, items: { type: u32, count: 7 } }\n" +
		"      ss:   { id: 3, type: array, items: { type: i32, count: 9 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})

	for _, want := range []string{
		// The hook gained the kind parameter; the old two-argument override no
		// longer overrides anything the corelib calls.
		"  void onArrayBegin(int id, sofab.ArrayKind kind, int count) {",
		// One arm per field, each testing ONLY its own declared element kind, with
		// the bound behind that test. A declared fp32 appears under fp32 and a
		// declared fp64 under fp64 -- never one shared "fixlen" arm.
		"      case 0:\n        if (kind == sofab.ArrayKind.fp32 && count > 3) e.inv = true;\n        return;",
		"      case 1:\n        if (kind == sofab.ArrayKind.fp64 && count > 5) e.inv = true;\n        return;",
		// Integer arrays take the same shape -- there is no second wire word on
		// that path, but the declared kind still gates the bound.
		"      case 2:\n        if (kind == sofab.ArrayKind.unsigned && count > 7) e.inv = true;\n        return;",
		"      case 3:\n        if (kind == sofab.ArrayKind.signed && count > 9) e.inv = true;\n        return;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}

	for _, notWant := range []string{
		// The pre-#259 arity: it compiles against nothing the corelib calls.
		"void onArrayBegin(int id, int count)",
		// A collapsed fixlen kind cannot separate the two fp slots at all.
		"sofab.ArrayKind.fixlen",
		// An un-gated bound rejects a header that §7.3 says to skip.
		"if (count > 3) e.inv = true;",
		"if (count > 5) e.inv = true;",
		// The fp32 field's N must never be reachable from the fp64 arm (and back).
		"if (kind == sofab.ArrayKind.fp64 && count > 3)",
		"if (kind == sofab.ArrayKind.fp32 && count > 5)",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("array header bound is not keyed by the declared element kind (%q):\n%s", notWant, out)
		}
	}
}

// TestDartSkippedStringIsNotValidated: UTF-8 validation belongs where a `string`
// is MATERIALIZED — read into a declared destination — never on a payload the
// decoder is skipping (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038).
//
// corelib-dart used to hand the visitor a finished `String`, which forced it to
// validate and transcode before the consumer could say whether it even wanted
// the field. It now delivers raw wire bytes through `onStringBytes`, so the
// generated arm resolves the destination first and only then checks and decodes.
// A skipped field reaches no arm and is never inspected.
func TestDartSkippedStringIsNotValidated(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      u:  { id: 1, type: string }\n" +
		"      b:  { id: 2, type: blob, maxlen: 8 }\n" +
		"      sa: { id: 3, type: array, items: { type: string, count: 4 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})

	// The decoder's string entry point is the raw-bytes one; the transcoding
	// `onString` override is gone.
	if !strings.Contains(out, "void onStringBytes(int id, Uint8List bytes)") {
		t.Errorf("the visitor must take raw wire bytes:\n%s", out)
	}
	if strings.Contains(out, "void onString(int id, String value)") {
		t.Errorf("onString must no longer be overridden — it cannot resolve a destination first:\n%s", out)
	}
	// Validate then transcode, inside the arm.
	for _, want := range []string{
		"if (!sofab.utf8Valid(bytes)) { e.inv = true; return; }",
		"o.s = utf8.decode(bytes);",
		"o.u = utf8.decode(bytes);",
		"import 'dart:convert';", // needed for the decode, emitted because a string exists
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	// The maxlen bound reads the wire length directly now — no re-encode.
	if !strings.Contains(out, "if (bytes.length > 8) { e.inv = true; return; }") {
		t.Errorf("the maxlen bound must measure the raw wire bytes:\n%s", out)
	}
	// A blob carries no encoding: its arm must not validate.
	if strings.Contains(out, "void onBlob(int id, Uint8List value)") &&
		strings.Contains(out, "utf8Valid(value)") {
		t.Errorf("blob must never be UTF-8-validated:\n%s", out)
	}
}

// A schema with no string at all must not carry the `dart:convert` import:
// `dart analyze` reports an unused import, and the corpus sweep builds exactly
// such definitions.
func TestDartNoConvertImportWithoutStrings(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: u32 }\n" +
		"      b: { id: 1, type: blob, maxlen: 8 }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	if strings.Contains(out, "import 'dart:convert';") {
		t.Errorf("a string-free schema must not import dart:convert:\n%s", out)
	}
}
