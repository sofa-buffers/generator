package dart

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the Dart backend (generator#239), and the type
// escapes of the naming contract (ARCHITECTURE §8, "Naming").
//
// MEMBERS. A Dart class member is in scope in the whole class body and SHADOWS
// every outer name of the same spelling -- a type, a top-level constant, an
// import prefix -- in every position, a type annotation included, but only
// inside that class body. So a member may take none of:
//
//   - a keyword or built-in identifier, or a dart:core / dart:typed_data name
//     the class body uses (dartKeywords);
//   - a member the class declares itself (dartMembers, unionFixed);
//   - a member every class inherits from Object (dartObjectMembers): a field
//     cannot override a method, nor runtimeType with another type;
//   - an outer name the class body uses (dartOuterNames): the `sofab` import
//     prefix, dart:typed_data and dart:core types, the top-level limits;
//   - a generated class the body names: its own class, and the class of every
//     struct, union or list element it holds.
//
// Dart has no identifier escape, so such a member takes a trailing `_`. Every
// escape is decided by the member's OWN spelling (see memberNames): a name
// never depends on which other fields, options or types the schema declares.
// The last item is therefore not looked up: every generated class starts with
// an upper-case letter, so every member that does takes the `_` -- a field `M`
// is `M_` whether or not a class `M` is in reach.
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
// calls them, so a private member -- a union option's slot is `_` + its member
// -- must not hide one.
var dartPrivateHelpers = map[string]bool{
	"_f32FromBits": true, "_prefixEq": true, "_boolsEq": true, "_bools01": true,
}

// memberReserved: a name no member may take, whatever the schema.
func memberReserved(n string) bool {
	return dartKeywords[n] || dartMembers[n] || dartObjectMembers[n] || dartOuterNames[n]
}

// ---- member names -----------------------------------------------------------
//
// Every member a schema name gets in its class is spelled from that name ALONE:
// the reserved lists above and the name's own shape decide, never a sibling
// field, a sibling option or another type of the schema. Adding, removing or
// renaming one name renames nothing else.
//
// For a schema name n (a struct/message field, a union option), with low(n)
// = n with its first letter lower-cased:
//
//	member   n + "__"   n starts upper-case and a type spelled n is escaped
//	                    (typeEscape: that class is n + "_", which a body may name)
//	         n + "_"    n starts upper-case, is on a reserved list (for an
//	                    option also unionFixed), or has a derived shape (below)
//	         n          otherwise
//	stem     low(n) + "_" when n has a derived shape, else low(n)
//	bits     stem + "Fp32Bits"          (fp32 fields and options)
//	default  "_" + member + "Default"   (destination fields with a default)
//	id       stem + "Id"                (options)
//	has      "has" + Pascal(n)          (options)
//	mutable  "mutable" + Pascal(n)      (options edited in place)
//	slot     "_" + member, plus "_" where that is a library helper (options)
//	bitSlot  "_" + bits                 (fp32 options)
//
// A DERIVED SHAPE is a spelling a member derived from another name can have:
// low(n) ends in `Fp32Bits`; for a union option also: low(n) ends in `Id`, or
// is `has` or `mutable`, alone or followed by an upper-case letter. A struct
// has no derived public member but the bits, so `userId` keeps its name as a
// struct field; as a union option it is `userId_`, and an option `user` has
// its constant `userId` whether or not that option exists.
//
// Why no two members of one class meet (TestMemberNamesInjective):
//
//   - Plain members are distinct schema names. Escaped ones end in `_` or `__`,
//     which no schema name does, and dropping it gives the name back; `n__`
//     is not `m_`, since then m = n + "_".
//   - Derived members never end in `_`: they end in `Fp32Bits` or `Id`, or are
//     `has`/`mutable` + Pascal, which has no `_`. So they meet no escaped
//     member, and no plain one: a plain member has no derived shape, and the
//     shape test is exactly "could be one of these spellings".
//   - Within one channel the spellings are injective: low and Pascal keep the
//     fold, which no two names of one scope share, and the stem adds `_` only
//     to a low(n) that does not end in it.
//   - Across channels: bits end in `s`, ids in `d`; a `has`/`mutable` member is
//     that word plus an upper-case letter and has no `_`. A shaped stem has a
//     `_`, so its id and bits are neither; an unshaped stem neither starts with
//     `has`/`mutable` + upper-case nor is `has`/`mutable`, and a shorter
//     prefix of those words followed by `Id` or `Fp32Bits` is neither either.
//   - No list holds a name ending in `_`, `Id` or `Fp32Bits` or starting with
//     `has`/`mutable` + upper-case (TestDerivedShapesMissTheLists). No class
//     starts lower-case, and every member starting upper-case is escaped: a
//     class ending in `_` is n + "_" for a type-escaped n, whose member is
//     n + "__".
//   - Private members start with `_`: slots are `_` + distinct members, bit
//     slots `_` + distinct bits; a library helper ends in neither `_` nor
//     `Fp32Bits`, and a default ends in `Default`, which no helper and not
//     `_decodeInto` does.
//
// An enum or bitfield class body names only its own class (its private
// constructor), so a constant or flag takes `_` when it is on a reserved list,
// and one more when it then reads as its own class (constName).

// lowerFirst is n with its first letter lower-cased.
func lowerFirst(n string) string {
	if n != "" && isUpperASCII(n[0]) {
		return string(n[0]+'a'-'A') + n[1:]
	}
	return n
}

func isUpperASCII(c byte) bool { return c >= 'A' && c <= 'Z' }

// startsWord reports whether l is w alone or w followed by an upper-case
// letter: how a member built as w + Pascal(name) begins.
func startsWord(l, w string) bool {
	return strings.HasPrefix(l, w) && (len(l) == len(w) || isUpperASCII(l[len(w)]))
}

// fieldShaped: a struct or message field spelled like an fp32 bits companion.
func fieldShaped(n string) bool { return strings.HasSuffix(lowerFirst(n), "Fp32Bits") }

// optionShaped: a union option spelled like a member derived from an option.
func optionShaped(n string) bool {
	l := lowerFirst(n)
	return strings.HasSuffix(l, "Fp32Bits") || strings.HasSuffix(l, "Id") ||
		startsWord(l, "has") || startsWord(l, "mutable")
}

// escapeMember is the member a schema name n is reached through.
func escapeMember(n string, shaped bool, reserved func(string) bool) string {
	switch {
	case isUpperASCII(n[0]) && isTypeReserved(n):
		return n + "__"
	case isUpperASCII(n[0]) || shaped || reserved(n):
		return n + "_"
	}
	return n
}

// stem is what an option's id and an fp32 member's bits are built from.
func stem(n string, shaped bool) string {
	if shaped {
		return lowerFirst(n) + "_"
	}
	return lowerFirst(n)
}

// constName is the member of an enum constant or bitfield flag in class cls.
func constName(n, cls string) string {
	if memberReserved(n) {
		n += "_"
	}
	if n == cls {
		n += "_"
	}
	return n
}

// fieldNames are the members one struct or message field is reached through.
type fieldNames struct {
	member string // the field itself
	bits   string // an fp32 field's raw-bits companion ("" otherwise)
	def    string // the private static holding a destination's declared default ("" when none)
}

// fieldMembers names the members of one struct or message field.
func fieldMembers(f *ir.Field) fieldNames {
	shaped := fieldShaped(f.Name)
	n := fieldNames{member: escapeMember(f.Name, shaped, memberReserved)}
	if f.Kind == ir.KindFP32 {
		n.bits = stem(f.Name, shaped) + "Fp32Bits"
	}
	if hasDestDefault(f) {
		n.def = "_" + n.member + "Default"
	}
	return n
}

// memberTable holds every member name the module derives from the schema.
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
	t := &memberTable{
		fields: map[*ir.Field]fieldNames{},
		unions: map[string]*unionShape{},
		consts: map[*ir.EnumConst]string{},
		flags:  map[*ir.BitfieldFlag]string{},
	}
	g.mem = t
	for _, key := range g.schema.NamedOrder {
		nt := g.schema.Named[key]
		switch nt.Category {
		case ir.CatStruct:
			for _, f := range nt.Fields {
				t.fields[f] = fieldMembers(f)
			}
		case ir.CatUnion:
			t.unions[key] = g.buildUnionShape(key, nt)
		case ir.CatEnum:
			for _, c := range nt.Consts {
				t.consts[c] = constName(c.Name, g.typeName(key))
			}
		case ir.CatBitfield:
			for _, fl := range nt.Flags {
				t.flags[fl] = constName(fl.Name, g.typeName(key))
			}
		}
	}
	for _, m := range g.schema.Messages {
		for _, f := range m.Fields {
			t.fields[f] = fieldMembers(f)
		}
	}
	return t
}

// field is the member names of a struct or message field. A field the table
// does not hold -- a union option's working copy, whose members the union
// shape names -- is named the same way: the names depend on the field alone.
func (g *gen) field(f *ir.Field) fieldNames {
	if n, ok := g.members().fields[f]; ok {
		return n
	}
	return fieldMembers(f)
}

// member is the member a struct or message field is reached through.
func (g *gen) member(f *ir.Field) string { return g.field(f).member }
