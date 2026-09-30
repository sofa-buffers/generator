package cpp

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// A schema union lowers to a tagged union (MESSAGE_SPEC §4.2: a union holds
// exactly one option), still a sofab::Message so it nests, encodes and decodes
// like a struct:
//
//	corelib: cpp    std::variant<one alternative per option, in id order>,
//	                accessed by INDEX only, so two options of one C++ type are fine.
//	corelib: c-cpp  a tag (Which _which) and a union of the options whose own
//	                constructor places the default option, so the class keeps a
//	                non-user-provided default constructor; copy operations that
//	                switch on the tag where an option is not trivially copyable.
//	                No <variant>: the profile is freestanding.
//
// The public API is the same on both:
//
//	which() / Which::<opt>   the held option
//	has_<opt>()              is <opt> held
//	<opt>()                  the held value, or the option's default when not held
//	set_<opt>(v)             select <opt> holding v
//	mutable_<opt>()          select <opt> if not held (at its default), return it
//	reset()                  back to default_id at its default
//
// What is generated per schema is only what differs per schema: the option ids,
// the per-option storage and accessors, one encode arm and one decode arm per
// option. The selection itself is the library's (std::variant::emplace) or one
// placement-new per option; no generic tagged-union helper is emitted.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	f     *ir.Field
	idx   int    // position in id order: the variant alternative index
	base  string // the getter, and the Which enumerator
	typ   string // C++ type of the option's storage
	store string // expression naming the held option's storage (valid only when held)
	// c-cpp only, the same for every option of one union: clear is set when an
	// option can own heap storage (allow_dynamic), so a switch and the
	// destructor must end the held option's lifetime (_clear()); copy is set
	// when an option is not trivially copyable, so the copy operations must
	// switch on the tag (_copy()). Neither is emitted where it is not needed --
	// an all-scalar union, or static storage without a struct/union option,
	// keeps the implicit, trivial special members.
	clear, copy bool
}

// optBase is the accessor base name of a union option.
func optBase(name string) string {
	b := cppIdent(name)
	if unionReserved[b] {
		b += "_"
	}
	return b
}

// unionOptions lists nt's options in id order with their derived names.
func (g *gen) unionOptions(nt *ir.NamedType) []*unionOpt {
	fields := append([]*ir.Field(nil), nt.Fields...)
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].ID < fields[j].ID })
	opts := make([]*unionOpt, len(fields))
	for i, fld := range fields {
		o := &unionOpt{f: fld, idx: i, base: optBase(fld.Name), typ: g.cppType(fld)}
		if g.clib {
			o.store = "_u." + o.base
		} else {
			o.store = fmt.Sprintf("(*std::get_if<%d>(&_opts))", i)
		}
		opts[i] = o
	}
	if g.clib {
		var clear, copy bool
		for _, o := range opts {
			clear = clear || (!g.fixed && !isScalarOpt(o.f.Kind))
			copy = copy || !g.triviallyCopyable(o.f.Kind, o.f.Elem, o.f.ElemItems)
		}
		for _, o := range opts {
			o.clear, o.copy = clear, copy
		}
	}
	return opts
}

// triviallyCopyable reports whether an option of this kind is stored in a
// trivially copyable type on the c-cpp leg: a scalar always; under static
// storage also sofab::FixedString / FixedBytes and an InlineVector of trivially
// copyable elements. A struct or union option is a polymorphic sofab::Message
// and never is; neither is any std::string / std::vector (allow_dynamic).
func (g *gen) triviallyCopyable(k ir.Kind, elem ir.Kind, items *ir.ArrayElem) bool {
	switch k {
	case ir.KindStruct, ir.KindUnion:
		return false
	case ir.KindString, ir.KindBlob:
		return g.fixed
	case ir.KindArray:
		if !g.fixed {
			return false
		}
		var ei *ir.ArrayElem
		var ee ir.Kind
		if items != nil {
			ee, ei = items.Elem, items.ElemItems
		}
		return g.triviallyCopyable(elem, ee, ei)
	}
	return true
}

