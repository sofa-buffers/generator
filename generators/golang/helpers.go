package golang

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

func cfgBool(cfg map[string]any, key string) bool {
	b, _ := cfg[key].(bool)
	return b
}

// cfgLimit reads an integer decode-limit key (generator#102). YAML/JSON decode
// integers into different Go types depending on the path, so all are accepted.
func cfgLimit(cfg map[string]any, key string) (int64, bool) {
	switch v := cfg[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// reservedGoMethod are the exported method names every generated object carries
// (the sofab.Visitor callbacks, plus Encode on messages). A struct field whose
// exported name matches one would collide with the method (Go forbids a field and
// method sharing a name), so goFieldName mangles it; the `json` tag keeps the wire
// name, so encoding/json is unaffected.
var reservedGoMethod = map[string]bool{
	"Unsigned": true, "Signed": true, "Float32": true, "Float64": true,
	"String": true, "Bytes": true, "UnsignedArray": true, "SignedArray": true,
	"Float32Array": true, "Float64Array": true, "BeginSequence": true, "EndSequence": true,
	"Encode": true,
}

// goFieldName is the exported struct-field name for a schema field, mangled with a
// trailing underscore when it would collide with a generated method.
func goFieldName(name string) string {
	n := exported(name)
	if reservedGoMethod[n] {
		return n + "_"
	}
	return n
}

// exported converts a schema name to an exported Go identifier (PascalCase,
// underscores folded into camel case).
func exported(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	if b.Len() == 0 {
		return "X"
	}
	return b.String()
}

// typeName is the exported Go type name for a named-type graph key (e.g.
// "struct/Point" -> "StructPoint", "msg_somestruct" -> "MsgSomestruct").
func (g *gen) typeName(key string) string {
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// goType is the Go field type for a field.
func (g *gen) goType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return goNumType(f.Kind)
	case ir.KindFP32:
		return "float32"
	case ir.KindFP64:
		return "float64"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "[]byte"
	case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return "[]" + g.goArrayElem(f.Elem, f.ElemRef, f.ElemItems)
	}
	return "any"
}

// goArrayElem is the Go type of an array element, recursing for nested arrays.
// Numeric/bool/enum/bitfield/struct/union map to their scalar Go type; a nested
// array prepends another slice level.
func (g *gen) goArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "[]byte"
	case ir.KindBool:
		return "bool"
	case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return "[]" + g.goArrayElem(items.Elem, items.ElemRef, items.ElemItems)
	default: // numeric
		return goNumType(elem)
	}
}

func goNumType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "uint8"
	case ir.KindU16:
		return "uint16"
	case ir.KindU32:
		return "uint32"
	case ir.KindU64:
		return "uint64"
	case ir.KindI8:
		return "int8"
	case ir.KindI16:
		return "int16"
	case ir.KindI32:
		return "int32"
	case ir.KindI64:
		return "int64"
	case ir.KindFP32:
		return "float32"
	case ir.KindFP64:
		return "float64"
	}
	return "int64"
}

// enumGoType backs an enum with the smallest signed width covering its range.
func enumGoType(nt *ir.NamedType) string {
	var lo, hi int64
	for _, c := range nt.Consts {
		if c.Value < lo {
			lo = c.Value
		}
		if c.Value > hi {
			hi = c.Value
		}
	}
	switch {
	case lo >= -128 && hi <= 127:
		return "int8"
	case lo >= -32768 && hi <= 32767:
		return "int16"
	default:
		return "int32"
	}
}

func bitfieldGoType(nt *ir.NamedType) string {
	var max int64
	for _, fl := range nt.Flags {
		if fl.Pos > max {
			max = fl.Pos
		}
	}
	switch {
	case max <= 7:
		return "uint8"
	case max <= 15:
		return "uint16"
	case max <= 31:
		return "uint32"
	default:
		return "uint64"
	}
}

// fieldDocText is the field's description plus a unit note, collapsed to one
// line ("" when the field has neither).
func fieldDocText(f *ir.Field) string {
	doc := oneline(f.Description)
	if f.Unit != "" {
		if doc != "" {
			doc += " "
		}
		doc += "(unit: " + f.Unit + ")"
	}
	return doc
}

func fieldDoc(f *ir.Field) string {
	doc := fieldDocText(f)
	if doc == "" {
		return ""
	}
	return " // " + doc
}

