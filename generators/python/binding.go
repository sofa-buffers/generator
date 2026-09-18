package python

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// This file emits the DESTINATION TABLE half of the decode: a corelib-py
// `Binding` per generated class, handed to the decoder through
// `Visitor.destinations()`.
//
// A field the table names is written straight into storage this module owns --
// one 64-bit slot per numeric value in `words`, one list entry per `str`/`bytes`
// in `objects` -- with NO Python call for it at all: no typed hook, no
// `on_field`, no `on_schema_bound`. The flat visitor in visitor.go keeps every
// field the table cannot carry, and the two compose in one decoder (CORELIB_PLAN
// §5.3.1: the table is reached THROUGH the one decode surface, never beside it).
//
// The slots are moved into the dataclass once, when the decode completes, by the
// emitted `scatter()`. That is the whole trade: N callbacks during the walk
// become one straight-line assignment run afterwards.
//
// Measured, tests/bench rows `python-native` and `python` on `vehicle_telemetry`
// (Callgrind Ir/op, same corelib-py build on both sides):
//
//	row            encode              decode
//	python-native  130,523 -> 130,396  803,018 -> 585,512  (-27.1%)
//	python         1,082,511 -> 1,082,537  2,317,390 -> 2,095,758  (-9.6%)
//
// Encode is untouched: corelib-py has no encode-side table, and a decode-side one
// changes nothing about how a message is written.
//
// --- WHAT THE TABLE MAY CARRY -----------------------------------------------
//
// Two rules decide, and both are about a verdict the table cannot reach:
//
//  1. A field whose value needs a WIDTH check stays on the visitor. An entry
//     carries a declared width for an ARRAY's elements (elem_min / elem_max,
//     which the decoder applies AT each element) but none for a scalar:
//     `words[at] = value` is the whole store. Generated code cannot take that
//     verdict afterwards either -- a §7.1 rejection has to outrank a truncation
//     that follows it (MESSAGE_SPEC §5.2), and a check run at scatter time runs
//     only for a decode that already completed. So u8..u32, i8..i32 and every
//     `enum`/`bitfield` narrower than the 64-bit accumulator keep their store in
//     the typed hook, where storeValue guards them.
//
//  2. A scope is entered by the table only if every field of its PARENT is bound
//     too. The decoder descends into a bound sequence by itself and tells the
//     visitor nothing (corelib-py#146), so the visitor's `_c` still names the
//     parent scope while the walk is inside the child. Any id the child's table
//     does not name -- an UNKNOWN id, which is what forward compatibility
//     delivers -- is then offered to the visitor under the PARENT's location,
//     and an arm there would store someone else's value into this scope's field.
//     A parent that binds everything it declares has no arms at all and its
//     `on_field` declines every id that reaches it, so the misroute cannot
//     happen. Hence: a message with one wrapper array binds its own scalars and
//     no nested scope; a message of nothing but scalars and structs binds its
//     whole tree.
//
// Neither rule is a limit of the wire format; both are limits of what a table
// can say today. corelib-py#149 (a declared width on a scalar entry) and #150 (a
// child table that declines what it does not name) would lift them.

// pyBindMin is the number of rows below which a class is left entirely on the
// visitor.
//
// A table is not free: the decode allocates the words buffer, the objects list
// and the typed views over them, and runs scatter() once. Measured against the
// callbacks it saves (Callgrind Ir/op, one decode of a message with N bindable
// u64 fields beside two unbindable narrow ones, native engine):
//
//	N   with table   without    delta
//	1      26,321     23,368    +12.6%   <- a loss
//	2      28,007     27,350     +2.4%   <- still a loss
//	3      30,603     31,379     -2.5%
//	4      32,273     36,611    -11.8%
//	6      35,224     44,252    -20.4%
//
// The crossover sits between two fields and three, so three is the floor: below
// it a class keeps today's shape exactly, table and scatter and all.
const pyBindMin = 3

