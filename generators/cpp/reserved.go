package cpp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// One reserved-name list for the C++ backend (generator#239, generator#624):
// every spelling a generated identifier cannot take, whatever the reason, in
// two scopes.
//
//   - Members (fields, union accessors): a keyword, a member the generated
//     class already declares, a macro of the headers the code reaches
//     (macros.go), or a namespace-level name the class body refers to -- every
//     type identifier and bitfield flag of the schema, so a field named like the
//     class it sits in, or like a type that class uses, neither redeclares nor
//     "changes the meaning of" it. member() reads it; a union adds the members
//     of its own on top (unionReserved, optBase).
//   - Types (docs/ARCHITECTURE.md §8, "Naming"): a type identifier equal to a
//     name a generated class body sees unqualified (cppTypeReserved), a macro,
//     or a component of the configured namespace. typeEscape reads it.
//
// C++ has no identifier escape, so a name on the list takes a trailing
// underscore, which no schema name and no type identifier ends with. The wire
// is keyed by id and the JSON keys are emitted as string literals, so neither
// changes; only the identifier does.
//
// Parameters and locals of the generated member functions all start with "_"
// (_os, _is, _id, _data, ...): a schema name never does, so no field can shadow
// one -- C++ reserves only "_" + an upper-case letter, and "__".

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
// context_, field_callback_, ...) need none either: a member ends with "_" only
// when it is escaped, and none of them is the escape of a name on this list.
var cppMembers = map[string]bool{
	"serialize": true, "deserialize": true, "reset": true,
	"encode": true, "encodeTo": true, "decode": true, "try_decode": true,
}

// corelibProbes are the members corelib-cpp looks for on the object a
// sequence decodes into, to tell a wrapper-array collector from a message:
// `cap` + `dynCap` (the element index bounds), the static `elemDestCap`,
// `elemWire`, `elemFix`, and `prepare()` (the §7.4 reset); corelib-c-cpp's
// OStreamObject reads a static `MAX_SIZE`. A member of one of these names turns
// a message into a collector -- its field values become index bounds, a
// non-static one fails to compile -- so none is a member spelling.
var corelibProbes = map[string]bool{
	"cap": true, "dynCap": true, "elemDestCap": true, "elemWire": true, "elemFix": true,
	"prepare": true, "MAX_SIZE": true,
}

// unionReserved are the members a union type declares on top of cppMembers.
var unionReserved = map[string]bool{
	"which": true, "Which": true,
}

// unionRoles are the prefixes of the accessors a union derives from each
// option name n: set_n(), has_n(), mutable_n(). They are built from the plain
// schema name, and the getter of an option whose own name starts with one of
// them takes a trailing underscore (optBase), so an option `set_a` beside an
// option `a` gives the getter set_a_() and the setter set_a() -- no two
// accessors share a spelling.
var unionRoles = []string{"set_", "has_", "mutable_"}

// cppTypeReserved are the names every generated class body sees unqualified
// before it sees the namespace's own types, so a type identifier spelled like
// one would be found as the wrong entity.
var cppTypeReserved = map[string]bool{
	// Every union declares a nested `enum class Which` and names its option
	// types in the same body.
	"Which": true,
	// Every generated type derives from sofab::Message, whose name -- and the
	// names of its two bases -- are injected into the derived class's scope.
	"Message": true, "OStreamMessage": true, "IStreamMessage": true,
	// corelib-c-cpp's IStreamMessage declares a private nested struct Context,
	// which lookup in a derived class finds first (and then refuses as private).
	"Context": true,
}

// sofabPrefixed reports whether an identifier falls in the macro namespace the
// corelibs and this backend claim: SOFAB_* (corelib, and the receiver caps this
// backend defines) and SOFABGEN_* (the RawArray include guard). Escaped by
// prefix rather than listed, so a macro a later corelib adds is covered too.
func sofabPrefixed(name string) bool {
	return strings.HasPrefix(name, "SOFAB_") || strings.HasPrefix(name, "SOFABGEN_")
}

// isMacro is the part of the list both scopes share: a name the preprocessor
// replaces.
func isMacro(name string) bool {
	return cppHeaderMacros[name] || sofabPrefixed(name)
}

// member is the identifier a schema name takes as a member of a generated
// type: the name itself, or the name with a trailing underscore where it is on
// the list. Distinct names of one scope stay distinct: a schema name never ends
// with "_".
func (g *gen) member(name string) string {
	if cppKeywords[name] || cppMembers[name] || corelibProbes[name] || isMacro(name) || g.nsNames[name] {
		return name + "_"
	}
	return name
}

