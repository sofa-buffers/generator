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
	// schema bounds, for the receiver-side decode limits (generator#102):
	elemMaxHas    bool // fkSeqLeaf: the string/blob element declares a maxlen
	innerHasCount bool // fkNativeMat: the inner array declares a count
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walk func(loc, path string, fields []*ir.Field)
	var addArray func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool)
	walk = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{kind: fkNormal, loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walk(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				addArray(loc+"_"+fld.Name, path+"."+javaIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, fld.ElemMaxHas)
			}
		}
	}
	// addArray registers the frame(s) entered inside the wrapper sequence of a
	// sequence-typed array (string/blob/struct/union/nested). listExpr is the List
	// accessor the frame collects into; `get` reaches the just-added last element.
	addArray = func(loc, listExpr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool) {
		get := listExpr + ".get(" + listExpr + ".size()-1)"
		switch elem {
		case ir.KindString, ir.KindBlob:
			out = append(out, frame{kind: fkSeqLeaf, loc: loc, listExpr: listExpr, elemKind: elem, elemMaxHas: elemMaxHas})
		case ir.KindStruct, ir.KindUnion:
			elemLoc := loc + "_e"
			out = append(out, frame{kind: fkSeqObj, loc: loc, listExpr: listExpr, childLoc: elemLoc, elemType: g.typeName(ref.Key)})
			walk(elemLoc, get, ref.Target.Fields)
		case ir.KindArray:
			if nativeArrayElem(items.Elem) {
				out = append(out, frame{kind: fkNativeMat, loc: loc, listExpr: listExpr, innerElem: items.Elem, innerRef: items.ElemRef, innerHasCount: items.HasCount})
			} else {
				innerLoc := loc + "_e"
				out = append(out, frame{kind: fkSeqMat, loc: loc, listExpr: listExpr, childLoc: innerLoc})
				addArray(innerLoc, get, items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas)
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

// activeLimits reports which receiver-side decode limits (generator#102) apply
// to this visitor: the limit must be configured AND the message must reach at
// least one schema-unbounded field the visitor can guard — an unbounded native
// array (count header via arrayBegin) or an unbounded string/blob (length via
// the `total` parameter). Otherwise no constant and no guard is emitted, so an
// unset or inert key leaves the output byte-identical.
func (g *gen) activeLimits(fs []frame) (limArr, limStr, limBlob bool) {
	for _, fr := range fs {
		switch fr.kind {
		case fkNormal:
			for _, fld := range fr.fields {
				switch {
				case fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) && !fld.HasCount:
					limArr = true
				case fld.Kind == ir.KindString && !fld.HasMaxlen:
					limStr = true
				case fld.Kind == ir.KindBlob && !fld.HasMaxlen:
					limBlob = true
				}
			}
		case fkSeqLeaf:
			if !fr.elemMaxHas {
				if fr.elemKind == ir.KindString {
					limStr = true
				} else {
					limBlob = true
				}
			}
		case fkNativeMat:
			if !fr.innerHasCount {
				limArr = true
			}
		}
	}
	return limArr && g.limits.arrayHas, limStr && g.limits.stringHas, limBlob && g.limits.blobHas
}

// limitThrow renders the generator#102 rejection: same unchecked-wrapper shape
// as the generator#100 schema guard (a Visitor callback cannot throw the
// checked SofabException), but with the LIMIT_EXCEEDED category — a receiver
// policy error, kept distinct from wire malformation.
func limitThrow(name, noun string, limit int64) string {
	return fmt.Sprintf("throw new java.io.UncheckedIOException(new SofabException(SofabError.LIMIT_EXCEEDED, \"%s: %s %d\"));",
		name, noun, limit)
}

// limitThrowGuard is limitThrow behind a condition, for arms that also do work
// when the guard passes.
func limitThrowGuard(cond, name, noun string, limit int64) string {
	return fmt.Sprintf("if (%s) %s", cond, limitThrow(name, noun, limit))
}

// locName is the human-readable field path of a frame loc for error details:
// the loc minus its "Root_" prefix (element hops keep their "_e" suffix).
func locName(loc string) string {
	if len(loc) > 5 && loc[:5] == "Root_" {
		return loc[5:]
	}
	return loc
}

// emitLenLimitGuard writes the receiver-side length guard (generator#102) at
// the very top of the string()/blob() callback: when the wire `total` exceeds
// the configured cap AND the (cur, id) destination is a schema-unbounded field
// of this kind, decoding fails with LIMIT_EXCEEDED before any byte is
// accumulated — the guard runs ahead of both the single-shot and the chunked
// path, so an oversized split payload is rejected on its first chunk.
// Schema-bounded fields fall through unaffected (governed by their own maxlen).
func (g *gen) emitLenLimitGuard(f *jfile, fs []frame, kind ir.Kind, constName, noun string, limit int64) {
	f.line("        if (total > %s) {", constName)
	f.line("            switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == kind && !fr.elemMaxHas {
			f.line("            case %d: %s", fr.idx, limitThrow(locName(fr.loc), noun+" above configured limit", limit))
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			if fld.Kind == kind && !fld.HasMaxlen {
				arms = append(arms, fmt.Sprintf("case %d: %s", fld.ID, limitThrow(fld.Name, noun+" above configured limit", limit)))
			}
		}
		if len(arms) > 0 {
			f.line("            case %d: switch (id) {", fr.idx)
			for _, a := range arms {
				f.line("                %s", a)
			}
			f.line("            } break;")
		}
	}
	f.line("            }")
	f.line("        }")
}

func (g *gen) emitVisitor(f *jfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	primBases := primArrayBasesUsed(fs) // "long"/"float"/"double" element bases needing lazy growth
	hasPrim := len(primBases) > 0
	limArr, limStr, limBlob := g.activeLimits(fs) // per-visitor decode limits (generator#102)

	f.line("class %sVisitor implements Visitor {", name)
	f.line("    private final %s m;", name)
	f.line("    private int cur = 0;")
	f.line("    private int ai = 0;                 // index into the primitive array currently being filled")
	if hasPrim {
		// The wire-supplied element count is UNTRUSTED: a malformed message can
		// claim ~2^31 elements, so we never allocate `new T[count]` up front (that
		// is an OutOfMemoryError DoS — see generator issue #96). Instead reserve a
		// small backing array and grow it as elements actually arrive, capped at
		// `acap` (the declared count) so the array still ends exactly right-sized.
		f.line("    private static final int ARRAY_INIT_CAP = 16; // bounded eager reservation; grow lazily")
		f.line("    private int acap = 0;               // declared element count = growth ceiling for the array being filled")
	}
	f.line("    private int[] stk = new int[16];    // sequence scope stack (unboxed, was ArrayDeque<Integer>)")
	f.line("    private int sp = 0;")
	f.line("    private java.io.ByteArrayOutputStream acc; // lazy: only split string/blob payloads need it")
	if limArr || limStr || limBlob {
		// Emitted only for the limits that are configured AND have at least one
		// schema-unbounded field in this message, so an unset or inert key changes
		// nothing in the output.
		f.line("    // Receiver-side decode limits (generator#102), baked from the sofabgen")
		f.line("    // config: caps on fields the schema left unbounded (no count / maxlen).")
		f.line("    // Exceeding one fails the decode with SofabError.LIMIT_EXCEEDED at the")
		f.line("    // wire count/length header, before any allocation or accumulation --")
		f.line("    // never a clamp. Schema-bounded fields are not governed by these caps;")
		f.line("    // they keep their own generator#100 schema-capacity guard.")
		if limArr {
			f.line("    static final long MAX_DYN_ARRAY_COUNT = %dL;", g.limits.arrayCount)
		}
		if limStr {
			f.line("    static final long MAX_DYN_STRING_LEN = %dL;", g.limits.stringLen)
		}
		if limBlob {
			f.line("    static final long MAX_DYN_BLOB_LEN = %dL;", g.limits.blobLen)
		}
	}
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
	if limStr {
		g.emitLenLimitGuard(f, fs, ir.KindString, "MAX_DYN_STRING_LEN", "string length", g.limits.stringLen)
	}
	f.line("        String _s;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _s = new String(data, chunkOffset, total, java.nio.charset.StandardCharsets.UTF_8);")
	f.line("        } else {")
	f.line("            if (acc == null) acc = new java.io.ByteArrayOutputStream();")
	f.line("            acc.write(data, chunkOffset, chunkLength);")
	f.line("            if (acc.size() < total) return;")
	f.line("            _s = new String(acc.toByteArray(), java.nio.charset.StandardCharsets.UTF_8);")
	f.line("            acc.reset();")
	f.line("        }")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindString {
			// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
			// element is omitted on the wire, so place the value at its id and fill
			// any gap with the element default ("").
			f.line("        case %d: while (%s.size() <= id) %s.add(\"\"); %s.set(id, _s); break;", fr.idx, fr.listExpr, fr.listExpr, fr.listExpr)
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
	if limBlob {
		g.emitLenLimitGuard(f, fs, ir.KindBlob, "MAX_DYN_BLOB_LEN", "blob length", g.limits.blobLen)
	}
	f.line("        byte[] _b;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _b = java.util.Arrays.copyOfRange(data, chunkOffset, chunkOffset + total);")
	f.line("        } else {")
	f.line("            if (acc == null) acc = new java.io.ByteArrayOutputStream();")
	f.line("            acc.write(data, chunkOffset, chunkLength);")
	f.line("            if (acc.size() < total) return;")
	f.line("            _b = acc.toByteArray();")
	f.line("            acc.reset();")
	f.line("        }")
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkSeqLeaf && fr.elemKind == ir.KindBlob {
			// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
			// element is omitted on the wire, so place the value at its id and fill
			// any gap with the element default (empty bytes).
			f.line("        case %d: while (%s.size() <= id) %s.add(new byte[0]); %s.set(id, _b); break;", fr.idx, fr.listExpr, fr.listExpr, fr.listExpr)
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

	// arrayBegin: a primitive array reserves a small backing store (capped, NOT
	// `new T[count]` — count is untrusted, see #96) and is grown/filled by index
	// (ai reset below); a boolean array clears its List; native-matrix rows append
	// a new inner list.
	f.line("    public void arrayBegin(int id, ArrayKind kind, int count) {")
	f.line("        ai = 0;")
	if hasPrim {
		f.line("        acap = count;")
	}
	f.line("        switch (cur) {")
	for _, fr := range fs {
		if fr.kind == fkNativeMat {
			// A native-matrix row is itself a native array: an inner array the
			// schema left unbounded is governed by the configured cap too
			// (generator#102), checked at its own count header.
			guard := ""
			if limArr && !fr.innerHasCount {
				guard = limitThrowGuard("count > MAX_DYN_ARRAY_COUNT", locName(fr.loc), "array count above configured limit", g.limits.arrayCount) + " "
			}
			f.line("        case %d: %s%s.add(new ArrayList<>()); break;", fr.idx, guard, fr.listExpr)
			continue
		}
		if fr.kind != fkNormal {
			continue
		}
		var arms []string
		for _, fld := range fr.fields {
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject up front, never clamp or keep-all
			// (generator#100). Unchecked wrapper: Visitor callbacks cannot throw
			// the checked SofabException; decode() rethrows as RuntimeException.
			// An UNBOUNDED array (no schema count) is instead governed by the
			// configured max_dyn_array_count when set (generator#102): exceeding
			// it is LIMIT_EXCEEDED — a receiver policy error, not INVALID_MSG.
			guard := ""
			if fld.HasCount {
				guard = fmt.Sprintf("if (count > %d) throw new java.io.UncheckedIOException(new SofabException(SofabError.INVALID_MSG, \"%s: array count above schema capacity %d\")); ",
					fld.Count, fld.Name, fld.Count)
			} else if limArr {
				guard = limitThrowGuard("count > MAX_DYN_ARRAY_COUNT", fld.Name, "array count above configured limit", g.limits.arrayCount) + " "
			}
			if fld.Kind == ir.KindArray && primitiveArrayElem(fld.Elem) {
				target := fr.path + "." + javaIdent(fld.Name)
				arms = append(arms, jcase(fld.ID, guard+target+" = new "+primArrayBase(fld.Elem)+"[Math.min(count, ARRAY_INIT_CAP)]"))
			} else if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) { // boolean List
				arms = append(arms, jcase(fld.ID, guard+fr.path+"."+javaIdent(fld.Name)+".clear()"))
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
	// Lazy-growth helper(s): enlarge the backing array to hold index `i`, doubling
	// but never exceeding `cap` (the declared element count) so a valid array ends
	// exactly right-sized. Growth tracks elements actually delivered, so an
	// untrusted count cannot force an up-front over-allocation (#96).
	for _, base := range primBases {
		f.line("    private static %s[] ensureCap(%s[] a, int i, int cap) {", base, base)
		f.line("        if (i < a.length) return a;")
		f.line("        long n = (long) a.length * 2;")
		f.line("        if (n < i + 1) n = i + 1;")
		f.line("        if (n > cap) n = cap;")
		f.line("        return java.util.Arrays.copyOf(a, (int) n);")
		f.line("    }")
	}
	f.line("}")
	f.blank()
}

// primArrayBasesUsed returns the distinct Java primitive element bases
// ("long"/"float"/"double") of the primitive-array fields across all frames, in
// a stable order, so emitVisitor can emit exactly the ensureCap overloads it needs.
func primArrayBasesUsed(fs []frame) []string {
	seen := map[string]bool{}
	var out []string
	for _, order := range []string{"long", "float", "double"} {
		for _, fr := range fs {
			if fr.kind != fkNormal {
				continue
			}
			for _, fld := range fr.fields {
				if fld.Kind == ir.KindArray && primitiveArrayElem(fld.Elem) && primArrayBase(fld.Elem) == order && !seen[order] {
					seen[order] = true
					out = append(out, order)
				}
			}
		}
	}
	return out
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
				// Grow the backing array on demand (never trust the wire count).
				stmt = target + " = ensureCap(" + target + ", ai, acap); " + target + "[ai++] = value"
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
