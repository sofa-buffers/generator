package rust

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// generateYAML is filesFromYAML with the generate error handed back.
func generateYAML(t *testing.T, src string, cfg map[string]any) ([]generator.File, error) {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "inline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return (&Backend{}).Generate(s, cfg)
}

// unionSrc covers every union site and option shape the backend lowers
// differently: a field union whose default_id is a STRUCT option that is not the
// first, scalar / string / compact-array / wrapper-array / struct options, a
// keyword option (`box`) and one on a reserved member (`which`); an array of
// unions whose default_id is its string option; a $defs union used with two
// default_ids (split into two types); and a single-option union.
const unionSrc = `
version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  m:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          box:   { id: 5, type: struct, fields: { z: { id: 0, type: u8 } } }
          which: { id: 6, type: boolean }
      v: { id: 1, type: array, items: { type: union, count: 4, default_id: 1, oneof: { i: { id: 0, type: i32 }, s: { id: 1, type: string, maxlen: 8 } } } }
      pf: { id: 2, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      po: { id: 3, type: union, oneof: { $ref: "#/$defs/union/Pick" } }
      one: { id: 4, type: union, oneof: { only: { id: 0, type: u8 } } }
`

var unionCfgs = []map[string]any{
	{"corelib": "rs"},
	{"corelib": "rs", "allow_dynamic": false},
	{"corelib": "rs-no-std"},
	{"corelib": "rs-no-std", "allow_dynamic": true},
}

func wantAll(t *testing.T, cfg map[string]any, got, why string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%v: %s, missing %q:\n%s", cfg, why, w, got)
		}
	}
}

// block returns the text from head up to the first line that is exactly "}".
func block(t *testing.T, m, head string) string {
	t.Helper()
	i := strings.Index(m, head)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", head, m)
	}
	rest := m[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+2]
	}
	return rest
}

// A union is a native enum: one tuple variant per option, renamed for serde to
// the option name (the externally tagged form IS the {"<option>": value} JSON
// form), and Default holds default_id at that option's own default -- here the
// struct option `pt`, which is not the first, so a Default that took the first
// variant would be caught.
func TestRustUnionIsAnEnum(t *testing.T) {
	for _, cfg := range unionCfgs {
		m := moduleFromYAML(t, unionSrc, cfg)
		rename := `    #[serde(rename = "num")]`
		if cfg["corelib"] == "rs-no-std" {
			rename = `    #[cfg_attr(feature = "serde", serde(rename = "num"))]`
		}
		wantAll(t, cfg, block(t, m, "pub enum M_U {"), "the union is not an enum with one renamed variant per option",
			rename+"\n    Num(u16),",
			"    Pt(M_U_Pt),",
			"    Box(M_U_Box),",
			"    Which(bool),",
		)
		if strings.Contains(block(t, m, "pub enum M_U {"), "pub num") || strings.Contains(m, "pub struct M_U {") {
			t.Errorf("%v: the union is still a record of every option", cfg)
		}
		wantAll(t, cfg, block(t, m, "impl Default for M_U {"), "Default must hold default_id (pt) at its default",
			"        Self::Pt(Default::default())")
		// the split $defs union: one type per default_id, each with its own Default
		wantAll(t, cfg, block(t, m, "impl Default for Pick__DefaultN {"), "the n-default split type", "        Self::N(6)")
		wantAll(t, cfg, block(t, m, "impl Default for Pick__DefaultT {"), "the t-default split type", "        Self::T(Default::default())")
		wantAll(t, cfg, m, "each site holds the split type of its own default_id",
			"    pub pf: Pick__DefaultT,", "    pub po: Pick__DefaultN,")
		// an array of unions gap-fills with the element type's Default: its D (s)
		wantAll(t, cfg, block(t, m, "impl Default for M_V {"), "the element union's Default is its default_id",
			"        Self::S(")
	}
}

