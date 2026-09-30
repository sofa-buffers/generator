package dart

import (
	"strings"
	"testing"
)

// unionSrc has a union whose default option is a struct at a non-zero default
// and NOT its first option, one of every option kind the encode rule treats
// differently (scalar, string, compact array, wrapper array, blob, union, fp32,
// boolean array), a string option too large to size eagerly, an option named
// after the union's own tag, a union whose default option is a scalar, an array
// of unions whose default option is a struct, a $defs union used with two
// default_ids (one type per default_id) and a union of 64-bit options.
const unionSrc = `version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  M:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:    { id: 5, type: blob, maxlen: 4 }
          inner: { id: 6, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
          f:     { id: 7, type: fp32, default: 1.5 }
          flags: { id: 8, type: array, items: { type: boolean, count: 2 } }
          big:   { id: 9, type: string, maxlen: 5000 }
          which: { id: 10, type: u64 }
      z: { id: 1, type: union, default_id: 0, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8, default: 3 } } }
      v: { id: 2, type: array, items: { type: union, count: 3, default_id: 1, oneof: { a: { id: 0, type: u8 }, p: { id: 1, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } } } } }
      pf: { id: 4, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      pe: { id: 5, type: array, items: { type: union, count: 3, oneof: { $ref: "#/$defs/union/Pick" } } }
      q: { id: 6, type: union, default_id: 1, oneof: { big: { id: 0, type: u64 }, sig: { id: 1, type: i64 } } }
`

func genUnion(t *testing.T) (lib, harness string) {
	t.Helper()
	files := dartFiles(t, unionSrc, map[string]any{"emit": "project"})
	return files["lib/message.dart"], files["bin/harness.dart"]
}

// classBody returns the text of `class <name> {` up to the next top-level
// declaration.
func classBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "\nclass "+name+" {")
	if i < 0 {
		t.Fatalf("no class %s in:\n%s", name, src)
	}
	rest := src[i+1:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+3]
	}
	return rest
}

func mustContain(t *testing.T, what, src string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(src, w) {
			t.Errorf("%s: missing %q in:\n%s", what, w, src)
		}
	}
}

func mustNotContain(t *testing.T, what, src string, bads ...string) {
	t.Helper()
	for _, b := range bads {
		if strings.Contains(src, b) {
			t.Errorf("%s: must not contain %q in:\n%s", what, b, src)
		}
	}
}

// A union is a tag plus one private typed slot per option: the scalars hold
// their default literal, every reference kind is nullable, and ONLY default_id's
// is constructed -- a fresh union allocates nothing for an option it does not
// hold. The tag starts at default_id, which is NOT the first option here.
func TestDartUnionStorage(t *testing.T) {
	lib, _ := genUnion(t)
	u := classBody(t, lib, "M_U")
	mustContain(t, "M_U storage", u,
		"  static const int numId = 0;",
		"  static const int ptId = 2;",
		"  static const int whichId = 10;",
		"  int _which = ptId;",
		"  int _num_ = 5;",
		"  sofab.InlineString? _s;",
		"  M_U_Pt? _pt = M_U_Pt();",
		"  sofab.InlineInt64Array? _arr;",
		"  List<sofab.InlineString>? _strs;",
		"  sofab.InlineBytes? _bl;",
		"  M_U_Inner? _inner;",
		"  double _f = 1.5;",
		"  int? _fFp32Bits;",
		// The option named like the tag gets a slot of its own: "_" + its mangled
		// getter, never the tag.
		"  int _which_ = 0;",
		"  int get which => _which;",
	)
	// Only default_id's reference slot is constructed.
	for _, bad := range []string{"_s = sofab.InlineString(8);\n  ", "M_U_Inner? _inner = ", "_arr = sofab.InlineInt64Array(4", "_strs = <"} {
		if strings.Contains(u[:strings.Index(u, "int get which")], bad) {
			t.Errorf("M_U constructs an option it does not hold (%q)", bad)
		}
	}
	// No product type any more: no member holds every option side by side.
	mustNotContain(t, "M_U storage", u, "final sofab.InlineString s ", "M_U_Pt pt = M_U_Pt();")
	// default_id is per TYPE: the scalar-default union starts at a.
	mustContain(t, "M_Z storage", classBody(t, lib, "M_Z"), "  int _which = aId;")
}

