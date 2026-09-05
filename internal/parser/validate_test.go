package parser

import (
	"path/filepath"
	"strings"
	"testing"
)

// validateString is a small helper: parse, resolve, validate.
func validateString(t *testing.T, src string) Errors {
	t.Helper()
	doc, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return Validate(resolved)
}

func TestExampleYAMLIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "messages", "example.yaml")
	doc, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if errs := Validate(resolved); errs != nil {
		t.Fatalf("example.yaml should validate, got:\n%s", errs.Error())
	}
}

func TestNegativeCases(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		expect string // substring of an expected error
	}{
		{
			name:   "duplicate ids in payload",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u8}\n      b: {id: 0, type: u8}\n",
			expect: "duplicate id 0",
		},
		{
			name:   "u8 default out of range",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u8, default: 300}\n",
			expect: "out of range for u8",
		},
		{
			name:   "enum default not in set",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: enum, enum: {RED: 0, BLUE: 2}, default: 5}\n",
			expect: "does not match any declared enum value",
		},
		{
			name:   "union default_id no match",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a:\n        id: 0\n        type: union\n        default_id: 9\n        oneof:\n          x: {id: 0, type: u8}\n",
			expect: "matches no option id",
		},
		{
			name:   "bitfield pos collision",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a:\n        id: 0\n        type: bitfield\n        bits:\n          x: {pos: 1}\n          y: {pos: 1}\n",
			expect: "duplicate pos 1",
		},
		{
			name:   "blob default longer than maxlen",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: blob, maxlen: 2, default: \"SGVsbG8=\"}\n",
			expect: "exceeds maxlen 2",
		},
		{
			name:   "string default longer than maxlen",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: string, maxlen: 3, default: \"hello\"}\n",
			expect: "exceeds maxlen 3",
		},
		{
			name:   "u64 oversize plain number rejected",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u64, default: 99999999999999999999}\n",
			expect: "default",
		},
		{
			name:   "array default exceeds count",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: i32, count: 3}, default: [1, 2, 3, 4]}\n",
			expect: "exceeds count 3",
		},
		{
			name:   "unknown top-level key",
			src:    "version: 1\nfoo: bar\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u8}\n",
			expect: "unknown top-level key",
		},
		{
			name:   "missing version",
			src:    "messages:\n  M:\n    payload:\n      a: {id: 0, type: u8}\n",
			expect: "missing required key \"version\"",
		},
		{
			name:   "unknown field key",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u8, bogus: 1}\n",
			expect: "unexpected key \"bogus\"",
		},
		{
			name:   "enum value out of signed 32-bit",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: enum, enum: {BIG: 3000000000}}\n",
			expect: "out of signed 32-bit range",
		},
		// Contract recursion into composite array elements (README §3–7):
		{
			name:   "array-of-struct duplicate id (uniqueIds)",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: struct, count: 2, fields: {x: {id: 0, type: i32}, y: {id: 0, type: i32}}}}\n",
			expect: "duplicate id 0",
		},
		{
			name:   "array-of-enum bad default (defaultMatchesEnum)",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: enum, count: 3, enum: {RED: 0, GREEN: 1}}, default: [5]}\n",
			expect: "does not match any declared enum value",
		},
		{
			name:   "array-of-union bad default_id (defaultIdMatchesUnion)",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: union, count: 2, default_id: 9, oneof: {x: {id: 0, type: i32}}}}\n",
			expect: "matches no option id",
		},
		{
			name:   "array-of-bitfield duplicate pos (uniquePositions)",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {A: {pos: 0}, B: {pos: 0}}}}\n",
			expect: "duplicate pos 0",
		},
		// An array is the only place a bitfield default is written as a NUMBER,
		// so it is the only place a mask can be misspelled (generator#482).
		{
			name:   "array-of-bitfield negative element mask",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [-1]}\n",
			expect: "must not be negative",
		},
		{
			// "-0" is the shape a sign check on the VALUE misses: big.Int reports
			// it as zero, so it has to be refused by spelling.
			name:   "array-of-bitfield negative-zero element mask",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [\"-0\"]}\n",
			expect: "element mask \"-0\" must not be negative",
		},
		{
			name:   "array-of-bitfield fractional element mask",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [3.5]}\n",
			expect: "must be an integer, not a fractional number",
		},
		{
			// An exact integer written as a number: the message has to name the
			// integer to write, because fmt's "%v" would spell it 1e+06.
			name:   "array-of-bitfield exact-valued float element mask",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [1000000.0]}\n",
			expect: "write it as the integer 1000000",
		},
		{
			// An integer literal past what yaml.v3 holds as an integer: it arrives
			// as a float64, and the author must be sent to the quoted form, not
			// told to write the integer they already wrote.
			name:   "array-of-bitfield unquoted element mask past 64 bits",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [18446744073709551616]}\n",
			expect: "is not an exact integer; quote it as a decimal string",
		},
		{
			// Decimal, but not a legal literal. The hex advice would be wrong here.
			name:   "array-of-bitfield element mask with leading zeros",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [\"0000000005\"]}\n",
			expect: "no leading zeros, no sign, no spacing",
		},
		{
			name:   "array-of-bitfield quoted hex element mask",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [\"0x10\"]}\n",
			expect: "is not a decimal integer literal",
		},
		{
			name:   "array-of-bitfield element mask past 64 bits",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, default: [\"18446744073709551616\"]}\n",
			expect: "does not fit the 64-bit backing",
		},
		{
			name:   "array-of-bitfield element mask past the declared backing width",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {A: {pos: 0}, C: {pos: 2}}}, default: [1000]}\n",
			expect: "does not fit the 8-bit backing of a bitfield whose highest declared pos is 2",
		},
		{
			name:   "array-of-bitfield boolean element mask",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {LOW: {pos: 0}}}, default: [true]}\n",
			expect: "element must be an integer mask or a quoted decimal integer string",
		},
		{
			name:   "struct array element missing fields",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: struct, count: 2}}\n",
			expect: "struct array element requires",
		},
		{
			name:   "dangling $ref",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/Nope'}}\n",
			expect: "", // handled at resolve time, see below
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "dangling $ref" {
				doc, _ := Parse([]byte(tc.src), "t.yaml")
				if _, err := doc.Resolve(); err == nil {
					t.Fatalf("expected resolve to fail on dangling $ref")
				}
				return
			}
			errs := validateString(t, tc.src)
			if errs == nil {
				t.Fatalf("expected an error containing %q, got none", tc.expect)
			}
			if !strings.Contains(errs.Error(), tc.expect) {
				t.Fatalf("expected error containing %q, got:\n%s", tc.expect, errs.Error())
			}
		})
	}
}

