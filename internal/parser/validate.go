package parser

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Error is a single, located validation failure (PLAN §1: "a clear, located
// error"). Loc is a logical JSON-pointer-ish path into the document.
type Error struct {
	Loc string
	Msg string
}

func (e Error) Error() string {
	if e.Loc == "" {
		return e.Msg
	}
	return e.Loc + ": " + e.Msg
}

// Errors is the all-at-once report (Ajv allErrors:true, README §9). It is
// sorted by location for deterministic output.
type Errors []Error

func (es Errors) Error() string {
	if len(es) == 0 {
		return "no errors"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation error(s):", len(es))
	for _, e := range es {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

var nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// numericTypes are the scalar wire primitives usable as array elements too.
var scalarRanges = map[string][2]int64{
	"u8":  {0, 255},
	"u16": {0, 65535},
	"u32": {0, 4294967295},
	"i8":  {-128, 127},
	"i16": {-32768, 32767},
	"i32": {-2147483648, 2147483647},
}

const fp32Max = 3.4028235e+38

// Validate runs the full hard-gate validation over the RESOLVED document
// (caller passes the output of Document.Resolve). It returns nil on success or
// a non-empty Errors collecting every problem found.
func Validate(resolved any) Errors {
	v := &validator{}
	root, ok := resolved.(map[string]any)
	if !ok {
		v.add("#", "document root must be a mapping")
		return v.errs
	}
	v.validateRoot(root)
	sort.SliceStable(v.errs, func(i, j int) bool { return v.errs[i].Loc < v.errs[j].Loc })
	if len(v.errs) == 0 {
		return nil
	}
	return v.errs
}

type validator struct{ errs Errors }

func (v *validator) add(loc, format string, args ...any) {
	v.errs = append(v.errs, Error{Loc: loc, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) validateRoot(root map[string]any) {
	// closed object: only version, $defs, messages
	for k := range root {
		switch k {
		case "version", "$defs", "messages":
		default:
			v.add("#", "unknown top-level key %q (allowed: version, $defs, messages)", k)
		}
	}
	// version required, const 1
	ver, ok := root["version"]
	if !ok {
		v.add("#", "missing required key \"version\"")
	} else if n, ok := asInt(ver); !ok || n != 1 {
		v.add("#/version", "version must be the integer 1")
	}
	// anyOf: $defs or messages present
	_, hasDefs := root["$defs"]
	_, hasMsgs := root["messages"]
	if !hasDefs && !hasMsgs {
		v.add("#", "document must contain \"$defs\", \"messages\", or both")
	}
	if hasDefs {
		v.validateDefs(root["$defs"], "#/$defs")
	}
	if hasMsgs {
		v.validateMessages(root["messages"], "#/messages")
	}
}

func (v *validator) validateDefs(node any, loc string) {
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "$defs must be a mapping")
		return
	}
	for k, val := range m {
		kloc := loc + "/" + k
		switch k {
		case "struct", "union":
			for name, def := range asMapOf(v, val, kloc) {
				dloc := kloc + "/" + name
				v.checkName(name, dloc)
				// struct/union $defs are id-scopes of fields
				v.validateIDScope(def, dloc)
			}
		case "enum":
			for name, def := range asMapOf(v, val, kloc) {
				dloc := kloc + "/" + name
				v.checkName(name, dloc)
				v.validateEnumDef(def, dloc)
			}
		case "bitfield":
			for name, def := range asMapOf(v, val, kloc) {
				dloc := kloc + "/" + name
				v.checkName(name, dloc)
				v.validateBitfieldDef(def, dloc)
			}
		default:
			v.add(kloc, "unknown $defs category %q (allowed: struct, union, enum, bitfield)", k)
		}
	}
}

func (v *validator) validateMessages(node any, loc string) {
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "messages must be a mapping")
		return
	}
	for name, val := range m {
		mloc := loc + "/" + name
		v.checkName(name, mloc)
		msg, ok := val.(map[string]any)
		if !ok {
			v.add(mloc, "message must be a mapping")
			continue
		}
		for k := range msg {
			switch k {
			case "summary", "payload":
			default:
				v.add(mloc, "unknown message key %q (allowed: summary, payload)", k)
			}
		}
		if s, ok := msg["summary"]; ok {
			if _, ok := s.(string); !ok {
				v.add(mloc+"/summary", "summary must be a string")
			}
		}
		payload, ok := msg["payload"]
		if !ok {
			v.add(mloc, "missing required key \"payload\"")
			continue
		}
		v.validateIDScope(payload, mloc+"/payload")
	}
}

// validateIDScope validates a payload/struct/union: a mapping of fieldName ->
// field, enforcing the uniqueIds custom keyword over its direct children
// (README §3: the scope applies to payload AND nested struct/union).
func (v *validator) validateIDScope(node any, loc string) {
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "must be a mapping of field definitions")
		return
	}
	ids := map[int64]string{}
	for name, val := range m {
		floc := loc + "/" + name
		v.checkName(name, floc)
		id := v.validateField(val, floc)
		if id != nil {
			if prev, dup := ids[*id]; dup {
				v.add(floc+"/id", "duplicate id %d (already used by %q in this scope)", *id, prev)
			} else {
				ids[*id] = name
			}
		}
	}
}

