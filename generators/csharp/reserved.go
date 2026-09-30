package csharp

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// One reserved-name list for the C# backend (generator#239, #624), at two
// levels.
//
// TYPE level (ARCHITECTURE §8, "Naming"). Every generated type is named after
// naming.TypeIdent of its schema path, and Message.cs reaches every name outside
// its own namespace fully qualified (`global::System.Array`,
// `global::sofab.OStream`) and imports nothing, so no schema type can shadow a
// corelib or BCL name: those names are unreachable, not listed. What a type
// identifier can still meet is listed in csTypeReserved -- a name the generated
// code uses UNQUALIFIED where it would be found before a namespace-level type --
// and, per class, the class's own members (CS0542: a member may not be named
// like its enclosing class). Either takes the escape, a trailing `_`
// (typeIdent). The harness (Program.cs) lives in the global namespace, imports
// no generated namespace and names every generated type `global::<ns>.<T>`, and
// its own classes start with `_`, so it adds nothing to the list.
//
// MEMBER level: every name a schema field cannot take as a field of a generated
// class. A keyword has an escape, the verbatim identifier `@name`, and it is
// used -- the field keeps the schema's spelling. A name that clashes with
// another DECLARATION has none (the verbatim identifier is still that name), so
// it takes a trailing `_`:
//
//   - a member the class declares itself (csMembers): C# puts fields, methods,
//     properties and nested types in ONE namespace (CS0102);
//   - a member every class inherits from object (csObjectMembers): a field of
//     that name hides it, a warning -warnaserror refuses.
//
// A name a class body uses in front of a dot (`System.Array`, `DecodeStatus`)
// needs no entry: the body writes it `global::`-qualified, which no field can
// shadow. The wire is keyed by id and the JSON key is a separate string literal,
// so neither changes. The struct path (csIdent) and the union path
// (unionOptProp) read the list; a union adds only its own members (unionFixed).
// TestCSharpNamesInScope keeps the lists equal to what the generated classes
// declare and use.

// csTypeReserved are the names a type identifier escapes with a trailing `_`,
// each with the reason generated code reaches it unqualified.
var csTypeReserved = map[string]string{
	// Inside a message class the simple name Decoder finds the nested class
	// before a namespace-level type, so a field of a type named Decoder would be
	// declared with the nested class instead.
	"Decoder": "the nested incremental decoder of every message class",
}

// csKeywords are C# reserved words; used as an identifier they need the
// verbatim-identifier escape `@name`. System.Text.Json serialises `@int` under
// the name "int", so JSON/wire names are unchanged.
var csKeywords = map[string]bool{
	"abstract": true, "as": true, "base": true, "bool": true, "break": true,
	"byte": true, "case": true, "catch": true, "char": true, "checked": true,
	"class": true, "const": true, "continue": true, "decimal": true, "default": true,
	"delegate": true, "do": true, "double": true, "else": true, "enum": true,
	"event": true, "explicit": true, "extern": true, "false": true, "finally": true,
	"fixed": true, "float": true, "for": true, "foreach": true, "goto": true,
	"if": true, "implicit": true, "in": true, "int": true, "interface": true,
	"internal": true, "is": true, "lock": true, "long": true, "namespace": true,
	"new": true, "null": true, "object": true, "operator": true, "out": true,
	"override": true, "params": true, "private": true, "protected": true, "public": true,
	"readonly": true, "ref": true, "return": true, "sbyte": true, "sealed": true,
	"short": true, "sizeof": true, "stackalloc": true, "static": true, "string": true,
	"struct": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "uint": true, "ulong": true, "unchecked": true,
	"unsafe": true, "ushort": true, "using": true, "virtual": true, "void": true,
	"volatile": true, "while": true,
}

// csMembers are the members every generated class declares: Serialize and
// IsDefault on all, and on a message Reset, Encode, EncodeTo, Decode,
// TryDecode, the nested Decoder class and the MaxSize/MaxSizeLimit constants.
var csMembers = map[string]bool{
	"Serialize": true, "IsDefault": true, "Reset": true, "Encode": true,
	"EncodeTo": true, "Decode": true, "TryDecode": true, "Decoder": true,
	"MaxSize": true, "MaxSizeLimit": true,
}

