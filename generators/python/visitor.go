package python

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// This file emits the decode half: a FLAT visitor per generated class
// (ARCHITECTURE §9.3 family 1, the shape Rust/C#/Java/Kotlin already use).
//
// corelib-py removed the pull API in favour of CORELIB_PLAN §5.3.1 ("the visitor
// is the only decode surface"), and its visitor is flat: on_sequence_begin
// returns a bool, not a child visitor, so one object receives every callback at
// every depth. A field id alone therefore no longer identifies a destination --
// id 0 means something different in every sequence -- so dispatch is keyed on
// (location, id): `location` is a small int naming the scope the walk is
// currently inside, maintained by on_sequence_begin/on_sequence_end over an
// explicit stack.
//
// Two scope kinds exist. An OBJECT scope is a message/struct/union: its fields
// dispatch by id, and each names a static Python path. An ARRAY scope is a
// wrapper-sequence array (string/blob/struct/union/nested-array elements, §5.1):
// there the id IS the element index, so it dispatches by index instead, with an
// index register carrying it down into an element scope.
//
// A declined sequence needs no `_DEAD` location the way the C# backend has one:
// corelib-py's `return False` skips the whole subtree -- nothing inside is
// delivered and no on_sequence_end fires for it -- so the stack is only ever
// pushed for a scope we actually entered.

// pyScope is one dispatch location in a flat visitor.
type pyScope struct {
	id   int
	name string // location constant, e.g. "_L_Point_start"

	// Object scope: fields dispatch by id, `path` names the object they land on.
	fields   []*ir.Field
	path     string
	seqChild map[int64]int // field id -> scope entered by its SEQUENCE_START

	// Array scope: elements dispatch by index.
	isArr      bool
	arrPath    string // the list the elements land in
	elem       ir.Kind
	elemRef    *ir.TypeRef
	elemItems  *ir.ArrayElem
	cap        int64 // element capacity; -1 when the schema leaves it open
	elemMaxHas bool
	elemMax    int64
	loc        string // schema location named in a rejection message
	ix         string // index register, "" when no element scope needs one
	child      int    // element scope id, -1 when the element is a value
}

type scopeSet struct{ scopes []*pyScope }

// buildScopes walks a class's tree and assigns one scope per sequence-framed
// location reachable from it, rooted at the class itself.
func (g *gen) buildScopes(typeName string, fields []*ir.Field) []*pyScope {
	ss := &scopeSet{}
	ss.object(typeName, "self._o", fields)
	return ss.scopes
}

func (ss *scopeSet) object(locName, path string, fields []*ir.Field) int {
	sc := &pyScope{
		id: len(ss.scopes), name: "_L_" + locName,
		fields: fields, path: path, seqChild: map[int64]int{}, child: -1,
	}
	ss.scopes = append(ss.scopes, sc)
	for _, fld := range fields {
		switch fld.Kind {
		case ir.KindStruct, ir.KindUnion:
			sc.seqChild[fld.ID] = ss.object(
				locName+"_"+fld.Name,
				path+"."+pyIdent(fld.Name),
				fld.Ref.Target.Fields)
		case ir.KindArray:
			// A native scalar array arrives whole through on_*_array; only a
			// wrapper-sequence array opens a scope of its own.
			if !isNativeArrayElem(fld.Elem) {
				sc.seqChild[fld.ID] = ss.array(
					locName+"_"+fld.Name,
					path+"."+pyIdent(fld.Name),
					fld.Name,
					fld.Elem, fld.ElemRef, fld.ElemItems,
					capOf(fld.HasCount, fld.Count), fld.ElemMaxHas, fld.ElemMax)
			}
		}
	}
	return sc.id
}

func (ss *scopeSet) array(locName, arrPath, loc string, elem ir.Kind, ref *ir.TypeRef,
	items *ir.ArrayElem, cap int64, emHas bool, em int64) int {
	sc := &pyScope{
		id: len(ss.scopes), name: "_L_" + locName,
		isArr: true, arrPath: arrPath, elem: elem, elemRef: ref, elemItems: items,
		cap: cap, elemMaxHas: emHas, elemMax: em, loc: loc, child: -1,
	}
	ss.scopes = append(ss.scopes, sc)
	switch elem {
	case ir.KindStruct, ir.KindUnion:
		sc.ix = fmt.Sprintf("_ix%d", sc.id)
		sc.child = ss.object(locName+"_e", fmt.Sprintf("%s[self.%s]", arrPath, sc.ix), ref.Target.Fields)
	case ir.KindArray:
		// A native row arrives whole through on_*_array at THIS scope, keyed by
		// its row index; only a wrapper row opens a scope of its own.
		if !isNativeArrayElem(items.Elem) {
			sc.ix = fmt.Sprintf("_ix%d", sc.id)
			sc.child = ss.array(locName+"_r", fmt.Sprintf("%s[self.%s]", arrPath, sc.ix), loc+" row",
				items.Elem, items.ElemRef, items.ElemItems,
				capOf(items.HasCount, items.Count), items.ElemMaxHas, items.ElemMax)
		}
	}
	return sc.id
}

