// Package model implements stage [2] of the pipeline: it lowers a validated,
// unresolved definition document into the language-neutral ir.Schema
// (Composite). It is run AFTER the parser's hard-gate validation, so it may
// assume structural well-formedness, but it still guards against malformed
// values to fail gracefully rather than panic.
//
// $ref structure is PRESERVED as shared-type references (ir.TypeRef.Key) and
// inline composites are hoisted into synthetic shared NamedTypes — so a later
// pass (internal/analysis) can wire one shared generated type per definition
// (PLAN §3.4), never duplicating.
package model

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/parser"
)

// Build lowers a validated, UNRESOLVED document into an ir.Schema whose
// composite fields carry unresolved TypeRefs (Target == nil). Call
// analysis.Analyze next to resolve the graph and run semantic checks.
func Build(doc *parser.Document) (*ir.Schema, error) {
	root, ok := doc.Root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document root is not a mapping")
	}
	b := &builder{schema: &ir.Schema{Named: map[string]*ir.NamedType{}}}

	if ver, ok := asInt(root["version"]); ok {
		b.schema.Version = int(ver)
	}

	// 1. Register $defs named types first, so $ref targets exist as graph keys.
	if defs, ok := root["$defs"].(map[string]any); ok {
		b.buildDefs(defs)
	}

	// 2. Lower messages (deterministic: sorted by name).
	if msgs, ok := root["messages"].(map[string]any); ok {
		for _, name := range sortedKeys(msgs) {
			m, _ := msgs[name].(map[string]any)
			b.schema.Messages = append(b.schema.Messages, b.buildMessage(name, m))
		}
	}
	return b.schema, nil
}

type builder struct{ schema *ir.Schema }

func (b *builder) register(nt *ir.NamedType) {
	if _, exists := b.schema.Named[nt.Key]; !exists {
		b.schema.NamedOrder = append(b.schema.NamedOrder, nt.Key)
	}
	b.schema.Named[nt.Key] = nt
}

func (b *builder) buildDefs(defs map[string]any) {
	for _, cat := range []string{"struct", "union", "enum", "bitfield"} {
		group, ok := defs[cat].(map[string]any)
		if !ok {
			continue
		}
		for _, name := range sortedKeys(group) {
			key := cat + "/" + name
			switch cat {
			case "struct", "union":
				category := ir.CatStruct
				if cat == "union" {
					category = ir.CatUnion
				}
				nt := &ir.NamedType{Category: category, Name: name, Key: key}
				b.register(nt) // register before recursing (supports self-reference)
				nt.Fields = b.buildFields(group[name], key)
			case "enum":
				b.register(b.buildEnum(name, key, group[name]))
			case "bitfield":
				b.register(b.buildBitfield(name, key, group[name]))
			}
		}
	}
}

func (b *builder) buildMessage(name string, m map[string]any) *ir.Message {
	msg := &ir.Message{Name: name}
	if s, ok := m["summary"].(string); ok {
		msg.Summary = strings.TrimSpace(s)
	}
	if payload, ok := m["payload"].(map[string]any); ok {
		msg.Fields = b.buildFields(payload, name)
	}
	return msg
}

// buildFields lowers an id scope (payload / struct fields / union options),
// returning fields sorted by id then name (ascending-id order, §6.1).
func (b *builder) buildFields(node any, parentKey string) []*ir.Field {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	var fields []*ir.Field
	for _, name := range sortedKeys(m) {
		fdef, ok := m[name].(map[string]any)
		if !ok {
			continue
		}
		fields = append(fields, b.buildField(name, fdef, parentKey))
	}
	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].ID != fields[j].ID {
			return fields[i].ID < fields[j].ID
		}
		return fields[i].Name < fields[j].Name
	})
	return fields
}

