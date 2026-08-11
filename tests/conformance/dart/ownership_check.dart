// A decoded message OWNS its bytes (CORELIB_PLAN §5.1's ownership rule applied
// to the decode side): no destination may keep a window into the buffer the
// bytes came from, or the message's lifetime would be silently tied to it.
//
// corelib-dart hands the visitor a VIEW for a string/blob on the one-shot path
// (`Uint8List.view(_buf.buffer, ...)`), so this property lives entirely in the
// generated destinations, which copy. Nothing here asserts what a destination
// looks like: it overwrites the input after decoding and re-encodes, which fails
// for any field that turned out to be a view.
//
// KNOWN REACH -- do not read a pass as covering every field. The corelib
// allocates the container itself for an integer array (`Int64List(count)`) and
// an fp array (`Float32List`/`Float64List(count)`) on BOTH paths, and
// reassembles a split string/blob into its own `Uint8List(length)` while
// streaming. Only a one-shot string/blob is a view, so only those legs can fail
// here: dropping the copy in an array destination passes this check (verified by
// mutating the generated code). The array copies are still required -- which side
// allocates is the corelib's choice to change -- they are just pinned by
// inspection rather than by this test.
//
// Run inside a generated project (`dart run bin/ownership_check.dart`); exits
// non-zero with a diff on failure.
import 'dart:typed_data';

import 'package:harness/message.dart';

Myfirstmessage _sample() => Myfirstmessage()
  ..somestring = 'héllo wörld'
  ..someblob = Uint8List.fromList([1, 2, 3, 4, 5])
  ..someuintarray = [9, 8, 7, 6]
  ..somefloatarray = [1.5, -2.5, 3.5]
  ..somestringarray = ['a', 'bb', 'ccc']
  ..someblobarray = [
    Uint8List.fromList([9, 9]),
    Uint8List.fromList([8]),
  ];

String _hex(Uint8List b) => b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();

void _mustMatch(String what, Uint8List want, Uint8List got) {
  if (_hex(want) != _hex(got)) {
    throw StateError('$what: a decoded field aliased its input buffer\n'
        '  want ${_hex(want)}\n  got  ${_hex(got)}');
  }
}

void main() {
  final want = _sample().encode();

  // One-shot: a MUTABLE input, so the scribble below is possible at all. decode()
  // must have detached from it by the time it returns.
  final wire = Uint8List.fromList(want);
  final got = Myfirstmessage.decode(wire);
  wire.fillRange(0, wire.length, 0xff);
  _mustMatch('one-shot decode', want, got.encode());

  // Streaming: the hazard is sharper here, because the chunk is the caller's and
  // is typically refilled. So every chunk is fed out of ONE reusable scratch
  // buffer that is overwritten immediately afterwards.
  const chunkLen = 7;
  final scratch = Uint8List(chunkLen);
  final out = Myfirstmessage();
  final dec = Myfirstmessage.decoder(out);
  for (var i = 0; i < want.length; i += chunkLen) {
    final n = (want.length - i) < chunkLen ? want.length - i : chunkLen;
    scratch.setRange(0, n, want, i);
    dec.feed(Uint8List.sublistView(scratch, 0, n));
    scratch.fillRange(0, chunkLen, 0xff);
  }
  final fin = dec.finish();
  if (fin == null) {
    throw StateError('streaming decode: ${dec.status.name}, expected complete');
  }
  _mustMatch('streaming decode', want, fin.encode());

  print('decoded message owns its bytes');
}
