package rust

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// frameKind classifies a sequence container reachable from a message.
type frameKind int

const (
	fkStruct       frameKind = iota // root / struct / union / struct-array element: named fields
	fkSeqArr                        // array of string/blob: elements pushed in string()/blob()
	fkStructArr                     // array of struct/union: per-element sequence pushes a default and descends
	fkNestedNative                  // array of native array: array_begin pushes an inner Vec, elements push to the last
	fkArrArr                        // array of (string/blob/struct/nested) array: per-element sequence descends
)

// frame is one sequence container reachable from a message. loc is the _Loc
// variant; path is the Rust accessor (e.g. "self.m.somestruct.nestedstruct").
type frame struct {
	loc      string
	path     string
	kind     frameKind
	fields   []*ir.Field // fkStruct
	elemLoc  string      // fkStructArr, fkArrArr: location to descend to on a per-element sequence_begin
	elemKind ir.Kind     // fkSeqArr: string/blob element; fkNestedNative: inner native element kind
	elemRef  *ir.TypeRef // fkNestedNative: enum/bitfield backing type
	// elemDyn marks a schema-unbounded element, the target of the receiver-side
	// decode limits (generator#102): fkSeqArr — the string/blob element has no
	// maxlen; fkNestedNative — the inner native array has no count.
	elemDyn bool
	// cap is the wrapper array's schema fixed-count bound N (-1 == dynamic/no
	// count): a wrapper element id >= N is a schema-bound violation (MESSAGE_SPEC
	// §5.1/§7), rejected as INVALID (self.inv = true) before the Vec grows — which
	// also bounds an over-index heap-amplification fill. Set on the array frames
	// (fkSeqArr / fkStructArr / fkArrArr).
	cap int64
	// emax is the fkSeqArr string/blob element's schema maxlen L (-1 == no bound):
	// an element whose wire byte length exceeds L is INVALID (MESSAGE_SPEC §7.1),
	// rejected before the read, never truncated.
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

// boundOf maps a schema maxlen/count presence+value to its bound: L when
// present, -1 (unbounded) otherwise.
func boundOf(has bool, v int64) int64 {
	if has {
		return v
	}
	return -1
}

// overIndexGuard returns the reject clause for a fixed-count string/blob wrapper
// array: an element id >= N sets self.inv (surfaced as Error::InvalidMsg) and
// returns before the Vec grows (MESSAGE_SPEC §5.1/§7 — issue #142), which also
// bounds an over-index heap-amplification fill. Empty for a dynamic array
// (cap == -1). Emitted on BOTH profiles: on no_std it fires ahead of the heapless
// Vec<_, N> capacity drop (issue #126), so an over-index element is INVALID, not
// silently dropped — the fixed-capacity twin of the over-maxlen reject
// (emitMaxlenGuard) and the convergence §7.1 requires across memory models
// (issue #149 / F-0013).
func overIndexGuard(cap int64) string {
	if cap < 0 {
		return ""
	}
	return fmt.Sprintf("if id as usize >= %d { self.inv = true; return; } ", cap)
}

// overIndexMark is the sequence_begin variant of overIndexGuard for struct/union
// and nested-array wrapper elements: those append in arrival order (dense, no
// id-keyed growth, so no amplification), so the fresh element is still pushed —
// keeping the descended element location's last_mut() valid — but an id >= N
// marks the decode INVALID (§5.1/§7). std profile only; on no_std the heapless
// Vec<_, N> is capacity-bounded and drops the element (issue #126).
func (g *gen) overIndexMark(cap int64) string {
	if g.noStd || cap < 0 {
		return ""
	}
	return fmt.Sprintf("if id as usize >= %d { self.inv = true; } ", cap)
}

// isWrapperElem reports whether an array element lowers to a wrapper sequence
// (vs a native array), i.e. it needs its own decode frame.
func isWrapperElem(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindStruct, ir.KindUnion, ir.KindArray:
		return true
	}
	return false
}

// isNativeArrayElem reports whether an array element uses a native array wire
// type (numeric/fp/enum/boolean/bitfield), delivered via array_begin + scalar
// callbacks rather than a wrapper sequence.
func isNativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindEnum, ir.KindBool, ir.KindBitfield:
		return true
	}
	return false
}

