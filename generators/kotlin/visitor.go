package kotlin

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// frameKind classifies a decode location in the flat-visitor state machine.
type frameKind int

const (
	fkNormal    frameKind = iota // object location: scalar/composite field routing
	fkSeqLeaf                    // string/blob array: elements via string/blob cb
	fkSeqObj                     // struct/union array: sequenceBegin adds an element
	fkNativeMat                  // nested array, native inner: arrayBegin/element per row
	fkSeqMat                     // nested array, sequence inner: sequenceBegin adds a row
)

type frame struct {
	idx    int
	kind   frameKind
	loc    string
	path   string      // fkNormal: object path
	fields []*ir.Field // fkNormal
	// array (fkSeqLeaf/fkSeqObj/fkNativeMat/fkSeqMat):
	listExpr  string  // the MutableList this frame collects into
	elemKind  ir.Kind // fkSeqLeaf: KindString / KindBlob
	childLoc  string  // fkSeqObj: element loc; fkSeqMat: inner-row loc
	elemType  string  // fkSeqObj: Kotlin class for the gap fill
	innerElem ir.Kind // fkNativeMat: inner element kind
	// schema bounds, for the receiver-side decode limits (generator#102):
	innerHasCount bool // fkNativeMat: the inner array declares a count
	// innerCap is the inner array's own schema count N (-1 == none) -- the bound on
	// a ROW's element count, which `cap` does not give: cap bounds the row's ID
	// against the outer array's capacity. Both are needed, for different reasons:
	// the id bound stops an over-index gap-fill, this one stops a row header
	// claiming more elements than the schema allows it (§7.1).
	innerCap int64
	// cap is the wrapper array's schema count bound N (-1 == no count). N is a
	// CAPACITY, not a length (MESSAGE_SPEC §3): it never reaches the wire and
	// never adds elements the wire did not carry. All it does here is bound the
	// array -- an element id >= N is a schema-bound violation (§5.1/§7),
	// rejected as INVALID before the list grows, which also bounds the id-keyed
	// gap fill against an over-index heap-amplification DoS.
	cap int64
	// emax is the fkSeqLeaf string/blob element's schema maxlen L (-1 == no
	// bound): an element whose wire byte length exceeds L is malformed input,
	// rejected as INVALID (MESSAGE_SPEC §7.1) before any bytes accumulate.
	emax int64
}

func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

func boundOf(hasMax bool, max int64) int64 {
	if hasMax {
		return max
	}
	return -1
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walk func(loc, path string, fields []*ir.Field)
	var addArray func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax, cap int64)
	walk = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{kind: fkNormal, loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walk(loc+"_"+fld.Name, path+"."+ktIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+ktIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, fld.ElemMaxHas, fld.ElemMax, capOf(fld.HasCount, fld.Count))
			}
		}
	}
	// addArray registers the frame(s) entered inside the wrapper sequence of a
	// sequence-typed array. listExpr is the MutableList the frame collects into;
	// `row` reaches the element the current element id names (never the
	// last-appended one -- see the placement notes on sequenceBegin/arrayBegin).
	addArray = func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax, cap int64) {
		row := listExpr + "[" + elemIdxVar(loc) + "]"
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{kind: fkSeqLeaf, loc: loc, listExpr: listExpr, elemKind: elem, cap: cap, emax: boundOf(elemMaxHas, elemMax)})
		case ir.KindStruct, ir.KindUnion:
			elemLoc := loc + "_e"
			out = append(out, frame{kind: fkSeqObj, loc: loc, listExpr: listExpr, childLoc: elemLoc, elemType: g.typeName(ref.Key), cap: cap})
			// The element id IS the array index (MESSAGE_SPEC §5.1), so the
			// element a child field writes into is the one sequenceBegin PLACED
			// at that index -- NOT the last one appended. A flat visitor has no
			// per-element child visitor to carry the position, so the index is
			// parked in a visitor field and the child accessor path reads it back.
			walk(elemLoc, row, ref.Target.Fields)
		case ir.KindArray:
			// A row of a matrix is placed at the index its element id names,
			// exactly like every other element kind. Appending would shift every
			// later row down by one across an interior id gap -- which an omitted
			// all-default row makes reachable (§2).
			if nativeArrayElem(items.Elem) {
				out = append(out, frame{kind: fkNativeMat, loc: loc, listExpr: listExpr, innerElem: items.Elem, innerHasCount: items.HasCount, innerCap: capOf(items.HasCount, items.Count), cap: cap})
			} else {
				innerLoc := loc + "_e"
				out = append(out, frame{kind: fkSeqMat, loc: loc, listExpr: listExpr, childLoc: innerLoc, cap: cap})
				addArray(innerLoc, row, items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax, capOf(items.HasCount, items.Count))
			}
		}
	}
	walk("Root", "m", m.Fields)
	for i := range out {
		out[i].idx = i
	}
	return out
}