// The accessor API: an id constant per option, which(), a getter returning
// Option<&T>, and a mutable accessor that SELECTS the option at its own default
// only when another one is held (select-if-not-held, never a reset of a held
// option). A keyword option is a raw identifier; an option on a reserved member
// takes the trailing underscore; a single-option union needs no fallback arm.
func TestRustUnionAccessors(t *testing.T) {
	for _, cfg := range unionCfgs {
		m := moduleFromYAML(t, unionSrc, cfg)
		imp := block(t, m, "impl M_U {")
		wantAll(t, cfg, imp, "the union API is incomplete",
			"    pub const NUM_ID: sofab::Id = 0;",
			"    pub const WHICH_ID: sofab::Id = 6;",
			"    pub fn which(&self) -> sofab::Id {",
			"            Self::Pt(_) => Self::PT_ID,",
			"    pub fn num(&self) -> Option<&u16> {",
			"        match self { Self::Num(v) => Some(v), _ => None }",
			"    pub fn pt_mut(&mut self) -> &mut M_U_Pt {",
			"        if !matches!(self, Self::Pt(_)) { *self = Self::Pt(Default::default()); }",
			"        match self { Self::Pt(v) => v, _ => unreachable!() }",
			"        if !matches!(self, Self::Num(_)) { *self = Self::Num(5); }",
			"    pub fn r#box(&self) -> Option<&M_U_Box> {",
			"    pub fn box_mut(&mut self) -> &mut M_U_Box {",
			"    pub fn which_(&self) -> Option<&bool> {",
			"    pub fn which_mut(&mut self) -> &mut bool {",
		)
		wantAll(t, cfg, block(t, m, "impl M_One {"), "a single-option union destructures irrefutably",
			"        let Self::Only(v) = self;\n        Some(v)",
			"        let Self::Only(v) = self;\n        v")
		// Footprint: the no_std select is out of line (one copy per option); std
		// leaves inlining to the compiler, and a single-option union's accessor is
		// a destructure with nothing to share.
		noinl := "    " + mutNoInline + "\n    pub fn pt_mut(&mut self)"
		if got := strings.Contains(imp, noinl); got != (cfg["corelib"] == "rs-no-std") {
			t.Errorf("%v: #[inline(never)] on pt_mut = %v, want it exactly on no_std:\n%s", cfg, got, imp)
		}
		if strings.Contains(block(t, m, "impl M_One {"), mutNoInline) {
			t.Errorf("%v: a single-option union's accessor is kept out of line", cfg)
		}
	}
}

// A getter or mutable accessor that would shadow a method every union has --
// derived (clone_from) or from a std blanket impl (to_owned, into, borrow,
// borrow_mut via the accessor of option `borrow`) -- takes the trailing
// underscore, so `u.to_owned()` stays the clone.
func TestRustUnionReservedMembers(t *testing.T) {
	src := "version: 1\nmessages:\n  m: { payload: { u: { id: 0, type: union, oneof: { to_owned: { id: 0, type: u8 }, borrow: { id: 1, type: u8 }, into: { id: 2, type: u8 }, clone_from: { id: 3, type: u8 }, type_id: { id: 4, type: u8 } } } } }\n"
	m := moduleFromYAML(t, src, map[string]any{"corelib": "rs"})
	wantAll(t, nil, block(t, m, "impl M_U {"), "a reserved member is shadowed",
		"    pub fn to_owned_(&self) -> Option<&u8> {",
		"    pub fn to_owned_mut(&mut self) -> &mut u8 {",
		"    pub fn borrow_(&self) -> Option<&u8> {",
		"    pub fn borrow__mut(&mut self) -> &mut u8 {",
		"    pub fn into_(&self) -> Option<&u8> {",
		"    pub fn clone_from_(&self) -> Option<&u8> {",
		"    pub fn type_id_(&self) -> Option<&u8> {")
	for _, bad := range []string{"pub fn to_owned(", "pub fn borrow(", "pub fn borrow_mut(", "pub fn into(", "pub fn clone_from(", "pub fn type_id("} {
		if strings.Contains(m, bad) {
			t.Errorf("a union accessor shadows a trait method: %q", bad)
		}
	}
}

