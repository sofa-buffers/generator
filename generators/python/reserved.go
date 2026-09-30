package python

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
// class's, so the type names they spell need no entry.
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
