package dart

import (
	"strings"
	"testing"
)

// inlineSchema carries the destination shapes whose behaviour the generated
// code decides rather than the corelib: a bool array (stored as the 64-bit
// elements the wire carries), an fp32 array whose default is not representable
// in fp32, an array too large to size at construction, unbounded
// strings/arrays, a string default, and a bool matrix.
const inlineSchema = `
version: 1
messages:
  Edge:
    payload:
      flags: { id: 0, type: array, items: { type: boolean, count: 4 }, default: [true, false] }
      f32d:  { id: 1, type: array, items: { type: fp32, count: 4 }, default: [0.1, 1.7] }
      big:   { id: 2, type: array, items: { type: u16, count: 2000 } }
      dyn:   { id: 3, type: array, items: { type: fp64 } }
      name:  { id: 4, type: string, maxlen: 8, default: "héj" }
      note:  { id: 5, type: string }
      brows: { id: 6, type: array, items: { type: array, count: 3, items: { type: boolean, count: 2 } } }
`

// TestDartInlineDestinationShapes pins the generated half of those shapes.
func TestDartInlineDestinationShapes(t *testing.T) {
	out := genFor(t, writeDef(t, inlineSchema), map[string]any{})
	for _, want := range []string{
		// A bool array is compared as booleans and written as canonical 0/1.
		"if (!_boolsEq(flags.storage, flags.length, const <int>[1, 0])) { e.writeUnsignedArray(0, _bools01(flags), flags.length); }",
		"if (_e0.length != 0 || _i0 == brows.length - 1) e.writeUnsignedArray(_i0, _bools01(_e0), _e0.length);",
		// An fp32 default is emitted already rounded to fp32, so the stored
		// elements compare equal to it.
		"sofab.InlineFloat32Array(4)..assign(const <double>[0.10000000149011612, 1.7000000476837158])",
		// A string default is its UTF-8 bytes.
		"final sofab.InlineString name = sofab.InlineString(8)..assign(const <int>[104, 195, 169, 106]);",
		// 2000 u16 elements are 16 KB of Int64List: sized at the header instead.
		"final sofab.InlineInt64Array big = sofab.InlineInt64Array(0, range: const sofab.ElemRange(0, 65535));",
		"if (o.big.capacity < count) o.big.storage = Int64List(count);",
		"if (o.dyn.capacity < count) o.dyn.storage = Float64List(count);",
		"if (o.note.capacity < length) o.note.storage = Uint8List(length);",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated Dart missing %q:\n%s", want, out)
		}
	}
}

// TestDartInlineDestinationsRoundTrip runs those shapes against the real corelib:
// the all-default object encodes to nothing, a decoded bool 5 re-encodes as 1
// and a [5, 0] equals the default [true, false], the lazily sized and unbounded
// destinations round-trip, a reused object keeps its storage and loses every
// stale field, and an over-count on a lazily sized array is still INVALID at the
// header. Gated on SOFAB_DART_CORELIB and the dart toolchain.
func TestDartInlineDestinationsRoundTrip(t *testing.T) {
	runDartDriver(t, inlineSchema, dartInlineDriver, "inline destinations: PASS")
}

const dartInlineDriver = `import 'dart:io';
import 'dart:typed_data';
import 'package:sofa_buffers_corelib/sofa_buffers_corelib.dart' as sofab;
import 'package:rt/message.dart';

void fail(String why) {
  stderr.writeln('FAIL: $why');
  exit(1);
}

String hex(Uint8List b) =>
    b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();

Uint8List wire(void Function(sofab.Encoder) f) =>
    sofab.Encoder.encodeToBytes(f);

void main() {
  if (Edge().encode().isNotEmpty) fail('all-default Edge encoded ${hex(Edge().encode())}');

  final m = Edge.decode(wire((e) => e.writeUnsignedArray(0, [5, 0, 7])));
  if (m.flags.length != 3 || m.flags[0] == 0 || m.flags[2] == 0) {
    fail('bool array not decoded: ${m.flags.toList()}');
  }
  final canon = wire((e) => e.writeUnsignedArray(0, [1, 0, 1]));
  if (hex(m.encode()) != hex(canon)) fail('bool re-encode ${hex(m.encode())}, want ${hex(canon)}');
  if (Edge.decode(wire((e) => e.writeUnsignedArray(0, [5, 0]))).encode().isNotEmpty) {
    fail('[5, 0] equals the default [true, false] and must be omitted');
  }

  final full = Edge();
  full.big.assign(List<int>.generate(1500, (i) => i));
  full.dyn.assign([1.5, -2.25]);
  full.note.assignString('grüße');
  full.brows = [
    sofab.InlineInt64Array.of([1, 0]),
    sofab.InlineInt64Array.of([]),
    sofab.InlineInt64Array.of([9]),
  ];
  final bytes = full.encode();
  final back = Edge();
  if (Edge.tryDecode(bytes, back) != sofab.DecodeStatus.complete) fail('decode full');
  if (hex(back.encode()) != hex(bytes)) fail('full round trip differs');
  if (back.big.length != 1500 || back.big[1499] != 1499) fail('big array');
  if (back.note.toString() != 'grüße') fail('note = ${back.note}');
  final bigStorage = back.big.storage;

  final small = Edge()..big.assign([7, 8]);
  if (Edge.tryDecode(small.encode(), back) != sofab.DecodeStatus.complete) fail('decode small');
  if (back.big.length != 2 || back.big[1] != 8) fail('reused big: ${back.big.toList()}');
  if (!identical(back.big.storage, bigStorage)) fail('reuse reallocated the big storage');
  if (back.note.length != 0 || back.dyn.length != 0 || back.brows.isNotEmpty) fail('reset left a stale field');
  if (back.name.toString() != 'héj') fail('string default not restored: ${back.name}');

  final over = wire((e) => e.writeUnsignedArray(2, List<int>.filled(2001, 1)));
  if (Edge.tryDecode(over, Edge()) != sofab.DecodeStatus.invalid) fail('over-count not INVALID');

  print('inline destinations: PASS');
}
`