// A member below a union option is reached through the option's accessor, so an
// arm that touches it more than once -- the completion store of a string or
// blob member, the clear + pre-size of a dynamic array member -- binds it once
// instead of calling the accessor per touch.
func TestRustUnionMemberBoundOnce(t *testing.T) {
	src := `
version: 1
messages:
  m:
    payload:
      u:
        id: 0
        type: union
        oneof:
          n:  { id: 0, type: u8 }
          st: { id: 1, type: struct, fields: { name: { id: 0, type: string, maxlen: 8 }, b: { id: 1, type: blob, maxlen: 4 }, xs: { id: 2, type: array, items: { type: i16, count: 3 } } } }
      plain: { id: 1, type: string, maxlen: 8 }
`
	for _, cfg := range unionCfgs {
		m := moduleFromYAML(t, src, cfg)
		dec := m[strings.Index(m, "mod _M__Decode {"):]
		if strings.Contains(sliceFn(t, m, "    fn string("), "st_mut().name.clear()") {
			t.Errorf("%v: the string completion store calls the accessor per touch", cfg)
		}
		if cfg["corelib"] == "rs-no-std" && cfg["allow_dynamic"] == nil {
			wantAll(t, cfg, dec, "a member store below an option is not bound once",
				"(_Loc::Root_u_st, 0) => { let _d = &mut self.m.u.st_mut().name; _d.clear(); let _ = _d.push_str(_s); if _d.len() != _s.len() { self.err = true; } }",
				"(_Loc::Root_u_st, 1) => { let _d = &mut self.m.u.st_mut().b; _d.clear(); let _ = _d.extend_from_slice(_b); if _d.len() != total { self.err = true; } }",
				// a union-free path keeps its spelled-out store
				"(_Loc::Root, 1) => { self.m.plain.clear(); let _ = self.m.plain.push_str(_s);")
		}
		if cfg["allow_dynamic"] == true {
			wantAll(t, cfg, sliceFn(t, m, "    fn array_begin("), "the clear + pre-size below an option is not bound once",
				"let _d = &mut self.m.u.st_mut().xs; _d.clear(); _d.reserve_exact(count) },")
		}
	}
}

// Encode (MESSAGE_SPEC §4.2): default_id keeps the ordinary guarded write of its
// kind (a struct D closes with the dropping end); every other option is written
// unconditionally, even at its own default -- a scalar as its value, a compact
// array as its count, a struct or wrapper-array option as a present frame
// (end_keep). An omitted non-default option would read back as default_id.
func TestRustUnionEncodeArms(t *testing.T) {
	for _, cfg := range unionCfgs {
		m := moduleFromYAML(t, unionSrc, cfg)
		ser := sliceFn(t, block(t, m, "impl M_U {"), "    pub fn serialize<")
		wantAll(t, cfg, ser, "an encode arm is wrong",
			"            Self::Num(v) => { os.write_unsigned(0, *v as sofab::Unsigned)?; }",
			"            Self::S(v) => { os.write_str(1, v)?; }",
			"            Self::Pt(v) => { os.write_sequence_begin_lazy(2)?; v.serialize(os)?; os.write_sequence_end()?; }",
			"            Self::Arr(v) => {\n                os.write_array_unsigned(3, v)?;\n            }",
			"                os.write_sequence_begin_lazy(4)?;",
			"                os.write_sequence_end_keep()?;\n            }",
			"            Self::Box(v) => { os.write_sequence_begin_lazy(5)?; v.serialize(os)?; os.write_sequence_end_keep()?; }",
			"            Self::Which(v) => { os.write_boolean(6, *v)?; }",
		)
		for _, bad := range []string{"*v != 5", "if *v {", "if !v.is_empty() { let _ = os.write_str(1"} {
			if strings.Contains(ser, bad) {
				t.Errorf("%v: a non-default option is written behind a ≠-default guard (%q):\n%s", cfg, bad, ser)
			}
		}
		// default_id at its default is omitted: the guard sits on D alone
		wantAll(t, cfg, sliceFn(t, block(t, m, "impl Pick__DefaultN {"), "    pub fn serialize<"), "D keeps its guard",
			"            Self::N(v) => { if *v != 6 { os.write_unsigned(0, *v as sofab::Unsigned)?; } }",
			"            Self::T(v) => { os.write_sequence_begin_lazy(1)?; v.serialize(os)?; os.write_sequence_end_keep()?; }")
		wantAll(t, cfg, sliceFn(t, block(t, m, "impl M_V {"), "    pub fn serialize<"), "the element union's D is guarded, the other forced",
			"            Self::I(v) => { os.write_signed(0, *v as sofab::Signed)?; }",
			"            Self::S(v) => { if !v.is_empty() { os.write_str(1, v)?; } }")
		// the union FIELD keeps its framing: a union at its default leaves the lazy
		// frame empty and the dropping end removes it
		wantAll(t, cfg, m, "the union field keeps its lazy frame",
			"os.write_sequence_begin_lazy(0)?; self.u.serialize(os)?; os.write_sequence_end()?;")
	}
}

