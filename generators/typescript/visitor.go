package typescript

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// This file emits the decode half: a FLAT visitor per generated class
// (ARCHITECTURE §9.3 family 1, the shape python/rust/csharp/java/kotlin use).
//
// corelib-ts removed its pull API in favour of CORELIB_PLAN §5.3.1 ("the visitor
// is the only decode surface"), and its visitor is flat: sequenceBegin returns a
// boolean, not a child visitor, so ONE object receives every callback at every
// depth. A field id alone therefore no longer identifies a destination -- id 0
// means something different in every sequence -- so dispatch is keyed on
// (location, id): `location` is a small int naming the scope the walk is
// currently inside, maintained by sequenceBegin/sequenceEnd over an explicit
// stack.
//
// Two scope kinds exist. An OBJECT scope is a message/struct/union: its fields
// dispatch by id, and each names a static property path. An ARRAY scope is a
// wrapper-sequence array (string/blob/struct/union/nested-array elements,
// MESSAGE_SPEC §5.1): there the id IS the element index, so it dispatches by
// index instead, with an index register carrying it down into an element scope.
//
// A declined sequence needs no dead location: corelib-ts's `return false` skips
// the whole subtree -- nothing inside is delivered and no sequenceEnd fires for
// it -- so the stack is only ever pushed for a scope we actually entered.

// tsScope is one dispatch location in a flat visitor.
type tsScope struct {
	id   int
	name string // location constant, e.g. "_L_Point_start"

	// Object scope: fields dispatch by id, `path` names the object they land on.
	fields   []*ir.Field
	path     string
	seqChild map[int64]int // field id -> scope entered by its SEQUENCE_START

	// Array scope: elements dispatch by index.
	isArr      bool
	arrPath    string // the array the elements land in
	elem       ir.Kind
	elemRef    *ir.TypeRef
	elemItems  *ir.ArrayElem
	cap        int64 // element capacity; -1 when the schema leaves it open
	elemMaxHas bool
	elemMax    int64
	loc        string // schema location named in a rejection message
	ix         string // index register, "" when no element scope needs one
	row        string // current-row register for a native matrix row, "" otherwise
	seq        string // StringSeq/BlobSeq slot, "" for a framed element kind
	child      int    // element scope id, -1 when the element is a value
	parent     int    // scope this one is entered from, -1 for the root
}

type tsScopeSet struct{ scopes []*tsScope }

// buildScopes walks a class's tree and assigns one scope per sequence-framed
// location reachable from it, rooted at the class itself.
func (g *gen) buildScopes(typeName string, fields []*ir.Field) []*tsScope {
	ss := &tsScopeSet{}
	ss.object(g, typeName, "this.o", fields)
	return ss.scopes
}

func (ss *tsScopeSet) object(g *gen, locName, path string, fields []*ir.Field) int {
	sc := &tsScope{
		id: len(ss.scopes), name: "_L_" + locName,
		fields: fields, path: path, seqChild: map[int64]int{}, child: -1, parent: -1,
	}
	ss.scopes = append(ss.scopes, sc)
	for _, fld := range fields {
		switch fld.Kind {
		case ir.KindStruct, ir.KindUnion:
			sc.seqChild[fld.ID] = ss.object(g, locName+"_"+fld.Name,
				g.visStorage(path, fld), fld.Ref.Target.Fields)
			ss.scopes[sc.seqChild[fld.ID]].parent = sc.id
		case ir.KindArray:
			// A native scalar array arrives element-wise through arrayBegin /
			// array<kind> at THIS scope; only a wrapper-sequence array opens a
			// scope of its own.
			if !nativeArrayElem(fld.Elem) {
				sc.seqChild[fld.ID] = ss.array(g, locName+"_"+fld.Name,
					g.visStorage(path, fld), fld.Name,
					fld.Elem, fld.ElemRef, fld.ElemItems,
					capOf(fld.HasCount, fld.Count), fld.ElemMaxHas, fld.ElemMax)
				ss.scopes[sc.seqChild[fld.ID]].parent = sc.id
			}
		}
	}
	return sc.id
}

func (ss *tsScopeSet) array(g *gen, locName, arrPath, loc string, elem ir.Kind, ref *ir.TypeRef,
	items *ir.ArrayElem, cap int64, emHas bool, em int64) int {
	sc := &tsScope{
		id: len(ss.scopes), name: "_L_" + locName,
		isArr: true, arrPath: arrPath, elem: elem, elemRef: ref, elemItems: items,
		cap: cap, elemMaxHas: emHas, elemMax: em, loc: loc, child: -1, parent: -1,
	}
	ss.scopes = append(ss.scopes, sc)
	switch elem {
	case ir.KindString, ir.KindBlob:
		// The corelib's own collector owns the whole element: both index bounds,
		// the per-element maxlen, the payload join and (for a string) the strict
		// UTF-8 decode. Generated code only routes the two events to it.
		sc.seq = fmt.Sprintf("_q%d", sc.id)
	case ir.KindStruct, ir.KindUnion:
		sc.ix = fmt.Sprintf("_ix%d", sc.id)
		sc.child = ss.object(g, locName+"_e", fmt.Sprintf("%s[this.%s]!", arrPath, sc.ix), ref.Target.Fields)
		ss.scopes[sc.child].parent = sc.id
	case ir.KindArray:
		// A native row arrives whole through arrayBegin/array<kind> at THIS scope,
		// keyed by its row index; only a wrapper row opens a scope of its own.
		if nativeArrayElem(items.Elem) {
			sc.row = fmt.Sprintf("_row%d", sc.id)
		} else {
			sc.ix = fmt.Sprintf("_ix%d", sc.id)
			sc.child = ss.array(g, locName+"_r", fmt.Sprintf("%s[this.%s]!", arrPath, sc.ix), loc+" row",
				items.Elem, items.ElemRef, items.ElemItems,
				capOf(items.HasCount, items.Count), items.ElemMaxHas, items.ElemMax)
			ss.scopes[sc.child].parent = sc.id
		}
	}
	return sc.id
}

// visStorage is storage() for the flat visitor, which is a class BESIDE the one
// it fills rather than a member of it -- CORELIB_PLAN §6.1.1 keeps decode_into
// off the generated object's surface -- and so cannot name a `private` backing
// field with a dot. Element access reaches it: TypeScript's `private` is a
// compile-time rule and bracket notation is its sanctioned escape hatch,
// emitting the very same property write. A Long-backed field therefore keeps
// bypassing its accessor pair on the hot path -- no getter call, no setter
// conversion.
func (g *gen) visStorage(recv string, f *ir.Field) string {
	if g.longBacked(f) {
		return fmt.Sprintf("%s[%q]", recv, "_"+f.Name)
	}
	return recv + "." + f.Name
}

// visitorName is the flat visitor class emitted for one generated type.
func visitorName(typeName string) string { return "_" + typeName + "Vis" }

