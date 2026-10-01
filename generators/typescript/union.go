package typescript

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// A schema union is a TypeScript class that holds exactly ONE option (MESSAGE_SPEC
// §4.2): a private tag `__which` plus one private, typed slot per option. Its API:
//
//	static readonly <OPT>_ID   the option's id
//	which                      the id of the option held (getter)
//	<opt>                      getter: the held value, else the option's default
//	                           -- a fresh object for an object kind -- storing
//	                           nothing; setter: selects <opt> and stores the value
//	                           (the reference, for an object)
//	has<Opt>()                 whether <opt> is held
//	mutable<Opt>()             (struct, union, wrapper-array options) select <opt>
//	                           at its default unless it is held, and return it
//	<opt>Fp32Raw               (fp32 options) the NaN wire bytes, as a struct's
//	                           fp32 member has them (§4.6)
//	clear()                    back to the default: default_id at its own default
//	serialize(os)              one `switch` arm per option
//
// One slot per option, never one shared slot: V8 keeps a representation per
// property, and a property that holds a number at one time and a boolean or an
// object at another is generalised to Tagged -- after which every store of a
// non-Smi double allocates a HeapNumber. Every slot is initialised in the class
// body, so the hidden class is fixed at construction: a scalar, string or typed
// array slot at the option's default (a typed array's is the module's shared
// empty instance), an object slot at `null` -- only default_id's is built.
//
// A real switch releases what the option left behind held (`__leave`, emitted
// only where some option holds anything): an object slot goes back to `null`, a
// string to "", a typed array to the shared empty one, so a union does not keep a
// discarded payload alive. An option selected again is built fresh; an object
// obtained from it earlier is never reset behind the caller's back.
//
// Everything emitted here differs per schema -- the slots and their types, the
// ids, one accessor set and one encode arm per option. Selection is a tag store;
// no generic tagged-union helper exists, in the module or in corelib-ts.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	// f is the option as a field, with the empty default of a string, blob or
	// array option dropped (MESSAGE_SPEC §4.2 allows no other), so every
	// field-shaped helper treats it as "no declared default".
	f       *ir.Field
	orig    *ir.Field // the option as declared, for its documentation
	prop    string    // the public getter/setter (mangled)
	base    string    // <Opt>, the Pascal option name
	has     string    // has<Opt>() (derivedMember)
	mutable string    // mutable<Opt>() ("" for a kind replaced whole)
	slot    string    // the private slot: "_" + prop, never `__which`/`__leave`
	raw     string    // the public fp32 raw-bytes property: prop + "Fp32Raw" ("" unless fp32)
	rawSlot string    // its private slot
	idConst string    // <OPT>_ID
	isD     bool      // the union's default option (default_id)
}

// unionShape is one union type with its options.
type unionShape struct {
	typeName string // declared (escaped) class name
	raw      string // unescaped identifier, for the derived names
	nt       *ir.NamedType
	opts     []*unionOpt
	d        *unionOpt
	byField  map[*ir.Field]*unionOpt
}

// unionOptProp is the getter/setter an option is reached through: the option's
// name, with a trailing underscore where it is a member of every union class
// (fixedMember) or has the shape of a member derived from an option -- an fp32
// raw-bytes companion, has<Opt>, mutable<Opt> (shaped).
func unionOptProp(name string) string {
	if fixedMember(name, true) || shaped(name, true) {
		return name + "_"
	}
	return name
}

// unionMutable reports whether an option gets a mutable<Opt>() accessor: the
// kinds edited in place -- a struct or union member by member, a wrapper array
// element by element. A scalar, string or blob is replaced whole, and so is a
// native array: a typed array cannot grow, and a Long[] is replaced through its
// converting setter like a struct's Long[] member.
func unionMutable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return true
	case ir.KindArray:
		return !nativeArrayElem(fld.Elem)
	}
	return false
}

