package rust

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// Decoding into a reused destination (generator#755), corelib-rs only (see
// gen.reuse for why corelib-rs-no-std does not take it).
//
// Two halves, and only the first is emitted per type:
//
//   - reset(): every field back to its schema default IN PLACE. A string, blob or
//     array is cleared and refilled from its declared default rather than
//     assigned a new container, so it keeps the heap capacity it holds; a nested
//     struct is reset and a union `_reset`. Its body is the schema's field list
//     and defaults and nothing else -- per schema, which is why it is emitted.
//   - the element storage of a wrapper array: an array of strings keeps each
//     String, an array of structs each struct. That logic is the same for every
//     schema -- a live count beside the container, existing slots reset in
//     place, the tail truncated at the end -- so it is corelib-rs's
//     `sofab::seq::reuse_leaf` / `reuse_elem` / `reuse_row` and
//     `SeqVec::truncate` (corelib-rs#117), and the generated code only names
//     which container, which live count and which element reset.
//
// The live counts exist only for a wrapper array reached through plain fields
// from the message (a FIXED path, frame.live): one level deep, as the corelib's
// protocol is. An array below an array element or inside a union option is
// reset with its owner and decoded through the `reserve_*` collectors as before.

// ownsStorage reports whether a field of kind k holds storage a reset can keep:
// a container, or an aggregate that may hold one.
func ownsStorage(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindArray, ir.KindStruct, ir.KindUnion:
		return true
	}
	return false
}

// resetStmts is the in-place reset of one field: lv is its assignable place
// (`self.x`, `*v`), recv the receiver a method is called on (`self.x`, `v`).
//
// A container is cleared, which keeps its capacity, and refilled from the
// declared default; a heapless container's refill returns a Result the default
// always satisfies (the schema checks a default against its bound). A struct is
// reset and a union `_reset`, recursively. Everything else is assigned the
// expression Default uses, so the two cannot drift.
func (g *gen) resetStmts(lv, recv string, fld *ir.Field) []string {
	fixed := g.storage(fld) == generator.StorageFixed
	fill := func(call string) string {
		if fixed {
			return "let _ = " + call + ";"
		}
		return call + ";"
	}
	switch fld.Kind {
	case ir.KindString:
		out := []string{recv + ".clear();"}
		if lit, ok := fld.Default.(string); ok && lit != "" {
			out = append(out, fill(fmt.Sprintf("%s.push_str(%q)", recv, lit)))
		}
		return out
	case ir.KindBlob:
		out := []string{recv + ".clear();"}
		if raw, ok := g.blobBytes(fld); ok {
			out = append(out, fill(fmt.Sprintf("%s.extend_from_slice(&%s)", recv, byteSliceLit(raw))))
		}
		return out
	case ir.KindArray:
		out := []string{recv + ".clear();"}
		if isNativeArrayElem(fld.Elem) {
			if parts, ok := g.rustNativeArrayParts(fld); ok {
				out = append(out, fill(fmt.Sprintf("%s.extend_from_slice(&[%s])", recv, parts)))
			}
		}
		return out
	case ir.KindStruct:
		return []string{recv + ".reset();"}
	case ir.KindUnion:
		return []string{recv + "._reset();"}
	}
	return []string{fmt.Sprintf("%s = %s;", lv, g.rustFieldDefault(fld))}
}

// emitReset emits a struct's or a message's public reset() into its impl block.
func (g *gen) emitReset(f *rfile, fields []*ir.Field) {
	f.line("    /// Every field back to its schema default, in place: a string, blob or")
	f.line("    /// array is cleared and refilled from its default, so it keeps the heap")
	f.line("    /// capacity it holds, and a nested struct or union is reset the same way.")
	f.line("    /// The elements of an array are dropped; the array keeps its own buffer.")
	if len(fields) == 0 {
		f.line("    pub fn reset(&mut self) {}")
		return
	}
	f.line("    pub fn reset(&mut self) {")
	for _, fld := range fields {
		p := "self." + rustIdent(fld.Name)
		for _, s := range g.resetStmts(p, p, fld) {
			f.line("        %s", s)
		}
	}
	f.line("    }")
}