// validateField validates a single field object and returns its id (if a valid
// integer id was present) for the uniqueIds check.
func (v *validator) validateField(node any, loc string) *int64 {
	f, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "field must be a mapping")
		return nil
	}
	// id (required, 0..2^31-1)
	var idp *int64
	idRaw, hasID := f["id"]
	if !hasID {
		v.add(loc, "missing required key \"id\"")
	} else if id, ok := asInt(idRaw); !ok || id < 0 || id > 2147483647 {
		v.add(loc+"/id", "id must be an integer in 0..2147483647")
	} else {
		idp = &id
	}
	// type (required, enum)
	typRaw, hasType := f["type"]
	if !hasType {
		v.add(loc, "missing required key \"type\"")
		return idp
	}
	typ, ok := typRaw.(string)
	if !ok {
		v.add(loc+"/type", "type must be a string")
		return idp
	}

	// per-type validation + closedness. allowed always: id, type, description,
	// deprecated. Per type we extend the allowed set.
	switch typ {
	case "u8", "u16", "u32", "i8", "i16", "i32":
		v.closed(f, loc, "id", "type", "default", "description", "unit", "deprecated")
		v.checkScalarDefault(f, loc, typ)
	case "u64", "i64":
		v.closed(f, loc, "id", "type", "default", "description", "unit", "deprecated")
		v.checkInt64Range(f, loc, typ)
	case "fp32", "fp64":
		v.closed(f, loc, "id", "type", "default", "description", "decimals", "unit", "deprecated")
		v.checkFloatDefault(f, loc, typ)
		v.checkDecimals(f, loc)
	case "boolean":
		v.closed(f, loc, "id", "type", "default", "description", "deprecated")
		if d, ok := f["default"]; ok {
			if _, ok := d.(bool); !ok {
				v.add(loc+"/default", "default must be a boolean")
			}
		}
	case "string":
		v.closed(f, loc, "id", "type", "maxlen", "default", "description", "deprecated")
		v.checkMaxlen(f, loc)
		v.checkStringDefault(f, loc)
	case "blob":
		v.closed(f, loc, "id", "type", "maxlen", "default", "description", "deprecated")
		v.checkMaxlen(f, loc)
		v.checkBlobDefault(f, loc)
	case "enum":
		v.closed(f, loc, "id", "type", "enum", "default", "description", "deprecated")
		v.checkEnumField(f, loc)
	case "bitfield":
		v.closed(f, loc, "id", "type", "bits", "description", "deprecated")
		v.checkBitfieldField(f, loc)
	case "array":
		v.closed(f, loc, "id", "type", "items", "default", "description", "deprecated")
		v.checkArrayField(f, loc)
	case "struct":
		v.closed(f, loc, "id", "type", "fields", "description", "deprecated")
		v.checkStructField(f, loc)
	case "union":
		v.closed(f, loc, "id", "type", "oneof", "default_id", "description", "deprecated")
		v.checkUnionField(f, loc)
	default:
		v.add(loc+"/type", "unknown type %q", typ)
	}
	return idp
}

// ---- per-type helpers ---------------------------------------------------

func (v *validator) checkScalarDefault(f map[string]any, loc, typ string) {
	d, ok := f["default"]
	if !ok {
		return
	}
	if msg, bad := integralFloatVerdict(d, "default"); bad {
		v.add(loc+"/default", "%s", msg)
		return
	}
	n, ok := asInt(d)
	if !ok {
		v.add(loc+"/default", "default for %s must be an integer", typ)
		return
	}
	r := scalarRanges[typ]
	if n < r[0] || n > r[1] {
		v.add(loc+"/default", "default %d out of range for %s (%d..%d)", n, typ, r[0], r[1])
	}
}

// checkInt64Range ports the int64Range custom keyword (README §8): accept an
// integer or a decimal string, range-check exactly against the 64-bit bounds.
// The rule itself lives in int64Verdict, which the array-element arm of
// checkArrayElem shares so a u64 FIELD default and a u64 array ELEMENT default
// cannot drift apart (generator#484).
func (v *validator) checkInt64Range(f map[string]any, loc, kind string) {
	d, ok := f["default"]
	if !ok {
		return
	}
	if msg := int64Verdict(d, kind, "default"); msg != "" {
		v.add(loc+"/default", "%s", msg)
	}
}

