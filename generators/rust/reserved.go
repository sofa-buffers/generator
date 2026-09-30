package rust

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// Type level (ARCHITECTURE §8, "Naming: conflict-free identifiers"). Every
// schema type is named naming.TypeIdent of its path, so two schema types never
// meet; what is left is a type landing on a name src/message.rs spends itself
// or reaches unqualified. The corelib (sofab::), serde, core/alloc/std and
// heapless are only ever spelled as paths, and a type identifier starts with an
// upper-case letter, so no crate path can be shadowed; the per-message decoder
// module declares only `_`-prefixed items, so its `use super::*` never loses a
// schema type to a local. What remains is this list: a type identifier equal
// to one of these names takes a trailing `_` (a TypeIdent never ends in one).
var rustTypeReserved = map[string]string{
	"Self":        "the one type-namespace keyword",
	"DecodeError": "the crate's own decode verdict enum",
	// The Rust 2021 prelude's type-namespace names. The generated code spells
	// several of them unqualified (Vec, String, Option, Result, Default, From),
	// and a module-level type of the same name would shadow the prelude for all
	// of it -- and for a user who glob-imports the module. The whole prelude is
	// listed rather than the handful used today, so a later emitter change
	// cannot reopen the hole.
	"Copy": "prelude", "Send": "prelude", "Sized": "prelude", "Sync": "prelude",
	"Unpin": "prelude", "Drop": "prelude", "Fn": "prelude", "FnMut": "prelude",
	"FnOnce": "prelude", "Box": "prelude", "ToOwned": "prelude", "Clone": "prelude",
	"PartialEq": "prelude", "PartialOrd": "prelude", "Eq": "prelude", "Ord": "prelude",
	"AsRef": "prelude", "AsMut": "prelude", "Into": "prelude", "From": "prelude",
	"Default": "prelude", "Iterator": "prelude", "Extend": "prelude",
	"IntoIterator": "prelude", "DoubleEndedIterator": "prelude",
	"ExactSizeIterator": "prelude", "Option": "prelude", "Result": "prelude",
	"String": "prelude", "ToString": "prelude", "Vec": "prelude",
	"TryFrom": "prelude", "TryInto": "prelude", "FromIterator": "prelude",
}

// typeIdent is the Rust type identifier of a schema path: naming.TypeIdent,
// escaped with a trailing `_` when it lands on rustTypeReserved.
func typeIdent(path []string) string {
	t := naming.TypeIdent(path)
	if _, ok := rustTypeReserved[t]; ok {
		return t + "_"
	}
	return t
}

// msgIdent is a message's Rust type identifier.
func msgIdent(m *ir.Message) string { return typeIdent([]string{m.Name}) }

// namedIdent is a named type's Rust identifier. A $defs union split by
// default_id is one type per default: the union's identifier plus the role
// `__Default<Option>`, built from the unescaped identifier (a role is never
// reserved: it contains `__`).
func namedIdent(nt *ir.NamedType) string {
	if nt.Variant != "" {
		return naming.TypeIdent(nt.Path) + "__Default" + naming.Pascal(nt.Variant)
	}
	return typeIdent(nt.Path)
}

// roleIdent is a generated companion of a message at module level: the
// unescaped type identifier, `__`, and a Pascal role word (M__Decoder).
func roleIdent(m *ir.Message, role string) string {
	return naming.TypeIdent([]string{m.Name}) + "__" + role
}

// privateIdent is a per-message name only generated code uses (the decoder's
// private module): `_` + the type identifier + `__` + a role word.
func privateIdent(m *ir.Message, role string) string {
	return "_" + roleIdent(m, role)
}

// typeNameAllow is the attribute a type whose identifier is not UpperCamelCase
// carries: an inline type's path (M_A), a split union's variant
// (Shape__DefaultPt), an escape (Vec_ is fine, M_A_ is not). rustc's
// non_camel_case_types lint refuses a `_` inside the name (leading and trailing
// ones are fine); "" when there is none.
func typeNameAllow(ident string) string {
	if !strings.Contains(strings.Trim(ident, "_"), "_") {
		return ""
	}
	return "#[allow(non_camel_case_types)] // spelled from the schema path"
}

// Member level: one reserved-name list for the Rust backend (generator#239). A struct field
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
