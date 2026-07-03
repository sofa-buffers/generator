package java

import (
	"fmt"
	"strconv"

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
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walk func(loc, path string, fields []*ir.Field)
	var addArray func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)
	walk = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{kind: fkNormal, loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walk(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems)
			}
		}
	}
	// addArray registers the frame(s) entered inside the wrapper sequence of a
	// sequence-typed array (string/blob/struct/union/nested). listExpr is the List
	// accessor the frame collects into; `get` reaches the just-added last element.
	addArray = func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		get := listExpr + ".get(" + listExpr + ".size()-1)"
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{kind: fkSeqLeaf, loc: loc, listExpr: listExpr, elemKind: elem})
		case ir.KindStruct, ir.KindUnion:
			elemLoc := loc + "_e"
			out = append(out, frame{kind: fkSeqObj, loc: loc, listExpr: listExpr, childLoc: elemLoc, elemType: g.typeName(ref.Key)})
			walk(elemLoc, get, ref.Target.Fields)
		case ir.KindArray:
			if nativeArrayElem(items.Elem) {
				out = append(out, frame{kind: fkNativeMat, loc: loc, listExpr: listExpr, innerElem: items.Elem, innerRef: items.ElemRef})
			} else {
				innerLoc := loc + "_e"
				out = append(out, frame{kind: fkSeqMat, loc: loc, listExpr: listExpr, childLoc: innerLoc})
				addArray(innerLoc, get, items.Elem, items.ElemRef, items.ElemItems)
			}
		}
	}
	walk("Root", "m", m.Fields)
	for i := range out {
		out[i].idx = i
	}
	return out
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

func (g *gen) emitVisitor(f *jfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})

	f.line("class %sVisitor implements Visitor {", name)
	f.line("    private final %s m;", name)
	f.line("    private int cur = 0;")
	f.line("    private int ai = 0;                 // index into the primitive array currently being filled")
	f.line("    private int[] stk = new int[16];    // sequence scope stack (unboxed, was ArrayDeque<Integer>)")
	f.line("    private int sp = 0;")
	f.line("    private final java.io.ByteArrayOutputStream acc = new java.io.ByteArrayOutputStream();")
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
				return "addBool", true // boolean array stays List<Boolean>
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

	// string. Single-shot: when the whole payload arrives in one chunk, decode
	// straight from the input slice, skipping the (synchronized) ByteArrayOutputStream.
	f.line("    public void string(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        String _s;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _s = new String(data, chunkOffset, total, java.nio.charset.StandardCharsets.UTF_8);")
	f.line("        } else {")
	f.line("            acc.write(data, chunkOffset, chunkLength);")
	f.line("            if (acc.size() < total) return;")
	f.line("            _s = new String(acc.toByteArray(), java.nio.charset.StandardCharsets.UTF_8);")
	f.line("            acc.reset();")
	f.line("        }")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindString {
			f.line("        case %d: %s.add(_s); break;", fr.idx, fr.listExpr)
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
	f.line("    }")

	// blob. Single-shot on the whole-in-one-chunk fast path (see string).
	f.line("    public void blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        byte[] _b;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _b = java.util.Arrays.copyOfRange(data, chunkOffset, chunkOffset + total);")
	f.line("        } else {")
	f.line("            acc.write(data, chunkOffset, chunkLength);")
	f.line("            if (acc.size() < total) return;")
	f.line("            _b = acc.toByteArray();")
	f.line("            acc.reset();")
	f.line("        }")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindBlob {
			f.line("        case %d: %s.add(_b); break;", fr.idx, fr.listExpr)
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

	// arrayBegin: a primitive array is right-sized to `count` and filled by index
	// (ai reset below); a boolean array clears its List; native-matrix rows append
	// a new inner list.
	f.line("    public void arrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        ai = 0;")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkNativeMat {
			f.line("        case %d: %s.add(new ArrayList<>()); break;", fr.idx, fr.listExpr)
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && primitiveArrayElem(fld.Elem) {
				target := fr.path + "." + javaIdent(fld.Name)
				arms = append(arms, jcase(fld.ID, target+" = new "+primArrayBase(fld.Elem)+"[count]"))
			} else if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) { // boolean List
				arms = append(arms, jcase(fld.ID, fr.path+"."+javaIdent(fld.Name)+".clear()"))
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
			f.line("        case %d: %s.add(new %s()); cur = %d; break;", fr.idx, fr.listExpr, fr.elemType, locIndex(fs, fr.childLoc))
		case fkSeqMat:
			f.line("        case %d: %s.add(new ArrayList<>()); cur = %d; break;", fr.idx, fr.listExpr, locIndex(fs, fr.childLoc))
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
	f.line("    public void sequenceEnd() { cur = sp > 0 ? stk[--sp] : 0; }")
	f.line("}")
	f.blank()
}

// emitScalarCb writes a callback that routes (cur,id) to a field assignment or a
// list .add. action() returns "= value" / "add" / "addBool" / "= value != 0".
// Native-matrix frames whose inner element matches this callback append the
// decoded value to the current row (no id switch: rows arrive index-ordered).
func (g *gen) emitScalarCb(f *jfile, fs []frame, cb, vtype string, action func(*ir.Field) (string, bool)) {
	f.line("    public void %s(int id, %s value) {", cb, vtype)
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkNativeMat {
			if nativeElemCb(fr.innerElem) == cb {
				row := fr.listExpr + ".get(" + fr.listExpr + ".size()-1)"
				f.line("        case %d: %s.add(%s); break;", fr.idx, row, matConv(fr.innerElem))
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
			var stmt string
			switch act {
			case "add":
				stmt = target + ".add(value)"
			case "addBool":
				stmt = target + ".add(value != 0)"
			case "index":
				stmt = target + "[ai++] = value"
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