// emitUnionReset emits a union's `_reset()`: default_id at its own default, in
// place when it is the option held -- so the storage that option holds is kept,
// exactly as a struct member's is -- and assigned otherwise, which drops the
// option held before.
//
// It is private, and named in the `_` channel no schema name can reach (a
// schema name starts with a letter): a public `reset` would take a union
// option's getter name `reset`, and the union already answers the public
// question with `Default`. Its callers are the generated reset() of a type
// holding the union and the element reset handed to `sofab::seq::reuse_elem`;
// rustc's dead_code lint skips a `_` name, so a union no message reaches costs
// no warning.
func (g *gen) emitUnionReset(f *rfile, opts []*unionOpt) {
	var d *unionOpt
	for _, o := range opts {
		if o.isD {
			d = o
		}
	}
	f.line("    /// `%s` at its own default: in place when it is the option held, so the", d.f.Name)
	f.line("    /// storage it holds is kept; otherwise the option held is dropped.")
	if !ownsStorage(d.f.Kind) {
		f.line("    fn _reset(&mut self) {")
		f.line("        *self = Self::default();")
		f.line("    }")
		return
	}
	body := strings.Join(g.resetStmts("*v", "v", d.f), " ")
	f.line("    fn _reset(&mut self) {")
	if len(opts) == 1 {
		f.line("        let Self::%s(v) = self;", d.variant)
		f.line("        %s", body)
	} else {
		f.line("        match self {")
		f.line("            Self::%s(v) => { %s }", d.variant, body)
		f.line("            _ => *self = Self::default(),")
		f.line("        }")
	}
	f.line("    }")
}

// emitTryDecodeInto is corelib-rs's try_decode: the decode into the caller's
// destination. The destination is re-armed first -- every field back to its
// default, the reused wrapper arrays' slots kept for the decode to write into --
// then fed, and the arrays cut to their live counts whatever the verdict.
func (g *gen) emitTryDecodeInto(f *rfile, name, vInit string, fields []*ir.Field, lives []frame) {
	f.line("    pub fn try_decode(data: &[u8], m: &mut %s) -> Result<(), DecodeError> {", name)
	switch stmts, _, _ := g.rearmStmts(fields); {
	case len(lives) == 0:
		f.line("        m.reset();")
	case len(stmts) > 0:
		f.line("        _rearm(m);")
	default:
		// Every field is a reused array: the live counts start at 0 and _trim
		// cuts what the decode does not reach, so there is nothing to re-arm.
	}
	g.emitTryDecodeBody(f, vInit, lives)
	f.line("        Ok(())")
	f.line("    }")
	f.blank()
}

// trimAllows / rearmAllows are the lints a module function reaching a message's
// fields directly can trip: a deprecated field, and a float default spelled as
// declared (approxConstantAllow).
func (g *gen) fnAllows(f *rfile, deprecated, float bool) {
	if deprecated {
		f.line("    #[allow(deprecated)]")
	}
	if float {
		f.line("    %s", approxConstantAllow)
	}
}

// emitTrim emits _trim: each reused wrapper array cut to its live count, through
// the corelib's SeqVec::truncate, which keeps the container's own buffer.
func (g *gen) emitTrim(f *rfile, fs []frame, name string, lives []frame) {
	if len(lives) == 0 {
		return
	}
	deprecated := false
	for _, fr := range fs {
		deprecated = deprecated || fieldsHaveDeprecated(fr.fields)
	}
	f.line("    /// Cut each reused wrapper array to the slots this decode made. Run after")
	f.line("    /// every decode, a refused one included: an omitted array, a cut-off input")
	f.line("    /// and an error never reach the array's end, so the tail an earlier decode")
	f.line("    /// left there is cut here.")
	g.fnAllows(f, deprecated, false)
	f.line("    fn _trim(m: &mut %s, lv: [usize; %d]) {", name, len(lives))
	for _, fr := range lives {
		f.line("        sofab::seq::SeqVec::truncate(&mut %s, lv[%d]);", "m"+strings.TrimPrefix(fr.path, "self.m"), fr.live)
	}
	f.line("    }")
	f.blank()
}

// hasFixedArray reports whether a field list holds a wrapper array through plain
// fields: directly, or in a struct member (a union option is not a plain field).
// Exactly those arrays get a live count (frame.live), so exactly those must be
// left alone by _rearm.
func hasFixedArray(fields []*ir.Field) bool {
	for _, fld := range fields {
		switch {
		case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
			return true
		case fld.Kind == ir.KindStruct && hasFixedArray(fld.Ref.Target.Fields):
			return true
		}
	}
	return false
}

// emitRearm emits _rearm: the message's reset(), except that a reused wrapper
// array is not cleared. Its slots are the storage the decode writes into; its
// live count starts at 0 and _trim cuts what the decode did not reach. A struct
// member that holds such an array is walked into for the same reason, any other
// one is reset() whole.
func (g *gen) emitRearm(f *rfile, fs []frame, name string, fields []*ir.Field, lives []frame) {
	stmts, deprecated, float := g.rearmStmts(fields)
	if len(lives) == 0 || len(stmts) == 0 {
		return
	}
	f.line("    /// `%s::reset`, except that a reused wrapper array keeps its slots: they", name)
	f.line("    /// are the storage the decode writes into, and _trim cuts what it did not")
	f.line("    /// reach.")
	g.fnAllows(f, deprecated, float)
	f.line("    fn _rearm(m: &mut %s) {", name)
	for _, s := range stmts {
		f.line("        %s", s)
	}
	f.line("    }")
	f.blank()
}

