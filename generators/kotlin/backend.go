// Package kotlin is the Kotlin Multiplatform throughput backend: classes with
// serialize over OStream and a flat-visitor decode (Visitor) against
// corelib-kotlin-mp. The Visitor is flat (sequenceBegin/end, no child visitors),
// so decode is a (location, id) state machine with a location stack -- the same
// shape Rust, C# and Java use.
//
// The emitted sources are plain `commonMain` Kotlin: the stdlib and `sofab`, no
// JVM API, so one generated file compiles for the JVM, for Node and the browser,
// and for a native binary -- which is the whole reason the corelib is
// multiplatform. Everything JVM-only lives in the `emit: project` scaffolding.
package kotlin

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for Kotlin.
type Backend struct{}

func (*Backend) Lang() string { return "kotlin" }

func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{
		schema:  s,
		pkg:     cfgString(cfg, "package", "message"),
		banner:  cfgString(cfg, "tool_banner", "sofabgen"),
		license: generator.LicenseID(cfg),
		limits:  resolveLimits(s, cfg),
		size:    generator.NewSizePolicy(cfg),
	}
	dir := "src/main/kotlin/" + strings.ReplaceAll(g.pkg, ".", "/") + "/"
	var files []generator.File
	// Every named type gets its OWN file. Kotlin would allow several public
	// declarations per file, but a type reached from two messages must be
	// emitted ONCE or the package does not compile, and one-file-per-type is the
	// simplest spelling of that rule.
	for _, key := range g.namedTypes() {
		nt := s.Named[key]
		files = append(files, generator.File{
			Path:    dir + g.typeName(key) + ".kt",
			Content: g.namedTypeFile(key, nt),
		})
	}
	for _, m := range s.Messages {
		files = append(files, generator.File{Path: dir + exported(m.Name) + ".kt", Content: g.messageFile(m)})
	}
	if cfgString(cfg, "emit", "sources") == "project" {
		files = append(files, g.projectFiles(s, cfg, dir)...)
	}
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	pkg     string
	banner  string
	license string // SPDX id, "" to omit the header line
	limits  limitSet
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
	// tmpN names the wrapper-array loop locals (`_t0`, `_t1`, ...) a serialize
	// body declares. Reset per emitted method, and counted per method rather
	// than per nesting depth because two array FIELDS of one class share a scope
	// and would otherwise collide.
	tmpN int
}

// messageSize resolves a message's worst-case encoded size via the shared walk
// (ir.MaxWireSize), falling back to the configured max_message_size ceiling when
// a field is unbounded. The emit path has no error channel, so a violation of an
// explicitly configured ceiling is recorded here and surfaced by Generate.
func (g *gen) messageSize(name string, fields []*ir.Field) generator.MessageSize {
	ms, err := g.size.Resolve(name, fields)
	if err != nil && g.sizeErr == nil {
		g.sizeErr = err
	}
	return ms
}

// limitSet is the receiver-side decode-limit configuration (generator#102).
// Every cap is always set -- the target carries a finite default that the config
// key only overrides (§9.5, generator#385) -- so an entry is active exactly when
// the schema actually has an unbounded field of that kind; otherwise the cap
// would be inert and no limit plumbing is emitted. The Kotlin visitor guards each unbounded field
// individually (like Rust/Java/C#), so the configured value is emitted as-is;
// schema-bounded fields never see it and keep their own §7.1 guard.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	d := generator.ClientDynLimits.Resolve(cfg)
	var l limitSet
	if b.HasDynArray {
		l.arrayCount, l.arrayHas = d.ArrayCount, true
	}
	if b.HasDynString {
		l.stringLen, l.stringHas = d.StringLen, true
	}
	if b.HasDynBlob {
		l.blobLen, l.blobHas = d.BlobLen, true
	}
	return l
}

// ---------------------------------------------------------------------------
// File scaffolding
// ---------------------------------------------------------------------------

type kfile struct{ b strings.Builder }

func (f *kfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *kfile) blank()        { f.b.WriteByte('\n') }
func (f *kfile) bytes() []byte { return []byte(f.b.String()) }

