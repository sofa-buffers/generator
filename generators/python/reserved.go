package python

import "strings"

// One reserved-name list for the Python backend (generator#239): every name a
// schema field cannot take as an attribute of a generated class, whatever the
// reason -- a keyword, a member the class already declares, or a builtin the
// class body evaluates while the class is being defined. The dataclass path
// (pyIdent) and the union path (unionOptProp) both read it; a union adds only the
// members of its own on top (unionReserved).
//
// Python has no identifier escape, so a name on the list is mangled with a
// trailing underscore. The wire is keyed by id and the JSON key is a separate
// string literal, so neither changes; only the attribute does. Schema names start
// with a letter, so the underscored and dunder members (_is_default, __init__,
// ...) cannot be reached and need no entry.

// pyKeywords are Python's hard reserved words, invalid as attribute names.
// (`match`/`case`/`type` are soft keywords, valid as identifiers.)
var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true,
	"assert": true, "async": true, "await": true, "break": true, "class": true,
	"continue": true, "def": true, "del": true, "elif": true, "else": true,
	"except": true, "finally": true, "for": true, "from": true, "global": true,
	"if": true, "import": true, "in": true, "is": true, "lambda": true,
	"nonlocal": true, "not": true, "or": true, "pass": true, "raise": true,
	"return": true, "try": true, "while": true, "with": true, "yield": true,
}

// pyMembers are the members every generated class (dataclass or union) declares
// itself. A field of the same name is rebound by the member defined after it in
// the class body -- a method silently replaces the field's default, a field
// silently shadows the method on the instance (`msg.encode` is then an int).
var pyMembers = map[string]bool{
	"serialize": true, "encode": true, "decode": true, "decoder": true,
	"to_jsonable": true, "from_jsonable": true, "MAX_SIZE": true, "MAX_SIZE_LIMIT": true,
}

// pyEvaluated are the names a class body EVALUATES while the class is being
// defined. A class body is one scope, so a field bound earlier under one of them
// rebinds it for everything after:
//
//   - classmethod: the decorator on from_jsonable/decoder/decode (the module
//     fails at import: 'int' object is not callable);
//   - property: the decorator on a union's option properties;
//   - field: dataclasses.field, in every later `field(default_factory=...)`;
//   - list: the default_factory of every later array field;
//   - REASSEMBLY: the default of decoder()'s reassembly parameter.
//
// Annotations are postponed (`from __future__ import annotations`) and default
// factories are lambdas, both of which read the module scope and not the
// class's, so the type names they spell need no entry. The two defaults that
// are evaluated in the class body do not name a type either: a blob default is
// a bytes literal, and an enum default goes through the enum's private alias
// (enumAlias), which no field can be spelled like.
var pyEvaluated = map[string]bool{
	"classmethod": true, "property": true, "field": true, "list": true,
	"REASSEMBLY": true,
}

// unionReserved are the members a union class declares on top of pyMembers.
var unionReserved = map[string]bool{
	"which": true, "clear": true,
}

// pyIdent is the attribute a schema field is reached through: the schema name,
// with a trailing underscore where it is on the list above.
func pyIdent(name string) string {
	if pyKeywords[name] || pyMembers[name] || pyEvaluated[name] {
		return name + "_"
	}
	return name
}

// unionDerivedShape reports whether an option name is spelled like a member the
// union derives from ANOTHER option: `has_<opt>`, `mutable_<opt>` or
// `<OPT>_ID`. Such an option's property takes a trailing underscore, whether or
// not the option it could meet exists -- so the test depends on the name alone,
// and no two options of a valid schema can ever derive one member (a derived
// member never ends with `_`; an escaped property always does).
func unionDerivedShape(name string) bool {
	return strings.HasPrefix(name, "has_") || strings.HasPrefix(name, "mutable_") ||
		strings.HasSuffix(name, "_ID")
}

// --- type level ------------------------------------------------------------
//
// Every generated class and enum is declared at MODULE level, beside what the
// module imports and the constants it states. A class spelled like one of those
// rebinds it for everything after its definition -- a message `status` would
// turn every `Status.COMPLETE` into an AttributeError, a message `reassembly`
// every decoder()'s default -- and the generator exits 0, because the breakage
// is only found at import or at the first decode.
//
// A type identifier (naming.TypeIdent) always starts with an upper-case letter,
// so only the upper-case names of the module scope can be reached, and a
// generator-owned private name (`_StreamDecoder`, `_M__Visitor`) never can: no
// schema name starts with `_`. What is left is this list, and a type identifier
// on it takes a trailing underscore (`Status_`), which no other type identifier
// has (ARCHITECTURE §8, "Naming").
//
// The imports stay unaliased and the constants public on purpose: MAX_FIELD_SPAN
// and REASSEMBLY are documented API a caller sizes decoder() with, and sofab's
// names are what a user of the module reads alongside it.

// pyTypeReserved is every module-level name a generated type must not take,
// each with the reason it is taken. Every upper-case name module() can import is
// on it (TestTypeReservedCoversImports).
var pyTypeReserved = map[string]string{
	// Keywords a class cannot be named at all.
	"False": "keyword", "None": "keyword", "True": "keyword",
	// Imported from the standard library by the type section.
	"IntEnum": "enum import", "IntFlag": "enum import", "ClassVar": "typing import",
	// Imported from corelib-py (the `from sofab import` line).
	"Binding": "sofab import", "Decoder": "sofab import", "Encoder": "sofab import",
	"Field": "sofab import", "FixlenSubtype": "sofab import",
	"SofaDecodeError": "sofab import", "SofaIncompleteError": "sofab import",
	"SofaLimitError": "sofab import", "Status": "sofab import",
	"UNBOUNDED": "sofab import", "Visitor": "sofab import", "WireType": "sofab import",
	// The module's own public constants.
	"MAX_DYN_ARRAY_COUNT": "module constant", "MAX_DYN_STRING_LEN": "module constant",
	"MAX_DYN_BLOB_LEN": "module constant", "MAX_FIELD_SPAN": "module constant",
	"REASSEMBLY": "module constant",
}

// escapeType is the class name of a type identifier: the identifier itself, or
// with a trailing underscore where it is on pyTypeReserved. Names derived from a
// type (its visitor, its dispatch locations) are built from the UNESCAPED
// identifier -- they carry `__`, so they cannot meet a list entry anyway.
func escapeType(ident string) string {
	if _, ok := pyTypeReserved[ident]; ok {
		return ident + "_"
	}
	return ident
}
