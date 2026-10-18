package model_test

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// An array field's `unit` and `decimals` describe its leaf element and must reach
// the IR exactly as they do on a scalar field: backends read Field.Unit and
// Field.Decimals without a kind check.
func TestArrayFieldCarriesUnitAndDecimals(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      v: {id: 0, type: array, unit: mV, decimals: 2, items: {type: array, items: {type: fp32, count: 4}}}\n" +
		"      s: {id: 1, type: fp64, unit: m, decimals: 7}\n"
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		unit     string
		decimals int
	}{"v": {"mV", 2}, "s": {"m", 7}}
	for _, f := range s.Messages[0].Fields {
		w := want[f.Name]
		if f.Unit != w.unit {
			t.Errorf("%s: Unit = %q, want %q", f.Name, f.Unit, w.unit)
		}
		if f.Decimals == nil || *f.Decimals != w.decimals {
			t.Errorf("%s: Decimals = %v, want %d", f.Name, f.Decimals, w.decimals)
		}
	}
}

// A union site's default_id travels on its TypeRef — for a field, an array's
// union element and a union element two array levels down alike — so analysis
// can bind it to the union type. An omitted default_id stays nil here.
func TestUnionDefaultIDReachesEveryTypeRef(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      f: {id: 0, type: union, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}\n" +
		"      e: {id: 1, type: array, items: {type: union, count: 2, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}}\n" +
		"      g: {id: 2, type: array, items: {type: array, count: 2, items: {type: union, count: 2, default_id: 1, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}}}\n" +
		"      o: {id: 3, type: union, oneof: {a: {id: 0, type: u8}, b: {id: 1, type: u8}}}\n"
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	fs := map[string]*ir.Field{}
	for _, f := range s.Messages[0].Fields {
		fs[f.Name] = f
	}
	for name, ref := range map[string]*ir.TypeRef{
		"field":          fs["f"].Ref,
		"element":        fs["e"].ElemRef,
		"nested element": fs["g"].ElemItems.ElemRef,
	} {
		if ref == nil || ref.DefaultID == nil || *ref.DefaultID != 1 {
			t.Errorf("%s: TypeRef.DefaultID = %v, want 1", name, ref)
		}
	}
	if d := fs["o"].Ref.DefaultID; d != nil {
		t.Errorf("omitted default_id: TypeRef.DefaultID = %d, want nil", *d)
	}
	if fs["f"].Default != int64(1) {
		t.Errorf("union field Default = %v, want the raw default_id 1 (read by the docs target)", fs["f"].Default)
	}
}

