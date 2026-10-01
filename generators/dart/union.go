package dart

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// A schema union is a Dart class that holds exactly ONE option (MESSAGE_SPEC
// §4.2): a private tag `_which` plus one private, typed slot per option. Its API:
//
//	<opt>Id           the option's id, a `static const int`
//	which             the id of the option held (getter)
//	<opt>             getter: the held value, else the option's default -- a
//	                  fresh object for a reference kind -- storing nothing
//	<opt> = v         setter (scalar, struct, union, wrapper-array option):
//	                  selects <opt> and stores v (the reference, for an object)
//	has<Opt>          whether <opt> is held (getter)
//	mutable<Opt>()    (struct, union, string, blob and array options) select
//	                  <opt> at its default unless it is held, and return the slot
//	<opt>Fp32Bits     (fp32 options) the captured NaN wire bits, as a struct's
//	                  fp32 member has them (§4.6)
//	reset()           back to the default: default_id at its own default, in place
//	serialize(e)      one `switch` arm per option
//
// Each slot has the option's own member type -- `int`, `double`, `bool`, the
// corelib's `Inline…` destination, a `List`, the generated class -- so no access
// boxes or casts. Every reference slot (a destination, a wrapper list, a struct
// or union) is created on its option's FIRST selection and KEPT when another
// option is selected; selecting it again resets it in place (`length = 0`,
// `clear()`, `reset()`), so a reused destination (tryDecode's reset() + decode)
// re-selects without allocating, and a fresh union allocates only default_id's
// slot. A string, blob or native-array option has no setter, exactly as a
// struct's destination member is `final`: it is filled in place through
// mutable<Opt>(), which is also the only place its destination is created --
// with the declared element width the codec checks every decoded element
// against, and at the schema bound where that bound is small (eagerDestBytes).
//
// Everything emitted here differs per schema -- the slots and their types, the
// ids, one accessor set and one encode arm per option. Selection is a tag store;
// no generic tagged-union helper exists, in the module or in corelib-dart.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	// f is the option as a field, with the empty default of a string, blob or
	// array option dropped (MESSAGE_SPEC §4.2 allows no other), so every
	// field-shaped helper treats it as "no declared default".
	f       *ir.Field
	orig    *ir.Field // the option as declared, for its documentation
	prop    string    // the public getter/setter: the option's name
	has     string    // has<Opt>
	mutable string    // mutable<Opt> ("" unless unionMutable)
	slot    string    // the private slot: "_" + prop
	bits    string    // the public fp32 raw-bits property ("" unless fp32)
	bitSlot string    // its private slot
	idConst string    // <opt>Id
	isD     bool      // the union's default option (default_id)
}

// unionShape is one union type with its options.
type unionShape struct {
	typeName string
	nt       *ir.NamedType
	opts     []*unionOpt
	d        *unionOpt
	byField  map[*ir.Field]*unionOpt
}

// unionMutable reports whether an option gets a mutable<Opt>() accessor: every
// kind that is edited in place -- a struct or union member by member, a wrapper
// list element by element, a destination (string, blob, native array) through
// its assign()/assignString(). Only the scalars (fp included) have none.
func unionMutable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion, ir.KindString, ir.KindBlob, ir.KindArray:
		return true
	}
	return false
}

// unionHasSetter reports whether an option has a setter: every kind but a
// destination, which -- like a struct's `final` destination member -- is filled
// in place through mutable<Opt>().
func unionHasSetter(fld *ir.Field) bool { return !isDest(fld) }

// unionShapeOf is the union type at graph key `key`, with every member name
// its class declares (built once, with the rest of the module's members).
func (g *gen) unionShapeOf(key string, _ *ir.NamedType) *unionShape {
	return g.members().unions[key]
}

