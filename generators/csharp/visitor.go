package csharp

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

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
	elem     ir.Kind       // element kind of this array
	ref      *ir.TypeRef   // enum/bitfield/struct/union element
	items    *ir.ArrayElem // nested-array element descriptor
	childLoc string        // struct/union element or sequence-nested inner-array scope
	elemDyn  bool          // string/blob element without a schema maxlen (generator#102)
	// cap is the wrapper array's schema fixed-count bound N (-1 == dynamic/no
	// count): a wrapper element id >= N is a schema-bound violation (MESSAGE_SPEC
	// §5.1/§7 — issue #142), rejected as INVALID before the List grows, which also
	// bounds an over-index heap-amplification fill.
	cap int64
}

// capOf maps a schema fixed-count bound to a frame's cap: N when the array
// declares a count, -1 (dynamic/unbounded) otherwise.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// overIndexGuard returns the reject clause for a fixed-count wrapper array: an
// element id >= N throws InvalidMessage (aborting decode) before the List grows
// (MESSAGE_SPEC §5.1/§7 — issue #142), which also bounds an over-index
// heap-amplification fill. Empty for a dynamic array (cap == -1).
func (g *gen) overIndexGuard(cap int64, loc string) string {
	if cap < 0 {
		return ""
	}
	return fmt.Sprintf("if (id >= %d) throw new SofabException(SofabError.InvalidMessage, \"%s element: array index above schema capacity %d\"); ", cap, loc, cap)
}

func (g *gen) frames(m *ir.Message) []frame {
	var out []frame
	var walkObj func(loc, path string, fields []*ir.Field)
	var walkArr func(loc, list string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemDyn bool, cap int64)

	walkObj = func(loc, path string, fields []*ir.Field) {
		out = append(out, frame{loc: loc, path: path, fields: fields})
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				walkObj(loc+"_"+fld.Name, path+"."+csIdent(fld.Name), fld.Ref.Target.Fields)
			case fld.Kind == ir.KindArray && seqArrayElem(fld.Elem):
				walkArr(loc+"_"+fld.Name, path+"."+csIdent(fld.Name), fld.Elem, fld.ElemRef, fld.ElemItems, !fld.ElemMaxHas, capOf(fld.HasCount, fld.Count))
			}
		}
	}

	// walkArr registers the array scope entered on SequenceBegin(field/index),
	// plus any child scope its elements descend into. elemDyn marks a string/
	// blob element scope whose elements carry no schema maxlen (generator#102);
	// cap is the array's schema fixed-count bound (-1 == dynamic).
	walkArr = func(loc, list string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemDyn bool, cap int64) {
		fr := frame{loc: loc, path: list, isArr: true, elem: elem, ref: ref, items: items, cap: cap}
		switch elem {
		case ir.KindStruct, ir.KindUnion:
			fr.childLoc = loc + "_e"
			out = append(out, fr)
			walkObj(fr.childLoc, lastElem(list), ref.Target.Fields)
		case ir.KindArray:
			if seqArrayElem(items.Elem) {
				fr.childLoc = loc + "_e"
				out = append(out, fr)
				walkArr(fr.childLoc, lastElem(list), items.Elem, items.ElemRef, items.ElemItems, !items.ElemMaxHas, capOf(items.HasCount, items.Count))
			} else {
				out = append(out, fr) // native inner rows collected in place
			}
		default: // string/blob
			fr.elemDyn = elemDyn
			out = append(out, fr)
		}
	}

	walkObj("Root", "m", m.Fields)
	return out
}

// hasDynPrimArray reports whether any object-scope field is a primitive (T[])
// array without a schema count: its wire count is untrusted AND unbounded, so
// the visitor needs the lazy-growth machinery (ArrayInitCap/acap/EnsureCap)
// instead of an eager new T[count] (cf. generator#96/#100/#102).
func hasDynPrimArray(fs []frame) bool {
	for _, fr := range fs {
		if fr.isArr {
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == ir.KindArray && primArrayElem(fld.Elem) && !fld.HasCount {
				return true
			}
		}
	}
	return false
}

// primFill is the statement filling the next slot of the primitive array field
// `target`. A fixed-count array was allocated at exactly its schema count N by
// ArrayBegin (its generator#100 guard rejects a wire count above N), so the
// wire's M <= N elements land in place and C#'s zero-initialization already
// leaves [M, N) at the element default the encoder elided (MESSAGE_SPEC §3). A
// count-less array starts small and grows on demand via EnsureCap, so an
// untrusted wire count never allocates.
func primFill(target string, fld *ir.Field, rhs string) string {
	if !fld.HasCount {
		return fmt.Sprintf("%s = EnsureCap(%s, ai, acap); %s[ai++] = %s;", target, target, target, rhs)
	}
	return fmt.Sprintf("%s[ai++] = %s;", target, rhs)
}

