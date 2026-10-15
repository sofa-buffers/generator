package java

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// The constant holders of the schema's enum and bitfield types.
//
// MESSAGE_SPEC §1 admits every value inside an enum's or bitfield's declared
// width, named or not, so a Java `enum` -- a closed set -- cannot be the field:
// a peer's in-width value that names no constant has to decode, be stored as
// received and re-encode unchanged. The field therefore stays the `long` it is
// (with its declared-width check), and the names the schema gives its values
// are constants BESIDE it, in one holder class per type: `m.level = Level.MAX`,
// `(m.perm & Perm.W) != 0`. The holder is not used by the generated code at all;
// it exists for the reader, and the field's javadoc links to it (constHolder).
//
// A holder is a public top-level class in its own file, named like every other
// generated type (typeIdent: the schema path, escaped where the package already
// uses the name), so it takes the same naming channel structs and unions do and
// cannot meet another generated class.

// emitEnumHolder writes `public final class <T>` with one `long` constant per
// enum value.
func emitEnumHolder(f *jfile, nt *ir.NamedType) {
	name := typeIdent(nt)
	doc := nt.Summary
	if doc == "" {
		doc = "Constants of enum {@code " + nt.Name + "}."
	}
	doc += "\n\n<p>A field of this type may also hold an in-width value that names no\nconstant; it is kept as received and re-encoded unchanged."
	f.javadoc("", doc)
	f.line("public final class %s {", name)
	f.line("    private %s() {}", name)
	for _, c := range nt.Consts {
		f.javadoc("    ", c.Description)
		f.line("    public static final long %s = %dL;", javaConstIdent(c.Name), c.Value)
	}
	f.line("}")
}

// emitBitfieldHolder writes `public final class <T>` with one `long` constant
// per flag, holding the flag's MASK (`1L << pos`), so a flag test reads like the
// schema. Bit 63 is `1L << 63`, the sign bit of the long carrier, which is the
// same unsigned word the field holds.
func emitBitfieldHolder(f *jfile, nt *ir.NamedType) {
	name := typeIdent(nt)
	doc := nt.Summary
	if doc == "" {
		doc = "Flags of bitfield {@code " + nt.Name + "}, each as its mask."
	}
	doc += "\n\n<p>A field of this type may also carry bits that no flag declares; they are\nkept as received and never masked."
	f.javadoc("", doc)
	f.line("public final class %s {", name)
	f.line("    private %s() {}", name)
	for _, fl := range nt.Flags {
		d := fl.Description
		if fl.HasDefault {
			note := fmt.Sprintf("(default: %t)", fl.Default)
			if d == "" {
				d = note
			} else {
				d += " " + note
			}
		}
		f.javadoc("    ", d)
		f.line("    public static final long %s = 1L << %d;", javaConstIdent(fl.Name), fl.Pos)
	}
	f.line("}")
}

// javaConstIdent is a constant of a holder class: the schema name, with a
// trailing underscore where it is a Java keyword. The holder declares nothing
// else a constant could meet -- no other field, no qualified expression -- so
// the keyword list is the whole of it; a schema name never ends with `_`, so a
// mangled constant meets no other constant.
func javaConstIdent(name string) string {
	if javaKeywords[name] {
		return name + "_"
	}
	return name
}

// constHolder is the holder class an enum/bitfield field -- or an array whose
// leaf element is one -- is documented against, or "" for any other field.
func constHolder(fld *ir.Field) string {
	kind, ref, items := fld.Kind, fld.Ref, (*ir.ArrayElem)(nil)
	if kind == ir.KindArray {
		kind, ref, items = fld.Elem, fld.ElemRef, fld.ElemItems
	}
	for kind == ir.KindArray && items != nil {
		kind, ref, items = items.Elem, items.ElemRef, items.ElemItems
	}
	if (kind == ir.KindEnum || kind == ir.KindBitfield) && ref != nil {
		return typeIdent(ref.Target)
	}
	return ""
}
