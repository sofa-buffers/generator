package java

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// frameKind classifies a decode location in the flat-visitor state machine.
type frameKind int

const (
	fkNormal    frameKind = iota // object location: scalar/composite field routing
	fkSeqLeaf                    // string/blob array: elements via string/blob cb
	fkSeqObj                     // struct/union array: sequenceBegin adds an element
	fkNativeMat                  // nested array, native inner: arrayBegin/arrayXxx per row
	fkSeqMat                     // nested array, sequence inner: sequenceBegin adds a row
)

type frame struct {
	idx    int
	kind   frameKind
	loc    string
	path   string      // fkNormal: object path
	fields []*ir.Field // fkNormal
	// array (fkSeqLeaf/fkSeqObj/fkNativeMat/fkSeqMat):
	listExpr  string      // the List<...> accessor this frame collects into
	elemKind  ir.Kind     // fkSeqLeaf: KindString / KindBlob
	childLoc  string      // fkSeqObj: element loc; fkSeqMat: inner-row loc
	elemType  string      // fkSeqObj: java class for `new X()`
	innerElem ir.Kind     // fkNativeMat: inner element kind
	innerRef  *ir.TypeRef // fkNativeMat: inner element ref (unused; kept for symmetry)
	// schema bounds, for the receiver-side decode limits (generator#102):
	elemMaxHas    bool // fkSeqLeaf: the string/blob element declares a maxlen
	innerHasCount bool // fkNativeMat: the inner array declares a count
	// innerCap is the inner array's own schema count N (-1 == none) -- the bound on
	// a ROW's element count, which `cap` above does not give: cap bounds the row's
	// ID against the outer array's capacity. Both are needed, and for different
	// reasons: the id bound stops an over-index gap-fill, this one stops a row
	// header claiming more elements than the schema allows it (§7.1).
	innerCap int64
	// cap is the wrapper array's schema count bound N (-1 == no count). N is a
	// CAPACITY, not a length (MESSAGE_SPEC §3): it never reaches the wire and never
	// adds elements the wire did not carry. All it does here is bound the array --
	// an element id >= N is a schema-bound violation (§5.1/§7 — issue #142),
	// rejected as INVALID before the List grows, which also bounds the id-keyed gap
	// fill against an over-index heap-amplification DoS. Set on every array frame.
	cap int64
	// emax is the fkSeqLeaf string/blob element's schema maxlen L (-1 == no
	// bound): an element whose wire byte length exceeds L is malformed input,
	// rejected as INVALID (MESSAGE_SPEC §7.1) before any bytes accumulate, never
	// truncated. Set on fkSeqLeaf.
	emax int64
}

// capOf maps a schema fixed-count bound to a frame's cap: N when the array
// declares a count, -1 (dynamic/unbounded) otherwise.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// boundOf maps a schema string/blob maxlen to a frame's emax: L when the
// element declares a maxlen, -1 (unbounded) otherwise.
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
				walk(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, fld.ElemMaxHas, fld.ElemMax, capOf(fld.HasCount, fld.Count))
			}
		}
	}
	// addArray registers the frame(s) entered inside the wrapper sequence of a
	// sequence-typed array (string/blob/struct/union/nested). listExpr is the List
	// accessor the frame collects into; `row` reaches the element the current
	// element id names (never the last-appended one -- see the placement notes on
	// sequenceBegin/arrayBegin); cap is the array's schema count bound (-1 == none).
	addArray = func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax, cap int64) {
		row := listExpr + ".get(" + elemIdxVar(loc) + ")"
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{kind: fkSeqLeaf, loc: loc, listExpr: listExpr, elemKind: elem, elemMaxHas: elemMaxHas, cap: cap, emax: boundOf(elemMaxHas, elemMax)})
		case ir.KindStruct, ir.KindUnion:
			elemLoc := loc + "_e"
			out = append(out, frame{kind: fkSeqObj, loc: loc, listExpr: listExpr, childLoc: elemLoc, elemType: g.typeName(ref.Key), cap: cap})
			// The element id IS the array index (MESSAGE_SPEC §5.1), so the element
			// a child field writes into is the one sequenceBegin PLACED at that
			// index -- NOT the last one appended. Java's flat visitor has no
			// per-element child visitor to carry the position (a nested-visitor
			// backend just returns a pointer to dest[id]), so the index is parked in
			// a visitor field and the child accessor path reads it back.
			walk(elemLoc, listExpr+".get("+elemIdxVar(loc)+")", ref.Target.Fields)
		case ir.KindArray:
			// A row of a matrix / an array-of-wrapper-arrays is placed at the index
			// its element id names, exactly like every other element kind, so the
			// row accessor reads that index back out of a visitor field rather than
			// reaching for the last-appended row. Appending would shift every later
			// row down by one across an interior id gap -- which an omitted
			// all-default row now makes reachable (§2).
			if nativeArrayElem(items.Elem) {
				out = append(out, frame{kind: fkNativeMat, loc: loc, listExpr: listExpr, innerElem: items.Elem, innerRef: items.ElemRef, innerHasCount: items.HasCount, innerCap: capOf(items.HasCount, items.Count), cap: cap})
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

// emitSequenceEnd writes sequenceEnd(): pop the location stack, and nothing else.
//
// A wrapper array's decoded length is *highest present id + 1* (MESSAGE_SPEC
// §5.1) -- the elements that arrived are the whole value. A declared `count: N` is
// a CAPACITY (§3): it bounds the element ids (see overIndexGuard) but never adds
// elements the wire did not carry, so there is nothing to fill in when the scope
// closes.
func (g *gen) emitSequenceEnd(f *jfile) {
	f.line("    public void sequenceEnd() { cur = sp > 0 ? stk[--sp] : 0; }")
}

// elemIdxVar is the visitor field holding the array index of the wrapper-array
// element currently being decoded at loc -- the struct/union element of a
// fkSeqObj, or the row of a fkSeqMat / fkNativeMat. Java's flat visitor has no
// per-element child visitor to carry the position (a nested-visitor backend just
// returns a pointer to dest[id]), so the index is parked in a visitor field and
// the child accessor path reads it back. Non-identifier characters in loc are
// folded to '_' so the name is always a legal Java identifier.
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

// overIndexGuard returns the reject clause for a fixed-count wrapper array: an
// element id >= N throws INVALID_MSG (aborting decode) before the List grows
// (MESSAGE_SPEC §5.1/§7 — issue #142), which also bounds an over-index
// heap-amplification fill. Empty for a dynamic array (cap == -1).
func overIndexGuard(cap int64, name string) string {
	if cap < 0 {
		return ""
	}
	return fmt.Sprintf("if (id >= %d) throw Sofab.invalid(\"%s element: array index above schema capacity %d\"); ", cap, name, cap)
}

// locIndex maps a loc name to its index (for sequenceBegin targets).
func locIndex(fs []frame, loc string) int {
	for _, fr := range fs {
		if fr.loc == loc {
			return fr.idx
		}
	}
	return 0
}

// activeLimits reports which receiver-side decode limits (generator#102) apply
// to this visitor: the limit must be configured AND the message must reach at
// least one schema-unbounded field the visitor can guard — an unbounded native
// array (count header via arrayBegin) or an unbounded string/blob (length via
// the `total` parameter). Otherwise no constant and no guard is emitted, so an
// unset or inert key leaves the output byte-identical.
func (g *gen) activeLimits(fs []frame) (limArr, limStr, limBlob bool) {
	for _, fr := range fs {
		switch fr.kind {
		case fkNormal:
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) && !fld.HasCount:
					limArr = true
				case fld.Kind == ir.KindString && !fld.HasMaxlen:
					limStr = true
				case fld.Kind == ir.KindBlob && !fld.HasMaxlen:
					limBlob = true
				}
			}
		case fkSeqLeaf:
			if !fr.elemMaxHas {
				if fr.elemKind == ir.KindString {
					limStr = true
				} else {
					limBlob = true
				}
			}
		case fkNativeMat:
			if !fr.innerHasCount {
				limArr = true
			}
		}
	}
	return limArr && g.limits.arrayHas, limStr && g.limits.stringHas, limBlob && g.limits.blobHas
}

