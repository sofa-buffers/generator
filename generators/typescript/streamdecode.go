package typescript

// Streaming decode for TypeScript: a push visitor over the corelib's resumable
// IStream, exposed as decoder() -> feed(chunk) / finish().
//
// This is the SECOND decoder the backend emits. The first — the monomorphic
// Cursor pull decoder in visitor.go — stays exactly as it is: it is the speed
// showcase and needs the whole message contiguous, which is the 90 % case. It
// simply cannot be fed in chunks, and corelib-ts's IStream (which can) has no
// generated visitor to drive. This file supplies one.
//
// Two decoders per type is a real cost — every §7 verdict now exists twice and
// could drift. Two things hold them together:
//
//   - The verdicts are emitted from the SAME helpers the cursor path uses
//     (widthCond, capOf, the maxlen/over-index messages), so a change to a rule
//     lands in both.
//   - A differential test feeds every corpus message through both paths and
//     requires identical results, value and error alike.
//
// Model: corelib-ts's Visitor is CHILD-VISITOR shaped, not flat — sequenceBegin
// returns the visitor for the nested scope, and array elements arrive through
// their own arrayUnsigned/arraySigned/arrayFp32/arrayFp64 callbacks carrying an
// index. That is the Go/Dart shape, not the Java/Rust/Zig location-stack shape,
// and it makes two whole classes of guard unnecessary:
//
//   - No location stack: each scope has its own visitor object.
//   - No askip/afill counters (generator#183/#188): an array arriving at a
//     scalar id lands in arrayUnsigned, which a scalar-only visitor does not
//     implement, so it evaporates structurally (§7.3) — as it does for Go and
//     Dart.
//
// Skipping an unknown sequence needs care. corelib-ts resolves the child as
// `parent.sequenceBegin?.(id) ?? parent`, so there is no "return null to skip":
// returning nothing keeps the PARENT visitor, and the unknown subtree's children
// would then bind into the enclosing scope (generator#268). The skip is a shared
// no-op visitor that returns ITSELF from sequenceBegin, so a whole subtree —
// children included — evaporates.

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// streamVisitorName is the class emitted for one object type's id scope.
func streamVisitorName(typeName string) string { return "_" + typeName + "Vis" }

// emitStreamPrelude writes the once-per-module support the visitors share: the
// skip visitor, and the Long narrowers on the Long channel. The payload
// accumulator and the UTF-8 transcode come from the corelib (PayloadAcc,
// decodeUtf8).
func (g *gen) emitStreamPrelude(f *tsfile, s *ir.Schema) {
	f.line("/**")
	f.line(" * The skip visitor. corelib-ts resolves a nested scope as")
	f.line(" * `parent.sequenceBegin?.(id) ?? parent`, so returning nothing keeps the")
	f.line(" * PARENT visitor and an unknown subtree's children would bind into the")
	f.line(" * enclosing scope. Returning this instead makes the whole subtree")
	f.line(" * evaporate: it implements no field callback, and its own sequenceBegin")
	f.line(" * returns itself, so nesting cannot escape it.")
	f.line(" */")
	if g.streamLongs() {
		f.line("/*")
		f.line(" * Every visitor in this module is on corelib-ts's opt-in Long channel")
		f.line(" * (`Visitor.longs`, corelib-ts#146): the four integer hooks deliver a `Long`")
		f.line(" * rather than the number-first `number | bigint`, so a 64-bit field takes the")
		f.line(" * value the wire's two halves already form and converts nothing.")
		f.line(" *")
		f.line(" * The flag is read ONCE, from the root visitor, and covers every integer")
		f.line(" * field and element in the message — this surface is driven by wire type")
		f.line(" * alone and never learns the schema. A narrow destination therefore narrows")
		f.line(" * back, through _u / _i below.")
		f.line(" */")
	}
	f.line("const _DEAD: %s = { %ssequenceBegin(): %s { return _DEAD; } };", g.visType(), g.longsFlagLit(), g.visType())
	f.blank()
	if g.streamLongs() {
		f.line("/** A Long from the `longs` channel, narrowed for an unsigned destination. */")
		f.line("function _u(v: Long): number {")
		f.line("  // The whole low half is exact as a number; only a value WITH a high half")
		f.line("  // takes the slow path, and every such value is already out of range for the")
		f.line("  // destinations this is called for — it arrives as the same wide number the")
		f.line("  // number-first channel produced, so the width reject is unchanged.")
		f.line("  return v.high === 0 ? v.low >>> 0 : Number(v.toBigInt(false));")
		f.line("}")
		f.blank()
		f.line("/** A Long from the `longs` channel, narrowed for a signed destination. */")
		f.line("function _i(v: Long): number {")
		f.line("  // Exact iff the high half is the sign extension of the low one, which is")
		f.line("  // every value an i8..i32 or enum destination can legally carry.")
		f.line("  const n = v.low | 0;")
		f.line("  return v.high === ((n >> 31) >>> 0) ? n : Number(v.toBigInt(true));")
		f.line("}")
		f.blank()
	}
}

// visType is the Visitor interface generated visitors implement: corelib-ts's
// `LongVisitor` (= `Visitor<Long>` plus `longs: true`) on the Long channel, the
// plain `Visitor` otherwise — so a module off the channel is unchanged.
func (g *gen) visType() string {
	if g.streamLongs() {
		return "LongVisitor"
	}
	return "Visitor"
}

