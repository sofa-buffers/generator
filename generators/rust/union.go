package rust

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// A schema union is a native Rust enum with one tuple variant per option, so it
// holds exactly one option by construction (MESSAGE_SPEC §4.2). Its API:
//
//	enum variants            read by `match`; write by assigning a variant
//	<OPT>_ID                 the option's id, an associated const
//	which()                  the id of the option held
//	<opt>()                  Some(&value) when <opt> is held, else None
//	<opt>_mut()              select <opt> at its default unless it is held; return it
//	Default                  default_id at that option's own default
//	serialize(os)            one match arm per option
//
// Everything emitted here differs per schema: the variants and their types, the
// ids, one accessor pair and one encode arm per option. Selection is the
// language's own (assigning a variant); no generic tagged-union helper exists.
// serde's externally tagged form IS the JSON form ({"<option>": value}), so no
// JSON code is generated either.

// unionEnumAllow sits on every union enum. A union is as large as its largest
// option, and that IS the design: boxing the large one would allocate on every
// selection, and the heap-free profile has nothing to box into
// (clippy::large_enum_variant). Variant names are the option names, so a shared
// prefix or suffix is the schema's, not a naming slip
// (clippy::enum_variant_names). Both are clippy style/perf lints.
const unionEnumAllow = "#[allow(clippy::large_enum_variant, clippy::enum_variant_names)] // one variant per option, held inline"

// mutNoInline sits on every `<opt>_mut()` of the no_std (footprint) profile.
// The decoder reaches an option through it at the option's select point and at
// every store below it (a struct member, an array element), and under
// `opt-level = "z"` LLVM inlines each of those copies of the select -- the tag
// test plus the construction of the option's default. Out of line, the select
// exists once per option and each site is a call. Measured on thumbv6m
// (`.text`, tests/bench/lang/rust.sh's link recipe), rs-no-std / rs-no-std with
// allow_dynamic: -44 / -28 B on the check_union.py schema, -252 / -60 B on a
// schema of struct, string, blob, array and union options. Keeping only the
// select out of line (an inlined tag test calling it) measured +420 / +356 B
// and +324 / +372 B over this, and reaching a member below an option by
// `if let` instead of the accessor +100 / -32 B and +56 / +48 B. std keeps the
// inliner's choice: maxspeed is measured in instructions, not bytes.
const mutNoInline = "#[inline(never)] // footprint profile: one copy of the select per option"

// unionOpt is one option of a union type with every name the backend derives
// from it.
type unionOpt struct {
	f       *ir.Field
	variant string // enum variant (PascalCase)
	getter  string // <opt>(): Option<&T>
	mut     string // <opt>_mut(): &mut T, select-if-not-held
	idConst string // <OPT>_ID
	isD     bool   // the union's default option (default_id)
}

// The option members, spelled so that no two can meet (ARCHITECTURE §8, the
// member channel). A schema name has no `__` and no trailing `_`, and the names
// of one union have distinct folds, so:
//
//   - a variant is Pascal(opt): injective, and `Self` -- the one PascalCase
//     keyword -- takes the trailing `_` no Pascal name ends with;
//   - an id constant is UPPER(opt)_ID: injective (a shared upper-case spelling
//     is a shared fold), and a getter that could spell one is escaped below;
//   - a getter is the option's own name, escaped with a trailing `_` when it is
//     a keyword that cannot be raw, a member the type already has
//     (unionReserved), a name ending in `_mut` (the mutable accessor's shape) or
//     an all-upper-case name ending in `_ID` (the id constant's shape) -- so no
//     unescaped getter has the shape of either, and no escaped one (it ends in
//     `_`) has the shape of anything else;
//   - the mutable accessor is <opt>_mut; where that is a member the type already
//     has (as_mut, borrow_mut, deref_mut) it is <opt>__mut instead, the one
//     spelling in the impl that contains `__`.

func optVariant(name string) string {
	v := naming.Pascal(name)
	if v == "Self" { // the one PascalCase keyword
		v += "_"
	}
	return v
}

func optGetter(name string) string {
	if unionReserved[name] || rustNonRaw[name] || strings.HasSuffix(name, "_mut") ||
		(strings.HasSuffix(name, "_ID") && strings.ToUpper(name) == name) {
		return name + "_"
	}
	return rustIdent(name)
}