// --- entry points -----------------------------------------------------------

// emitDecode generates the class-side half of the decode surface: the single
// public entry point decode(bytes), which runs the corelib's one-shot decode
// against this type's flat visitor.
//
// decode(bytes) is the whole decode surface the generated CLASS carries.
// CORELIB_PLAN §6.1.1 closes the generated object's name set to
// encode/decode/try_decode/serialize/deserialize/decoder and names `decode_from`
// and `decode_into` among the spellings a port must not invent beside them.
func (g *gen) emitDecode(f *tsfile, name string) {
	f.line("  static decode(bytes: Uint8Array): %s {", name)
	f.line("    const o = new %s();", name)
	f.line("    _decode(bytes, new %s(o, new PayloadAcc()));", visitorName(name))
	f.line("    return o;")
	f.line("  }")
}

// --- the flat visitor -------------------------------------------------------

// emitVisitor writes the flat visitor class for one generated type, preceded by
// its location constants.
func (g *gen) emitVisitor(f *tsfile, name string, fields []*ir.Field) {
	scopes := g.buildScopes(name, fields)

	f.line("// Dispatch locations for %s: one per sequence-framed scope in its tree.", name)
	f.line("// A field id is only unique WITHIN a scope -- a nested sequence opens a fresh")
	f.line("// id space -- so the visitor below keys every hook on (location, id).")
	for _, sc := range scopes {
		f.line("const %s = %d;", sc.name, sc.id)
	}
	f.blank()

	f.line("/**")
	f.line(" * Flat decode visitor for {@link %s}.", name)
	f.line(" *")
	f.line(" * corelib-ts's visitor is flat -- one object receives every callback at every")
	f.line(" * depth -- so the scope the walk is currently inside is tracked here, in `_c`,")
	f.line(" * and every hook keys on it. sequenceBegin sets it; sequenceEnd restores the")
	f.line(" * parent, which is static: the scopes form a tree, so no stack is needed.")
	f.line(" */")
	f.line("class %s implements Visitor {", visitorName(name))
	f.line("  private _c = %s;", scopes[0].name)
	for _, sc := range scopes {
		if sc.ix != "" {
			f.line("  private %s = 0;", sc.ix)
		}
		if sc.row != "" {
			f.line("  private %s: %s = [];", sc.row, g.matRowType(sc))
		}
		if sc.seq != "" {
			f.line("  private %s: %s | null = null;", sc.seq, g.seqClass(sc.elem))
		}
	}
	if anyBulk(scopes) {
		// ONE target for the whole visitor, re-pointed per array: the corelib holds
		// it only for that array's lifetime, so a fresh object per array would be an
		// allocation with nothing to show for it.
		f.line("  private readonly _bt: ArrayTarget = { out: [], min: 0, max: 0 };")
	}
	for _, sc := range scopes {
		for _, x := range sc.fields {
			if x.Kind != ir.KindArray || !nativeArrayElem(x.Elem) {
				continue
			}
			f.line("  private %s: %s = [];", arrayDst(sc, x), g.tsArrayType(x.Elem, x.ElemRef, x.ElemItems))
			if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
				f.line("  private %s: Uint8Array | null = null;", fp32RawScratch(sc, x))
				f.line("  private %s = false;", fp32RawSeen(sc, x))
			}
		}
	}
	f.line("  constructor(readonly o: %s, readonly a: PayloadAcc) {}", name)

	g.emitSeqHooks(f, scopes)
	g.emitScalarCb(f, scopes, "unsigned", unsignedKinds)
	g.emitScalarCb(f, scopes, "signed", signedKinds)
	g.emitFpCb(f, scopes)
	g.emitFixlenBegin(f, scopes)
	g.emitPayloadCb(f, scopes, "string")
	g.emitPayloadCb(f, scopes, "blob")
	g.emitArrayCbs(f, scopes)

	f.line("}")
	f.blank()
}

// scopeSwitch renders a hook body as one switch on the current location. Each
// arm is a scope whose body is non-empty; nothing is emitted when no scope has
// one, so the Visitor's own optional-method default stands and the corelib never
// looks the callback up.
func (g *gen) scopeSwitch(f *tsfile, sig string, arms map[int][]string, scopes []*tsScope, tail string) {
	if len(arms) == 0 {
		return
	}
	f.line("  %s {", sig)
	// A single-scope hook needs no dispatch at all: the one arm is guarded by its
	// own location test, which is cheaper than a switch and keeps the common
	// leaf-message shape (no nesting) exactly as monomorphic as it was.
	if len(arms) == 1 {
		for _, sc := range scopes {
			body, ok := arms[sc.id]
			if !ok {
				continue
			}
			f.line("    if (this._c !== %s) return%s;", sc.name, tail)
			for _, ln := range body {
				f.line("%s", ln)
			}
		}
		if tail != "" {
			f.line("    return%s;", tail)
		}
	} else {
		f.line("    switch (this._c) {")
		for _, sc := range scopes {
			body, ok := arms[sc.id]
			if !ok {
				continue
			}
			f.line("      case %s: {", sc.name)
			for _, ln := range body {
				f.line("    %s", ln)
			}
			if !endsWithReturn(body) {
				f.line("        break;")
			}
			f.line("      }")
		}
		f.line("      default: break;")
		f.line("    }")
		if tail != "" {
			f.line("    return%s;", tail)
		}
	}
	f.line("  }")
}

// endsWithReturn reports whether a rendered arm always leaves the callback, so
// the `break` that would follow it is unreachable.
func endsWithReturn(body []string) bool {
	if len(body) == 0 {
		return false
	}
	return strings.HasSuffix(strings.TrimSpace(body[len(body)-1]), "return true;")
}

// idSwitch renders the inner dispatch of one object scope: a switch on the field
// id, or a single `if` when the scope contributes exactly one arm.
func idSwitch(arms []string) []string {
	if len(arms) == 0 {
		return nil
	}
	out := []string{"    switch (id) {"}
	out = append(out, arms...)
	out = append(out, "    default: break;", "    }")
	return out
}

// --- sequenceBegin / sequenceEnd -------------------------------------------