// childVis is the declared return type of a sequenceBegin that can hand back a
// corelib collector — StringSeq / BlobSeq, which are plain `Visitor`s and carry
// no `longs` flag. Off the Long channel that is `Visitor`, exactly as before; on
// it the narrower `LongVisitor` would exclude them while saying nothing extra,
// since the channel is read once from the ROOT visitor and a child's own flag is
// never consulted. `AnyVisitor` is corelib-ts's own name for "either shape", and
// is what its decoders are implemented against.
func (g *gen) childVis() string {
	if g.streamLongs() {
		return "AnyVisitor"
	}
	return "Visitor"
}

// hookInt is the declared type of the value parameter on the four integer hooks:
// `Long` on corelib-ts's opt-in Long channel (`Visitor.longs`, corelib-ts#146),
// the number-first `number | bigint` otherwise. Emitted code names the module
// alias `_Vis` for the visitor interface itself, so only the prelude decides.
func (g *gen) hookInt() string {
	if g.streamLongs() {
		return "Long"
	}
	return "number | bigint"
}

// numU / numI narrow a hook value to a JS `number` for a destination that is one
// — u8..u32, i8..i32, a bitfield, an enum. Off the Long channel that is the
// `Number(v)` it always was; on it, the two helpers the prelude emits, which
// return the IDENTICAL number for every input — so no declared-width verdict
// (§7.1) depends on which channel delivered the value.
func (g *gen) numU(v string) string {
	if g.streamLongs() {
		return "_u(" + v + ")"
	}
	return "Number(" + v + ")"
}

func (g *gen) numI(v string) string {
	if g.streamLongs() {
		return "_i(" + v + ")"
	}
	return "Number(" + v + ")"
}

// boolFrom is the boolean conversion. `Boolean(v)` is WRONG on the Long channel:
// a Long is an object, so `Boolean(Long)` is true for the 64-bit zero too.
func (g *gen) boolFrom(v string) string {
	if g.streamLongs() {
		return "(" + v + ".low | " + v + ".high) !== 0"
	}
	return "Boolean(" + v + ")"
}

// longFrom is the 64-bit store. On the Long channel the value already IS the
// Long the field holds — nothing to convert, which is the point of the flag.
// Off it, `Long.fromValue` of a number goes through `BigInt`, i.e. a bigint per
// value on the one path these modes exist to keep bigint-free.
func (g *gen) longFrom(v string) string {
	if g.streamLongs() {
		return v
	}
	return "Long.fromValue(" + v + ")"
}

// matConvArrow renders the `conv` lambda a _MatSeq holds: the element
// conversion, plus the element's declared-width check when its kind has one.
//
// The check has to sit here rather than at the call site for the same reason it
// sits in the flat array's per-element arm (§7.1, generator#321): a row arrives
// element by element, and a scan of the finished row cannot reject an element a
// truncation may prevent from ever completing the row. The nested twin of that
// arm was missed when #330 closed the gap for go and dart — TypeScript was
// recorded as already guarding, which was true of the pull path only, so the
// same bytes were INVALID through Cursor and COMPLETE through the visitor.
func (g *gen) matConvArrow(elem ir.Kind, ref *ir.TypeRef, name string) string {
	conv := g.matConv(elem, ref)
	cond := widthCond("_e", elem)
	if cond == "" {
		return fmt.Sprintf("(v) => %s", conv)
	}
	// `conv` already yields the number this kind decodes to on either channel,
	// so the guard reads it back rather than re-deriving it.
	return fmt.Sprintf("(v) => { const _e = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, %q); return _e; }",
		conv, cond, fmt.Sprintf("%s element: value outside declared width %s", name, elem))
}

// matConv is a native matrix row's element conversion. Its parameter is a UNION
// — the same lambda is called from the integer hooks and from the fp32/fp64
// ones, which stay `number` on both channels — so on the Long channel it
// narrows with a `typeof` rather than a cast: an fp row arriving at an integer
// row's id must not reach a Long helper. Rare shape (array<array<int>>), so the
// branch costs nothing that matters.
func (g *gen) matConv(elem ir.Kind, ref *ir.TypeRef) string {
	if !g.streamLongs() {
		switch {
		case isBig(elem):
			if g.longArrays() {
				return "Long.fromValue(v)"
			}
			return "BigInt(v)"
		case elem == ir.KindBool:
			return "Boolean(v)"
		}
		return "Number(v)"
	}
	switch {
	case isBig(elem):
		return "typeof v === \"number\" ? Long.fromValue(v) : v"
	case elem == ir.KindBool:
		return "typeof v === \"number\" ? v !== 0 : (v.low | v.high) !== 0"
	case elem == ir.KindEnum, ir.IsNarrow(elem) && signedNarrow(elem):
		return "typeof v === \"number\" ? v : _i(v)"
	}
	return "typeof v === \"number\" ? v : _u(v)"
}

// signedNarrow reports whether a narrow integer kind rides the SIGNED hook.
func signedNarrow(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32
}