// optMut is the select-if-not-held accessor's name, built from the raw option
// name: `<keyword>_mut` is never a keyword, but it can be a member the type
// already has (option `borrow` -> `borrow_mut`), which takes `__mut`.
func optMut(name string) string {
	if unionReserved[name+"_mut"] {
		return name + "__mut"
	}
	return name + "_mut"
}

func optConst(name string) string { return strings.ToUpper(name) + "_ID" }

// unionOptions lists nt's options in IR order with their derived names.
func unionOptions(nt *ir.NamedType) []*unionOpt {
	out := make([]*unionOpt, 0, len(nt.Fields))
	for _, f := range nt.Fields {
		out = append(out, &unionOpt{
			f:       f,
			variant: optVariant(f.Name),
			getter:  optGetter(f.Name),
			mut:     optMut(f.Name),
			idConst: optConst(f.Name),
			isD:     nt.IsDefaultOption(f),
		})
	}
	return out
}

// emitUnion emits one union type: the enum, its Default (default_id at that
// option's own default) and the impl with the ids, which(), the accessors and
// serialize.
func (g *gen) emitUnion(f *rfile, name string, nt *ir.NamedType) {
	opts := unionOptions(nt)
	dopt := nt.DefaultOption()
	deprecated := fieldsHaveDeprecated(nt.Fields)
	floatDefault := g.fieldsHaveFloatDefault(nt.Fields)
	single := len(opts) == 1

	f.line("/// A union: holds exactly one of its options. `Default` holds `%s` at its", dopt.Name)
	f.line("/// own default.")
	if g.noStd {
		f.line("#[derive(Debug, Clone, PartialEq)]")
		f.line("#[cfg_attr(feature = \"serde\", derive(serde::Serialize, serde::Deserialize))]")
	} else {
		f.line("#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]")
	}
	f.line(unionEnumAllow)
	if a := typeNameAllow(name); a != "" {
		f.line("%s", a)
	}
	f.line("pub enum %s {", name)
	for _, o := range opts {
		f.emitDoc("    ", fieldDoc(o.f, generator.BoundNote(o.f, g.storage(o.f))))
		if o.f.Deprecated {
			f.line("    #[deprecated]")
		}
		if g.noStd {
			f.line("    #[cfg_attr(feature = \"serde\", serde(rename = %q))]", o.f.Name)
		} else {
			f.line("    #[serde(rename = %q)]", o.f.Name)
		}
		f.line("    %s(%s),", o.variant, g.rustType(o.f))
	}
	f.line("}")
	f.blank()

	allows := func() {
		if deprecated {
			f.line("#[allow(deprecated)]")
		}
		if floatDefault {
			f.line(approxConstantAllow)
		}
	}
	f.line(derivableImplsAllow)
	allows()
	f.line("impl Default for %s {", name)
	f.line("    fn default() -> Self {")
	for _, o := range opts {
		if o.isD {
			f.line("        Self::%s(%s)", o.variant, g.rustFieldDefault(o.f))
		}
	}
	f.line("    }")
	f.line("}")
	f.blank()

	allows()
	f.line("impl %s {", name)
	for _, o := range opts {
		f.line("    /// The id of option `%s`.", o.f.Name)
		f.line("    pub const %s: sofab::Id = %d;", o.idConst, o.f.ID)
	}
	f.line("    /// The id of the option this union holds.")
	f.line("    pub fn which(&self) -> sofab::Id {")
	f.line("        match self {")
	for _, o := range opts {
		f.line("            Self::%s(_) => Self::%s,", o.variant, o.idConst)
	}
	f.line("        }")
	f.line("    }")
	for _, o := range opts {
		typ := g.rustType(o.f)
		f.line("    /// `%s` when it is the option held.", o.f.Name)
		if a := nonSnakeAllow(o.getter); a != "" {
			f.line("    %s", a)
		}
		f.line("    pub fn %s(&self) -> Option<&%s> {", o.getter, typ)
		if single {
			f.line("        let Self::%s(v) = self;", o.variant)
			f.line("        Some(v)")
		} else {
			f.line("        match self { Self::%s(v) => Some(v), _ => None }", o.variant)
		}
		f.line("    }")
		f.line("    /// `%s`, selected at its own default first unless it is the option held", o.f.Name)
		f.line("    /// (the one held before is dropped). An option already held is kept as is.")
		if g.noStd && !single {
			f.line("    %s", mutNoInline)
		}
		if a := nonSnakeAllow(o.mut); a != "" {
			f.line("    %s", a)
		}
		f.line("    pub fn %s(&mut self) -> &mut %s {", o.mut, typ)
		if single {
			f.line("        let Self::%s(v) = self;", o.variant)
			f.line("        v")
		} else {
			f.line("        if !matches!(self, Self::%s(_)) { *self = Self::%s(%s); }", o.variant, o.variant, g.rustFieldDefault(o.f))
			f.line("        match self { Self::%s(v) => v, _ => unreachable!() }", o.variant)
		}
		f.line("    }")
	}
	// MESSAGE_SPEC §4.2: default_id is written like an ordinary field of its kind
	// (omitted at its default, so a union at its default leaves the frame empty
	// and the enclosing lazy frame drops it); any other option is written even at
	// its own default, a sequence-framed one as a present frame (end_keep), since
	// an omitted option would read back as default_id.
	f.line("    pub fn serialize<_F: sofab::Flush>(&self, os: &mut sofab::OStream<'_, _F>) {")
	f.line("        match self {")
	for _, o := range opts {
		g.emitUnionArm(f, o)
	}
	f.line("        }")
	f.line("    }")
	f.line("}")
	f.blank()
}