// --- hook routing -----------------------------------------------------------

// pyHook names the corelib-py Visitor method a kind is delivered through.
// Booleans have no wire type of their own (§4.4) and arrive as unsigned.
func pyHook(k ir.Kind) string {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBool, ir.KindBitfield:
		return "on_unsigned"
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "on_signed"
	case ir.KindFP32:
		return "on_float32"
	case ir.KindFP64:
		return "on_float64"
	case ir.KindString:
		return "on_string"
	case ir.KindBlob:
		return "on_bytes"
	}
	return ""
}

// pyArrayHook names the hook a NATIVE array of `elem` is delivered through.
func pyArrayHook(elem ir.Kind) string {
	switch elem {
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "on_signed_array"
	case ir.KindFP32:
		return "on_float32_array"
	case ir.KindFP64:
		return "on_float64_array"
	default: // u8/u16/u32/u64, bool, bitfield
		return "on_unsigned_array"
	}
}

// pyValueHooks is every typed hook, in emission order.
var pyValueHooks = []string{
	"on_unsigned", "on_signed", "on_float32", "on_float64",
	"on_string", "on_bytes",
	"on_unsigned_array", "on_signed_array", "on_float32_array", "on_float64_array",
}

// --- emitter ----------------------------------------------------------------

// emitVisitor writes the flat visitor class for one generated dataclass.
func (g *gen) emitVisitor(f *pyfile, name string, fields []*ir.Field) {
	scopes := g.buildScopes(name, fields)

	f.line("# Dispatch locations for %s: one per sequence-framed scope in its tree.", name)
	f.line("# A field id is only unique WITHIN a scope -- a nested sequence opens a fresh")
	f.line("# id space -- so the visitor below keys every hook on (location, id).")
	for _, sc := range scopes {
		f.line("%s = %d", sc.name, sc.id)
	}
	f.blank()

	f.line("class _%sVisitor(Visitor):", name)
	f.line(`    """Flat decode visitor for :class:`+"`"+`%s`+"`"+`.`, name)
	f.line("")
	f.line("    corelib-py's visitor is flat -- one object receives every callback at every")
	f.line("    depth -- so the current scope is tracked here, in ``_c``, over the stack")
	f.line("    ``_s`` that ``on_sequence_begin`` / ``on_sequence_end`` maintain.")
	f.line(`    """`)
	f.line("")
	f.line("    def __init__(self, o: %s) -> None:", name)
	f.line("        self._o = o")
	f.line("        self._c = %s", scopes[0].name)
	f.line("        self._s: list[int] = []")
	for _, sc := range scopes {
		if sc.ix != "" {
			f.line("        self.%s = 0", sc.ix)
		}
	}
	f.blank()

	g.emitSeqHooks(f, scopes)
	g.emitOnArrayBegin(f, scopes)
	for _, hook := range pyValueHooks {
		g.emitValueHook(f, scopes, hook)
	}
	g.emitOnField(f, scopes)
	g.emitOnSchemaBound(f, scopes)
}

// emitSeqHooks writes on_sequence_begin / on_sequence_end.
//
// Returning False declines the scope: corelib-py then skips the whole subtree --
// nothing inside is delivered and no on_sequence_end fires -- which is exactly
// §7.3's "treat it like an unknown id" for a sequence, and is why the stack is
// pushed only on the accepting arms.
func (g *gen) emitSeqHooks(f *pyfile, scopes []*pyScope) {
	f.line("    def on_sequence_begin(self, fid: int) -> bool:")
	f.line("        c = self._c")
	first := true
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
		f.line("        %s c == %s:", kw(&first), sc.name)
		for _, ln := range body {
			f.line("            %s", ln)
		}
	}
	if first {
		f.line("        return False")
	} else {
		f.line("        return False")
	}
	f.blank()
	f.line("    def on_sequence_end(self) -> None:")
	f.line("        if self._s:")
	f.line("            self._c = self._s.pop()")
	f.blank()
}

