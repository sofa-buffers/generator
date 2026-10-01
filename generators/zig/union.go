package zig

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// A schema union is a native Zig tagged union (`union(enum)`) with one field per
// option, so it holds exactly one option by construction (MESSAGE_SPEC §4.2).
// Its API:
//
//	fields                   read by `switch`; write by assigning `.{ .<opt> = v }`
//	<opt>_id                 the option's id, a `pub const`
//	init                     default_id at that option's own default (`= .init`)
//	which()                  the id of the option held
//	<opt>Mut()               select <opt> at its default unless it is held; return it
//	serialize(os)            one switch prong per option
//	isDefault()              default_id held and at its own default
//
// Everything emitted here differs per schema: the fields and their types, the
// ids, one accessor and one encode prong per option. Selection is the language's
// own (assigning a tagged-union value); no generic tagged-union helper exists.
//
// Reading an option that is not held is safety-checked illegal behaviour in
// Zig, so every path the decoder takes INTO an option (a struct member, an array
// element, a nested option) goes through <opt>Mut(); a whole-value store (a
// scalar, a completed string/blob) assigns the tagged union directly.

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	f       *ir.Field
	ident   string // the tagged-union field (zigIdent, union declarations mangled)
	mut     string // <opt>Mut(): select-if-not-held accessor
	idConst string // <opt>_id
	isD     bool   // the union's default option (default_id)
}

// optIdent renders the option `name` of a union as its tagged-union field. A
// Zig container's fields and declarations share one namespace, and the union
// declares, besides its fixed members (unionDecls), two members per option:
// `<opt>_id` and `<opt>Mut`. The escape is decided by the option's own spelling
// alone, never by which other options exist: an option that has the SHAPE of a
// union member -- it ends in `_id`, ends in `Mut`, or is a fixed declaration --
// takes the trailing `_` unconditionally; any other option is zigIdent's (its
// spelling, `@"kw"` for a keyword, `<name>_` for a struct declaration name).
//
// Injective by construction. Options are distinct and match naming.NameRe, so
// none ends with `_`:
//   - field vs field: an escaped field is `<name>_`, which is injective and
//     ends with `_`; a plain field never does; a quoted keyword is its own name.
//   - field vs member: every member ends in `_id` or `Mut` or is a fixed
//     declaration, none of which ends with `_`. A plain or quoted field has
//     none of those shapes (that is the condition, and no keyword or zigDecls
//     name has them), and an escaped one ends with `_`.
//   - member vs member: `<a>_id` and `<b>Mut` differ in their suffix, two
//     options never share a fold so their accessors differ, and no fixed
//     declaration ends in `_id` or `Mut`.
func optIdent(name string) string {
	if unionDecls[name] || strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "Mut") {
		return name + "_"
	}
	return zigIdent(name)
}

// optMut is the select-if-not-held accessor's name: the option name with its
// first letter lower-cased, then `Mut`. Lower-casing the first letter keeps
// options apart (two options of one union never share a fold) and makes the
// accessor a name no type identifier can be -- those start upper-case, and Zig
// rejects a reference that two declarations of one name could mean. `<x>Mut`
// is never a keyword, and nothing the union declares otherwise ends in `Mut`.
func optMut(name string) string { return strings.ToLower(name[:1]) + name[1:] + "Mut" }

// optConst is the option-id constant's name. Its last segment is the lower-case
// `id`, so it is never a type identifier either (each of those segments starts
// upper-case).
func optConst(name string) string { return name + "_id" }

// unionOptions lists nt's options in IR order with their derived names.
func unionOptions(nt *ir.NamedType) []*unionOpt {
	out := make([]*unionOpt, 0, len(nt.Fields))
	for _, f := range nt.Fields {
		out = append(out, &unionOpt{
			f:       f,
			ident:   optIdent(f.Name),
			mut:     optMut(f.Name),
			idConst: optConst(f.Name),
			isD:     nt.IsDefaultOption(f),
		})
	}
	return out
}