// limitThrow renders the generator#102 rejection: the same unchecked wrapper the
// schema guards reach through Sofab.invalid (a Visitor callback cannot throw the
// checked SofabException), but spelled out here because its category is
// LIMIT_EXCEEDED — a receiver policy error, kept distinct from wire malformation,
// and deliberately not covered by Sofab.invalid.
func limitThrow(name, noun string, limit int64) string {
	return fmt.Sprintf("throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, \"%s: %s %d\"));",
		name, noun, limit)
}

// limitThrowGuard is limitThrow behind a condition, for arms that also do work
// when the guard passes.
func limitThrowGuard(cond, name, noun string, limit int64) string {
	return fmt.Sprintf("if (%s) %s", cond, limitThrow(name, noun, limit))
}

// locName is the human-readable field path of a frame loc for error details:
// the loc minus its "Root_" prefix (element hops keep their "_e" suffix).
func locName(loc string) string {
	if len(loc) > 5 && loc[:5] == "Root_" {
		return loc[5:]
	}
	return loc
}

// emitLenLimitGuard writes the receiver-side length guard (generator#102) at
// the very top of the string()/blob() callback: when the wire `total` exceeds
// the configured cap AND the (cur, id) destination is a schema-unbounded field
// of this kind, decoding fails with LIMIT_EXCEEDED before any byte is
// accumulated — the guard runs ahead of both the single-shot and the chunked
// path, so an oversized split payload is rejected on its first chunk.
// Schema-bounded fields fall through unaffected (governed by their own maxlen).
func (g *gen) emitLenLimitGuard(f *jfile, fs []frame, kind ir.Kind, constName, noun string, limit int64) {
	f.line("        if (total > %s) {", constName)
	f.line("            switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && !fr.elemMaxHas {
			f.line("            case %d: %s", fr.idx, limitThrow(locName(fr.loc), noun+" above configured limit", limit))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == kind && !fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("case %d: %s", fld.ID, limitThrow(fld.Name, noun+" above configured limit", limit)))
			}
		}
		if len(arms) > 0 {
			f.line("            case %d: switch (id) {", fr.idx)
			for _, a := range arms {
				f.line("                %s", a)
			}
			f.line("            } break;")
		}
	}
	f.line("            }")
	f.line("        }")
}

// maxlenThrow renders the schema-maxlen rejection (MESSAGE_SPEC §7.1): a bounded
// string/blob whose wire byte length exceeds its declared maxlen is malformed
// input, so it fails the decode with INVALID_MSG — Sofab.invalid, the corelib's
// unchecked channel for exactly this (a Visitor callback cannot throw the checked
// SofabException), kept distinct from the generator#102 LIMIT_EXCEEDED
// receiver-policy cap on schema-unbounded fields.
func maxlenThrow(name, noun string, max int64) string {
	return fmt.Sprintf("throw Sofab.invalid(\"%s: %s above schema maxlen %d\");", name, noun, max)
}

// widthThrow renders the declared-width rejection (MESSAGE_SPEC §7.1,
// documentation#32): a `u8`/`u16`/`u32`/`i8`/`i16`/`i32` destination receiving a
// value outside its declared range is malformed input, rejected through the same
// unchecked INVALID_MSG channel as maxlenThrow — never masked to the width, never
// kept. Returns "" for the 64-bit kinds, whose range IS the accumulator the value
// arrives in.
//
// The `value < 0` term is not redundant on the unsigned side. The corelib
// delivers an unsigned wire value as a Java `long`, which has no unsigned type: a
// u64 at or above 2^63 arrives with its sign bit set, so `value > 255` alone
// would read it as negative and let precisely the largest values through the
// guard. Treating negative as out-of-range is correct for every narrow kind,
// since all of their maxima are below 2^63.
func widthThrow(k ir.Kind, name string) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	// Spelled as the pair of comparisons the declared width IS, not as the
	// equivalent one-operation forms ((value & ~255L) != 0 for an unsigned width,
	// (byte) value != value for a signed one). Those were tried: worth 42 Ir on
	// the arena's fifty elements and −40 on vehicle_telemetry, i.e. nothing, and
	// generated code is read by people who have the schema and nothing else.
	cond := fmt.Sprintf("value < 0 || value > %dL", hi)
	if lo < 0 {
		cond = fmt.Sprintf("value < %dL || value > %dL", lo, hi)
	}
	return fmt.Sprintf("if (%s) throw Sofab.invalid(\"%s: value outside declared width %s\"); ", cond, name, k)
}