// nativeListFill is the statement filling the next slot of a native List<T>
// array field `target` (boolean/enum/bitfield elements — these value-convert
// element-wise and so stay List<T>, cf. primArrayElem). A fixed-count list was
// pre-filled to exactly N element defaults by ArrayBegin, so the wire's M <= N
// elements overwrite [0, M) by index and the trailing default run [M, N) the
// encoder elided is already materialized (MESSAGE_SPEC §3). A dynamic array
// appends: its length is exactly the wire count.
func nativeListFill(target string, fld *ir.Field, rhs string) string {
	if !fld.HasCount {
		return fmt.Sprintf("%s.Add(%s);", target, rhs)
	}
	return fmt.Sprintf("%s[ai++] = %s;", target, rhs)
}

// emitLenGuard writes the generator#102 length guard at the top of the String/
// Blob callback: when the wire `total` exceeds the configured cap and the
// target (cur, id) is a schema-unbounded field, decode fails with
// LimitExceeded before any bytes are accumulated (single-shot and chunked
// paths alike). Schema-bounded fields fall through unaffected.
func (g *gen) emitLenGuard(f *cfile, fs []frame, kind ir.Kind, constName, what string, limit int64) {
	f.line("        if (total > %s) {", constName)
	f.line("            switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == kind && fr.elemDyn {
				f.line("            case (%s, _): throw new SofabException(SofabError.LimitExceeded, \"%s element: %s above configured limit %d\");", fr.loc, fr.loc, what, limit)
			}
			continue
		}
		for _, fld := range fr.fields {
			if fld.Kind == kind && !fld.HasMaxlen {
				f.line("            case (%s, %d): throw new SofabException(SofabError.LimitExceeded, \"%s: %s above configured limit %d\");", fr.loc, fld.ID, fld.Name, what, limit)
			}
		}
	}
	f.line("            }")
	f.line("        }")
}

