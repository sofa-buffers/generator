package zig

import "strings"

// One reserved-name list for the Zig backend (generator#239). A Zig container's
// fields and declarations share one namespace, so a field may take neither a
// keyword nor the name of a declaration the generated type carries. A keyword
// has an escape, `@"name"`, and it is used; a declaration name has none -- the
// quoted identifier IS the declaration -- so that field takes a trailing `_`.
// The wire is keyed by id and the JSON key is the schema name, so neither
// changes. The struct path (zigIdent) and the union path (optIdent) read the
// list; a union adds only the declarations it alone has (unionDecls).

// zigKeywords are reserved words that, used verbatim as a struct field name,
// are a syntax error and must be written as a quoted identifier (@"name").
// Primitive names (u8, bool, true, null, undefined, ...) are NOT keywords in
// field position, and neither are the words Zig has retired (async, await,
// usingnamespace): those stay unescaped, since `zig fmt` strips a quote that is
// not needed and a quoted one would fail a user's `zig fmt --check`. Every use
// of zigIdent is a member position (a field, `.name`, `self.name`).
var zigKeywords = map[string]bool{
	"addrspace": true, "align": true, "allowzero": true, "and": true,
	"anyframe": true, "anytype": true, "asm": true, "break": true,
	"callconv": true, "catch": true,
	"comptime": true, "const": true, "continue": true, "defer": true,
	"else": true, "enum": true, "errdefer": true, "error": true,
	"export": true, "extern": true, "fn": true, "for": true, "if": true,
	"inline": true, "linksection": true, "noalias": true, "noinline": true,
	"nosuspend": true, "opaque": true, "or": true, "orelse": true,
	"packed": true, "pub": true, "resume": true, "return": true,
	"struct": true, "suspend": true, "switch": true, "test": true,
	"threadlocal": true, "try": true, "union": true, "unreachable": true,
	"var": true, "volatile": true, "while": true,
}

// zigDecls are the declarations every generated struct carries -- serialize and
// isDefault on every struct, and on a message also MAX_SIZE (MAX_SIZE_LIMIT for
// an unbounded one), encode, decode, the Decoder type and decoder(). One list
// for both kinds, so a name spells the same field in a struct and a message.
var zigDecls = map[string]bool{
	"serialize": true, "isDefault": true, "encode": true, "decode": true,
	"Decoder": true, "decoder": true, "MAX_SIZE": true, "MAX_SIZE_LIMIT": true,
}

// unionDecls are the declarations every union type carries besides its options'
// own. An option whose field name would land on one takes the backend's trailing
// underscore, exactly as a struct field on a struct's declarations does
// (zigDecls).
var unionDecls = map[string]bool{
	"init": true, "which": true, "serialize": true, "isDefault": true,
}

// zigIdent renders a schema field name as a Zig identifier: @"name" for a
// keyword, name_ for a decl-clashing name, else unchanged.
func zigIdent(name string) string {
	if zigDecls[name] {
		return name + "_"
	}
	if zigKeywords[name] {
		return `@"` + name + `"`
	}
	return name
}

// zigTypeReserved are the type identifiers a generated type must not take
// (ARCHITECTURE §8, "Naming": the escape channel). A type identifier is
// PascalCase, so only a name starting upper-case can meet one; the file's own
// lower-case names (`std`, `sofab`, `max_dyn_*`), Zig's primitives and the
// generated members (`serialize`, `init`, `<option>Mut`, ...) cannot. What is
// left is every upper-case name the generated code declares and refers to
// unqualified inside a container that also refers to a type -- Zig rejects such
// a reference as ambiguous, even when the two declarations live in different
// containers (a message's `Decoder` against a file-scope type `Decoder`):
//
//   - DecodeError: the file-scope error set every decode() returns.
//   - Decoder: the incremental decoder every message declares; its methods and
//     `decoder()` name it unqualified.
//   - MAX_SIZE, MAX_SIZE_LIMIT: every message's size constants; MAX_SIZE refers
//     to MAX_SIZE_LIMIT. Both are type identifiers too (a message `MAX` with an
//     inline field `SIZE` is the type MAX_SIZE).
//
// Everything else is unreachable rather than listed: the visitor behind
// decode() is private (`_<T>__Visitor`), and the harness names every type
// qualified (`message.<T>`) and its helpers with a prefix (`toJson_<T>`).
var zigTypeReserved = map[string]bool{
	"DecodeError":    true,
	"Decoder":        true,
	"MAX_SIZE":       true,
	"MAX_SIZE_LIMIT": true,
}

// zigTypeEscape is a type identifier as declared: with a trailing `_` when it is
// reserved (zigTypeReserved). A type identifier never ends with `_`, so the
// escaped one meets no other type. Roles and private companions are built from
// the UNESCAPED identifier.
func zigTypeEscape(t string) string {
	if zigTypeReserved[t] {
		return t + "_"
	}
	return t
}

// visitorName is the private flat-visitor type behind a message's decode():
// `_` + the message's UNESCAPED type identifier + `__Visitor`. No schema name
// starts with `_`, and a type identifier has no `__`, so it meets nothing else.
func visitorName(base string) string { return "_" + base + "__Visitor" }

// Parameters and locals. Zig rejects a parameter or local that shadows ANY
// declaration of an enclosing container, which for generated code means the
// file scope and the type's own container. The generated ones (`self`, `os`,
// `alloc`, `data`, `out`, `chunk`, `id`, `value`, `m`, `v`, `st`, `_i0`, ...)
// are fixed lower-case words, so none of them can meet a declaration there:
// every type is PascalCase, the file's other names are `std`, `sofab`,
// `DecodeError` and `max_dyn_*`, a container's fixed declarations are none of
// those words, and the only schema-derived declarations inside a container --
// a union's `<option>_id` and `<option>Mut` -- end in `_id` or `Mut`, which no
// parameter or local does. Fields do not shadow: they are reached through
// `self.` only. So they stay readable instead of `_`-prefixed.

// locChild is the _Loc tag of the field `name` below the tag `loc`. The tags
// spell the schema path joined by `_` (root_a_b), so an underscore INSIDE a name
// is doubled: the path a.b is root_a_b and a field a_b is root_a__b, not the
// same tag twice. A schema name starts with a letter, so the doubling cannot be
// read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}