// checkUnionNames refuses a union whose options derive the same accessor: the
// getter of one and the set_/has_/mutable_ accessor of another (`foo` and
// `set_foo`), which C++ would silently take as an overload set.
func (g *gen) checkUnionNames(s *ir.Schema) error {
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category != ir.CatUnion {
			continue
		}
		owner := map[string]string{}
		for r := range unionReserved {
			owner[r] = ""
		}
		for _, o := range g.unionOptions(nt) {
			for _, n := range []string{o.base, "set_" + o.base, "has_" + o.base, "mutable_" + o.base} {
				if prev, ok := owner[n]; ok && prev != "" {
					return fmt.Errorf("cpp: union %q: options %q and %q both generate the accessor %q; rename one", key, prev, o.f.Name, n)
				}
				owner[n] = o.f.Name
			}
		}
	}
	return nil
}

// optDefaultArgs renders the constructor arguments that put option o at its
// declared default ("" for a value-initialised one).
func (g *gen) optDefaultArgs(o *unionOpt) string {
	if o.f.Kind == ir.KindStruct || o.f.Kind == ir.KindUnion {
		return ""
	}
	d := g.cppDefault(o.f)
	if d == "{}" || d == `""` {
		return ""
	}
	return d
}

// isScalarOpt reports whether an option is held by value (returned by value by
// its getter) rather than as a class object.
func isScalarOpt(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindArray, ir.KindStruct, ir.KindUnion:
		return false
	}
	return true
}

// unionHas reports whether any type this header emits is a union.
func (g *gen) unionHas(m *ir.Message) bool {
	for _, key := range g.reachable(m) {
		if g.schema.Named[key].Category == ir.CatUnion {
			return true
		}
	}
	return false
}

func (g *gen) emitUnion(f *hfile, name string, nt *ir.NamedType) {
	opts := g.unionOptions(nt)
	d := nt.DefaultOption()
	var dopt *unionOpt
	for _, o := range opts {
		if o.f == d {
			dopt = o
		}
	}
	emitStructDoc(f, nt.Summary)
	hasDeprecated := false
	for _, o := range opts {
		hasDeprecated = hasDeprecated || o.f.Deprecated
	}
	if hasDeprecated {
		f.line("#pragma GCC diagnostic push")
		f.line("#pragma GCC diagnostic ignored \"-Wdeprecated-declarations\"")
	}
	f.line("struct %s : sofab::Message {", name)
	f.line("    /// The option ids: which() names the held option. A fresh value holds `%s`.", d.Name)
	f.line("    enum class Which : sofab::id {")
	for _, o := range opts {
		if doc := fieldDoc(o.f); doc != "" {
			f.line("        %s = %d,  ///< %s", o.base, o.f.ID, doc)
		} else {
			f.line("        %s = %d,", o.base, o.f.ID)
		}
	}
	f.line("    };")
	f.blank()
	if g.clib {
		g.emitUnionSpecials(f, name, opts, dopt)
	}

	f.line("    /** @brief The option this value holds. */")
	if g.clib {
		f.line("    Which which() const noexcept { return _which; }")
	} else {
		ids := make([]string, len(opts))
		for i, o := range opts {
			ids[i] = fmt.Sprintf("%d", o.f.ID)
		}
		f.line("    Which which() const noexcept { return static_cast<Which>(_ids[_opts.index()]); }")
		f.line("    static constexpr sofab::id _ids[] = {%s};", strings.Join(ids, ", "))
	}
	f.blank()
	for _, o := range opts {
		g.emitUnionAccessors(f, o)
	}

	// reset(): back to default_id at its default, in place when it is held.
	f.line("    /** @brief Back to the default: `%s` at its declared default, in place when held. */", d.Name)
	if d.Kind == ir.KindStruct || d.Kind == ir.KindUnion {
		f.line("    void reset() noexcept { mutable_%s().reset(); }", dopt.base)
	} else {
		f.line("    void reset() noexcept { mutable_%s() = %s; }", dopt.base, g.cppDefault(d))
	}
	f.blank()

	// _isDefault(): the negation of serialize writing anything (§0: a union is
	// default iff it holds default_id and that option equals its own default).
	f.line("    /** @brief True when this union holds `%s` at its declared default: serialize writes nothing. */", d.Name)
	f.line("    bool _isDefault() const noexcept { return %s && %s; }", g.unionHeld(dopt), g.fieldIsDefaultExprAt(d, dopt.store))
	f.blank()

	// serialize: one arm per option. default_id is written like an ordinary
	// field of its kind (omitted at its default); every other option is written
	// whatever its value, closing a sequence-framed one with the keeping end.
	f.line("    /**")
	f.line("     * @brief Write the held option. A held option other than `%s` is written", d.Name)
	f.line("     *        even at its own default; `%s` at its default writes nothing.", d.Name)
	f.line("     */")
	f.line("    sofab::OStreamImpl::Result serialize(sofab::OStreamImpl &os) const noexcept override {")
	if g.clib {
		f.line("        switch (_which) {")
	} else {
		f.line("        switch (_opts.index()) {")
	}
	for _, o := range opts {
		f.line("        case %s", g.unionCase(o))
		g.emitSerializeAt(f, o.f, o.store, "            ", o != dopt)
		f.line("            break;")
	}
	if !g.clib {
		f.line("        default: break;")
	}
	f.line("        }")
	f.line("        return os.writeIf(0, false, false);")
	f.line("    }")
	f.blank()

	g.emitUnionDeserialize(f, opts)

	f.line("private:")
	if g.clib {
		// The tag leads, as in the C target's layout. default_id at its default
		// is the union's initial member through its default member initializer,
		// and the union's default constructor stays defaulted: see
		// emitUnionSpecials for why neither the class nor the union provides
		// one. Both choices are measured on cpp-c-cpp-dyn (ARCHITECTURE §11).
		f.line("    Which _which = Which::%s;", dopt.base)
		f.line("    union _Opts {")
		f.line("        _Opts() noexcept = default;")
		if dopt.copy {
			// The copy constructor's start: no option alive. A union
			// constructor that names no variant member would initialize
			// default_id from its default member initializer
			// ([class.base.init]/9), and _copy would then place the held
			// option over a live one. Naming _none, whose constructor does
			// nothing, starts no option and costs no code.
			f.line("        struct _None { _None() noexcept {} };")
			f.line("        explicit _Opts(std::nullptr_t) noexcept : _none() {}")
		}
		if dopt.clear {
			f.line("        ~_Opts() noexcept {}")
		}
		for _, o := range opts {
			if o == dopt {
				f.line("        %s %s{%s};", o.typ, o.base, g.optDefaultArgs(o))
				continue
			}
			f.line("        %s %s;", o.typ, o.base)
		}
		if dopt.copy {
			f.line("        _None _none;")
		}
		f.line("    } _u;")
	} else {
		types := make([]string, len(opts))
		for i, o := range opts {
			types[i] = o.typ
		}
		init := fmt.Sprintf("std::in_place_index<%d>", dopt.idx)
		if a := g.optDefaultArgs(dopt); a != "" {
			init += ", " + a
		}
		f.line("    std::variant<%s> _opts{%s};", strings.Join(types, ", "), init)
	}
	f.line("};")
	if hasDeprecated {
		f.line("#pragma GCC diagnostic pop")
	}
	f.blank()
}

