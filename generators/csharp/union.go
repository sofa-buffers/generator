package csharp

import (
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// A schema union is a C# class that holds exactly ONE option (MESSAGE_SPEC
// §4.2): a private tag `_which` plus one private, typed slot per option. Its
// API:
//
//	Id_<Opt>          the option's id, a public const int
//	Which             the id of the option held (read-only property)
//	<Opt>             property: <opt> when held, else its default (the getter
//	                  stores nothing); assigning it selects <opt> with that value
//	Has_<Opt>         whether <opt> is held (read-only property)
//	Mutable_<Opt>()   (struct, union and List-backed array options) select <opt>
//	                  at its default unless it is held, and return the slot
//	Clear()           back to the default: default_id at its own default
//	Serialize(os)     one switch arm per option
//
// The names are distinct by construction (ARCHITECTURE §8, "Naming"): <Opt> is
// naming.Pascal of the option, which has no `_` and differs per option (the
// fold rule), and takes a trailing `_` only off a reserved or fixed member. A
// derived member is a fixed word, `_`, and the UNESCAPED <Opt>, so it holds
// exactly one inner `_` and splits back into word and option: an option `a`'s
// Id_A and an option `a_id` (AId), or `x`'s Has_X and an option `has_x`
// (HasX), can no longer meet. The slots start with `_` and use the schema name.
//
// Each slot has the option's exact member type -- `ushort`, `float`, `string`,
// `byte[]`, the primitive array, a `List<T>`, the generated struct -- so no
// access boxes or casts. Only default_id's slot is initialised: a fresh union
// allocates nothing for the options it does not hold.
//
// C# has no destination-reuse path (every decode surface constructs its
// message), and a C# struct class has no in-place reset, so a real switch to a
// reference option (struct, union, list) puts a FRESH default instance in its
// slot, exactly as the first selection does; an option that is already held is
// never touched. A reference obtained earlier is therefore never reset behind the
// caller's back -- it is merely no longer held.
//
// Everything emitted here differs per schema -- the slots and their types, the
// ids, one accessor set and one encode arm per option. Selection is a tag store;
// no generic tagged-union helper exists, in the namespace or in corelib-cs.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	// f is the option as a field, with the empty default of a string, blob or
	// array option dropped (MESSAGE_SPEC §4.2 allows no other), so every
	// field-shaped helper treats it as "no declared default" -- no hoisted
	// compare static, no literal.
	f       *ir.Field
	orig    *ir.Field // the option as declared, for its documentation
	prop    string    // <Opt>: the public property, mangled off the union's own members
	has     string    // Has_<Opt>
	mutable string    // Mutable_<Opt>
	slot    string    // the private slot
	idConst string    // Id_<Opt>
	isD     bool      // the union's default option (default_id)
}

// unionShape is one union type with its options.
type unionShape struct {
	typeName string
	nt       *ir.NamedType
	opts     []*unionOpt
	d        *unionOpt
}

// unionOptProp is the property an option is reached through: the option name in
// PascalCase, mangled off the union's fixed members. A property named like the
// union type itself is the type's to avoid (typeIdent, CS0542).
func unionOptProp(fld *ir.Field) string {
	p := naming.Pascal(fld.Name)
	if unionFixed[p] || csReserved(p) {
		p += "_"
	}
	return p
}

// unionDerived is a member derived from an option: a fixed word, `_`, and the
// option's unescaped PascalCase name (Has_Pt, Mutable_Pt, Id_Pt).
func unionDerived(word string, fld *ir.Field) string {
	return word + "_" + naming.Pascal(fld.Name)
}

// unionSlot is the private field holding an option's value.
func unionSlot(fld *ir.Field) string {
	slot := "_" + fld.Name
	if slot == "_which" {
		slot += "_"
	}
	return slot
}

// unionOptMembers is every member of the union class an option declares.
func unionOptMembers(fld *ir.Field) []string {
	ms := []string{unionOptProp(fld), unionSlot(fld), unionDerived("Id", fld), unionDerived("Has", fld)}
	if unionMutable(fld) {
		ms = append(ms, unionDerived("Mutable", fld))
	}
	return ms
}