// emitUnionArm emits one serialize arm. `v` is a reference to the held option.
func (g *gen) emitUnionArm(f *rfile, o *unionOpt) {
	fld := o.f
	id := fld.ID
	head := fmt.Sprintf("            Self::%s(v) => ", o.variant)
	// guarded wraps the write in the option's ≠-default test when it is D; any
	// other option is written unconditionally.
	guarded := func(ne, write string) {
		if o.isD {
			f.line("%s{ if %s { %s } }", head, ne, write)
		} else {
			f.line("%s{ %s }", head, write)
		}
	}
	// The ≠-default test is an ordinary field's (rustLeafNe), so D and a struct
	// member of the same kind cannot drift apart.
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		guarded(g.rustLeafNe("*v", fld), fmt.Sprintf("let _ = os.write_unsigned(%d, *v as sofab::Unsigned);", id))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		guarded(g.rustLeafNe("*v", fld), fmt.Sprintf("let _ = os.write_signed(%d, *v as sofab::Signed);", id))
	case ir.KindBool:
		guarded(g.rustLeafNe("*v", fld), fmt.Sprintf("let _ = os.write_boolean(%d, *v);", id))
	case ir.KindFP32:
		guarded(g.rustLeafNe("*v", fld), fmt.Sprintf("let _ = os.write_fp32(%d, *v);", id))
	case ir.KindFP64:
		guarded(g.rustLeafNe("*v", fld), fmt.Sprintf("let _ = os.write_fp64(%d, *v);", id))
	case ir.KindString:
		// `v` is a reference: the test's method call derefs it (`*v.x()` would
		// deref the call's result).
		guarded(g.rustLeafNe("v", fld), fmt.Sprintf("let _ = os.write_str(%d, v);", id))
	case ir.KindBlob:
		guarded("!v.is_empty()", fmt.Sprintf("let _ = os.write_blob(%d, v);", id))
	case ir.KindStruct, ir.KindUnion:
		closer := "write_sequence_end_keep"
		if o.isD {
			closer = "write_sequence_end"
		}
		f.line("%s{ let _ = os.write_sequence_begin_lazy(%d); v.serialize(os); let _ = os.%s(); }", head, id, closer)
	case ir.KindArray:
		// depth 1: `v` is already a reference, like an element of an outer array.
		if isNativeArrayElem(fld.Elem) {
			f.line("%s{", head)
			ind := "                "
			if o.isD {
				f.line("                if !v.is_empty() {")
				ind += "    "
			}
			g.serializeArray(f, ind, fmt.Sprintf("%d", id), "v", fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, 1, "")
			if o.isD {
				f.line("                }")
			}
			f.line("            }")
			return
		}
		keep := keepAlways
		if o.isD {
			keep = ""
		}
		f.line("%s{", head)
		g.serializeArray(f, "                ", fmt.Sprintf("%d", id), "v", fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, 1, keep)
		f.line("            }")
	}
}

// keepAlways is emitSeqEnd's keepIf for a frame that must always survive: a
// wrapper-array option other than default_id, written even when empty.
const keepAlways = "true"