// int64Verdict is the one spelling-and-range rule for a u64/i64 default, shared
// by the field-level checkInt64Range (README §8) and checkArrayElem's u64/i64 arm
// (README §8.2). noun is the word the message uses for what was checked —
// "default" or "element". It returns "" when the value is acceptable.
//
// It is deliberately the same shape as checkMaskElem, which #482 gave the
// array-of-bitfield arm: a bitfield mask IS a u64, so a divergence between the
// two would mean the same literal is legal for one element type and not the
// other. What differs is only what a bitfield adds on top — its backing width —
// and the sign, since an i64 default may be negative.
//
// One rule here is WIDER than what the code accepted before #484: an unquoted
// integer past 2^63-1 (9223372036854775808 .. 18446744073709551615) now
// validates for a u64. yaml.v3 hands such a literal over as a uint64 and the arm
// below has a uint64 case, where the old element arm read it through asInt —
// signed — and reported "element must be an integer". The quoted decimal string
// remains the portable spelling and is what the corpus documents, because JSON
// (and any other reader of the same definition) has no unsigned 64-bit number.
//
// Two rules here are narrower than what the code accepted before #484:
//
//   - A float64 is rejected in EVERY spelling, even one whose value is an exact
//     integer. This is a spelling refusal, not a value one: the backends render a
//     numeric default through fmt's "%v", which is %g's shortest form and flips to
//     exponent notation at 1e6, so `default: 100000.0` renders `100000` and
//     `default: 1000000.0` renders `1e+06` — an invisible threshold. Measured on
//     `default: 1000000.0` for a u64, one build per target: c (`invalid suffix
//     'ULL' on floating constant`), cpp (`unable to find numeric literal operator
//     'operator""ULL'`), rust (`E0308: expected u64, found floating-point
//     number`), kotlin (`unresolved reference 'uL'`), csharp (`CS1002: ; expected`
//     — `1e+06UL` does not even lex) and typescript (`1e+06n` is a SyntaxError) do
//     not compile; java throws NumberFormatException at class-init from
//     `Long.parseUnsignedLong("1e+06")`; python binds a float to a field declared
//     `int`. Only go, zig and dart render it correctly. So the shape was never
//     portably legal, and one rule in the validator beats correcting %v in ten
//     renderers on two paths each. yaml.v3 routes three different author mistakes
//     into one float64, so the arm gives three diagnoses. The exact-integer one is
//     integralFloatVerdict, which the u8..i32 and enum default and element arms
//     share, so the refusal is the same message wherever an integer is declared.
//   - A quoted u64 must match udecIntRe, not decIntRe. "-0" parses to a big.Int
//     whose Sign() is 0, so a value-only check waves it straight through and the
//     spelling reaches every backend verbatim — measured before this change on
//     `default: "-0"` for a u64 field: `negzero: -0` into a Rust u64 (E0600:
//     cannot apply unary operator `-`) and `-0ULL` in C++. A negative u64 is
//     refused by SPELLING, exactly as #482 learned for a mask. The shipped JSON
//     Schema already carried the unsigned pattern for a u64 default, so this is
//     the Go side catching up to the documented contract rather than a new rule.
func int64Verdict(d any, kind, noun string) string {
	signed := kind != "u64"
	var n *big.Int
	switch x := d.(type) {
	case string:
		switch {
		case udecIntRe.MatchString(x):
			n = mustBig(x)
		case signed && decIntRe.MatchString(x):
			n = mustBig(x)
		case decIntRe.MatchString(x):
			// Decimal, but signed, and kind is u64. "-0" lands here too and has to:
			// its VALUE is zero, so the range check below would never fire for it.
			return fmt.Sprintf("%s %q must not be negative (%s is unsigned)", noun, x, kind)
		default:
			return fmt.Sprintf("%s %q is not a decimal integer literal (%s)",
				noun, x, quotedIntHint(kind, signed, x))
		}
	case int:
		n = int64ToBig(int64(x))
	case int64:
		n = int64ToBig(x)
	case uint64:
		n = uint64ToBig(x)
	case float64:
		switch {
		case x != math.Trunc(x):
			return fmt.Sprintf("%s %v must be an integer, not a fractional number", noun, x)
		case !isSafeInteger(x):
			// The author DID write an integer (18446744073709551616, say); it is
			// only past the range a number carries exactly. Quoting is the route
			// that then reports the real 64-bit verdict, so say that rather than
			// "write the integer" — they already did.
			return fmt.Sprintf("%s %v is not an exact integer; quote it as a decimal string for exact 64-bit values", noun, x)
		default:
			// An exact integer spelled as a number, e.g. 1000000.0 or 1e6. Refused
			// for its spelling; the message names the integer to write instead. The
			// same refusal, from the same helper, guards every other integer default
			// in the schema — see integralFloatVerdict.
			msg, _ := integralFloatVerdict(x, noun)
			return msg
		}
	default:
		return fmt.Sprintf("%s must be an integer or a quoted decimal integer string (%s)", noun, kind)
	}
	if !signed && n.Sign() < 0 {
		// Reached only from the NUMBER spellings above: a quoted "-1" was already
		// refused by udecIntRe. Without this the verdict would be the range one,
		// and "out of exact u64 range" is the wrong diagnosis for -1 — it is
		// refused for its sign, not its width, and the author has to be told to
		// drop the minus. checkMaskElem says the same thing for the same value.
		return fmt.Sprintf("%s %s must not be negative (%s is unsigned)", noun, n.String(), kind)
	}
	if !in64Range(n, kind) {
		return fmt.Sprintf("%s %s out of exact %s range", noun, n.String(), kind)
	}
	return ""
}

