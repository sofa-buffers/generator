package golang

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
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

// exported is a schema name as an exported Go identifier: naming.Pascal
// (`vehicle_telemetry` -> VehicleTelemetry). The naming rules keep it distinct
// within a scope.
func exported(name string) string { return naming.Pascal(name) }

// typeName is the Go type identifier of the named type at graph key key
// (ARCHITECTURE §8, "Naming"): see typeIdent.
func (g *gen) typeName(key string) string {
	return typeIdent(g.schema.Named[key])
}

// typeBase is a named type's identifier before the escape: the path spelled
// by naming.TypeIdent, plus, for a variant of a split union, the role
// "__Default" + the Pascal option name. Everything DERIVED from the type -- an
// enum constant, a bitfield flag, a union option's id constant -- is built
// from this, never from the escaped identifier.
func typeBase(nt *ir.NamedType) string {
	t := naming.TypeIdent(nt.Path)
	if nt.Variant != "" {
		t += "__Default" + naming.Pascal(nt.Variant)
	}
	return t
}

// typeIdent is the Go identifier a named type is declared under: typeBase,
// escaped with a trailing underscore when it spells a package-level name the
// generated package declares itself (reserved.go).
func typeIdent(nt *ir.NamedType) string { return escapeType(typeBase(nt)) }

// msgBase and msgIdent are typeBase and typeIdent for a message, whose path is
// its own name.
func msgBase(m *ir.Message) string  { return naming.TypeIdent([]string{m.Name}) }
func msgIdent(m *ir.Message) string { return escapeType(msgBase(m)) }

// msgFile is a message's file: its folded name (naming.Lower), which has no
// "_". So it can end neither in `_test` nor in a `_<GOOS>`/`_<GOARCH>` build
// constraint, and it never meets the package's fixed files, which all carry one
// (sofab_types.go, sofab_visitor.go).
//
// A fold that is a Windows device name (con, nul, com1, …) cannot be a file on
// Windows and is refused by the Go module zip, so it takes a trailing "_":
// con_.go. No other message file has a "_", so the suffix is free; the fixed
// files start with "sofab_", which no device name does; and a trailing "_"
// leaves an empty last segment, which is neither `test` nor a GOOS/GOARCH.
func msgFile(m *ir.Message) string {
	stem := naming.Lower([]string{m.Name})
	if naming.IsDeviceStem(stem) {
		stem += "_"
	}
	return stem + ".go"
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
		// against it. A composite array is a wrapper sequence whose declared default
		// is not materialized, which is what makes its dropping closer correct (§2).
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
// a whole when equal to their default); a composite/dynamic-element array is
// a wrapper sequence, opened lazily and closed with the dropping end at field
// level (MESSAGE_SPEC §2).
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
	// Not padded to a declared `count: N`: that is a capacity, not a length
	// (MESSAGE_SPEC §3), so the default stands exactly as written -- and so does
	// the value it is compared against, which is what keeps a length-N all-zero
	// array distinct from the empty one.
	return fmt.Sprintf("[]%s{%s}", g.goArrayElem(f.Elem, f.ElemRef, f.ElemItems), strings.Join(parts, ", ")), true
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