// frames walks a message and returns every sequence container, root first.
func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walkFields func(loc, path string, fields []*ir.Field)
	var addArray func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64, cap int64)

	walkFields = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, kind: fkStruct, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				cl := loc + "_" + fld.Name
				walkFields(cl, path+"."+rustIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+rustIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, fld.ElemMaxHas, fld.ElemMax, capOf(fld.HasCount, fld.Count))
			}
		}
	}

	// addArray builds the frame(s) for a wrapper-sequence array whose Vec is at
	// (loc, path) and whose element is (elem, ref, items). elemMaxHas is the
	// string/blob element's maxlen presence (unused for other element kinds); cap
	// is the array's schema fixed-count bound (-1 == dynamic).
	addArray = func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64, cap int64) {
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{loc: loc, path: path, kind: fkSeqArr, elemKind: elem, elemDyn: !elemMaxHas, cap: cap, emax: boundOf(elemMaxHas, elemMax)})
		case ir.KindStruct, ir.KindUnion:
			el := loc + "_e"
			out = append(out, frame{loc: loc, path: path, kind: fkStructArr, elemLoc: el, cap: cap})
			walkFields(el, path+".last_mut().unwrap()", ref.Target.Fields)
		case ir.KindArray:
			// The element is an inner array (items). A native inner array is handled
			// by a single wrapper frame (array_begin pushes a new inner Vec, elements
			// push to the last); a wrapper inner array descends recursively with its
			// own inner count bound.
			if isNativeArrayElem(items.Elem) {
				out = append(out, frame{loc: loc, path: path, kind: fkNestedNative, elemKind: items.Elem, elemRef: items.ElemRef, elemDyn: !items.HasCount, cap: cap})
			} else {
				el := loc + "_e"
				out = append(out, frame{loc: loc, path: path, kind: fkArrArr, elemLoc: el, cap: cap})
				addArray(el, path+".last_mut().unwrap()", items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax, capOf(items.HasCount, items.Count))
			}
		}
	}

	walkFields("Root", "self.m", m.Fields)
	return out
}

// visitorUse records which optional Visitor callbacks a message actually needs.
// The corelib-rs-no-std Visitor gates fp32/string/blob (fixlen), fp64 (fp64),
// array_begin (array) and sequence_begin/end (sequence) behind Cargo features,
// so the generated impl must override only the callbacks the schema uses —
// unused ones fall back to the trait's default no-op and never reference a
// gated-out method. unsigned/signed are always present, so always emitted.
type visitorUse struct {
	fp32, fp64, str, blob, scalarArray, sequence bool
}

func visitorUseOf(fs []frame) visitorUse {
	u := visitorUse{}
	if len(fs) > 1 { // any nested struct/union or wrapper-array frame
		u.sequence = true
	}
	for _, fr := range fs {
		switch fr.kind {
		case fkSeqArr:
			u.str = u.str || fr.elemKind == ir.KindString
			u.blob = u.blob || fr.elemKind == ir.KindBlob
		case fkNestedNative:
			u.scalarArray = true
			switch fr.elemKind {
			case ir.KindFP32:
				u.fp32 = true
			case ir.KindFP64:
				u.fp64 = true
			}
		}
		for _, fld := range fr.fields {
			switch fld.Kind {
			case ir.KindFP32:
				u.fp32 = true
			case ir.KindFP64:
				u.fp64 = true
			case ir.KindString:
				u.str = true
			case ir.KindBlob:
				u.blob = true
			case ir.KindArray:
				switch fld.Elem {
				case ir.KindString:
					u.str = true
				case ir.KindBlob:
					u.blob = true
				case ir.KindFP32:
					u.fp32, u.scalarArray = true, true
				case ir.KindFP64:
					u.fp64, u.scalarArray = true, true
				case ir.KindStruct, ir.KindUnion, ir.KindArray:
					// wrapper element — handled by its own sub-frame
				default: // numeric/enum/bool/bitfield native leaf
					u.scalarArray = true
				}
			}
		}
	}
	return u
}

// emitArraySkipGuard prepends the §7.3 discard clause to unsigned()/signed()
// (generator#183). corelib-rs delivers an integer array element-by-element
// through the very callback a lone scalar uses, so a field id declared as a
// SCALAR that receives an integer ARRAY header would otherwise store the
// elements — the one wire-type contradiction the id dispatch cannot detect on
// its own. array_begin arms askip with the announced element count; here they
// are discarded one by one, which self-terminates without an array-end callback
// and works across feed chunk boundaries (askip lives in the visitor).
func (g *gen) emitArraySkipGuard(f *rfile, arrSkip bool) {
	if !arrSkip {
		return
	}
	f.line("        if self.askip > 0 { self.askip -= 1; return; } // S7.3 array at a scalar id")
}

// emitArraySkipArm arms the §7.3 discard counter in array_begin
// (generator#183). Only integer arrays are armed: their elements land in
// unsigned()/signed(), the callbacks a scalar shares. Every (scope, id) that
// genuinely declares an integer-element native array disarms it, so a legitimate
// array stores normally; everything else — a scalar-declared id, an unknown id —
// discards exactly `count` elements, after which a real scalar at the same id
// still decodes. Arrays of any other kind (fp) deliver through fp32/fp64 and
// cannot reach a scalar arm, so they are left disarmed.
func (g *gen) emitArraySkipArm(f *rfile, fs []frame, arrSkip bool) {
	if !arrSkip {
		return
	}
	f.line("        self.askip = match kind {")
	f.line("            ArrayKind::Unsigned | ArrayKind::Signed => match (self.cur, id) {")
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) && fld.Elem != ir.KindFP32 && fld.Elem != ir.KindFP64 {
					f.line("                (_Loc::%s, %d) => 0,", fr.loc, fld.ID)
				}
			}
		case fkNestedNative:
			if fr.elemKind != ir.KindFP32 && fr.elemKind != ir.KindFP64 {
				f.line("                (_Loc::%s, _) => 0,", fr.loc)
			}
		}
	}
	f.line("                _ => count,")
	f.line("            },")
	f.line("            _ => 0,")
	f.line("        };")
}