// emitSeqHooks writes sequenceBegin / sequenceEnd.
//
// Returning false declines the scope: corelib-ts then skips the whole subtree --
// nothing inside is delivered and no sequenceEnd fires -- which is exactly §7.3's
// "treat it like an unknown id" for a sequence, and is why the stack is pushed
// only on the accepting arms.
func (g *gen) emitSeqHooks(f *tsfile, scopes []*tsScope) {
	arms := map[int][]string{}
	for _, sc := range scopes {
		var body []string
		if sc.isArr {
			if sc.child < 0 {
				continue // value elements open no scope
			}
			body = g.arrSeqArm(sc, scopes[sc.child])
		} else {
			if len(sc.seqChild) == 0 {
				continue
			}
			body = g.objSeqArm(sc, scopes)
		}
		if len(body) > 0 {
			arms[sc.id] = body
		}
	}
	if len(arms) == 0 {
		// No nested scope anywhere in this tree: every sequence is unknown and must
		// be declined, which is what one constant answer says.
		f.line("  sequenceBegin(): boolean { return false; }")
		return
	}
	g.scopeSwitch(f, "sequenceBegin(id: number): boolean", arms, scopes, " false")
	// The scope graph is a TREE: a location is assigned per (type, path), so a
	// scope is only ever entered from one parent, and sequenceEnd can restore it
	// from a static map rather than from a stack the visitor pushes. corelib-ts
	// fires sequenceEnd only for a scope this visitor actually accepted (a
	// declined subtree reports nothing at all), so `_c` always names one of the
	// cases below.
	f.line("  sequenceEnd(): void {")
	f.line("    switch (this._c) {")
	for _, sc := range scopes {
		if sc.parent < 0 {
			continue
		}
		f.line("    case %s: this._c = %s; break;", sc.name, scopes[sc.parent].name)
	}
	f.line("    default: break;")
	f.line("    }")
	f.line("  }")
}

// objSeqArm renders an object scope's SEQUENCE_START arms, one per struct/union
// or wrapper-array field.
func (g *gen) objSeqArm(sc *tsScope, scopes []*tsScope) []string {
	var arms []string
	for _, fld := range sc.fields {
		child, ok := sc.seqChild[fld.ID]
		if !ok {
			continue
		}
		ch := scopes[child]
		var b string
		if fld.Kind == ir.KindArray {
			// §7.4: an array wrapper REPLACES the value, so the destination starts
			// empty rather than merging into whatever the defaults put there.
			acc := g.visStorage("this.o", fld)
			b = fmt.Sprintf("    case %d: { const _t: %s = []; %s = _t; ", fld.ID, g.arrElemType(ch), acc)
			if ch.seq != "" {
				b += fmt.Sprintf("this.%s = %s; ", ch.seq, g.seqCtor(ch, "_t"))
			}
		} else {
			b = fmt.Sprintf("    case %d: { ", fld.ID)
		}
		b += fmt.Sprintf("this._c = %s; return true; }", ch.name)
		arms = append(arms, b)
	}
	return idSwitch(arms)
}

// arrSeqArm renders an array scope's element arm. The id IS the index, so there
// is no id test -- only the §5.1 capacity bound, then the gap-fill that places
// the element at its index (an interior element equal to the element default is
// omitted on the wire, MESSAGE_SPEC §2).
//
// The gap-fill is emitted here rather than taken from the corelib's ElementSeq
// because a framed element's default is a fresh OBJECT: ElementSeq writes one
// shared `def` into every gap, which is right for the immutable "" / empty bytes
// its string and blob collectors fill with and would alias every gap of a struct
// array onto one instance.
func (g *gen) arrSeqArm(sc *tsScope, child *tsScope) []string {
	out := []string{fmt.Sprintf("    const _t = %s;", sc.arrPath)}
	out = append(out, g.indexBound(sc)...)
	out = append(out, fmt.Sprintf("    while (_t.length <= id) _t.push(%s);", g.elemDefault(sc)))
	if sc.elem == ir.KindArray {
		// A ROW is itself an array wrapper, so a re-opened row index REPLACES what
		// an earlier opening built (§7.4) rather than merging into it -- unlike a
		// struct/union element, whose scope merges. A framed element is left alone
		// here: the gap-fill above already put an instance at the index, and the
		// element scope decodes into it.
		out = append(out, fmt.Sprintf("    const _e: %s = []; _t[id] = _e;", g.arrElemType(child)))
		if child.seq != "" {
			out = append(out, fmt.Sprintf("    this.%s = %s;", child.seq, g.seqCtor(child, "_e")))
		}
	}
	out = append(out,
		fmt.Sprintf("    this.%s = id;", sc.ix),
		fmt.Sprintf("    this._c = %s;", child.name),
		"    return true;")
	return out
}

// indexBound renders the two §5.1/§6.2.1 index bounds for an array scope whose
// elements generated code places itself: the schema capacity as validity, or --
// where the schema left the array open -- the receiver cap as policy. Never
// both: §6.2.1 keeps a cap off a field the schema already bounds.
func (g *gen) indexBound(sc *tsScope) []string {
	if sc.cap >= 0 {
		return []string{
			fmt.Sprintf("    if (id >= %d) throw new SofabError(SofabErrorCode.InvalidMsg, %q);",
				sc.cap, fmt.Sprintf("%s: array index above schema capacity %d", sc.loc, sc.cap)),
		}
	}
	if !g.limits.arrayHas {
		return nil
	}
	return []string{
		fmt.Sprintf("    if (id >= MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, %q + id + %q + MAX_DYN_ARRAY_COUNT);",
			sc.loc+": array index ", " exceeds the receiver cap "),
	}
}

// elemDefault is the value a gap-filled element of an array scope takes.
func (g *gen) elemDefault(sc *tsScope) string {
	switch sc.elem {
	case ir.KindArray:
		return "[]"
	default: // struct / union
		return "new " + g.typeName(sc.elemRef.Key) + "()"
	}
}

// seqClass / seqCtor name the corelib collector a string or blob wrapper array
// is driven through, and build one over the destination. Both index bounds, both
// element-length bounds, the payload join and the strict UTF-8 decode live there
// (ARCHITECTURE §8): none of it knows a schema, all of it arrives as arguments.
// arrayCountBound is the count reject for one native array field, emitted in
// arrayBegin ahead of the destination it sizes.
//
// TWO bounds land here and they are mutually exclusive by rule: an array the
// schema counts is INVALID above that count, and one the schema leaves uncounted
// is LimitExceeded above the receiver's configured cap (generator#388). §9.5: the
// caps govern ONLY what the schema left unbounded. CORELIB_PLAN §6.2.1: the two
// categories must not be folded, the cap being a policy rejection of well-formed
// bytes. `what` names the field for the message.
func (g *gen) arrayCountBound(cap int64, what string) string {
	if cap >= 0 {
		return fmt.Sprintf("if (count > %d) throw new SofabError(SofabErrorCode.InvalidMsg, %q); ",
			cap, fmt.Sprintf("%s: array count above schema capacity %d", what, cap))
	}
	if !g.limits.arrayHas {
		return ""
	}
	return fmt.Sprintf("if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, %q + MAX_DYN_ARRAY_COUNT); ",
		fmt.Sprintf("%s: array count above configured limit ", what))
}

func (g *gen) seqClass(elem ir.Kind) string {
	if elem == ir.KindBlob {
		return "BlobSeq"
	}
	return "StringSeq"
}