// pyBindArrayMax is the largest declared element count a native array may have
// and still go on the table.
//
// An array is the one kind a table materializes TWICE: the decoder writes every
// element into a slot, and the scatter then builds the list the dataclass holds
// out of those slots -- where the typed hook receives the list the corelib built
// once, in one call. The destination also costs `count` slots whether the array
// arrives or not, and the storage is prefilled per decode.
//
// So the trade turns with the count. Measured (Callgrind Ir/op, native engine,
// one decode of a message with three u64 scalars beside one u8 array, once with
// the array ON the wire and once absent):
//
//	cap    present: bound / on the visitor    absent: bound / on the visitor
//	   8      26,583 / 29,505  (-9.9%)          23,272 / 22,586  (+3.0%)
//	  16      27,396 / 29,879  (-8.3%)          23,275 / 22,586  (+3.0%)
//	  32      29,060 / 30,645  (-5.2%)          23,285 / 22,892  (+1.7%)
//	  64      32,441 / 32,253  (+0.6%)          23,445 / 22,647  (+3.5%)
//	 512      88,679 / 67,189  (+32%)           24,757 / 23,203  (+6.7%)
//	4096     544,496 / 322,369 (+69%)           56,755 / 22,956  (+147%)
//
// 32 is the last count that pays for a message carrying the array, and the price
// of an absent one stays under 2% there. Past 64 the double materialization runs
// away, and a large declared count would make every decode pay for a field the
// message may not even hold.
//
// Nothing else has this shape: a bound string or blob lands as ONE object in the
// objects list and the scatter moves the reference, and a scalar is one slot.
const pyBindArrayMax = 32

// bindAbsent is the sentinel every arrival test compares against -- a slot of
// all ones, which no arrival can write: an array's count slot holds its element
// count and every other kind's holds 1.
const bindAbsent = "0xFFFFFFFFFFFFFFFF"

// bindRow is one row of an emitted Binding, and the scatter line reading it back.
type bindRow struct {
	method string // binder method on Binding
	args   string // rendered arguments, slots included
	dest   string // scatter destination, e.g. "m.captured_at.seconds"
	expr   string // scatter source, e.g. "U[3]"
	cnt    int64  // count_at slot; the arrival test reads it
}

// bindTable is one Binding object: a class's own, or a nested scope's.
type bindTable struct {
	name    string // module-level python name
	rows    []bindRow
	seqRows []string // `.sequence(id, child=...)`, rendered after the child exists
}

// bindPlan is everything one class's table tree needs.
type bindPlan struct {
	name    string
	tables  []*bindTable // root first, children after
	boundSc map[int]bool // scope id -> entered by the table, so it has no visitor arms
	boundFd map[int]idSet
	needU   bool // the scatter reads a uint64 view (every arrival test does)
	needS   bool // ... an int64 view
	needF   bool // ... a double view
	needObj bool // the table names a string/blob slot
	rows    int  // rows over the whole tree, against pyBindMin
}

type idSet map[int64]bool

func (p *bindPlan) isBoundField(scopeID int, fid int64) bool {
	if p == nil {
		return false
	}
	return p.boundFd[scopeID][fid]
}

func (p *bindPlan) isBoundScope(scopeID int) bool {
	return p != nil && p.boundSc[scopeID]
}

// unboundFields is the scope's fields the visitor still handles.
func (p *bindPlan) unboundFields(sc *pyScope) []*ir.Field {
	if p == nil {
		return sc.fields
	}
	var out []*ir.Field
	for _, fld := range sc.fields {
		if !p.isBoundField(sc.id, fld.ID) {
			out = append(out, fld)
		}
	}
	return out
}

// --- building ---------------------------------------------------------------

// slotAlloc hands out `words` and `objects` indexes. ONE counter spans the whole
// tree: a child Binding writes into the same storage as its parent, so slots
// have to be disjoint across every table reachable from the root.
type slotAlloc struct{ words, objects int64 }

func (a *slotAlloc) word(n int64) int64 { at := a.words; a.words += n; return at }
func (a *slotAlloc) object() int64      { at := a.objects; a.objects++; return at }

// buildBindPlan decides what one class's table carries, or returns nil when it
// would carry too little to pay for itself.
func (g *gen) buildBindPlan(name string, scopes []*pyScope) *bindPlan {
	p := &bindPlan{name: name, boundSc: map[int]bool{}, boundFd: map[int]idSet{}}
	descend := g.fullyBindable(scopes, scopes[0], map[int]bool{})
	g.bindScope(p, &slotAlloc{}, scopes, scopes[0], "m", descend)
	// The root is the visitor's OWN location -- it is never entered by the table,
	// and the fields it does not bind still dispatch there.
	delete(p.boundSc, scopes[0].id)
	if p.rows < pyBindMin {
		return nil
	}
	return p
}

