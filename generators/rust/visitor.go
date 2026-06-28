package rust

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// frame is one sequence container reachable from a message: the root, a
// struct/union field, or an array-of-string/blob field. loc is the _Loc variant;
// path is the Rust accessor (e.g. "self.m.somestruct.nestedstruct").
type frame struct {
	loc      string
	path     string
	fields   []*ir.Field // struct/union frames
	seqArr   bool        // array-of-string/blob frame
	elemKind ir.Kind
}

// frames walks a message and returns every sequence container, root first.
func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walk func(loc, path string, fields []*ir.Field)
	walk = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				cl := loc + "_" + fld.Name
				walk(cl, path+"."+fld.Name, fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob):
				out = append(out, frame{loc: loc + "_" + fld.Name, path: path + "." + fld.Name, seqArr: true, elemKind: fld.Elem})
			}
		}
	}
	walk("Root", "self.m", m.Fields)
	return out
}

func (g *gen) emitVisitor(f *rfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})

	// Wrap the decoder in a private module so _Loc / V don't clash across
	// messages in a multi-message crate.
	f.line("mod %s_dec {", strings.ToLower(name))
	f.line("    use super::*;")
	f.line("    use sofab::{IStream, Visitor, Id, Unsigned, Signed, ArrayKind};")
	f.blank()
	f.line("    pub fn decode(data: &[u8]) -> %s {", name)
	f.line("        let mut m = %s::default();", name)
	f.line("        {")
	f.line("            let mut v = V { m: &mut m, stack: Vec::new(), cur: _Loc::Root, acc: Vec::new() };")
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
	f.line("}")
	f.blank()

	f.line("impl<'a> Visitor for V<'a> {")

	// unsigned: u*/bitfield scalars, bool, and unsigned array elements
	f.line("    fn unsigned(&mut self, id: Id, value: Unsigned) {")
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindU8 || fld.Kind == ir.KindU16 || fld.Kind == ir.KindU32 || fld.Kind == ir.KindU64 || fld.Kind == ir.KindBitfield:
				f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, fld.Name, g.rustType(fld))
			case fld.Kind == ir.KindBool:
				f.line("            (_Loc::%s, %d) => %s.%s = value != 0,", fr.loc, fld.ID, fr.path, fld.Name)
			case fld.Kind == ir.KindArray && isUnsignedElem(fld.Elem):
				f.line("            (_Loc::%s, %d) => %s.%s.push(value as %s),", fr.loc, fld.ID, fr.path, fld.Name, numRustType(fld.Elem))
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	// signed: i*/enum scalars + signed array elements
	f.line("    fn signed(&mut self, id: Id, value: Signed) {")
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64:
				f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, fld.Name, g.rustType(fld))
			case fld.Kind == ir.KindEnum:
				f.line("            (_Loc::%s, %d) => %s.%s = value as %s,", fr.loc, fld.ID, fr.path, fld.Name, enumBacking(fld.Ref.Target))
			case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
				f.line("            (_Loc::%s, %d) => %s.%s.push(value as %s),", fr.loc, fld.ID, fr.path, fld.Name, numRustType(fld.Elem))
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	g.emitFloatVisit(f, fs, ir.KindFP32, "fp32", "f32")
	g.emitFloatVisit(f, fs, ir.KindFP64, "fp64", "f64")

	// string: scalar strings + string-array elements
	f.line("    fn string(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {")
	f.line("        self.acc.extend_from_slice(chunk);")
	f.line("        if self.acc.len() < total { return; }")
	f.line("        let _s = String::from_utf8_lossy(&self.acc).into_owned();")
	f.line("        self.acc.clear();")
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		if fr.seqArr && fr.elemKind == ir.KindString {
			f.line("            (_Loc::%s, _) => %s.push(_s),", fr.loc, fr.path)
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindString {
				f.line("            (_Loc::%s, %d) => %s.%s = _s,", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	// blob: scalar blobs + blob-array elements
	f.line("    fn blob(&mut self, id: Id, total: usize, offset: usize, chunk: &[u8]) {")
	f.line("        self.acc.extend_from_slice(chunk);")
	f.line("        if self.acc.len() < total { return; }")
	f.line("        let _b = self.acc.clone();")
	f.line("        self.acc.clear();")
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		if fr.seqArr && fr.elemKind == ir.KindBlob {
			f.line("            (_Loc::%s, _) => %s.push(_b),", fr.loc, fr.path)
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindBlob {
				f.line("            (_Loc::%s, %d) => %s.%s = _b,", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	// array_begin clears the target vec so element pushes start fresh
	f.line("    fn array_begin(&mut self, id: Id, _kind: ArrayKind, _count: usize) {")
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && fld.Elem != ir.KindString && fld.Elem != ir.KindBlob {
				f.line("            (_Loc::%s, %d) => %s.%s.clear(),", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("            _ => {}")
	f.line("        }")
	f.line("    }")

	// sequence_begin: push current, descend; clear seq-array vecs on entry
	f.line("    fn sequence_begin(&mut self, id: Id) {")
	f.line("        self.stack.push(self.cur);")
	f.line("        self.cur = match (self.cur, id) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				f.line("            (_Loc::%s, %d) => _Loc::%s,", fr.loc, fld.ID, fr.loc+"_"+fld.Name)
			case fld.Kind == ir.KindArray && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob):
				f.line("            (_Loc::%s, %d) => { %s.%s.clear(); _Loc::%s },", fr.loc, fld.ID, fr.path, fld.Name, fr.loc+"_"+fld.Name)
			}
		}
	}
	f.line("            _ => self.cur,")
	f.line("        };")
	f.line("    }")
	f.line("    fn sequence_end(&mut self) {")
	f.line("        self.cur = self.stack.pop().unwrap_or(_Loc::Root);")
	f.line("    }")

	f.line("}") // impl Visitor
	f.line("}") // mod <name>_dec
	f.blank()
}

func (g *gen) emitFloatVisit(f *rfile, fs []frame, kind ir.Kind, cb, rtype string) {
	f.line("    fn %s(&mut self, id: Id, value: %s) {", cb, rtype)
	f.line("        match (self.cur, id) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == kind:
				f.line("            (_Loc::%s, %d) => %s.%s = value,", fr.loc, fld.ID, fr.path, fld.Name)
			case fld.Kind == ir.KindArray && fld.Elem == kind:
				f.line("            (_Loc::%s, %d) => %s.%s.push(value),", fr.loc, fld.ID, fr.path, fld.Name)
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
