package csharp

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// frame is one location in the flat-visitor state machine: either an object
// scope (root / struct / union / array-element object, with fields) or an array
// scope (the wrapper sequence collecting elements of one array field into the
// List `path`). Array scopes cover every wrapper-sequence element kind:
// string/blob (primitive collectors), struct/union (element object descends via
// childLoc), and nested arrays (native inner collected in place, sequence inner
// descends via childLoc).
type frame struct {
	loc    string
	path   string
	fields []*ir.Field // object scope
	// array scope (fields == nil, isArr == true):
	isArr    bool
	elem     ir.Kind       // element kind of this array
	ref      *ir.TypeRef   // enum/bitfield/struct/union element
	items    *ir.ArrayElem // nested-array element descriptor
	childLoc string        // struct/union element or sequence-nested inner-array scope
	elemDyn  bool          // string/blob element without a schema maxlen (generator#102)
	// cap is the wrapper array's schema count bound N (-1 == no count). N is a
	// CAPACITY, not a length: it never reaches the wire and never adds elements the
	// wire did not carry. All it does here is bound the array — a wrapper element id
	// >= N is a schema-bound violation (MESSAGE_SPEC §5.1/§7 — issue #142), rejected
	// as INVALID before the List grows, which also bounds an over-index
	// heap-amplification fill.
	cap int64
	// emax is a string/blob element's schema maxlen L (-1 == no bound): an element
	// whose wire byte length exceeds L is malformed input, INVALID (MESSAGE_SPEC
	// §7.1), rejected at the length header before any bytes accumulate — never
	// truncated.
	emax int64
}

// capOf maps a schema count bound to a frame's cap: N when the array declares a
// count, -1 (unbounded) otherwise. N is a CAPACITY: the visitor uses it only to
// reject an out-of-range element id, never to size the result.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// boundOf maps a schema maxlen presence+value to its bound: L when present, -1
// (unbounded) otherwise.
func boundOf(has bool, v int64) int64 {
	if has {
		return v
	}
	return -1
}

// overIndexGuard returns the reject clause for a `count: N` wrapper array: an
// element id >= N throws InvalidMessage (aborting decode) before the List grows
// (MESSAGE_SPEC §5.1/§7 — issue #142), which also bounds an over-index
// heap-amplification fill. Empty for a count-less array (cap == -1).
func (g *gen) overIndexGuard(cap int64, loc string) string {
	if cap < 0 {
		return ""
	}
	return fmt.Sprintf("if (id >= %d) throw new SofabException(SofabError.InvalidMessage, \"%s element: array index above schema capacity %d\"); ", cap, loc, cap)
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walkObj func(loc, path string, fields []*ir.Field)
	var walkArr func(loc, list string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemDyn bool, cap, emax int64)

	walkObj = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walkObj(loc+"_"+fld.Name, path+"."+csIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				walkArr(loc+"_"+fld.Name, path+"."+csIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, !fld.ElemMaxHas, capOf(fld.HasCount, fld.Count), boundOf(fld.ElemMaxHas, fld.ElemMax))
			}
		}
	}

	// walkArr registers the array scope entered on SequenceBegin(field/index),
	// plus any child scope its elements descend into. elemDyn marks a string/
	// blob element scope whose elements carry no schema maxlen (generator#102);
	// cap is the array's schema fixed-count bound (-1 == dynamic); emax is the
	// string/blob element's schema maxlen (-1 == unbounded).
	walkArr = func(loc, list string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemDyn bool, cap, emax int64) {
		fr := frame{loc: loc, path: list, isArr: true, elem: elem, ref: ref, items: items, cap: cap, emax: emax}
		switch elem {
		case ir.KindStruct, ir.KindUnion:
			fr.childLoc = loc + "_e"
			out = append(out, fr)
			// The element id IS the array index (MESSAGE_SPEC §5.1), so the element
			// scope decodes into the element that id names — never into "the one just
			// appended" (generator#247). SequenceBegin records the id in this scope's
			// own index variable, which the whole child sub-tree's paths hang off.
			walkObj(fr.childLoc, elemAt(list, loc), ref.Target.Fields)
		case ir.KindArray:
			if seqArrayElem(items.Elem) {
				fr.childLoc = loc + "_e"
				out = append(out, fr)
				// Like the struct/union element above, a wrapper ROW is decoded into the
				// element its id names, never into "the one just appended": an interior
				// row equal to the element default is omitted by a conformant encoder
				// (§2), so an id gap is ordinary and appending would shift every later
				// row down by one.
				walkArr(fr.childLoc, elemAt(list, loc), items.Elem, items.ElemRef, items.ElemItems, !items.ElemMaxHas, capOf(items.HasCount, items.Count), boundOf(items.ElemMaxHas, items.ElemMax))
			} else {
				out = append(out, fr) // native inner rows collected in place
			}
		default: // string/blob
			fr.elemDyn = elemDyn
			out = append(out, fr)
		}
	}

	walkObj("Root", "m", m.Fields)
	return out
}

