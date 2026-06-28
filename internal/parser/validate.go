package parser

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
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
func (v *validator) checkInt64Range(f map[string]any, loc, kind string) {
	d, ok := f["default"]
	if !ok {
		return
	}
	var big *big.Int
	switch x := d.(type) {
	case string:
		if !decIntRe.MatchString(x) {
			v.add(loc+"/default", "default string %q is not a valid integer literal", x)
			return
		}
		big = mustBig(x)
	case int:
		big = int64ToBig(int64(x))
	case int64:
		big = int64ToBig(x)
	case uint64:
		big = uint64ToBig(x)
	case float64:
		if x != math.Trunc(x) || !isSafeInteger(x) {
			v.add(loc+"/default", "default %v is not an exact integer; quote it as a string for exact 64-bit values", x)
			return
		}
		big = int64ToBig(int64(x))
	default:
		v.add(loc+"/default", "default for %s must be an integer or a quoted integer string", kind)
		return
	}
	if !in64Range(big, kind) {
		v.add(loc+"/default", "default %s out of exact %s range", big.String(), kind)
	}
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
func (v *validator) validateBitfieldDef(node any, loc string) {
	m, ok := node.(map[string]any)
	if !ok {
		v.add(loc, "bitfield must be a mapping of FLAG -> {pos, default?}")
		return
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
	}
}

func (v *validator) checkArrayField(f map[string]any, loc string) {
	itemsRaw, ok := f["items"]
	if !ok {
		v.add(loc, "array field requires \"items\"")
		return
	}
	items, ok := itemsRaw.(map[string]any)
	if !ok {
		v.add(loc+"/items", "items must be a mapping {type, count, maxlen?}")
		return
	}
	for k := range items {
		switch k {
		case "type", "count", "maxlen":
		default:
			v.add(loc+"/items", "unknown items key %q (allowed: type, count, maxlen)", k)
		}
	}
	etypRaw, ok := items["type"]
	etyp, _ := etypRaw.(string)
	if !ok || !arrayElemTypes[etyp] {
		v.add(loc+"/items/type", "array element type must be one of u8..u64,i8..i64,fp32,fp64,string,blob")
	}
	var count int64 = -1
	if cRaw, ok := items["count"]; !ok {
		v.add(loc+"/items", "items requires \"count\"")
	} else if c, ok := asInt(cRaw); !ok || c < 1 || c > 2147483647 {
		v.add(loc+"/items/count", "count must be an integer in 1..2147483647")
	} else {
		count = c
	}
	if _, ok := items["maxlen"]; ok {
		if etyp != "string" && etyp != "blob" {
			v.add(loc+"/items/maxlen", "items.maxlen is only valid for string/blob elements")
		} else if m, ok := asInt(items["maxlen"]); !ok || m < 1 || m > 2147483647 {
			v.add(loc+"/items/maxlen", "items.maxlen must be an integer in 1..2147483647")
		}
	}
	// array default: $data rule len == count, plus per-element type/range.
	if d, ok := f["default"]; ok {
		arr, ok := d.([]any)
		if !ok {
			v.add(loc+"/default", "array default must be a sequence")
			return
		}
		if count >= 0 && int64(len(arr)) != count {
			v.add(loc+"/default", "array default has %d elements, must equal count %d", len(arr), count)
		}
		for i, el := range arr {
			v.checkArrayElem(etyp, el, fmt.Sprintf("%s/default/%d", loc, i))
		}
	}
}

func (v *validator) checkArrayElem(etyp string, el any, loc string) {
	switch etyp {
	case "u8", "u16", "u32", "i8", "i16", "i32":
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
		if _, ok := asInt(el); !ok {
			if _, ok := el.(string); !ok {
				v.add(loc, "element must be an integer")
			}
		}
	case "fp32", "fp64":
		if _, ok := asFloat(el); !ok {
			v.add(loc, "element must be a number")
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
	}
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
	decIntRe  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	base64Re  = regexp.MustCompile(`^[A-Za-z0-9+/\s]+={0,2}$`)
	arrayElem = []string{"u8", "u16", "u32", "u64", "i8", "i16", "i32", "i64", "fp32", "fp64", "string", "blob"}
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
