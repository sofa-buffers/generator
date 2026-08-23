// Package typescript is the TypeScript throughput backend (PLAN §6.4): it emits
// one class per object with serialize(OStream) and a visitor-based decode against
// corelib-ts (@sofa-buffers/corelib). 64-bit fields use bigint by default; the
// `int64` config key (bigint | long | number) can back 64-bit arrays with
// corelib Long[] and map 64-bit scalars to number for a bigint-free hot path
// (all modes are wire-identical).
package typescript

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for TypeScript.
type Backend struct{}

func (*Backend) Lang() string { return "typescript" }

const corelibPkg = "@sofa-buffers/corelib"

// Generate emits a single message.ts module; project mode adds a harness +
// package.json + tsconfig.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{schema: s, banner: cfgString(cfg, "tool_banner", "sofabgen"), license: generator.LicenseID(cfg), i64rep: cfgInt64Mode(cfg), limits: resolveLimits(s, cfg), size: generator.NewSizePolicy(cfg)}
	// The push decoder's Long channel, decided once for the whole module: a nested
	// scope is driven on the ROOT visitor's channel whatever its own flag says
	// (corelib-ts#146), and a struct's visitor class is shared by every message
	// that embeds it — so this cannot be a per-message choice. See streamLongs for
	// the trade, and longsThreshold for where the line sits and why.
	if g.longScalars() {
		big, narrow := intPositions(s)
		g.longs = big > 0 && narrow <= longsThreshold*big
	}
	files := []generator.File{{Path: "message.ts", Content: g.module(s)}}
	if cfgString(cfg, "emit", "sources") == "project" {
		files = append(files, g.projectFiles(s, cfg)...)
	}
	// After emission, not before: the size policy is applied per message while the
	// module is built, so a violation only exists once g.module has run.
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	banner  string
	license string    // SPDX id, "" to omit the header line
	i64rep  int64Mode // 64-bit representation (config key `int64`)
	// longs: the generated visitors take corelib-ts's opt-in Long channel on the
	// push decoders. Schema-wide, decided in Generate — see streamLongs.
	longs  bool
	limits limitSet // receiver-side decode limits (generator#102)
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
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

// limitSet is the receiver-side decode-limit configuration (generator#102),
// resolved against the schema: each active entry is the configured cap raised
// to the largest schema bound of its kind, so a schema-bounded field larger
// than the cap stays governed by its schema bound alone (the corelib enforces
// these globally per decode). An entry is active only when its key is
// configured AND the schema actually has an unbounded field of that kind —
// otherwise the option would be inert and no plumbing is emitted.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits reads the max_dyn_* config keys and resolves them against the
// schema's bounds (see limitSet).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	var l limitSet
	if v, ok := cfgLimit(cfg, "max_dyn_array_count"); ok && b.HasDynArray {
		l.arrayCount, l.arrayHas = max(v, b.MaxCount), true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_string_len"); ok && b.HasDynString {
		l.stringLen, l.stringHas = max(v, b.MaxStringLen), true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_blob_len"); ok && b.HasDynBlob {
		l.blobLen, l.blobHas = max(v, b.MaxBlobLen), true
	}
	return l
}

// cursorLimits renders the DecodeLimits argument every generated static
// decode() passes to its Cursor ("" when no limit is active). The object
// literal references the exported MAX_DYN_* constants, independent of the
// int64 representation mode.
func (g *gen) cursorLimits() string {
	if !g.limits.any() {
		return ""
	}
	return ", _LIMITS"
}

// limitFields renders the configured caps as DecodeLimits object fields, for the
// single module-level `_LIMITS` the decode entry points share.
func (g *gen) limitFields() []string {
	var parts []string
	if g.limits.arrayHas {
		parts = append(parts, "maxArrayCount: MAX_DYN_ARRAY_COUNT")
	}
	if g.limits.stringHas {
		parts = append(parts, "maxStringLen: MAX_DYN_STRING_LEN")
	}
	if g.limits.blobHas {
		parts = append(parts, "maxBlobLen: MAX_DYN_BLOB_LEN")
	}
	return parts
}

type tsfile struct{ b strings.Builder }

func (f *tsfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *tsfile) blank()        { f.b.WriteByte('\n') }
func (f *tsfile) bytes() []byte { return []byte(f.b.String()) }

