// tryDecode(bytes, out) into a destination that holds an earlier message
// (generator#760), driven against the project generated from reuse_storage.yaml
// with tight receiver caps (array count 4, string and blob length 8). Typechecks
// in every int64 mode: no 64-bit value is written or read here except through
// JSON.
//
// check_reused_destination.py already proves, through the harness, that the
// destination reads as a fresh decode. What it cannot see is the STORAGE, which
// is the point of this entry point, and two shapes its table never builds:
//
//   reused     the same shapes decoded again land in the very arrays, buffers
//              and element objects the destination already held
//   pairs      every ordered pair of images, the reused-storage shapes included,
//              still reads as a fresh decode and re-encodes byte for byte
//   cut        every prefix of every image, decoded into a destination holding
//              another message, reads as the incremental Decoder's fresh
//              destination after the same prefix -- the field the cut lands in
//              included, which is where storage written over in place would
//              still show the older message's elements
//   refused    a receiver cap hit inside an array (LIMIT_EXCEEDED) leaves what a
//              fresh decode leaves, with none of the held array's tail
//   verdicts   Complete / Incomplete are returned, a refusal is thrown
import { DecodeStatus, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";
import { Store, Store__Decoder } from "./message.js";

let failed = 0;
function check(ok: boolean, what: string): void {
  if (!ok) {
    process.stderr.write(`FAIL: ${what}\n`);
    failed++;
  }
}
const hex = (b: Uint8Array): string => Buffer.from(b).toString("hex");
const json = (m: Store): string => JSON.stringify(m.toJSON());
const enc = (d: Record<string, unknown>): Uint8Array => Store.fromJSON(d).encode();

// Every field away from its default.
const FULL = {
  a: [1, 2, 3], fa: [1.5, 2.5, 3.5], ok: [true, false, true], e: [2, 1, 0], w: [1, 2, 3],
  b: [1, 2, 3, 4], bu: [9, 9, 9], bd: [5, 6, 7], sa: ["x", "yy", "zzz", "w"],
  ba: [[1], [2, 2], [3, 3, 3]], bx: [[4, 4], [5], [6, 6, 6], [7]],
  ra: [{ z: 1, t: [1, 2] }, { z: 2, t: [3] }, { z: 3, t: [] }],
  ua: [{ m: 5 }, { s: "ab" }, { m: -1 }],
  mx: [[1, 2], [3, 4, 5], [6]], mw: [["a", "b"], ["c"]],
  ns: { ta: [1, 2, 3], nb: [1, 2], sl: ["p", "q"] }, un: { xs: [1, 2, 3] }, f64: [1.25, -2.5, 3.75],
};
// FULL's shapes -- every length the same -- with other values: the steady state
// of a receive loop, where every piece of storage fits again.
const SAME = {
  a: [9, 8, 7], fa: [-1.5, 0.5, 9.5], ok: [false, true, true], e: [0, 2, 1], w: [7, 8, 9],
  b: [8, 7, 6, 5], bu: [1, 1, 2], bd: [9, 9, 9], sa: ["q", "rr", "sss", "t"],
  ba: [[9], [8, 8], [7, 7, 7]], bx: [[3, 3], [2], [1, 1, 1], [9]],
  ra: [{ z: 7, t: [5, 6] }, { z: 8, t: [7] }, { z: 9, t: [] }],
  ua: [{ s: "zz" }, { m: 4 }, { m: 3 }],
  mx: [[9, 8], [7, 6, 5], [4]], mw: [["x", "y"], ["z"]],
  ns: { ta: [4, 5, 6], nb: [3, 4], sl: ["r", "s"] }, un: { xs: [4, 5, 6] }, f64: [-1.25, 2.5, -3.75],
};
// Shorter everywhere, with interior gaps (an element at its default is not on
// the wire) and fields absent.
const SHORTER = {
  a: [5], ok: [true], e: [1], b: [1], bu: [], sa: ["", "k"], ba: [[], [4]], bx: [[1]],
  ra: [{ z: 0, t: [] }, { z: 4, t: [2] }], ua: [{ m: 0 }, { s: "q" }], mx: [[], [2]],
  ns: { ta: [7] }, un: { n: 3 },
};
// Longer than FULL wherever the caps leave room.
const LONGER = {
  a: [1, 2, 3, 4, 5, 6, 7, 8], fa: [1, 2, 3, 4], ok: [true, true, true, true], e: [1, 1, 1, 1],
  w: [1, 2, 3, 4], b: [1, 2, 3, 4, 5, 6, 7, 8], bu: [1, 2, 3, 4, 5, 6, 7, 8], bd: [1, 2, 3, 4, 5, 6, 7, 8],
  sa: ["a", "b", "c", "dddddddd"], ba: [[1], [2], [3], [4, 4, 4, 4, 4, 4, 4, 4]],
  bx: [[1, 1, 1, 1, 1, 1, 1, 1], [2], [3], [4]],
  ra: [{ z: 1, t: [1] }, { z: 2, t: [2] }, { z: 3, t: [3] }, { z: 4, t: [1, 2, 3, 4] }],
  ua: [{ m: 1 }, { m: 2 }, { s: "abcd" }], mx: [[1, 2, 3, 4], [5], [6, 7, 8, 9]],
  mw: [["a", "b"], ["c", "d"]], ns: { ta: [1, 2, 3, 4], nb: [1, 2, 3, 4], sl: ["a", "b"] },
  un: { xs: [1, 2, 3, 4] }, f64: [1, 2, 3],
};
const IMAGES: [string, Uint8Array][] = [
  ["full", enc(FULL)], ["same", enc(SAME)], ["shorter", enc(SHORTER)], ["longer", enc(LONGER)],
  ["empty", new Uint8Array(0)],
];
// Every field of SAME and then every field of FULL again: each id occurs twice
// (MESSAGE_SPEC §7.4, the last occurrence wins, an array wrapper replaces), with
// the same lengths both times -- so the second occurrence fits the storage the
// first one just filled, which is the case a kept buffer that is not released
// gets wrong. Not canonical, so it takes part in the value and cut legs, not in
// the byte-for-byte one.
const cat = (a: Uint8Array, b: Uint8Array): Uint8Array => {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
};
const REPEATED: [string, Uint8Array][] = [["repeated", cat(enc(SAME), enc(FULL))]];

// --- verdicts --------------------------------------------------------------
{
  const dst = Store.decode(enc(FULL));
  check(Store.tryDecode(enc(SAME), dst) === DecodeStatus.Complete, "a whole message is Complete");
  const full = enc(FULL);
  check(Store.tryDecode(full.subarray(0, full.length - 1), Store.decode(full)) === DecodeStatus.Incomplete,
    "a message cut inside a field is Incomplete, returned");
  let code = "none";
  try {
    Store.tryDecode(Uint8Array.of(0x07), dst); // an unbalanced sequence end
  } catch (e) {
    code = e instanceof SofabError ? e.code : String(e);
  }
  check(code === SofabErrorCode.InvalidMsg, `a malformed message throws INVALID_MSG, got ${code}`);
}

// --- reused: the storage FULL left is the storage SAME lands in --------------
{
  const dst = Store.decode(enc(FULL));
  const kept = {
    a: dst.a, fa: dst.fa, ok: dst.ok, e: dst.e, w: dst.w, f64: dst.f64, b: dst.b, bu: dst.bu, bd: dst.bd,
    sa: dst.sa, ba: dst.ba, ba1: dst.ba[1], bx: dst.bx, bx2: dst.bx[2], ra: dst.ra, ra0: dst.ra[0],
    ua: dst.ua, ua1: dst.ua[1], mx: dst.mx, mx1: dst.mx[1], ta: dst.ns.ta, nb: dst.ns.nb, sl: dst.ns.sl,
  };
  check(Store.tryDecode(enc(SAME), dst) === DecodeStatus.Complete, "reused: Complete");
  check(json(dst) === json(Store.decode(enc(SAME))), `reused: reads as a fresh decode\n  got  ${json(dst)}`);
  const same: [string, unknown, unknown][] = [
    ["a", kept.a, dst.a], ["fa", kept.fa, dst.fa], ["ok", kept.ok, dst.ok], ["e", kept.e, dst.e],
    ["f64", kept.f64, dst.f64], ["b", kept.b, dst.b], ["bu", kept.bu, dst.bu], ["bd", kept.bd, dst.bd],
    ["sa", kept.sa, dst.sa], ["ba", kept.ba, dst.ba], ["ba[1]", kept.ba1, dst.ba[1]],
    ["bx", kept.bx, dst.bx], ["bx[2]", kept.bx2, dst.bx[2]], ["ra", kept.ra, dst.ra],
    ["ra[0]", kept.ra0, dst.ra[0]], ["ua", kept.ua, dst.ua], ["ua[1]", kept.ua1, dst.ua[1]],
    ["mx", kept.mx, dst.mx], ["mx[1]", kept.mx1, dst.mx[1]], ["ns.ta", kept.ta, dst.ns.ta],
    ["ns.nb", kept.nb, dst.ns.nb], ["ns.sl", kept.sl, dst.ns.sl],
  ];
  // A typed 64-bit array (int64: bigint) is reused like any other; the Long[] of
  // the other two modes is rebuilt, as decode() builds it.
  if (kept.w instanceof BigUint64Array) same.push(["w", kept.w, dst.w]);
  for (const [name, before, after] of same) check(before === after, `reused: ${name} is the storage the previous decode left`);

  // A plain decode never reaches into another message's storage.
  const fresh = Store.decode(enc(SAME));
  check(fresh.a !== dst.a && fresh.sa !== dst.sa && fresh.ra[0] !== dst.ra[0], "decode() builds its own storage");
}

// --- pairs ------------------------------------------------------------------
for (const [hn, held] of IMAGES) {
  for (const [nn, next] of IMAGES) {
    const dst = Store.decode(held);
    const st = Store.tryDecode(next, dst);
    check(st === DecodeStatus.Complete, `${hn} -> ${nn}: verdict ${st}`);
    check(json(dst) === json(Store.decode(next)), `${hn} -> ${nn}: reads as a fresh decode\n  got  ${json(dst)}\n  want ${json(Store.decode(next))}`);
    check(hex(dst.encode()) === hex(next), `${hn} -> ${nn}: re-encodes to the input`);
  }
}

for (const [hn, held] of IMAGES) {
  for (const [nn, next] of REPEATED) {
    const dst = Store.decode(held);
    check(Store.tryDecode(next, dst) === DecodeStatus.Complete, `${hn} -> ${nn}: Complete`);
    check(json(dst) === json(Store.decode(next)), `${hn} -> ${nn}: reads as a fresh decode\n  got  ${json(dst)}`);
  }
}

// --- JSON -------------------------------------------------------------------
// A JSON value already returned, and the input a message was built from, are
// the caller's: a later tryDecode into the message must change neither.
{
  const input = JSON.parse(JSON.stringify(FULL)) as Record<string, unknown>;
  const dst = Store.fromJSON(input);
  const snap = JSON.stringify(dst.toJSON());
  const out = dst.toJSON();
  Store.tryDecode(enc(SAME), dst);
  check(JSON.stringify(input) === JSON.stringify(FULL), "fromJSON's input is not written by a later tryDecode");
  check(JSON.stringify(out) === snap, "a toJSON value is not written by a later tryDecode");
}

// --- cut --------------------------------------------------------------------
// The incremental Decoder resets its destination and builds fresh storage, so
// after the same prefix it holds exactly what a fresh decode holds -- the
// partly decoded field included.
for (const [hn, held] of IMAGES) {
  for (const [nn, next] of [...IMAGES, ...REPEATED]) {
    for (let cut = 0; cut < next.length; cut++) {
      const prefix = next.subarray(0, cut);
      const dst = Store.decode(held);
      const st = Store.tryDecode(prefix, dst);
      const ref = new Store__Decoder();
      const want = ref.feed(prefix);
      check(st === want, `${hn} -> ${nn} cut at ${cut}: verdict ${st}, Decoder says ${want}`);
      check(json(dst) === json(ref.message),
        `${hn} -> ${nn} cut at ${cut}: holds what a fresh decode holds\n  got  ${json(dst)}\n  want ${json(ref.message)}`);
    }
  }
}

// --- refused ----------------------------------------------------------------
// bx is unbounded; its element 1 is longer than the 8-byte receiver cap, so the
// decode stops inside the array with only element 0 placed. The destination
// held FULL's four elements: none of the three past element 0 may survive.
{
  const over = enc({ a: [3], bx: [[1], [1, 2, 3, 4, 5, 6, 7, 8, 9]] });
  for (const [hn, held] of IMAGES) {
    const dst = Store.decode(held);
    let code = "none";
    try {
      Store.tryDecode(over, dst);
    } catch (e) {
      code = e instanceof SofabError ? e.code : String(e);
    }
    check(code === SofabErrorCode.LimitExceeded, `${hn} -> over cap: throws LIMIT_EXCEEDED, got ${code}`);
    const ref = new Store__Decoder();
    try {
      ref.feed(over);
    } catch {
      /* the same refusal */
    }
    check(json(dst) === json(ref.message),
      `${hn} -> over cap: holds what a fresh decode holds\n  got  ${json(dst)}\n  want ${json(ref.message)}`);
  }
}

if (failed > 0) {
  process.stderr.write(`reuse_storage_check: ${failed} failure(s)\n`);
  process.exit(1);
}
process.stdout.write("reuse_storage_check: ok\n");