// hasDynPrimArray reports whether any object-scope field is a primitive (T[])
// array without a schema count: its wire count is untrusted AND unbounded, so
// the visitor needs the lazy-growth machinery (ArrayInitCap/acap/EnsureCap)
// instead of an eager new T[count] (cf. generator#96/#100/#102).
func hasDynPrimArray(fs []frame) bool {
	for _, fr := range fs {
		if fr.isArr {
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && primArrayElem(fld.Elem) && !fld.HasCount {
				return true
			}
		}
	}
	return false
}

// primFill is the statement filling the next slot of the primitive array field
// `target`. A `count: N` array was allocated at exactly the WIRE count M by
// ArrayBegin — M IS the array's length (MESSAGE_SPEC §3) and the generator#100
// guard already rejected M > N — so the elements land in place with nothing to
// fill in behind them. A count-less array starts small and grows on demand via
// EnsureCap, so an untrusted wire count never allocates.
func primFill(target string, fld *ir.Field, rhs string) string {
	if !fld.HasCount {
		return fillGuard + fmt.Sprintf("%s = EnsureCap(%s, ai, acap); %s[ai++] = %s;", target, target, target, rhs)
	}
	return fillGuard + fmt.Sprintf("%s[ai++] = %s;", target, rhs)
}

// fillGuard fronts every native-array fill arm (generator#188): the fill runs
// only while ArrayBegin has this array armed (afill > 0). A bare scalar delivered
// at an array id arrives with afill == 0 — no ArrayBegin preceded it — so it
// breaks out of the (cur, id) switch and is skipped like an unknown id, the
// mirror of the askip guard that skips an array at a scalar id (MESSAGE_SPEC §7.3).
const fillGuard = "if (afill == 0) break; afill--; "

// nativeListFill is the statement filling the next slot of a native List<T>
// array field `target` (boolean/enum/bitfield elements — these value-convert
// element-wise and so stay List<T>, cf. primArrayElem). It appends, with or
// without a declared `count: N`: the wire count M IS the array's length
// (MESSAGE_SPEC §3), so the M elements that arrived are the whole value and a
// capacity N never adds any behind them.
func nativeListFill(target, rhs string) string {
	return fillGuard + fmt.Sprintf("%s.Add(%s);", target, rhs)
}

// placeRow is the statement that stores a decoded ROW of a nested array (an array
// whose elements are themselves arrays) at the index its element id names, growing
// the outer List with empty rows so an id GAP decodes as an empty row instead of
// shifting every later row down by one. Gaps are ordinary here: an interior row
// equal to the element default (the empty row) is omitted by a conformant encoder
// (MESSAGE_SPEC §2), and only the LAST row is guaranteed present — which is what
// makes the decoded length, highest present id + 1, exact. A re-opened id gets a
// fresh row: an array wrapper IS the array's value and is replaced whole, not
// merged (§7.4).
//
// The over-index guard runs first: `count: N` is a capacity, so a row id >= N is
// INVALID (§5.1/§7) and rejecting before the grow also bounds the id-keyed fill
// against an over-index amplification DoS.
func (g *gen) placeRow(fr frame) string {
	row, _ := g.csSeqElemDefault(fr.elem, fr.ref, fr.items) // the empty row
	return fmt.Sprintf("%swhile (%s.Count <= id) %s.Add(%s); %s[id] = %s; %s = id; ",
		g.overIndexGuard(fr.cap, fr.loc), fr.path, fr.path, row, fr.path, row, ixVar(fr.loc))
}

// emitLenGuard writes the generator#102 length guard at the top of the String/
// Blob callback: when the wire `total` exceeds the configured cap and the
// target (cur, id) is a schema-unbounded field, decode fails with
// LimitExceeded before any bytes are accumulated (single-shot and chunked
// paths alike). Schema-bounded fields fall through unaffected.
func (g *gen) emitLenGuard(f *cfile, fs []frame, kind ir.Kind, constName, what string, limit int64) {
	f.line("        if (total > %s) {", constName)
	f.line("            switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == kind && fr.elemDyn {
				f.line("            case (%s, _): throw new SofabException(SofabError.LimitExceeded, \"%s element: %s above configured limit %d\");", fr.loc, fr.loc, what, limit)
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind && !fld.HasMaxlen {
				f.line("            case (%s, %d): throw new SofabException(SofabError.LimitExceeded, \"%s: %s above configured limit %d\");", fr.loc, fld.ID, fld.Name, what, limit)
			}
		}
	}
	f.line("            }")
	f.line("        }")
}

