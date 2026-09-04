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
		"import 'package:sofa_buffers_corelib/sofa_buffers_corelib.dart' as sofab;",
		"class Myfirstmessage {",
		"void serialize(sofab.Encoder e) {",
		// example.yaml has an unbounded field, so encode() takes the scratch+sink
		// arm (TestDartCallerOwnsTheEncodeBuffer covers both).
		"final e = sofab.Encoder(out.add, buffer: Uint8List(512));",
		"static sofab.DecodeStatus tryDecode(Uint8List data, Myfirstmessage out) {",
		"static Myfirstmessage decode(Uint8List data) {",
		"class _MyfirstmessageVisitor extends sofab.VisitorBase {",
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
		// Scalars/strings/blobs are values: assignment is the reset.
		"    someu8 = 7;",
		"    somestring = '';",
		"    someblob = Uint8List.fromList(<int>[72, 101, 108, 108, 111]);",
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
	if got, want := strings.Count(out, "  void reset() {"), strings.Count(out, "  void serialize(sofab.Encoder e) {"); got != want {
		t.Errorf("reset() on %d classes, serialize on %d: every object class needs both", got, want)
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
		"int? somefp32Fp32Bits;",
		"void onFp32Bits(int id, int bits) {",
		"o.somefp32Fp32Bits = bits;",
		"o.somefp32 = _f32FromBits(bits);",
		"o.somefp32Fp32Bits = null;",
		"if (somefp32.isNaN && somefp32Fp32Bits != null) { e.writeFp32Bits(8, somefp32Fp32Bits!); }",
		// Array: a bit-exact Float32List copy, never a widening List<double>.from.
		"o.somefloatarray = sofab.copyFp32(values, values.length);",
		// exactly the wire count: `count: 3` is a capacity and adds no elements (§3)
		"o.somefloatarray = sofab.copyFp32(values, values.length);",
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
		"if (kind == sofab.ArrayKind.unsigned && count > 4) invalidate();", // someuintarray, count 4
		// Each maxlen guard is gated on the DECLARED fixlen subtype: onFixlenHeader
		// fires for any subtype at a field id, and a contradicting one must be
		// skipped, not measured against this field's bound (§7.3, generator#224).
		"if (subtype == sofab.FixlenType.string && length > 50) invalidate();", // somestring
		"if (subtype == sofab.FixlenType.blob && length > 16) invalidate();",   // someblob
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing header-visitor guard %q", want)
		}
	}
	// The bound must never be enforced on length alone — an un-gated compare is
	// exactly the generator#224 defect (an fp64 landing on a `maxlen: 4` blob was
	// rejected as INVALID instead of skipped).
	for _, notWant := range []string{
		"if (length > 50) invalidate();",
		"if (length > 16) invalidate();",
		// ...and the array count bound is the same defect one hook over
		// (generator#259 / F-0042): an un-gated compare measures a contradicting
		// array kind against this field's N.
		"if (count > 4) invalidate();",
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

// TestDartArrayElemBound covers generator#267's element position: an array
// element outside its DECLARED WIDTH is INVALID (§7.1) and, established by its
// own bytes, dominates a truncation behind it (§5.2). The `for (final _v in
// values)` scan decides an array that arrives and never runs for one that does
// not, so the bound also goes to the corelib as onArrayElemBound, which applies
// it while the elements go past.
func TestDartArrayElemBound(t *testing.T) {
	out := genFor(t, exampleDef, map[string]any{})
	for _, want := range []string{
		"sofab.ElemRange? onArrayElemBound(int id, sofab.ArrayKind kind) {",
		// Gated on the declared element kind, like every other header-time bound
		// (§7.3): the hook is asked per field id, and an array whose wire kind
		// contradicts the declaration is skipped, never measured against this
		// field's width.
		"if (kind == sofab.ArrayKind.unsigned) {\n          return const sofab.ElemRange(0, 4294967295);\n        }",
		"if (kind == sofab.ArrayKind.signed) {\n          return const sofab.ElemRange(-2147483648, 2147483647);\n        }",
		// `const`, so answering costs no allocation.
		"return const sofab.ElemRange(",
		"return null;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing element bound %q", want)
		}
	}
	// The scan over the assembled list stays: it still bounds the elements
	// against a corelib that does not know the callback, over a list in hand.
	if !strings.Contains(out, "for (final _v in values)") {
		t.Errorf("the assembled-list width scan must stay")
	}
	// A message with no narrowed array element must not override it at all —
	// scalars.yaml has no arrays.
	plain := genFor(t, "../../tests/matrix/corpus/defs/scalars.yaml", map[string]any{})
	if strings.Contains(plain, "onArrayElemBound(") {
		t.Errorf("a message with no narrowed array element must not override onArrayElemBound")
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
		// the unbounded string and blob: policy, at the length word, gated on the
		// declared subtype so a §7.3 mismatch is skipped rather than capped.
		"case 0:\n        if (subtype == sofab.FixlenType.string && length > maxDynStringLen) limitExceeded();",
		"case 1:\n        if (subtype == sofab.FixlenType.blob && length > maxDynBlobLen) limitExceeded();",
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
		"case 0:\n        if (kind == sofab.ArrayKind.unsigned && count > maxDynArrayCount) limitExceeded();",
		"case 1:\n        if (kind == sofab.ArrayKind.signed && count > 100000) invalidate();",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing %q:\n%s", want, out)
		}
	}
}