// integralFloatVerdict refuses an integer default that was written as a NUMBER
// with a decimal point or an exponent — 1000000.0, 1e6 — and names the integer to
// write instead. It reports false, with no message, for anything that is not such
// a value: a non-float, a fractional float, or one past the double-safe range,
// each of which the caller's own arm already has a verdict for.
//
// It is a SPELLING refusal, not a value one, and it is why it has to be shared.
// Every backend renders a numeric default through fmt's "%v", which is %g's
// shortest form and flips to exponent notation at 1e6, so `default: 100000.0`
// renders `100000` and works everywhere while `default: 1000000.0` renders
// `1e+06` and does not — an invisible threshold no schema keyword names. Measured
// on this tree for `default: 1000000.0` on a u32 and on an array-of-u32 element:
// c `.a = 1e+06`, rust `a: 1e+06` into a u32 (E0308), java `new int[]{1e+06}`
// (incompatible types), and the same shape in the other eight. #482 chose this
// refusal for a bitfield mask and #484 for a u64/i64; the narrow and enum sites
// are the rest of the family, so that the SAME literal cannot be illegal for one
// integer type and legal-but-uncompilable for the one declared next to it. Every
// place an integer default can be written goes through here:
//
//   - checkScalarDefault  — a u8..i32 field default
//   - checkEnumField      — an enum field default
//   - checkArrayElem      — a u8..i32 array element, and an enum array element
//   - int64Verdict        — a u64/i64 field default and array element (above)
//   - checkMaskElem       — an array-of-bitfield element mask (#482)
//
// The first three reach the value through asInt, which accepts an integral
// float64; int64Verdict and checkMaskElem have float arms of their own and call
// this one for the exact-integer case, so the sentence an author reads is the
// same at all six and only the noun changes ("default", "element", "enum
// default", "enum element", "element mask").
func integralFloatVerdict(d any, noun string) (string, bool) {
	f, ok := d.(float64)
	if !ok || f != math.Trunc(f) || !isSafeInteger(f) {
		return "", false
	}
	return fmt.Sprintf("%s %v is spelled as a decimal number; write it as the integer %s",
		noun, f, strconv.FormatFloat(f, 'f', -1, 64)), true
}

func (v *validator) checkFloatDefault(f map[string]any, loc, typ string) {
	d, ok := f["default"]
	if !ok {
		return
	}
	n, ok := asFloat(d)
	if !ok {
		v.add(loc+"/default", "default for %s must be a number", typ)
		return
	}
	if typ == "fp32" && (n < -fp32Max || n > fp32Max) {
		v.add(loc+"/default", "default %v out of fp32 range", n)
	}
}

func (v *validator) checkDecimals(f map[string]any, loc string) {
	d, ok := f["decimals"]
	if !ok {
		return
	}
	if n, ok := asInt(d); !ok || n < 0 || n > 15 {
		v.add(loc+"/decimals", "decimals must be an integer in 0..15")
	}
}

func (v *validator) checkMaxlen(f map[string]any, loc string) {
	d, ok := f["maxlen"]
	if !ok {
		return
	}
	if n, ok := asInt(d); !ok || n < 1 || n > 2147483647 {
		v.add(loc+"/maxlen", "maxlen must be an integer in 1..2147483647")
	}
}

// checkStringDefault ports the string $data rule (README §2): len(default) <=
// maxlen when maxlen is present.
func (v *validator) checkStringDefault(f map[string]any, loc string) {
	d, ok := f["default"]
	if !ok {
		return
	}
	s, ok := d.(string)
	if !ok {
		v.add(loc+"/default", "default for string must be a string")
		return
	}
	if ml, ok := asInt(f["maxlen"]); ok {
		if int64(len(s)) > ml {
			v.add(loc+"/default", "default string length %d exceeds maxlen %d", len(s), ml)
		}
	}
}

// checkBlobDefault ports the blobDefaultLength keyword (README §5): base64
// pattern + decoded byte length <= maxlen.
func (v *validator) checkBlobDefault(f map[string]any, loc string) {
	d, ok := f["default"]
	if !ok {
		return
	}
	s, ok := d.(string)
	if !ok {
		v.add(loc+"/default", "default for blob must be a base64 string")
		return
	}
	if !base64Re.MatchString(s) {
		v.add(loc+"/default", "default blob is not valid base64")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		v.add(loc+"/default", "default blob is not decodable base64: %v", err)
		return
	}
	if ml, ok := asInt(f["maxlen"]); ok && int64(len(raw)) > ml {
		v.add(loc+"/default", "default blob decodes to %d bytes, exceeds maxlen %d", len(raw), ml)
	}
}

func (v *validator) checkEnumField(f map[string]any, loc string) {
	em, ok := f["enum"]
	if !ok {
		v.add(loc, "enum field requires an \"enum\" map (or $ref)")
		return
	}
	// after Resolve, a $ref enum has been replaced by the enum map already.
	values := v.validateEnumDef(em, loc+"/enum")
	// defaultMatchesEnum (README §4): presence test, not truthiness.
	if d, ok := f["default"]; ok {
		if msg, bad := integralFloatVerdict(d, "enum default"); bad {
			v.add(loc+"/default", "%s", msg)
			return
		}
		dn, ok := asInt(d)
		if !ok || dn < -2147483648 || dn > 2147483647 {
			v.add(loc+"/default", "enum default must be a signed 32-bit integer")
			return
		}
		if !containsInt(values, dn) {
			v.add(loc+"/default", "enum default %d does not match any declared enum value", dn)
		}
	}
}

