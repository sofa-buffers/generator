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
		// generator#484: the u64/i64 arm of checkArrayElem used to accept ANY
		// string, and checkInt64Range accepted an exact-valued float. Both now
		// run int64Verdict, so a field default and an array element agree.
		{
			name:   "array-of-u64 non-decimal element string",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [\"nonsense\"]}\n",
			expect: "element \"nonsense\" is not a decimal integer literal",
		},
		{
			name:   "array-of-u64 empty element string",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [\"\"]}\n",
			expect: "no leading zeros, no sign, no spacing",
		},
		{
			name:   "array-of-u64 quoted hex element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [\"0x10\"]}\n",
			expect: "write hex unquoted, e.g. 0x10, and YAML converts it",
		},
		{
			name:   "array-of-u64 quoted element past 64 bits",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [\"18446744073709551616\"]}\n",
			expect: "element 18446744073709551616 out of exact u64 range",
		},
		{
			// Refused for its SIGN, not its width: "out of exact u64 range" would
			// send the author looking for a smaller number instead of telling them
			// to drop the minus. checkMaskElem says the same for the same -1.
			name:   "array-of-u64 negative element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [-1]}\n",
			expect: "element -1 must not be negative (u64 is unsigned)",
		},
		{
			name:   "u64 default, unquoted negative",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u64, default: -1}\n",
			expect: "default -1 must not be negative (u64 is unsigned)",
		},
		{
			// The shape a sign check on the VALUE misses, as in the bitfield twin.
			name:   "array-of-u64 negative-zero element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [\"-0\"]}\n",
			expect: "element \"-0\" must not be negative (u64 is unsigned)",
		},
		{
			// Exact as a value, wrong as a spelling: "%v" writes it 1e+06.
			name:   "array-of-u64 exact-valued float element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [1000000.0]}\n",
			expect: "write it as the integer 1000000",
		},
		{
			name:   "array-of-u64 fractional element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [3.5]}\n",
			expect: "must be an integer, not a fractional number",
		},
		{
			// The one arm that used to drop the declared type from the message.
			name:   "array-of-u64 boolean element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u64, count: 2}, default: [true]}\n",
			expect: "element must be an integer or a quoted decimal integer string (u64)",
		},
		{
			name:   "i64 boolean default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: i64, default: true}\n",
			expect: "default must be an integer or a quoted decimal integer string (i64)",
		},
		{
			// A leading zero is what javac would have read as octal (#479); the
			// i64 hint says "-" is allowed, where the u64 one says no sign at all.
			name:   "array-of-i64 element with leading zeros",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: i64, count: 2}, default: [\"010\"]}\n",
			expect: "an optional leading \"-\", no leading zeros, no spacing",
		},
		{
			name:   "array-of-i64 element past 64 bits",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: i64, count: 2}, default: [\"9223372036854775808\"]}\n",
			expect: "element 9223372036854775808 out of exact i64 range",
		},
		// The same rule, at the FIELD level, where checkInt64Range now shares it.
		{
			name:   "u64 default spelled as a float",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u64, default: 1000000.0}\n",
			expect: "default 1e+06 is spelled as a decimal number; write it as the integer 1000000",
		},
		{
			name:   "u64 default \"-0\"",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u64, default: \"-0\"}\n",
			expect: "default \"-0\" must not be negative (u64 is unsigned)",
		},
		{
			name:   "i64 default with a quoted radix spelling",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: i64, default: \"0xff\"}\n",
			expect: "write hex unquoted, e.g. 0x10, and YAML converts it",
		},
		// The same spelling refusal on every OTHER integer default in the schema.
		// Before this change these four validated and rendered `1e+06` into a
		// 32-bit member, which rust, java, kotlin, csharp and cpp all reject;
		// leaving them out would have made the identical literal illegal for a u64
		// and legal-but-uncompilable for the u32 declared next to it.
		{
			name:   "u32 default spelled as a float",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u32, default: 1000000.0}\n",
			expect: "default 1e+06 is spelled as a decimal number; write it as the integer 1000000",
		},
		{
			name:   "array-of-u32 exact-valued float element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: u32, count: 2}, default: [1000000.0]}\n",
			expect: "element 1e+06 is spelled as a decimal number; write it as the integer 1000000",
		},
		{
			name:   "enum default spelled as a float",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: enum, enum: {LOW: 0, HIGH: 1000000}, default: 1000000.0}\n",
			expect: "enum default 1e+06 is spelled as a decimal number; write it as the integer 1000000",
		},
		{
			name:   "array-of-enum exact-valued float element",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: enum, count: 2, enum: {LOW: 0, HIGH: 1000000}}, default: [1000000.0]}\n",
			expect: "enum element 1e+06 is spelled as a decimal number; write it as the integer 1000000",
		},
		{
			// An i32 default BELOW the 1e6 threshold renders correctly today, and
			// is refused all the same: the threshold is invisible in the schema.
			name:   "i32 default spelled as a float below the exponent threshold",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: i32, default: 5.0}\n",
			expect: "default 5 is spelled as a decimal number; write it as the integer 5",
		},
		{
			// Unchanged by this rule: a fractional or oversize float keeps the
			// verdict it always had, since neither is an integer written wrong.
			name:   "u32 fractional default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u32, default: 3.5}\n",
			expect: "default for u32 must be an integer",
		},
		{
			// generator#497: an array element that is lowered to a WRAPPER
			// SEQUENCE takes no default at all. Before the fix all five of these
			// validated and then reached nothing: all eleven code backends
			// emitted no initializer, and only `docs` printed the written value.
			name:   "array-of-struct default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: struct, count: 2, fields: {x: {id: 0, type: u32}}}, default: [{x: 7}]}\n",
			expect: "an array of struct takes no default",
		},
		{
			name:   "array-of-union default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: union, count: 2, oneof: {p: {id: 0, type: u32}}}, default: [{p: 7}]}\n",
			expect: "an array of union takes no default",
		},
		{
			// The shape the issue was filed for. Both halves of the element rule
			// it dodges are enforced one level up: "nonsense" is refused for an
			// array<u64> element and 1000000.0 for every integer default (#484).
			name:   "array-of-array default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: array, count: 2, items: {type: u64, count: 2}}, default: [[\"nonsense\", 1000000.0]]}\n",
			expect: "an array of array takes no default",
		},
		{
			// The two leaf-looking members of the same set, and the half the
			// first draft of #497 missed. A `string`/`blob` element default LOOKS
			// like a scalar literal and was validated element-by-element, but it
			// is routed to a wrapper sequence exactly like a struct element, so
			// no code backend materializes it either: measured on all twelve
			// targets, `default: ["zzmarkerzz"]` on an array<string> appears only
			// in the docs HTML -- cpp constructs `std::vector<std::string> = {}`,
			// go `Strs []string` reset to `m.Strs[:0]`, rust `Vec::new()`.
			name:   "array-of-string default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: string, count: 2, maxlen: 8}, default: [\"a\", \"b\"]}\n",
			expect: "an array of string takes no default",
		},
		{
			name:   "array-of-blob default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: blob, count: 1, maxlen: 4}, default: [\"AAE=\"]}\n",
			expect: "an array of blob takes no default",
		},
		{
			// An EMPTY default is refused for the same reason as a populated
			// one: the key has no meaning for these element types, and a rule
			// that turned on the contents could not be stated in one sentence.
			name:   "array-of-struct empty default",
			src:    "version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: array, items: {type: struct, count: 2, fields: {x: {id: 0, type: u32}}}, default: []}\n",
			expect: "an array of struct takes no default",
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

// TestU64ArrayElementSpellingsAccepted is the other half of the #484 rule: every
// way an author reasonably writes a u64 array default still validates. A plain
// integer, an unquoted YAML hex integer (YAML has converted it to an integer
// before the validator sees it, which is why no hex STRING is needed), a value at
// 2^63 that a signed 64-bit type could not hold, and a quoted decimal string for
// the exact top of the unsigned range — the only spelling any reader of the same
// definition can carry past 2^63-1, since JSON has no unsigned 64-bit number.
//
// The unquoted 9223372036854775808 is the one shape #484 WIDENED rather than
// narrowed. yaml.v3 hands a literal past 2^63-1 over as a uint64, which
// int64Verdict reads; the element arm before it went through asInt — signed — and
// reported "element must be an integer". The FIELD default already accepted it,
// so this is the element arm catching up rather than a new spelling; the corpus
// carries it as arrays.yaml's `wide_unquoted` so a backend has to compile it.
func TestU64ArrayElementSpellingsAccepted(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: {id: 0, type: array, items: {type: u64, count: 5}, " +
		"default: [0, 1, 0x10, 9223372036854775808, \"18446744073709551615\"]}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("every legal u64 element spelling should validate, got:\n%s", errs.Error())
	}
}

// The signed twin. An i64 element may be negative in either spelling, and "-0" is
// a legal signed decimal (decIntRe accepts it, and so does the shipped schema's
// i64 pattern) — the sign is only refused where the type is unsigned.
func TestI64ArrayElementSpellingsAccepted(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: {id: 0, type: array, items: {type: i64, count: 5}, " +
		"default: [-1, 0, \"-0\", \"-9223372036854775808\", \"9223372036854775807\"]}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("every legal i64 element spelling should validate, got:\n%s", errs.Error())
	}
}

// TestWrapperArrayElementsWithoutDefaultStillValidate is the other half of the
// generator#497 rule, and the thing a too-broad version of it would break: an
// array whose element type is a string, a blob, a struct, a union or a nested
// array is a normal, supported shape — the corpus generates all five for every
// backend. Only the `default` KEY is refused, and only on those element types; a
// native-element array keeps its default.
func TestWrapperArrayElementsWithoutDefaultStillValidate(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      st: {id: 0, type: array, items: {type: string, count: 2, maxlen: 8}}\n" +
		"      bl: {id: 1, type: array, items: {type: blob, count: 2, maxlen: 8}}\n" +
		"      s: {id: 2, type: array, items: {type: struct, count: 2, fields: {x: {id: 0, type: u32}}}}\n" +
		"      u: {id: 3, type: array, items: {type: union, count: 2, oneof: {p: {id: 0, type: u32}}}}\n" +
		"      n: {id: 4, type: array, items: {type: array, count: 2, items: {type: u64, count: 2}}}\n" +
		"      leaf: {id: 5, type: array, items: {type: u64, count: 2}, default: [1, 2]}\n"
	if errs := validateString(t, src); errs != nil {
		t.Fatalf("wrapper array elements without a default should validate, got:\n%s", errs.Error())
	}
}

// TestEveryArrayElemKindRejectsABogusDefault pins the partition that IS this bug.
//
// Three lists have to stay in sync: `arrayElem` (every legal element kind),
// `wrapperArrayElem` (the kinds whose `default` is refused outright), and the
// `switch etyp` arms of checkArrayElem (the kinds whose default is checked
// element by element). They partition arrayElem today — but a kind added to
// arrayElem with NEITHER an arm nor a wrapperArrayElem entry falls through both,
// which is exactly how an `array<array<T>>` default came to be accepted by
// nothing and emitted by nothing (#497), and how `array<string>` and
// `array<blob>` were then missed by the first draft of the fix.
//
// So: for every kind in arrayElem, hand it a default that is wrong for a native
// element (a mapping is not an integer, a number, a boolean or a mask) and
// forbidden outright for a wrapper one, and require an error. A future kind with
// no arm and no entry accepts the mapping silently and fails here, naming itself.
// This is the third fix in this family (#477, #484, #497) whose first draft
// missed a type; nothing but a test over the list itself catches the fourth.
func TestEveryArrayElemKindRejectsABogusDefault(t *testing.T) {
	// An otherwise VALID `items` mapping per kind, so the only thing that can be
	// wrong about each schema below is the default.
	items := map[string]string{
		"u8": "{type: u8, count: 2}", "u16": "{type: u16, count: 2}",
		"u32": "{type: u32, count: 2}", "u64": "{type: u64, count: 2}",
		"i8": "{type: i8, count: 2}", "i16": "{type: i16, count: 2}",
		"i32": "{type: i32, count: 2}", "i64": "{type: i64, count: 2}",
		"fp32": "{type: fp32, count: 2}", "fp64": "{type: fp64, count: 2}",
		"boolean":  "{type: boolean, count: 2}",
		"string":   "{type: string, count: 2, maxlen: 8}",
		"blob":     "{type: blob, count: 2, maxlen: 8}",
		"enum":     "{type: enum, count: 2, enum: {A: 0, B: 1}}",
		"bitfield": "{type: bitfield, count: 2, bits: {a: {pos: 0}}}",
		"struct":   "{type: struct, count: 2, fields: {x: {id: 0, type: u32}}}",
		"union":    "{type: union, count: 2, oneof: {p: {id: 0, type: u32}}}",
		"array":    "{type: array, count: 2, items: {type: u64, count: 2}}",
	}
	for _, kind := range arrayElem {
		spec, ok := items[kind]
		if !ok {
			t.Fatalf("arrayElem gained %q with no items spelling in this test; add one, "+
				"and give the kind either a checkArrayElem arm or a wrapperArrayElem entry", kind)
		}
		src := "version: 1\nmessages:\n  M:\n    payload:\n" +
			"      a: {id: 0, type: array, items: " + spec + ", default: [{__bogus__: 1}]}\n"
		if errs := validateString(t, src); errs == nil {
			t.Errorf("array<%s> accepted a mapping as its default element: %q is in arrayElem "+
				"but has neither a checkArrayElem arm nor a wrapperArrayElem entry, so its "+
				"default is validated by nothing and (per #497) emitted by nothing", kind, kind)
		}
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
