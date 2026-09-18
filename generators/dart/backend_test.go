package dart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// genFor parses + analyzes a definition file, generates with cfg, and returns all
// emitted files concatenated (path-delimited) for substring assertions.
func genFor(t *testing.T, def string, cfg map[string]any) string {
	t.Helper()
	files, err := (&Backend{}).Generate(schemaFor(t, def), cfg)
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

// schemaFor parses, validates and analyzes a definition file into the IR.
func schemaFor(t *testing.T, def string) *ir.Schema {
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
	return s
}

const exampleDef = "../../examples/messages/example.yaml"

func TestModuleShape(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		"import 'package:sofa_buffers_corelib/sofa_buffers_corelib.dart' as sofab;",
		"class Myfirstmessage {",
		"void serialize(sofab.Encoder e) {",
		// example.yaml has an unbounded field, so encode() takes the scratch+sink
		// arm (TestDartCallerOwnsTheEncodeBuffer covers both).
		"final e = sofab.Encoder(out.add, buffer: Uint8List(512), depth: maxDepth);",
		"static const int maxDepth = 2;",
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
		"e.beginSequenceLazy(20); somestruct.serialize(e); e.endSequence();",
		"e.beginSequenceLazy(21); someunion.serialize(e); e.endSequence();",
		// FIELD: the struct-array wrapper (id 23) -- also the dropping closer.
		"e.beginSequenceLazy(23);",
		// ELEMENT: the closer is chosen from the position in the VALUE.
		"e.beginSequenceLazy(_i0); somestructarray[_i0].serialize(e);\n" +
			"      if (_i0 == somestructarray.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }",
		"e.beginSequenceLazy(_i0); someunionarray[_i0].serialize(e);\n" +
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
		// Scalars are values: assignment is the reset.
		"    someu8 = 7;",
		// A destination keeps its storage: the length goes back to 0, or the
		// declared default is copied into it -- no reallocation, and none of the
		// default either: it is a typed list built once per class, so the copy is
		// a memmove.
		"    somestring.length = 0;",
		"    someblob.assign(_someblobDefault);",
		"  static final Uint8List _someblobDefault = Uint8List.fromList(const <int>[72, 101, 108, 108, 111]);",
		// fp32 drops the captured NaN wire bits with the value (S4.6).
		"    somefp32 = 0.0;\n    somefp32Fp32Bits = null;",
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
		// A native array with a declared default is refilled from a const literal,
		// in place. The literal is the default EXACTLY as written, never padded out
		// to N -- someenumarray declares count: 4 with a 3-element default. An fp32
		// array is no exception any more: its storage is the destination's own.
		"    someuintarray.assign(_someuintarrayDefault);",
		"  static final Int64List _someuintarrayDefault = Int64List.fromList(const <int>[0, 1, 1000, 4294967295]);",
		"    someenumarray.assign(_someenumarrayDefault);",
		"  static final Int64List _someenumarrayDefault = Int64List.fromList(const <int>[2, 1, 0]);",
		"    somefloatarray.assign(_somefloatarrayDefault);",
		"  static final Float32List _somefloatarrayDefault = Float32List.fromList(const <double>[0.0, -1.5, 3.25]);",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reset() missing %q", want)
		}
	}
	// Every generated object class carries one, message and named type alike.
	if got, want := strings.Count(out, "  void reset() {"), strings.Count(out, "  void serialize(sofab.Encoder e) {"); got != want {
		t.Errorf("reset() on %d classes, serialize on %d: every object class needs both", got, want)
	}
	// The S7.4 replace-on-reopen clear stays where it was.
	if !strings.Contains(out, "        o.somestringarray.clear();\n        return sofab.StringSeq(o.somestringarray,") {
		t.Error("the S7.4 sequence-start clear must remain in the visitor")
	}
}

// TestResetIsInPlaceForReuse: reset must not hand the field a fresh container, or
// the reuse entry point reallocates everything it was meant to recycle. A
// destination keeps its storage (length back to 0, or the default assigned); a
// wrapper list is cleared.
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
		// in place. `dyn` is count-less and has none: its length goes back to 0.
		"    names.clear();",
		"    nums.assign(_numsDefault);",
		"    dyn.length = 0;",
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
// corelib-dart's raw-bits API (onFp32Bits / writeFp32Bits) for the scalar, and
// the array lives in an InlineFloat32Array, whose Float32List storage the codec
// copies the wire bytes into and writeFp32Array copies back out -- never widened
// through a double. example.yaml has a scalar `somefp32` (id 8) and a
// fixed-count fp32 array `somefloatarray` (id 17).
func TestFp32SignalingNaNPreserved(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		// Scalar: a private companion bits slot, captured in onFp32Bits and cleared
		// in onFp32, and re-emitted via writeFp32Bits when the value is a NaN.
		"int? somefp32Fp32Bits;",
		"void onFp32Bits(int id, int bits) {",
		"o.somefp32Fp32Bits = bits;",
		"o.somefp32 = _f32FromBits(bits);",
		"o.somefp32Fp32Bits = null;",
		"if (somefp32.isNaN && somefp32Fp32Bits != null) { e.writeFp32Bits(8, somefp32Fp32Bits!); }",
		// Array: decoded into and encoded from the raw fp32 storage.
		"final sofab.InlineFloat32Array somefloatarray = sofab.InlineFloat32Array(3)",
		"sofab.InlineFloat32Array? onFp32Array(int id, int count) {",
		"        return o.somefloatarray;",
		"e.writeFp32Array(17, somefloatarray.storage, somefloatarray.length);",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fp32 sNaN codegen missing %q", want)
		}
	}
	// The widening path the bug rode on must be gone for fp32 arrays.
	if strings.Contains(out, "List<double>") {
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
// TestDartHarnessRejectsARoundedJSONNumber pins the harness against the silent
// clamp generator#535 measured: `jsonDecode` hands back a double the moment a
// literal no longer fits an int, so a u64 above 2^63-1 is ALREADY rounded when
// the harness sees it, and `(v as num).toInt()` then saturates at
// 9223372036854775807 -- a different value, reported as success.
//
// The fix is not to recover the value, which is gone, but to say so: an int
// passes, a double passes only where it is exactly an integer within +-2^53
// (`1e3` and the bench payload's timestamps stay readable), and anything beyond
// throws while naming the string spelling that survives.
func TestDartHarnessRejectsARoundedJSONNumber(t *testing.T) {
	h := genFor(t, exampleDef, map[string]any{"emit": "project"})
	for _, want := range []string{
		"int _exact64(Object? v) {",
		"if (v is int) return v;",
		"v == v.roundToDouble() && v.abs() <= 9007199254740992.0",
		"throw FormatException(",
		"BigInt.from(_exact64(",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("harness.dart is missing %q", want)
		}
	}
	if strings.Contains(h, "BigInt.from((") && strings.Contains(h, "as num).toInt())") {
		t.Error("the harness parses a 64-bit field through (v as num).toInt() again: a JSON number above 2^63-1 saturates there, silently")
	}
}

// methodBody returns the body of the generated override whose signature line
// contains sig, within the class that starts at the first occurrence of cls ("" =
// the first match anywhere); "" when no such override is emitted.
func methodBody(out, cls, sig string) string {
	if cls != "" {
		i := strings.Index(out, cls)
		if i < 0 {
			return ""
		}
		out = out[i:]
		if j := strings.Index(out[1:], "\nclass "); j >= 0 {
			out = out[:j+1]
		}
	}
	i := strings.Index(out, sig)
	if i < 0 {
		return ""
	}
	body := out[i:]
	if j := strings.Index(body, "\n  }\n"); j >= 0 {
		body = body[:j]
	}
	return body
}

