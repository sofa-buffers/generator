// Not generated: dropped into the generated project by the conformance harness,
// which builds it into the same jar as the message classes.
//
// A blob at an id the schema does not declare is SKIPPED -- its bytes are walked
// over, never materialised (MESSAGE_SPEC 7.3; CORELIB_PLAN 6.2.1 "a skipped field
// is never capped", 6.6 "the codec allocates no payload storage", 6.7.2).
//
// The reason this is a MEASUREMENT and not a "does it decode" row is that
// dropping a materialised payload also decodes. The generated blob() callback
// used to hand EVERY delivered payload to acc.blob(), which sizes a byte[] from
// the wire `total` and copies the bytes into it, and only then switch on (cur,
// id) and find no arm. A 1 MiB blob at an unknown id therefore cost 1 MiB of heap
// for a field nobody reads -- and every "a skipped field decodes COMPLETE" row in
// this suite passed while it did. So this counts the bytes the decode allocates
// and requires them to be a small fraction of the skipped payload.
//
// The project is built with max_dyn_blob_len: 8, so the row carries both halves
// at once: a 1 MiB blob at an unknown id is over the receiver cap by five orders
// of magnitude and must STILL decode COMPLETE -- a skipped field is never capped
// -- and must still cost nothing.
package message;

import java.lang.management.ManagementFactory;
import java.util.Arrays;
import org.sofabuffers.sofab.DecodeStatus;

public final class SkippedBlobAlloc {
    private static int putVarint(byte[] b, int p, long v) {
        while (v >= 0x80) { b[p++] = (byte) ((v & 0x7f) | 0x80); v >>>= 7; }
        b[p++] = (byte) v;
        return p;
    }

    private static void fail(String why) { System.err.println("FAIL: " + why); System.exit(1); }

    public static void main(String[] args) throws Exception {
        final int N = 1 << 20;        // 1 MiB payload
        final int SKIPPED_ID = 7;     // no field of `skipblob` declares it

        // fixlen header (id<<3 | 2), fixlen word (len<<3 | 3 == blob), payload.
        byte[] buf = new byte[N + 32];
        int p = putVarint(buf, 0, ((long) SKIPPED_ID << 3) | 2);
        p = putVarint(buf, p, ((long) N << 3) | 3);
        Arrays.fill(buf, p, p + N, (byte) 0x61);
        final byte[] msg = Arrays.copyOf(buf, p + N);

        Skipblob out = new Skipblob();
        DecodeStatus st = Skipblob.tryDecode(msg, out);
        if (st != DecodeStatus.COMPLETE) fail("a skipped 1 MiB blob must decode COMPLETE; got " + st);
        if (out.b.length != 0) fail("the skipped payload landed in the declared blob b");
        if (!out.s.isEmpty()) fail("the skipped payload landed in the declared string s");

        com.sun.management.ThreadMXBean mx =
            (com.sun.management.ThreadMXBean) ManagementFactory.getThreadMXBean();
        if (!mx.isThreadAllocatedMemorySupported()) fail("this JVM cannot measure thread allocation");
        mx.setThreadAllocatedMemoryEnabled(true);

        // Warm up: class loading, the visitor and the IStream all allocate once.
        for (int i = 0; i < 200; i++) Skipblob.tryDecode(msg, out);

        final int reps = 16;
        long before = mx.getCurrentThreadAllocatedBytes();
        for (int i = 0; i < reps; i++) Skipblob.tryDecode(msg, out);
        long per = (mx.getCurrentThreadAllocatedBytes() - before) / reps;
        System.out.println("    skipped-blob decode allocates " + per + " bytes (payload " + N + ")");

        // With the string-only destination gate this is >= 1 MiB per decode:
        // acc.blob() sizes a byte[] from the wire `total` and copies the payload
        // into it, and only then does the dispatch find no arm and drop it.
        if (per > N / 8) fail("a skipped blob was materialised: " + per + " bytes allocated for a payload of " + N);
    }
}
