package typescript

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// One reserved-name list for the TypeScript backend (generator#239): every name
// a schema field cannot take as an INSTANCE member of a generated class. Statics
// (fromJSON, decode, MAX_SIZE, ...) live in a namespace of their own, so an
// instance field of the same name is legal and needs no entry. Keywords need
// none either: TypeScript accepts every keyword as a class member (`class`,
// `typeof`, `true` are all valid fields).
//
// JavaScript has no escape for a member name, so a name on the list is mangled
// with a trailing `_`. The wire is keyed by id and the JSON key is a separate
// string literal, so neither changes; only the member does. The struct path
// (tsIdent) and the union path (unionOptProp) both read the list; a union adds
// only the members it alone has (unionReserved). Schema names start with a
// letter, so the underscored members (_which, the private Long backings) cannot
// be reached by a field.
//
// Nothing here is refused at generation time. Two fields never derive one member
// (a schema name never ends with `_`), and a member the backend DERIVES from a
// field -- an fp32 raw-bytes companion, a union's has<Opt>/mutable<Opt> -- that
// lands on another member takes trailing underscores until it is free
// (freeMember): the schema's own names keep their spelling, the derived one
// yields.

// tsClassBody are the names the class body itself rejects. A field named
// `constructor` is a SyntaxError ("Classes may not have a field named
// 'constructor'"), and so is an accessor pair of that name; a quoted or computed
// key is refused too, or shadows the prototype's `constructor`.
var tsClassBody = map[string]bool{"constructor": true}

// tsMembers are the instance methods the backend declares on every generated
// class (serialize, toJSON, isDefault) and on every message (encode). A field of
// the same name is a duplicate identifier.
var tsMembers = map[string]bool{
	"serialize": true, "toJSON": true, "isDefault": true, "encode": true,
}

// tsObjectMembers are the members every class inherits from Object. A field of
// the same name compiles, but replaces the method on the instance: `${msg}` or
// String(msg) then throws, and so does any caller of msg.hasOwnProperty.
// (`__proto__` cannot be a schema name: those start with a letter.)
var tsObjectMembers = map[string]bool{
	"toString": true, "toLocaleString": true, "valueOf": true, "hasOwnProperty": true,
	"isPrototypeOf": true, "propertyIsEnumerable": true,
}

// unionReserved are the instance members a union class declares on top of the
// above. The underscored ones cannot be an option's name, but an option's
// private slot is `_<option>`, so they bound what an option derives.
//
// A union's statics need no list: an option's id constant is `<OPTION>_ID`, and
// no static of the class or of Function (fromJSON, decode, prototype, name,
// length) is spelled that way.
var unionReserved = map[string]bool{
	"which": true, "clear": true, "_which": true, "_leave": true,
}

// tsIdent is the class member a schema field is reached through: the schema
// name, with a trailing `_` where it is on the list.
func tsIdent(name string) string {
	if tsClassBody[name] || tsMembers[name] || tsObjectMembers[name] {
		return name + "_"
	}
	return name
}

// fp32RawNames assigns every fp32 field of one class its raw-bytes companion
// member (fp32RawCompanion): `<name>Fp32Raw`, with a trailing `_` added until it
// is free where the class already has that member -- a SIBLING field spelled
// `fFp32Raw` beside an fp32 field `f`. The schema's own fields keep their names
// and the derived member yields: the member channel's rule for a clash with
// another declaration (ARCHITECTURE §8). The fields themselves need no check:
// tsIdent is injective over what the validator accepts, since a mangled `encode_`
// cannot be a schema name (none ends with `_`).
func fp32RawNames(fields []*ir.Field, into map[*ir.Field]string) {
	taken := map[string]bool{}
	for _, set := range []map[string]bool{tsClassBody, tsMembers, tsObjectMembers} {
		for n := range set {
			taken[n] = true
		}
	}
	for _, f := range fields {
		taken[tsIdent(f.Name)] = true
	}
	for _, f := range fields {
		if fp32RawCompanion(f) {
			into[f] = freeMember(taken, fp32RawName(f.Name))
		}
	}
}

// freeMember returns n, or n with trailing underscores until no member of the
// class has it yet, and marks the result taken. Distinct inputs stay distinct:
// every result is recorded, so a later one never lands on an earlier one.
func freeMember(taken map[string]bool, n string) string {
	for taken[n] {
		n += "_"
	}
	taken[n] = true
	return n
}

