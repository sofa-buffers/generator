package java

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the Java backend (generator#239): every name a
// schema field cannot take as a field of a generated class. Java keeps fields
// and methods apart, so a field `encode` beside the method encode() is legal
// and needs no entry. What a field does collide with:
//
//   - a keyword;
//   - a static field the class declares itself (javaStatics);
//   - a name the class body uses as the qualifier of an expression
//     (`Seq.reset(...)`, `java.util.Arrays.equals(...)`): inside the class a
//     field of that name OBSCURES the type or package (JLS 6.4.2), so the
//     expression no longer compiles (javaQualifiers).
//
// Java has no identifier escape, so a name on the list is mangled with a
// trailing underscore; the JSON key is a separate string literal and the wire
// is keyed by id, so neither changes. The struct path (javaIdent) and the union
// slots (unionSlot) read the list. TestJavaNamesInScope keeps javaStatics and
// javaQualifiers equal to what the generated classes actually use.

// javaKeywords are Java reserved words. Java has no raw-identifier escape, so a
// field with such a name is mangled (trailing underscore); the JSON key keeps the
// original name (emitted as a separate string literal).
var javaKeywords = map[string]bool{
	"abstract": true, "assert": true, "boolean": true, "break": true, "byte": true,
	"case": true, "catch": true, "char": true, "class": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extends": true, "final": true, "finally": true, "float": true,
	"for": true, "goto": true, "if": true, "implements": true, "import": true,
	"instanceof": true, "int": true, "interface": true, "long": true, "native": true,
	"new": true, "package": true, "private": true, "protected": true, "public": true,
	"return": true, "short": true, "static": true, "strictfp": true, "super": true,
	"switch": true, "synchronized": true, "this": true, "throw": true, "throws": true,
	"transient": true, "try": true, "void": true, "volatile": true, "while": true,
	"true": true, "false": true, "null": true, "var": true, "record": true, "yield": true,
}

// javaStatics are the static fields every generated message class declares.
// (The option-id constants of a union are checkUnionNames'; the _arrdef_*
// constants start with `_`, which no schema name does.)
var javaStatics = map[string]bool{"MAX_SIZE": true, "MAX_SIZE_LIMIT": true}

// javaQualifiers are the names a generated class body uses as the qualifier of
// an expression: corelib and java.util types (`List.of(...)` in an array
// default), and the `java` package root of a
// fully qualified call.
var javaQualifiers = map[string]bool{
	"Arrays": true, "DecodeStatus": true, "List": true, "OStream": true, "Seq": true,
	"System": true, "java": true,
}

// javaIdent is the field a schema field is reached through: the schema name,
// with a trailing underscore where it is on the list.
func javaIdent(name string) string {
	if javaKeywords[name] || javaStatics[name] || javaQualifiers[name] {
		return name + "_"
	}
	return name
}

// checkFieldNames rejects a struct or message whose fields give one Java field:
// a mangled name landing on a field that already has it (`int` and `int_`).
// javac would report a duplicate variable far from the schema. Union slots are
// checkUnionNames'. Located: the error names the type and both fields.
func checkFieldNames(s *ir.Schema) error {
	check := func(owner string, fields []*ir.Field) error {
		seen := map[string]string{}
		for _, f := range fields {
			n := javaIdent(f.Name)
			if prev, ok := seen[n]; ok {
				return fmt.Errorf("java backend: %s: fields %q and %q both generate the field %s; rename one", owner, prev, f.Name, n)
			}
			seen[n] = f.Name
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

// locChild is the decoder frame of the field `name` below the frame `loc`. The
// frames are named by the schema path joined with `_` and looked up by name
// (locIndex), so an underscore INSIDE a name is doubled: the path a.b is
// Root_a_b and a field a_b is Root_a__b. Without it the two shared one frame and
// the decoder silently stored the field a_b into a.b. A schema name starts with
// a letter, so the doubling cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}