// The accessors: the getter answers the option's default while another option is
// held (a fresh object for a reference kind, capacity 0 for a destination) and
// stores nothing; a setter selects and stores; a destination option has no
// setter (it is filled in place, exactly as a struct's `final` destination);
// mutable<Opt>() is select-if-not-held -- it creates the storage on first
// selection, resets it in place only when ANOTHER option is held, and never
// touches a held one.
func TestDartUnionAccessors(t *testing.T) {
	lib, _ := genUnion(t)
	u := classBody(t, lib, "M_U")
	mustContain(t, "M_U accessors", u,
		"  int get num_ => _which == numId ? _num_ : 5;",
		"  set num_(int v) {\n    _which = numId;\n    _num_ = v;\n  }",
		"  bool get hasNum => _which == numId;",
		"  M_U_Pt get pt => _which == ptId ? _pt! : M_U_Pt();",
		"  set pt(M_U_Pt v) {\n    _which = ptId;\n    _pt = v;\n  }",
		"  sofab.InlineString get s => _which == sId ? _s! : sofab.InlineString(0);",
		"  sofab.InlineInt64Array get arr => _which == arrId ? _arr! : sofab.InlineInt64Array(0, range: const sofab.ElemRange(0, 65535));",
		// select-if-not-held, one per kind edited in place
		"  M_U_Pt mutablePt() {\n    var s = _pt;\n    if (s == null) {\n      s = M_U_Pt();\n      _pt = s;\n    } else if (_which != ptId) {\n      s.reset();\n    }\n    _which = ptId;\n    return s;\n  }",
		"  sofab.InlineString mutableS() {\n    var s = _s;\n    if (s == null) {\n      s = sofab.InlineString(8);\n      _s = s;\n    } else if (_which != sId) {\n      s.length = 0;\n    }\n    _which = sId;\n    return s;\n  }",
		// The destination is created WITH the declared element width the codec
		// checks every decoded element against.
		"      s = sofab.InlineInt64Array(4, range: const sofab.ElemRange(0, 65535));",
		// A bool array option is a boolean destination, held and detached alike:
		// the codec stores any non-zero element as 1 (CORELIB_PLAN §4.4).
		"      s = sofab.InlineInt64Array(2, range: sofab.ElemRange.boolean);",
		"  sofab.InlineInt64Array get flags => _which == flagsId ? _flags! : sofab.InlineInt64Array(0, range: sofab.ElemRange.boolean);",
		"    } else if (_which != strsId) {\n      s.clear();\n    }",
		// A destination too large to size eagerly starts empty; the header sizes it.
		"      s = sofab.InlineString(0);\n      _big = s;",
		// fp32: a new value drops the NaN bits of the old one; the bits are public.
		"  set f(double v) {\n    _which = fId;\n    _f = v;\n    _fFp32Bits = null;\n  }",
		"  int? get fFp32Bits => _which == fId ? _fFp32Bits : null;",
		// An option named like a union member takes the trailing underscore.
		"  int get which_ => _which == whichId ? _which_ : 0;",
		// reset(): default_id at its default, in place.
		"  void reset() {\n    _which = ptId;\n    final s = _pt;\n    if (s == null) {\n      _pt = M_U_Pt();\n    } else {\n      s.reset();\n    }\n  }",
	)
	// A destination option has no setter: its storage (and the element width it
	// carries) is only ever the one mutable<Opt>() created.
	mustNotContain(t, "M_U accessors", u,
		"set s(", "set arr(", "set bl(", "set flags(", "set big(",
		// never an unconditional reset of a held option
		"    s.reset();\n    _which = ptId;",
	)
	// A scalar option has no mutable accessor.
	mustNotContain(t, "M_U accessors", u, "mutableNum(", "mutableF(")
}

