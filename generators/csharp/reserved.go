package csharp

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the C# backend (generator#239): every name a schema
// field cannot take as a field of a generated class. A keyword has an escape,
// the verbatim identifier `@name`, and it is used -- the field keeps the
// schema's spelling. A name that clashes with another DECLARATION has none (the
// verbatim identifier is still that name), so it takes a trailing `_`:
//
//   - a member the class declares itself (csMembers): C# puts fields, methods,
//     properties and nested types in ONE namespace (CS0102);
//   - a member every class inherits from object (csObjectMembers): a field of
//     that name hides it, a warning -warnaserror refuses;
//   - a name the class body uses as the qualifier of an expression
//     (`System.Array.Empty<T>()`, `DecodeStatus.Complete`): inside the class
//     simple-name lookup finds the field first (csQualifiers).
//
// A field named after its own class (CS0542) cannot be listed -- it depends on
// the type -- so checkFieldNames rejects it. The wire is keyed by id and the
// JSON key is a separate string literal, so neither changes. The struct path
// (csIdent) and the union path (unionOptProp) read the list; a union adds only
// its own members (unionFixed). TestCSharpNamesInScope keeps the lists equal to
// what the generated classes declare and use.

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

// csQualifiers are the names a generated class body uses as the qualifier of an
// expression.
var csQualifiers = map[string]bool{"Array": true, "DecodeStatus": true, "System": true}

// unionFixed are the members a union class declares on top of the above.
var unionFixed = map[string]bool{"Which": true, "Clear": true}

// unionFixedAll is every name a union option's derived members must avoid.
func unionFixedAll() map[string]bool {
	all := map[string]bool{}
	for _, set := range []map[string]bool{csMembers, csObjectMembers, csQualifiers, unionFixed} {
		for n := range set {
			all[n] = true
		}
	}
	return all
}

// csReserved reports whether n clashes with a declaration a generated class
// carries or uses.
func csReserved(n string) bool {
	return csMembers[n] || csObjectMembers[n] || csQualifiers[n]
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

// checkFieldNames rejects a struct or message whose fields give one C# field
// (`Encode` is mangled to `Encode_`, which a field `Encode_` already is), a
// field named after its own class (CS0542: a message `m` is the class M, so a
// field `M` cannot be its member), and two enum constants or bitfield flags
// that give one PascalCase member (`a_b` and `aB` are both AB). Union options are checkUnionNames'.
// Located: the error names the type and the fields.
func (g *gen) checkFieldNames() error {
	check := func(owner, typeName string, fields []*ir.Field) error {
		seen := map[string]string{}
		for _, f := range fields {
			n := strings.TrimPrefix(csIdent(f.Name), "@")
			if n == typeName {
				return fmt.Errorf("csharp backend: %s: field %q is named like its own class %s (CS0542); rename the field", owner, f.Name, typeName)
			}
			if prev, ok := seen[n]; ok {
				return fmt.Errorf("csharp backend: %s: fields %q and %q both generate the field %s; rename one", owner, prev, f.Name, n)
			}
			seen[n] = f.Name
		}
		return nil
	}
	// Enum constants and bitfield flags are PascalCase members of their class:
	// `a_b` and `aB` are both AB.
	consts := func(owner, what string, names []string) error {
		seen := map[string]string{}
		for _, n := range names {
			id := exported(n)
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("csharp backend: %s: %s %q and %q both generate %s; rename one", owner, what, prev, n, id)
			}
			seen[id] = n
		}
		return nil
	}
	for _, key := range g.schema.NamedOrder {
		nt := g.schema.Named[key]
		var err error
		switch nt.Category {
		case ir.CatStruct:
			err = check("struct "+key, g.typeName(key), nt.Fields)
		case ir.CatEnum:
			names := make([]string, len(nt.Consts))
			for i, c := range nt.Consts {
				names[i] = c.Name
			}
			err = consts("enum "+key, "constants", names)
		case ir.CatBitfield:
			names := make([]string, len(nt.Flags))
			for i, fl := range nt.Flags {
				names[i] = fl.Name
			}
			err = consts("bitfield "+key, "flags", names)
		}
		if err != nil {
			return err
		}
	}
	for _, m := range g.schema.Messages {
		if err := check("message "+m.Name, exported(m.Name), m.Fields); err != nil {
			return err
		}
	}
	return nil
}

// csRenamed reports whether a field's C# name differs from its schema name
// other than by the `@` escape -- the fields that need their JSON name spelled
// out.
func csRenamed(name string) bool { return csReserved(name) }
