package generator

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// FieldStorage describes how a backend lowered a bounded field — which decides
// how much of the bound the generated type already states, and therefore what
// the doc note still has to say.
type FieldStorage int

const (
	// StorageDynamic is a growable container (std::vector, Vec, List, []T,
	// number[]): the bound appears nowhere in the type, so the note carries all
	// of it.
	StorageDynamic FieldStorage = iota
	// StorageFixed is a fixed-capacity container sized from the bound
	// (sofab::InlineVector<T, N>, heapless::Vec<T, N>, FixedString<N>, char[N+1]):
	// the capacity is visible in the type, the LENGTH is not.
	StorageFixed
	// StorageCompanion is raw storage plus a separate length member — the C
	// target's `T name[N]` beside `name_len`. The length is not just invisible,
	// it is a second member the caller has to remember to set, which is the one
	// way to get a silently EMPTY field on the wire.
	StorageCompanion
)

// BoundDoc renders the one-line documentation note for a field's schema bounds.
//
// The note exists because the bound is enforced everywhere and stated nowhere
// the caller is looking (generator#308): the field's doc comment carries the
// schema `description` and nothing else, so `count: 3` reads as "three
// elements" rather than as "at most three, starting at zero". Every usage
// example written against this API before the note existed made that mistake.
//
// The wording is deliberately about the two things a caller can get wrong —
// that the container starts EMPTY, and that exceeding the bound is a rejection
// rather than a truncation (MESSAGE_SPEC §3, §7.1) — and not about the wire
// format, which the type does not decide.
type BoundDoc struct {
	// Storage is how this backend lowered THIS field. It varies per field
	// within one message: on static storage a bounded array is inline while an
	// unbounded one stays dynamic.
	Storage FieldStorage
	// LenMember is the companion length member's name, for StorageCompanion.
	// Ignored otherwise.
	LenMember string
}

// Note returns the doc line for f, or "" when f carries no schema bound (an
// array without `count`, a string/blob without `maxlen`, or any other kind).
// The result is a single line with no comment syntax: the caller wraps it.
func (d BoundDoc) Note(f *ir.Field) string {
	if f == nil {
		return ""
	}
	switch f.Kind {
	case ir.KindArray:
		return d.arrayNote(f)
	case ir.KindString, ir.KindBlob:
		if !f.HasMaxlen {
			return ""
		}
		return d.maxlenNote(f.Maxlen)
	default:
		return ""
	}
}

func (d BoundDoc) arrayNote(f *ir.Field) string {
	if !f.HasCount {
		// An unbounded array has no bound to state. What governs it instead is a
		// receiver-side max_dyn_array_count, which is a decode-time policy rather
		// than a property of this field.
		return ""
	}
	// "Starts empty" is the whole point of the note — but it is false for a
	// field with a declared default, which starts at that default and not at
	// zero elements. Getting THAT wrong would replace one wrong belief with
	// another.
	start := "starts empty"
	lenStart := "starts at 0"
	if hasSeededDefault(f) {
		start = "starts at its declared default"
		lenStart = "starts at the declared default's length"
	}
	var b strings.Builder
	switch d.Storage {
	case StorageFixed:
		fmt.Fprintf(&b, "Schema bound: count %d -- the capacity is in the type; the LENGTH %s and is what reaches the wire.", f.Count, lenStart)
	case StorageCompanion:
		fmt.Fprintf(&b, "Schema bound: count %d is a capacity; %s carries the length -- elements set without it encode an EMPTY array. Over %d is INVALID.", f.Count, d.LenMember, f.Count)
	default:
		fmt.Fprintf(&b, "Schema bound: count %d is a CAPACITY, not a length -- %s; over %d elements is INVALID, never truncated.", f.Count, start, f.Count)
	}
	if f.ElemMaxHas {
		fmt.Fprintf(&b, " Element maxlen %d, same rule.", f.ElemMax)
	}
	return b.String()
}

func (d BoundDoc) maxlenNote(maxlen int64) string {
	switch d.Storage {
	case StorageFixed:
		return fmt.Sprintf("Schema bound: maxlen %d -- the capacity is in the type; a longer value is INVALID, never truncated.", maxlen)
	case StorageCompanion:
		return fmt.Sprintf("Schema bound: maxlen %d -- %s carries the length; a longer value is INVALID, never truncated.", maxlen, d.LenMember)
	default:
		return fmt.Sprintf("Schema bound: maxlen %d -- a longer value is INVALID, never truncated.", maxlen)
	}
}

// hasSeededDefault reports whether the field's schema default puts elements in
// the container before the caller touches it.
func hasSeededDefault(f *ir.Field) bool {
	vals, ok := f.Default.([]any)
	return ok && len(vals) > 0
}

// BoundNote is the common case: one storage mode for every field, no companion
// member.
func BoundNote(f *ir.Field, s FieldStorage) string {
	return BoundDoc{Storage: s}.Note(f)
}

// AppendDoc joins a backend's existing doc text with the bound note, keeping
// the note on a line of its own. Either side may be empty.
func AppendDoc(doc, note string) string {
	switch {
	case note == "":
		return doc
	case doc == "":
		return note
	default:
		return doc + "\n" + note
	}
}
