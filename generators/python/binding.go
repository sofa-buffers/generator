package python

import (
	"fmt"
	"math"
	"strconv"
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
//	row            encode                   decode
//	python-native  130,361 -> 130,544       789,779 -> 424,135    (-46.3%)
//	python         1,081,799 -> 1,082,257   2,336,693 -> 1,931,996 (-17.3%)
//
// Encode is untouched: corelib-py has no encode-side table, and a decode-side one
// changes nothing about how a message is written.
//
// --- WHAT THE TABLE MAY CARRY -----------------------------------------------
//
// Four rules decide, and each is about a verdict the table cannot reach:
//
//  1. A SHAPE the table has no entry for: a wrapper-sequence array (its elements
//     are per-index scopes), and an array the schema leaves unbounded (its
//     destination would be `count` slots chosen by the WIRE, which §6.6 forbids).
//     Both stay on the visitor.
//
//  2. A scope is entered only when its WHOLE subtree is bindable, and its table
//     is then `closed`. The decoder descends into a bound sequence by itself and
//     tells the visitor nothing (corelib-py#146), so while the walk is inside the
//     child the visitor's `_c` still names the parent -- and an id the child does
//     not name would be offered to it under the PARENT's location, where an arm
//     would store someone else's value. `Binding(closed=True)` (corelib-py#150)
//     is what makes that impossible: an id a closed table does not name is
//     skipped by the codec, sequence and all, exactly as a decoder with no
//     visitor skips it. A scope that still has an unbindable field cannot be
//     closed, so the table does not descend into it at all.
//
//  3. A UNION is on a table as a ONE-OF table (corelib-py#165), under rule 2
//     like any scope: every option -- a scalar, a string/blob, a native array
//     the rules above admit, a struct or another union whose own subtree is
//     bindable -- must be one the table can carry. `Binding(which_at=N)` makes
//     the union's rows alternatives: the decoder writes the arriving option's id
//     into words[N] behind the §7.3 tag test it already runs, so the last
//     correctly-typed option wins (MESSAGE_SPEC §7.4.1) and a mistyped or
//     unknown option neither switches nor discards the held one. The prefill
//     seeds the slot with default_id. The scatter reads the which slot FIRST
//     and then only the held option's slots: whatever a discarded option left
//     behind is stale by design and never read.
//
//     A union with a wrapper-array or unbounded-array option stays on the
//     visitor, its scope open, and so does every scope holding it. So does a
//     union as a wrapper-array ELEMENT (rule 1: per-index scopes) and a union
//     CLASS decoded on its own -- its options are the class's own fields, and
//     the root is the visitor's own location (buildBindPlan).
//
//  4. A RE-SELECTED struct/union option starts from its own default (§7.4.1):
//     the corelib resets the option's whole subtree when a different option
//     arrives (corelib-py#167), from what the table states -- `default=` on
//     every scalar row inside an option's subtree whose default is not all-zero
//     bits, `default_id=` on every union nested inside one. A string/blob row
//     restarts empty and an array at count 0; that is right for an option of
//     those kinds (§4.2 forbids a non-empty default there) but NOT for a member
//     of a struct option that declares a non-empty one, so such a member keeps
//     its union on the visitor (resetLosesDefault).
//
// Everything else is on the table, including every narrow width: an entry states
// it (`max_value` / `min_value`, corelib-py#149) and the decoder checks it at the
// value, before the store -- so a message truncated behind an out-of-width value
// is INVALID and not INCOMPLETE, which is what kept these kinds on the visitor
// until the corelib could say it.

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
	attr   string // scatter destination attribute, e.g. "seconds"
	expr   string // scatter source, e.g. "U[3]"
	cnt    int64  // count_at slot; the arrival test reads it
	id     int64  // the field id; a one-of table's scatter arm tests it
}

