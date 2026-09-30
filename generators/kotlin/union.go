package kotlin

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// A schema union is a Kotlin class that holds exactly ONE option (MESSAGE_SPEC
// §4.2): a tag `which` plus one private, typed slot per option. Its API:
//
//	<OPT>_ID          the option's id, a `const val` in the companion
//	which             the id of the option held (read-only property)
//	<opt>             property: <opt> when held, else its default (the getter
//	                  stores nothing); assigning it selects <opt> with that value
//	has<Opt>()        whether <opt> is held
//	mutable<Opt>()    (struct, union and wrapper-array options) select <opt> at
//	                  its default unless it is held, and return the slot
//	reset()           back to the default: default_id at its own default, in place
//	serialize(os)     one `when` arm per option
//
// Each slot has the option's exact member type -- `UShort`, `Float`, `String`,
// `UShortArray`, the generated struct -- so no access boxes or casts. A struct,
// union or wrapper-array option (the kinds edited in place) is created on its
// first selection and KEPT when another option is selected; selecting it again
// resets it in place, so a reused destination (reset() + decode) re-selects
// without allocating, and a fresh union allocates only default_id's slot. Every
// other slot is replaced whole on selection and starts at its option's default
// literal or the corelib's shared zero-length array -- a constant, never an
// allocation.
//
// Everything emitted here differs per schema -- the slots and their types, the
// ids, one accessor set and one encode arm per option. Selection is a tag store;
// no generic tagged-union helper exists, in the package or in corelib-kotlin-mp.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	// f is the option as a field, with the empty default of a string, blob or
	// array option dropped (MESSAGE_SPEC §4.2 allows no other), so every
	// field-shaped helper treats it as "no declared default" -- no hoisted
	// compare constant, no literal.
	f       *ir.Field
	orig    *ir.Field // the option as declared, for its documentation
	prop    string    // the public property (escaped / mangled)
	base    string    // <Opt>: has<Opt>, mutable<Opt>
	slot    string    // the private slot
	idConst string    // <OPT>_ID
	isD     bool      // the union's default option (default_id)
	jvmSet  string    // the @set:JvmName annotation, "" unless the setter needs one (jvmSetterRenames)
}

// unionShape is one union type with its options.
type unionShape struct {
	typeName string
	nt       *ir.NamedType
	opts     []*unionOpt
	d        *unionOpt
}

// unionOptProp is the property an option is reached through. The union's own
// members (`which`, `serialize`, `isDefault`, `reset`) and the generated
// members every class avoids take the trailing underscore; a hard keyword is
// backtick-escaped, exactly as a struct member's name is.
//
// So does a name spelled like an id constant (isIDConstSpelling): inside the
// class a property outranks the companion's constant of the same name, and the
// `when (which)` arms would compare against the property.
//
// Every other name the union derives from its options is distinct by the naming
// rules (ARCHITECTURE §8): has<Opt>/mutable<Opt> carry naming.Pascal of the
// option, the id constant its upper-cased name, the slot `_` + the name -- and
// options differ in their fold. A property may share its name with a function
// (an option `hasFoo` beside `foo`'s hasFoo()): Kotlin and the JVM keep a
// property's accessors and a function apart.
func unionOptProp(name string) string {
	if unionReserved[name] || isIDConstSpelling(name) {
		return name + "_"
	}
	return ktIdent(name)
}

// isIDConstSpelling reports whether an option name is spelled like an id
// constant -- upper case, digits and `_`, ending in `_ID` -- so it could be the
// `<OPT>_ID` of a sibling. It is decided on the name alone, so every property
// path (the class, the visitor's stores, the harness) spells it alike.
func isIDConstSpelling(name string) bool {
	return strings.HasSuffix(name, "_ID") && name == strings.ToUpper(name)
}

// unionMutable reports whether an option gets a mutable<Opt>() accessor: the
// kinds edited in place, member by member or element by element. A primitive
// array (every native element kind, `boolean` included) cannot grow in place: it
// is replaced whole through the property and its elements are edited through the
// array the getter returns.
func unionMutable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return true
	case ir.KindArray:
		return !nativeArrayElem(fld.Elem)
	}
	return false
}

// unionSlotNullable reports whether an option's slot starts out null: exactly
// the kinds a selection creates and later resets in place. Every other slot
// holds a value from the start (a literal, or a shared zero-length constant).
func unionSlotNullable(fld *ir.Field) bool { return unionMutable(fld) }

func (g *gen) unionShapeOf(key string, nt *ir.NamedType) *unionShape {
	u := &unionShape{typeName: g.typeName(key), nt: nt}
	for _, fld := range nt.Fields {
		cp := *fld
		switch fld.Kind {
		case ir.KindString, ir.KindBlob, ir.KindArray:
			cp.Default = nil
		}
		o := &unionOpt{
			f:       &cp,
			orig:    fld,
			prop:    unionOptProp(fld.Name),
			base:    naming.Pascal(fld.Name),
			slot:    "_" + fld.Name,
			idConst: strings.ToUpper(fld.Name) + "_ID",
			isD:     nt.IsDefaultOption(fld),
		}
		u.opts = append(u.opts, o)
		if o.isD {
			u.d = o
		}
	}
	props := make([]string, len(u.opts))
	for i, o := range u.opts {
		props[i] = o.prop
	}
	renames := jvmSetterRenames(props)
	for _, o := range u.opts {
		o.jvmSet = renames[o.prop]
	}
	return u
}