// buildUnionShape names the members of one union class. A class's static and
// instance members share one namespace -- the getters, has/mutable members, id
// constants, raw-bits properties, private slots, and the union's own `which` /
// `_which` / serialize / reset -- and every option's names are spelled from
// that option's name alone (optionMembers), so they are distinct by
// construction (see "member names" in reserved.go) and an option's names never
// change with the other options of the union.
func (g *gen) buildUnionShape(key string, nt *ir.NamedType) *unionShape {
	u := &unionShape{typeName: g.typeName(key), nt: nt, byField: map[*ir.Field]*unionOpt{}}
	for _, fld := range nt.Fields {
		o := optionMembers(fld)
		o.isD = nt.IsDefaultOption(fld)
		u.opts = append(u.opts, o)
		u.byField[fld] = o
		if o.isD {
			u.d = o
		}
	}
	return u
}

// optionMembers is one union option with every member name it gets, from its
// own name and kind alone.
func optionMembers(fld *ir.Field) *unionOpt {
	cp := *fld
	switch fld.Kind {
	case ir.KindString, ir.KindBlob, ir.KindArray:
		cp.Default = nil
	}
	shaped := optionShaped(fld.Name)
	s := stem(fld.Name, shaped)
	pascal := naming.Pascal(fld.Name)
	o := &unionOpt{
		f:       &cp,
		orig:    fld,
		prop:    escapeMember(fld.Name, shaped, func(n string) bool { return memberReserved(n) || unionFixed[n] }),
		idConst: s + "Id",
		has:     "has" + pascal,
	}
	if unionMutable(fld) {
		o.mutable = "mutable" + pascal
	}
	o.slot = "_" + o.prop
	if dartPrivateHelpers[o.slot] {
		o.slot += "_"
	}
	if fld.Kind == ir.KindFP32 {
		o.bits = s + "Fp32Bits"
		o.bitSlot = "_" + o.bits
	}
	return o
}

// unionIsRef reports whether an option's slot holds an object (nullable, created
// on first selection) rather than a value.
func unionIsRef(fld *ir.Field) bool { return unionMutable(fld) }

// unionNew is the expression building an option's storage at its default: the
// ordinary member initializer (a destination at its eager bound, with the
// declared element width; an empty list; a fresh struct/union).
func (g *gen) unionNew(o *unionOpt) string {
	return strings.TrimPrefix(g.dartInit(o.f), " = ")
}

// unionDetached is what the getter of an option that is NOT held answers: its
// default, never stored. A destination is answered at capacity 0 -- it holds
// nothing, and nothing written into it reaches the union.
func (g *gen) unionDetached(o *unionOpt) string {
	switch {
	case o.f.Kind == ir.KindString || o.f.Kind == ir.KindBlob:
		return g.dartType(o.f) + "(0)"
	case isDest(o.f):
		return fmt.Sprintf("%s(0%s)", inlineArrayType(o.f.Elem), g.rangeArg(o.f.Elem, o.f.ElemRef))
	}
	return g.unionNew(o)
}