// seqCtor builds the collector for one string/blob wrapper array: the schema
// bounds first (`cap`, `elemMax`) and then, behind them, the receiver caps for
// whichever of the two the schema left open.
//
// All four are passed, always. The collector is where BOTH of this shape's
// receiver bounds land -- a wrapper array's elements never reach the generated
// visitor, neither their index nor their length word -- and an omitted argument
// is not "the corelib's default" but the format ceiling, i.e. no receiver bound
// at all. Each pair is exclusive by rule (§6.2.1): where the schema declares a
// `count`/`maxlen` the cap beside it is inert and the violation is INVALID, and
// where it does not, the cap governs and its violation is LimitExceeded.
func (g *gen) seqCtor(sc *tsScope, dst string) string {
	emax := int64(-1)
	if sc.elemMaxHas {
		emax = sc.elemMax
	}
	return fmt.Sprintf("new %s(%s, this.a, %d, %d, %q, %s, %s)",
		g.seqClass(sc.elem), dst, sc.cap, emax, sc.loc, g.arrayCap(), g.elemMaxCap(sc.elem))
}

// --- typed value callbacks --------------------------------------------------

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

// emitScalarCb writes the unsigned/signed callback: an arm per scope, each a
// switch on id applying that field's declared-width verdict (§7.1).
func (g *gen) emitScalarCb(f *tsfile, scopes []*tsScope, cb string, in kindSet) {
	arms := map[int][]string{}
	for _, sc := range scopes {
		if sc.isArr {
			continue // an array scope's elements are never bare scalars
		}
		var ids []string
		for _, x := range sc.fields {
			if !in(x) {
				continue
			}
			ids = append(ids, "    "+g.scalarArm(sc, x, cb))
		}
		if body := idSwitch(ids); body != nil {
			arms[sc.id] = body
		}
	}
	g.scopeSwitch(f, fmt.Sprintf("%s(id: number, v: number | bigint, lo: number, hi: number): void", cb), arms, scopes, "")
}

// scalarArm renders one field's store inside an integer callback.
func (g *gen) scalarArm(sc *tsScope, x *ir.Field, cb string) string {
	acc := g.visStorage(sc.path, x)
	switch x.Kind {
	case ir.KindBool:
		return fmt.Sprintf("case %d: %s = Boolean(v); break;", x.ID, acc)
	case ir.KindU64, ir.KindI64:
		return fmt.Sprintf("case %d: %s = %s; break;", x.ID, acc, g.big64(x.Kind == ir.KindI64))
	case ir.KindEnum:
		return fmt.Sprintf("case %d: %s = Number(v) as %s; break;", x.ID, acc, g.typeName(x.Ref.Key))
	}
	cond := widthCond("_v", x.Kind)
	if cond == "" {
		return fmt.Sprintf("case %d: %s = Number(v); break;", x.ID, acc)
	}
	return fmt.Sprintf("case %d: { const _v = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, %q); %s = _v; break; }",
		x.ID, narrowCast, cond, fmt.Sprintf("%s: value outside declared width %s", x.Name, x.Kind), acc)
}

// big64 is the 64-bit store expression for the configured int64 representation.
//
// `lo` / `hi` are the exact wire halves the varint reader already holds, so the
// Long and number forms never touch a bigint: that is what the withdrawn
// `Visitor.longs` channel bought, now available on every hook without an opt-in
// flag and without making narrow fields pay for it (corelib-ts#161).
func (g *gen) big64(signed bool) string {
	switch {
	case g.longScalars():
		return "Long.fromBits(lo, hi)"
	case g.numberScalars():
		return "Number(v)"
	default:
		return "typeof v === \"bigint\" ? v : BigInt(v)"
	}
}

// emitFpCb writes the fp32/fp64 callbacks. fp32 keeps the raw wire bits beside
// the value for a NaN: widening an fp32 signaling NaN into a JS double quiets
// it, so the bits are the only faithful carrier (§6.5).
func (g *gen) emitFpCb(f *tsfile, scopes []*tsScope) {
	a32, a64 := map[int][]string{}, map[int][]string{}
	for _, sc := range scopes {
		if sc.isArr {
			continue
		}
		var i32, i64 []string
		for _, x := range sc.fields {
			acc := g.visStorage(sc.path, x)
			switch x.Kind {
			case ir.KindFP32:
				if fp32RawCompanion(x) {
					i32 = append(i32, fmt.Sprintf("    case %d: { %s = v; %s = Number.isNaN(v) ? _fp32Raw(bits) : null; break; }",
						x.ID, acc, g.fp32RawStorage(sc.path, x)))
				} else {
					i32 = append(i32, fmt.Sprintf("    case %d: %s = v; break;", x.ID, acc))
				}
			case ir.KindFP64:
				i64 = append(i64, fmt.Sprintf("    case %d: %s = v; break;", x.ID, acc))
			}
		}
		if body := idSwitch(i32); body != nil {
			a32[sc.id] = body
		}
		if body := idSwitch(i64); body != nil {
			a64[sc.id] = body
		}
	}
	g.scopeSwitch(f, "fp32(id: number, v: number, bits: number): void", a32, scopes, "")
	g.scopeSwitch(f, "fp64(id: number, v: number): void", a64, scopes, "")
}

// emitFixlenBegin writes the fixlenBegin callback: the maxlen verdict, taken at
// the LENGTH WORD.
//
// It cannot live in the string/blob payload callback: that fires only once
// payload bytes arrive, so a message ending right after an over-maxlen length
// word would degrade to INCOMPLETE where §5.2.3 requires INVALID. The announced
// SUBTYPE is tested, not ignored: a `string` arriving at a `blob` field's id is a
// §7.3 wire-type mismatch and must be skipped, not bounded by this field.
func (g *gen) emitFixlenBegin(f *tsfile, scopes []*tsScope) {
	arms := map[int][]string{}
	for _, sc := range scopes {
		if sc.isArr {
			// A wrapper array's elements are bounded by the corelib collector, which
			// takes the same verdict at the same word.
			if sc.seq != "" {
				arms[sc.id] = []string{fmt.Sprintf("    this.%s?.begin(id, sub, total);", sc.seq)}
			}
			continue
		}
		var ids []string
		for _, x := range sc.fields {
			var sub, kind string
			var capConst string
			var capOn bool
			switch x.Kind {
			case ir.KindString:
				sub, kind, capConst, capOn = "FixlenSubtype.String", "string", "MAX_DYN_STRING_LEN", g.limits.stringHas
			case ir.KindBlob:
				sub, kind, capConst, capOn = "FixlenSubtype.Blob", "blob", "MAX_DYN_BLOB_LEN", g.limits.blobHas
			default:
				continue
			}
			// TWO bounds land here and they are mutually exclusive by rule: a field
			// the schema bounds is governed by its own `maxlen` and is INVALID above
			// it; a field the schema leaves unbounded is governed by the receiver's
			// configured cap and is LimitExceeded above it (generator#388). §9.5:
			// the caps govern ONLY what the schema left unbounded. CORELIB_PLAN
			// §6.2.1: the two categories must not be folded, the cap being a policy
			// rejection of well-formed bytes.
			switch {
			case x.HasMaxlen:
				ids = append(ids, fmt.Sprintf("    case %d: if (sub === %s && total > %d) throw new SofabError(SofabErrorCode.InvalidMsg, %q); break;",
					x.ID, sub, x.Maxlen, fmt.Sprintf("%s: %s byte length above schema maxlen %d", x.Name, kind, x.Maxlen)))
			case capOn:
				ids = append(ids, fmt.Sprintf("    case %d: if (sub === %s && total > %s) throw new SofabError(SofabErrorCode.LimitExceeded, %q + %s); break;",
					x.ID, sub, capConst, fmt.Sprintf("%s: %s byte length above configured limit ", x.Name, kind), capConst))
			}
		}
		if body := idSwitch(ids); body != nil {
			arms[sc.id] = body
		}
	}
	g.scopeSwitch(f, "fixlenBegin(id: number, sub: FixlenSubtype, total: number): void", arms, scopes, "")
}