// csObjectMembers are what every class inherits from object.
var csObjectMembers = map[string]bool{
	"Equals": true, "GetHashCode": true, "GetType": true, "ToString": true,
	"MemberwiseClone": true, "Finalize": true, "ReferenceEquals": true,
}

// unionFixed are the members a union class declares on top of the above.
var unionFixed = map[string]bool{"Which": true, "Clear": true}

// csReserved reports whether n clashes with a declaration a generated class
// carries.
func csReserved(n string) bool {
	return csMembers[n] || csObjectMembers[n]
}

// csIdent is the field a schema field is reached through: `name_` where it
// clashes with a declaration, `@name` where it is a keyword, else the name.
func csIdent(name string) string {
	if csReserved(name) {
		return name + "_"
	}
	if csKeywords[name] {
		return "@" + name
	}
	return name
}

// locChild is the visitor's location constant of the field `name` below `loc`.
// The constants spell the schema path joined with `_`, so an underscore INSIDE
// a name is doubled: the path a.b is Root_a_b and a field a_b is Root_a__b,
// not one constant defined twice. A schema name starts with a letter, so the
// doubling cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}

// csRenamed reports whether a field's C# name differs from its schema name
// other than by the `@` escape -- the fields that need their JSON name spelled
// out.
func csRenamed(name string) bool { return csReserved(name) }

// typeIdent is the C# identifier of a type whose unescaped name is base, in a
// class declaring members: base, with a trailing `_` while it is on
// csTypeReserved or names one of those members (CS0542). A base never ends
// with `_` (naming.TypeIdent, and the `__Default<Opt>` of a split variant), so
// stripping the escape gives base back and two types stay two identifiers. The
// loop runs a second time only where a member took the member escape itself: a
// message `encode` (Encode, a member of its own) holding a field `Encode`
// (Encode_) is the class Encode__.
func typeIdent(base string, members map[string]bool) string {
	t := base
	for csTypeReserved[t] != "" || members[t] {
		t += "_"
	}
	return t
}

// nameTypes fixes the identifier of every type the module declares before
// anything is emitted: a named type's from its schema path (and, for a split
// union, its variant), a message's from its name. The unescaped name is kept
// too: the private per-type names derive from it (_<T>__Visitor).
func (g *gen) nameTypes() {
	g.types, g.bases = map[string]string{}, map[string]string{}
	g.msgTypes, g.msgBases = map[string]string{}, map[string]string{}
	for key, nt := range g.schema.Named {
		base := naming.TypeIdent(nt.Path)
		if nt.Variant != "" {
			base += "__Default" + naming.Pascal(nt.Variant)
		}
		g.bases[key] = base
		g.types[key] = typeIdent(base, classMembers(nt))
	}
	for _, m := range g.schema.Messages {
		base := naming.TypeIdent([]string{m.Name})
		g.msgBases[m.Name] = base
		g.msgTypes[m.Name] = typeIdent(base, messageMembers(m.Fields))
	}
}

// fieldMembers are the members a struct class declares: its fields and the two
// every class has.
func fieldMembers(fields []*ir.Field) map[string]bool {
	ms := map[string]bool{"Serialize": true, "IsDefault": true}
	for _, f := range fields {
		ms[strings.TrimPrefix(csIdent(f.Name), "@")] = true
	}
	return ms
}

// messageMembers adds what a message class declares on top of a struct's.
func messageMembers(fields []*ir.Field) map[string]bool {
	ms := fieldMembers(fields)
	for n := range csMembers {
		ms[n] = true
	}
	return ms
}

// classMembers are the members a named type's class declares. An enum or a
// bitfield contributes none: C# allows an enum constant named like its enum.
func classMembers(nt *ir.NamedType) map[string]bool {
	switch nt.Category {
	case ir.CatStruct:
		return fieldMembers(nt.Fields)
	case ir.CatUnion:
		ms := map[string]bool{"Serialize": true, "IsDefault": true}
		for n := range unionFixed {
			ms[n] = true
		}
		for _, f := range nt.Fields {
			for _, n := range unionOptMembers(f) {
				ms[n] = true
			}
		}
		return ms
	}
	return nil
}