// Encode: one `switch` arm per option. default_id is written like an ordinary
// field of its kind (guarded, a struct closed with the dropping closer); every
// other option is FORCED -- unguarded, a destination as its (possibly empty)
// payload, a struct/union/wrapper-array option closed with the keeping closer --
// because its presence IS the selection (MESSAGE_SPEC §4.2).
func TestDartUnionEncodeArms(t *testing.T) {
	lib, _ := genUnion(t)
	u := classBody(t, lib, "M_U")
	mustContain(t, "M_U.serialize", u,
		"    switch (_which) {",
		"      case numId:\n        e.writeUnsigned(0, _num_);",
		"      case sId:\n        final v = _s!;\n        e.writeStringUtf8(1, v.storage, v.length);",
		"      case ptId:\n        final v = _pt!;\n        e.beginSequenceLazy(2); v.serialize(e); e.endSequence();",
		"      case arrId:\n        final v = _arr!;\n        e.writeUnsignedArray(3, v.storage, v.length);",
		"      case blId:\n        final v = _bl!;\n        e.writeBlob(5, v.storage, v.length);",
		"      case innerId:\n        final v = _inner!;\n        e.beginSequenceLazy(6); v.serialize(e); e.endSequenceKeep();",
		"      case fId:\n        if (_f.isNaN && _fFp32Bits != null) { e.writeFp32Bits(7, _fFp32Bits!); } else { e.writeFp32(7, _f); }",
		"      case flagsId:\n        final v = _flags!;\n        e.writeUnsignedArray(8, _bools01(v), v.length);",
		"      case whichId:\n        e.writeUnsigned(10, _which_);",
	)
	// The wrapper-array option: framed, interior elements sparse, kept.
	strs := u[strings.Index(u, "      case strsId:"):]
	strs = strs[:strings.Index(strs, "      case blId:")]
	mustContain(t, "M_U strs arm", strs, "e.beginSequenceLazy(4);", "        e.endSequenceKeep();")
	mustNotContain(t, "M_U strs arm", strs, "e.endSequence();", "if (v.length")
	// No guard anywhere but default_id's arm (which is a struct here, so none).
	mustNotContain(t, "M_U.serialize", u, "if (_num_ != 5)", "if (v.length != 0) { e.writeStringUtf8(1", "if (_f != 1.5)")
	// A scalar default_id keeps its guard.
	mustContain(t, "M_Z.serialize", classBody(t, lib, "M_Z"),
		"      case aId:\n        if (_a != 0) { e.writeUnsigned(0, _a); }",
		"      case bId:\n        e.writeUnsigned(1, _b);",
	)
	// The union FIELD keeps its lazy frame and dropping closer: a union holding
	// default_id at its default writes nothing, so the frame vanishes.
	mustContain(t, "M.serialize", lib, "e.beginSequenceLazy(0); u.serialize(e); e.endSequence();")
}

// Decode: every store into a union goes through the option's own API, which is
// the §7.4.1 switch, and only in the hook for the option's DECLARED wire kind
// (the §7.3 gate is structural in corelib-dart). A scalar selects through its
// setter after the width guard; a destination at its one header call, after the
// bound and before the hand-over, through mutable<Opt>(); a struct/union option
// descends into mutable<Opt>()'s object; a wrapper-array option clears what
// mutable<Opt>() returns (§7.4 replace).
func TestDartUnionDecodeSwitch(t *testing.T) {
	lib, _ := genUnion(t)
	v := classBody(t, lib, "_M_U__Visitor extends sofab.MessageVisitor")
	mustContain(t, "_M_U__Visitor", v,
		"      case 0:\n        if (value < 0 || value > 65535) { invalidate(); return; }\n        o.num_ = value;\n        return;",
		"      case 7:\n        o.f = value;\n        return;",
		"      case 7:\n        o.f = _f32FromBits(bits);\n        o.fFp32Bits = bits;\n        return;",
		"      case 1:\n        if (length > 8) invalidate();\n        return o.mutableS();",
		"      case 9:\n        if (length > 5000) invalidate();\n        final d = o.mutableBig();\n        if (d.capacity < length) d.storage = Uint8List(length);\n        return d;",
		"      case 3:\n        if (count > 4) invalidate();\n        return o.mutableArr();",
		"      case 2:\n        return _M_U_Pt__Visitor(o.mutablePt());",
		"      case 6:\n        return _M_U_Inner__Visitor(o.mutableInner());",
		"      case 4:\n        final l = o.mutableStrs();\n        l.clear();\n        return sofab.StringSeq(l, 3, 4,",
	)
	// Never a store around the API, never a switch before the bound.
	mustNotContain(t, "_M_U__Visitor", v, "o._", "return o.mutableS();\n        if (length")
	// The string arm lives in onString only: a blob or anything else at id 1 lands
	// in a different call with no arm for it and switches nothing (D20-D22).
	blob := v[strings.Index(v, "onBlob("):]
	blob = blob[:strings.Index(blob, "\n  }\n")]
	mustNotContain(t, "_M_U__Visitor.onBlob", blob, "case 1:", "mutableS(")
}