// TestBitfieldArrayElementSpellingsAccepted pins the other half of the rule the
// negative cases above cover: an array-of-bitfield default written the way an
// author reasonably would still validates. A plain integer, an unquoted YAML hex
// integer (YAML has already turned it into an integer by the time the validator
// sees it, which is why no hex STRING is needed), a mask with bit 63 set, and a
// quoted decimal string for the top of the unsigned range. The last element sets
// bits at positions no flag declares: legal, because nothing masks a bitfield down
// to its declared positions and the wire carries the whole unsigned value.
func TestBitfieldArrayElementSpellingsAccepted(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: {id: 0, type: array, items: {type: bitfield, count: 5, bits: {LOW: {pos: 0}, HIGH: {pos: 63}}}, " +
		"default: [0, 1, 0x10, 9223372036854775808, \"18446744073709551615\"]}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("every legal mask spelling should validate, got:\n%s", errs.Error())
	}
}

// A narrow bitfield is backed by the smallest unsigned type holding its highest
// declared pos, so 255 is the widest mask a two-flag bitfield can carry — and it
// must be accepted, undeclared bits and all.
func TestNarrowBitfieldArrayElementFillsItsBacking(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {A: {pos: 0}, C: {pos: 2}}}, default: [5, 255]}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("a mask filling the 8-bit backing should validate, got:\n%s", errs.Error())
	}
}

func TestUInt64MaxStringAccepted(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u64, default: \"18446744073709551615\"}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("u64 max as string should validate, got:\n%s", errs.Error())
	}
}

func TestRefResolutionSharesType(t *testing.T) {
	src := `version: 1
$defs:
  struct:
    Point:
      x: {id: 0, type: i32}
      y: {id: 1, type: i32}
messages:
  M:
    payload:
      p: {id: 0, type: struct, fields: {$ref: '#/$defs/struct/Point'}}
`
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("ref-using doc should validate, got:\n%s", errs.Error())
	}
}
