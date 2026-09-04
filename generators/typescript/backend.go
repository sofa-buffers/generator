// Package typescript is the TypeScript throughput backend (PLAN §6.4): it emits
// one class per object with serialize(OStream) and a visitor-based decode against
// corelib-ts (@sofa-buffers/corelib). 64-bit fields use bigint by default; the
// `int64` config key (bigint | long | number) can back 64-bit arrays with
// corelib Long[] and map 64-bit scalars to number for a bigint-free hot path
// (all modes are wire-identical).
package typescript

import (
	"fmt"
	"strconv"
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
	limits  limitSet  // receiver-side decode limits (generator#102)
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

// limitSet is the receiver-side decode-limit configuration (generator#102).
//
// Every cap is always SET — the target carries a finite default that the config
// key only overrides (§9.5, generator#385) — so the three values are always
// available to emit. The `*Has` flags say something narrower: whether the schema
// actually has an unbounded field of that kind, and therefore whether the
// EXPORTED constant is emitted at all. A cap nothing in the schema can reach is
// inert, and an inert constant is dead code in every generated module.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits resolves the max_dyn_* caps over the target's finite defaults,
// and reads off the schema which of them the module actually exports.
//
// The values are emitted AS CONFIGURED. They used to be raised to the largest
// schema bound of their kind, because the caps rode into the corelib as a
// DecodeLimits that applies GLOBALLY per decode: corelib-ts measured every fixlen
// length and every array count against them with no schema exemption, so a cap
// below a sibling's `maxlen`/`count` would have rejected a field the schema
// declares perfectly legal. Keeping those decodable cost every UNBOUNDED field in
// the message exactly that much tightness -- §9.5 records it, #388 removed it.
// Enforced per field, where the schema is known, no raise is needed anywhere: the
// corelib is handed no DecodeLimits at all now, and the two caps a wrapper array's
// collector takes are exclusive with the schema bounds beside them (corelib-ts#164).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	d := generator.ClientDynLimits.Resolve(cfg)
	return limitSet{
		arrayCount: d.ArrayCount, arrayHas: b.HasDynArray,
		stringLen: d.StringLen, stringHas: b.HasDynString,
		blobLen: d.BlobLen, blobHas: b.HasDynBlob,
	}
}

// arrayCap / elemMaxCap render the receiver-side caps a wrapper array's collector
// is handed (§6.2.1) -- the element INDEX bound and the element LENGTH bound.
//
// Both are ALWAYS emitted. corelib-ts's own fallback for an omitted argument is
// the format ceiling, which bounds nothing the format does not already reject one
// step earlier, so leaving one out is not "the corelib's default" but no receiver
// bound at all. §6.2.1 puts the number here regardless: it comes from generated
// code, which knows the schema and the target, never from one the corelib invented.
//
// The exported constant is preferred so the module keeps one number per kind; it
// is emitted only where the schema has an unbounded field of that kind, and where
// it does not, the collector's argument is inert (the schema bound beside it
// governs and the two are exclusive) -- so the configured value goes in as a
// literal rather than the module growing a constant nothing reads.
func (g *gen) arrayCap() string {
	if g.limits.arrayHas {
		return "MAX_DYN_ARRAY_COUNT"
	}
	return strconv.FormatInt(g.limits.arrayCount, 10)
}

func (g *gen) elemMaxCap(elem ir.Kind) string {
	if elem == ir.KindBlob {
		if g.limits.blobHas {
			return "MAX_DYN_BLOB_LEN"
		}
		return strconv.FormatInt(g.limits.blobLen, 10)
	}
	if g.limits.stringHas {
		return "MAX_DYN_STRING_LEN"
	}
	return strconv.FormatInt(g.limits.stringLen, 10)
}

type tsfile struct{ b strings.Builder }

func (f *tsfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *tsfile) blank()        { f.b.WriteByte('\n') }
func (f *tsfile) bytes() []byte { return []byte(f.b.String()) }

func (g *gen) module(s *ir.Schema) []byte {
	// The body is rendered first and the import list derived from it: the decode
	// half is emitted per SCOPE, so which corelib names a module reaches is a
	// property of the emitted text rather than something a second scan of the
	// schema can restate without drifting from it.
	b := &tsfile{}
	g.moduleBody(b, s)
	body := b.b.String()

	f := &tsfile{}
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	f.line("import { %s } from %q;", strings.Join(usedImports(body), ", "), corelibPkg)
	f.blank()
	f.line("%s", body)
	return f.bytes()
}

// corelibNames is every name a generated module may take from the corelib, in
// the order they are imported. `decode` is aliased: the generated class carries a
// static `decode` of its own (§6.1.1), and the alias keeps the two apart at the
// one call site that needs the free function.
var corelibNames = []string{
	"OStream", "WireType", "FixlenSubtype", "ArrayKind", "DecodeStatus",
	"Long", "SofabError", "SofabErrorCode", "elementsEqual",
	"Visitor", "ArrayTarget", "IStream", "PayloadAcc", "decodeUtf8", "StringSeq", "BlobSeq",
}

