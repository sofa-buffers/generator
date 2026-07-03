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
	var addArray func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)

	walkFields = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, kind: fkStruct, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				cl := loc + "_" + fld.Name
				walkFields(cl, path+"."+rustIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+rustIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems)
			}
		}
	}

	// addArray builds the frame(s) for a wrapper-sequence array whose Vec is at
	// (loc, path) and whose element is (elem, ref, items).
	addArray = func(loc, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{loc: loc, path: path, kind: fkSeqArr, elemKind: elem})
		case ir.KindStruct, ir.KindUnion:
			el := loc + "_e"
			out = append(out, frame{loc: loc, path: path, kind: fkStructArr, elemLoc: el})
			walkFields(el, path+".last_mut().unwrap()", ref.Target.Fields)
		case ir.KindArray:
			// The element is an inner array (items). A native inner array is handled
			// by a single wrapper frame (array_begin pushes a new inner Vec, elements
			// push to the last); a wrapper inner array descends recursively.
			if isNativeArrayElem(items.Elem) {
				out = append(out, frame{loc: loc, path: path, kind: fkNestedNative, elemKind: items.Elem, elemRef: items.ElemRef})
			} else {
				el := loc + "_e"
				out = append(out, frame{loc: loc, path: path, kind: fkArrArr, elemLoc: el})
				addArray(el, path+".last_mut().unwrap()", items.Elem, items.ElemRef, items.ElemItems)
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

func (g *gen) emitVisitor(f *rfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	use := visitorUseOf(fs)

	// Wrap the decoder in a private module so _Loc / V don't clash across
	// messages in a multi-message crate.
	f.line("mod %s_dec {", strings.ToLower(name))
	f.line("    use super::*;")
	// ArrayKind is gated behind the no-std `array` feature; import it only when an
	// array_begin override is emitted (i.e. the message has a native array).
	arrayKind := ""
	if use.scalarArray {
		arrayKind = ", ArrayKind"
	}
	f.line("    use sofab::{IStream, Visitor, Id, Unsigned, Signed%s};", arrayKind)
	f.blank()
	f.line("    pub fn decode(data: &[u8]) -> %s {", name)
	f.line("        let mut m = %s::default();", name)
	f.line("        {")
	f.line("            let mut v = V { m: &mut m, stack: Vec::new(), cur: _Loc::Root, acc: Vec::new(), ai: 0 };")
	f.line("            let mut is = IStream::new();")
	f.line("            let _ = is.feed(data, &mut v);")
	f.line("        }")
	f.line("        m")
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
	f.line("    stack: Vec<_Loc>,")
	f.line("    cur: _Loc,")
	f.line("    acc: Vec<u8>,")
	f.line("    ai: usize, // index into the fixed native array currently being filled")
	f.line("}")
	f.blank()

	f.line("impl<'a> Visitor for V<'a> {")

	// unsigned: u*/bitfield scalars, bool, and unsigned/bool/bitfield array elements
	f.line("    fn unsigned(&mut self, id: Id, value: Unsigned) {")
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
			switch {
			case isUnsignedElem(fr.elemKind):
				f.line("            (_Loc::%s, _) => %s.last_mut().unwrap().push(value as %s),", fr.loc, fr.path, numRustType(fr.elemKind))
			case fr.elemKind == ir.KindBool:
				f.line("            (_Loc::%s, _) => %s.last_mut().unwrap().push(value != 0),", fr.loc, fr.path)
			case fr.elemKind == ir.KindBitfield:
				f.line("            (_Loc::%s, _) => %s.last_mut().unwrap().push(value as %s),", fr.loc, fr.path, bitfieldBacking(fr.elemRef.Target))
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	// signed: i*/enum scalars + signed/enum array elements
	f.line("    fn signed(&mut self, id: Id, value: Signed) {")
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
			switch {
			case isSignedElem(fr.elemKind):
				f.line("            (_Loc::%s, _) => %s.last_mut().unwrap().push(value as %s),", fr.loc, fr.path, numRustType(fr.elemKind))
			case fr.elemKind == ir.KindEnum:
				f.line("            (_Loc::%s, _) => %s.last_mut().unwrap().push(value as %s),", fr.loc, fr.path, enumBacking(fr.elemRef.Target))
			}
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
		f.line("        // Single-shot: whole payload in one chunk -> build straight from the")
		f.line("        // slice, skipping the `acc` accumulate + second copy.")
		f.line("        let _s = if offset == 0 && chunk.len() >= total {")
		f.line("            String::from_utf8_lossy(&chunk[..total]).into_owned()")
		f.line("        } else {")
		f.line("            self.acc.extend_from_slice(chunk);")
		f.line("            if self.acc.len() < total { return; }")
		f.line("            let s = String::from_utf8_lossy(&self.acc).into_owned();")
		f.line("            self.acc.clear();")
		f.line("            s")
		f.line("        };")
		f.line("        match (self.cur, id) {")
		for _, fr := range fs {
			if fr.kind == fkSeqArr && fr.elemKind == ir.KindString {
				f.line("            (_Loc::%s, _) => %s.push(_s),", fr.loc, fr.path)
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

	if use.blob {
		// blob: scalar blobs + blob-array elements
		f.line("    fn blob(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {")
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
				f.line("            (_Loc::%s, _) => %s.push(_b),", fr.loc, fr.path)
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

	if use.scalarArray {
		// array_begin clears a native-array target (scalar array field) or starts a
		// fresh inner Vec (nested native array).
		// Reset the fixed-array fill index for every array. Fixed `[T; N]` fields are
		// pre-allocated in the struct default, so they need no per-begin action; a
		// dynamic native array clears its Vec; a nested-native scope pushes a fresh
		// inner Vec.
		f.line("    fn array_begin(&mut self, id: Id, _kind: ArrayKind, _count: usize) {")
		f.line("        self.ai = 0;")
		f.line("        match (self.cur, id) {")
		for _, fr := range fs {
			switch fr.kind {
			case fkStruct:
				for _, fld := range fr.fields {
					if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
						if _, _, ok := g.fixedNativeArray(fld); ok {
							continue // fixed [T; N]: nothing to clear
						}
						f.line("            (_Loc::%s, %d) => %s.%s.clear(),", fr.loc, fld.ID, fr.path, rustIdent(fld.Name))
					}
				}
			case fkNestedNative:
				f.line("            (_Loc::%s, _) => %s.push(Vec::new()),", fr.loc, fr.path)
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
		f.line("        self.stack.push(self.cur);")
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
				f.line("            (_Loc::%s, _) => { %s.push(Default::default()); _Loc::%s },", fr.loc, fr.path, fr.elemLoc)
			case fkArrArr:
				f.line("            (_Loc::%s, _) => { %s.push(Vec::new()); _Loc::%s },", fr.loc, fr.path, fr.elemLoc)
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

// emitNativeArrayStore emits one match arm for a direct native array element: an
// indexed store `x[self.ai] = rhs; self.ai += 1;` for a fixed `[T; N]` array, or
// a `.push(rhs)` for a dynamic (count-less) `Vec` array.
func (g *gen) emitNativeArrayStore(f *rfile, fr frame, fld *ir.Field, rhs string) {
	if _, _, ok := g.fixedNativeArray(fld); ok {
		f.line("            (_Loc::%s, %d) => { %s.%s[self.ai] = %s; self.ai += 1; }", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), rhs)
		return
	}
	f.line("            (_Loc::%s, %d) => %s.%s.push(%s),", fr.loc, fld.ID, fr.path, rustIdent(fld.Name), rhs)
}

func (g *gen) emitFloatVisit(f *rfile, fs []frame, kind ir.Kind, cb, rtype string) {
	f.line("    fn %s(&mut self, id: Id, value: %s) {", cb, rtype)
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		if fr.kind == fkNestedNative && fr.elemKind == kind {
			f.line("            (_Loc::%s, _) => %s.last_mut().unwrap().push(value),", fr.loc, fr.path)
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

func isUnsignedElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
}
func isSignedElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
}

var _ = strings.TrimSpace
var _ = fmt.Sprintf
