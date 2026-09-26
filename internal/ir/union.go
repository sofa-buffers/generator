package ir

// DefaultOption is the option a fresh union holds: the field whose ID is
// *n.DefaultID. After analysis it is non-nil for every union; it is nil for any
// other category and for a union whose DefaultID is not (yet) bound.
func (n *NamedType) DefaultOption() *Field {
	if n == nil || n.Category != CatUnion || n.DefaultID == nil {
		return nil
	}
	for _, f := range n.Fields {
		if f.ID == *n.DefaultID {
			return f
		}
	}
	return nil
}

// IsDefaultOption reports whether f is n's default option (the one a fresh
// value holds and the only one an encoder may omit at its own default).
func (n *NamedType) IsDefaultOption(f *Field) bool {
	return f != nil && n.DefaultOption() == f
}
