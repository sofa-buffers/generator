package java

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// A schema union is a Java class that holds exactly ONE option (MESSAGE_SPEC
// §4.2): a private tag `which` plus one private, typed slot `_<opt>` per option.
// Its API:
//
//	<OPT>_ID          the option's id, a public static final int
//	which()           the id of the option held
//	has<Opt>()        whether <opt> is held
//	get<Opt>()        <opt> when held, else its default (stores nothing)
//	set<Opt>(v)       select <opt> with the value v; the option held before is discarded
//	mutable<Opt>()    (struct, union and List-backed array options) select <opt> at
//	                  its default unless it is held, and return the slot
//	reset()           back to the default: default_id at its own default, in place
//	serialize(os)     one switch arm per option
//
// Primitives stay unboxed in slots of their own type (`long` for every integer
// kind, as a struct member), so no access casts and no store allocates. A
// reference option (string, blob, array, struct, union) is created on its first
// selection and KEPT when another option is selected; selecting it again resets
// it to its default in place. So a reused destination (reset() + decode)
// re-selects without allocating, and a fresh union allocates only default_id's
// slot, never every option. The constructor creates only default_id.
//
// Everything emitted here differs per schema -- the slots and their types, the
// ids, one accessor set and one encode arm per option. Selection is a tag store;
// no generic tagged-union helper exists, in the package or in corelib-java.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	// f is the option as a field, with the empty default of a string, blob or
	// array option dropped (MESSAGE_SPEC §4.2 allows no other), so every
	// field-shaped helper treats it as "no declared default" -- no hoisted
	// compare static, no literal.
	f       *ir.Field
	base    string // <Opt>: the option name in Java casing, getClass-guarded
	slot    string // the private slot: `_` + the option name
	idConst string // <OPT>_ID
	isD     bool   // the union's default option (default_id)
}

// unionShape is one union type with its options.
type unionShape struct {
	typeName string
	nt       *ir.NamedType
	opts     []*unionOpt
	d        *unionOpt
}

// unionOptBase is the <Opt> part of an option's accessors. `getClass` is
// java.lang.Object's final method, so an option named `class` would not compile
// as `getClass()`; it takes the backend's trailing underscore (`getClass_()`,
// and with it `setClass_`, `hasClass_`, `mutableClass_`). No other accessor can
// land on an inherited or union method: every one carries a get/set/has/mutable
// prefix, and the union's own methods (which, reset, serialize, isDefault) carry
// none.
//
// Pascal keeps two options of one union apart: their names differ in their
// folds (the validator's naming rules), and so do their Pascal forms.
func unionOptBase(fld *ir.Field) string {
	b := naming.Pascal(fld.Name)
	if b == "Class" {
		b += "_"
	}
	return b
}

// unionSlot is the private field holding an option: `_` + the option's schema
// name. No schema name starts with `_`, so a slot meets neither the tag `which`
// nor an id constant (`<OPT>_ID` starts with a letter), and it needs no keyword
// escape.
func unionSlot(fld *ir.Field) string { return "_" + fld.Name }

// unionIDConst names an option's id constant. Two options of one union have
// distinct folds, so their upper-cased names differ too; the `_ID` suffix keeps
// the constant off the tag `which` and off every `_`-led slot.
func unionIDConst(fld *ir.Field) string { return strings.ToUpper(fld.Name) + "_ID" }

// unionMutable reports whether an option gets a mutable<Opt>() accessor: the
// kinds edited in place, member by member or element by element. A primitive
// array option is replaced whole through its setter and edited through the array
// get<Opt>() returns, so it has none.
func unionMutable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return true
	case ir.KindArray:
		return !primitiveArrayElem(fld.Elem)
	}
	return false
}

func (g *gen) unionShapeOf(key string, nt *ir.NamedType) *unionShape {
	u := &unionShape{typeName: typeIdent(nt), nt: nt}
	for _, fld := range nt.Fields {
		cp := *fld
		switch fld.Kind {
		case ir.KindString, ir.KindBlob, ir.KindArray:
			cp.Default = nil
		}
		o := &unionOpt{
			f:       &cp,
			base:    unionOptBase(fld),
			slot:    unionSlot(fld),
			idConst: unionIDConst(fld),
			isD:     nt.IsDefaultOption(fld),
		}
		u.opts = append(u.opts, o)
		if o.isD {
			u.d = o
		}
	}
	return u
}

// unionDefaultExpr is what get<Opt>() returns while another option is held: the
// option's own default, as a fresh value for a mutable reference kind so that
// nothing a caller does to it reaches the union.
func (g *gen) unionDefaultExpr(o *unionOpt) string {
	return g.javaDefaultValue(o.f)
}

