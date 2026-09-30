package golang

import (
	"fmt"
	"sort"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A schema union is a Go struct that holds exactly ONE option (MESSAGE_SPEC
// §4.2): an unexported tag plus one unexported, typed slot per option. Its API:
//
//	<Type><Opt>ID     the option's id, a package-level sofab.ID constant
//	Which()           the id of the option held
//	Has<Opt>()        whether <opt> is held
//	<Opt>()           <opt> when held, else a fresh copy of its default (stores nothing)
//	Set<Opt>(v)       select <opt> with the value v; the option held before is discarded
//	Mut<Opt>()        (struct, union and array options) select <opt> at its default
//	                  unless it is held, and return a pointer into its slot
//	Clear()           back to the default: default_id at its own default
//	Serialize(e)      one switch arm per option
//	MarshalJSON / UnmarshalJSON   {"<option>": value}, the held option only
//
// The tag stores the held id XOR default_id's id, so Go's zero value holds
// default_id -- the convention every other generated type keeps (a zero value is
// the default wherever the schema declares no non-zero one), and what lets a
// union element's gap fill be the collector's plain zero value plus setDefaults.
// For default_id 0 the XOR folds away and is not emitted.
//
// Options are held BY VALUE, never boxed: selecting one allocates nothing, a
// decode into a union allocates exactly what a struct member of the same types
// would, and the memory is the struct's own. An option that is not held keeps
// whatever its slot last held; nothing reads it (every read tests the tag first),
// and selecting it again resets it first.
//
// Everything emitted here differs per schema -- the slots and their types, the
// ids, one accessor set and one encode/decode/JSON arm per option. Selection is a
// tag store; no generic tagged-union helper exists, in the package or in
// corelib-go.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	f       *ir.Field
	getter  string // <Opt>, with a trailing underscore on a reserved name (reserved.go)
	setter  string // Set<Opt>
	has     string // Has<Opt>
	mut     string // Mut<Opt>; "" for an option that is not edited in place
	slot    string // the unexported slot, opt<Opt>
	idConst string // <Type><Opt>ID
	isD     bool   // the union's default option (default_id)
}

// unionShape is one union type with its options.
type unionShape struct {
	typeName string
	nt       *ir.NamedType
	opts     []*unionOpt
	byField  map[*ir.Field]*unionOpt
	d        *unionOpt
}

// hasMut reports whether an option of this kind gets a Mut<Opt>() accessor: the
// kinds that are edited in place, member by member or element by element.
func hasMut(k ir.Kind) bool {
	return k == ir.KindStruct || k == ir.KindUnion || k == ir.KindArray
}

func (g *gen) unionShapeOf(nt *ir.NamedType) *unionShape {
	u := &unionShape{typeName: g.typeName(nt.Key), nt: nt, byField: map[*ir.Field]*unionOpt{}}
	for _, f := range nt.Fields {
		base := exported(f.Name)
		o := &unionOpt{
			f:       f,
			getter:  base,
			setter:  "Set" + base,
			has:     "Has" + base,
			slot:    "opt" + base,
			idConst: u.typeName + base + "ID",
			isD:     nt.IsDefaultOption(f),
		}
		if goReserved(base) || unionReserved[base] {
			o.getter = base + "_"
		}
		if hasMut(f.Kind) {
			o.mut = "Mut" + base
		}
		u.opts = append(u.opts, o)
		u.byField[f] = o
		if o.isD {
			u.d = o
		}
	}
	return u
}

// tag is the value the unexported tag holds while o is held: o's id relative to
// default_id's, so that default_id is 0.
func (u *unionShape) tag(o *unionOpt) string {
	if o.isD {
		return "0"
	}
	if u.d.f.ID == 0 {
		return o.idConst
	}
	return o.idConst + "^" + u.d.idConst
}

// checkUnionNames rejects a union whose options derive the same Go method, or a
// method the union type already carries. Located: the error names the union and
// both options.
func checkUnionNames(u *unionShape) error {
	owner := map[string]string{}
	for _, set := range []map[string]bool{goVisitorMembers, goStringCheckMembers, goMembers, unionReserved} {
		for n := range set {
			owner[n] = ""
		}
	}
	for _, o := range u.opts {
		names := []string{o.getter, o.setter, o.has}
		if o.mut != "" {
			names = append(names, o.mut)
		}
		for _, n := range names {
			prev, taken := owner[n]
			switch {
			case taken && prev == "":
				return fmt.Errorf("go backend: union %s: option %q generates the method %s, which the union type already has; rename the option", u.nt.Key, o.f.Name, n)
			case taken && prev != o.f.Name:
				return fmt.Errorf("go backend: union %s: options %q and %q both generate the method %s; rename one", u.nt.Key, prev, o.f.Name, n)
			}
			owner[n] = o.f.Name
		}
	}
	return nil
}

// checkPackageNames rejects an option-id constant that collides with any other
// package-level identifier the backend emits. Such a constant is named after its
// type and option (<Type><Opt>ID), so it can land on a type name, an enum or
// bitfield constant, a message's generated constants and functions, or another
// union's constant -- none of which the Core name check can see.
func (g *gen) checkPackageNames() error {
	seen := map[string]string{}
	add := func(name, what string) {
		if _, ok := seen[name]; !ok {
			seen[name] = what
		}
	}
	for _, key := range g.schema.NamedOrder {
		nt := g.schema.Named[key]
		tn := g.typeName(key)
		add(tn, "the type "+tn)
		switch nt.Category {
		case ir.CatEnum:
			for _, c := range nt.Consts {
				add(tn+exported(c.Name), "a constant of the enum "+tn)
			}
		case ir.CatBitfield:
			for _, fl := range nt.Flags {
				add(tn+exported(fl.Name), "a constant of the bitfield "+tn)
			}
		}
	}
	for _, m := range g.schema.Messages {
		tn := exported(m.Name)
		add(tn, "the message type "+tn)
		for _, n := range []string{"New" + tn, "Decode" + tn, "Decode" + tn + "From", tn + "MaxSize", tn + "MaxSizeLimit", tn + "MaxDepth", "_" + tn + "EncOpts"} {
			add(n, "a declaration of the message "+tn)
		}
	}
	for _, n := range []string{"MaxDynArrayCount", "MaxDynStringLen", "MaxDynBlobLen", "_caps", "_isDefaulter"} {
		add(n, "a package declaration")
	}
	var keys []string
	for _, key := range g.schema.NamedOrder {
		if g.schema.Named[key].Category == ir.CatUnion {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		u := g.unionShapeOf(g.schema.Named[key])
		if err := checkUnionNames(u); err != nil {
			return err
		}
		for _, o := range u.opts {
			if what, ok := seen[o.idConst]; ok {
				return fmt.Errorf("go backend: union %s: option %q's id constant %s collides with %s; rename one", key, o.f.Name, o.idConst, what)
			}
			seen[o.idConst] = fmt.Sprintf("the id constant of option %q of union %s", o.f.Name, key)
		}
	}
	return nil
}

// optDefaultExpr is the value a read of a leaf option that is not held returns:
// its declared default, or the zero value. A string/blob/array option declares
// no non-empty default (MESSAGE_SPEC §4.2), so for those it is the empty value.
func (g *gen) optDefaultExpr(fld *ir.Field) string {
	switch fld.Kind {
	case ir.KindBlob, ir.KindArray:
		return "nil"
	}
	return g.defaultCompare(fld)
}

// emitOptReset puts the slot of o at the option's own default, in place: what
// selecting it anew and Mut<Opt>() on an option not held do.
func (g *gen) emitOptReset(f *gofile, ind string, o *unionOpt) {
	switch o.f.Kind {
	case ir.KindStruct, ir.KindUnion:
		t := g.typeName(o.f.Ref.Key)
		f.line("%sm.%s = %s{}", ind, o.slot, t)
		if g.needsDefaults(o.f.Ref.Key) {
			f.line("%sm.%s.setDefaults()", ind, o.slot)
		}
	default: // array
		f.line("%sm.%s = nil", ind, o.slot)
	}
}

// emitUnion emits a union type: the struct, its option-id constants and
// accessors, Serialize/isDefault, the decode visitor and the JSON methods.
func (g *gen) emitUnion(f *gofile, nt *ir.NamedType) {
	u := g.unionShapeOf(nt)
	tn := u.typeName
	f.imp(corelibImport)

	dname := u.d.f.Name
	f.line("// %s is a generated SofaBuffers union: it holds exactly one of its options.", tn)
	f.line("// A fresh one -- the zero value, after setDefaults where %q declares a", dname)
	f.line("// default -- holds %q at that option's default. Set<Option> and Mut<Option>", dname)
	f.line("// select an option; the one held before is discarded.")
	f.line("type %s struct {", tn)
	f.line("\tsofab.VisitorBase")
	if hasStringField(nt.Fields) {
		f.line("\tsofab.StringCheck")
	}
	if u.d.f.ID == 0 {
		f.line("\t// which is the id of the option held.")
	} else {
		f.line("\t// which is the id of the option held, XOR %s, so the zero value", u.d.idConst)
		f.line("\t// holds %q.", dname)
	}
	f.line("\twhich sofab.ID")
	for _, fld := range ir.SortedForLayout(nt.Fields) {
		o := u.byField[fld]
		f.line("\t%s %s%s", o.slot, g.goType(fld), fieldDoc(fld))
	}
	if hasFixlenField(nt.Fields) {
		f.line("\t// _acc assembles a string or blob payload the codec delivers in pieces")
		f.line("\t// (S6.6.3).")
		f.line("\t_acc sofab.PayloadAcc")
	}
	f.line("}")
	f.blank()

	f.line("// The option ids of %s, as Which reports them.", tn)
	f.line("const (")
	for _, o := range u.opts {
		f.line("\t%s sofab.ID = %d", o.idConst, o.f.ID)
	}
	f.line(")")
	f.blank()

	f.line("// Which returns the id of the option held.")
	if u.d.f.ID == 0 {
		f.line("func (m *%s) Which() sofab.ID { return m.which }", tn)
	} else {
		f.line("func (m *%s) Which() sofab.ID { return m.which ^ %s }", tn, u.d.idConst)
	}
	f.blank()

	for _, o := range u.opts {
		g.emitUnionAccessors(f, u, o)
	}

	f.line("// Clear puts the union back to its default: %q at its own default.", dname)
	f.line("func (m *%s) Clear() {", tn)
	f.line("\t*m = %s{}", tn)
	if g.needsDefaults(nt.Key) {
		f.line("\tm.setDefaults()")
	}
	f.line("}")
	f.blank()

	if g.needsDefaults(nt.Key) {
		f.line("// setDefaults seeds the default of %q, the option the zero value holds.", dname)
		f.line("func (m *%s) setDefaults() {", tn)
		if lit, ok := g.defaultLiteral(u.d.f); ok {
			f.line("\tm.%s = %s", u.d.slot, lit)
		} else {
			f.line("\tm.%s.setDefaults()", u.d.slot)
		}
		f.line("}")
		f.blank()
	}

	// Serialize: MESSAGE_SPEC §4.2. The default option is written like a field of
	// its kind -- omitted at its own default, which is exactly when the union is;
	// any other option held is written whatever its value.
	f.line("func (m *%s) Serialize(e *sofab.Encoder) {", tn)
	f.line("\tswitch m.Which() {")
	for _, o := range u.opts {
		f.line("\tcase %s:", o.idConst)
		g.emitMarshalFieldAt(f, o.f, "m."+o.slot, "\t\t", !o.isD)
	}
	f.line("\t}")
	f.line("}")
	f.blank()

	f.line("// isDefault: the union is at its default only while it holds %q at that", dname)
	f.line("// option's own default. Any other option held is written, so it is not.")
	f.line("func (m *%s) isDefault() bool {", tn)
	f.line("\treturn m.which == 0 && %s", g.fieldIsDefaultExprAt(f, u.d.f, "m."+u.d.slot))
	f.line("}")
	f.blank()

	g.emitVisitorMethods(f, tn, nt.Fields, u)
	g.emitUnionJSON(f, u)
}

// emitUnionAccessors emits Has/getter/Set (and Mut) for one option.
func (g *gen) emitUnionAccessors(f *gofile, u *unionShape, o *unionOpt) {
	tn, typ, tag := u.typeName, g.goType(o.f), u.tag(o)
	name := o.f.Name
	f.line("// %s reports whether %q is the option held.", o.has, name)
	f.line("func (m *%s) %s() bool { return m.which == %s }", tn, o.has, tag)
	f.blank()

	switch o.f.Kind {
	case ir.KindStruct, ir.KindUnion:
		f.line("// %s returns a copy of %q when it is held, else a fresh one at its default.", o.getter, name)
		f.line("func (m *%s) %s() %s {", tn, o.getter, typ)
		f.line("\tif m.which != %s {", tag)
		if g.needsDefaults(o.f.Ref.Key) {
			f.line("\t\tvar d %s", typ)
			f.line("\t\td.setDefaults()")
			f.line("\t\treturn d")
		} else {
			f.line("\t\treturn %s{}", typ)
		}
	default:
		f.line("// %s returns %q when it is held, else its default.", o.getter, name)
		f.line("func (m *%s) %s() %s {", tn, o.getter, typ)
		f.line("\tif m.which != %s {", tag)
		f.line("\t\treturn %s", g.optDefaultExpr(o.f))
	}
	f.line("\t}")
	f.line("\treturn m.%s", o.slot)
	f.line("}")
	f.blank()

	f.line("// %s selects %q with the value v; the option held before is discarded.", o.setter, name)
	f.line("func (m *%s) %s(v %s) {", tn, o.setter, typ)
	f.line("\tm.which = %s", tag)
	f.line("\tm.%s = v", o.slot)
	f.line("}")
	f.blank()

	if o.mut == "" {
		return
	}
	f.line("// %s selects %q at its default unless it is already held -- then it is", o.mut, name)
	f.line("// left as it is -- and returns a pointer to it for editing in place.")
	f.line("func (m *%s) %s() *%s {", tn, o.mut, typ)
	f.line("\tif m.which != %s {", tag)
	f.line("\t\tm.which = %s", tag)
	g.emitOptReset(f, "\t\t", o)
	f.line("\t}")
	f.line("\treturn &m.%s", o.slot)
	f.line("}")
	f.blank()
}

// emitUnionJSON emits the union's JSON form: an object with exactly one member,
// the held option. The slots are unexported, so encoding/json cannot see them;
// these two methods are the only JSON code the Go backend emits.
func (g *gen) emitUnionJSON(f *gofile, u *unionShape) {
	f.imp("encoding/json")
	f.imp("fmt")
	tn := u.typeName
	f.line("// MarshalJSON renders the union as {\"<option>\": value}, the held option only.")
	f.line("func (m %s) MarshalJSON() ([]byte, error) {", tn)
	f.line("\tswitch m.Which() {")
	for _, o := range u.opts {
		f.line("\tcase %s:", o.idConst)
		f.line("\t\treturn json.Marshal(struct {")
		f.line("\t\t\tV %s `json:%q`", g.goType(o.f), o.f.Name)
		f.line("\t\t}{m.%s})", o.slot)
	}
	f.line("\t}")
	f.line("\treturn nil, fmt.Errorf(\"%s holds no option (tag %%d)\", m.which)", tn)
	f.line("}")
	f.blank()

	f.line("// UnmarshalJSON reads {\"<option>\": value}: exactly one member, naming an")
	f.line("// option, which is selected from its default and then set from value.")
	f.line("func (m *%s) UnmarshalJSON(b []byte) error {", tn)
	f.line("\tvar one map[string]json.RawMessage")
	f.line("\tif err := json.Unmarshal(b, &one); err != nil {")
	f.line("\t\treturn err")
	f.line("\t}")
	f.line("\tif len(one) != 1 {")
	f.line("\t\treturn fmt.Errorf(\"%s holds exactly one option, got %%d members\", len(one))", tn)
	f.line("\t}")
	f.line("\tm.Clear()")
	f.line("\tfor k, v := range one {")
	f.line("\t\tswitch k {")
	// Sorted by option name so the arms read like a lookup table.
	opts := append([]*unionOpt(nil), u.opts...)
	sort.SliceStable(opts, func(i, j int) bool { return opts[i].f.Name < opts[j].f.Name })
	for _, o := range opts {
		f.line("\t\tcase %q:", o.f.Name)
		if o.mut != "" {
			f.line("\t\t\treturn json.Unmarshal(v, m.%s())", o.mut)
			continue
		}
		f.line("\t\t\tvar x %s", g.goType(o.f))
		f.line("\t\t\tif err := json.Unmarshal(v, &x); err != nil {")
		f.line("\t\t\t\treturn err")
		f.line("\t\t\t}")
		f.line("\t\t\tm.%s(x)", o.setter)
		f.line("\t\t\treturn nil")
	}
	f.line("\t\t}")
	f.line("\t\treturn fmt.Errorf(\"%s has no option %%q\", k)", tn)
	f.line("\t}")
	f.line("\treturn nil")
	f.line("}")
	f.blank()
}
