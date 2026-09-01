package python

import "github.com/sofa-buffers/generator/internal/ir"

// How large the corelib's reassembly buffer has to be, for this schema.
//
// corelib-py joins a construct split across fed chunks in a buffer the CALLER
// supplies, sized once at construction and never grown (CORELIB_PLAN §6.6.2).
// The library holds no size of its own and defaults none — §6.2.1's "MUST NOT
// supply a default for one it was not given" applies to a reassembly size as
// squarely as to the three max_dyn_* caps, because the size decides which
// well-formed messages a receiver can stream (corelib-py#139). So the number is
// derived here, from the schema and the configured caps, the same way C++
// derives SOFAB_MAX_DYN_BUFFERED_FIELD.
//
// What has to fit is ONE CONSTRUCT, not one message and not one field sub-tree:
//
//   - A value being built has to be whole before it can be handed over — a
//     string or blob payload, or an array's whole element run — so the largest
//     of those is the floor.
//   - A sequence is NOT one construct. Its fields are read one at a time, so a
//     nested message needs room for its largest single field, never the sum of
//     them. This is where the number differs from the C++ one, which is a
//     refusal ceiling rather than an allocation: over-estimating there costs
//     nothing and over-estimating here allocates the difference.
//   - A field being SKIPPED needs no room at all. It has no value to rebuild,
//     so corelib-py discards it as it arrives, across as many chunks as it
//     takes. An unknown id of any size is free, which is why this walk sizes
//     for what a receiver READS.
//
// Every varint is charged its widest form and every missing schema bound is
// filled by the configured cap — both of those come from ir.MaxFieldDecodeSpan,
// which is the shared walk this one is built out of.
type spanWalk struct {
	caps *ir.DynCaps
	seen map[string]bool
}

// maxConstructSpan returns the largest single construct any message in the
// schema can carry, and whether one could be derived at all. It is false only if
// some construct stays unbounded even with the caps applied, which cannot happen
// while every cap is finite (ARCHITECTURE §9.5) — the flag is kept so a future
// unbounded case fails loudly instead of emitting a number that is too small.
func maxConstructSpan(s *ir.Schema, caps *ir.DynCaps) (int64, bool) {
	w := &spanWalk{caps: caps, seen: map[string]bool{}}
	var worst int64
	for _, m := range s.Messages {
		for _, f := range m.Fields {
			c, ok := w.field(f)
			if !ok {
				return 0, false
			}
			worst = max(worst, c)
		}
	}
	return worst, true
}

// field returns the largest construct reachable through one field.
func (w *spanWalk) field(f *ir.Field) (int64, bool) {
	switch f.Kind {
	case ir.KindStruct, ir.KindUnion:
		// The sequence framing is not a construct; its fields are.
		return w.named(f.Ref)
	case ir.KindArray:
		return w.array(f, f.Elem, f.ElemRef, f.ElemItems)
	default:
		// A scalar, a string or a blob: the field IS the construct.
		return ir.MaxFieldDecodeSpan(f, map[string]bool{}, w.caps)
	}
}

// array splits an array by how it lowers. A native element array is one payload
// and has to be whole; every other element kind lowers to a wrapper sequence
// whose elements are separate constructs.
func (w *spanWalk) array(f *ir.Field, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) (int64, bool) {
	switch elem {
	case ir.KindStruct, ir.KindUnion:
		return w.named(ref)
	case ir.KindArray:
		if items == nil {
			return 0, false
		}
		return w.array(f, items.Elem, items.ElemRef, items.ElemItems)
	case ir.KindString, ir.KindBlob:
		// One element is one fixlen payload. Charged through the shared walk by
		// asking it for a one-element array, so the element's framing and the
		// cap substitution stay in the one place that knows them.
		one := *f
		one.Kind = ir.KindArray
		one.Elem = elem
		one.ElemRef = nil
		one.ElemItems = nil
		one.HasCount, one.Count = true, 1
		return ir.MaxFieldDecodeSpan(&one, map[string]bool{}, w.caps)
	default:
		// Native varint / fp array: the whole element run is one payload.
		return ir.MaxFieldDecodeSpan(f, map[string]bool{}, w.caps)
	}
}

// named walks a struct/union's own fields. A type reached recursively adds no
// construct the first visit has not already measured, so a cycle contributes
// nothing rather than reporting the schema unbounded.
func (w *spanWalk) named(ref *ir.TypeRef) (int64, bool) {
	if ref == nil || ref.Target == nil {
		return 0, false
	}
	if w.seen[ref.Key] {
		return 0, true
	}
	w.seen[ref.Key] = true
	defer delete(w.seen, ref.Key)

	var worst int64
	for _, c := range ref.Target.Fields {
		cc, ok := w.field(c)
		if !ok {
			return 0, false
		}
		worst = max(worst, cc)
	}
	return worst, true
}
