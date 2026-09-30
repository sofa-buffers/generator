package cpp

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the C++ backend (generator#239): every name a
// schema field cannot take as a member of a generated type, whatever the reason
// -- a keyword, or a member the generated class already declares. The struct
// and message path (cppIdent) and the union path (optBase) both read it; a union
// adds only the members of its own on top (unionReserved).
//
// C++ has no identifier escape, so a name on the list is mangled with a
// trailing underscore. The wire is keyed by id and the JSON keys are emitted as
// string literals, so neither changes; only the member identifier does.

// cppKeywords are the C++ reserved words (a superset of C's).
var cppKeywords = map[string]bool{
	"alignas": true, "alignof": true, "and": true, "and_eq": true, "asm": true,
	"auto": true, "bitand": true, "bitor": true, "bool": true, "break": true,
	"case": true, "catch": true, "char": true, "char8_t": true, "char16_t": true,
	"char32_t": true, "class": true, "compl": true, "concept": true, "const": true,
	"consteval": true, "constexpr": true, "constinit": true, "const_cast": true,
	"continue": true, "co_await": true, "co_return": true, "co_yield": true,
	"decltype": true, "default": true, "delete": true, "do": true, "double": true,
	"dynamic_cast": true, "else": true, "enum": true, "explicit": true, "export": true,
	"extern": true, "false": true, "float": true, "for": true, "friend": true,
	"goto": true, "if": true, "inline": true, "int": true, "long": true,
	"mutable": true, "namespace": true, "new": true, "noexcept": true, "not": true,
	"not_eq": true, "nullptr": true, "operator": true, "or": true, "or_eq": true,
	"private": true, "protected": true, "public": true, "register": true,
	"reinterpret_cast": true, "requires": true, "return": true, "short": true,
	"signed": true, "sizeof": true, "static": true, "static_assert": true,
	"static_cast": true, "struct": true, "switch": true, "template": true, "this": true,
	"thread_local": true, "throw": true, "true": true, "try": true, "typedef": true,
	"typeid": true, "typename": true, "union": true, "unsigned": true, "using": true,
	"virtual": true, "void": true, "volatile": true, "wchar_t": true, "while": true,
	"xor": true, "xor_eq": true,
}

// cppMembers are the members every generated class declares itself. A data
// member of the same name is a redeclaration, and the class no longer compiles.
//
//   - every struct, union and message: serialize / deserialize (the
//     sofab::Message overrides) and reset();
//   - every message on top: encode(), encodeTo(), decode(), try_decode().
//
// One list for all three kinds: a struct field named `encode` would compile,
// but the same name then spells one member in a struct and another in a
// message, and the list stays one per language. The underscored members
// (_maxSize, _isDefault, _opts, ...) need no entry: a schema name starts with
// a letter. The private members of the corelib base classes (decoder_,
// context_, ...) need none either: a derived member hides them, which is legal
// and leaves the base's own uses alone.
var cppMembers = map[string]bool{
	"serialize": true, "deserialize": true, "reset": true,
	"encode": true, "encodeTo": true, "decode": true, "try_decode": true,
}

// unionReserved are the members a union type declares on top of cppMembers.
var unionReserved = map[string]bool{
	"which": true, "Which": true,
}

// cppIdent is the member a schema field is reached through: the schema name,
// with a trailing underscore where it is a keyword or a generated member.
func cppIdent(name string) string {
	if cppKeywords[name] || cppMembers[name] {
		return name + "_"
	}
	return name
}

// checkConstNames rejects two constants of one enum or two flags of one
// bitfield that give one generated name: the backend spells them through
// exported(), so `a_b` and `aB` are both AB. The compiler would reject the
// duplicate far from the schema. Located: the error names the type and both
// names.
func checkConstNames(s *ir.Schema) error {
	dup := func(owner, what string, names []string) error {
		seen := map[string]string{}
		for _, n := range names {
			id := exported(n)
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("cpp: %s: %s %q and %q both generate %s; rename one", owner, what, prev, n, id)
			}
			seen[id] = n
		}
		return nil
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			names := make([]string, len(nt.Consts))
			for i, c := range nt.Consts {
				names[i] = c.Name
			}
			if err := dup("enum "+key, "constants", names); err != nil {
				return err
			}
		case ir.CatBitfield:
			names := make([]string, len(nt.Flags))
			for i, fl := range nt.Flags {
				names[i] = fl.Name
			}
			if err := dup("bitfield "+key, "flags", names); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkNamespaceNames rejects two namespace-level names the backend emits with
// one spelling. A bitfield's flags are enumerators of an unscoped enum,
// spelled <Type><Flag>, so they share the namespace with every type: `F.a_b`
// and `FA.b` are both BitfieldFAB, and so is a bitfield type `FAB`. (An
// enum's constants are members of an enum class, scoped to it, and
// checkConstNames covers them.) Located: the error names both owners.
func (g *gen) checkNamespaceNames(s *ir.Schema) error {
	seen := map[string]string{}
	claim := func(name, what string) error {
		if prev, ok := seen[name]; ok && prev != what {
			return fmt.Errorf("cpp: %s and %s both generate the name %s; rename one", prev, what, name)
		}
		seen[name] = what
		return nil
	}
	for _, m := range s.Messages {
		if err := claim(exported(m.Name), "message "+m.Name); err != nil {
			return err
		}
	}
	for _, key := range s.NamedOrder {
		if err := claim(g.typeName(key), key); err != nil {
			return err
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category != ir.CatBitfield {
			continue
		}
		for _, fl := range nt.Flags {
			if err := claim(g.typeName(key)+exported(fl.Name), fmt.Sprintf("flag %q of %s", fl.Name, key)); err != nil {
				return err
			}
		}
	}
	return nil
}