func (g *gen) module(s *ir.Schema) []byte {
	f := &tsfile{}
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	use := g.scanHelpers(s)
	// Which wrapper-sequence collectors the schema reaches decides both an import
	// (the corelib's StringSeq / BlobSeq) and an emission (the ones with no corelib
	// counterpart), so it is scanned once here and handed to both.
	stream := g.scanStreamUse(s)
	imports := []string{"OStream", "Cursor"}
	if decodesAnyField(s) {
		// The per-field wire-type guard in the pull decoder (issue #160) references
		// WireType; only classes with at least one decode case use it, so a
		// field-less schema (enums/bitfields only) keeps the import out.
		imports = append(imports, "WireType")
	}
	if schemaHasFixlenGuard(s) {
		// The §7.3 guard on a fixlen-framed field also references FixlenSubtype
		// (fp32/fp64/string/blob and the fp32/fp64 native arrays share one wire
		// type, so only the subtype separates them — corelib-ts#58). A schema with
		// no such field never names it, so keep the import out then.
		imports = append(imports, "FixlenSubtype")
	}
	if use.long {
		imports = append(imports, "Long")
	}
	if g.streamLongs() {
		// The opt-in Long channel's visitor shape (corelib-ts#146): Visitor<Long>
		// plus `longs: true`. Named only when the generated visitors take it.
		imports = append(imports, "LongVisitor")
	}
	if use.countedArr || use.overIdxArr || use.maxlenField || use.narrowInt {
		// The over-count scalar-array reject (generator#100), the over-index
		// wrapper-array reject (generator#142), the over-maxlen string/blob reject
		// and the over-width integer reject (MESSAGE_SPEC §7.1, generator#266) all
		// throw SofabError.
		imports = append(imports, "SofabError", "SofabErrorCode")
	}
	if use.elemEq {
		// The element-wise not-equal-to-default test the sparse-canonical serialize
		// takes before it writes a leaf array (corelib-ts#151).
		imports = append(imports, "elementsEqual")
	}
	if decodesAnyField(s) {
		// The streaming decoder (streamdecode.go): a push Visitor over the
		// corelib's resumable IStream, driven with the corelib's payload
		// accumulator. SofabError/SofabErrorCode come with it too -- finish()
		// reports a truncated stream -- so make sure they are present even for a
		// schema whose cursor path needed no reject.
		imports = append(imports, "Visitor", "IStream", "DecodeStatus", "ArrayKind", "PayloadAcc")
		if g.streamLongs() {
			// The child a sequenceBegin hands back may be a corelib collector, which
			// is a plain Visitor whatever channel the root chose -- see childVis.
			imports = append(imports, "AnyVisitor")
		}
		if schemaHasStringField(s) {
			// A string payload reaches the visitor as raw wire bytes, so the store
			// site transcodes it with the corelib's strict decoder. A string ARRAY
			// needs no import of its own: StringSeq decodes its elements itself.
			imports = append(imports, "decodeUtf8")
		}
		if stream.str {
			imports = append(imports, "StringSeq")
		}
		if stream.blob {
			imports = append(imports, "BlobSeq")
		}
		if !slices.Contains(imports, "SofabError") {
			imports = append(imports, "SofabError", "SofabErrorCode")
		}
	}
	f.line("import { %s } from %q;", strings.Join(imports, ", "), corelibPkg)
	f.blank()
	if g.limits.any() {
		f.line("// Receiver-side decode limits, baked from the sofabgen config")
		f.line("// (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len). They govern")
		f.line("// only fields the schema left unbounded; each cap is raised to the largest")
		f.line("// schema bound of its kind, so a schema-bounded field stays governed by its")
		f.line("// own bound alone. Every static decode() passes them to its Cursor; exceeding")
		f.line("// a cap throws SofabError with code LimitExceeded, before allocation.")
		if g.limits.arrayHas {
			f.line("export const MAX_DYN_ARRAY_COUNT = %d;", g.limits.arrayCount)
		}
		if g.limits.stringHas {
			f.line("export const MAX_DYN_STRING_LEN = %d;", g.limits.stringLen)
		}
		if g.limits.blobHas {
			f.line("export const MAX_DYN_BLOB_LEN = %d;", g.limits.blobLen)
		}
		f.blank()
		// One frozen object, not a literal per decode. The caps are compile-time
		// constants, so a fresh `{ maxArrayCount: … }` at every `decode(bytes)`
		// call site is an allocation with a constant value — on a decode hot path,
		// and paid by every schema that configures a cap at all.
		f.line("// The DecodeLimits every static decode() hands its Cursor. Module-level and")
		f.line("// frozen: the caps above are constants, so this is built once rather than")
		f.line("// re-allocated on every decode.")
		f.line("const _LIMITS = Object.freeze({ %s });", strings.Join(g.limitFields(), ", "))
		f.blank()
	}
	if use.longArrEq {
		f.line("%s", longArrEqHelper)
		f.blank()
	}
	if use.fp32Raw {
		f.line("%s", fp32RawHelper)
		f.blank()
	}
	if use.fp32ArrRaw {
		f.line("%s", fp32ArrayRawHelper)
		f.blank()
	}

	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfield(f, nt)
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			// A struct/union serializes a headerless field RUN, not a message, so it
			// gets no MAX_SIZE and no encode(): bytes handed back from one would not
			// be a message any decoder could read on its own.
			g.emitClass(f, g.typeName(key), nt.Summary, nt.Fields, false)
		}
	}
	for _, m := range s.Messages {
		g.emitClass(f, exported(m.Name), m.Summary, m.Fields, true)
	}

	// The streaming decode surface, after the classes it fills: the shared skip
	// visitor and payload accumulator, the wrapper-sequence collectors, one
	// visitor per object type, and a public Decoder per message.
	if decodesAnyField(s) {
		g.emitStreamPrelude(f, s)
		g.emitStreamCollectors(f, stream)
		for _, key := range s.NamedOrder {
			nt := s.Named[key]
			if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
				g.emitStreamVisitor(f, g.typeName(key), nt.Fields)
			}
		}
		for _, m := range s.Messages {
			g.emitStreamVisitor(f, exported(m.Name), m.Fields)
			g.emitStreamDecoderClass(f, exported(m.Name))
		}
	}
	return f.bytes()
}

