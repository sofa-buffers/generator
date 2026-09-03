// Not generated: dropped into a generated project by the conformance harness,
// REPLACING the generated Program.cs -- the project's own Main is the JSON
// harness, and this needs its own entry point in the same assembly, the way
// SkippedBlobAlloc.cs does.
//
// A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412).
//
// The rule: no value the codec delivers may outlive the callback it arrived in.
// §6.0 fixes that for `Feed` -- a chunk is borrowed only for the duration of the
// call, so once it returns the caller may reuse, overwrite or free that memory
// and the decoded message must not be affected -- and §6.7.1 gives the one-shot
// path no exemption: `Decode(buffer)` copies too, because a message whose
// lifetime depends on which entry point produced it is the divergence §6.7 ends.
//
// The oracle is DESTRUCTIVE, not comparative: encode a sample, decode it out of
// storage this program controls, DESTROY that storage, re-encode and diff.
// Nothing else in this suite reaches it. Every other decode here hands the
// harness a buffer that stays alive and unmodified for the whole run -- the
// chunk-invariance row (generator#413) included, which compares two readers of
// the SAME live bytes and would see identical values out of an aliased
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
//   * IVisitor.String/.Blob hand over a `byte[] data` plus offsets, never a
//     ReadOnlySpan or ReadOnlyMemory, and C# has no sub-range view of a byte[]
//     and no mutable string. So the only destination that CAN alias today is one
//     that keeps the whole `data` array it was handed -- which is only ever the
//     right bytes when a payload fills a chunk exactly. That is a real regression
//     shape (it is what a "fast path for the whole-payload case" looks like when
//     it is written wrong) and the streaming leg is the one that catches it; the
//     one-shot legs are near-vacuous here, and are kept because §6.7.1 names that
//     path explicitly.
//   * The row's real future is a corelib that moves the payload callback to
//     ReadOnlySpan<byte>/ReadOnlyMemory<byte>. A Memory<byte> destination would
//     then be a genuine view of the input, and this is the check that would see
//     it -- which is why the leg exists even though nothing can fail it today.
//   * The corelib copies on both branches: PayloadAcc.Blob allocates a new byte[]
//     and Array.Copy's into it whether the payload arrived whole or in pieces,
//     and .String goes through Utf8.Decode, which builds a string.
//   * A native array (someuintarray, somefloatarray, ...) never passes through a
//     payload callback at all: the generated visitor offers the message's OWN
//     array as the codec's bulk destination, which is §6.7 route 1 (the caller's
//     storage), and cannot alias anywhere.
//   * The scribble byte is 0x41 ('A'), not 0xff, for the reason the family
//     settled on: an aliased string destination must still RE-ENCODE, so the
//     oracle stays a byte diff and never becomes a UTF-8 error that unrelated
//     causes could produce.
//
// The `using` below names the namespace the harness config sets. A project
// generated WITHOUT targets.csharp.namespace lands in `Message` instead, so this
// file is built in a project generated from the suite's own $WORK/cfg.yaml.
using System;
using System.Collections.Generic;
using System.Text;
using sofab;
using Sofabuffers;

internal static class OwnershipCheck {

    /// <summary>See the header note: an aliased string destination must still encode.</summary>
    private const byte Scribble = 0x41;

    /// <summary>
    /// The sweep ends at a size larger than the whole message: only a chunk at
    /// least as long as a payload reaches the corelib's whole-payload branch, and
    /// only a chunk that STARTS at a payload reaches it with chunkOffset 0.
    /// </summary>
    private static readonly int[] ChunkSizes = { 1, 7, 16, 32, 64, 4096 };

    private static int failures = 0;

    private static string Hex(byte[] b) {
        var sb = new StringBuilder(b.Length * 2);
        foreach (var x in b) { sb.Append(x.ToString("x2")); }
        return sb.ToString();
    }