func (b *builder) buildField(name string, f map[string]any, parentKey string) *ir.Field {
	fld := &ir.Field{Name: name}
	if id, ok := asInt(f["id"]); ok {
		fld.ID = id
	}
	if s, ok := f["description"].(string); ok {
		fld.Description = s
	}
	if s, ok := f["unit"].(string); ok {
		fld.Unit = s
	}
	if dep, ok := f["deprecated"].(bool); ok {
		fld.Deprecated = dep
	}
	typ, _ := f["type"].(string)
	fld.Kind = kindOf(typ)

	switch typ {
	case "string", "blob":
		if ml, ok := asInt(f["maxlen"]); ok {
			fld.HasMaxlen, fld.Maxlen = true, ml
		}
		fld.Default = f["default"]
	case "fp32", "fp64":
		if d, ok := asInt(f["decimals"]); ok {
			di := int(d)
			fld.Decimals = &di
		}
		fld.Default = f["default"]
	case "enum":
		fld.Ref = b.refForComposite(f["enum"], ir.CatEnum, name, parentKey)
		fld.Default = f["default"]
	case "bitfield":
		fld.Ref = b.refForComposite(f["bits"], ir.CatBitfield, name, parentKey)
	case "struct":
		fld.Ref = b.refForComposite(f["fields"], ir.CatStruct, name, parentKey)
	case "union":
		fld.Ref = b.refForComposite(f["oneof"], ir.CatUnion, name, parentKey)
		if id, ok := asInt(f["default_id"]); ok {
			fld.Default = id
		}
	case "array":
		b.buildArray(fld, f, name, parentKey)
	default: // scalars + boolean
		fld.Default = f["default"]
	}
	return fld
}

func (b *builder) buildArray(fld *ir.Field, f map[string]any, name, parentKey string) {
	items, _ := f["items"].(map[string]any)
	etyp, _ := items["type"].(string)
	fld.Elem = kindOf(etyp)
	if c, ok := asInt(items["count"]); ok {
		fld.HasCount, fld.Count = true, c // count is optional (capacity)
	}
	if ml, ok := asInt(items["maxlen"]); ok {
		fld.ElemMaxHas, fld.ElemMax = true, ml
	}
	fld.ElemRef = b.elemRef(etyp, items, name, parentKey)
	if etyp == "array" {
		inner, _ := items["items"].(map[string]any)
		fld.ElemItems = b.buildArrayElem(inner, name+"_elem", parentKey)
	}
	fld.Default = f["default"]
}

// elemRef hoists/refs a composite array element type (enum/bitfield/struct/union)
// to a shared NamedType, reusing the field-level inline-hoisting path. Returns
// nil for leaf elements (scalars/string/blob/boolean) and nested arrays.
func (b *builder) elemRef(etyp string, items map[string]any, name, parentKey string) *ir.TypeRef {
	switch etyp {
	case "enum":
		return b.refForComposite(items["enum"], ir.CatEnum, name+"_elem", parentKey)
	case "bitfield":
		return b.refForComposite(items["bits"], ir.CatBitfield, name+"_elem", parentKey)
	case "struct":
		return b.refForComposite(items["fields"], ir.CatStruct, name+"_elem", parentKey)
	case "union":
		return b.refForComposite(items["oneof"], ir.CatUnion, name+"_elem", parentKey)
	}
	return nil
}

// buildArrayElem lowers a nested array element (items.items...) recursively.
func (b *builder) buildArrayElem(items map[string]any, name, parentKey string) *ir.ArrayElem {
	if items == nil {
		return nil
	}
	etyp, _ := items["type"].(string)
	e := &ir.ArrayElem{Elem: kindOf(etyp)}
	if c, ok := asInt(items["count"]); ok {
		e.HasCount, e.Count = true, c
	}
	if ml, ok := asInt(items["maxlen"]); ok {
		e.ElemMaxHas, e.ElemMax = true, ml
	}
	e.ElemRef = b.elemRef(etyp, items, name, parentKey)
	if etyp == "array" {
		inner, _ := items["items"].(map[string]any)
		e.ElemItems = b.buildArrayElem(inner, name+"_elem", parentKey)
	}
	return e
}

