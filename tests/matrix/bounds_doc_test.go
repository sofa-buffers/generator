package matrix

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// TestBoundsReachTheFieldDoc pins generator#308: a field's schema bound has to
// appear in the FIELD's own doc comment, in every backend.
//
// It used to appear only in internal decode plumbing (the array collector, the
// visitor's cap parameter) — places a caller writing `msg.fixed[0] = 100` has
// no reason to read. Every usage example written against this API before the
// note existed made the same mistake in the same direction: treating `count: 3`
// as three elements already there rather than as a capacity over an empty
// container. In C, where the length is a separate member, that mistake encodes
// an empty field with no crash and no warning.
//
// The note text itself comes from one shared helper
// (internal/generator.BoundDoc), so this test checks that every backend calls it
// for every bounded field, renders it as a comment, and does not invent one for
// an unbounded field.
func TestBoundsReachTheFieldDoc(t *testing.T) {
	s, err := buildIR(t, filepath.Join("testdata", "bounds.yaml"))
	if err != nil {
		t.Fatalf("bounds.yaml should validate: %v", err)
	}

	// One anchor per bounded field. The wording varies with how the backend
	// stored the field, so only the bound itself is pinned here; the phrasing
	// per storage mode is internal/generator's own test.
	anchors := []string{
		"Schema bound: maxlen 8",
		"Schema bound: maxlen 16",
		"Schema bound: count 3",
		"Schema bound: count 4",
		"Schema bound: count 2",
	}

	for _, lang := range generator.Registered() {
		t.Run(lang, func(t *testing.T) {
			b, _ := generator.Lookup(lang)
			files, err := b.Generate(s, map[string]any{})
			if err != nil {
				// C is heapless by default and rejects the unbounded field, which
				// is its own deliberate error (generator#104).
				if fixedOnlyTarget(lang) {
					t.Skipf("%s rejects an unbounded field by design: %v", lang, err)
				}
				t.Fatalf("generate: %v", err)
			}
			var sb strings.Builder
			for _, f := range files {
				sb.WriteString(string(f.Content))
				sb.WriteByte('\n')
			}
			out := sb.String()

			// The docs target is a rendered table, not code: the bound is in the
			// Type column (u32[3], string (maxlen 8)) and what the note says in
			// code is a legend under the table.
			if lang == "docs" {
				for _, want := range []string{"maxlen 8", "maxlen 16", "u32[3]", "u32[4]"} {
					if !strings.Contains(out, want) {
						t.Errorf("docs output does not state %q", want)
					}
				}
				for _, want := range []string{"is a <strong>capacity</strong>", "INVALID", "never truncated"} {
					if !strings.Contains(out, want) {
						t.Errorf("docs table has no legend fragment %q", want)
					}
				}
				return
			}

			for _, want := range anchors {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in the generated output", want)
					continue
				}
				if !everyOccurrenceIsAComment(out, want) {
					t.Errorf("%q is not on a comment line", want)
				}
			}

			// Every note names the outcome of exceeding the bound: it is a
			// rejection, never a truncation (MESSAGE_SPEC §7.1). That is the half
			// of the note a reader cannot infer from the type.
			if !strings.Contains(out, "INVALID") {
				t.Error("no note states that exceeding a bound is INVALID")
			}

			// A wrapper array states its ELEMENT's bound too.
			if !strings.Contains(out, "Element maxlen 5") {
				t.Error("the wrapper array's element maxlen is not documented")
			}

			// One note per bounded field and not one more: `free` is unbounded and
			// must be documented exactly as it was before the note existed.
			if got, want := strings.Count(out, "Schema bound:"), len(anchors); got != want {
				t.Errorf("got %d bound notes, want %d (an unbounded field must get none)", got, want)
			}
		})
	}
}

// everyOccurrenceIsAComment reports whether every line carrying frag is a
// comment line — a note emitted as code rather than as documentation fails here.
func everyOccurrenceIsAComment(out, frag string) bool {
	found := false
	for _, ln := range strings.Split(out, "\n") {
		if !strings.Contains(ln, frag) {
			continue
		}
		found = true
		if !onCommentLine(ln, frag) {
			return false
		}
	}
	return found
}
