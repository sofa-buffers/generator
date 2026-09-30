package c

import (
	"fmt"
	"strings"
)

// One reserved-name list for the C backend (generator#239, #624), in two
// halves, one per namespace a schema name reaches:
//
//   - MEMBERS (cKeywords, cHeaderMacros, the corelib's SOFAB_ macros): a C
//     struct declares no members of its own -- no methods, no embedded base --
//     so what a field can meet is a keyword or an object-like macro of a header
//     the generated code includes. Such a field is mangled with a trailing
//     underscore; the wire and the JSON keys are unchanged. What the backend
//     adds beside a field is a ROLE of it (lenMember: `<member>__len`), which no
//     field name can spell, and every macro the backend defines has a "__" in it
//     (names.go), which no field name has either -- so no member needs refusing.
//   - TYPEDEFS (cReservedTypes): the one generated name without a "__" or a
//     leading "_" is a type's own typedef, `<prefix><path>_t`. It takes the
//     escape, a trailing underscore, where it would name a typedef the
//     generated code or the harness sees: the C library's and the corelib's.

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
// name, with a trailing underscore where it is on the list or in the corelib's
// macro namespace (SOFAB_H, SOFAB_MAX_DEPTH, and the SOFAB_DISABLE_* switches a
// build defines on the command line). A schema name never ends with "_", so the
// escape meets no other field.
func cIdent(name string) string {
	if cKeywords[name] || cHeaderMacros[name] || strings.HasPrefix(name, "SOFAB_") {
		return name + "_"
	}
	return name
}

// lenMember is the companion length member of a sized blob or native array
// field: a role of the member, which no field name can spell.
func lenMember(name string) string { return cIdent(name) + "__len" }

// cReservedTypes are the typedefs the generated code and the project harness
// see: <stdint.h>, <stddef.h>, <stdio.h>, <stdlib.h> and <string.h> (ISO C99
// to C23, and the POSIX names glibc's GNU mode adds through them), and the
// corelib's headers the generated header and the harness include
// (sofab/sofab.h, sofab/object.h and what they pull in, sofab_test_json.h).
// A type's typedef is `<prefix><path>_t`, so only a symbol_prefix reaches one
// (prefix `u` with a message `int8`, prefix `sofab_` with a message `ostream`);
// the default `message_` reaches none.
var cReservedTypes = map[string]bool{
	// <stddef.h>, <stdio.h>, <stdlib.h>, <wchar.h> via them, C11/C23 additions
	"size_t": true, "ptrdiff_t": true, "wchar_t": true, "max_align_t": true,
	"nullptr_t": true, "fpos_t": true, "div_t": true, "ldiv_t": true, "lldiv_t": true,
	"wint_t": true, "mbstate_t": true, "char8_t": true, "char16_t": true, "char32_t": true,
	"intptr_t": true, "uintptr_t": true, "intmax_t": true, "uintmax_t": true,
	"rsize_t": true, "errno_t": true,
	// POSIX typedefs glibc's GNU mode declares through <stdio.h>/<stdlib.h>
	"off_t": true, "off64_t": true, "ssize_t": true, "locale_t": true,
	"pid_t": true, "uid_t": true, "gid_t": true, "mode_t": true, "time_t": true,
	"clock_t": true, "dev_t": true, "ino_t": true, "nlink_t": true, "blksize_t": true,
	"blkcnt_t": true, "id_t": true, "key_t": true, "suseconds_t": true,
	"useconds_t": true, "timer_t": true, "clockid_t": true, "sigset_t": true,
	"register_t": true, "caddr_t": true, "daddr_t": true, "loff_t": true,
	"fsid_t": true, "quad_t": true, "u_quad_t": true, "fd_mask_t": true,
	// corelib-c-cpp
	"sofab_check_size_char_t": true, "sofab_fixlentype_t": true, "sofab_id_t": true,
	"sofab_istream_decoder_t": true, "sofab_istream_field_cb_t": true,
	"sofab_istream_t": true, "sofab_json_t": true, "sofab_json_type_t": true,
	"sofab_object_decoder_t": true, "sofab_object_descr_field_t": true,
	"sofab_object_descr_id_t": true, "sofab_object_descr_offset_t": true,
	"sofab_object_descr_size_t": true, "sofab_object_descr_t": true,
	"sofab_object_field_t": true, "sofab_ostream_flush_cb_t": true,
	"sofab_ostream_t": true, "sofab_ret_t": true, "sofab_signed_t": true,
	"sofab_test_vectors_result_t": true, "sofab_type_t": true, "sofab_unsigned_t": true,
}

func init() {
	// <stdint.h>: the exact, least and fast families, glibc's BSD aliases, and
	// the corelib's per-width size checks.
	for _, w := range []string{"8", "16", "32", "64"} {
		for _, n := range []string{"int%s_t", "uint%s_t", "int_least%s_t", "uint_least%s_t",
			"int_fast%s_t", "uint_fast%s_t", "u_int%s_t", "sofab_check_size_int%s_t",
			"sofab_check_size_uint%s_t"} {
			cReservedTypes[fmt.Sprintf(n, w)] = true
		}
	}
}