// emitPayloadCb writes the string/blob callback.
//
// The maxlen verdict is taken against `total` -- the word that establishes the
// violation -- not against the assembled payload, so an over-maxlen field stays
// INVALID even when the message is truncated inside it and an over-long payload
// is never buffered (§5.2, issue #267).
func (g *gen) emitPayloadCb(f *tsfile, scopes []*tsScope, cb string) {
	want := ir.KindString
	if cb == "blob" {
		want = ir.KindBlob
	}
	arms := map[int][]string{}
	for _, sc := range scopes {
		if sc.isArr {
			if sc.seq != "" && sc.elem == want {
				arms[sc.id] = []string{fmt.Sprintf("    this.%s?.element(id, total, offset, src, start, end);", sc.seq)}
			}
			continue
		}
		var ids []string
		for _, x := range sc.fields {
			if x.Kind != want {
				continue
			}
			acc := g.visStorage(sc.path, x)
			var pre string
			if x.HasMaxlen {
				pre = fmt.Sprintf("if (total > %d) throw new SofabError(SofabErrorCode.InvalidMsg, %q); ",
					x.Maxlen, fmt.Sprintf("%s: %s byte length above schema maxlen %d", x.Name, cb, x.Maxlen))
			}
			var store string
			if want == ir.KindString {
				// A payload that arrived whole is transcoded straight out of the
				// caller's chunk: decodeUtf8 builds a new string, so nothing aliases
				// the input and the accumulator's copy is not needed at all.
				// A payload that arrived whole is transcoded straight out of the
				// caller's chunk: decodeUtf8 builds a new string, so nothing aliases
				// the input and the accumulator's copy is not needed at all. A split
				// one is joined first, and the destination is left untouched until
				// its last piece lands.
				store = fmt.Sprintf("if (offset === 0 && end - start === total) { %s = decodeUtf8(src, start, end); } else { const _p = this.a.take(total, offset, src, start, end); if (_p !== null) %s = decodeUtf8(_p); }", acc, acc)
			} else {
				store = fmt.Sprintf("{ const _p = this.a.take(total, offset, src, start, end); if (_p !== null) %s = _p; }", acc)
			}
			ids = append(ids, fmt.Sprintf("    case %d: { %s%s break; }", x.ID, pre, store))
		}
		if body := idSwitch(ids); body != nil {
			arms[sc.id] = body
		}
	}
	g.scopeSwitch(f, fmt.Sprintf("%s(id: number, total: number, offset: number, src: Uint8Array, start: number, end: number): void", cb), arms, scopes, "")
}

// --- native arrays ----------------------------------------------------------

