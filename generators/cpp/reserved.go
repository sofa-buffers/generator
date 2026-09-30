package cpp

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
