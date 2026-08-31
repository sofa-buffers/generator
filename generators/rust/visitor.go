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
	fkStructArr                     // array of struct/union: per-element sequence descends into the element the id names
	fkNestedNative                  // array of native array: array_begin opens the row the id names, elements push into it
	fkArrArr                        // array of (string/blob/struct/nested) array: per-element sequence descends into the row the id names
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
	// also bounds an over-index heap-amplification fill. Set on every array frame
	// (fkSeqArr / fkStructArr / fkNestedNative / fkArrArr).
	cap int64
	// ixVar is the visitor-state field holding the CURRENT element index of a
	// fkStructArr / fkArrArr / fkNestedNative frame (generator#247). The element id
	// IS the array index (§5.1), and the flat visitor has no child-visitor object
	// to hold that index for it, so the element's own location addresses it as
	// `<path>[self.<ixVar>]`. One field per frame rather than a shared stack: a
	// location can only be active once at a time, so its index needs no depth
	// arithmetic. It is part of the persistent visitor state because an element can
	// straddle a chunk boundary in the incremental decoder.
	ixVar string
	// emax is the fkSeqArr string/blob element's schema maxlen L (-1 == no bound):
	// an element whose wire byte length exceeds L is INVALID (MESSAGE_SPEC §7.1),
	// rejected before the read, never truncated.
	emax int64
	// ecap is the fkNestedNative ROW's own schema count M (-1 == dynamic/no
	// count) — the bound on the elements INSIDE one row, as distinct from cap,
	// which bounds the rows. A row whose wire element count exceeds M is INVALID
	// (MESSAGE_SPEC §3+§7), rejected at the count header exactly like a top-level
	// native array (generator#216 / F-0032), which is what keeps the row's fill
	// inside its declared capacity on both profiles.
	ecap int64
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

// overIndexGuard returns the reject clause for a wrapper array's element id,
// emitted ahead of the grow it bounds.
//
// A wrapper array carries no count HEADER: its elements are keyed by an
// unbounded varint index and the destination grows to id + 1, so the index IS
// the array's length (MESSAGE_SPEC §5.1 — two elements at id 0 and id 16383 are
// a 16384-slot Vec). A single over-index element is therefore an amplification
// vector by itself, and it is the INDEX that has to be bounded: capping how many
// elements arrived would not bound the allocation, because a sparse array
// allocates by its highest id.
//
// Which bound applies depends on whether the schema counts the array, and the
// two differ only in that and in what the failure is called (ARCHITECTURE §9.5):
//
//   - `count: N` -> id >= N sets self.inv, surfaced as Error::InvalidMsg (issue
//     #142). The bytes contradict the schema both peers agreed on. Emitted on
//     BOTH profiles: on no_std it fires ahead of the heapless Vec<_, N> capacity
//     drop (issue #126), so an over-index element is INVALID rather than silently
//     dropped — the convergence §7.1 requires across memory models (#149/F-0013).
//   - no count -> id >= MAX_DYN_ARRAY_COUNT sets self.lim, surfaced as
//     Error::LimitExceeded (issue #387). The bytes are well formed and the same
//     message decodes under a looser cap, so folding this into INVALID is
//     forbidden by CORELIB_PLAN §6.2.1. std only: corelib-rs-no-std has no
//     LimitExceeded, and checkBounded has already refused an unbounded field
//     there, so the case cannot arise — g.limits is left zero for that profile
//     and this returns "" for it.
func (g *gen) overIndexGuard(cap int64) string {
	if cap >= 0 {
		return fmt.Sprintf("if id as usize >= %d { self.inv = true; return; } ", cap)
	}
	if !g.limits.arrayHas {
		return ""
	}
	return "if id as usize >= MAX_DYN_ARRAY_COUNT { self.lim = true; return; } "
}

// rowReject builds one reject clause for a NATIVE ROW frame (fkNestedNative) in
// array_begin: it sets the sticky verdict flag, DISARMS the fill counter and
// returns before the row is opened or grown.
//
// The disarm is what makes the clause a reject. array_begin arms afill with the
// announced element count BEFORE this arm runs (emitArrayFillArm), and the row's
// elements arrive afterwards through the plain unsigned()/signed()/fp callbacks,
// which store while afill > 0 into whatever row the index slot still names. A
// clause that only returned would therefore reject the header and then let the
// very elements it rejected stream into the previously opened row, unbounded —
// the fill the reject exists to stop. Zeroing afill routes them into the
// fillGuard's `afill == 0` skip instead, so they are discarded like a bare
// scalar at an array id.
func rowReject(cond, flag string) string {
	return fmt.Sprintf("if %s { self.%s = true; self.afill = 0; return; } ", cond, flag)
}

// rowGuards returns the reject clauses that front a native row's array_begin arm,
// in the order §5.2 needs them decided: the ROW id against the outer array's
// count (cap) — or, for a count-less outer array, against the receiver's
// configured array limit — then the row's own element count against the row's
// count (ecap), or again against that limit. Both bounds are decided at the header, before the row is opened
// or filled, so INVALID dominates a truncated tail (generator#216).
func (g *gen) rowGuards(fr frame) string {
	var out string
	switch {
	case fr.cap >= 0:
		out += rowReject(fmt.Sprintf("id as usize >= %d", fr.cap), "inv")
	case g.limits.arrayHas:
		// A row of a count-less matrix: its ID is the outer array's length, so
		// the receiver cap binds it exactly as it binds a leaf wrapper element
		// (issue #387, see overIndexGuard).
		out += rowReject("id as usize >= MAX_DYN_ARRAY_COUNT", "lim")
	}
	switch {
	case fr.ecap >= 0:
		out += rowReject(fmt.Sprintf("count > %d", fr.ecap), "inv")
	case g.limits.arrayHas:
		out += rowReject("count > MAX_DYN_ARRAY_COUNT", "lim")
	}
	return out
}