// emitStringCb writes the string() visitor callback: the destination gate, the
// schema and receiver bounds, then the corelib accumulator that reassembles a
// split payload and validates it.
//
// A message that declares no string at all still gets the callback — Visitor
// declares it, and the corelib still delivers string fields to a message that
// has none — but its body is EMPTY. Every string reaching it is by definition
// skipped, and an empty body is what skipping means: decoding one only to drop
// it would validate a payload nobody reads, which is what CORELIB_PLAN §6.4
// forbids (generator#257). Java rejects unreachable statements, so this is a
// separate shape rather than a guard placed in front of dead code.
func (g *gen) emitStringCb(f *jfile, fs []frame, limStr bool) {
	f.line("    public void string(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	defer f.line("    }")

	dests := kindDests(fs, ir.KindString)
	if len(dests) == 0 {
		f.line("        // No field of this message is a string, so every string payload the")
		f.line("        // decoder delivers is skipped whole -- its bytes are never inspected.")
		return
	}
	g.emitDestGuard(f, fs, dests)
	if limStr {
		g.emitLenLimitGuard(f, fs, ir.KindString, "MAX_DYN_STRING_LEN", "string length", g.limits.stringLen)
	}
	g.emitMaxlenGuard(f, fs, ir.KindString, "string length")
	// The accumulator answers a whole-in-one-chunk payload straight out of the
	// input array and buffers only a split one, and validates UTF-8 once the
	// payload is complete; null means more chunks are still to come.
	f.line("        String _s = acc.string(total, offset, data, chunkOffset, chunkLength);")
	f.line("        if (_s == null) return;")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindString {
			// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
			// element is omitted on the wire, so place the value at its id and fill
			// any gap with the element default ("").
			f.line("        case %d: %swhile (%s.size() <= id) %s.add(\"\"); %s.set(id, _s); break;", fr.idx, overIndexGuard(fr.cap, fr.loc), fr.listExpr, fr.listExpr, fr.listExpr)
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindString {
				arms = append(arms, jcase(fld.ID, fr.path+"."+javaIdent(fld.Name)+" = _s"))
			}
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
}

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

// emitDestGuard writes the skip gate at the very top of the string() callback
// (CORELIB_PLAN §6.4, generator#257): "skipped fields are never validated".
// Skipping is a length jump over bytes that are not inspected (§5.2), and UTF-8
// validation runs only where a `string` is materialized — read into a
// destination. So the destination is resolved FIRST: every (cur, id) that
// declares a string, plus the wrapper-sequence rows whose element kind is
// string, falls through; anything else returns right here.
//
// Returning here is what makes the skip a true skip: an unknown id, or a §7.3
// wire-type contradiction routed down the same path, is never validated and never
// enters the shared accumulator (so a later declared field cannot inherit its
// bytes). Without it a lone continuation byte at an undeclared id turned an
// otherwise valid message into INVALID_MSG.
//
// Placed ahead of the maxlen/limit guards, which are already destination-scoped
// and therefore unaffected — §5.2's INVALID-over-INCOMPLETE ordering is
// preserved.
//
// A schema with no field of this kind at all has no destination *anywhere*, so
// the callback body is empty rather than guarded — see stringDests, whose empty
// result is what the caller keys that on. (The callback itself is still emitted:
// Visitor declares it, and the corelib still delivers strings to a message that
// declares none.)
func (g *gen) emitDestGuard(f *jfile, fs []frame, dests []destFrame) {
	f.line("        // A payload this scope does not declare is skipped: its bytes are jumped")
	f.line("        // over, never inspected. Resolve the destination first and leave before a")
	f.line("        // byte is buffered, decoded or checked.")
	f.line("        switch (cur) {")
	for _, d := range dests {
		if len(d.ids) == 0 {
			f.line("        case %d: break;", d.idx)
			continue
		}
		var labels []string
		for _, id := range d.ids {
			labels = append(labels, fmt.Sprintf("case %d:", id))
		}
		f.line("        case %d: switch (id) { %s break; default: return; } break;", d.idx, strings.Join(labels, " "))
	}
	f.line("        default: return;")
	f.line("        }")
}

// emitFixlenBegin latches every schema bound a fixlen field's LENGTH WORD already
// decides, at that word (CORELIB_PLAN §5.2, generator#267).
//
// The bounds are not new -- a scalar/element `maxlen` and a wrapper element's
// `id >= count` were both already rejected -- but they were rejected in the
// PAYLOAD callback, which only fires once payload bytes arrive. A message
// truncated immediately after the length word therefore never reached them and
// reported INCOMPLETE, while the same bytes read whole are INVALID. §5.2 makes
// INVALID dominate INCOMPLETE precisely because the violation is already
// established by the bytes seen; corelib-java#62 added the hook.
//
// Every guard sits inside the DECLARED-subtype test. The hook fires for whatever
// fixlen subtype arrived at a field id -- the corelib resolves what arrived but
// cannot know what was declared -- so a contradicting subtype is a §7.3 skip and
// must not be measured against this field's bound (#224/#259, one position over).
//
// The payload-side guards stay: unreachable for a message that gets this far, and
// the only thing still bounding a consumer built against an older corelib.
func (g *gen) emitFixlenBegin(f *jfile, fs []frame) {
	str := g.fixlenBeginArms(fs, ir.KindString, "string length")
	blob := g.fixlenBeginArms(fs, ir.KindBlob, "blob length")
	if len(str) == 0 && len(blob) == 0 {
		return
	}
	f.line("    @Override")
	f.line("    public void fixlenBegin(int id, FixlenType subtype, int total) {")
	f.line("        // Decided at the LENGTH WORD, not once payload bytes arrive: S5.2 makes")
	f.line("        // INVALID dominate INCOMPLETE, so truncating right after this word must")
	f.line("        // not downgrade the verdict. The subtype test is S7.3 -- a contradicting")
	f.line("        // fixlen kind at this id is a SKIPPED field, not this field's length.")
	for _, a := range []struct {
		variant string
		arms    []string
	}{{"STRING", str}, {"BLOB", blob}} {
		if len(a.arms) == 0 {
			continue
		}
		f.line("        if (subtype == FixlenType.%s) {", a.variant)
		f.line("            switch (cur) {")
		for _, arm := range a.arms {
			f.line("%s", arm)
		}
		f.line("            default: break;")
		f.line("            }")
		f.line("        }")
	}
	f.line("    }")
}

// fixlenBeginArms builds the per-scope arms for one fixlen subtype: a wrapper
// element carries its array's over-index bound AND its element maxlen, a scalar
// field carries its own maxlen. Over-index first -- an element that is not this
// array's element at all must not have its length measured against the element
// bound.
func (g *gen) fixlenBeginArms(fs []frame, kind ir.Kind, noun string) []string {
	var arms []string
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && (fr.cap >= 0 || fr.emax >= 0) {
			body := overIndexGuard(fr.cap, fr.loc)
			if fr.emax >= 0 {
				body += fmt.Sprintf("if (total > %d) %s ", fr.emax, maxlenThrow(locName(fr.loc)+" element", noun, fr.emax))
			}
			arms = append(arms, fmt.Sprintf("            case %d: %sbreak;", fr.idx, body))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var inner []string
		for _, fld := range fr.fields {
			if fld.Kind == kind && fld.HasMaxlen {
				inner = append(inner, fmt.Sprintf("case %d: if (total > %d) %s break;", fld.ID, fld.Maxlen, maxlenThrow(fld.Name, noun, fld.Maxlen)))
			}
		}
		if len(inner) > 0 {
			arm := fmt.Sprintf("            case %d: switch (id) { ", fr.idx)
			arm += strings.Join(inner, " ")
			arm += " default: break; } break;"
			arms = append(arms, arm)
		}
	}
	return arms
}

// emitMaxlenGuard writes the schema-maxlen reject (MESSAGE_SPEC §7.1) at the top
// of the string()/blob() callback, the bounded-field twin of emitLenLimitGuard:
// every field of this kind that declares a schema `maxlen` (scalar fields and
// wrapper-sequence elements alike) gets a (cur, id) arm that rejects a declared
// `total` above its own maxlen with INVALID_MSG — before any byte is accumulated,
// so an oversized split payload is rejected on its first chunk, and never
// truncated. Schema-unbounded fields fall through unaffected (governed by their
// own generator#102 configured limit). Emitted unconditionally: with no bounded
// field of this kind the guard is absent, leaving the output byte-identical.
func (g *gen) emitMaxlenGuard(f *jfile, fs []frame, kind ir.Kind, noun string) {
	// Detect whether any bounded field of this kind exists before emitting.
	any := false
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && fr.emax >= 0 {
			any = true
		}
		if fr.kind == fkNormal {
			for _, fld := range fr.fields {
				if fld.Kind == kind && fld.HasMaxlen {
					any = true
				}
			}
		}
	}
	if !any {
		return
	}
	f.line("        // Bounded fields (schema maxlen): a wire byte length above the")
	f.line("        // declared maxlen is malformed input, INVALID before any byte is")
	f.line("        // accumulated -- never a truncation.")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && fr.emax >= 0 {
			f.line("        case %d: if (total > %d) %s break;", fr.idx, fr.emax, maxlenThrow(locName(fr.loc)+" element", noun, fr.emax))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == kind && fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("case %d: if (total > %d) %s break;", fld.ID, fld.Maxlen, maxlenThrow(fld.Name, noun, fld.Maxlen)))
			}
		}
		if len(arms) > 0 {
			f.line("        case %d: switch (id) {", fr.idx)
			for _, a := range arms {
				f.line("            %s", a)
			}
			f.line("        } break;")
		}
	}
	f.line("        }")
}

