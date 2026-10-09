package kotlin

import (
	"strings"
	"testing"
)

// TestKotlinEncodeBounds pins the encode-side refusal of a value past its schema
// bound (ARCHITECTURE §9.6): every bounded field gets its guard (or, for a
// string, the bounded corelib write), with the ARGUMENT category, ahead of the
// write; an unbounded field and a type that already holds the declared width
// get nothing.
func TestKotlinEncodeBounds(t *testing.T) {
	src := "version: 1\n" +
		"$defs:\n  enum:\n    E: { A: 0, B: 1, C: 2 }\n    W: { LO: -40000, Z: 0, HI: 40000 }\n" +
		"  bitfield:\n    F: { x: { pos: 0 }, y: { pos: 9 } }\n    G: { x: { pos: 63 } }\n" +
		"messages:\n  M:\n    payload:\n" +
		"      s: { id: 0, type: string, maxlen: 4 }\n" +
		"      b: { id: 1, type: blob, maxlen: 5 }\n" +
		"      u8: { id: 2, type: u8 }\n" +
		"      i16: { id: 3, type: i16 }\n" +
		"      au: { id: 4, type: array, items: { type: u32, count: 3 } }\n" +
		"      as: { id: 5, type: array, items: { type: string, count: 6, maxlen: 7 } }\n" +
		"      e: { id: 6, type: enum, enum: { $ref: \"#/$defs/enum/E\" } }\n" +
		"      f: { id: 7, type: bitfield, bits: { $ref: \"#/$defs/bitfield/F\" } }\n" +
		"      g: { id: 8, type: bitfield, bits: { $ref: \"#/$defs/bitfield/G\" } }\n" +
		"      w: { id: 9, type: enum, enum: { $ref: \"#/$defs/enum/W\" } }\n" +
		"      ab: { id: 10, type: array, items: { type: blob, count: 2, maxlen: 9 } }\n" +
		"      nn: { id: 11, type: array, items: { type: array, count: 4, items: { type: u16, count: 2 } } }\n" +
		"      ns: { id: 12, type: array, items: { type: array, count: 3, items: { type: string, count: 5, maxlen: 6 } } }\n" +
		"      ae: { id: 13, type: array, items: { type: enum, enum: { $ref: \"#/$defs/enum/E\" }, count: 8 } }\n" +
		"      us: { id: 14, type: string }\n" +
		"      ub: { id: 15, type: blob }\n" +
		"      ua: { id: 16, type: array, items: { type: u32 } }\n"
	m := genFromYAML(t, src, map[string]any{})["src/main/kotlin/message/M.kt"]
	ser := m[strings.Index(m, "public fun serialize(os: OStream)"):]
	ser = ser[:strings.Index(ser, "\n    }\n")]
	for _, want := range []string{
		"if (this.s.isNotEmpty()) os.writeString(0, this.s, 4)",
		"if (this.b.size > 5) throw SofabException(SofabError.ARGUMENT, \"b: longer than maxlen 5 bytes\")",
		"if (this.au.size > 3) throw SofabException(SofabError.ARGUMENT, \"au: more than count 3 elements\")",
		"if (_t0.size > 6) throw SofabException(SofabError.ARGUMENT, \"as: more than count 6 elements\")",
		"os.writeString(_i0, _e0, 7)",
		"if (this.e < -128 || this.e > 127) throw SofabException(SofabError.ARGUMENT, \"e: value outside declared enum width\")",
		"if (this.w < -2147483648 || this.w > 2147483647)",
		"if ((this.f and 0xffffUL.inv()) != 0UL) throw SofabException(SofabError.ARGUMENT, \"f: value outside declared bitfield width\")",
		"if (_e0.size > 9) throw SofabException(SofabError.ARGUMENT, \"ab element: longer than maxlen 9 bytes\")",
		"if (_t2.size > 4) throw SofabException(SofabError.ARGUMENT, \"nn: more than count 4 elements\")",
		"if (_e0.size > 2) throw SofabException(SofabError.ARGUMENT, \"nn row: more than count 2 elements\")",
		"if (_t3.size > 3) throw SofabException(SofabError.ARGUMENT, \"ns: more than count 3 elements\")",
		"if (_t4.size > 5) throw SofabException(SofabError.ARGUMENT, \"ns row: more than count 5 elements\")",
		"os.writeString(_i1, _e1, 6)",
		"if (this.ae.size > 8) throw SofabException(SofabError.ARGUMENT, \"ae: more than count 8 elements\")",
		"if (this.us.isNotEmpty()) os.writeString(14, this.us)\n",
	} {
		if want == "if (this.w < -2147483648 || this.w > 2147483647)" {
			// An enum whose declared width is the whole `Int` needs no check.
			if strings.Contains(ser, want) || strings.Contains(ser, "this.w <") {
				t.Errorf("an i32-wide enum held in an Int must carry no width guard:\n%s", ser)
			}
			continue
		}
		if !strings.Contains(ser, want) {
			t.Errorf("serialize missing %q:\n%s", want, ser)
		}
	}
	for _, not := range []string{
		"this.u8 >", "this.i16 >", "this.i16 <", // the type is the declared width
		"this.g and", // a pos-63 bitfield is the whole ULong
		"this.ub.size >", "this.ua.size >",
	} {
		if strings.Contains(ser, not) {
			t.Errorf("serialize must not guard %q:\n%s", not, ser)
		}
	}
	// The guard precedes its write, and each array guard precedes its frame.
	if strings.Index(ser, "this.b.size > 5") > strings.Index(ser, "os.writeBlob(1, this.b)") {
		t.Error("the blob guard must precede the write")
	}
	if strings.Index(ser, "_t0.size > 6") > strings.Index(ser, "os.writeSequenceBeginLazy(5)") {
		t.Error("the count guard must precede the opened frame")
	}
}

// TestKotlinEncodeBoundsUnion: a union option is serialized by the same arm, so
// its bounded option is refused as a field is, forced or not.
func TestKotlinEncodeBoundsUnion(t *testing.T) {
	src := "version: 1\n$defs:\n  enum:\n    E: { A: 0, B: 1 }\nmessages:\n  M:\n    payload:\n" +
		"      u: { id: 0, type: union, oneof: {\n" +
		"        s: { id: 0, type: string, maxlen: 3 },\n" +
		"        e: { id: 1, type: enum, enum: { $ref: \"#/$defs/enum/E\" } },\n" +
		"        a: { id: 2, type: array, items: { type: i8, count: 2 } } } }\n"
	var all strings.Builder
	for _, c := range genFromYAML(t, src, map[string]any{}) {
		all.WriteString(c)
	}
	got := all.String()
	for _, want := range []string{
		"os.writeString(0, this._s, 3)",
		"if (this._e < -128 || this._e > 127) throw SofabException(SofabError.ARGUMENT",
		"if (this._a.size > 2) throw SofabException(SofabError.ARGUMENT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("union serialize missing %q", want)
		}
	}
}
