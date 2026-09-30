package parser

import (
	"strings"
	"testing"
)

// Every target derives identifiers from names by changing case and adding or
// removing underscores, and uses "__" and a trailing "_" as separators and
// escapes. The three naming rules keep names apart under all of that:
//
//   - a name has no "__" and no trailing "_";
//   - names in one scope differ in more than case and underscores;
//   - messages and every $defs category share that one top-level scope.
func TestNamingRulesReject(t *testing.T) {
	cases := []struct {
		name, src, loc, want string
	}{
		{
			name: "double underscore",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      a__b: {id: 0, type: u8}\n",
			loc:  "#/messages/m/payload/a__b", want: `no "__"`,
		},
		{
			name: "trailing underscore",
			src:  "version: 1\nmessages:\n  m_:\n    payload:\n      a: {id: 0, type: u8}\n",
			loc:  "#/messages/m_", want: `no trailing "_"`,
		},
		{
			name: "fields folding together",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      foo_bar: {id: 0, type: u8}\n      fooBar: {id: 1, type: u8}\n",
			loc:  "#/messages/m/payload/foo_bar", want: `"foo_bar" and "fooBar" (at #/messages/m/payload/fooBar)`,
		},
		{
			name: "fields differing only in case",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      x: {id: 0, type: u8}\n      X: {id: 1, type: u8}\n",
			loc:  "#/messages/m/payload/x", want: "differ only in case or underscores",
		},
		{
			name: "union options",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      u: {id: 0, type: union, oneof: {a_b: {id: 0, type: u8}, AB: {id: 1, type: u8}}}\n",
			loc:  "#/messages/m/payload/u/oneof/a_b", want: "one payload, struct or union",
		},
		{
			name: "inline struct fields",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      s: {id: 0, type: struct, fields: {ab: {id: 0, type: u8}, a_b: {id: 1, type: u8}}}\n",
			loc:  "#/messages/m/payload/s/fields/ab", want: "one payload, struct or union",
		},
		{
			name: "enum constants",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      e: {id: 0, type: enum, enum: {RED: 0, Red: 1}}\n",
			loc:  "#/messages/m/payload/e/enum/Red", want: "one enum",
		},
		{
			name: "bitfield flags",
			src:  "version: 1\nmessages:\n  m:\n    payload:\n      b: {id: 0, type: bitfield, bits: {on_off: {pos: 0}, onOff: {pos: 1}}}\n",
			loc:  "#/messages/m/payload/b/bits/on_off", want: "one bitfield",
		},
		{
			name: "messages",
			src:  "version: 1\nmessages:\n  Foo:\n    payload:\n      a: {id: 0, type: u8}\n  foo:\n    payload:\n      a: {id: 0, type: u8}\n",
			loc:  "#/messages/foo", want: "messages and $defs",
		},
		{
			name: "message against a $defs struct",
			src: "version: 1\n$defs:\n  struct:\n    point: {x: {id: 0, type: u8}}\n" +
				"messages:\n  Point:\n    payload:\n      p: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/point'}}\n",
			loc: "#/messages/Point", want: `"Point" and "point" (at #/$defs/struct/point)`,
		},
		{
			name: "one spelling in two $defs categories",
			src: "version: 1\n$defs:\n  struct:\n    P: {x: {id: 0, type: u8}}\n  enum:\n    P: {A: 0}\n" +
				"messages:\n  m:\n    payload:\n      p: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/P'}}\n",
			loc: "#/$defs/struct/P", want: `"P" and "P" (at #/$defs/enum/P)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateString(t, c.src)
			for _, e := range errs {
				if e.Loc == c.loc && strings.Contains(e.Msg, c.want) {
					return
				}
			}
			t.Fatalf("want an error at %s containing %q, got: %v", c.loc, c.want, errs)
		})
	}
}

// The rules are per scope: the same fold in different scopes is fine, because
// every target keeps the scopes apart (a type path joins its segments with a
// separator no name can produce).
func TestNamingRulesAcceptAcrossScopes(t *testing.T) {
	src := "version: 1\n" +
		"$defs:\n  struct:\n    Point: {x: {id: 0, type: u8}, X_1: {id: 1, type: u8}}\n" +
		"messages:\n" +
		"  m:\n    payload:\n      a: {id: 0, type: struct, fields: {b_c: {id: 0, type: u8}}}\n" +
		"      e: {id: 1, type: enum, enum: {A: 0, B: 1}}\n" +
		"  m_a:\n    payload:\n      bC: {id: 0, type: u8}\n" +
		"      e: {id: 1, type: enum, enum: {A: 0, B: 1}}\n" +
		"  struct_point:\n    payload:\n      p: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/Point'}}\n" +
		"      a_1b: {id: 1, type: u8}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("want valid, got: %v", errs)
	}
}
