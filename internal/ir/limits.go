package ir

// BoundsInfo summarizes the schema bounds of a message's reachable fields, for
// the receiver-side decode limits (generator#102). HasDyn* report whether any
// reachable array / string / blob is unbounded (no schema count / maxlen) —
// only those fields are governed by the configured max_dyn_* caps, and a
// backend that emits none of them at all needs no limit plumbing in the module.
//
// Nothing here records how LARGE the schema bounds are, because nothing needs
// it any more. The caps are applied PER FIELD now, at that field's own
// count/length header and behind the MESSAGE_SPEC §7.3 tag test, so a cap never
// reaches a field the schema bounds (CORELIB_PLAN §6.2.1) and never has to be
// raised past one. The raise existed only while a cap travelled as a
// decoder-level option that bound every field of the message alike: it had to
// clear the largest maxlen/count in the message or reject a schema-bounded
// field §6.2.1 forbids it to touch — and that loosened the cap for exactly the
// unbounded fields it exists to protect. With the cap on the field, the number
// travels AS CONFIGURED.
type BoundsInfo struct {
	HasDynArray  bool
	HasDynString bool
	HasDynBlob   bool
}

// HasDyn reports whether any reachable field is unbounded at all — when false
// the configured decode limits are inert for this message and backends emit no
// limit plumbing.
func (b BoundsInfo) HasDyn() bool { return b.HasDynArray || b.HasDynString || b.HasDynBlob }

// Bounds walks fields (recursing into struct/union targets and array element
// nesting) and returns their BoundsInfo. Shared named types are visited once.
func Bounds(fields []*Field) BoundsInfo {
	var b BoundsInfo
	seen := map[*NamedType]bool{}

	var walkFields func([]*Field)
	var walkElem func(elem Kind, ref *TypeRef, items *ArrayElem, hasCount bool, elemMaxHas bool)

	walkElem = func(elem Kind, ref *TypeRef, items *ArrayElem, hasCount bool, elemMaxHas bool) {
		if !hasCount {
			b.HasDynArray = true
		}
		switch elem {
		case KindString:
			if !elemMaxHas {
				b.HasDynString = true
			}
		case KindBlob:
			if !elemMaxHas {
				b.HasDynBlob = true
			}
		case KindStruct, KindUnion:
			if ref != nil && ref.Target != nil && !seen[ref.Target] {
				seen[ref.Target] = true
				walkFields(ref.Target.Fields)
			}
		case KindArray:
			if items != nil {
				walkElem(items.Elem, items.ElemRef, items.ElemItems, items.HasCount, items.ElemMaxHas)
			}
		}
	}

	walkFields = func(fields []*Field) {
		for _, f := range fields {
			switch f.Kind {
			case KindString:
				if !f.HasMaxlen {
					b.HasDynString = true
				}
			case KindBlob:
				if !f.HasMaxlen {
					b.HasDynBlob = true
				}
			case KindStruct, KindUnion:
				if f.Ref != nil && f.Ref.Target != nil && !seen[f.Ref.Target] {
					seen[f.Ref.Target] = true
					walkFields(f.Ref.Target.Fields)
				}
			case KindArray:
				walkElem(f.Elem, f.ElemRef, f.ElemItems, f.HasCount, f.ElemMaxHas)
			}
		}
	}

	walkFields(fields)
	return b
}