// bindTable is one Binding object: a class's own, or a nested scope's.
type bindTable struct {
	name    string // module-level python name
	closed  bool   // an id this table does not name is skipped by the codec
	rows    []bindRow
	seqRows []string // `.sequence(id, child=...)`, rendered after the child exists
	// kids are the struct/union children a `sequence` row enters, in field
	// order; the scatter descends into them after this table's own rows.
	kids []bindKid

	// A ONE-OF table (a union, rule 3): the `words` slot the decoder writes the
	// arriving option's id into, and the union's default_id -- what that slot
	// starts at, and (on a nested union) what the corelib resets it to. which
	// < 0 on every other table. arms are the options in field order, each an
	// index into rows or into kids.
	which    int64
	whichDef int64
	nested   bool // inside an option: the corelib resets it to default_id, so the table states it
	arms     []bindArm
	path     string // where the scope sits, e.g. "m.u.pt"; named in a comment only
}

// bindKid is a struct/union child a table descends into.
type bindKid struct {
	attr string // the member's attribute on the parent object
	mut  string // a union option's mutable_<opt>(): select unless held, return it
	t    *bindTable
}

// bindArm is one option of a one-of table: a row (row >= 0) or a kid.
type bindArm struct {
	id       int64
	row, kid int
}

// bindPlan is everything one class's table tree needs.
type bindPlan struct {
	name    string       // the class name
	base    string       // its unescaped type identifier, which the private names derive from
	tables  []*bindTable // root first, children after
	boundSc map[int]bool // scope id -> entered by the table, so it has no visitor arms
	boundFd map[int]idSet
	needU   bool // the scatter reads a uint64 view (every arrival test does)
	needS   bool // ... an int64 view
	needF   bool // ... a double view
	needObj bool // the table names a string/blob slot
	oneOf   bool // a one-of table is in the tree: the prefill seeds its which slot
	rows    int  // rows over the whole tree, against pyBindMin
	// closed says the ROOT table carries every id its scope declares, so the
	// visitor is never called at all -- not even to decline an unknown id.
	closed bool
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
func (g *gen) buildBindPlan(c decodeClass, scopes []*pyScope) *bindPlan {
	if scopes[0].union != nil {
		// A union class's own fields are its options, so its table would be a
		// one-of table at the ROOT -- the visitor's own location, with the union
		// object itself as the scatter's target. Not attempted: a union type is
		// decoded as a member, where rule 3 binds it, far more often than on its
		// own, and this path keeps the visitor exactly as before.
		return nil
	}
	p := &bindPlan{name: c.name, base: c.base, boundSc: map[int]bool{}, boundFd: map[int]idSet{}}
	// A table is CLOSED when its scope needs nothing from the visitor, which is
	// the same question that decides whether a parent may descend into it.
	p.closed = g.bindableSubtree(scopes, scopes[0], map[int]bool{})
	g.bindScope(p, &slotAlloc{}, scopes, scopes[0], "m", false)
	// The root is the visitor's OWN location -- it is never entered by the table,
	// and the fields it does not bind still dispatch there.
	delete(p.boundSc, scopes[0].id)
	if p.rows < pyBindMin {
		return nil
	}
	return p
}

// bindableSubtree reports whether every field of `sc` can go in the table --
// recursively, so a struct field counts only when its own scope does too.
//
// It decides rule 2 above twice over: a scope is DESCENDED INTO only when it
// answers true, and the table of a scope that answers true is `closed`, because
// closed is exactly the promise that nothing inside it needs the visitor.
//
// `seen` breaks the cycle a recursive schema makes (a struct reaching itself); a
// scope still being decided is taken as bindable, which is the answer the fixed
// point settles on for a cycle that is otherwise clean.
func (g *gen) bindableSubtree(scopes []*pyScope, sc *pyScope, seen map[int]bool) bool {
	return g.bindableIn(scopes, sc, seen, false)
}

// bindableIn is bindableSubtree with the one piece of context it depends on:
// whether `sc` sits inside a union OPTION's subtree, which the corelib's option
// reset writes whole (rule 4). A union's own scope puts its options there.
func (g *gen) bindableIn(scopes []*pyScope, sc *pyScope, seen map[int]bool, inOption bool) bool {
	if sc.isArr {
		return false // a wrapper array's elements are per-index scopes
	}
	if seen[sc.id] {
		return true
	}
	seen[sc.id] = true
	if sc.union != nil {
		inOption = true
	}
	for _, fld := range sc.fields {
		switch fld.Kind {
		case ir.KindStruct, ir.KindUnion:
			child, ok := sc.seqChild[fld.ID]
			if !ok || !g.bindableIn(scopes, scopes[child], seen, inOption) {
				return false
			}
		default:
			if !bindable(fld) || (inOption && resetLosesDefault(fld)) {
				return false
			}
		}
	}
	return true
}

// resetLosesDefault reports a field the corelib's option reset cannot put back
// at its declared default: a string, blob or array whose default is NOT empty.
// The reset (corelib-py#167) starts every string/blob row at "" and every array
// at count 0 -- right for an option of those kinds, which §4.2 forbids a
// non-empty default, but not for a MEMBER of a struct option, which may declare
// one. Such a member keeps its whole union on the visitor (rule 4).
func resetLosesDefault(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindString, ir.KindBlob:
		s, _ := fld.Default.(string)
		return strings.TrimSpace(s) != ""
	case ir.KindArray:
		vals, _ := fld.Default.([]any)
		return len(vals) > 0
	}
	return false
}

