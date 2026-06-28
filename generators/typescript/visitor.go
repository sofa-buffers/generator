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
			case x.Kind == ir.KindArray && x.Elem == ir.KindString:
				f.line("        case %d: self.%s = []; return stringListVisitor(self.%s);", x.ID, x.Name, x.Name)
			case x.Kind == ir.KindArray && x.Elem == ir.KindBlob:
				f.line("        case %d: self.%s = []; return blobListVisitor(self.%s);", x.ID, x.Name, x.Name)
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

// emitArrayCbs handles numeric arrays (string/blob arrays are sequences).
func (g *gen) emitArrayCbs(f *tsfile, fields []*ir.Field) {
	numArr := func(x *ir.Field) bool {
		return x.Kind == ir.KindArray && x.Elem != ir.KindString && x.Elem != ir.KindBlob
	}
	if !has(fields, numArr) {
		return
	}
	// arrayBegin initialises the target slice.
	f.line("      arrayBegin(id: number, _kind: number, _count: number): void {")
	f.line("        switch (id) {")
	for _, x := range fields {
		if numArr(x) {
			f.line("        case %d: self.%s = []; break;", x.ID, x.Name)
		}
	}
	f.line("        }")
	f.line("      },")

	// Each callback is emitted ONCE; per-field it decides bigint vs Number.
	emitElem := func(cb, valType string, match func(ir.Kind) bool) {
		var sel []*ir.Field
		for _, x := range fields {
			if numArr(x) && match(x.Elem) {
				sel = append(sel, x)
			}
		}
		if len(sel) == 0 {
			return
		}
		f.line("      %s(id: number, index: number, value: %s): void {", cb, valType)
		f.line("        switch (id) {")
		for _, x := range sel {
			if isBig(x.Elem) || valType == "number" {
				f.line("        case %d: self.%s[index] = value; break;", x.ID, x.Name)
			} else {
				f.line("        case %d: self.%s[index] = Number(value); break;", x.ID, x.Name)
			}
		}
		f.line("        }")
		f.line("      },")
	}
	emitElem("arrayUnsigned", "bigint", func(k ir.Kind) bool {
		return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
	})
	emitElem("arraySigned", "bigint", func(k ir.Kind) bool {
		return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
	})
	emitElem("arrayFp32", "number", func(k ir.Kind) bool { return k == ir.KindFP32 })
	emitElem("arrayFp64", "number", func(k ir.Kind) bool { return k == ir.KindFP64 })
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
		(x.Kind == ir.KindArray && (x.Elem == ir.KindString || x.Elem == ir.KindBlob))
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
}`
