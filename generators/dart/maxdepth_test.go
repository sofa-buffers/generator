package dart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// deepSchema nests every sequence-opening shape serialize emits -- struct
// field, union arm, array of struct, wrapper rows of wrapper rows, string and
// blob wrappers -- so the deepest path (rows -> row -> element -> leaves ->
// leaf -> tags) opens six frames at once. Loose is the same depth question on
// the unbounded arm of encode() (the scratch+sink Encoder): rows of strings,
// no count and no maxlen anywhere.
const deepSchema = `
version: 1
$defs:
  struct:
    Leaf:
      tags: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }
      n: { id: 1, type: u32 }
    Mid:
      leaves: { id: 0, type: array, items: { type: struct, count: 2, fields: { $ref: '#/$defs/struct/Leaf' } } }
      cube:
        id: 1
        type: array
        items: { type: array, count: 2, items: { type: array, count: 2, items: { type: blob, count: 2, maxlen: 4 } } }
messages:
  Deep:
    payload:
      mid: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Mid' } }
      rows:
        id: 1
        type: array
        items: { type: array, count: 2, items: { type: struct, count: 2, fields: { $ref: '#/$defs/struct/Mid' } } }
      u:
        id: 2
        type: union
        default_id: 0
        oneof:
          x: { id: 0, type: u16 }
          m: { id: 1, type: struct, fields: { $ref: '#/$defs/struct/Mid' } }
  Loose:
    payload:
      rows: { id: 0, type: array, items: { type: array, items: { type: string } } }
`

// encode() passes the message's static nesting depth to the Encoder it builds
// (`depth:`), so corelib-dart sizes its held-back sequence run to the schema
// instead of MAX_DEPTH. The depth must count exactly the frames serialize
// opens: one short and a valid value throws invalidArgument, which the
// corelib-gated TestDartEncodeMaxDepthRoundTrip below proves on real encodes.
func TestDartEncodeMaxDepth(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		depth         int
	}{
		// No frame at all: the constant is 0, and the call still passes 1,
		// because the corelib accepts depth 1..MAX_DEPTH only.
		{"scalars only", `a: { id: 0, type: u32 }`, 0},
		{"native array", `v: { id: 0, type: array, items: { type: u32, count: 4 } }`, 0},
		// The outer wrapper opens a frame; each native row is one value inside it.
		{"native rows", `m: { id: 0, type: array, items: { type: array, count: 2, items: { type: u8, count: 2 } } }`, 1},
		{"string array", `s: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }`, 1},
		{"wrapper rows of strings", `
      c:
        id: 0
        type: array
        items: { type: array, count: 2, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }`, 3},
		// Unbounded: encode() takes the scratch+sink arm, which must pass it too.
		{"unbounded string array", `s: { id: 0, type: array, items: { type: string } }`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := genFor(t, writeDef(t, "version: 1\nmessages:\n  d:\n    payload:\n      "+strings.TrimSpace(tc.payload)+"\n"), map[string]any{})
			arg := "depth: maxDepth)"
			if tc.depth == 0 {
				arg = "depth: 1)"
			}
			if want := "  static const int maxDepth = " + strconv.Itoa(tc.depth) + ";\n"; !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
			// The one Encoder the file constructs (encode(), either arm) takes the
			// bound; encodeTo's encoder is the caller's.
			if n, all := strings.Count(got, arg+";"), strings.Count(got, "final e = sofab.Encoder"); n != all || n != 1 {
				t.Errorf("%d of %d encoder constructors pass %q, want 1 of 1:\n%s", n, all, arg, got)
			}
		})
	}

	deep := genFor(t, writeDef(t, deepSchema), map[string]any{})
	for _, want := range []string{
		"  static const int maxDepth = 6;\n", // Deep
		"  static const int maxDepth = 2;\n", // Loose
		"    final e = sofab.Encoder.overBuffer(buf, depth: maxDepth);",
		"    final e = sofab.Encoder(out.add, buffer: Uint8List(512), depth: maxDepth);",
	} {
		if !strings.Contains(deep, want) {
			t.Errorf("deepSchema: missing %q:\n%s", want, deep)
		}
	}
	// Only messages are encoded standalone: a struct gets no constant.
	if n := strings.Count(deep, "static const int maxDepth"); n != 2 {
		t.Errorf("maxDepth emitted %d times, want once per message (2):\n%s", n, deep)
	}
}

// TestDartEncodeMaxDepthRoundTrip builds deepSchema against a real corelib-dart
// and fills the deepest path of every branch, so every frame the depth counts
// is actually opened. The value must encode through encode() (both arms) and a
// caller-built encodeTo encoder identically, decode, and re-encode
// byte-identically; an Encoder built ONE level tighter than maxDepth must refuse
// the same value with invalidArgument -- which pins the count from both sides.
// Gated on SOFAB_DART_CORELIB (a checkout whose Encoder takes `depth:`) and the
// dart toolchain.
func TestDartEncodeMaxDepthRoundTrip(t *testing.T) {
	runDartDriver(t, deepSchema, dartDepthDriver, "depth round trip: PASS")
}

