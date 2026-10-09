package typescript

import (
	"regexp"
	"strings"
	"testing"
)

// encodeBoundsSrc bounds every kind an encoder can overfill: a string and a blob
// by maxlen, native, wrapper and nested arrays by count, leaf string/blob
// elements by maxlen, and every scalar whose `number`/`bigint` member is wider
// than its declared width. `free` is the unbounded twin of each, plus the kinds
// whose storage cannot leave its width.
const encodeBoundsSrc = `
version: 1
messages:
  m:
    payload:
      s:   { id: 0, type: string, maxlen: 4 }
      b:   { id: 1, type: blob, maxlen: 5 }
      u8:  { id: 2, type: u8 }
      u16: { id: 3, type: u16 }
      u32: { id: 4, type: u32 }
      i8:  { id: 5, type: i8 }
      i16: { id: 6, type: i16 }
      i32: { id: 7, type: i32 }
      e:   { id: 8, type: enum, enum: { A: 0, B: 1, C: 2 } }
      f:   { id: 9, type: bitfield, bits: { x: { pos: 0 }, y: { pos: 9 } } }
      fw:  { id: 10, type: bitfield, bits: { x: { pos: 31 } } }
      au:  { id: 11, type: array, items: { type: u32, count: 3 } }
      as:  { id: 12, type: array, items: { type: string, count: 6, maxlen: 7 } }
      ab:  { id: 13, type: array, items: { type: blob, count: 2, maxlen: 8 } }
      rows: { id: 14, type: array, items: { type: array, count: 2, items: { type: u16, count: 9 } } }
      n:
        id: 15
        type: struct
        fields:
          arr: { id: 0, type: array, items: { type: u16, count: 2 } }
  free:
    payload:
      s:   { id: 0, type: string }
      b:   { id: 1, type: blob }
      a:   { id: 2, type: array, items: { type: u32 } }
      as:  { id: 3, type: array, items: { type: string } }
      u64: { id: 4, type: u64 }
      i64: { id: 5, type: i64 }
      bo:  { id: 6, type: boolean }
      fl:  { id: 7, type: fp64 }
      fwide: { id: 8, type: bitfield, bits: { x: { pos: 40 } } }
`

func argThrow(cond, msg string) string {
	return "if (" + cond + ") throw new SofabError(SofabErrorCode.Argument, \"" + msg + "\");"
}