// fullyBindable reports whether every field of `sc` can go in the table --
// recursively, so a struct field counts only when its own scope does too. It
// decides rule 2 above: only a scope whose parent binds EVERYTHING may itself be
// entered by the table.
//
// `seen` breaks the cycle a recursive schema makes (a struct reaching itself); a
// scope still being decided is taken as bindable, which is the answer the fixed
// point settles on for a cycle that is otherwise clean.
func (g *gen) fullyBindable(scopes []*pyScope, sc *pyScope, seen map[int]bool) bool {
	if sc.isArr {
		return false // a wrapper array's elements are per-index scopes
	}
	if seen[sc.id] {
		return true
	}
	seen[sc.id] = true
	for _, fld := range sc.fields {
		switch fld.Kind {
		case ir.KindStruct, ir.KindUnion:
			child, ok := sc.seqChild[fld.ID]
			if !ok || !g.fullyBindable(scopes, scopes[child], seen) {
				return false
			}
		default:
			if !bindable(fld) {
				return false
			}
		}
	}
	return true
}

// bindScope renders one table: a row per bindable field, plus a `sequence` row
// per struct/union child when `descend` says the whole tree is bound.
//
// `path` is the scatter's name for the object this scope's values land on -- "m"
// for the message itself, "m.captured_at" for a nested struct.
func (g *gen) bindScope(p *bindPlan, alloc *slotAlloc, scopes []*pyScope,
	sc *pyScope, path string, descend bool) {

	// Named after the scope, not after the path: two different paths can spell
	// one name (a field `a` whose struct has a field `b`, beside a sibling field
	// `a_b`), and the scope tree has already made that unique -- see scopeSet.uniq.
	// Deriving it here rather than rebuilding it is what keeps the two in step: a
	// duplicate table name would hand both scopes the SAME Binding, so one would
	// decode into the other's slots and the other into none.
	t := &bindTable{name: bindTableName(sc)}
	p.tables = append(p.tables, t)
	p.boundSc[sc.id] = true
	ids := idSet{}
	p.boundFd[sc.id] = ids

	for _, fld := range sc.fields {
		dest := path + "." + pyIdent(fld.Name)
		if fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion {
			child, ok := sc.seqChild[fld.ID]
			if !descend || !ok {
				continue
			}
			g.bindScope(p, alloc, scopes, scopes[child], dest, descend)
			// No count slot: every member carries its own arrival, and the
			// dataclass already holds a default-constructed sub-object for a
			// struct that never arrives at all.
			t.seqRows = append(t.seqRows,
				fmt.Sprintf(".sequence(%d, child=%s)", fld.ID, bindTableName(scopes[child])))
			ids[fld.ID] = true
			continue
		}
		if !bindable(fld) {
			continue
		}
		t.rows = append(t.rows, bindRowFor(fld, alloc, dest))
		ids[fld.ID] = true
		p.rows++
		p.needU = true // every arrival test reads the uint64 view
		switch fld.Kind {
		case ir.KindString, ir.KindBlob:
			p.needObj = true
		case ir.KindFP32, ir.KindFP64:
			p.needF = true
		case ir.KindI64, ir.KindEnum:
			p.needS = true
		case ir.KindArray:
			switch pyBindArrayMethod(fld.Elem) {
			case "signed_array":
				p.needS = true
			case "float32_array", "float64_array":
				p.needF = true
			}
		}
	}
}

// bindTableName is a scope's Binding, named after the scope's own unique
// location name so no two scopes can ever share one table.
func bindTableName(sc *pyScope) string {
	return "_BIND_" + strings.TrimPrefix(sc.name, "_L_")
}

// bindable reports whether a table entry can carry this field with every rule
// the visitor's own store applies -- see the two rules at the top of this file.
func bindable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindString, ir.KindBlob, ir.KindBool, ir.KindU64, ir.KindI64,
		ir.KindFP32, ir.KindFP64:
		return true
	case ir.KindEnum:
		// Bindable only where the implied width IS the slot: a narrower one is a
		// §7.1 verdict per value, which an entry cannot take.
		_, _, narrow := ir.EnumWidthRange(fld.Ref)
		return !narrow
	case ir.KindBitfield:
		_, narrow := ir.BitfieldWidthMax(fld.Ref)
		return !narrow
	case ir.KindArray:
		// A wrapper array's elements are sequence-framed, and an array the
		// schema leaves unbounded has no destination to declare: the slots are
		// `cap` wide, and a count the WIRE chooses may not size storage (§6.6).
		// A large declared count is excluded on cost, not correctness --
		// see pyBindArrayMax.
		return isNativeArrayElem(fld.Elem) && fld.HasCount && fld.Count <= pyBindArrayMax
	}
	return false // u8..u32 / i8..i32: a declared width no entry carries
}

