// A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412):
// no destination may keep a window into the buffer the bytes came from, or the
// message's lifetime would be silently tied to it. §6.0 fixes that for `feed` --
// a chunk is borrowed only for the duration of the call -- and §6.7.1 extends it
// to the one-shot path, which gets no exemption.
//
// Nothing here asserts what a destination looks like: the oracle is destructive,
// not comparative. It decodes, then OVERWRITES the storage the bytes came from,
// then re-encodes and diffs. Comparing two decoders against each other cannot
// see this -- both would read the same live buffer.
//
// REACH. corelib-dart decodes into caller-supplied destinations (§6.6.3,
// corelib-dart#96): at each string, blob and native-array header the generated
// visitor hands over the object's own `Inline…` field, and the codec copies the
// payload into its storage. So every payload kind this message carries -- the
// strings and arrays included, not only the blobs -- lands in storage the
// decoded object owns, and every one of them is covered by the scribble below.
// The copy is the codec's (the destination is only ever written through its
// storage); a pass therefore says the two layers together own the bytes, which
// is the property, not either layer.
//
// CHUNK SIZE IS THE AXIS, not the entry point. A payload SPLIT across chunks is
// reassembled into the corelib's own accumulator and copied out of it whether or
// not the destination wanted a view, so a small-chunk-only feed is structurally
// unable to fail. The sweep therefore ends at a size that carries the whole
// message, where every payload arrives in one piece and the aliasing branch is
// reachable at all.
//
// The scribble byte is 0x41 ('A'), deliberately: with 0xff an aliased string
// makes the re-encode fail validation instead, and the oracle would become an
// error rather than a byte comparison. A throwing re-encode is still reported as
// a failure here -- it is caught, never propagated out of the comparison.
//
// Run inside a generated project (`dart run bin/ownership_check.dart`); exits
// non-zero with a diff on failure.
import 'dart:io';
import 'dart:typed_data';

import 'package:harness/message.dart';
import 'package:sofa_buffers_corelib/sofa_buffers_corelib.dart' as sofab;

/// Every chunk size but the last; `main` appends one at least as long as the
/// whole message, because only a chunk that carries a payload whole reaches the
/// corelib's no-copy branch.
const List<int> _chunkSizes = <int>[1, 7, 16, 32, 64];

/// The scribble byte: printable ASCII, so an aliased string still re-encodes and
/// the oracle stays a byte diff rather than turning into an encoding error.
const int _scribble = 0x41;

var _failures = 0;

void _fail(String msg) {
  stderr.writeln('FAIL: $msg');
  _failures++;
}

/// A message filling every aliasing-capable field kind: string, blob,
/// array<string>, array<blob>, a string nested in a struct, a string in a union,
/// a string in a wrapper-array row, a struct-with-array's label, and the string
/// key of a dynamic map row -- plus the native arrays, which are here so the
/// wire carries them, not because they can alias.
Myfirstmessage _sample() {
  final m = Myfirstmessage()
    ..someuintarray.assign([9, 8, 7, 6])
    ..somefloatarray.assign([1.5, -2.5, 3.5])
    ..somestringarray = [
      for (final s in ['a', 'bb', 'ccc']) sofab.InlineString.of(s),
    ]
    ..someblobarray = [
      sofab.InlineBytes.of([9, 9]),
      sofab.InlineBytes.of([8]),
    ]
    ..someunionarray = [
      MyfirstmessageSomeunionarrayElem()..asstring.assignString('row payload'),
    ]
    ..somemap = [
      MyfirstmessageSomemapElem()
        ..key.assignString('first key')
        ..value = 1,
      MyfirstmessageSomemapElem()
        ..key.assignString('second key')
        ..value = 2,
    ];
  m.somestring.assignString('héllo wörld payload');
  m.someblob.assign([1, 2, 3, 4, 5]);
  m.somestruct.nestedstring.assignString('nested payload');
  m.someunion.option2.assignString('union payload');
  m.somestructwitharray.label.assignString('labelled');
  return m;
}

String _hex(Uint8List b) =>
    b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();

String _hexOf(sofab.InlineBytes b) => _hex(b.toBytes());

/// Re-encodes [got] and diffs it against [want]. A re-encode that THROWS is a
/// failure of this check too, not an escaped exception: a scribbled destination
/// can come back as an error rather than as different bytes.
void _mustMatch(String what, Uint8List want, Myfirstmessage got) {
  final Uint8List re;
  try {
    re = got.encode();
  } catch (e) {
    _fail('$what: re-encoding the decoded message threw $e '
        '-- a destination aliased its input');
    return;
  }
  if (_hex(want) != _hex(re)) {
    _fail('$what: a decoded field aliased the buffer it was decoded from\n'
        '  want ${_hex(want)}\n  got  ${_hex(re)}\n'
        '  somestring = ${got.somestring}\n'
        '  someblob   = ${_hexOf(got.someblob)}');
    for (var i = 0; i < got.someblobarray.length; i++) {
      stderr.writeln('  someblobarray[$i] = ${_hexOf(got.someblobarray[i])}');
    }
  }
}

void main() {
  final want = _sample().encode();

  // 1. One-shot, out of a MUTABLE copy. §6.7.1 gives this path no exemption:
  // the buffer may be reused the moment decode() returns.
  final wire = Uint8List.fromList(want);
  final got = Myfirstmessage.decode(wire);
  wire.fillRange(0, wire.length, _scribble);
  _mustMatch('one-shot decode', want, got);

  // 2. Streaming, every chunk out of ONE reusable scratch that is overwritten
  // the instant feed returns (§6.0: the borrow ends there). The last size
  // carries the whole message in one chunk -- see CHUNK SIZE IS THE AXIS above.
  final sizes = <int>[..._chunkSizes, want.length];
  for (final chunkLen in sizes) {
    final scratch = Uint8List(chunkLen);
    final out = Myfirstmessage();
    final dec = Myfirstmessage.decoder(out);
    for (var i = 0; i < want.length; i += chunkLen) {
      final n = (want.length - i) < chunkLen ? want.length - i : chunkLen;
      scratch.setRange(0, n, want, i);
      dec.feed(Uint8List.sublistView(scratch, 0, n));
      scratch.fillRange(0, chunkLen, _scribble);
    }
    final fin = dec.finish();
    if (fin == null) {
      _fail('streaming decode(chunk=$chunkLen): '
          '${dec.feed(const <int>[]).name}, '
          'expected complete');
      continue;
    }
    _mustMatch('streaming decode(chunk=$chunkLen)', want, fin);
  }

  if (_failures > 0) exit(1);
  print('decoded message owns its bytes: '
      'one-shot + ${sizes.length} chunk sizes');
}