// usedImports selects the corelib names a rendered module actually references.
func usedImports(body string) []string {
	var out []string
	for _, n := range corelibNames {
		if !identUsed(body, n) {
			continue
		}
		if n == "decode" {
			continue
		}
		out = append(out, n)
	}
	if identUsed(body, "_decode") {
		out = append(out, "decode as _decode")
	}
	return out
}

// identUsed reports whether `name` occurs in `body` as a whole identifier, so
// `Long` is not matched inside `LongArray` nor `decode` inside `decodeUtf8`.
func identUsed(body, name string) bool {
	isPart := func(c byte) bool {
		return c == '_' || c == '$' || (c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	for i := 0; ; {
		j := strings.Index(body[i:], name)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isPart(body[j-1])
		k := j + len(name)
		after := k >= len(body) || !isPart(body[k])
		if before && after {
			return true
		}
		i = j + 1
	}
}

// moduleBody renders everything below the import line.
func (g *gen) moduleBody(f *tsfile, s *ir.Schema) {
	use := g.scanHelpers(s)
	if g.limits.any() {
		f.line("// Receiver-side decode limits, baked from the sofabgen config")
		f.line("// (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len). They govern")
		f.line("// only fields the schema left unbounded -- a schema-bounded field keeps its")
		f.line("// own bound and its own INVALID verdict -- and they are enforced per field,")
		f.line("// at the count/length header, before any allocation: exceeding one throws")
		f.line("// SofabError with code LimitExceeded, which is a policy rejection of")
		f.line("// well-formed bytes and deliberately not InvalidMsg.")
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
	}
	if use.longArrEq {
		f.line("%s", longArrEqHelper)
		f.blank()
	}
	if use.fp32Raw || use.fp32ArrRaw {
		f.line("%s", fp32RawHelper)
		f.blank()
		f.line("%s", fp32BitsHelper)
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

	// The decode surface, after the classes it fills: one flat visitor per object
	// type and a public incremental Decoder per message.
	if decodesAnyField(s) {
		for _, key := range s.NamedOrder {
			nt := s.Named[key]
			if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
				g.emitVisitor(f, g.typeName(key), nt.Fields)
			}
		}
		for _, m := range s.Messages {
			g.emitVisitor(f, exported(m.Name), m.Fields)
			g.emitDecoderClass(f, exported(m.Name))
		}
	}
}

// schemaHasStringField reports whether any emitted class holds a `string` leaf --
// a scalar field or an array element -- i.e. whether the module transcodes a
// payload itself rather than leaving it to a corelib collector.
func schemaHasStringField(s *ir.Schema) bool {
	var walkElem func(elem ir.Kind, items *ir.ArrayElem) bool
	walkElem = func(elem ir.Kind, items *ir.ArrayElem) bool {
		if elem == ir.KindString {
			return true
		}
		if elem == ir.KindArray && items != nil {
			return walkElem(items.Elem, items.ElemItems)
		}
		return false
	}
	var has func(fields []*ir.Field) bool
	seen := map[string]bool{}
	has = func(fields []*ir.Field) bool {
		for _, x := range fields {
			switch x.Kind {
			case ir.KindString:
				return true
			case ir.KindArray:
				if walkElem(x.Elem, x.ElemItems) {
					return true
				}
			case ir.KindStruct, ir.KindUnion:
				if !seen[x.Ref.Key] {
					seen[x.Ref.Key] = true
					if has(x.Ref.Target.Fields) {
						return true
					}
				}
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
	// A TypeScript `enum` member can only be a number. A 64-bit-backed bitfield
	// is carried as a bigint (wideBitfield), so its masks are emitted as a frozen
	// const object instead — an enum of numbers would hand the caller masks that
	// neither assign to the field nor combine with `|` above bit 31.
	if wideBitfieldType(nt) {
		f.line("export const %s = {", g.typeName(nt.Key))
		for _, fl := range nt.Flags {
			f.emitDoc("  ", flagDoc(fl))
			f.line("  %s: %dn,", exported(fl.Name), uint64(1)<<uint(fl.Pos))
		}
		f.line("} as const;")
		f.blank()
		return
	}
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

	// decode: the class carries only the public decode(bytes) entry, which runs
	// the corelib's one-shot decode against this type's flat visitor (visitor.go).
	g.emitDecode(f, name)
	f.line("}")
	f.blank()
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
// corelib-ts's `growingOStream()` is deliberately not used anywhere: it allocates
// a slab and doubles it as the message grows, i.e. the CORELIB owning the
// storage, which is what §5.1 puts on this side of the call.
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
	// The sink is handed the installed buffer plus the region's coordinates, never
	// a view of its own: a subarray would be an allocation per flush, and the
	// encoder allocates nothing after construction (CORELIB_PLAN §6.6, §5.1.6).
	f.line("    const _os = new OStream(new Uint8Array(%d), 0, (_b, _s, _e) => { const _k = _b.slice(_s, _e); _out.push(_k); _n += _k.length; });", tsScratchSize)
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
