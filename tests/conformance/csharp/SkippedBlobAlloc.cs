// Not generated: dropped into the generated project by the conformance harness in
// place of the harness Program.cs, so it is the project's Main.
//
// A blob at an id the schema does not declare is SKIPPED -- its bytes are walked
// over, never materialised (MESSAGE_SPEC 7.3; CORELIB_PLAN 6.2.1 "a skipped field
// is never capped", 6.6 "the codec allocates no payload storage", 6.7.2).
//
// The reason this is a MEASUREMENT and not a "does it decode" row is that
// dropping a materialised payload also decodes. The generated Blob() callback
// used to hand EVERY delivered payload to PayloadAcc.Blob, which sizes a byte[]
// from the wire `total` and copies the bytes into it, and only then switch on
// (cur, id) and find no arm. A 1 MiB blob at an unknown id therefore cost 1 MiB
// of heap for a field nobody reads -- and every "a skipped field decodes
// Complete" row in this suite passed while it did. So this counts the bytes the
// decode allocates and requires them to be a small fraction of the skipped
// payload.
//
// The project is built with max_dyn_blob_len: 8, so the row carries both halves
// at once: a 1 MiB blob at an unknown id is over the receiver cap by five orders
// of magnitude and must STILL decode Complete -- a skipped field is never capped
// -- and must still cost nothing.
using System;
using sofab;
using Message;

static class SkippedBlobAlloc {
    static int PutVarint(byte[] b, int p, ulong v) {
        while (v >= 0x80) { b[p++] = (byte)((v & 0x7f) | 0x80); v >>= 7; }
        b[p++] = (byte)v;
        return p;
    }

    static int Fail(string why) { Console.Error.WriteLine("FAIL: " + why); return 1; }

    static int Main(string[] args) {
        const int N = 1 << 20;      // 1 MiB payload
        const int SkippedId = 7;    // no field of `skipblob` declares it

        // fixlen header (id<<3 | 2), fixlen word (len<<3 | 3 == blob), payload.
        var buf = new byte[N + 32];
        int p = PutVarint(buf, 0, ((ulong)SkippedId << 3) | 2);
        p = PutVarint(buf, p, ((ulong)N << 3) | 3);
        for (int i = 0; i < N; i++) buf[p + i] = 0x61;
        var msg = new byte[p + N];
        Array.Copy(buf, msg, msg.Length);

        var st = Skipblob.TryDecode(msg, out var outMsg);
        if (st != DecodeStatus.Complete) return Fail($"a skipped 1 MiB blob must decode Complete; got {st}");
        if (outMsg.b.Length != 0) return Fail("the skipped payload landed in the declared blob b");
        if (outMsg.s.Length != 0) return Fail("the skipped payload landed in the declared string s");

        // Warm up: the JIT and the first-touch allocations of the visitor path.
        for (int i = 0; i < 200; i++) Skipblob.TryDecode(msg, out _);

        const int reps = 16;
        long before = GC.GetAllocatedBytesForCurrentThread();
        for (int i = 0; i < reps; i++) Skipblob.TryDecode(msg, out _);
        long per = (GC.GetAllocatedBytesForCurrentThread() - before) / reps;
        Console.WriteLine($"    skipped-blob decode allocates {per} bytes (payload {N})");

        // With the String-only destination gate this is >= 1 MiB per decode:
        // PayloadAcc.Blob sizes a byte[] from the wire `total` and copies the
        // payload into it, and only then does the dispatch find no arm and drop it.
        if (per > N / 8) return Fail($"a skipped blob was materialised: {per} bytes allocated for a payload of {N}");
        return 0;
    }
}