// emitArraySkipArm arms the §7.3 discard counter at the top of arrayBegin
// (generator#183). Native arrays are armed: their elements land in
// unsigned()/signed()/fp32()/fp64(), the very callbacks a lone scalar shares, so an
// array header at an id whose declared type is a scalar would otherwise be decoded
// instead of skipped — the one wire-type contradiction the id dispatch cannot
// detect on its own (MESSAGE_SPEC §7.3: skip it like an unknown id). Every
// (scope, id) that genuinely declares a native array of the matching element
// kind disarms the counter, so a legitimate array stores normally; everything
// else — a scalar-declared id, an unknown id — discards exactly `count` elements,
// after which a real scalar at the same id still decodes.
//
// One arm per wire ArrayKind, and each arm disarms ONLY at the ids whose declared
// element type maps to that very kind (generator#254): UNSIGNED covers u*/boolean/
// bitfield, SIGNED covers i*/enum, FP32 covers fp32 and FP64 covers fp64. Treating
// UNSIGNED and SIGNED as one case let an array-signed header at an
// unsigned-declared array id disarm the counter, i.e. decode a header §7.3 says to
// skip; the same reasoning splits the two fixlen subtypes (generator#259 /
// Crucible F-0042). The counter self-terminates on `count`, so no array-end
// callback is needed, and it lives in the visitor, so it survives a feed chunk
// boundary.
func (g *gen) emitArraySkipArm(f *jfile, fs []frame) {
	f.line("        // A native array delivered at an id that does not declare one")
	f.line("        // of the SAME array kind is a wire-type contradiction -- arm a discard")
	f.line("        // counter so the element callbacks drop exactly `count` elements. Every id")
	f.line("        // that really declares an array of that element kind disarms it below.")
	f.line("        askip = 0;")
	f.line("        afill = 0;")
	arm := func(lead, wireKind string, want func(ir.Kind) bool) {
		f.line("        %s (kind == ArrayKind.%s) {", lead, wireKind)
		f.line("            askip = count;")
		f.line("            switch (cur) {")
		for _, fr := range fs {
			switch fr.kind {
			case fkNativeMat:
				// A nested-native row: elements arrive without an id switch, so the
				// whole frame disarms the skip and arms the fill — but only when the
				// row's kind on the wire is the one the inner element declares.
				if want(fr.innerElem) {
					f.line("            case %d: askip = 0; afill = count; break;", fr.idx)
				}
			case fkNormal:
				var ids []string
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && want(fld.Elem) {
						ids = append(ids, fmt.Sprintf("case %d:", fld.ID))
					}
				}
				if len(ids) > 0 {
					f.line("            case %d: switch (id) {", fr.idx)
					f.line("                %s askip = 0; afill = count; break;", strings.Join(ids, " "))
					f.line("            } break;")
				}
			}
		}
		f.line("            }")
		f.line("        }")
	}
	arm("if", "UNSIGNED", unsignedArrayElem)
	arm("else if", "SIGNED", signedArrayElem)
	// Fixlen arrays deliver through fp32()/fp64(), the callbacks a lone fp scalar
	// shares (generator#193), so they are armed exactly like the integer arms — but
	// with ONE ARM PER SUBTYPE (generator#259 / Crucible F-0042). corelib-java used
	// to collapse fp32 and fp64 into a single ArrayKind.FIXLEN, announced on the
	// count word before the fixlen_word had even been read; per CORELIB_PLAN §4.8 it
	// now announces the array after the fixlen_word and names FP32 or FP64. That
	// makes an fp64 header at a declared fp32[] slot (and vice versa) a wire-type
	// contradiction this hook can see: the field's own arm is not entered, so the
	// discard counter stays armed and the declared array is never sized from the
	// skipped header's count (§7.3).
	arm("else if", "FP32", fp32ArrayElem)
	arm("else if", "FP64", fp64ArrayElem)
}

// emitArraySkipGuard prepends the §7.3 discard clause to unsigned()/signed()
// (generator#183) and fp32()/fp64() (generator#193): while arrayBegin has an
// array armed at a scalar id, each delivered element is dropped rather than
// routed by id. Emitted for those four callbacks — the ones an array shares with
// a lone scalar; string()/blob() are unaffected.
func (g *gen) emitArraySkipGuard(f *jfile) {
	f.line("        // Drop an element of an array whose id does")
	f.line("        // not declare one -- armed by arrayBegin, self-terminating on count.")
	f.line("        if (askip > 0) { askip--; return; }")
}