// bindScope renders one table: a row per bindable field, plus a `sequence` row
// per struct/union child whose OWN subtree is bindable. A union's own scope
// renders as a one-of table (rule 3): its rows and kids are the options, plus
// the which slot.
//
// `path` names the object this scope's values land on -- "m" for the message
// itself, "m.captured_at" for a nested struct. The scatter derives its own
// expressions; path only labels a which slot's prefill. `inOption` says the
// scope sits inside a union option's subtree, whose scalar rows state their
// declared default (rule 4).
func (g *gen) bindScope(p *bindPlan, alloc *slotAlloc, scopes []*pyScope,
	sc *pyScope, path string, inOption bool) *bindTable {

	// Named after the scope's own path suffix, with the location constant's
	// injective spelling (scopeSet): a duplicate table name would hand two
	// scopes the SAME Binding, so one would decode into the other's slots and
	// the other into none.
	t := &bindTable{name: bindTableName(p.base, sc), closed: g.bindableSubtree(scopes, sc, map[int]bool{}), which: -1, path: path}
	// A union's own option rows are inside an option's subtree only when the
	// union itself is: a top-level option is written whole by its own arrival,
	// so the reset's value for it is never read.
	rowsInOption := inOption
	if sc.union != nil {
		// Only ever reached for a union bindableSubtree admitted, so the table
		// is closed too: an option id a newer sender added is skipped by the
		// codec, not handed to the visitor under the parent's location.
		t.which = alloc.word(1)
		t.whichDef = sc.union.d.f.ID
		t.nested = inOption
		p.oneOf = true
		inOption = true
	}
	p.tables = append(p.tables, t)
	p.boundSc[sc.id] = true
	ids := idSet{}
	p.boundFd[sc.id] = ids

	for _, fld := range sc.fields {
		attr := pyIdent(fld.Name)
		if fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion {
			child, ok := sc.seqChild[fld.ID]
			// Only into a subtree that needs nothing from the visitor: its table
			// is then closed, so an id it does not name is skipped by the codec
			// rather than offered to the visitor under THIS scope's location. A
			// union that cannot be a one-of table (rules 3 and 4) is not such a
			// subtree: its scope stays open, and the visitor receives the union's
			// on_sequence_begin and switches there.
			if !ok || !g.bindableIn(scopes, scopes[child], map[int]bool{}, inOption) {
				continue
			}
			ct := g.bindScope(p, alloc, scopes, scopes[child], path+"."+attr, inOption)
			// No count slot: every member carries its own arrival, a union its
			// which slot, and the dataclass already holds a default-constructed
			// sub-object for a struct or union that never arrives at all.
			t.seqRows = append(t.seqRows,
				fmt.Sprintf(".sequence(%d, child=%s)", fld.ID, ct.name))
			k := bindKid{attr: attr, t: ct}
			if sc.union != nil {
				k.mut = sc.union.byField[fld].mut
				t.arms = append(t.arms, bindArm{id: fld.ID, row: -1, kid: len(t.kids)})
			}
			t.kids = append(t.kids, k)
			ids[fld.ID] = true
			continue
		}
		if !bindable(fld) {
			continue
		}
		r := bindRowFor(fld, alloc, attr, g.bindDefault(fld, rowsInOption))
		r.id = fld.ID
		if sc.union != nil {
			t.arms = append(t.arms, bindArm{id: fld.ID, row: len(t.rows), kid: -1})
		}
		t.rows = append(t.rows, r)
		ids[fld.ID] = true
		p.rows++
		p.needU = true // every arrival test reads the uint64 view
		switch fld.Kind {
		case ir.KindString, ir.KindBlob:
			p.needObj = true
		case ir.KindFP32, ir.KindFP64:
			p.needF = true
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
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
	return t
}

// bindTableName is a scope's Binding: `_` + base + `__Bind` + the scope's path,
// the same private spelling as its location constant (scopeSet) with another
// role word, so no two scopes can ever share one table.
func bindTableName(base string, sc *pyScope) string {
	return "_" + base + "__Bind" + sc.suffix
}

// words, objects and fill are a class's table sizes and slot prefill:
// `_` + base + `__Words` / `__Objects` / `__Fill`.
func (p *bindPlan) words() string   { return "_" + p.base + "__Words" }
func (p *bindPlan) objects() string { return "_" + p.base + "__Objects" }
func (p *bindPlan) fill() string    { return "_" + p.base + "__Fill" }

// bindable reports whether a table entry can carry this field with every rule
// the visitor's own store applies -- see the two rules at the top of this file.
func bindable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return false // decided per scope, not per field: see bindableSubtree
	case ir.KindArray:
		// A wrapper array's elements are sequence-framed, and an array the
		// schema leaves unbounded has no destination to declare: the slots are
		// `cap` wide, and a count the WIRE chooses may not size storage (§6.6).
		// A large declared count is excluded on cost, not correctness --
		// see pyBindArrayMax.
		return isNativeArrayElem(fld.Elem) && fld.HasCount && fld.Count <= pyBindArrayMax
	}
	// Every scalar kind, narrow ones included: the entry states the declared
	// width and the decoder checks it at the value (corelib-py#149).
	return true
}