// bindRowFor allocates the field's slots and renders both halves of its row: the
// binder call and the scatter line.
func bindRowFor(fld *ir.Field, alloc *slotAlloc, dest string) bindRow {
	r := bindRow{dest: dest}
	switch fld.Kind {
	case ir.KindString, ir.KindBlob:
		at := alloc.object()
		r.cnt = alloc.word(1)
		method, maxlen := "string", int64(0)
		if fld.Kind == ir.KindBlob {
			method = "bytes"
		}
		if fld.HasMaxlen {
			maxlen = fld.Maxlen
		}
		// maxlen=0 says the schema bounds nothing here, which is what leaves the
		// receiver cap on the field (CORELIB_PLAN §6.2.1) -- the same split
		// on_schema_bound makes by answering -1.
		r.method = method
		r.args = fmt.Sprintf("%d, at=%d, maxlen=%d, count_at=%d", fld.ID, at, maxlen, r.cnt)
		r.expr = fmt.Sprintf("OB[%d]", at)
	case ir.KindArray:
		at := alloc.word(fld.Count)
		r.cnt = alloc.word(1)
		r.method = pyBindArrayMethod(fld.Elem)
		r.args = fmt.Sprintf("%d, at=%d, cap=%d, count_at=%d%s",
			fld.ID, at, fld.Count, r.cnt, bindElemWidth(fld.Elem, fld.ElemRef))
		r.expr = bindArrayExpr(fld.Elem, at, r.cnt)
	default:
		at := alloc.word(1)
		r.cnt = alloc.word(1)
		r.method, r.expr = bindScalarMethod(fld), ""
		r.args = fmt.Sprintf("%d, at=%d, count_at=%d", fld.ID, at, r.cnt)
		switch fld.Kind {
		case ir.KindBool:
			// §4.4: a boolean has no width -- every non-zero reads as true --
			// and `bool` is what the typed hook stores.
			r.expr = fmt.Sprintf("U[%d] != 0", at)
		case ir.KindI64, ir.KindEnum:
			r.expr = fmt.Sprintf("S[%d]", at)
		case ir.KindFP32, ir.KindFP64:
			r.expr = fmt.Sprintf("F[%d]", at)
		default:
			r.expr = fmt.Sprintf("U[%d]", at)
		}
	}
	return r
}

// bindScalarMethod names the binder for a scalar, matching the wire type it
// travels on -- the same split pyHook makes.
func bindScalarMethod(fld *ir.Field) string {
	switch fld.Kind {
	case ir.KindBool:
		return "boolean"
	case ir.KindI64, ir.KindEnum:
		return "signed"
	case ir.KindFP32:
		return "float32"
	case ir.KindFP64:
		return "float64"
	}
	return "unsigned"
}

// pyBindArrayMethod names the binder for a native array of `elem` -- the same
// split pyArrayHook makes.
func pyBindArrayMethod(elem ir.Kind) string {
	switch elem {
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "signed_array"
	case ir.KindFP32:
		return "float32_array"
	case ir.KindFP64:
		return "float64_array"
	default:
		return "unsigned_array"
	}
}

// bindElemWidth renders the declared element width as the binder's keyword
// arguments -- the same bound arrayBeginBody states for an unbound array: the
// declared width for an integer element, the implied one for an `enum` or a
// `bitfield` (MESSAGE_SPEC §1), and nothing for a boolean, which has no width,
// or for a float, whose binder takes none.
func bindElemWidth(elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindBool, ir.KindFP32, ir.KindFP64:
		return ""
	case ir.KindEnum:
		lo, hi, ok := ir.EnumWidthRange(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf(", elem_min=%d, elem_max=%d", lo, hi)
	case ir.KindBitfield:
		hi, ok := ir.BitfieldWidthMax(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf(", elem_max=%d", hi)
	}
	lo, hi, ok := ir.NarrowRange(elem)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf(", elem_min=%d, elem_max=%d", lo, hi)
	}
	return fmt.Sprintf(", elem_max=%d", hi)
}

// bindArrayExpr renders the scatter's list for a native array: the WIRE's own
// element count worth of slots, read through the view its elements landed in.
func bindArrayExpr(elem ir.Kind, at, cnt int64) string {
	view := "U"
	switch pyBindArrayMethod(elem) {
	case "signed_array":
		view = "S"
	case "float32_array", "float64_array":
		view = "F"
	}
	span := fmt.Sprintf("%s[%d:%d + U[%d]]", view, at, at, cnt)
	if elem == ir.KindBool {
		return fmt.Sprintf("[_v != 0 for _v in %s]", span)
	}
	return fmt.Sprintf("list(%s)", span)
}

// --- emission ---------------------------------------------------------------