// TestDartHeaderVisitorReject: every schema bound on an aggregate is judged in
// the field's ONE header call, before the destination is handed over -- so a
// field that is both over-bound and truncated is INVALID, not INCOMPLETE
// (generator#216, §5.2) -- and the arm lives only in the call for the DECLARED
// wire kind. corelib-dart picks the call from the wire kind, so a contradicting
// array or fixlen subtype lands in a call with no arm for the id and is skipped,
// never measured against this field's bound (§7.3, generator#224, generator#259
// / F-0042): the gate the old header hooks had to spell out is structural.
func TestDartHeaderVisitorReject(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, c := range []struct{ sig, arm string }{
		{"sofab.InlineString? onString(int id, int length) {", "case 11:\n        if (length > 50) invalidate();\n        return o.somestring;"},
		{"sofab.InlineBytes? onBlob(int id, int length) {", "case 12:\n        if (length > 16) invalidate();\n        return o.someblob;"},
		{"sofab.InlineInt64Array? onUnsignedArray(int id, int count) {", "case 15:\n        if (count > 4) invalidate();\n        return o.someuintarray;"},
		{"sofab.InlineInt64Array? onSignedArray(int id, int count) {", "case 16:\n        if (count > 5) invalidate();\n        return o.someintarray;"},
		{"sofab.InlineFloat32Array? onFp32Array(int id, int count) {", "case 17:\n        if (count > 3) invalidate();\n        return o.somefloatarray;"},
	} {
		body := methodBody(out, "class _MyfirstmessageVisitor", c.sig)
		if !strings.Contains(body, c.arm) {
			t.Errorf("%s is missing the header arm %q:\n%s", c.sig, c.arm, body)
		}
	}
	// Each id is answered by exactly one call: someuintarray (15) is unsigned, so
	// a signed or fixlen array at id 15 has no arm and is skipped.
	for _, sig := range []string{"onSignedArray(int id", "onFp32Array(int id", "onFp64Array(int id", "onString(int id", "onBlob(int id"} {
		if strings.Contains(methodBody(out, "class _MyfirstmessageVisitor", sig), "case 15:") {
			t.Errorf("id 15 (array<u32>) answered in %s: a contradicting wire kind must be skipped (§7.3)", sig)
		}
	}
	// A message with no aggregate field overrides none of the header calls: it
	// inherits the corelib's default, which skips.
	plain := genFor(t, "../../tests/matrix/corpus/defs/scalars.yaml", map[string]any{})
	for _, notWant := range []string{"onString(", "onBlob(", "onUnsignedArray(", "onSignedArray(", "onFp32Array(", "onFp64Array(", "onSequenceStart("} {
		if strings.Contains(plain, notWant) {
			t.Errorf("a message without that field kind must not override %q", notWant)
		}
	}
}

// TestDartArrayElemBound covers generator#267's element position: an array
// element outside its DECLARED WIDTH is INVALID (§7.1) and, established by its
// own bytes, dominates a truncation behind it (§5.2). The bound travels on the
// destination (`range:`), and the codec applies it to every element as it is
// decoded, on both surfaces -- so no scan over an assembled list is left to miss
// the array that never completes.
func TestDartArrayElemBound(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		"final sofab.InlineInt64Array someuintarray = sofab.InlineInt64Array(4, range: const sofab.ElemRange(0, 4294967295))",
		"final sofab.InlineInt64Array someintarray = sofab.InlineInt64Array(5, range: const sofab.ElemRange(-2147483648, 2147483647))",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing element bound %q", want)
		}
	}
	for _, gone := range []string{"for (final _v in values)", "onArrayElemBound("} {
		if strings.Contains(out, gone) {
			t.Errorf("the removed per-array scan/hook %q is still emitted", gone)
		}
	}
}

// TestDecodeLimitsPlumbing: the max_dyn_* keys reach generated Dart as module
// constants and are enforced BY generated Dart, per field, at each field's own
// count/length header. Nothing is passed into the corelib any more
// (corelib-dart#88: "the numbers and the allocation are not the codec's").
func TestDecodeLimitsPlumbing(t *testing.T) {
	// no_maxlen.yaml has an unbounded string `s`, an unbounded blob `b`, and a
	// `count: 2` string array `names`.
	out := genFor(t, "../../tests/matrix/corpus/defs/no_maxlen.yaml", map[string]any{"max_dyn_string_len": 8})
	if strings.Contains(out, "sofab.DecoderLimits") || strings.Contains(out, "limits: _limits") {
		t.Errorf("the corelib must be handed no receiver cap:\n%s", out)
	}
	for _, want := range []string{
		"const int maxDynStringLen = 8;",
		// the unbounded string and blob: policy, at the length word, and only then
		// is the destination sized -- a hostile length never sizes anything.
		"case 0:\n        if (length > maxDynStringLen) limitExceeded();\n        if (o.s.capacity < length) o.s.storage = Uint8List(length);\n        return o.s;",
		"case 1:\n        if (length > maxDynBlobLen) limitExceeded();\n        if (o.b.capacity < length) o.b.storage = Uint8List(length);\n        return o.b;",
		// an unbounded destination starts empty
		"final sofab.InlineString s = sofab.InlineString(0);",
		// the decode entry points drive the visitor and nothing else
		"sofab.Decoder.decode(data, _DynVisitor(out));",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing %q:\n%s", want, out)
		}
	}
	// A cap is a POLICY rejection and must never be folded into INVALID, nor
	// reach the schema-bounded array beside it.
	if strings.Contains(out, "length > maxDynStringLen) invalidate()") {
		t.Errorf("a cap must not be reported as INVALID:\n%s", out)
	}
	if strings.Contains(out, "count > maxDynArrayCount") {
		t.Errorf("a `count: 2` array must not be judged against a cap:\n%s", out)
	}
}

// A cap must not reach a field the schema bounds, and the two must be able to
// disagree: max_dyn_array_count 4 beside a sibling's count: 100000 is exactly
// what a per-decode DecoderLimits could not express, and the raise that made it
// decodable loosened the cap for the unbounded field too.
func TestDartCapsTravelAsConfigured(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: array, items: { type: u64 } }\n" +
		"      b: { id: 1, type: array, items: { type: i32, count: 100000 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{"max_dyn_array_count": 4})
	if !strings.Contains(out, "const int maxDynArrayCount = 4;") {
		t.Errorf("the cap must be emitted AS CONFIGURED, unraised:\n%s", out)
	}
	for _, want := range []string{
		"case 0:\n        if (count > maxDynArrayCount) limitExceeded();\n        if (o.a.capacity < count) o.a.storage = Int64List(count);\n        return o.a;",
		"case 1:\n        if (count > 100000) invalidate();\n        if (o.b.capacity < count) o.b.storage = Int64List(count);\n        return o.b;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing %q:\n%s", want, out)
		}
	}
}

