package kotlin

import (
	"strings"
)

// The Kotlin backend's reserved names, in two lists (ARCHITECTURE §8).
//
// MEMBERS (generator#239): every name a schema field cannot take as a property
// of a generated class. A hard keyword has an escape, backticks, and it is used
// -- the property keeps the schema's spelling. A name that clashes with another
// DECLARATION has no escape, so it takes a trailing `_`:
//
//   - a member the class or its companion object declares (ktReservedMembers);
//   - a name the class body uses as the qualifier of an expression
//     (`Seq.boolsToBytes(...)`, `Long.MIN_VALUE`): inside the class a property
//     of that name takes precedence, and the expression no longer resolves
//     (ktQualifiers).
//
// The wire is keyed by id and the JSON key is the schema name, so neither
// changes. The struct path (ktIdent) and the union path (unionOptProp) read
// the list; a union adds only its own members (unionReserved).
// TestKotlinNamesInScope keeps ktReservedMembers and ktQualifiers equal to what
// the generated classes use.
//
// TYPES ("Naming: conflict-free identifiers"): every generated class and object
// sits in ONE package, next to the names the generated files use unqualified --
// the corelib's through `import org.sofabuffers.sofab.*` and Kotlin's default
// imports. A declaration of the package outranks both, so a message `string`
// spelled `String` would retype every string member of every class. A type
// identifier on ktOuterNames therefore takes a trailing `_` (ktTypeIdent), which
// no type identifier ends with. TestKotlinOuterNames keeps the list equal to
// what the generated sources use. The harness is kept out of it: its own
// declarations start with `_`, and the JVM-only names it alone needs are
// written fully qualified.

// ktHardKeywords are the Kotlin *hard* keywords: the ones that can never appear
// where an identifier is expected. Kotlin has a real escape (backticks), so a
// colliding field name is ESCAPED rather than mangled -- the identifier stays
// the schema's, which keeps the JSON key and the generated member spelled the
// same (ARCHITECTURE §8, "escape where the language allows"). Soft keywords
// (`by`, `where`, `data`, ...) are legal identifiers already and are left alone.
var ktHardKeywords = map[string]bool{
	"as": true, "break": true, "class": true, "continue": true, "do": true,
	"else": true, "false": true, "for": true, "fun": true, "if": true,
	"in": true, "interface": true, "is": true, "null": true, "object": true,
	"package": true, "return": true, "super": true, "this": true, "throw": true,
	"true": true, "try": true, "typealias": true, "typeof": true, "val": true,
	"var": true, "when": true, "while": true,
}

// ktReservedMembers are the names the generated class already gives a member.
// A schema field with one of these names would redeclare it, so it is mangled
// with a trailing underscore -- a backtick escape cannot help here, because the
// clash is with another DECLARATION rather than with the grammar. The JSON key
// keeps the schema name (see the harness, which emits fld.Name). `Companion` is
// the name of every class's companion object, a static field on the JVM.
var ktReservedMembers = map[string]bool{
	"serialize": true, "isDefault": true, "reset": true, "encode": true,
	"encodeTo": true, "decode": true, "tryDecode": true, "decoder": true,
	"MAX_SIZE": true, "MAX_SIZE_LIMIT": true, "ENC_SCRATCH": true, "Decoder": true,
	"Companion": true,
}

// ktQualifiers are the names a generated class body uses as the qualifier of an
// expression: corelib types and kotlin.Long's constants.
var ktQualifiers = map[string]bool{"DecodeStatus": true, "Long": true, "Seq": true}

// unionReserved are the members a union class declares on top of the above.
var unionReserved = map[string]bool{"which": true}

