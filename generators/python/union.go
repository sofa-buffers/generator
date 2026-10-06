package python

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A schema union is a dataclass that holds exactly ONE option (MESSAGE_SPEC
// §4.2): two dataclass fields, `_which` (the held option's id) and `_value` (its
// value), and per option:
//
//	<OPT>_ID: ClassVar[int]   the option's id
//	which                     the id of the option held (read-only property)
//	<opt>                     property: the held value, else the option's
//	                          default -- a fresh object for a struct, union or
//	                          array option -- storing nothing; the setter selects
//	                          <opt> and stores the value it is given (the
//	                          reference, not a copy)
//	has_<opt>()               whether <opt> is held
//	mutable_<opt>()           (struct, union and array options) select <opt> at
//	                          its default unless it is held, and return it
//	clear()                   back to the default: default_id at its own default
//	serialize(e)              one arm per option
//
// One `_value` slot, not one per option: CPython stores every attribute as an
// object reference, so there is no representation to keep monomorphic, and a
// slot per option would only add dataclass fields. A dataclass still, so
// `__eq__`/`__repr__` are the dataclass's and not emitted code.
//
// Everything emitted here differs per schema -- the ids, one accessor set and one
// encode arm per option, the option types. Selection is two attribute stores; no
// generic tagged-union helper exists, in the module or in corelib-py.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	// f is the option as a field, with the empty default of a string, blob or
	// array option dropped (MESSAGE_SPEC §4.2 allows no other), so every
	// field-shaped helper treats it as "no declared default".
	f       *ir.Field
	orig    *ir.Field // the option as declared, for its documentation
	prop    string    // the property (keyword- and member-mangled)
	has     string    // has_<name>
	mut     string    // mutable_<name>, "" for a kind replaced whole
	idConst string    // <NAME>_ID
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

// unionOptProp is the property an option is reached through: the option's name,
// mangled as a struct member's is (pyIdent), with the trailing underscore where it
// lands on one of the members only a union has (unionReserved) or is spelled
// like a member the union derives from an option (unionDerivedShape).
//
// That makes the whole union namespace injective without a check: the derived
// members (`has_<opt>`, `mutable_<opt>`, `<OPT>_ID`) never end with `_` and are
// distinct per option (options have distinct folds), and every property that
// could spell one of them is escaped.
func unionOptProp(name string) string {
	p := pyIdent(name)
	if p == name && (unionReserved[name] || unionDerivedShape(name)) {
		return p + "_"
	}
	return p
}

// unionMutable reports whether an option gets a mutable_<opt>() accessor: the
// kinds edited in place -- a struct or union member by member, an array (a
// Python list) element by element. A scalar, string or blob is immutable here
// and is replaced whole through the property.
func unionMutable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion, ir.KindArray:
		return true
	}
	return false
}

// unionShapeOf returns the union's shape, built once per type.
func (g *gen) unionShapeOf(nt *ir.NamedType) *unionShape {
	if u, ok := g.unions[nt]; ok {
		return u
	}
	u := &unionShape{typeName: typeIdent(nt), nt: nt, byField: map[*ir.Field]*unionOpt{}}
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
			has:     "has_" + fld.Name,
			idConst: strings.ToUpper(fld.Name) + "_ID",
			isD:     nt.IsDefaultOption(fld),
		}
		if unionMutable(fld) {
			o.mut = "mutable_" + fld.Name
		}
		u.opts = append(u.opts, o)
		u.byField[fld] = o
		if o.isD {
			u.d = o
		}
	}
	if g.unions == nil {
		g.unions = map[*ir.NamedType]*unionShape{}
	}
	g.unions[nt] = u
	return u
}

// pyDefaultValue is a field's default as an EXPRESSION -- what pyDefault spells
// as a dataclass default, with a fresh object for the mutable kinds instead of a
// `field(default_factory=...)`.
func (g *gen) pyDefaultValue(fld *ir.Field) string {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return g.refName(fld.Ref) + "()"
	case ir.KindArray:
		if lit, ok := g.pyNativeArrayDefault(fld); ok {
			return g.arrayDefaultValue(fld, lit)
		}
		return "[]"
	}
	return g.pyDefault(fld)
}

// emitUnionFields writes the two dataclass fields and the id constants.
func (g *gen) emitUnionFields(f *pyfile, u *unionShape) {
	f.line("    #: The id of the option held; read it through ``which``.")
	f.line("    _which: int = %d", u.d.f.ID)
	f.line("    #: The held option's value.")
	f.line("    _value: object = %s", g.pyDefault(u.d.f))
	f.blank()
	for _, o := range u.opts {
		f.line("    %s: ClassVar[int] = %d", o.idConst, o.f.ID)
	}
}

