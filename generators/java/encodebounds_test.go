package java

import (
	"strings"
	"testing"
)

// The encode half of the schema bounds (ARCHITECTURE §9.6): a value past its
// maxlen, count or declared width is refused by serialize with the corelib's
// ARGUMENT category, never written. Each guard sits on the write it protects and
// compares against the schema literal; what the Java storage type already
// guarantees (a primitive array element held in its declared width's primitive)
// is not re-checked, and a dynamic field carries no guard at all.
func TestJavaEncodeRefusesOverBoundValues(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      s:   { id: 0, type: string, maxlen: 4 }
      b:   { id: 1, type: blob, maxlen: 4 }
      u8:  { id: 2, type: u8 }
      i16: { id: 3, type: i16 }
      au:  { id: 4, type: array, items: { type: u32, count: 3 } }
      as:  { id: 5, type: array, items: { type: string, count: 3, maxlen: 4 } }
      e:   { id: 6, type: enum, enum: { A: 0, B: 1, C: 2 } }
      f:   { id: 7, type: bitfield, bits: { x: { pos: 0 }, y: { pos: 1 }, z: { pos: 2 } } }
      ab:  { id: 8, type: array, items: { type: blob, count: 2, maxlen: 3 } }
      ar:  { id: 9, type: array, items: { type: array, count: 2, items: { type: u16, count: 5 } } }
      abo: { id: 10, type: array, items: { type: boolean, count: 6 } }
      u32: { id: 11, type: u32 }
      u64: { id: 12, type: u64 }
      i64: { id: 13, type: i64 }
      ds:  { id: 14, type: string }
      db:  { id: 15, type: blob }
      da:  { id: 16, type: array, items: { type: u32 } }
`
	got := javaMethod(t, genJavaFromYAML(t, src, map[string]any{})["src/main/java/message/M.java"], "public void serialize(OStream os)")
	for _, want := range []string{
		// String: the bound rides the corelib's measuring pass.
		`os.writeString(0, this.s == null ? "" : this.s, 4);`,
		`if (this.b != null && this.b.length > 4) throw new SofabException(SofabError.ARGUMENT, "b: blob length above schema maxlen 4"); os.writeBlob(1,`,
		// Scalars: the decode store's comparison, on the field.
		`if (this.u8 != 0L) { if (this.u8 < 0 || this.u8 > 255L) throw new SofabException(SofabError.ARGUMENT, "u8: value outside declared width u8"); os.writeUnsigned(2, this.u8); }`,
		`if (this.i16 < -32768L || this.i16 > 32767L) throw new SofabException(SofabError.ARGUMENT, "i16: value outside declared width i16"); os.writeSigned(3, this.i16);`,
		`if (this.u32 < 0 || this.u32 > 4294967295L) throw new SofabException(SofabError.ARGUMENT, "u32: value outside declared width u32");`,
		`if (this.e < -128L || this.e > 127L) throw new SofabException(SofabError.ARGUMENT, "e: value outside declared enum width"); os.writeSigned(6, this.e);`,
		`if ((this.f & ~0xffL) != 0) throw new SofabException(SofabError.ARGUMENT, "f: value outside declared bitfield width"); os.writeUnsigned(7, this.f);`,
		// Arrays: the capacity on the length, before the first element is written.
		`if (this.au.length > 3) throw new SofabException(SofabError.ARGUMENT, "au: array count above schema capacity 3");`,
		`if (_t0.size() > 3) throw new SofabException(SofabError.ARGUMENT, "as: array count above schema capacity 3");`,
		`os.writeString(_i0, _e0, 4);`,
		`if (_t1.size() > 2) throw new SofabException(SofabError.ARGUMENT, "ab: array count above schema capacity 2");`,
		`if (_e0.length > 3) throw new SofabException(SofabError.ARGUMENT, "ab element: blob length above schema maxlen 3");`,
		`if (_t2.size() > 2) throw new SofabException(SofabError.ARGUMENT, "ar: array count above schema capacity 2");`,
		`if (_e0.length > 5) throw new SofabException(SofabError.ARGUMENT, "ar element: array count above schema capacity 5");`,
		`if (this.abo.size() > 6) throw new SofabException(SofabError.ARGUMENT, "abo: array count above schema capacity 6");`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("serialize missing encode guard %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		// 64-bit kinds: the long IS the declared range.
		`"u64: value outside`, `"i64: value outside`,
		// Dynamic fields declare no bound.
		`"ds:`, `"db:`, `"da:`,
		// A primitive element is held in its declared width: never re-checked.
		`element: value outside`,
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("serialize carries a guard it must not (%q):\n%s", unwanted, got)
		}
	}
	for _, want := range []string{`os.writeString(14, this.ds == null ? "" : this.ds);`} {
		if !strings.Contains(got, want) {
			t.Errorf("an unbounded string must take the unbounded write (%q):\n%s", want, got)
		}
	}
}