// stringDestLabels collects the switch labels for every (loc, id) that can
// materialize a string: the string-declaring fields plus the wrapper-sequence
// rows whose element kind is string. An empty result means the message never
// materializes a string at all.
func (g *gen) stringDestLabels(fs []frame) []string {
	var labels []string
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindString {
				labels = append(labels, fmt.Sprintf("case (%s, _):", fr.loc))
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindString {
				labels = append(labels, fmt.Sprintf("case (%s, %d):", fr.loc, fld.ID))
			}
		}
	}
	return labels
}

// emitStringCb writes the String visitor callback. Single-shot: when the whole
// payload arrives in one chunk, decode straight from the contiguous input slice;
// the per-byte List<byte> accumulator is only the fallback for a genuinely split
// payload.
//
// A message that declares no string at all still gets the callback — the Visitor
// interface declares it, and the corelib still delivers string fields to a
// message that has none — but its body is EMPTY. Every string reaching it is by
// definition skipped, and an empty body is what skipping means: decoding one
// only to drop it would validate a payload nobody reads, which is what
// CORELIB_PLAN §6.4 forbids (generator#257).
func (g *gen) emitStringCb(f *cfile, fs []frame, limStr bool) {
	f.line("    public void String(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	defer f.line("    }")

	labels := g.stringDestLabels(fs)
	if len(labels) == 0 {
		f.line("        // No field of this message is a string, so every string payload the")
		f.line("        // decoder delivers is skipped whole -- its bytes are never inspected.")
		return
	}
	g.emitDestGuard(f, labels)
	// MESSAGE_SPEC §7.1: a bounded string whose wire byte length exceeds its
	// schema maxlen is malformed input, rejected as INVALID at the `total` header
	// before any bytes accumulate (never truncated).
	g.emitMaxlenGuard(f, fs, ir.KindString, "string length")
	if limStr {
		// generator#102: reject an over-cap unbounded string at its `total`
		// header, before the fast path decodes or the accumulator grows.
		g.emitLenGuard(f, fs, ir.KindString, "MaxDynStringLen", "string length", g.limits.stringLen)
	}
	f.line("        string _s;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _s = _Utf8(data, chunkOffset, total);")
	f.line("        } else {")
	f.line("            acc ??= new List<byte>();")
	f.line("            for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("            if (acc.Count < total) return;")
	f.line("            _s = _Utf8(acc.ToArray(), 0, total);")
	f.line("            acc.Clear();")
	f.line("        }")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindString {
				// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
				// element is omitted on the wire, so place each value at its id and
				// grow the list, filling any gap with the element default ("").
				f.line("            case (%s, _): %swhile (%s.Count <= id) %s.Add(\"\"); %s[id] = _s; break;", fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, fr.path, fr.path)
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindString {
				f.line("            case (%s, %d): %s.%s = _s; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
			}
		}
	}
	f.line("        }")
}

// emitDestGuard writes the skip gate at the very top of the String callback
// (CORELIB_PLAN §6.4, generator#257): "skipped fields are never validated".
// Skipping is a length jump over bytes that are not inspected (§5.2), and UTF-8
// validation runs only where a `string` is materialized — read into a
// destination. So the destination is resolved FIRST: every (loc, id) that
// declares a string, plus the wrapper-sequence rows whose element kind is
// string, falls through; anything else returns right here.
//
// Returning here is what makes the skip a true skip: an unknown id, or a §7.3
// wire-type contradiction routed down the same path, never reaches _Utf8() and
// never enters the shared `acc` (so a later declared field cannot inherit its
// bytes). Without it a lone continuation byte at an undeclared id turned an
// otherwise valid message into InvalidMessage.
//
// Placed ahead of the maxlen/limit guards, which are already destination-scoped
// and therefore unaffected — §5.2's INVALID-over-INCOMPLETE ordering is
// preserved. The case labels mirror the materializing switch below one for one,
// so a loc is never both a `(loc, _)` row and a `(loc, id)` field arm.
func (g *gen) emitDestGuard(f *cfile, labels []string) {
	f.line("        // A payload this scope does not declare is skipped: its bytes are jumped")
	f.line("        // over, never inspected. Resolve the destination first and leave before a")
	f.line("        // byte is buffered, decoded or checked.")
	f.line("        switch ((cur, id)) {")
	for _, l := range labels {
		f.line("            %s", l)
	}
	f.line("                break;")
	f.line("            default: return;")
	f.line("        }")
}

// emitMaxlenGuard writes the schema-maxlen reject (MESSAGE_SPEC §7.1) at the top
// of the String/Blob callback, the bounded-field twin of emitLenGuard: every
// field of that kind with a schema `maxlen` (scalar fields and wrapper-sequence
// elements alike) gets a (loc, id) arm that throws InvalidMessage when the wire
// `total` exceeds its own maxlen — checked at the length header before any bytes
// accumulate (single-shot and chunked paths alike), never truncated. Unbounded
// fields get no arm: the generator#102 configured limit governs them. Each
// bounded field carries a distinct maxlen, so the arms compare per-arm rather
// than sharing one outer `if` (unlike emitLenGuard's single constant cap).
func (g *gen) emitMaxlenGuard(f *cfile, fs []frame, kind ir.Kind, what string) {
	var arms []string
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == kind && fr.emax >= 0 {
				arms = append(arms, fmt.Sprintf("            case (%s, _): if (total > %d) throw new SofabException(SofabError.InvalidMessage, \"%s element: %s above schema maxlen %d\"); break;", fr.loc, fr.emax, fr.loc, what, fr.emax))
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind && fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("            case (%s, %d): if (total > %d) throw new SofabException(SofabError.InvalidMessage, \"%s: %s above schema maxlen %d\"); break;", fr.loc, fld.ID, fld.Maxlen, fld.Name, what, fld.Maxlen))
			}
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("        switch ((cur, id)) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("        }")
}

