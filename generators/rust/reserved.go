package rust

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the Rust backend (generator#239). A struct field
// and a method live in different namespaces in Rust -- `m.encode` and
// `m.encode()` coexist -- so a struct field is bound by the keywords alone. A
// union's option ACCESSORS are methods, so the union path adds the method names
// a union type already has (unionReserved). Rust has an escape for a keyword
// (`r#name`), and the escape is used wherever the language takes it; only the
// four keywords it refuses take a trailing underscore (rustNonRaw).

// rustKeywords are reserved words that, used verbatim as a struct field name,
// are a syntax error and must be written as a raw identifier (`r#name`). serde's
// derives strip the `r#` prefix, so JSON field names are unchanged.
var rustKeywords = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "crate": true,
	"dyn": true, "else": true, "enum": true, "extern": true, "false": true,
	"fn": true, "for": true, "if": true, "impl": true, "in": true, "let": true,
	"loop": true, "match": true, "mod": true, "move": true, "mut": true,
	"pub": true, "ref": true, "return": true, "static": true, "struct": true,
	"trait": true, "true": true, "type": true, "unsafe": true, "use": true,
	"where": true, "while": true, "async": true, "await": true, "yield": true,
	"gen": true, "abstract": true, "become": true, "box": true, "do": true,
	"final": true, "macro": true, "override": true, "priv": true, "typeof": true,
	"unsized": true, "virtual": true, "try": true,
}

// rustNonRaw are the four keywords that CANNOT be written as raw identifiers
// (`r#self` etc. is rejected). A field with one of these names is mangled with a
// trailing underscore instead; rustNeedsRename then forces a serde rename so the
// JSON/wire name stays the original.
var rustNonRaw = map[string]bool{"self": true, "Self": true, "crate": true, "super": true}

// rustIdent renders a schema field name as a Rust identifier: `r#name` for a
// keyword, `name_` for the four non-raw-able keywords, else unchanged.
func rustIdent(name string) string {
	if rustNonRaw[name] {
		return name + "_"
	}
	if rustKeywords[name] {
		return "r#" + name
	}
	return name
}

// rustNeedsRename reports whether a field needs a serde rename to preserve its
// JSON name — true only for the underscore-mangled non-raw-able keywords (serde
// already strips `r#`, so r#-escaped fields don't need it).
func rustNeedsRename(name string) bool { return rustNonRaw[name] }

// unionReserved are the value-namespace members a union type has without any
// option: its own API, the methods of the traits it derives (Debug, Clone,
// PartialEq, Default, serde's two), and the methods the standard library's
// blanket impls give every such type (ToOwned, Borrow, BorrowMut, Into,
// TryInto, Any). A same-named inherent method would shadow one at a call site
// (`u.clone()`, `u.to_owned()`), so a generated accessor that would land on one
// -- the getter `<opt>` or the mutable `<opt>_mut` -- takes the backend's
// trailing underscore, exactly as a keyword that cannot be a raw identifier
// does.
var unionReserved = map[string]bool{
	"which": true, "serialize": true, "deserialize": true,
	"default": true, "clone": true, "clone_from": true, "eq": true, "ne": true, "fmt": true,
	"to_owned": true, "clone_into": true, "borrow": true, "borrow_mut": true,
	"into": true, "try_into": true, "type_id": true,
	// `<opt>_mut(&mut self) -> &mut T` has the shape of AsMut::as_mut and
	// DerefMut::deref_mut, which clippy (should_implement_trait) refuses under
	// -D warnings for options `as` and `deref`. The whole look-alike family was
	// probed (as_ref, next, from_str, into_iter, hash, cmp, index, add, ...);
	// only these three are refused.
	"as_mut": true, "deref_mut": true,
	// A getter `new(&self)` trips clippy::new_ret_no_self.
	"new": true,
}

// locChild is the _Loc variant of the field `name` below the variant `loc`. The
// variants spell the schema path joined by `_`, so an underscore INSIDE a name
// is doubled: the path a.b is Root_a_b and a field a_b is Root_a__b. A schema
// name starts with a letter, so the doubling cannot be read the other way.
func locChild(loc, name string) string {
	return loc + "_" + strings.ReplaceAll(name, "_", "__")
}

// nonSnakeAllow is the attribute a struct with a field, or a union accessor,
// spelled from a non-snake_case schema name carries (`Foo`, `a__b`, or `Self`
// mangled to `Self_`): rustc's non_snake_case lint would otherwise fail a `-D warnings`
// build over a name the schema chose. "" for a snake_case name.
func nonSnakeAllow(ident string) string {
	// rustc's is_snake_case: no upper case, and no `__` inside the name
	// (leading and trailing underscores are fine).
	if strings.ToLower(ident) == ident && !strings.Contains(strings.Trim(ident, "_"), "__") {
		return ""
	}
	return "#[allow(non_snake_case)] // spelled as the schema names it"
}

// checkFieldNames rejects what the list cannot prevent: two struct fields that
// give one Rust member (`self` is mangled to `self_`, which a field `self_`
// already is), and two constants of one enum or bitfield that give one Rust
// constant (they are upper-cased, so `a` and `A` are both `A`). rustc would
// report E0124/E0428 far from the schema. Union options are checkUnionNames'.
// Located: the error names the type and both names.
func checkFieldNames(s *ir.Schema) error {
	dup := func(owner, what string, names []string, ident func(string) string) error {
		seen := map[string]string{}
		for _, n := range names {
			id := strings.TrimPrefix(ident(n), "r#")
			if prev, ok := seen[id]; ok {
				return fmt.Errorf("rust: %s: %s %q and %q both generate %s; rename one", owner, what, prev, n, id)
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
			err = dup("struct "+key, "fields", fieldNames(nt.Fields), rustIdent)
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
		if err := dup("message "+m.Name, "fields", fieldNames(m.Fields), rustIdent); err != nil {
			return err
		}
	}
	return nil
}
