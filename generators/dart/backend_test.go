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
