// Package testschema builds schemas for tests that need one far too deep to
// commit as a file.
package testschema

import "fmt"

// Shapes are the nesting constructs Deep chains.
var Shapes = []string{"struct", "union", "arr_struct", "arr_union", "arr_arr_struct", "mixed", "str"}

// Deep returns a definition with message M holding n chained levels of shape,
// the innermost holding a u8 (shape "str": an array of string). A level is
// "a: <construct>" wrapping the next one; the cap counts the sequences they open.
func Deep(shape string, n int) string {
	inner := "{ x: { id: 0, type: u8 } }"
	if shape == "str" {
		inner = "{ s: { id: 0, type: array, items: { type: string, count: 1, maxlen: 4 } } }"
	}
	for i := 0; i < n; i++ {
		kind := shape
		if shape == "mixed" {
			kind = "struct"
			if (n-1-i)%2 == 1 {
				kind = "arr_struct"
			}
		}
		switch kind {
		case "arr_struct":
			inner = fmt.Sprintf("{ a: { id: 0, type: array, items: { type: struct, count: 1, fields: %s } } }", inner)
		case "arr_union":
			inner = fmt.Sprintf("{ a: { id: 0, type: array, items: { type: union, count: 1, oneof: %s } } }", inner)
		case "arr_arr_struct":
			inner = fmt.Sprintf("{ a: { id: 0, type: array, items: { type: array, count: 1, items: { type: struct, count: 1, fields: %s } } } }", inner)
		case "union":
			inner = fmt.Sprintf("{ a: { id: 0, type: union, oneof: %s } }", inner)
		default: // struct, str
			inner = fmt.Sprintf("{ a: { id: 0, type: struct, fields: %s } }", inner)
		}
	}
	return fmt.Sprintf("version: 1\nmessages:\n  M:\n    payload: %s\n", inner)
}

// Boundary is, per shape, the last accepted N and the sequences it opens.
var Boundary = map[string]struct{ Last, Depth int }{
	"struct":         {255, 255},
	"union":          {255, 255},
	"arr_struct":     {127, 254},
	"arr_union":      {127, 254},
	"arr_arr_struct": {85, 255},
	"mixed":          {170, 255},
	"str":            {254, 255},
}