// ixVarsOf lists the element-index state slots the message's frames need
// (frame.ixVar), in the order frames() handed them out.
func ixVarsOf(fs []frame) []string {
	var out []string
	for _, fr := range fs {
		if fr.ixVar != "" {
			out = append(out, fr.ixVar)
		}
	}
	return out
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
	nix := 0 // running number of element-index slots handed out (see frame.ixVar)
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
			// The element id IS the array index (§5.1), so the element location
			// addresses out[id] through the frame's index slot -- it is NOT the last
			// element pushed. Appending would shorten the array by the size of any
			// interior id gap, and would decode a REOPENED id as a second element
			// instead of merging into the first (§7.4). generator#247.
			ix := fmt.Sprintf("_ix%d", nix)
			nix++
			out = append(out, frame{loc: loc, path: path, kind: fkStructArr, elemLoc: el, cap: cap, ixVar: ix})
			walkFields(el, fmt.Sprintf("%s[self.%s]", path, ix), ref.Target.Fields)
		case ir.KindArray:
			// The element is an inner array (items). A native inner row is handled by
			// a single wrapper frame (array_begin opens the row the id names, elements
			// push into it); a wrapper inner row descends recursively with its own
			// inner count bound.
			// Both row collectors place the row at out[id] (§5.1) through their own
			// index slot, exactly like the struct-element frame above: an interior
			// row equal to the element default is omitted (§2), so appending would
			// close that gap and shift every later row down by one.
			ix := fmt.Sprintf("_ix%d", nix)
			nix++
			if isNativeArrayElem(items.Elem) {
				out = append(out, frame{loc: loc, path: path, kind: fkNestedNative, elemKind: items.Elem, elemRef: items.ElemRef, elemDyn: !items.HasCount, cap: cap, ixVar: ix, ecap: boundOf(items.HasCount, items.Count)})
			} else {
				el := loc + "_e"
				out = append(out, frame{loc: loc, path: path, kind: fkArrArr, elemLoc: el, cap: cap, ixVar: ix})
				addArray(el, fmt.Sprintf("%s[self.%s]", path, ix), items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax, capOf(items.HasCount, items.Count))
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
// (generator#183) and fp32()/fp64() (generator#193). corelib-rs delivers an
// array element-by-element through the very callback a lone scalar uses, so a
// field id declared as a SCALAR that receives an ARRAY header (integer arrays via
// unsigned/signed, fp arrays via fp32/fp64) would otherwise store the elements —
// the one wire-type contradiction the id dispatch cannot detect on
// its own. array_begin arms askip with the announced element count; here they
// are discarded one by one, which self-terminates without an array-end callback
// and works across feed chunk boundaries (askip lives in the visitor).
func (g *gen) emitArraySkipGuard(f *rfile, arrSkip bool) {
	if !arrSkip {
		return
	}
	f.line("        if self.askip > 0 { self.askip -= 1; return; } // array delivered at a scalar id")
}

// wantUnsignedArrayElem / wantSignedArrayElem / wantFP32Elem / wantFP64Elem are
// the four element-kind predicates that key an array_begin arm to a wire
// ArrayKind. They partition the native array element kinds exactly as the
// corelib's ArrayKind partitions the wire — one predicate per kind, so the §7.3
// kind check is decided by the match itself. (The first two live beside
// arrayKindPat, which spells the same partition as a pattern.)
func wantFP32Elem(k ir.Kind) bool { return k == ir.KindFP32 }
func wantFP64Elem(k ir.Kind) bool { return k == ir.KindFP64 }

// anyArrayElem reports whether the message declares at least one native array
// whose element kind satisfies want — i.e. whether an arm keyed to that kind
// would carry any field at all.
//
// This is also the emission condition for naming ArrayKind::Fp32 /
// ArrayKind::Fp64 in generated code. Under the no_std profile both variants are
// `#[cfg(feature = "fixlen")]`, so naming one in a crate built without `fixlen`
// would not compile. A schema that declares an fp array necessarily provisions
// `fixlen` (capabilities(), which today provisions the full wire-type set
// unconditionally), and a schema that declares none never names the variant —
// the catch-all arm covers that wire kind instead. That keeps the emission
// correct even if the provisioned feature set is ever narrowed again.
func anyArrayElem(fs []frame, want func(ir.Kind) bool) bool {
	for _, fr := range fs {
		switch fr.kind {
		case fkStruct:
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && want(fld.Elem) {
					return true
				}
			}
		case fkNestedNative:
			if want(fr.elemKind) {
				return true
			}
		}
	}
	return false
}

// arrayKindPat is the ArrayKind pattern that fronts a native array field's arm
// in array_begin's target match. A fixlen array is keyed to its own declared
// element subtype, so a header of the *other* fp subtype never reaches the
// field's arm; an integer array stays kind-agnostic (`_`), see the target
// match's own comment.
func arrayKindPat(k ir.Kind) string {
	switch {
	case k == ir.KindFP32:
		return "ArrayKind::Fp32"
	case k == ir.KindFP64:
		return "ArrayKind::Fp64"
	case wantSignedArrayElem(k):
		return "ArrayKind::Signed"
	default:
		return "ArrayKind::Unsigned"
	}
}

// wantUnsignedArrayElem / wantSignedArrayElem split the integer element kinds by
// the wire ARRAY KIND they map to (§1): signed integers and enum travel as
// ArraySigned, unsigned integers, bool and bitfield as ArrayUnsigned.
//
// Keeping the two apart is what makes the §7.3 kind check decide before anything
// else in array_begin. Collapsing them into one "integer array" family let an
// ArrayUnsigned header disarm the discard counter of a declared `i8[]` — so the
// mistyped array was skipped but left the fill counter armed, and the NEXT bare
// scalar was absorbed into the array (generator#270 / Crucible F-0045) — and let
// an ArrayFixlen header match an integer field's count bound, rejecting a message
// on a bound belonging to a field that header is not (generator#271 / F-0046).
func wantUnsignedArrayElem(k ir.Kind) bool {
	return isNativeArrayElem(k) && !wantSignedArrayElem(k) && k != ir.KindFP32 && k != ir.KindFP64
}

func wantSignedArrayElem(k ir.Kind) bool {
	return isSignedElem(k) || k == ir.KindEnum
}

// emitArraySkipArm arms the §7.3 discard counter in array_begin
// (generator#183, extended to fp by generator#193). Every array kind whose
// elements land in a callback a scalar shares is armed: integers land in
// unsigned()/signed(), fp lands in fp32()/fp64(). Every (scope, id) that
// genuinely declares a native array of that element kind disarms it (=> 0), so a
// legitimate array stores normally; everything else — a scalar-declared id, an
// unknown id, an fp64 header at a declared fp32 array — discards exactly `count`
// elements, after which a real scalar at the same id still decodes.
//
// The fixlen arm is keyed by element SUBTYPE (Fp32 / Fp64), not by a collapsed
// "some fixlen array" (CORELIB_PLAN §4.8, generator#259 / Crucible F-0042): the
// corelib now delivers array_begin only after the fixlen_word, so `kind` names
// the subtype actually on the wire, and a declared fp32[N] must disarm only for
// an Fp32 header. See emitArrayKindArms for why the catch-all is conditional.
func (g *gen) emitArraySkipArm(f *rfile, fs []frame, arrSkip bool) {
	if !arrSkip {
		return
	}
	emit := func(pat string, want func(ir.Kind) bool) {
		f.line("            %s => match (self.cur, id) {", pat)
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && want(fld.Elem) {
						f.line("                (_Loc::%s, %d) => 0,", fr.loc, fld.ID)
					}
				}
			case fkNestedNative:
				if want(fr.elemKind) {
					f.line("                (_Loc::%s, _) => 0,", fr.loc)
				}
			}
		}
		f.line("                _ => count,")
		f.line("            },")
	}
	f.line("        self.askip = match kind {")
	g.emitArrayKindArms(f, fs, emit, "            _ => count,")
	f.line("        };")
}

// emitArrayKindArms lays out the ArrayKind-keyed arms shared by the skip and
// fill counters: the integer arm (always emitted, since it also carries the
// "nothing declared here" default for integer headers), then an Fp32 and an Fp64
// arm for each fp element subtype the schema actually declares, then a catch-all
// carrying `dflt`.
//
// The catch-all is omitted exactly when both fp arms were named, because
// {Unsigned, Signed, Fp32, Fp64} is then already exhaustive and a trailing `_`
// would be an unreachable pattern (a Rust warning in generated code). When
// either fp subtype is undeclared the catch-all is what arms the counter for a
// header of that kind — a declared-fp32 message must still discard the elements
// of an fp64 array that arrives at any id.
func (g *gen) emitArrayKindArms(f *rfile, fs []frame, emit func(pat string, want func(ir.Kind) bool), dflt string) {
	// One arm per wire kind, never a collapsed integer family: a declared `i8[]`
	// must disarm only for ArraySigned, so an ArrayUnsigned header at that id is
	// skipped AND leaves the fill counter at 0 (generator#270 / F-0045).
	emit("ArrayKind::Unsigned", wantUnsignedArrayElem)
	emit("ArrayKind::Signed", wantSignedArrayElem)
	fp32, fp64 := anyArrayElem(fs, wantFP32Elem), anyArrayElem(fs, wantFP64Elem)
	if fp32 {
		emit("ArrayKind::Fp32", wantFP32Elem)
	}
	if fp64 {
		emit("ArrayKind::Fp64", wantFP64Elem)
	}
	if !fp32 || !fp64 {
		f.line("%s", dflt)
	}
}

// emitArrayFillArm arms the §7.3 fill counter in array_begin (generator#188), the
// mirror of emitArraySkipArm: armed with the element count at a legitimate
// native-array position matching the wire array kind (integer arrays under
// Unsigned/Signed, fp32 arrays under Fp32, fp64 arrays under Fp64), 0 everywhere
// else, so a bare scalar at an array id (afill == 0) falls through its fill arm
// and is skipped — and so does an fp64 array header that lands on a declared
// fp32 array, which is a §7.3 skip, not that field's value (generator#259).
func (g *gen) emitArrayFillArm(f *rfile, fs []frame, fillArm bool) {
	if !fillArm {
		return
	}
	emit := func(pat string, want func(ir.Kind) bool) {
		f.line("            %s => match (self.cur, id) {", pat)
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && want(fld.Elem) {
						f.line("                (_Loc::%s, %d) => count,", fr.loc, fld.ID)
					}
				}
			case fkNestedNative:
				if want(fr.elemKind) {
					f.line("                (_Loc::%s, _) => count,", fr.loc)
				}
			}
		}
		f.line("                _ => 0,")
		f.line("            },")
	}
	f.line("        self.afill = match kind {")
	g.emitArrayKindArms(f, fs, emit, "            _ => 0,")
	f.line("        };")
}