func (g *gen) unionShapeOf(key string, nt *ir.NamedType) *unionShape {
	u := &unionShape{typeName: g.typeName(key), raw: g.typeRaw(key), nt: nt, byField: map[*ir.Field]*unionOpt{}}
	// Every member is a function of the option's own name alone, so an option
	// added beside it never renames it; reserved.go proves that no two meet.
	for _, fld := range nt.Fields {
		cp := *fld
		switch fld.Kind {
		case ir.KindString, ir.KindBlob, ir.KindArray:
			cp.Default = nil
		}
		prop := unionOptProp(fld.Name)
		o := &unionOpt{
			f:    &cp,
			orig: fld,
			prop: prop,
			base: naming.Pascal(fld.Name),
			slot: "_" + prop,
			// Upper-cased with its underscores kept, so it is as fold-unique as
			// the option name, and the fixed `_ID` suffix meets no static.
			idConst: strings.ToUpper(fld.Name) + "_ID",
			isD:     nt.IsDefaultOption(fld),
		}
		o.has = derivedMember("has" + o.base)
		if unionMutable(o.f) {
			o.mutable = derivedMember("mutable" + o.base)
		}
		if fp32RawCompanion(o.orig) {
			o.raw = fp32RawName(prop)
			o.rawSlot = "_" + o.raw
		}
		u.opts = append(u.opts, o)
		u.byField[fld] = o
		if o.isD {
			u.d = o
		}
	}
	return u
}

// unionIsRef reports whether an option's slot holds an OBJECT that is built on
// selection (and is `null` while the option was never selected or was left): a
// struct, a union, a wrapper array, a Long[] and a blob. A typed native array is
// not one -- its default is the module's shared empty instance, which costs
// nothing to hold -- and neither is a string.
func (g *gen) unionIsRef(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion, ir.KindBlob:
		return true
	case ir.KindArray:
		return !nativeArrayElem(fld.Elem) || g.tsTypedArray(fld.Elem, fld.ElemRef) == ""
	}
	return false
}

// unionLeave is the statement releasing what an option left behind holds, "" when
// it holds nothing worth releasing (a number, a boolean, a bigint, an immutable
// Long).
func (g *gen) unionLeave(o *unionOpt) string {
	switch {
	case g.unionIsRef(o.f):
		return fmt.Sprintf("this.%s = null;", o.slot)
	case o.f.Kind == ir.KindString:
		return fmt.Sprintf("this.%s = \"\";", o.slot)
	case o.f.Kind == ir.KindArray:
		return fmt.Sprintf("this.%s = %s;", o.slot, g.tsDefault(o.f))
	}
	return ""
}

// hasLeave reports whether any option of u leaves something to release, i.e.
// whether the union needs `__leave` at all.
func (g *gen) hasLeave(u *unionShape) bool {
	for _, o := range u.opts {
		if g.unionLeave(o) != "" {
			return true
		}
	}
	return false
}

// selectStmt renders "o becomes the held option" at ind. With `__leave`, a real
// switch releases the option left behind first; the test keeps a store into the
// held option from releasing its own slot.
func (g *gen) selectStmt(f *tsfile, ind string, u *unionShape, o *unionOpt) {
	if g.hasLeave(u) {
		f.line("%sif (this.__which !== %d) {", ind, o.f.ID)
		f.line("%s  this.__leave();", ind)
		f.line("%s  this.__which = %d;", ind, o.f.ID)
		f.line("%s}", ind)
		return
	}
	f.line("%sthis.__which = %d;", ind, o.f.ID)
}

// unionHeld is the expression reading an option's slot while it is held.
func (g *gen) unionHeld(o *unionOpt) string {
	if g.unionIsRef(o.f) {
		return "this." + o.slot + "!"
	}
	return "this." + o.slot
}