func (g *gen) emitVisitor(f *jfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	limArr, limStr, limBlob := g.activeLimits(fs) // per-visitor decode limits (generator#102)

	f.line("class %sVisitor implements Visitor {", name)
	f.line("    private final %s m;", name)
	f.line("    private int cur = 0;")
	// The SKIPPED-SUBTREE scope. sequenceBegin moves here for any (scope, id) the
	// schema does not declare, and every callback dispatches on `cur` with a case
	// per real scope -- so nothing matches while cur is _DEAD and the whole subtree
	// is discarded, children included (generator#268 / #272).
	f.line("    private static final int _DEAD = -1;")
	f.line("    private int ai = 0;                 // index into the primitive array currently being filled")
	// §7.3 array-vs-scalar skip counter (generator#183): an integer array whose id
	// is declared as a SCALAR is a wire-type contradiction and must be skipped like
	// an unknown id. corelib-java delivers array elements one-by-one through the
	// same unsigned()/signed() callbacks a lone scalar uses, so the id dispatch
	// alone cannot tell them apart; arrayBegin arms this with the announced element
	// count and the two callbacks discard exactly that many.
	f.line("    private int askip = 0;              // elements left to discard from a wire-type-contradictory array (S7.3)")
	// §7.3 mirror (generator#188): a bare scalar delivered at an id whose declared
	// type is an ARRAY of that scalar's element type is the opposite wire-type
	// contradiction — the id dispatch would route it into the array-fill arm and
	// store it as element 0. arrayBegin arms this with the announced element count
	// at legitimate native-array positions only; a fill arm acts only while it is
	// positive and decrements it, so a real array (armed by its own arrayBegin)
	// fills normally while a bare scalar (afill == 0) falls through and is skipped.
	f.line("    private int afill = 0;              // elements still expected by an armed native-array fill (S7.3)")
	// The armed fill's destination, resolved once per array by arrayBegin instead
	// of once per element by the (scope, id) switches. Emitted only when the
	// message has a native array at all.
	if hasFill(fs) {
		f.line("    private int atgt = 0;               // which destination the armed fill writes into")
	}
	// The destination handed to the corelib's bulk offer, and which field it is.
	// Set by the same arrayBegin arm that arms the fill, so the offer itself is a
	// field read rather than a third dispatch over (scope, id).
	if hasBulk(fs) {
		f.line("    private Object abulk;               // destination offered to Visitor.arrayBulk, null when not offered")
	}
	f.line("    private int[] stk = new int[16];    // sequence scope stack (unboxed, was ArrayDeque<Integer>)")
	f.line("    private int sp = 0;")
	// One per wrapper array whose elements have inner state: the index
	// sequenceBegin/arrayBegin placed the element at, which its child field or row
	// accessors read back (MESSAGE_SPEC §5.1).
	for _, fr := range fs {
		switch fr.kind {
		case fkSeqObj, fkSeqMat, fkNativeMat:
			f.line("    private int %s = 0;  // index of the element being decoded in %s (S5.1: the element id IS the index)", elemIdxVar(fr.loc), fr.loc)
		}
	}
	// The primitive matrix row being filled, parked by arrayBegin. Only one native
	// array fill is ever open at a time (arrays do not nest on the wire), so one
	// cursor per element base serves every matrix in the message.
	for _, base := range primRowBasesUsed(fs) {
		f.line("    private %s[] %s = %s;  // primitive matrix row currently being filled", base, rowCursor(base), emptyPrimFor(base))
	}
	// One accumulator per visitor, as PayloadAcc documents: a payload arriving in
	// one chunk never touches its buffer, so holding it costs nothing until a
	// string or blob is actually split.
	f.line("    private final PayloadAcc acc = new PayloadAcc();")
	if limArr || limStr || limBlob {
		// Emitted only for the limits that are configured AND have at least one
		// schema-unbounded field in this message, so an unset or inert key changes
		// nothing in the output.
		f.line("    // Receiver-side decode limits, baked from the sofabgen")
		f.line("    // config: caps on fields the schema left unbounded (no count / maxlen).")
		f.line("    // Exceeding one fails the decode with SofabError.LIMIT_EXCEEDED at the")
		f.line("    // wire count/length header, before any allocation or accumulation --")
		f.line("    // never a clamp. Schema-bounded fields are not governed by these caps;")
		f.line("    // they keep their own schema-capacity guard.")
		if limArr {
			f.line("    static final long MAX_DYN_ARRAY_COUNT = %dL;", g.limits.arrayCount)
		}
		if limStr {
			f.line("    static final long MAX_DYN_STRING_LEN = %dL;", g.limits.stringLen)
		}
		if limBlob {
			f.line("    static final long MAX_DYN_BLOB_LEN = %dL;", g.limits.blobLen)
		}
	}
	f.line("    %sVisitor(%s msg) { m = msg; }", name, name)
	f.blank()

	// unsigned: u*/bitfield scalars, bool, unsigned/bool array elements, and
	// unsigned/bool native-matrix rows.
	g.emitScalarCb(f, fs, "unsigned", "long", func(fld *ir.Field) (string, bool) {
		switch {
		case fld.Kind == ir.KindU8 || fld.Kind == ir.KindU16 || fld.Kind == ir.KindU32 || fld.Kind == ir.KindU64 || fld.Kind == ir.KindBitfield:
			return "= value", true
		case fld.Kind == ir.KindBool:
			return "= value != 0", true
		case fld.Kind == ir.KindArray:
			switch {
			case isUnsignedElem(fld.Elem) || fld.Elem == ir.KindBitfield:
				return "index", true // primitive long[] fill
			case fld.Elem == ir.KindBool:
				// A boolean array stays a List<Boolean>, cleared at arrayBegin and
				// grown by the M elements the wire carries -- M IS the length, with
				// or without a declared count (MESSAGE_SPEC §3).
				return "addBool", true
			}
		}
		return "", false
	})

	g.emitScalarCb(f, fs, "signed", "long", func(fld *ir.Field) (string, bool) {
		switch {
		case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64 || fld.Kind == ir.KindEnum:
			return "= value", true
		case fld.Kind == ir.KindArray && (isSignedElem(fld.Elem) || fld.Elem == ir.KindEnum):
			return "index", true // primitive long[] fill
		}
		return "", false
	})

	g.emitScalarCb(f, fs, "fp32", "float", func(fld *ir.Field) (string, bool) {
		if fld.Kind == ir.KindFP32 {
			return "= value", true
		}
		if fld.Kind == ir.KindArray && fld.Elem == ir.KindFP32 {
			return "index", true // primitive float[] fill
		}
		return "", false
	})
	g.emitScalarCb(f, fs, "fp64", "double", func(fld *ir.Field) (string, bool) {
		if fld.Kind == ir.KindFP64 {
			return "= value", true
		}
		if fld.Kind == ir.KindArray && fld.Elem == ir.KindFP64 {
			return "index", true // primitive double[] fill
		}
		return "", false
	})

	// Strict UTF-8 decode (MESSAGE_SPEC §8 / CORELIB_PLAN §6.4) is the corelib's
	// Utf8.decode, reached through the accumulator: a `string` is UTF-8 and Java's
	// String is a Unicode type, so the platform `new String(bytes, UTF_8)` -- which
	// substitutes U+FFFD -- can never be what a payload is materialized with.

	// Every schema bound the LENGTH WORD already decides, latched at that word
	// rather than once payload bytes arrive.
	g.emitFixlenBegin(f, fs)

	g.emitStringCb(f, fs, limStr)

	// blob, through the same accumulator.
	f.line("    public void blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	if limBlob {
		g.emitLenLimitGuard(f, fs, ir.KindBlob, "MAX_DYN_BLOB_LEN", "blob length", g.limits.blobLen)
	}
	g.emitMaxlenGuard(f, fs, ir.KindBlob, "blob length")
	f.line("        byte[] _b = acc.blob(total, offset, data, chunkOffset, chunkLength);")
	f.line("        if (_b == null) return;")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindBlob {
			// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
			// element is omitted on the wire, so place the value at its id and fill
			// any gap with the element default (empty bytes).
			f.line("        case %d: %swhile (%s.size() <= id) %s.add(new byte[0]); %s.set(id, _b); break;", fr.idx, overIndexGuard(fr.cap, fr.loc), fr.listExpr, fr.listExpr, fr.listExpr)
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindBlob {
				arms = append(arms, jcase(fld.ID, fr.path+"."+javaIdent(fld.Name)+" = _b"))
			}
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")

	// arrayBegin: one dispatch, not two.
	//
	// It used to run the §7.3 skip/fill arming and the destination reset as two
	// separate (cur, id) walks, the first behind a four-way `kind ==` chain. Both
	// are keyed on the same (scope, id, kind) triple, so they are one switch: each
	// arm tests its own field's array kind, rejects an over-capacity count, disarms
	// the skip, arms the fill, parks the fill target and resets the destination.
	// Measured on the arena message (ten arrays per decode) the two-pass shape was
	// 9.7 % of decode — more than the element callbacks that follow it.
	//
	// SKIPPING IS THE DEFAULT (MESSAGE_SPEC §7.3). `askip = count` up front is what
	// an id this scope does not declare, or declares with a different array kind,
	// falls through to: its elements are dropped one by one and a real scalar at
	// that id still decodes afterwards. Every arm that runs disarms it. This is
	// exactly what the old first pass computed, since ArrayKind has no fifth value.
	//
	// A primitive array reserves a small backing store (capped, NOT `new T[count]`
	// — count is untrusted, see #96) and is grown/filled by index (ai reset here);
	// a boolean array clears its List; a native-matrix row is placed at the index
	// its element id names.
	f.line("    public void arrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        ai = 0;")
	f.line("        // An array delivered at an id that does not declare one of the SAME")
	f.line("        // array kind is a wire-type contradiction: drop exactly `count` elements")
	f.line("        // and leave the declared field untouched (S7.3). Every arm below that")
	f.line("        // runs is a declared array at a matching kind, and disarms this.")
	f.line("        askip = count;")
	f.line("        afill = 0;")
	if hasBulk(fs) {
		f.line("        abulk = null;      // no bulk destination unless an arm below offers one")
	}
	f.line("        switch (cur) {")
	for i := range fs {
		fr := &fs[i]
		if fr.kind == fkNativeMat {
			// A native-matrix row is itself a native array, and its own element
			// count needs its own bound -- `cap` above bounds the row's ID, not
			// how many elements the row claims. A row the schema counts is bounded
			// by that count (INVALID above it, §7.1); one the schema leaves
			// unbounded is governed by the configured cap (LIMIT_EXCEEDED,
			// generator#102). Either way the row is bounded BEFORE it is sized,
			// which is what lets the sizing below be exact.
			guard := ""
			switch {
			case fr.innerHasCount:
				guard = fmt.Sprintf("if (count > %d) throw Sofab.invalid(\"%s element: array count above schema capacity %d\"); ",
					fr.innerCap, locName(fr.loc), fr.innerCap)
			case limArr:
				guard = limitThrowGuard("count > MAX_DYN_ARRAY_COUNT", locName(fr.loc), "array count above configured limit", g.limits.arrayCount) + " "
			}
			// A row whose header carries a different array kind than the inner
			// element declares is skipped whole (§7.3, generator#254). Checked
			// FIRST, so a bound below can only ever reject a row that survives the
			// kind test.
			kindGuard := arrayKindGuard(fr.innerElem)
			// The row's ID is bounded before its element count, matching the order
			// every other backend takes the two verdicts in -- both are INVALID, so
			// only the message differs, and a family that words the same rejection
			// differently is a conformance diff waiting to happen.
			arm := kindGuard + overIndexGuard(fr.cap, fr.loc) + guard + armFill(fs, fr, nil)
			// The row's element id IS its index in the outer array (§5.1), so it is
			// PLACED there after gap-filling with empty rows -- never appended.
			// Appending ignored the id, which an interior gap (an omitted all-default
			// row, §2) turns into a one-off shift of every later row. The outer
			// array's count bounds the id, which also bounds the gap fill.
			if primitiveArrayElem(fr.innerElem) {
				// A primitive row is placed as a right-sized (capped) array and parked
				// in its cursor, so the element fill neither re-reads it out of the
				// List nor boxes a value into one.
				base := primArrayBase(fr.innerElem)
				// Sized at exactly the wire count, once: the guard above has just
				// bounded it against the row's schema count or against the cap
				// (ARCHITECTURE §9.5, shape A). The wire already said how big the row
				// is, so growing into it would only add copies.
				if guard == "" {
					panic("java: unbounded matrix row with no cap -- every target has a finite default (§9.5)")
				}
				f.line("        case %d: %s%s = Seq.%s(%s, id, count); %s = id; break;",
					fr.idx, arm, rowCursor(base), reserveRowFn(base), fr.listExpr, elemIdxVar(fr.loc))
				continue
			}
			f.line("        case %d: %sSeq.reserveRow(%s, id); %s = id; break;", fr.idx, arm, fr.listExpr, elemIdxVar(fr.loc))
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
			// §7.3 comes FIRST (generator#254): a header whose array kind is not the
			// one this field's declared element type maps to must be skipped exactly
			// like an unknown id -- the declared field must not be touched at all,
			// which includes not being RESIZED from the skipped header's count.
			// Ordering matters as much as the test: the schema bound below applies
			// only to a field that survives this check, so an over-count MIS-TYPED
			// array is skipped, not a false INVALID. Since the fixlen kinds are per
			// subtype (generator#259 / Crucible F-0042), that now covers an fp64
			// header at a declared fp32[N] too: it is skipped, never bounded and
			// never sized.
			kindGuard := arrayKindGuard(fld.Elem)
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject up front, never clamp or keep-all
			// (generator#100). Unchecked wrapper: Visitor callbacks cannot throw
			// the checked SofabException; decode() rethrows as RuntimeException.
			// An UNBOUNDED array (no schema count) is instead governed by the
			// configured max_dyn_array_count when set (generator#102): exceeding
			// it is LIMIT_EXCEEDED — a receiver policy error, not INVALID_MSG.
			guard := ""
			if fld.HasCount {
				guard = fmt.Sprintf("if (count > %d) throw Sofab.invalid(\"%s: array count above schema capacity %d\"); ",
					fld.Count, fld.Name, fld.Count)
			} else if limArr {
				guard = limitThrowGuard("count > MAX_DYN_ARRAY_COUNT", fld.Name, "array count above configured limit", g.limits.arrayCount) + " "
			}
			// The wire count M IS the array's length (MESSAGE_SPEC §3): the M
			// elements that arrived are the whole value, so the container is grown
			// as they come and ends exactly M long. A declared `count: N` is a
			// CAPACITY and bounds M (the guard above); it never adds elements, so
			// there is nothing to materialize at [M, N) and a count:N array is
			// filled exactly like a count-less one.
			target := fr.path + "." + javaIdent(fld.Name)
			arm := kindGuard + guard + armFill(fs, fr, fld)
			// Allocated at exactly the wire count, once (ARCHITECTURE §9.5, shape
			// A): the guard above has already bounded that count against the
			// schema capacity or the configured cap, so the wire has said how big
			// the array is and a bound has said the claim is allowed. Growing into
			// it from a capped reservation -- the #96/#98 shape, written the day
			// before the caps of #102 existed -- would only add doubling and copies.
			if primitiveArrayElem(fld.Elem) {
				if guard == "" {
					panic("java: native array with neither a schema count nor a cap -- every target has a finite default (§9.5)")
				}
				alloc := target + " = new " + primArrayBase(fld.Elem) + "[count]"
				if bulkCapable(fld) {
					// A long-backed field IS the bulk destination; a narrowed one is
					// filled through the scratch and reduced into the field by
					// arrayBulkEnd. Either way the field is allocated here, so the
					// per-element arm stays the fallback for a decoder that declines.
					alloc = "abulk = " + alloc
				}
				arms = append(arms, jcase(fld.ID, arm+alloc))
			} else { // boolean List
				arms = append(arms, jcase(fld.ID, arm+target+".clear()"))
			}
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")

	g.emitBulkCbs(f, fs)

	// sequenceBegin / sequenceEnd
	f.line("    public void sequenceBegin(int id) {")
	f.line("        if (sp == stk.length) stk = java.util.Arrays.copyOf(stk, sp * 2);")
	f.line("        stk[sp++] = cur;")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		switch fr.kind {
		case fkSeqObj:
			// MESSAGE_SPEC §5.1: the element id IS the array index, exactly as on the
			// string/blob leaf-element paths above, so the element is PLACED at
			// list.get(id) after gap-filling with default elements -- never appended.
			// Appending shortened the array by the size of any interior id gap and
			// decoded a REOPENED id as a second element instead of merging into the
			// first (§7.4 struct-merge, which placement gives for free: the existing
			// element is reused and the reopened frame's fields land on top of it).
			// The over-index guard still rejects id >= N, which also bounds the
			// gap-fill.
			f.line("        case %d: %swhile (%s.size() <= id) %s.add(new %s()); %s = id; cur = %d; break;",
				fr.idx, overIndexGuard(fr.cap, fr.loc), fr.listExpr, fr.listExpr, fr.elemType, elemIdxVar(fr.loc), locIndex(fs, fr.childLoc))
		case fkSeqMat:
			// A row of an array-of-wrapper-arrays is placed at the index its element
			// id names, for the same reason as the struct element above and the
			// native-matrix row in arrayBegin: appending ignored the id, and an
			// interior gap (an omitted all-default row, §2) then shifts every later
			// row down by one. An array wrapper IS the array's value, so a REOPENED
			// row id replaces the row rather than merging into it (§7.4).
			f.line("        case %d: %sSeq.reserveRow(%s, id); %s = id; cur = %d; break;", fr.idx, overIndexGuard(fr.cap, fr.loc), fr.listExpr, elemIdxVar(fr.loc), locIndex(fs, fr.childLoc))
		case fkNormal:
			var arms []string
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
					arms = append(arms, jcase(fld.ID, "cur = "+itoa(locIndex(fs, fr.loc+"_"+fld.Name))))
				case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
					arms = append(arms, jcase(fld.ID, fr.path+"."+javaIdent(fld.Name)+".clear(); cur = "+itoa(locIndex(fs, fr.loc+"_"+fld.Name))))
				}
			}
			// A skipping default even when this scope declares no sequence at all:
			// reaching sequenceBegin here at all means an id that is not one of them.
			if len(arms) == 0 {
				f.line("        case %d: cur = _DEAD; break;", fr.idx)
			} else {
				arms = append(arms, "default: cur = _DEAD; break;")
				g.frameSwitch(f, fr.idx, arms)
			}
		}
	}
	// And the same for a scope with no case above -- a leaf array scope, say --
	// where the switch would otherwise fall straight through and leave `cur` on the
	// enclosing frame (generator#272).
	f.line("        default: cur = _DEAD; break;")
	f.line("        }")
	f.line("    }")
	g.emitSequenceEnd(f)
	f.line("}")
	f.blank()
}

