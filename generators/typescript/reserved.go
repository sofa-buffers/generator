package typescript

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
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
var unionReserved = map[string]bool{
	"which": true, "clear": true, "_which": true, "_leave": true,
}

// unionReservedStatic are a union class's own statics (and a Function's): an
// option whose id constant lands on one is refused (checkUnionNames).
var unionReservedStatic = map[string]bool{
	"fromJSON": true, "decode": true, "prototype": true, "name": true,
	"length": true, "caller": true, "arguments": true,
}

// tsIdent is the class member a schema field is reached through: the schema
// name, with a trailing `_` where it is on the list.
func tsIdent(name string) string {
	if tsClassBody[name] || tsMembers[name] || tsObjectMembers[name] {
		return name + "_"
	}
	return name
}

// checkFieldNames rejects a struct or message whose fields derive the same
// member: a mangled name landing on a field that already carries it (`encode`
// and `encode_`), or an fp32 field's raw-bytes companion landing on another
// field (`f` gives fFp32Raw). Located: the error names the type and both fields.
func checkFieldNames(s *ir.Schema) error {
	check := func(owner string, fields []*ir.Field) error {
		seen := map[string]string{}
		for _, f := range fields {
			names := []string{tsIdent(f.Name)}
			if fp32RawCompanion(f) {
				names = append(names, fp32RawName(f.Name))
			}
			for _, n := range names {
				if prev, ok := seen[n]; ok {
					return fmt.Errorf("typescript backend: %s: fields %q and %q both generate the member %s; rename one", owner, prev, f.Name, n)
				}
				seen[n] = f.Name
			}
		}
		return nil
	}
	for _, key := range s.NamedOrder {
		if nt := s.Named[key]; nt.Category == ir.CatStruct {
			if err := check("struct "+key, nt.Fields); err != nil {
				return err
			}
		}
	}
	for _, m := range s.Messages {
		if err := check("message "+m.Name, m.Fields); err != nil {
			return err
		}
	}
	return nil
}

// checkConstNames rejects two constants of one enum or two flags of one
// bitfield that give one generated name: the backend spells them through
// exported(), so `a_b` and `aB` are both AB. The compiler would reject the
// duplicate far from the schema. Located: the error names the type and both
// names.
func checkConstNames(s *ir.Schema) error {
	dup := func(owner, what string, names []string) error {
		seen := map[string]string{}
		for _, n := range names {
			id := exported(n)
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("typescript backend: %s: %s %q and %q both generate %s; rename one", owner, what, prev, n, id)
			}
			seen[id] = n
		}
		return nil
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			names := make([]string, len(nt.Consts))
			for i, c := range nt.Consts {
				names[i] = c.Name
			}
			if err := dup("enum "+key, "constants", names); err != nil {
				return err
			}
		case ir.CatBitfield:
			names := make([]string, len(nt.Flags))
			for i, fl := range nt.Flags {
				names[i] = fl.Name
			}
			if err := dup("bitfield "+key, "flags", names); err != nil {
				return err
			}
		}
	}
	return nil
}

// locChild is the decoder scope of the field `name` below the scope `loc`. The
// scopes are declared as constants spelled from the schema path joined with `_`
// (_L_M_a_b), so an underscore INSIDE a name is doubled: the path a.b is
// _L_M_a_b and a field a_b is _L_M_a__b, not one constant declared twice. A
// schema name starts with a letter, so the doubling cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}