// An array of unions fills its gaps through the corelib's MessageSeq with the
// union's own constructor -- per TYPE, so each $defs split fills with its own
// default_id. No gap-fill code is emitted.
func TestDartUnionGapFillPerType(t *testing.T) {
	lib, _ := genUnion(t)
	mustContain(t, "gap fill", lib,
		"sofab.MessageSeq<M_V>(o.v, 3, () => M_V(), (x) => _M_V__Visitor(x), rcap: 16384)",
		"sofab.MessageSeq<Pick__DefaultN>(o.pe, 3, () => Pick__DefaultN(), (x) => _Pick__DefaultN__Visitor(x), rcap: 16384)",
		"  Pick__DefaultT pf = Pick__DefaultT();",
	)
	mustContain(t, "M_V", classBody(t, lib, "M_V"), "  int _which = pId;", "  M_V_P? _p = M_V_P();")
	mustContain(t, "Pick_default_n", classBody(t, lib, "Pick__DefaultN"), "  int _which = nId;")
	mustContain(t, "Pick_default_t", classBody(t, lib, "Pick__DefaultT"), "  int _which = tId;")
}

// The harness renders a union as exactly ONE member, the held option, and reads
// one by selecting through the option's setter or mutable accessor, in the
// order the members arrive, so the last one wins. The library stays JSON-free.
func TestDartUnionJSONHarness(t *testing.T) {
	lib, h := genUnion(t)
	mustContain(t, "harness", h,
		"Map<String, dynamic> _M_U__ToJson(M_U m) {\n  switch (m.which) {",
		"    case M_U.numId:\n      return <String, dynamic>{'num': m.num_};",
		"    case M_U.sId:\n      return <String, dynamic>{'s': m.s.toString()};",
		"    case M_U.ptId:\n      return <String, dynamic>{'pt': _M_U_Pt__ToJson(m.pt)};",
		"  throw StateError('M_U holds no option');",
		"  for (final kv in j.entries) {\n    switch (kv.key) {",
		"      case 's':\n        m.mutableS().assignString(kv.value as String);",
		"      case 'pt':\n        m.pt = _M_U_Pt__FromJson(kv.value as Map<String, dynamic>);",
		"      case 'arr':\n        m.mutableArr().assign(",
		"      case 'strs':\n        m.strs = <sofab.InlineString>[",
	)
	mustNotContain(t, "harness", h, "'num': m.num_,\n")
	mustNotContain(t, "library", lib, "jsonEncode", "__ToJson")
}

// The harness reads a 64-bit SIGNED scalar in either JSON spelling -- a bare
// number jsonDecode hands back exactly, and the decimal string that carries any
// i64 whole -- exactly as it reads a u64: check_union.py drives this harness
// with --int64-json string, and a `(x as num).toInt()` reader threw on the
// quoted i64 there.
func TestDartI64JSONReadsAString(t *testing.T) {
	_, h := genUnion(t)
	mustContain(t, "harness", h,
		"      case 'sig':\n        m.sig = (kv.value is String ? BigInt.parse(kv.value as String) : BigInt.from(_exact64(kv.value))).toSigned(64).toInt();",
		"      case 'big':\n        m.big = (kv.value is String ? BigInt.parse(kv.value as String) : BigInt.from(_exact64(kv.value))).toSigned(64).toInt();",
	)
	mustNotContain(t, "harness", h, "m.sig = (kv.value as num).toInt();")
	// A plain struct member reads the same way.
	got := genFor(t, writeDef(t, "version: 1\nmessages:\n  M:\n    payload:\n      a: { id: 0, type: i64 }\n      b: { id: 1, type: i32 }\n"), map[string]any{"emit": "project"})
	mustContain(t, "struct i64", got,
		"m.a = (j['a'] is String ? BigInt.parse(j['a'] as String) : BigInt.from(_exact64(j['a']))).toSigned(64).toInt();",
		"m.b = (j['b'] as num).toInt();",
	)
}