// emitArrayCbs writes arrayBegin, the four element callbacks and arrayEnd.
//
// The over-count verdict is taken in arrayBegin, at the COUNT WORD, before a
// single element arrives: that is what keeps an over-count array INVALID rather
// than INCOMPLETE when the message is truncated inside it (§5.2, F-0032), and it
// costs nothing -- the corelib hands the declared count in.
func (g *gen) emitArrayCbs(f *tsfile, scopes []*tsScope) {
	begin, end := map[int][]string{}, map[int][]string{}
	uns, sig, f32, f64 := map[int][]string{}, map[int][]string{}, map[int][]string{}, map[int][]string{}

	for _, sc := range scopes {
		if sc.isArr {
			// A native matrix row: the array header arrives at THIS scope keyed by
			// the row index, so the row is reserved here and the elements land in
			// the row register the begin arm just set.
			if sc.row == "" {
				continue
			}
			b := []string{fmt.Sprintf("    const _t = %s;", sc.arrPath)}
			// `return`, not `break`: a scope arm is emitted bare when it is the only
			// one (no switch to break out of) and inside a `case` when it is not, and
			// leaving the callback is the right thing in both -- nothing else in the
			// hook would run.
			b = append(b, fmt.Sprintf("    if (kind !== ArrayKind.%s) return;", tsArrayKind(sc.elemItems.Elem)))
			b = append(b, g.indexBound(sc)...)
			if bound := g.arrayCountBound(capOf(sc.elemItems.HasCount, sc.elemItems.Count), sc.loc+" element"); bound != "" {
				b = append(b, "    "+strings.TrimSuffix(bound, " "))
			}
			b = append(b, "    while (_t.length <= id) _t.push([]);",
				fmt.Sprintf("    const _r: %s = []; _t[id] = _r; this.%s = _r;", g.matRowType(sc), sc.row))
			begin[sc.id] = b
			conv, dst := g.elemConv(sc.elemItems.Elem, sc.elemItems.ElemRef)
			line := g.rowElemLine(sc, conv)
			switch dst {
			case "unsigned":
				uns[sc.id] = line
			case "signed":
				sig[sc.id] = line
			case "fp32":
				f32[sc.id] = line
			case "fp64":
				f64[sc.id] = line
			}
			continue
		}

		var ib, ie, iu, is, i32, i64 []string
		for _, x := range sc.fields {
			if x.Kind != ir.KindArray || !nativeArrayElem(x.Elem) {
				continue
			}
			acc := g.visStorage(sc.path, x)
			cap := capOf(x.HasCount, x.Count)
			// The kind test comes FIRST, and both of the things after it depend on
			// that. The corelib routes an array header by id alone, so this arm also
			// receives a header whose element kind CONTRADICTS the declared one --
			// and such a field is skipped whole (§7.3), which means its count must
			// not be measured against this field's capacity and the destination must
			// not be cleared (a correctly-typed earlier occurrence survives, §7.4).
			// Braced: the arm declares `_d`, and sibling `case` clauses share ONE
			// block scope, so two array fields in a scope would redeclare it.
			b := fmt.Sprintf("    case %d: { ", x.ID)
			b += fmt.Sprintf("if (kind !== ArrayKind.%s) break; ", tsArrayKind(x.Elem))
			b += g.arrayCountBound(cap, x.Name)
			// Built once, assigned to the field AND kept in the register the element
			// arms read. A re-opened array id replaces (§7.4), so both are rebuilt.
			b += fmt.Sprintf("const _d: %s = []; %s = _d; this.%s = _d; ",
				g.tsArrayType(x.Elem, x.ElemRef, x.ElemItems), acc, arrayDst(sc, x))
			if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
				// A re-opened array id REPLACES (§7.4), so the companion is reset here
				// too. Sized from the announced count, which the guard above has
				// already bounded, so an over-count header cannot size this.
				b += fmt.Sprintf("%s = null; this.%s = new Uint8Array(count * 4); this.%s = false; ",
					g.fp32RawStorage(sc.path, x), fp32RawScratch(sc, x), fp32RawSeen(sc, x))
			}
			b += "break; }"
			ib = append(ib, b)

			if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
				ie = append(ie, fmt.Sprintf("    case %d: %s = this.%s ? this.%s : null; this.%s = null; break;",
					x.ID, g.fp32RawStorage(sc.path, x), fp32RawSeen(sc, x), fp32RawScratch(sc, x), fp32RawScratch(sc, x)))
			}

			conv, dst := g.elemConv(x.Elem, x.ElemRef)
			var line string
			if ec := widthCond("_e", x.Elem); ec != "" {
				// The element's declared width is a validity bound too (§7.1), checked
				// as each element arrives so a truncation behind an out-of-range
				// element cannot downgrade the verdict.
				// `_e` IS the store value: every kind that carries a narrow width
				// decodes to a plain number, so re-deriving it from `v` would run the
				// same conversion a second time, per element.
				_ = conv
				line = fmt.Sprintf("    case %d: { const _e = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, %q); this.%s[i] = _e; break; }",
					x.ID, narrowCast, ec, fmt.Sprintf("%s: value outside declared width %s", x.Name, x.Elem), arrayDst(sc, x))
			} else if x.Elem == ir.KindFP32 && fp32RawCompanion(x) {
				line = fmt.Sprintf("    case %d: { this.%s[i] = v; const _r = this.%s; if (_r !== null && (i + 1) * 4 <= _r.length) _fp32RawInto(_r, i * 4, bits); if (Number.isNaN(v)) this.%s = true; break; }",
					x.ID, arrayDst(sc, x), fp32RawScratch(sc, x), fp32RawSeen(sc, x))
			} else {
				line = fmt.Sprintf("    case %d: this.%s[i] = %s; break;", x.ID, arrayDst(sc, x), conv)
			}
			switch dst {
			case "unsigned":
				iu = append(iu, line)
			case "signed":
				is = append(is, line)
			case "fp32":
				i32 = append(i32, line)
			case "fp64":
				i64 = append(i64, line)
			}
			// No fill-to-count on arrayEnd: a declared `count: N` is a CAPACITY, not
			// a length (MESSAGE_SPEC §3), so the wire count IS the array's length.
		}
		put := func(m map[int][]string, ids []string) {
			if body := idSwitch(ids); body != nil {
				m[sc.id] = body
			}
		}
		put(begin, ib)
		put(end, ie)
		put(uns, iu)
		put(sig, is)
		put(f32, i32)
		put(f64, i64)
	}

	g.scopeSwitch(f, "arrayBegin(id: number, kind: ArrayKind, count: number): void", begin, scopes, "")
	g.emitArrayBulk(f, scopes)
	g.scopeSwitch(f, "arrayUnsigned(id: number, i: number, v: number | bigint, lo: number, hi: number): void", uns, scopes, "")
	g.scopeSwitch(f, "arraySigned(id: number, i: number, v: number | bigint, lo: number, hi: number): void", sig, scopes, "")
	g.scopeSwitch(f, "arrayFp32(id: number, i: number, v: number, bits: number): void", f32, scopes, "")
	g.scopeSwitch(f, "arrayFp64(id: number, i: number, v: number): void", f64, scopes, "")
	g.scopeSwitch(f, "arrayEnd(id: number): void", end, scopes, "")
}

// emitArrayBulk writes the bulk destination hand-off.
//
// The corelib calls it right after arrayBegin, which has already built the
// destination and put it in this field's register — so all this does is point the
// shared target at that register and state the element's declared bounds. The
// decoder then fills it directly and the element callback never fires for this
// array (measured: 2361 → 1721 Ir per element, −27%).
//
// The per-element arms stay emitted regardless. They are what runs for an array
// the hand-off declines, and what runs against a corelib that predates it — which
// is what makes taking it additive rather than a version bump.
func (g *gen) emitArrayBulk(f *tsfile, scopes []*tsScope) {
	arms := map[int][]string{}
	for _, sc := range scopes {
		if sc.isArr {
			continue
		}
		var ids []string
		for _, x := range sc.fields {
			if x.Kind != ir.KindArray || !nativeArrayElem(x.Elem) || !bulkEligible(x) {
				continue
			}
			lo, hi, _ := ir.NarrowRange(x.Elem)
			// The kind test is the same §7.3 guard arrayBegin takes: a contradicting
			// header is not this field's array, so it gets neither this field's
			// destination nor this field's bounds.
			ids = append(ids, fmt.Sprintf(
				"    case %d: { if (kind !== ArrayKind.%s) break; const _t = this._bt; _t.out = this.%s; _t.min = %d; _t.max = %d; return _t; }",
				x.ID, tsArrayKind(x.Elem), arrayDst(sc, x), lo, hi))
		}
		if body := idSwitch(ids); body != nil {
			arms[sc.id] = body
		}
	}
	g.scopeSwitch(f, "arrayBulk(id: number, kind: ArrayKind, count: number): ArrayTarget | null", arms, scopes, " null")
}

// rowElemLine renders a native matrix row's element store: straight into the row
// register arrayBegin set, with the element's declared-width verdict (§7.1) taken
// as each element arrives.
func (g *gen) rowElemLine(sc *tsScope, conv string) []string {
	elem := sc.elemItems.Elem
	if ec := widthCond("_e", elem); ec != "" {
		// As in the flat arm above: `_e` is the store value, not merely the value
		// the verdict was taken on.
		return []string{
			fmt.Sprintf("    const _e = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, %q);",
				narrowCast, ec, fmt.Sprintf("%s element: value outside declared width %s", sc.loc, elem)),
			fmt.Sprintf("    this.%s[i] = _e;", sc.row),
		}
	}
	return []string{fmt.Sprintf("    this.%s[i] = %s;", sc.row, conv)}
}

// elemConv gives a native array element's store expression and which element
// callback delivers it.
func (g *gen) elemConv(elem ir.Kind, ref *ir.TypeRef) (string, string) {
	switch elem {
	case ir.KindBool:
		return "Boolean(v)", "unsigned"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindBitfield:
		return "Number(v)", "unsigned"
	case ir.KindU64:
		return g.big64Arr(), "unsigned"
	case ir.KindI8, ir.KindI16, ir.KindI32:
		return "Number(v)", "signed"
	case ir.KindEnum:
		return fmt.Sprintf("Number(v) as %s", g.typeName(ref.Key)), "signed"
	case ir.KindI64:
		return g.big64Arr(), "signed"
	case ir.KindFP32:
		return "v", "fp32"
	case ir.KindFP64:
		return "v", "fp64"
	}
	return "v", "unsigned"
}