func (g *gen) emitVisitor(f *rfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	use := visitorUseOf(fs)

	// §7.3 array-vs-scalar skip (generator#183). Emitting it needs an array_begin
	// override, which the no_std profile can only have when the `array` Cargo
	// feature is on — and when it is off that corelib cannot decode an array wire
	// type at all, so no element can reach a scalar callback. corelib-rs (std)
	// compiles every wire type in unconditionally, so it always needs the guard.
	arrSkip := !g.noStd || g.hasCap("array")
	// array_begin is emitted for its own array-target work, and additionally
	// whenever the §7.3 guard needs a place to arm itself.
	emitArrayBegin := use.scalarArray || arrSkip

	// no_std string/blob accumulation buffer: reconstructs a payload split across
	// feed chunks (generator#81), matching the std profile's `acc`. Sized to the
	// message's max encoded size (a safe bound on any single payload); an
	// alloc-fallback crate uses an unbounded Vec. The std profile always carries
	// an acc (a heap Vec), so this is only conditional under no_std.
	needAcc := g.noStd && (use.str || use.blob)
	accType, accNew := "", ""
	if needAcc {
		if g.usesAlloc(g.schema) {
			accType, accNew = "alloc::vec::Vec<u8>", "alloc::vec::Vec::new()"
		} else {
			sz, _ := g.maxSize(fields)
			accType, accNew = fmt.Sprintf("heapless::Vec<u8, %d>", sz), "heapless::Vec::new()"
		}
	}

	// Wrap the decoder in a private module so _Loc / V don't clash across
	// messages in a multi-message crate.
	f.line("mod %s_dec {", strings.ToLower(name))
	f.line("    use super::*;")
	// ArrayKind is gated behind the no-std `array` feature; import it only when an
	// array_begin override is emitted (i.e. the message has a native array).
	arrayKind := ""
	if emitArrayBegin {
		arrayKind = ", ArrayKind"
	}
	f.line("    use sofab::{IStream, Visitor, Id, Unsigned, Signed%s};", arrayKind)
	f.blank()
	// Bounded decode stack for the no_std profile: nesting depth never exceeds the
	// number of reachable frames, so that is a safe fixed capacity (min 4).
	stackCap := len(fs)
	if stackCap < 4 {
		stackCap = 4
	}
	// The sticky lim flag exists only when a receiver-side decode limit is
	// active (generator#102) — std profile only, so the no_std inits never carry it.
	limInit := ""
	if g.limits.any() {
		limInit = " lim: false,"
	}
	askipInit := ""
	if arrSkip {
		askipInit = ", askip: 0"
	}
	vInit := fmt.Sprintf("let mut v = V { m: &mut m, stack: Vec::new(), cur: _Loc::Root, acc: Vec::new(), err: false, inv: false,%s ai: 0%s };", limInit, askipInit)
	if g.noStd {
		if needAcc {
			vInit = fmt.Sprintf("let mut v = V { m: &mut m, stack: heapless::Vec::new(), cur: _Loc::Root, acc: %s, err: false, inv: false, ai: 0%s };", accNew, askipInit)
		} else {
			vInit = fmt.Sprintf("let mut v = V { m: &mut m, stack: heapless::Vec::new(), cur: _Loc::Root, err: false, inv: false, ai: 0%s };", askipInit)
		}
	}
	// Infallible, best-effort decode: kept for back-compat. It discards feed's
	// Result and returns whatever was filled, so it can never reject malformed
	// input — prefer try_decode when the accept/reject verdict matters.
	f.line("    pub fn decode(data: &[u8]) -> %s {", name)
	f.line("        let mut m = %s::default();", name)
	f.line("        {")
	f.line("            %s", vInit)
	f.line("            let mut is = IStream::new();")
	f.line("            let _ = is.feed(data, &mut v);")
	f.line("        }")
	f.line("        m")
	f.line("    }")
	f.blank()
	// Fallible decode: surfaces the corelib's accept/reject decision. IStream::feed
	// detects malformed input and returns Err, but the infallible decode above drops
	// it, so the public Rust API could otherwise never reject (generator#79). Emitted
	// for both the std and no_std profiles.
	f.line("    pub fn try_decode(data: &[u8]) -> Result<%s, sofab::Error> {", name)
	f.line("        let mut m = %s::default();", name)
	f.line("        let overflow;")
	f.line("        let invalid;")
	if g.limits.any() {
		f.line("        let limited;")
	}
	f.line("        {")
	f.line("            %s", vInit)
	f.line("            let mut is = IStream::new();")
	f.line("            is.feed(data, &mut v)?;")
	f.line("            overflow = v.err;")
	f.line("            invalid = v.inv;")
	if g.limits.any() {
		f.line("            limited = v.lim;")
	}
	f.line("        }")
	f.line("        // A scalar array carried more elements than its schema `count`.")
	f.line("        // An element count above the schema capacity is invalid and is rejected, never clamped.")
	f.line("        if invalid { return Err(sofab::Error::InvalidMsg); }")
	if g.limits.any() {
		f.line("        // An unbounded field exceeded a configured receiver-side decode")
		f.line("        // limit: reject, never clamp.")
		f.line("        if limited { return Err(sofab::Error::LimitExceeded); }")
	}
	f.line("        // A fixed-capacity field overflowed during the fill:")
	f.line("        // report it rather than return a silently-truncated value.")
	f.line("        if overflow { return Err(sofab::Error::BufferFull); }")
	f.line("        Ok(m)")
	f.line("    }")
	f.blank()

	// _Loc enum
	f.line("#[derive(Clone, Copy, PartialEq)]")
	f.line("enum _Loc {")
	for _, fr := range fs {
		f.line("    %s,", fr.loc)
	}
	f.line("}")
	f.blank()

	f.line("struct V<'a> {")
	f.line("    m: &'a mut %s,", name)
	if g.noStd {
		// Heap-free: bounded location stack. `acc` reassembles a string/blob split
		// across feed chunks (generator#81); omitted when the message has neither.
		f.line("    stack: heapless::Vec<_Loc, %d>,", stackCap)
		f.line("    cur: _Loc,")
		if needAcc {
			f.line("    acc: %s,", accType)
		}
	} else {
		f.line("    stack: Vec<_Loc>,")
		f.line("    cur: _Loc,")
		f.line("    acc: Vec<u8>,")
	}
	// Sticky decode-failure flag: a no_std fixed-capacity fill that overflows
	// (heapless String/Vec push past capacity) sets this so try_decode can report
	// it instead of silently truncating (generator#82). The std profile has no
	// fixed capacity, so it never sets it.
	f.line("    err: bool,")
	// Sticky malformed-message flag: a native array delivered more elements than
	// its schema `count` capacity (generator#100). MESSAGE_SPEC 3+7 make this
	// INVALID, so try_decode must reject — clamping is non-conformant.
	f.line("    inv: bool,")
	// Sticky limit-exceeded flag: an unbounded field's declared wire count/length
	// exceeded a configured max_dyn_* receiver cap (generator#102); try_decode
	// rejects with LimitExceeded. Emitted only when a limit is active (std profile).
	if g.limits.any() {
		f.line("    lim: bool,")
	}
	f.line("    ai: usize, // index into the fixed native array currently being filled")
	// §7.3 array-vs-scalar skip counter (generator#183): an integer array whose id
	// is declared as a SCALAR is a wire-type contradiction and must be skipped like
	// an unknown id. corelib-rs delivers array elements through the same
	// unsigned()/signed() callbacks a lone scalar uses, so the id dispatch alone
	// cannot tell them apart; array_begin arms this with the announced element
	// count and the callbacks discard exactly that many.
	if arrSkip {
		f.line("    askip: usize, // elements left to discard from a S7.3-contradictory array")
	}
	f.line("}")
	f.blank()

	// The flat visitor assigns into deprecated fields (self.m.<path>) directly, so
	// suppress the deprecated lint over the whole impl when any reachable field is
	// deprecated; keeps the generated crate warning-clean.
	for _, fr := range fs {
		if fieldsHaveDeprecated(fr.fields) {
			f.line("#[allow(deprecated)]")
			break
		}
	}
	f.line("impl<'a> Visitor for V<'a> {")

	// unsigned: u*/bitfield scalars, bool, and unsigned/bool/bitfield array elements
	f.line("    fn unsigned(&mut self, id: Id, value: Unsigned) {")
	g.emitArraySkipGuard(f, arrSkip)
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindU8 || fld.Kind == ir.KindU16 || fld.Kind == ir.KindU32 || fld.Kind == ir.KindU64 || fld.Kind == ir.KindBitfield:
					f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), g.rustType(fld))
				case fld.Kind == ir.KindBool:
					f.line("            (_Loc::%s, %d) => %s.%s = value != 0,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
				case fld.Kind == ir.KindArray && isUnsignedElem(fld.Elem):
					g.emitNativeArrayStore(f, fr, fld, fmt.Sprintf("value as %s", numRustType(fld.Elem)))
				case fld.Kind == ir.KindArray && fld.Elem == ir.KindBool:
					g.emitNativeArrayStore(f, fr, fld, "value != 0")
				case fld.Kind == ir.KindArray && fld.Elem == ir.KindBitfield:
					g.emitNativeArrayStore(f, fr, fld, fmt.Sprintf("value as %s", bitfieldBacking(fld.ElemRef.Target)))
				}
			}
		case fkNestedNative:
			tgt := fr.path + ".last_mut().unwrap()"
			var store string
			switch {
			case isUnsignedElem(fr.elemKind):
				store = g.pushExpr(tgt, "value as "+numRustType(fr.elemKind))
			case fr.elemKind == ir.KindBool:
				store = g.pushExpr(tgt, "value != 0")
			case fr.elemKind == ir.KindBitfield:
				store = g.pushExpr(tgt, "value as "+bitfieldBacking(fr.elemRef.Target))
			default:
				continue
			}
			if g.limits.arrayHas && fr.elemDyn {
				store = g.limArrayStore(store)
			}
			f.line("            (_Loc::%s, _) => %s,", fr.loc, store)
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	// signed: i*/enum scalars + signed/enum array elements
	f.line("    fn signed(&mut self, id: Id, value: Signed) {")
	g.emitArraySkipGuard(f, arrSkip)
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64:
					f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), g.rustType(fld))
				case fld.Kind == ir.KindEnum:
					f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), enumBacking(fld.Ref.Target))
				case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
					g.emitNativeArrayStore(f, fr, fld, fmt.Sprintf("value as %s", numRustType(fld.Elem)))
				case fld.Kind == ir.KindArray && fld.Elem == ir.KindEnum:
					g.emitNativeArrayStore(f, fr, fld, fmt.Sprintf("value as %s", enumBacking(fld.ElemRef.Target)))
				}
			}
		case fkNestedNative:
			tgt := fr.path + ".last_mut().unwrap()"
			var store string
			switch {
			case isSignedElem(fr.elemKind):
				store = g.pushExpr(tgt, "value as "+numRustType(fr.elemKind))
			case fr.elemKind == ir.KindEnum:
				store = g.pushExpr(tgt, "value as "+enumBacking(fr.elemRef.Target))
			default:
				continue
			}
			if g.limits.arrayHas && fr.elemDyn {
				store = g.limArrayStore(store)
			}
			f.line("            (_Loc::%s, _) => %s,", fr.loc, store)
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	if use.fp32 {
		g.emitFloatVisit(f, fs, ir.KindFP32, "fp32", "f32")
	}
	if use.fp64 {
		g.emitFloatVisit(f, fs, ir.KindFP64, "fp64", "f64")
	}

	if use.str {
		// string: scalar strings + string-array elements
		f.line("    fn string(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {")
		g.emitMaxlenGuard(f, fs, ir.KindString)
		if g.limits.stringHas {
			g.emitLimitGuard(f, fs, ir.KindString, "MAX_DYN_STRING_LEN")
		}
		if g.noStd {
			// Accumulate across chunks so a streaming (multi-feed) string is
			// reconstructed like the std profile (generator#81), bounded by `acc`'s
			// capacity. The single-shot fast path (whole payload in one chunk) reads
			// the slice directly and skips the acc copy. offset==0 starts a fresh
			// payload; acc is built up only while the payload is still incomplete.
			f.line("        if offset == 0 { self.acc.clear(); }")
			f.line("        let _s = if offset == 0 && chunk.len() >= total {")
			f.line("            match core::str::from_utf8(&chunk[..total]) { Ok(_v) => _v, Err(_) => { self.inv = true; \"\" } }")
			f.line("        } else {")
			f.line("            let _ = self.acc.extend_from_slice(chunk);")
			f.line("            if self.acc.len() < total { return; }")
			f.line("            match core::str::from_utf8(&self.acc[..total]) { Ok(_v) => _v, Err(_) => { self.inv = true; \"\" } }")
			f.line("        };")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindString {
					f.line("            (_Loc::%s, _) => { %s%s if let Some(_e) = %s.get_mut(id as usize) { let _ = _e.push_str(_s); if _e.len() != _s.len() { self.err = true; } } }", fr.loc, overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
				}
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindString {
						f.line("            (_Loc::%s, %d) => { %s.%s.clear(); let _ = %s.%s.push_str(_s); if %s.%s.len() != _s.len() { self.err = true; } }", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), fr.path, rustIdent(fld.Name), fr.path, rustIdent(fld.Name))
					}
				}
			}
			f.line("            _ => {}")
			f.line("        }")
			f.line("    }")
		} else {
			f.line("        // Single-shot: whole payload in one chunk -> build straight from the")
			f.line("        // slice, skipping the `acc` accumulate + second copy.")
			f.line("        // MESSAGE_SPEC 8 / CORELIB_PLAN 6.4: a string is UTF-8 and Rust's")
			f.line("        // String is a Unicode type, so it is always strict. Invalid UTF-8 is")
			f.line("        // the INVALID decode outcome (self.inv -> Error::InvalidMsg), never a")
			f.line("        // lossy U+FFFD and never empty; the two Rust profiles agree (subsumes #80).")
			f.line("        let _s = if offset == 0 && chunk.len() >= total {")
			f.line("            match core::str::from_utf8(&chunk[..total]) { Ok(_v) => _v.to_owned(), Err(_) => { self.inv = true; String::new() } }")
			f.line("        } else {")
			f.line("            self.acc.extend_from_slice(chunk);")
			f.line("            if self.acc.len() < total { return; }")
			f.line("            let s = match core::str::from_utf8(&self.acc) { Ok(_v) => _v.to_owned(), Err(_) => { self.inv = true; String::new() } };")
			f.line("            self.acc.clear();")
			f.line("            s")
			f.line("        };")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindString {
					f.line("            (_Loc::%s, _) => { %s%s %s[id as usize] = _s; }", fr.loc, overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
				}
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindString {
						f.line("            (_Loc::%s, %d) => %s.%s = _s,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
					}
				}
			}
			f.line("            _ => {}")
			f.line("        }")
			f.line("    }")
		}
	}

	if use.blob {
		// blob: scalar blobs + blob-array elements
		f.line("    fn blob(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {")
		g.emitMaxlenGuard(f, fs, ir.KindBlob)
		if g.limits.blobHas {
			g.emitLimitGuard(f, fs, ir.KindBlob, "MAX_DYN_BLOB_LEN")
		}
		if g.noStd {
			// Accumulate across chunks like the string visitor / std profile
			// (generator#81); single-shot fast path reads the slice directly.
			f.line("        if offset == 0 { self.acc.clear(); }")
			f.line("        let _b: &[u8] = if offset == 0 && chunk.len() >= total {")
			f.line("            &chunk[..total]")
			f.line("        } else {")
			f.line("            let _ = self.acc.extend_from_slice(chunk);")
			f.line("            if self.acc.len() < total { return; }")
			f.line("            &self.acc[..total]")
			f.line("        };")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindBlob {
					f.line("            (_Loc::%s, _) => { %s%s if let Some(_e) = %s.get_mut(id as usize) { let _ = _e.extend_from_slice(_b); if _e.len() != total { self.err = true; } } }", fr.loc, overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
				}
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindBlob {
						f.line("            (_Loc::%s, %d) => { %s.%s.clear(); let _ = %s.%s.extend_from_slice(_b); if %s.%s.len() != total { self.err = true; } }", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), fr.path, rustIdent(fld.Name), fr.path, rustIdent(fld.Name))
					}
				}
			}
			f.line("            _ => {}")
			f.line("        }")
			f.line("    }")
		} else {
			f.line("        let _b = if offset == 0 && chunk.len() >= total {")
			f.line("            chunk[..total].to_vec()")
			f.line("        } else {")
			f.line("            self.acc.extend_from_slice(chunk);")
			f.line("            if self.acc.len() < total { return; }")
			f.line("            let b = self.acc.clone();")
			f.line("            self.acc.clear();")
			f.line("            b")
			f.line("        };")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindBlob {
					f.line("            (_Loc::%s, _) => { %s%s %s[id as usize] = _b; }", fr.loc, overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
				}
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindBlob {
						f.line("            (_Loc::%s, %d) => %s.%s = _b,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
					}
				}
			}
			f.line("            _ => {}")
			f.line("        }")
			f.line("    }")
		}
	}

	if emitArrayBegin {
		// array_begin clears a native-array target (scalar array field) or starts a
		// fresh inner Vec (nested native array).
		// Reset the fixed-array fill index for every array. Fixed `[T; N]` fields are
		// pre-allocated in the struct default, so they need no per-begin action; a
		// dynamic native array clears its Vec; a nested-native scope pushes a fresh
		// inner Vec.
		f.line("    fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize) {")
		f.line("        self.ai = 0;")
		g.emitArraySkipArm(f, fs, arrSkip)
		f.line("        match (self.cur, id) {")
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
						if _, _, ok := g.fixedNativeArray(fld); ok {
							// A fixed `[T; N]` is pre-allocated in the struct default, so
							// the M wire elements store straight into it and no clear is
							// needed to make room. But the encoder trims the trailing
							// default run (MESSAGE_SPEC S3), so positions [M, N) are never
							// stored and must read back as the ELEMENT default (zero). A
							// non-zero schema `default:` would otherwise leak through that
							// untouched tail, so wipe it to the zero image first. Reaching
							// array_begin means the field is PRESENT on the wire, so this
							// never disturbs the sparse-omission contract: an ABSENT field
							// keeps its full schema default.
							if zero, need := g.rustFixedArrayNeedsReset(fld); need {
								f.line("            (_Loc::%s, %d) => %s.%s = %s,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), zero)
							}
							continue
						}
						// Unbounded array under an active receiver cap (generator#102):
						// reject an over-cap wire count at the header, before any
						// elements accumulate.
						if g.limits.arrayHas && !fld.HasCount {
							f.line("            (_Loc::%s, %d) => { if count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } %s.%s.clear() },", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
							continue
						}
						f.line("            (_Loc::%s, %d) => %s.%s.clear(),", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
					}
				}
			case fkNestedNative:
				if g.limits.arrayHas && fr.elemDyn {
					f.line("            (_Loc::%s, _) => { if count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } %s },", fr.loc, g.pushExpr(fr.path, g.innerNew()))
					continue
				}
				f.line("            (_Loc::%s, _) => %s,", fr.loc, g.pushExpr(fr.path, g.innerNew()))
			}
		}
		f.line("            _ => {}")
		f.line("        }")
		f.line("    }")
	}

	if use.sequence {
		// sequence_begin: push current, descend. String/blob/composite array fields
		// clear their Vec on entry; struct/nested-array wrapper frames push a fresh
		// element and descend on each per-element sequence_begin.
		f.line("    fn sequence_begin(&mut self, id: Id) {")
		f.line("        %s", g.pushStmt("self.stack", "self.cur"))
		f.line("        self.cur = match (self.cur, id) {")
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					switch {
					case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
						f.line("            (_Loc::%s, %d) => _Loc::%s,", fr.loc, fld.ID, fr.loc+"_"+fld.Name)
					case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
						f.line("            (_Loc::%s, %d) => { %s.%s.clear(); _Loc::%s },", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), fr.loc+"_"+fld.Name)
					}
				}
			case fkStructArr:
				f.line("            (_Loc::%s, _) => { %s %s_Loc::%s },", fr.loc, g.pushStmt(fr.path, "Default::default()"), g.overIndexMark(fr.cap), fr.elemLoc)
			case fkArrArr:
				f.line("            (_Loc::%s, _) => { %s %s_Loc::%s },", fr.loc, g.pushStmt(fr.path, g.innerNew()), g.overIndexMark(fr.cap), fr.elemLoc)
			}
		}
		f.line("            _ => self.cur,")
		f.line("        };")
		f.line("    }")
		f.line("    fn sequence_end(&mut self) {")
		f.line("        self.cur = self.stack.pop().unwrap_or(_Loc::Root);")
		f.line("    }")
	}

	f.line("}") // impl Visitor
	f.line("}") // mod <name>_dec
	f.blank()
}