// TestDartDestinationSizing pins where a destination's storage comes from. A
// small schema bound is allocated once, at construction, and the header call
// only hands it over: a reused object never allocates again, and the codec
// never grows anything. A bound too large to pay for on every fresh object
// (eagerDestBytes) starts empty and gets storage of exactly the count at the
// header -- after the bound check, so the count that sizes it has already been
// accepted (ARCHITECTURE §9.5: allocated once, never grown element by element).
func TestDartDestinationSizing(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      small: { id: 0, type: array, items: { type: u8, count: 128 } }\n" +
		"      big:   { id: 1, type: array, items: { type: u8, count: 129 } }\n" +
		"      s1k:   { id: 2, type: string, maxlen: 1024 }\n" +
		"      s1k1:  { id: 3, type: blob, maxlen: 1025 }\n" +
		"      f32:   { id: 4, type: array, items: { type: fp32, count: 256 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	for _, want := range []string{
		"final sofab.InlineInt64Array small = sofab.InlineInt64Array(128, range: const sofab.ElemRange(0, 255));",
		"final sofab.InlineInt64Array big = sofab.InlineInt64Array(0, range: const sofab.ElemRange(0, 255));",
		"final sofab.InlineString s1k = sofab.InlineString(1024);",
		"final sofab.InlineBytes s1k1 = sofab.InlineBytes(0);",
		"final sofab.InlineFloat32Array f32 = sofab.InlineFloat32Array(256);",
		"case 0:\n        if (count > 128) invalidate();\n        return o.small;",
		"case 1:\n        if (count > 129) invalidate();\n        if (o.big.capacity < count) o.big.storage = Int64List(count);\n        return o.big;",
		"case 2:\n        if (length > 1024) invalidate();\n        return o.s1k;",
		"case 3:\n        if (length > 1025) invalidate();\n        if (o.s1k1.capacity < length) o.s1k1.storage = Uint8List(length);\n        return o.s1k1;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing %q:\n%s", want, out)
		}
	}
}

// Rule 5 for Dart: an id a scope does not bind is SKIPPED, and a skipped field
// allocates nothing (§6.2.1, §7.3). corelib-dart's header calls answer null by
// default, so a scope that binds no field of a kind emits no override for it,
// and a scope that binds some answers null for every other id.
func TestDartDeclinesUnboundDestinations(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string }\n" +
		"      a: { id: 1, type: array, items: { type: u32 } }\n" +
		"  N:\n    payload:\n      x: { id: 0, type: u32 }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	for _, c := range []struct{ sig, want string }{
		{"sofab.InlineString? onString(int id, int length) {", "    }\n    return null;"},
		{"sofab.InlineInt64Array? onUnsignedArray(int id, int count) {", "    }\n    return null;"},
	} {
		if body := methodBody(out, "class _MVisitor", c.sig); !strings.Contains(body, c.want) {
			t.Errorf("%s must answer null for an id it does not bind:\n%s", c.sig, body)
		}
	}
	nv := out[strings.Index(out, "class _NVisitor"):]
	for _, notWant := range []string{"onString(", "onBlob(", "onUnsignedArray(", "onSignedArray(", "onFp32Array(", "onFp64Array(", "onSequenceStart("} {
		if strings.Contains(nv, notWant) {
			t.Errorf("a scope binding no such field must inherit the skipping default, not override %q:\n%s", notWant, nv)
		}
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
		// Initializers: empty for every count:N array, wrapper and native alike. A
		// native array's N sizes its STORAGE -- the capacity the codec decodes into
		// -- and its length starts at 0.
		"  List<VecFixedElem> fixed = <VecFixedElem>[];",
		"  List<sofab.InlineString> fstrs = <sofab.InlineString>[];",
		"  List<sofab.InlineBytes> fblobs = <sofab.InlineBytes>[];",
		"  final sofab.InlineInt64Array fnums = sofab.InlineInt64Array(4, range: const sofab.ElemRange(0, 4294967295));",
		// A declared default is materialized EXACTLY as written -- count: 4 with a
		// 2-element default stays 2 elements long.
		"  final sofab.InlineInt64Array withdef = sofab.InlineInt64Array(4, range: const sofab.ElemRange(0, 4294967295))..assign(_withdefDefault);",
		"  static final Int64List _withdefDefault = Int64List.fromList(const <int>[1, 2]);",
		// reset() restores the same thing, in place.
		"    fixed.clear();",
		"    fstrs.clear();",
		"    fblobs.clear();",
		"    fnums.length = 0;",
		"    withdef.assign(_withdefDefault);",
		// The field omit test: emptiness, or an exact compare against the declared
		// default -- neither side padded to N, and only the `length` in use read.
		"    if (fnums.length != 0) { e.writeUnsignedArray(4, fnums.storage, fnums.length); }",
		"    if (!_prefixEq(withdef.storage, withdef.length, _withdefDefault)) { e.writeUnsignedArray(5, withdef.storage, withdef.length); }",
		// ...and _isDefault is the exact negation of it.
		"    if (!(fnums.length == 0)) return false;",
		"    if (!(_prefixEq(withdef.storage, withdef.length, _withdefDefault))) return false;",
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
		"<sofab.InlineString>[sofab.InlineString(",
		"<sofab.InlineBytes>[sofab.InlineBytes(",
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
		"    for (var _i0 = 0; _i0 < fstrs.length; _i0++) {\n" +
			"      final _e0 = fstrs[_i0];\n" +
			"      if (_e0.length != 0 || _i0 == fstrs.length - 1) e.writeStringUtf8(_i0, _e0.storage, _e0.length);\n" +
			"    }",
		"    for (var _i0 = 0; _i0 < fblobs.length; _i0++) {\n" +
			"      final _e0 = fblobs[_i0];\n" +
			"      if (_e0.length != 0 || _i0 == fblobs.length - 1) e.writeBlob(_i0, _e0.storage, _e0.length);\n" +
			"    }",
		// Sequence-form elements: the loop runs to length (no trailing elision) and
		// the CLOSER decides -- dropping in the interior, keeping at the last index.
		"    for (var _i0 = 0; _i0 < fixed.length; _i0++) {\n" +
			"      e.beginSequenceLazy(_i0); fixed[_i0].serialize(e);\n" +
			"      if (_i0 == fixed.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }\n" +
			"    }",
		"    for (var _i0 = 0; _i0 < dynamic_.length; _i0++) {\n" +
			"      e.beginSequenceLazy(_i0); dynamic_[_i0].serialize(e);\n" +
			"      if (_i0 == dynamic_.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }\n" +
			"    }",
		// A NATIVE row has no frame of its own, so the rule lands on the write.
		"      if (_e0.length != 0 || _i0 == rows.length - 1) e.writeUnsignedArray(_i0, _e0.storage, _e0.length);",
		// A WRAPPER row has one, so it takes the closer -- and its own elements obey
		// the same rule one level down.
		"      if (_i0 == srows.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }",
		"        final _e1 = srows[_i0][_i1];\n" +
			"        if (_e1.length != 0 || _i1 == srows[_i0].length - 1) e.writeStringUtf8(_i1, _e1.storage, _e1.length);",
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
		"serialize(e); e.endSequenceKeep();",
		"if (_e0.length != 0) e.write",
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

	// The collector BODIES are corelib code since corelib-dart#74 -- placement by
	// id, the gap fill, the capacity and maxlen rejects, the element-width bound --
	// and are tested there, against decoded bytes. What is still this backend's to
	// get right is the CALL: which collector a field picks, and which bounds reach
	// it. That is what this asserts.
	for _, want := range []string{
		// the schema count reaches every collector as its cap; count-less is -1, and
		// the ROW collectors take the OUTER array's cap (a row id is its index there)
		"sofab.MessageSeq<VecFixedElem>(o.fixed, 5,",
		"sofab.MessageSeq<VecDynamicElem>(o.dynamic_, -1,",
		// ...and beside each schema bound its receiver sibling, ALWAYS emitted:
		// corelib-dart requires them (§6.2.1 gives that library no number to
		// invent) and consults each one only where the schema bound beside it is
		// -1, so the two can never both be in play. `relemMax` is a literal here
		// because every string/blob in this schema declares a maxlen -- no
		// module constant of that kind exists to name.
		"sofab.StringSeq(o.fstrs, 3, 8, rcap: maxDynArrayCount, relemMax: 262144)",
		"sofab.BlobSeq(o.fblobs, 4, 8, rcap: maxDynArrayCount, relemMax: 1048576)",
		// A matrix has two axes and four bounds: cap/rcap on the ROW ID, and
		// rowCount/rowCap on the row's OWN element count -- the inner `count: 3`,
		// which this backend used to drop on the floor, leaving the row's count
		// header bounded by nothing but the decoder-wide cap that is now gone.
		"sofab.IntMatrixSeq(o.rows, 3, false, 0, 4294967295, rcap: maxDynArrayCount, rowCount: 3, rowCap: maxDynArrayCount)",
		"sofab.NestedSeq<sofab.InlineString>(o.srows, 3, (p) => sofab.StringSeq(p, -1, 4, rcap: maxDynArrayCount, relemMax: 262144), rcap: maxDynArrayCount)",
		// M elements arrived, M is the length: the count word is bounded at the
		// header, and the codec sets the destination's length to exactly M --
		// nothing is filled in behind it.
		"      case 4:\n        if (count > 4) invalidate();\n        return o.fnums;",
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

// An fp32/fp64 array decodes into its own destination, and its LENGTH is the
// WIRE count -- the codec sets it at the header. A `count: N` sizes the storage
// (a capacity) and never the length (§3): a fresh count:3 array is empty. Pinned
// separately because the fp32 path once passed the schema N as the length, which
// was the fill-to-N in disguise.
func TestDartFp32ArrayTakesTheWireLength(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      f32s: { id: 0, type: array, items: { type: fp32, count: 3 } }\n" +
		"      f64s: { id: 1, type: array, items: { type: fp64, count: 3 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	for _, want := range []string{
		"  final sofab.InlineFloat32Array f32s = sofab.InlineFloat32Array(3);",
		"  final sofab.InlineFloat64Array f64s = sofab.InlineFloat64Array(3);",
		"      case 0:\n        if (count > 3) invalidate();\n        return o.f32s;",
		"      case 1:\n        if (count > 3) invalidate();\n        return o.f64s;",
		"    if (f32s.length != 0) { e.writeFp32Array(0, f32s.storage, f32s.length); }",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "f32s.length = 3") || strings.Contains(out, "_padTo(") {
		t.Errorf("an fp32/fp64 count:N array must not be pre-sized to N:\n%s", out)
	}
}

// The schema `count` bound is keyed by the field's DECLARED element kind
// (generator#259 / Crucible F-0042, CORELIB_PLAN §4.8).
//
// An array whose element kind contradicts the declaration was never this field's
// value (MESSAGE_SPEC §7.3). It is a skipped field, so its element count is not
// this field's count and must not be measured against N -- the driver: an fp64
// array announcing 8 elements arriving at `f32s`, declared `count: 3`, must be
// SKIPPED and the message ACCEPTED.
//
// corelib-dart makes that structural: it asks for a destination through a
// different call per wire kind -- and for a fixlen array only past the
// fixlen_word, so fp32 and fp64 are never one collapsed "fixlen" -- and each
// field's arm sits in the call for its own declared kind. The contradicting
// array reaches a call with no arm for the id, answers null, and evaporates,
// leaving any correctly typed earlier occurrence of the same id intact (§7.4).
func TestDartArrayHeaderBoundIsKeyedByElementKind(t *testing.T) {
	src := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      f32s: { id: 0, type: array, items: { type: fp32, count: 3 } }\n" +
		"      f64s: { id: 1, type: array, items: { type: fp64, count: 5 } }\n" +
		"      us:   { id: 2, type: array, items: { type: u32, count: 7 } }\n" +
		"      ss:   { id: 3, type: array, items: { type: i32, count: 9 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})

	calls := []struct{ sig, arm string }{
		{"sofab.InlineFloat32Array? onFp32Array(int id, int count) {", "      case 0:\n        if (count > 3) invalidate();\n        return o.f32s;"},
		{"sofab.InlineFloat64Array? onFp64Array(int id, int count) {", "      case 1:\n        if (count > 5) invalidate();\n        return o.f64s;"},
		{"sofab.InlineInt64Array? onUnsignedArray(int id, int count) {", "      case 2:\n        if (count > 7) invalidate();\n        return o.us;"},
		{"sofab.InlineInt64Array? onSignedArray(int id, int count) {", "      case 3:\n        if (count > 9) invalidate();\n        return o.ss;"},
	}
	for i, c := range calls {
		body := methodBody(out, "class _VecVisitor", c.sig)
		if !strings.Contains(body, c.arm) {
			t.Errorf("%s is missing the arm %q:\n%s", c.sig, c.arm, body)
		}
		// ...and no other field's id: each call answers only its own kind.
		for j := range calls {
			if j != i && strings.Contains(body, fmt.Sprintf("case %d:", j)) {
				t.Errorf("%s answers id %d, whose declared kind is another call's (§7.3):\n%s", c.sig, j, body)
			}
		}
	}
	for _, notWant := range []string{"onArrayBegin(", "sofab.ArrayKind"} {
		if strings.Contains(out, notWant) {
			t.Errorf("the removed header hook %q is still emitted:\n%s", notWant, out)
		}
	}
}

// TestDartSkippedStringIsNotValidated: UTF-8 validation belongs where a `string`
// is MATERIALIZED — read into a declared destination — never on a payload the
// decoder is skipping (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038).
//
// corelib-dart validates a string once its payload is whole, and only for a
// destination the visitor handed over at the header. So the generated arm
// resolves the destination and nothing else: no transcoding, no validation of
// its own, and a field with no arm answers null and is never inspected.
func TestDartSkippedStringIsNotValidated(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      u:  { id: 1, type: string }\n" +
		"      b:  { id: 2, type: blob, maxlen: 8 }\n" +
		"      sa: { id: 3, type: array, items: { type: string, count: 4 } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})

	body := methodBody(out, "class _MVisitor", "sofab.InlineString? onString(int id, int length) {")
	for _, want := range []string{
		// The maxlen bound reads the announced wire length, at the header.
		"      case 0:\n        if (length > 8) invalidate();\n        return o.s;",
		"      case 1:\n",
		"        if (o.u.capacity < length) o.u.storage = Uint8List(length);\n        return o.u;",
		"    return null;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onString missing %q:\n%s", want, body)
		}
	}
	// No generated validation or transcoding is left: the codec owns it.
	for _, gone := range []string{"decodeUtf8Strict", "utf8Valid", "onStringBytes", "void onString(int id, String value)"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q is still generated -- UTF-8 is the codec's now:\n%s", gone, out)
		}
	}
}

