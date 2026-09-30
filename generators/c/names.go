package c

import (
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// Every identifier this backend emits is built here, from the channels of
// docs/ARCHITECTURE.md §8 ("Naming: conflict-free identifiers"), in the spelling
// the contract gives C, which keeps schema names verbatim:
//
//   - a schema PATH is naming.CPath (segments joined with "___") behind the
//     symbol_prefix — the BASE of a type: message_m, message_m___a,
//     message_point;
//   - a split union variant adds "__default_" + the variant to its base:
//     message_shape__default_pt;
//   - a ROLE follows "__": message_m__init, message_m__decoder_t,
//     message_m___arr__elems_t (the element holder of array field m.arr);
//   - a PRIVATE name leads with "_": _message_m__fields, _message_m__to_json;
//   - a MACRO upper-cases the base and the role: MESSAGE_M__H,
//     MESSAGE_M__MAX_SIZE; a bitfield flag and a union option extend the PATH
//     instead: MESSAGE_M___FLAGS___ON, MESSAGE_M___U___PT__ID;
//   - a typedef is the base + "_t", escaped with a trailing "_" where it would
//     name a typedef the generated code or the harness sees (cReservedTypes).
//
// A schema name has no "__", starts with a letter and never ends with "_", so
// the run length of underscores tells a path (3) from a role (2) from a name
// (1), and every identifier above parses back to one path and one role. The
// fold rule keeps names of one scope apart once upper-cased, which is what the
// macros need.
//
// What the guarantee assumes of symbol_prefix (user config, not schema): it
// starts with a letter. Then every name above but the plain typedef contains
// "__" or starts with "_" + a letter, and neither the corelib, the C standard
// library nor the harness declares such a name; the typedef is checked against
// the list. A prefix may coincide with the corelib's own (`sofab_`): a message
// `ostream` then takes sofab_ostream_t_, and its functions (sofab_ostream__init)
// never meet the corelib's (sofab_ostream_init).

// base is the C spelling of a schema path: symbol_prefix + CPath.
func (g *gen) base(path []string) string { return g.prefix + naming.CPath(path) }

// msgBase is a message's base: its path is its name.
func (g *gen) msgBase(m *ir.Message) string { return g.base([]string{m.Name}) }

// ntBase is a named type's base. A $defs union split by default_id shares its
// path with its sibling variants, so a variant adds the role
// "default_<option>" — a role word no other role starts with.
func (g *gen) ntBase(nt *ir.NamedType) string {
	b := g.base(nt.Path)
	if nt.Variant != "" {
		b += "__default_" + nt.Variant
	}
	return b
}

// fieldPath is the schema path of field f of the scope at path: the path of an
// inline type f declares, and the base its element holder is a role of.
func fieldPath(path []string, name string) []string {
	out := make([]string, len(path), len(path)+1)
	copy(out, path)
	return append(out, name)
}

// holderBase is the base of the element holder of the array field at fpath.
func (g *gen) holderBase(fpath []string) string { return g.base(fpath) + "__elems" }

// typeName is the typedef of a base (a type's own base or a role on it).
func (g *gen) typeName(base string) string {
	t := base + "_t"
	if cReservedTypes[t] || g.harnessNames[t] {
		return t + "_"
	}
	return t
}

// role is a namespace-level companion of base: a function, a descriptor.
func role(base, word string) string { return base + "__" + word }

// private is a generated name only generated code uses (static tables, the
// harness's JSON functions).
func private(base, word string) string { return "_" + base + "__" + word }

// macro is a role macro of base, upper-cased: MESSAGE_M__H.
func macro(base, word string) string { return strings.ToUpper(base) + "__" + word }

// descrSym is the descriptor of the object at base. It is public: the header
// declares it for the sofab_object_* API and for selecting a sequence option of
// a union at its default.
func descrSym(base string) string { return role(base, "descr") }

func fieldsSym(base string) string   { return private(base, "fields") }
func nestedSym(base string) string   { return private(base, "nested") }
func defaultsSym(base string) string { return private(base, "defaults") }

// headerFile and sourceFile are a message's files: the lower-cased name
// (injective, because two message names never share a fold) and the fixed
// marker "_sofab", which keeps them off every header the build includes by
// name — a message `stdint` would otherwise shadow <stdint.h> through
// -Igenerated — and off the names Windows reserves (`con.h` is the console
// device, `con_sofab.h` is a file).
func headerFile(m *ir.Message) string { return strings.ToLower(m.Name) + "_sofab.h" }
func sourceFile(m *ir.Message) string { return strings.ToLower(m.Name) + "_sofab.c" }
