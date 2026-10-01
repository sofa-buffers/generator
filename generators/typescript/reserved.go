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
// only the members it alone has (unionReserved).
//
// Nothing here is refused at generation time, and no spelling depends on which
// sibling names exist: every member is a function of its OWN schema name, so
// adding a field or an option never renames another one's members.
//
// The members of one class, and why no two meet. A schema name n matches
// naming.NameRe -- it starts with a letter, has no `__`, never ends with `_` --
// and sibling names have distinct folds. The spellings derived from it:
//
//	prop(n)    the field / option member: n, or n+"_" where n is on a list
//	           above or has a derived member's shape (shaped)
//	raw(n)     an fp32 field's raw-bytes companion: prop(n)+"Fp32Raw"
//	has(n)     a union option's "has"+Pascal(n), with "__" appended where that
//	           is a fixed member (derivedMember: only Object's hasOwnProperty,
//	           reached by an option spelled `own_property`)
//	mutable(n) a union option's "mutable"+Pascal(n), the same way
//	slot(n)    a union option's private slot: "_"+prop(n)
//	rawSlot(n) its fp32 raw-bytes slot: "_"+raw(n)
//	_<n>       a struct's private Long backing
//	fixed      the class's own members (the lists below) and a union's private
//	           tag and release method, `__which` and `__leave`
//
// The shapes (shaped): ends with "Fp32Raw"; in a union also starts with "has"
// or "mutable" followed by nothing, an upper-case letter or a digit.
//
//   - prop is injective: two names differ, and n is never m+"_" (no name ends
//     with `_`). raw, slot and rawSlot are prop plus a fixed affix, so they are
//     injective too.
//   - prop vs a derived member: an unescaped prop is not shaped, but every raw
//     ends with "Fp32Raw" and every has/mutable is "has"/"mutable" + an
//     upper-case letter (Pascal(n) starts with one). An escaped prop ends with
//     exactly one `_`, which no derived member does (Pascal has no `_`, raw ends
//     with "Raw", the derivedMember escape with two).
//   - has vs mutable: they differ in their first letter. Each is injective,
//     since Pascal(n) lower-cased is fold(n).
//   - raw vs has (and mutable): raw(m) = prop(m)+"Fp32Raw" starting with "has"
//     + an upper-case letter means prop(m) is "has" or starts with "has" + an
//     upper-case letter or digit; then m is shaped, prop(m) = m+"_", and raw(m)
//     holds a `_` that has(n) cannot. (That is why a bare `has` / `mutable` is
//     shaped: raw(`has`) would otherwise be has(`fp32_raw`).)
//   - private vs public: private members start with `_`, public ones with a
//     letter. slot vs rawSlot follows from prop vs raw; a slot is `_` + a
//     letter, so `__which`/`__leave` are out of its reach; a struct's only
//     private members are the backings `_<n>`, injective in n.
//   - fixed vs schema-derived: tsIdent/unionOptProp escape every listed name;
//     no fixed member ends with "Fp32Raw" or starts with "mutable", and the one
//     spelled "has" + an upper-case letter is escaped by derivedMember.

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

// unionReserved are the public instance members a union class declares on top
// of the above. Its private tag and release method are spelled with a leading
// `__` (`__which`, `__leave`), which no `_`+prop slot can be.
//
// A union's statics need no list: an option's id constant is `<OPTION>_ID`, and
// no static of the class or of Function (fromJSON, decode, prototype, name,
// length) is spelled that way.
var unionReserved = map[string]bool{"which": true, "clear": true}

// fp32RawSuffix ends every fp32 raw-bytes companion member.
const fp32RawSuffix = "Fp32Raw"

// fixedMember reports whether n is a member every generated class declares or
// inherits -- and, with union, one every union class declares.
func fixedMember(n string, union bool) bool {
	return tsClassBody[n] || tsMembers[n] || tsObjectMembers[n] || (union && unionReserved[n])
}

// derivedPrefix reports whether n starts with p followed by nothing, an
// upper-case letter or a digit: the shape of a member spelled p+Pascal(...).
func derivedPrefix(n, p string) bool {
	if !strings.HasPrefix(n, p) {
		return false
	}
	if len(n) == len(p) {
		return true
	}
	c := n[len(p)]
	return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// shaped reports whether a schema name has the shape of a member the backend
// derives from a name: an fp32 raw-bytes companion in every class, and in a
// union has<Opt>/mutable<Opt> too. Such a name takes the escape, so every
// derived member keeps its plain spelling whatever the siblings are.
func shaped(n string, union bool) bool {
	if strings.HasSuffix(n, fp32RawSuffix) {
		return true
	}
	return union && (derivedPrefix(n, "has") || derivedPrefix(n, "mutable"))
}

// tsIdent is the struct/message member a schema field is reached through: the
// schema name, with a trailing `_` where it is on the list or shaped.
func tsIdent(name string) string {
	if fixedMember(name, false) || shaped(name, false) {
		return name + "_"
	}
	return name
}

// fp32RawName is the raw-bytes companion of the member prop.
func fp32RawName(prop string) string { return prop + fp32RawSuffix }

// derivedMember is the spelling of a union member derived as d: d itself, or d
// with `__` where d is a fixed member -- has<Opt> of an option `own_property` is
// Object's hasOwnProperty. No prop ends with `__`.
func derivedMember(d string) string {
	if fixedMember(d, true) {
		return d + "__"
	}
	return d
}

// fp32RawNames assigns every fp32 field of one class its raw-bytes companion
// member (fp32RawCompanion): the field's own member plus "Fp32Raw".
func fp32RawNames(fields []*ir.Field, into map[*ir.Field]string) {
	for _, f := range fields {
		if fp32RawCompanion(f) {
			into[f] = fp32RawName(tsIdent(f.Name))
		}
	}
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
	"Array": true, "BigInt": true, "Boolean": true, "Number": true, "Object": true, "Record": true,
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