// elemIdxVar is the visitor field holding the array index of the wrapper-array
// element currently being decoded at loc. Non-identifier characters in loc are
// folded to '_' so the name is always a legal Kotlin identifier.
func elemIdxVar(loc string) string {
	var b strings.Builder
	b.WriteString("_ex_")
	for _, r := range loc {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// locIndex maps a loc name to its frame index (for sequenceBegin targets).
func locIndex(fs []frame, loc string) int {
	for _, fr := range fs {
		if fr.loc == loc {
			return fr.idx
		}
	}
	return 0
}

// locName is the human-readable field path of a frame loc for error details.
func locName(loc string) string {
	if len(loc) > 5 && loc[:5] == "Root_" {
		return loc[5:]
	}
	return loc
}

// invalidThrow renders the schema-bound rejection. Kotlin has no checked
// exceptions, so the corelib's own SofabException is thrown straight from a
// Visitor callback -- no unchecked wrapper of the kind Java needs, and the
// category reaching the caller is exactly the one the format defines.
func invalidThrow(detail string) string {
	return fmt.Sprintf("throw SofabException(SofabError.INVALID_MSG, %s)", ktStringLit(detail))
}

// limitThrow renders the generator#102 rejection: the LIMIT_EXCEEDED category, a
// receiver POLICY error deliberately kept distinct from wire malformation --
// the bytes are well formed and decode under a looser or unset limit.
func limitThrow(name, noun string, limit int64) string {
	return fmt.Sprintf("throw SofabException(SofabError.LIMIT_EXCEEDED, %s)",
		ktStringLit(fmt.Sprintf("%s: %s %d", name, noun, limit)))
}

// overIndexGuard returns the reject clause for a wrapper array's element id,
// emitted ahead of the grow it bounds.
//
// A wrapper array carries no count HEADER: its elements are keyed by an
// unbounded varint index and the collector grows the list to id + 1, so the
// index IS the array's length (MESSAGE_SPEC §5.1 — two elements at id 0 and id
// 16383 are a 16384-slot list). A single over-index element is therefore an
// amplification vector by itself, and it is the INDEX that has to be bounded:
// capping how many elements arrived would not bound the allocation, because a
// sparse array allocates by its highest id.
//
// Which bound applies depends on whether the schema counts the array, and the
// two differ only in that and in what the failure is called (ARCHITECTURE §9.5):
// `count: N` makes id >= N INVALID_MSG (the bytes contradict the agreed schema),
// no count makes id >= MAX_DYN_ARRAY_COUNT LIMIT_EXCEEDED (the bytes are well
// formed and the same message decodes under a looser cap — folding the two
// together is forbidden by CORELIB_PLAN §6.2.1).
//
// This is the half of the index bound that is still GENERATED code: the frames
// whose gap fill is emitted here -- a string/blob leaf, a struct element. A
// matrix row's index rides `Seq.reserveRow*`/`Seq.reserveRowList` instead, which
// take both numbers and reject the id before they grow the outer list; the two
// routes are exclusive, so no frame kind is bounded twice (§6.2.1, "one
// implementation, wherever it runs").
func (g *gen) overIndexGuard(cap int64, name string) string {
	if cap < 0 {
		if !g.limArr {
			panic("kotlin: uncounted wrapper array with no cap constant -- every target has a finite default (§9.5)")
		}
		return fmt.Sprintf("if (id >= MAX_DYN_ARRAY_COUNT) %s; ",
			limitThrow(locName(name)+" element", "array index above configured limit", g.limits.arrayCount))
	}
	return fmt.Sprintf("if (id >= %d) %s; ", cap,
		invalidThrow(fmt.Sprintf("%s element: array index above schema capacity %d", locName(name), cap)))
}

// widthThrow renders the declared-width rejection (MESSAGE_SPEC §7.1): a
// narrow-width destination receiving a value outside its declared range is
// malformed input, rejected as INVALID -- never masked to the width, never kept.
// Returns "" for the 64-bit kinds, whose range IS the accumulator the value
// arrives in.
//
// The `value < 0` term is not redundant on the unsigned side: the corelib
// delivers an unsigned wire value as a `Long`, so a u64 at or above 2^63 arrives
// with its sign bit set and `value > 255` alone would read it as negative,
// letting precisely the largest values through.
//
// `enum` is covered too, at the signed 32-bit range MESSAGE_SPEC §1 binds it to
// -- this target stores an enum as an `Int`, so the bound and the storage are
// the same fact, and letting an out-of-range value through would be the silent
// truncation §7.1 rules out.
func widthThrow(k ir.Kind, name string) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		if k != ir.KindEnum {
			return ""
		}
		lo, hi = -2147483648, 2147483647
	}
	cond := fmt.Sprintf("value < 0L || value > %dL", hi)
	if lo < 0 {
		cond = fmt.Sprintf("value < %dL || value > %dL", lo, hi)
	}
	return fmt.Sprintf("if (%s) %s; ", cond,
		invalidThrow(fmt.Sprintf("%s: value outside declared width %s", name, k)))
}

// ---------------------------------------------------------------------------
// Array wire-kind partitioning (MESSAGE_SPEC §7.3)
// ---------------------------------------------------------------------------

func isUnsignedElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
}
func isSignedElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
}

// unsignedArrayElem / signedArrayElem / fp32ArrayElem / fp64ArrayElem partition
// the native array element kinds by the wire ArrayKind an array of them maps to
// (MESSAGE_SPEC §1/§3): unsigned-array for u*/boolean/bitfield, signed-array for
// i*/enum, and -- since the fixlen header names its element subtype
// (CORELIB_PLAN §4.8) -- FP32 for fp32 and FP64 for fp64, one kind each rather
// than a single collapsed "fixlen" bucket. A header carrying any OTHER kind at
// such a field is a wire-type contradiction and must be skipped whole (§7.3) --
// never stored, and never sized into the declared field.
func unsignedArrayElem(k ir.Kind) bool {
	return isUnsignedElem(k) || k == ir.KindBool || k == ir.KindBitfield
}
func signedArrayElem(k ir.Kind) bool { return isSignedElem(k) || k == ir.KindEnum }
func fp32ArrayElem(k ir.Kind) bool   { return k == ir.KindFP32 }
func fp64ArrayElem(k ir.Kind) bool   { return k == ir.KindFP64 }

// arrayWireKind is the ArrayKind constant an array of `k` is encoded with -- the
// only kind whose header may decode into that field.
func arrayWireKind(k ir.Kind) string {
	switch {
	case unsignedArrayElem(k):
		return "UNSIGNED"
	case signedArrayElem(k):
		return "SIGNED"
	case fp32ArrayElem(k):
		return "FP32"
	case fp64ArrayElem(k):
		return "FP64"
	}
	return ""
}

// nativeElemCb maps a native array element kind to the corelib callback that
// delivers its values.
func nativeElemCb(k ir.Kind) string {
	switch k {
	case ir.KindFP32:
		return "fp32"
	case ir.KindFP64:
		return "fp64"
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "signed"
	default: // unsigned, bool, bitfield
		return "unsigned"
	}
}

// ---------------------------------------------------------------------------
// The emitted visitor
// ---------------------------------------------------------------------------

// rowCursor is the visitor field holding the primitive matrix row currently
// being filled. The row also lives in the message's MutableList at its element
// index, but reading it back through the list on every element -- and storing
// the grown array back -- is per-element work for a reference that changes at
// most log2(count) times.
func rowCursor(arrType string) string { return "_arow" + baseSuffix(arrType) }