// unionHeld is the "option o is held" test.
func (g *gen) unionHeld(o *unionOpt) string {
	if g.clib {
		return "_which == Which::" + o.base
	}
	return fmt.Sprintf("_opts.index() == %d", o.idx)
}

// unionCase is the serialize switch label of option o: the tag on c-cpp, the
// variant index (commented with the option's name) on corelib-cpp.
func (g *gen) unionCase(o *unionOpt) string {
	if g.clib {
		return "Which::" + o.base + ":"
	}
	return fmt.Sprintf("%d:  // %s", o.idx, o.f.Name)
}

// emitUnionSelect writes the body of mutable_<opt>(): select the option at its
// declared default unless it is already held. It never touches a held option,
// which is what makes it safe in a decode arm that runs once per chunk of a
// split payload and again when a resumed decode re-enters an open sequence.
func (g *gen) emitUnionSelect(f *hfile, o *unionOpt) {
	args := g.optDefaultArgs(o)
	if g.clib {
		f.line("        if (_which != Which::%s) {", o.base)
		if o.clear {
			f.line("            _clear();")
		}
		f.line("            ::new (&_u.%s) %s(%s);", o.base, o.typ, args)
		f.line("            _which = Which::%s;", o.base)
		f.line("        }")
		f.line("        return _u.%s;", o.base)
		return
	}
	if args != "" {
		args = ", " + args
	}
	f.line("        if (_opts.index() != %d) { _opts.emplace<%d>(%s); }", o.idx, o.idx, strings.TrimPrefix(args, ", "))
	f.line("        return %s;", o.store)
}

