package typescript

import "github.com/sofa-buffers/generator/internal/ir"

// emitVisitor generates a decode() static method and an _visitor() that builds
// `this` from the corelib's push-visitor (PLAN §4.1: TS decode is visitor-based;
// sequenceBegin returns a child visitor). Only the callbacks a message actually
// needs are emitted (Visitor methods are all optional).
func (g *gen) emitVisitor(f *tsfile, name string, fields []*ir.Field) {
	f.line("  static decode(bytes: Uint8Array): %s {", name)
	f.line("    const o = new %s();", name)
	f.line("    decode(bytes, o._visitor());")
	f.line("    return o;")
	f.line("  }")
	f.blank()
	f.line("  _visitor(): Visitor {")
	f.line("    const self = this;")
	f.line("    const acc = new ChunkAcc();")
	f.line("    return {")

	// unsigned: u8/u16/u32/u64/bitfield/bool
	if has(fields, func(x *ir.Field) bool {
		return isUnsignedish(x) || x.Kind == ir.KindBool
	}) {
		f.line("      unsigned(id: number, value: bigint): void {")
		f.line("        switch (id) {")
		for _, x := range fields {
			switch {
			case x.Kind == ir.KindU64:
				f.line("        case %d: self.%s = value; break;", x.ID, x.Name)
			case x.Kind == ir.KindU8 || x.Kind == ir.KindU16 || x.Kind == ir.KindU32 || x.Kind == ir.KindBitfield:
				f.line("        case %d: self.%s = Number(value); break;", x.ID, x.Name)
			case x.Kind == ir.KindBool:
				f.line("        case %d: self.%s = value !== 0n; break;", x.ID, x.Name)
			}
		}
		f.line("        }")
		f.line("      },")
	}

	// signed: i8/i16/i32/i64/enum
	if has(fields, isSignedish) {
		f.line("      signed(id: number, value: bigint): void {")
		f.line("        switch (id) {")
		for _, x := range fields {
			switch {
			case x.Kind == ir.KindI64:
				f.line("        case %d: self.%s = value; break;", x.ID, x.Name)
			case x.Kind == ir.KindI8 || x.Kind == ir.KindI16 || x.Kind == ir.KindI32:
				f.line("        case %d: self.%s = Number(value); break;", x.ID, x.Name)
			case x.Kind == ir.KindEnum:
				f.line("        case %d: self.%s = Number(value) as %s; break;", x.ID, x.Name, g.typeName(x.Ref.Key))
			}
		}
		f.line("        }")
		f.line("      },")
	}

	g.emitFloatCb(f, fields, ir.KindFP32, "fp32")
	g.emitFloatCb(f, fields, ir.KindFP64, "fp64")

	// string
	if has(fields, func(x *ir.Field) bool { return x.Kind == ir.KindString }) {
		f.line("      string(id: number, total: number, offset: number, chunk: Uint8Array): void {")
		f.line("        const s = acc.str(id, total, offset, chunk);")
		f.line("        if (s === null) return;")
		f.line("        switch (id) {")
		for _, x := range fields {
			if x.Kind == ir.KindString {
				f.line("        case %d: self.%s = s; break;", x.ID, x.Name)
			}
		}
		f.line("        }")
		f.line("      },")
	}

	// blob
	if has(fields, func(x *ir.Field) bool { return x.Kind == ir.KindBlob }) {
		f.line("      blob(id: number, total: number, offset: number, chunk: Uint8Array): void {")
		f.line("        const b = acc.blob(id, total, offset, chunk);")
		f.line("        if (b === null) return;")
		f.line("        switch (id) {")
		for _, x := range fields {
			if x.Kind == ir.KindBlob {
				f.line("        case %d: self.%s = b; break;", x.ID, x.Name)
			}
		}
		f.line("        }")
		f.line("      },")
	}

	// numeric arrays
	g.emitArrayCbs(f, fields)

	// sequences: struct/union + array-of-string/blob
	if has(fields, func(x *ir.Field) bool { return seqField(x) }) {
		f.line("      sequenceBegin(id: number): Visitor | void {")
		f.line("        switch (id) {")
		for _, x := range fields {
			switch {
			case x.Kind == ir.KindStruct || x.Kind == ir.KindUnion:
				f.line("        case %d: self.%s = new %s(); return self.%s._visitor();", x.ID, x.Name, g.typeName(x.Ref.Key), x.Name)
			case x.Kind == ir.KindArray:
				f.line("        case %d: self.%s = []; return %s;", x.ID, x.Name, g.listVisitor("self."+x.Name, x.Elem, x.ElemRef, x.ElemItems))
			}
		}
		f.line("        }")
		f.line("      },")
	}

	f.line("    };")
	f.line("  }")
}