// unionSlotInit is the value a slot is declared with: default_id's option at its
// default, every other option at its default literal where that is a constant,
// and null where it would be an allocation (struct, union, wrapper list).
func (g *gen) unionSlotInit(o *unionOpt) string {
	if unionSlotNullable(o.f) && !o.isD {
		return "null"
	}
	return g.ktDefaultValue(o.f)
}

// unionSlotType is the declared type of an option's slot.
func (g *gen) unionSlotType(o *unionOpt) string {
	if unionSlotNullable(o.f) {
		return g.ktType(o.f) + "?"
	}
	return g.ktType(o.f)
}

// unionSlotAcc reads the slot of the HELD option: a nullable slot is non-null
// whenever its option is held (the constructor, reset(), the setter and
// mutable<Opt>() all store a value before or as they select), so the assertion
// never fires.
func unionSlotAcc(o *unionOpt) string {
	if unionSlotNullable(o.f) {
		return "this." + o.slot + "!!"
	}
	return "this." + o.slot
}

// emitUnionClass writes the class of one union type.
func (g *gen) emitUnionClass(f *kfile, key string, nt *ir.NamedType) {
	u := g.unionShapeOf(key, nt)
	f.kdoc("", nt.Summary)
	if anyDeprecated(nt.Fields) {
		f.line("%s", deprecationSuppress)
	}
	f.line("public class %s {", u.typeName)
	f.line("    /** The id of the option this union holds: one of the `*_ID` constants. */")
	f.line("    public var which: Int = %s", u.d.idConst)
	f.line("        private set")
	for _, o := range u.opts {
		f.line("    private var %s: %s = %s", o.slot, g.unionSlotType(o), g.unionSlotInit(o))
	}
	for _, o := range u.opts {
		g.emitUnionAccessors(f, o)
	}
	f.blank()

	g.tmpN = 0
	f.line("    /** Write the held option into [os]. Streaming out: nothing is flushed. */")
	f.line("    public fun serialize(os: OStream) {")
	f.line("        when (which) {")
	for _, o := range u.opts {
		f.line("            %s -> {", o.idConst)
		// default_id is written like an ordinary field of its kind (omitted at its
		// default); every other option is forced (MESSAGE_SPEC §4.2).
		g.emitMarshalAt(f, "                ", o.f, unionSlotAcc(o), !o.isD)
		f.line("            }")
	}
	f.line("        }")
	f.line("    }")
	f.blank()

	// The union is default exactly when serialize writes nothing: default_id held
	// and at its own default. Any other held option is always written.
	f.line("    /** True when the union holds its default option at that option's default -- i.e. serialize would write nothing at all. */")
	f.line("    internal fun isDefault(): Boolean = which == %s && %s", u.d.idConst, negate(g.ktWritesExpr(u.d.f, unionSlotAcc(u.d))))
	f.blank()
	f.line("    /** Restore the default -- the default option at its own default, in place; call before reusing an instance as a decode destination. */")
	f.line("    public fun reset() {")
	f.line("        which = %s", u.d.idConst)
	g.emitUnionSlotReset(f, "        ", u.d)
	f.line("    }")
	f.blank()
	f.line("    public companion object {")
	for _, o := range u.opts {
		f.line("        public const val %s: Int = %d", o.idConst, o.f.ID)
	}
	f.line("    }")
	f.line("}")
	f.blank()
}

// negate is the Kotlin negation of a writes-expression: a single `!operand`
// (`!s.isDefault()`) loses its `!`, anything else is parenthesised.
func negate(expr string) string {
	if strings.HasPrefix(expr, "!") && !strings.ContainsAny(expr, " |&") {
		return expr[1:]
	}
	return "!(" + expr + ")"
}

// emitUnionSlotReset puts an option's slot at the option's default: in place
// where the slot holds a reusable object, created where it holds none yet.
func (g *gen) emitUnionSlotReset(f *kfile, ind string, o *unionOpt) {
	switch {
	case o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion:
		f.line("%sval s = %s; if (s == null) %s = %s() else s.reset()", ind, o.slot, o.slot, g.typeName(o.f.Ref.Key))
	case unionMutable(o.f): // wrapper list
		f.line("%sval s = %s; if (s == null) %s = mutableListOf() else s.clear()", ind, o.slot, o.slot)
	default:
		f.line("%s%s = %s", ind, o.slot, g.ktDefaultValue(o.f))
	}
}