func (g *gen) emitVisitor(f *kfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	primTypes := primArrayTypesUsed(fs)
	rowTypes := primRowTypesUsed(fs)
	limArr, limStr, limBlob := g.activeLimits(fs)
	g.limArr = limArr // for overIndexGuard, which cannot reach fs
	// A native array is the only thing that fills an array destination, and a
	// string or blob destination is the only thing that ever reassembles a split
	// payload -- so a message with neither carries neither piece of state. The
	// accumulator in particular is a per-decode allocation that would otherwise be
	// made for every message whether or not anything could reach it.
	hasArray := len(primTypes) > 0
	needsAcc := len(kindDests(fs, ir.KindString)) > 0 || len(kindDests(fs, ir.KindBlob)) > 0

	f.line("/**")
	f.line(" * Flat decode visitor for [%s].", name)
	f.line(" *")
	f.line(" * The corelib drives this with flat callbacks, so decode is a (location, id)")
	f.line(" * state machine: `cur` names the scope, `stk` restores it at a sequence end,")
	f.line(" * and every callback routes on the pair. Nothing here is public -- it is the")
	f.line(" * streaming decode half of the message API, reached through [%s.decode],", name)
	f.line(" * [%s.tryDecode] and [%s.Decoder].", name, name)
	f.line(" */")
	f.line("internal class %sVisitor(private val m: %s) : Visitor {", name, name)
	f.line("    private var cur = 0")
	// The SKIPPED-SUBTREE scope. sequenceBegin moves here for any (scope, id) the
	// schema does not declare, and every callback dispatches on `cur` with an arm
	// per real scope -- so nothing matches while cur is DEAD and the whole
	// subtree is discarded, children included.
	if hasArray {
		f.line("    private var ai = 0                  // index into the primitive array currently being filled")
	}
	// §7.3 array-vs-scalar skip counter: an integer array whose id is declared as
	// a SCALAR is a wire-type contradiction and must be skipped like an unknown
	// id. The corelib delivers array elements one-by-one through the same
	// unsigned()/signed() callbacks a lone scalar uses, so the id dispatch alone
	// cannot tell them apart; arrayBegin arms this with the announced element
	// count and the callbacks discard exactly that many.
	f.line("    private var askip = 0               // elements left to discard from a wire-type-contradictory array (S7.3)")
	// §7.3 mirror: a bare scalar delivered at an id whose declared type is an
	// ARRAY of that element type would otherwise be routed into the array-fill
	// arm and stored as element 0. Only a fill arrayBegin armed can be open, so
	// `afill != 0` is the whole test.
	if hasArray {
		f.line("    private var afill = 0               // elements still expected by an armed native-array fill (S7.3)")
		f.line("    private var atgt = 0                // which destination the armed fill writes into")
	}
	if hasBulk(fs) {
		f.line("    private var abulk: Any? = null      // destination offered to Visitor.arrayBulk, null when not offered")
	}
	f.line("    private var stk = IntArray(16)      // sequence scope stack")
	f.line("    private var sp = 0")
	for _, fr := range fs {
		switch fr.kind {
		case fkSeqObj, fkSeqMat, fkNativeMat:
			f.line("    private var %s = 0  // index of the element being decoded in %s (S5.1: the element id IS the index)", elemIdxVar(fr.loc), fr.loc)
		}
	}
	for _, t := range rowTypes {
		f.line("    private var %s: %s = %s  // primitive matrix row currently being filled", rowCursor(t), t, emptyArrayExpr(t))
	}
	if needsAcc {
		f.line("    private val acc = PayloadAcc()      // reassembly of a string/blob payload split across chunks")
	}
	if limArr || limStr || limBlob {
		f.line("    // Receiver-side decode limits, baked from the sofabgen config: caps on")
		f.line("    // fields the schema left unbounded (no count / maxlen). Exceeding one")
		f.line("    // fails the decode with SofabError.LIMIT_EXCEEDED at the wire")
		f.line("    // count/length header, before any allocation or accumulation -- never a")
		f.line("    // clamp. Schema-bounded fields are not governed by these caps; they keep")
		f.line("    // their own schema-capacity guard.")
		f.line("    //")
		f.line("    // Each number is compared exactly once, but not always here. A payload")
		f.line("    // length goes to PayloadAcc.string/blob and a wrapper row index to")
		f.line("    // Seq.reserveRow*, beside the schema bound they are exclusive with: the")
		f.line("    // corelib call this visitor already makes is where the length or index is")
		f.line("    // in hand before it is spent, so the check rides it instead of standing in")
		f.line("    // front of it. The numbers stay this file's -- they are passed per call and")
		f.line("    // the corelib retains, defaults and clamps to none of them.")
	}
	f.blank()
	f.line("    private companion object {")
	f.line("        private const val DEAD = -1     // the skipped-subtree scope: no arm below matches it")
	if limArr {
		f.line("        const val MAX_DYN_ARRAY_COUNT = %d", g.limits.arrayCount)
	}
	if limStr {
		f.line("        const val MAX_DYN_STRING_LEN = %d", g.limits.stringLen)
	}
	if limBlob {
		f.line("        const val MAX_DYN_BLOB_LEN = %d", g.limits.blobLen)
	}
	f.line("    }")
	f.blank()

	g.emitScalarCb(f, fs, "unsigned", "Long", func(fld *ir.Field) bool {
		switch fld.Kind {
		case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield, ir.KindBool:
			return true
		}
		return false
	})
	g.emitScalarCb(f, fs, "signed", "Long", func(fld *ir.Field) bool {
		switch fld.Kind {
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
			return true
		}
		return false
	})
	g.emitScalarCb(f, fs, "fp32", "Float", func(fld *ir.Field) bool { return fld.Kind == ir.KindFP32 })
	g.emitScalarCb(f, fs, "fp64", "Double", func(fld *ir.Field) bool { return fld.Kind == ir.KindFP64 })

	g.emitFixlenBegin(f, fs)
	g.emitStringCb(f, fs)
	g.emitBlobCb(f, fs)
	g.emitArrayBegin(f, fs, limArr, hasArray)
	g.emitBulkCbs(f, fs)
	g.emitSequenceCbs(f, fs)

	f.line("}")
	f.blank()
}

// ---------------------------------------------------------------------------
// Destination resolution
// ---------------------------------------------------------------------------

// destFrame is one frame that can materialize a value of the scanned kind:
// `ids` empty means every id lands there (a wrapper-sequence row).
type destFrame struct {
	idx int
	ids []int64
}

// kindDests collects the frames that can materialize `kind`, in emission order.
// An empty result means the message never materializes a value of that kind.
func kindDests(fs []frame, kind ir.Kind) []destFrame {
	var dests []destFrame
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind {
			dests = append(dests, destFrame{idx: fr.idx})
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var ids []int64
		for _, fld := range fr.fields {
			if fld.Kind == kind {
				ids = append(ids, fld.ID)
			}
		}
		if len(ids) > 0 {
			dests = append(dests, destFrame{idx: fr.idx, ids: ids})
		}
	}
	return dests
}

// emitDestGuard writes the skip gate at the very top of the string()/blob()
// callback (CORELIB_PLAN §6.4): "skipped fields are never validated". Skipping
// is a length jump over bytes that are not inspected, and UTF-8 validation runs
// only where a `string` is materialized -- read into a destination. So the
// destination is resolved FIRST: every (cur, id) that declares one, plus the
// wrapper-sequence rows whose element kind matches, falls through; anything else
// returns right here.
//
// Returning here is what makes the skip a true skip: an unknown id, or a §7.3
// wire-type contradiction routed down the same path, never reaches the
// converter and never enters the shared accumulator, so a later declared field
// cannot inherit its bytes.
//
// Placed ahead of the maxlen/limit guards, which are already
// destination-scoped and therefore unaffected -- §5.2's INVALID-over-INCOMPLETE
// ordering is preserved.
func (g *gen) emitDestGuard(f *kfile, dests []destFrame) {
	f.line("        // A payload this scope does not declare is skipped: its bytes are jumped")
	f.line("        // over, never inspected. Resolve the destination first and leave before a")
	f.line("        // byte is buffered, decoded or checked.")
	f.line("        when (cur) {")
	for _, d := range dests {
		if len(d.ids) == 0 {
			f.line("            %d -> {}", d.idx)
			continue
		}
		var labels []string
		for _, id := range d.ids {
			labels = append(labels, itoa64(id))
		}
		f.line("            %d -> when (id) { %s -> {}; else -> return }", d.idx, strings.Join(labels, ", "))
	}
	f.line("            else -> return")
	f.line("        }")
}

// ---------------------------------------------------------------------------
// Receiver-side decode limits (generator#102)
// ---------------------------------------------------------------------------