// unionMutable reports whether an option gets a Mutable<Opt>() accessor: the
// kinds edited in place, member by member or element by element. A primitive
// array option (`ushort[]`, `float[]`, ...) cannot grow in place: it is replaced
// whole through its property and its elements are edited through the array the
// getter returns.
func unionMutable(fld *ir.Field) bool {
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		return true
	case ir.KindArray:
		return !primArrayElem(fld.Elem)
	}
	return false
}

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
			prop:    unionOptProp(fld),
			has:     unionDerived("Has", fld),
			mutable: unionDerived("Mutable", fld),
			slot:    unionSlot(fld),
			idConst: unionDerived("Id", fld),
			isD:     nt.IsDefaultOption(fld),
		}
		u.opts = append(u.opts, o)
		if o.isD {
			u.d = o
		}
	}
	return u
}

// unionFresh is an option's default as a fresh value: what the getter answers
// while another option is held, what a real switch puts into a reference slot,
// and what Clear() restores. A mutable kind is a new instance, so nothing a
// caller does to it reaches the union; everything else is a constant (a literal,
// or the shared zero-length array).
func (g *gen) unionFresh(o *unionOpt) string {
	switch o.f.Kind {
	case ir.KindStruct, ir.KindUnion:
		return "new " + g.typeName(o.f.Ref.Key) + "()"
	case ir.KindArray:
		if primArrayElem(o.f.Elem) {
			return "global::System.Array.Empty<" + primArrayBase(o.f.Elem, o.f.ElemRef) + ">()"
		}
		return "new " + g.csType(o.f) + "()"
	}
	return g.csDefaultValue(o.f)
}

// emitUnionClass writes the class of one union type.
func (g *gen) emitUnionClass(f *cfile, key string, nt *ir.NamedType) {
	u := g.unionShapeOf(key, nt)
	emitDoc(f, "", nt.Summary)
	f.line("public sealed class %s {", u.typeName)
	for _, o := range u.opts {
		f.line("    public const int %s = %d;", o.idConst, o.f.ID)
	}
	f.blank()
	f.line("    private int _which = %s;", u.d.idConst)
	for _, o := range u.opts {
		init := ""
		if o.isD {
			// The field initializer of an ordinary member of that kind: a
			// constructed struct/union, a list presized to a small count.
			init = g.csInit(o.f)
		}
		f.line("    private %s %s%s;", g.csType(o.f), o.slot, init)
	}
	f.blank()
	f.line("    /// <summary>The id of the option this union holds: one of the <c>Id_*</c> constants.</summary>")
	f.line("    public int Which => _which;")
	for _, o := range u.opts {
		g.emitUnionAccessors(f, o)
	}
	f.blank()

	f.line("    public void Serialize(global::sofab.OStream os) {")
	f.line("        switch (_which) {")
	for _, o := range u.opts {
		f.line("        case %s: {", o.idConst)
		// default_id is written like an ordinary field of its kind (omitted at its
		// default); every other option is forced (MESSAGE_SPEC §4.2).
		g.emitMarshalAt(f, "            ", o.f, "this."+o.slot, !o.isD)
		f.line("            break;")
		f.line("        }")
	}
	f.line("        }")
	f.line("    }")

	// The union is default exactly when Serialize writes nothing: default_id held
	// and at its own default. Any other held option is always written.
	f.line("    /// <summary>True when the union holds its default option at that option's default -- i.e. Serialize would write nothing at all.</summary>")
	f.line("    public bool IsDefault() => _which == %s && (%s);", u.d.idConst, g.fieldIsDefaultExpr(u.d.f, "this."+u.d.slot))
	f.blank()
	f.line("    /// <summary>Back to the default: the default option at its own default.</summary>")
	f.line("    public void Clear() {")
	f.line("        _which = %s;", u.d.idConst)
	f.line("        %s = %s;", u.d.slot, g.unionFresh(u.d))
	f.line("    }")
	f.line("}")
	f.blank()
}