// refForComposite resolves a composite member to a shared NamedType. If the
// sub-definition is {$ref: "#/$defs/<cat>/<Name>"} it points at that graph key;
// otherwise it hoists the inline definition into a synthetic NamedType named
// after its owner (PLAN §5.3 nested-type namespacing).
func (b *builder) refForComposite(def any, cat ir.Category, fieldName, parentKey string) *ir.TypeRef {
	if key, ok := refKey(def); ok {
		return &ir.TypeRef{Key: key}
	}
	// inline → synthesize
	synthKey := parentKey + "_" + fieldName
	switch cat {
	case ir.CatStruct, ir.CatUnion:
		nt := &ir.NamedType{Category: cat, Name: synthKey, Key: synthKey, Inline: true}
		b.register(nt)
		nt.Fields = b.buildFields(def, synthKey)
	case ir.CatEnum:
		nt := b.buildEnum(synthKey, synthKey, def)
		nt.Inline = true
		b.register(nt)
	case ir.CatBitfield:
		nt := b.buildBitfield(synthKey, synthKey, def)
		nt.Inline = true
		b.register(nt)
	}
	return &ir.TypeRef{Key: synthKey}
}

func (b *builder) buildEnum(name, key string, def any) *ir.NamedType {
	nt := &ir.NamedType{Category: ir.CatEnum, Name: name, Key: key}
	m, ok := def.(map[string]any)
	if !ok {
		return nt
	}
	for _, cname := range sortedKeys(m) {
		ec := &ir.EnumConst{Name: cname}
		switch x := m[cname].(type) {
		case map[string]any:
			if val, ok := asInt(x["value"]); ok {
				ec.Value = val
			}
			if d, ok := x["description"].(string); ok {
				ec.Description = d
			}
		default:
			if val, ok := asInt(m[cname]); ok {
				ec.Value = val
			}
		}
		nt.Consts = append(nt.Consts, ec)
	}
	sort.SliceStable(nt.Consts, func(i, j int) bool {
		if nt.Consts[i].Value != nt.Consts[j].Value {
			return nt.Consts[i].Value < nt.Consts[j].Value
		}
		return nt.Consts[i].Name < nt.Consts[j].Name
	})
	return nt
}

func (b *builder) buildBitfield(name, key string, def any) *ir.NamedType {
	nt := &ir.NamedType{Category: ir.CatBitfield, Name: name, Key: key}
	m, ok := def.(map[string]any)
	if !ok {
		return nt
	}
	for _, fname := range sortedKeys(m) {
		flag := &ir.BitfieldFlag{Name: fname}
		if fm, ok := m[fname].(map[string]any); ok {
			if p, ok := asInt(fm["pos"]); ok {
				flag.Pos = p
			}
			if d, ok := fm["default"].(bool); ok {
				flag.Default, flag.HasDefault = d, true
			}
			if d, ok := fm["description"].(string); ok {
				flag.Description = d
			}
		}
		nt.Flags = append(nt.Flags, flag)
	}
	sort.SliceStable(nt.Flags, func(i, j int) bool { return nt.Flags[i].Pos < nt.Flags[j].Pos })
	return nt
}

// ---- helpers ------------------------------------------------------------

// refKey returns the graph key for a {$ref:"#/$defs/<cat>/<Name>"} object.
func refKey(def any) (string, bool) {
	m, ok := def.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	ref, ok := m["$ref"].(string)
	if !ok {
		return "", false
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	return strings.TrimPrefix(ref, prefix), true
}

var kindByName = map[string]ir.Kind{
	"u8": ir.KindU8, "u16": ir.KindU16, "u32": ir.KindU32, "u64": ir.KindU64,
	"i8": ir.KindI8, "i16": ir.KindI16, "i32": ir.KindI32, "i64": ir.KindI64,
	"fp32": ir.KindFP32, "fp64": ir.KindFP64, "boolean": ir.KindBool,
	"string": ir.KindString, "blob": ir.KindBlob, "array": ir.KindArray,
	"enum": ir.KindEnum, "bitfield": ir.KindBitfield, "struct": ir.KindStruct, "union": ir.KindUnion,
}

func kindOf(t string) ir.Kind {
	if k, ok := kindByName[t]; ok {
		return k
	}
	return ir.KindInvalid
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		return int64(x), true
	case float64:
		return int64(x), true
	default:
		return 0, false
	}
}