// emitPayloadFeed hands one string/blob chunk to the corelib's PayloadAcc and
// binds `name` to the whole payload, returning while bytes are still outstanding.
// Reassembly itself carries no schema knowledge -- (total, offset, chunk) in,
// the field's bytes out -- which is why it is the corelib's and not emitted here
// (ARCHITECTURE §8, generator#345).
//
// The no_std accumulator holds finite storage, so it has a third answer: a split
// payload larger than that storage can never be assembled and is BufferFull, the
// same verdict a fixed-capacity destination gives when it overflows. The maxlen
// guard emitted above already keeps every declared field under the bound, so the
// arm is a backstop rather than a reachable outcome -- and a backstop that
// rejects, where the previous inline form would have waited for a completion that
// could not arrive and dropped the field in silence.
//
// Keyed on the CORELIB rather than the no_std build flag, for the same reason the
// accumulator's TYPE is: corelib-rs-no-std returns the Result whichever way the
// crate is built, so `corelib: rs-no-std` with `no_std: false` needs this arm too.
func (g *gen) emitPayloadFeed(f *rfile, name string) {
	if g.corelib == "rs-no-std" {
		f.line("        let %s = match self.acc.feed(total, offset, chunk) { Ok(Some(_v)) => _v, Ok(None) => return, Err(_) => { self.err = true; return; } };", name)
		return
	}
	f.line("        let %s = match self.acc.feed(total, offset, chunk) { Some(_v) => _v, None => return };", name)
}