// Two options whose derived members would meet are not an error: the option
// keeps its own name and the derived member takes the trailing `_` -- `a`'s
// id constant beside an option `aId`, `x`'s has<X> beside an option `hasX`.
// The union's own members and Object's are escaped the same way.
func TestDartUnionNamesEscaped(t *testing.T) {
	for _, tc := range []struct {
		oneof string
		want  []string
	}{
		{"{ a: { id: 0, type: u8 }, aId: { id: 1, type: u8 } }", []string{"  static const int aId_ = 0;", "  static const int aIdId = 1;", "  int get aId => "}},
		{"{ x: { id: 0, type: u8 }, hasX: { id: 1, type: u8 } }", []string{"  bool get hasX_ => _which == xId;", "  int get hasX => _which == hasXId"}},
		{"{ reset: { id: 0, type: u8 }, toString: { id: 1, type: u8 } }", []string{"  int get reset_ => ", "  int get toString_ => "}},
	} {
		lib := dartFiles(t, "version: 1\nmessages:\n  M:\n    payload:\n      u: { id: 0, type: union, oneof: "+tc.oneof+" }\n", map[string]any{})["message.dart"]
		mustContain(t, tc.oneof, lib, tc.want...)
	}
}

// TestDartUnionRuntime runs the generated API against a real corelib-dart: the
// fresh value, a getter of an option not held, select-if-not-held (a held
// option is continued, a re-selected one restarts at its default in place, on
// the very object it had), the forced write of a non-default option at its own
// default, reset(), and a decode that switches. Gated on SOFAB_DART_CORELIB.
func TestDartUnionRuntime(t *testing.T) {
	runDartDriver(t, unionSrc, dartUnionDriver, "union runtime: PASS")
}

const dartUnionDriver = `import 'dart:io';
import 'dart:typed_data';
import 'package:rt/message.dart';

void check(bool ok, String what) {
  if (!ok) {
    stderr.writeln('FAIL: $what');
    exit(1);
  }
}

String hex(Uint8List b) =>
    b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();

void main() {
  final m = M();
  // Fresh: default_id (pt) at its own default; nothing written.
  check(m.u.which == M_U.ptId && m.u.hasPt && m.u.pt.x == 7, 'fresh union holds pt at its default');
  check(m.encode().isEmpty, 'a fresh message encodes to zero bytes');

  // A getter of an option not held answers its default and stores nothing.
  check(m.u.num_ == 5 && m.u.which == M_U.ptId, 'getter of a non-held option');
  m.u.s.assignString('lost');
  check(m.u.which == M_U.ptId && !m.u.hasS, 'writing into a detached default selects nothing');

  // mutable: select at default; a held option is continued, not reset.
  m.u.mutablePt().x = 1;
  final pt = m.u.pt;
  m.u.mutableS().assignString('ab');
  check(m.u.which == M_U.sId && m.u.s.toString() == 'ab', 'mutableS selects s');
  final dest = m.u.mutableS();
  check(dest.toString() == 'ab', 'mutableS of a held option keeps its value');

  // Away and back: pt restarts at ITS default, on the very object it had.
  m.u.mutablePt().y = 3;
  check(identical(m.u.pt, pt) && m.u.pt.x == 7 && m.u.pt.y == 3, 're-selected struct option restarts at its default in place');
  m.u.mutableS();
  check(identical(m.u.s, dest) && m.u.s.length == 0, 're-selected string option restarts empty on its own storage');

  // A non-default option at its own default is still written (forced).
  m.u.num_ = 5;
  check(hex(m.encode()) == '06000507', 'num at its default is written: ' + hex(m.encode()));
  m.u.mutableBl();
  check(hex(m.encode()) == '062a0307', 'an empty blob option is written: ' + hex(m.encode()));

  // reset(): default_id at its default, storage kept.
  m.u.reset();
  check(m.u.which == M_U.ptId && m.u.pt.x == 7 && m.u.pt.y == 0 && identical(m.u.pt, pt), 'reset restores pt in place');
  check(m.encode().isEmpty, 'a reset message encodes to zero bytes');

  // Decode: the last option wins; the other one's state does not survive.
  final w = Uint8List.fromList([0x06, 0x00, 0x05, 0x0a, 0x0a, 0x78, 0x07]);
  final d = M.decode(w);
  check(d.u.which == M_U.sId && d.u.s.toString() == 'x', 'decode: last option wins');
  final r = M();
  r.u.mutablePt().x = 9;
  check(M.tryDecode(w, r).name == 'complete' && r.u.hasS && r.u.pt.x == 7, 'decode into a reused message');

  print('union runtime: PASS');
}
`
