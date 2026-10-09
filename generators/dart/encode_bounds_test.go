package dart

import (
	"strings"
	"testing"
)

// The encode-side schema bounds (ARCHITECTURE §9.6): a value the caller filled
// past what the schema declares is refused by serialize with the encoder's
// InvalidArgument exception, before it is written, at every position a bound
// lives -- a field, a struct member, a union option (forced or not), a wrapper
// array level and its elements, a matrix row.
const encodeBoundsSrc = `
version: 1
messages:
  M:
    payload:
      s: { id: 0, type: string, maxlen: 4 }
      su: { id: 1, type: string }
      u64: { id: 2, type: u64 }
      i64: { id: 3, type: i64 }
      bo: { id: 4, type: boolean }
      fl: { id: 5, type: fp64 }
      pts: { id: 6, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: u8 } } } }
      mat: { id: 7, type: array, items: { type: array, count: 2, items: { type: u16, count: 3 } } }
      nest: { id: 8, type: array, items: { type: array, count: 2, items: { type: string, count: 3, maxlen: 4 } } }
      un: { id: 9, type: union, default_id: 0, oneof: { a: { id: 0, type: u8 }, t: { id: 1, type: string, maxlen: 2 } } }
      ua: { id: 10, type: array, items: { type: u32 } }
      bl: { id: 11, type: array, items: { type: blob, count: 2, maxlen: 3 } }
      au: { id: 12, type: array, items: { type: u32, count: 3 } }
      i16: { id: 13, type: i16 }
      u32: { id: 14, type: u32 }
      en: { id: 15, type: enum, enum: { A: 0, B: 1, C: 2 } }
      bf: { id: 16, type: bitfield, bits: { x: { pos: 0 }, y: { pos: 9 } } }
      bmax: { id: 17, type: blob, maxlen: 2 }
      ea: { id: 18, type: array, items: { type: enum, count: 3, enum: { A: 0, B: 1, Z: 10 } } }
      bfa: { id: 19, type: array, items: { type: bitfield, count: 3, bits: { a: { pos: 0 }, b: { pos: 9 } } } }
      ai8: { id: 20, type: array, items: { type: i8 } }
      a64: { id: 21, type: array, items: { type: u64 } }
      mbo: { id: 22, type: array, items: { type: boolean } }
`

const refuse = "throw const sofab.SofabException(sofab.SofabError.invalidArgument, "