// TestDartStringFreeScopeSkipsStrings: the residual of #257 (generator#265 /
// Crucible F-0038). A scope with no string field must skip a string at any id
// without inspecting it: a lone continuation byte at an undeclared id — `4a 0a
// 8a` — once turned an otherwise valid message INVALID in dart alone, on 12
// implementations that accept it. corelib-dart's MessageVisitor now defaults
// every aggregate and every sequence to "skip" (null), so a string-free scope
// simply emits no onString override, and a sequence at a leaf element position
// is never bound as that element (generator#272,
// TestDartMistypedSequenceElementIsSkipped).
func TestDartStringFreeScopeSkipsStrings(t *testing.T) {
	// Nothing here declares a string: the top-level message, its nested struct,
	// and every collector scope (blob array, struct array, native matrix) are all
	// string-free.
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a:  { id: 0, type: u32 }\n" +
		"      b:  { id: 1, type: blob, maxlen: 8 }\n" +
		"      n:  { id: 2, type: struct, fields: { k: { id: 0, type: u32 } } }\n" +
		"      ba: { id: 3, type: array, items: { type: blob, count: 4, maxlen: 8 } }\n" +
		"      sa: { id: 4, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }\n" +
		"      m:  { id: 5, type: array, items: { type: array, count: 2, items: { type: u32, count: 2 } } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})

	for _, decl := range []string{
		"class _MVisitor extends sofab.MessageVisitor {",
		"class _MNVisitor extends sofab.MessageVisitor {",
	} {
		if !strings.Contains(out, decl) {
			t.Errorf("missing %q:\n%s", decl, out)
		}
	}
	if strings.Contains(out, "onString(") || strings.Contains(out, "sofab.VisitorBase") {
		t.Errorf("a string-free schema must inherit the skipping default:\n%s", out)
	}
	if strings.Contains(out, "utf8Valid") || strings.Contains(out, "utf8.decode") {
		t.Errorf("a string-free schema must never validate or transcode a string:\n%s", out)
	}
}