// primArrayBasesUsed returns the distinct Java primitive element bases
// ("long"/"float"/"double") of the primitive-array fields across all frames, in
// a stable order. Non-empty means the visitor fills a primitive array at all.
func primArrayBasesUsed(fs []frame) []string {
	seen := map[string]bool{}
	var out []string
	for _, order := range primBaseOrder {
		for _, fr := range fs {
			// A native-matrix ROW is a primitive array too (List<long[]>), sized by
			// the same arrayBegin and indexed by the same ai as a top-level one.
			if fr.kind == fkNativeMat && primitiveArrayElem(fr.innerElem) &&
				primArrayBase(fr.innerElem) == order && !seen[order] {
				seen[order] = true
				out = append(out, order)
				continue
			}
			if fr.kind != fkNormal {
				continue
			}
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && primitiveArrayElem(fld.Elem) && primArrayBase(fld.Elem) == order && !seen[order] {
					seen[order] = true
					out = append(out, order)
				}
			}
		}
	}
	return out
}

// primBaseOrder is every Java primitive an array field can be backed by, in a
// fixed order so the emitted row cursors come out stable.
var primBaseOrder = []string{"byte", "short", "int", "long", "float", "double"}

// primRowBasesUsed is primArrayBasesUsed restricted to native-matrix ROWS: the
// bases needing a `_arow<B>` cursor field and a Seq.reserveRow<B>s factory.
func primRowBasesUsed(fs []frame) []string {
	seen := map[string]bool{}
	var out []string
	for _, order := range primBaseOrder {
		for _, fr := range fs {
			if fr.kind == fkNativeMat && primitiveArrayElem(fr.innerElem) &&
				primArrayBase(fr.innerElem) == order && !seen[order] {
				seen[order] = true
				out = append(out, order)
			}
		}
	}
	return out
}