// validateEnumDef validates an enum value map and returns the declared values.
func (v *validator) validateEnumDef(node any, loc string) []int64 {
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "enum must be a mapping of NAME -> value")
		return nil
	}
	var values []int64
	for name, val := range m {
		eloc := loc + "/" + name
		v.checkName(name, eloc)
		var n int64
		switch x := val.(type) {
		case map[string]any:
			for k := range x {
				switch k {
				case "value", "description":
				default:
					v.add(eloc, "unknown enum-constant key %q (allowed: value, description)", k)
				}
			}
			vv, ok := x["value"]
			if !ok {
				v.add(eloc, "enum constant requires \"value\"")
				continue
			}
			ni, ok := asInt(vv)
			if !ok {
				v.add(eloc+"/value", "enum value must be an integer")
				continue
			}
			n = ni
		default:
			ni, ok := asInt(val)
			if !ok {
				v.add(eloc, "enum value must be an integer or {value, description}")
				continue
			}
			n = ni
		}
		if n < -2147483648 || n > 2147483647 {
			v.add(eloc, "enum value %d out of signed 32-bit range", n)
		}
		values = append(values, n)
	}
	return values
}

func (v *validator) checkBitfieldField(f map[string]any, loc string) {
	bits, ok := f["bits"]
	if !ok {
		v.add(loc, "bitfield field requires \"bits\" (or $ref)")
		return
	}
	v.validateBitfieldDef(bits, loc+"/bits")
}

// validateBitfieldDef validates a bitfield and enforces uniquePositions (§6).
//
// It returns the highest VALID declared flag position, or -1 when the definition
// declares none. Only the array-element default check uses that: every backend
// gives a bitfield the smallest unsigned backing type that holds its highest
// position (ARCHITECTURE §10, mirrored by ir.bitfieldAlign), so the highest
// position is what bounds a mask written as a number. Every other caller ignores
// it.
func (v *validator) validateBitfieldDef(node any, loc string) int64 {
	maxPos := int64(-1)
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "bitfield must be a mapping of FLAG -> {pos, default?}")
		return maxPos
	}
	positions := map[int64]string{}
	for name, val := range m {
		floc := loc + "/" + name
		v.checkName(name, floc)
		flag, ok := val.(map[string]any)
		if !ok {
			v.add(floc, "bitfield flag must be a mapping")
			continue
		}
		for k := range flag {
			switch k {
			case "pos", "default", "description":
			default:
				v.add(floc, "unknown bitfield-flag key %q (allowed: pos, default, description)", k)
			}
		}
		posRaw, ok := flag["pos"]
		if !ok {
			v.add(floc, "bitfield flag requires \"pos\"")
			continue
		}
		pos, ok := asInt(posRaw)
		if !ok || pos < 0 || pos > 63 {
			v.add(floc+"/pos", "pos must be an integer in 0..63")
			continue
		}
		if d, ok := flag["default"]; ok {
			if _, ok := d.(bool); !ok {
				v.add(floc+"/default", "bitfield default must be a boolean")
			}
		}
		if prev, dup := positions[pos]; dup {
			v.add(floc+"/pos", "duplicate pos %d (already used by %q)", pos, prev)
		} else {
			positions[pos] = name
		}
		if pos > maxPos {
			maxPos = pos
		}
	}
	return maxPos
}

func (v *validator) checkArrayField(f map[string]any, loc string) {
	itemsRaw, ok := f["items"]
	if !ok {
		v.add(loc, "array field requires \"items\"")
		return
	}
	items, ok := itemsRaw.(map[string]any)
	if !ok {
		v.add(loc+"/items", "items must be a mapping {type, count?, ...}")
		return
	}
	etyp, enumValues, bitMaxPos := v.checkArrayItems(items, loc+"/items")

	// array default: length <= count (capacity), plus per-element validation.
	// Only leaf-typed element arrays carry a flat default.
	if d, ok := f["default"]; ok {
		arr, ok := d.([]any)
		if !ok {
			v.add(loc+"/default", "array default must be a sequence")
			return
		}
		if c, ok := asInt(items["count"]); ok && int64(len(arr)) > c {
			v.add(loc+"/default", "array default has %d elements, exceeds count %d", len(arr), c)
		}
		for i, el := range arr {
			v.checkArrayElem(etyp, el, enumValues, bitMaxPos, fmt.Sprintf("%s/default/%d", loc, i))
		}
	}
}