// emitUnionAccessors writes has_/get/set_/mutable_ for one option.
func (g *gen) emitUnionAccessors(f *hfile, o *unionOpt) {
	attr := ""
	if o.f.Deprecated {
		attr = "[[deprecated]] "
	}
	doc := fieldDoc(o.f)
	if doc != "" {
		doc = ": " + doc
	}
	f.line("    /// @name Option `%s` (id %d)%s", o.f.Name, o.f.ID, doc)
	if note := generator.BoundNote(o.f, cppStorage(o.typ)); note != "" {
		f.line("    /// %s", note)
	}
	f.line("    ///@{")
	f.line("    %sbool has_%s() const noexcept { return %s; }", attr, o.base, g.unionHeld(o))
	def := g.cppDefault(o.f)
	if isScalarOpt(o.f.Kind) {
		if o.f.Kind == ir.KindBitfield {
			def = fmt.Sprintf("static_cast<%s>(%s)", o.typ, def)
		}
		f.line("    %s%s %s() const noexcept { return has_%s() ? %s : %s; }", attr, o.typ, o.base, o.base, o.store, def)
		f.line("    %svoid set_%s(%s v) noexcept { mutable_%s() = v; }", attr, o.base, o.typ, o.base)
	} else {
		f.line("    %sconst %s &%s() const noexcept {", attr, o.typ, o.base)
		f.line("        if (has_%s()) { return %s; }", o.base, o.store)
		f.line("        static const %s _d{};", o.typ)
		f.line("        return _d;")
		f.line("    }")
		f.line("    %svoid set_%s(const %s &v) { mutable_%s() = v; }", attr, o.base, o.typ, o.base)
	}
	f.line("    %s%s &mutable_%s() noexcept {", attr, o.typ, o.base)
	g.emitUnionSelect(f, o)
	f.line("    }")
	f.line("    ///@}")
	f.blank()
}

// emitUnionSpecials writes the c-cpp tagged union's special members where the
// implicit ones do not do: the copy operations switching on the tag, and --
// where an option can own heap storage (allow_dynamic) -- the destructor and
// _clear() that end the held option's lifetime.
//
// The default constructor is never user-provided. default_id is placed by its
// default member initializer inside the nested union, whose own default
// constructor is defaulted (emitUnion), and the tag by its own, so the class
// keeps the implicit -- or, beside a user-declared copy constructor, the
// defaulted -- one. That is what keeps `T{}` a
// zero-initialization, exactly as for a struct: the corelib's containers reset
// an element with `buf_[i] = T{}`, and after a user-provided constructor that
// temporary's IStreamMessage decoder state would be indeterminate when the move
// copies it (GCC: -Wmaybe-uninitialized). A plain default-initialization -- an
// InlineVector's `T buf_[N]` -- still pays no zero-fill.
func (g *gen) emitUnionSpecials(f *hfile, name string, opts []*unionOpt, dopt *unionOpt) {
	if dopt.copy {
		f.line("    %s() noexcept = default;", name)
		// noexcept in both storage modes, like every other generated member: an
		// allocation failing inside a copy terminates rather than leaving the
		// tag naming an option whose lifetime already ended. _u(nullptr)
		// starts the union's do-nothing _none member, so no option is alive
		// (default_id's default member initializer does not run); _copy then
		// places the one o holds.
		f.line("    %s(const %s &o) noexcept : sofab::Message(o), _which(o._which), _u(nullptr) { _copy(o); }", name, name)
		f.line("    %s &operator=(const %s &o) noexcept {", name, name)
		f.line("        if (this != &o) {")
		f.line("            sofab::Message::operator=(o);")
		if dopt.clear {
			f.line("            _clear();")
		}
		f.line("            _which = o._which;")
		f.line("            _copy(o);")
		f.line("        }")
		f.line("        return *this;")
		f.line("    }")
	}
	if dopt.clear {
		f.line("    ~%s() { _clear(); }", name)
	}
	if !dopt.copy && !dopt.clear {
		return // every special member stays implicit
	}
	f.blank()
	f.line("private:")
	if dopt.copy {
		f.line("    void _copy(const %s &o) noexcept {", name)
		f.line("        switch (_which) {")
		for _, o := range opts {
			f.line("        case Which::%s: ::new (&_u.%s) %s(o._u.%s); break;", o.base, o.base, o.typ, o.base)
		}
		f.line("        }")
		f.line("    }")
	}
	if dopt.clear {
		f.line("    void _clear() noexcept {")
		f.line("        switch (_which) {")
		for _, o := range opts {
			if isScalarOpt(o.f.Kind) {
				continue
			}
			f.line("        case Which::%s: std::destroy_at(&_u.%s); break;", o.base, o.base)
		}
		f.line("        default: break;")
		f.line("        }")
		f.line("    }")
	}
	f.blank()
	f.line("public:")
}