func (g *gen) emitFloatCb(f *tsfile, fields []*ir.Field, kind ir.Kind, cb string) {
	if !has(fields, func(x *ir.Field) bool { return x.Kind == kind }) {
		return
	}
	f.line("      %s(id: number, value: number): void {", cb)
	f.line("        switch (id) {")
	for _, x := range fields {
		if x.Kind == kind {
			f.line("        case %d: self.%s = value; break;", x.ID, x.Name)
		}
	}
	f.line("        }")
	f.line("      },")
}

// emitArrayCbs handles native arrays: numeric, plus enum (signed), boolean and
// bitfield (unsigned). String/blob/struct/union/nested arrays are sequences and
// are routed through sequenceBegin instead.
func (g *gen) emitArrayCbs(f *tsfile, fields []*ir.Field) {
	natArr := func(x *ir.Field) bool {
		return x.Kind == ir.KindArray && nativeArrayElem(x.Elem)
	}
	if !has(fields, natArr) {
		return
	}
	// arrayBegin initialises the target slice.
	f.line("      arrayBegin(id: number, _kind: number, _count: number): void {")
	f.line("        switch (id) {")
	for _, x := range fields {
		if natArr(x) {
			f.line("        case %d: self.%s = []; break;", x.ID, x.Name)
		}
	}
	f.line("        }")
	f.line("      },")

	// Each callback is emitted ONCE; per-field it converts the wire value.
	emitElem := func(cb, valType string, match func(ir.Kind) bool) {
		var sel []*ir.Field
		for _, x := range fields {
			if natArr(x) && match(x.Elem) {
				sel = append(sel, x)
			}
		}
		if len(sel) == 0 {
			return
		}
		f.line("      %s(id: number, index: number, value: %s): void {", cb, valType)
		f.line("        switch (id) {")
		for _, x := range sel {
			f.line("        case %d: self.%s[index] = %s; break;", x.ID, x.Name, g.arrayElemRHS(x.Elem, x.ElemRef, "value"))
		}
		f.line("        }")
		f.line("      },")
	}
	emitElem("arrayUnsigned", "bigint", func(k ir.Kind) bool {
		return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64 || k == ir.KindBool || k == ir.KindBitfield
	})
	emitElem("arraySigned", "bigint", func(k ir.Kind) bool {
		return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64 || k == ir.KindEnum
	})
	emitElem("arrayFp32", "number", func(k ir.Kind) bool { return k == ir.KindFP32 })
	emitElem("arrayFp64", "number", func(k ir.Kind) bool { return k == ir.KindFP64 })
}

// arrayElemRHS converts a decoded array element `value` to the member type:
// 64-bit ints and floats pass through; bool becomes a comparison; enum casts;
// the rest narrow via Number().
func (g *gen) arrayElemRHS(elem ir.Kind, ref *ir.TypeRef, value string) string {
	switch elem {
	case ir.KindU64, ir.KindI64, ir.KindFP32, ir.KindFP64:
		return value
	case ir.KindBool:
		return value + " !== 0n"
	case ir.KindEnum:
		return "Number(" + value + ") as " + g.typeName(ref.Key)
	default: // u8/u16/u32, i8/i16/i32, bitfield
		return "Number(" + value + ")"
	}
}

// nativeArrayElem reports whether an array element is encoded as a native array
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

// listVisitor returns a TS expression for a Visitor that collects array elements
// (described by elem/ref/items) into the array expression `out`. Used for the
// wrapper-sequence element kinds.
func (g *gen) listVisitor(out string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "stringListVisitor(" + out + ")"
	case ir.KindBlob:
		return "blobListVisitor(" + out + ")"
	case ir.KindStruct, ir.KindUnion:
		return "structListVisitor(" + out + ", () => new " + g.typeName(ref.Key) + "())"
	case ir.KindArray:
		return g.nestedArrayListVisitor(out, items)
	}
	return "{}"
}