// emitBulkCbs writes the two halves of the corelib's bulk-array offer
// (Visitor.arrayBulk / arrayBulkEnd), the fast path for an integer array.
//
// arrayBegin has resolved and sized the destination, so the offer is a field read
// -- not a third walk over (scope, id) -- and the decoder then writes the elements
// straight into the field's own array. The array's WIDTH is what tells the decoder
// the declared width: handing back a byte[] says "u8/i8 elements", and a value
// that does not fit is INVALID (§7.1) rather than truncated, checked in the same
// pass that decodes. So all arrayBulkEnd has left to do is clear the fill counter,
// which no element callback was there to count down.
func (g *gen) emitBulkCbs(f *jfile, fs []frame) {
	if !hasBulk(fs) {
		return
	}
	// Deliberately NOT @Override. Visitor declares both with a default, so a
	// corelib that has them calls these and takes the fast path, while one that
	// predates them simply never does -- and the generated code still compiles
	// against it, with the per-element arms above filling the very same array.
	// @Override would make the newer corelib a hard requirement for code that
	// works either way.
	f.line("    public Object arrayBulk(int id, ArrayKind kind, int count) {")
	f.line("        // Offered iff arrayBegin sized a schema-bounded destination just now.")
	f.line("        // Its element width IS the declared width, so the decoder checks and")
	f.line("        // narrows in the pass that decodes.")
	f.line("        return abulk;")
	f.line("    }")
	f.line("    public void arrayBulkEnd(int id, int n) {")
	f.line("        afill = 0;   // the elements never went through the element callbacks")
	f.line("        abulk = null;")
	f.line("    }")
}

