// Not generated: dropped into the generated project by the conformance harness,
// which builds it into the same jar as the message classes.
//
// A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412).
//
// The rule: no value the codec delivers may outlive the callback it arrived in.
// §6.0 fixes that for `feed` -- a chunk is borrowed only for the duration of the
// call, so once it returns the caller may reuse, overwrite or free that memory
// and the decoded message must not be affected -- and §6.7.1 gives the one-shot
// path no exemption: `decode(buffer)` copies too, because a message whose
// lifetime depends on which entry point produced it is the divergence §6.7 ends.
//
// The oracle is DESTRUCTIVE, not comparative: encode a sample, decode it out of
// storage this program controls, DESTROY that storage, re-encode and diff.
// Nothing else in this suite reaches it. Every other decode here hands the
// harness a buffer that stays alive and unmodified for the whole run -- the
// chunk-invariance row (generator#413) included, which compares two readers of
// the same live bytes and would see identical values out of an aliased
// destination.
//
// CHUNK SIZE IS THE AXIS, not the entry point. A payload SPLIT across chunks is
// reassembled into PayloadAcc's own buffer and copied out of it whether or not
// the destination wanted a view, so a small-chunk-only feed is structurally
// unable to fail. The sweep therefore spans sizes and ends at one that carries
// the entire message.
//
// KNOWN REACH -- do not read a pass as "every field is copied":
//
//   * Java has no sub-range view of a byte[] and no mutable String, so the only
//     destination that CAN alias is one that keeps the whole `data` array the
//     callback was handed -- which is only ever the right bytes when a payload
//     fills a chunk exactly. That is a real regression shape (it is what a
//     "fast path for the whole-payload case" looks like when it is written
//     wrong) and the streaming leg is the one that catches it; the one-shot legs
//     are near-vacuous here, and are kept because §6.7.1 names that path
//     explicitly and a future backend change could give it teeth.
//   * The corelib copies today, on both branches: PayloadAcc.blob returns
//     Arrays.copyOfRange / Arrays.copyOf and PayloadAcc.string returns a String,
//     which is a copy the language makes. A corelib that stopped copying would
//     be caught here only where the generated code stores the result directly.
//   * A native array (someuintarray, somefloatarray, ...) never passes through a
//     payload callback at all: the generated visitor returns the message's OWN
//     int[]/float[] from Visitor.arrayBulk for the codec to fill, which is §6.7
//     route 1 (the caller's storage), and cannot alias anywhere.
//   * The scribble byte is 0x41 ('A'), not 0xff, for the reason the family
//     settled on: an aliased string destination must still RE-ENCODE, so the
//     oracle stays a byte diff and never becomes a UTF-8 error that unrelated
//     causes could produce.
package message;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import org.sofabuffers.sofab.DecodeStatus;

public final class OwnershipCheck {

    /** See the header note: an aliased string destination must still encode. */
    private static final byte SCRIBBLE = (byte) 0x41;

    /**
     * The sweep ends at a size larger than the whole message: only a chunk at
     * least as long as a payload reaches the corelib's whole-payload branch, and
     * only a chunk that STARTS at a payload reaches it with chunkOffset 0.
     */
    private static final int[] CHUNK_SIZES = {1, 7, 16, 32, 64, 4096};

