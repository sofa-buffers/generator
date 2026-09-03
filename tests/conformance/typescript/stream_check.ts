// Differential check: ONE decoder, fed two ways, must not drift.
//
// There is exactly one decode surface (CORELIB_PLAN §5.3.1). The corelib's
// `decode(bytes, visitor)` is one `IStream.feed` of the whole buffer, and the
// generated `decode()` and `decoder()/feed()` build the same visitor over it.
// What can still drift is where the chunk boundaries fall: only the streaming
// entry point makes a field arrive in pieces, and every §7 verdict has to be
// reached identically whether or not it does. The property under test is that
// the boundaries are INVISIBLE:
//
//   1. the same bytes decode to deeply equal values however they are chunked,
//   2. at every chunk size, one byte at a time included — where every varint,
//      every string payload and every array element is split across feeds, so
//      any parse state the visitor fails to carry between calls shows up,
//   3. a truncated stream is rejected rather than returned half-filled.
//
// It also pins the LIFETIME half of the same rule: a decoded message must OWN
// its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412), so the buffer it came
// from may be reused, overwritten or freed the moment the call returns — §6.0
// for `feed`, and §6.7.1 for the one-shot path, which gets no exemption. That
// oracle is DESTRUCTIVE rather than comparative: decode, destroy the input,
// re-encode and diff. Comparing values between two feeds cannot see it — both
// would be reading the same live bytes and would simply agree.
//
// KNOWN REACH of the ownership legs — do not read a pass as "every field is
// copied". The only TypeScript destination that CAN alias is a `Uint8Array`:
// the scalar `blob`, each element of a blob array, and a blob row. A `string`
// cannot — a JS string is an immutable copy the runtime makes — and a numeric
// array is a fresh JS array the generated collector fills element by element.
// The hazard is real on the blob legs: `PayloadAcc.take` is handed a window
// into the buffer being read and copies out of it, and a destination that kept
// the window instead would make the message's lifetime the buffer's.
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

/**
 * The same feed, but every chunk is copied into ONE reusable scratch buffer that
 * is DESTROYED the instant `feed` returns — what a caller refilling a socket
 * buffer does, and what §6.0 says it is allowed to do.
 */
function feedFromScratch<T>(mk: () => { feed(c: Uint8Array): DecodeStatus; finish(): T },
                            wire: Uint8Array, size: number): T {
  const d = mk();
  const scratch = new Uint8Array(size);
  for (let i = 0; i < wire.length; i += size) {
    const n = Math.min(size, wire.length - i);
    scratch.set(wire.subarray(i, i + n), 0);
    d.feed(scratch.subarray(0, n));
    scratch.fill(SCRIBBLE);
  }
  return d.finish();
}

/**
 * The byte an input buffer is destroyed with: 'A', not 0xff. An aliased string
 * destination must still RE-ENCODE, or the oracle stops being a byte comparison
 * and becomes an encoder error that unrelated causes could produce.
 */
const SCRIBBLE = 0x41;

/**
 * The chunk sizes every sweep uses. It has to END at one that delivers every
 * payload whole: a payload SPLIT across chunks is reassembled into the corelib's
 * accumulator and copied out of it whether or not the destination wanted a view,
 * so which sizes can see an aliased destination is decided by where the
 * boundaries happen to fall, not by how small the chunks are. Measured with the
 * generated blob arm broken on purpose (`this.o.someblob = src.subarray(...)` on
 * the whole-payload path) against the example subject: 1, 2, 3 and 16 pass while
 * 7, 32, 64 and the whole-message size fail. Only the last size is guaranteed —
 * it is the one that cannot split anything.
 */
