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
	path := filepath.Join("..", "..", "examples", "example.yaml")
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
			name:   "array default count mismatch",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: i32, count: 3}, default: [1, 2]}\n",
			expect: "must equal count 3",
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
