// Differential check: the two TypeScript decoders must not drift.
//
// The backend emits TWO decoders per type and that is deliberate. decode() runs
// the monomorphic Cursor over a contiguous buffer — the speed showcase, and the
// reason the push/visitor path was removed in the first place. It cannot be fed
// in chunks, so decoder()/feed() drives a visitor over the corelib's resumable
// IStream instead.
//
// The cost of two decoders is that every §7 verdict now exists twice and could
// drift apart. Emitting both from the same helpers narrows that; this pins it.
// The property under test is that the two are INDISTINGUISHABLE:
//
//   1. the same bytes decode to deeply equal values on both paths,
//   2. at every chunk size, one byte at a time included — where every varint,
//      every string payload and every array element is split across feeds, so
//      any parse state the visitor fails to carry between calls shows up,
//   3. a truncated stream is rejected rather than returned half-filled.
//
// It also pins the ownership rule the two decoders can silently disagree on:
// a decoded message must own its bytes (CORELIB_PLAN §5.1 read on the decode
// side), so overwriting the input buffer afterwards may not change it. The
// streaming path always copied; the cursor path handed blob destinations a view
// into the buffer, and no value comparison between the two could see it.
//
// Run against the shared example so every field shape is covered, and against
// nested_rows so the wrapper-row collectors (array<array<string|blob|struct>>,
// depth 3) are covered too — those need a different collector per row kind and
// recurse, which a scalar-only message never exercises.

import { DecodeStatus, SofabError } from "@sofa-buffers/corelib";
//SOFAB_IMPORT

// Canonical form for comparison: bigint and Uint8Array have no JSON encoding of
// their own, and a raw === would compare object identity rather than value.
function norm(x: unknown): string {
  return JSON.stringify(x, (_k, v) =>
    typeof v === "bigint" ? v.toString() :
    v instanceof Uint8Array ? Array.from(v) : v);
}

/** The generated entry point, which allocates and owns its buffer (§5.1). */
type Encodable = { encode(): Uint8Array };

/**
 * Feed `wire` through a fresh decoder in fixed-size chunks and return the
 * decoded message. `size` of 1 is the sharp end.
 */
function feedInChunks<T>(mk: () => { feed(c: Uint8Array): DecodeStatus; finish(): T },
                         wire: Uint8Array, size: number): T {
  const d = mk();
  for (let i = 0; i < wire.length; i += size) {
    d.feed(wire.subarray(i, Math.min(i + size, wire.length)));
  }
  return d.finish();
}

let checks = 0;

/** One subject: a populated message, its one-shot decode, and its chunked ones. */
function check<T extends Encodable>(
  label: string,
  m: T,
  oneShot: (b: Uint8Array) => T,
  mk: () => { feed(c: Uint8Array): DecodeStatus; finish(): T },
): void {
  const wire = m.encode();
  const want = norm(oneShot(wire));

  // A decoded message OWNS its bytes (CORELIB_PLAN §5.1 read on the decode side):
  // the cursor hands blob payloads over as views into the input buffer, and a
  // destination that kept one would make the message's lifetime the buffer's.
  // Scribbling over a throwaway copy of the wire after decoding is what shows the
  // difference — comparing VALUES between the two decoders cannot, because the
  // streaming path has always copied and the two would simply agree while both
  // reading freed memory.
  const scratch = wire.slice();
  const owned = oneShot(scratch);
  const before = norm(owned);
  const reencodedBefore = norm(owned.encode());
  scratch.fill(0xff);
  if (norm(owned) !== before) {
    console.error(`FAIL ${label}: the decoded message aliases its input buffer`);
    console.error(`  before: ${before}`);
    console.error(`  after : ${norm(owned)}`);
    process.exit(1);
  }
  if (norm(owned.encode()) !== reencodedBefore) {
    console.error(`FAIL ${label}: re-encoding changed once the input buffer was overwritten`);
    process.exit(1);
  }
  checks++;

  // (1) + (2): every chunk size must agree with the cursor path.
  for (const size of [1, 2, 3, 7, 16, wire.length]) {
    const got = norm(feedInChunks(mk, wire, size));
    if (got !== want) {
      console.error(`FAIL ${label}: chunk size ${size} disagrees with decode()`);
      console.error(`  decode(): ${want}`);
      console.error(`  feed()  : ${got}`);
      process.exit(1);
    }
    checks++;
  }

  // (3) truncation: a stream that ends mid-field must not yield a message.
  if (wire.length > 1) {
    const d = mk();
    d.feed(wire.subarray(0, wire.length - 1));
    let rejected = false;
    try { d.finish(); } catch { rejected = true; }
    if (!rejected) {
      console.error(`FAIL ${label}: truncated stream was accepted`);
      process.exit(1);
    }
    checks++;
  }

  console.log(`   [${label}] ${wire.length} bytes, cursor === feed at 6 chunk sizes (1 byte included), truncation rejected, decode owns its bytes`);
}

/**
 * A REJECTED message: both paths must refuse the same bytes the same WAY.
 *
 * The value checks above only compare messages that decode. A rejection has a
 * second observable besides "it threw" — the exception type — and the two paths
 * reach it through different code: the cursor decodes strings inside the corelib
 * (Cursor.readString), the visitor transcodes in generated code. When only one
 * of them converted the fatal TextDecoder's TypeError, feed() threw a raw
 * TypeError that walked straight past a `catch (e) { e instanceof SofabError }`
 * — a rejected message and an unhandled exception look very different to a
 * caller feeding untrusted bytes (generator#297).
 *
 * Taken over raw wire bytes rather than an encoded message, because the point is
 * input this library would never produce.
 */
function checkReject(label: string, wire: Uint8Array,
                     oneShot: (b: Uint8Array) => unknown,
                     mk: () => { feed(c: Uint8Array): DecodeStatus; finish(): unknown }): void {
  const grab = (fn: () => unknown): Error => {
    try { fn(); } catch (e) { return e as Error; }
    console.error(`FAIL ${label}: expected a rejection, got none`);
    process.exit(1);
  };

  const viaCursor = grab(() => oneShot(wire));
  if (!(viaCursor instanceof SofabError)) {
    console.error(`FAIL ${label}: decode() threw ${viaCursor.constructor.name}, not SofabError`);
    process.exit(1);
  }
  checks++;

  for (const size of [1, 2, 3, 7, 16, wire.length]) {
    const viaFeed = grab(() => feedInChunks(mk, wire, size));
    if (!(viaFeed instanceof SofabError)) {
      console.error(`FAIL ${label}: feed() at chunk size ${size} threw ${viaFeed.constructor.name}, not SofabError`);
      console.error(`  decode() threw SofabError(${viaCursor.code}): ${viaCursor.message}`);
      console.error(`  feed()   threw ${viaFeed.constructor.name}: ${viaFeed.message}`);
      process.exit(1);
    }
    if (viaFeed.code !== viaCursor.code) {
      console.error(`FAIL ${label}: chunk size ${size} rejects with code ${viaFeed.code}, decode() with ${viaCursor.code}`);
      process.exit(1);
    }
    checks++;
  }

  console.log(`   [${label}] rejected as SofabError(${viaCursor.code}) by decode() and by feed() at 6 chunk sizes`);
}

//SOFAB_BODY

console.log(`streaming: ${checks} differential checks, decode() and feed() agree everywhere`);
