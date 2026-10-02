package ir

// SeqDepth returns the deepest sequence nesting encoding fields opens, counted
// exactly as a corelib Encoder counts open sequences:
//
//   - a struct/union field opens one frame around its target's own fields;
//   - a native array (scalar/enum/bool/bitfield/float) is one count-prefixed
//     value and opens none;
//   - every other array opens its wrapper, plus ArrayDepth of its elements.
//
// ok is false when a type recurses into itself: its nesting would then depend on
// the value, not the schema. The count is an UPPER bound -- a lazy frame that
// stays contentless is still counted, because the Encoder counts it while open.
// This is the one definition every backend and the analysis cap share.
func SeqDepth(fields []*Field, onPath map[string]bool) (int, bool) {
	best := 0
	for _, f := range fields {
		d, ok := FieldSeqDepth(f, onPath)
		if !ok {
			return 0, false
		}
		best = max(best, d)
	}
	return best, true
}

// FieldSeqDepth is the sequences one field opens (see SeqDepth).
func FieldSeqDepth(f *Field, onPath map[string]bool) (int, bool) {
	switch f.Kind {
	case KindStruct, KindUnion:
		d, ok := targetSeqDepth(f.Ref, onPath)
		return d + 1, ok
	case KindArray:
		return ArrayDepth(f.Elem, f.ElemRef, f.ElemItems, onPath)
	}
	return 0, true
}

// targetSeqDepth is SeqDepth of a struct/union target's fields, reporting a
// recursive back-edge as unbounded.
func targetSeqDepth(ref *TypeRef, onPath map[string]bool) (int, bool) {
	if ref == nil || ref.Target == nil {
		return 0, true
	}
	key := ref.Target.Key
	if onPath[key] {
		return 0, false
	}
	onPath[key] = true
	defer delete(onPath, key)
	return SeqDepth(ref.Target.Fields, onPath)
}

// ArrayDepth is the nesting an array field or row with this element opens: 0 for
// a native element (no frame), else 1 for its wrapper plus what one element
// nests -- nothing for a string/blob leaf, a per-element frame plus the target's
// depth for a struct/union, and the inner array's own depth for a nested array.
func ArrayDepth(elem Kind, ref *TypeRef, items *ArrayElem, onPath map[string]bool) (int, bool) {
	switch elem {
	case KindStruct, KindUnion:
		d, ok := targetSeqDepth(ref, onPath)
		return 2 + d, ok
	case KindArray:
		d, ok := ArrayDepth(items.Elem, items.ElemRef, items.ElemItems, onPath)
		return 1 + d, ok
	case KindString, KindBlob:
		return 1, true
	}
	return 0, true
}

// DeepestSeqPath names the field chain that reaches SeqDepth(fields), as
// "a/b/c" below the fields' owner, for an error message. A back-edge ends it.
func DeepestSeqPath(fields []*Field, onPath map[string]bool) string {
	var best *Field
	bestD := -1
	for _, f := range fields {
		if d, ok := FieldSeqDepth(f, onPath); ok && d > bestD {
			best, bestD = f, d
		}
	}
	if best == nil || bestD == 0 {
		return ""
	}
	var ref *TypeRef
	switch best.Kind {
	case KindStruct, KindUnion:
		ref = best.Ref
	case KindArray:
		ref = best.ElemRef
		for items := best.ElemItems; ref == nil && items != nil; items = items.ElemItems {
			ref = items.ElemRef
		}
	}
	return pathInto(best.Name, ref, onPath)
}

func pathInto(path string, ref *TypeRef, onPath map[string]bool) string {
	if ref == nil || ref.Target == nil || onPath[ref.Target.Key] {
		return path
	}
	onPath[ref.Target.Key] = true
	defer delete(onPath, ref.Target.Key)
	if sub := DeepestSeqPath(ref.Target.Fields, onPath); sub != "" {
		return path + "/" + sub
	}
	return path
}