// fillTargetsFor lists, in frame order, every destination an armed native-array
// fill can write into whose elements arrive through callback cb, numbering them
// densely from 1. arrayBegin parks a target's number in `atgt`; the callback
// switches on it.
//
// The numbering is PER CALLBACK, and may repeat across callbacks, because only
// one native-array fill is ever open at a time and its element kind decides which
// callback delivers it: while an UNSIGNED array is armed, signed()/fp32()/fp64()
// cannot be called at all. Dense numbers keep each callback's switch a
// tableswitch.
func fillTargetsFor(fs []frame, cb string) map[*frame]map[int64]int {
	out := map[*frame]map[int64]int{}
	n := 0
	for i := range fs {
		fr := &fs[i]
		switch fr.kind {
		case fkNativeMat:
			// A matrix row has no id switch of its own: the whole frame is one target.
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

// hasBulk reports whether the message has any array the corelib's bulk offer can
// be taken for.
//
// Exactly the integer arrays: the offer needs a destination sized to `count` up
// front, which every native array now has -- the count is checked against the
// schema bound or the cap before it is allocated from, so the untrusted-count
// objection that once restricted this to SCHEMA-BOUNDED arrays (#96) is answered
// by the check rather than by the reservation (ARCHITECTURE §9.5, shape A). That
// leaves out boolean arrays (a List), fp arrays (the offer is integer-only) and
// matrix rows (whose destination is a row cursor, not a field).
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
	if fld.Kind != ir.KindArray || !primitiveArrayElem(fld.Elem) {
		return false
	}
	switch primArrayBase(fld.Elem) {
	case "byte", "short", "int", "long":
		return true
	}
	return false // fp: the decoder's fixlen element loop, not the integer one
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

// rowCursor is the visitor field holding the primitive matrix row currently being
// filled. The row also lives in the message's List<T[]> at its element index, but
// reading it back through List.get on every element -- and storing the grown array
// back through List.set -- is per-element work for a reference that changes at most
// log2(count) times. arrayBegin parks it here; only a growth writes it back.
func rowCursor(base string) string { return "_arow" + strings.ToUpper(base[:1]) + base[1:] }

// reserveRowFn is the corelib factory that places a primitive matrix row of the
// given element base: Seq.reserveRowBytes, reserveRowShorts and so on. The corelib
// spells the suffix in the plural, after the element type rather than the array.
func reserveRowFn(base string) string {
	if base == "byte" {
		return "reserveRowBytes"
	}
	return "reserveRow" + strings.ToUpper(base[:1]) + base[1:] + "s"
}

// emitScalarCb writes a callback that routes (cur,id) to a field assignment or a
// list .add. action() returns "= value" / "add" / "addBool" / "setBool" /
// "index" / "= value != 0".
//
// An ARRAY ELEMENT does not go through that (cur,id) routing at all. Its
// destination was already resolved by the arrayBegin that armed the fill, so the
// element arms hang off `atgt` — one dense switch — ahead of the scalar routing,
// which the array ids then leave entirely. Before, every element of every array
// re-derived its destination through a switch on the scope and a second switch on
// the id, and the scalar switches carried the array ids as well; on the arena
// message (fifty array elements per decode) unsigned()+signed() were 14 % of
// decode. Only a fill that arrayBegin armed can be open when a callback runs, so
// `afill != 0` is the whole test.
func (g *gen) emitScalarCb(f *jfile, fs []frame, cb, vtype string, action func(*ir.Field) (string, bool)) {
	f.line("    public void %s(int id, %s value) {", cb, vtype)
	// The §7.3 discard guard heads every callback an array shares with a lone
	// scalar: unsigned/signed for integer arrays, fp32/fp64 for fp arrays (#193).
	isElemCb := cb == "unsigned" || cb == "signed" || cb == "fp32" || cb == "fp64"
	if isElemCb {
		g.emitArrayFillArm(f, fs, cb)
		g.emitArraySkipGuard(f)
	}
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			act, ok := action(fld)
			if !ok {
				continue
			}
			// Array fills live in the atgt arm above, not here.
			if fld.Kind == ir.KindArray {
				continue
			}
			target := fr.path + "." + javaIdent(fld.Name)
			arms = append(arms, jcase(fld.ID, widthThrow(fld.Kind, fld.Name)+target+" "+act))
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")
}

// emitArrayFillArm writes the armed-fill prologue of an element callback: while
// arrayBegin has a native array armed, every value is an element of THAT array,
// so it is stored against the target arrayBegin parked rather than routed by
// (scope, id) again.
//
// The width guard sits inside the arm, never in front of it: an over-width scalar
// arriving at an array id with no arrayBegin in front of it is a §7.3 skip, not an
// INVALID, and it never reaches this arm because afill is zero.
func (g *gen) emitArrayFillArm(f *jfile, fs []frame, cb string) {
	tgts := fillTargetsFor(fs, cb)
	if len(tgts) == 0 {
		return
	}
	// Emit in frame order so the switch labels come out dense and sorted.
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
			if primitiveArrayElem(fr.innerElem) {
				// Fill the row through the cursor arrayBegin parked. The row was sized
				// at exactly the announced count, so there is no growth and no
				// reference to write back into the List (§9.5, shape A).
				cur := rowCursor(primArrayBase(fr.innerElem))
				arms = append(arms, arm{ids[-1], fmt.Sprintf("%s%s[ai++] = %svalue",
					widthThrow(fr.innerElem, fr.loc+" element"), cur, primArrayCast(fr.innerElem))})
				continue
			}
			// A boxed row (boolean): the row arrayBegin PLACED at the element id, not
			// the last-appended one -- an interior id gap must leave an empty row, not
			// shift the values into the wrong row.
			row := fr.listExpr + ".get(" + elemIdxVar(fr.loc) + ")"
			arms = append(arms, arm{ids[-1], fmt.Sprintf("%s%s.add(%s)",
				widthThrow(fr.innerElem, fr.loc+" element"), row, matConv(fr.innerElem))})
			continue
		}
		for _, fld := range fr.fields {
			code, ok := ids[fld.ID]
			if !ok {
				continue
			}
			target := fr.path + "." + javaIdent(fld.Name)
			if fld.Elem == ir.KindBool {
				// A boolean array stays a List<Boolean>, cleared at arrayBegin and
				// grown by the M elements the wire carries -- M IS the length, with
				// or without a declared count (MESSAGE_SPEC §3).
				arms = append(arms, arm{code, target + ".add(value != 0)"})
				continue
			}
			// A plain indexed store. arrayBegin allocated the destination at exactly
			// the announced count, having first bounded that count against the schema
			// capacity or the configured cap (§9.5, shape A), so nothing here can
			// run past the end and nothing has to grow: no doubling, no copies, and
			// no reference store into the message object per element.
			arms = append(arms, arm{code, widthThrow(fld.Elem, fld.Name+" element") +
				target + "[ai++] = " + primArrayCast(fld.Elem) + "value"})
		}
	}
	f.line("        // An element of the array arrayBegin armed: its destination is already")
	f.line("        // resolved, so it is stored against that target rather than routed by")
	f.line("        // (scope, id) again. Self-terminating on the announced count.")
	f.line("        if (afill != 0) {")
	f.line("            afill--;")
	f.line("            switch (atgt) {")
	for _, a := range arms {
		f.line("            case %d: %s; return;", a.code, a.stmt)
	}
	f.line("            }")
	f.line("            return;")
	f.line("        }")
}

// frameSwitch emits `case <idx>: switch(id){ <arms> } break;`.
func (g *gen) frameSwitch(f *jfile, idx int, arms []string) {
	f.line("        case %d: switch (id) {", idx)
	for _, a := range arms {
		f.line("            %s", a)
	}
	f.line("        } break;")
}

func jcase(id int64, stmt string) string {
	return fmt.Sprintf("case %d: %s; break;", id, stmt)
}

func isUnsignedElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
}
func isSignedElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
}

// unsignedArrayElem / signedArrayElem / fp32ArrayElem / fp64ArrayElem partition
// the native array element kinds by the wire ArrayKind an array of them maps to
// (MESSAGE_SPEC §1/§3): unsigned-array for u*/boolean/bitfield, signed-array for
// i*/enum, and — since the fixlen header names its element subtype (CORELIB_PLAN
// §4.8) — FP32 for fp32 and FP64 for fp64, one kind each rather than a single
// collapsed "fixlen" bucket. A header carrying any OTHER kind at such a field is a
// wire-type contradiction and must be skipped whole (§7.3, generator#254 and
// generator#259 / Crucible F-0042) — never stored, and never sized into the
// declared field.
func unsignedArrayElem(k ir.Kind) bool {
	return isUnsignedElem(k) || k == ir.KindBool || k == ir.KindBitfield
}
func signedArrayElem(k ir.Kind) bool {
	return isSignedElem(k) || k == ir.KindEnum
}
func fp32ArrayElem(k ir.Kind) bool {
	return k == ir.KindFP32
}
func fp64ArrayElem(k ir.Kind) bool {
	return k == ir.KindFP64
}

// arrayWireKind is the ArrayKind constant an array of `k` is encoded with — the
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

// arrayKindGuard is the leading clause of an arrayBegin arm: leave the arm
// untouched unless the header's array kind is the one this element type maps to
// (§7.3). Emitted BEFORE the schema-bound guard so a mis-typed header is skipped
// rather than rejected (generator#254).
func arrayKindGuard(k ir.Kind) string {
	wk := arrayWireKind(k)
	if wk == "" {
		return ""
	}
	return "if (kind != ArrayKind." + wk + ") break; "
}

// nativeArrayElem reports whether an array element is carried by the native array
// wire type (numeric/enum/boolean/bitfield) rather than a wrapper sequence.
func nativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindEnum, ir.KindBool, ir.KindBitfield:
		return true
	}
	return false
}

// seqArrayElem reports whether an array element lowers to a wrapper sequence
// (string/blob/struct/union, or a nested array).
func seqArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindStruct, ir.KindUnion, ir.KindArray:
		return true
	}
	return false
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

// matConv converts a native-matrix inner value to its boxed member type: boolean
// compares against 0, everything else autoboxes.
func matConv(k ir.Kind) string {
	if k == ir.KindBool {
		return "value != 0"
	}
	return "value"
}

func itoa(i int) string     { return strconv.Itoa(i) }
func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