// emitArraySkipGuard prepends the S7.3 discard clause to Unsigned()/Signed()
// (generator#183) and Fp32()/Fp64() (generator#193). corelib-cs delivers an
// array element-by-element through the very callback a lone scalar uses, so a
// field id declared as a SCALAR that receives an ARRAY header (integer arrays via
// Unsigned/Signed, fp arrays via Fp32/Fp64) would otherwise store the elements —
// the one wire-type contradiction the (cur, id) dispatch cannot
// detect on its own. ArrayBegin arms askip with the announced element count;
// here they are discarded one by one, which self-terminates without an
// array-end callback and works across feed chunk boundaries (askip lives on
// the visitor).
func (g *gen) emitArraySkipGuard(f *cfile) {
	f.line("        if (askip > 0) { askip--; return; }   // discard a contradictory array at a scalar id")
}

// emitArraySkipArm arms the S7.3 discard counter in ArrayBegin (generator#183,
// extended to fp by generator#193). Every array kind whose elements land in a
// callback a scalar shares is armed: integers land in Unsigned()/Signed(), fp
// lands in Fp32()/Fp64(). Every (scope, id) that genuinely declares a native
// array of that element kind — plus every nested native-inner-array scope, whose
// rows arrive as ArrayBegin(index) — disarms it (=> 0), so a legitimate array
// stores normally; everything else (a scalar-declared id, an unknown id, a
// wrapper-sequence id) discards exactly `count` elements, after which a real
// scalar at the same id still decodes. Mirrors emitArrayFillArm.
//
// One arm per wire ArrayKind, and each arm disarms ONLY where the declared
// element type maps to that very kind (generator#254): Unsigned covers
// u*/boolean/bitfield, Signed covers i*/enum, Fixlen covers fp32/fp64. Folding
// Unsigned and Signed into one arm let an array-signed header at an
// unsigned-declared array id disarm the counter, i.e. decode a header S7.3 says
// to skip.
func (g *gen) emitArraySkipArm(f *cfile, fs []frame) {
	arm := func(pat string, want func(ir.Kind) bool) {
		f.line("            %s => (cur, id) switch {", pat)
		for _, fr := range fs {
			if fr.isArr {
				if fr.elem == ir.KindArray && want(fr.items.Elem) {
					f.line("                (%s, _) => 0,", fr.loc)
				}
				continue
			}
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && want(fld.Elem) {
					f.line("                (%s, %d) => 0,", fr.loc, fld.ID)
				}
			}
		}
		f.line("                _ => count,")
		f.line("            },")
	}
	f.line("        // An array header at an id that does not declare a native array of the")
	f.line("        // matching element kind is a wire-type contradiction: discard its")
	f.line("        // `count` elements, exactly as an unknown id would be skipped.")
	f.line("        askip = kind switch {")
	arm("ArrayKind.Unsigned", unsignedArrayElem)
	arm("ArrayKind.Signed", signedArrayElem)
	// The fixlen SUBTYPE (fp32 vs fp64) is not visible in this hook — the corelib
	// collapses both into ArrayKind.Fixlen — so a subtype contradiction is caught
	// downstream, where the element lands in Fp32() or Fp64().
	arm("ArrayKind.Fixlen", fpArrayElem)
	f.line("            _ => 0,")
	f.line("        };")
}