    /// <summary>
    /// Fills every aliasing-capable field kind: string, blob, array&lt;string&gt;,
    /// array&lt;blob&gt;, a string nested in a struct, a string in a union and the
    /// string key of a dynamic wrapper-array row -- plus the native arrays, which
    /// are here so the wire carries them, not because they can alias.
    /// </summary>
    private static Myfirstmessage Sample() {
        var m = new Myfirstmessage();
        m.somestring = "héllo wörld payload";
        m.someblob = new byte[]{1, 2, 3, 4, 5};
        m.someuintarray = new uint[]{9, 8, 7, 6};
        m.somefloatarray = new float[]{1.5f, -2.5f, 3.5f};
        m.somestringarray = new List<string>{"a", "bb", "ccc"};
        m.someblobarray = new List<byte[]>{ new byte[]{9, 9}, new byte[]{8} };
        m.somestruct.nestedstring = "nested payload";
        m.someunion.option2 = "union payload";
        m.somemap = new List<MyfirstmessageSomemapElem>{
            new MyfirstmessageSomemapElem{ key = "first key", value = 1 },
            new MyfirstmessageSomemapElem{ key = "second key", value = 2 },
        };
        return m;
    }

    /// <summary>
    /// Re-encodes and diffs. Comparing bytes rather than fields is the stronger
    /// statement anyway: two messages that encode identically ARE the same message
    /// on the wire. A re-encode that THROWS counts as a failure of this check too
    /// -- the encoder validates its input, so a scribbled destination can surface
    /// as an exception rather than as different bytes.
    /// </summary>
    private static void MustMatch(string what, byte[] want, Myfirstmessage got) {
        byte[] re;
        try {
            re = got.Encode();
        } catch (Exception e) {
            Console.WriteLine($"FAIL: {what}: re-encoding the decoded message threw: {e.Message}");
            failures++;
            return;
        }
        if (Hex(re) != Hex(want)) {
            Console.WriteLine($"FAIL: {what}: a decoded field aliased the buffer it was decoded from");
            Console.WriteLine("  want " + Hex(want));
            Console.WriteLine("  got  " + Hex(re));
            Console.WriteLine($"  somestring = \"{got.somestring}\"  someblob = " + Hex(got.someblob));
            for (int i = 0; i < got.someblobarray.Count; i++) {
                Console.WriteLine($"  someblobarray[{i}] = " + Hex(got.someblobarray[i]));
            }
            failures++;
        }
    }

    private static int Main() {
        var want = Sample().Encode();
        if (want.Length == 0) { Console.WriteLine("FAIL: the sample encoded to nothing"); return 2; }

        // ---- 1. one-shot, out of a MUTABLE copy scrubbed on return ----------
        // §6.7.1: `data` may be reused or overwritten the moment Decode returns.
        var wire = (byte[])want.Clone();
        var st = Myfirstmessage.TryDecode(wire, out var one);
        if (st != DecodeStatus.Complete) {
            Console.WriteLine("FAIL: one-shot TryDecode reported " + st);
            return 1;
        }
        Array.Fill(wire, Scribble);
        MustMatch("one-shot TryDecode", want, one);

        // ...and the convenience surface beside it, which wraps the same decode.
        var wire2 = (byte[])want.Clone();
        var two = Myfirstmessage.Decode(wire2);
        Array.Fill(wire2, Scribble);
        MustMatch("one-shot Decode", want, two);

        // ---- 2. streaming, every chunk out of ONE reusable scratch ----------
        // §6.0: the borrow ends when Feed returns, so the scratch is destroyed
        // there and reused for the next chunk -- which is what a caller reading a
        // socket into a fixed buffer actually does.
        foreach (var size in ChunkSizes) {
            var scratch = new byte[size];
            var dec = new Myfirstmessage.Decoder();
            DecodeStatus last = DecodeStatus.Incomplete;
            for (int i = 0; i < want.Length; i += size) {
                int n = Math.Min(size, want.Length - i);
                Array.Copy(want, i, scratch, 0, n);
                last = dec.Feed(scratch, 0, n);
                Array.Fill(scratch, Scribble);
            }
            if (last != DecodeStatus.Complete) {
                Console.WriteLine($"FAIL: streaming Feed(chunk={size}) reported {last}, expected Complete");
                failures++;
                continue;
            }
            MustMatch($"streaming Feed(chunk={size})", want, dec.Finish());
        }

        if (failures > 0) { return 1; }
        Console.WriteLine($"ownership: {want.Length} bytes, decoded message owns them after the "
            + $"input was scribbled -- one-shot x2 + {ChunkSizes.Length} chunk sizes");
        return 0;
    }
}