// emitStreamVisitor writes one object type's visitor class.
func (g *gen) emitStreamVisitor(f *tsfile, typeName string, fields []*ir.Field) {
	cls := streamVisitorName(typeName)
	f.line("/** Streaming decode visitor for {@link %s}. */", typeName)
	f.line("class %s implements %s {", cls, g.visType())
	f.line("  constructor(readonly o: %s, readonly a: PayloadAcc) {}", typeName)
	if g.streamLongs() {
		// Declared on EVERY visitor class, not only a message's: which one is the
		// root depends on who is decoding, and the corelib reads the flag from the
		// root. The literal type is what `LongVisitor` requires — a plain `= true`
		// would widen to boolean.
		f.line("  readonly longs: true = true;")
	}

	// fp32Raw is an opt-in channel: without it the corelib never allocates the
	// per-value raw view. Declare it only when some field actually needs the bits
	// (an fp32 with a raw companion), so a value-only message pays nothing.
	if anyFp32Raw(fields) {
		f.line("  readonly fp32Raw = true;")
	}
	// Scratch for an fp32 ARRAY's raw companion. The cursor path keeps the whole
	// payload in one slice (readFp32ArrayRaw) and nulls the companion when no
	// element was a NaN; the visitor sees elements one at a time, so it assembles
	// the same buffer across arrayBegin -> arrayFp32 -> arrayEnd. It has to be the
	// WHOLE payload, not just the NaN slots: a bit-exact consumer reads the
	// companion for every element once it exists (that is what the fp32 leaf of
	// Crucible's materialized walk does), so a partially filled buffer would
	// report zeros as values.
	for _, x := range fields {
		if x.Kind == ir.KindArray && x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
			f.line("  private %s: Uint8Array | null = null;", fp32RawScratch(x))
			f.line("  private %s = false;", fp32RawSeen(x))
		}
	}

	g.emitStreamScalarCb(f, "unsigned", fields, unsignedKinds)
	g.emitStreamScalarCb(f, "signed", fields, signedKinds)
	g.emitStreamFp(f, fields)
	g.emitStreamFixlenBegin(f, fields)
	g.emitStreamPayload(f, "string", fields)
	g.emitStreamPayload(f, "blob", fields)
	g.emitStreamArray(f, fields)
	g.emitStreamSequence(f, fields)

	f.line("}")
	f.blank()
}

// streamStorage is the destination expression for a field, from OUTSIDE the
// message class.
//
// It differs from storage() in one case and for one reason: a Long-backed array
// keeps a PRIVATE `_name` backing field that the cursor path writes directly,
// which it may because decodeInto is a static method of the class itself. The
// visitor is a separate class and cannot, so it goes through the public
// accessor. That is not merely a workaround -- the getter hands back the backing
// array itself, so an indexed element write still lands on it, and the setter's
// one-time conversion is exactly right for `= []`.
func (g *gen) streamStorage(x *ir.Field) string {
	return "this.o." + x.Name
}

// ---- callback bodies ------------------------------------------------------

type kindSet func(*ir.Field) bool

func unsignedKinds(x *ir.Field) bool {
	switch x.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBool, ir.KindBitfield:
		return true
	}
	return false
}

func signedKinds(x *ir.Field) bool {
	switch x.Kind {
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return true
	}
	return false
}

// fp32RawScratch / fp32RawSeen name the visitor-private slots that assemble an
// fp32 array's raw companion. Private, and prefixed, so they cannot collide with
// a schema field name (a schema name may not begin with `_`).
func fp32RawScratch(f *ir.Field) string { return "_raw" + exported(f.Name) }
func fp32RawSeen(f *ir.Field) string    { return "_rawNaN" + exported(f.Name) }

func anyFp32Raw(fields []*ir.Field) bool {
	for _, x := range fields {
		if fp32RawCompanion(x) {
			return true
		}
	}
	return false
}

