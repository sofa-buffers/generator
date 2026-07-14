package c

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/ir"
)

// checkBounded enforces the C target's sizing policy: the C object model has no
// dynamic containers, so every field that lowers to fixed storage must be sized
// by the schema. A string or blob needs a maxlen; every array (of ANY element
// kind, including a native scalar array) needs a count; a string/blob array
// element needs its own element maxlen.
//
// An unbounded such field is a hard generate-time error naming the offending
// field. Unlike the C++ c-cpp and Rust no_std fixed-capacity profiles there is
// NO allow_dynamic escape — the C object model cannot fall back to a heap
// container — so the error does not suggest one.
//
// Without this the backend silently invented a minimal capacity (char[1], a
// zero-length T[0], or a single holder slot), generation succeeded, and every
// real message carrying the field was later rejected at runtime as
// SOFAB_RET_E_INVALID_MSG — misreporting a schema/target mismatch as wire
// malformation (generator#104, cf. the policy-vs-malformation distinction in
// generator#102).
func checkBounded(s *ir.Schema) error {
	seen := map[string]bool{}
	var walkFields func(key, owner string, fields []*ir.Field) error
	var walkArray func(owner, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool) error

	walkArray = func(owner, path string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool) error {
		// Every array level needs a count — including a native scalar array, whose
		// missing count otherwise became a zero-length member T[0].
		if count <= 0 {
			return unboundedErr(owner, path, "count")
		}
		switch elem {
		case ir.KindString, ir.KindBlob:
			if !elemMaxHas {
				return unboundedErr(owner, path, "element maxlen")
			}
		case ir.KindStruct, ir.KindUnion:
			return walkFields(ref.Key, ref.Target.Name, ref.Target.Fields)
		case ir.KindArray:
			return walkArray(owner, path+"[]", items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas)
		}
		return nil
	}

	walkFields = func(key, owner string, fields []*ir.Field) error {
		if seen[key] {
			return nil
		}
		seen[key] = true
		for _, f := range fields {
			switch f.Kind {
			case ir.KindString, ir.KindBlob:
				if !f.HasMaxlen {
					return unboundedErr(owner, f.Name, "maxlen")
				}
			case ir.KindStruct, ir.KindUnion:
				if err := walkFields(f.Ref.Key, f.Ref.Target.Name, f.Ref.Target.Fields); err != nil {
					return err
				}
			case ir.KindArray:
				if err := walkArray(owner, f.Name, f.Elem, f.ElemRef, f.ElemItems, f.Count, f.ElemMaxHas); err != nil {
					return err
				}
			}
		}
		return nil
	}

	for _, m := range s.Messages {
		if err := walkFields("message/"+m.Name, m.Name, m.Fields); err != nil {
			return err
		}
	}
	return nil
}

func unboundedErr(owner, path, missing string) error {
	return fmt.Errorf("c: field %q of %q has no %s; the fixed-storage C target requires a bound on every string/blob (maxlen) and array (count) — the C object model has no dynamic-container fallback", path, owner, missing)
}