// decodesAnyField reports whether any emitted class (message, struct or union)
// has at least one field, i.e. whether the pull decoder emits any switch case and thus
// the WireType-guarded import is needed. Enums and bitfields carry no fields.
func decodesAnyField(s *ir.Schema) bool {
	for _, m := range s.Messages {
		if len(m.Fields) > 0 {
			return true
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if (nt.Category == ir.CatStruct || nt.Category == ir.CatUnion) && len(nt.Fields) > 0 {
			return true
		}
	}
	return false
}

// schemaHasFixlenGuard reports whether any §7.3 guard the module emits references
// FixlenSubtype — a fixlen scalar (fp32/fp64/string/blob) or a native fp32/fp64
// array, the only kinds the wire type alone does not settle. Mirrors the decision
// in tsWireGuardCond AND in tsElemWireGuardCond so the import matches the emitted
// code: guards exist at two levels, and the element-level one was missing
// (generator#246) — an `array<string>` or an `array<array<fp32>>` names
// FixlenSubtype from a wrapper ELEMENT guard while no *field* is fixlen.
func schemaHasFixlenGuard(s *ir.Schema) bool {
	has := func(fields []*ir.Field) bool {
		for _, x := range fields {
			if fieldHasFixlenGuard(x) {
				return true
			}
		}
		return false
	}
	for _, m := range s.Messages {
		if has(m.Fields) {
			return true
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if (nt.Category == ir.CatStruct || nt.Category == ir.CatUnion) && has(nt.Fields) {
			return true
		}
	}
	return false
}

func (g *gen) emitEnum(f *tsfile, nt *ir.NamedType) {
	f.line("export enum %s {", g.typeName(nt.Key))
	for _, c := range nt.Consts {
		f.emitDoc("  ", c.Description)
		f.line("  %s = %d,", exported(c.Name), c.Value)
	}
	f.line("}")
	f.blank()
}

func (g *gen) emitBitfield(f *tsfile, nt *ir.NamedType) {
	f.line("export enum %s {", g.typeName(nt.Key))
	for _, fl := range nt.Flags {
		f.emitDoc("  ", flagDoc(fl))
		f.line("  %s = %d,", exported(fl.Name), uint64(1)<<uint(fl.Pos))
	}
	f.line("}")
	f.blank()
}

// tsScratchSize is the fixed output buffer the unbounded encode arm writes
// through. It is not a limit on the message: the buffer is drained into the
// caller's storage every time it fills, so it only trades sink calls against
// resident bytes. Matches the Go, Rust and Python backends.
const tsScratchSize = 512

func (g *gen) emitClass(f *tsfile, name, summary string, fields []*ir.Field, isMessage bool) {
	f.emitDoc("", summary)
	f.line("export class %s {", name)
	for _, fld := range fields {
		if g.longBacked(fld) {
			g.emitLongAccessor(f, fld)
			continue
		}
		f.emitDoc("  ", fieldDoc(fld, generator.BoundNote(fld, generator.StorageDynamic)))
		f.line("  %s: %s = %s;", fld.Name, g.tsType(fld), g.tsDefault(fld))
		if fp32RawCompanion(fld) {
			// The value slot above is the field's API; this is its wire companion
			// (MESSAGE_SPEC §4.6, generator#235). A JS number is a 64-bit double and
			// cannot carry an fp32 NaN's payload bits, so decode parks the raw wire
			// bytes here whenever the decoded value is a NaN — and only then. marshal
			// re-emits them verbatim for a value that is still that NaN; every other
			// value renders from the number, which is exact for every non-NaN fp32.
			f.emitDoc("  ", fp32RawDoc(fld))
			f.line("  %s: Uint8Array | null = null;", fp32RawName(fld.Name))
		}
	}
	f.blank()

	// marshal
	f.line("  serialize(os: OStream): void {")
	for _, fld := range fields {
		g.emitMarshal(f, fld)
	}
	f.line("  }")
	f.blank()

	if isMessage {
		g.emitEncode(f, name, fields)
	}

	// all-default predicate (the explicit form of marshal's lazy framing)
	g.emitIsDefault(f, fields)

	// JSON
	g.emitJSON(f, name, fields)

	// decode (monomorphic pull cursor): the class carries only the public
	// decode(bytes) entry; its loop follows at module level (see emitDecode).
	g.emitDecode(f, name)
	f.line("}")
	f.blank()
	g.emitDecodeLoop(f, name, fields)
}

// emitEncode emits a message's worst-case size constant and the convenience
// encode entry point (CORELIB_PLAN §6.1.1) built on it.
//
// The output buffer belongs to the CALLER (§5.1): corelib-ts writes into storage
// it is handed and neither allocates nor grows one of its own, so the allocation
// is made here — generated code IS that caller. Which shape it takes is a
// property of the SCHEMA, not of the value, which is why there are two arms;
// sizing a buffer from the ceiling in the unbounded case would refuse a larger
// message the caller legitimately built.
//
// `new OStream()` — the no-argument form — is deliberately not used anywhere:
// corelib-ts deprecates it as an alias for growingOStream(), which allocates a
// slab and doubles it as the message grows, i.e. the corelib owning the storage.
func (g *gen) emitEncode(f *tsfile, name string, fields []*ir.Field) {
	ms := g.messageSize(name, fields)
	if ms.Bounded {
		f.line("  // Worst-case encoded size, derived from the schema: no value of this class")
		f.line("  // can encode to more, which is what lets encode() size one exact buffer.")
		f.line("  static readonly MAX_SIZE = %d;", ms.Size)
		f.blank()
		f.line("  /**")
		f.line("   * Encode into a buffer this call allocates and owns.")
		f.line("   *")
		f.line("   * The buffer is exactly `MAX_SIZE` bytes, the schema's worst case, so every")
		f.line("   * value the schema permits fits. A value filled PAST a declared count/maxlen")
		f.line("   * does not: it throws `SofabError` (BUFFER_FULL) rather than coming back")
		f.line("   * short, because partial output must never pass for a whole message.")
		f.line("   */")
		f.line("  encode(): Uint8Array {")
		f.line("    const _buf = new Uint8Array(%s.MAX_SIZE);", name)
		f.line("    // No flush sink, so nothing can be split and no minimum buffer size applies:")
		f.line("    // a field-less message encodes through a 0-byte buffer.")
		f.line("    const _os = new OStream(_buf);")
		f.line("    this.serialize(_os);")
		f.line("    // A copy, not _os.bytes(): that is a view into this call's scratch, and the")
		f.line("    // returned message has to outlive it.")
		f.line("    return _buf.slice(0, _os.bytesUsed);")
		f.line("  }")
		f.blank()
		return
	}
	f.line("  // Configured ceiling (max_message_size), NOT a size this class cannot exceed:")
	f.line("  // a field of it is unbounded, so the schema supplies no worst case and encode()")
	f.line("  // must not size a buffer from this number.")
	f.line("  static readonly MAX_SIZE_LIMIT = %d;", ms.Size)
	f.line("  static readonly MAX_SIZE = %s.MAX_SIZE_LIMIT;", name)
	f.blank()
	f.line("  /**")
	f.line("   * Encode into storage this call allocates and owns.")
	f.line("   *")
	f.line("   * A field of this class is unbounded, so there is no worst-case size to hand")
	f.line("   * the encoder. It writes through a fixed %d-byte scratch buffer that is copied", tsScratchSize)
	f.line("   * out each time it fills: the message may be any size, and `MAX_SIZE` never")
	f.line("   * bounds it.")
	f.line("   */")
	f.line("  encode(): Uint8Array {")
	f.line("    const _out: Uint8Array[] = [];")
	f.line("    let _n = 0;")
	f.line("    // A COPYING sink: it takes a snapshot and returns without installing a")
	f.line("    // replacement buffer, so the encoder keeps the scratch and resumes at 0.")
	f.line("    const _os = new OStream(new Uint8Array(%d), 0, (_c) => { const _k = _c.slice(); _out.push(_k); _n += _k.length; });", tsScratchSize)
	f.line("    this.serialize(_os);")
	f.line("    _os.flush();")
	f.line("    // One drain is the common case (a message below the scratch size), and that")
	f.line("    // chunk is already an owned copy, so joining it with itself would copy twice.")
	f.line("    if (_out.length === 1) return _out[0]!;")
	f.line("    const _all = new Uint8Array(_n);")
	f.line("    let _p = 0;")
	f.line("    for (const _c of _out) { _all.set(_c, _p); _p += _c.length; }")
	f.line("    return _all;")
	f.line("  }")
	f.blank()
}

// emitLongAccessor declares a Long-backed 64-bit field — an array (Long[]) or,
// under int64: long, a scalar (Long): a private backing field the hot paths
// read/write directly, plus a get/set accessor pair keeping the public surface
// ergonomic — assignment accepts Long | bigint | number and converts ONCE, off
// the per-encode path. In-place mutation of an array (msg.x.push(v)) operates on
// the Long[] itself, so v must be a Long.
func (g *gen) emitLongAccessor(f *tsfile, fld *ir.Field) {
	t := g.tsType(fld)
	f.line("  private _%s: %s = %s;", fld.Name, t, g.tsDefault(fld))
	f.emitDoc("  ", fieldDoc(fld, generator.BoundNote(fld, generator.StorageDynamic)))
	f.line("  get %s(): %s { return this._%s; }", fld.Name, t, fld.Name)
	if isBig(fld.Kind) {
		// Scalar: the same shape one level down from the array setter. Long.fromValue
		// returns a Long argument as-is, so assigning a Long costs nothing and
		// assigning a bigint/number converts exactly once, here.
		f.line("  set %s(v: Long | bigint | number) { this._%s = Long.fromValue(v); }", fld.Name, fld.Name)
		return
	}
	f.line("  set %s(vals: %s) { this._%s = %s; }", fld.Name, g.longSetterParam(fld), fld.Name, g.longConvert("vals", fld.Elem, fld.ElemItems, 0))
}

// longSetterParam is the accessor setter's parameter type: element positions
// accept Long | bigint | number at every nesting depth.
func (g *gen) longSetterParam(fld *ir.Field) string {
	t := "readonly (Long | bigint | number)[]"
	for e, it := fld.Elem, fld.ElemItems; e == ir.KindArray; e, it = it.Elem, it.ElemItems {
		t = "readonly (" + t + ")[]"
	}
	return t
}

// longConvert builds the setter's one-time conversion of the accepted mixed
// input to the canonical Long[] backing value, mapping Long.fromValue at the
// element depth (nested arrays map per row).
func (g *gen) longConvert(val string, elem ir.Kind, items *ir.ArrayElem, depth int) string {
	if elem == ir.KindArray {
		v := fmt.Sprintf("_v%d", depth)
		return fmt.Sprintf("%s.map((%s) => %s)", val, v, g.longConvert(v, items.Elem, items.ElemItems, depth+1))
	}
	return val + ".map(Long.fromValue)"
}

// emitIsDefault emits the object's all-default predicate. It is the exact
// negation of what marshal writes: the object equals its declared default iff
// marshal would emit no child at all, evaluated per field and recursively
// (MESSAGE_SPEC §2). It is the explicit form of the predicate lazy framing applies
// implicitly ("not one child was written"), generated from the very same per-field
// expressions the writer uses so the two cannot drift apart. Keep this in lockstep
// with emitMarshal: a predicate that disagrees with the writer either omits a
// field that is on the wire or keeps one that is not.
func (g *gen) emitIsDefault(f *tsfile, fields []*ir.Field) {
	f.line("  // True iff serialize would write no child at all, i.e. this object equals its")
	f.line("  // declared default -- compared per field and recursively, never as a byte image.")
	f.line("  isDefault(): boolean {")
	for _, fld := range fields {
		f.line("    if (!(%s)) return false;", g.fieldIsDefaultExpr(fld))
	}
	f.line("    return true;")
	f.line("  }")
	f.blank()
}

// fieldIsDefaultExpr is the boolean expression "this field equals its default",
// i.e. the negation of emitMarshal's write guard for the same field.
func (g *gen) fieldIsDefaultExpr(fld *ir.Field) string {
	acc := g.storage("this", fld)
	switch fld.Kind {
	case ir.KindU64, ir.KindI64:
		// A Long is an object: `===` would compare identity, so the test is the
		// (low, high) pair, exactly as longArrEq does per element.
		if g.longScalars() {
			return g.longScalarIsDefault(acc, fld)
		}
	case ir.KindBlob:
		if blobHasNonEmptyDefault(fld) {
			return fmt.Sprintf("elementsEqual(%s, %s)", acc, g.tsDefault(fld))
		}
		return fmt.Sprintf("%s.length === 0", acc)
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed: the frame survives iff the nested marshal wrote a child,
		// which is exactly "the nested object is not default".
		return fmt.Sprintf("%s.isDefault()", acc)
	case ir.KindArray:
		return g.arrayIsDefaultExpr(fld, acc)
	}
	return fmt.Sprintf("%s === %s", acc, g.tsDefault(fld))
}

// arrayIsDefaultExpr mirrors emitMarshalArray. An array's declared `count: N` is a
// CAPACITY, never a length (MESSAGE_SPEC §3), so it takes no part in this test: the
// value is compared against the declared default exactly as written, with no
// padding to N on either side, and against the empty collection when none is
// declared. A count:N array is therefore default only when it is EMPTY — an
// all-zero N-element value is a length-N array, which differs from the empty one
// and stays on the wire.
func (g *gen) arrayIsDefaultExpr(fld *ir.Field, acc string) string {
	if nativeArrayElem(fld.Elem) {
		if def, ok := g.nativeArrayDefault(fld); ok {
			eq := "elementsEqual"
			if g.longBacked(fld) {
				eq = "longArrEq"
			}
			return fmt.Sprintf("%s(%s, %s)", eq, acc, def)
		}
		return fmt.Sprintf("%s.length === 0", acc)
	}
	// Wrapper array: the writer emits a child for every element it holds, because
	// the LAST element is written whatever its value (§2) — so "no child is written"
	// is exactly "the array is empty", and the two cannot drift apart.
	return fmt.Sprintf("%s.length === 0", acc)
}

func (g *gen) emitMarshal(f *tsfile, fld *ir.Field) {
	acc := g.storage("this", fld)
	rawAcc := ""
	if fp32RawCompanion(fld) {
		rawAcc = g.fp32RawStorage("this", fld)
	}
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		write = fmt.Sprintf("os.writeUnsigned(%d, %s);", fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindEnum:
		write = fmt.Sprintf("os.writeSigned(%d, %s);", fld.ID, acc)
	case ir.KindU64:
		// Under int64: long the value IS a Long, so it goes out through the
		// corelib's bigint-free scalar writer — identical wire, and neither side
		// ever materialises a bigint (corelib-ts#143).
		if g.longScalars() {
			write = fmt.Sprintf("os.writeUnsignedLong(%d, %s);", fld.ID, acc)
		} else {
			write = fmt.Sprintf("os.writeUnsigned(%d, %s);", fld.ID, acc)
		}
	case ir.KindI64:
		if g.longScalars() {
			write = fmt.Sprintf("os.writeSignedLong(%d, %s);", fld.ID, acc)
		} else {
			write = fmt.Sprintf("os.writeSigned(%d, %s);", fld.ID, acc)
		}
	case ir.KindBool:
		write = fmt.Sprintf("os.writeBoolean(%d, %s);", fld.ID, acc)
	case ir.KindFP32:
		// The omission test is untouched: emit iff the value differs from its
		// default (MESSAGE_SPEC §2), decided on the VALUE alone. Carrying wire bytes
		// says nothing about presence — widening the test to "or the raw slot is
		// set" would re-emit an explicit +0.0 that §2 requires omitted.
		//
		// Inside that guard the raw channel is a rendering concern only: the four
		// captured bytes are re-emitted verbatim for a value that is still the NaN
		// it decoded as, because a JS number cannot carry an fp32 NaN's payload
		// (§4.6, generator#235). Every other value — including a NaN the caller set
		// by hand, and any value assigned over a decoded one — renders from the
		// number, which is exact for every non-NaN fp32. There is no scalar
		// writeFp32Raw in corelib-ts by design: writeFixlen with subtype fp32 emits
		// the identical fixlenHead(id, 4, Fp32) + 4 raw bytes (corelib-ts's own doc
		// comment on readFp32Raw prescribes this route).
		f.line("    if (%s !== %s) {", acc, g.tsDefault(fld))
		f.line("      if (Number.isNaN(%s) && %s !== null && %s.length === 4) {", acc, rawAcc, rawAcc)
		f.line("        os.writeFixlen(%d, %s, FixlenSubtype.Fp32);", fld.ID, rawAcc)
		f.line("      } else {")
		f.line("        os.writeFp32(%d, %s);", fld.ID, acc)
		f.line("      }")
		f.line("    }")
		return
	case ir.KindFP64:
		write = fmt.Sprintf("os.writeFp64(%d, %s);", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("os.writeString(%d, %s);", fld.ID, acc)
	case ir.KindBlob:
		// blob is a leaf: omit when equal to its default (empty if none). An empty
		// default tests emptiness directly (no per-encode `new Uint8Array()` to
		// compare against); a non-empty default needs an element-wise elementsEqual.
		if blobHasNonEmptyDefault(fld) {
			f.line("    if (!elementsEqual(%s, %s)) {", acc, g.tsDefault(fld))
		} else {
			f.line("    if (%s.length !== 0) {", acc)
		}
		f.line("      os.writeBlob(%d, %s);", fld.ID, acc)
		f.line("    }")
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so the frame is opened LAZILY -- the corelib holds the header
		// back until a child field appears. The nested marshal omits each child that
		// equals its default, so "no child was written" IS "the object equals its
		// declared default", evaluated per field and recursively. Closing with the
		// dropping end therefore omits an all-default nested object instead of
		// emitting it as an empty wrapper.
		f.line("    os.writeSequenceBeginLazy(%d);", fld.ID)
		f.line("    %s.serialize(os);", acc)
		f.line("    os.writeSequenceEnd();")
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield leaf: always omit when equal to the default;
	// sparse encoding is canonical (MESSAGE_SPEC S2) and the decoder reconstructs
	// the omitted field from its default (materialized at construction).
	if g.longScalars() && isBig(fld.Kind) {
		// A Long-backed scalar compares by its halves, not by `!==` (object
		// identity). Same predicate as isDefault's — one helper, so the writer and
		// the predicate cannot drift apart.
		f.line("    if (!(%s)) {", g.longScalarIsDefault(acc, fld))
		f.line("      %s", write)
		f.line("    }")
		return
	}
	f.line("    if (%s !== %s) {", acc, g.tsDefault(fld))
	f.line("      %s", write)
	f.line("    }")
}

func (g *gen) emitMarshalArray(f *tsfile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized at construction), else when empty. A composite/dynamic-element
	// array is a wrapper sequence, opened lazily and closed with the dropping end
	// at depth 0 (see marshalArray), so it too vanishes when no element is written.
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written — neither side padded to N —
	// and against the empty collection when no default is declared.
	if nativeArrayElem(fld.Elem) {
		if def, ok := g.nativeArrayDefault(fld); ok {
			// Long elements are object identities: compare with the (low, high)
			// word-pair helper instead of elementsEqual's element !==.
			eq := "elementsEqual"
			if g.longBacked(fld) {
				eq = "longArrEq"
			}
			f.line("    if (!%s(%s, %s)) {", eq, acc, def)
		} else {
			f.line("    if (%s.length !== 0) {", acc)
		}
		if fld.Elem == ir.KindFP32 {
			// The array half of the §4.6 raw channel (generator#235). The omission
			// test above is untouched — the raw payload takes no part in deciding
			// presence — and inside it the captured wire bytes only supply the bits a
			// JS number cannot carry: _fp32ArrayRaw re-renders every element from its
			// number except the ones that are still the NaN they decoded as. With no
			// capture (a fresh or hand-built value) the plain writer runs, unchanged.
			raw := g.fp32RawStorage("this", fld)
			f.line("      if (%s !== null) {", raw)
			f.line("        os.writeFp32ArrayRaw(%d, _fp32ArrayRaw(%s, %s));", fld.ID, acc, raw)
			f.line("      } else {")
			g.marshalArray(f, "        ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
			f.line("      }")
			f.line("    }")
			return
		}
		g.marshalArray(f, "      ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
		f.line("    }")
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today (the generated
	// default is the empty collection), so absent and explicitly-empty denote the
	// same value. If that gap is ever closed, this call needs a guard --
	// `if (!eq(value, default)) { ... os.writeSequenceEndKeep(); }` -- so that a
	// value differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC S2, S3).
	g.marshalArray(f, "    ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over the value av.
//
// It is the whole of the positional half of MESSAGE_SPEC §2's element rule. A
// wrapper array carries no length field: its decoded length is *highest present
// id + 1* (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length, and nothing that carries the length may be elided.
// Everything before it may be: an interior element equal to the element default is
// indistinguishable from an absent one, because the decoder restores an absent id
// from that same default. Hence: interior sparse, last always written.
//
// A declared `count: N` changes nothing here. N is a capacity, not a length (§3),
// so it can never restore an elided tail — the same test applies with or without
// one.
func lastElemExpr(iv, av string) string {
	return fmt.Sprintf("%s === %s.length - 1", iv, av)
}

// emitSeqEnd closes the wrapper sequence opened at ind, choosing between the two
// closers the corelib offers. Every sequence is opened LAZILY (the corelib holds
// the header back until a child is written), so the closer alone decides whether a
// contentless one survives: writeSequenceEnd drops it, writeSequenceEndKeep forces
// the empty frame out.
//
// keepIf is the condition under which an empty frame must survive:
//   - "" — never. A sequence-typed FIELD (a struct/union field, an array wrapper):
//     an all-default one is omitted and absence reconstructs it (§2).
//   - a lastElemExpr — a sequence-form array ELEMENT, kept only at the array's
//     last index. In the interior it is dropped and leaves an id GAP, which is
//     what makes an all-default element sparse like any other default value.
//     Note this is decided from the position in the VALUE, at run time; the schema
//     cannot answer it.
func emitSeqEnd(f *tsfile, ind, keepIf string) {
	if keepIf == "" {
		f.line("%sos.writeSequenceEnd();", ind)
		return
	}
	f.line("%sif (%s) {", ind, keepIf)
	f.line("%s  os.writeSequenceEndKeep();", ind)
	f.line("%s} else {", ind)
	f.line("%s  os.writeSequenceEnd();", ind)
	f.line("%s}", ind)
}

// marshalArray writes the array `val` as field `idExpr`. Numeric/enum/boolean/
// bitfield elements use the native array wire type (enum->signed, bool/bitfield->
// unsigned); string/blob/struct/union/array elements lower to a wrapper sequence
// whose child ids are the 0-based index (per MESSAGE_SPEC). Recurses for nested
// arrays.
//
// Every element the value holds is written — no trailing run is elided, of either
// element kind, because the wire count IS the array's length (§3) and the highest
// wrapper id IS its last index (§5.1). What the interior may drop is a value that
// is indistinguishable from absence, and only that.
//
// keepIf is the closer this call's own wrapper takes (see emitSeqEnd); the native
// element kinds open no sequence and ignore it.
func (g *gen) marshalArray(f *tsfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int, keepIf string) {
	ev := fmt.Sprintf("_e%d", depth)
	iv := fmt.Sprintf("_i%d", depth)
	av := fmt.Sprintf("_a%d", depth)
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32:
		f.line("%sos.writeUnsignedArray(%s, %s);", ind, idExpr, val)
	case ir.KindU64:
		if g.longArrays() {
			f.line("%sos.writeUnsignedArrayLong(%s, %s);", ind, idExpr, val)
		} else {
			f.line("%sos.writeUnsignedArray(%s, %s);", ind, idExpr, val)
		}
	case ir.KindI8, ir.KindI16, ir.KindI32:
		f.line("%sos.writeSignedArray(%s, %s);", ind, idExpr, val)
	case ir.KindI64:
		if g.longArrays() {
			f.line("%sos.writeSignedArrayLong(%s, %s);", ind, idExpr, val)
		} else {
			f.line("%sos.writeSignedArray(%s, %s);", ind, idExpr, val)
		}
	case ir.KindEnum:
		f.line("%sos.writeSignedArray(%s, %s);", ind, idExpr, val)
	case ir.KindBool:
		f.line("%sos.writeUnsignedArray(%s, %s.map((%s) => (%s ? 1 : 0)));", ind, idExpr, val, ev, ev)
	case ir.KindBitfield:
		f.line("%sos.writeUnsignedArray(%s, %s);", ind, idExpr, val)
	case ir.KindFP32:
		f.line("%sos.writeFp32Array(%s, %s);", ind, idExpr, val)
	case ir.KindFP64:
		f.line("%sos.writeFp64Array(%s, %s);", ind, idExpr, val)
	case ir.KindString:
		// Leaf sequence: an indexed for (not .forEach) avoids a per-marshal closure
		// allocation and inlines the monomorphic write body. A string element is a
		// leaf keyed by index id: in the array's INTERIOR it is omitted when it equals
		// the element default (empty), leaving an id gap the decoder restores from
		// that same default — the ordinary sparse-field rule of MESSAGE_SPEC §2,
		// applied to an element. At the LAST index it is written whatever its value:
		// see lastElemExpr. The value is bound ONCE in the loop init (scoped to the
		// for statement, so sibling array fields cannot collide).
		f.line("%sos.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (let %s = 0, %s = %s; %s < %s.length; %s++) {", ind, iv, av, val, iv, av, iv)
		f.line("%s  if (%s[%s]! !== \"\" || %s) {", ind, av, iv, lastElemExpr(iv, av))
		f.line("%s    os.writeString(%s, %s[%s]!);", ind, iv, av, iv)
		f.line("%s  }", ind)
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindBlob:
		// A blob element is a leaf, exactly like the string element above.
		f.line("%sos.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%sfor (let %s = 0, %s = %s; %s < %s.length; %s++) {", ind, iv, av, val, iv, av, iv)
		f.line("%s  if (%s[%s]!.length !== 0 || %s) {", ind, av, iv, lastElemExpr(iv, av))
		f.line("%s    os.writeBlob(%s, %s[%s]!);", ind, iv, av, iv)
		f.line("%s  }", ind)
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above —
		// one rule for both kinds — and the lazily-held frame is where it is applied.
		// The nested marshal writes no child exactly when the element equals its
		// declared default, so the CLOSER alone decides: the dropping one in the
		// interior, where an all-default element vanishes into an id gap; the keeping
		// one at the last index, where it survives as an empty frame because that
		// presence is what fixes the array's length.
		f.line("%sos.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%s%s.forEach((%s, %s, %s) => {", ind, val, ev, iv, av)
		f.line("%s  os.writeSequenceBeginLazy(%s);", ind, iv)
		f.line("%s  %s.serialize(os);", ind, ev)
		emitSeqEnd(f, ind+"  ", lastElemExpr(iv, av))
		f.line("%s});", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindArray:
		f.line("%sos.writeSequenceBeginLazy(%s);", ind, idExpr)
		f.line("%s%s.forEach((%s, %s, %s) => {", ind, val, ev, iv, av)
		if nativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its own,
			// so the rule lands on the WRITE rather than on a closer: an interior row
			// equal to the element default (the empty row) is not written at all, and
			// the last row always is.
			f.line("%s  if (%s.length !== 0 || %s) {", ind, ev, lastElemExpr(iv, av))
			g.marshalArray(f, ind+"    ", iv, ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, "")
			f.line("%s  }", ind)
		} else {
			// A wrapper row has its own frame, so it takes the closer instead — the
			// same interior/last choice, expressed the same way as for a struct element
			// above.
			g.marshalArray(f, ind+"  ", iv, ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, lastElemExpr(iv, av))
		}
		f.line("%s});", ind)
		emitSeqEnd(f, ind, keepIf)
	}
}
