package java

import (
	"fmt"
	"strconv"

	"github.com/sofa-buffers/generator/internal/ir"
)

type frame struct {
	loc      string
	idx      int
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
				walk(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob):
				out = append(out, frame{loc: loc + "_" + fld.Name, path: path + "." + javaIdent(fld.Name), seqArr: true, elemKind: fld.Elem})
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
	f.line("    private final java.util.Deque<Integer> stack = new java.util.ArrayDeque<>();")
	f.line("    private final java.io.ByteArrayOutputStream acc = new java.io.ByteArrayOutputStream();")
	f.line("    %sVisitor(%s msg) { m = msg; }", name, name)
	f.blank()

	// unsigned: u*/bitfield scalars, bool, unsigned array elements
	g.emitScalarCb(f, fs, "unsigned", "long", func(fld *ir.Field) (string, bool) {
		switch {
		case fld.Kind == ir.KindU8 || fld.Kind == ir.KindU16 || fld.Kind == ir.KindU32 || fld.Kind == ir.KindU64 || fld.Kind == ir.KindBitfield:
			return "= value", true
		case fld.Kind == ir.KindBool:
			return "= value != 0", true
		case fld.Kind == ir.KindArray && isUnsignedElem(fld.Elem):
			return "add", true
		}
		return "", false
	})

	g.emitScalarCb(f, fs, "signed", "long", func(fld *ir.Field) (string, bool) {
		switch {
		case fld.Kind == ir.KindI8 || fld.Kind == ir.KindI16 || fld.Kind == ir.KindI32 || fld.Kind == ir.KindI64 || fld.Kind == ir.KindEnum:
			return "= value", true
		case fld.Kind == ir.KindArray && isSignedElem(fld.Elem):
			return "add", true
		}
		return "", false
	})

	g.emitScalarCb(f, fs, "fp32", "float", func(fld *ir.Field) (string, bool) {
		if fld.Kind == ir.KindFP32 {
			return "= value", true
		}
		if fld.Kind == ir.KindArray && fld.Elem == ir.KindFP32 {
			return "add", true
		}
		return "", false
	})
	g.emitScalarCb(f, fs, "fp64", "double", func(fld *ir.Field) (string, bool) {
		if fld.Kind == ir.KindFP64 {
			return "= value", true
		}
		if fld.Kind == ir.KindArray && fld.Elem == ir.KindFP64 {
			return "add", true
		}
		return "", false
	})

	// string
	f.line("    public void string(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        acc.write(data, chunkOffset, chunkLength);")
	f.line("        if (acc.size() < total) return;")
	f.line("        String _s = new String(acc.toByteArray(), java.nio.charset.StandardCharsets.UTF_8);")
	f.line("        acc.reset();")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		var arms []string
		if fr.seqArr && fr.elemKind == ir.KindString {
			f.line("        case %d: %s.add(_s); break;", fr.idx, fr.path)
			continue
		}
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

	// blob
	f.line("    public void blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	f.line("        acc.write(data, chunkOffset, chunkLength);")
	f.line("        if (acc.size() < total) return;")
	f.line("        byte[] _b = acc.toByteArray();")
	f.line("        acc.reset();")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.seqArr && fr.elemKind == ir.KindBlob {
			f.line("        case %d: %s.add(_b); break;", fr.idx, fr.path)
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

	// arrayBegin clears the list
	f.line("    public void arrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && fld.Elem != ir.KindString && fld.Elem != ir.KindBlob {
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
	f.line("        stack.push(cur);")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		var arms []string
		for _, fld := range fr.fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				arms = append(arms, jcase(fld.ID, "cur = "+itoa(locIndex(fs, fr.loc+"_"+fld.Name))))
			case fld.Kind == ir.KindArray && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob):
				arms = append(arms, jcase(fld.ID, fr.path+"."+javaIdent(fld.Name)+".clear(); cur = "+itoa(locIndex(fs, fr.loc+"_"+fld.Name))))
			}
		}
		if len(arms) > 0 {
			g.frameSwitch(f, fr.idx, arms)
		}
	}
	f.line("        }")
	f.line("    }")
	f.line("    public void sequenceEnd() { cur = stack.isEmpty() ? 0 : stack.pop(); }")
	f.line("}")
	f.blank()
}

// emitScalarCb writes a callback that routes (cur,id) to a field assignment or a
// list .add. action() returns "= value" / "add" / "= value != 0".
func (g *gen) emitScalarCb(f *jfile, fs []frame, cb, vtype string, action func(*ir.Field) (string, bool)) {
	f.line("    public void %s(int id, %s value) {", cb, vtype)
	f.line("        switch (cur) {")
	for _, fr := range fs {
		var arms []string
		for _, fld := range fr.fields {
			act, ok := action(fld)
			if !ok {
				continue
			}
			target := fr.path + "." + javaIdent(fld.Name)
			var stmt string
			if act == "add" {
				stmt = target + ".add(value)"
			} else {
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

func itoa(i int) string     { return strconv.Itoa(i) }
func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
