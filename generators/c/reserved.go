package c

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// One reserved-name list for the C backend (generator#239). A C struct declares
// no members of its own -- no methods, no embedded base -- so the list is the
// keywords and the macros of the headers the generated code includes; what the
// backend adds beside a field is DERIVED from it (a
// sized blob's or native array's `<field>_len`), and checkMemberNames rejects a
// field landing on one. C has no identifier escape, so a name on the list is
// mangled with a trailing underscore; the wire and the JSON keys are unchanged.
// The macro namespace (option ids, flag masks, MAX_SIZE) is checkMacroNames'.

// cKeywords are the names a C struct member cannot take: the C99/C11 reserved
// words, the words C23 added (a user may build the generated code with
// -std=c23), and the <stdbool.h> macros the corelib headers pull in -- before
// C23 `bool`, `true` and `false` are macros, so a member `true` expands to `1`.
// C has no identifier escape, so a field with such a name is mangled (trailing
// underscore); the struct member and its descriptor entry use the mangled name,
// while the JSON harness keys (emitted elsewhere as string literals) keep the
// original name.
var cKeywords = map[string]bool{
	"auto": true, "break": true, "case": true, "char": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extern": true, "float": true, "for": true, "goto": true,
	"if": true, "inline": true, "int": true, "long": true, "register": true,
	"restrict": true, "return": true, "short": true, "signed": true, "sizeof": true,
	"static": true, "struct": true, "switch": true, "typedef": true, "union": true,
	"unsigned": true, "void": true, "volatile": true, "while": true,
	// C23 keywords; bool/true/false are <stdbool.h> macros before C23.
	"alignas": true, "alignof": true, "bool": true, "constexpr": true, "false": true,
	"nullptr": true, "static_assert": true, "thread_local": true, "true": true,
	"typeof": true, "typeof_unqual": true,
}

// cHeaderMacros are the object-like macros of the standard headers the
// generated code reaches: <stdint.h> and <stddef.h> (the generated header
// includes them), <stdbool.h>, <stdlib.h> and <string.h> (the corelib headers
// do), and <stdio.h> (the project harness does), in ISO C99 to C23. A member
// of one of these
// names is replaced by the preprocessor -- `uint8_t NULL;` becomes a cast
// expression, `uint8_t UINT8_MAX;` a number. Function-like macros (offsetof,
// INT8_C, assert) need no entry: they expand only when a `(` follows.
// The stdint limits are the full family, filled in by init below.
var cHeaderMacros = map[string]bool{
	"NULL": true, "SIZE_MAX": true, "PTRDIFF_MIN": true, "PTRDIFF_MAX": true,
	"INTPTR_MIN": true, "INTPTR_MAX": true, "UINTPTR_MAX": true,
	"INTMAX_MIN": true, "INTMAX_MAX": true, "UINTMAX_MAX": true,
	"SIG_ATOMIC_MIN": true, "SIG_ATOMIC_MAX": true,
	"WCHAR_MIN": true, "WCHAR_MAX": true, "WINT_MIN": true, "WINT_MAX": true,
	"EXIT_SUCCESS": true, "EXIT_FAILURE": true, "RAND_MAX": true, "MB_CUR_MAX": true,
	"EOF": true, "BUFSIZ": true, "SEEK_SET": true, "SEEK_CUR": true, "SEEK_END": true,
	"FILENAME_MAX": true, "FOPEN_MAX": true, "TMP_MAX": true, "L_tmpnam": true,
	"stdin": true, "stdout": true, "stderr": true, // macros on musl and newlib
	// C23 additions to those headers
	"INTPTR_WIDTH": true, "UINTPTR_WIDTH": true, "INTMAX_WIDTH": true, "UINTMAX_WIDTH": true,
	"PTRDIFF_WIDTH": true, "SIZE_WIDTH": true, "SIG_ATOMIC_WIDTH": true,
	"WCHAR_WIDTH": true, "WINT_WIDTH": true, "ONCE_FLAG_INIT": true,
	// What gcc's default GNU mode and glibc add over those headers: `linux` and
	// `unix` are predefined as 1, the rest arrive through <stdlib.h>/<stdio.h>.
	// Only the plausible field names; an ISO mode (-std=c99/c11/c23) has none.
	"linux": true, "unix": true, "BYTE_ORDER": true, "LITTLE_ENDIAN": true,
	"BIG_ENDIAN": true, "PDP_ENDIAN": true, "FD_SETSIZE": true, "NFDBITS": true,
	"WNOHANG": true, "WUNTRACED": true, "WEXITED": true, "WSTOPPED": true,
	"WCONTINUED": true, "WNOWAIT": true, "P_tmpdir": true, "L_ctermid": true,
}