// Rule 5 for Dart: corelib-dart's onBytesDest/onArrayDest defaults allocate a
// destination sized from the wire, so a generated scope has to DECLINE every id
// it does not bind or a §7.3-skipped field is materialized anyway — the one
// shape no receiver cap ever covered, and the decoder now holds none.
func TestDartDeclinesUnboundDestinations(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string }\n" +
		"      a: { id: 1, type: array, items: { type: u32 } }\n" +
		"  N:\n    payload:\n      x: { id: 0, type: u32 }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	for _, want := range []string{
		"Uint8List? onBytesDest(int id, int subtype, int total) {",
		"TypedData? onArrayDest(int id, sofab.ArrayKind kind, int count) {",
		// a bound id keeps the corelib's exactly-sized destination...
		"case 0:\n        if (subtype == sofab.FixlenType.string) return super.onBytesDest(id, subtype, total);\n        return null;",
		"case 1:\n        if (kind == sofab.ArrayKind.unsigned) return super.onArrayDest(id, kind, count);\n        return null;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated module missing %q:\n%s", want, out)
		}
	}
	// ...and a scope that binds NOTHING of that shape still declines: the
	// override is emitted with no switch at all, which is the case that would
	// otherwise fall through to the allocating default.
	nv := out[strings.Index(out, "class _NVisitor"):]
	for _, want := range []string{
		"Uint8List? onBytesDest(int id, int subtype, int total) {\n    return null;\n  }",
		"TypedData? onArrayDest(int id, sofab.ArrayKind kind, int count) {\n    return null;\n  }",
	} {
		if !strings.Contains(nv, want) {
			t.Errorf("a scope binding no payload must decline every destination, missing %q:\n%s", want, nv)
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
		"    if (!sofab.elementsEqual(withdef, <int>[1, 2])) { e.writeUnsignedArray(5, withdef); }",
		// ...and _isDefault is the exact negation of it.
		"    if (!(fnums.isEmpty)) return false;",
		"    if (!(sofab.elementsEqual(withdef, <int>[1, 2]))) return false;",
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
			"      e.beginSequenceLazy(_i0); fixed[_i0].serialize(e);\n" +
			"      if (_i0 == fixed.length - 1) { e.endSequenceKeep(); } else { e.endSequence(); }\n" +
			"    }",
		"    for (var _i0 = 0; _i0 < dynamic_.length; _i0++) {\n" +
			"      e.beginSequenceLazy(_i0); dynamic_[_i0].serialize(e);\n" +
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
		"serialize(e); e.endSequenceKeep();",
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
		"sofab.NestedSeq<String>(o.srows, 3, (p) => sofab.StringSeq(p, -1, 4, rcap: maxDynArrayCount, relemMax: 262144), rcap: maxDynArrayCount)",
		// M elements arrived, M is the length: the count word is bounded, nothing
		// is filled in behind it.
		("        if (values.length > 4) { invalidate(); return; }\n" +
			"        for (final _v in values) { if (_v < 0 || _v > 4294967295) { invalidate(); return; } }\n" +
			"        o.fnums = List<int>.from(values);\n        return;"),
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
		"o.f32s = sofab.copyFp32(values, values.length);",
		"o.f64s = List<double>.from(values);",
		"  List<double> f32s = <double>[];",
		"  List<double> f64s = <double>[];",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sofab.copyFp32(values, 3)") || strings.Contains(out, "_padTo(") {
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
		"      case 0:\n        if (kind == sofab.ArrayKind.fp32 && count > 3) invalidate();\n        return;",
		"      case 1:\n        if (kind == sofab.ArrayKind.fp64 && count > 5) invalidate();\n        return;",
		// Integer arrays take the same shape -- there is no second wire word on
		// that path, but the declared kind still gates the bound.
		"      case 2:\n        if (kind == sofab.ArrayKind.unsigned && count > 7) invalidate();\n        return;",
		"      case 3:\n        if (kind == sofab.ArrayKind.signed && count > 9) invalidate();\n        return;",
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
		"if (count > 3) invalidate();",
		"if (count > 5) invalidate();",
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
	// One strict corelib decode, inside the arm: valid bytes in, String out, null
	// for anything malformed. The arm is braced because Dart switch cases share
	// one scope.
	for _, want := range []string{
		"final s = sofab.decodeUtf8Strict(bytes);",
		"if (s == null) { invalidate(); return; }",
		"o.s = s;",
		"o.u = s;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
	// The maxlen bound reads the wire length directly now — no re-encode.
	if !strings.Contains(out, "if (bytes.length > 8) { invalidate(); return; }") {
		t.Errorf("the maxlen bound must measure the raw wire bytes:\n%s", out)
	}
	// A blob carries no encoding: its arm must not validate.
	if strings.Contains(out, "void onBlob(int id, Uint8List value)") &&
		strings.Contains(out, "utf8Valid(value)") {
		t.Errorf("blob must never be UTF-8-validated:\n%s", out)
	}
}

// TestDartStringFreeScopeSkipsStrings: the residual of #257 (generator#265 /
// Crucible F-0038). #257 fixed the scopes that HAVE a string field; a scope with
// none emitted no onStringBytes override at all and therefore inherited
// sofab.MessageVisitor's default — which validates the payload as UTF-8 and
// flags the decode INVALID. That default is right for a hand-written visitor
// (it has no schema, so every string it is handed is one it wanted) and wrong
// for generated code, where the id decides: an undeclared string is a skip whose
// bytes are never inspected (CORELIB_PLAN §6.4, MESSAGE_SPEC §7.3).
//
// A lone continuation byte at an undeclared id — `4a 0a 8a` — therefore turned
// an otherwise valid message INVALID in dart alone, on 12 implementations that
// accept it. The fix is one shared base carrying the no-op, so the property
// holds for every visitor by construction and not per emission site.
func TestDartStringFreeScopeSkipsStrings(t *testing.T) {
	// Nothing here declares a string: the top-level message, its nested struct,
	// and every collector scope (blob array, struct array, native matrix) are all
	// string-free — exactly the scopes that inherited the validating default.
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a:  { id: 0, type: u32 }\n" +
		"      b:  { id: 1, type: blob, maxlen: 8 }\n" +
		"      n:  { id: 2, type: struct, fields: { k: { id: 0, type: u32 } } }\n" +
		"      ba: { id: 3, type: array, items: { type: blob, count: 4, maxlen: 8 } }\n" +
		"      sa: { id: 4, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }\n" +
		"      m:  { id: 5, type: array, items: { type: array, count: 2, items: { type: u32, count: 2 } } }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})

	// The base is corelib-dart's sofab.VisitorBase (corelib-dart#65), so a copy of
	// it may no longer be emitted.
	if strings.Contains(out, "abstract class _Visitor") {
		t.Errorf("the visitor base belongs to the corelib and must not be emitted:\n%s", out)
	}
	// Every generated visitor routes through it. Extending sofab.MessageVisitor
	// directly is the defect: that is what re-inherits the validating onStringBytes
	// default, and the DESCENDING onSequenceStart default one wire type over --
	// which is what would let a sequence at a leaf element position bind its child
	// as that element (generator#272, TestDartMistypedSequenceElementIsSkipped).
	for _, decl := range []string{
		"class _MVisitor extends sofab.VisitorBase {",
		"class _MNVisitor extends sofab.VisitorBase {",
	} {
		if !strings.Contains(out, decl) {
			t.Errorf("missing %q — every visitor must extend the corelib base:\n%s", decl, out)
		}
	}
	if strings.Contains(out, "extends sofab.MessageVisitor") {
		t.Errorf("no generated visitor may extend sofab.MessageVisitor directly:\n%s", out)
	}
	// A string-free module still must not validate, transcode or import for one.
	if strings.Contains(out, "utf8Valid") || strings.Contains(out, "utf8.decode") {
		t.Errorf("a string-free schema must never validate or transcode a string:\n%s", out)
	}
}

// A string-declaring scope keeps its own arms and still falls through to the
// base's no-op for every id it does not match — the switch has no default arm,
// so an unmatched id leaves the method without inspecting the bytes.
func TestDartStringScopeFallsThroughToSkip(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string, maxlen: 8 }\n"
	out := genFor(t, writeDef(t, src), map[string]any{})
	if !strings.Contains(out, "class _MVisitor extends sofab.VisitorBase {") {
		t.Errorf("a string-declaring visitor must extend the base too:\n%s", out)
	}
	i := strings.Index(out, "void onStringBytes(int id, Uint8List bytes) {\n    switch (id) {")
	if i < 0 {
		t.Fatalf("expected an id switch in the string destination override:\n%s", out)
	}
	if j := strings.Index(out[i:], "default:"); j >= 0 && j < strings.Index(out[i:], "\n  }") {
		t.Errorf("the override must fall out of the switch for an unmatched id, not handle it:\n%s", out)
	}
}

// No generated module carries the `dart:convert` import any more: the only thing
// that ever needed it was `utf8.decode` in the string destinations, and those
// call sofab.decodeUtf8Strict now. An import nothing uses is a `dart analyze`
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
		// The array arrives whole: one scan over the elements decides it.
		"for (final _v in values) { if (_v < 0 || _v > 255) { invalidate(); return; } }",
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
// The fix sits on the shared base beside the onStringBytes no-op — corelib-dart's
// sofab.VisitorBase since corelib-dart#65 — so every collector inherits the skip
// by construction, including ones added later.
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
	if strings.Contains(got, "extends sofab.MessageVisitor") {
		t.Errorf("every visitor must inherit the base's sequence skip, never MessageVisitor's descent:\n%s", got)
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
		"    final e = sofab.Encoder.overBuffer(buf);",
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
		"    final e = sofab.Encoder(out.add, buffer: Uint8List(512));",
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
// `List<int>.from(values)` and an over-width element went in unchecked.
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
		"for (final _x in (j['arr'] as List)) (_x is String ? BigInt.parse(_x) :",
		"for (final _x in (j['rows'] as List)) <int>[for (final _y in (_x as List)) (_y is String ? BigInt.parse(_y) :",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a u64 array element must NOT cast a promoted local (unnecessary_cast is fatal here); missing %q in:\n%s", want, got)
		}
	}
	// The shape the analyzer rejects, in either nesting.
	for _, gone := range []string{"BigInt.parse(_x as String)", "BigInt.parse(_y as String)"} {
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
}