// emitLimitGuard emits the receiver-side decode-limit pre-check (generator#102)
// at the top of the string/blob callback, before any accumulation: every
// schema-unbounded field of that kind (no maxlen — scalar fields and wrapper-
// sequence elements alike) gets a (loc, id) arm that rejects a declared `total`
// above the configured cap by setting the sticky lim flag and bailing out.
// Placing the check ahead of the single-shot/chunked split covers both paths,
// and on a chunked payload every later chunk re-hits the guard, so nothing is
// ever buffered. Bounded fields get no arm: their schema maxlen governs them.
// Emitted only when the limit is active, i.e. never under no_std.
func (g *gen) emitLimitGuard(f *rfile, fs []frame, kind ir.Kind, constName string) {
	var arms []string
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind && fr.elemDyn {
			arms = append(arms, fmt.Sprintf("            (_Loc::%s, _) => if total > %s { self.lim = true; return; },", fr.loc, constName))
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind && !fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("            (_Loc::%s, %d) => if total > %s { self.lim = true; return; },", fr.loc, fld.ID, constName))
			}
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("        // Unbounded fields under an active receiver cap:")
	f.line("        // reject an over-cap declared total before any bytes accumulate.")
	f.line("        match (self.cur, id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("            _ => {}")
	f.line("        }")
}