// nestedArrayListVisitor returns a Visitor literal that collects rows (each row an
// inner array described by items) into `out`. Native inner rows arrive via
// arrayBegin/arrayXxx; sequence inner rows arrive via sequenceBegin and recurse.
func (g *gen) nestedArrayListVisitor(out string, items *ir.ArrayElem) string {
	if nativeArrayElem(items.Elem) {
		cb, valType := arrayCb(items.Elem)
		rhs := g.arrayElemRHS(items.Elem, items.ElemRef, "value")
		return "{ arrayBegin(id: number): void { " + out + "[id] = []; }, " +
			cb + "(id: number, index: number, value: " + valType + "): void { " + out + "[id][index] = " + rhs + "; } }"
	}
	inner := g.listVisitor(out+"[id]", items.Elem, items.ElemRef, items.ElemItems)
	return "{ sequenceBegin(id: number): Visitor { " + out + "[id] = []; return " + inner + "; } }"
}

// arrayCb maps a native array element kind to its corelib callback name and the
// value type that callback receives.
func arrayCb(k ir.Kind) (string, string) {
	switch k {
	case ir.KindFP32:
		return "arrayFp32", "number"
	case ir.KindFP64:
		return "arrayFp64", "number"
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "arraySigned", "bigint"
	default: // unsigned, bool, bitfield
		return "arrayUnsigned", "bigint"
	}
}

func has(fields []*ir.Field, pred func(*ir.Field) bool) bool {
	for _, x := range fields {
		if pred(x) {
			return true
		}
	}
	return false
}

func isUnsignedish(x *ir.Field) bool {
	return x.Kind == ir.KindU8 || x.Kind == ir.KindU16 || x.Kind == ir.KindU32 || x.Kind == ir.KindU64 || x.Kind == ir.KindBitfield
}
func isSignedish(x *ir.Field) bool {
	return x.Kind == ir.KindI8 || x.Kind == ir.KindI16 || x.Kind == ir.KindI32 || x.Kind == ir.KindI64 || x.Kind == ir.KindEnum
}
func seqField(x *ir.Field) bool {
	return x.Kind == ir.KindStruct || x.Kind == ir.KindUnion ||
		(x.Kind == ir.KindArray && seqArrayElem(x.Elem))
}

func ifStr(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// tsPrelude holds the shared decode helpers: string/blob chunk accumulation and
// the list collectors for array-of-string/blob sequences.
const tsPrelude = `const _utf8 = new TextDecoder();

// arrEq is an element-wise equality check used by the sparse-canonical marshal to
// decide whether a leaf blob or native scalar array equals its default (and may
// thus be omitted). Works for Uint8Array and number/bigint/boolean arrays.
function arrEq(a: ArrayLike<unknown>, b: ArrayLike<unknown>): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

class ChunkAcc {
  private parts = new Map<number, Uint8Array[]>();
  private got = new Map<number, number>();
  private push(id: number, total: number, _offset: number, chunk: Uint8Array): Uint8Array | null {
    let arr = this.parts.get(id);
    if (!arr) { arr = []; this.parts.set(id, arr); this.got.set(id, 0); }
    arr.push(chunk);
    this.got.set(id, (this.got.get(id) ?? 0) + chunk.length);
    if ((this.got.get(id) ?? 0) >= total) {
      const out = new Uint8Array(total);
      let o = 0;
      for (const c of arr) { if (o >= total) break; out.set(c.subarray(0, Math.min(c.length, total - o)), o); o += c.length; }
      this.parts.delete(id); this.got.delete(id);
      return out;
    }
    return null;
  }
  str(id: number, total: number, offset: number, chunk: Uint8Array): string | null {
    const b = this.push(id, total, offset, chunk);
    return b === null ? null : _utf8.decode(b);
  }
  blob(id: number, total: number, offset: number, chunk: Uint8Array): Uint8Array | null {
    return this.push(id, total, offset, chunk);
  }
}

function stringListVisitor(out: string[]): Visitor {
  const acc = new ChunkAcc();
  return { string(id, total, offset, chunk) { const s = acc.str(id, total, offset, chunk); if (s !== null) out.push(s); } };
}

function blobListVisitor(out: Uint8Array[]): Visitor {
  const acc = new ChunkAcc();
  return { blob(id, total, offset, chunk) { const b = acc.blob(id, total, offset, chunk); if (b !== null) out.push(b); } };
}

function structListVisitor<T extends { _visitor(): Visitor }>(out: T[], make: () => T): Visitor {
  return { sequenceBegin(_id: number): Visitor { const o = make(); out.push(o); return o._visitor(); } };
}`