// kdoc writes a KDoc comment for text at the given indent, or nothing when text
// is empty. Any `*/` in the text is neutralised to `* /` so it can never close
// the comment early; UTF-8 passes through byte-for-byte.
func (f *kfile) kdoc(indent, text string) {
	if text == "" {
		return
	}
	text = strings.ReplaceAll(text, "*/", "* /")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 {
		f.line("%s/** %s */", indent, lines[0])
		return
	}
	f.line("%s/**", indent)
	for _, ln := range lines {
		if ln == "" {
			f.line("%s *", indent)
		} else {
			f.line("%s * %s", indent, ln)
		}
	}
	f.line("%s */", indent)
}

// header writes the @generated banner, an optional SPDX line, and the file-level
// annotations every generated Kotlin file carries.
//
//   - `ExperimentalUnsignedTypes` -- the unsigned ARRAY types are still opt-in
//     even though the unsigned scalars are stable, and mapping `u8[]` to
//     anything but `UByteArray` would give up the exact width this target maps
//     integers at.
//   - `DEPRECATION` -- generated encode/decode still touches a `deprecated:`
//     field, which is the point of it still being on the wire; the marker is for
//     the CALLER, so the self-use warning is suppressed here rather than
//     dropping the annotation (ARCHITECTURE §8).
func (g *gen) header(f *kfile) {
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	f.line("@file:OptIn(ExperimentalUnsignedTypes::class)")
	f.line("@file:Suppress(\"DEPRECATION\", \"RedundantVisibilityModifier\", \"unused\", \"UNUSED_PARAMETER\", \"RemoveRedundantCallsOfConversionMethods\", \"KotlinConstantConditions\")")
	f.blank()
	f.line("package %s", g.pkg)
	f.blank()
	f.line("import org.sofabuffers.sofab.*")
	f.blank()
}

// fieldDoc is the KDoc body for a field: its description, with a unit suffix
// appended (or used alone), the schema-bound note, and a deprecation line.
func fieldDoc(fld *ir.Field, note string) string {
	d := fld.Description
	if fld.Unit != "" {
		if d == "" {
			d = "(unit: " + fld.Unit + ")"
		} else {
			d += " (unit: " + fld.Unit + ")"
		}
	}
	d = generator.AppendDoc(d, note)
	if fld.Deprecated {
		const tag = "Deprecated: this field is deprecated and may be removed in a future version."
		if d == "" {
			d = tag
		} else {
			d += "\n\n" + tag
		}
	}
	return d
}

// namedTypes lists every struct/union/enum/bitfield the schema's messages reach,
// deduplicated and in a stable order: each message's reachable set in message
// order, which keeps a type ahead of whatever refers to it.
func (g *gen) namedTypes() []string {
	var order []string
	seen := map[string]bool{}
	for _, m := range g.schema.Messages {
		for _, key := range g.reachable(m) {
			if !seen[key] {
				seen[key] = true
				order = append(order, key)
			}
		}
	}
	return order
}

// namedTypeFile emits <Type>.kt: a class for a struct/union, or an object of
// named constants for an enum/bitfield.
//
// The constants exist because ARCHITECTURE §8 asks every backend to render enum
// constant and bitfield flag `description` on the symbol it generates -- and a
// target that lowers the field to a bare integer has no such symbol. Kotlin can
// have both: the FIELD stays an integer (so an unknown wire value survives
// decode, which an `enum class` could not represent), and the declared members
// are named constants beside it, each carrying its documentation.
func (g *gen) namedTypeFile(key string, nt *ir.NamedType) []byte {
	f := &kfile{}
	g.header(f)
	switch nt.Category {
	case ir.CatEnum:
		g.emitEnumConsts(f, g.typeName(key), nt)
	case ir.CatBitfield:
		g.emitBitfieldConsts(f, g.typeName(key), nt)
	default:
		g.emitClass(f, g.typeName(key), nt.Fields, nt.Summary, false)
	}
	return f.bytes()
}

