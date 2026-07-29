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
				out = append(out, frame{kind: fkNativeMat, loc: loc, listExpr: listExpr, innerElem: items.Elem, innerRef: items.ElemRef, innerHasCount: items.HasCount, cap: cap})
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
	return fmt.Sprintf("if (id >= %d) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"%s element: array index above schema capacity %d\")); ", cap, name, cap)
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

// limitThrow renders the generator#102 rejection: same unchecked-wrapper shape
// as the generator#100 schema guard (a Visitor callback cannot throw the
// checked SofabException), but with the LIMIT_EXCEEDED category — a receiver
// policy error, kept distinct from wire malformation.
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
// input, so it fails the decode with INVALID_MSG — the same unchecked-wrapper
// channel java uses for the generator#100/#142 schema guards (a Visitor callback
// cannot throw the checked SofabException), kept distinct from the generator#102
// LIMIT_EXCEEDED receiver-policy cap on schema-unbounded fields.
func maxlenThrow(name, noun string, max int64) string {
	return fmt.Sprintf("throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"%s: %s above schema maxlen %d\"));",
		name, noun, max)
}

// emitStringCb writes the string() visitor callback. Single-shot: when the whole
// payload arrives in one chunk, decode straight from the input slice, skipping
// the (synchronized) ByteArrayOutputStream.
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
	f.line("        String _s;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _s = _utf8(data, chunkOffset, total);")
	f.line("        } else {")
	f.line("            if (acc == null) acc = new java.io.ByteArrayOutputStream();")
	f.line("            acc.write(data, chunkOffset, chunkLength);")
	f.line("            if (acc.size() < total) return;")
	f.line("            _s = _utf8(acc.toByteArray(), 0, total);")
	f.line("            acc.reset();")
	f.line("        }")
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
// wire-type contradiction routed down the same path, never reaches _utf8() and
// never enters the shared `acc` (so a later declared field cannot inherit its
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
// bitfield, SIGNED covers i*/enum, FIXLEN covers fp32/fp64. Treating UNSIGNED and
// SIGNED as one case let an array-signed header at an unsigned-declared array id
// disarm the counter, i.e. decode a header §7.3 says to skip. The counter
// self-terminates on `count`, so no array-end callback is needed, and it lives in
// the visitor, so it survives a feed chunk boundary.
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
	// Fixlen (fp32/fp64) arrays deliver through fp32()/fp64(), the callbacks a lone
	// fp scalar shares (generator#193), so they are armed exactly like the integer
	// arms. The fixlen SUBTYPE (fp32 vs fp64) is not visible in this hook — the
	// corelib collapses both into ArrayKind.FIXLEN — so a subtype contradiction is
	// caught downstream, where the element lands in fp32() or fp64().
	arm("else if", "FIXLEN", fpArrayElem)
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
	primBases := primArrayBasesUsed(fs) // "long"/"float"/"double" element bases needing lazy growth
	hasPrim := len(primBases) > 0
	limArr, limStr, limBlob := g.activeLimits(fs) // per-visitor decode limits (generator#102)

	f.line("class %sVisitor implements Visitor {", name)
	f.line("    private final %s m;", name)
	f.line("    private int cur = 0;")
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
	if hasPrim {
		// The wire-supplied element count is UNTRUSTED: a malformed message can
		// claim ~2^31 elements, so we never allocate `new T[count]` up front (that
		// is an OutOfMemoryError DoS — see generator issue #96). Instead reserve a
		// small backing array and grow it as elements actually arrive, capped at
		// `acap` (the declared count) so the array still ends exactly right-sized.
		f.line("    private static final int ARRAY_INIT_CAP = 16; // bounded eager reservation; grow lazily")
		f.line("    private int acap = 0;               // declared element count = growth ceiling for the array being filled")
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
	f.line("    private java.io.ByteArrayOutputStream acc; // lazy: only split string/blob payloads need it")
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

	// Strict UTF-8 decode (MESSAGE_SPEC §8 / CORELIB_PLAN §6.4): a `string` is
	// UTF-8 and Java's String is a Unicode type, so it is always strict — the
	// platform `new String(bytes, UTF_8)` is LOSSY (substitutes U+FFFD), which
	// §8 forbids in every mode. Validity is a property of the complete payload, so
	// the check runs once the full `total` bytes are present.
	//
	// Rather than a REPORTing CharsetDecoder (which allocates a fresh decoder +
	// CharBuffer per call — ~52 ns/string, dominated by that setup), validate the
	// bytes with an allocation-free well-formedness scan and, when they are valid,
	// hand them to the JVM-intrinsic `new String(b, off, len, UTF_8)` (vectorized,
	// and with valid input it never substitutes). This mirrors protobuf-java's
	// hand-rolled Utf8 validator + intrinsic decode and measured ~43 % faster on
	// the arena strings, at zero per-string allocation. The scan itself is
	// sofab.Utf8.valid, which accepts exactly well-formed UTF-8 (RFC 3629) — the
	// same set the fatal decoder rejects.
	f.line("    private static String _utf8(byte[] b, int off, int len) {")
	f.line("        if (Utf8.valid(b, off, off + len)) return new String(b, off, len, java.nio.charset.StandardCharsets.UTF_8);")
	f.line("        throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"string: invalid UTF-8\"));")
	f.line("    }")

	// string. Single-shot: when the whole payload arrives in one chunk, decode
	// straight from the input slice, skipping the (synchronized) ByteArrayOutputStream.
	g.emitStringCb(f, fs, limStr)

	// blob. Single-shot on the whole-in-one-chunk fast path (see string).
	f.line("    public void blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	if limBlob {
		g.emitLenLimitGuard(f, fs, ir.KindBlob, "MAX_DYN_BLOB_LEN", "blob length", g.limits.blobLen)
	}
	g.emitMaxlenGuard(f, fs, ir.KindBlob, "blob length")
	f.line("        byte[] _b;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _b = java.util.Arrays.copyOfRange(data, chunkOffset, chunkOffset + total);")
	f.line("        } else {")
	f.line("            if (acc == null) acc = new java.io.ByteArrayOutputStream();")
	f.line("            acc.write(data, chunkOffset, chunkLength);")
	f.line("            if (acc.size() < total) return;")
	f.line("            _b = acc.toByteArray();")
	f.line("            acc.reset();")
	f.line("        }")
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

	// arrayBegin: a primitive array reserves a small backing store (capped, NOT
	// `new T[count]` — count is untrusted, see #96) and is grown/filled by index
	// (ai reset below); a boolean array clears its List; a native-matrix row is
	// placed at the index its element id names.
	f.line("    public void arrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        ai = 0;")
	if hasPrim {
		f.line("        acap = count;")
	}
	g.emitArraySkipArm(f, fs)
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkNativeMat {
			// A native-matrix row is itself a native array: an inner array the
			// schema left unbounded is governed by the configured cap too
			// (generator#102), checked at its own count header.
			guard := ""
			if limArr && !fr.innerHasCount {
				guard = limitThrowGuard("count > MAX_DYN_ARRAY_COUNT", locName(fr.loc), "array count above configured limit", g.limits.arrayCount) + " "
			}
			// A row whose header carries a different array kind than the inner
			// element declares is skipped whole (§7.3, generator#254): its elements
			// are already discarded by the skip counter above, and the row itself
			// must not be materialized either. Checked FIRST, so a bound below can
			// only ever reject a row that survives the kind test.
			kindGuard := arrayKindGuard(fr.innerElem)
			// The row's element id IS its index in the outer array (§5.1), so it is
			// PLACED there after gap-filling with empty rows -- never appended.
			// Appending ignored the id, which an interior gap (an omitted all-default
			// row, §2) turns into a one-off shift of every later row. The outer
			// array's count bounds the id, which also bounds the gap fill.
			f.line("        case %d: %s%s%sSbuf.placeRow(%s, id); %s = id; break;", fr.idx, kindGuard, guard, overIndexGuard(fr.cap, fr.loc), fr.listExpr, elemIdxVar(fr.loc))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			// §7.3 comes FIRST (generator#254): a header whose array kind is not the
			// one this field's declared element type maps to must be skipped exactly
			// like an unknown id -- its elements are dropped by the skip counter
			// above, and the declared field must not be touched at all, which
			// includes not being RESIZED from the skipped header's count. Ordering
			// matters as much as the test: the schema bound below applies only to a
			// field that survives this check, so an over-count MIS-TYPED array is
			// skipped, not a false INVALID.
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
				guard = fmt.Sprintf("if (count > %d) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"%s: array count above schema capacity %d\")); ",
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
			if fld.Kind == ir.KindArray && primitiveArrayElem(fld.Elem) {
				arms = append(arms, jcase(fld.ID, kindGuard+guard+target+" = new "+primArrayBase(fld.Elem)+"[Math.min(count, ARRAY_INIT_CAP)]"))
			} else if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) { // boolean List
				arms = append(arms, jcase(fld.ID, kindGuard+guard+target+".clear()"))
			}
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")

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
			f.line("        case %d: %sSbuf.placeRow(%s, id); %s = id; cur = %d; break;", fr.idx, overIndexGuard(fr.cap, fr.loc), fr.listExpr, elemIdxVar(fr.loc), locIndex(fs, fr.childLoc))
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
			if len(arms) > 0 {
				g.frameSwitch(f, fr.idx, arms)
			}
		}
	}
	f.line("        }")
	f.line("    }")
	g.emitSequenceEnd(f)
	// Lazy-growth helper(s): enlarge the backing array to hold index `i`, doubling
	// but never exceeding `cap` (the declared element count) so a valid array ends
	// exactly right-sized. Growth tracks elements actually delivered, so an
	// untrusted count cannot force an up-front over-allocation (#96).
	for _, base := range primBases {
		f.line("    private static %s[] ensureCap(%s[] a, int i, int cap) {", base, base)
		f.line("        if (i < a.length) return a;")
		f.line("        long n = (long) a.length * 2;")
		f.line("        if (n < i + 1) n = i + 1;")
		f.line("        if (n > cap) n = cap;")
		f.line("        return java.util.Arrays.copyOf(a, (int) n);")
		f.line("    }")
	}
	f.line("}")
	f.blank()
}