// emitUnionAccessors writes the property, Has_<Opt> and -- for a kind edited in
// place -- Mutable_<Opt>() of one option.
func (g *gen) emitUnionAccessors(f *cfile, o *unionOpt) {
	t := g.csType(o.f)
	dep := func() {
		if o.f.Deprecated {
			f.line("    [global::System.Obsolete]")
		}
	}
	f.blank()
	emitDoc(f, "    ", fieldDoc(o.orig, generator.BoundNote(o.orig, generator.StorageDynamic)))
	dep()
	// The getter reads the slot only while the option is held; otherwise it
	// answers the option's default -- a fresh object for a kind edited in place,
	// so nothing a caller does to it reaches the union -- and stores nothing. The
	// statement form lets a constant default convert to the property's type.
	f.line("    public %s %s {", t, o.prop)
	f.line("        get { if (_which == %s) return %s; return %s; }", o.idConst, o.slot, g.unionFresh(o))
	f.line("        set { _which = %s; %s = value; }", o.idConst, o.slot)
	f.line("    }")
	dep()
	f.line("    public bool %s => _which == %s;", o.has, o.idConst)
	if !unionMutable(o.f) {
		return
	}
	// Select if not held: an option that is already held is returned untouched,
	// so a decode that reaches it again -- a repeated or re-opened occurrence, a
	// resumed feed -- continues it (MESSAGE_SPEC §7.4) instead of wiping it.
	dep()
	f.line("    public %s %s() {", t, o.mutable)
	f.line("        if (_which != %s || %s == null) { %s = %s; _which = %s; }", o.idConst, o.slot, o.slot, g.unionFresh(o), o.idConst)
	f.line("        return %s;", o.slot)
	f.line("    }")
}

// memberRef is the member a decode store names for field fld of the object at
// path: the plain field, or -- when that object is a union, utype naming its C#
// type ("" otherwise) -- the option's property, whose setter is the §7.4.1 switch
// and the store in one assignment, and whose getter reads the slot an earlier
// selection filled.
func memberRef(path string, fld *ir.Field, utype string) string {
	if utype != "" {
		return path + "." + unionOptProp(fld)
	}
	return path + "." + csIdent(fld.Name)
}

// memberPath is the expression a decode path takes to reach member fld of the
// object at path: the member itself, or -- when that object is a union and the
// option is edited in place -- the option's mutable accessor, which selects it
// at its default unless it is already held. Every path into a struct, union or
// List-backed array option goes through it, so a store below an option is
// correct even where no begin arm ran first.
func memberPath(path string, fld *ir.Field, utype string) string {
	if utype != "" && unionMutable(fld) {
		return path + "." + unionDerived("Mutable", fld) + "()"
	}
	return memberRef(path, fld, utype)
}

// emitUnionConverters emits the harness's System.Text.Json converter for every
// union type: exactly ONE member, the held option, `{"<option>": value}` --
// printed even when that is the default option at its default. Read selects each
// member it reads through the option's property, so the last member read wins,
// as the last option on the wire does. The converters live in the harness only;
// the library stays JSON-free.
func (g *gen) emitUnionConverters(f *cfile, s *ir.Schema) []string {
	var names []string
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category != ir.CatUnion {
			continue
		}
		u := g.unionShapeOf(key, nt)
		cn := "_" + g.bases[key] + "__JsonConverter"
		names = append(names, cn)
		dep := hasDeprecatedDirect(nt.Fields)
		if dep {
			f.line("#pragma warning disable 612 // the harness reads a union option marked [Obsolete] (CS0612)")
		}
		f.line("sealed class %s : JsonConverter<%s> {", cn, u.typeName)
		f.line("    public override %s Read(ref Utf8JsonReader r, Type t, JsonSerializerOptions o) {", u.typeName)
		f.line("        if (r.TokenType != JsonTokenType.StartObject) throw new JsonException(\"%s: a union is a JSON object\");", u.typeName)
		f.line("        var u = new %s();", u.typeName)
		f.line("        while (r.Read() && r.TokenType != JsonTokenType.EndObject) {")
		f.line("            var k = r.GetString(); r.Read();")
		f.line("            switch (k) {")
		for _, o := range u.opts {
			f.line("            case %q: u.%s = JsonSerializer.Deserialize<%s>(ref r, o); break;", o.f.Name, o.prop, g.csType(o.f))
		}
		f.line("            default: r.Skip(); break;")
		f.line("            }")
		f.line("        }")
		f.line("        return u;")
		f.line("    }")
		f.line("    public override void Write(Utf8JsonWriter w, %s v, JsonSerializerOptions o) {", u.typeName)
		f.line("        w.WriteStartObject();")
		f.line("        switch (v.Which) {")
		for _, o := range u.opts {
			f.line("        case %s.%s: w.WritePropertyName(%q); JsonSerializer.Serialize(w, v.%s, o); break;", u.typeName, o.idConst, o.f.Name, o.prop)
		}
		f.line("        }")
		f.line("        w.WriteEndObject();")
		f.line("    }")
		f.line("}")
		if dep {
			f.line("#pragma warning restore 612")
		}
		f.blank()
	}
	return names
}
