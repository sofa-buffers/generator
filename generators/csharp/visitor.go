package csharp

import "github.com/sofa-buffers/generator/internal/ir"

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
	elem     ir.Kind      // element kind of this array
	ref      *ir.TypeRef  // enum/bitfield/struct/union element
	items    *ir.ArrayElem // nested-array element descriptor
	childLoc string       // struct/union element or sequence-nested inner-array scope
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walkObj func(loc, path string, fields []*ir.Field)
	var walkArr func(loc, list string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)

	walkObj = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walkObj(loc+"_"+fld.Name, path+"."+csIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				walkArr(loc+"_"+fld.Name, path+"."+csIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems)
			}
		}
	}

	// walkArr registers the array scope entered on SequenceBegin(field/index),
	// plus any child scope its elements descend into.
	walkArr = func(loc, list string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		fr := frame{loc: loc, path: list, isArr: true, elem: elem, ref: ref, items: items}
		switch elem {
		case ir.KindStruct, ir.KindUnion:
			fr.childLoc = loc + "_e"
			out = append(out, fr)
			walkObj(fr.childLoc, lastElem(list), ref.Target.Fields)
		case ir.KindArray:
			if seqArrayElem(items.Elem) {
				fr.childLoc = loc + "_e"
				out = append(out, fr)
				walkArr(fr.childLoc, lastElem(list), items.Elem, items.ElemRef, items.ElemItems)
			} else {
				out = append(out, fr) // native inner rows collected in place
			}
		default: // string/blob
			out = append(out, fr)
		}
	}

	walkObj("Root", "m", m.Fields)
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

	// Unsigned: u*/bitfield scalars, bool, unsigned array elements (numeric/
	// boolean/bitfield), and native-nested unsigned inner rows.
	f.line("    public void Unsigned(int id, ulong value) {")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && unsignedArrayElem(fr.items.Elem) {
				f.line("            case (%s, _): %s.Add(%s); break;", fr.loc, lastElem(fr.path), g.arrayElemAddRHS(fr.items.Elem, fr.items.ElemRef, "value"))
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
			case fld.Kind == ir.KindArray && unsignedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s.%s.Add(%s); break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value"))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	// Signed: i*/enum scalars, signed array elements (numeric/enum), and
	// native-nested signed inner rows.
	f.line("    public void Signed(int id, long value) {")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && signedArrayElem(fr.items.Elem) {
				f.line("            case (%s, _): %s.Add(%s); break;", fr.loc, lastElem(fr.path), g.arrayElemAddRHS(fr.items.Elem, fr.items.ElemRef, "value"))
			}
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.csType(fld))
			case fld.Kind == ir.KindEnum:
				f.line("            case (%s, %d): %s.%s = (%s)value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.typeName(fld.Ref.Key))
			case fld.Kind == ir.KindArray && signedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s.%s.Add(%s); break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name), g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value"))
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
		if fr.isArr {
			if fr.elem == ir.KindString {
				f.line("            case (%s, _): %s.Add(_s); break;", fr.loc, fr.path)
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
	f.line("    }")

	// Blob
	f.line("    public void Blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("        if (acc.Count < total) return;")
	f.line("        var _b = acc.ToArray();")
	f.line("        acc.Clear();")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindBlob {
				f.line("            case (%s, _): %s.Add(_b); break;", fr.loc, fr.path)
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

	// ArrayBegin: clear direct native arrays; start a fresh inner row for a
	// native-nested (array-of-array) scope (each row arrives as ArrayBegin(index)).
	f.line("    public void ArrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && nativeArrayElem(fr.items.Elem) {
				f.line("            case (%s, _): %s.Add(new List<%s>()); break;", fr.loc, fr.path, g.csArrayElemType(fr.items.Elem, fr.items.ElemRef, fr.items.ElemItems))
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) {
				f.line("            case (%s, %d): %s.%s.Clear(); break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
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
	f.line("        stack.Push(cur);")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			switch {
			case fr.elem == ir.KindStruct || fr.elem == ir.KindUnion:
				f.line("            case (%s, _): %s.Add(new %s()); cur = %s; break;", fr.loc, fr.path, g.typeName(fr.ref.Key), fr.childLoc)
			case fr.elem == ir.KindArray && seqArrayElem(fr.items.Elem):
				f.line("            case (%s, _): %s.Add(new List<%s>()); cur = %s; break;", fr.loc, fr.path, g.csArrayElemType(fr.items.Elem, fr.items.ElemRef, fr.items.ElemItems), fr.childLoc)
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
	f.line("    public void SequenceEnd() { cur = stack.Count > 0 ? stack.Pop() : 0; }")
	f.line("}")
	f.blank()
}

func (g *gen) emitFloatVisit(f *cfile, fs []frame, kind ir.Kind, cb, ctype string) {
	f.line("    public void %s(int id, %s value) {", cb, ctype)
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && fr.items.Elem == kind {
				f.line("            case (%s, _): %s.Add(value); break;", fr.loc, lastElem(fr.path))
			}
			continue
		}
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == kind:
				f.line("            case (%s, %d): %s.%s = value; break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
			case fld.Kind == ir.KindArray && fld.Elem == kind:
				f.line("            case (%s, %d): %s.%s.Add(value); break;", fr.loc, fld.ID, fr.path, csIdent(fld.Name))
			}
		}
	}
	f.line("        }")
	f.line("    }")
}
