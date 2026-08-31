package ir

// BoundsInfo summarizes the schema bounds of a message's reachable fields, for
// the receiver-side decode limits (generator#102). HasDyn* report whether any
// reachable array / string / blob is unbounded (no schema count / maxlen) —
// only those fields are governed by the configured max_dyn_* caps, and a backend
// that emits none of them at all needs no limit plumbing in the module.
//
// Max* carry the largest schema-declared bound of each kind (0 when none), and
// they are NOT a floor under any cap. They used to be: while a cap travelled as
// a decoder-level option that bound every field of the message alike, the number
// passed in had to be raised to at least these or a schema-bounded field larger
// than the cap was rejected — the very thing CORELIB_PLAN §6.2.1 forbids a cap to
// touch — and that raise loosened the cap for exactly the unbounded fields it
// exists to protect. Every backend now applies the caps PER FIELD, at that
// field's own count/length header and behind the MESSAGE_SPEC §7.3 tag test, so
// the cap never reaches a bounded field and travels AS CONFIGURED.
//
// What still reads Max* is the worst-case size walk (ARCHITECTURE §9.6) and the
// tests/matrix guard over the bench rows, which asserts that an `-unbounded` row
// keeps a schema-BOUNDED array and string beside the unbounded ones — the cost
// that row measures is a decoder telling a schema bound (INVALID) from a receiver
// cap (LimitExceeded) per field, and with no bounded twin left it measures caps
// in isolation instead. Reaching for Max* to size a cap is the mistake; reading
// it to ask what the schema declared is not.
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