// activeLimits reports which receiver-cap CONSTANTS this visitor's emitted code
// names. A constant is emitted exactly when some emitted line reads it, which is
// no longer the same question as "can this cap ever fire".
//
// Two sources, and the second is what changed (CORELIB_PLAN §6.2.1):
//
//   - a guard this visitor still writes itself -- a native array's count header,
//     a wrapper element's index where the gap fill is generated code. Live only
//     where the schema left the field unbounded, as before;
//   - a bound this visitor now STATES to the corelib: every `acc.string` /
//     `acc.blob` call carries `max_dyn_string_len` / `max_dyn_blob_len`, every
//     `Seq.reserveRow*` and `Seq.reserveRowList` call carries
//     `max_dyn_array_count`. Those arguments are required and are always passed,
//     including where the schema bound handed in beside one makes it inert:
//     omitting it would leave the codec to read silence as "unlimited", which
//     §6.2.1 forbids, and there is nothing for it to fall back to.
//
// So a fully schema-bounded message can now emit a constant it never reads at
// run time. That is the price of stating the number rather than defaulting it,
// and it is exactly what the collector arguments in Go, Dart and TypeScript
// already do (§9.5).
func (g *gen) activeLimits(fs []frame) (limArr, limStr, limBlob bool) {
	// A string/blob destination means an acc.string/acc.blob call, and every such
	// call states its cap.
	limStr = len(kindDests(fs, ir.KindString)) > 0
	limBlob = len(kindDests(fs, ir.KindBlob)) > 0
	for _, fr := range fs {
		switch fr.kind {
		case fkNormal:
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) && !fld.HasCount {
					limArr = true // the count guard this visitor still writes
				}
			}
		case fkNativeMat, fkSeqMat:
			limArr = true // Seq.reserveRow* / Seq.reserveRowList state the cap
		}
		// A wrapper array the schema leaves uncounted bounds its element INDEX,
		// which is the array's length (§9.5, generator#387). Where the gap fill is
		// generated -- a string/blob leaf, a struct element -- that guard is still
		// this visitor's and names the constant.
		if fr.kind != fkNormal && fr.cap < 0 {
			limArr = true
		}
	}
	return limArr, limStr, limBlob
}

// emitMaxlenArg writes the `maxlen` local the string()/blob() callback hands to
// PayloadAcc.string / .blob -- the SCHEMA's number for the (cur, id) destination
// this delivery is for, and -1 where the schema declares none. Returns false
// when no destination of this kind is bounded, in which case the caller passes
// the literal -1 and no local is emitted at all.
//
// -1 is not "unlimited", and it is not a default the codec may invent: it is the
// schema FACT the accumulator needs to pick which of the two numbers applies
// (CORELIB_PLAN §6.2.1). A bounded field is governed by its `maxlen` and its
// violation is INVALID_MSG (MESSAGE_SPEC §7.1); an unbounded one is governed by
// the receiver cap passed beside it and its violation is the LIMIT_EXCEEDED
// policy category (§6.3). Exactly one is ever in play, so a receiver cap can
// never be applied to a schema-bounded field and an unbounded field can never be
// decoded uncapped.
//
// Resolving the number in the caller and comparing it in the accumulator is what
// keeps ONE implementation of the comparison (§6.2.1): the visitor knows the
// schema and nothing else, the accumulator holds the length and nothing else.
//
// This runs BEHIND emitDestGuard, so an id this scope does not declare -- an
// unknown one, or a §7.3 wire-type contradiction -- has already returned and
// never reaches the call. A skipped field is never capped.
func (g *gen) emitMaxlenArg(f *kfile, fs []frame, kind ir.Kind) bool {
	type arm struct {
		idx   int
		val   string   // fkSeqLeaf: the element bound, used directly
		inner []string // fkNormal: one `id -> bound` per bounded field
	}
	var arms []arm
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && fr.emax >= 0 {
			arms = append(arms, arm{idx: fr.idx, val: itoa64(ktCap(fr.emax))})
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var inner []string
		for _, fld := range fr.fields {
			if fld.Kind == kind && fld.HasMaxlen {
				inner = append(inner, fmt.Sprintf("%d -> %s", fld.ID, itoa64(ktCap(fld.Maxlen))))
			}
		}
		if len(inner) > 0 {
			arms = append(arms, arm{idx: fr.idx, inner: inner})
		}
	}
	if len(arms) == 0 {
		return false
	}
	f.line("        // The schema's own bound for this destination, or -1 where it declares")
	f.line("        // none. The accumulator takes it beside the receiver cap and rejects an")
	f.line("        // oversized `total` at the length header, before a byte is buffered: over")
	f.line("        // a declared maxlen is INVALID (S7.1), over the cap is LIMIT_EXCEEDED, and")
	f.line("        // the schema fact below is what decides which of the two can apply.")
	f.line("        val maxlen = when (cur) {")
	for _, a := range arms {
		if a.inner == nil {
			f.line("            %d -> %s", a.idx, a.val)
			continue
		}
		f.line("            %d -> when (id) { %s; else -> -1 }", a.idx, strings.Join(a.inner, "; "))
	}
	f.line("            else -> -1")
	f.line("        }")
	return true
}

// ---------------------------------------------------------------------------
// fixlenBegin: every bound the LENGTH WORD already decides
// ---------------------------------------------------------------------------

// emitFixlenBegin latches every schema bound a fixlen field's length word
// already decides, at that word (CORELIB_PLAN §5.2, generator#267).
//
// The bounds are not new -- a scalar/element `maxlen` and a wrapper element's
// `id >= count` are both rejected in the payload callback too -- but that
// callback only fires once payload bytes arrive. A message truncated immediately
// after the length word would therefore report INCOMPLETE, while the same bytes
// read whole are INVALID. §5.2 makes INVALID dominate INCOMPLETE precisely
// because the violation is already established by the bytes seen.
//
// Every guard sits inside the DECLARED-subtype test. The hook fires for whatever
// fixlen subtype arrived at a field id -- the corelib resolves what arrived but
// cannot know what was declared -- so a contradicting subtype is a §7.3 skip and
// must not be measured against this field's bound.
//
// The payload-side guards stay: unreachable for a message that gets this far,
// and the only thing still bounding a consumer built against an older corelib.
func (g *gen) emitFixlenBegin(f *kfile, fs []frame) {
	limStr, limBlob := kindDests(fs, ir.KindString), kindDests(fs, ir.KindBlob)
	str := g.fixlenBeginArms(fs, ir.KindString, "string length", capConstFor(len(limStr) > 0, "MAX_DYN_STRING_LEN"))
	blob := g.fixlenBeginArms(fs, ir.KindBlob, "blob length", capConstFor(len(limBlob) > 0, "MAX_DYN_BLOB_LEN"))
	if len(str) == 0 && len(blob) == 0 {
		return
	}
	f.line("    override fun fixlenBegin(id: Int, subtype: FixlenType, total: Int) {")
	f.line("        // Decided at the LENGTH WORD, not once payload bytes arrive: a message")
	f.line("        // that ends right after this word reaches no payload callback at all, and")
	f.line("        // both verdicts outrank the INCOMPLETE it would otherwise report -- a")
	f.line("        // schema maxlen because S5.2 makes INVALID dominate, a receiver cap")
	f.line("        // because S6.2.1 puts it \"at the count/length header, before the")
	f.line("        // allocation it is meant to prevent\" and S6.3 makes the refusal terminal.")
	f.line("        // The subtype test is S7.3 -- a contradicting fixlen kind at this id is a")
	f.line("        // SKIPPED field, not this field's length, and a skipped field is never")
	f.line("        // capped.")
	for _, a := range []struct {
		variant string
		arms    []string
	}{{"STRING", str}, {"BLOB", blob}} {
		if len(a.arms) == 0 {
			continue
		}
		f.line("        if (subtype == FixlenType.%s) {", a.variant)
		f.line("            when (cur) {")
		for _, arm := range a.arms {
			f.line("%s", arm)
		}
		f.line("                else -> {}")
		f.line("            }")
		f.line("        }")
	}
	f.line("    }")
	f.blank()
}