// A union's held option is written even at its default (forced), and its guard
// sits on that write all the same.
func TestJavaEncodeBoundOnUnionOption(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      u:
        id: 0
        type: union
        default_id: 0
        oneof:
          a: { id: 0, type: u8 }
          s: { id: 1, type: string, maxlen: 2 }
          b: { id: 2, type: blob, maxlen: 3 }
`
	files := genJavaFromYAML(t, src, map[string]any{})
	all := ""
	for _, c := range files {
		all += c
	}
	for _, want := range []string{
		`value outside declared width u8"); os.writeUnsigned(0,`,
		`, 2);`, // the bounded string write of option s
		`blob length above schema maxlen 3"); os.writeBlob(2,`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("union serialize missing encode guard %q:\n%s", want, all)
		}
	}
}

// serialize refuses through a nested serialize call too, so a class whose only
// bounded values sit in a struct member or a struct array element still
// documents the refusal; a class with nothing bounded anywhere does not.
func TestJavaEncodeBoundDocReachesNestedTypes(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      n:  { id: 0, type: struct, fields: { x: { id: 0, type: u8 } } }
      ar: { id: 1, type: array, items: { type: struct, fields: { y: { id: 0, type: string, maxlen: 3 } } } }
  D:
    payload:
      n:  { id: 0, type: struct, fields: { x: { id: 0, type: u64 } } }
      ar: { id: 1, type: array, items: { type: struct, fields: { y: { id: 0, type: string } } } }
`
	files := genJavaFromYAML(t, src, map[string]any{})
	const throws = "@throws SofabException with {@code ARGUMENT}"
	if m := files["src/main/java/message/M.java"]; !strings.Contains(m, throws) {
		t.Errorf("M.serialize lacks the refusal Javadoc although its nested types refuse:\n%s", m)
	}
	if d := files["src/main/java/message/D.java"]; strings.Contains(d, throws) {
		t.Errorf("D.serialize documents a refusal although nothing in it is bounded:\n%s", d)
	}
	// encode() and encodeTo() follow serialize: they document the refusal
	// exactly where serialize can raise it.
	for _, c := range []struct {
		file string
		want bool
	}{{"M.java", true}, {"D.java", false}} {
		src := files["src/main/java/message/"+c.file]
		for _, doc := range []string{
			"wrapping the {@link SofabException} that",
			"A field past its schema bound makes {@link #serialize} throw",
		} {
			if got := strings.Contains(src, doc); got != c.want {
				t.Errorf("%s: encode/encodeTo doc %q present = %v, want %v:\n%s", c.file, doc, got, c.want, src)
			}
		}
	}
}

// A union serialize documents the refusal like a struct serialize does: for a
// union field and for a union array element, each refusing only through one of
// its options; a union whose options are all unbounded does not.
func TestJavaEncodeBoundDocOnUnionClasses(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      u:  { id: 0, type: union, default_id: 0, oneof: { a: { id: 0, type: u8 }, arr: { id: 1, type: array, items: { type: u16, count: 3 } } } }
      au: { id: 1, type: array, items: { type: union, oneof: { x: { id: 0, type: i64 }, y: { id: 1, type: blob, maxlen: 2 } } } }
      fu: { id: 2, type: union, default_id: 0, oneof: { p: { id: 0, type: u64 }, q: { id: 1, type: string } } }
`
	files := genJavaFromYAML(t, src, map[string]any{})
	const throws = "@throws SofabException with {@code ARGUMENT}"
	for _, name := range []string{"M_U", "M_Au"} {
		c, ok := files["src/main/java/message/"+name+".java"]
		if !ok {
			t.Fatalf("no %s.java generated; files: %v", name, keysOf(files))
		}
		if strings.Count(c, throws) != 1 {
			t.Errorf("%s.serialize lacks the refusal Javadoc although an option refuses:\n%s", name, c)
		}
	}
	if c := files["src/main/java/message/M_Fu.java"]; strings.Contains(c, throws) {
		t.Errorf("M_Fu.serialize documents a refusal although no option is bounded:\n%s", c)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