// emitUnionClass writes the class of one union type.
func (g *gen) emitUnionClass(f *tsfile, key string, nt *ir.NamedType) {
	u := g.unionShapeOf(key, nt)
	f.emitDoc("", nt.Summary)
	f.line("export class %s {", u.typeName)
	for _, o := range u.opts {
		f.line("  /** The id of option `%s`. */", o.f.Name)
		f.line("  static readonly %s = %d;", o.idConst, o.f.ID)
	}
	f.blank()
	f.line("  private __which: number = %d;", u.d.f.ID)
	for _, o := range u.opts {
		t := g.tsType(o.f)
		switch {
		case !g.unionIsRef(o.f):
			f.line("  private %s: %s = %s;", o.slot, t, g.tsDefault(o.f))
		case o.isD:
			f.line("  private %s: %s | null = %s;", o.slot, t, g.tsDefault(o.f))
		default:
			f.line("  private %s: %s | null = null;", o.slot, t)
		}
		if o.rawSlot != "" {
			f.line("  private %s: Uint8Array | null = null;", o.rawSlot)
		}
	}
	f.blank()
	f.line("  /** The id of the option this union holds: one of the `..._ID` constants. */")
	f.line("  get which(): number {")
	f.line("    return this.__which;")
	f.line("  }")
	for _, o := range u.opts {
		g.emitUnionAccessors(f, u, o)
	}
	if g.hasLeave(u) {
		f.blank()
		f.line("  // Releases what the held option holds, before another one is selected.")
		f.line("  private __leave(): void {")
		f.line("    switch (this.__which) {")
		for _, o := range u.opts {
			if s := g.unionLeave(o); s != "" {
				f.line("      case %d:", o.f.ID)
				f.line("        %s", s)
				f.line("        break;")
			}
		}
		f.line("    }")
		f.line("  }")
	}
	f.blank()
	f.line("  /** Back to the default: `%s`, at its own default. */", u.d.f.Name)
	f.line("  clear(): void {")
	if g.hasLeave(u) {
		f.line("    this.__leave();")
	}
	f.line("    this.__which = %d;", u.d.f.ID)
	f.line("    this.%s = %s;", u.d.slot, g.tsDefault(u.d.f))
	if u.d.rawSlot != "" {
		f.line("    this.%s = null;", u.d.rawSlot)
	}
	f.line("  }")
	f.blank()

	// serialize: default_id is written like an ordinary field of its kind
	// (omitted at its default); every other option is FORCED (MESSAGE_SPEC §4.2)
	// -- a scalar at its default, an empty string/blob/array, and a
	// struct/union/wrapper option as a present frame closed with the keeping end.
	f.line("  serialize(os: OStream): void {")
	f.line("    switch (this.__which) {")
	for _, o := range u.opts {
		f.line("      case %d: {", o.f.ID)
		raw := ""
		if o.rawSlot != "" {
			raw = "this." + o.rawSlot
		}
		g.emitMarshalAt(f, "        ", o.f, g.unionHeld(o), raw, !o.isD)
		f.line("        break;")
		f.line("      }")
	}
	f.line("    }")
	f.line("  }")
	f.blank()

	f.line("  // True iff serialize writes nothing: default_id held, at its own default.")
	f.line("  isDefault(): boolean {")
	f.line("    return this.__which === %d && %s;", u.d.f.ID, g.fieldIsDefaultExprAt(u.d.f, g.unionHeld(u.d)))
	f.line("  }")
	f.blank()

	// JSON: exactly ONE member, the held option -- printed even when that is the
	// default option at its default. fromJSON selects each member it reads, so a
	// member read later wins.
	f.line("  toJSON(): Record<string, unknown> {")
	f.line("    switch (this.__which) {")
	for _, o := range u.opts {
		if o.isD {
			continue
		}
		f.line("      case %d:", o.f.ID)
		f.line("        return { %q: %s };", o.f.Name, g.toJSONExprAt(o.f, g.unionHeld(o)))
	}
	f.line("    }")
	f.line("    return { %q: %s };", u.d.f.Name, g.toJSONExprAt(u.d.f, g.unionHeld(u.d)))
	f.line("  }")
	f.blank()
	f.line("  static fromJSON(d: Record<string, unknown>): %s {", u.typeName)
	f.line("    const o = new %s();", u.typeName)
	for _, o := range u.opts {
		f.line("    if (Object.prototype.hasOwnProperty.call(d, %q)) %s;", o.f.Name, g.fromJSONStmtAt(o.f, "o."+o.prop))
	}
	f.line("    return o;")
	f.line("  }")
	f.blank()
	g.emitDecode(f, u.typeName, u.raw)
	f.line("}")
	f.blank()
}