    private static int failures = 0;

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(b.length * 2);
        for (byte x : b) {
            sb.append(String.format("%02x", x));
        }
        return sb.toString();
    }

    /**
     * Fills every aliasing-capable field kind: string, blob, array&lt;string&gt;,
     * array&lt;blob&gt;, a string nested in a struct, a string in a union and the
     * string key of a dynamic wrapper-array row -- plus the native arrays, which
     * are here so the wire carries them, not because they can alias.
     */
    private static Myfirstmessage sample() {
        Myfirstmessage m = new Myfirstmessage();
        m.somestring = "héllo wörld payload";
        m.someblob = new byte[]{1, 2, 3, 4, 5};
        m.someuintarray = new int[]{9, 8, 7, 6};
        m.somefloatarray = new float[]{1.5f, -2.5f, 3.5f};
        List<String> strings = new ArrayList<>();
        strings.add("a");
        strings.add("bb");
        strings.add("ccc");
        m.somestringarray = strings;
        List<byte[]> blobs = new ArrayList<>();
        blobs.add(new byte[]{9, 9});
        blobs.add(new byte[]{8});
        m.someblobarray = blobs;
        m.somestruct.nestedstring = "nested payload";
        m.someunion.option2 = "union payload";
        MyfirstmessageSomemapElem first = new MyfirstmessageSomemapElem();
        first.key = "first key";
        first.value = 1L;
        MyfirstmessageSomemapElem second = new MyfirstmessageSomemapElem();
        second.key = "second key";
        second.value = 2L;
        m.somemap = new ArrayList<>(List.of(first, second));
        return m;
    }

    /**
     * Re-encodes and diffs. Comparing bytes rather than fields is the stronger
     * statement anyway: two messages that encode identically ARE the same message
     * on the wire. A re-encode that THROWS counts as a failure of this check too
     * -- the encoder validates its input, so a scribbled destination can surface
     * as an exception rather than as different bytes.
     */
    private static void mustMatch(String what, byte[] want, Myfirstmessage got) {
        byte[] re;
        try {
            re = got.encode();
        } catch (RuntimeException e) {
            System.out.println("FAIL: " + what + ": re-encoding the decoded message threw: " + e);
            failures++;
            return;
        }
        if (!Arrays.equals(want, re)) {
            System.out.println("FAIL: " + what
                + ": a decoded field aliased the buffer it was decoded from");
            System.out.println("  want " + hex(want));
            System.out.println("  got  " + hex(re));
            System.out.println("  somestring = \"" + got.somestring + "\""
                + "  someblob = " + hex(got.someblob));
            for (int i = 0; i < got.someblobarray.size(); i++) {
                System.out.println("  someblobarray[" + i + "] = " + hex(got.someblobarray.get(i)));
            }
            failures++;
        }
    }

    public static void main(String[] args) throws Exception {
        final byte[] want = sample().encode();
        if (want.length == 0) {
            System.out.println("FAIL: the sample encoded to nothing");
            System.exit(2);
        }

        // ---- 1. one-shot, out of a MUTABLE copy scrubbed on return ----------
        // §6.7.1: `data` may be reused or overwritten the moment decode returns.
        byte[] wire = want.clone();
        Myfirstmessage one = new Myfirstmessage();
        DecodeStatus st = Myfirstmessage.tryDecode(wire, one);
        if (st != DecodeStatus.COMPLETE) {
            System.out.println("FAIL: one-shot tryDecode reported " + st);
            System.exit(1);
        }
        Arrays.fill(wire, SCRIBBLE);
        mustMatch("one-shot tryDecode", want, one);

        // ...and the convenience surface beside it, which wraps the same decode.
        byte[] wire2 = want.clone();
        Myfirstmessage two = Myfirstmessage.decode(wire2);
        Arrays.fill(wire2, SCRIBBLE);
        mustMatch("one-shot decode", want, two);

        // ---- 2. streaming, every chunk out of ONE reusable scratch ----------
        // §6.0: the borrow ends when feed returns, so the scratch is destroyed
        // there and reused for the next chunk -- which is what a caller reading
        // a socket into a fixed buffer actually does.
        for (int size : CHUNK_SIZES) {
            byte[] scratch = new byte[size];
            Myfirstmessage.Decoder dec = Myfirstmessage.decoder();
            DecodeStatus last = null;
            for (int i = 0; i < want.length; i += size) {
                int n = Math.min(size, want.length - i);
                System.arraycopy(want, i, scratch, 0, n);
                last = dec.feed(scratch, 0, n);
                Arrays.fill(scratch, SCRIBBLE);
            }
            if (last != DecodeStatus.COMPLETE) {
                System.out.println("FAIL: streaming feed(chunk=" + size + ") reported " + last
                    + ", expected COMPLETE");
                failures++;
                continue;
            }
            mustMatch("streaming feed(chunk=" + size + ")", want, dec.finish());
        }

        if (failures > 0) {
            System.exit(1);
        }
        System.out.println("ownership: " + want.length + " bytes, decoded message owns them after "
            + "the input was scribbled -- one-shot x2 + " + CHUNK_SIZES.length + " chunk sizes");
    }
}
