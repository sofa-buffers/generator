package python

import (
	"fmt"
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

// emitOnField writes the header-time bounds hook.
//
// It exists for the ordering §5.2 requires: INVALID dominates INCOMPLETE, so a
// message truncated right after a length or count word -- where the violation is
// already fully established -- must still be INVALID. on_field is called at the
// header, before a payload byte is read or an element decoded, which is the only
// point in the visitor surface where that verdict can be reached (the typed hooks
// receive a value that has already fully arrived).
//
// It costs a Field object per field, so it is emitted only when the tree
// actually carries a bound: corelib-py builds one only for a visitor that
// overrides this method.
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
		// The receiver caps govern every id the SCHEMA does not bound -- an
		// unbounded field of a bounded scope, and an unknown id alike -- so they
		// go in the else of the schema-bound chain, and give every scope an arm.
		if cap := g.capArm(sc, len(body) > 0); len(cap) > 0 {
			body = append(body, cap...)
		}
		if len(body) > 0 {
			arms = append(arms, arm{sc, body})
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("    def on_field(self, fld: Field) -> bool:")
	f.line(`        """Schema bounds that must be decided at the field HEADER.`)
	f.line("")
	f.line("        A malformed message outranks a truncated one, so a length or count that")
	f.line("        already breaches the schema is rejected here -- before the payload is")
	f.line("        read -- and stays INVALID however little of the field follows.")
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

// objFieldArm renders an object scope's header bounds: a bounded string/blob's
// byte length, and a native array's element count.
func (g *gen) objFieldArm(sc *pyScope) []string {
	var out []string
	inner := true
	for _, fld := range sc.fields {
		var chk []string
		switch fld.Kind {
		case ir.KindString, ir.KindBlob:
			if !fld.HasMaxlen {
				continue
			}
			chk = fixlenCheck(fld.Kind, fld.Maxlen, fld.Name)
		case ir.KindArray:
			// An INTEGER array is bounded in on_array_begin, which carries the
			// count as an argument and can state the element width besides. Only
			// a float array is left here: it has no declared width to state, so
			// the corelib does not call that hook for one.
			if !isNativeArrayElem(fld.Elem) || !isIntArrayElem(fld.Elem) {
				if !isNativeArrayElem(fld.Elem) || !fld.HasCount {
					continue
				}
				chk = countCheck(fld.Elem, fld.Count, fld.Name)
				break
			}
			continue
		default:
			continue
		}
		out = append(out, fmt.Sprintf("%s fld.id == %d:", kw(&inner), fld.ID))
		out = append(out, indent(chk)...)
	}
	return out
}

// arrFieldArm renders an array scope's header bounds: the §5.1 index bound, a
// bounded string/blob element's byte length, and a native row's element count.
func (g *gen) arrFieldArm(sc *pyScope) []string {
	var out []string
	if sc.child < 0 {
		// A value element is bounded here; an element that opens a scope is
		// bounded in on_sequence_begin, which no on_field precedes.
		out = append(out, g.indexBound(sc.cap, "fld.id", sc.loc)...)
	}
	switch sc.elem {
	case ir.KindString, ir.KindBlob:
		if sc.elemMaxHas {
			out = append(out, elemFixlenCheck(sc.elem, sc.elemMax, sc.loc)...)
		}
	case ir.KindArray:
		// As above: an integer row is bounded in on_array_begin.
		if isNativeArrayElem(sc.elemItems.Elem) && !isIntArrayElem(sc.elemItems.Elem) &&
			sc.elemItems.HasCount {
			out = append(out, countCheck(sc.elemItems.Elem, sc.elemItems.Count, sc.loc+" row")...)
		}
	}
	return out
}

// fixlenCheck bounds a string/blob payload by its declared byte length. The
// subtype test is §7.3: a header whose fixlen kind contradicts the schema is a
// SKIPPED field, not this field's length.
func fixlenCheck(k ir.Kind, maxlen int64, loc string) []string {
	kindWord := "string"
	if k == ir.KindBlob {
		kindWord = "blob"
	}
	return []string{
		fmt.Sprintf("if fld.subtype == %s and fld.size > %d:", pyFixlenSubtype(k), maxlen),
		fmt.Sprintf("    raise SofaDecodeError(%q)",
			fmt.Sprintf("%s: %s byte length above schema maxlen %d", loc, kindWord, maxlen)),
	}
}

func elemFixlenCheck(k ir.Kind, maxlen int64, loc string) []string {
	kindWord := "string"
	if k == ir.KindBlob {
		kindWord = "blob"
	}
	return []string{
		fmt.Sprintf("if fld.subtype == %s and fld.size > %d:", pyFixlenSubtype(k), maxlen),
		fmt.Sprintf("    raise SofaDecodeError(%q)",
			fmt.Sprintf("%s: %s element byte length above schema maxlen %d", loc, kindWord, maxlen)),
	}
}

// countCheck bounds a native array's wire element count by the schema capacity.
// The wire-type test is §7.3, as in fixlenCheck.
func countCheck(elem ir.Kind, count int64, loc string) []string {
	x := &ir.Field{Kind: ir.KindArray, Elem: elem}
	cond := "fld.type == " + pyExpectedWire(x)
	if sub := pyFixlenSubtype(elem); sub != "" {
		cond += " and fld.subtype == " + sub
	}
	return []string{
		fmt.Sprintf("if %s and fld.count > %d:", cond, count),
		fmt.Sprintf("    raise SofaDecodeError(%q)",
			fmt.Sprintf("%s: array count above schema capacity %d", loc, count)),
	}
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

// capArm renders the receiver-side decode limits (CORELIB_PLAN §6.2.1) for the
// ids of one scope that the schema leaves unbounded.
//
// The caps are enforced HERE rather than handed to the Decoder, because
// corelib-py applies a Decoder cap to every field indiscriminately -- including
// the ones whose schema declares a count:/maxlen:, which §6.2.1 forbids
// ("MUST NOT be applied to a field the schema already bounds") and §6.3 backs up
// by forbidding SofaLimitError there. The removed pull API had
// d.schema_bounded() to say so per field; the visitor surface has no equivalent,
// so the split is made where the schema is actually known: in generated code.
//
// `bounded` says the scope already emitted schema bounds, so the caps become the
// else of that chain and cannot fire on a field the schema governs.
func (g *gen) capArm(sc *pyScope, bounded bool) []string {
	if !g.limits.any() {
		return nil
	}
	// A scope whose every id IS schema-bounded has nothing left for a cap: an
	// array scope dispatches by index, so one bound covers all of it.
	if sc.isArr && bounded && sc.elemMaxHas {
		return nil
	}
	var chk []string
	if g.limits.stringHas {
		chk = append(chk,
			"if fld.subtype == FixlenSubtype.STRING and fld.size > MAX_DYN_STRING_LEN:",
			`    raise SofaLimitError("string length %d exceeds max_string_len %d" % (fld.size, MAX_DYN_STRING_LEN))`)
	}
	if g.limits.blobHas {
		chk = append(chk,
			"if fld.subtype == FixlenSubtype.BLOB and fld.size > MAX_DYN_BLOB_LEN:",
			`    raise SofaLimitError("blob length %d exceeds max_blob_len %d" % (fld.size, MAX_DYN_BLOB_LEN))`)
	}
	if g.limits.arrayHas {
		// fld.count is 0 for everything that is not an array, so the wire type
		// needs no separate test.
		chk = append(chk,
			"if fld.count > MAX_DYN_ARRAY_COUNT:",
			`    raise SofaLimitError("array count %d exceeds max_array_count %d" % (fld.count, MAX_DYN_ARRAY_COUNT))`)
	}
	if len(chk) == 0 {
		return nil
	}
	if !bounded {
		return chk
	}
	return append([]string{"else:"}, indent(chk)...)
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
// The schema capacity is checked here too, on the count argument, for the same
// reason and one hook earlier than it used to be.
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
	f.line("        or is cut short behind it. The schema capacity is checked on the count")
	f.line("        the header carries, one step ahead of the first element.")
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
		body := arrayBeginBody(fld.Elem, capOf(fld.HasCount, fld.Count), fld.Name)
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
	return arrayBeginBody(sc.elemItems.Elem,
		capOf(sc.elemItems.HasCount, sc.elemItems.Count), sc.loc+" row")
}

// arrayBeginBody is the shared body: bound the count, then state the width.
func arrayBeginBody(elem ir.Kind, cap int64, loc string) []string {
	var out []string
	if cap >= 0 {
		out = append(out,
			fmt.Sprintf("if count > %d:", cap),
			fmt.Sprintf("    raise SofaDecodeError(%q)",
				fmt.Sprintf("%s: array count above schema capacity %d", loc, cap)))
	}
	lo, hi, ok := ir.NarrowRange(elem)
	switch {
	case !ok:
		// u64/i64 (and enum/bitfield): the declared width IS the value domain,
		// so there is nothing to narrow and the default answer will do.
		if len(out) > 0 {
			out = append(out, "return None")
		}
	case lo < 0:
		out = append(out, fmt.Sprintf("return (None, %d, %d)", lo, hi))
	default:
		out = append(out, fmt.Sprintf("return (None, None, %d)", hi))
	}
	return out
}