// emitUnionAccessors writes the getter, setter, has<Opt>, the raw-bytes pair of
// an fp32 option and -- for a kind edited in place -- mutable<Opt>().
func (g *gen) emitUnionAccessors(f *tsfile, u *unionShape, o *unionOpt) {
	t := g.tsType(o.f)
	id := o.f.ID
	f.blank()
	f.emitDoc("  ", fieldDoc(o.orig, generator.BoundNote(o.orig, generator.StorageDynamic)))
	// The getter reads the slot only while the option is held; otherwise it
	// answers the option's default -- a fresh object for an object kind, so
	// nothing a caller does to it reaches the union -- and stores nothing.
	f.line("  get %s(): %s {", o.prop, t)
	f.line("    return this.__which === %d ? %s : %s;", id, g.unionHeld(o), g.tsDefault(o.f))
	f.line("  }")
	// The setter selects the option and stores the value -- the reference, for an
	// object, exactly as assigning a struct member does. A Long-backed option
	// takes what a struct's Long member takes and converts once, here.
	param, store := "v: "+t, "v"
	if g.longBacked(o.f) {
		if isBig(o.f.Kind) {
			param, store = "v: Long | bigint | number", "Long.fromValue(v)"
		} else {
			param, store = "v: "+g.longSetterParam(o.f), g.longConvert("v", o.f.Elem, o.f.ElemItems, 0)
		}
	}
	f.line("  set %s(%s) {", o.prop, param)
	g.selectStmt(f, "    ", u, o)
	f.line("    this.%s = %s;", o.slot, store)
	if o.rawSlot != "" {
		// A new value drops the NaN bytes captured for the old one (§4.6).
		f.line("    this.%s = null;", o.rawSlot)
	}
	f.line("  }")
	f.line("  %s(): boolean {", o.has)
	f.line("    return this.__which === %d;", id)
	f.line("  }")
	if o.rawSlot != "" {
		f.line("  /**")
		f.line("   * Wire bytes of `%s`, captured on decode only when the decoded value is a", o.prop)
		f.line("   * NaN, so that a signaling NaN re-encodes bit-for-bit; null while another")
		f.line("   * option is held. Setting them selects `%s` (at its default unless held).", o.prop)
		f.line("   */")
		f.line("  get %s(): Uint8Array | null {", o.raw)
		f.line("    return this.__which === %d ? this.%s : null;", id, o.rawSlot)
		f.line("  }")
		f.line("  set %s(b: Uint8Array | null) {", o.raw)
		f.line("    if (this.__which !== %d) {", id)
		if g.hasLeave(u) {
			f.line("      this.__leave();")
		}
		f.line("      this.__which = %d;", id)
		f.line("      this.%s = %s;", o.slot, g.tsDefault(o.f))
		f.line("    }")
		f.line("    this.%s = b;", o.rawSlot)
		f.line("  }")
	}
	if !unionMutable(o.f) {
		return
	}
	// Select if not held: an option that is already held is returned untouched,
	// so a decode that reaches it again -- a repeated or re-opened occurrence, a
	// resumed feed -- continues it (MESSAGE_SPEC §7.4) instead of wiping it. A
	// real switch builds the option fresh at its default.
	f.line("  %s(): %s {", o.mutable, t)
	f.line("    if (this.__which !== %d) {", id)
	if g.hasLeave(u) {
		f.line("      this.__leave();")
	}
	f.line("      this.__which = %d;", id)
	f.line("      this.%s = %s;", o.slot, g.tsDefault(o.f))
	f.line("    }")
	f.line("    return this.%s!;", o.slot)
	f.line("  }")
}
