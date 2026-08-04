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
// Run against the shared example so every field shape is covered, and against
// nested_rows so the wrapper-row collectors (array<array<string|blob|struct>>,
// depth 3) are covered too — those need a different collector per row kind and
// recurse, which a scalar-only message never exercises.

import { OStream, DecodeStatus } from "@sofa-buffers/corelib";
//SOFAB_IMPORT

// Canonical form for comparison: bigint and Uint8Array have no JSON encoding of
// their own, and a raw === would compare object identity rather than value.
function norm(x: unknown): string {
  return JSON.stringify(x, (_k, v) =>
    typeof v === "bigint" ? v.toString() :
    v instanceof Uint8Array ? Array.from(v) : v);
}

function encode(m: { serialize(os: OStream): void }): Uint8Array {
  const os = new OStream();
  m.serialize(os);
  return os.bytes();
}

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
function check<T extends { serialize(os: OStream): void }>(
  label: string,
  m: T,
  oneShot: (b: Uint8Array) => T,
  mk: () => { feed(c: Uint8Array): DecodeStatus; finish(): T },
): void {
  const wire = encode(m);
  const want = norm(oneShot(wire));

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

  console.log(`   [${label}] ${wire.length} bytes, cursor === feed at 6 chunk sizes (1 byte included), truncation rejected`);
}

//SOFAB_BODY

console.log(`streaming: ${checks} differential checks, decode() and feed() agree everywhere`);
