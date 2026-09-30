package golang

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

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
// the union path (unionShapeOf, checkUnionNames) both read the list; a union
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

// checkFieldNames rejects a struct or message whose fields derive the same Go
// field. exported() folds underscores into camel case, so `a_b` and `aB` are
// both `AB`, and `encode_` lands on the mangled `encode`. Located: the error
// names the type and both fields.
func (g *gen) checkFieldNames() error {
	check := func(owner string, fields []*ir.Field) error {
		seen := map[string]string{}
		for _, f := range fields {
			n := goFieldName(f.Name)
			if prev, ok := seen[n]; ok {
				return fmt.Errorf("go backend: %s: fields %q and %q both generate the Go field %s; rename one", owner, prev, f.Name, n)
			}
			seen[n] = f.Name
		}
		return nil
	}
	for _, key := range g.schema.NamedOrder {
		if nt := g.schema.Named[key]; nt.Category == ir.CatStruct {
			if err := check("struct "+key, nt.Fields); err != nil {
				return err
			}
		}
	}
	for _, m := range g.schema.Messages {
		if err := check("message "+m.Name, m.Fields); err != nil {
			return err
		}
	}
	return nil
}