func (g *gen) emitVisitor(f *cfile, name string, fields []*ir.Field) {
	fs := g.frames(&ir.Message{Name: name, Fields: fields})
	dynPrim := hasDynPrimArray(fs)
	// A configured max_dyn_* cap is live only when this message actually has a
	// schema-unbounded field of that kind — otherwise it is inert and no
	// constant or guard is emitted (generator#102).
	b := ir.Bounds(fields)
	limArr := g.limits.arrayHas && b.HasDynArray
	limStr := g.limits.stringHas && b.HasDynString
	limBlob := g.limits.blobHas && b.HasDynBlob

	f.line("internal sealed class %sVisitor : IVisitor {", name)
	f.line("    private readonly %s m;", name)
	f.line("    private int cur = 0;")
	f.line("    private int ai = 0;                // index into the primitive array currently being filled")
	if dynPrim {
		// The wire-supplied element count of a count-less array is untrusted:
		// never allocate `new T[count]` up front (an out-of-memory DoS, cf.
		// generator#96/#100). Reserve a small backing array and grow it as
		// elements actually arrive, capped at the wire count so an honest
		// array still ends exactly right-sized.
		f.line("    private const int ArrayInitCap = 16; // bounded eager reservation for count-less arrays; grow lazily")
		f.line("    private int acap = 0;              // wire count = growth ceiling for the count-less array being filled")
	}
	f.line("    private int[] stk = new int[16];   // sequence scope stack (unboxed, was Stack<int>)")
	f.line("    private int sp = 0;")
	f.line("    private List<byte> acc;            // lazy: only split string/blob payloads need it")
	f.line("    public %sVisitor(%s msg) { m = msg; }", name, name)
	for i, fr := range fs {
		f.line("    private const int %s = %d;", fr.loc, i)
	}
	if limArr || limStr || limBlob {
		// Receiver-side decode limits (generator#102): configured caps on the
		// fields the schema leaves unbounded (array without count, string/blob
		// without maxlen). Exceeding a cap fails decode with LimitExceeded at
		// the count/total header, before any allocation.
		f.line("    // Receiver-side decode limits: caps on schema-unbounded")
		f.line("    // fields only; exceeding one throws SofabError.LimitExceeded.")
		if limArr {
			f.line("    private const long MaxDynArrayCount = %d;", g.limits.arrayCount)
		}
		if limStr {
			f.line("    private const long MaxDynStringLen = %d;", g.limits.stringLen)
		}
		if limBlob {
			f.line("    private const long MaxDynBlobLen = %d;", g.limits.blobLen)
		}
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
			case fld.Kind == ir.KindArray && primArrayElem(fld.Elem) && unsignedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, primFill(fr.path+"."+csIdent(fld.Name), fld, g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			case fld.Kind == ir.KindArray && unsignedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, nativeListFill(fr.path+"."+csIdent(fld.Name), fld, g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
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
			case fld.Kind == ir.KindArray && primArrayElem(fld.Elem) && signedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, primFill(fr.path+"."+csIdent(fld.Name), fld, g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			case fld.Kind == ir.KindArray && signedArrayElem(fld.Elem):
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, nativeListFill(fr.path+"."+csIdent(fld.Name), fld, g.arrayElemAddRHS(fld.Elem, fld.ElemRef, "value")))
			}
		}
	}
	f.line("        }")
	f.line("    }")

	g.emitFloatVisit(f, fs, ir.KindFP32, "Fp32", "float")
	g.emitFloatVisit(f, fs, ir.KindFP64, "Fp64", "double")

	// String. Single-shot: when the whole payload arrives in one chunk, decode
	// straight from the contiguous input slice; the per-byte List<byte> accumulator
	// is only the fallback for a genuinely split payload.
	f.line("    public void String(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	if limStr {
		// generator#102: reject an over-cap unbounded string at its `total`
		// header, before the fast path decodes or the accumulator grows.
		g.emitLenGuard(f, fs, ir.KindString, "MaxDynStringLen", "string length", g.limits.stringLen)
	}
	f.line("        string _s;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _s = Encoding.UTF8.GetString(data, chunkOffset, total);")
	f.line("        } else {")
	f.line("            acc ??= new List<byte>();")
	f.line("            for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("            if (acc.Count < total) return;")
	f.line("            _s = Encoding.UTF8.GetString(acc.ToArray());")
	f.line("            acc.Clear();")
	f.line("        }")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindString {
				// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
				// element is omitted on the wire, so place each value at its id and
				// grow the list, filling any gap with the element default ("").
				f.line("            case (%s, _): %swhile (%s.Count <= id) %s.Add(\"\"); %s[id] = _s; break;", fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, fr.path, fr.path)
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

	// Blob. Single-shot on the whole-in-one-chunk fast path (see String).
	f.line("    public void Blob(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {")
	if limBlob {
		// generator#102: reject an over-cap unbounded blob at its `total`
		// header, before the fast path allocates or the accumulator grows.
		g.emitLenGuard(f, fs, ir.KindBlob, "MaxDynBlobLen", "blob length", g.limits.blobLen)
	}
	f.line("        byte[] _b;")
	f.line("        if (offset == 0 && chunkLength >= total) {")
	f.line("            _b = new byte[total];")
	f.line("            System.Array.Copy(data, chunkOffset, _b, 0, total);")
	f.line("        } else {")
	f.line("            acc ??= new List<byte>();")
	f.line("            for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);")
	f.line("            if (acc.Count < total) return;")
	f.line("            _b = acc.ToArray();")
	f.line("            acc.Clear();")
	f.line("        }")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindBlob {
				// Elements are keyed by index id (MESSAGE_SPEC S2): a default (empty)
				// element is omitted on the wire, so place each value at its id and
				// grow the list, filling any gap with the element default (empty bytes).
				f.line("            case (%s, _): %swhile (%s.Count <= id) %s.Add(Array.Empty<byte>()); %s[id] = _b; break;", fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, fr.path, fr.path)
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
	f.line("        ai = 0;")
	if dynPrim {
		f.line("        acap = count;")
	}
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			if fr.elem == ir.KindArray && nativeArrayElem(fr.items.Elem) {
				// A count-less inner row of a nested array is governed by the
				// configured cap at its own count header (generator#102).
				guard := ""
				if limArr && !fr.items.HasCount {
					guard = fmt.Sprintf("if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"%s element: array count above configured limit %d\"); ",
						fr.loc, g.limits.arrayCount)
				}
				f.line("            case (%s, _): %s%s.Add(new List<%s>()); break;", fr.loc, guard, fr.path, g.csArrayElemType(fr.items.Elem, fr.items.ElemRef, fr.items.ElemItems))
			}
			continue
		}
		for _, fld := range fr.fields {
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject up front, never clamp or keep-all
			// (generator#100). The guard also bounds the eager `new T[count]`
			// below to the schema capacity (the count is untrusted, cf. #96).
			// A count-less array instead gets the configured generator#102 cap
			// (when set) and a lazily-grown backing array, never new T[count].
			guard := ""
			switch {
			case fld.HasCount:
				guard = fmt.Sprintf("if (count > %d) throw new SofabException(SofabError.InvalidMessage, \"%s: array count above schema capacity %d\"); ",
					fld.Count, fld.Name, fld.Count)
			case limArr && fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem):
				guard = fmt.Sprintf("if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"%s: array count above configured limit %d\"); ",
					fld.Name, g.limits.arrayCount)
			}
			if fld.Kind == ir.KindArray && primArrayElem(fld.Elem) {
				// A `count: N` array is FIXED-LENGTH: allocate exactly N (not the
				// wire count M <= N, which the guard above already bounds). C#
				// zero-initializes, so [M, N) is the element default the encoder
				// elided and Length is N on every decode (MESSAGE_SPEC §3).
				alloc := fmt.Sprintf("new %%s[%d]", fld.Count)
				if !fld.HasCount {
					alloc = "new %s[Math.Min(count, ArrayInitCap)]"
				}
				f.line("            case (%s, %d): %s%s.%s = "+alloc+"; break;", fr.loc, fld.ID, guard, fr.path, csIdent(fld.Name), g.csArrayElemType(fld.Elem, fld.ElemRef, fld.ElemItems))
			} else if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) {
				acc := fr.path + "." + csIdent(fld.Name)
				if fld.HasCount {
					// Fixed-length List<T>: pre-fill the schema count with element
					// defaults; the wire's M <= N elements then overwrite [0, M) by
					// index (nativeListFill) and [M, N) stays default.
					f.line("            case (%s, %d): %s%s.Clear(); for (int _p = 0; _p < %d; _p++) %s.Add(default(%s)); break;",
						fr.loc, fld.ID, guard, acc, fld.Count, acc, g.csArrayElemType(fld.Elem, fld.ElemRef, fld.ElemItems))
				} else {
					f.line("            case (%s, %d): %s%s.Clear(); break;", fr.loc, fld.ID, guard, acc)
				}
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
	f.line("        if (sp == stk.Length) System.Array.Resize(ref stk, sp * 2);")
	f.line("        stk[sp++] = cur;")
	f.line("        switch ((cur, id)) {")
	for _, fr := range fs {
		if fr.isArr {
			switch {
			case fr.elem == ir.KindStruct || fr.elem == ir.KindUnion:
				f.line("            case (%s, _): %s%s.Add(new %s()); cur = %s; break;", fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, g.typeName(fr.ref.Key), fr.childLoc)
			case fr.elem == ir.KindArray && seqArrayElem(fr.items.Elem):
				f.line("            case (%s, _): %s%s.Add(new List<%s>()); cur = %s; break;", fr.loc, g.overIndexGuard(fr.cap, fr.loc), fr.path, g.csArrayElemType(fr.items.Elem, fr.items.ElemRef, fr.items.ElemItems), fr.childLoc)
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
	f.line("    public void SequenceEnd() { cur = sp > 0 ? stk[--sp] : 0; }")
	if dynPrim {
		// Lazy-growth helper: enlarge the backing array to hold index `i`,
		// doubling but never past `cap` (the wire count), so growth tracks
		// elements actually delivered and an honest array ends exactly
		// right-sized while an untrusted count allocates nothing up front.
		f.line("    // Grow a to hold index i: double, never past cap (the wire count), so")
		f.line("    // growth tracks elements actually delivered (untrusted count).")
		f.line("    private static T[] EnsureCap<T>(T[] a, int i, int cap) {")
		f.line("        if (i < a.Length) return a;")
		f.line("        long n = (long)a.Length * 2;")
		f.line("        if (n < i + 1) n = i + 1;")
		f.line("        if (n > cap) n = cap;")
		f.line("        System.Array.Resize(ref a, (int)n);")
		f.line("        return a;")
		f.line("    }")
	}
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
				f.line("            case (%s, %d): %s break;", fr.loc, fld.ID, primFill(fr.path+"."+csIdent(fld.Name), fld, "value"))
			}
		}
	}
	f.line("        }")
	f.line("    }")
}