func (g *gen) emitEnumConsts(f *kfile, name string, nt *ir.NamedType) {
	summary := nt.Summary
	if summary == "" {
		summary = "Declared values of the `" + nt.Name + "` enum."
	}
	f.kdoc("", summary+"\n\nThe field itself is an `Int`: an enum is a SIGNED 32-bit value on the\nwire, and a value outside this set still has to survive a decode, which a\nclosed `enum class` could not represent.")
	f.line("public object %s {", name)
	for _, c := range nt.Consts {
		f.kdoc("    ", c.Description)
		f.line("    public const val %s: Int = %d", enumConstName(c.Name), c.Value)
	}
	if len(nt.Consts) == 0 {
		f.line("    // no declared constants")
	}
	f.line("}")
	f.blank()
}

func (g *gen) emitBitfieldConsts(f *kfile, name string, nt *ir.NamedType) {
	summary := nt.Summary
	if summary == "" {
		summary = "Flag masks of the `" + nt.Name + "` bitfield."
	}
	f.kdoc("", summary+"\n\nThe field itself is a `ULong`: flag positions run 0..63 and the wire word\nis an unsigned varint, so `ULong` is the whole domain and nothing a peer\ncan send is lost.")
	f.line("public object %s {", name)
	for _, fl := range nt.Flags {
		doc := fl.Description
		if fl.HasDefault {
			note := fmt.Sprintf("(default: %t)", fl.Default)
			if doc == "" {
				doc = note
			} else {
				doc += " " + note
			}
		}
		f.kdoc("    ", doc)
		f.line("    public const val %s: ULong = %duL", enumConstName(fl.Name), uint64(1)<<uint(fl.Pos))
	}
	if len(nt.Flags) == 0 {
		f.line("    // no declared flags")
	}
	f.line("}")
	f.blank()
}

// enumConstName renders a schema constant name as a Kotlin constant identifier.
// Schema names already match `[A-Za-z][A-Za-z0-9_]*`, so only a hard-keyword
// collision needs the backtick escape.
func enumConstName(name string) string {
	if ktHardKeywords[name] {
		return "`" + name + "`"
	}
	return name
}

// messageFile emits <Message>.kt: the message class and the decode visitor that
// fills it. The named types it refers to are their own files.
func (g *gen) messageFile(m *ir.Message) []byte {
	f := &kfile{}
	g.header(f)
	g.emitClass(f, exported(m.Name), m.Fields, m.Summary, true)
	return f.bytes()
}

func (g *gen) emitClass(f *kfile, name string, fields []*ir.Field, summary string, isMessage bool) {
	f.kdoc("", summary)
	f.line("public class %s {", name)
	for _, fld := range fields {
		f.kdoc("    ", fieldDoc(fld, generator.BoundNote(fld, generator.StorageDynamic)))
		if fld.Deprecated {
			f.line("    @Deprecated(\"This field is deprecated and may be removed in a future version.\")")
		}
		f.line("    public var %s: %s = %s", ktIdent(fld.Name), g.ktType(fld), g.ktDefaultValue(fld))
	}
	f.blank()

	// serialize
	g.tmpN = 0
	f.line("    /** Write this object's fields into [os]. Streaming out: nothing is flushed -- see [encodeTo]. */")
	f.line("    public fun serialize(os: OStream) {")
	for _, fld := range fields {
		g.emitMarshal(f, fld)
	}
	f.line("    }")
	f.blank()

	g.emitIsDefault(f, fields)
	f.blank()
	g.emitReset(f, fields)

	if isMessage {
		f.blank()
		g.emitMessageAPI(f, name, fields)
	}
	// Hoisted omit-compare defaults. `serialize` only ever READS them, so one
	// shared instance per field suffices and encode stops rebuilding the literal
	// on every call; the mutable per-object initializer above stays its own
	// allocation.
	var hoisted []*ir.Field
	for _, fld := range fields {
		if _, ok := g.arrayCompareDefault(fld); ok {
			hoisted = append(hoisted, fld)
		}
	}
	if len(hoisted) > 0 && !isMessage {
		f.blank()
		f.line("    internal companion object {")
		for _, fld := range hoisted {
			def, _ := g.arrayCompareDefault(fld)
			f.line("        internal val %s: %s = %s", arrDefName(fld), g.ktType(fld), def)
		}
		f.line("    }")
	}
	f.line("}")
	f.blank()

	if isMessage {
		g.emitVisitor(f, name, fields)
	}
}