// bindRowFor allocates the field's slots and renders both halves of its row: the
// binder call and the scatter line.
func bindRowFor(fld *ir.Field, alloc *slotAlloc, attr, dflt string) bindRow {
	r := bindRow{attr: attr}
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
		r.args = fmt.Sprintf("%d, at=%d, count_at=%d%s%s",
			fld.ID, at, r.cnt, bindScalarWidth(fld), dflt)
		switch fld.Kind {
		case ir.KindBool:
			// §4.4: a boolean has no width -- every non-zero reads as true --
			// and `bool` is what the typed hook stores.
			r.expr = fmt.Sprintf("U[%d] != 0", at)
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
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
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "signed"
	case ir.KindFP32:
		return "float32"
	case ir.KindFP64:
		return "float64"
	}
	return "unsigned"
}

// bindDefault renders a scalar row's declared default as the binder's
// `default=` argument (corelib-py#167) -- only inside a union option's subtree,
// the one place the corelib reads it: a re-selected option starts from it
// (MESSAGE_SPEC §7.4.1, rule 4). Everywhere else an absent field must leave its
// slot at the all-ones "never arrived", so the corelib ignores it there and it
// is not emitted.
//
// Empty for a default whose slot pattern is all zeros: that is the binder's own
// default, so stating it would only lengthen the row. A boolean is stated as 1
// (§4.4), an enum or bitfield as its integer, a float exactly as the schema
// writes it -- the corelib rounds an fp32 default through single precision.
func (g *gen) bindDefault(fld *ir.Field, inOption bool) string {
	if !inOption {
		return ""
	}
	lit := ""
	switch fld.Kind {
	case ir.KindString, ir.KindBlob, ir.KindArray:
		return "" // §4.2 / rule 4: these start empty, and resetLosesDefault keeps the rest off
	case ir.KindBool:
		if b, _ := fld.Default.(bool); b {
			lit = "1"
		}
	case ir.KindBitfield:
		if v := g.bitfieldDefault(fld); v != 0 {
			lit = strconv.FormatUint(v, 10)
		}
	case ir.KindFP32, ir.KindFP64:
		lit = pyFloatLit(fld.Default)
	default: // the integers and enum
		if fld.Default != nil {
			if s := scalarLit(fld.Default); s != "0" {
				lit = s
			}
		}
	}
	if lit == "" {
		return ""
	}
	return ", default=" + lit
}