// emitArrayFillArm arms the S7.3 fill counter in ArrayBegin (generator#188), the
// mirror of emitArraySkipArm. It is armed at a legitimate native-array position
// whose declared element kind matches the array kind on the wire — integer arrays
// under Unsigned/Signed, fp arrays under Fixlen — and stays 0 everywhere else, so
// a bare scalar delivered at an array id (no ArrayBegin, afill == 0) falls through
// its fill arm and is skipped. Every native element kind is covered because both
// the integer fills (Unsigned/Signed) and the fp fills (Fp32/Fp64) are gated.
func (g *gen) emitArrayFillArm(f *cfile, fs []frame) {
	arm := func(pat string, want func(ir.Kind) bool) {
		f.line("            %s => (cur, id) switch {", pat)
		for _, fr := range fs {
			if fr.isArr {
				if fr.elem == ir.KindArray && want(fr.items.Elem) {
					f.line("                (%s, _) => count,", fr.loc)
				}
				continue
			}
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && want(fld.Elem) {
					f.line("                (%s, %d) => count,", fr.loc, fld.ID)
				}
			}
		}
		f.line("                _ => 0,")
		f.line("            },")
	}
	f.line("        afill = kind switch {")
	// One arm per wire ArrayKind, exactly complementary to emitArraySkipArm: a
	// header whose kind is not the one the declared element maps to arms the skip
	// counter, never a fill (generator#254).
	arm("ArrayKind.Unsigned", unsignedArrayElem)
	arm("ArrayKind.Signed", signedArrayElem)
	arm("ArrayKind.Fixlen", fpArrayElem)
	f.line("            _ => 0,")
	f.line("        };")
}