// emitMessageAPI writes the closed public entry-point set of CORELIB_PLAN §6.1.1
// -- encode / encodeTo / decode / tryDecode / decoder -- plus the incremental
// Decoder and the companion holding the statics.
func (g *gen) emitMessageAPI(f *kfile, name string, fields []*ir.Field) {
	ms := g.messageSize(name, fields)

	// encode(): the one-shot convenience, and the one place generated code owns
	// an output buffer (CORELIB_PLAN §5.1 -- the corelib allocates nothing).
	if ms.Bounded {
		f.line("    /**")
		f.line("     * The complete message as bytes.")
		f.line("     *")
		f.line("     * The schema bounds this message, so one exactly-sized buffer holds it")
		f.line("     * and no flush can occur: a value filled past its own declared bound")
		f.line("     * does not fit and is REPORTED (buffer-full) rather than emitted short.")
		f.line("     */")
		f.line("    public fun encode(): ByteArray {")
		f.line("        val buf = ByteArray(MAX_SIZE)")
		f.line("        val os = OStream(buf)")
		f.line("        serialize(os)")
		f.line("        return buf.copyOf(os.bytesUsed)")
		f.line("    }")
	} else {
		f.line("    /**")
		f.line("     * The complete message as bytes.")
		f.line("     *")
		f.line("     * A field of this message is schema-unbounded, so [MAX_SIZE] is an")
		f.line("     * imposed ceiling rather than a worst case and must not size a buffer:")
		f.line("     * a larger message is legal and would be silently refused. What is")
		f.line("     * used instead is a fixed scratch drained by a flush sink, so memory")
		f.line("     * is bounded by the scratch and not by the message.")
		f.line("     */")
		f.line("    public fun encode(): ByteArray {")
		f.line("        val out = PayloadAcc()")
		f.line("        val os = OStream(ByteArray(ENC_SCRATCH), 0, out)")
		f.line("        serialize(os)")
		f.line("        os.flush()")
		f.line("        return out.toByteArray()")
		f.line("    }")
	}
	f.blank()
	// serialize vs encodeTo: `serialize` writes this object's fields and nothing
	// else, so a nested message can be written into a frame its parent already
	// opened. `encodeTo` is the entry point for a caller who owns the stream: it
	// serialises AND flushes the tail the last write left in the buffer.
	f.line("    /**")
	f.line("     * Encode into a stream the caller owns, then flush the tail.")
	f.line("     *")
	f.line("     * With a [FlushSink] on [os] the buffer may be smaller than the message:")
	f.line("     * it is drained as it fills, so what bounds memory is the buffer rather")
	f.line("     * than the message.")
	f.line("     */")
	f.line("    public fun encodeTo(os: OStream) {")
	f.line("        serialize(os)")
	f.line("        os.flush()")
	f.line("    }")
	f.blank()

	g.emitDecoder(f, name)
	f.blank()

	f.line("    public companion object {")
	if !ms.Bounded {
		f.line("        /** Configured ceiling (max_message_size): an unbounded field means this size is imposed, not derived from the schema. */")
		f.line("        public const val MAX_SIZE_LIMIT = %d", ms.Size)
		f.line("        public const val MAX_SIZE = MAX_SIZE_LIMIT")
		f.line("        /** Scratch window the unbounded encode drains through; any size at or above Sofab.MIN_OUTPUT_BUFFER yields identical bytes. */")
		f.line("        private const val ENC_SCRATCH = 512")
	} else {
		f.line("        /** Worst-case encoded size of this message, derived from the schema. */")
		f.line("        public const val MAX_SIZE = %d", ms.Size)
	}
	// Hoisted omit-compare defaults live here for a message class, where the
	// companion exists anyway.
	for _, fld := range fields {
		if def, ok := g.arrayCompareDefault(fld); ok {
			f.line("        internal val %s: %s = %s", arrDefName(fld), g.ktType(fld), def)
		}
	}
	f.blank()
	// decode: the one-shot convenience. Kotlin is an exception language, so this
	// "fails in the language's own way" for BOTH non-COMPLETE outcomes -- feed
	// throws SofabException(INVALID_MSG) for malformed bytes, and a terminal
	// INCOMPLETE is raised here. This target has no back-compat surface to
	// preserve (the zig precedent), and returning a half-filled object from a
	// truncated message is exactly the verdict MESSAGE_SPEC §7 says generated
	// code must not hide. Use [tryDecode] to take the status instead.
	f.line("        /**")
	f.line("         * Build a %s from a COMPLETE message.", name)
	f.line("         *")
	f.line("         * @throws SofabException the bytes are malformed ([SofabError.INVALID_MSG]).")
	f.line("         * @throws IllegalStateException the bytes end inside a field or an open")
	f.line("         *   sequence. That is not a malformed message -- nothing is wrong with")
	f.line("         *   the bytes -- so it is deliberately a different exception; use")
	f.line("         *   [tryDecode] to receive the status instead of an exception.")
	f.line("         */")
	f.line("        public fun decode(data: ByteArray): %s {", name)
	f.line("            val m = %s()", name)
	f.line("            val ist = IStream()")
	f.line("            ist.feed(data, %sVisitor(m))", name)
	f.line("            check(ist.status == DecodeStatus.COMPLETE) { \"%s: stream ended mid-field (\" + ist.status + \")\" }", name)
	f.line("            return m")
	f.line("        }")
	f.blank()
	// tryDecode: `out` is caller-supplied and may carry a previous decode.
	// Absence is the encoding of an all-default field (MESSAGE_SPEC §2), and an
	// absent field fires no callback at all, so the destination is re-armed HERE
	// -- the last point at which absence is still observable.
	f.line("        /**")
	f.line("         * Decode into [out] and return the corelib's terminal status.")
	f.line("         *")
	f.line("         * [out] is reset first: absence IS the encoding of an all-default field")
	f.line("         * and fires no callback, so a reused destination has to be re-armed")
	f.line("         * before the feed rather than during it -- the last point at which")
	f.line("         * absence is still observable.")
	f.line("         *")
	f.line("         * @throws SofabException the bytes are malformed ([SofabError.INVALID_MSG]).")
	f.line("         */")
	f.line("        public fun tryDecode(data: ByteArray, out: %s): DecodeStatus {", name)
	f.line("            out.reset()")
	f.line("            val ist = IStream()")
	f.line("            ist.feed(data, %sVisitor(out))", name)
	f.line("            return ist.status")
	f.line("        }")
	f.blank()
	f.line("        /**")
	f.line("         * An incremental decoder for this message: hold it and feed chunks as")
	f.line("         * they arrive, instead of buffering the whole message first.")
	f.line("         */")
	f.line("        public fun decoder(): Decoder = Decoder()")
	f.line("    }")
}