// pyFloatLit renders a float default as a Python float literal, "" when it is
// +0.0 (the all-zeros pattern). Shortest round-trip form, and always a float:
// "-0" would read back as the integer 0 and lose the sign bit.
func pyFloatLit(v any) string {
	var x float64
	switch n := v.(type) {
	case float64:
		x = n
	case int:
		x = float64(n)
	case int64:
		x = float64(n)
	case uint64:
		x = float64(n)
	default:
		return ""
	}
	if x == 0 && !math.Signbit(x) {
		return ""
	}
	s := strconv.FormatFloat(x, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}

// bindScalarWidth renders the declared width as the scalar binder's keyword
// arguments -- the same bound storeValue used to raise on, moved into the entry
// where the decoder applies it at the value (corelib-py#149).
//
// The width is the declared one for an integer, and the one the declaration
// IMPLIES for an `enum` or a `bitfield` (MESSAGE_SPEC §1): the smallest signed
// type holding every constant, the smallest unsigned type holding the highest
// `pos`. Empty where the implied width IS the 64-bit slot the value lands in,
// and for a boolean, which §4.4 gives no width at all, and for the floats.
func bindScalarWidth(fld *ir.Field) string {
	min, max, ok := declaredWidth(fld.Kind, fld.Ref)
	switch {
	case !ok:
		return ""
	case min != "":
		return fmt.Sprintf(", min_value=%s, max_value=%s", min, max)
	}
	return fmt.Sprintf(", max_value=%s", max)
}

// declaredWidth is the range a value of kind k is bound to by its declared
// width, as Python literals: the declared width for an integer, the one the
// declaration IMPLIES for an `enum` (the smallest signed type holding every
// constant: two-sided) or a `bitfield` (the smallest unsigned type holding the
// highest `pos`: one-sided, its floor is the unsigned wire type's 0) --
// MESSAGE_SPEC §1. min is "" for a one-sided (unsigned) width. ok is false
// where the width IS the 64-bit slot the value lands in (u64, i64, an enum
// needing i64, a bitfield whose highest `pos` is 32 or above), for a boolean,
// which §4.4 gives no width, and for the floats.
//
// The one source of these numbers for both directions: the decode side hands
// them to the destination table (bindScalarWidth, bindElemWidth), the encode
// side to the corelib writer (widthBounds), so the two cannot drift.
func declaredWidth(k ir.Kind, ref *ir.TypeRef) (min, max string, ok bool) {
	switch k {
	case ir.KindBool, ir.KindFP32, ir.KindFP64:
		return "", "", false
	case ir.KindEnum:
		lo, hi, ok := ir.EnumWidthRange(ref)
		if !ok {
			return "", "", false
		}
		return fmt.Sprint(lo), fmt.Sprint(hi), true
	case ir.KindBitfield:
		// Stated as the width's maximum rather than as a mask: the two refuse
		// exactly the same values, since a mask of a width IS [0, width max].
		hi, ok := ir.BitfieldWidthMax(ref)
		if !ok {
			return "", "", false
		}
		return "", fmt.Sprint(hi), true
	}
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return "", "", false
	}
	if lo < 0 {
		return fmt.Sprint(lo), fmt.Sprint(hi), true
	}
	return "", fmt.Sprint(hi), true
}