// big64Arr is the 64-bit ELEMENT store. Long-backed arrays take the wire halves
// directly (no bigint per element, which is the whole point of the Long modes);
// under `int64: bigint` the hook's own number-first value converts.
func (g *gen) big64Arr() string {
	if g.longArrays() {
		return "Long.fromBits(lo, hi)"
	}
	return "typeof v === \"bigint\" ? v : BigInt(v)"
}

// matRowType / arrElemType name the TypeScript element types the generated
// destinations hold, so a freshly built row or array is typed rather than any[].
func (g *gen) matRowType(sc *tsScope) string {
	return g.tsArrayType(sc.elemItems.Elem, sc.elemItems.ElemRef, sc.elemItems.ElemItems)
}

func (g *gen) arrElemType(sc *tsScope) string {
	return g.tsArrayType(sc.elem, sc.elemRef, sc.elemItems)
}

// bulkEligible reports whether a native array can be filled through the corelib's
// bulk destination hand-off (Visitor.arrayBulk) instead of one callback per
// element.
//
// Exactly the kinds that carry a DECLARED NARROW WIDTH qualify, and that is not a
// coincidence: the hand-off's whole contract is that the consumer states the
// element's bounds and the decoder applies them as it fills (§7.1). A kind with
// no declared width has no bounds to state — inventing a pair would reject values
// the schema permits — and a kind whose destination is not a plain JS number
// (u64/i64 → bigint or Long, boolean, fp32 with its raw-bits companion) cannot be
// written into directly at all. Both decline, and keep the element callbacks.
func bulkEligible(x *ir.Field) bool {
	if _, _, ok := ir.NarrowRange(x.Elem); !ok {
		return false
	}
	// ...and only where the array can be long enough to pay for the offer.
	//
	// corelib-ts gates the hand-off at BULK_MIN elements because the offer is a
	// call out to the visitor and costs more than a short array's fill saves. A
	// declared `count` is a CAPACITY, so an array declared below that threshold can
	// never reach it on the wire — the offer would be made, refused by the corelib,
	// and paid for anyway on every message. Leaving the arm out is the same verdict
	// taken statically, where it costs nothing at all. An array the schema leaves
	// open keeps its arm: only the wire knows how long it is, and the corelib's own
	// gate decides per message.
	if !x.HasCount {
		return true
	}
	return x.Count >= tsBulkMin
}

// tsBulkMin mirrors corelib-ts's BULK_MIN. It is a threshold, not a contract: if
// the two drift, the corelib still refuses the short arrays this lets through, and
// the only cost is the offer nobody wanted.
const tsBulkMin = 16

// anyBulk reports whether any scope of the tree has an array the hand-off covers.
func anyBulk(scopes []*tsScope) bool {
	for _, sc := range scopes {
		for _, x := range sc.fields {
			if x.Kind == ir.KindArray && nativeArrayElem(x.Elem) && bulkEligible(x) {
				return true
			}
		}
	}
	return false
}

// arrayDst names the visitor-private register that holds a native array's
// destination for the duration of its element run.
//
// The element callbacks are the hottest sites in a decode -- one per element, and
// an array is the only field that produces more than one -- so reaching the
// destination through `this.o.<field>` there costs two property loads per element
// for a value that cannot change while the run lasts. arrayBegin already builds
// that array; keeping it is free, and the element arm then does one load.
//
// It is the same register the nested matrix rows already use (`_rowN`), applied
// one level up. Scoped by location as well as name: the same field name may occur
// in two scopes of one tree.
func arrayDst(sc *tsScope, f *ir.Field) string {
	return fmt.Sprintf("_a%d%s", sc.id, exported(f.Name))
}

// fp32RawScratch / fp32RawSeen name the visitor-private slots that assemble an
// fp32 array's raw companion. Scoped by location as well as name: the same field
// name may occur in two scopes of one tree.
func fp32RawScratch(sc *tsScope, f *ir.Field) string {
	return fmt.Sprintf("_raw%d%s", sc.id, exported(f.Name))
}

func fp32RawSeen(sc *tsScope, f *ir.Field) string {
	return fmt.Sprintf("_rawNaN%d%s", sc.id, exported(f.Name))
}

// --- shared -----------------------------------------------------------------

func capOf(hasCount bool, count int64) int64 {
	if !hasCount {
		return -1
	}
	return count
}

// narrowCast reads an integer hook's number-first value as the `number` a NARROW
// destination holds, without a conversion call.
//
// It is an assertion, and the declared-width guard that follows it is what makes
// the assertion true. corelib-ts hands over a `bigint` in exactly one case --
// a magnitude above 2^53-1 -- and every narrow width tops out at 2^32-1, so such
// a value fails the guard and throws before it can be stored. A `Number(v)` here
// would convert a value the very next line rejects, per scalar field and per
// array element, on the hot path. Where no guard follows (bitfield, enum) the
// conversion stays.
const narrowCast = "v as number"

// widthCond renders the out-of-range test for a narrow declared width, or "" for
// u64/i64 and the kinds that carry no width.
func widthCond(v string, k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("%s < %d || %s > %d", v, lo, v, hi)
	}
	return fmt.Sprintf("%s > %d", v, hi)
}

func nativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindEnum, ir.KindBool, ir.KindBitfield:
		return true
	}
	return false
}

// tsArrayKind names the corelib ArrayKind a declared native array element is
// delivered as, so the visitor can tell "this header is for my field" from "this
// header contradicts my field and must be skipped" (MESSAGE_SPEC §7.3).
//
// For a fixlen array the kind names the ELEMENT SUBTYPE, not merely "fixlen": an
// fp64 header at a declared fp32 array is a contradiction like any other.
func tsArrayKind(elem ir.Kind) string {
	switch elem {
	// An ENUM is signed on the wire -- serialize writes it with writeSignedArray,
	// and its elements arrive on arraySigned. Classifying it as Unsigned here made
	// arrayBegin reject every enum array as a §7.3 contradiction, so its count
	// bound never ran and a re-opened id merged into the old value instead of
	// replacing it (§7.4), while the elements still landed through arraySigned.
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "Signed"
	case ir.KindFP32:
		return "Fp32"
	case ir.KindFP64:
		return "Fp64"
	}
	// u8..u64, bool and bitfield all travel as unsigned elements.
	return "Unsigned"
}

// --- module-level decode support -------------------------------------------