func (g *gen) emitVisitor(f *rfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	use := visitorUseOf(fs)

	// §7.3 array-vs-scalar skip (generator#183). Emitting it needs an array_begin
	// override, which requires the `array` Cargo feature under no_std. The decoder
	// now provisions the full wire-type set unconditionally (generator#215), so the
	// feature is always on and hasCap("array") is always true — but keep the guard
	// so the emit stays tied to the feature it depends on. corelib-rs (std) compiles
	// every wire type in unconditionally, so it always needs the guard too.
	arrSkip := !g.noStd || g.hasCap("array")
	// array_begin is emitted for its own array-target work, and additionally
	// whenever the §7.3 guard needs a place to arm itself.
	emitArrayBegin := use.scalarArray || arrSkip

	// String/blob chunk reassembly is the corelib's `PayloadAcc` (corelib-rs#88,
	// corelib-rs-no-std#92): the same handful of lines for every schema, so the
	// crate holds one instead of inlining it per callback. Carried only when the
	// message actually has a string or blob field to reassemble.
	//
	// The no-std twin keeps its storage in the caller, so its capacity is named
	// here: the message's max encoded size, which bounds any single payload
	// inside it. corelib-rs-no-std requires a maxlen on every string/blob in BOTH
	// storage modes, so that bound always resolves from the schema, and the
	// per-field maxlen guard rejects an over-long payload before a byte ever
	// reaches the accumulator.
	//
	// Keyed on the CORELIB, not on the no_std build flag: PayloadAcc<N> is what
	// corelib-rs-no-std declares whichever way the crate is built, so
	// `corelib: rs-no-std` with `no_std: false` needs the parameter too. Reading
	// g.noStd here emitted the std spelling for that configuration and did not
	// compile (caught by the _review corpus, which is the only place that pairs
	// this corelib with an allocating build).
	needAcc := use.str || use.blob
	accType, accNew := "sofab::PayloadAcc", "sofab::PayloadAcc::new()"
	if g.corelib == "rs-no-std" {
		accType = fmt.Sprintf("sofab::PayloadAcc<%d>", g.messageSize(name, fields).Size)
	}

	// Wrap the decoder in a private module so _Loc / V don't clash across
	// messages in a multi-message crate.
	// The decoder module stays private -- _Loc and V are implementation detail --
	// but the incremental Decoder is part of the public API, so it is re-exported
	// under the message's own name.
	f.line("pub use %s_dec::Decoder as %sDecoder;", strings.ToLower(name), name)
	f.line("mod %s_dec {", strings.ToLower(name))
	f.line("    use super::*;")
	// ArrayKind is gated behind the no-std `array` feature; import it only when an
	// array_begin override is emitted (i.e. the message has a native array).
	arrayKind := ""
	if emitArrayBegin {
		arrayKind = ", ArrayKind"
	}
	// FixlenType only for the fixlen_begin override, on the same on-demand rule --
	// a message with no bounded string/blob names neither type.
	fixlenType := ""
	if len(g.fixlenBeginArms(fs, ir.KindString)) > 0 || len(g.fixlenBeginArms(fs, ir.KindBlob)) > 0 {
		fixlenType = ", FixlenType"
	}
	f.line("    use sofab::{IStream, Visitor, Id, Unsigned, Signed%s%s};", arrayKind, fixlenType)
	f.blank()
	// Bounded decode stack for the no_std profile. Only LIVE scopes are stacked --
	// a sequence opened inside a skipped subtree is depth-counted in `dead` instead
	// (see sequence_begin) -- so a chain of live scopes, one entry per reachable
	// frame, is what has to fit. Without that, the stack was sized from the SCHEMA
	// while its depth came from the WIRE (up to MAX_DEPTH == 255, §4.9/§6.2, since
	// an unknown sequence may nest arbitrarily), and the surplus pushes were
	// dropped: the matching pops then restored the wrong scope and a field written
	// after the unwind bound nowhere (generator#283 / Crucible F-0055). The min-4
	// floor is slack: a message with no sequence of its own needs exactly one entry
	// (the root, held while its skipped subtree is open).
	//
	// One path can still push repeatedly without descending: an over-index wrapper
	// element (overIndexGuard) returns with cur left on the array frame so its own
	// sequence_end pops the entry back off. It has set self.inv by then, so such a
	// message is InvalidMsg whatever the stack does -- and an overflow there is
	// reported (err) rather than silently dropped.
	stackCap := len(fs)
	if stackCap < 4 {
		stackCap = 4
	}
	// The sticky lim flag exists only when a receiver-side decode limit is
	// active (generator#102) — std profile only, so the no_std inits never carry it.
	limInit := ""
	if g.limits.any() {
		limInit = ", lim: false"
	}
	askipInit := ""
	if arrSkip {
		askipInit = ", askip: 0"
	}
	if use.scalarArray {
		askipInit += ", afill: 0"
	}
	// Element-index slots (generator#247); the field order in a struct literal is
	// free, so they simply follow the rest of the state.
	ixVars := ixVarsOf(fs)
	for _, ix := range ixVars {
		askipInit += ", " + ix + ": 0"
	}
	accInit := ""
	if needAcc {
		accInit = ", acc: " + accNew
	}
	vInit := fmt.Sprintf("let mut v = V { m: &mut m, stack: Vec::new(), cur: _Loc::Root, dead: 0%s, err: false, inv: false%s%s };", accInit, limInit, askipInit)
	if g.noStd {
		vInit = fmt.Sprintf("let mut v = V { m: &mut m, stack: heapless::Vec::new(), cur: _Loc::Root, dead: 0%s, err: false, inv: false%s };", accInit, askipInit)
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
	// feed's structural verdict is captured, NOT propagated with `?` here: a
	// message that is both malformed (a schema-bound violation the visitor flagged
	// in v.inv) AND truncated must report INVALID, because §5.2 makes INVALID
	// dominate INCOMPLETE ("malformed regardless of what follows"). Propagating the
	// feed error first would surface the truncation and discard the INVALID signal
	// (generator#190). So read the sticky flags, apply the INVALID check, and only
	// then surface feed's Incomplete/InvalidMsg.
	f.line("        let fed;")
	f.line("        {")
	f.line("            %s", vInit)
	f.line("            let mut is = IStream::new();")
	f.line("            fed = is.feed(data, &mut v);")
	f.line("            overflow = v.err;")
	f.line("            invalid = v.inv;")
	if g.limits.any() {
		f.line("            limited = v.lim;")
	}
	f.line("        }")
	f.line("        // A scalar array carried more elements than its schema `count`, an")
	f.line("        // invalid-UTF-8 string, an over-length string/blob, or an over-index")
	f.line("        // wrapper element: INVALID, and it dominates a truncated tail (S5.2).")
	f.line("        if invalid { return Err(sofab::Error::InvalidMsg); }")
	if g.limits.any() {
		f.line("        // A structural INVALID from feed still dominates: those bytes are")
		f.line("        // malformed regardless of what follows (S5.2.3).")
		f.line("        if let Err(sofab::Error::InvalidMsg) = fed { return Err(sofab::Error::InvalidMsg); }")
		f.line("        // An unbounded field exceeded a configured receiver-side decode")
		f.line("        // limit: reject, never clamp -- and report it AHEAD of a truncated")
		f.line("        // tail. The cap was decided at the count/length header (S6.2.1), the")
		f.line("        // rejection is terminal (S6.3), and no continuation can lift it, so")
		f.line("        // Incomplete would tell the caller to feed bytes that cannot help.")
		f.line("        if limited { return Err(sofab::Error::LimitExceeded); }")
	}
	f.line("        // Nothing refused above: now surface feed's own verdict (a clean")
	f.line("        // Incomplete on a truncated-but-otherwise-valid message, or a")
	f.line("        // structural InvalidMsg).")
	f.line("        fed?;")
	f.line("        // A fixed-capacity field overflowed during the fill:")
	f.line("        // report it rather than return a silently-truncated value.")
	f.line("        if overflow { return Err(sofab::Error::BufferFull); }")
	f.line("        Ok(m)")
	f.line("    }")
	f.blank()

	// --- incremental decoder -------------------------------------------------
	//
	// decode/try_decode above own the IStream and the visitor for the length of
	// one call, so the caller must have the whole message as one contiguous
	// slice. The corelib does not require that -- IStream::feed is incremental
	// and reports Incomplete so it can be called again -- but the generated API
	// gave no way to hold the parse state across calls. At a transport that
	// means buffering the entire message before decoding it, which is what
	// streaming exists to avoid, and on a constrained target it means RAM for
	// the whole message.
	//
	// The decoder owns the message and the visitor's persistent state as plain
	// fields; V borrows them for the duration of one feed and is destructured
	// afterwards, so nothing here is self-referential.
	stateFields := g.visitorState(stackCap, needAcc, accType, accNew, arrSkip, use.scalarArray, ixVars)
	f.line("    /// Incremental decoder: hold one and feed the message as bytes arrive.")
	f.line("    ///")
	f.line("    /// The wire format has no end marker at the top level -- a message ends")
	f.line("    /// where its bytes end -- so `feed` cannot tell you the message is")
	f.line("    /// complete, and does not try to. Its verdict is about the bytes handed")
	f.line("    /// in: `Ok(())` means they ended on a clean field boundary (the message")
	f.line("    /// COULD end here), `Err(Incomplete)` means they ended mid-field. Neither")
	f.line("    /// is a failure mid-stream. The caller's own framing -- a length prefix, a")
	f.line("    /// datagram boundary, a closed socket -- decides when to stop; `finish`")
	f.line("    /// then gives the verdict for the message as a whole.")
	f.line("    ///")
	f.line("    /// Any error other than `Incomplete` is terminal: discard the decoder.")
	f.line("    pub struct Decoder {")
	f.line("        m: %s,", name)
	f.line("        is: IStream,")
	for _, sf := range stateFields {
		f.line("        %s: %s,", sf.name, sf.typ)
	}
	f.line("    }")
	f.blank()
	f.line("    impl Decoder {")
	f.line("        pub fn new() -> Self {")
	inits := make([]string, 0, len(stateFields))
	for _, sf := range stateFields {
		inits = append(inits, fmt.Sprintf("%s: %s", sf.name, sf.init))
	}
	f.line("            Self { m: %s::default(), is: IStream::new(), %s }", name, strings.Join(inits, ", "))
	f.line("        }")
	f.blank()
	f.line("        /// Feed the next chunk. `Ok(())` if it ended on a field boundary,")
	f.line("        /// `Err(Incomplete)` if it ended mid-field -- see the type docs: neither")
	f.line("        /// answers whether the MESSAGE is done, only whether these bytes were.")
	f.line("        pub fn feed(&mut self, chunk: &[u8]) -> Result<(), sofab::Error> {")
	f.line("            let fed = {")
	takes := make([]string, 0, len(stateFields))
	names := make([]string, 0, len(stateFields))
	for _, sf := range stateFields {
		names = append(names, sf.name)
		if sf.copy {
			takes = append(takes, fmt.Sprintf("%s: self.%s", sf.name, sf.name))
		} else {
			takes = append(takes, fmt.Sprintf("%s: core::mem::take(&mut self.%s)", sf.name, sf.name))
		}
	}
	f.line("                let mut v = V { m: &mut self.m, %s };", strings.Join(takes, ", "))
	f.line("                let r = self.is.feed(chunk, &mut v);")
	f.line("                // `..` covers `m`, ending its borrow before the write-back.")
	f.line("                let V { %s, .. } = v;", strings.Join(names, ", "))
	for _, sf := range stateFields {
		f.line("                self.%s = %s;", sf.name, sf.name)
	}
	f.line("                r")
	f.line("            };")
	f.line("            // INVALID dominates a truncated tail (S5.2), so it is reported")
	f.line("            // ahead of feed's own Incomplete verdict.")
	f.line("            if self.inv { return Err(sofab::Error::InvalidMsg); }")
	if g.limits.any() {
		f.line("            // A crossed receiver cap is TERMINAL (S6.3): surface it on the very")
		f.line("            // feed that crossed it rather than at finish(), so a caller does not")
		f.line("            // keep reading a stream this decoder has already refused.")
		f.line("            if self.lim { return Err(sofab::Error::LimitExceeded); }")
	}
	f.line("            fed")
	f.line("        }")
	f.blank()
	f.line("        /// Take the decoded message once the caller's framing says the input")
	f.line("        /// is over. Applies the same checks as try_decode, including that the")
	f.line("        /// stream actually ended at a clean boundary -- a truncated message")
	f.line("        /// must be rejected, not returned half-filled.")
	f.line("        pub fn finish(mut self) -> Result<%s, sofab::Error> {", name)
	f.line("            if self.inv { return Err(sofab::Error::InvalidMsg); }")
	if g.limits.any() {
		f.line("            // Ahead of the end-of-input probe: a crossed cap is terminal and")
		f.line("            // was decided at a header, so it must not be downgraded to the")
		f.line("            // Incomplete a truncated tail would report (S6.2.1/S6.3).")
		f.line("            if self.lim { return Err(sofab::Error::LimitExceeded); }")
	}
	f.line("            // An empty chunk probes end-of-input without supplying any: Ok only")
	f.line("            // when nothing is half-read. This is what makes a truncated stream")
	f.line("            // an error here rather than a silently partial value.")
	f.line("            self.feed(&[])?;")
	f.line("            if self.err { return Err(sofab::Error::BufferFull); }")
	f.line("            Ok(self.m)")
	f.line("        }")
	f.line("    }")
	f.blank()
	f.line("    impl Default for Decoder {")
	f.line("        fn default() -> Self { Self::new() }")
	f.line("    }")
	f.blank()

	// _Loc enum. Dead is the SKIPPED-SUBTREE scope: sequence_begin moves here for
	// any (scope, id) the schema does not declare, and every callback arm is keyed
	// to a real scope, so nothing matches while cur is Dead and the whole subtree
	// is discarded. See the sequence_begin default arm (generator#268/#272).
	f.line("#[derive(Clone, Copy, PartialEq)]")
	f.line("enum _Loc {")
	for _, fr := range fs {
		f.line("    %s,", fr.loc)
	}
	f.line("    Dead,")
	f.line("}")
	f.blank()

	f.line("struct V<'a> {")
	f.line("    m: &'a mut %s,", name)
	if g.noStd {
		// Heap-free: bounded location stack.
		f.line("    stack: heapless::Vec<_Loc, %d>,", stackCap)
	} else {
		f.line("    stack: Vec<_Loc>,")
	}
	f.line("    cur: _Loc,")
	f.line("    dead: u16, // depth of the skipped subtree cur sits in (see sequence_begin)")
	if needAcc {
		f.line("    acc: %s,", accType)
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
	// §7.3 array-vs-scalar skip counter (generator#183): an integer array whose id
	// is declared as a SCALAR is a wire-type contradiction and must be skipped like
	// an unknown id. corelib-rs delivers array elements through the same
	// unsigned()/signed() callbacks a lone scalar uses, so the id dispatch alone
	// cannot tell them apart; array_begin arms this with the announced element
	// count and the callbacks discard exactly that many.
	if arrSkip {
		f.line("    askip: usize, // elements left to discard from a wire-type-contradictory array")
	}
	// §7.3 mirror (generator#188): a bare scalar delivered at a native-array id
	// would land in that array's fill arm as element 0. array_begin arms this with
	// the announced count at legitimate native-array positions; a fill runs only
	// while it is positive, so an unarmed bare scalar (afill == 0) is skipped.
	if use.scalarArray {
		f.line("    afill: usize, // elements still expected by an armed native-array fill (S7.3)")
	}
	// The array index of the element each struct/union wrapper array is currently
	// decoding (generator#247): the element id IS that index (S5.1), and a flat
	// visitor has no child-visitor object to carry it, so the element location
	// addresses its object through this slot instead of the last one pushed.
	for _, ix := range ixVars {
		f.line("    %s: usize, // index of the wrapper-array element currently decoding", ix)
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
					f.line("            (_Loc::%s, %d) => { %s%s.%s = value as %s },", fr.loc, fld.ID, widthGuard(fld.Kind), fr.path, rustIdent(fld.Name), g.rustType(fld))
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
			var store string
			switch {
			case isUnsignedElem(fr.elemKind):
				store = g.rowStore(fr, "value as "+numRustType(fr.elemKind))
			case fr.elemKind == ir.KindBool:
				store = g.rowStore(fr, "value != 0")
			case fr.elemKind == ir.KindBitfield:
				store = g.rowStore(fr, "value as "+bitfieldBacking(fr.elemRef.Target))
			default:
				continue
			}
			if g.limits.arrayHas && fr.elemDyn {
				store = g.limArrayStore(store)
			}
			f.line("            (_Loc::%s, _) => { %s%s%s; },", fr.loc, fillGuard, widthGuard(fr.elemKind), store)
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
					f.line("            (_Loc::%s, %d) => { %s%s.%s = value as %s },", fr.loc, fld.ID, widthGuard(fld.Kind), fr.path, rustIdent(fld.Name), g.rustType(fld))
				case fld.Kind == ir.KindEnum:
					f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), enumBacking(fld.Ref.Target))
				case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
					g.emitNativeArrayStore(f, fr, fld, fmt.Sprintf("value as %s", numRustType(fld.Elem)))
				case fld.Kind == ir.KindArray && fld.Elem == ir.KindEnum:
					g.emitNativeArrayStore(f, fr, fld, fmt.Sprintf("value as %s", enumBacking(fld.ElemRef.Target)))
				}
			}
		case fkNestedNative:
			var store string
			switch {
			case isSignedElem(fr.elemKind):
				store = g.rowStore(fr, "value as "+numRustType(fr.elemKind))
			case fr.elemKind == ir.KindEnum:
				store = g.rowStore(fr, "value as "+enumBacking(fr.elemRef.Target))
			default:
				continue
			}
			if g.limits.arrayHas && fr.elemDyn {
				store = g.limArrayStore(store)
			}
			f.line("            (_Loc::%s, _) => { %s%s%s; },", fr.loc, fillGuard, widthGuard(fr.elemKind), store)
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	if use.fp32 {
		g.emitFloatVisit(f, fs, ir.KindFP32, "fp32", "f32", arrSkip)
	}
	if use.fp64 {
		g.emitFloatVisit(f, fs, ir.KindFP64, "fp64", "f64", arrSkip)
	}

	g.emitFixlenBegin(f, fs, use)

	if use.str {
		// string: scalar strings + string-array elements
		f.line("    fn string(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {")
		g.emitDestGuard(f, fs, ir.KindString)
		g.emitMaxlenGuard(f, fs, ir.KindString)
		if g.limits.stringHas {
			g.emitLimitGuard(f, fs, ir.KindString, "MAX_DYN_STRING_LEN")
		}
		if g.fixedFields() {
			// Same strict rule as the std profile, and stated in the output for the
			// same reason: it is the decision a reader of the generated crate would
			// otherwise have to reconstruct.
			f.line("        // A Rust string type is Unicode, so a string is always strict. Invalid")
			f.line("        // UTF-8 is the INVALID decode outcome (self.inv -> Error::InvalidMsg),")
			f.line("        // never a lossy U+FFFD and never empty; the two Rust profiles agree")
			f.line("        // (subsumes #80). The verdict is passed on the ASSEMBLED payload, which")
			f.line("        // is why it sits after the feed and not per chunk.")
			g.emitPayloadFeed(f, "_p")
			f.line("        let _s = match core::str::from_utf8(_p) { Ok(_v) => _v, Err(_) => { self.inv = true; \"\" } };")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindString {
					// clear() FIRST: the element is REPLACED, not appended to. A repeated
					// element id is last-occurrence-wins (MESSAGE_SPEC §7.4), and chunk
					// reassembly already happened above (`acc`), so every arm here
					// receives one complete value. Without the clear a second occurrence
					// concatenated onto the first, and the capacity check below — written
					// for an empty destination — then tripped into Error::BufferFull on
					// any repeat at any size (generator#273 / Crucible F-0048).
					f.line("            (_Loc::%s, _) => { %s%s if let Some(_e) = %s.get_mut(id as usize) { _e.clear(); let _ = _e.push_str(_s); if _e.len() != _s.len() { self.err = true; } } }", fr.loc, g.overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
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
			f.line("        // A Rust string type is Unicode, so a string is always strict. Invalid")
			f.line("        // UTF-8 is the INVALID decode outcome (self.inv -> Error::InvalidMsg),")
			f.line("        // never a lossy U+FFFD and never empty; the two Rust profiles agree")
			f.line("        // (subsumes #80). The verdict is passed on the ASSEMBLED payload, which")
			f.line("        // is why it sits after the feed and not per chunk.")
			g.emitPayloadFeed(f, "_p")
			f.line("        let _s = match core::str::from_utf8(_p) { Ok(_v) => _v.to_owned(), Err(_) => { self.inv = true; String::new() } };")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindString {
					f.line("            (_Loc::%s, _) => { %s%s %s[id as usize] = _s; }", fr.loc, g.overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
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
		if g.fixedFields() {
			g.emitPayloadFeed(f, "_b")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindBlob {
					// The blob twin of the string arm above: replace, never append
					// (generator#273 / F-0048).
					f.line("            (_Loc::%s, _) => { %s%s if let Some(_e) = %s.get_mut(id as usize) { _e.clear(); let _ = _e.extend_from_slice(_b); if _e.len() != total { self.err = true; } } }", fr.loc, g.overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
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
			g.emitPayloadFeed(f, "_p")
			f.line("        let _b = _p.to_vec();")
			f.line("        match (self.cur, id) {")
			for _, fr := range fs {
				if fr.kind == fkSeqArr && fr.elemKind == ir.KindBlob {
					f.line("            (_Loc::%s, _) => { %s%s %s[id as usize] = _b; }", fr.loc, g.overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.path)
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
		// array_begin resets a native-array target (a scalar array field) or opens
		// the row an id names (a nested native array).
		//
		// Every native array is cleared, whatever its declared `count`. The wire
		// count M IS the array's length (MESSAGE_SPEC §3) -- `count: N` is a
		// capacity that bounds M and sizes the container, never a length that
		// pre-fills it -- so the M elements that arrive are the whole value and
		// there is no [M, N) tail to size, wipe or refill.
		//
		// The target match is keyed by (kind, loc, id), not just (loc, id): a
		// fixlen array announces its element SUBTYPE, and a declared fp32[N] is
		// only reached by an Fp32 header. An fp64 header at that id contradicts
		// the declared type, so the whole field is skipped (§7.3) -- it is not
		// that field's value, so its schema `count` is not this header's bound
		// and its container is not cleared. Both of those live INSIDE the arm,
		// behind the kind test, for exactly that reason: the header must fall
		// through to the discard counter armed above, never to a reject.
		//
		// Integer arrays keep the kind-agnostic `_` pattern, and that is a KNOWN
		// GAP, not a claim that there is nothing to guard. An fp32/fp64 (or
		// mis-signed integer) header arriving at a declared integer array id still
		// reaches that field's arm here: it applies the field's schema `count`
		// bound to a count that is not the field's, and clears the container --
		// while the discard counter armed above is simultaneously throwing the
		// elements away. The two halves contradict each other.
		//
		// It is left alone deliberately. That face is generator#254 / Crucible
		// F-0039, which was fixed for java and csharp only and never for rust; it
		// is a different codegen path (its primary form is a non-fixlen
		// ARRAY_SIGNED header at a u8[] slot) and it is tracked on its own. Fixing
		// it here would change what an over-count MIS-TYPED header decodes to in
		// rust, which is outside what the fixlen-subtype work decides. rust is the
		// last of the six backends still carrying it: java, csharp, zig, go and
		// dart all key their integer arms by kind.
		f.line("    fn array_begin(&mut self, id: Id, kind: ArrayKind, count: usize) {")
		g.emitArraySkipArm(f, fs, arrSkip)
		g.emitArrayFillArm(f, fs, use.scalarArray)
		f.line("        match (kind, self.cur, id) {")
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
						kp := arrayKindPat(fld.Elem)
						clear := fmt.Sprintf("%s.%s.clear()", fr.path, rustIdent(fld.Name))
						if fld.HasCount {
							// Over-count reject at the count header (generator#216 / F-0032):
							// a wire element count above the schema `count` N is INVALID
							// (MESSAGE_SPEC §3+§7), and deciding it HERE — before the elements
							// are read — makes INVALID dominate a truncated tail per §5.2. A
							// check only at the element store never fires when truncation cuts
							// the array short of N, so an over-count-AND-truncated array would
							// misreport INCOMPLETE.
							//
							// It sits inside the kind-keyed arm (generator#259 / F-0042): the
							// count word is read before the fixlen_word, so a bound applied on
							// the strength of the count alone would reject a header that turns
							// out to belong to no declared field at all.
							f.line("            (%s, _Loc::%s, %d) => { if count > %d { self.inv = true; return; } %s },", kp, fr.loc, fld.ID, fld.Count, clear)
							continue
						}
						// Unbounded array under an active receiver cap (generator#102):
						// reject an over-cap wire count at the header, before any
						// elements accumulate.
						if g.limits.arrayHas {
							f.line("            (%s, _Loc::%s, %d) => { if count > MAX_DYN_ARRAY_COUNT { self.lim = true; return; } %s },", kp, fr.loc, fld.ID, clear)
							continue
						}
						f.line("            (%s, _Loc::%s, %d) => %s,", kp, fr.loc, fld.ID, clear)
					}
				}
			case fkNestedNative:
				// The row's element id IS its index in the outer array (§5.1), so the
				// row is OPENED AT out[id] -- grown into with empty rows, never
				// appended. Appending was id-blind: an interior row equal to the
				// element default (the empty row) is omitted by a conformant encoder
				// (§2) and leaves an id gap, which would then shift every later row
				// down by one. The over-index reject runs first, so it also bounds the
				// gap-fill against an amplification DoS -- a bound this collector did
				// not have before.
				//
				// Both of the row's schema bounds are decided here, at the header
				// (rowGuards): the ROW id against the outer `count`, and the row's own
				// element count against the INNER `count`. The inner bound is the twin
				// of the top-level over-count reject above -- without it a `count: M`
				// row is not a bound at all on decode, and a row grows to whatever
				// element count the wire announces (std) or silently drops the excess
				// past its heapless capacity (no_std), instead of INVALID per §3+§7.
				//
				// Keyed by the row's own element subtype for the same reason as the
				// leaf arms: an fp64 row header at a declared fp32 row is a skipped
				// field, so neither of these bounds is its bound.
				f.line("            (%s, _Loc::%s, _) => { %s%s self.%s = id as usize; },",
					arrayKindPat(fr.elemKind), fr.loc, g.rowGuards(fr), g.seqElemGrow(fr.path), fr.ixVar)
			}
		}
		f.line("            _ => {}")
		f.line("        }")
		f.line("    }")
	}

	// sequence_begin/sequence_end are emitted UNCONDITIONALLY, even for a message
	// that declares no sequence of its own. The corelib cannot know which ids the
	// schema declares, so it delivers every sequence and the generated code decides
	// -- which means a message with no override at all would let the CHILDREN of an
	// unknown sequence arrive with cur still on the enclosing scope and bind there
	// (generator#268 / Crucible F-0044). Overriding it is what makes the skip real.
	{
		// sequence_begin: push current, descend. String/blob/composite array fields
		// clear their Vec on entry; struct/nested-array wrapper frames push a fresh
		// element and descend on each per-element sequence_begin.
		var arms []string
		add := func(format string, a ...any) { arms = append(arms, fmt.Sprintf(format, a...)) }
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					switch {
					case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
						add("            (_Loc::%s, %d) => _Loc::%s,", fr.loc, fld.ID, fr.loc+"_"+fld.Name)
					case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
						add("            (_Loc::%s, %d) => { %s.%s.clear(); _Loc::%s },", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), fr.loc+"_"+fld.Name)
					}
				}
			case fkStructArr:
				// generator#247: the element id IS the array index (§5.1), so the
				// element is PLACED at out[id] after gap-filling with default
				// elements -- exactly like the leaf string/blob path above -- and
				// never appended. The over-index reject runs FIRST, so it bounds the
				// gap-fill (and, on no_std, keeps the index inside the heapless
				// capacity); returning early leaves cur on the array frame, which the
				// element's own sequence_end pops back off the already-pushed stack.
				add("            (_Loc::%s, _) => { %s%s self.%s = id as usize; _Loc::%s },",
					fr.loc, g.overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.ixVar, fr.elemLoc)
			case fkArrArr:
				// Same rule as fkStructArr above: the element id IS the row's index
				// (§5.1), so the row is placed at out[id] rather than appended -- an
				// interior all-default row is omitted (§2) and leaves an id gap that
				// an appending collector would close, shifting every later row down.
				add("            (_Loc::%s, _) => { %s%s self.%s = id as usize; _Loc::%s },",
					fr.loc, g.overIndexGuard(fr.cap), g.seqElemGrow(fr.path), fr.ixVar, fr.elemLoc)
			}
		}
		// The default arm is a SKIP, not "stay where you are". An id the schema does
		// not declare here -- an unknown sequence (§5.2/§4.9), or a sequence landing
		// on a position declared as something else (§7.3) -- must be discarded WHOLE,
		// children included. Keeping cur put let those children bind into the
		// enclosing scope: a child id 3 inside an unknown sequence set the root's own
		// field 3 (generator#268), and a sequence opened at a string-array element
		// position bound its string as that element (generator#272).
		//
		// A sequence opened while cur is already Dead is DEPTH-COUNTED, not stacked:
		// every scope inside a skipped subtree is Dead, so the stack would only
		// record which level to return to -- and `dead` records that in two bytes,
		// whatever the wire nests. That is what keeps the stack bounded by the
		// number of reachable frames rather than by MAX_DEPTH (255): the no_std
		// stack is a fixed-capacity heapless::Vec, and stacking dead levels let a
		// legal message overrun it, after which the surplus pops restored the WRONG
		// scope and a field written after the unwind bound nowhere -- accepted, and
		// silently missing a field (generator#283 / Crucible F-0055).
		// `id` is only read by the arms; without any, name it _id so the generated
		// crate stays warning-clean.
		idParam := "id"
		if len(arms) == 0 {
			idParam = "_id"
		}
		f.line("    fn sequence_begin(&mut self, %s: Id) {", idParam)
		f.line("        // Inside a skipped subtree: count the level and stay Dead.")
		f.line("        if self.cur == _Loc::Dead { self.dead = self.dead.saturating_add(1); return; }")
		if g.noStd {
			// The stack is sized for a chain of live scopes and dead levels are
			// counted rather than stacked, so a well-formed message cannot get here
			// (an over-index element chain can, and has already set inv). Either
			// way the push returns a Result, and DROPPING it is exactly what turned
			// an overrun into a silent wrong value. Report it instead: sticky err
			// surfaces as Error::BufferFull, and the level is entered as a skipped
			// subtree so a scope that was never stacked can never be popped into.
			f.line("        if self.stack.push(self.cur).is_err() { self.err = true; self.dead = self.dead.saturating_add(1); self.cur = _Loc::Dead; return; }")
		} else {
			f.line("        %s", g.pushStmt("self.stack", "self.cur"))
		}
		if len(arms) == 0 {
			// The message declares no sequence at all, so every sequence that arrives
			// is unknown. A match with only a wildcard would be a Clippy
			// match_single_binding in generated code; the assignment says the same.
			f.line("        self.cur = _Loc::Dead;")
		} else {
			f.line("        self.cur = match (self.cur, id) {")
			for _, a := range arms {
				f.line("%s", a)
			}
			f.line("            _ => _Loc::Dead,")
			f.line("        };")
		}
		f.line("    }")
		f.line("    fn sequence_end(&mut self) {")
		f.line("        // Closing a level of a skipped subtree: nothing was stacked for it.")
		f.line("        if self.dead > 0 { self.dead -= 1; return; }")
		// Nothing to reconcile here: the wrapper array's decoded length is *highest
		// present id + 1* (MESSAGE_SPEC §5.1) and every element that carries it has
		// already been placed. A declared `count: N` is a capacity, so there is no
		// fill-to-N -- filling would turn the M elements the wire carried into N,
		// which is a different value.
		f.line("        self.cur = self.stack.pop().unwrap_or(_Loc::Root);")
		f.line("    }")
	}

	f.line("}") // impl Visitor
	f.line("}") // mod <name>_dec
	f.blank()
}

// emitDestGuard emits the skip gate at the very top of the string callback
// (CORELIB_PLAN §6.4, generator#257): "skipped fields are never validated".
// Skipping is a length jump over bytes that are not inspected (§5.2), and
// UTF-8 validation runs only where a `string` is materialized — read into a
// destination. So the destination is resolved FIRST: every (loc, id) that
// declares a string, plus the wrapper-sequence rows whose element kind is
// string, falls through; anything else returns right here.
//
// Returning here is what makes the skip a true skip: an unknown id, or a §7.3
// wire-type contradiction routed down the same path, never accumulates into
// the shared `acc` (so a later declared field cannot inherit its bytes), never
// transcodes, and never trips the sticky `inv` flag. Without it a 3-byte
// isolate carrying a lone continuation byte at an undeclared id turned an
// otherwise valid message into INVALID.
//
// Placed ahead of the maxlen/limit guards, which are already destination-scoped
// and therefore unaffected — §5.2's INVALID-over-INCOMPLETE ordering is
// preserved.
func (g *gen) emitDestGuard(f *rfile, fs []frame, kind ir.Kind) {
	var arms []string
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind {
			arms = append(arms, fmt.Sprintf("            (_Loc::%s, _) => {},", fr.loc))
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind {
				arms = append(arms, fmt.Sprintf("            (_Loc::%s, %d) => {},", fr.loc, fld.ID))
			}
		}
	}
	if len(arms) == 0 {
		return
	}
	f.line("        // A payload this scope does not declare is skipped: its bytes are jumped")
	f.line("        // over, never inspected. Resolve the destination first and leave before a")
	f.line("        // byte is buffered, decoded or checked.")
	f.line("        match (self.cur, id) {")
	for _, a := range arms {
		f.line("%s", a)
	}
	f.line("            _ => return,")
	f.line("        }")
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

// emitFixlenBegin latches every schema bound a fixlen field's LENGTH WORD already
// decides, at that word (CORELIB_PLAN §5.2, generator#267).
//
// The bounds themselves are not new -- a scalar/element `maxlen` and a wrapper
// element's `id >= count` were both already rejected. They were rejected in the
// PAYLOAD callback, which only fires once payload bytes arrive. So a message
// truncated immediately after the length word never reached them and reported
// INCOMPLETE, while the same bytes read whole are INVALID. §5.2 makes INVALID
// dominate INCOMPLETE precisely because the violation is already established by
// the bytes seen; corelib-rs#47 / corelib-rs-no-std#68 added the hook that makes
// the verdict available there.
//
// Everything sits inside the DECLARED-subtype match. The hook fires for whatever
// fixlen subtype arrived at a field id -- the corelib resolves what arrived but
// cannot know what was declared -- so a contradicting subtype must not be
// measured against this field's bound; that is a §7.3 skip. Same rule as #224 /
// #259, applied one position over.
//
// The payload-side guards STAY. They are unreachable for a message that gets this
// far, and they are the only thing still bounding a consumer built against a
// corelib whose hook predates this.
func (g *gen) emitFixlenBegin(f *rfile, fs []frame, use visitorUse) {
	str := g.fixlenBeginArms(fs, ir.KindString)
	blob := g.fixlenBeginArms(fs, ir.KindBlob)
	if len(str) == 0 && len(blob) == 0 {
		return
	}
	f.line("    fn fixlen_begin(&mut self, id: Id, subtype: FixlenType, total: usize) {")
	f.line("        // Every bound below is fully established by the LENGTH WORD, so it is")
	f.line("        // decided here rather than once payload bytes arrive: S5.2 makes INVALID")
	f.line("        // dominate INCOMPLETE, so truncating right after this word must not")
	f.line("        // downgrade the verdict. The subtype match is S7.3 -- a contradicting")
	f.line("        // fixlen kind at this id is a SKIPPED field, not this field's length.")
	f.line("        match subtype {")
	for _, a := range []struct {
		variant string
		arms    []string
	}{{"Str", str}, {"Blob", blob}} {
		if len(a.arms) == 0 {
			continue
		}
		f.line("            FixlenType::%s => match (self.cur, id) {", a.variant)
		for _, arm := range a.arms {
			f.line("%s", arm)
		}
		f.line("                _ => {}")
		f.line("            },")
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")
	_ = use
}

// fixlenBeginArms builds the (location, id) arms for one fixlen subtype: a
// wrapper element carries its array's over-index bound AND its element maxlen,
// a scalar field carries its own maxlen. The over-index reject comes first --
// an element that is not this array's element at all must not have its length
// measured against the array's element bound.
func (g *gen) fixlenBeginArms(fs []frame, kind ir.Kind) []string {
	var arms []string
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind && (fr.cap >= 0 || fr.emax >= 0) {
			body := ""
			if fr.cap >= 0 {
				body += fmt.Sprintf("if id as usize >= %d { self.inv = true; return; } ", fr.cap)
			}
			if fr.emax >= 0 {
				body += fmt.Sprintf("if total > %d { self.inv = true; return; } ", fr.emax)
			}
			arms = append(arms, fmt.Sprintf("                (_Loc::%s, _) => { %s},", fr.loc, body))
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind && fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("                (_Loc::%s, %d) => if total > %d { self.inv = true; return; },", fr.loc, fld.ID, fld.Maxlen))
			}
		}
	}
	return arms
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
// `.push(rhs)` into the field's container, which array_begin cleared. Every
// native array takes the same shape, `count: N` or not -- the wire count M IS
// the array's length (MESSAGE_SPEC §3), so the elements that arrive are simply
// collected, and the container's capacity (a schema-sized heapless::Vec under
// no_std) is N. An over-count array was already rejected at its count header
// (INVALID per §3+§7, never clamped -- generator#100/#216), which is also what
// keeps the push inside a fixed capacity.
func (g *gen) emitNativeArrayStore(f *rfile, fr frame, fld *ir.Field, rhs string) {
	store := g.pushExpr(fr.path+"."+rustIdent(fld.Name), rhs)
	if g.limits.arrayHas && !fld.HasCount {
		store = g.limArrayStore(store)
	}
	// widthGuard AFTER fillGuard: an element only breaches the declared width once
	// it is actually being stored (§7.1). Ahead of the fill check it would reject a
	// bare scalar at an array id, which §7.3 says to skip.
	f.line("            (_Loc::%s, %d) => { %s%s%s; },", fr.loc, fld.ID, fillGuard, widthGuard(fld.Elem), store)
}