func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// defaultLiteral returns a Go literal for a field's schema default, or
// ("", false) when there is none / it is not emitted (arrays/composites are
// left zero; the harness sets them explicitly).
func (g *gen) defaultLiteral(f *ir.Field) (string, bool) {
	if f.Default == nil {
		// bitfield default is derived from its flags, not a field Default.
		if f.Kind == ir.KindBitfield {
			return g.bitfieldDefault(f)
		}
		// A `count: N` native array is fixed-length even with no schema default:
		// its value is N element defaults, so materialize them. Without this a
		// fresh (or all-default, hence omitted-on-the-wire) array would decode to
		// an empty slice on this growable backend while the fixed-storage camp
		// yields N zeros — the same MESSAGE_SPEC §3 divergence as the trailing
		// default run, reached through the omission path.
		if f.Kind == ir.KindArray && isNativeArrayElem(f.Elem) && f.HasCount {
			return g.nativeArrayLiteral(f)
		}
		return "", false
	}
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return fmt.Sprintf("%v", scalarLit(f.Default)), true
	case ir.KindBool:
		return fmt.Sprintf("%v", f.Default), true
	case ir.KindFP32, ir.KindFP64:
		return fmt.Sprintf("%v", f.Default), true
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", s), true
		}
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return byteSliceLit(raw), true
			}
		}
	case ir.KindEnum:
		return fmt.Sprintf("%s(%v)", g.typeName(f.Ref.Key), scalarLit(f.Default)), true
	case ir.KindArray:
		// A NATIVE scalar array is a leaf field: materialize its default so an
		// omitted default array reconstructs correctly, and so marshal can compare
		// against it. Composite arrays are wrapper sequences (always framed) and
		// are left zero.
		if isNativeArrayElem(f.Elem) {
			return g.nativeArrayLiteral(f)
		}
	}
	return "", false
}

func (g *gen) bitfieldDefault(f *ir.Field) (string, bool) {
	var bits uint64
	any := false
	for _, fl := range f.Ref.Target.Flags {
		if fl.HasDefault && fl.Default {
			bits |= 1 << uint(fl.Pos)
			any = true
		}
	}
	if !any {
		return "", false
	}
	return fmt.Sprintf("%s(%d)", g.typeName(f.Ref.Key), bits), true
}

// isNativeArrayElem reports whether an array element uses a native scalar array
// wire type (vs. a wrapper sequence). Native arrays are a leaf field (omitted as
// a whole when equal to their default); composite/dynamic-element arrays are
// always framed.
func isNativeArrayElem(elem ir.Kind) bool {
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// nativeArrayLiteral renders a native scalar array's schema default as a Go
// slice literal ([]T{...}); ("", false) when there is no default.
func (g *gen) nativeArrayLiteral(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok && f.Default != nil {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch f.Elem {
		case ir.KindBool, ir.KindFP32, ir.KindFP64:
			parts[i] = fmt.Sprintf("%v", v)
		default: // numeric / enum / bitfield: an untyped constant converts in []T
			parts[i] = scalarLit(v)
		}
	}
	// A `count: N` array is exactly N elements long: a shorter schema default
	// leaves the trailing ones at the element default. Tail-pad to N so this
	// backend's initial value matches the fixed-storage camp's zero-filled
	// `[T; N]` / `std::array<T, N>` (MESSAGE_SPEC §3).
	if f.HasCount {
		zero := "0"
		if f.Elem == ir.KindBool {
			zero = "false"
		}
		for int64(len(parts)) < f.Count {
			parts = append(parts, zero)
		}
	}
	return fmt.Sprintf("[]%s{%s}", g.goArrayElem(f.Elem, f.ElemRef, f.ElemItems), strings.Join(parts, ", ")), true
}

// nativeArrayTrimmedDefault renders a count:N native array's schema default with
// its trailing element-default run removed (the §3 canonical content of the
// default), as a `[]T{...}` literal. The marshal omit-guard for a fixed-count
// array compares the value's trimmed form against this (issue #139). The bool
// result is false when the trimmed default is empty (no schema default, or an
// all-element-default one) - the guard then reduces to a `len() != 0` check on
// the trimmed value.
func (g *gen) nativeArrayTrimmedDefault(f *ir.Field) (string, bool) {
	vals, _ := f.Default.([]any)
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		switch f.Elem {
		case ir.KindBool, ir.KindFP32, ir.KindFP64:
			parts = append(parts, fmt.Sprintf("%v", v))
		default: // numeric / enum / bitfield
			parts = append(parts, scalarLit(v))
		}
	}
	// Drop the trailing element-default run (mirrors _trimTail on constants; a
	// float -0.0 renders as "-0", so it is kept, matching the bit-pattern trim).
	for len(parts) > 0 && isNativeElemDefaultLit(parts[len(parts)-1], f.Elem) {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return "", false
	}
	return fmt.Sprintf("[]%s{%s}", g.goArrayElem(f.Elem, f.ElemRef, f.ElemItems), strings.Join(parts, ", ")), true
}

// isNativeElemDefaultLit reports whether a rendered element literal is the
// element default (0 / false / +0.0), for trimming a fixed-count array default.
func isNativeElemDefaultLit(lit string, elem ir.Kind) bool {
	if elem == ir.KindBool {
		return lit == "false"
	}
	return lit == "0" // numeric/enum/bitfield 0, and +0.0 (which %v renders as "0")
}

// scalarLit renders a decoded integer default (int64 or a quoted big string).
func scalarLit(v any) string {
	switch x := v.(type) {
	case string:
		return x // exact 64-bit value given as a string literal
	default:
		return fmt.Sprintf("%v", x)
	}
}

func byteSliceLit(b []byte) string {
	var sb strings.Builder
	sb.WriteString("[]byte{")
	for i, x := range b {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "0x%02x", x)
	}
	sb.WriteString("}")
	return sb.String()
}