// checkArrayItems validates an array element definition (the `items` mapping) and
// returns the element type plus the context the caller's per-element default check
// needs: an enum element's declared values, and a bitfield element's highest
// declared flag position (-1 when it has none). It recurses into composite/nested
// element types, enforcing the full contract (uniqueIds / uniquePositions /
// defaultMatchesEnum / defaultIdMatchesUnion) exactly as field-level composites do.
func (v *validator) checkArrayItems(items map[string]any, loc string) (etyp string, enumValues []int64, bitMaxPos int64) {
	bitMaxPos = -1
	etyp, ok := items["type"].(string)
	if !ok || !arrayElemTypes[etyp] {
		v.add(loc+"/type", "array element type must be one of u8..u64,i8..i64,fp32,fp64,boolean,string,blob,enum,bitfield,struct,union,array")
		return etyp, nil, bitMaxPos
	}
	// per-element-type allowed keys (additionalProperties:false)
	allowed := []string{"type", "count"}
	switch etyp {
	case "string", "blob":
		allowed = append(allowed, "maxlen")
	case "enum":
		allowed = append(allowed, "enum")
	case "bitfield":
		allowed = append(allowed, "bits")
	case "struct":
		allowed = append(allowed, "fields")
	case "union":
		allowed = append(allowed, "oneof", "default_id")
	case "array":
		allowed = append(allowed, "items")
	}
	v.closed(items, loc, allowed...)

	// count is OPTIONAL (capacity); range-check when present.
	if cRaw, ok := items["count"]; ok {
		if c, ok := asInt(cRaw); !ok || c < 1 || c > 2147483647 {
			v.add(loc+"/count", "count must be an integer in 1..2147483647")
		}
	}
	// maxlen only for string/blob (the key set above already rejects it elsewhere).
	if mlRaw, ok := items["maxlen"]; ok && (etyp == "string" || etyp == "blob") {
		if m, ok := asInt(mlRaw); !ok || m < 1 || m > 2147483647 {
			v.add(loc+"/maxlen", "items.maxlen must be an integer in 1..2147483647")
		}
	}

	// composite / nested element sub-definitions → reuse the field validators so
	// every custom-keyword check recurses into array elements.
	switch etyp {
	case "enum":
		if em, ok := items["enum"]; ok {
			enumValues = v.validateEnumDef(em, loc+"/enum")
		} else {
			v.add(loc, "enum array element requires an \"enum\" map")
		}
	case "bitfield":
		if b, ok := items["bits"]; ok {
			bitMaxPos = v.validateBitfieldDef(b, loc+"/bits")
		} else {
			v.add(loc, "bitfield array element requires a \"bits\" map")
		}
	case "struct":
		if fields, ok := items["fields"]; ok {
			v.validateIDScope(fields, loc+"/fields") // fresh id scope (uniqueIds)
		} else {
			v.add(loc, "struct array element requires \"fields\"")
		}
	case "union":
		if _, ok := items["oneof"]; ok {
			v.checkUnionField(items, loc) // oneof uniqueIds + defaultIdMatchesUnion
		} else {
			v.add(loc, "union array element requires \"oneof\"")
		}
	case "array":
		if inner, ok := items["items"].(map[string]any); ok {
			v.checkArrayItems(inner, loc+"/items") // recurse (array of arrays)
		} else if _, present := items["items"]; present {
			v.add(loc+"/items", "items must be a mapping")
		} else {
			v.add(loc, "array array element requires \"items\"")
		}
	}
	return etyp, enumValues, bitMaxPos
}

func (v *validator) checkArrayElem(etyp string, el any, enumValues []int64, bitMaxPos int64, loc string) {
	switch etyp {
	case "u8", "u16", "u32", "i8", "i16", "i32":
		// The same spelling refusal the u64/i64 arm below gets from int64Verdict:
		// asInt accepts an integral float64, and 1e+06 in a `uint32_t` initializer
		// is not a literal any of rust, java, kotlin, csharp or cpp will take.
		if msg, bad := integralFloatVerdict(el, "element"); bad {
			v.add(loc, "%s", msg)
			return
		}
		n, ok := asInt(el)
		if !ok {
			v.add(loc, "element must be an integer")
			return
		}
		r := scalarRanges[etyp]
		if n < r[0] || n > r[1] {
			v.add(loc, "element %d out of range for %s", n, etyp)
		}
	case "u64", "i64":
		// The same rule as the u64/i64 FIELD default, from the same helper: an
		// array element and a field default are the same concept, and before #484
		// this arm tested only "is it an int, or is it a string" -- so ANY string
		// at all was carried into all eleven backends verbatim ("nonsense" became
		// `nonsenseULL` in C, `vec![nonsense]` in Rust, `nonsensen` in TypeScript).
		if msg := int64Verdict(el, etyp, "element"); msg != "" {
			v.add(loc, "%s", msg)
		}
	case "fp32", "fp64":
		if _, ok := asFloat(el); !ok {
			v.add(loc, "element must be a number")
		}
	case "boolean":
		if _, ok := el.(bool); !ok {
			v.add(loc, "element must be a boolean")
		}
	case "enum":
		if msg, bad := integralFloatVerdict(el, "enum element"); bad {
			v.add(loc, "%s", msg)
			return
		}
		n, ok := asInt(el)
		if !ok {
			v.add(loc, "enum element must be an integer")
			return
		}
		if enumValues != nil && !containsInt(enumValues, n) {
			v.add(loc, "enum element %d does not match any declared enum value", n)
		}
	case "string":
		if _, ok := el.(string); !ok {
			v.add(loc, "element must be a string")
		}
	case "blob":
		s, ok := el.(string)
		if !ok || !base64Re.MatchString(s) {
			v.add(loc, "element must be a base64 string")
		}
	case "bitfield":
		v.checkMaskElem(el, bitMaxPos, loc)
	}
}