// constIdent is the enumerator of an enum constant inside its `enum class`:
// Pascal(name), escaped where a macro would replace it. Pascal keeps the
// constants of one enum apart (their folds differ) and never yields a keyword;
// the enumerators are scoped, so they meet no other name.
func constIdent(name string) string {
	id := naming.Pascal(name)
	if isMacro(id) {
		return id + "_"
	}
	return id
}

// typeEscape is the escape channel for a namespace-level identifier: a
// trailing underscore where it is a name the generated code sees unqualified,
// a macro, or a component of the configured namespace. A type identifier (or a
// bitfield flag) never ends with "_", so the escaped spelling is no other
// name's.
func (g *gen) typeEscape(id string) string {
	if cppTypeReserved[id] || isMacro(id) || g.nsParts[id] {
		return id + "_"
	}
	return id
}

// qualifierNamespaces are the namespaces the generated code names with a
// qualifier -- std::, the corelib's sofab:: and the backend's own sofabgen::
// (RawArray). A configured namespace with one of them as a component either
// puts the generated types into the corelib's namespace, where a message
// `OStream` redefines sofab::OStream, or makes `sofab::...` / `std::...` find
// the configured namespace instead.
var qualifierNamespaces = map[string]bool{"std": true, "sofab": true, "sofabgen": true}

// cppIdent is the spelling of a C++ identifier.
var cppIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkNamespace refuses a configured namespace the generated code cannot be
// wrapped in. It is a configuration error, never a schema one: every schema
// generates under the default namespace and under any namespace this accepts.
// Each "::"-separated component must be a C++ identifier that is not reserved
// to the implementation ("__", "_" + upper case), not a keyword, not a macro
// of the headers the code includes, and not one of qualifierNamespaces.
func checkNamespace(ns string) error {
	for _, p := range strings.Split(ns, "::") {
		var why string
		switch {
		case !cppIdent.MatchString(p):
			why = "is not a C++ identifier"
		case strings.Contains(p, "__") || (len(p) > 1 && p[0] == '_' && p[1] >= 'A' && p[1] <= 'Z'):
			why = "is reserved to the C++ implementation"
		case cppKeywords[p]:
			why = "is a C++ keyword"
		case isMacro(p):
			why = "is a macro of the headers the generated code includes"
		case qualifierNamespaces[p]:
			why = "is a namespace the generated code refers to (std, sofab, sofabgen)"
		}
		if why != "" {
			return fmt.Errorf("cpp: config namespace %q: component %q %s; choose another namespace", ns, p, why)
		}
	}
	return nil
}

// assignNames fills the namespace-level identifiers from the schema paths
// (docs/ARCHITECTURE.md §8, "Naming"):
//
//   - a message, $defs, inline or array-element type: naming.TypeIdent(path);
//   - a split union variant: TypeIdent(path) + "_default_" + Pascal(variant).
//     C++ reserves every identifier containing "__", so the role is "_" and a
//     lower-case role word, which no Pascal segment starts with;
//   - a bitfield flag -- an enumerator of an unscoped enum, and so a
//     namespace-level name: TypeIdent(path) + "_" + Pascal(flag), the child of
//     a leaf type, which has no child paths.
//
// Each then passes typeEscape; the derived names are built from the unescaped
// identifier. nsNames collects every namespace-level identifier, which member()
// keeps the members away from.
func (g *gen) assignNames(s *ir.Schema) {
	g.nsParts = map[string]bool{}
	for _, p := range strings.Split(g.ns, "::") {
		g.nsParts[p] = true
	}
	g.types = map[string]string{}
	g.msgTypes = map[*ir.Message]string{}
	g.flags = map[string][]string{}
	g.nsNames = map[string]bool{}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		t := naming.TypeIdent(nt.Path)
		id := g.typeEscape(t)
		if nt.Variant != "" {
			id = t + "_default_" + naming.Pascal(nt.Variant)
		}
		g.types[key] = id
		g.nsNames[id] = true
		if nt.Category == ir.CatBitfield {
			names := make([]string, len(nt.Flags))
			for i, fl := range nt.Flags {
				names[i] = g.typeEscape(t + "_" + naming.Pascal(fl.Name))
				g.nsNames[names[i]] = true
			}
			g.flags[key] = names
		}
	}
	for _, m := range s.Messages {
		id := g.typeEscape(naming.TypeIdent([]string{m.Name}))
		g.msgTypes[m] = id
		g.nsNames[id] = true
	}
}