// emitMaxlenGuard emits the schema-maxlen reject (MESSAGE_SPEC §7.1) at the top
// of the string/blob callback, the bounded-field twin of emitLimitGuard: every
// field of that kind with a schema `maxlen` (scalar fields and wrapper-sequence
// elements alike) gets a (loc, id) arm that rejects a declared `total` above its
// own maxlen with the sticky `inv` flag (Error::InvalidMsg) — before any bytes
// accumulate and never truncated. Emitted on BOTH profiles: on no_std it also
// supersedes the heapless BufferFull path so the outcome is INVALID, not a
// capacity error.
func (g *gen) emitMaxlenGuard(f *rfile, fs []frame, kind ir.Kind) {
	var arms []string
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind && fr.emax >= 0 {
			arms = append(arms, fmt.Sprintf("            (_Loc::%s, _) => if total > %d { self.inv = true; return; },", fr.loc, fr.emax))
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind && fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("            (_Loc::%s, %d) => if total > %d { self.inv = true; return; },", fr.loc, fld.ID, fld.Maxlen))
			}
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("        // Bounded fields: a wire byte length above the schema maxlen is")
	f.line("        // malformed input, INVALID before any bytes accumulate (never truncated).")
	f.line("        match (self.cur, id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("            _ => {}")
	f.line("        }")
}