// emitDecoderClass writes the public incremental decoder for one message.
func (g *gen) emitDecoderClass(f *tsfile, name string) {
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
	f.line("  private readonly out: %s;", name)
	f.line("  private readonly is: IStream;")
	f.blank()
	f.line("  constructor(out?: %s) {", name)
	f.line("    this.out = out ?? new %s();", name)
	f.line("    this.is = new IStream(new %s(this.out, new PayloadAcc()));", visitorName(name))
	f.line("  }")
	f.blank()
	f.line("  /**")
	f.line("   * Feed the next chunk, of any size. Returns `Complete` if it ended on a")
	f.line("   * field boundary, `Incomplete` if it ended mid-field -- neither answers")
	f.line("   * whether the MESSAGE is done.")
	f.line("   *")
	f.line("   * @throws SofabError the bytes are malformed; terminal.")
	f.line("   */")
	f.line("  feed(chunk: Uint8Array): DecodeStatus { return this.is.feed(chunk); }")
	f.blank()
	f.line("  /** The outcome for everything fed so far, without feeding more. */")
	f.line("  get status(): DecodeStatus { return this.is.status(); }")
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
	f.line("    if (this.is.status() !== DecodeStatus.Complete) {")
	f.line("      throw new SofabError(SofabErrorCode.Incomplete, \"%s: stream ended mid-field\");", name)
	f.line("    }")
	f.line("    return this.out;")
	f.line("  }")
	f.line("}")
	f.blank()
}

// fp32BitsHelper turns the fp32 hook's 32-bit wire word back into the four bytes
// the generated companion slot holds (MESSAGE_SPEC §4.6). The word is what the
// corelib delivers -- a number costs nothing to pass, where the byte view it
// replaced was an allocation per value and a borrowed slice §6.7 forbids -- so
// the four bytes are materialized here, only for the NaN that needs them.
const fp32BitsHelper = `// _fp32RawInto writes the four little-endian wire bytes of an fp32's 32-bit word
// to out[off].
function _fp32RawInto(out: Uint8Array, off: number, bits: number): void {
  out[off] = bits & 0xff;
  out[off + 1] = (bits >>> 8) & 0xff;
  out[off + 2] = (bits >>> 16) & 0xff;
  out[off + 3] = (bits >>> 24) & 0xff;
}

// _fp32Raw is the scalar flavour: a fresh 4-byte companion for one value. Built
// only for a NaN, which is the only value a JS number cannot re-encode exactly.
function _fp32Raw(bits: number): Uint8Array {
  const out = new Uint8Array(4);
  _fp32RawInto(out, 0, bits);
  return out;
}`

// fp32RawHelper is the scalar half of the fp32 raw-bits channel (MESSAGE_SPEC
// §4.6, generator#235). A JS number is a 64-bit double, and widening an fp32
// SIGNALING NaN into one quiets it (0x7F800001 -> 0x7FC00001), so the number
// alone can never re-encode that field bit-for-bit. Decode therefore reads the
// four wire bytes (Cursor.readFp32Raw) and widens them here for the value
// consumer, keeping a copy of the bytes only when the value is a NaN. Every
// non-NaN fp32 narrows back to its own bits exactly, so nothing else needs them.
// Emitted only when the schema has an fp32 scalar field.
const fp32RawHelper = `// Shared 4-byte scratch word for the fp32 raw-bytes path below: widening an
// fp32's wire bytes allocates nothing per decode.
const _fp32Buf = new ArrayBuffer(4);
const _fp32Bytes = new Uint8Array(_fp32Buf);
const _fp32View = new DataView(_fp32Buf);

// _fp32FromRaw widens the four little-endian wire bytes at raw[off] to a JS
// number. A signaling NaN quiets in the widening (0x7F800001 -> 0x7FC00001),
// which is exactly why the caller keeps the bytes beside the number.
function _fp32FromRaw(raw: Uint8Array, off: number): number {
  _fp32Bytes[0] = raw[off]!;
  _fp32Bytes[1] = raw[off + 1]!;
  _fp32Bytes[2] = raw[off + 2]!;
  _fp32Bytes[3] = raw[off + 3]!;
  return _fp32View.getFloat32(0, true);
}`

// fp32ArrayRawHelper is the array half of the same channel: it renders an fp32
// array's wire payload from the value, substituting the captured wire bytes ONLY
// for an element that is still the NaN it decoded as. An element the caller has
// changed since -- and any element whose captured bytes are not themselves a NaN
// -- re-renders from its number, so a hand-set value is never overwritten by a
// stale capture. Emitted only when the schema has a native fp32 array field.
const fp32ArrayRawHelper = `// _fp32RawFrom narrows v to fp32 and writes its four little-endian wire bytes to
// out[off], through the same shared scratch word _fp32FromRaw reads back.
function _fp32RawFrom(out: Uint8Array, off: number, v: number): void {
  _fp32View.setFloat32(0, v, true);
  out[off] = _fp32Bytes[0]!;
  out[off + 1] = _fp32Bytes[1]!;
  out[off + 2] = _fp32Bytes[2]!;
  out[off + 3] = _fp32Bytes[3]!;
}

// _fp32ArrayRaw renders an fp32 array's wire payload (count * 4 little-endian
// bytes) from vals, keeping the captured wire bits of every element that is
// STILL the NaN it decoded as. An element the caller has changed since
// re-renders from its number, so a hand-set value never loses to a stale
// capture: only the bits a JS number cannot carry come from ` + "`raw`" + `.
//
// Both directions go through the module-level 4-byte scratch (_fp32FromRaw /
// _fp32RawFrom) rather than a DataView built over the payload: this runs on
// every encode of an fp32 array that decoded with a NaN, and the two DataViews
// it used to construct per call are a heavyweight allocation apiece.
function _fp32ArrayRaw(vals: readonly number[], raw: Uint8Array): Uint8Array {
  const out = new Uint8Array(vals.length * 4);
  for (let i = 0, o = 0; i < vals.length; i++, o += 4) {
    const v = vals[i]!;
    if (Number.isNaN(v) && o + 4 <= raw.length && Number.isNaN(_fp32FromRaw(raw, o))) {
      out[o] = raw[o]!;
      out[o + 1] = raw[o + 1]!;
      out[o + 2] = raw[o + 2]!;
      out[o + 3] = raw[o + 3]!;
    } else {
      _fp32RawFrom(out, o, v);
    }
  }
  return out;
}`

// longArrEqHelper is the Long[] flavour of elementsEqual: Long elements are object
// identities, so the sparse-omission default compare goes by (low, high) word
// pairs instead of element !==. Emitted only when some Long-backed 64-bit
// array carries a non-empty schema default (see scanHelpers).
const longArrEqHelper = `// longArrEq is elementsEqual for Long[]: element-wise compare by (low, high) word pair
// (Long objects are identities, so !== would never match a default literal).
function longArrEq(a: readonly Long[], b: readonly Long[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i]!.low !== b[i]!.low || a[i]!.high !== b[i]!.high) return false;
  return true;
}`