// checkMaskElem validates one array-of-bitfield element default.
//
// An array is the only place a bitfield default is written as a NUMBER: the
// field-level form is a set of per-flag booleans (validateBitfieldDef) and the
// backends fold it into a mask themselves, so this is the only mask an author
// spells out. A mask is a u64, so the accepted spellings are checkInt64Range's for
// u64 — an integer, or a quoted DECIMAL string where the value needs the top of
// the range — with two deliberate differences, both narrower:
//
//   - A float is rejected in every spelling, even one whose value is an exact
//     integer, because every backend renders a numeric default through fmt's "%v",
//     which puts "1e+06" in a uint64_t initializer (measured). yaml.v3 routes three
//     different author mistakes into one float64, so the arm below gives three
//     diagnoses: fractional, exact-but-number-spelled (name the integer to write),
//     and an INTEGER too wide for yaml.v3 to hold as one (send them to the quoted
//     form, which then reports the real 64-bit verdict). YAML's own 0x10/0b101/0o17
//     arrive here as integers and are fine; it is only the QUOTED form that must be
//     decimal, which is what checkInt64Range requires of a u64 too.
//   - The bound is the bitfield's own backing width, not a flat 64 bits. That is
//     an INTERSECTION across the eleven targets, not something they all do. SIX
//     narrow the storage to the smallest unsigned type holding the highest
//     declared pos — bitfieldC (generators/c/backend.go), bitfieldBacking
//     (generators/{cpp,rust,zig,csharp}/helpers.go) and bitfieldGoType
//     (generators/golang/helpers.go) — and a wider mask does not fit the member
//     they emit. The member each of the six emits for a two-flag bitfield (pos 0,
//     pos 2) was measured: `uint8_t narrow[2]` in C, `std::vector<std::uint8_t>`
//     in C++, `Vec<u8>` in Rust, `[]BfNarrowElem` over a `uint8` in Go,
//     `FixedArray(u8, 2)` in Zig, a `byte`-backed enum in C#. The other FIVE carry
//     every bitfield at full width whatever it declares — measured as `long[]`
//     (java), `ULongArray` (kotlin), `number[]` (typescript), `list[int]` (python)
//     and `List<int>` (dart) — so a mask of 1000 was legal and correct there
//     before this check. One
//     definition has to generate for all eleven, so the schema takes the
//     narrowest target's bound. It subsumes the 64-bit one (a bitfield declaring
//     pos 63 gets the full 64).
//
// A bit set at a position no flag declares is ACCEPTED as long as it fits that
// width. No backend masks a bitfield value down to its declared positions —
// each one only ORs declared positions together when it builds a default — the
// wire carries the whole unsigned value, and an undeclared bit is exactly how a
// peer built from a newer schema carries a flag this one does not declare yet.
//
// It stays a separate function from int64Verdict, which #484 gave the u64/i64
// element and field arms, rather than calling it: the two agree line for line on
// the spelling and the sign, but every message here names an "element mask" and
// the width bound above has no counterpart there. The parallel is deliberate and
// both doc comments say so — a change to one belongs in the other. The one line
// they do share outright is the exact-valued-float sentence, which comes from
// integralFloatVerdict; that one guards every integer default in the schema, not
// just the 64-bit ones, so it could not stay duplicated here.
func (v *validator) checkMaskElem(el any, maxPos int64, loc string) {
	var n *big.Int
	switch x := el.(type) {
	case string:
		switch {
		case udecIntRe.MatchString(x):
			n = mustBig(x)
		case decIntRe.MatchString(x):
			// A decimal literal, but signed. "-0" lands here too, and has to: its
			// VALUE is zero, so the n.Sign() check below would never fire for it
			// and the spelling would reach every backend verbatim (measured:
			// `.a = { -0, 1ULL }` in C, `vec![-0, 1]` into a `Vec<u64>` in Rust,
			// which rustc rejects). A negative mask is refused by spelling.
			v.add(loc, "element mask %q must not be negative (a bitfield is unsigned)", x)
			return
		default:
			v.add(loc, "element mask %q is not a decimal integer literal (%s)", x, maskSpellingHint(x))
			return
		}
	case int:
		n = int64ToBig(int64(x))
	case int64:
		n = int64ToBig(x)
	case uint64:
		n = uint64ToBig(x)
	case float64:
		// yaml.v3 hands over a float64 for anything with a decimal point or an
		// exponent AND for an integer literal too wide to hold as one, so the three
		// cases need three different diagnoses.
		switch {
		case x != math.Trunc(x):
			v.add(loc, "element mask %v must be an integer, not a fractional number (a bit pattern has no fractional part)", x)
		case !isSafeInteger(x):
			// The author DID write an integer (18446744073709551616, say); it is
			// only past the range a number can carry exactly. Quoting is the route
			// that then reports the real 64-bit/backing verdict, so say that rather
			// than "must be an integer". Same wording as checkInt64Range.
			v.add(loc, "element mask %v is not an exact integer; quote it as a decimal string for exact 64-bit values", x)
		default:
			// An exact integer spelled as a number, e.g. 1000000.0 or 1e6. Rejected
			// for its spelling, not its value: every backend renders a numeric
			// default through fmt's "%v", which puts `1e+06` in a uint64_t
			// initializer, so the message names the integer to write instead. The
			// sentence is integralFloatVerdict's, shared with every other integer
			// default in the schema so the same mistake reads the same way; only the
			// noun is this arm's.
			msg, _ := integralFloatVerdict(x, "element mask")
			v.add(loc, "%s", msg)
		}
		return
	default:
		v.add(loc, "element must be an integer mask or a quoted decimal integer string")
		return
	}
	if n.Sign() < 0 {
		v.add(loc, "element mask %s must not be negative (a bitfield is unsigned)", n.String())
		return
	}
	width := maskWidthBits(maxPos)
	if n.BitLen() > width {
		if maxPos < 0 {
			// No flag was declared (an empty or malformed `bits` map, which has an
			// error of its own): one byte is what the six narrowing backends give
			// it, so it is what maskWidthBits gives it too.
			v.add(loc, "element mask %s does not fit the %d-bit backing of a bitfield that declares no flags", n.String(), width)
			return
		}
		v.add(loc, "element mask %s does not fit the %d-bit backing of a bitfield whose highest declared pos is %d", n.String(), width, maxPos)
	}
}