// emitNativeArrayStore emits one match arm for a direct native array element: a
// bounds-checked indexed store `if self.ai < N { x[self.ai] = rhs; self.ai += 1; }`
// for a fixed `[T; N]` array, or a `.push(rhs)` for a dynamic (count-less) `Vec`
// array. The bound keeps an over-count element from the indexed write — which
// would panic, a crash/DoS on untrusted data (generator#78) — and flags the
// message as malformed: a wire element count above the schema's `count` is
// INVALID per MESSAGE_SPEC 3+7 and must reject, not clamp (generator#100).
func (g *gen) emitNativeArrayStore(f *rfile, fr frame, fld *ir.Field, rhs string) {
	if _, n, ok := g.fixedNativeArray(fld); ok {
		f.line("            (_Loc::%s, %d) => { if self.ai < %d { %s.%s[self.ai] = %s; self.ai += 1; } else { self.inv = true; } }", fr.loc, fld.ID, n, fr.path, rustIdent(fld.Name), rhs)
		return
	}
	store := g.pushExpr(fr.path+"."+rustIdent(fld.Name), rhs)
	if g.limits.arrayHas && !fld.HasCount {
		store = g.limArrayStore(store)
	}
	f.line("            (_Loc::%s, %d) => %s,", fr.loc, fld.ID, store)
}