// emitUnionAccessors writes which, one property (+ setter), has_<opt>() and --
// where the kind is edited in place -- mutable_<opt>() per option, and clear().
func (g *gen) emitUnionAccessors(f *pyfile, u *unionShape) {
	f.line("    @property")
	f.line("    def which(self) -> int:")
	f.line("        \"\"\"The id of the option held: one of the ``*_ID`` constants.\"\"\"")
	f.line("        return self._which")
	f.blank()
	for _, o := range u.opts {
		annot := g.pyAnnot(o.f)
		f.line("    @property")
		f.line("    def %s(self) -> %s:", o.prop, annot)
		if doc := pyFieldDocLines(o.orig); len(doc) == 1 {
			f.line(`        """%s"""`, doc[0])
		} else if len(doc) > 1 {
			f.line(`        """%s`, doc[0])
			for _, dl := range doc[1:] {
				f.line("        %s", dl)
			}
			f.line(`        """`)
		}
		f.line("        return self._value if self._which == %d else %s", o.f.ID, g.pyDefaultValue(o.f))
		f.blank()
		f.line("    @%s.setter", o.prop)
		f.line("    def %s(self, v: %s) -> None:", o.prop, annot)
		f.line("        self._which = %d", o.f.ID)
		f.line("        self._value = v")
		f.blank()
		f.line("    def %s(self) -> bool:", o.has)
		f.line("        return self._which == %d", o.f.ID)
		f.blank()
		if o.mut != "" {
			// Select if not held: an option already held is returned as it is,
			// never reset -- decode reaches it once per occurrence and continues
			// it (MESSAGE_SPEC §7.4).
			f.line("    def %s(self) -> %s:", o.mut, annot)
			f.line("        if self._which != %d:", o.f.ID)
			f.line("            self._which = %d", o.f.ID)
			f.line("            self._value = %s", g.pyDefaultValue(o.f))
			f.line("        return self._value")
			f.blank()
		}
	}
	f.line("    def clear(self) -> None:")
	f.line("        \"\"\"Back to the default: ``%s`` at its own default.\"\"\"", u.d.orig.Name)
	f.line("        self._which = %d", u.d.f.ID)
	f.line("        self._value = %s", g.pyDefaultValue(u.d.f))
	f.blank()
}

// emitUnionIsDefault: a union is default when it holds default_id at that
// option's own default. Every other option is written whatever its value, so
// holding it is never default.
func (g *gen) emitUnionIsDefault(f *pyfile, u *unionShape) {
	f.line("    def _is_default(self) -> bool:")
	f.line("        return self._which == %d and %s", u.d.f.ID, g.fieldIsDefaultExprAt(u.d.f, "self._value"))
	f.blank()
}

// emitUnionSerialize writes the held option, one arm per option: default_id
// exactly as a field of its kind (omitted at its default), every other option
// forced (emitMarshalAt).
func (g *gen) emitUnionSerialize(f *pyfile, u *unionShape) {
	f.line("    def serialize(self, e: Encoder) -> None:")
	f.line("        _w = self._which")
	f.line("        _v = self._value")
	first := true
	for _, o := range u.opts {
		f.line("        %s _w == %d:", kw(&first), o.f.ID)
		g.emitMarshalAt(f, o.f, "_v", "            ", !o.isD)
	}
	f.blank()
}

// emitUnionJSON writes to_jsonable (`{"<option>": value}`, the held option only)
// and from_jsonable (each member present selects its option through the setter).
func (g *gen) emitUnionJSON(f *pyfile, u *unionShape) {
	f.line("    def to_jsonable(self) -> dict:")
	f.line("        _w = self._which")
	f.line("        _v = self._value")
	for _, o := range u.opts {
		if o.isD {
			continue
		}
		f.line("        if _w == %d:", o.f.ID)
		f.line("            return {%q: %s}", o.orig.Name, g.toJSONExprAt(o.f, "_v"))
	}
	f.line("        return {%q: %s}", u.d.orig.Name, g.toJSONExprAt(u.d.f, "_v"))
	f.blank()
	f.line("    @classmethod")
	f.line("    def from_jsonable(cls, d: dict) -> %q:", u.typeName)
	f.line("        o = cls()")
	for _, o := range u.opts {
		f.line("        if %q in d:", o.orig.Name)
		g.fromJSONStmt(f, o.f, "o."+o.prop)
	}
	f.line("        return o")
	f.blank()
}
