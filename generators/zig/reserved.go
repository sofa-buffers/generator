package zig

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

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

// zigFileScope are the file-scope declarations message.zig carries besides the
// generated types: a message or named type whose Zig type name lands on one is a
// duplicate member of the file (`decode_error` is the type DecodeError). `std`
// and `sofab` need no entry: a type name is PascalCase.
var zigFileScope = map[string]bool{"DecodeError": true}

// locChild is the _Loc tag of the field `name` below the tag `loc`. The tags
// spell the schema path joined by `_` (root_a_b), so an underscore INSIDE a name
// is doubled: the path a.b is root_a_b and a field a_b is root_a__b, not the
// same tag twice. A schema name starts with a letter, so the doubling cannot be
// read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}

// checkFieldNames rejects what the list cannot prevent: two struct fields that
// give one Zig field (`encode` is mangled to `encode_`, which a field `encode_`
// already is), two constants of one enum or bitfield that give one Zig
// constant (they are upper-cased, so `a` and `A` are both `A`), and a type
// named after a file-scope declaration (zigFileScope). zig would report
// a duplicate member far from the schema. Union options are checkUnionNames'.
// Located: the error names the type and both names.
func checkFieldNames(s *ir.Schema) error {
	dup := func(owner, what string, names []string, ident func(string) string) error {
		seen := map[string]string{}
		for _, n := range names {
			id := bareIdent(ident(n))
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("zig: %s: %s %q and %q both generate %s; rename one", owner, what, prev, n, id)
			}
			seen[id] = n
		}
		return nil
	}
	fieldNames := func(fields []*ir.Field) []string {
		out := make([]string, len(fields))
		for i, f := range fields {
			out[i] = f.Name
		}
		return out
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		var err error
		switch nt.Category {
		case ir.CatStruct:
			err = dup("struct "+key, "fields", fieldNames(nt.Fields), zigIdent)
		case ir.CatEnum:
			names := make([]string, len(nt.Consts))
			for i, c := range nt.Consts {
				names[i] = c.Name
			}
			err = dup("enum "+key, "constants", names, strings.ToUpper)
		case ir.CatBitfield:
			names := make([]string, len(nt.Flags))
			for i, fl := range nt.Flags {
				names[i] = fl.Name
			}
			err = dup("bitfield "+key, "flags", names, strings.ToUpper)
		}
		if err != nil {
			return err
		}
	}
	for _, m := range s.Messages {
		if err := dup("message "+m.Name, "fields", fieldNames(m.Fields), zigIdent); err != nil {
			return err
		}
		if n := exported(m.Name); zigFileScope[n] {
			return fmt.Errorf("zig: message %q is the type %s, which message.zig already declares; rename the message", m.Name, n)
		}
	}
	for _, key := range s.NamedOrder {
		if n := (&gen{}).typeName(key); zigFileScope[n] {
			return fmt.Errorf("zig: %s is the type %s, which message.zig already declares; rename it", key, n)
		}
	}
	return nil
}