// pyBindArrayMethod names the binder for a native array of `elem` -- the same
// split pyArrayHook makes.
//
// A boolean element takes `boolean_array` (corelib-py#158), the element half of
// the `boolean` the scalar takes: it is accepted under the array-of-unsigned tag,
// because §4.4 gives a boolean no wire type of its own, and the corelib maps each
// element to 0/1 as the array completes. Its slots stay in the UNSIGNED view --
// bindArrayExpr's default -- so only the binder's name changes.
//
// Routing a boolean here is also what keeps §4.4's "no width at all" out of reach
// of a mistake: `unsigned_array` HAS an `elem_max`, and binding a boolean with a
// ceiling of 1 would make 256 INVALID, which §4.4 forbids. `boolean_array` takes
// no such argument, so that bug -- #581, in the C++ backend -- cannot be written
// here at all.
func pyBindArrayMethod(elem ir.Kind) string {
	switch elem {
	case ir.KindBool:
		return "boolean_array"
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
	min, max, ok := declaredWidth(elem, ref)
	switch {
	case !ok:
		return ""
	case min != "":
		return fmt.Sprintf(", elem_min=%s, elem_max=%s", min, max)
	}
	return fmt.Sprintf(", elem_max=%s", max)
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
		// The slot is an int and the field is a bool, so this CONVERTS; it no
		// longer normalizes, `boolean_array` having stored 0/1 already. It stays
		// `!= 0` rather than `== 1` deliberately: `== 1` is correct against the
		// binder above and silently turns a non-canonical true into FALSE if that
		// ever stops holding, which is the shape #581 had.
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
	f.line("# schema leaves unbounded, every wrapper-sequence array, and a union with an")
	f.line("# option of those shapes. Every other union is a one-of table: the decoder")
	f.line("# writes the held option's id into its `which_at` slot, and a re-selected")
	f.line("# option starts again from the `default=` / `default_id=` its rows state.")
	// Children first: a sequence row names its child table by name.
	for i := len(p.tables) - 1; i >= 0; i-- {
		emitOneTable(f, p.tables[i])
	}
	f.line("%s = %s.tree_words_required", p.words(), p.tables[0].name)
	f.line("%s = %s.tree_objects_required", p.objects(), p.tables[0].name)
	f.line("# Every slot starts at ALL ONES, which no arrival can write: an array's count")
	f.line("# slot holds its element count and every other kind's holds 1. That is what")
	f.line("# tells a field that never arrived from one that arrived EMPTY -- an empty")
	f.line("# array replaces the default, and a zero count slot could not say so.")
	// The which-slot seed below is NOT what makes absence decode correctly: every
	// leaf arm of the scatter tests its own arrival and every struct/union arm
	// only selects an option at its default, so a which slot left at all ones
	// would match no arm and leave the same default standing. What the seed does
	// is make the slot state the value, which the corelib reads: holding
	// default_id, the first arrival of that option is a continuation (§7.4)
	// rather than a switch that replays the option's reset first. The result is
	// the same either way, so it is a saved reset, not a correctness rule.
	if !p.oneOf {
		f.line("%s = b\"\\xff\" * (%s * 8)", p.fill(), p.words())
		f.blank()
		return
	}
	f.line("%s = bytearray(b\"\\xff\" * (%s * 8))", p.fill(), p.words())
	f.line("# A union's which slot starts at its default_id instead, so it states the value")
	f.line("# an absent union or an empty frame holds. Absence does not depend on it --")
	f.line("# scatter() tests each option's own arrival -- but the first arrival of")
	f.line("# default_id then continues the held option instead of resetting it.")
	for _, t := range p.tables {
		if t.which >= 0 {
			f.line("memoryview(%s).cast(\"Q\")[%d] = %d  # %s", p.fill(), t.which, t.whichDef, t.path)
		}
	}
	f.line("%s = bytes(%s)", p.fill(), p.fill())
	f.blank()
}

func emitOneTable(f *pyfile, t *bindTable) {
	if t.closed {
		// Everything this scope declares is on the table, so an id that is not
		// on it is one the schema does not name: the codec skips it -- sequence
		// and all -- instead of offering it to a visitor that is not tracking
		// this scope (corelib-py#150).
		switch {
		case t.which >= 0 && t.nested:
			// A union inside an option: when that option is re-selected the
			// corelib resets this which slot too, to the default_id stated here
			// (corelib-py#167, rule 4).
			f.line("%s = (Binding(closed=True, which_at=%d, default_id=%d)", t.name, t.which, t.whichDef)
		case t.which >= 0:
			// A one-of table: its rows are alternatives, and the decoder writes
			// the arriving option's id into words[which] (corelib-py#165).
			f.line("%s = (Binding(closed=True, which_at=%d)", t.name, t.which)
		default:
			f.line("%s = (Binding(closed=True)", t.name)
		}
	} else {
		f.line("%s = (Binding()", t.name)
	}
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
	f.line("        self._w = bytearray(%s)", p.fill())
	if p.needObj {
		f.line("        self._ob: list = [None] * %s", p.objects())
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
	f.line("        ``on_schema_bound``, whose bound rides the entry instead.")
	if p.closed {
		f.line("")
		f.line("        The table is ``closed``, so an id it does not name is skipped by the")
		f.line("        codec: nothing reaches this class at all.")
	} else {
		f.line("        Everything the table does not name reaches the hooks below unchanged.")
	}
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
	emitTableScatter(f, p.tables[0], "m", "        ", 0)
	f.blank()
}

// emitTableScatter writes one table's part of the scatter onto `obj`: its rows,
// then each struct/union child onto the child's attribute. `depth` counts the
// one-of tables enclosing this one, so a nested union's locals never clobber an
// enclosing option's object while a later sibling still reads it.
func emitTableScatter(f *pyfile, t *bindTable, obj, ind string, depth int) {
	if t.which >= 0 {
		emitOneOfScatter(f, t, obj, ind, depth)
		return
	}
	for _, r := range t.rows {
		f.line("%sif U[%d] != _ABSENT: %s.%s = %s", ind, r.cnt, obj, r.attr, r.expr)
	}
	for _, k := range t.kids {
		emitTableScatter(f, k.t, obj+"."+k.attr, ind, depth)
	}
}

// emitOneOfScatter selects a bound union's held option: the which slot FIRST,
// then that option's slots and no other's -- a discarded option's slots are
// stale by design (rule 3).
//
// A leaf option keeps its own arrival test: the which slot starts at
// default_id, so default_id held says nothing about whether its value arrived,
// and one that did not leaves the union's own default standing. That test --
// not the prefill's default_id -- is what makes an absent union decode as its
// default: with the slot at all ones no arm matches, and the same default
// stands. A struct/union option has no slot of its own to test: it is selected
// through mutable_<opt>() -- the held object on a fresh message, else a new one
// at the option's default -- and its members follow with their own tests. A
// member the corelib reset (a re-selected option, §7.4.1) reads as arrived at
// the default the row states (rule 4), which is the value it must have.
//
// The stores are the ones the visitor's unionStore and mutable_<opt>() make; the
// scatter runs once, on the fully decoded message, so it selects with the last
// arrival.
func emitOneOfScatter(f *pyfile, t *bindTable, obj, ind string, depth int) {
	sfx := ""
	if depth > 0 {
		sfx = strconv.Itoa(depth)
	}
	u, x, v := "_u"+sfx, "_x"+sfx, "_v"+sfx
	f.line("%s%s = %s", ind, u, obj)
	f.line("%s%s = U[%d]", ind, x, t.which)
	first := true
	for _, a := range t.arms {
		if a.row >= 0 {
			r := t.rows[a.row]
			f.line("%s%s %s == %d and U[%d] != _ABSENT:", ind, kw(&first), x, a.id, r.cnt)
			f.line("%s    %s._which = %d", ind, u, a.id)
			f.line("%s    %s._value = %s", ind, u, r.expr)
			continue
		}
		k := t.kids[a.kid]
		f.line("%s%s %s == %d:", ind, kw(&first), x, a.id)
		sel := fmt.Sprintf("%s.%s()", u, k.mut)
		if k.t.which >= 0 {
			// A union option: the nested scatter names it itself.
			emitOneOfScatter(f, k.t, sel, ind+"    ", depth+1)
			continue
		}
		f.line("%s    %s = %s", ind, v, sel)
		emitTableScatter(f, k.t, v, ind+"    ", depth+1)
	}
}
