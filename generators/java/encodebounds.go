package java

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// Encode-side schema bounds (ARCHITECTURE §9.6).
//
// A value the caller filled past its own declared bound -- a string or blob past
// its `maxlen` in UTF-8 bytes, an array past its `count`, a scalar, enum or
// bitfield outside its declared width -- is REFUSED by serialize with the
// corelib's argument category (SofabException, SofabError.ARGUMENT), never
// written. The decoder refuses each of these as INVALID_MSG (MESSAGE_SPEC §7.1);
// these guards are the encode half, so a message this code encodes is one its own
// decoder accepts.
//
// Every guard is a per-field comparison against the schema literal, emitted next
// to the write it protects and on its taken branch only: an omitted (default)
// field costs nothing. What the Java storage type already guarantees is not
// re-checked: a primitive array element is held in the declared width's own
// primitive (primArrayBase), so only the long-backed SCALAR kinds carry a width
// guard. A string's byte length is known only inside the corelib's measuring
// pass, so the bound rides that call: OStream.writeString(id, s, maxlen).

// argThrow is the encode-side refusal: the checked argument category serialize
// already declares (IOException), raised before the field's first byte.
func argThrow(name, detail string) string {
	return fmt.Sprintf("throw new SofabException(SofabError.ARGUMENT, \"%s: %s\");", name, detail)
}

// encodeWidthGuard refuses a long-backed scalar of kind k held in acc that lies
// outside its declared width, with the same comparison the decode store applies
// (widthTest). "" for the kinds whose range is the long itself.
func encodeWidthGuard(k ir.Kind, ref *ir.TypeRef, acc, name string) string {
	cond, what := widthTest(k, ref, acc)
	if cond == "" {
		return ""
	}
	return fmt.Sprintf("if (%s) %s ", cond, argThrow(name, "value outside declared "+what))
}

// arrBound is what one array level declares: its capacity and, for a string or
// blob element, the element maxlen. name labels the refusal.
type arrBound struct {
	name       string
	hasCount   bool
	count      int64
	elemMaxHas bool
	elemMax    int64
}

func fieldArrBound(fld *ir.Field) arrBound {
	return arrBound{fld.Name, fld.HasCount, fld.Count, fld.ElemMaxHas, fld.ElemMax}
}

// inner is the bound of one element row of this array.
func (b arrBound) inner(items *ir.ArrayElem) arrBound {
	return arrBound{b.name + " element", items.HasCount, items.Count, items.ElemMaxHas, items.ElemMax}
}

// countGuard refuses an array whose length (lenExpr) is past its declared
// capacity; "" for a dynamic array.
func (b arrBound) countGuard(lenExpr string) string {
	if !b.hasCount {
		return ""
	}
	return fmt.Sprintf("if (%s > %d) %s", lenExpr, b.count, argThrow(b.name, fmt.Sprintf("array count above schema capacity %d", b.count)))
}

// blobMaxlenGuard refuses a blob (acc, possibly null) longer than its maxlen.
func blobMaxlenGuard(acc, name string, max int64) string {
	return fmt.Sprintf("if (%s != null && %s.length > %d) %s ", acc, acc, max, argThrow(name, fmt.Sprintf("blob length above schema maxlen %d", max)))
}

// writeStringCall is the corelib string write, bounded when the schema declares
// a maxlen: the corelib measures the UTF-8 length anyway and refuses past the
// bound before writing a byte.
func writeStringCall(idExpr, val string, hasMax bool, max int64) string {
	if hasMax {
		return fmt.Sprintf("os.writeString(%s, %s, %d);", idExpr, val, max)
	}
	return fmt.Sprintf("os.writeString(%s, %s);", idExpr, val)
}

// hasEncodeBound reports whether serialize can refuse a value, i.e. whether its
// @throws documentation has anything to say. A struct or union member, and a
// struct or union array element, refuses through its own nested serialize, so
// those types are searched too.
func hasEncodeBound(fields []*ir.Field) bool {
	return fieldsHaveEncodeBound(fields, map[*ir.NamedType]bool{})
}

func fieldsHaveEncodeBound(fields []*ir.Field, seen map[*ir.NamedType]bool) bool {
	nested := func(ref *ir.TypeRef) bool {
		if ref == nil || ref.Target == nil || seen[ref.Target] {
			return false
		}
		seen[ref.Target] = true
		return fieldsHaveEncodeBound(ref.Target.Fields, seen)
	}
	for _, fld := range fields {
		switch fld.Kind {
		case ir.KindString, ir.KindBlob:
			if fld.HasMaxlen {
				return true
			}
		case ir.KindStruct, ir.KindUnion:
			if nested(fld.Ref) {
				return true
			}
		case ir.KindArray:
			if fld.HasCount || fld.ElemMaxHas || nested(fld.ElemRef) {
				return true
			}
			for it := fld.ElemItems; it != nil; it = it.ElemItems {
				if it.HasCount || it.ElemMaxHas || nested(it.ElemRef) {
					return true
				}
			}
		default:
			if c, _ := widthTest(fld.Kind, fld.Ref, "v"); c != "" {
				return true
			}
		}
	}
	return false
}

// emitSerializeRefusalDoc writes the Javadoc of a struct or union serialize
// when it can refuse a value past its schema bound, directly or through a
// nested serialize. lead is the first sentence; who names what holds the value.
func emitSerializeRefusalDoc(f *jfile, fields []*ir.Field, lead, who string) {
	if !hasEncodeBound(fields) {
		return
	}
	f.line("    /**")
	f.line("     * %s", lead)
	f.line("     *")
	f.line("     * @throws SofabException with {@code ARGUMENT} when %s a value", who)
	f.line("     *         past its schema bound (a string or blob over its maxlen in")
	f.line("     *         UTF-8 bytes, an array over its count, an integer, enum or")
	f.line("     *         bitfield outside its declared width); the value is never")
	f.line("     *         written, clamped or masked")
	f.line("     * @throws IOException on buffer overflow or sink failure")
	f.line("     */")
}
