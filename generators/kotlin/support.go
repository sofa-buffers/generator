package kotlin

import (
	"fmt"
	"strings"
)

// sbufSupport emits Sbuf.kt: the small schema-driven support the generated
// message classes share.
//
// It is generated code TODAY, and should not stay that way. Everything here is
// static under ARCHITECTURE §8: placing a wrapper element at the index its id
// names, growing a native array against a declared capacity rather than an
// untrusted wire count, reassembling a payload that straddled a chunk -- each
// has the same shape for every schema, and takes its schema dependence (the
// capacity, the index, the length) as an argument. That is the test §8 applies,
// and it puts all of this in corelib-kotlin-mp; see generator#345 for the move
// and the names it lands under.
//
// Until then it only calls or feeds the corelib's typed API and never touches
// wire bytes, and it is emitted once per package. Note what it does NOT do: the
// three loops below run over every primitive base type regardless of what the
// schema uses, so a schema with one u32 array still gets all 36 members.
func (g *gen) sbufSupport() []byte {
	f := &kfile{}
	g.header(f)
	f.line("/**")
	f.line(" * Shared support for the generated message classes.")
	f.line(" *")
	f.line(" * `internal`, and not part of any generated type's public surface: a caller")
	f.line(" * uses the message classes, never this.")
	f.line(" */")
	f.line("internal object Sbuf {")

	f.line("    // Shared zero-length defaults: field initializers and reset() reference these")
	f.line("    // instead of allocating a fresh empty array per instance (a decode replaces")
	f.line("    // them anyway, and an empty array has no state to share wrongly).")
	for _, t := range primBaseOrder {
		f.line("    internal val EMPTY_%s: %s = %s(0)", strings.ToUpper(baseSuffix(t)), t, t)
	}
	f.blank()

	f.line("    /**")
	f.line("     * A growable byte sink.")
	f.line("     *")
	f.line("     * Two jobs, both of them cases where the SIZE is not known in advance:")
	f.line("     * reassembling a string/blob payload the corelib delivered in chunks, and")
	f.line("     * draining the flush sink of an encode whose message the schema cannot")
	f.line("     * bound. [buf] is exposed so a reassembled payload can be converted out of")
	f.line("     * the backing array without a second copy; only `[0, size)` is live.")
	f.line("     */")
	f.line("    internal class Acc {")
	f.line("        internal var buf: ByteArray = ByteArray(64)")
	f.line("            private set")
	f.line("        internal var size: Int = 0")
	f.line("            private set")
	f.blank()
	f.line("        internal fun write(data: ByteArray, off: Int, len: Int) {")
	f.line("            if (size + len > buf.size) {")
	f.line("                var n = buf.size * 2")
	f.line("                while (n < size + len) n *= 2")
	f.line("                buf = buf.copyOf(n)")
	f.line("            }")
	f.line("            data.copyInto(buf, size, off, off + len)")
	f.line("            size += len")
	f.line("        }")
	f.blank()
	f.line("        internal fun reset() { size = 0 }")
	f.blank()
	f.line("        internal fun toByteArray(): ByteArray = buf.copyOf(size)")
	f.line("    }")
	f.blank()

	f.line("    /**")
	f.line("     * Enlarge a native array's backing store to hold index [i], doubling but")
	f.line("     * never exceeding [cap] -- the DECLARED element count -- so a valid array")
	f.line("     * ends exactly right-sized.")
	f.line("     *")
	f.line("     * Growth tracks elements actually delivered, so a malformed message")
	f.line("     * claiming ~2^31 elements cannot force an up-front allocation: the wire")
	f.line("     * count is the sender's claim, and only a schema bound is trusted.")
	f.line("     */")
	for _, t := range primBaseOrder {
		f.line("    internal fun ensureCap%s(a: %s, i: Int, cap: Int): %s {", baseSuffix(t), t, t)
		f.line("        if (i < a.size) return a")
		f.line("        var n = a.size.toLong() * 2")
		f.line("        if (n < i + 1L) n = i + 1L")
		f.line("        if (n > cap.toLong()) n = cap.toLong()")
		f.line("        return a.copyOf(n.toInt())")
		f.line("    }")
	}
	f.blank()

	f.line("    /**")
	f.line("     * Store a fresh row of a matrix (an array whose elements are themselves")
	f.line("     * arrays) at the index its element id names, growing the outer list with")
	f.line("     * EMPTY rows so an id GAP decodes as an empty row instead of shifting every")
	f.line("     * later row down by one.")
	f.line("     *")
	f.line("     * Gaps are ordinary: an interior row equal to the element default (the")
	f.line("     * empty row) is omitted by a conformant encoder, and only")
	f.line("     * the LAST row is guaranteed present -- which is what makes the decoded")
	f.line("     * length, highest present id + 1, exact. The row is REPLACED rather than")
	f.line("     * merged into, because an array wrapper IS the array's value: a later")
	f.line("     * occurrence of its field id replaces it whole. The")
	f.line("     * caller's over-index guard bounds the id against the outer array's schema")
	f.line("     * capacity before this grows anything, and [n] is that caller's capped")
	f.line("     * reservation -- never the wire count.")
	f.line("     */")
	for _, t := range primBaseOrder {
		f.line("    internal fun placeRow%s(l: MutableList<%s>, id: Int, n: Int): %s {", baseSuffix(t), t, t)
		f.line("        val row = %s(n)", t)
		f.line("        while (l.size < id) l.add(EMPTY_%s)", strings.ToUpper(baseSuffix(t)))
		f.line("        if (l.size == id) l.add(row) else l[id] = row")
		f.line("        return row")
		f.line("    }")
	}
	f.blank()

	f.line("    /**")
	f.line("     * [placeRow] for a row that is itself a wrapper array (a list): same")
	f.line("     * id-keyed placement and the same gap fill with the empty row.")
	f.line("     *")
	f.line("     * \"Replaced\" is a statement about the VALUE, not the object: an")
	f.line("     * already-present row is emptied in place rather than swapped for a fresh")
	f.line("     * list, so decoding N rows allocates N lists and not 2N.")
	f.line("     */")
	f.line("    internal fun <T> placeRowList(l: MutableList<MutableList<T>>, id: Int) {")
	f.line("        while (l.size < id) l.add(mutableListOf())")
	f.line("        if (l.size == id) { l.add(mutableListOf()); return }")
	f.line("        l[id].clear()")
	f.line("    }")
	f.blank()

	f.line("    /**")
	f.line("     * A boolean array as the `0`/`1` unsigned array it is on the wire")
	f.line("     * as an unsigned array. The one native element kind with no OStream overload")
	f.line("     * of its own, so it is the one that has to be materialised for the write.")
	f.line("     */")
	f.line("    internal fun boolBytes(v: BooleanArray): ByteArray {")
	f.line("        val a = ByteArray(v.size)")
	f.line("        for (i in v.indices) a[i] = if (v[i]) 1 else 0")
	f.line("        return a")
	f.line("    }")
	f.line("}")
	return f.bytes()
}

var _ = fmt.Sprintf