func (g *gen) emitVisitor(f *cfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	dynPrim := hasDynPrimArray(fs)
	// A configured max_dyn_* cap is live only when this message actually has a
	// schema-unbounded field of that kind — otherwise it is inert and no
	// constant or guard is emitted (generator#102).
	b := ir.Bounds(fields)
	limArr := g.limits.arrayHas && b.HasDynArray
	limStr := g.limits.stringHas && b.HasDynString
	limBlob := g.limits.blobHas && b.HasDynBlob

	f.line("internal sealed class %sVisitor : IVisitor {", name)
	f.line("    private readonly %s m;", name)
	f.line("    private int cur = 0;")
	f.line("    private int ai = 0;                // index into the primitive array currently being filled")
	// S7.3 array-vs-scalar skip counter (generator#183): an integer array whose id
	// is declared as a SCALAR is a wire-type contradiction and must be skipped like
	// an unknown id. corelib-cs delivers array elements through the same
	// Unsigned()/Signed() callbacks a lone scalar uses, so the (cur, id) dispatch
	// alone cannot tell them apart; ArrayBegin arms this with the announced element
	// count and the callbacks discard exactly that many.
	f.line("    private int askip = 0;             // elements left to discard from a wire-type-contradictory array")
	// S7.3 mirror (generator#188): a bare scalar at an array id would land in the
	// array-fill arm and be stored as element 0. ArrayBegin arms this with the
	// element count at legitimate native-array positions; a fill arm runs only
	// while it is positive, so an unarmed bare scalar (afill == 0) is skipped.
	f.line("    private int afill = 0;             // elements still expected by an armed native-array fill (S7.3)")
	if dynPrim {
		// The wire-supplied element count of a count-less array is untrusted:
		// never allocate `new T[count]` up front (an out-of-memory DoS, cf.
		// generator#96/#100). Reserve a small backing array and grow it as
		// elements actually arrive, capped at the wire count so an honest
		// array still ends exactly right-sized.
		f.line("    private const int ArrayInitCap = 16; // bounded eager reservation for count-less arrays; grow lazily")
		f.line("    private int acap = 0;              // wire count = growth ceiling for the count-less array being filled")
	}
	f.line("    private int[] stk = new int[16];   // sequence scope stack (unboxed, was Stack<int>)")
	f.line("    private int sp = 0;")
	f.line("    private List<byte> acc;            // lazy: only split string/blob payloads need it")
	f.line("    public %sVisitor(%s msg) { m = msg; }", name, name)
	for i, fr := range fs {
		f.line("    private const int %s = %d;", fr.loc, i)
	}
	// One element-index latch per struct/union or nested-array scope
	// (generator#247): the element id IS the array index (MESSAGE_SPEC §5.1), and
	// the element's own callbacks arrive after SequenceBegin/ArrayBegin has placed
	// it, so the id is held here for the element's paths to address.
	for _, fr := range fs {
		if fr.isArr && (fr.elem == ir.KindStruct || fr.elem == ir.KindUnion || fr.elem == ir.KindArray) {
			f.line("    private int %s = 0;   // element index this array scope decodes into", ixVar(fr.loc))
		}
	}
	if limArr || limStr || limBlob {
		// Receiver-side decode limits (generator#102): configured caps on the
		// fields the schema leaves unbounded (array without count, string/blob
		// without maxlen). Exceeding a cap fails decode with LimitExceeded at
		// the count/total header, before any allocation.
		f.line("    // Receiver-side decode limits: caps on schema-unbounded")
		f.line("    // fields only; exceeding one throws SofabError.LimitExceeded.")
		if limArr {
			f.line("    private const long MaxDynArrayCount = %d;", g.limits.arrayCount)
		}
		if limStr {
			f.line("    private const long MaxDynStringLen = %d;", g.limits.stringLen)
		}
		if limBlob {
			f.line("    private const long MaxDynBlobLen = %d;", g.limits.blobLen)
		}
	}
	f.blank()

	// Unsigned: u*/bitfield scalars, bool, unsigned array elements (numeric/
	// boolean/bitfield), and native-nested unsigned inner rows.
	f.line("    public void Unsigned(int id, ulong value) {")
	g.emitArraySkipGuard(f)
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && unsignedArrayElem(fr.items.Elem) {
				f.line("            case (%s, _): %s%s.Add(%s); break;", fr.loc, fillGuard, elemAt(fr.path, fr.loc), g.arrayElemAddRHS(fr.items.Elem, fr.items.ElemRef, "value"))
			}
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindU8 || fld.Kind == ir.KindU16 || fld.Kind == ir.KindU32 || fld.Kind == ir.KindU64:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.csType(fld))
			case fld.Kind == ir.KindBitfield:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.typeName(fld.Ref.Key))
			case fld.Kind == ir.KindBool:
				f.line("            case (%s, %d): %s.%s = value != 0; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
			case fld.Kind == ir.KindArray && primArrayElem(fld.Elem) && unsignedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, primFill(fr.path+"."+csIdent(fld.Name), fld, g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			case fld.Kind == ir.KindArray && unsignedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, nativeListFill(fr.path+"."+csIdent(fld.Name), g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// Signed: i*/enum scalars, signed array elements (numeric/enum), and
	// native-nested signed inner rows.
	f.line("    public void Signed(int id, long value) {")
	g.emitArraySkipGuard(f)
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && signedArrayElem(fr.items.Elem) {
				f.line("            case (%s, _): %s%s.Add(%s); break;", fr.loc, fillGuard, elemAt(fr.path, fr.loc), g.arrayElemAddRHS(fr.items.Elem, fr.items.ElemRef, "value"))
			}
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.csType(fld))
			case fld.Kind == ir.KindEnum:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.typeName(fld.Ref.Key))
			case fld.Kind == ir.KindArray && primArrayElem(fld.Elem) && signedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, primFill(fr.path+"."+csIdent(fld.Name), fld, g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			case fld.Kind == ir.KindArray && signedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, nativeListFill(fr.path+"."+csIdent(fld.Name), g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	g.emitFloatVisit(f, fs, ir.KindFP32, "Fp32", "float")
	g.emitFloatVisit(f, fs, ir.KindFP64, "Fp64", "double")

	// Strict UTF-8 decode (MESSAGE_SPEC §8 / CORELIB_PLAN §6.4): a `string` is
	// UTF-8 and C#'s string is a Unicode type, so it is always strict. The default
	// `Encoding.UTF8` is LOSSY (replacement-fallback → U+FFFD), which §8 forbids in
	// every mode; a throwOnInvalidBytes encoding rejects invalid bytes as the
	// INVALID decode outcome. Validity is a property of the complete payload, so
	// the check runs once the full `total` bytes are present.
	f.line("    private static readonly System.Text.UTF8Encoding _strictUtf8 = new System.Text.UTF8Encoding(false, true);")
	f.line("    private static string _Utf8(byte[] b, int off, int len) {")
	f.line("        try { return _strictUtf8.GetString(b, off, len); }")
	f.line("        catch (System.Text.DecoderFallbackException) { throw new SofabException(SofabError.InvalidMessage, \"string: invalid UTF-8\"); }")
	f.line("    }")

	g.emitStringCb(f, fs, limStr)

	// Blob. Single-shot on the whole-in-one-chunk fast path (see String).
	f.line("    public void Blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	// MESSAGE_SPEC §7.1: a bounded blob whose wire byte length exceeds its schema
	// maxlen is malformed input, rejected as INVALID at the `total` header before
	// any bytes accumulate (never truncated).
	g.emitMaxlenGuard(f, fs, ir.KindBlob, "blob length")
	if limBlob {
		// generator#102: reject an over-cap unbounded blob at its `total`
		// header, before the fast path allocates or the accumulator grows.
		g.emitLenGuard(f, fs, ir.KindBlob, "MaxDynBlobLen", "blob length", g.limits.blobLen)
	}
	f.line("        byte[] _b;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _b = new byte[total];")
	f.line("            System.Array.Copy(data, chunkOffset, _b, 0, total);")
	f.line("        } else {")
	f.line("            acc ??= new List<byte>();")
	f.line("            for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("            if (acc.Count < total) return;")
	f.line("            _b = acc.ToArray();")
	f.line("            acc.Clear();")
	f.line("        }")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindBlob {
				// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
				// element is omitted on the wire, so place each value at its id and
				// grow the list, filling any gap with the element default (empty bytes).
				f.line("            case (%s, _): %swhile (%s.Count <= id) %s.Add(Array.Empty<byte>()); %s[id] = _b; break;", fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, fr.path, fr.path)
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindBlob {
				f.line("            case (%s, %d): %s.%s = _b; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// ArrayBegin: clear direct native arrays; place a fresh inner row for a
	// native-nested (array-of-array) scope (each row arrives as ArrayBegin(index),
	// and the index IS the row's position, see placeRow).
	f.line("    public void ArrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        ai = 0;")
	if dynPrim {
		f.line("        acap = count;")
	}
	g.emitArraySkipArm(f, fs)
	g.emitArrayFillArm(f, fs)
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && nativeArrayElem(fr.items.Elem) {
				// A count-less inner row of a nested array is governed by the
				// configured cap at its own count header (generator#102).
				guard := ""
				if limArr && !fr.items.HasCount {
					guard = fmt.Sprintf("if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"%s element: array count above configured limit %d\"); ",
						fr.loc, g.limits.arrayCount)
				}
				// A row whose header carries a different array kind than the inner
				// element declares is skipped whole (S7.3, generator#254): its elements
				// are already discarded by the skip counter above, and the row itself
				// must not be materialized either. Checked FIRST, so any bound below
				// only ever rejects a row that survives the kind test.
				f.line("            case (%s, _): %s%s%sbreak;", fr.loc, arrayKindGuard(fr.items.Elem), guard, g.placeRow(fr))
			}
			continue
		}
		for _, fld := range fr.fields {
			// S7.3 comes FIRST (generator#254): a header whose array kind is not the
			// one this field's declared element type maps to must be skipped exactly
			// like an unknown id -- its elements are dropped by the skip counter
			// above, and the declared field must not be touched at all, which
			// includes not being RESIZED (or cleared) from the skipped header's
			// count. Ordering matters as much as the test: the schema bound below
			// applies only to a field that survives this check, so an over-count
			// MIS-TYPED array is skipped, not a false InvalidMessage.
			kindGuard := arrayKindGuard(fld.Elem)
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject up front, never clamp or keep-all
			// (generator#100). The guard also bounds the eager `new T[count]`
			// below to the schema capacity (the count is untrusted, cf. #96).
			// A count-less array instead gets the configured generator#102 cap
			// (when set) and a lazily-grown backing array, never new T[count].
			guard := ""
			switch {
			case fld.HasCount:
				guard = fmt.Sprintf("if (count > %d) throw new SofabException(SofabError.InvalidMessage, \"%s: array count above schema capacity %d\"); ",
					fld.Count, fld.Name, fld.Count)
			case limArr && fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem):
				guard = fmt.Sprintf("if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"%s: array count above configured limit %d\"); ",
					fld.Name, g.limits.arrayCount)
			}
			if fld.Kind == ir.KindArray && primArrayElem(fld.Elem) {
				// The wire count M IS the array's length (MESSAGE_SPEC §3), so a
				// `count: N` array is allocated at exactly M -- bounded by the schema
				// capacity N through the guard above, so the untrusted count can never
				// over-allocate. A count-less array has no such bound and still grows
				// lazily from a small reservation (cf. #96).
				alloc := "new %s[count]"
				if !fld.HasCount {
					alloc = "new %s[Math.Min(count, ArrayInitCap)]"
				}
				f.line("            case (%s, %d): %s%s%s.%s = "+alloc+"; break;", fr.loc, fld.ID, kindGuard, guard, fr.path, csIdent(fld.Name), g.csArrayElemType(fld.Elem, fld.ElemRef, fld.ElemItems))
			} else if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) {
				// List<T> (boolean/enum/bitfield): cleared and appended to, with or
				// without a count -- the M elements the wire carried are the whole value.
				f.line("            case (%s, %d): %s%s%s.%s.Clear(); break;", fr.loc, fld.ID, kindGuard, guard, fr.path, csIdent(fld.Name))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// SequenceBegin / SequenceEnd. Object scope: descend into a struct/union
	// field, or into an array field's wrapper scope (clearing the list first).
	// Array scope: each element opens a sub-sequence -- struct/union appends a
	// fresh element then descends; a sequence-nested inner array appends a fresh
	// inner list then descends.
	f.line("    public void SequenceBegin(int id) {")
	f.line("        if (sp == stk.Length) System.Array.Resize(ref stk, sp * 2);")
	f.line("        stk[sp++] = cur;")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			switch {
			case fr.elem == ir.KindStruct || fr.elem == ir.KindUnion:
				// The element id IS the array index (MESSAGE_SPEC §5.1), exactly as for
				// the String/Blob element arms above: gap-fill with default elements up
				// to id, then decode INTO element id -- never append (generator#247).
				// Appending shortened the array by the size of any interior id gap and
				// decoded a REOPENED id as a second element instead of merging into the
				// first (§7.4 -- placement gives that merge for free). The over-index
				// guard rejects id >= N first, which also bounds the gap-fill.
				f.line("            case (%s, _): %swhile (%s.Count <= id) %s.Add(new %s()); %s = id; cur = %s; break;",
					fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, fr.path, g.typeName(fr.ref.Key), ixVar(fr.loc), fr.childLoc)
			case fr.elem == ir.KindArray && seqArrayElem(fr.items.Elem):
				// A wrapper ROW is placed at the index its id names too (see placeRow):
				// an interior all-default row is omitted, so appending would shift every
				// later row down by one.
				f.line("            case (%s, _): %scur = %s; break;", fr.loc, g.placeRow(fr), fr.childLoc)
			}
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				f.line("            case (%s, %d): cur = %s; break;", fr.loc, fld.ID, fr.loc+"_"+fld.Name)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s.%s.Clear(); cur = %s; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), fr.loc+"_"+fld.Name)
			}
		}
	}
	f.line("        }")
	f.line("    }")
	// The scope pop, and nothing else. There is no fill-to-N here: `count: N` is a
	// CAPACITY (MESSAGE_SPEC §3), so a wrapper array's decoded length is exactly
	// highest present id + 1 -- the last element is never elided, so that is exact.
	f.line("    public void SequenceEnd() { cur = sp > 0 ? stk[--sp] : 0; }")
	if dynPrim {
		// Lazy-growth helper: enlarge the backing array to hold index `i`,
		// doubling but never past `cap` (the wire count), so growth tracks
		// elements actually delivered and an honest array ends exactly
		// right-sized while an untrusted count allocates nothing up front.
		f.line("    // Grow a to hold index i: double, never past cap (the wire count), so")
		f.line("    // growth tracks elements actually delivered (untrusted count).")
		f.line("    private static T[] EnsureCap<T>(T[] a, int i, int cap) {")
		f.line("        if (i < a.Length) return a;")
		f.line("        long n = (long)a.Length * 2;")
		f.line("        if (n < i + 1) n = i + 1;")
		f.line("        if (n > cap) n = cap;")
		f.line("        System.Array.Resize(ref a, (int)n);")
		f.line("        return a;")
		f.line("    }")
	}
	f.line("}")
	f.blank()
}

func (g *gen) emitFloatVisit(f *cfile, fs []frame, kind ir.Kind, cb, ctype string) {
	f.line("    public void %s(int id, %s value) {", cb, ctype)
	g.emitArraySkipGuard(f)
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && fr.items.Elem == kind {
				f.line("            case (%s, _): %s%s.Add(value); break;", fr.loc, fillGuard, elemAt(fr.path, fr.loc))
			}
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == kind:
				f.line("            case (%s, %d): %s.%s = value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
			case fld.Kind == ir.KindArray && fld.Elem == kind:
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, primFill(fr.path+"."+csIdent(fld.Name), fld, "value"))
			}
		}
	}
	f.line("        }")
	f.line("    }")
}