// rearmStmts is _rearm's body over a message's fields `m.<field>`, with whether
// it reaches a deprecated field and spells a float default (for its allows).
func (g *gen) rearmStmts(fields []*ir.Field) (stmts []string, deprecated, float bool) {
	var walk func(prefix string, fields []*ir.Field)
	walk = func(prefix string, fields []*ir.Field) {
		for _, fld := range fields {
			p := prefix + "." + rustIdent(fld.Name)
			switch {
			case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
				continue
			case fld.Kind == ir.KindStruct && hasFixedArray(fld.Ref.Target.Fields):
				// Reached through the member: a deprecated one needs the allow too.
				deprecated = deprecated || fld.Deprecated
				walk(p, fld.Ref.Target.Fields)
				continue
			}
			deprecated = deprecated || fld.Deprecated
			float = float || g.fieldsHaveFloatDefault([]*ir.Field{fld})
			stmts = append(stmts, g.resetStmts(p, p, fld)...)
		}
	}
	walk("m", fields)
	return stmts, deprecated, float
}

// reuseLeaf is the reservation of a string/blob element slot of a reused wrapper
// array: corelib-rs's reuse_leaf with the element type's own clear() as the
// reset, so a slot an earlier decode left keeps its buffer.
func (g *gen) reuseLeaf(fr frame) string {
	elem := g.rustArrayElem(fr.elemKind, nil, nil, fr.emax >= 0, fr.emax)
	return fmt.Sprintf("sofab::seq::reuse_leaf(&mut %s, %s, id, %s, <%s>::clear)", fr.path, liveCount(fr), g.seqBound(fr.cap), elem)
}

// emitStringInPlace is corelib-rs's string() for dynamic storage: the payload is
// written into the String the destination already holds (clear + push_str), so a
// reused destination keeps its buffer, instead of a new String per field.
func (g *gen) emitStringInPlace(f *rfile, fs []frame) {
	f.line("        // A Rust string type is Unicode, so a string is always strict. Invalid")
	f.line("        // UTF-8 is the INVALID decode outcome (self.inv -> Error::InvalidMsg),")
	f.line("        // never a lossy U+FFFD and never empty; the two Rust profiles agree")
	f.line("        // (subsumes #80). The verdict is passed on the ASSEMBLED payload, which")
	f.line("        // is why it sits after the feed and not per chunk.")
	g.emitPayloadFeed(f, "_p")
	f.line("        let _s = match core::str::from_utf8(_p) { Ok(_v) => _v, Err(_) => { self.inv = true; \"\" } };")
	g.emitLeafInPlace(f, fs, ir.KindString, "_s", "push_str")
}

// emitBlobInPlace is emitStringInPlace's blob twin (extend_from_slice).
func (g *gen) emitBlobInPlace(f *rfile, fs []frame) {
	g.emitPayloadFeed(f, "_b")
	g.emitLeafInPlace(f, fs, ir.KindBlob, "_b", "extend_from_slice")
}

// emitLeafInPlace emits the completion-store match of a string or blob: every
// destination is cleared and written into. A repeated id replaces (§7.4), as the
// assignment it supersedes did.
func (g *gen) emitLeafInPlace(f *rfile, fs []frame, kind ir.Kind, val, write string) {
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		if fr.kind == fkSeqArr && fr.elemKind == kind {
			if fr.live >= 0 {
				f.line("            (_Loc::%s, _) => { %smatch %s { Ok(_e) => _e.%s(%s), Err(_e) => { %s } } }", fr.loc, g.limGate(fr.cap), g.reuseLeaf(fr), write, val, seqRefuse)
				continue
			}
			f.line("            (_Loc::%s, _) => { %smatch sofab::seq::reserve_elem(&mut %s, id, %s) { Ok(_e) => { _e.clear(); _e.%s(%s); } Err(_e) => { %s } } }", fr.loc, g.limGate(fr.cap), fr.path, g.seqBound(fr.cap), write, val, seqRefuse)
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind {
				f.line("            (_Loc::%s, %d) => { let _d = %s; _d.clear(); _d.%s(%s); }", fr.loc, fld.ID, memberRef(fr, fld), write, val)
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")
}