// emitUnionClass writes the class of one union type (and its decode visitor
// when a message reaches it).
func (g *gen) emitUnionClass(f *dfile, key string, nt *ir.NamedType, withVisitor bool) {
	u := g.unionShapeOf(key, nt)
	emitDoc(f, "", nt.Summary)
	f.line("class %s {", u.typeName)
	for _, o := range u.opts {
		f.line("  /// The id of option `%s`.", o.f.Name)
		f.line("  static const int %s = %d;", o.idConst, o.f.ID)
	}
	f.blank()
	f.line("  int _which = %s;", u.d.idConst)
	for _, o := range u.opts {
		switch {
		case !unionIsRef(o.f):
			f.line("  %s %s%s;", g.dartType(o.f), o.slot, g.dartInit(o.f))
		case o.isD:
			f.line("  %s? %s = %s;", g.dartType(o.f), o.slot, g.unionNew(o))
		default:
			f.line("  %s? %s;", g.dartType(o.f), o.slot)
		}
		if o.bitSlot != "" {
			f.line("  int? %s;", o.bitSlot)
		}
	}
	f.blank()
	f.line("  /// The id of the option this union holds: one of the `...Id` constants.")
	f.line("  int get which => _which;")
	for _, o := range u.opts {
		g.emitUnionAccessors(f, o)
	}
	f.blank()

	f.line("  void serialize(sofab.Encoder e) {")
	f.line("    switch (_which) {")
	for _, o := range u.opts {
		f.line("      case %s:", o.idConst)
		// default_id is written like an ordinary field of its kind (omitted at its
		// default); every other option is forced (MESSAGE_SPEC §4.2).
		acc := o.slot
		if unionIsRef(o.f) {
			f.line("        final v = %s!;", o.slot)
			acc = "v"
		}
		g.emitMarshalAt(f, "        ", o.f, acc, o.bitSlot, !o.isD)
	}
	f.line("    }")
	f.line("  }")
	f.blank()

	f.line("  /// Restores the default -- the default option at its own default -- in")
	f.line("  /// place: the storage of every option ever selected is kept, so a reused")
	f.line("  /// instance decodes without allocating.")
	f.line("  void reset() {")
	f.line("    _which = %s;", u.d.idConst)
	g.emitUnionSlotReset(f, "    ", u.d, true)
	f.line("  }")
	f.line("}")
	f.blank()

	if withVisitor {
		g.emitVisitor(f, u.typeName, g.rawTypeName(key), nt.Fields, u)
	}
}

// emitUnionSlotReset puts an option's slot at the option's default: in place
// where the slot holds an object, created where it holds none yet (create is
// false inside mutable<Opt>(), which creates on its own).
func (g *gen) emitUnionSlotReset(f *dfile, ind string, o *unionOpt, create bool) {
	var inPlace string
	switch {
	case o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion:
		inPlace = "s.reset();"
	case isDest(o.f):
		inPlace = "s.length = 0;"
	case o.f.Kind == ir.KindArray: // wrapper list
		inPlace = "s.clear();"
	default:
		f.line("%s%s = %s;", ind, o.slot, g.dartDefaultValue(o.f))
		if o.bitSlot != "" {
			f.line("%s%s = null;", ind, o.bitSlot)
		}
		return
	}
	f.line("%sfinal s = %s;", ind, o.slot)
	f.line("%sif (s == null) {", ind)
	f.line("%s  %s = %s;", ind, o.slot, g.unionNew(o))
	f.line("%s} else {", ind)
	f.line("%s  %s", ind, inPlace)
	f.line("%s}", ind)
}