// emitStreamScalarCb writes the unsigned/signed callback: one switch on id,
// each arm applying the same declared-width verdict the cursor path applies.
func (g *gen) emitStreamScalarCb(f *tsfile, cb string, fields []*ir.Field, in kindSet) {
	var arms []string
	for _, x := range fields {
		if !in(x) {
			continue
		}
		acc := g.streamStorage(x)
		switch x.Kind {
		case ir.KindBool:
			arms = append(arms, fmt.Sprintf("      case %d: %s = %s; break;", x.ID, acc, g.boolFrom("v")))
		case ir.KindU64, ir.KindI64:
			if g.numberScalars() {
				arms = append(arms, fmt.Sprintf("      case %d: %s = Number(v); break;", x.ID, acc))
			} else if g.longScalars() {
				// On the Long channel `v` IS the Long the field holds. Off it the hooks
				// are number-first and the field's SETTER converts (this arm stores
				// through the public name), exactly once. Either way the field holds a
				// Long whichever decode API filled it — the invariant #335 established.
				arms = append(arms, fmt.Sprintf("      case %d: %s = v; break;", x.ID, acc))
			} else {
				arms = append(arms, fmt.Sprintf("      case %d: %s = BigInt(v); break;", x.ID, acc))
			}
		case ir.KindEnum:
			arms = append(arms, fmt.Sprintf("      case %d: %s = %s as %s; break;", x.ID, acc, g.numI("v"), g.typeName(x.Ref.Key)))
		default:
			// Which hook delivers it decides the narrowing; `in` has already selected
			// the fields this callback carries.
			num := g.numU("v")
			if cb == "signed" {
				num = g.numI("v")
			}
			cond := widthCond("_v", x.Kind)
			if cond == "" {
				arms = append(arms, fmt.Sprintf("      case %d: %s = %s; break;", x.ID, acc, num))
			} else {
				arms = append(arms, fmt.Sprintf("      case %d: { const _v = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: value outside declared width %s\"); %s = _v; break; }",
					x.ID, num, cond, x.Name, x.Kind, acc))
			}
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("  %s(id: number, v: %s): void {", cb, g.hookInt())
	f.line("    switch (id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("      default: break;")
	f.line("    }")
	f.line("  }")
}

// emitStreamFp writes the fp32/fp64 callbacks. fp32 keeps the raw wire bytes
// beside the value for a NaN, exactly as the cursor path does: widening an fp32
// signaling NaN into a JS double quiets it, so the bytes are the only faithful
// carrier (§4.6).
func (g *gen) emitStreamFp(f *tsfile, fields []*ir.Field) {
	var f32, f64 []string
	for _, x := range fields {
		acc := g.streamStorage(x)
		switch x.Kind {
		case ir.KindFP32:
			if fp32RawCompanion(x) {
				f32 = append(f32, fmt.Sprintf("      case %d: { %s = v; %s = (Number.isNaN(v) && raw !== undefined) ? raw.slice() : null; break; }",
					x.ID, acc, g.fp32RawStorage("this.o", x)))
			} else {
				f32 = append(f32, fmt.Sprintf("      case %d: %s = v; break;", x.ID, acc))
			}
		case ir.KindFP64:
			f64 = append(f64, fmt.Sprintf("      case %d: %s = v; break;", x.ID, acc))
		}
	}
	if len(f32) > 0 {
		f.line("  fp32(id: number, v: number, raw?: Uint8Array): void {")
		f.line("    switch (id) {")
		for _, a := range f32 {
			f.line("%s", a)
		}
		f.line("      default: break;")
		f.line("    }")
		f.line("  }")
	}
	if len(f64) > 0 {
		f.line("  fp64(id: number, v: number): void {")
		f.line("    switch (id) {")
		for _, a := range f64 {
			f.line("%s", a)
		}
		f.line("      default: break;")
		f.line("    }")
		f.line("  }")
	}
}

// emitStreamFixlenBegin writes the fixlenBegin callback: the maxlen verdict, taken
// at the LENGTH WORD.
//
// It cannot live in the string/blob payload callback, which is where it used to
// be: those fire only once payload bytes arrive, so a message that ends right
// after an over-maxlen length word never reached the check and degraded to
// INCOMPLETE -- while the same bytes through Cursor.readString are INVALID (§5.2
// gives INVALID precedence over INCOMPLETE, generator#300). fixlenBegin is the
// string/blob counterpart of arrayBegin and fires at the word, so the two decode
// paths now agree.
//
// The announced SUBTYPE is tested, not ignored. A `string` arriving at a `blob`
// field's id is a §7.3 wire-type mismatch and must be skipped, not bounded by the
// declared field's maxlen -- the same trap the array path fell into by ignoring
// its kind parameter (generator#300, first half).
func (g *gen) emitStreamFixlenBegin(f *tsfile, fields []*ir.Field) {
	var arms []string
	for _, x := range fields {
		if !x.HasMaxlen {
			continue
		}
		var sub, kind string
		switch x.Kind {
		case ir.KindString:
			sub, kind = "FixlenSubtype.String", "string"
		case ir.KindBlob:
			sub, kind = "FixlenSubtype.Blob", "blob"
		default:
			continue
		}
		arms = append(arms, fmt.Sprintf("      case %d: if (sub === %s && total > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: %s byte length above schema maxlen %d\"); break;",
			x.ID, sub, x.Maxlen, x.Name, kind, x.Maxlen))
	}
	if len(arms) == 0 {
		return
	}
	f.line("  fixlenBegin(id: number, sub: FixlenSubtype, total: number): void {")
	f.line("    switch (id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("      default: break;")
	f.line("    }")
	f.line("  }")
}

// emitStreamPayload writes the string/blob callback.
//
// The maxlen verdict is taken at offset 0, against `total` — the word that
// establishes the violation — NOT against the assembled payload. That keeps an
// over-maxlen field INVALID even when the message is truncated inside it, and
// means an over-long payload is never buffered (§5.2/§7, issue #267). Only after
// that does the accumulator run.
func (g *gen) emitStreamPayload(f *tsfile, cb string, fields []*ir.Field) {
	want := ir.KindString
	if cb == "blob" {
		want = ir.KindBlob
	}
	var arms []string
	for _, x := range fields {
		if x.Kind != want {
			continue
		}
		acc := g.streamStorage(x)
		var pre string
		if x.HasMaxlen {
			pre = fmt.Sprintf("if (total > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: %s byte length above schema maxlen %d\"); ",
				x.Maxlen, x.Name, cb, x.Maxlen)
		}
		var store string
		if want == ir.KindString {
			store = fmt.Sprintf("%s = decodeUtf8(_p);", acc)
		} else {
			store = fmt.Sprintf("%s = _p.slice();", acc)
		}
		arms = append(arms, fmt.Sprintf("      case %d: { %sconst _p = this.a.take(total, offset, chunk); if (_p === null) break; %s break; }",
			x.ID, pre, store))
	}
	if len(arms) == 0 {
		return
	}
	f.line("  %s(id: number, total: number, offset: number, chunk: Uint8Array): void {", cb)
	f.line("    switch (id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("      default: break;")
	f.line("    }")
	f.line("  }")
}

// emitStreamArray writes the native-array callbacks: arrayBegin (the header
// hook) plus one element callback per element wire kind, and arrayEnd.
//
// The over-count verdict is taken in arrayBegin, at the COUNT WORD, before a
// single element arrives. That is what keeps an over-count array INVALID rather
// than INCOMPLETE when the message is truncated inside it (§5.2/§7, F-0032), and
// it costs nothing — the corelib hands the declared count in.
func (g *gen) emitStreamArray(f *tsfile, fields []*ir.Field) {
	type arm struct {
		id   int64
		body string
	}
	var begin, end []string
	uns, sig, f32, f64 := []string{}, []string{}, []string{}, []string{}

	for _, x := range fields {
		if x.Kind != ir.KindArray || !nativeArrayElem(x.Elem) {
			continue
		}
		acc := g.streamStorage(x)
		cap := capOf(x.HasCount, x.Count)
		// arrayBegin: check the announced element KIND first, then reject an
		// over-count array at the count word, then replace the destination whole
		// (a re-opened array id replaces, S7.4).
		//
		// The kind test has to come first, and both of the things after it depend
		// on that. The corelib routes an array header by ID alone, so this arm also
		// receives a header whose element kind CONTRADICTS the declared one -- and
		// such a field is skipped whole (S7.3), which means:
		//
		//   * its count must not be measured against this field's capacity. It is
		//     not this field's count. Bounding it turned a skippable contradiction
		//     into INVALID, and when the message was also truncated inside that
		//     array the verdict flipped from INCOMPLETE to INVALID -- observable
		//     only when chunked, because only then does the header arrive without
		//     the elements behind it;
		//   * the destination must not be cleared. A correctly-typed earlier
		//     occurrence survives a mis-typed later one (S7.4).
		//
		// The cursor path has always had this order (`if (c.wire !== ...) skip`),
		// which is precisely why the two decoders disagreed.
		b := fmt.Sprintf("      case %d: ", x.ID)
		b += fmt.Sprintf("if (kind !== ArrayKind.%s) break; ", tsArrayKind(x.Elem))
		if cap >= 0 {
			b += fmt.Sprintf("if (count > %d) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: array count above schema capacity %d\"); ", cap, x.Name, cap)
		}
		b += fmt.Sprintf("%s = []; ", acc)
		// A re-opened array id REPLACES (§7.4), so the companion is reset here
		// too -- otherwise a second occurrence would inherit the first's raw
		// bytes. Sized from the announced count, which the guard above has
		// already bounded, so an over-count header cannot size this.
		if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
			b += fmt.Sprintf("%s = null; this.%s = new Uint8Array(count * 4); this.%s = false; ",
				g.fp32RawStorage("this.o", x), fp32RawScratch(x), fp32RawSeen(x))
		}
		b += "break;"
		begin = append(begin, b)

		// arrayEnd decides the companion, exactly as the cursor path does after
		// its whole-array read: keep the payload when SOME element was a NaN,
		// null otherwise. Emitted only for an fp32 array -- every other element
		// kind round-trips through its own type without loss.
		if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
			end = append(end, fmt.Sprintf("      case %d: %s = this.%s ? this.%s : null; this.%s = null; break;",
				x.ID, g.fp32RawStorage("this.o", x), fp32RawSeen(x), fp32RawScratch(x), fp32RawScratch(x)))
		}

		// element store, by the wire kind the corelib will deliver
		conv, dst := g.streamElemConv(x)
		// The element's declared width is a validity bound too (S7.1): the cursor
		// path scans the whole array after reading it, the visitor checks each
		// element as it arrives -- same verdict, one element earlier.
		var line string
		if ec := widthCond("_e", x.Elem); ec != "" {
			num := g.numU("v")
			if dst == "signed" {
				num = g.numI("v")
			}
			line = fmt.Sprintf("      case %d: { const _e = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, \"%s: value outside declared width %s\"); %s[i] = %s; break; }",
				x.ID, num, ec, x.Name, x.Elem, acc, conv)
		} else if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
			// §6.5: a JS number is a 64-bit double, and widening an fp32 SIGNALING
			// NaN into one sets the quiet bit -- so `v` alone cannot round-trip the
			// element. Keep the 4 wire bytes; `raw` is why the class declares
			// fp32Raw. Bounds-checked because the buffer is sized from the
			// ANNOUNCED count and `i` comes from the same wire.
			line = fmt.Sprintf("      case %d: { %s[i] = %s; const _r = this.%s; if (raw !== undefined && _r !== null && (i + 1) * 4 <= _r.length) _r.set(raw, i * 4); if (Number.isNaN(v)) this.%s = true; break; }",
				x.ID, acc, conv, fp32RawScratch(x), fp32RawSeen(x))
		} else {
			line = fmt.Sprintf("      case %d: %s[i] = %s; break;", x.ID, acc, conv)
		}
		switch dst {
		case "unsigned":
			uns = append(uns, line)
		case "signed":
			sig = append(sig, line)
		case "fp32":
			f32 = append(f32, line)
		case "fp64":
			f64 = append(f64, line)
		}

		// No fill-to-count on arrayEnd. A declared `count: N` is a CAPACITY, not a
		// length (MESSAGE_SPEC S3): the wire count IS the array's length, and the
		// cursor path likewise assigns what it read. Padding here would make the
		// two decoders disagree on every short array. The only thing arrayEnd
		// does is close out the fp32 raw companion (above).
	}
	emit := func(sig string, arms []string) {
		if len(arms) == 0 {
			return
		}
		f.line("  %s {", sig)
		f.line("    switch (id) {")
		for _, a := range arms {
			f.line("%s", a)
		}
		f.line("      default: break;")
		f.line("    }")
		f.line("  }")
	}
	emit("arrayBegin(id: number, kind: ArrayKind, count: number): void", begin)
	emit(fmt.Sprintf("arrayUnsigned(id: number, i: number, v: %s): void", g.hookInt()), uns)
	emit(fmt.Sprintf("arraySigned(id: number, i: number, v: %s): void", g.hookInt()), sig)
	emit("arrayFp32(id: number, i: number, v: number, raw?: Uint8Array): void", f32)
	emit("arrayFp64(id: number, i: number, v: number): void", f64)
	emit("arrayEnd(id: number): void", end)
}

// streamElemConv gives the element store expression and which element callback
// delivers it.
func (g *gen) streamElemConv(x *ir.Field) (string, string) {
	switch x.Elem {
	case ir.KindBool:
		return g.boolFrom("v"), "unsigned"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		return g.numU("v"), "unsigned"
	case ir.KindU64:
		if g.longArrays() {
			return g.longFrom("v"), "unsigned"
		}
		return "BigInt(v)", "unsigned"
	case ir.KindI8, ir.KindI16, ir.KindI32:
		return g.numI("v"), "signed"
	case ir.KindEnum:
		return fmt.Sprintf("%s as %s", g.numI("v"), g.typeName(x.ElemRef.Key)), "signed"
	case ir.KindI64:
		if g.longArrays() {
			return g.longFrom("v"), "signed"
		}
		return "BigInt(v)", "signed"
	case ir.KindFP32:
		return "v", "fp32"
	case ir.KindFP64:
		return "v", "fp64"
	}
	return "v", "unsigned"
}

// emitStreamSequence writes sequenceBegin: the one callback that decides where a
// nested scope's fields go.
//
// A struct/union descends into the EXISTING member's visitor, so a re-opened
// scope merges into what an earlier opening set (S7.4). A wrapper-sequence array
// gets a fresh collector that clears its destination first, so a re-opened
// wrapper replaces the array whole. Every id the schema does not declare returns
// _DEAD, which discards the subtree with its children.
func (g *gen) emitStreamSequence(f *tsfile, fields []*ir.Field) {
	var arms []string
	for _, x := range fields {
		acc := g.streamStorage(x)
		switch {
		case x.Kind == ir.KindStruct || x.Kind == ir.KindUnion:
			arms = append(arms, fmt.Sprintf("      case %d: return new %s(%s, this.a);",
				x.ID, streamVisitorName(g.typeName(x.Ref.Key)), acc))
		case x.Kind == ir.KindArray && !nativeArrayElem(x.Elem):
			if c := g.streamCollector(x, acc); c != "" {
				arms = append(arms, fmt.Sprintf("      case %d: %s = []; return %s;", x.ID, acc, c))
			}
		}
	}
	f.line("  sequenceBegin(id: number): %s {", g.childVis())
	if len(arms) > 0 {
		f.line("    switch (id) {")
		for _, a := range arms {
			f.line("%s", a)
		}
		f.line("      default: break;")
		f.line("    }")
	}
	f.line("    return _DEAD;")
	f.line("  }")
}

// streamCollector builds the collector for one wrapper-sequence array.
func (g *gen) streamCollector(x *ir.Field, acc string) string {
	emax := int64(-1)
	if x.ElemMaxHas {
		emax = x.ElemMax
	}
	return g.streamSeqExpr(acc, x.Elem, x.ElemRef, x.ElemItems,
		capOf(x.HasCount, x.Count), emax, x.Name)
}

// streamSeqExpr builds the collector for a wrapper sequence whose elements are
// of kind `elem`, recursing for a row of rows.
//
// The recursion is what the corpus definition nested_rows.yaml exists to catch:
// array<array<string>> and deeper. A row whose elements are NATIVE arrives as an
// array at the row id (arrayBegin/arrayUnsigned), a row whose elements are
// WRAPPERS arrives as a sequence at the row id (sequenceBegin) — two different
// callbacks, so two different collectors, chosen by the inner element kind.
func (g *gen) streamSeqExpr(acc string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap, emax int64, name string) string {
	switch elem {
	case ir.KindString:
		return fmt.Sprintf("new StringSeq(%s, this.a, %d, %d, %q)", acc, cap, emax, name)
	case ir.KindBlob:
		return fmt.Sprintf("new BlobSeq(%s, this.a, %d, %d, %q)", acc, cap, emax, name)
	case ir.KindStruct, ir.KindUnion:
		el := g.typeName(ref.Key)
		return fmt.Sprintf("new _ObjSeq(%s, this.a, %d, %q, () => new %s(), (e) => new %s(e, this.a))",
			acc, cap, name, el, streamVisitorName(el))
	case ir.KindArray:
		if items == nil {
			return ""
		}
		rowCap := capOf(items.HasCount, items.Count)
		if nativeArrayElem(items.Elem) {
			return fmt.Sprintf("new _MatSeq(%s, %d, %d, %q, %s)", acc, cap, rowCap, name, g.matConvArrow(items.Elem, items.ElemRef, name))
		}
		// A row of wrappers: descend one level and build the row's own collector.
		rowMax := int64(-1)
		if items.ElemMaxHas {
			rowMax = items.ElemMax
		}
		inner := g.streamSeqExpr("_r", items.Elem, items.ElemRef, items.ElemItems, rowCap, rowMax, name)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf("new _RowSeq(%s, %d, %q, (_r) => %s)", acc, cap, name, inner)
	}
	return ""
}

// emitStreamCollectors writes the wrapper-sequence collectors that have no
// corelib counterpart: the visitors an object/matrix/row array hands its
// elements to. A string or blob row is collected by corelib-ts's own StringSeq /
// BlobSeq (corelib-ts#151), which take the capacity, the element maxlen and the
// field name as arguments.
//
// MESSAGE_SPEC S5.1 makes the element id the array INDEX, so an element is
// placed at its id rather than appended: an interior element equal to its
// default is omitted on the wire and the gap it leaves is filled with the
// element default. The last element is always present, so the highest id + 1 is
// the array's exact length. An id at or beyond the schema capacity is INVALID
// and is rejected before the destination grows -- which also bounds the id-keyed
// fill against an over-index amplification.
func (g *gen) emitStreamCollectors(f *tsfile, use streamUse) {
	if use.obj {
		f.line("/**")
		f.line(" * Collects the elements of a struct/union wrapper-sequence array.")
		f.line(" *")
		f.line(" * A re-opened element id returns the visitor for the element ALREADY at")
		f.line(" * that index, so the second opening merges into the first (S7.4) instead")
		f.line(" * of appending a duplicate.")
		f.line(" */")
		f.line("class _ObjSeq<T> implements %s {", g.visType())
		g.emitLongsFlag(f)
		f.line("  constructor(readonly out: T[], readonly a: PayloadAcc, readonly cap: number,")
		f.line("              readonly nm: string, readonly make: () => T,")
		f.line("              readonly vis: (e: T) => %s) {}", g.visType())
		f.line("  sequenceBegin(id: number): %s {", g.visType())
		f.line("    if (this.cap >= 0 && id >= this.cap) throw new SofabError(SofabErrorCode.InvalidMsg, `${this.nm}: array index above schema capacity ${this.cap}`);")
		f.line("    while (this.out.length <= id) this.out.push(this.make());")
		f.line("    return this.vis(this.out[id]);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
	if use.mat {
		f.line("/**")
		f.line(" * Collects the rows of a NATIVE matrix: an array whose rows are arrays of")
		f.line(" * numbers, so each row arrives as an array at its row index.")
		f.line(" */")
		f.line("class _MatSeq<E> implements %s {", g.visType())
		g.emitLongsFlag(f)
		f.line("  constructor(readonly out: E[][], readonly cap: number, readonly rowCap: number,")
		f.line("              readonly nm: string, readonly conv: (v: %s | number) => E) {}", g.hookInt())
		f.line("  private row(id: number): E[] {")
		f.line("    if (this.cap >= 0 && id >= this.cap) throw new SofabError(SofabErrorCode.InvalidMsg, `${this.nm}: array index above schema capacity ${this.cap}`);")
		f.line("    while (this.out.length <= id) this.out.push([]);")
		f.line("    return this.out[id];")
		f.line("  }")
		f.line("  arrayBegin(id: number, _k: ArrayKind, count: number): void {")
		f.line("    if (this.rowCap >= 0 && count > this.rowCap) throw new SofabError(SofabErrorCode.InvalidMsg, `${this.nm} element: array count above schema capacity ${this.rowCap}`);")
		f.line("    this.row(id).length = 0;")
		f.line("  }")
		f.line("  arrayUnsigned(id: number, i: number, v: %s): void { this.row(id)[i] = this.conv(v); }", g.hookInt())
		f.line("  arraySigned(id: number, i: number, v: %s): void { this.row(id)[i] = this.conv(v); }", g.hookInt())
		// A float row arrives on its own callbacks, not on arrayUnsigned/Signed.
		// Leaving these out does not fail to compile and does not throw -- the row
		// simply never lands, which is why the cursor-vs-feed differential test
		// exists rather than a typecheck.
		f.line("  arrayFp32(id: number, i: number, v: number): void { this.row(id)[i] = this.conv(v); }")
		f.line("  arrayFp64(id: number, i: number, v: number): void { this.row(id)[i] = this.conv(v); }")
		f.line("  sequenceBegin(): %s { return _DEAD; }", g.visType())
		f.line("}")
		f.blank()
	}
	if use.row {
		f.line("/**")
		f.line(" * Collects the rows of a WRAPPER matrix: an array whose rows are arrays of")
		f.line(" * strings/blobs/structs, or of further such rows.")
		f.line(" *")
		f.line(" * The distinction from _MatSeq is which callback a row arrives on. A row of")
		f.line(" * numbers is an ARRAY (arrayBegin); a row of wrappers is a SEQUENCE, so it")
		f.line(" * comes through sequenceBegin and needs a collector of its own -- which for")
		f.line(" * a row of rows is another _RowSeq, hence the factory.")
		f.line(" */")
		f.line("class _RowSeq<E> implements %s {", g.visType())
		g.emitLongsFlag(f)
		f.line("  constructor(readonly out: E[][], readonly cap: number, readonly nm: string,")
		f.line("              readonly mk: (row: E[]) => %s) {}", g.childVis())
		f.line("  sequenceBegin(id: number): %s {", g.childVis())
		f.line("    if (this.cap >= 0 && id >= this.cap) throw new SofabError(SofabErrorCode.InvalidMsg, `${this.nm}: array index above schema capacity ${this.cap}`);")
		f.line("    while (this.out.length <= id) this.out.push([]);")
		f.line("    this.out[id].length = 0;")
		f.line("    return this.mk(this.out[id]);")
		f.line("  }")
		f.line("}")
		f.blank()
	}
}

// schemaHasStringField reports whether any emitted class has a scalar `string`
// field — the one position whose stream store site transcodes a payload itself
// (a string ARRAY leaves that to the corelib's StringSeq), and thus the only one
// that names decodeUtf8.
func schemaHasStringField(s *ir.Schema) bool {
	has := func(fields []*ir.Field) bool {
		for _, x := range fields {
			if x.Kind == ir.KindString {
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
		if has(s.Named[key].Fields) {
			return true
		}
	}
	return false
}

// streamUse records which collectors a schema actually needs, so a module never
// carries a class no message can reach.
type streamUse struct{ str, blob, obj, mat, row bool }

func (g *gen) scanStreamUse(s *ir.Schema) streamUse {
	var u streamUse
	var walk func(elem ir.Kind, items *ir.ArrayElem)
	walk = func(elem ir.Kind, items *ir.ArrayElem) {
		switch elem {
		case ir.KindString:
			u.str = true
		case ir.KindBlob:
			u.blob = true
		case ir.KindStruct, ir.KindUnion:
			u.obj = true
		case ir.KindArray:
			if items == nil {
				return
			}
			if nativeArrayElem(items.Elem) {
				u.mat = true
				return
			}
			u.row = true
			walk(items.Elem, items.ElemItems)
		}
	}
	visit := func(fields []*ir.Field) {
		for _, x := range fields {
			if x.Kind == ir.KindArray && !nativeArrayElem(x.Elem) {
				walk(x.Elem, x.ElemItems)
			}
		}
	}
	for _, m := range s.Messages {
		visit(m.Fields)
	}
	for _, key := range s.NamedOrder {
		visit(s.Named[key].Fields)
	}
	return u
}

// emitStreamDecoderClass writes the public incremental decoder for a message.
//
// It is a handle on the corelib's resumable IStream plus the destination it
// fills; the IStream carries the parse state, so this class carries none. A
// malformed message throws SofabError out of feed, exactly as it throws out of
// the one-shot decode -- the two paths report the same way, which is half of
// what keeps them from drifting. (The other half is the differential test.)
func (g *gen) emitStreamDecoderClass(f *tsfile, name string) {
	f.line("/**")
	f.line(" * Incremental decoder for {@link %s}: hold one and feed the message as", name)
	f.line(" * bytes arrive, instead of buffering it whole first.")
	f.line(" *")
	f.line(" * The wire format has no end marker at the top level -- a message ends where")
	f.line(" * its bytes end -- so a feed cannot report that the MESSAGE is complete, only")
	f.line(" * that the bytes handed in ended on a field boundary (`Complete`) or")
	f.line(" * mid-field (`Incomplete`). Neither is a failure mid-stream; the caller's own")
	f.line(" * framing decides when the input is over, and `finish` then gives the verdict")
	f.line(" * for the message as a whole.")
	f.line(" *")
	f.line(" * Nothing is retained from the chunks you feed: a string is decoded and a")
	f.line(" * blob copied before it reaches the destination, so a chunk may be reused as")
	f.line(" * soon as `feed` returns.")
	f.line(" */")
	f.line("export class %sDecoder {", name)
	f.line("  private readonly is = new IStream(%s);", g.streamLimitsArg())
	f.line("  private readonly acc = new PayloadAcc();")
	f.line("  private readonly out: %s;", name)
	f.line("  private readonly vis: %s;", g.visType())
	f.blank()
	f.line("  constructor(out?: %s) {", name)
	f.line("    this.out = out ?? new %s();", name)
	f.line("    this.vis = new %s(this.out, this.acc);", streamVisitorName(name))
	f.line("  }")
	f.blank()
	f.line("  /**")
	f.line("   * Feed the next chunk, of any size. Returns `Complete` if it ended on a")
	f.line("   * field boundary, `Incomplete` if it ended mid-field -- neither answers")
	f.line("   * whether the MESSAGE is done.")
	f.line("   *")
	f.line("   * @throws SofabError the bytes are malformed; terminal.")
	f.line("   */")
	f.line("  feed(chunk: Uint8Array): DecodeStatus {")
	f.line("    this.is.feed(chunk, this.vis);")
	f.line("    return this.is.end();")
	f.line("  }")
	f.blank()
	f.line("  /** The outcome for everything fed so far, without feeding more. */")
	f.line("  get status(): DecodeStatus { return this.is.end(); }")
	f.blank()
	f.line("  /** The destination, holding whatever has been decoded so far. */")
	f.line("  get message(): %s { return this.out; }", name)
	f.blank()
	f.line("  /**")
	f.line("   * Take the decoded message once the caller's framing says the input is")
	f.line("   * over. Rejects a stream that ended mid-field rather than returning a")
	f.line("   * half-filled value; read `message` to get it anyway.")
	f.line("   */")
	f.line("  finish(): %s {", name)
	f.line("    if (this.is.end() !== DecodeStatus.Complete) {")
	f.line("      throw new SofabError(SofabErrorCode.Incomplete, \"%s: stream ended mid-field\");", name)
	f.line("    }")
	f.line("    return this.out;")
	f.line("  }")
	f.line("}")
	f.blank()
}

// streamLimitsArg passes the configured receiver-side decode limits into the
// IStream, so the streaming path enforces the same caps as the cursor path.
// cursorLimits() renders them as a trailing argument (leading ", "); the IStream
// takes them as its only argument, so the separator is dropped.
func (g *gen) streamLimitsArg() string {
	return strings.TrimPrefix(g.cursorLimits(), ", ")
}

// tsArrayKind names the corelib ArrayKind a declared native array element is
// delivered as. It mirrors how the corelib classifies an array header, so the
// streaming visitor can tell "this header is for my field" from "this header
// contradicts my field and must be skipped" (MESSAGE_SPEC S7.3).
//
// For a fixlen array the kind names the ELEMENT SUBTYPE, not merely "fixlen":
// an fp64 header at a declared fp32 array is a contradiction like any other, so
// the two must not collapse to one case.
func tsArrayKind(elem ir.Kind) string {
	switch elem {
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return "Signed"
	case ir.KindFP32:
		return "Fp32"
	case ir.KindFP64:
		return "Fp64"
	}
	// u*, enum, bool and bitfield all travel as unsigned elements.
	return "Unsigned"
}

// emitLongsFlag writes the `longs` declaration a generated visitor class carries on
// the Long channel, and the empty string off it. Every class declares it, not
// only a message's: which visitor is the ROOT depends on who is decoding, and
// the corelib reads the flag from the root (corelib-ts#146). The literal type is
// what `LongVisitor` requires — a plain `= true` would widen to `boolean`.
func (g *gen) emitLongsFlag(f *tsfile) {
	if g.streamLongs() {
		f.line("  readonly longs: true = true;")
	}
}

// longsFlagLit is the same flag as an object-literal member, for _DEAD.
func (g *gen) longsFlagLit() string {
	if g.streamLongs() {
		return "longs: true, "
	}
	return ""
}