// ktOuterNames are the names a type identifier must not take: each is used
// unqualified by the generated sources, where a class of the same package would
// win the lookup, or is a class member whose scope would hide the type inside
// its own body.
var ktOuterNames = map[string]string{
	// Nested in every message class, and in scope throughout its body.
	"Decoder":   "the nested incremental decoder: inside it, `Decoder()` would build itself",
	"Companion": "the companion object's name, in scope in the class body",

	// corelib-kotlin-mp, through `import org.sofabuffers.sofab.*`.
	"ArrayKind":      "corelib: array wire kinds the visitor routes on",
	"DecodeStatus":   "corelib: the feed's verdict",
	"FixlenType":     "corelib: the fixlen subtype fixlenBegin routes on",
	"IStream":        "corelib: the decode stream",
	"OStream":        "corelib: the encode stream",
	"PayloadAcc":     "corelib: payload reassembly and the unbounded encode's sink",
	"Seq":            "corelib: array helpers and shared empty arrays",
	"SofabError":     "corelib: refusal categories",
	"SofabException": "corelib: the refusal thrown from the visitor",
	"Visitor":        "corelib: the interface every decode visitor implements",

	// Kotlin's default imports (kotlin.*, kotlin.collections.*, ...).
	"Any":                       "kotlin: the bulk destination slot",
	"Boolean":                   "kotlin: boolean members",
	"BooleanArray":              "kotlin: boolean arrays",
	"Byte":                      "kotlin: i8 members",
	"ByteArray":                 "kotlin: blob members, i8 arrays",
	"Deprecated":                "kotlin: the annotation on deprecated fields",
	"Double":                    "kotlin: fp64 members",
	"DoubleArray":               "kotlin: fp64 arrays",
	"ExperimentalUnsignedTypes": "kotlin: the file-level opt-in",
	"Float":                     "kotlin: fp32 members",
	"FloatArray":                "kotlin: fp32 arrays",
	"Int":                       "kotlin: i32 and enum members",
	"IntArray":                  "kotlin: i32 arrays, the scope stack",
	"Long":                      "kotlin: i64 members, the visitor's carrier",
	"LongArray":                 "kotlin: i64 arrays",
	"MutableList":               "kotlin: wrapper arrays",
	"OptIn":                     "kotlin: the file-level opt-in",
	"Short":                     "kotlin: i16 members",
	"ShortArray":                "kotlin: i16 arrays",
	"String":                    "kotlin: string members",
	"Suppress":                  "kotlin: warning suppressions",
	"UByte":                     "kotlin: u8 members",
	"UByteArray":                "kotlin: u8 arrays",
	"UInt":                      "kotlin: u32 members",
	"UIntArray":                 "kotlin: u32 arrays",
	"ULong":                     "kotlin: u64 and bitfield members",
	"ULongArray":                "kotlin: u64 arrays",
	"UShort":                    "kotlin: u16 members",
	"UShortArray":               "kotlin: u16 arrays",
}

// ktTypeIdent escapes a type identifier that ktOuterNames holds with a trailing
// `_` -- the escape channel: a TypeIdent never ends with `_`, so the escaped
// spelling is no other type's. Names a backend derives from a type (a variant,
// the private visitor) are built from the UNESCAPED identifier.
func ktTypeIdent(t string) string {
	if _, ok := ktOuterNames[t]; ok {
		return t + "_"
	}
	return t
}

// ktIdent renders a schema field name as a Kotlin member identifier: suffixed
// when it would collide with a generated declaration, escaped with backticks
// when it is a hard keyword, and otherwise passed through unchanged.
func ktIdent(name string) string {
	if ktReservedMembers[name] || ktQualifiers[name] {
		return name + "_"
	}
	if ktHardKeywords[name] {
		return "`" + name + "`"
	}
	return name
}

// locChild is the decoder frame of the field `name` below the frame `loc`. The
// frames are named by the schema path joined with `_` and looked up by name
// (locIndex), so an underscore INSIDE a name is doubled: the path a.b is
// Root_a_b and a field a_b is Root_a__b. Sharing one frame, the decoder stored
// the field a_b into a.b. A schema name starts with a letter, so the doubling
// cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}

// jvmAccessors is the JVM getter and setter Kotlin gives a `var` property: a
// name `is` + a non-lower-case letter keeps its name as the getter and drops the
// `is` for the setter (`isOpen` -> isOpen/setOpen); any other name is
// get/set + the name with its first letter upper-cased (`foo` -> getFoo/setFoo).
func jvmAccessors(bare string) (getter, setter string) {
	if len(bare) > 2 && strings.HasPrefix(bare, "is") && !(bare[2] >= 'a' && bare[2] <= 'z') {
		return bare, "set" + bare[2:]
	}
	cap := strings.ToUpper(bare[:1]) + bare[1:]
	return "get" + cap, "set" + cap
}

// jvmSetterRenames gives a JVM name to every setter that would otherwise share
// one with a sibling's, keyed by the property as emitted (props: the members of
// one class, escaped or backticked).
//
// The naming rules leave exactly one such pair (ARCHITECTURE §8): an `is`
// property drops its `is` for the setter, so `isOpen` and `open` both get
// setOpen -- a JVM "platform declaration clash" kotlinc reports although the
// Kotlin names differ. Getters never meet: an `is` property's getter is its own
// name, every other one starts with `get`. The `is` property's setter is
// renamed to set + its whole name + `__` (`setIsOpen__`): no property gives a
// JVM accessor with `__`, because a name contains none and an escape adds one
// trailing `_`. The Kotlin API is untouched; only a Java caller sees the name.
func jvmSetterRenames(props []string) map[string]string {
	setters := map[string]int{}
	for _, p := range props {
		_, s := jvmAccessors(strings.Trim(p, "`"))
		setters[s]++
	}
	out := map[string]string{}
	for _, p := range props {
		bare := strings.Trim(p, "`")
		g, s := jvmAccessors(bare)
		if g == bare && setters[s] > 1 { // an `is` property on a shared setter
			out[p] = `@set:kotlin.jvm.JvmName("set` + strings.ToUpper(bare[:1]) + bare[1:] + `__")`
		}
	}
	return out
}