// objSeqArm renders an object scope's SEQUENCE_START arms, one per struct/union
// or wrapper-array field.
func (g *gen) objSeqArm(sc *pyScope, scopes []*pyScope) []string {
	var out []string
	inner := true
	for _, fld := range sc.fields {
		child, ok := sc.seqChild[fld.ID]
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s fid == %d:", kw(&inner), fld.ID))
		if fld.Kind == ir.KindArray {
			// §7.4: an array wrapper REPLACES the value, so the list starts empty
			// rather than merging into whatever the defaults put there.
			out = append(out, fmt.Sprintf("    %s.%s = []", sc.path, pyIdent(fld.Name)))
		}
		out = append(out,
			"    self._s.append(c)",
			fmt.Sprintf("    self._c = %s", scopes[child].name),
			"    return True")
	}
	return out
}

// indexBound renders the reject for a wrapper array's element INDEX, emitted
// ahead of the gap-fill it bounds. `idExpr` names the index in the caller's
// scope (`fid` in the value hooks, `fld.id` in on_field).
//
// A wrapper array carries no count HEADER: its elements are keyed by an
// unbounded varint index and the gap-fill extends the list to fid + 1, so the
// index IS the array's length (MESSAGE_SPEC §5.1 -- two elements at id 0 and id
// 16383 are a 16384-slot list). A single over-index element is therefore an
// amplification vector by itself, and it is the INDEX that has to be bounded:
// capping how many elements arrived would not bound the allocation, because a
// sparse array allocates by its highest id.
//
// Which bound applies depends on whether the schema counts the array, and the
// two differ only in that and in what the failure is called (ARCHITECTURE §9.5):
// `count: N` makes fid >= N a SofaDecodeError (the bytes contradict the agreed
// schema, issue #142), no count makes it a SofaLimitError against the configured
// cap (the bytes are well formed and the same message decodes under a looser
// cap, issue #387 -- folding the two together is forbidden by CORELIB_PLAN
// §6.2.1).
//
// Empty only when the array is dynamic AND no cap is live for this schema.
func (g *gen) indexBound(cap int64, idExpr, loc string) []string {
	if cap >= 0 {
		return []string{
			fmt.Sprintf("if %s >= %d:", idExpr, cap),
			fmt.Sprintf("    raise SofaDecodeError(%q)",
				fmt.Sprintf("%s: array index above schema capacity %d", loc, cap)),
		}
	}
	if !g.limits.arrayHas {
		return nil
	}
	return []string{
		fmt.Sprintf("if %s >= MAX_DYN_ARRAY_COUNT:", idExpr),
		fmt.Sprintf(`    raise SofaLimitError("%s: array index %%d exceeds max_array_count %%d" %% (%s, MAX_DYN_ARRAY_COUNT))`, loc, idExpr),
	}
}

// arrSeqArm renders an array scope's element arm. The id IS the index, so there
// is no id test -- only the §5.1 capacity bound, then the gap-fill that places
// the element at its index (an interior element equal to the element default is
// omitted on the wire, MESSAGE_SPEC §2).
func (g *gen) arrSeqArm(sc *pyScope, child *pyScope) []string {
	out := g.indexBound(sc.cap, "fid", sc.loc)
	out = append(out, fmt.Sprintf("_t = %s", sc.arrPath))
	out = append(out, "while len(_t) <= fid:")
	out = append(out, fmt.Sprintf("    _t.append(%s)", g.elemDefault(sc)))
	out = append(out,
		fmt.Sprintf("self.%s = fid", sc.ix),
		"self._s.append(c)",
		fmt.Sprintf("self._c = %s", child.name),
		"return True")
	return out
}

// elemDefault is the value a gap-filled element of an array scope takes.
func (g *gen) elemDefault(sc *pyScope) string {
	switch sc.elem {
	case ir.KindString:
		return `""`
	case ir.KindBlob:
		return `b""`
	case ir.KindArray:
		return "[]"
	default: // struct / union
		return g.typeName(sc.elemRef.Key) + "()"
	}
}

