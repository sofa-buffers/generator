package kotlin

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the Kotlin backend (generator#239): every name a
// schema field cannot take as a property of a generated class. A hard keyword
// has an escape, backticks, and it is used -- the property keeps the schema's
// spelling. A name that clashes with another DECLARATION has no escape, so it
// takes a trailing `_`:
//
//   - a member the class or its companion object declares (ktReservedMembers);
//   - a name the class body uses as the qualifier of an expression
//     (`Seq.boolsToBytes(...)`, `Long.MIN_VALUE`): inside the class a property
//     of that name takes precedence, and the expression no longer resolves
//     (ktQualifiers).
//
// The wire is keyed by id and the JSON key is the schema name, so neither
// changes. The struct path (ktIdent) and the union path (unionOptProp) read
// the list; a union adds only its own members (unionReserved). TestKotlinNamesInScope
// keeps ktReservedMembers and ktQualifiers equal to what the generated classes use.

// ktHardKeywords are the Kotlin *hard* keywords: the ones that can never appear
// where an identifier is expected. Kotlin has a real escape (backticks), so a
// colliding field name is ESCAPED rather than mangled -- the identifier stays
// the schema's, which keeps the JSON key and the generated member spelled the
// same (ARCHITECTURE §8, "escape where the language allows"). Soft keywords
// (`by`, `where`, `data`, ...) are legal identifiers already and are left alone.
var ktHardKeywords = map[string]bool{
	"as": true, "break": true, "class": true, "continue": true, "do": true,
	"else": true, "false": true, "for": true, "fun": true, "if": true,
	"in": true, "interface": true, "is": true, "null": true, "object": true,
	"package": true, "return": true, "super": true, "this": true, "throw": true,
	"true": true, "try": true, "typealias": true, "typeof": true, "val": true,
	"var": true, "when": true, "while": true,
}

// ktReservedMembers are the names the generated class already gives a member.
// A schema field with one of these names would redeclare it, so it is mangled
// with a trailing underscore -- a backtick escape cannot help here, because the
// clash is with another DECLARATION rather than with the grammar. The JSON key
// keeps the schema name (see the harness, which emits fld.Name).
var ktReservedMembers = map[string]bool{
	"serialize": true, "isDefault": true, "reset": true, "encode": true,
	"encodeTo": true, "decode": true, "tryDecode": true, "decoder": true,
	"MAX_SIZE": true, "MAX_SIZE_LIMIT": true, "ENC_SCRATCH": true, "Decoder": true,
}

// ktQualifiers are the names a generated class body uses as the qualifier of an
// expression: corelib types and kotlin.Long's constants.
var ktQualifiers = map[string]bool{"DecodeStatus": true, "Long": true, "Seq": true}

// unionReserved are the members a union class declares on top of the above.
var unionReserved = map[string]bool{"which": true}

// ktIdent renders a schema field name as a Kotlin member identifier: suffixed
// when it would collide with a generated declaration, escaped with backticks
// when it is a hard keyword, and otherwise passed through unchanged.
func ktIdent(name string) string {
	if ktReservedMembers[name] || ktQualifiers[name] {
		return name + "_"
	}
	if ktHardKeywords[name] {
		return "`" + name + "`"
	}
	return name
}

// locChild is the decoder frame of the field `name` below the frame `loc`. The
// frames are named by the schema path joined with `_` and looked up by name
// (locIndex), so an underscore INSIDE a name is doubled: the path a.b is
// Root_a_b and a field a_b is Root_a__b. Sharing one frame, the decoder stored
// the field a_b into a.b. A schema name starts with a letter, so the doubling
// cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}

// checkFieldNames rejects a struct or message whose fields give one Kotlin
// property (`encode` is mangled to `encode_`, which a field `encode_` already
// is) or one JVM accessor (`foo` and `Foo` are both getFoo/setFoo; `isOpen` and
// `open` are both setOpen). kotlinc would report a redeclaration or a platform
// declaration clash far from the schema. Union options are checkUnionNames'.
// Located: the error names the type and both fields.
func checkFieldNames(s *ir.Schema) error {
	check := func(owner string, fields []*ir.Field) error {
		seen := map[string]string{}
		for _, f := range fields {
			bare := strings.Trim(ktIdent(f.Name), "`")
			getter, setter := jvmAccessors(bare)
			for _, n := range []string{bare, getter, setter} {
				// An `is...` property's getter is its own name: counted once.
				if prev, ok := seen[n]; ok && prev != f.Name {
					return fmt.Errorf("kotlin backend: %s: fields %q and %q both generate %s; rename one", owner, prev, f.Name, n)
				}
				seen[n] = f.Name
			}
		}
		return nil
	}
	for _, key := range s.NamedOrder {
		if nt := s.Named[key]; nt.Category == ir.CatStruct {
			if err := check("struct "+key, nt.Fields); err != nil {
				return err
			}
		}
	}
	for _, m := range s.Messages {
		if err := check("message "+m.Name, m.Fields); err != nil {
			return err
		}
	}
	return nil
}

// jvmAccessors is the JVM getter and setter Kotlin gives a `var` property: a
// name `is` + a non-lower-case letter keeps its name as the getter and drops the
// `is` for the setter (`isOpen` -> isOpen/setOpen); any other name is
// get/set + the name with its first letter upper-cased (`foo` -> getFoo/setFoo).
func jvmAccessors(bare string) (getter, setter string) {
	if len(bare) > 2 && strings.HasPrefix(bare, "is") && !(bare[2] >= 'a' && bare[2] <= 'z') {
		return bare, "set" + bare[2:]
	}
	cap := strings.ToUpper(bare[:1]) + bare[1:]
	return "get" + cap, "set" + cap
}