// emitDecoder writes the public incremental decoder: a handle on the corelib's
// resumable IStream plus the destination it fills. The corelib already suspends
// and resumes at any byte boundary, so this class carries no parse state of its
// own -- it exists to make that reachable from outside, which an internal
// Visitor was not.
func (g *gen) emitDecoder(f *kfile, name string) {
	f.line("    /**")
	f.line("     * Incremental decoder for [%s]: hold one and feed the message as bytes", name)
	f.line("     * arrive.")
	f.line("     *")
	f.line("     * The wire format has no end marker at the top level -- a message ends")
	f.line("     * where its bytes end -- so a feed cannot report that the MESSAGE is")
	f.line("     * complete, only that the bytes handed in ended on a field boundary")
	f.line("     * (COMPLETE) or mid-field (INCOMPLETE). Neither is a failure mid-stream;")
	f.line("     * the caller's own framing decides when the input is over, and [finish]")
	f.line("     * then gives the verdict for the message as a whole.")
	f.line("     */")
	f.line("    public class Decoder {")
	f.line("        private val m = %s()", name)
	f.line("        private val ist = IStream()")
	f.line("        private val v = %sVisitor(m)", name)
	f.blank()
	f.line("        /**")
	f.line("         * Feed the next chunk, of any size.")
	f.line("         *")
	f.line("         * @throws SofabException the bytes are malformed (INVALID); terminal.")
	f.line("         */")
	f.line("        public fun feed(chunk: ByteArray): DecodeStatus {")
	f.line("            ist.feed(chunk, v)")
	f.line("            return ist.status")
	f.line("        }")
	f.blank()
	f.line("        /** As [feed], over a slice of [chunk]. */")
	f.line("        public fun feed(chunk: ByteArray, off: Int, len: Int): DecodeStatus {")
	f.line("            ist.feed(chunk, off, len, v)")
	f.line("            return ist.status")
	f.line("        }")
	f.blank()
	f.line("        /** The outcome for everything fed so far, without feeding more. */")
	f.line("        public val status: DecodeStatus get() = ist.status")
	f.blank()
	f.line("        /** The destination, holding whatever has been decoded so far. */")
	f.line("        public val message: %s get() = m", name)
	f.blank()
	f.line("        /**")
	f.line("         * Take the decoded message once the caller's framing says the input is")
	f.line("         * over. Rejects a stream that ended mid-field rather than returning a")
	f.line("         * half-filled value; read [message] to take it anyway.")
	f.line("         *")
	f.line("         * @throws IllegalStateException the message ended inside a field or an")
	f.line("         *   open sequence -- which is not a malformed message, so it is not a")
	f.line("         *   SofabException.")
	f.line("         */")
	f.line("        public fun finish(): %s {", name)
	f.line("            check(ist.status == DecodeStatus.COMPLETE) { \"%s: stream ended mid-field (\" + ist.status + \")\" }", name)
	f.line("            return m")
	f.line("        }")
	f.line("    }")
}