// A string-declaring scope keeps its own arms and answers null for every id it
// does not match — the switch has no default arm, so an unmatched id falls out
// of it to `return null` without inspecting the bytes.
func TestDartStringScopeFallsThroughToSkip(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string, maxlen: 8 }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	body := methodBody(out, "class _MVisitor", "sofab.InlineString? onString(int id, int length) {\n    switch (id) {")
	if body == "" {
		t.Fatalf("expected an id switch in the string header call:\n%s", out)
	}
	if strings.Contains(body, "default:") || !strings.HasSuffix(body, "    }\n    return null;") {
		t.Errorf("the override must fall out of the switch to `return null` for an unmatched id:\n%s", body)
	}
}

// No generated module carries the `dart:convert` import: a string is held as its
// UTF-8 bytes, validated by the codec on decode and by writeStringUtf8 on encode,
// so nothing generated transcodes. An import nothing uses is a `dart analyze`
// warning, so the string-carrying schema is checked here beside the one without.
func TestDartNoConvertImport(t *testing.T) {
	for _, src := range []string{
		"version: 1\nmessages:\n  M:\n    payload:\n" +
			"      a: { id: 0, type: u32 }\n" +
			"      b: { id: 1, type: blob, maxlen: 8 }\n",
		"version: 1\nmessages:\n  M:\n    payload:\n" +
			"      s:  { id: 0, type: string, maxlen: 8 }\n" +
			"      sa: { id: 1, type: array, items: { type: string, count: 4 } }\n",
	} {
		out := genFor(t, writeDef(t, src), map[string]any{})
		if strings.Contains(out, "import 'dart:convert';") {
			t.Errorf("no generated module may import dart:convert:\n%s", out)
		}
	}
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound.
//
// The `value < 0` term on the unsigned side is load-bearing: Dart's int is a
// 64-bit SIGNED integer with no unsigned counterpart, so an unsigned wire value
// at or above 2^63 arrives negative and `value > 255` alone would wave through
// exactly the largest values.
func TestDartDeclaredWidthIsAValidityBound(t *testing.T) {
	got := genFor(t, writeDef(t, `
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
`), map[string]any{})
	for _, want := range []string{
		"case 0:\n        if (value < 0 || value > 255) { invalidate(); return; }\n        o.a_u8 = value;",
		"case 2:\n        if (value < 0 || value > 4294967295) { invalidate(); return; }\n        o.c_u32 = value;",
		"case 4:\n        if (value < -128 || value > 127) { invalidate(); return; }\n        o.e_i8 = value;",
		"case 6:\n        if (value < -2147483648 || value > 2147483647) { invalidate(); return; }\n        o.g_i32 = value;",
		// The array's elements are bounded by the codec, from the destination's
		// declared range, as they are decoded.
		"final sofab.InlineInt64Array arr_u8 = sofab.InlineInt64Array(4, range: const sofab.ElemRange(0, 255));",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.dart missing width guard %q:\n%s", want, got)
		}
	}
	for _, want := range []string{
		"case 3:\n        o.d_u64 = value;",
		"case 7:\n        o.h_i64 = value;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.dart: a 64-bit destination must store unguarded (%q):\n%s", want, got)
		}
	}
}

// generator#272 (Crucible F-0047): a wrapper-array ELEMENT position opened as a
// sequence must be skipped whole (MESSAGE_SPEC §7.3), but the leaf element
// collectors (_StrSeq / _BlobSeq) declare no sequence of their own and so never
// overrode onSequenceStart — inheriting sofab.MessageVisitor's DESCENDING
// default, which returns `this`. A sequence at an element position therefore
// descended into the collector itself and its child string bound as that element.
//
// The fix sits on the corelib's base: sofab.MessageVisitor's onSequenceStart
// answers null (skip) since corelib-dart#96 (sofab.VisitorBase before it), so
// every collector inherits the skip by construction, including ones added later.
func TestDartMistypedSequenceElementIsSkipped(t *testing.T) {
	got := genFor(t, writeDef(t, `
version: 1
messages:
  Probe:
    payload:
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
      blob_array:   { id: 201, type: array, items: { type: blob,   count: 5, maxlen: 64 } }
      obj_array:    { id: 202, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
`), map[string]any{})
	// The only generated visitor is the message's own, and it overrides
	// onSequenceStart with arms for its declared sequences and `return null` for
	// everything else.
	if body := methodBody(got, "class _ProbeVisitor", "sofab.MessageVisitor? onSequenceStart(int id) {"); !strings.HasSuffix(body, "    }\n    return null;") {
		t.Errorf("onSequenceStart must skip every sequence it does not bind:\n%s", body)
	}
	// The leaf collectors are corelib types since corelib-dart#74, and inherit the
	// skip there; what this backend owes is handing the scope to one of them
	// rather than to a visitor of its own that would descend.
	for _, call := range []string{"sofab.StringSeq(", "sofab.BlobSeq(", "sofab.MessageSeq<"} {
		if !strings.Contains(got, call) {
			t.Errorf("the wrapper array must be collected by %s, not by an emitted class:\n%s", call, got)
		}
	}
	for _, cls := range []string{"class _StrSeq", "class _BlobSeq", "class _ObjSeq"} {
		if strings.Contains(got, cls) {
			t.Errorf("%s belongs to the corelib and must not be emitted:\n%s", cls, got)
		}
	}
}

// generator#275 (Crucible F-0049): CORELIB_PLAN §6.5 requires a double-only
// target to provide the raw-wire path "for bit-exact CONSUMERS" — a transcoder, a
// comparator, a materialized walk — not merely for the type's own re-encode.
//
// Dart privacy is per LIBRARY, so the captured bits sitting behind a leading
// underscore were out of reach of every consumer outside the generated file. The
// round-trip stayed bit-exact, which is exactly why a round-trip-only test never
// saw it, while any external walk got the widened double — whose quiet bit is
// already set, so a signaling NaN is unrecoverable.
//
// The array position was never affected: a decoded fp32 array is a Float32List
// whose byte buffer is public, so this is about scalar-field visibility alone.
func TestDartFp32RawBitsAreConsumerVisible(t *testing.T) {
	got := genFor(t, writeDef(t, `
version: 1
messages:
  Probe:
    payload:
      f32:  { id: 0, type: fp32 }
      arr:  { id: 1, type: array, items: { type: fp32, count: 4 } }
`), map[string]any{})
	// Public companion, reachable from another library.
	if !strings.Contains(got, "  int? f32Fp32Bits;") {
		t.Errorf("the fp32 raw-bits companion must be consumer-visible:\n%s", got)
	}
	// The defect: a library-private slot no consumer can read.
	if strings.Contains(got, "_f32Fp32Bits") {
		t.Errorf("the raw-bits companion must not be library-private (§6.5):\n%s", got)
	}
	// Behaviour is unchanged — it is still captured on a NaN decode, cleared on a
	// non-NaN one, reset by reset(), and preferred by serialize.
	for _, want := range []string{
		"o.f32Fp32Bits = bits;",
		"o.f32Fp32Bits = null;",
		"f32Fp32Bits = null;",
		"if (f32.isNaN && f32Fp32Bits != null)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the raw-bits channel must keep working, missing %q:\n%s", want, got)
		}
	}
}