// primArrayBasesUsed returns the distinct Java primitive element bases
// ("long"/"float"/"double") of the primitive-array fields across all frames, in
// a stable order, so emitVisitor can emit exactly the ensureCap overloads it needs.
func primArrayBasesUsed(fs []frame) []string {
	seen := map[string]bool{}
	var out []string
	for _, order := range []string{"long", "float", "double"} {
		for _, fr := range fs {
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

// emitScalarCb writes a callback that routes (cur,id) to a field assignment or a
// list .add. action() returns "= value" / "add" / "addBool" / "setBool" /
// "index" / "= value != 0".
// Native-matrix frames whose inner element matches this callback append the
// decoded value to the current row (no id switch: rows arrive index-ordered).
func (g *gen) emitScalarCb(f *jfile, fs []frame, cb, vtype string, action func(*ir.Field) (string, bool)) {
	f.line("    public void %s(int id, %s value) {", cb, vtype)
	// The §7.3 discard guard heads every callback an array shares with a lone
	// scalar: unsigned/signed for integer arrays, fp32/fp64 for fp arrays (#193).
	if cb == "unsigned" || cb == "signed" || cb == "fp32" || cb == "fp64" {
		g.emitArraySkipGuard(f)
	}
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkNativeMat {
			if nativeElemCb(fr.innerElem) == cb {
				// The row arrayBegin PLACED at the element id, not the last-appended
				// one: an interior id gap must leave an empty row, not shift the
				// values into the wrong row.
				row := fr.listExpr + ".get(" + elemIdxVar(fr.loc) + ")"
				// Gated like the fkNormal fills (generator#188): a matrix inner row
				// is armed by its own arrayBegin; a bare scalar in the matrix scope
				// (afill == 0) is skipped.
				f.line("        case %d: if (afill == 0) break; afill--; %s.add(%s); break;", fr.idx, row, matConv(fr.innerElem))
			}
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			act, ok := action(fld)
			if !ok {
				continue
			}
			target := fr.path + "." + javaIdent(fld.Name)
			// A fill arm runs only while arrayBegin has this native array armed
			// (generator#188): a bare scalar at an array id arrives with afill == 0
			// and is skipped like an unknown id (S7.3). The scalar arms (default)
			// are never gated — a scalar at a scalar id is exactly right.
			const fillGuard = "if (afill == 0) break; afill--; "
			var stmt string
			switch act {
			case "add":
				stmt = fillGuard + target + ".add(value)"
			case "addBool":
				stmt = fillGuard + target + ".add(value != 0)"
			case "index":
				// Grow the backing array on demand (never trust the wire count), up
				// to the announced count, so a valid array ends exactly M long.
				stmt = fillGuard + target + " = ensureCap(" + target + ", ai, acap); " + target + "[ai++] = value"
			default:
				stmt = target + " " + act
			}
			arms = append(arms, jcase(fld.ID, stmt))
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")
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

// unsignedArrayElem / signedArrayElem / fpArrayElem partition the native array
// element kinds by the wire ArrayKind an array of them maps to (MESSAGE_SPEC
// §1/§3): unsigned-array for u*/boolean/bitfield, signed-array for i*/enum,
// fixlen-array for fp32/fp64. A header carrying any OTHER kind at such a field is
// a wire-type contradiction and must be skipped whole (§7.3, generator#254) —
// never stored, and never sized into the declared field.
func unsignedArrayElem(k ir.Kind) bool {
	return isUnsignedElem(k) || k == ir.KindBool || k == ir.KindBitfield
}
func signedArrayElem(k ir.Kind) bool {
	return isSignedElem(k) || k == ir.KindEnum
}
func fpArrayElem(k ir.Kind) bool {
	return k == ir.KindFP32 || k == ir.KindFP64
}

// arrayWireKind is the ArrayKind constant an array of `k` is encoded with — the
// only kind whose header may decode into that field.
func arrayWireKind(k ir.Kind) string {
	switch {
	case unsignedArrayElem(k):
		return "UNSIGNED"
	case signedArrayElem(k):
		return "SIGNED"
	case fpArrayElem(k):
		return "FIXLEN"
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