// TestTSEncodeRefusesOverBound pins generator#656: a value past its schema bound
// is refused at encode with SofabError(ARGUMENT), before it is written. A
// string's maxlen is handed to the corelib's writeString (only it knows the UTF-8
// length); every other bound is one per-field compare against the schema literal,
// emitted right before the write it protects. All three int64 modes emit the same
// guards: none of them touches the narrow kinds.
func TestTSEncodeRefusesOverBound(t *testing.T) {
	for _, mode := range []string{"bigint", "long", "number"} {
		t.Run(mode, func(t *testing.T) {
			mod := genTSWith(t, encodeBoundsSrc, map[string]any{"int64": mode})
			m := tsClass(t, mod, "M")
			mustContain(t, "M serialize", m,
				"    if (this.s !== \"\") {\n      os.writeString(0, this.s, 4);\n    }",
				"    if (this.b.length !== 0) {\n      "+argThrow("this.b.length > 5", "b: blob byte length above schema maxlen 5")+"\n      os.writeBlob(1, this.b);",
				"      "+argThrow("this.u8 > 255", "u8: value outside declared width u8")+"\n      os.writeUnsigned(2, this.u8);",
				"      "+argThrow("this.u16 > 65535", "u16: value outside declared width u16")+"\n      os.writeUnsigned(3, this.u16);",
				"      "+argThrow("this.u32 > 4294967295", "u32: value outside declared width u32")+"\n      os.writeUnsigned(4, this.u32);",
				"      "+argThrow("this.i8 < -128 || this.i8 > 127", "i8: value outside declared width i8")+"\n      os.writeSigned(5, this.i8);",
				"      "+argThrow("this.i16 < -32768 || this.i16 > 32767", "i16: value outside declared width i16")+"\n      os.writeSigned(6, this.i16);",
				"      "+argThrow("this.i32 < -2147483648 || this.i32 > 2147483647", "i32: value outside declared width i32")+"\n      os.writeSigned(7, this.i32);",
				// The enum is compared as a plain number: TypeScript rejects a compare
				// of an enum-typed value against a bound outside its members.
				"      "+argThrow("(this.e as number) < -128 || (this.e as number) > 127", "e: value outside declared enum width")+"\n      os.writeSigned(8, this.e);",
				// A bitfield's width is the one its highest pos implies: pos 9 -> u16,
				// pos 31 -> u32 held as a bigint.
				"      "+argThrow("this.f > 65535", "f: value outside declared bitfield width")+"\n      os.writeUnsigned(9, this.f);",
				"      "+argThrow("this.fw > 4294967295n", "fw: value outside declared bitfield width")+"\n      os.writeUnsigned(10, this.fw);",
				"    if (this.au.length !== 0) {\n      "+argThrow("this.au.length > 3", "au: array count above schema capacity 3")+"\n      os.writeUnsignedArray(11, this.au);",
				// A wrapper array: its count before the frame opens, its element maxlen
				// on each element's write.
				"    "+argThrow("this.as.length > 6", "as: array count above schema capacity 6")+"\n    os.writeSequenceBeginLazy(12);",
				"        os.writeString(_i0, _a0[_i0]!, 7);",
				"    "+argThrow("this.ab.length > 2", "ab: array count above schema capacity 2")+"\n    os.writeSequenceBeginLazy(13);",
				"        "+argThrow("_a0[_i0]!.length > 8", "ab[]: blob byte length above schema maxlen 8")+"\n        os.writeBlob(_i0, _a0[_i0]!);",
				// A nested array: the outer count, and each row's own.
				"    "+argThrow("this.rows.length > 2", "rows: array count above schema capacity 2")+"\n    os.writeSequenceBeginLazy(14);",
				"        "+argThrow("_e0.length > 9", "rows[]: array count above schema capacity 9")+"\n        os.writeUnsignedArray(_i0, _e0);",
			)
			// The nested struct's array is guarded in that struct's own serialize.
			mustContain(t, "M_N serialize", tsClass(t, mod, "M_N"),
				"      "+argThrow("this.arr.length > 2", "arr: array count above schema capacity 2")+"\n      os.writeUnsignedArray(0, this.arr);")

			// Unbounded: no maxlen argument, no guard. A typed array, a 64-bit
			// value, a boolean, a float and a u64-wide bitfield cannot leave their
			// width, so nothing is emitted for them either.
			free := tsClass(t, mod, "Free")
			free = free[:strings.Index(free, "static readonly MAX_SIZE")]
			mustContain(t, "Free serialize", free,
				"os.writeString(0, this.s);", "os.writeString(_i0, _a0[_i0]!);")
			if strings.Contains(free, "SofabErrorCode.Argument") {
				t.Errorf("an unbounded field must not be guarded:\n%s", free)
			}
		})
	}
}

// TestTSEncodeGuardsArePerField is CLAUDE.md's mechanical static-helper check
// for the encode guards: two schemas differing in bounds, element types and
// field count. Once the literals, field names and member expressions are
// normalised, a guard is a single `if (...) throw` line in front of its write and
// nothing else, so no schema-independent block is emitted beside the writes.
func TestTSEncodeGuardsArePerField(t *testing.T) {
	a := genTSWith(t, `
version: 1
messages:
  m:
    payload:
      s: { id: 0, type: string, maxlen: 4 }
      au: { id: 1, type: array, items: { type: u32, count: 3 } }
`, map[string]any{})
	b := genTSWith(t, `
version: 1
messages:
  m:
    payload:
      s: { id: 0, type: string, maxlen: 40 }
      au: { id: 1, type: array, items: { type: u8, count: 30 } }
      i: { id: 2, type: i16 }
`, map[string]any{})
	guard := regexp.MustCompile(`(?m)^.*SofabErrorCode\.Argument.*$`)
	for _, src := range []string{a, b} {
		for _, line := range guard.FindAllString(src, -1) {
			if !strings.HasPrefix(strings.TrimSpace(line), "if (") || !strings.HasSuffix(line, ");") {
				t.Errorf("an encode guard must be one per-field line, got %q", line)
			}
		}
	}
	if na, nb := len(guard.FindAllString(a, -1)), len(guard.FindAllString(b, -1)); na != 1 || nb != 2 {
		t.Errorf("expected 1 and 2 guards (one per bounded array/narrow scalar), got %d and %d", na, nb)
	}
}
