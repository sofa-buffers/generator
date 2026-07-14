package ir

// BoundsInfo summarizes the schema bounds of a message's reachable fields, for
// the receiver-side decode limits (generator#102). HasDyn* report whether any
// reachable array / string / blob is unbounded (no schema count / maxlen) —
// only those fields are governed by the configured max_dyn_* caps. Max* carry
// the largest schema-declared bound of each kind (0 when none): backends whose
// corelib enforces the limits globally (Go, Python, TypeScript) raise the cap
// they pass in to at least these, so a schema-bounded field larger than the
// configured cap stays governed by its schema bound alone (the #102 escape
// hatch), while its own generator#100 guard still rejects over-schema counts.
type BoundsInfo struct {
	HasDynArray  bool
	HasDynString bool
	HasDynBlob   bool
	MaxCount     int64 // largest schema `count` over all reachable arrays
	MaxStringLen int64 // largest schema `maxlen` over all reachable strings
	MaxBlobLen   int64 // largest schema `maxlen` over all reachable blobs
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
	var walkElem func(elem Kind, ref *TypeRef, items *ArrayElem, hasCount bool, count int64, elemMaxHas bool, elemMax int64)

	walkElem = func(elem Kind, ref *TypeRef, items *ArrayElem, hasCount bool, count int64, elemMaxHas bool, elemMax int64) {
		if hasCount {
			if count > b.MaxCount {
				b.MaxCount = count
			}
		} else {
			b.HasDynArray = true
		}
		switch elem {
		case KindString:
			if elemMaxHas {
				if elemMax > b.MaxStringLen {
					b.MaxStringLen = elemMax
				}
			} else {
				b.HasDynString = true
			}
		case KindBlob:
			if elemMaxHas {
				if elemMax > b.MaxBlobLen {
					b.MaxBlobLen = elemMax
				}
			} else {
				b.HasDynBlob = true
			}
		case KindStruct, KindUnion:
			if ref != nil && ref.Target != nil && !seen[ref.Target] {
				seen[ref.Target] = true
				walkFields(ref.Target.Fields)
			}
		case KindArray:
			if items != nil {
				walkElem(items.Elem, items.ElemRef, items.ElemItems, items.HasCount, items.Count, items.ElemMaxHas, items.ElemMax)
			}
		}
	}

	walkFields = func(fields []*Field) {
		for _, f := range fields {
			switch f.Kind {
			case KindString:
				if f.HasMaxlen {
					if f.Maxlen > b.MaxStringLen {
						b.MaxStringLen = f.Maxlen
					}
				} else {
					b.HasDynString = true
				}
			case KindBlob:
				if f.HasMaxlen {
					if f.Maxlen > b.MaxBlobLen {
						b.MaxBlobLen = f.Maxlen
					}
				} else {
					b.HasDynBlob = true
				}
			case KindStruct, KindUnion:
				if f.Ref != nil && f.Ref.Target != nil && !seen[f.Ref.Target] {
					seen[f.Ref.Target] = true
					walkFields(f.Ref.Target.Fields)
				}
			case KindArray:
				walkElem(f.Elem, f.ElemRef, f.ElemItems, f.HasCount, f.Count, f.ElemMaxHas, f.ElemMax)
			}
		}
	}

	walkFields(fields)
	return b
}