// Schema elements whose paths join to one string with "_" stay distinct
// types. An inline type's key used to be its owner's key and field name joined
// by "_", so `m.a_b` and `m_a.b`, or `$defs` struct `P`'s field `f` and a
// struct `P_f`, lowered to one key and one type silently replaced the other.
// Keys now join with ".", and every type carries its Path.
func TestSchemaPathsKeepTypesApart(t *testing.T) {
	type want struct {
		path  string // the referenced type's Path, dotted
		field string // the name of its first field, option or constant
	}
	cases := []struct {
		name, src string
		refs      map[string]want // "<message>.<field>" ("[]" for the element) -> its type
	}{
		{
			name: "inline paths",
			src: "version: 1\nmessages:\n" +
				"  m:\n    payload:\n      a_b: {id: 0, type: struct, fields: {y: {id: 0, type: u8}}}\n" +
				"  m_a:\n    payload:\n      b: {id: 0, type: struct, fields: {z: {id: 0, type: string, maxlen: 8}}}\n",
			refs: map[string]want{"m.a_b": {"m.a_b", "y"}, "m_a.b": {"m_a.b", "z"}},
		},
		{
			name: "inline in $defs against $defs",
			src: "version: 1\n$defs:\n  struct:\n" +
				"    P:\n      f: {id: 0, type: struct, fields: {y: {id: 0, type: u8}}}\n" +
				"    P_f:\n      q: {id: 0, type: fp64}\n" +
				"messages:\n  m:\n    payload:\n" +
				"      p: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/P'}}\n" +
				"      r: {id: 1, type: struct, fields: {$ref: '#/$defs/struct/P_f'}}\n",
			refs: map[string]want{"m.p": {"P", "f"}, "m.r": {"P_f", "q"}},
		},
		{
			name: "array element next to a field named like the old suffix",
			src: "version: 1\nmessages:\n  m:\n    payload:\n" +
				"      a: {id: 0, type: array, items: {type: struct, count: 2, fields: {y: {id: 0, type: u8}}}}\n" +
				"      a_elem: {id: 1, type: struct, fields: {z: {id: 0, type: u16}}}\n",
			refs: map[string]want{"m.a[]": {"m.a", "y"}, "m.a_elem": {"m.a_elem", "z"}},
		},
		{
			name: "nested inline against a field, across categories",
			src: "version: 1\nmessages:\n  m:\n    payload:\n" +
				"      a: {id: 0, type: struct, fields: {b: {id: 0, type: enum, enum: {X: 0, Y: 1}}}}\n" +
				"      a_b: {id: 1, type: union, oneof: {u: {id: 0, type: u8}, v: {id: 1, type: u16}}}\n",
			refs: map[string]want{"m.a": {"m.a", "b"}, "m.a_b": {"m.a_b", "u"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := parser.Parse([]byte(c.src), "t.yaml")
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := doc.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if errs := parser.Validate(resolved); errs != nil {
				t.Fatalf("validate: %v", errs)
			}
			s, err := model.Build(doc)
			if err != nil {
				t.Fatal(err)
			}
			refs := map[string]*ir.TypeRef{}
			for _, m := range s.Messages {
				for _, f := range m.Fields {
					if f.Ref != nil {
						refs[m.Name+"."+f.Name] = f.Ref
					}
					if f.ElemRef != nil {
						refs[m.Name+"."+f.Name+"[]"] = f.ElemRef
					}
				}
			}
			for site, w := range c.refs {
				r := refs[site]
				if r == nil {
					t.Fatalf("%s: no type reference", site)
				}
				nt := s.Named[r.Key]
				if nt == nil {
					t.Fatalf("%s: key %q is not in the graph", site, r.Key)
				}
				if got := strings.Join(nt.Path, "."); got != w.path {
					t.Errorf("%s: Path = %q, want %q", site, got, w.path)
				}
				first := ""
				switch {
				case len(nt.Fields) > 0:
					first = nt.Fields[0].Name
				case len(nt.Consts) > 0:
					first = nt.Consts[0].Name
				}
				if first != w.field {
					t.Errorf("%s: first member = %q, want %q", site, first, w.field)
				}
			}
			if len(s.Named) != len(s.NamedOrder) {
				t.Errorf("Named has %d types, NamedOrder %d", len(s.Named), len(s.NamedOrder))
			}
		})
	}
}

func build(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("validate: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A `$ref` may stand for a whole payload (the struct's fields become the
// message's) and for a single field (a copy of that field definition, id
// included, under the referencing key's name).
func TestScopeAndFieldRefsExpandInPlace(t *testing.T) {
	s := build(t, `version: 1
$defs:
  struct:
    Motor:
      temperature: { id: 0, type: fp32, unit: degC }
      rpm:         { id: 1, type: u32, unit: rpm }
      enabled:     { id: 2, type: boolean }
      pos:         { id: 3, type: struct, fields: { a: { id: 0, type: u8 } } }
    Alias:
      power: { $ref: '#/$defs/struct/Motor/enabled' }
messages:
  GetMotor:
    payload:
      $ref: '#/$defs/struct/Motor'
  SetMotorEnabled:
    payload:
      on:  { $ref: '#/$defs/struct/Motor/enabled' }
  Other:
    payload:
      speed: { $ref: '#/$defs/struct/Motor/rpm' }
      via:   { $ref: '#/$defs/struct/Alias/power' }
`)
	type fl struct {
		name string
		id   uint64
		kind ir.Kind
	}
	got := func(m *ir.Message) (out []fl) {
		for _, f := range m.Fields {
			out = append(out, fl{f.Name, uint64(f.ID), f.Kind})
		}
		return
	}
	var get, set *ir.Message
	for _, m := range s.Messages {
		switch m.Name {
		case "GetMotor":
			get = m
		case "SetMotorEnabled":
			set = m
		}
	}
	if g := got(get); len(g) != 4 || g[0] != (fl{"temperature", 0, ir.KindFP32}) || g[3].kind != ir.KindStruct {
		t.Fatalf("GetMotor fields = %+v", g)
	}
	if get.Fields[0].Unit != "degC" {
		t.Errorf("unit lost: %q", get.Fields[0].Unit)
	}
	if g := got(set); len(g) != 1 || g[0] != (fl{"on", 2, ir.KindBool}) {
		t.Fatalf("SetMotorEnabled fields = %+v", g)
	}
	// A ref to a field that is itself a ref follows the chain.
	for _, m := range s.Messages {
		if m.Name == "Other" {
			if g := got(m); len(g) != 2 || g[0] != (fl{"speed", 1, ir.KindU32}) || g[1] != (fl{"via", 2, ir.KindBool}) {
				t.Fatalf("Other fields = %+v", g)
			}
		}
	}
}

// buildErr runs the whole hard gate over src and returns the model's error.
// The parser's own stages must PASS: the point of the category check is that a
// mismatched $ref gets through validation (which runs on the dereferenced tree,
// where a struct's field map and a union's option map are the same shape), so a
// test whose source were refused earlier would prove nothing about it.
func buildErr(t *testing.T, src string) string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatalf("resolve (the model never sees this source): %v", err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("validate (the model never sees this source): %v", errs)
	}
	_, err = model.Build(doc)
	if err == nil {
		t.Fatal("want a model error, got none")
	}
	return err.Error()
}

// A $ref into the wrong $defs group is refused, at the site that declares the
// category and naming both halves (generator#776).
//
// struct and union are the pair that used to get through: the generated field
// silently took the TARGET's category, so a field declared `union` became a
// plain struct field -- a different wire contract (MESSAGE_SPEC §2, §4.2), on
// every target, with nothing said.
func TestRefToWrongDefsCategoryIsRefused(t *testing.T) {
	const defs = `version: 1
$defs:
  struct:
    Vec2: { x: { id: 0, type: i32 }, y: { id: 1, type: i32 } }
  union:
    Shape: { c: { id: 0, type: u8 }, s: { id: 1, type: u16 } }
  enum:
    Mode: { A: 0, B: 1 }
  bitfield:
    Flags: { f: { pos: 0 } }
messages:
  probe:
    payload:
`
	for name, tc := range map[string]struct{ field, want string }{
		"union body to struct": {
			"      u: { id: 1, type: union, oneof: { $ref: '#/$defs/struct/Vec2' } }\n",
			`#/messages/probe/payload/u/oneof: $ref "#/$defs/struct/Vec2" names a $defs/struct, but the field is declared "union"`,
		},
		"struct body to union": {
			"      s: { id: 1, type: struct, fields: { $ref: '#/$defs/union/Shape' } }\n",
			`#/messages/probe/payload/s/fields: $ref "#/$defs/union/Shape" names a $defs/union, but the field is declared "struct"`,
		},
		"array element to the wrong category": {
			"      a: { id: 1, type: array, items: { type: union, count: 2, oneof: { $ref: '#/$defs/struct/Vec2' } } }\n",
			`#/messages/probe/payload/a/items/oneof: $ref "#/$defs/struct/Vec2" names a $defs/struct, but the field is declared "union"`,
		},
	} {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if got := buildErr(t, defs+tc.field); !strings.Contains(got, tc.want) {
				t.Errorf("error does not name the mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// The two scope sites: a whole payload and a whole $defs entry may be a $ref,
// and both are id scopes of members -- which is what a struct is.
func TestRefForAWholeScopeMustBeAStruct(t *testing.T) {
	payload := buildErr(t, "version: 1\n$defs:\n  union:\n    Shape: { c: { id: 0, type: u8 } }\n"+
		"messages:\n  probe:\n    payload:\n      $ref: '#/$defs/union/Shape'\n")
	if want := `#/messages/probe/payload: $ref "#/$defs/union/Shape" names a $defs/union, but a message payload is a struct scope`; !strings.Contains(payload, want) {
		t.Errorf("payload:\n got: %s\nwant: %s", payload, want)
	}
	entry := buildErr(t, "version: 1\n$defs:\n  struct:\n    Alias: { $ref: '#/$defs/union/Shape' }\n"+
		"  union:\n    Shape: { c: { id: 0, type: u8 } }\n"+
		"messages:\n  probe:\n    payload:\n      x: { id: 0, type: u8 }\n")
	if want := `#/$defs/struct/Alias: $ref "#/$defs/union/Shape" names a $defs/union, but this $defs entry is a struct`; !strings.Contains(entry, want) {
		t.Errorf("$defs entry:\n got: %s\nwant: %s", entry, want)
	}
}

// The enum and bitfield halves of the same mismatch never reach the model: an
// enum body and a bitfield body are structurally disjoint (`value` against
// `pos`), and a struct's field map is neither, so the validator refuses them on
// the dereferenced tree. That is why only struct<->union got through, and it is
// what makes the model's check for the other two categories belt and braces
// rather than the only guard. Pinned so a validator change that loosened those
// shapes would show up here as the model error taking over.
func TestEnumBitfieldRefMismatchIsRefusedEarlier(t *testing.T) {
	for name, field := range map[string]string{
		"enum body to bitfield": "      e: { id: 1, type: enum, enum: { $ref: '#/$defs/bitfield/Flags' } }\n",
		"bitfield body to enum": "      b: { id: 1, type: bitfield, bits: { $ref: '#/$defs/enum/Mode' } }\n",
		"enum body to struct":   "      e: { id: 1, type: enum, enum: { $ref: '#/$defs/struct/Vec2' } }\n",
	} {
		field := field
		t.Run(name, func(t *testing.T) {
			src := "version: 1\n$defs:\n" +
				"  struct:\n    Vec2: { x: { id: 0, type: i32 } }\n" +
				"  enum:\n    Mode: { A: 0, B: 1 }\n" +
				"  bitfield:\n    Flags: { f: { pos: 0 } }\n" +
				"messages:\n  probe:\n    payload:\n" + field
			doc, err := parser.Parse([]byte(src), "t.yaml")
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := doc.Resolve()
			if err != nil {
				return // refused even earlier; still refused
			}
			if errs := parser.Validate(resolved); errs == nil {
				t.Fatal("the validator accepted a mismatched enum/bitfield $ref")
			}
		})
	}
}

// What the check must NOT refuse: the same-category forms the generator has
// always supported -- a payload or a composite member pointing at its own
// category, and a $ref to a single FIELD of a $defs entry, which is a field
// definition and carries no category of its own (that path never reaches the
// check, and this pins it).
func TestRefOfTheRightCategoryStillBuilds(t *testing.T) {
	s := build(t, `version: 1
$defs:
  struct:
    Motor: { rpm: { id: 0, type: u32 }, on: { id: 1, type: boolean } }
  union:
    Shape: { c: { id: 0, type: u8 }, s: { id: 1, type: u16 } }
  enum:
    Mode: { A: 0, B: 1 }
  bitfield:
    Flags: { f: { pos: 0 } }
messages:
  whole:
    payload:
      $ref: '#/$defs/struct/Motor'
  members:
    payload:
      st: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Motor' } }
      un: { id: 1, type: union, default_id: 0, oneof: { $ref: '#/$defs/union/Shape' } }
      en: { id: 2, type: enum, enum: { $ref: '#/$defs/enum/Mode' } }
      bf: { id: 3, type: bitfield, bits: { $ref: '#/$defs/bitfield/Flags' } }
  copies:
    payload:
      one: { $ref: '#/$defs/struct/Motor/rpm' }
      opt: { $ref: '#/$defs/union/Shape/s' }
`)
	kinds := map[string]ir.Kind{}
	for _, m := range s.Messages {
		for _, f := range m.Fields {
			kinds[m.Name+"."+f.Name] = f.Kind
		}
	}
	for name, want := range map[string]ir.Kind{
		"members.st": ir.KindStruct, "members.un": ir.KindUnion,
		"members.en": ir.KindEnum, "members.bf": ir.KindBitfield,
		// A payload $ref expands the struct's own members in place.
		"whole.rpm": ir.KindU32, "whole.on": ir.KindBool,
		// A field $ref copies the field, so these take the referenced field's
		// own type and carry no category of their own.
		"copies.one": ir.KindU32, "copies.opt": ir.KindU16,
	} {
		if kinds[name] != want {
			t.Errorf("%s: kind = %v, want %v", name, kinds[name], want)
		}
	}
}

// A circular field ref is an error, not a hang.
func TestCircularFieldRefIsRefused(t *testing.T) {
	doc, err := parser.Parse([]byte(`version: 1
$defs:
  struct:
    A:
      x: { $ref: '#/$defs/struct/A/x' }
messages:
  M:
    payload:
      y: { $ref: '#/$defs/struct/A/x' }
`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Resolve(); err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("want circular $ref error, got %v", err)
	}
}
