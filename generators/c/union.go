package c

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// A schema union lowers to a tagged C union (MESSAGE_SPEC §4.2: a union holds
// exactly one option):
//
//	typedef struct {
//	    sofab_object_descr_id_t which;   /* the held option's id */
//	    union { <one member per option> } u;
//	} <T>;
//
// described by SOFAB_OBJECT_DESCR_UNION with every option addressed as
// u.<option>. Everything a union does at run time — init holding default_id,
// the held non-default option written even at its own default, the switch on
// decode, "last option wins" — is corelib-c-cpp's object walk; the generated
// code is the type, the option-id macros and the descriptor table, nothing
// else.

// isSeqOption reports whether a union option is framed as a sequence of its own
// (struct, union, wrapper array) rather than being a leaf.
func isSeqOption(f *ir.Field) bool {
	return f.Kind == ir.KindStruct || f.Kind == ir.KindUnion || (f.Kind == ir.KindArray && isHolderElem(f.Elem))
}

// unionLeafMember is scalarMember for a leaf option of a union. The options
// overlay each other, so a sized blob or sized array cannot keep its length as a
// sibling member the way a struct field does (the sibling would overlay too):
// each is wrapped as { len; data/items[]; }, which keeps the length immediately
// before the storage for the *_SIZED descriptor macros.
func (g *gen) unionLeafMember(cType string, f *ir.Field) (decl, entry string, err error) {
	mn := cIdent(f.Name)
	switch f.Kind {
	case ir.KindBlob:
		decl = fmt.Sprintf("struct { %s len; uint8_t data[%d]; } %s;", blobLenC(f.Maxlen), f.Maxlen, mn)
		entry = fmt.Sprintf("    SOFAB_OBJECT_FIELD_BLOB_SIZED(%d, %s, u.%s.data, u.%s.len),", f.ID, cType, mn, mn)
		return decl, entry, nil
	case ir.KindArray:
		et := g.arrayElemCType(f.Elem, f.ElemRef)
		w := lenWidth(f.Count, cScalarWidth(et))
		decl = fmt.Sprintf("struct { %s len; %s items[%d]; } %s;", lenC(w), et, f.Count, mn)
		entry = fmt.Sprintf("    SOFAB_OBJECT_FIELD_ARRAY_SIZED(%d, %s, u.%s.items, u.%s.len, %s),", f.ID, cType, mn, mn, arrayFieldType(f.Elem))
		return decl, entry, nil
	}
	return g.scalarMember(cType, f, "u."+mn)
}

// unionMemberNote is memberNote for a union option: a sized option keeps its
// length inside the option (x.u.<option>.len), not beside it.
func unionMemberNote(f *ir.Field) string {
	switch f.Kind {
	case ir.KindBlob, ir.KindArray:
		return generator.BoundDoc{Storage: generator.StorageCompanion, LenMember: cIdent(f.Name) + ".len"}.Note(f)
	}
	return memberNote(f)
}

// unionPrefix is the macro prefix of a union's option ids, built exactly like a
// bitfield's flag prefix: <PREFIX><named key>, upper-cased.
func (g *gen) unionPrefix(key string) string {
	return strings.ToUpper(g.prefix + sanitize(key, ""))
}

// optionMacro is the #define naming one option's id, e.g. MESSAGE_SHAPE_PT_ID.
func (g *gen) optionMacro(key string, f *ir.Field) string {
	return g.unionPrefix(key) + "_" + strings.ToUpper(sanitizeKey(f.Name)) + "_ID"
}

// emitUnionConsts emits the option-id macros of one union type — what `which`
// is compared against and assigned — and names the default option.
func (g *gen) emitUnionConsts(h *cfile, p *objectPlan) {
	d := p.union.DefaultOption()
	h.doc("Option ids of %s (the value of its `which`); a fresh value holds %s.", p.cType, d.Name)
	for _, f := range p.union.Fields {
		h.line("#define %s %d", g.optionMacro(p.key, f), f.ID)
	}
	h.blank()
}

// emitUnionMembers writes the tag and the option overlay of a union type.
func (g *gen) emitUnionMembers(h *cfile, p *objectPlan) {
	h.line("    sofab_object_descr_id_t which;  /**< The held option: one of the %s_*_ID macros. */", g.unionPrefix(p.key))
	h.line("    union {")
	for _, m := range p.members {
		g.emitMember(h, m, "        ")
	}
	h.line("    } u;")
}

// unionImage decides the default image of a union and returns the expression
// SOFAB_OBJECT_DESCR_UNION takes for it: "NULL" or "&<symbol>", emitting the
// image (and, for a leaf default option, its type and the offset check) into c.
//
// The corelib reads a union's image at two places only — the tag (default_id)
// and, for a leaf default option, that option's own bytes and length (init, and
// the ≠-default test of a held default option). A held non-default option is
// written unconditionally and never compared, and a sequence option is always
// seeded through its own descriptor. So the image is a PREFIX of the union type,
// never a full sizeof(T) image (which would scale with the largest option):
//
//   - NULL iff default_id is 0 and the default option is a sequence, or a leaf
//     whose default is zero/empty — exactly what an all-zero image would say;
//   - the tag alone when the default option is a sequence;
//   - the tag plus the default option otherwise, in a union aligned like T's
//     option union so the option sits at the same offset; the .c asserts that.
//
// A string/blob/array option's default is empty by the schema rule, so the
// leaf default is a scalar whenever it is non-zero.
func (g *gen) unionImage(c *cfile, p *objectPlan) string {
	un := p.union
	d := un.DefaultOption()
	id := *un.DefaultID
	sym := g.defaultsSym(p.key)
	if isSeqOption(d) {
		if id == 0 {
			return "NULL"
		}
		c.line("static const struct { sofab_object_descr_id_t which; } %s = { %d };", sym, id)
		return "&" + sym
	}
	expr, nonZero := g.cDefaultInit(d)
	if id == 0 && !nonZero {
		return "NULL"
	}
	imgT := "_" + g.prefix + "defimg_" + sanitizeKey(p.key) + "_t"
	align := lenC(g.cAlignFields(un.Fields))
	c.line("typedef struct { sofab_object_descr_id_t which; union { %s %s _align; } u; } %s;", p.optDecl[d.ID], align, imgT)
	c.line("typedef char _%sdefimg_%s_at_u[(offsetof(%s, u) == offsetof(%s, u)) ? 1 : -1];", g.prefix, sanitizeKey(p.key), imgT, p.cType)
	init := fmt.Sprintf(".which = %d", id)
	if nonZero {
		init += fmt.Sprintf(", .u.%s = %s", cIdent(d.Name), expr)
	}
	c.line("static const %s %s = { %s };", imgT, sym, init)
	return "&" + sym
}

// optionDescr is the descriptor symbol of a sequence option of the union at key
// (the one collect records in seqOptDescrs), for the harness's select-at-default.
func (g *gen) optionDescr(key string, f *ir.Field) string {
	if f.Kind == ir.KindStruct || f.Kind == ir.KindUnion {
		return g.descrSym("named/" + f.Ref.Key)
	}
	return g.descrSym(key + "/" + f.Name + "#elems")
}