// unionGate is the §7.3 test a decode arm runs before it selects its option,
// spelled as the condition under which the arm stops (the field is skipped).
//
// corelib-cpp: cppWireGuard with its tag types, which it keeps in sofab::detail
// and returns from the documented IStreamImpl::wire()/fixType() -- for exactly
// this, code that branches on the delivered form. fixType() is valid at the
// callback for an ArrayFixlen field too (the corelib's own arrayTagMatches
// reads it there), so an fp32/fp64 array option is gated on its subtype as
// well.
//
// corelib-c-cpp: IStreamImpl::delivered(), the same test as ONE comparison --
// the wire type and the fixlen subtype share one byte of the stream state. The
// footprint profile pays for every compare-and-branch per option arm.
func (g *gen) unionGate(fld *ir.Field) string {
	if g.clib {
		sub := cppFixSubtype(fld.Kind)
		if fld.Kind == ir.KindArray && isNativeArrayElem(fld.Elem) {
			sub = cppFixSubtype(fld.Elem)
		}
		if sub != "" {
			return fmt.Sprintf("!is.delivered(%s, %s)", cppExpectedWire(fld), sub)
		}
		return fmt.Sprintf("!is.delivered(%s)", cppExpectedWire(fld))
	}
	c := cppWireGuard(fld)
	c = strings.ReplaceAll(c, "sofab::Wire::", "sofab::detail::Wire::")
	c = strings.ReplaceAll(c, "sofab::Fix::", "sofab::detail::Fix::")
	return c
}

// cppScalarBind is the C++ type a c-cpp scalar option is bound through with
// readMatch -- an enum's underlying type, else the option's own -- or "" for a
// non-scalar option. Two options with the same bind type are decoded by the
// same bind: the same destination address, the same wire type.
func cppScalarBind(o *unionOpt) string {
	if !isScalarOpt(o.f.Kind) {
		return ""
	}
	if o.f.Kind == ir.KindEnum {
		return enumBacking(o.f.Ref.Target)
	}
	return o.typ
}

// selectingRead reports whether a c-cpp decode arm emitted with the selector
// sel as its destination names it exactly once, as the destination of a
// corelib-c-cpp read that has a selecting overload.
func selectingRead(arm, sel string) bool {
	if strings.Count(arm, sel) != 1 {
		return false
	}
	for _, fn := range []string{"is.readString(", "is.readBlob(", "is.readArray("} {
		if strings.Contains(arm, fn+sel+", ") {
			return true
		}
	}
	return selectingSeq.MatchString(arm) && strings.Contains(arm, ", "+sel+");")
}

// selectingSeq matches a wrapper-array read through a collector, whose
// destination is the second argument.
var selectingSeq = regexp.MustCompile(`is\.readSequence\(_r[0-9]+, \[this\]`)