const chunkSizes = (wire: Uint8Array): number[] => [1, 2, 3, 7, 16, wire.length];

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

  // A decoded message OWNS its bytes, on the ONE-SHOT path (CORELIB_PLAN §6.7 /
  // §6.7.1, generator#412): `decode(buffer)` copies too, so the buffer may be
  // overwritten the moment it returns. Scribbling over a throwaway copy of the
  // wire after decoding is what shows a destination that kept a window into it —
  // comparing VALUES between two feeds cannot, because both read the same live
  // bytes and would simply agree.
  const scratch = wire.slice();
  const owned = oneShot(scratch);
  const before = norm(owned);
  const reencodedBefore = norm(owned.encode());
  scratch.fill(SCRIBBLE);
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

  // ...and on the STREAMING path, which is where a retained chunk pointer hides:
  // §6.0 ends the borrow when `feed` returns, so every chunk here is fed out of
  // ONE reusable scratch that is destroyed immediately afterwards. Both the value
  // and the re-encoded bytes must be what the one-shot decode produced.
  for (const size of chunkSizes(wire)) {
    const got = feedFromScratch(mk, wire, size);
    if (norm(got) !== before || norm(got.encode()) !== reencodedBefore) {
      console.error(`FAIL ${label}: chunk size ${size}: a decoded field aliased the buffer it was fed from`);
      console.error(`  want: ${before}`);
      console.error(`  got : ${norm(got)}`);
      console.error(`  want bytes: ${reencodedBefore}`);
      console.error(`  got  bytes: ${norm(got.encode())}`);
      process.exit(1);
    }
    checks++;
  }

  // (1) + (2): every chunk size must agree with the one-shot decode.
  for (const size of chunkSizes(wire)) {
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

  console.log(`   [${label}] ${wire.length} bytes, decode() === feed() at 6 chunk sizes (1 byte included), truncation rejected, decoded message owns its bytes on both paths`);
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

/**
 * ACCEPTED raw wire bytes: both paths must take them, and take them the same way.
 *
 * checkReject above is the other half of one rule. CORELIB_PLAN §6.4.5 says a
 * string a decoder STEPS OVER is never UTF-8-validated, in any mode, and §6.4.4
 * says a read one is judged at payload completion — so the same invalid bytes are
 * a rejection at a declared id and a non-event at a skipped one. Only the
 * rejection was pinned here, and that is the half the CORELIB owns: decodeUtf8
 * raises it wherever it is called from. Which fields are read at all is generated
 * code's decision, and the accept half is what pins it (generator#417).
 *
 * `check` cannot serve: it starts from an Encodable and encodes it, so it can
 * only ever build wire this library would itself produce — and no encoder emits a
 * field at an id its schema does not declare. Like checkReject this therefore
 * takes raw bytes, and like it, it sweeps six chunk sizes: the two skip defects
 * this family has already had (generator#297, generator#300) were both invisible
 * on the one-shot path and one of them only appeared when the header and its
 * payload landed in different feeds.
 *
 * `want` is asserted on the one-shot result and every chunked result is required
 * to equal it, so a decoder that skipped the whole message cannot satisfy it.
 */
function checkAccept(label: string, wire: Uint8Array,
                     oneShot: (b: Uint8Array) => unknown,
                     mk: () => { feed(c: Uint8Array): DecodeStatus; finish(): unknown },
                     want: Record<string, unknown>): void {
  let viaOneShot: unknown;
  try {
    viaOneShot = oneShot(wire);
  } catch (e) {
    console.error(`FAIL ${label}: decode() rejected bytes it must accept: ${String(e)}`);
    process.exit(1);
  }
  const fields = (viaOneShot as { toJSON(): Record<string, unknown> }).toJSON();
  for (const [k, v] of Object.entries(want)) {
    if (!(k in fields)) {
      console.error(`FAIL ${label}: decode() printed no ${k}; got ${norm(fields)}`);
      process.exit(1);
    }
    if (norm(fields[k]) !== norm(v)) {
      console.error(`FAIL ${label}: decode() left ${k} = ${norm(fields[k])}, want ${norm(v)}`);
      process.exit(1);
    }
  }
  const expected = norm(viaOneShot);
  checks++;

  for (const size of [1, 2, 3, 7, 16, wire.length]) {
    let got: string;
    try {
      got = norm(feedInChunks(mk, wire, size));
    } catch (e) {
      console.error(`FAIL ${label}: feed() at chunk size ${size} rejected bytes decode() accepted: ${String(e)}`);
      process.exit(1);
    }
    if (got !== expected) {
      console.error(`FAIL ${label}: chunk size ${size} disagrees with decode()`);
      console.error(`  decode(): ${expected}`);
      console.error(`  feed()  : ${got}`);
      process.exit(1);
    }
    checks++;
  }

  console.log(`   [${label}] accepted with the expected values by decode() and by feed() at 6 chunk sizes`);
}

//SOFAB_BODY

console.log(`streaming: ${checks} differential checks, decode() and feed() agree everywhere`);