// emitIsDefault writes `isDefault()`: the object's all-default predicate, and the
// exact negation of what serialize writes -- the object is default iff serialize
// would emit no child at all, evaluated per field and recursively, never as a
// byte image (MESSAGE_SPEC §2).
//
// Keep this in lockstep with emitMarshal: every arm is the negation of that
// field's write guard, built from the very same expression, so the two cannot
// state different truth tables.
func (g *gen) emitIsDefault(f *kfile, fields []*ir.Field) {
	f.line("    /** True when every field still equals its declared default, compared per field and recursively -- i.e. serialize would write nothing at all. */")
	f.line("    internal fun isDefault(): Boolean {")
	for _, fld := range fields {
		f.line("        if (%s) return false", g.ktWritesExpr(fld))
	}
	f.line("        return true")
	f.line("    }")
}

// emitReset writes `reset()`: every field back to its declared default, in place.
//
// It exists because MESSAGE_SPEC §2 made ABSENCE the encoding of an all-default
// field. The visitor's §7.4 clear (a wrapper array is replaced whole by a later
// occurrence) hangs off sequenceBegin/arrayBegin -- a callback an omitted field
// never fires -- so decoding into a REUSED destination would leave the previous
// decode's elements standing. Absence is only observable before the feed starts,
// so the reset has to happen there.
func (g *gen) emitReset(f *kfile, fields []*ir.Field) {
	f.line("    /** Restore every field to its declared default, in place; call before reusing an instance as a decode destination. */")
	f.line("    public fun reset() {")
	for _, fld := range fields {
		g.emitResetField(f, fld)
	}
	if len(fields) == 0 {
		f.line("        // no fields")
	}
	f.line("    }")
}

func (g *gen) emitResetField(f *kfile, fld *ir.Field) {
	acc := "this." + ktIdent(fld.Name)
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		// Recurse rather than re-allocate.
		f.line("        %s.reset()", acc)
	case ir.KindArray:
		if nativeArrayElem(fld.Elem) {
			if _, ok := g.arrayCompareDefault(fld); ok {
				f.line("        %s = %s.copyOf()", acc, arrDefName(fld))
				return
			}
			f.line("        %s = %s", acc, emptyArrayExpr(primArrayType(fld.Elem)))
			return
		}
		// A wrapper array's declared per-element default is not materialised, so
		// the element defaults ARE its default -- the same rule the dropping
		// closer relies on. Cleared in place, keeping its capacity.
		f.line("        %s.clear()", acc)
	case ir.KindBlob:
		// The hoisted constant is READ-ONLY (serialize compares against it), and
		// the field is mutable, so the copy is not an optimisation -- it is what
		// keeps a caller's write from reaching the shared default.
		if _, ok := g.arrayCompareDefault(fld); ok {
			f.line("        %s = %s.copyOf()", acc, arrDefName(fld))
			return
		}
		f.line("        %s = %s", acc, g.ktDefaultValue(fld))
	default:
		f.line("        %s = %s", acc, g.ktDefaultValue(fld))
	}
}