// emitUnionAccessors writes the property, has<Opt>() and -- for a kind edited in
// place -- mutable<Opt>() of one option.
func (g *gen) emitUnionAccessors(f *kfile, o *unionOpt) {
	t := g.ktType(o.f)
	dep := func() {
		if o.f.Deprecated {
			f.line("    @Deprecated(\"This field is deprecated and may be removed in a future version.\")")
		}
	}
	f.blank()
	f.kdoc("    ", fieldDoc(o.orig, generator.BoundNote(o.orig, generator.StorageDynamic)))
	dep()
	// The getter reads the slot only while the option is held; otherwise it
	// answers the option's default -- a fresh object for a kind edited in place,
	// so nothing a caller does to it reaches the union -- and stores nothing.
	held := "this." + o.slot
	if unionSlotNullable(o.f) {
		held += "!!"
	}
	if o.jvmSet != "" {
		f.line("    %s", o.jvmSet)
	}
	f.line("    public var %s: %s", o.prop, t)
	f.line("        get() = if (which == %s) %s else %s", o.idConst, held, g.ktDefaultValue(o.f))
	f.line("        set(v) { which = %s; %s = v }", o.idConst, o.slot)
	dep()
	f.line("    public fun has%s(): Boolean = which == %s", o.base, o.idConst)
	if !unionMutable(o.f) {
		return
	}
	// Select if not held: an option that is already held is returned untouched,
	// so a decode that reaches it again -- a repeated or re-opened occurrence, a
	// resumed feed -- continues it (MESSAGE_SPEC §7.4) instead of wiping it.
	dep()
	f.line("    public fun mutable%s(): %s {", o.base, t)
	f.line("        var s = %s", o.slot)
	if o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion {
		f.line("        if (s == null) { s = %s(); %s = s } else if (which != %s) s.reset()", g.typeName(o.f.Ref.Key), o.slot, o.idConst)
	} else {
		f.line("        if (s == null) { s = mutableListOf(); %s = s } else if (which != %s) s.clear()", o.slot, o.idConst)
	}
	f.line("        which = %s", o.idConst)
	f.line("        return s")
	f.line("    }")
}

// memberName is the member a decode path or a store names for field fld of an
// object: the plain member, or -- in a union -- the option's property, whose
// setter is the §7.4.1 switch.
func memberName(fld *ir.Field, uni bool) string {
	if uni {
		return unionOptProp(fld.Name)
	}
	return ktIdent(fld.Name)
}

// memberPath is the expression a decode path takes to reach member fld of the
// object at path: the member itself, or -- when that object is a union and the
// option is edited in place -- the option's mutable accessor, which selects it at
// its default unless it is already held. Every path into a struct, union or
// wrapper-array option goes through it, so a store below an option is correct
// even where no begin arm ran first.
func memberPath(path string, fld *ir.Field, uni bool) string {
	if uni && unionMutable(fld) {
		return path + ".mutable" + naming.Pascal(fld.Name) + "()"
	}
	return path + "." + memberName(fld, uni)
}

// emitUnionJSONFns emits the harness's to/from for a union type: exactly ONE
// member, the held option, `{"<option>": value}` -- printed even when that is
// the default option at its default. `from` selects each member it reads through
// the option's property or mutable accessor, so the last member read wins, as
// the last option on the wire does.
func (g *gen) emitUnionJSONFns(f *kfile, key string, nt *ir.NamedType) {
	u := g.unionShapeOf(key, nt)
	dep := anyDeprecated(nt.Fields)
	if dep {
		f.line("    %s", deprecationSuppress)
	}
	f.line("    internal fun to(o: %s, b: kotlin.text.StringBuilder) {", u.typeName)
	f.line("        b.append('{')")
	f.line("        when (o.which) {")
	for _, o := range u.opts {
		f.line("            %s.%s -> {", u.typeName, o.idConst)
		f.line("                b.append(%s)", ktStringLit("\""+o.f.Name+"\":"))
		g.emitToAt(f, "                ", o.f, "o."+o.prop)
		f.line("            }")
	}
	f.line("        }")
	f.line("        b.append('}')")
	f.line("    }")
	if dep {
		f.line("    %s", deprecationSuppress)
	}
	f.line("    internal fun from(j: kotlin.collections.Map<String, _JsonValue>, o: %s) {", u.typeName)
	f.line("        for ((k, e) in j) {")
	f.line("            if (e.isNull) continue")
	f.line("            when (k) {")
	for _, o := range u.opts {
		f.line("                %s -> {", ktStringLit(o.f.Name))
		const ind = "                    "
		switch {
		case o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion:
			f.line("%sfrom(e.obj(), o.mutable%s())", ind, o.base)
		case unionMutable(o.f): // wrapper list: filled in place
			f.line("%sval _u = o.mutable%s()", ind, o.base)
			g.jsonFromArray(f, ind, "_u", "e.arr()", o.f.Elem, o.f.ElemRef, o.f.ElemItems, 0)
		default:
			g.emitFromAt(f, ind, o.f, "o."+o.prop)
		}
		f.line("                }")
	}
	f.line("            }")
	f.line("        }")
	f.line("    }")
}