// emitUnionDeserialize writes the decode arms. Each one selects its option
// (MESSAGE_SPEC §7.4.1) only once the field has passed the §7.3 test for the
// option's declared kind -- wire type, fixlen subtype and array kind -- so a
// mistyped or unknown child never switches, and then binds the option exactly
// as a struct member of that kind is bound.
//
// Where the read reports the match itself -- a pure-corelib scalar read into a
// temporary, a c-cpp scalar bound with readMatch -- the select sits inside that
// `if`; everywhere else the option must exist before it is bound, so the test
// is spelled in front of the select.
// Either way the select is "if not held": a held option is continued, never
// reset, so a split string/blob/array delivered once per chunk and a sequence
// re-entered on resume keep what already arrived.
func (g *gen) emitUnionDeserialize(f *hfile, opts []*unionOpt) {
	fields := make([]*ir.Field, len(opts))
	for i, o := range opts {
		fields[i] = o.f
	}
	sizeParam, countParam := g.deserializeParams(fields)
	f.line("    /**")
	f.line("     * @brief Bind one decoded child to its option, selecting that option.")
	f.line("     *")
	f.line("     * The last correctly-typed option wins: a child naming another option")
	f.line("     * replaces the held one (which starts from its default); a child naming")
	f.line("     * the held option continues it. A mistyped or unknown child is skipped")
	f.line("     * and switches nothing.")
	f.line("     */")
	f.line("    void deserialize(sofab::IStreamImpl &is, sofab::id id, %s, %s) noexcept override {", sizeParam, countParam)
	f.line("        switch (id) {")
	// c-cpp: scalar options bound through the same C++ type share one arm.
	// They sit at the same address in the storage union and readMatch binds
	// them identically, so only the tag differs -- and the Which enumerators
	// ARE the option ids, so the arm takes the tag from the id it was called
	// with. Every scalar arm this folds away is a bind, a branch and a store the
	// footprint profile no longer pays for.
	groups := map[string][]*unionOpt{}
	if g.clib {
		for _, o := range opts {
			if k := cppScalarBind(o); k != "" {
				groups[k] = append(groups[k], o)
			}
		}
	}
	done := map[*unionOpt]bool{}
	for _, o := range opts {
		if done[o] {
			continue
		}
		mut := "mutable_" + o.base + "()"
		fld := o.f
		group := []*unionOpt{o}
		if k := cppScalarBind(o); g.clib && k != "" {
			group = groups[k]
		}
		for _, m := range group {
			f.line("        case %d:", m.f.ID)
			done[m] = true
		}
		switch {
		case !g.clib && (ir.IsNarrow(fld.Kind) || fld.Kind == ir.KindEnum || fld.Kind == ir.KindBitfield):
			// Read into a 64-bit temporary; the store -- and with it the select --
			// runs only once read() matched the tag and the width check passed.
			g.emitDeserializeAt(f, fld, mut)
		case !g.clib && isScalarOpt(fld.Kind):
			f.line("            { %s _v{}; if (sofab::read(is, _v)) { %s = _v; } }", o.typ, mut)
		case isScalarOpt(fld.Kind):
			// c-cpp scalar: the deferred read overwrites the whole value, so the
			// select is the tag alone -- seeding the option's default first
			// (mutable_) would be a store the read always overwrites. readMatch
			// binds and reports the §7.3 test the stream applies to that bind, and
			// the value is written only after this callback returns, so the tag
			// is set behind the bind: the test runs once, out of line in the
			// corelib, instead of being spelled in front of every arm. Writing
			// the tag of an option already held changes nothing. Under
			// allow_dynamic the option left behind may own heap storage, so it is
			// ended first; its destructor runs before any byte of the new value
			// is written into the storage the two share.
			tag := "Which::" + o.base
			if len(group) > 1 {
				tag = "static_cast<Which>(id)"
			}
			sel := fmt.Sprintf("_which = %s;", tag)
			cond := ""
			if o.clear {
				sel = fmt.Sprintf("_clear(); _which = %s;", tag)
				cond = fmt.Sprintf(" && _which != %s", tag)
			}
			if fld.Kind == ir.KindEnum {
				// As in emitDeserializeAt: the enum's underlying-typed storage is
				// bound by address only, so -Wstrict-aliasing is a false positive.
				f.line("#pragma GCC diagnostic push")
				f.line(`#pragma GCC diagnostic ignored "-Wstrict-aliasing"`)
				f.line("            if (is.readMatch(reinterpret_cast<%s &>(%s))%s) { %s }", enumBacking(fld.Ref.Target), o.store, cond, sel)
				f.line("#pragma GCC diagnostic pop")
			} else {
				f.line("            if (is.readMatch(%s)%s) { %s }", o.store, cond, sel)
			}
		default:
			if g.clib {
				// c-cpp: where the arm's read is one of corelib-c-cpp's selecting
				// overloads -- readString/readBlob/readArray/readSequence given a
				// callable -- the read runs its own §7.3 test and selects the
				// option (calls mutable_<opt>()) only behind it. Spelling the test
				// in front instead makes it twice: the read repeats it, and the
				// select in between is a store the compiler cannot prove leaves the
				// stream's state alone. Any other shape (a struct/union option, a
				// read that names the destination more than once) keeps the gate.
				sel := fmt.Sprintf("[this]() -> auto & { return %s; }", mut)
				var arm hfile
				g.emitDeserializeAt(&arm, fld, sel)
				if out := arm.b.String(); selectingRead(out, sel) {
					f.b.WriteString(out)
					break
				}
			}
			f.line("            if (%s) break;", g.unionGate(fld))
			g.emitDeserializeAt(f, fld, mut)
		}
		f.line("            break;")
	}
	f.line("        default: break;")
	f.line("        }")
	f.line("    }")
	f.blank()
}