// emitBindTables writes one class's module-level Binding tree, plus the two
// sizes and the prefill its visitor allocates from.
func (g *gen) emitBindTables(f *pyfile, p *bindPlan) {
	f.line("# Destination table for %s: where the decoder writes the fields it can place", p.name)
	f.line("# without calling back into Python. Built once, at import -- a Binding is a")
	f.line("# build-once artifact and every decoder over it reuses the compiled map.")
	f.line("#")
	f.line("# What is not here is on the visitor below: a value whose declared width an")
	f.line("# entry cannot carry (u8..u32, i8..i32, a narrow enum/bitfield), an array the")
	f.line("# schema leaves unbounded, and every wrapper-sequence array.")
	// Children first: a sequence row names its child table by name.
	for i := len(p.tables) - 1; i >= 0; i-- {
		emitOneTable(f, p.tables[i])
	}
	f.line("_W_%s = %s.tree_words_required", p.name, p.tables[0].name)
	f.line("_O_%s = %s.tree_objects_required", p.name, p.tables[0].name)
	f.line("# Every slot starts at ALL ONES, which no arrival can write: an array's count")
	f.line("# slot holds its element count and every other kind's holds 1. That is what")
	f.line("# tells a field that never arrived from one that arrived EMPTY -- an empty")
	f.line("# array replaces the default, and a zero count slot could not say so.")
	f.line("_FILL_%s = b\"\\xff\" * (_W_%s * 8)", p.name, p.name)
	f.blank()
}

func emitOneTable(f *pyfile, t *bindTable) {
	f.line("%s = (Binding()", t.name)
	for _, r := range t.rows {
		f.line("    .%s(%s)", r.method, r.args)
	}
	for _, s := range t.seqRows {
		f.line("    %s", s)
	}
	f.line(")")
}

// emitBindStorage writes the visitor's storage and its destinations() answer.
//
// The triple is asked for ONCE, when the decoder is built, so nothing the wire
// says can change it -- which is what §6.6 asks of a decode's storage: every
// slot exists before a byte is read and no path grows one.
func (g *gen) emitBindStorage(f *pyfile, p *bindPlan) {
	f.line("        self._w = bytearray(_FILL_%s)", p.name)
	if p.needObj {
		f.line("        self._ob: list = [None] * _O_%s", p.name)
	} else {
		f.line("        self._ob: list = []")
	}
	f.line("        # Typed views over the one buffer: no copy, no second buffer.")
	if p.needU {
		f.line("        self._vu = memoryview(self._w).cast(\"Q\")")
	}
	if p.needS {
		f.line("        self._vs = memoryview(self._w).cast(\"q\")")
	}
	if p.needF {
		f.line("        self._vf = memoryview(self._w).cast(\"d\")")
	}
	f.blank()
	f.line("    def destinations(self):")
	f.line(`        """Where the decoder is to put the fields this table names.`)
	f.line("")
	f.line("        Asked once, when the Decoder is built, so nothing the wire says can")
	f.line("        change it. A field the table names is written straight into its slot")
	f.line("        and NO hook fires for it -- not the typed one, not ``on_field``, not")
	f.line("        ``on_schema_bound``, whose bound rides the entry instead. Everything")
	f.line("        the table does not name reaches the hooks below unchanged.")
	f.line(`        """`)
	f.line("        return (%s, self._w, self._ob)", p.tables[0].name)
	f.blank()
}

// emitScatter writes the one pass that moves the table's slots onto the
// dataclass: an arrival test and an assignment per bound field, straight-line.
//
// It runs when the decode COMPLETES, and only then, so a field the table carries
// appears on the message at the moment the message is finished while everything
// the visitor handles appears as it arrives. A loop over a (kind, slot,
// attribute) table would be the same work interpreted -- measured at 32 us
// against 23 for this shape, on the same message.
func (g *gen) emitScatter(f *pyfile, p *bindPlan) {
	f.line("    def scatter(self) -> None:")
	f.line(`        """Move the table's slots onto the message.`)
	f.line("")
	f.line("        Called when the decode completes. A slot no field arrived in leaves")
	f.line("        the dataclass default standing, which is how absence is reported")
	f.line("        without inventing a sentinel value for it.")
	f.line(`        """`)
	f.line("        m = self._o")
	if p.needU {
		f.line("        U = self._vu")
	}
	if p.needS {
		f.line("        S = self._vs")
	}
	if p.needF {
		f.line("        F = self._vf")
	}
	if p.needObj {
		// Not `O`: a lone capital O is flagged as an ambiguous name by every
		// Python linter (pycodestyle E741), and generated code lands in the
		// caller's lint run.
		f.line("        OB = self._ob")
	}
	for _, t := range p.tables {
		for _, r := range t.rows {
			f.line("        if U[%d] != _ABSENT: %s = %s", r.cnt, r.dest, r.expr)
		}
	}
	f.blank()
}
