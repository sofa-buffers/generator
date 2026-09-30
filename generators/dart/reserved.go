package dart

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the Dart backend (generator#239). A Dart class
// member is in scope in the whole class body and SHADOWS every outer name of
// the same spelling -- a type, a top-level constant, an import prefix -- in
// every position, a type annotation included. So a field may take none of:
//
//   - a keyword or built-in identifier, or a dart:core / dart:typed_data name
//     the class body uses (dartKeywords);
//   - a member the class declares itself (dartMembers);
//   - a member every class inherits from Object (dartObjectMembers): a field
//     cannot override a method, nor runtimeType with another type;
//   - an outer name the class body uses (dartOuterNames): the `sofab` import
//     prefix, dart:typed_data and dart:core types, the top-level limits.
//
// A generated class name (`StructPoint`, `MDecoder`) cannot be listed -- it
// depends on the schema -- so checkFieldNames rejects a field spelled like one.
// Dart has no identifier escape, so a name on the list is mangled with a
// trailing `_`; the wire is keyed by id and the JSON key is the schema name,
// so neither changes. The struct path (dartIdent) and the union path
// (unionOptProp) read the list; a union adds only its own `which`.
// TestDartNamesInScope keeps the lists equal to what the generated classes use.

// dartKeywords are Dart reserved words: used as a field/member name they are a
// hard error. Dart has no verbatim-identifier escape (no C# `@`), so a collision
// is mangled with a trailing `_` (the C/Java/Python convention). The wire is
// keyed by id, and the JSON name stays the original (the harness maps the raw
// name), so mangling is source-only.
var dartKeywords = map[string]bool{
	"assert": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "default": true, "do": true, "else": true,
	"enum": true, "extends": true, "false": true, "final": true, "finally": true,
	"for": true, "if": true, "in": true, "is": true, "new": true, "null": true,
	"rethrow": true, "return": true, "super": true, "switch": true, "this": true,
	"throw": true, "true": true, "try": true, "var": true, "void": true,
	"while": true, "with": true,
	// Contextual/built-in identifiers that are unsafe as a member name.
	"await": true, "yield": true, "dynamic": true,
	// Core type names: a field named `int` would shadow the `int` type the
	// generated code references, so these are mangled too. (A field named after a
	// generated class depends on the schema, so it is not listed here:
	// checkFieldNames rejects it.)
	"int": true, "double": true, "bool": true, "num": true, "String": true,
	"List": true, "Map": true, "Set": true, "Object": true, "Iterable": true,
	"Null": true, "Never": true, "Function": true, "Uint8List": true,
	"Symbol": true, "Type": true, "Enum": true, "Record": true,
	// The encoder parameter of serialize/encodeTo: a field named `e` would shadow
	// it inside those bodies (the field's own read then names the encoder).
	"e": true,
}

// dartMembers are the members every generated class declares: serialize and
// reset on all, and on a message also encode, encodeTo, decode, tryDecode,
// decoder and the maxSize / maxSizeLimit / maxDepth constants.
var dartMembers = map[string]bool{
	"serialize": true, "reset": true, "encode": true, "encodeTo": true,
	"decode": true, "tryDecode": true, "decoder": true,
	"maxSize": true, "maxSizeLimit": true, "maxDepth": true,
}

// dartObjectMembers are what every class inherits from Object.
var dartObjectMembers = map[string]bool{
	"hashCode": true, "runtimeType": true, "toString": true, "noSuchMethod": true,
}

// dartOuterNames are the outer names a generated class body uses beyond those
// dartKeywords already holds.
var dartOuterNames = map[string]bool{
	"sofab": true, "BytesBuilder": true, "Deprecated": true, "Float32List": true,
	"Float64List": true, "Int64List": true,
	"maxDynArrayCount": true, "maxDynBlobLen": true, "maxDynStringLen": true,
}

// unionFixed are the members a union class declares on top of the above.
var unionFixed = map[string]bool{"which": true}

// dartIdent is the member a schema field is reached through: the schema name,
// with a trailing underscore where it is on the list.
func dartIdent(name string) string {
	if dartKeywords[name] || dartMembers[name] || dartObjectMembers[name] || dartOuterNames[name] {
		return name + "_"
	}
	return name
}

// dartTypeNames are the class names the generated module declares: a message's
// class and its Decoder, and every named type's class.
func (g *gen) dartTypeNames(s *ir.Schema) map[string]string {
	names := map[string]string{}
	for _, m := range s.Messages {
		names[exported(m.Name)] = "the message class of " + m.Name
		names[exported(m.Name)+"Decoder"] = "the decoder class of " + m.Name
	}
	for _, key := range s.NamedOrder {
		names[g.typeName(key)] = "the class of " + key
	}
	return names
}

// checkFieldNames rejects a struct or message whose fields give one Dart member
// -- a mangled name landing on a field that already has it (`encode` and
// `encode_`), or an fp32 field's bits companion landing on another field (`f`
// gives fFp32Bits) -- and a field spelled like a generated class, which would
// shadow that class in the whole body; and two enum constants or bitfield flags
// that give one static member, or one named like its class. Located: the
// error names the type and the names.
func (g *gen) checkFieldNames(s *ir.Schema) error {
	types := g.dartTypeNames(s)
	check := func(owner string, fields []*ir.Field) error {
		seen := map[string]string{}
		for _, f := range fields {
			names := []string{dartIdent(f.Name)}
			if f.Kind == ir.KindFP32 {
				names = append(names, fp32BitsField(f.Name))
			}
			for _, n := range names {
				if what, ok := types[n]; ok {
					return fmt.Errorf("dart backend: %s: field %q generates the member %s, which is also %s; rename the field", owner, f.Name, n, what)
				}
				if prev, ok := seen[n]; ok {
					return fmt.Errorf("dart backend: %s: fields %q and %q both generate the member %s; rename one", owner, prev, f.Name, n)
				}
				seen[n] = f.Name
			}
		}
		return nil
	}
	// Enum constants and bitfield flags are static members of their own class,
	// named through dartIdent: `class` is class_, which a constant `class_`
	// already is, and a constant cannot share its class's name.
	consts := func(owner, what, class string, names []string) error {
		seen := map[string]string{}
		for _, n := range names {
			id := dartIdent(n)
			if id == class {
				return fmt.Errorf("dart backend: %s: %s %q is named like its own class %s; rename it", owner, what, n, class)
			}
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("dart backend: %s: %s %q and %q both generate %s; rename one", owner, what, prev, n, id)
			}
			seen[id] = n
		}
		return nil
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		var err error
		switch nt.Category {
		case ir.CatStruct:
			err = check("struct "+key, nt.Fields)
		case ir.CatEnum:
			names := make([]string, len(nt.Consts))
			for i, c := range nt.Consts {
				names[i] = c.Name
			}
			err = consts("enum "+key, "constants", g.typeName(key), names)
		case ir.CatBitfield:
			names := make([]string, len(nt.Flags))
			for i, fl := range nt.Flags {
				names[i] = fl.Name
			}
			err = consts("bitfield "+key, "flags", g.typeName(key), names)
		}
		if err != nil {
			return err
		}
	}
	for _, m := range s.Messages {
		if err := check("message "+m.Name, m.Fields); err != nil {
			return err
		}
	}
	return nil
}