// --- type-level names (ARCHITECTURE §8, "Naming: conflict-free identifiers") --
//
// message.ts is one flat module. A type is naming.TypeIdent of its schema path (a
// split union variant adds `__Default<Variant>`), a type's companion at module
// level is `<T>__<Role>` (the Decoder class, an enum's Array alias) and a private
// module name is `_<T>__<Role>` (the flat visitor, its scope constants, the
// element factory) or a fixed `_` name (`_E_Uint8Array`, `_MK_ARR`, `_decode`).
// None of those can meet a TypeIdent, which has no `__`, never starts with `_` and
// never ends with one. What is left is a TypeIdent equal to a name the module
// declares or reaches unqualified -- tsTypeReserved -- and such a type takes a
// trailing `_` (a message `long` is the class `Long_`).
//
// The corelib names stay imported by name, not behind a namespace alias: they
// are the vocabulary of the generated class's public signatures (`serialize(os:
// OStream)`, `feed(): DecodeStatus`), and the escape costs only the schemas that
// spell one of them.

// tsModuleNames are the fixed names message.ts declares at module level without a
// leading underscore: the receiver-side limit constants. A four-segment path such
// as max.dyn.array.count spells the first one as a TypeIdent.
var tsModuleNames = map[string]bool{
	"MAX_DYN_ARRAY_COUNT": true, "MAX_DYN_STRING_LEN": true, "MAX_DYN_BLOB_LEN": true,
}

// tsGlobals are the globals the emitted module uses unqualified, in a value or a
// type position. A class of the same name shadows the global for the whole module
// -- `new Uint8Array(0)` would build the message class -- so a type spelled like
// one is escaped. Only names a TypeIdent can spell are listed (it starts
// upper-case); TestTSGlobalsListed fails when the emitted code starts using one
// that is missing.
var tsGlobals = map[string]bool{
	"Array": true, "BigInt": true, "Boolean": true, "Number": true, "Record": true,
	"Uint8Array": true, "Int8Array": true, "Uint16Array": true, "Int16Array": true,
	"Uint32Array": true, "Int32Array": true, "BigUint64Array": true, "BigInt64Array": true,
	"Float32Array": true, "Float64Array": true,
}

// tsTypeReserved reports whether a type identifier lands on a module-level name:
// a fixed one (tsModuleNames), a corelib import (corelibNames, the one list the
// import line is built from, so a name added there is covered here too), or a
// global (tsGlobals).
func tsTypeReserved(id string) bool {
	if tsModuleNames[id] || tsGlobals[id] {
		return true
	}
	for _, n := range corelibNames {
		if n == id {
			return true
		}
	}
	return false
}

// rawTypeIdent is the UNESCAPED identifier of a named type: TypeIdent of its path,
// and for one variant of a split union `__Default` plus its default option. Every
// derived name (role, private) is built from this, never from the escaped one.
func rawTypeIdent(nt *ir.NamedType) string {
	id := naming.TypeIdent(nt.Path)
	if nt.Variant != "" {
		id += "__Default" + naming.Pascal(nt.Variant)
	}
	return id
}

// escapeType is the declared spelling of a type identifier: a trailing `_` where
// it is reserved at module level. A variant's `__` already keeps it apart.
func escapeType(id string) string {
	if tsTypeReserved(id) {
		return id + "_"
	}
	return id
}

// typeName / typeRaw are the declared and the unescaped identifier of the named
// type at graph key `key`.
func (g *gen) typeName(key string) string { return escapeType(g.typeRaw(key)) }
func (g *gen) typeRaw(key string) string  { return rawTypeIdent(g.schema.Named[key]) }

// msgRaw / msgName are the same pair for a message, whose path is its own name.
func msgRaw(m *ir.Message) string  { return naming.TypeIdent([]string{m.Name}) }
func msgName(m *ir.Message) string { return escapeType(msgRaw(m)) }

// The role and private spellings, all built from the unescaped identifier.
func decoderName(raw string) string    { return raw + "__Decoder" }
func enumArrayAlias(raw string) string { return raw + "__Array" }
func visitorName(raw string) string    { return "_" + raw + "__Visitor" }
func makeName(raw string) string       { return "_" + raw + "__Make" }
func scopeRoot(raw string) string      { return "_" + raw + "__Loc" }

// locChild is the decoder scope of the field `name` below the scope `loc`. The
// scopes are declared as constants spelled from the schema path joined with `_`
// below the type's own root (scopeRoot), so an underscore INSIDE a name is
// doubled: the path a.b is _M__Loc_a_b and a field a_b is _M__Loc_a__b, not one
// constant declared twice. A schema name starts with a letter, so the doubling
// cannot be read the other way; the root is per type, so two types' trees never
// share a constant.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}