// ---------------------------------------------------------------------------
// serialize
// ---------------------------------------------------------------------------

func (g *gen) emitMarshal(f *kfile, fld *ir.Field) {
	acc := "this." + ktIdent(fld.Name)
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		write = fmt.Sprintf("os.writeUnsigned(%d, %s)", fld.ID, toWireUnsigned(fld.Kind, acc))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		write = fmt.Sprintf("os.writeSigned(%d, %s)", fld.ID, toWireSigned(fld.Kind, acc))
	case ir.KindBool:
		write = fmt.Sprintf("os.writeBoolean(%d, %s)", fld.ID, acc)
	case ir.KindFP32:
		write = fmt.Sprintf("os.writeFp32(%d, %s)", fld.ID, acc)
	case ir.KindFP64:
		write = fmt.Sprintf("os.writeFp64(%d, %s)", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("os.writeString(%d, %s)", fld.ID, acc)
	case ir.KindBlob:
		write = fmt.Sprintf("os.writeBlob(%d, %s)", fld.ID, acc)
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC §2: the != default test is per field and a sequence-typed
		// field is no exception, so the frame is opened LAZILY -- the corelib
		// holds the header back until a child field actually appears. The nested
		// serialize omits every child that equals its default, so "no child was
		// written" IS "the object equals its declared default", evaluated per
		// field and recursively, with no byte image ever compared.
		f.line("        os.writeSequenceBeginLazy(%d); %s.serialize(os); os.writeSequenceEnd()", fld.ID, acc)
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Leaf: always omit when equal to the default; sparse encoding is canonical
	// (MESSAGE_SPEC §2) and the decoder reconstructs the omitted field from its
	// declared default.
	f.line("        if (%s) %s", g.ktWritesExpr(fld), write)
}

func (g *gen) emitMarshalArray(f *kfile, fld *ir.Field, acc string) {
	// A native array is a leaf field: omit it when equal to its default. A
	// composite/dynamic-element array is a wrapper sequence: opened lazily and
	// closed with the dropping end at field level, so an empty one is omitted
	// rather than framed empty (MESSAGE_SPEC §2).
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written -- neither side padded to
	// N -- and against the empty array when no default is declared.
	if nativeArrayElem(fld.Elem) {
		f.line("        if (%s) {", g.ktWritesExpr(fld))
		f.line("            %s", arrayWriteCall(fld.Elem, itoa64(fld.ID), acc))
		f.line("        }")
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialised (the generated
	// member is the empty list), so absent and explicitly-empty denote the same
	// value.
	g.marshalArray(f, "        ", itoa64(fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over the list lv.
//
// It is the whole of the positional half of MESSAGE_SPEC §2's element rule. A
// wrapper array carries no length field: its decoded length is *highest present
// id + 1* (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length, and nothing that carries the length may be
// elided. Everything before it may be: an interior element equal to the element
// default is indistinguishable from an absent one, because the decoder restores
// an absent id from that same default.
//
// A declared `count: N` changes nothing here. N is a capacity, not a length
// (§3), so it can never restore an elided tail -- the same test applies with or
// without one, and it is read off the position in the VALUE at run time.
func lastElemExpr(iv, lv string) string {
	return fmt.Sprintf("%s == %s.size - 1", iv, lv)
}

// seqEndStmt closes a lazily-opened sequence, choosing between the two closers
// the corelib offers. Every sequence is opened LAZILY (the corelib holds the
// header back until a child is written), so the closer alone decides whether a
// contentless one survives: writeSequenceEnd drops it, writeSequenceEndKeep
// forces the empty frame out.
//
// keepIf is the condition under which an empty frame must survive:
//   - "" -- never. A sequence-typed FIELD (a struct/union field, an array
//     wrapper): an all-default one is omitted and absence reconstructs it (§2).
//   - a lastElemExpr -- a sequence-form array ELEMENT, kept only at the array's
//     last index. In the interior it is dropped and leaves an id GAP, which is
//     what makes an all-default element sparse like any other default value.
func seqEndStmt(keepIf string) string {
	if keepIf == "" {
		return "os.writeSequenceEnd()"
	}
	return fmt.Sprintf("if (%s) os.writeSequenceEndKeep() else os.writeSequenceEnd()", keepIf)
}

// elemLoopList declares the local the serialize element loop runs over and
// returns its name. The local exists so the last-element test measures the very
// list the loop indexes, and so a nested loop cannot re-evaluate an accessor.
func (g *gen) elemLoopList(f *kfile, ind, val, typ string) string {
	name := fmt.Sprintf("_t%d", g.tmpN)
	g.tmpN++
	f.line("%sval %s: %s = %s", ind, name, typ, val)
	return name
}

// marshalArray writes the array val as field idExpr. Numeric/enum/boolean/
// bitfield elements use the native array wire type; string/blob/struct/union/
// array elements lower to a wrapper sequence whose child ids are the 0-based
// index (MESSAGE_SPEC §5.1). Recurses for nested arrays, depth-suffixing loop
// vars to avoid collisions.
//
// Every element the value holds is written -- no trailing run is elided, of
// either element kind, because the wire count IS the array's length
// (MESSAGE_SPEC §3) and the highest wrapper id IS its last index (§5.1). What
// the interior may drop is a value that is indistinguishable from absence, and
// only that.
//
// keepIf is the closer this call's own wrapper takes (see seqEndStmt); the
// native element kinds open no sequence and ignore it.
func (g *gen) marshalArray(f *kfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int, keepIf string) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	if nativeArrayElem(elem) {
		f.line("%s%s", ind, arrayWriteCall(elem, idExpr, val))
		return
	}
	lv := g.elemLoopList(f, ind, val, "MutableList<"+g.ktArrayElemType(elem, ref, items)+">")
	f.line("%sos.writeSequenceBeginLazy(%s)", ind, idExpr)
	switch elem {
	case ir.KindString:
		// A string element is a leaf: in the array's INTERIOR it is omitted when
		// it equals the element default (empty), leaving an id gap the decoder
		// restores from that same default -- the ordinary sparse-field rule of
		// MESSAGE_SPEC §2 applied to an element. At the LAST index it is written
		// whatever its value.
		f.line("%sfor (%s in 0 until %s.size) { val %s = %s[%s]; if (%s.isNotEmpty() || %s) os.writeString(%s, %s) }",
			ind, iv, lv, ev, lv, iv, ev, lastElemExpr(iv, lv), iv, ev)
	case ir.KindBlob:
		f.line("%sfor (%s in 0 until %s.size) { val %s = %s[%s]; if (%s.isNotEmpty() || %s) os.writeBlob(%s, %s) }",
			ind, iv, lv, ev, lv, iv, ev, lastElemExpr(iv, lv), iv, ev)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above
		// -- one rule for both kinds -- and the lazily-held frame is where it is
		// applied. The nested serialize writes no child exactly when the element
		// equals its declared default, so the CLOSER alone decides: the dropping
		// one in the interior, where an all-default element vanishes into an id
		// gap; the keeping one at the last index, where it survives as an empty
		// frame because that presence is what fixes the array's length.
		f.line("%sfor (%s in 0 until %s.size) { os.writeSequenceBeginLazy(%s); %s[%s].serialize(os); %s }",
			ind, iv, lv, iv, lv, iv, seqEndStmt(lastElemExpr(iv, lv)))
	case ir.KindArray:
		f.line("%sfor (%s in 0 until %s.size) {", ind, iv, lv)
		f.line("%s    val %s = %s[%s]", ind, ev, lv, iv)
		if nativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its
			// own, so the rule lands on the WRITE rather than on a closer: an
			// interior row equal to the element default (the empty row) is not
			// written at all, and the last row always is.
			f.line("%s    if (%s.isNotEmpty() || %s) {", ind, ev, lastElemExpr(iv, lv))
			g.marshalArray(f, ind+"        ", iv, ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, "")
			f.line("%s    }", ind)
		} else {
			// A wrapper row has its own frame, so it takes the closer instead --
			// the same interior/last choice, expressed the same way as for a
			// struct element above.
			g.marshalArray(f, ind+"    ", iv, ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, lastElemExpr(iv, lv))
		}
		f.line("%s}", ind)
	}
	f.line("%s%s", ind, seqEndStmt(keepIf))
}
