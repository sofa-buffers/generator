package dart

import (
	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the Dart backend (generator#239), and the type
// escapes of the naming contract (ARCHITECTURE §8, "Naming").
//
// MEMBERS. A Dart class member is in scope in the whole class body and SHADOWS
// every outer name of the same spelling -- a type, a top-level constant, an
// import prefix -- in every position, a type annotation included. So a member
// may take none of:
//
//   - a keyword or built-in identifier, or a dart:core / dart:typed_data name
//     the class body uses (dartKeywords);
//   - a member the class declares itself (dartMembers, unionFixed);
//   - a member every class inherits from Object (dartObjectMembers): a field
//     cannot override a method, nor runtimeType with another type;
//   - an outer name the class body uses (dartOuterNames): the `sofab` import
//     prefix, dart:typed_data and dart:core types, the top-level limits;
//   - a class the module declares. That set depends on the schema, so it is
//     not listed: members() reads it off the schema.
//
// Dart has no identifier escape, so such a member takes a trailing `_` -- as
// many as it needs to clear every name above and every member its class already
// holds (memberAlloc). A schema name never ends with `_`, so no field is spelled
// like an escaped one; the wire is keyed by id and the JSON key is the schema
// name, so neither changes.
//
// TYPES. A type identifier (naming.TypeIdent) spelled like a name the library
// or its harness uses unqualified takes a trailing `_` (typeEscape): the
// capitalised entries of the member lists -- the names a class body uses -- and
// dartTopLevelNames, the ones only top-level code uses. A class the library
// declares shadows its own dart:typed_data import without a word, and one the
// harness imports shadows dart:core the same way, so each would change what the
// generated code means rather than fail to build. The harness imports dart:io,
// dart:convert and dart:typed_data under a prefix instead, which makes every
// name of theirs unreachable: a class sharing one would otherwise be an
// ambiguous import there.
//
// TestDartNamesInScope keeps the lists equal to what the generated code uses.

// dartKeywords are Dart reserved words: used as a field/member name they are a
// hard error. Dart has no verbatim-identifier escape (no C# `@`), so a collision
// is mangled with a trailing `_` (the C/Java/Python convention). The wire is
// keyed by id, and the JSON name stays the original (the harness maps the raw
// name), so mangling is source-only.
var dartKeywords = map[string]bool{
	"assert": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "default": true, "do": true, "else": true,
	"enum": true, "extends": true, "false": true, "final": true, "finally": true,
	"for": true, "if": true, "in": true, "is": true, "new": true, "null": true,
	"rethrow": true, "return": true, "super": true, "switch": true, "this": true,
	"throw": true, "true": true, "try": true, "var": true, "void": true,
	"while": true, "with": true,
	// Contextual/built-in identifiers that are unsafe as a member name.
	// `Function` is a built-in identifier: no class may be named that either.
	"await": true, "yield": true, "dynamic": true,
	// Core type names: a field named `int` would shadow the `int` type the
	// generated code references, so these are mangled too -- and a type spelled
	// like the capitalised ones is escaped (typeEscape).
	"int": true, "double": true, "bool": true, "num": true, "String": true,
	"List": true, "Map": true, "Set": true, "Object": true, "Iterable": true,
	"Null": true, "Never": true, "Function": true, "Uint8List": true,
	"Symbol": true, "Type": true, "Enum": true, "Record": true,
	// The encoder parameter of serialize/encodeTo: a field named `e` would shadow
	// it inside those bodies (the field's own read then names the encoder).
	"e": true,
}

// dartMembers are the members every generated class declares: serialize and
// reset on all, and on a message also encode, encodeTo, decode, tryDecode,
// decoder and the maxSize / maxSizeLimit / maxDepth constants.
var dartMembers = map[string]bool{
	"serialize": true, "reset": true, "encode": true, "encodeTo": true,
	"decode": true, "tryDecode": true, "decoder": true,
	"maxSize": true, "maxSizeLimit": true, "maxDepth": true,
}

// dartObjectMembers are what every class inherits from Object.
var dartObjectMembers = map[string]bool{
	"hashCode": true, "runtimeType": true, "toString": true, "noSuchMethod": true,
}

// dartOuterNames are the outer names a generated class body uses beyond those
// dartKeywords already holds.
var dartOuterNames = map[string]bool{
	"sofab": true, "BytesBuilder": true, "Deprecated": true, "Float32List": true,
	"Float64List": true, "Int64List": true,
	"maxDynArrayCount": true, "maxDynBlobLen": true, "maxDynStringLen": true,
}

// unionFixed are the members a union class declares on top of the above.
var unionFixed = map[string]bool{"which": true}

// dartTopLevelNames are the capitalised names generated code uses unqualified
// OUTSIDE a class body -- in the library's top-level helpers or in the harness
// -- and that no member list holds, each with its reason. A type is escaped
// away from them; a member cannot shadow them, so members may take them.
var dartTopLevelNames = map[string]string{
	"BigInt":          "dart:core; the harness carries a u64 through it",
	"ByteData":        "dart:typed_data; _f32FromBits widens fp32 NaN bits through it",
	"Endian":          "dart:typed_data; _f32FromBits reads little-endian",
	"FormatException": "dart:core; the harness refuses a rounded 64-bit JSON number",
	"StateError":      "dart:core; the harness refuses a union holding no option",
}

// typeEscape is the class a type identifier becomes: the identifier itself,
// or with a trailing `_` where the library or harness uses a name of that
// spelling unqualified (see TYPES above). A type identifier never ends with
// `_`, so no other type can take the escaped name.
func typeEscape(ident string) string {
	if isTypeReserved(ident) {
		return ident + "_"
	}
	return ident
}

// isTypeReserved: a capitalised name on a member list (what a class body
// uses) or on dartTopLevelNames (what top-level code uses).
func isTypeReserved(n string) bool {
	if n == "" || n[0] < 'A' || n[0] > 'Z' {
		return false
	}
	_, top := dartTopLevelNames[n]
	return top || dartKeywords[n] || dartOuterNames[n]
}

// dartPrivateHelpers are the library's private top-level helpers. A class body
// calls them, so a private member -- a union option's slot is `_` + its name --
// must not hide one.
var dartPrivateHelpers = map[string]bool{
	"_f32FromBits": true, "_prefixEq": true, "_boolsEq": true, "_bools01": true,
}

// memberReserved: a name no member may take, whatever the schema.
func memberReserved(n string) bool {
	return dartKeywords[n] || dartMembers[n] || dartObjectMembers[n] || dartOuterNames[n] || dartPrivateHelpers[n]
}

// memberAlloc hands out the members of ONE class body: each wanted name, with
// as many trailing `_` as it needs to clear the reserved lists, the module's
// classes and every member already handed out. The set is checked, so no two
// members of a class ever meet and no member hides a class -- by construction,
// with nothing left to refuse. Callers take the schema's own names first, so a
// field keeps its name and a derived member (a bits companion, a has<Opt>)
// is the one that yields.
type memberAlloc struct {
	blocked func(string) bool
	taken   map[string]bool
}

func newMemberAlloc(blocked func(string) bool, fixed ...string) *memberAlloc {
	a := &memberAlloc{blocked: blocked, taken: map[string]bool{}}
	for _, n := range fixed {
		a.taken[n] = true
	}
	return a
}

func (a *memberAlloc) take(want string) string {
	for a.taken[want] || a.blocked(want) {
		want += "_"
	}
	a.taken[want] = true
	return want
}

// fieldNames are the members one struct or message field is reached through.
type fieldNames struct {
	member string // the field itself
	bits   string // an fp32 field's raw-bits companion ("" otherwise)
	def    string // the private static holding a destination's declared default ("" when none)
}

// memberTable holds every member name the module derives from the schema,
// allocated class by class.
type memberTable struct {
	fields map[*ir.Field]fieldNames
	unions map[string]*unionShape
	consts map[*ir.EnumConst]string
	flags  map[*ir.BitfieldFlag]string
}

// moduleClasses is every class the library declares: each type's and each
// message's class, a message's decoder, and the private visitors.
func (g *gen) moduleClasses() map[string]bool {
	s := g.schema
	out := map[string]bool{}
	for _, key := range s.NamedOrder {
		out[g.typeName(key)] = true
		out[visitorName(g.rawTypeName(key))] = true
	}
	for _, m := range s.Messages {
		raw := messageRaw(m.Name)
		out[typeEscape(raw)] = true
		out[decoderName(raw)] = true
		out[visitorName(raw)] = true
	}
	return out
}

// members builds (once) the member names of every class of the module.
func (g *gen) members() *memberTable {
	if g.mem != nil {
		return g.mem
	}
	classes := g.moduleClasses()
	blocked := func(n string) bool { return memberReserved(n) || classes[n] }
	t := &memberTable{
		fields: map[*ir.Field]fieldNames{},
		unions: map[string]*unionShape{},
		consts: map[*ir.EnumConst]string{},
		flags:  map[*ir.BitfieldFlag]string{},
	}
	g.mem = t
	object := func(fields []*ir.Field, isMessage bool) {
		fixed := []string(nil)
		if isMessage {
			fixed = []string{"_decodeInto"}
		}
		a := newMemberAlloc(blocked, fixed...)
		for _, f := range fields {
			t.fields[f] = fieldNames{member: a.take(f.Name)}
		}
		for _, f := range fields {
			n := t.fields[f]
			if f.Kind == ir.KindFP32 {
				n.bits = a.take(n.member + "Fp32Bits")
			}
			if hasDestDefault(f) {
				n.def = a.take("_" + n.member + "Default")
			}
			t.fields[f] = n
		}
	}
	for _, key := range g.schema.NamedOrder {
		nt := g.schema.Named[key]
		switch nt.Category {
		case ir.CatStruct:
			object(nt.Fields, false)
		case ir.CatUnion:
			t.unions[key] = g.buildUnionShape(key, nt, blocked)
		case ir.CatEnum:
			a := newMemberAlloc(blocked)
			for _, c := range nt.Consts {
				t.consts[c] = a.take(c.Name)
			}
		case ir.CatBitfield:
			a := newMemberAlloc(blocked)
			for _, fl := range nt.Flags {
				t.flags[fl] = a.take(fl.Name)
			}
		}
	}
	for _, m := range g.schema.Messages {
		object(m.Fields, true)
	}
	return t
}

// field is the member names of a struct or message field. A field the table
// does not hold -- a union option's working copy, whose members the union
// shape names -- is named as a lone field would be.
func (g *gen) field(f *ir.Field) fieldNames {
	if n, ok := g.members().fields[f]; ok {
		return n
	}
	a := newMemberAlloc(memberReserved)
	n := fieldNames{member: a.take(f.Name)}
	if f.Kind == ir.KindFP32 {
		n.bits = a.take(n.member + "Fp32Bits")
	}
	if hasDestDefault(f) {
		n.def = a.take("_" + n.member + "Default")
	}
	return n
}

// member is the member a struct or message field is reached through.
func (g *gen) member(f *ir.Field) string { return g.field(f).member }