// capConstFor names the constant carrying a kind's receiver cap, or "" where
// this message has no destination of that kind and the constant is not declared
// -- naming it there would not compile.
func capConstFor(live bool, name string) string {
	if !live {
		return ""
	}
	return name
}

// capCheck renders the §6.2.1 receiver-cap comparison AT THE LENGTH WORD: the
// corelib's own check, reached on its own rather than through a payload chunk.
//
// It is the same routine PayloadAcc.string/.blob run at the top of themselves,
// so this is one implementation of the rule applied at two points, not two
// implementations of it. The -1 is the schema FACT this destination carries no
// `maxlen` -- the same number the payload call passes, and the reason the
// accumulator can tell the two rules apart; it is not "unlimited".
func capCheck(kind ir.Kind, constName string) string {
	if kind == ir.KindBlob {
		return fmt.Sprintf("PayloadAcc.checkBlobLength(total, -1, %s)", constName)
	}
	return fmt.Sprintf("PayloadAcc.checkStringLength(total, -1, %s)", constName)
}

// fixlenBeginArms builds the per-scope arms for one fixlen subtype: a wrapper
// element carries its array's over-index bound AND its element bound, a scalar
// field carries its own. Over-index FIRST -- an element that is not this array's
// element at all must not have its length measured against the element bound.
//
// Which bound a destination gets is §6.2.1's exclusivity rule, and it settles the
// CATEGORY with the number: a schema `maxlen` is a statement about validity, so
// its breach is INVALID_MSG and it is thrown here; a schema-unbounded field is
// governed by the receiver's configured cap, whose breach is LIMIT_EXCEEDED and
// which the corelib call compares. Never both on one field, and never one
// answering in the other's category.
func (g *gen) fixlenBeginArms(fs []frame, kind ir.Kind, noun, constName string) []string {
	var arms []string
	for _, fr := range fs {
		elemCapped := constName != "" && fr.emax < 0
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && (fr.cap >= 0 || fr.emax >= 0 || elemCapped) {
			body := g.overIndexGuard(fr.cap, fr.loc)
			switch {
			case fr.emax >= 0:
				body += fmt.Sprintf("if (total > %d) %s", fr.emax,
					invalidThrow(fmt.Sprintf("%s element: %s above schema maxlen %d", locName(fr.loc), noun, fr.emax)))
			case elemCapped:
				body += capCheck(kind, constName)
			}
			arms = append(arms, fmt.Sprintf("                %d -> { %s }", fr.idx, strings.TrimSuffix(body, "; ")))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var inner []string
		for _, fld := range fr.fields {
			if fld.Kind != kind {
				continue
			}
			switch {
			case fld.HasMaxlen:
				inner = append(inner, fmt.Sprintf("%d -> if (total > %d) %s", fld.ID, fld.Maxlen,
					invalidThrow(fmt.Sprintf("%s: %s above schema maxlen %d", fld.Name, noun, fld.Maxlen))))
			case constName != "":
				inner = append(inner, fmt.Sprintf("%d -> %s", fld.ID, capCheck(kind, constName)))
			}
		}
		if len(inner) > 0 {
			arm := fmt.Sprintf("                %d -> when (id) { ", fr.idx)
			arm += strings.Join(inner, "; ")
			arm += " }"
			arms = append(arms, arm)
		}
	}
	return arms
}

// ---------------------------------------------------------------------------
// string / blob
// ---------------------------------------------------------------------------

func (g *gen) emitStringCb(f *kfile, fs []frame) {
	f.line("    override fun string(id: Int, total: Int, offset: Int, data: ByteArray, chunkOffset: Int, chunkLength: Int) {")
	dests := kindDests(fs, ir.KindString)
	if len(dests) == 0 {
		// A message that declares no string at all still gets the callback (the
		// interface declares it, and the corelib still routes string fields at
		// unknown ids here), and every string reaching it is skipped by
		// definition -- so the body is EMPTY, not guarded. Decoding one only to
		// drop it would validate a payload nobody reads, which CORELIB_PLAN §6.4
		// forbids.
		f.line("        // No field of this message is a string, so every string payload the")
		f.line("        // decoder delivers is skipped whole -- its bytes are never inspected.")
		f.line("    }")
		f.blank()
		return
	}
	g.emitDestGuard(f, dests)
	bound := "-1"
	if g.emitMaxlenArg(f, fs, ir.KindString) {
		bound = "maxlen"
	}
	// The accumulator answers a payload delivered in one chunk straight out of
	// the input, buffers one delivered in several, and validates the UTF-8 once
	// the payload is complete -- so nothing is routed until there is a value. It
	// is also where BOTH length bounds are compared, against the two numbers
	// handed in here (CORELIB_PLAN §6.2.1): the check rides a call this callback
	// already makes, and lands at the length header, ahead of the buffering.
	f.line("        val s = acc.string(total, offset, data, chunkOffset, chunkLength, %s, MAX_DYN_STRING_LEN) ?: return", bound)
	f.line("        when (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindString {
			// Elements are keyed by index id (MESSAGE_SPEC §2/§5.1): a default
			// (empty) element is omitted on the wire, so the value is PLACED at
			// its id and any gap filled with the element default ("").
			f.line("            %d -> { %swhile (%s.size <= id) %s.add(\"\"); %s[id] = s }",
				fr.idx, g.overIndexGuard(fr.cap, fr.loc), fr.listExpr, fr.listExpr, fr.listExpr)
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindString {
				arms = append(arms, fmt.Sprintf("%d -> %s.%s = s", fld.ID, fr.path, ktIdent(fld.Name)))
			}
		}
		if len(arms) > 0 {
			g.frameWhen(f, "            ", fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")
	f.blank()
}

func (g *gen) emitBlobCb(f *kfile, fs []frame) {
	f.line("    override fun blob(id: Int, total: Int, offset: Int, data: ByteArray, chunkOffset: Int, chunkLength: Int) {")
	dests := kindDests(fs, ir.KindBlob)
	if len(dests) == 0 {
		f.line("        // No field of this message is a blob, so every blob payload the decoder")
		f.line("        // delivers is skipped whole.")
		f.line("    }")
		f.blank()
		return
	}
	g.emitDestGuard(f, dests)
	bound := "-1"
	if g.emitMaxlenArg(f, fs, ir.KindBlob) {
		bound = "maxlen"
	}
	f.line("        val b = acc.blob(total, offset, data, chunkOffset, chunkLength, %s, MAX_DYN_BLOB_LEN) ?: return", bound)
	f.line("        when (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindBlob {
			f.line("            %d -> { %swhile (%s.size <= id) %s.add(Seq.EMPTY_BYTES); %s[id] = b }",
				fr.idx, g.overIndexGuard(fr.cap, fr.loc), fr.listExpr, fr.listExpr, fr.listExpr)
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindBlob {
				arms = append(arms, fmt.Sprintf("%d -> %s.%s = b", fld.ID, fr.path, ktIdent(fld.Name)))
			}
		}
		if len(arms) > 0 {
			g.frameWhen(f, "            ", fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")
	f.blank()
}

// ---------------------------------------------------------------------------
// arrayBegin
// ---------------------------------------------------------------------------

// emitArrayBegin writes arrayBegin: one dispatch, not two.
//
// SKIPPING IS THE DEFAULT (MESSAGE_SPEC §7.3). `askip = count` up front is what
// an id this scope does not declare -- or declares with a different array kind
// -- falls through to: its elements are dropped one by one and a real scalar at
// that id still decodes afterwards. Every arm that runs disarms it.
//
// A primitive array reserves a small backing store (capped, NOT the wire count
// -- which is untrusted) and is grown and filled by index; a native-matrix row
// is placed at the index its element id names.
func (g *gen) emitArrayBegin(f *kfile, fs []frame, limArr, hasArray bool) {
	f.line("    override fun arrayBegin(id: Int, kind: ArrayKind, count: Int) {")
	if hasArray {
		f.line("        ai = 0")
	}
	f.line("        // An array delivered at an id that does not declare one of the SAME array")
	f.line("        // kind is a wire-type contradiction: drop exactly `count` elements and")
	f.line("        // leave the declared field untouched (S7.3). Every arm below that runs is")
	f.line("        // a declared array at a matching kind, and disarms this.")
	f.line("        askip = count")
	if hasArray {
		f.line("        afill = 0")
	}
	if hasBulk(fs) {
		f.line("        abulk = null      // no bulk destination unless an arm below offers one")
	}
	f.line("        when (cur) {")
	for i := range fs {
		fr := &fs[i]
		if fr.kind == fkNativeMat {
			// A native-matrix row is itself a native array, and its own element
			// count needs its own bound -- `cap` bounds the row's ID, not how many
			// elements the row claims. A row the schema counts is bounded by that
			// count (INVALID above it, §7.1); one the schema leaves unbounded is
			// governed by the configured cap (LIMIT_EXCEEDED). Either way the row
			// is bounded BEFORE it is sized, which is what lets the sizing be exact.
			guard := ""
			switch {
			case fr.innerHasCount:
				guard = fmt.Sprintf("if (count > %d) %s; ", fr.innerCap,
					invalidThrow(fmt.Sprintf("%s element: array count above schema capacity %d", locName(fr.loc), fr.innerCap)))
			case limArr:
				guard = fmt.Sprintf("if (count > MAX_DYN_ARRAY_COUNT) %s; ",
					limitThrow(locName(fr.loc), "array count above configured limit", g.limits.arrayCount))
			}
			if guard == "" {
				panic("kotlin: unbounded matrix row with no cap -- every target has a finite default (§9.5)")
			}
			// A row whose header carries a different array kind than the inner
			// element declares is skipped whole (§7.3). Checked FIRST, so no bound
			// can reject a row that was never this array's row at all.
			//
			// The row's INDEX is bounded by Seq.reserveRow* itself, which takes the
			// outer array's schema `count` and the receiver cap and rejects the id
			// before it grows the outer list (CORELIB_PLAN §6.2.1) -- the same call
			// that reserves the row, so nothing new is called to carry the check.
			// Its element COUNT stays here: the count header arrives in this
			// callback, and the reservation is sized from it, so there is no earlier
			// call to hang it on.
			body := guard + armFill(fs, fr, nil)
			arrType := primArrayType(fr.innerElem)
			// Sized at exactly the wire count, once: the guard above bounded it
			// (§9.5, shape A). The wire already said how big the row is.
			f.line("            %d -> if (kind == ArrayKind.%s) { %s%s = Seq.reserveRow%s(%s, id, count, %s, MAX_DYN_ARRAY_COUNT); %s = id }",
				fr.idx, arrayWireKind(fr.innerElem), body, rowCursor(arrType), seqSuffix(arrType), fr.listExpr, itoa64(ktCap(fr.cap)), elemIdxVar(fr.loc))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind != ir.KindArray || !nativeArrayElem(fld.Elem) {
				continue
			}
			// §7.3 comes FIRST: a header whose array kind is not the one this
			// field's declared element type maps to must be skipped exactly like
			// an unknown id -- the declared field must not be touched at all,
			// which includes not being RESIZED from the skipped header's count.
			// Ordering matters as much as the test: the schema bound below
			// applies only to a field that survives this check, so an over-count
			// MIS-TYPED array is skipped, not a false INVALID.
			guard := ""
			if fld.HasCount {
				// A wire element count above the schema `count` capacity is
				// INVALID per MESSAGE_SPEC §3+§7 -- reject up front, never clamp
				// or keep-all.
				guard = fmt.Sprintf("if (count > %d) %s; ", fld.Count,
					invalidThrow(fmt.Sprintf("%s: array count above schema capacity %d", fld.Name, fld.Count)))
			} else if limArr {
				// An UNBOUNDED array is instead governed by the configured
				// max_dyn_array_count when set: exceeding it is LIMIT_EXCEEDED, a
				// receiver policy error kept distinct from wire malformation.
				guard = fmt.Sprintf("if (count > MAX_DYN_ARRAY_COUNT) %s; ",
					limitThrow(fld.Name, "array count above configured limit", g.limits.arrayCount))
			}
			// The wire count M IS the array's length (MESSAGE_SPEC §3): the M
			// elements that arrived are the whole value, so the container is
			// grown as they come and ends exactly M long. A declared `count: N`
			// is a CAPACITY and bounds M (the guard above); it never adds
			// elements, so there is nothing to materialize at [M, N).
			target := fr.path + "." + ktIdent(fld.Name)
			body := guard + armFill(fs, fr, fld)
			arrType := primArrayType(fld.Elem)
			// Allocated at exactly the wire count, once (ARCHITECTURE §9.5, shape
			// A): the guard above has already bounded that count against the
			// schema capacity or the configured cap. Growing into it from a capped
			// reservation -- the #96/#98 shape, written the day before the caps of
			// #102 existed -- would only add doubling and copies.
			if guard == "" {
				panic("kotlin: native array with neither a schema count nor a cap -- every target has a finite default (§9.5)")
			}
			body += fmt.Sprintf("%s = %s(count)", target, arrType)
			if bulkCapable(fld) {
				// Its element WIDTH is what tells the decoder the declared width, so
				// the §7.1 check and the narrowing happen in the pass that decodes.
				// The per-element arms stay the fallback for a decoder that declines.
				body += fmt.Sprintf("; abulk = %s", bulkView(fld.Elem, target))
			}
			arms = append(arms, fmt.Sprintf("%d -> if (kind == ArrayKind.%s) { %s }", fld.ID, arrayWireKind(fld.Elem), body))
		}
		if len(arms) > 0 {
			g.frameWhen(f, "            ", fr.idx, arms)
		}
	}
	f.line("            else -> {}")
	f.line("        }")
	f.line("    }")
	f.blank()
}

// bulkView is the signed array the corelib's bulk offer is handed for a
// destination of element kind k. Kotlin's unsigned arrays are inline classes
// over their signed peers, so `asByteArray()` and friends are the SAME backing
// array under another type -- the decoder fills the field itself, with no copy
// and no second pass.
func bulkView(k ir.Kind, target string) string {
	switch k {
	case ir.KindU8:
		return target + ".asByteArray()"
	case ir.KindU16:
		return target + ".asShortArray()"
	case ir.KindU32:
		return target + ".asIntArray()"
	case ir.KindU64, ir.KindBitfield:
		return target + ".asLongArray()"
	}
	return target
}

// hasBulk reports whether the message has any array the corelib's bulk offer can
// be taken for.
//
// Exactly the integer arrays: the offer needs a destination sized to `count` up
// front, which every native array now has -- the count is checked against the
// schema bound or the cap before it is allocated from, so the untrusted-count
// objection that once restricted this to SCHEMA-BOUNDED arrays is answered by the
// check rather than by the reservation (ARCHITECTURE §9.5, shape A). That leaves
// out boolean arrays (no integer element), fp arrays (the offer is integer-only)
// and matrix rows (whose destination is a row cursor, not a field).
func hasBulk(fs []frame) bool {
	for i := range fs {
		fr := &fs[i]
		if fr.kind != fkNormal {
			continue
		}
		for _, fld := range fr.fields {
			if bulkCapable(fld) {
				return true
			}
		}
	}
	return false
}

// bulkCapable reports whether a field is one of those arrays.
func bulkCapable(fld *ir.Field) bool {
	if fld.Kind != ir.KindArray {
		return false
	}
	switch fld.Elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// emitBulkCbs writes the two halves of the corelib's bulk-array offer
// (Visitor.arrayBulk / arrayBulkEnd), the fast path for an integer array whose
// length the schema already bounds.
//
// arrayBegin has resolved and sized the destination, so the offer is a field
// read -- not a third walk over (scope, id) -- and the decoder then writes the
// elements straight into the field's own array. The array's WIDTH is what tells
// the decoder the declared width: handing back a `ByteArray` says "u8/i8
// elements", and a value that does not fit is INVALID (§7.1) rather than
// truncated, checked in the same pass that decodes. So all arrayBulkEnd has left
// to do is clear the fill counter, which no element callback was there to count
// down.
func (g *gen) emitBulkCbs(f *kfile, fs []frame) {
	if !hasBulk(fs) {
		return
	}
	f.line("    override fun arrayBulk(id: Int, kind: ArrayKind, count: Int): Any? {")
	f.line("        // Offered iff arrayBegin sized a schema-bounded destination just now. Its")
	f.line("        // element width IS the declared width, so the decoder checks and narrows")
	f.line("        // in the pass that decodes.")
	f.line("        return abulk")
	f.line("    }")
	f.blank()
	f.line("    override fun arrayBulkEnd(id: Int, n: Int) {")
	f.line("        afill = 0   // the elements never went through the element callbacks")
	f.line("        abulk = null")
	f.line("    }")
	f.blank()
}

// ---------------------------------------------------------------------------
// sequenceBegin / sequenceEnd
// ---------------------------------------------------------------------------

func (g *gen) emitSequenceCbs(f *kfile, fs []frame) {
	f.line("    override fun sequenceBegin(id: Int) {")
	f.line("        if (sp == stk.size) stk = stk.copyOf(sp * 2)")
	f.line("        stk[sp++] = cur")
	f.line("        when (cur) {")
	for _, fr := range fs {
		switch fr.kind {
		case fkSeqObj:
			// MESSAGE_SPEC §5.1: the element id IS the array index, exactly as on
			// the string/blob leaf-element paths, so the element is PLACED at
			// list[id] after gap-filling with default elements -- never appended.
			// Appending shortened the array by the size of any interior id gap
			// and decoded a REOPENED id as a second element instead of merging
			// into the first (§7.4 struct-merge, which placement gives for free).
			f.line("            %d -> { %swhile (%s.size <= id) %s.add(%s()); %s = id; cur = %d }",
				fr.idx, g.overIndexGuard(fr.cap, fr.loc), fr.listExpr, fr.listExpr, fr.elemType, elemIdxVar(fr.loc), locIndex(fs, fr.childLoc))
		case fkSeqMat:
			// A row of an array-of-wrapper-arrays is placed at the index its
			// element id names, for the same reason as the struct element above.
			// An array wrapper IS the array's value, so a REOPENED row id
			// replaces the row rather than merging into it (§7.4).
			//
			// The index bound rides the reservation, as it does for a native
			// matrix row: reserveRowList takes the schema `count` and the receiver
			// cap and rejects the id before it grows the outer list (§6.2.1).
			f.line("            %d -> { Seq.reserveRowList(%s, id, %s, MAX_DYN_ARRAY_COUNT); %s = id; cur = %d }",
				fr.idx, fr.listExpr, itoa64(ktCap(fr.cap)), elemIdxVar(fr.loc), locIndex(fs, fr.childLoc))
		case fkNormal:
			var arms []string
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
					arms = append(arms, fmt.Sprintf("%d -> cur = %d", fld.ID, locIndex(fs, fr.loc+"_"+fld.Name)))
				case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
					// §7.4: an array wrapper IS the array's value, so a later
					// occurrence REPLACES it whole. The clear sits inside this
					// callback, which the corelib invokes only for an actual
					// sequence header -- so the wire-type dispatch shields it and
					// a §7.3-skipped later occurrence cannot wipe a valid earlier
					// array.
					arms = append(arms, fmt.Sprintf("%d -> { %s.%s.clear(); cur = %d }",
						fld.ID, fr.path, ktIdent(fld.Name), locIndex(fs, fr.loc+"_"+fld.Name)))
				}
			}
			// A skipping default even when this scope declares no sequence at
			// all: reaching sequenceBegin here means an id that is not one of
			// them, and its CHILDREN must go with it (the dead scope).
			if len(arms) == 0 {
				f.line("            %d -> cur = DEAD", fr.idx)
			} else {
				arms = append(arms, "else -> cur = DEAD")
				g.frameWhen(f, "            ", fr.idx, arms)
			}
		}
	}
	// And the same for a scope with no arm above -- a leaf array scope, say --
	// where the when would otherwise fall through and leave `cur` on the
	// enclosing frame.
	f.line("            else -> cur = DEAD")
	f.line("        }")
	f.line("    }")
	f.blank()
	// A wrapper array's decoded length is *highest present id + 1* (MESSAGE_SPEC
	// §5.1) -- the elements that arrived are the whole value. A declared
	// `count: N` is a CAPACITY (§3): it bounds the element ids but never adds
	// elements the wire did not carry, so there is nothing to fill in when the
	// scope closes.
	f.line("    override fun sequenceEnd() {")
	f.line("        cur = if (sp > 0) stk[--sp] else 0")
	f.line("    }")
}

// ---------------------------------------------------------------------------
// Scalar callbacks and the armed native-array fill
// ---------------------------------------------------------------------------

// emitScalarCb writes a callback that routes (cur, id) to a field assignment.
//
// An ARRAY ELEMENT does not go through that routing at all. Its destination was
// already resolved by the arrayBegin that armed the fill, so the element arms
// hang off `atgt` -- one dense when -- ahead of the scalar routing, which the
// array ids then leave entirely.
func (g *gen) emitScalarCb(f *kfile, fs []frame, cb, vtype string, want func(*ir.Field) bool) {
	f.line("    override fun %s(id: Int, value: %s) {", cb, vtype)
	g.emitArrayFillArm(f, fs, cb)
	// The §7.3 discard guard heads every callback an array shares with a lone
	// scalar: unsigned/signed for integer arrays, fp32/fp64 for fp arrays.
	f.line("        // Drop an element of an array whose id does not declare one -- armed by")
	f.line("        // arrayBegin, self-terminating on count.")
	f.line("        if (askip > 0) { askip--; return }")
	f.line("        when (cur) {")
	for _, fr := range fs {
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray || !want(fld) {
				continue
			}
			target := fr.path + "." + ktIdent(fld.Name)
			guard := ""
			if cb == "unsigned" || cb == "signed" {
				guard = widthThrow(fld.Kind, fld.Name)
			}
			rhs := "value"
			if cb == "unsigned" || cb == "signed" {
				rhs = fromWire(fld.Kind, "value")
			}
			arms = append(arms, fmt.Sprintf("%d -> { %s%s = %s }", fld.ID, guard, target, rhs))
		}
		if len(arms) > 0 {
			g.frameWhen(f, "            ", fr.idx, arms)
		}
	}
	f.line("            else -> {}")
	f.line("        }")
	f.line("    }")
	f.blank()
}

// fillTargetsFor lists, in frame order, every destination an armed native-array
// fill can write into whose elements arrive through callback cb, numbering them
// densely from 1. arrayBegin parks a target's number in `atgt`; the callback
// switches on it.
//
// The numbering is PER CALLBACK, and may repeat across callbacks, because only
// one native-array fill is ever open at a time and its element kind decides
// which callback delivers it.
func fillTargetsFor(fs []frame, cb string) map[*frame]map[int64]int {
	out := map[*frame]map[int64]int{}
	n := 0
	for i := range fs {
		fr := &fs[i]
		switch fr.kind {
		case fkNativeMat:
			if nativeElemCb(fr.innerElem) == cb {
				n++
				out[fr] = map[int64]int{-1: n}
			}
		case fkNormal:
			for _, fld := range fr.fields {
				if fld.Kind != ir.KindArray || !nativeArrayElem(fld.Elem) || nativeElemCb(fld.Elem) != cb {
					continue
				}
				n++
				if out[fr] == nil {
					out[fr] = map[int64]int{}
				}
				out[fr][fld.ID] = n
			}
		}
	}
	return out
}

// hasFill reports whether the message declares any native array at all, i.e.
// whether an armed fill can ever be open.
func hasFill(fs []frame) bool {
	for _, cb := range []string{"unsigned", "signed", "fp32", "fp64"} {
		if len(fillTargetsFor(fs, cb)) > 0 {
			return true
		}
	}
	return false
}

// armFill is the arrayBegin statement that hands a declared native array over to
// the element callbacks: disarm the §7.3 skip, arm the fill for exactly the
// announced count, and park which destination the elements belong to.
func armFill(fs []frame, fr *frame, fld *ir.Field) string {
	cb, key := "", int64(-1)
	if fld == nil {
		cb = nativeElemCb(fr.innerElem)
	} else {
		cb, key = nativeElemCb(fld.Elem), fld.ID
	}
	return fmt.Sprintf("askip = 0; afill = count; atgt = %d; ", fillTargetsFor(fs, cb)[fr][key])
}

// emitArrayFillArm writes the armed-fill prologue of an element callback: while
// arrayBegin has a native array armed, every value is an element of THAT array,
// so it is stored against the target arrayBegin parked rather than routed by
// (scope, id) again.
//
// The width guard sits inside the arm, never in front of it: an over-width
// scalar arriving at an array id with no arrayBegin in front of it is a §7.3
// skip, not an INVALID, and it never reaches this arm because afill is zero.
func (g *gen) emitArrayFillArm(f *kfile, fs []frame, cb string) {
	tgts := fillTargetsFor(fs, cb)
	if len(tgts) == 0 {
		return
	}
	type arm struct {
		code int
		stmt string
	}
	var arms []arm
	for i := range fs {
		fr := &fs[i]
		ids, ok := tgts[fr]
		if !ok {
			continue
		}
		if fr.kind == fkNativeMat {
			// Fill the row through the cursor arrayBegin parked. The row was sized
			// at exactly the announced count, so there is no growth and no
			// reference to write back into the list (§9.5, shape A).
			cur := rowCursor(primArrayType(fr.innerElem))
			guard := ""
			if cb == "unsigned" || cb == "signed" {
				guard = widthThrow(fr.innerElem, locName(fr.loc)+" element")
			}
			arms = append(arms, arm{ids[-1], fmt.Sprintf("%s%s[ai] = %s; ai++",
				guard, cur, elemStore(fr.innerElem, cb))})
			continue
		}
		for _, fld := range fr.fields {
			code, ok := ids[fld.ID]
			if !ok {
				continue
			}
			target := fr.path + "." + ktIdent(fld.Name)
			guard := ""
			if cb == "unsigned" || cb == "signed" {
				guard = widthThrow(fld.Elem, fld.Name+" element")
			}
			// A plain indexed store. arrayBegin allocated the destination at
			// exactly the announced count, having first bounded that count against
			// the schema capacity or the configured cap (§9.5, shape A), so nothing
			// here can run past the end and nothing has to grow.
			arms = append(arms, arm{code, fmt.Sprintf("%s%s[ai] = %s; ai++",
				guard, target, elemStore(fld.Elem, cb))})
		}
	}
	f.line("        // An element of the array arrayBegin armed: its destination is already")
	f.line("        // resolved, so it is stored against that target rather than routed by")
	f.line("        // (scope, id) again. Self-terminating on the announced count.")
	f.line("        if (afill != 0) {")
	f.line("            afill--")
	f.line("            when (atgt) {")
	for _, a := range arms {
		f.line("                %d -> { %s }", a.code, a.stmt)
	}
	f.line("            }")
	f.line("            return")
	f.line("        }")
}

// elemStore narrows a delivered element into the array's element type. `bool` is
// the one kind whose storage is not an integer of the declared width.
func elemStore(k ir.Kind, cb string) string {
	if cb == "fp32" || cb == "fp64" {
		return "value"
	}
	if k == ir.KindBool {
		return "value != 0L"
	}
	return fromWire(k, "value")
}

// frameWhen emits `<idx> -> when (id) { <arms> }`.
func (g *gen) frameWhen(f *kfile, ind string, idx int, arms []string) {
	f.line("%s%d -> when (id) {", ind, idx)
	for _, a := range arms {
		f.line("%s    %s", ind, a)
	}
	f.line("%s}", ind)
}

// primArrayTypesUsed returns the distinct Kotlin primitive-array types of the
// native array fields across all frames, in a stable order.
func primArrayTypesUsed(fs []frame) []string {
	seen := map[string]bool{}
	var out []string
	for _, order := range primBaseOrder {
		for _, fr := range fs {
			if fr.kind == fkNativeMat && primArrayType(fr.innerElem) == order && !seen[order] {
				seen[order] = true
				out = append(out, order)
				continue
			}
			if fr.kind != fkNormal {
				continue
			}
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) && primArrayType(fld.Elem) == order && !seen[order] {
					seen[order] = true
					out = append(out, order)
				}
			}
		}
	}
	return out
}

// primRowTypesUsed is primArrayTypesUsed restricted to native-matrix ROWS: the
// types needing an `_arow<T>` cursor field.
func primRowTypesUsed(fs []frame) []string {
	seen := map[string]bool{}
	var out []string
	for _, order := range primBaseOrder {
		for _, fr := range fs {
			if fr.kind == fkNativeMat && primArrayType(fr.innerElem) == order && !seen[order] {
				seen[order] = true
				out = append(out, order)
			}
		}
	}
	return out
}