// emitUnionClass writes the class of one union type.
func (g *gen) emitUnionClass(f *jfile, key string, nt *ir.NamedType) {
	u := g.unionShapeOf(key, nt)
	f.javadoc("", nt.Summary)
	f.line("public class %s {", u.typeName)
	for _, o := range u.opts {
		f.line("    public static final int %s = %d;", o.idConst, o.f.ID)
	}
	f.blank()
	f.line("    private int which = %s;", u.d.idConst)
	for _, o := range u.opts {
		init := ""
		if o.isD {
			init = g.javaInit(o.f)
		}
		f.line("    private %s %s%s;", g.javaType(o.f), o.slot, init)
	}
	f.blank()
	f.line("    /** The id of the option this union holds: one of the {@code *_ID} constants. */")
	f.line("    public int which() { return which; }")
	for _, o := range u.opts {
		g.emitUnionAccessors(f, o)
	}
	f.blank()

	g.tmpN = 0
	f.line("    public void serialize(OStream os) throws IOException {")
	f.line("        switch (which) {")
	for _, o := range u.opts {
		f.line("        case %s: {", o.idConst)
		// default_id is written like an ordinary field of its kind (omitted at its
		// default); every other option is forced (MESSAGE_SPEC §4.2).
		g.emitMarshalAt(f, "            ", o.f, "this."+o.slot, !o.isD)
		f.line("            break;")
		f.line("        }")
	}
	f.line("        default: break;")
	f.line("        }")
	f.line("    }")

	// The union is default exactly when serialize writes nothing: default_id held
	// and at its own default. Any other held option is always written.
	f.line("    /** True when the union holds its default option at that option's default -- i.e. serialize would write nothing at all. */")
	f.line("    boolean isDefault() {")
	f.line("        return which == %s && !(%s);", u.d.idConst, g.fieldWritesExpr(u.d.f, "this."+u.d.slot))
	f.line("    }")

	f.line("    /** Restores the default: the default option at its own default, in place; call before reusing an instance as a decode destination. */")
	f.line("    public void reset() {")
	f.line("        which = %s;", u.d.idConst)
	g.emitUnionSlotReset(f, "        ", u.d)
	f.line("    }")
	f.line("}")
	f.blank()
}

// memberPath is the expression a decode path takes to reach member fld of the
// object at path: the field itself, or -- when that object is a union -- the
// option's mutable accessor, which selects the option at its default unless it
// is already held. Every path into a struct, union or List-backed array option
// goes through it, so a store below an option is correct even where no begin
// arm ran first.
func memberPath(path string, fld *ir.Field, uni bool) string {
	if uni {
		return path + ".mutable" + unionOptBase(fld) + "()"
	}
	return path + "." + javaIdent(fld.Name)
}

// memberStore is the statement that stores rhs into member fld of the frame's
// object: a plain assignment, or -- in a union -- the option's setter, which is
// the §7.4.1 switch and the store in one call.
func memberStore(fr *frame, fld *ir.Field, rhs string) string {
	if fr.uni {
		return fr.path + ".set" + unionOptBase(fld) + "(" + rhs + ")"
	}
	return fr.path + "." + javaIdent(fld.Name) + " = " + rhs
}

// memberArray is the primitive array a native-array fill writes into: the field,
// or a union option's slot through its getter -- which returns the slot, since
// arrayBegin selected the option before arming the fill.
func memberArray(fr *frame, fld *ir.Field) string {
	if fr.uni {
		return fr.path + ".get" + unionOptBase(fld) + "()"
	}
	return fr.path + "." + javaIdent(fld.Name)
}

// emitUnionSlotReset puts an option's slot at the option's default, in place
// where the slot holds a reusable object.
func (g *gen) emitUnionSlotReset(f *jfile, ind string, o *unionOpt) {
	s := o.slot
	switch {
	case o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion:
		f.line("%sif (%s == null) %s = new %s(); else %s.reset();", ind, s, s, g.refType(o.f.Ref), s)
	case o.f.Kind == ir.KindArray && !primitiveArrayElem(o.f.Elem):
		f.line("%s%s = Seq.reset(%s);", ind, s, s)
	default:
		f.line("%s%s = %s;", ind, s, g.javaDefaultValue(o.f))
	}
}

// emitUnionAccessors writes has/get/set (and mutable where the kind is edited in
// place) for one option.
func (g *gen) emitUnionAccessors(f *jfile, o *unionOpt) {
	t := g.javaType(o.f)
	dep := func() {
		if o.f.Deprecated {
			f.line("    @Deprecated")
		}
	}
	f.blank()
	f.javadoc("    ", fieldDoc(o.f, ""))
	dep()
	f.line("    public %s get%s() { return which == %s ? %s : %s; }", t, o.base, o.idConst, o.slot, g.unionDefaultExpr(o))
	dep()
	f.line("    public boolean has%s() { return which == %s; }", o.base, o.idConst)
	dep()
	// `this.` on the slot: it names the field whatever the parameter is called.
	f.line("    public void set%s(%s v) { which = %s; this.%s = v; }", o.base, t, o.idConst, o.slot)
	if !unionMutable(o.f) {
		return
	}
	// Select if not held: an option that is already held is returned untouched,
	// so a decode that reaches it again -- a repeated or re-opened occurrence, a
	// resumed feed -- continues it (MESSAGE_SPEC §7.4) instead of wiping it.
	dep()
	f.line("    public %s mutable%s() {", t, o.base)
	f.line("        if (which != %s || %s == null) {", o.idConst, o.slot)
	g.emitUnionSlotReset(f, "            ", o)
	f.line("            which = %s;", o.idConst)
	f.line("        }")
	f.line("        return %s;", o.slot)
	f.line("    }")
}
