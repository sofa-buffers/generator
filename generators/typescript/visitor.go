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
			f.line("  private %s: %s = %s;", sc.row, g.matRowType(sc),
				g.emptyRegisterLit(sc.elemItems.Elem, sc.elemItems.ElemRef, g.matRowType(sc)))
		}
		if sc.seq != "" {
			f.line("  private %s: %s | null = null;", sc.seq, g.seqClass(sc.elem))
		}
	}
	g.emitBulkState(f, g.bulkNeedsOf(scopes))
	for _, sc := range scopes {
		for _, x := range sc.fields {
			if x.Kind != ir.KindArray || !nativeArrayElem(x.Elem) {
				continue
			}
			f.line("  private %s: %s = %s;", arrayDst(sc, x),
				g.tsArrayType(x.Elem, x.ElemRef, x.ElemItems),
				g.emptyRegisterLit(x.Elem, x.ElemRef, g.tsArrayType(x.Elem, x.ElemRef, x.ElemItems)))
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
		done := false
		for _, sc := range scopes {
			body, ok := arms[sc.id]
			if !ok {
				continue
			}
			f.line("    if (this._c !== %s) return%s;", sc.name, tail)
			for _, ln := range body {
				f.line("%s", ln)
			}
			done = endsWithReturn(body)
		}
		// ...unless the one arm already left. A bare scope arm that ends in
		// `return _t;` -- which is every arrayBulk arm for a matrix row -- would
		// otherwise be followed by an unreachable `return null;`: legal under the
		// emitted tsconfig, but an error for a consumer building with
		// `allowUnreachableCode: false` or linting `no-unreachable`.
		if tail != "" && !done {
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
	last := strings.TrimSpace(body[len(body)-1])
	return strings.HasPrefix(last, "return ") && strings.HasSuffix(last, ";")
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
	}
	init, cond, cast, what := g.guarded(x.Kind, x.Ref)
	if cond == "" {
		return fmt.Sprintf("case %d: %s = %s%s; break;", x.ID, acc, init, cast)
	}
	return fmt.Sprintf("case %d: { const _v = %s; if (%s) throw new SofabError(SofabErrorCode.InvalidMsg, %q); %s = _v%s; break; }",
		x.ID, init, cond, fmt.Sprintf("%s: value outside declared %s", x.Name, what), acc, cast)
}

// guarded describes how a value the schema declares with Kind k -- and, for a
// composite kind, the named type `ref` carries the rest of that declaration --
// is read, tested and stored:
//
//	init   the expression reading the hook's `v` into the temporary `_v`
//	cond   the reject test on `_v`, or "" when nothing reachable breaches it
//	cast   the suffix the value is stored through, "" for most kinds
//	what   how the breached declaration is named in the InvalidMsg text
//
// What the schema declares is what binds, and every declaration binds a WIDTH
// (MESSAGE_SPEC §7.1, documentation#32): a value outside the declared range is
// malformed input and fails the decode, never clamped and never kept. An `enum`
// and a `bitfield` bind the width their declaration IMPLIES (§1) -- the smallest
// signed type holding every constant, the smallest unsigned type holding the
// highest `pos` -- and declaredWidthCond answers for those.
//
// One clause serves every position: emitScalarCb walks each id scope, so the
// scalar, struct-member, struct-array-member and union-member stores are ONE arm
// per kind serving four positions, and the native array element and matrix row
// element carry the same clause through the same helper. A value the schema does
// not declare therefore gets one verdict wherever it lands (generator#516).
func (g *gen) guarded(k ir.Kind, ref *ir.TypeRef) (init, cond, cast, what string) {
	switch k {
	case ir.KindEnum:
		// The comparison runs on a plain `number`, and only the store carries the
		// enum type: TypeScript narrows a value typed as an enum to the members it
		// declares, so a comparison against a bound outside them is one the compiler
		// rejects outright — and everything this clause exists to refuse is outside
		// them.
		return "Number(v)", declaredWidthCond("_v", k, ref),
			" as " + g.typeName(ref.Key), declaredWidthWhat(k)
	case ir.KindBitfield:
		// The unsigned callback delivers a number below 2^53 and a bigint above,
		// so a wide bitfield has to normalise — `Number(v)` would silently round
		// away the top bits of the value this field exists to carry.
		if wideBitfield(ref) {
			return "BigInt(v)", declaredWidthCond("_v", k, ref), "", declaredWidthWhat(k)
		}
		return narrowCast, declaredWidthCond("_v", k, ref), "", declaredWidthWhat(k)
	}
	if cond := widthCond("_v", k); cond != "" {
		return narrowCast, cond, "", "width " + k.String()
	}
	return "Number(v)", "", "", ""
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

// corelib-ts#177 made the bulk hand-off the ONLY way an array's elements reach a
// visitor: `Visitor.arrayUnsigned` / `arraySigned` / `arrayFp32` / `arrayFp64` are
// gone, and `arrayBulk` answers with the destination the decoder then fills
// itself, one write per element and no callback at all. Declining (returning
// `null`, or declaring no hook) is the §6.7.2 `skip` intent: the elements are
// walked over and never decoded into existence.
//
// So there is no longer a threshold to weigh, and no "eligible" subset: a
// generated class wants every one of its declared arrays, so it offers a
// destination for every one of them. What varies per element kind is only WHICH
// destination, and whether the member the schema declares can BE that destination
// or has to be filled from a scratch one afterwards.
//
//	u8..u32, i8..i32     values  -- the member array itself
//	enum, narrow bitfield values  -- the member array itself
//	u64/i64 (long modes) longs   -- the member Long[] itself
//	u64/i64 (bigint)     values  -- scratch, then one BigInt per element
//	bool                 values  -- scratch, then Boolean per element
//	wide bitfield        values  -- scratch, then one BigInt per element
//	fp32                 bits    -- scratch, read back through a Float32Array view
//	fp64                 f64     -- scratch, copied into the member number[]
//
// The member types are unchanged, which is the point: generator#549 measured
// typed-array MEMBERS as a loss and as a breaking change to every generated class,
// and that verdict stands. The destination shape and the member type are separate
// decisions, and only the destination moves here.

// emptyArrayLit / newArrayDecl / newArrayExpr spell "an array of this element
// kind" for the two member shapes: a plain `[]`, whose type has to be written out
// because an empty literal is `never[]`, and a typed array, which carries its own
// type and takes the length in the constructor.
// emptyRegisterLit is emptyArrayLit with the member's own type asserted on, for
// the private registers whose declared type is the enum alias.
func (g *gen) emptyRegisterLit(elem ir.Kind, ref *ir.TypeRef, typ string) string {
	if elem == ir.KindEnum {
		return fmt.Sprintf("new %s(0) as %s", g.tsTypedArray(elem, ref), typ)
	}
	return g.emptyArrayLit(elem, ref)
}

func (g *gen) emptyArrayLit(elem ir.Kind, ref *ir.TypeRef) string {
	if t := g.tsTypedArray(elem, ref); t != "" {
		return fmt.Sprintf("new %s(0)", t)
	}
	return "[]"
}

func (g *gen) newArrayDecl(elem ir.Kind, ref *ir.TypeRef, typ string) string {
	if g.tsTypedArray(elem, ref) != "" {
		return "" // `new Uint16Array(count)` carries its own type
	}
	return ": " + typ
}

// newArrayExpr builds the destination at the WIRE count, which arrayBegin has
// already measured against the schema capacity or the receiver cap on the line
// before -- so a forged count never reaches an allocation (§6.2.1).
//
// An enum's container takes numbers, so the member's alias is asserted once here
// rather than carried by every element.
func (g *gen) newArrayExpr(elem ir.Kind, ref *ir.TypeRef, typ string) string {
	t := g.tsTypedArray(elem, ref)
	if t == "" {
		return "[]"
	}
	if elem == ir.KindEnum {
		return fmt.Sprintf("new %s(count) as %s", t, typ)
	}
	return fmt.Sprintf("new %s(count)", t)
}

// bulkShape names the ArrayTarget destination one element kind is filled through.
//
// Every native array is a TYPED ARRAY on the message object, so in every case the
// member IS the destination and the corelib fills it in place: nothing is copied
// out afterwards, nothing is converted, and generated code never touches an
// element. What differs is only which of the corelib's five shapes carries it.
type bulkShape int

const (
	// IntegerArrayTarget.typed -- every integer width, including the 64-bit pair
	// (filled through the halves of its own buffer), an `enum` at the signed width
	// its constants imply and a `bitfield` at the unsigned width its highest `pos`
	// implies (§1).
	shTyped bulkShape = iota
	// BoolArrayTarget.bool -- one byte per element, and the only integer shape
	// with no bound: §4.4 gives a boolean none, and the corelib normalizes every
	// non-zero to 1 while filling rather than masking it (256 would become 0).
	shBool
	// FloatArrayTarget.bits over the Float32Array member's OWN buffer. Not `f32`:
	// that destination stores values, and an fp32 signaling NaN cannot survive
	// being widened to a double and narrowed back (§4.6/§6.5). The words land
	// exactly and reading the member still gives the values -- one buffer, both.
	shBits
	// FloatArrayTarget.f64 -- a double carries all 64 bits, payload NaNs included,
	// so there is nothing a bits channel would add.
	shF64
	// IntegerArrayTarget.longs -- `Long[]` under `int64: long`/`number`, the one
	// member that is deliberately not a typed array (see tsTypedArray).
	shLongs
)

// bulkPlan is what the two hooks need to know about one native array.
type bulkPlan struct {
	kind     string      // ArrayKind arm name, for the §7.3 contradiction test
	shape    bulkShape   // which destination
	elemKind ir.Kind     // the element kind this plan was made for
	elemRef  *ir.TypeRef // its named type, for an enum or a bitfield
}

// planBulk picks the destination for one native array element kind.
//
// There is no residue and no second pass anywhere in here, and that is the point
// of the mapping: every bound this format has is a WIDTH (§1 included, since the
// closed-set reading was withdrawn), a width is an interval, an interval travels
// with the destination, and the container the member is held in enforces the same
// width a second time for free. So the decoder compares once while filling and
// generated code compares nothing at all.
func (g *gen) planBulk(elem ir.Kind, ref *ir.TypeRef) bulkPlan {
	p := bulkPlan{kind: tsArrayKind(elem), elemKind: elem, elemRef: ref}
	switch elem {
	case ir.KindBool:
		p.shape = shBool
	case ir.KindFP32:
		p.shape = shBits
	case ir.KindFP64:
		p.shape = shF64
	case ir.KindU64, ir.KindI64:
		if g.longArrays() {
			p.shape = shLongs
		} else {
			p.shape = shTyped
		}
	default:
		p.shape = shTyped
	}
	return p
}

// elemBound is the element interval the hand-off carries, as the four unsigned
// 32-bit halves IntegerArrayTarget states it in (two's complement for a signed
// array). It is the SCHEMA's bound, so its violation is INVALID (§7.1).
func (g *gen) elemBound(elem ir.Kind, ref *ir.TypeRef) [4]uint32 {
	switch elem {
	case ir.KindU64:
		return boundHalves(0, -1)
	case ir.KindI64:
		return [4]uint32{0, 0x80000000, 0xffffffff, 0x7fffffff}
	case ir.KindEnum:
		// The width the declaration implies, and the WHOLE rule (§1): an undeclared
		// value inside it is valid, so there is no set to re-check.
		if lo, hi, ok := ir.EnumWidthRange(ref); ok {
			return boundHalves(lo, hi)
		}
		return [4]uint32{0, 0x80000000, 0xffffffff, 0x7fffffff}
	case ir.KindBitfield:
		// The same one width down. An undeclared BIT inside it is valid.
		if hi, ok := ir.BitfieldWidthMax(ref); ok {
			return [4]uint32{0, 0, uint32(hi), uint32(hi >> 32)}
		}
		return boundHalves(0, -1)
	}
	if lo, hi, ok := ir.NarrowRange(elem); ok {
		return boundHalves(lo, hi)
	}
	return boundHalves(0, -1)
}

// boundHalves splits a declared interval into the four halves, hi == -1 meaning
// the widest unsigned value (0xffffffff_ffffffff, which no int64 can name).
func boundHalves(lo, hi int64) [4]uint32 {
	h := uint64(hi)
	if hi == -1 {
		h = ^uint64(0)
	}
	l := uint64(lo)
	return [4]uint32{uint32(l), uint32(l >> 32), uint32(h), uint32(h >> 32)}
}

// bulkNeeds records which reusable target objects a visitor class declares.
type bulkNeeds struct{ typed, bool_, bits, f64, longs bool }

func (g *gen) bulkNeedsOf(scopes []*tsScope) bulkNeeds {
	var n bulkNeeds
	add := func(p bulkPlan) {
		switch p.shape {
		case shTyped:
			n.typed = true
		case shBool:
			n.bool_ = true
		case shBits:
			n.bits = true
		case shF64:
			n.f64 = true
		case shLongs:
			n.longs = true
		}
	}
	for _, sc := range scopes {
		if sc.isArr {
			if sc.row != "" {
				add(g.planBulk(sc.elemItems.Elem, sc.elemItems.ElemRef))
			}
			continue
		}
		for _, x := range sc.fields {
			if x.Kind == ir.KindArray && nativeArrayElem(x.Elem) {
				add(g.planBulk(x.Elem, x.ElemRef))
			}
		}
	}
	return n
}

// emitBulkState declares the reusable target objects.
//
// ONE per destination shape for the whole visitor, re-pointed per array rather
// than rebuilt: the corelib holds a target only for that array's lifetime, and
// arrays never overlap -- an array's elements all arrive before anything else is
// delivered -- so one of each is enough and a fresh one per array would be an
// allocation with nothing to show for it. The shapes are kept APART rather than
// sharing one object with several optional slots, because the corelib refuses a
// target naming more than one destination (§6.3).
//
// There are no scratch buffers here any more, and that is the whole change: with
// the member itself as the destination there is nothing to fill on the way to it.
func (g *gen) emitBulkState(f *tsfile, n bulkNeeds) {
	if n.typed {
		f.line("  private readonly _tt: IntegerArrayTarget = { typed: new Uint8Array(0), minLo: 0, minHi: 0, maxLo: 0, maxHi: 0 };")
	}
	if n.longs {
		f.line("  private readonly _tl: IntegerArrayTarget = { longs: [], minLo: 0, minHi: 0, maxLo: 0, maxHi: 0 };")
	}
	if n.bool_ {
		f.line("  private readonly _tq: BoolArrayTarget = { bool: new Uint8Array(0) };")
	}
	if n.bits {
		f.line("  private readonly _tb: FloatArrayTarget = { bits: new Uint32Array(0) };")
	}
	if n.f64 {
		f.line("  private readonly _td: FloatArrayTarget = { f64: new Float64Array(0) };")
	}
}

func (g *gen) emitArrayCbs(f *tsfile, scopes []*tsScope) {
	begin, bulk := map[int][]string{}, map[int][]string{}

	for _, sc := range scopes {
		if sc.isArr {
			// A native matrix row: the array header arrives at THIS scope keyed by
			// the row index, so the row is reserved here and handed over as this
			// row's destination.
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
			b = append(b, fmt.Sprintf("    while (_t.length <= id) _t.push(%s);",
				g.emptyArrayLit(sc.elemItems.Elem, sc.elemItems.ElemRef)),
				fmt.Sprintf("    const _r%s = %s; _t[id] = _r; this.%s = _r;",
					g.newArrayDecl(sc.elemItems.Elem, sc.elemItems.ElemRef, g.matRowType(sc)),
					g.newArrayExpr(sc.elemItems.Elem, sc.elemItems.ElemRef, g.matRowType(sc)), sc.row))
			begin[sc.id] = b

			p := g.planBulk(sc.elemItems.Elem, sc.elemItems.ElemRef)
			bulk[sc.id] = append(
				[]string{fmt.Sprintf("    if (kind !== ArrayKind.%s) return null;", p.kind)},
				g.bulkOffer(p, "this."+sc.row, "    ")...)
			continue
		}

		var ib, ibk []string
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
			// Built once, assigned to the field AND kept in the register the offer
			// and the arrayEnd pass read. A re-opened array id replaces (§7.4), so
			// both are rebuilt.
			b += fmt.Sprintf("const _d%s = %s; %s = _d; this.%s = _d; ",
				g.newArrayDecl(x.Elem, x.ElemRef, g.tsArrayType(x.Elem, x.ElemRef, x.ElemItems)),
				g.newArrayExpr(x.Elem, x.ElemRef, g.tsArrayType(x.Elem, x.ElemRef, x.ElemItems)),
				acc, arrayDst(sc, x))
			b += "break; }"
			ib = append(ib, b)

			p := g.planBulk(x.Elem, x.ElemRef)
			arm := []string{fmt.Sprintf("    case %d: {", x.ID),
				fmt.Sprintf("      if (kind !== ArrayKind.%s) break;", p.kind)}
			arm = append(arm, g.bulkOffer(p, "this."+arrayDst(sc, x), "      ")...)
			arm = append(arm, "    }")
			ibk = append(ibk, arm...)
			// No arrayEnd arm, for any kind: the member IS the destination, so there
			// is nothing to copy out and nothing the interval could not state. And no
			// fill-to-count either -- a declared `count: N` is a CAPACITY, not a
			// length (MESSAGE_SPEC §3), so the wire count IS the array's length.
		}
		put := func(m map[int][]string, ids []string) {
			if body := idSwitch(ids); body != nil {
				m[sc.id] = body
			}
		}
		put(begin, ib)
		put(bulk, ibk)
	}

	g.scopeSwitch(f, "arrayBegin(id: number, kind: ArrayKind, count: number): void", begin, scopes, "")
	g.scopeSwitch(f, "arrayBulk(id: number, kind: ArrayKind, count: number): ArrayTarget | null", bulk, scopes, " null")
}

// bulkOffer renders the body of one arrayBulk arm: point the shape's target at
// the member, state the element interval where the shape carries one, and hand it
// over. Three or four lines, and no arm anywhere that reads or writes an element.
func (g *gen) bulkOffer(p bulkPlan, dst, ind string) []string {
	var out []string
	line := func(s string, a ...any) { out = append(out, ind+fmt.Sprintf(s, a...)) }
	switch p.shape {
	case shTyped:
		// The member itself, at its declared width. The bound still travels and is
		// still compared: a typed store MASKS, and §7.1 forbids masking an
		// over-width element away rather than refusing it.
		line("const _t = this._tt; _t.typed = %s;", dst)
	case shLongs:
		line("const _t = this._tl; _t.longs = %s;", dst)
	case shBool:
		// No bound: §4.4 gives a boolean none, and the corelib normalizes every
		// non-zero to 1 while filling.
		line("const _t = this._tq; _t.bool = %s;", dst)
	case shBits:
		// A Uint32Array over the Float32Array member's OWN buffer: the decoder
		// writes the wire words, which is what keeps a signaling NaN's payload
		// (§4.6/§6.5), and the same bytes read back as the values. One view object
		// per array -- the member is a different array each time, so there is
		// nothing to cache.
		line("const _m = %s; const _t = this._tb;", dst)
		line("_t.bits = new Uint32Array(_m.buffer, _m.byteOffset, _m.length);")
	case shF64:
		line("const _t = this._td; _t.f64 = %s;", dst)
	}
	if p.shape == shTyped || p.shape == shLongs {
		bd := g.elemBound(p.elemKind, p.elemRef)
		line("_t.minLo = %d; _t.minHi = %d; _t.maxLo = %d; _t.maxHi = %d;", bd[0], bd[1], bd[2], bd[3])
	}
	line("return _t;")
	return out
}

// matRowType / arrElemType name the TypeScript element types the generated
// destinations hold, so a freshly built row or array is typed rather than any[].
func (g *gen) matRowType(sc *tsScope) string {
	return g.tsArrayType(sc.elemItems.Elem, sc.elemItems.ElemRef, sc.elemItems.ElemItems)
}

func (g *gen) arrElemType(sc *tsScope) string {
	return g.tsArrayType(sc.elem, sc.elemRef, sc.elemItems)
}

// arrayDst names the visitor-private register that holds a native array's
// destination for the duration of its element run.
//
// arrayBegin builds that array and two later hooks need it again: arrayBulk hands
// it (or the scratch that fills it) to the decoder, and arrayEnd copies out of
// that scratch. Reaching it through `this.o.<field>` walks the whole nested path
// each time, for a value that cannot change while the array lasts; keeping the
// register is free, and each hook then does one load. It carried more weight
// still when an element callback read it per element -- that callback is gone
// (corelib-ts#177) and the register is not, because the per-ARRAY hooks want it.
//
// It is the same register the nested matrix rows already use (`_rowN`), applied
// one level up. Scoped by location as well as name: the same field name may occur
// in two scopes of one tree.
func arrayDst(sc *tsScope, f *ir.Field) string {
	return fmt.Sprintf("_a%d%s", sc.id, exported(f.Name))
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
// It is an assertion, and the declared bound that follows it is what makes the
// assertion true. corelib-ts hands over a `bigint` in exactly one case -- a
// magnitude above 2^53-1 -- and every narrow width tops out at 2^32-1, as does
// the widest a NUMBER-carried bitfield can imply, so such a value fails the
// clause and throws before it can be stored: a bigint/number relational
// comparison is legal JS and answers it. A `Number(v)` here would convert a
// value the very next line rejects, per scalar field and per array element, on
// the hot path. An `enum` still keeps `Number(v)`, not because of the clause but
// because of the STORE -- its carrier is a numeric enum, so a bigint that
// reached it would be kept as a bigint -- and a WIDE bitfield keeps `BigInt(v)`,
// because the top bits are the value.
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

// declaredWidthCond is the reject comparison for an `enum` and a `bitfield`,
// MESSAGE_SPEC §1: each is bound by the WIDTH its declaration IMPLIES -- for an
// enum the smallest SIGNED type holding every declared constant, for a bitfield
// the smallest UNSIGNED type holding its highest declared `pos`. It returns ""
// for every other kind (widthCond owns those) and wherever the implied width is
// the 64-bit accumulator itself, where the test is a tautology and the clause
// would be dead code.
//
// A value INSIDE that width is valid even when the schema names no constant for
// it and even when it carries an undeclared bit; only a value outside it is
// malformed input. So an enum {RED=1, GREEN=2, BLUE=3} admits 5 and refuses 200,
// and a bitfield declaring positions 0, 1 and 3 admits 4 -- the undeclared bit 2
// -- and refuses 256. An undeclared bit is NOT masked away: masking would turn
// malformed-looking input into a DECLARED combination and report it Ok.
//
// This replaces the set/mask bound of generator#530, which implemented the
// closed-type reading MESSAGE_SPEC carried for six days (doc PR #89, `a50db95`)
// and doc PR #95 (`382159e`) withdrew. Both bounds are ordinary intervals now,
// which is §1's own reason for the change: an array's elements are consumed
// inside the corelib loop, so a bound must cross that channel as an interval,
// and a width fits where a set does not.
//
// Storage is still never the bound. A field whose declared positions are 0..3
// does not become 0..2^53 valid because TypeScript holds it in a double, and
// §1's fourth consequence names that case directly: a receiver that cannot hold
// the field at exactly the declared width holds it wider and MUST then enforce
// the width as an explicit check, because nothing about its storage will. The
// comparison therefore runs on the value the hook delivered, ahead of the store.
//
// Both spellings are ONE relational test, and the bitfield needs no mask term any
// more -- the ToInt32 corner that forced `v > MASK` beside `v & ~MASK` is gone
// with the mask. The carrier still picks the literal: a NARROW bitfield compares
// against a `number`, which also answers the bigint the hook delivers above 2^53
// (a bigint/number relational comparison is legal JS), and that is what keeps
// `v as number` safe as the read. A WIDE bitfield -- a flag at position 31 or
// above -- reads `BigInt(v)`, so its bound is a bigint literal; the only implied
// width that reaches it is u32, because a position at 32 or above implies the u64
// the accumulator already is and carries no guard at all.
func declaredWidthCond(v string, k ir.Kind, ref *ir.TypeRef) string {
	switch k {
	case ir.KindEnum:
		lo, hi, ok := ir.EnumWidthRange(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf("%s < %d || %s > %d", v, lo, v, hi)
	case ir.KindBitfield:
		hi, ok := ir.BitfieldWidthMax(ref)
		if !ok {
			return ""
		}
		if wideBitfield(ref) {
			return fmt.Sprintf("%s > %dn", v, hi)
		}
		return fmt.Sprintf("%s > %d", v, hi)
	}
	return ""
}

// declaredWidthWhat names the breached declaration in the InvalidMsg text, so
// the message says which declaration implied the width that was breached.
func declaredWidthWhat(k ir.Kind) string {
	if k == ir.KindEnum {
		return "enum width"
	}
	return "bitfield width"
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
	// and the corelib announces its header as ArrayKind.Signed. Classifying it as
	// Unsigned here made arrayBegin reject every enum array as a §7.3
	// contradiction, so its count bound never ran and a re-opened id merged into
	// the old value instead of replacing it (§7.4) -- while the elements, routed
	// by kind and not by that arm, still arrived.
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
	// No catch, and no remembered status. A refusal is terminal and the STREAM
	// latches it (CORELIB_PLAN §5.2 for malformed bytes, §6.3 for a receiver
	// limit), re-throwing the very code it was refused with from every later call.
	// Recording a second copy here bought nothing the stream did not already hold,
	// and flattened LimitExceeded -- a policy stop on well-formed bytes -- into an
	// Incomplete that says something untrue about the wire. finish asks the stream
	// instead; see below.
	f.line("  feed(chunk: Uint8Array): DecodeStatus {")
	f.line("    return this.is.feed(chunk);")
	f.line("  }")
	f.blank()
	f.line("  /** The destination, holding whatever has been decoded so far. */")
	f.line("  get message(): %s { return this.out; }", name)
	f.blank()
	f.line("  /**")
	f.line("   * Take the decoded message once the caller's framing says the input is")
	f.line("   * over. Rejects a stream that ended mid-field rather than returning a")
	f.line("   * half-filled value; read `message` to get it anyway.")
	f.line("   *")
	f.line("   * A stream that was REFUSED throws that refusal's own code instead: the")
	f.line("   * rejection is terminal, so asking the stream again re-throws it. A")
	f.line("   * decoder that rejected a message cannot hand one back.")
	f.line("   */")
	f.line("  finish(): %s {", name)
	// One call answers both halves. If the stream refused, its terminal guard runs
	// before a byte is looked at and re-throws that refusal's own code. If it did
	// not, the return value is the outcome for everything fed so far, computed
	// from the stream's own state at this field boundary; an empty chunk moves
	// that state nowhere.
	f.line("    // The stream latched any refusal and re-throws it here, consuming no")
	f.line("    // byte and driving no visitor callback; otherwise this is the outcome")
	f.line("    // for everything fed so far.")
	f.line("    const st = this.is.feed(new Uint8Array(0));")
	f.line("    if (st !== DecodeStatus.Complete) {")
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