func TestDartEncodeRefusesOverBoundValues(t *testing.T) {
	lib := dartFiles(t, encodeBoundsSrc, map[string]any{})["message.dart"]
	mustContain(t, "encode bounds", lib,
		// string / blob maxlen in UTF-8 bytes: the storage holds the bytes, so its
		// length is compared directly, inside the write branch.
		"if (s.length != 0) { if (s.length > 4) "+refuse+"'s: over maxlen 4'); e.writeStringUtf8(0, ",
		"if (bmax.length != 0) { if (bmax.length > 2) "+refuse+"'bmax: over maxlen 2'); e.writeBlob(17, ",
		// native array count
		"if (au.length != 0) { if (au.length > 3) "+refuse+"'au: over count 3'); e.writeUnsignedArrayInRange(12, au.storage, au.length, 0, 4294967295); }",
		// native array element width: the Int64 storage is wider, so the declared
		// width rides the corelib in-range writer as its two plain bounds
		"e.writeUnsignedArrayInRange(10, ua.storage, ua.length, 0, 4294967295);",
		"e.writeSignedArrayInRange(18, ea.storage, ea.length, -128, 127);",
		"e.writeUnsignedArrayInRange(19, bfa.storage, bfa.length, 0, 65535);",
		"e.writeSignedArrayInRange(20, ai8.storage, ai8.length, -128, 127);",
		"e.writeUnsignedArrayInRange(_i0, _e0.storage, _e0.length, 0, 65535);",
		// scalar widths: the decode side's comparison shapes
		"if (i16 < -32768 || i16 > 32767) "+refuse+"'i16: outside its declared width');",
		"if (u32 < 0 || u32 > 4294967295) "+refuse+"'u32: outside its declared width');",
		"if (en < -128 || en > 127) "+refuse+"'en: outside its declared width');",
		"if ((bf & ~0xffff) != 0) "+refuse+"'bf: outside its declared width');",
		// struct member
		"if (x != 0) { if (x < 0 || x > 255) "+refuse+"'x: outside its declared width'); e.writeUnsigned(0, x); }",
		// wrapper arrays: the level count before the frame opens, every element's
		// bound before it is written, at every nesting level
		"if (pts.length > 2) "+refuse+"'pts: over count 2');\n    e.beginSequenceLazy(6);",
		"if (mat.length > 2) "+refuse+"'mat: over count 2');",
		"if (_e0.length > 3) "+refuse+"'mat: element over count 3');",
		"if (nest.length > 2) "+refuse+"'nest: over count 2');",
		"if (nest[_i0].length > 3) "+refuse+"'nest: over count 3');",
		"if (_e1.length > 4) "+refuse+"'nest: element over maxlen 4');",
		"if (bl.length > 2) "+refuse+"'bl: over count 2');",
		"if (_e0.length > 3) "+refuse+"'bl: element over maxlen 3');",
		// union options: the default one inside its omission test, a forced one
		// unconditionally
		"if (_a != 0) { if (_a < 0 || _a > 255) "+refuse+"'a: outside its declared width'); e.writeUnsigned(0, _a); }",
		"if (v.length > 2) "+refuse+"'t: over maxlen 2'); e.writeStringUtf8(1, v.storage, v.length);",
		// encode() documents it
		"throws\n  /// [sofab.SofabException] (`invalidArgument`)",
	)
	// Nothing where the storage cannot exceed what the schema declares, or the
	// schema declares nothing.
	mustContain(t, "unguarded", lib,
		"if (su.length != 0) { e.writeStringUtf8(1, su.storage, su.length); }",
		"if (u64 != 0) { e.writeUnsigned(2, u64); }",
		"if (i64 != 0) { e.writeSigned(3, i64); }",
		"if (bo != false) { e.writeBool(4, bo); }",
		"if (a64.length != 0) { e.writeUnsignedArray(21, a64.storage, a64.length); }",
		"e.writeUnsignedArray(22, _bools01(mbo), mbo.length);",
	)
	// The guard is one compare per bound: no helper, no copy, no extra pass.
	if n := strings.Count(lib, refuse); n != 20 {
		t.Errorf("want 20 encode refusals, got %d:\n%s", n, lib)
	}
	mustNotContain(t, "bufferFull doc", lib, "(`bufferFull`) rather than being")
}

// The harness reports a refused encode as a plain exit 1, never as an
// unhandled exception the conformance drivers would read as a crash -- and only
// the encode work: a SofabException out of a decode verb stays a crash.
func TestDartHarnessReportsARefusedEncode(t *testing.T) {
	h := dartFiles(t, encodeBoundsSrc, map[string]any{"emit": "project"})["bin/harness.dart"]
	mustContain(t, "harness", h,
		"typed.Uint8List _encode(typed.Uint8List Function() run) {\n  try {\n    return run();\n  } on sofab.SofabException catch (ex) {\n    io.stderr.writeln('encode refused: $ex');\n    io.exit(1);\n  }\n}",
		"      if (mode == 'encode') {\n        final obj = _M__FromJson(",
		"        io.stdout.add(_encode(obj.encode));\n      } else if (mode == 'streamencode') {",
		"          io.stdout.add(_encode(() {\n            obj.encodeTo(",
		"        io.stdout.add(_encode(obj.encode));\n      } else {",
	)
	if n := strings.Count(h, "_encode("); n != 5 {
		t.Errorf("want _encode defined once and used by encode, both streamencode arms and recode (5), got %d", n)
	}
	if n := strings.Count(h, "on sofab.SofabException"); n != 1 {
		t.Errorf("want exactly one SofabException catch (the encode wrapper), got %d", n)
	}
}

// TestDartEncodeBoundsRuntime runs the guards against a real corelib-dart:
// every over-bound position throws invalidArgument, every at-bound value
// encodes and decodes back. Gated on SOFAB_DART_CORELIB.
func TestDartEncodeBoundsRuntime(t *testing.T) {
	runDartDriver(t, encodeBoundsSrc, dartEncodeBoundsDriver, "encode bounds runtime: PASS")
}

