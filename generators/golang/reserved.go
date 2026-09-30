package golang

// One reserved-name list for the Go backend (generator#239): every name a
// schema field cannot take as a member of a generated type. Go keywords need no
// entry -- a field is exported, so its name starts upper-case and no keyword
// does -- which leaves the members the generated type already carries. A field
// of the same name is a compile error: Go forbids a field and a method sharing
// a name, and two fields sharing one; a field that merely hides a PROMOTED
// method compiles, but the type then no longer implements sofab.Visitor.
//
// Go has no identifier escape, so a name on the list is mangled with a trailing
// underscore. The wire is keyed by id and the `json` tag keeps the schema name,
// so neither changes; only the Go field does. The struct path (goFieldName) and
// the union path (unionShapeOf) both read the list; a union
// adds only the members it alone has (unionReserved).

// goVisitorMembers are the sofab.Visitor callbacks every generated struct,
// union and message implements -- itself or through the embedded
// sofab.VisitorBase -- and that embedded field's own name. It is the whole
// interface, not the callbacks a type happens to declare: a field named after
// one it inherits hides the promoted method, and the type stops being a
// Visitor.
var goVisitorMembers = map[string]bool{
	"Unsigned": true, "Signed": true, "Float32": true, "Float64": true,
	"FixlenBegin": true, "String": true, "Bytes": true,
	"ArrayBegin": true, "ArrayUnsigned": true, "ArraySigned": true,
	"ArrayFloat32": true, "ArrayFloat64": true, "ArrayEnd": true,
	"BeginSequence": true, "EndSequence": true,
	"VisitorBase": true,
}

// goStringCheckMembers are the embedded sofab.StringCheck (a type holding a
// string field carries it) and what it promotes. A field hiding SetStringCheck
// would silently take the decode's UTF-8 policy off the type.
var goStringCheckMembers = map[string]bool{
	"StringCheck": true, "UTF8Valid": true, "SetStringCheck": true,
}

// goMembers are the methods the backend declares on every generated type
// (Serialize) and on every message (Encode, EncodeTo). One list for all kinds,
// so a name spells the same member in a struct and in a message.
var goMembers = map[string]bool{
	"Serialize": true, "Encode": true, "EncodeTo": true,
}

// unionReserved are the members a union type declares on top of the above.
var unionReserved = map[string]bool{
	"Which": true, "Clear": true, "MarshalJSON": true, "UnmarshalJSON": true,
}

// goReserved reports whether n, an exported name, is on the list.
func goReserved(n string) bool {
	return goVisitorMembers[n] || goStringCheckMembers[n] || goMembers[n]
}

// goFieldName is the exported struct-field name for a schema field, mangled with
// a trailing underscore when it is on the list.
func goFieldName(name string) string {
	n := exported(name)
	if goReserved(n) {
		return n + "_"
	}
	return n
}

// No field-name check follows goFieldName, and none is needed: the naming rules
// (ARCHITECTURE §8, "Naming") give the fields of one scope distinct folds, so
// their Pascal names differ, and a mangled name ends in "_", which no schema
// name does. The same holds for enum constants and bitfield flags, which are
// children of their type (Type_Name).

// goPackageNames are the EXPORTED package-level names the generated package
// declares itself, beside the types (the escape channel of ARCHITECTURE §8,
// "Naming"). A type identifier spelled like one takes a trailing underscore
// (escapeType). Everything else the package declares at package level is out
// of a type identifier's reach without an entry:
//
//   - a message's companions are roles (M__New, M__Decode, M__MaxSize, ...),
//     and enum constants, bitfield flags and union option ids are children
//     (Color_Red, U_Opt__ID): no type identifier contains "__", and the path
//     of an enum, a bitfield or a union option has no child to spell the rest;
//   - the private names (_isDefaulter, _caps, _M__EncOpts) start with "_";
//   - imports (sofab, io, fmt, json, bytes, ...), Go's keywords and predeclared
//     identifiers are all lower-case, and a type identifier starts upper-case;
//   - the harness (emit: project) is package main of its own, and names the
//     generated types only qualified (message.M).
var goPackageNames = map[string]bool{
	// The receiver-side decode limits (sofab_visitor.go), exported for the
	// caller: a message `max_dyn_string_len` is the type MaxDynStringLen_.
	"MaxDynArrayCount": true,
	"MaxDynStringLen":  true,
	"MaxDynBlobLen":    true,
}

// escapeType is the escape channel: a type identifier on goPackageNames takes a
// trailing underscore, which no type identifier ends with.
func escapeType(t string) string {
	if goPackageNames[t] {
		return t + "_"
	}
	return t
}

// unionMember reports whether n is a member every union type already has: a
// name on the list, or one only a union declares.
func unionMember(n string) bool { return goReserved(n) || unionReserved[n] }

// accessorPrefixes are the words a union's per-option accessors put in front
// of the option's Pascal name: Set<Opt>, Has<Opt>, Mut<Opt>.
var accessorPrefixes = []string{"Set", "Has", "Mut"}

// unionGetter is the getter of an option whose Pascal name is p. It is p, with
// a trailing underscore when p is a union member or when p itself reads as an
// accessor of another option: `set_x` is SetX, the setter of `x`, so the getter
// of `set_x` is SetX_. A Pascal name never contains "_", so a getter either has
// none or ends in one.
func unionGetter(p string) string {
	if unionMember(p) {
		return p + "_"
	}
	for _, pre := range accessorPrefixes {
		if len(p) > len(pre) && p[:len(pre)] == pre && p[len(pre)] >= 'A' && p[len(pre)] <= 'Z' {
			return p + "_"
		}
	}
	return p
}

// unionAccessor is prefix+p, or prefix+"_"+p when that spells a union member
// (the setter of an option `string_check` would hide the promoted
// SetStringCheck and silently drop the decode's UTF-8 policy). The inner
// underscore is a spelling no getter has -- a getter's only "_" is its last
// byte -- and no other accessor: the prefix is fixed and p is distinct per
// option. So every option's accessors are distinct from every other's and from
// the union's own members, by construction.
func unionAccessor(prefix, p string) string {
	if n := prefix + p; !unionMember(n) {
		return n
	}
	return prefix + "_" + p
}