// emitUnion emits one union type: the tagged union, its id constants, `init`
// (default_id at that option's own default), which(), one select accessor per
// option, serialize and isDefault.
func (g *gen) emitUnion(f *zfile, name string, nt *ir.NamedType) {
	opts := unionOptions(nt)
	dopt := nt.DefaultOption()
	var d *unionOpt
	for _, o := range opts {
		if o.isD {
			d = o
		}
	}

	f.line("/// A union: holds exactly one of its options. `init` holds `%s` at its own", dopt.Name)
	f.line("/// default; selecting another option discards the one held.")
	f.line("pub const %s = union(enum) {", name)
	for _, o := range opts {
		typ := g.zigType(o.f)
		f.emitDoc("    ", fieldDoc(o.f, generator.BoundNote(o.f, zigStorage(typ))))
		f.line("    %s: %s,", o.ident, typ)
	}
	f.blank()
	for _, o := range opts {
		f.line("    /// The id of option `%s`.", o.f.Name)
		f.line("    pub const %s: sofab.Id = %d;", o.idConst, o.f.ID)
	}
	f.blank()
	f.line("    /// A fresh value: `%s` (default_id) at its own default.", dopt.Name)
	f.line("    pub const init: %s = .{ .%s = %s };", name, d.ident, g.zigFieldDefault(d.f))
	f.blank()
	f.line("    /// The id of the option this union holds.")
	f.line("    pub fn which(self: *const %s) sofab.Id {", name)
	f.line("        return switch (self.*) {")
	for _, o := range opts {
		f.line("            .%s => %s,", o.ident, o.idConst)
	}
	f.line("        };")
	f.line("    }")
	for _, o := range opts {
		f.blank()
		f.line("    /// `%s`, selected at its own default first unless it is the option held", o.f.Name)
		f.line("    /// (the one held before is discarded). An option already held is kept as is.")
		f.line("    pub fn %s(self: *%s) *%s {", o.mut, name, g.zigType(o.f))
		if len(opts) > 1 {
			f.line("        if (self.* != .%s) self.* = .{ .%s = %s };", o.ident, o.ident, g.zigFieldDefault(o.f))
		}
		f.line("        return &self.%s;", o.ident)
		f.line("    }")
	}
	f.blank()

	// MESSAGE_SPEC §4.2: default_id is written like an ordinary field of its kind
	// (omitted at its default, so a union at its default leaves the frame empty
	// and the enclosing lazy frame drops it); any other option is written even at
	// its own default, a sequence-framed one as a present frame (end_keep), since
	// an omitted option would read back as default_id.
	f.line("    /// Write the option held to `os`: default_id like an ordinary field (omitted")
	f.line("    /// at its default), any other option always, a framed one as a present frame.")
	f.line("    pub fn serialize(self: *const %s, os: *sofab.OStream) sofab.Error!void {", name)
	f.line("        switch (self.*) {")
	for _, o := range opts {
		f.line("            .%s => {", o.ident)
		g.emitMarshalAt(f, "                ", o.f, "self."+o.ident, !o.isD)
		f.line("            },")
	}
	f.line("        }")
	f.line("    }")
	f.blank()
	f.line("    /// True when default_id is held at its own default -- i.e. when serialize")
	f.line("    /// would write no child at all.")
	f.line("    pub fn isDefault(self: *const %s) bool {", name)
	f.line("        return self.* == .%s and %s;", d.ident, negate(g.fieldNeExpr(d.f, "self."+d.ident)))
	f.line("    }")
	f.line("};")
	f.blank()
}

// negate renders the boolean negation of a ≠-default test: the leading `!` of a
// single postfix expression is dropped (`!x.isDefault()` -> `x.isDefault()`),
// anything else is wrapped (`!(a != 5)`).
func negate(expr string) string {
	if strings.HasPrefix(expr, "!") && !strings.HasPrefix(expr, "!(") {
		depth := 0
		simple := true
		for _, c := range expr[1:] {
			switch c {
			case '(', '{', '[':
				depth++
			case ')', '}', ']':
				depth--
			case ' ':
				if depth == 0 {
					simple = false
				}
			}
		}
		if simple {
			return expr[1:]
		}
	}
	return "!(" + expr + ")"
}