// Decode (MESSAGE_SPEC §7.4.1): the switch sits at the hook reached only past the
// §7.3 gate of the option's kind, and it is select-if-not-held.
//   - scalar: the typed value callback assigns the variant after the width guard
//     -- switch and store in one statement;
//   - string: the completion store, after the payload accumulator has the whole
//     value -- never fixlen_begin, never per chunk;
//   - compact array: array_begin's kind-keyed arm, behind the over-count reject;
//   - struct / union / wrapper array: the sequence_begin arm, through <opt>_mut();
//   - every path below an option goes through <opt>_mut().
func TestRustUnionDecodeSwitch(t *testing.T) {
	for _, cfg := range unionCfgs {
		m := moduleFromYAML(t, unionSrc, cfg)
		unsigned := sliceFn(t, m, "    fn unsigned(")
		wantAll(t, cfg, unsigned, "a scalar option does not select by assigning its variant behind the width guard",
			"(_Loc::Root_u, 0) => { if value > 65535 { self.inv = true; return; }; self.m.u = M_U::Num(value as u16) },",
			"(_Loc::Root_u, 6) => self.m.u = M_U::Which(value != 0),",
			"self.m.u.arr_mut().push(value as u16)",
			"(_Loc::Root_u_box, 0) => { if value > 255 { self.inv = true; return; }; self.m.u.box_mut().z = value as u8 },",
			"self.m.pf = Pick__DefaultT::N(value as u16)",
			"self.m.pf.t_mut().k = value as u8",
		)
		signed := sliceFn(t, m, "    fn signed(")
		wantAll(t, cfg, signed, "a union element's scalar option does not assign the element's variant",
			"self.m.v[self._ix0] = M_V::I(value as i32)",
			"self.m.u.pt_mut().x = value as i32")
		str := sliceFn(t, m, "    fn string(")
		if cfg["corelib"] == "rs" && cfg["allow_dynamic"] == nil {
			wantAll(t, cfg, str, "a string option is not selected at the completion store",
				"(_Loc::Root_u, 1) => self.m.u = M_U::S(_s),",
				"(_Loc::Root_v_e, 1) => self.m.v[self._ix0] = M_V::S(_s),",
				"sofab::seq::place_elem(&mut (*self.m.u.strs_mut()), id,")
		}
		if cfg["corelib"] == "rs-no-std" && cfg["allow_dynamic"] == nil {
			wantAll(t, cfg, str, "a fixed-capacity string option is not selected at the completion store",
				"(_Loc::Root_u, 1) => { let _d = self.m.u.s_mut(); _d.clear(); let _ = _d.push_str(_s); if _d.len() != _s.len() { self.err = true; } }")
		}
		// ...and only there: the accumulator feed returns before the match while
		// chunks are outstanding, and nothing in front of it touches an option.
		feed := strings.Index(str, "self.acc.feed(")
		if feed < 0 {
			t.Fatalf("%v: no payload feed in string():\n%s", cfg, str)
		}
		if pre := str[:feed]; strings.Contains(pre, "_mut()") || strings.Contains(pre, "M_U::") {
			t.Errorf("%v: a union option is touched before the payload is complete:\n%s", cfg, pre)
		}
		if fb := sliceFn(t, m, "    fn fixlen_begin("); strings.Contains(fb, "_mut()") || strings.Contains(fb, "M_U::") {
			t.Errorf("%v: fixlen_begin selects a union option; it must only latch bounds:\n%s", cfg, fb)
		}
		ab := sliceFn(t, m, "    fn array_begin(")
		// A dynamic Vec is pre-sized after the clear, so the accessor's result is
		// bound once rather than called twice.
		arrArm := "(sofab::ArrayKind::Unsigned, _Loc::Root_u, 3) => { if count > 4 { self.inv = true; self.afill = 0; return; }; self.m.u.arr_mut().clear() },"
		if cfg["allow_dynamic"] == true || (cfg["corelib"] == "rs" && cfg["allow_dynamic"] == nil) {
			arrArm = "(sofab::ArrayKind::Unsigned, _Loc::Root_u, 3) => { if count > 4 { self.inv = true; self.afill = 0; return; }; let _d = self.m.u.arr_mut(); _d.clear(); _d.reserve_exact(count) },"
		}
		wantAll(t, cfg, ab, "a compact-array option is not selected in its kind-keyed arm behind the over-count reject", arrArm)
		sb := sliceFn(t, m, "    fn sequence_begin(")
		wantAll(t, cfg, sb, "a sequence option is not selected where its frame opens",
			"(_Loc::Root_u, 2) => { self.m.u.pt_mut(); _Loc::Root_u_pt },",
			"(_Loc::Root_u, 4) => { self.m.u.strs_mut().clear(); _Loc::Root_u_strs },",
			"(_Loc::Root_u, 5) => { self.m.u.box_mut(); _Loc::Root_u_box },",
			"(_Loc::Root_pf, 1) => { self.m.pf.t_mut(); _Loc::Root_pf_t },",
			// the union FIELD itself is entered, never reset: its scope continues (§7.4)
			"(_Loc::Root, 0) => _Loc::Root_u,")
		// never an unconditional re-emplace in a hook: a variant is only ever
		// assigned whole by a LEAF store, and a sequence option only through its
		// select-if-not-held accessor
		for _, bad := range []string{"= M_U::Pt(", "= M_U::Box(", "= M_U::Strs(", "= M_U::Arr(", "= Pick__DefaultT::T("} {
			if strings.Contains(m[strings.Index(m, "mod _M__Decode {"):], bad) {
				t.Errorf("%v: the decoder re-emplaces a sequence/array option (%q), which wipes what a repeated or resumed occurrence already decoded", cfg, bad)
			}
		}
	}
}