// runDartDriver generates `schema` into a scratch package named `rt`, adds
// `driver` as bin/rt.dart and runs it against the corelib-dart checkout in
// SOFAB_DART_CORELIB, failing unless its output contains `pass`. Skipped when
// SOFAB_DART_CORELIB or the dart toolchain is missing.
func runDartDriver(t *testing.T, schema, driver, pass string) {
	t.Helper()
	corelib := os.Getenv("SOFAB_DART_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_DART_CORELIB to a corelib-dart checkout to run the Dart driver")
	}
	if _, err := exec.LookPath("dart"); err != nil {
		t.Skip("dart toolchain not on PATH")
	}
	abs, err := filepath.Abs(corelib)
	if err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(schemaFor(t, writeDef(t, schema)), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, "lib", f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pubspec := "name: rt\npublish_to: none\nenvironment:\n  sdk: ^3.4.0\n" +
		"dependencies:\n  sofa_buffers_corelib:\n    path: " + abs + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "rt.dart"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	// Analyzed with infos fatal before it runs: the conformance suite's gate on
	// generated code (ARCHITECTURE §12 gate 9) holds here too.
	for _, args := range [][]string{{"pub", "get"}, {"analyze", "--fatal-infos"}, {"run", "bin/rt.dart"}} {
		cmd := exec.Command("dart", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dart %v: %v\n%s", args, err, out)
		}
		if args[0] == "run" && !strings.Contains(string(out), pass) {
			t.Fatalf("driver did not report %q:\n%s", pass, out)
		}
	}
}

const dartDepthDriver = `import 'dart:io';
import 'dart:typed_data';
import 'package:sofa_buffers_corelib/sofa_buffers_corelib.dart' as sofab;
import 'package:rt/message.dart';

sofab.InlineBytes b(List<int> v) => sofab.InlineBytes.of(v);

List<sofab.InlineString> strs(List<String> v) =>
    [for (final s in v) sofab.InlineString.of(s)];

StructMid mid() => StructMid()
  ..leaves = [
    StructLeaf()..n = 1,
    StructLeaf()
      ..tags = strs(['a', 'bc'])
      ..n = 2,
  ]
  ..cube = [
    [
      [b([1])],
      [b([2, 3]), b([4])],
    ],
    [
      [b([5])],
    ],
  ];

Deep deep() {
  final m = Deep()
    ..mid = mid()
    ..rows = [
      [mid()],
      [mid(), mid()],
    ];
  m.u.m = mid();
  return m;
}

Loose loose() => Loose()
  ..rows = [
    strs(['a']),
    strs(['', 'bc']),
  ];

void fail(String why) {
  stderr.writeln(why);
  exit(1);
}

bool same(Uint8List a, Uint8List b) => sofab.elementsEqual(a, b);

// encodeTo through a caller-built streaming encoder at an explicit depth.
Uint8List viaSink(void Function(sofab.Encoder) enc, int depth) {
  final out = BytesBuilder(copy: true);
  enc(sofab.Encoder(out.add, buffer: Uint8List(64), depth: depth));
  return out.toBytes();
}

// The same value one level tighter than the generated bound must be refused
// with invalidArgument: the bound is the exact depth serialize reaches.
void mustRefuse(String what, void Function(sofab.Encoder) ser, int depth) {
  try {
    ser(sofab.Encoder.overBuffer(Uint8List(4096), depth: depth));
  } on sofab.SofabException catch (e) {
    if (e.code != sofab.SofabError.invalidArgument) {
      fail('$what at depth $depth: want invalidArgument, got $e');
    }
    return;
  }
  fail('$what at depth $depth: encoded, but must be refused');
}

void main() {
  if (Deep.maxDepth != 6) fail('Deep.maxDepth = ${Deep.maxDepth}, want 6');
  if (Loose.maxDepth != 2) fail('Loose.maxDepth = ${Loose.maxDepth}, want 2');

  final enc = deep().encode();
  final sink = viaSink(deep().encodeTo, Deep.maxDepth);
  if (!same(enc, sink)) fail('Deep: encode and encodeTo disagree');
  final again = Deep.decode(enc).encode();
  if (!same(enc, again)) fail('Deep: re-encode differs');
  mustRefuse('Deep', deep().serialize, Deep.maxDepth - 1);

  final lenc = loose().encode();
  final lsink = viaSink(loose().encodeTo, Loose.maxDepth);
  if (!same(lenc, lsink)) fail('Loose: encode and encodeTo disagree');
  final lagain = Loose.decode(lenc).encode();
  if (!same(lenc, lagain)) fail('Loose: re-encode differs');
  mustRefuse('Loose', loose().serialize, Loose.maxDepth - 1);

  print('depth round trip: PASS');
}
`
