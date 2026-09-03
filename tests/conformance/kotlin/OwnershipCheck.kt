// Not generated: dropped into the generated project by the conformance harness,
// which compiles it into the same install image as the message classes. It is
// the first hand-written source this suite adds to a generated project, so the
// pattern is stated here: one file in the message package with a top-level
// `main`, reached as `message.OwnershipCheckKt` -- NOT through the application
// plugin's mainClass, which stays the JSON harness.
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
// Nothing else in this suite reaches it. The streaming row above feeds one
// caller-owned array it never mutates -- it pins that decoder STATE survives a
// boundary at every byte offset, which is a different property, and an aliased
// destination reads back perfectly from a buffer that stays alive.
//
// CHUNK SIZE IS THE AXIS, not the entry point. A payload SPLIT across chunks is
// reassembled into PayloadAcc's own buffer and copied out of it whether or not
// the destination wanted a view, so a small-chunk-only feed is structurally
// unable to fail. The sweep therefore spans sizes and ends at one that carries
// the entire message.
//
// KNOWN REACH -- do not read a pass as "every field is copied":
//
//   * Kotlin has no sub-range view of a ByteArray and no mutable String, so the
//     only destination that CAN alias is one that keeps the whole `data` array
//     the callback was handed -- which is only ever the right bytes when a
//     payload fills a chunk exactly. That is a real regression shape (it is what
//     a "fast path for the whole-payload case" looks like when it is written
//     wrong) and the streaming leg is the one that catches it; the one-shot legs
//     are near-vacuous here, and are kept because §6.7.1 names that path
//     explicitly and a future backend change could give it teeth.
//   * The corelib copies today, on both branches: PayloadAcc.blob answers with a
//     fresh ByteArray and PayloadAcc.string with a String, which is a copy the
//     language makes. A corelib that stopped copying would be caught here only
//     where the generated code stores the result directly.
//   * A native array (someuintarray, somefloatarray, ...) never passes through a
//     payload callback at all: the generated visitor offers the message's OWN
//     UIntArray/FloatArray as the codec's destination, which is §6.7 route 1
//     (the caller's storage), and cannot alias anywhere.
//   * The scribble byte is 0x41 ('A'), not 0xff, for the reason the family
//     settled on: an aliased string destination must still RE-ENCODE, so the
//     oracle stays a byte diff and never becomes a UTF-8 error that unrelated
//     causes could produce.
package message

import org.sofabuffers.sofab.DecodeStatus

/** See the header note: an aliased string destination must still encode. */
private const val SCRIBBLE: Byte = 0x41

/**
 * The sweep ends at a size larger than the whole message: only a chunk at least
 * as long as a payload reaches the corelib's whole-payload branch, and only a
 * chunk that STARTS at a payload reaches it with chunkOffset 0.
 */
private val CHUNK_SIZES = intArrayOf(1, 7, 16, 32, 64, 4096)

private var failures = 0

private fun hex(b: ByteArray): String = b.joinToString("") { "%02x".format(it) }

/**
 * Fills every aliasing-capable field kind: string, blob, array<string>,
 * array<blob>, a string nested in a struct, a string in a union and the string
 * key of a dynamic wrapper-array row -- plus the native arrays, which are here
 * so the wire carries them, not because they can alias.
 */
private fun sample(): Myfirstmessage {
    val m = Myfirstmessage()
    m.somestring = "héllo wörld payload"
    m.someblob = byteArrayOf(1, 2, 3, 4, 5)
    m.someuintarray = uintArrayOf(9u, 8u, 7u, 6u)
    m.somefloatarray = floatArrayOf(1.5f, -2.5f, 3.5f)
    m.somestringarray = mutableListOf("a", "bb", "ccc")
    m.someblobarray = mutableListOf(byteArrayOf(9, 9), byteArrayOf(8))
    m.somestruct.nestedstring = "nested payload"
    m.someunion.option2 = "union payload"
    m.somemap = mutableListOf(
        MyfirstmessageSomemapElem().also { it.key = "first key"; it.value = 1u },
        MyfirstmessageSomemapElem().also { it.key = "second key"; it.value = 2u },
    )
    return m
}

/**
 * Re-encodes and diffs. Comparing bytes rather than fields is the stronger
 * statement anyway: two messages that encode identically ARE the same message on
 * the wire. A re-encode that THROWS counts as a failure of this check too -- the
 * encoder validates its input, so a scribbled destination can surface as an
 * exception rather than as different bytes.
 */
private fun mustMatch(what: String, want: ByteArray, got: Myfirstmessage) {
    val re = try {
        got.encode()
    } catch (e: RuntimeException) {
        println("FAIL: $what: re-encoding the decoded message threw: $e")
        failures++
        return
    }
    if (!want.contentEquals(re)) {
        println("FAIL: $what: a decoded field aliased the buffer it was decoded from")
        println("  want " + hex(want))
        println("  got  " + hex(re))
        println("  somestring = \"${got.somestring}\"  someblob = " + hex(got.someblob))
        got.someblobarray.forEachIndexed { i, b -> println("  someblobarray[$i] = " + hex(b)) }
        failures++
    }
}

fun main() {
    val want = sample().encode()
    if (want.isEmpty()) {
        println("FAIL: the sample encoded to nothing")
        kotlin.system.exitProcess(2)
    }

    // ---- 1. one-shot, out of a MUTABLE copy scrubbed on return -------------
    // §6.7.1: `data` may be reused or overwritten the moment decode returns.
    val wire = want.copyOf()
    val one = Myfirstmessage()
    val st = Myfirstmessage.tryDecode(wire, one)
    if (st != DecodeStatus.COMPLETE) {
        println("FAIL: one-shot tryDecode reported $st")
        kotlin.system.exitProcess(1)
    }
    wire.fill(SCRIBBLE)
    mustMatch("one-shot tryDecode", want, one)

    // ...and the convenience surface beside it, which wraps the same decode.
    val wire2 = want.copyOf()
    val two = Myfirstmessage.decode(wire2)
    wire2.fill(SCRIBBLE)
    mustMatch("one-shot decode", want, two)

    // ---- 2. streaming, every chunk out of ONE reusable scratch -------------
    // §6.0: the borrow ends when feed returns, so the scratch is destroyed there
    // and reused for the next chunk -- which is what a caller reading a socket
    // into a fixed buffer actually does.
    for (size in CHUNK_SIZES) {
        val scratch = ByteArray(size)
        val dec = Myfirstmessage.decoder()
        var last: DecodeStatus? = null
        var i = 0
        while (i < want.size) {
            val n = minOf(size, want.size - i)
            want.copyInto(scratch, 0, i, i + n)
            last = dec.feed(scratch, 0, n)
            scratch.fill(SCRIBBLE)
            i += n
        }
        if (last != DecodeStatus.COMPLETE) {
            println("FAIL: streaming feed(chunk=$size) reported $last, expected COMPLETE")
            failures++
            continue
        }
        mustMatch("streaming feed(chunk=$size)", want, dec.finish())
    }

    if (failures > 0) {
        kotlin.system.exitProcess(1)
    }
    println(
        "ownership: ${want.size} bytes, decoded message owns them after the input was " +
            "scribbled -- one-shot x2 + ${CHUNK_SIZES.size} chunk sizes"
    )
}