// TestDartCallerOwnsTheEncodeBuffer: the output buffer belongs to the caller, and
// generated code IS that caller — it allocates the storage and hands it to the
// corelib, which allocates and grows nothing (CORELIB_PLAN §5.1).
//
// corelib-dart's `Encoder.encodeToBytes` is the shape that breaks this: it builds
// its own `Uint8List(bufferSize)` and its own BytesBuilder inside the package.
// Nothing this backend emits may use it, in any file.
//
// Which of the two conformant shapes applies is a property of the SCHEMA, so both
// arms are asserted here: a fully bounded message gets one exactly-sized buffer,
// an unbounded one a fixed scratch draining into caller-owned storage — sizing a
// buffer from the configured CEILING would silently refuse a larger message the
// caller legitimately built.
func TestDartCallerOwnsTheEncodeBuffer(t *testing.T) {
	bounded := genFor(t, writeDef(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      a: { id: 0, type: u32 }\n"+
		"      s: { id: 1, type: string, maxlen: 4 }\n"), map[string]any{})
	for _, want := range []string{
		"  static const int maxSize = 12;",
		"    final buf = Uint8List(maxSize);",
		"    final e = sofab.Encoder.overBuffer(buf, depth: 1);",
		"    return e.written;",
	} {
		if !strings.Contains(bounded, want) {
			t.Errorf("a bounded message must encode through one exactly-sized caller buffer: missing %q\n%s", want, bounded)
		}
	}
	// The derived size must not be dressed up as a ceiling: maxSizeLimit is how a
	// reader tells an IMPOSED number from one the schema supplies.
	if strings.Contains(bounded, "maxSizeLimit") {
		t.Errorf("a bounded message must emit the derived maxSize alone, not a ceiling:\n%s", bounded)
	}
	if strings.Contains(bounded, "BytesBuilder") {
		t.Errorf("a bounded message needs no drain sink at all:\n%s", bounded)
	}

	unbounded := genFor(t, writeDef(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      s: { id: 0, type: string }\n"), map[string]any{"max_message_size": 2048})
	for _, want := range []string{
		"  static const int maxSizeLimit = 2048;",
		"  static const int maxSize = maxSizeLimit;",
		"    final out = BytesBuilder(copy: true);",
		"    final e = sofab.Encoder(out.add, buffer: Uint8List(512), depth: 1);",
		"    e.flush();",
		"    return out.toBytes();",
	} {
		if !strings.Contains(unbounded, want) {
			t.Errorf("an unbounded message must drain a fixed scratch into caller storage: missing %q\n%s", want, unbounded)
		}
	}
	// The ceiling may never size the buffer: a message above it is legal.
	if strings.Contains(unbounded, "Uint8List(maxSize)") || strings.Contains(unbounded, "Uint8List(2048)") {
		t.Errorf("the configured ceiling must not size an encode buffer:\n%s", unbounded)
	}
	if strings.Contains(unbounded, "overBuffer") {
		t.Errorf("an unbounded message must not encode through a sink-less buffer:\n%s", unbounded)
	}

	// The corelib-allocating helper must appear nowhere — module, harness or
	// bench. Each of those encodes through the generated encode(), which is what
	// makes one assertion over the whole project enough.
	project := genFor(t, exampleDef, map[string]any{"emit": "project"})
	if strings.Contains(project, "encodeToBytes") {
		t.Errorf("an emitted file calls the corelib-allocating encodeToBytes:\n%s", project)
	}
}

// TestDartStructsGetNoEncodeEntryPoint: a struct/union serializes a headerless
// field RUN, not a message. Bytes handed back from an encode() on one would not be
// a message any decoder could read on its own, so only a message gets the entry
// point and the size constant that sizes its buffer.
func TestDartStructsGetNoEncodeEntryPoint(t *testing.T) {
	mod := genFor(t, writeDef(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      p: { id: 0, type: struct, fields: { x: { id: 0, type: i32 } } }\n"), map[string]any{})
	cls := mod[strings.Index(mod, "class MP {"):strings.Index(mod, "class M {")]
	if strings.Contains(cls, "Uint8List encode()") || strings.Contains(cls, "maxSize") {
		t.Errorf("a struct must not carry a message encode entry point:\n%s", cls)
	}
	if !strings.Contains(mod[strings.Index(mod, "class M {"):], "Uint8List encode()") {
		t.Errorf("the message must carry one:\n%s", mod)
	}
}

// TestDartNestedRowElemWidth is generator#330: a NESTED native row
// (array<array<u8>>) got no element-width guard at all — the row was stored with
// `_i64List(values)` and an over-width element went in unchecked.
// MESSAGE_SPEC §7.1 makes that INVALID, never a silent store.
//
// Unlike #267 this is an ABSENT bound rather than a late one, so it shows on a
// COMPLETE message — which is why the differential corpus never reached it.
func TestDartNestedRowElemWidth(t *testing.T) {
	got := genFor(t, writeDef(t, `
version: 1
messages:
  M:
    payload:
      urows: { id: 1, type: array, items: { type: array, count: 2, items: { type: u8,  count: 3 } } }
      srows: { id: 2, type: array, items: { type: array, count: 2, items: { type: i16, count: 3 } } }
      wide:  { id: 3, type: array, items: { type: array, count: 2, items: { type: u64, count: 3 } } }
`), map[string]any{})
	for _, want := range []string{
		// The scan itself is sofab.IntMatrixSeq's (corelib-dart#74, tested there
		// against decoded bytes). What this backend decides is the pair of bounds
		// it hands over, per row element kind.
		"sofab.IntMatrixSeq(o.urows, 2, false, 0, 255, rcap: 16384, rowCount: 3, rowCap: 16384)",
		"sofab.IntMatrixSeq(o.srows, 2, true, -32768, 32767, rcap: 16384, rowCount: 3, rowCap: 16384)",
		// u64 spans the callback parameter's own range, so lo == hi switches the
		// scan off rather than emitting a bound that can never fire.
		"sofab.IntMatrixSeq(o.wide, 2, false, 0, 0, rcap: 16384, rowCount: 3, rowCap: 16384)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestDartU64JSONCastOnlyWhereItDoesNotPromote: the harness parses a u64 from
// either a JSON string (the canonical carrier -- a u64 above 2^53 has no exact
// JSON-number form) or a bare number, and the `is String` test decides which. In
// the true arm the accessor is a String, so whether an `as String` belongs there
// is not a style question: Dart's flow analysis PROMOTES a local variable, and
// `dart analyze --fatal-warnings` -- this backend's entire build gate -- then
// rejects the cast as `unnecessary_cast`. A map index expression like
// `j['x']` does not promote (a second read could return something else), so
// there the cast is required.
//
// Both shapes therefore have to be emitted, and both have to be tested: a u64
// SCALAR takes the non-promoting arm and always worked, while a u64 ARRAY reads
// through the comprehension's own local and did not build at all. Nothing in the
// corpus had a 64-bit array until the bench schema gained one (generator#336),
// which is how a backend whose gate is "it analyzes cleanly" shipped a schema
// shape that could not analyze.
func TestDartU64JSONCastOnlyWhereItDoesNotPromote(t *testing.T) {
	got := genFor(t, writeDef(t, `
version: 1
messages:
  M:
    payload:
      scalar: { id: 0, type: u64 }
      arr:    { id: 1, type: array, items: { type: u64, count: 4 } }
      rows:   { id: 2, type: array, items: { type: array, count: 2, items: { type: u64, count: 3 } } }
`), map[string]any{"emit": "project"})

	// The scalar reads a map index: no promotion, so the cast stays.
	if !strings.Contains(got, "j['scalar'] is String ? BigInt.parse(j['scalar'] as String)") {
		t.Errorf("a u64 scalar must keep `as String` (a map index does not promote):\n%s", got)
	}
	// Array elements read the comprehension local: promoted, so no cast.
	for _, want := range []string{
		"m.arr.assign(<int>[for (final _b in (j['arr'] as List)) (_b is String ? BigInt.parse(_b) :",
		"for (final _x in (j['rows'] as List)) sofab.InlineInt64Array.of(<int>[for (final _b in (_x as List)) (_b is String ? BigInt.parse(_b) :",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a u64 array element must NOT cast a promoted local (unnecessary_cast is fatal here); missing %q in:\n%s", want, got)
		}
	}
	// The shape the analyzer rejects, in either nesting.
	for _, gone := range []string{"BigInt.parse(_b as String)", "BigInt.parse(_x as String)", "BigInt.parse(_y as String)"} {
		if strings.Contains(got, gone) {
			t.Errorf("emitted %q, which `dart analyze --fatal-warnings` rejects as unnecessary_cast:\n%s", gone, got)
		}
	}
}

// TestDartWrapperIndexCapTravelsUnraised: every receiver cap the module carries
// is the number the deployment configured, and NOTHING is raised any more.
//
// A wrapper array carries no count header — its elements are keyed by an
// unbounded varint index and the list is grown to fit — so the index IS the
// length and the index is what bounds the allocation. corelib-dart's collectors
// take that bound as `rcap`, consulted only where the schema declared no
// `count:`.
//
// The two numbers used to differ on purpose: `maxDynArrayCount` reached a
// DecoderLimits, which applies per decode to every field alike, so it had to
// clear the largest schema `count:` in the message or it rejected a
// schema-bounded field CORELIB_PLAN §6.2.1 forbids it to touch — and the wrapper
// index cap, which cannot reach a bounded field at all, needed a second,
// unraised constant of its own. With EVERY cap enforced per field, nothing needs
// the raise and the second constant collapses back into the first.
func TestDartWrapperIndexCapTravelsUnraised(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      w: { id: 0, type: array, items: { type: string } }
      b: { id: 1, type: array, items: { type: string, count: 100 } }
`
	out := genFor(t, writeDef(t, src), map[string]any{"max_dyn_array_count": 4})

	for _, want := range []string{
		// One constant, AS CONFIGURED, below b's count: 100.
		"const int maxDynArrayCount = 4;",
		// The unbounded array is governed by it...
		"sofab.StringSeq(o.w, -1, -1, rcap: maxDynArrayCount,",
		// ...and the schema-bounded one carries it too, inert: there the schema
		// bound governs and its breach is INVALID, never limitExceeded. Emitting it
		// anyway is what keeps the argument list one shape, and corelib-dart
		// requires it — leaving it out is not "the corelib's default" but a
		// compile error, §6.2.1 admitting no unset state.
		"sofab.StringSeq(o.b, 100, -1, rcap: maxDynArrayCount,",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "maxDynWrapperIndex") {
		t.Errorf("the second, unraised constant is gone with the raise:\n%s", out)
	}
	if strings.Contains(out, "const int maxDynArrayCount = 100;") {
		t.Errorf("no cap may be raised to a sibling's schema bound:\n%s", out)
	}
}

// TestDartBitfieldReadsJSONAsUnsigned: the generated harness reads a bitfield the
// way it reads a u64, because that is what a bitfield is -- an unsigned 64-bit
// mask in a SIGNED Dart `int`.
//
// The arm it used to share with the small integers, `(x as num).toInt()`, cannot
// carry one. jsonDecode hands back a double for any integer literal above 2^53,
// which has already lost bits, and `toInt()` then CLAMPS to 2^63-1 rather than
// throwing: a mask with bit 63 set encoded four bytes short, with a zero exit
// status (generator#470). The u64 arm parses the quoted spelling exactly through
// BigInt and still accepts a bare JSON number, so it covers both inputs.
func TestDartBitfieldReadsJSONAsUnsigned(t *testing.T) {
	// bitfields.yaml declares LOW at pos 0 and HIGH at pos 63.
	out := genFor(t, "../../tests/matrix/corpus/defs/bitfields.yaml", map[string]any{"emit": "project"})
	if !strings.Contains(out, "BigInt.parse(j['flags'] as String)") {
		t.Error("a bitfield must read its JSON through the u64 BigInt path")
	}
	if strings.Contains(out, "m.flags = (j['flags'] as num).toInt();") {
		t.Error("(x as num).toInt() clamps a mask with bit 63 set instead of carrying it")
	}
	// The WRITE half of the same treatment. Dart's `int` is signed, so a mask
	// with bit 63 set prints as -1 unless it goes out the way a u64 does --
	// where python, go, rust, C#, kotlin and typescript all print
	// 18446744073709551615. Nothing else catches it: the shared max-fill message
	// is encode-only in every suite, so the asymmetry never shows up as a failed
	// comparison.
	if !strings.Contains(out, "BigInt.from(m.flags).toUnsigned(64).toString()") {
		t.Error("a bitfield reaching bit 63 must WRITE its JSON unsigned, not as a negative int")
	}
	// A narrow bitfield keeps the plain JSON number every hand-written input in
	// the tree spells and the round-trip greps expect (`"somebitfield":2`).
	ex := genFor(t, exampleDef, map[string]any{"emit": "project"})
	if strings.Contains(ex, "BigInt.from(m.somebitfield)") {
		t.Error("a bitfield that fits below bit 32 must stay a plain JSON number")
	}
	if !strings.Contains(ex, "'somebitfield': m.somebitfield,") {
		t.Error("a narrow bitfield's JSON write must pass straight through")
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
// UNSIGNED type holding its highest declared `pos`. Here that is i8 (-128..127)
// for {0, 1, 2, 10} and u8 (0..255) for positions 0, 1 and 3.
//
// The bound is not the integer the target stores the field in: Dart keeps both
// kinds in its own 64-bit `int`, which is §1's fourth consequence -- a receiver
// that cannot hold the field at exactly the declared width holds it wider and
// MUST enforce the width as an explicit check, because nothing about its storage
// will. Nothing is narrowed on the way in, so the guard runs on the value the
// corelib handed over and no truncation can get in front of it.
//
// All six positions are pinned by name -- scalar, native array element, struct
// member, struct-array element member, union member, matrix row element -- for
// both kinds. Four of them share one emitted arm per kind (this backend gives a
// struct, a struct-array element and a union their own child visitors, and the
// arm text is identical in all of them), which is exactly why "the arm is
// shared" is not worth trusting after the next refactor.
func TestDartEnumAndBitfieldWidthBoundAtEverySixPositions(t *testing.T) {
	got := genFor(t, writeDef(t, widthSixSrc), map[string]any{})
	// The bitfield test masks the WIDTH off the raw 64-bit word and deliberately
	// has no `value < 0` term: an unsigned wire value at or above 2^63 arrives
	// negative in Dart, and such a value has bits set above the width anyway, so
	// one mask refuses it and every over-width value in a single operation.
	const bfRej = "        if ((value & ~0xff) != 0) { invalidate(); return; }\n        "
	const enRej = "        if (value < -128 || value > 127) { invalidate(); return; }\n        "
	for _, want := range []string{
		// 1. scalar
		enRej + "o.en = value;",
		bfRej + "o.bf = value;",
		// 3./4./5. struct member, struct-array element member, union member -- one
		// arm text, emitted into each of the three child visitors.
		enRej + "o.se = value;",
		bfRej + "o.sbf = value;",
		enRej + "o.ue = value;",
		bfRej + "o.ubf = value;",
		// 2. native array element: the interval rides on the destination, and the
		// codec applies it AT each element as it is decoded -- so a value outside
		// the width is refused whether the array completes or is cut short behind
		// it (§5.2).
		"final sofab.InlineInt64Array ea = sofab.InlineInt64Array(4, range: const sofab.ElemRange(-128, 127));",
		"final sofab.InlineInt64Array bfa = sofab.InlineInt64Array(4, range: const sofab.ElemRange(0, 255));",
		// 6. matrix row element. The row's values never reach the generated visitor
		// -- sofab.IntMatrixSeq gathers them and places the finished row -- so the
		// collector's own lo/hi pair is the whole bound, and an interval is exactly
		// what it can carry.
		"return sofab.IntMatrixSeq(o.mat, 2, true, -128, 127, rcap: 16384, rowCount: 3, rowCap: 16384);",
		"return sofab.IntMatrixSeq(o.mbf, 2, false, 0, 255, rcap: 16384, rowCount: 3, rowCap: 16384);",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Closed message.dart: a position stores without its §1 width bound, missing %q", want)
		}
	}
	for _, bad := range []string{
		"      case 0:\n        o.en = value;",
		"      case 1:\n        o.bf = value;",
		"sofab.InlineInt64Array ea = sofab.InlineInt64Array(4);",
		"sofab.InlineInt64Array bfa = sofab.InlineInt64Array(4);",
		// The generated collector subclass the set/mask bound needed is gone: the
		// bound travels through the corelib's own lo/hi pair now.
		"extends sofab.IntMatrixSeq {",
		"sofab.IntMatrixSeq(o.mat, 2, true, 0, 0,",
		"sofab.IntMatrixSeq(o.mbf, 2, false, 0, 0,",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("Closed message.dart still stores an enum/bitfield unguarded (%q)", bad)
		}
	}
}

// The elision under the width rule: a guard is emitted only where the implied
// width is NARROWER than the 64-bit accumulator the value arrives in. A bitfield
// whose highest declared position is 63 implies u64, so nothing reachable can
// breach the bound and the clause would be dead code. The enum half of the same
// elision is unreachable from a valid schema -- the validator caps a constant at
// the signed 32-bit range, so an enum never implies more than i32.
//
// Note what is NOT an elision any more: whether an enum's constants are
// contiguous no longer matters at all. The width is derived from the extremes,
// so {0,1,2} and {0,1,2,10} produce the identical i8 guard, and the membership
// chain a gapped set used to need is gone.
func TestDartEnumBitfieldWidthElisions(t *testing.T) {
	var bits []string
	for i := 0; i < 64; i++ {
		bits = append(bits, fmt.Sprintf("F%d: { pos: %d }", i, i))
	}
	got := genFor(t, writeDef(t, "version: 1\nmessages:\n  W:\n    payload:\n"+
		"      f: { id: 0, type: bitfield, bits: { "+strings.Join(bits, ", ")+" } }\n"+
		"      e: { id: 1, type: enum, enum: { R: 0, G: 1, B: 2 }, default: 0 }\n"),
		map[string]any{})
	if !strings.Contains(got, "      case 0:\n        o.f = value;") {
		t.Errorf("a bitfield implying the full u64 width must store unguarded:\n%s", got)
	}
	if strings.Contains(got, "0xffffffffffffffff") {
		t.Errorf("a tautological mask guard was emitted:\n%s", got)
	}
	// {R:0, G:1, B:2} implies i8, NOT the 0..2 hull of its constants: 5 is a valid
	// wire value for this field and must decode.
	if !strings.Contains(got, "        if (value < -128 || value > 127) { invalidate(); return; }\n        o.e = value;") {
		t.Errorf("a contiguous enum must take the implied i8 width, not its constant hull:\n%s", got)
	}
}

// A bitfield declaring position 63 implies u64 -- the accumulator's own width --
// so under the width rule it carries NO guard at all, at either position. Under
// the withdrawn closed-set rule the same declaration emitted a
// `~0x8000000000000001` mask, which is the literal-rendering trap of
// generator#470: Dart has no unsigned int, so that mask exists only as a hex
// literal. Deriving the bound from the highest position removes both the trap
// and the comparison, and the interval hooks that had to decline such a maximum
// are not asked for one any more.
func TestDartBitfieldSpanningBit63IsUnguarded(t *testing.T) {
	got := genFor(t, writeDef(t, "version: 1\nmessages:\n  W:\n    payload:\n"+
		"      g:  { id: 0, type: bitfield, bits: { LOW: { pos: 0 }, HIGH: { pos: 63 } } }\n"+
		"      ga: { id: 1, type: array, items: { type: bitfield, count: 2, bits: { LOW: { pos: 0 }, HIGH: { pos: 63 } } } }\n"),
		map[string]any{})
	for _, want := range []string{
		"      case 0:\n        o.g = value;",
		"final sofab.InlineInt64Array ga = sofab.InlineInt64Array(2);",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a bitfield implying the full u64 width must store unguarded, missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "0x8000000000000001") {
		t.Errorf("the withdrawn flag-mask guard was emitted:\n%s", got)
	}
	if strings.Contains(got, "sofab.ElemRange(") {
		t.Errorf("a u64-implying element width states no interval:\n%s", got)
	}
}

// The behavioural difference the width rule makes, stated as the values
// themselves: a gapped enum admits a value between its constants, and a bitfield
// admits an undeclared bit -- both INVALID under the withdrawn closed-set rule.
// Pinned on the emitted bound so a silent reversion is loud.
func TestDartWidthAdmitsUndeclaredValues(t *testing.T) {
	got := genFor(t, writeDef(t, widthSixSrc), map[string]any{})
	// enum {0,1,2,10}: the guard must admit 5 -- i.e. be the i8 interval, never a
	// membership chain over the constants, and never their 0..10 hull.
	if strings.Contains(got, "value != 10") || strings.Contains(got, "_v != 10") {
		t.Errorf("the withdrawn membership chain over enum constants was emitted:\n%s", got)
	}
	if strings.Contains(got, "sofab.ElemRange(0, 10)") {
		t.Errorf("the withdrawn constant hull was stated as the element interval:\n%s", got)
	}
	// bitfield pos{0,1,3}: the guard must admit 4 -- i.e. mask the WIDTH (0xff),
	// never the flag mask (0xb).
	if strings.Contains(got, "~0xb)") {
		t.Errorf("the withdrawn flag-mask guard was emitted:\n%s", got)
	}
	if !strings.Contains(got, "~0xff)") {
		t.Errorf("the bitfield width mask is missing:\n%s", got)
	}
}

// TestDartDecoderAsksTheStreamForItsVerdict pins issue #555, the Dart half of
// #541: the generated decoder remembers NOTHING about the outcome. corelib-dart's
// Decoder already latches a refusal -- `if (_terminal) return _terminalStatus;`
// at the top of feed, before a byte is looked at -- so a copy here could only
// restate it, and CORELIB_PLAN §5.2.1 rules out any surface beside feed holding
// one. finish therefore ASKS, with a zero-length feed.
func TestDartDecoderAsksTheStreamForItsVerdict(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		// feed forwards and nothing more.
		"  sofab.DecodeStatus feed(List<int> chunk) => _d.feed(chunk);",
		"  Myfirstmessage? finish() =>",
		"      _d.feed(const <int>[]) == sofab.DecodeStatus.complete ? _out : null;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated decoder missing %q (generator#555)", want)
		}
	}
	for _, gone := range []string{
		"sofab.DecodeStatus _st",
		"_st = _d.feed",
		"get status",
		"dec.status",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("generated code still carries the removed status copy %q (generator#555)", gone)
		}
	}
}
