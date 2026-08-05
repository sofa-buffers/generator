package generator

import (
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// A field's schema bound has to reach the field's OWN doc comment: the three
// facts a caller can get wrong are that the bound is a capacity, that the
// container therefore starts empty, and that exceeding it is a rejection rather
// than a truncation (generator#308).
func TestBoundNote(t *testing.T) {
	arr := &ir.Field{Name: "fixed", Kind: ir.KindArray, Elem: ir.KindU32, HasCount: true, Count: 3}
	str := &ir.Field{Name: "name", Kind: ir.KindString, HasMaxlen: true, Maxlen: 8}

	tests := []struct {
		name string
		got  string
		want []string
		not  []string
	}{{
		name: "dynamic array names the capacity, the empty start and the reject",
		got:  BoundNote(arr, StorageDynamic),
		want: []string{"count 3", "CAPACITY", "starts empty", "INVALID", "never truncated"},
	}, {
		name: "fixed array does not repeat what the type already says",
		got:  BoundNote(arr, StorageFixed),
		want: []string{"count 3", "capacity is in the type", "LENGTH starts at 0"},
		not:  []string{"starts empty"},
	}, {
		name: "companion array names the length member",
		got:  BoundDoc{Storage: StorageCompanion, LenMember: "fixed_len"}.Note(arr),
		want: []string{"count 3", "fixed_len", "EMPTY array", "INVALID"},
	}, {
		name: "dynamic maxlen",
		got:  BoundNote(str, StorageDynamic),
		want: []string{"maxlen 8", "INVALID", "never truncated"},
	}, {
		name: "fixed maxlen defers to the type for the capacity",
		got:  BoundNote(str, StorageFixed),
		want: []string{"maxlen 8", "capacity is in the type", "never truncated"},
	}, {
		name: "companion blob names the length member",
		got:  BoundDoc{Storage: StorageCompanion, LenMember: "data_len"}.Note(&ir.Field{Name: "data", Kind: ir.KindBlob, HasMaxlen: true, Maxlen: 8}),
		want: []string{"maxlen 8", "data_len", "never truncated"},
	}, {
		name: "a declared default seeds the container, so it does not start empty",
		got: BoundNote(&ir.Field{Name: "levels", Kind: ir.KindArray, Elem: ir.KindU32,
			HasCount: true, Count: 4, Default: []any{int64(10), int64(20)}}, StorageDynamic),
		want: []string{"count 4", "starts at its declared default"},
		not:  []string{"starts empty"},
	}, {
		name: "an array's element maxlen is its own bound and is stated too",
		got: BoundNote(&ir.Field{Name: "tags", Kind: ir.KindArray, Elem: ir.KindString,
			HasCount: true, Count: 2, ElemMaxHas: true, ElemMax: 4}, StorageDynamic),
		want: []string{"count 2", "Element maxlen 4"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.got, "\n") {
				t.Errorf("note must be one line, got:\n%s", tc.got)
			}
			for _, w := range tc.want {
				if !strings.Contains(tc.got, w) {
					t.Errorf("note %q missing %q", tc.got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(tc.got, n) {
					t.Errorf("note %q should not contain %q", tc.got, n)
				}
			}
		})
	}
}

// A field with nothing to bound gets no note at all, so an unbounded schema's
// output is unchanged. The receiver-side max_dyn_* limits are decode policy,
// not a property of the field, and are documented where they are configured.
func TestBoundNoteEmpty(t *testing.T) {
	for _, f := range []*ir.Field{
		{Name: "count", Kind: ir.KindU32},
		{Name: "free", Kind: ir.KindArray, Elem: ir.KindU32},             // no count
		{Name: "note", Kind: ir.KindString},                              // no maxlen
		{Name: "blob", Kind: ir.KindBlob},                                // no maxlen
		{Name: "inner", Kind: ir.KindStruct, Ref: &ir.TypeRef{Key: "x"}}, // composite
	} {
		for _, s := range []FieldStorage{StorageDynamic, StorageFixed, StorageCompanion} {
			if got := BoundNote(f, s); got != "" {
				t.Errorf("%s (storage %d): want no note, got %q", f.Name, s, got)
			}
		}
	}
	if got := BoundNote(nil, StorageDynamic); got != "" {
		t.Errorf("nil field: want no note, got %q", got)
	}
}

func TestAppendDoc(t *testing.T) {
	if got := AppendDoc("desc", "note"); got != "desc\nnote" {
		t.Errorf("got %q", got)
	}
	if got := AppendDoc("", "note"); got != "note" {
		t.Errorf("got %q", got)
	}
	if got := AppendDoc("desc", ""); got != "desc" {
		t.Errorf("got %q", got)
	}
}