func init() {
	for _, w := range []string{"8", "16", "32", "64"} {
		for _, n := range []string{"INT%s_MIN", "INT%s_MAX", "UINT%s_MAX",
			"INT_LEAST%s_MIN", "INT_LEAST%s_MAX", "UINT_LEAST%s_MAX",
			"INT_FAST%s_MIN", "INT_FAST%s_MAX", "UINT_FAST%s_MAX",
			"INT%s_WIDTH", "UINT%s_WIDTH", "INT_LEAST%s_WIDTH", "UINT_LEAST%s_WIDTH",
			"INT_FAST%s_WIDTH", "UINT_FAST%s_WIDTH"} {
			cHeaderMacros[fmt.Sprintf(n, w)] = true
		}
	}
}

// cIdent is the struct member a schema field is reached through: the schema
// name, with a trailing underscore where it is on the list.
func cIdent(name string) string {
	if cKeywords[name] || cHeaderMacros[name] {
		return name + "_"
	}
	return name
}

// hasLenCompanion reports whether a struct field is emitted with a sibling
// `<member>_len`: a sized blob, or a native array. A wrapper array is a holder
// struct carrying its own `.len`, and a union option keeps its length inside
// the option (unionLeafMember), so neither has a sibling.
func hasLenCompanion(f *ir.Field) bool {
	return f.Kind == ir.KindBlob || (f.Kind == ir.KindArray && !isHolderElem(f.Elem))
}

// checkMemberNames rejects a struct, message or union whose fields derive the
// same member: a mangled keyword landing on a field that already has the name
// (`int` and `int_`), or a field named after another field's length companion
// (`b_len` beside a blob `b`). Either would be a duplicate member, which the
// compiler reports far from its cause. It also rejects a member named after a
// macro this backend defines (checkMacroNames' set: an include guard, a
// MAX_SIZE, an option id, a flag mask): the preprocessor would replace the
// member. That one is not mangled -- the macro names follow symbol_prefix, so
// the field is renamed or the prefix changed. Located: the errors name the type
// and the fields, or the field and the macro's owner.
func (g *gen) checkMemberNames(s *ir.Schema) error {
	check := func(owner string, fields []*ir.Field, union bool) error {
		seen := map[string]string{}
		for _, f := range fields {
			names := []string{cIdent(f.Name)}
			if !union && hasLenCompanion(f) {
				names = append(names, cIdent(f.Name)+"_len")
			}
			for _, n := range names {
				if prev, ok := seen[n]; ok {
					return fmt.Errorf("c backend: %s: fields %q and %q both generate the member %s; rename one", owner, prev, f.Name, n)
				}
				if macro, ok := g.macros[n]; ok {
					return fmt.Errorf("c backend: %s: field %q generates the member %s, which is also the generated macro for %s; rename the field or change symbol_prefix", owner, f.Name, n, macro)
				}
				seen[n] = f.Name
			}
		}
		return nil
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatStruct, ir.CatUnion:
			kind := "struct "
			if nt.Category == ir.CatUnion {
				kind = "union "
			}
			if err := check(kind+key, nt.Fields, nt.Category == ir.CatUnion); err != nil {
				return err
			}
		}
	}
	for _, m := range s.Messages {
		if err := check("message "+m.Name, m.Fields, false); err != nil {
			return err
		}
	}
	return nil
}