// emitValueHook writes one typed hook, with an arm per scope that has a
// destination for it. Nothing is emitted when no scope does: the base class's
// no-op is then the right implementation, and an empty override would cost a
// Python call per field for nothing.
func (g *gen) emitValueHook(f *pyfile, scopes []*pyScope, hook string) {
	type arm struct {
		sc   *pyScope
		body []string
	}
	var arms []arm
	for _, sc := range scopes {
		var body []string
		if sc.isArr {
			body = g.arrValueArm(sc, hook)
		} else {
			body = g.objValueArm(sc, hook)
		}
		if len(body) > 0 {
			arms = append(arms, arm{sc, body})
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("    def %s(self, fid: int, value: %s) -> None:", hook, pyHookArgType(hook))
	f.line("        c = self._c")
	first := true
	for _, a := range arms {
		f.line("        %s c == %s:", kw(&first), a.sc.name)
		for _, ln := range a.body {
			f.line("            %s", ln)
		}
	}
	f.blank()
}

// pyHookArgType annotates a hook's value parameter.
func pyHookArgType(hook string) string {
	switch hook {
	case "on_unsigned", "on_signed":
		return "int"
	case "on_float32", "on_float64":
		return "float"
	case "on_string":
		return "str"
	case "on_bytes":
		return "bytes"
	case "on_unsigned_array", "on_signed_array":
		return "list[int]"
	}
	return "list[float]"
}

// objValueArm renders an object scope's arms for one hook.
func (g *gen) objValueArm(sc *pyScope, hook string) []string {
	var out []string
	inner := true
	for _, fld := range sc.fields {
		var want string
		if fld.Kind == ir.KindArray {
			if !isNativeArrayElem(fld.Elem) {
				continue
			}
			want = pyArrayHook(fld.Elem)
		} else {
			want = pyHook(fld.Kind)
		}
		if want != hook {
			continue
		}
		acc := sc.path + "." + pyIdent(fld.Name)
		out = append(out, fmt.Sprintf("%s fid == %d:", kw(&inner), fld.ID))
		out = append(out, indent(g.storeValue(acc, fld.Name, fld))...)
	}
	return out
}

// storeValue renders the assignment (and its §7.1 declared-width rejection) for
// one destination.
func (g *gen) storeValue(acc, loc string, fld *ir.Field) []string {
	if fld.Kind == ir.KindArray {
		return g.storeNativeArray(acc, loc, fld.Elem)
	}
	var out []string
	if fld.Kind == ir.KindBool {
		return append(out, fmt.Sprintf("%s = bool(value)", acc))
	}
	// The declared width is checked on the value the corelib handed over.
	// Python's int is unbounded, so nothing masks an out-of-range value -- the
	// whole defect this guards was that such a value was simply KEPT -- and the
	// raise aborts the decode before the object is handed back.
	if cond := widthCond("value", fld.Kind); cond != "" {
		out = append(out,
			fmt.Sprintf("if %s:", cond),
			fmt.Sprintf("    raise SofaDecodeError(%q)",
				fmt.Sprintf("%s: value outside declared width %s", loc, fld.Kind)))
	}
	return append(out, fmt.Sprintf("%s = value", acc))
}

// storeNativeArray renders a native array's store.
//
// It carries NO element-width scan. The §7.1 element bound is stated once, in
// on_array_begin, as the (elem_min, elem_max) pair the decoder applies AT each
// element -- which is where the bound has to be taken anyway, since a scan of
// the finished list cannot reject an element a truncation prevents the array
// from ever completing (§5.2's INVALID over INCOMPLETE). Repeating it here was a
// second, weaker verdict on a list the decoder had already vetted: a pure-Python
// pass over every element of every integer array, per message, for nothing.
//
// The two stay in lockstep by construction: widthCond is non-empty exactly for
// the narrow kinds ir.NarrowRange answers for, and arrayBeginBody emits the pair
// for exactly those (see isIntArrayElem).
func (g *gen) storeNativeArray(acc, loc string, elem ir.Kind) []string {
	if elem == ir.KindBool {
		return []string{fmt.Sprintf("%s = [bool(_v) for _v in value]", acc)}
	}
	return []string{fmt.Sprintf("%s = value", acc)}
}

// widthCond renders the out-of-range test for a narrow declared width, or "" for
// u64/i64 and the kinds that carry no width.
func widthCond(v string, k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("%s < %d or %s > %d", v, lo, v, hi)
	}
	return fmt.Sprintf("%s > %d", v, hi)
}

// arrValueArm renders an array scope's arm for one hook: a value element
// (string/blob) or a native row, both keyed by index.
func (g *gen) arrValueArm(sc *pyScope, hook string) []string {
	var want string
	var body []string
	switch sc.elem {
	case ir.KindString, ir.KindBlob:
		want = pyHook(sc.elem)
		body = []string{"_t[fid] = value"}
	case ir.KindArray:
		if !isNativeArrayElem(sc.elemItems.Elem) {
			return nil
		}
		want = pyArrayHook(sc.elemItems.Elem)
		body = g.storeNativeArray("_t[fid]", sc.loc+" row", sc.elemItems.Elem)
	default:
		return nil // struct/union elements arrive through on_sequence_begin
	}
	if want != hook {
		return nil
	}
	// The §5.1 index bound is NOT repeated here: a value element is bounded in
	// on_field, one step earlier, at the header (see arrFieldArm).
	out := []string{
		fmt.Sprintf("_t = %s", sc.arrPath),
		"while len(_t) <= fid:",
		fmt.Sprintf("    _t.append(%s)", g.elemDefault(sc)),
	}
	return append(out, body...)
}

// --- on_field ---------------------------------------------------------------

// emitOnField writes the header-time ACCEPT/DECLINE hook.
//
// Two things live here, and both have to be decided at the header, before a
// payload byte is read or an element decoded:
//
//   - the §7.3 tag test for a field the schema BOUNDS. A header whose wire type
//     -- or, for fixlen, whose subtype -- contradicts the declared type is a
//     skipped field, and MESSAGE_SPEC §7.3 is explicit that against a schema
//     bound this clause wins: "the subtype is therefore decided first and the
//     schema bound applied only to a field that survives it". Declining here is
//     what keeps the bound this scope declares in on_schema_bound off a field
//     that is not the declared field's value -- the hook is asked one step
//     later and is told only the id and the announced count/length, so it
//     cannot make that distinction itself.
//   - the DECLINE of an id the scope does not declare at all. That field is not
//     one this handler was ever going to read, so §6.7.2 walks it and §6.2.1
//     forbids a cap to reach it -- and the decoder cannot know that on its own:
//     a field a visitor ACCEPTS is a field it reads, whatever the visitor then
//     does with the value. Saying so here is what keeps a decode that steps over
//     an over-cap field it does not want at COMPLETE.
//
// Neither the schema bound nor the receiver cap is here. The bound is a number
// returned from on_schema_bound; the caps are the three the Decoder is built
// with. Both are applied by the corelib, in the one place that knows what a
// declared count/length means (CORELIB_PLAN §6.2.1), and generated code states
// them rather than comparing against them a second time.
//
// It costs a Field object per field, so it is emitted only when the tree
// actually needs one: corelib-py builds one only for a visitor that overrides
// this method.
func (g *gen) emitOnField(f *pyfile, scopes []*pyScope) {
	type arm struct {
		sc   *pyScope
		body []string
	}
	var arms []arm
	for _, sc := range scopes {
		var body []string
		if sc.isArr {
			body = g.arrFieldArm(sc)
		} else {
			body = g.objFieldArm(sc)
		}
		if len(body) > 0 {
			arms = append(arms, arm{sc, body})
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("    def on_field(self, fld: Field) -> bool:")
	f.line(`        """Accept or decline a field at its HEADER, before its value is read.`)
	f.line("")
	f.line("        An id is declined when the header's wire type -- or, for a fixlen one,")
	f.line("        its subtype -- is not the one its declared type maps to. Such a field is")
	f.line("        SKIPPED, exactly like an unknown id, so neither the bound")
	f.line("        ``on_schema_bound`` declares nor a receiver-side cap may reach it.")
	f.line("")
	f.line("        An id this scope does not declare AT ALL is declined for the same")
	f.line("        reason: it is not a field this handler reads, so it is walked rather")
	f.line("        than materialized, and no receiver cap may reach it. A decode that")
	f.line("        steps over an over-cap field it does not want stays COMPLETE.")
	f.line(`        """`)
	f.line("        c = self._c")
	first := true
	for _, a := range arms {
		f.line("        %s c == %s:", kw(&first), a.sc.name)
		for _, ln := range a.body {
			f.line("            %s", ln)
		}
	}
	f.line("        return True")
	f.blank()
}

// objFieldArm renders an object scope's header work, ONE ARM PER DECLARED ID.
//
// Each arm carries whichever of two mutually exclusive things the field needs,
// and both sit behind the §7.3 tag test:
//
// Every count- or length-bearing field is DECLINED when the header does not
// carry the tag its declared type maps to, so a value that was never this
// field's reaches neither the bound on_schema_bound declares one hook later nor
// the receiver cap the Decoder parked one hook earlier. §7.3 asks for a skip, and
// a skipped field is never capped (§6.2.1) — the codec drops the parked verdict
// the moment this hook declines.
//
// The caps themselves are NOT here. The Decoder is handed all three and compares
// them at the count/length header, off any field on_schema_bound declares and off
// any field this hook skips, so a second comparison in generated code would be
// the two routes to one rule §6.2.1 forbids. What remains generated is the one
// number the codec cannot see: a WRAPPER array's element index, which is a field
// id and not a count (indexBound).
//
// Never on an id this scope does not declare, either: the caps used to sit in
// the ELSE of this chain, where they also fired on an unknown id -- a field the
// handler was never going to read, which §6.2.1 says is skipped and "allocates
// nothing", so a decode that steps over an over-cap field it does not want stays
// COMPLETE.
func (g *gen) objFieldArm(sc *pyScope) []string {
	out := []string{
		fmt.Sprintf("if fld.id not in %s:", pyIDSet(sc.fields)),
		"    return False  # an id this scope does not declare is walked, not read",
	}
	inner := true
	for _, fld := range sc.fields {
		var body []string
		switch fld.Kind {
		case ir.KindString, ir.KindBlob:
			body = declineOnMismatch(tagMismatch(fld.Kind, 0), fld.Name)
		case ir.KindArray:
			// A wrapper array carries no count header of its own -- its length is
			// its highest element index -- so it is bounded in on_sequence_begin
			// and in the array scope's own arm, not here.
			if !isNativeArrayElem(fld.Elem) {
				continue
			}
			body = declineOnMismatch(tagMismatch(ir.KindArray, fld.Elem), fld.Name)
		default:
			continue
		}
		if len(body) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s fld.id == %d:", kw(&inner), fld.ID))
		out = append(out, indent(body)...)
	}
	return out
}

// arrFieldArm renders an array scope's header work. Every element of a scope
// shares one declared type, so there is no id test in front of any of it -- but
// there is a TAG test, and it comes FIRST:
//
//  1. the §7.3 decline. An element whose wire type (or fixlen subtype)
//     contradicts the declared element type was never this array's element, so
//     neither its index nor its length nor its count may be measured against
//     this array's bounds. It used to sit BEHIND the index bound, which made a
//     mistyped element at an over-capacity index INVALID where §7.3 asks for a
//     skip -- the same ordering corelib-go's and corelib-dart's collectors take
//     for the wrapper arrays they own.
//  2. the §5.1 index bound: the schema `count:` as INVALID, or the receiver cap
//     as a policy rejection where the schema declares none. A wrapper array
//     announces no count, so the INDEX is the length and the index is what
//     bounds the allocation.
//
// An element's own header number -- a string/blob element's byte LENGTH, a
// matrix row's element COUNT -- is not bounded here. Both are count/length words
// the Decoder reads, so it applies the receiver cap to them itself; what it
// cannot see is the INDEX, which is a field id.
func (g *gen) arrFieldArm(sc *pyScope) []string {
	var out []string
	switch sc.elem {
	case ir.KindString, ir.KindBlob:
		out = append(out, declineOnMismatch(tagMismatch(sc.elem, 0), sc.loc)...)
	case ir.KindArray:
		if isNativeArrayElem(sc.elemItems.Elem) {
			out = append(out, declineOnMismatch(
				tagMismatch(ir.KindArray, sc.elemItems.Elem), sc.loc+" row")...)
		}
	}
	if sc.child < 0 {
		// A value element is bounded here; an element that opens a scope is
		// bounded in on_sequence_begin, which no on_field precedes.
		out = append(out, g.indexBound(sc.cap, "fld.id", sc.loc)...)
	}
	return out
}

// pyIDSet renders the scope's declared ids as a set display of literals. CPython
// folds `x in {1, 2, 3}` into a frozenset constant, so the membership test is one
// hash lookup rather than a walk down the id chain -- and it is reached once per
// field, ahead of everything else this hook does.
func pyIDSet(fields []*ir.Field) string {
	if len(fields) == 0 {
		// An empty set display is a dict literal; frozenset() is the constant.
		return "frozenset()"
	}
	ids := make([]string, 0, len(fields))
	for _, f := range fields {
		ids = append(ids, strconv.FormatInt(f.ID, 10))
	}
	return "{" + strings.Join(ids, ", ") + "}"
}

// tagMismatch renders the test that a header does NOT carry the tag its declared
// type maps to -- MESSAGE_SPEC §7.3's wire type, plus the subtype for a fixlen
// one. `elem` is read only for ir.KindArray.
//
// A string/blob needs no wire-type test in front of its subtype: `subtype` is
// set only for a fixlen scalar and a fixlen ARRAY, and a fixlen array's subtype
// is always fp32/fp64 (the corelib rejects any other at the header), so
// `subtype == STRING` already says "a fixlen scalar carrying a string". A fixlen
// ARRAY does need both, because fp32/fp64 name a scalar subtype too.
func tagMismatch(kind, elem ir.Kind) string {
	switch kind {
	case ir.KindString, ir.KindBlob:
		return "fld.subtype != " + pyFixlenSubtype(kind)
	case ir.KindArray:
		x := &ir.Field{Kind: ir.KindArray, Elem: elem}
		cond := "fld.type != " + pyExpectedWire(x)
		if sub := pyFixlenSubtype(elem); sub != "" {
			cond += " or fld.subtype != " + sub
		}
		return cond
	}
	return ""
}

// declineOnMismatch renders the §7.3 skip: a header that contradicts the
// declared type is not this field's value, so it is declined here rather than
// measured against the schema bound one hook later.
//
// Declining is what an unknown id gets, which is what §7.3 asks for ("skipped,
// exactly as a field with an unknown id is skipped"). It is also strictly less
// work than letting the value through to a typed hook with no arm for it: the
// payload is neither materialized nor validated (CORELIB_PLAN §6.7.2).
func declineOnMismatch(cond, loc string) []string {
	if cond == "" {
		return nil
	}
	return []string{
		fmt.Sprintf("if %s:", cond),
		fmt.Sprintf("    return False  # %s: header is not the declared type -- skip it", loc),
	}
}

// --- on_schema_bound --------------------------------------------------------

// emitOnSchemaBound writes the hook that names the count/length the SCHEMA
// declares for a field, or -1.
//
// The comparison itself is the corelib's: it is asked at the count/length
// header, before a payload byte is read or any storage is written, and a wire
// count/length above the answer is INVALID (MESSAGE_SPEC §7.1) while the
// receiver-side cap stops applying to the field (CORELIB_PLAN §6.2.1). Both
// halves matter here -- the second is what keeps a schema-bounded field
// decodable under a tighter deployment cap, which §6.3 states as "never raised
// for a field the schema bounds".
//
// The hook is handed the wire's tag as well as the two integers (corelib-py#135):
// reached from a route with no table entry in front of it, it could not otherwise
// tell its own field's value from a header that reuses the id under a
// contradicting wire type. Generated code does not consult it, and that is
// deliberate -- the §7.3 test runs one hook earlier, in on_field, which DECLINES
// such a header so the field is skipped before any bound or cap can reach it.
// Testing the tag again here would be a second implementation of a rule
// CORELIB_PLAN §5.3.1 requires to have exactly one. The parameters are accepted
// because the decoder passes them positionally, and named for what they are.
//
// Overriding it still costs no object per field: the decoder builds a Field only
// for on_field.
//
// Only a field carrying a count or a length on the wire reaches it: a string or
// blob with a `maxlen`, and a NATIVE array with a `count` (integer and float
// alike). A wrapper array's length is its highest element index, with no count
// header anywhere on the wire, so §5.1 bounds it in on_sequence_begin/on_field
// instead.
func (g *gen) emitOnSchemaBound(f *pyfile, scopes []*pyScope) {
	type arm struct {
		sc   *pyScope
		body []string
	}
	var arms []arm
	for _, sc := range scopes {
		var body []string
		if sc.isArr {
			body = arrSchemaBoundArm(sc)
		} else {
			body = objSchemaBoundArm(sc)
		}
		if len(body) > 0 {
			arms = append(arms, arm{sc, body})
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("    def on_schema_bound(self, fid: int, n: int, wt, st) -> int:")
	f.line(`        """The count or length the SCHEMA declares for this field, or -1.`)
	f.line("")
	f.line("        Answered at the count/length header, before any payload byte is read.")
	f.line("        A wire count/length above it is INVALID; a field that declares one is")
	f.line("        no longer governed by the receiver-side caps, which bound only what the")
	f.line("        schema left open.")
	f.line("")
	f.line("        ``wt``/``st`` are the header's wire type and fixlen subtype. Neither is")
	f.line("        consulted here: ``on_field`` has already declined a header whose type")
	f.line("        contradicts the one this field declares, so a field that reaches this")
	f.line("        hook is the declared field and no other.")
	f.line(`        """`)
	f.line("        c = self._c")
	first := true
	for _, a := range arms {
		f.line("        %s c == %s:", kw(&first), a.sc.name)
		for _, ln := range a.body {
			f.line("            %s", ln)
		}
	}
	f.line("        return -1")
	f.blank()
}

// objSchemaBoundArm renders an object scope's declarations, one per bounded id.
func objSchemaBoundArm(sc *pyScope) []string {
	var out []string
	inner := true
	for _, fld := range sc.fields {
		var n int64
		var why string
		switch fld.Kind {
		case ir.KindString, ir.KindBlob:
			if !fld.HasMaxlen {
				continue
			}
			n, why = fld.Maxlen, "maxlen"
		case ir.KindArray:
			if !isNativeArrayElem(fld.Elem) || !fld.HasCount {
				continue
			}
			n, why = fld.Count, "count"
		default:
			continue
		}
		out = append(out, fmt.Sprintf("%s fid == %d:", kw(&inner), fld.ID))
		out = append(out, fmt.Sprintf("    return %d  # %s: schema %s", n, fld.Name, why))
	}
	return out
}

// arrSchemaBoundArm renders an array scope's declaration. Every element of a
// scope shares one declared type, so the answer is the same for every index and
// needs no test.
func arrSchemaBoundArm(sc *pyScope) []string {
	switch sc.elem {
	case ir.KindString, ir.KindBlob:
		if sc.elemMaxHas {
			return []string{fmt.Sprintf("return %d  # %s: schema element maxlen", sc.elemMax, sc.loc)}
		}
	case ir.KindArray:
		if isNativeArrayElem(sc.elemItems.Elem) && sc.elemItems.HasCount {
			return []string{fmt.Sprintf("return %d  # %s row: schema count", sc.elemItems.Count, sc.loc)}
		}
	}
	return nil
}

// --- small helpers ----------------------------------------------------------

// kw yields "if" once, then "elif", so a chain can be built in a loop.
func kw(first *bool) string {
	if *first {
		*first = false
		return "if"
	}
	return "elif"
}

func indent(lines []string) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = "    " + ln
	}
	return out
}

// visitorNeeds reports which sofab names the emitted decode section references.
//
// Read off the EMITTED TEXT rather than re-deriving it from the schema: the
// import line and the code have to agree exactly, and a second walk that says
// "this schema needs FixlenSubtype" is a second implementation of the emitter's
// own conditions -- which is how the import drifted out of lockstep before
// (generator#246). Here they cannot disagree.
func visitorNeeds(section string) (field, wire, fixlen bool) {
	// Substring, not "WireType.": on_array_begin names the type as a bare
	// parameter annotation, with no member access after it.
	return strings.Contains(section, "def on_field("),
		strings.Contains(section, "WireType"),
		strings.Contains(section, "FixlenSubtype")
}

// isIntArrayElem reports whether a native array element rides the integer array
// wire types -- the ones on_array_begin is called for. fp32/fp64 arrays are
// fixlen-framed, carry no declared width to state, and are moved into their
// destination in one piece, so the corelib does not call the hook for them.
func isIntArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindFP32, ir.KindFP64:
		return false
	}
	return isNativeArrayElem(k)
}

// emitOnArrayBegin writes the integer-array header hook.
//
// It is the only point at which anything can be said about an array's ELEMENTS:
// the typed hook below it receives them already decoded, so a scan there is
// exact for an array that arrives and never runs for one that does not -- and a
// message truncated behind an out-of-width element is exactly the case where a
// malformed verdict must outrank a truncated one. The declared width is
// therefore STATED here and applied by the decoder at each element.
//
// The element WIDTH is all that is left here. The schema capacity is declared
// in on_schema_bound, which the corelib asks one hook earlier -- so the count
// verdict still lands before the first element, and it lands in the one place
// that applies a declared bound (CORELIB_PLAN §6.2.1).
func (g *gen) emitOnArrayBegin(f *pyfile, scopes []*pyScope) {
	type arm struct {
		sc   *pyScope
		body []string
	}
	var arms []arm
	for _, sc := range scopes {
		var body []string
		if sc.isArr {
			body = g.arrArrayBeginArm(sc)
		} else {
			body = g.objArrayBeginArm(sc)
		}
		if len(body) > 0 {
			arms = append(arms, arm{sc, body})
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("    def on_array_begin(self, fid: int, wtype: WireType, count: int):")
	f.line("        \"\"\"An integer array's header, before any element is decoded.")
	f.line("")
	f.line("        Returns the declared element width for the decoder to apply AT each")
	f.line("        element, so a value outside it is rejected whether the array completes")
	f.line("        or is cut short behind it. The schema capacity is not checked here:")
	f.line("        ``on_schema_bound`` declares it one hook earlier.")
	f.line("        \"\"\"")
	f.line("        c = self._c")
	first := true
	for _, a := range arms {
		f.line("        %s c == %s:", kw(&first), a.sc.name)
		for _, ln := range a.body {
			f.line("            %s", ln)
		}
	}
	f.line("        return None")
	f.blank()
}

// objArrayBeginArm renders an object scope's integer-array arms.
func (g *gen) objArrayBeginArm(sc *pyScope) []string {
	var out []string
	inner := true
	for _, fld := range sc.fields {
		if fld.Kind != ir.KindArray || !isIntArrayElem(fld.Elem) {
			continue
		}
		body := arrayBeginBody(fld.Elem)
		if len(body) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s fid == %d:", kw(&inner), fld.ID))
		out = append(out, indent(body)...)
	}
	return out
}

// arrArrayBeginArm renders an array scope's integer ROW arm, keyed by row index.
func (g *gen) arrArrayBeginArm(sc *pyScope) []string {
	if sc.elem != ir.KindArray || !isIntArrayElem(sc.elemItems.Elem) {
		return nil
	}
	return arrayBeginBody(sc.elemItems.Elem)
}

// arrayBeginBody is the shared body: state the declared element width.
//
// Empty for u64/i64 (and enum/bitfield), whose declared width IS the value
// domain -- there is nothing to narrow, so the base class's default answer will
// do and no arm is worth a dispatch.
func arrayBeginBody(elem ir.Kind) []string {
	lo, hi, ok := ir.NarrowRange(elem)
	switch {
	case !ok:
		return nil
	case lo < 0:
		return []string{fmt.Sprintf("return (None, %d, %d)", lo, hi)}
	default:
		return []string{fmt.Sprintf("return (None, None, %d)", hi)}
	}
}