// emitUnionAccessors writes the getter, setter, has<Opt> and -- for a kind
// edited in place -- mutable<Opt>() of one option.
func (g *gen) emitUnionAccessors(f *dfile, o *unionOpt) {
	t := g.dartType(o.f)
	dep := func() {
		if o.f.Deprecated {
			f.line("  @Deprecated('retained for backward compatibility only')")
		}
	}
	f.blank()
	emitDoc(f, "  ", fieldDoc(o.orig, generator.BoundNote(o.orig, generator.StorageDynamic)))
	dep()
	// The getter reads the slot only while the option is held; otherwise it
	// answers the option's default -- a fresh object for a reference kind, so
	// nothing a caller does to it reaches the union -- and stores nothing.
	held := o.slot
	if unionIsRef(o.f) {
		held += "!"
	}
	f.line("  %s get %s => _which == %s ? %s : %s;", t, o.prop, o.idConst, held, g.unionDetached(o))
	if unionHasSetter(o.f) {
		dep()
		if o.bitSlot != "" {
			// A new value drops the NaN bits captured for the old one (§4.6).
			f.line("  set %s(%s v) {", o.prop, t)
			f.line("    _which = %s;", o.idConst)
			f.line("    %s = v;", o.slot)
			f.line("    %s = null;", o.bitSlot)
			f.line("  }")
		} else {
			f.line("  set %s(%s v) {", o.prop, t)
			f.line("    _which = %s;", o.idConst)
			f.line("    %s = v;", o.slot)
			f.line("  }")
		}
	}
	dep()
	f.line("  bool get %s => _which == %s;", o.has, o.idConst)
	if o.bitSlot != "" {
		// The raw 32 wire bits of a NaN value, exactly as a struct's fp32 member
		// keeps them: PUBLIC, so a bit-exact consumer can read a signaling NaN and
		// a caller can emit one (set the value to NaN, then the bits).
		f.line("  /// The raw 32 wire bits of a NaN [%s], or null: a signaling NaN survives.", o.prop)
		dep()
		f.line("  int? get %s => _which == %s ? %s : null;", o.bits, o.idConst, o.bitSlot)
		dep()
		f.line("  set %s(int? bits) {", o.bits)
		f.line("    if (_which != %s) {", o.idConst)
		f.line("      _which = %s;", o.idConst)
		f.line("      %s = %s;", o.slot, g.dartDefaultValue(o.f))
		f.line("    }")
		f.line("    %s = bits;", o.bitSlot)
		f.line("  }")
	}
	if !unionMutable(o.f) {
		return
	}
	// Select if not held: an option that is already held is returned untouched,
	// so a decode that reaches it again -- a repeated or re-opened occurrence, a
	// resumed feed -- continues it (MESSAGE_SPEC §7.4) instead of wiping it.
	dep()
	f.line("  %s %s() {", t, o.mutable)
	f.line("    var s = %s;", o.slot)
	f.line("    if (s == null) {")
	f.line("      s = %s;", g.unionNew(o))
	f.line("      %s = s;", o.slot)
	f.line("    } else if (_which != %s) {", o.idConst)
	switch {
	case o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion:
		f.line("      s.reset();")
	case isDest(o.f):
		f.line("      s.length = 0;")
	default: // wrapper list
		f.line("      s.clear();")
	}
	f.line("    }")
	f.line("    _which = %s;", o.idConst)
	f.line("    return s;")
	f.line("  }")
}

// ---- JSON harness -----------------------------------------------------------

// emitUnionJSONCodec emits the harness's `_<T>__ToJson` / `_<T>__FromJson` for a
// union type: exactly ONE member, the held option, `{"<option>": value}` --
// printed even when that is the default option at its default. `_<T>__FromJson`
// selects each member it reads through the option's setter or mutable accessor,
// so the last member read wins, as the last option on the wire does.
func (g *gen) emitUnionJSONCodec(f *dfile, key string, nt *ir.NamedType) {
	u := g.unionShapeOf(key, nt)
	f.line("Map<String, dynamic> %s(%s m) {", toJSONName(g.rawTypeName(key)), u.typeName)
	f.line("  switch (m.which) {")
	for _, o := range u.opts {
		f.line("    case %s.%s:", u.typeName, o.idConst)
		f.line("      return <String, dynamic>{%s: %s};", dartStringLit(o.f.Name), g.jsonTo(o.f, "m."+o.prop))
	}
	f.line("  }")
	f.line("  throw StateError('%s holds no option');", u.typeName)
	f.line("}")
	f.blank()
	f.line("%s %s(Map<String, dynamic> j) {", u.typeName, fromJSONName(g.rawTypeName(key)))
	f.line("  final m = %s();", u.typeName)
	f.line("  for (final kv in j.entries) {")
	f.line("    switch (kv.key) {")
	for _, o := range u.opts {
		acc := "m." + o.prop
		if isDest(o.f) {
			acc = "m." + o.mutable + "()"
		}
		f.line("      case %s:", dartStringLit(o.f.Name))
		f.line("        %s", g.jsonFromStmt(o.f, acc, "kv.value"))
	}
	f.line("    }")
	f.line("  }")
	f.line("  return m;")
	f.line("}")
	f.blank()
}