// fillGuard fronts every native-array fill arm (generator#188): the fill runs
// only while array_begin has this array armed (afill > 0). A bare scalar at an
// array id arrives with afill == 0 — no array_begin preceded it — so it returns
// without storing and is skipped like an unknown id, the mirror of the askip
// guard that skips an array delivered at a scalar id (MESSAGE_SPEC §7.3).
const fillGuard = "if self.afill == 0 { return; } self.afill -= 1; "

// widthGuard returns the §7.1 over-width reject clause for a store into a
// destination the schema declares with Kind k, or "" when k spans the whole
// accumulator the value arrives in (the 64-bit kinds, where no reachable value
// can breach the bound).
//
// The declared width is a normative validity bound, not a storage hint
// (MESSAGE_SPEC §1/§7.1, documentation#32): an out-of-range value is INVALID and
// must be neither masked to the width nor kept. Without this clause the `value as
// u8` that follows is exactly the mask the clause forbids. Same sticky flag, and
// so the same Error::InvalidMsg, as the maxlen and count guards.
//
// Placement matters as much as the comparison. In an ARRAY arm the clause goes
// *after* fillGuard, never before: an over-width scalar arriving at an array id
// with no array_begin in front of it is a §7.3 skip, and rejecting it ahead of
// the fill check would turn that skip into a spurious INVALID.
// The comparison form follows from the kind itself: a u* destination is
// delivered through unsigned() as a u64, where only the upper bound is
// reachable, while an i* destination arrives through signed() as an i64 and
// needs both ends.
func widthGuard(k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("if value < %d || value > %d { self.inv = true; return; } ", lo, hi)
	}
	return fmt.Sprintf("if value > %d { self.inv = true; return; } ", hi)
}