const dartEncodeBoundsDriver = `import 'dart:io';
import 'package:rt/message.dart';
import 'package:sofa_buffers_corelib/sofa_buffers_corelib.dart' as sofab;

void refused(String what, void Function(M m) fill) {
  final m = M();
  fill(m);
  try {
    m.encode();
  } on sofab.SofabException catch (e) {
    if (e.code == sofab.SofabError.invalidArgument) return;
    stderr.writeln('FAIL: $what: wrong category ${e.code}');
    exit(1);
  }
  stderr.writeln('FAIL: $what: encoded');
  exit(1);
}

void encodes(String what, void Function(M m) fill) {
  final m = M();
  fill(m);
  final d = M.decode(m.encode());
  if (d.encode().length != m.encode().length) {
    stderr.writeln('FAIL: $what: round trip changed');
    exit(1);
  }
}

sofab.InlineString str(String s) => sofab.InlineString.of(s);

void main() {
  refused('string maxlen', (m) => m.s.assignString('xxxxx'));
  refused('string maxlen in bytes', (m) => m.s.assignString('xxx\u00e9'));
  refused('blob maxlen', (m) => m.bmax.assign([1, 2, 3]));
  refused('array count', (m) => m.au.assign([1, 2, 3, 4]));
  refused('u32 width', (m) => m.u32 = 4294967296);
  refused('u32 negative', (m) => m.u32 = -1);
  refused('i16 over', (m) => m.i16 = 32768);
  refused('i16 under', (m) => m.i16 = -32769);
  refused('enum width', (m) => m.en = 128);
  refused('bitfield width', (m) => m.bf = 0x10000);
  refused('struct member width', (m) => m.pts.add(M_Pts()..x = 256));
  refused('struct array count', (m) => m.pts.addAll([M_Pts(), M_Pts(), M_Pts()]));
  refused('matrix count', (m) => m.mat.addAll([for (var i = 0; i < 3; i++) sofab.InlineInt64Array.of([1])]));
  refused('matrix row count', (m) => m.mat.add(sofab.InlineInt64Array.of([1, 2, 3, 4])));
  refused('nested count', (m) => m.nest.addAll([[], [], []]));
  refused('nested row count', (m) => m.nest.add([str('a'), str('b'), str('c'), str('d')]));
  refused('nested element maxlen', (m) => m.nest.add([str('abcde')]));
  refused('blob array element maxlen', (m) => m.bl.add(sofab.InlineBytes.of([1, 2, 3, 4])));
  refused('union default option width', (m) => m.un.a = 256);
  refused('union forced option maxlen', (m) => m.un.mutableT().assignString('abc'));
  refused('u32 array element width', (m) => m.au.assign([1, 2, 4294967296]));
  refused('u32 array negative element', (m) => m.ua.assign([-1]));
  refused('matrix row element width', (m) => m.mat.add(sofab.InlineInt64Array.of([70000])));
  refused('enum array element width', (m) => m.ea.assign([0, 128]));
  refused('bitfield array element width', (m) => m.bfa.assign([0x10000]));
  refused('i8 array element under', (m) => m.ai8.assign([-129]));

  encodes('at bound', (m) {
    m.s.assignString('\u00e9\u00e9');
    m.bmax.assign([1, 2]);
    m.au.assign([1, 2, 4294967295]);
    m.u32 = 4294967295;
    m.i16 = -32768;
    m.en = 127;
    m.bf = 0xffff;
    m.pts.addAll([M_Pts()..x = 255, M_Pts()]);
    m.mat.addAll([sofab.InlineInt64Array.of([1, 2, 3]), sofab.InlineInt64Array.of([65535])]);
    m.nest.add([str('abcd'), str(''), str('x')]);
    m.bl.addAll([sofab.InlineBytes.of([1, 2, 3])]);
    m.un.mutableT().assignString('ab');
    m.ea.assign([-128, 127, 10]);
    m.bfa.assign([0xffff]);
    m.ai8.assign([-128, 127]);
    m.a64.assign([-1]);
  });
  print('encode bounds runtime: PASS');
}
`