// limArrayStore wraps an unbounded-array element store so it is dropped once
// the sticky lim flag is set (generator#102): the over-cap array was rejected
// at its count header, so its elements must not accumulate either. For a
// nested-native array this also keeps the last_mut().unwrap() safe after the
// tripped array_begin skipped its inner-Vec push.
func (g *gen) limArrayStore(expr string) string {
	return fmt.Sprintf("{ if !self.lim { %s; } }", expr)
}

func (g *gen) emitFloatVisit(f *rfile, fs []frame, kind ir.Kind, cb, rtype string) {
	f.line("    fn %s(&mut self, id: Id, value: %s) {", cb, rtype)
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		if fr.kind == fkNestedNative && fr.elemKind == kind {
			store := g.pushExpr(fr.path+".last_mut().unwrap()", "value")
			if g.limits.arrayHas && fr.elemDyn {
				store = g.limArrayStore(store)
			}
			f.line("            (_Loc::%s, _) => %s,", fr.loc, store)
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == kind:
				f.line("            (_Loc::%s, %d) => %s.%s = value,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
			case fld.Kind == ir.KindArray && fld.Elem == kind:
				g.emitNativeArrayStore(f, fr, fld, "value")
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")
}

// pushExpr / pushStmt / innerNew handle the heapless-vs-heap container push: under
// no_std push returns a Result that must be consumed (let _ = ...) and inner
// containers are heapless::Vec; the std path uses a bare Vec push.
func (g *gen) pushExpr(target, val string) string {
	if g.noStd {
		return fmt.Sprintf("{ let _ = %s.push(%s); }", target, val)
	}
	return fmt.Sprintf("%s.push(%s)", target, val)
}

func (g *gen) pushStmt(target, val string) string {
	if g.noStd {
		return fmt.Sprintf("let _ = %s.push(%s);", target, val)
	}
	return fmt.Sprintf("%s.push(%s);", target, val)
}

func (g *gen) innerNew() string {
	if g.noStd {
		return "heapless::Vec::new()"
	}
	return "Vec::new()"
}

// seqElemGrow emits the id-indexed growth prefix for a wrapper-sequence string/
// blob element collector: grow the container to id+1, filling the gap with the
// element default (empty), so a decoded element lands at index = its wire id and
// omitted default elements leave the right gaps (MESSAGE_SPEC S2). Under no_std the
// container is a fixed-capacity heapless::Vec (or an alloc fallback under
// allow_dynamic): push may be a no-op when full, so the loop breaks when the length
// stops growing to avoid spinning on an out-of-capacity id; get_mut then no-ops.
func (g *gen) seqElemGrow(path string) string {
	if g.noStd {
		return fmt.Sprintf("while %s.len() <= id as usize { let _n = %s.len(); let _ = %s.push(Default::default()); if %s.len() == _n { break; } }", path, path, path, path)
	}
	return fmt.Sprintf("while %s.len() <= id as usize { %s.push(Default::default()); }", path, path)
}

func isUnsignedElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
}
func isSignedElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
}

var _ = strings.TrimSpace
var _ = fmt.Sprintf
