package csharp

import "github.com/sofa-buffers/generator/internal/ir"

type frame struct {
	loc      string
	path     string
	fields   []*ir.Field
	seqArr   bool
	elemKind ir.Kind
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walk func(loc, path string, fields []*ir.Field)
	walk = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walk(loc+"_"+fld.Name, path+"."+fld.Name, fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob):
				out = append(out, frame{loc: loc + "_" + fld.Name, path: path + "." + fld.Name, seqArr: true, elemKind: fld.Elem})
			}
		}
	}
	walk("Root", "m", m.Fields)
	return out
}

func (g *gen) emitVisitor(f *cfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})

	f.line("internal sealed class %sVisitor : IVisitor {", name)
	f.line("    private readonly %s m;", name)
	f.line("    private int cur = 0;")
	f.line("    private readonly Stack<int> stack = new();")
	f.line("    private readonly List<byte> acc = new();")
	f.line("    public %sVisitor(%s msg) { m = msg; }", name, name)
	for i, fr := range fs {
		f.line("    private const int %s = %d;", fr.loc, i)
	}
	f.blank()

	// Unsigned: u*/bitfield scalars, bool, unsigned array elements
	f.line("    public void Unsigned(int id, ulong value) {")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindU8 || fld.Kind == ir.KindU16 || fld.Kind == ir.KindU32 || fld.Kind == ir.KindU64:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, fld.Name, g.csType(fld))
			case fld.Kind == ir.KindBitfield:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, fld.Name, g.typeName(fld.Ref.Key))
			case fld.Kind == ir.KindBool:
				f.line("            case (%s, %d): %s.%s = value != 0; break;", fr.loc, fld.ID, fr.path, fld.Name)
			case fld.Kind == ir.KindArray && isUnsignedElem(fld.Elem):
				f.line("            case (%s, %d): %s.%s.Add((%s)value); break;", fr.loc, fld.ID, fr.path, fld.Name, numCsType(fld.Elem))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// Signed: i*/enum scalars, signed array elements
	f.line("    public void Signed(int id, long value) {")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, fld.Name, g.csType(fld))
			case fld.Kind == ir.KindEnum:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, fld.Name, g.typeName(fld.Ref.Key))
			case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
				f.line("            case (%s, %d): %s.%s.Add((%s)value); break;", fr.loc, fld.ID, fr.path, fld.Name, numCsType(fld.Elem))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	g.emitFloatVisit(f, fs, ir.KindFP32, "Fp32", "float")
	g.emitFloatVisit(f, fs, ir.KindFP64, "Fp64", "double")

	// String
	f.line("    public void String(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("        if (acc.Count < total) return;")
	f.line("        var _s = Encoding.UTF8.GetString(acc.ToArray());")
	f.line("        acc.Clear();")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.seqArr && fr.elemKind == ir.KindString {
			f.line("            case (%s, _): %s.Add(_s); break;", fr.loc, fr.path)
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindString {
				f.line("            case (%s, %d): %s.%s = _s; break;", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// Blob
	f.line("    public void Blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("        if (acc.Count < total) return;")
	f.line("        var _b = acc.ToArray();")
	f.line("        acc.Clear();")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.seqArr && fr.elemKind == ir.KindBlob {
			f.line("            case (%s, _): %s.Add(_b); break;", fr.loc, fr.path)
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindBlob {
				f.line("            case (%s, %d): %s.%s = _b; break;", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// ArrayBegin clears the list
	f.line("    public void ArrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && fld.Elem != ir.KindString && fld.Elem != ir.KindBlob {
				f.line("            case (%s, %d): %s.%s.Clear(); break;", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// SequenceBegin / SequenceEnd
	f.line("    public void SequenceBegin(int id) {")
	f.line("        stack.Push(cur);")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				f.line("            case (%s, %d): cur = %s; break;", fr.loc, fld.ID, fr.loc+"_"+fld.Name)
			case fld.Kind == ir.KindArray && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob):
				f.line("            case (%s, %d): %s.%s.Clear(); cur = %s; break;", fr.loc, fld.ID, fr.path, fld.Name, fr.loc+"_"+fld.Name)
			}
		}
	}
	f.line("        }")
	f.line("    }")
	f.line("    public void SequenceEnd() { cur = stack.Count > 0 ? stack.Pop() : 0; }")
	f.line("}")
	f.blank()
}

func (g *gen) emitFloatVisit(f *cfile, fs []frame, kind ir.Kind, cb, ctype string) {
	f.line("    public void %s(int id, %s value) {", cb, ctype)
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == kind:
				f.line("            case (%s, %d): %s.%s = value; break;", fr.loc, fld.ID, fr.path, fld.Name)
			case fld.Kind == ir.KindArray && fld.Elem == kind:
				f.line("            case (%s, %d): %s.%s.Add(value); break;", fr.loc, fld.ID, fr.path, fld.Name)
			}
		}
	}
	f.line("        }")
	f.line("    }")
}

func isUnsignedElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
}
func isSignedElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
}