// Options whose derived members share a shape -- a getter `x_mut` beside the
// mutable accessor of `x`, a getter `A_ID` beside the id constant of `a`, the
// escaped getter of `borrow_mut` beside the accessor of `borrow` -- are kept
// apart by the member spelling, never refused.
func TestRustUnionMembersAreDistinct(t *testing.T) {
	src := "version: 1\nmessages:\n  m: { payload: { u: { id: 0, type: union, oneof: { x: { id: 0, type: u8 }, x_mut: { id: 1, type: u8 }, a: { id: 2, type: u8 }, A_ID: { id: 3, type: u8 }, borrow: { id: 4, type: u8 }, borrow_mut: { id: 5, type: u8 } } } } }\n"
	m := moduleFromYAML(t, src, map[string]any{"corelib": "rs"})
	wantAll(t, nil, block(t, m, "impl M_U {"), "the members are not the channel spellings",
		"    pub fn x(&self) -> Option<&u8> {",
		"    pub fn x_mut(&mut self) -> &mut u8 {",
		"    pub fn x_mut_(&self) -> Option<&u8> {",
		"    pub fn x_mut_mut(&mut self) -> &mut u8 {",
		"    pub const A_ID: sofab::Id = 2;",
		"    pub const A_ID_ID: sofab::Id = 3;",
		"    pub fn A_ID_(&self) -> Option<&u8> {",
		"    pub fn borrow_(&self) -> Option<&u8> {",
		"    pub fn borrow__mut(&mut self) -> &mut u8 {",
		"    pub fn borrow_mut_(&self) -> Option<&u8> {",
		"    pub fn borrow_mut_mut(&mut self) -> &mut u8 {",
	)
	// `self` cannot be a variant: PascalCase Self is a keyword.
	m = moduleFromYAML(t, "version: 1\nmessages:\n  m: { payload: { u: { id: 0, type: union, oneof: { self: { id: 0, type: u8 }, b: { id: 1, type: u8 } } } } }\n", map[string]any{"corelib": "rs"})
	wantAll(t, nil, m, "the `self` option is not mangled", "    Self_(u8),", "    pub fn self_(&self) -> Option<&u8> {", "    pub fn self_mut(&mut self) -> &mut u8 {")
}

// The union enum carries its one narrow allow, and only the union enum does.
func TestRustUnionEnumAllowSitsOnTheEnum(t *testing.T) {
	m := moduleFromYAML(t, unionSrc, map[string]any{"corelib": "rs"})
	n := strings.Count(m, unionEnumAllow+"\npub enum ") + strings.Count(m, unionEnumAllow+"\n"+typeNameAllow("M_U")+"\npub enum ")
	if e := strings.Count(m, unionEnumAllow); n != e || n != 5 {
		t.Errorf("union enum allow on %d enums of %d uses, want 5 of 5", n, e)
	}
}