// limArrayStore wraps an unbounded-array element store so it is dropped once
// the sticky lim flag is set (generator#102): the over-cap array was rejected
// at its count header, so its elements must not accumulate either. For a
// nested-native array this also keeps the elements out of whatever row the index
// slot still names, after the tripped array_begin returned without opening a new
// one (rowStore's get_mut is what makes that a no-op rather than a panic).
func (g *gen) limArrayStore(expr string) string {
	return fmt.Sprintf("{ if !self.lim { %s; } }", expr)
}

func (g *gen) emitFloatVisit(f *rfile, fs []frame, kind ir.Kind, cb, rtype string, arrSkip bool) {
	f.line("    fn %s(&mut self, id: Id, value: %s) {", cb, rtype)
	g.emitArraySkipGuard(f, arrSkip)
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		if fr.kind == fkNestedNative && fr.elemKind == kind {
			store := g.rowStore(fr, "value")
			if g.limits.arrayHas && fr.elemDyn {
				store = g.limArrayStore(store)
			}
			f.line("            (_Loc::%s, _) => { %s%s; },", fr.loc, fillGuard, store)
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

// rowStore pushes one element into the row of a nested-native array (an array
// whose elements are native arrays) that the element belongs to: the row at the
// index its own array_begin recorded, since the element id IS the row's index in
// the outer array (§5.1). `get_mut` rather than an index expression, so an
// element that arrives without its array_begin -- which is what a §7.3-skipped or
// limit-rejected header leaves behind -- is a no-op instead of a panic on
// untrusted input.
func (g *gen) rowStore(fr frame, val string) string {
	return fmt.Sprintf("if let Some(_r) = %s.get_mut(self.%s) { %s }", fr.path, fr.ixVar, g.pushFieldStmt("_r", val))
}

// pushExpr / pushStmt handle the heapless-vs-heap container push: under
// no_std push returns a Result that must be consumed (let _ = ...); the std path
// uses a bare Vec push. A grown-into row is `Default::default()` on both, which
// is the empty container whichever one the profile chose.
func (g *gen) pushExpr(target, val string) string {
	if g.fixedFields() {
		return fmt.Sprintf("{ let _ = %s.push(%s); }", target, val)
	}
	return fmt.Sprintf("%s.push(%s)", target, val)
}

// pushStmt is the DECODER's own stack push, which follows the environment: a
// bounded heapless stack under no_std, a heap Vec otherwise. Static field
// storage does not change it -- staticStore is about message fields, and the
// decoder's scratch is free to stay on the heap where there is one.
func (g *gen) pushStmt(target, val string) string {
	if g.noStd {
		return fmt.Sprintf("let _ = %s.push(%s);", target, val)
	}
	return fmt.Sprintf("%s.push(%s);", target, val)
}

// pushFieldStmt is the same for a MESSAGE container, which follows the storage
// axis instead. The Result-consuming form is also correct for a Vec (push
// returns unit), so one form serves the mixed case a per-field bound produces.
func (g *gen) pushFieldStmt(target, val string) string {
	if g.fixedFields() {
		return fmt.Sprintf("let _ = %s.push(%s);", target, val)
	}
	return fmt.Sprintf("%s.push(%s);", target, val)
}

// seqElemGrow emits the id-indexed growth prefix for a wrapper-sequence string/
// blob element collector: grow the container to id+1, filling the gap with the
// element default (empty), so a decoded element lands at index = its wire id and
// omitted default elements leave the right gaps (MESSAGE_SPEC S2). Under no_std the
// container is a fixed-capacity heapless::Vec (or an alloc fallback under
// allow_dynamic): push may be a no-op when full, so the loop breaks when the length
// stops growing to avoid spinning on an out-of-capacity id; get_mut then no-ops.
func (g *gen) seqElemGrow(path string) string {
	if g.fixedFields() {
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

// vField is one field of the visitor's state that must survive between feed
// calls. `copy` marks a Copy type, which the decoder can read straight out of
// itself; the rest are moved out with core::mem::take.
type vField struct {
	name string
	typ  string
	init string
	copy bool
}

// visitorState lists the visitor's persistent fields — everything in V except
// the `&mut message` borrow. It is the single description used for the V
// construction inside decode/try_decode and for the incremental Decoder, so the
// two cannot drift apart.
func (g *gen) visitorState(stackCap int, needAcc bool, accType, accNew string, arrSkip, scalarArray bool, ixVars []string) []vField {
	var out []vField
	if g.noStd {
		out = append(out, vField{"stack", fmt.Sprintf("heapless::Vec<_Loc, %d>", stackCap), "heapless::Vec::new()", false})
	} else {
		out = append(out, vField{"stack", "Vec<_Loc>", "Vec::new()", false})
	}
	out = append(out,
		vField{"cur", "_Loc", "_Loc::Root", true},
		vField{"dead", "u16", "0", true})
	if needAcc {
		out = append(out, vField{"acc", accType, accNew, false})
	}
	out = append(out,
		vField{"err", "bool", "false", true},
		vField{"inv", "bool", "false", true})
	if g.limits.any() {
		out = append(out, vField{"lim", "bool", "false", true})
	}
	if arrSkip {
		out = append(out, vField{"askip", "usize", "0", true})
	}
	if scalarArray {
		out = append(out, vField{"afill", "usize", "0", true})
	}
	// One index slot per wrapper-array frame that decodes ELEMENTS at an id --
	// struct/union elements, wrapper rows and native rows: the array index of the
	// element currently being decoded (generator#247). It must survive between feed
	// calls, since an element can straddle a chunk boundary.
	for _, ix := range ixVars {
		out = append(out, vField{ix, "usize", "0", true})
	}
	return out
}