// maskWidthBits is the width of the unsigned type the six narrowing backends back
// a bitfield with: the smallest that holds its highest declared flag position. The
// choosers it has to agree with are bitfieldC (generators/c/backend.go),
// bitfieldBacking (generators/{cpp,rust,zig,csharp}/helpers.go) and bitfieldGoType
// (generators/golang/helpers.go). It happens to agree numerically with
// ir.bitfieldAlign, but that one's job is member-declaration ORDERING, not storage
// selection, so it is not the reference. A bitfield with no declared flag is one
// byte wide in all of them and here.
func maskWidthBits(maxPos int64) int {
	switch {
	case maxPos <= 7:
		return 8
	case maxPos <= 15:
		return 16
	case maxPos <= 31:
		return 32
	default:
		return 64
	}
}

// maskSpellingHint explains why a quoted mask was refused (#482). A mask is an
// unsigned value, so it takes quotedIntHint's unsigned wording.
func maskSpellingHint(s string) string { return quotedIntHint("mask", false, s) }

// quotedIntHint explains why a quoted integer literal was refused. A radix
// spelling and a stray decimal one need different advice: "0x10" should be
// written unquoted and let YAML convert it, while "0000000005" is already decimal
// and only has to lose its leading zeros — pointing that author at hex would send
// them the wrong way. what names the thing in the author's own words ("mask",
// "u64", "i64"); signed says whether a leading "-" is part of the accepted
// spelling, which is the one place a u64 and an i64 differ.
func quotedIntHint(what string, signed bool, s string) string {
	if looksRadixSpelled(s) {
		return "a quoted " + what + " must be decimal; write hex unquoted, e.g. 0x10, and YAML converts it"
	}
	if signed {
		return "a quoted " + what + " must be a plain decimal integer: an optional leading \"-\", no leading zeros, no spacing"
	}
	return "a quoted " + what + " must be a plain decimal integer: no leading zeros, no sign, no spacing"
}

// looksRadixSpelled reports whether a rejected quoted mask reads as a non-decimal
// RADIX: a 0x/0b/0o prefix, or bare hex digits with at least one letter ("ff").
func looksRadixSpelled(s string) bool {
	t := strings.TrimLeft(s, "+-")
	if len(t) > 1 && t[0] == '0' && strings.ContainsRune("xXbBoO", rune(t[1])) {
		return true
	}
	letter := false
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			letter = true
		default:
			return false
		}
	}
	return letter && t != ""
}

func (v *validator) checkStructField(f map[string]any, loc string) {
	fields, ok := f["fields"]
	if !ok {
		v.add(loc, "struct field requires \"fields\" (or $ref)")
		return
	}
	v.validateIDScope(fields, loc+"/fields") // fresh id scope (§3.3)
}

func (v *validator) checkUnionField(f map[string]any, loc string) {
	oneof, ok := f["oneof"]
	if !ok {
		v.add(loc, "union field requires \"oneof\" (or $ref)")
		return
	}
	v.validateIDScope(oneof, loc+"/oneof")
	// defaultIdMatchesUnion (README §7): presence test on default_id.
	if d, ok := f["default_id"]; ok {
		dn, ok := asInt(d)
		if !ok || dn < 0 || dn > 2147483647 {
			v.add(loc+"/default_id", "default_id must be an integer in 0..2147483647")
			return
		}
		om, _ := oneof.(map[string]any)
		found := false
		for _, opt := range om {
			if o, ok := opt.(map[string]any); ok {
				if oid, ok := asInt(o["id"]); ok && oid == dn {
					found = true
					break
				}
			}
		}
		if !found {
			v.add(loc+"/default_id", "default_id %d matches no option id in the union", dn)
		}
	}
}

// ---- generic helpers ----------------------------------------------------

func (v *validator) checkName(name, loc string) {
	if !nameRe.MatchString(name) {
		v.add(loc, "name %q must match ^[A-Za-z][A-Za-z0-9_]*$", name)
	}
}

// closed rejects any key not in allowed (additionalProperties:false).
func (v *validator) closed(f map[string]any, loc string, allowed ...string) {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	for k := range f {
		if !set[k] {
			v.add(loc, "unexpected key %q for this field type (allowed: %s)", k, strings.Join(allowed, ", "))
		}
	}
}

func asMapOf(v *validator, node any, loc string) map[string]any {
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "expected a mapping")
		return nil
	}
	return m
}

var (
	decIntRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	// The unsigned half of decIntRe, and the exact spelling the shipped JSON
	// Schema pins for a quoted bitfield mask. It is a separate pattern rather
	// than a sign check on the value because "-0" is a negative SPELLING whose
	// value is zero.
	udecIntRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	base64Re  = regexp.MustCompile(`^[A-Za-z0-9+/\s]+={0,2}$`)
	arrayElem = []string{
		"u8", "u16", "u32", "u64", "i8", "i16", "i32", "i64", "fp32", "fp64",
		"boolean", "string", "blob", "enum", "bitfield", "struct", "union", "array",
	}
)

var arrayElemTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range arrayElem {
		m[t] = true
	}
	return m
}()

func containsInt(s []int64, n int64) bool {
	for _, x := range s {
		if x == n {
			return true
		}
	}
	return false
}
