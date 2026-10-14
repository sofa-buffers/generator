package cpp

import (
	"strings"
	"testing"
)

// reset() clears a container in place, which keeps the CONTAINER's buffer and
// destroys its elements. For an element that owns storage of its own -- a
// std::string element, a std::vector row of a matrix, a struct element holding
// either -- that storage goes back to the allocator and the next decode takes it
// again. Measured on examples/messages/realworld/vehicle_telemetry.yaml against
// corelib-cpp: 11 free + 11 alloc per decode into a reused destination.
//
// So the doc says it, and says it only where such an element exists: a message
// whose containers hold scalars, and every message under the fixed-capacity
// profile, loses nothing and is told nothing.
const resetKeptClaim = "a container keeps its own"
const resetLostClaim = "Its ELEMENTS are not part of that buffer."

// The sentence this replaced, which promised more than reset() delivers.
const resetOldClaim = "has already paid for is kept"

func TestResetDocNamesWhatIsNotKept(t *testing.T) {
	src := `
version: 1
messages:
  M:
    payload:
      name:     { id: 0, type: string, maxlen: 16 }
      counts:   { id: 1, type: array, items: { type: u32, count: 4 } }
      warnings: { id: 2, type: array, items: { type: string, maxlen: 24, count: 4 } }
`
	h := headerFromYAML(t, src, "m.hpp")
	if !strings.Contains(h, resetKeptClaim) {
		t.Fatalf("reset() doc does not say what IS kept:\n%s", h)
	}
	if !strings.Contains(h, resetLostClaim) {
		t.Errorf("a std::vector<std::string> field loses its elements' storage on reset(), and the doc does not say so:\n%s", h)
	}
	if strings.Contains(h, resetOldClaim) {
		t.Error("reset() doc still claims the whole capacity is kept")
	}
}

func TestResetDocSilentWhenNothingIsLost(t *testing.T) {
	// Scalar containers only: a std::string member and a std::vector<std::uint32_t>
	// both keep their buffer across reset(), and their elements own nothing.
	src := `
version: 1
messages:
  M:
    payload:
      name:   { id: 0, type: string, maxlen: 16 }
      counts: { id: 1, type: array, items: { type: u32, count: 4 } }
      flag:   { id: 2, type: boolean }
`
	h := headerFromYAML(t, src, "m.hpp")
	if !strings.Contains(h, resetKeptClaim) {
		t.Fatalf("reset() doc does not say what IS kept:\n%s", h)
	}
	if strings.Contains(h, resetLostClaim) {
		t.Error("no element here owns storage, so reset() should not warn about one")
	}
}

func TestResetDocSilentUnderFixedStorage(t *testing.T) {
	// The same string array as the first test. Under the fixed-capacity profile
	// the element is a sofab::FixedString<24>, which owns no heap at all, so
	// clearing the container gives nothing back.
	src := `
version: 1
messages:
  M:
    payload:
      name:     { id: 0, type: string, maxlen: 16 }
      warnings: { id: 1, type: array, items: { type: string, maxlen: 24, count: 4 } }
      matrix:   { id: 2, type: array, items: { type: array, count: 2, items: { type: u16, count: 8 } } }
`
	h, err := fixedHeader(t, src, "m.hpp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, resetLostClaim) {
		t.Errorf("fixed storage owns no heap, so reset() should not warn about element storage:\n%s", h)
	}
}

func TestResetDocCoversElementsItCannotSee(t *testing.T) {
	// Two shapes a value-level probe can miss: a struct element whose own string
	// happens to fit SSO in the payload at hand, and a matrix row. Both own heap
	// by construction under dynamic storage, so both are the doc's business.
	for _, tc := range []struct{ name, src string }{
		{"struct element", `
version: 1
$defs:
  struct:
    Code:
      text: { id: 0, type: string, maxlen: 8 }
messages:
  M:
    payload:
      codes: { id: 0, type: array, items: { type: struct, count: 4, fields: { $ref: '#/$defs/struct/Code' } } }
`},
		{"matrix row", `
version: 1
messages:
  M:
    payload:
      cells: { id: 0, type: array, items: { type: array, count: 4, items: { type: u16, count: 8 } } }
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := headerFromYAML(t, tc.src, "m.hpp")
			if !strings.Contains(h, resetLostClaim) {
				t.Errorf("%s owns heap storage that reset() gives back, and the doc does not say so:\n%s", tc.name, h)
			}
		})
	}
}

// A struct/union MEMBER is reached through its own reset(), so what that one
// gives back is given back by the owner too — and the owner's doc has to say it.
func TestResetDocFollowsNestedMembers(t *testing.T) {
	src := `
version: 1
$defs:
  struct:
    Inner:
      tags: { id: 0, type: array, items: { type: string, maxlen: 8, count: 4 } }
messages:
  M:
    payload:
      inner: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
`
	h := headerFromYAML(t, src, "m.hpp")
	if !strings.Contains(h, resetLostClaim) {
		t.Errorf("a nested member's elements are given back through the owner's reset(), and its doc does not say so:\n%s", h)
	}
}

// try_decode calls reset(), so its own doc must not promise more than reset()
// does either.
func TestTryDecodeDocDefersToReset(t *testing.T) {
	src := `
version: 1
messages:
  M:
    payload:
      warnings: { id: 0, type: array, items: { type: string, maxlen: 24, count: 4 } }
`
	h := headerFromYAML(t, src, "m.hpp")
	if strings.Contains(h, "without giving its buffers back") {
		t.Error("try_decode() doc still promises every buffer is kept")
	}
	if !strings.Contains(h, "keeping the buffers @ref reset keeps") {
		t.Errorf("try_decode() doc does not point at reset() for what reuse keeps:\n%s", h)
	}
}

// corelib: c-cpp with allow_dynamic: true is a shipped configuration (the
// bench's cpp-c-cpp-dyn row, the c-cpp-dynamic conformance profile) and its
// elements are std::string, exactly as on corelib-cpp. The STORAGE MODE decides
// what an element owns, not the corelib, so the caveat belongs there too.
func TestResetDocCoversDynamicCCpp(t *testing.T) {
	src := `
version: 1
messages:
  M:
    payload:
      warnings: { id: 0, type: array, items: { type: string, maxlen: 24, count: 4 } }
`
	h, err := genHeader(t, src, "m.hpp", map[string]any{
		"namespace": "sofabuffers", "corelib": "c-cpp", "allow_dynamic": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h, "std::vector<std::string>") {
		t.Fatalf("expected dynamic element storage on this profile:\n%s", h)
	}
	if !strings.Contains(h, resetLostClaim) {
		t.Errorf("c-cpp with dynamic storage loses the same element storage, and the doc does not say so:\n%s", h)
	}
}

// A struct MEMBER is reset, not destroyed: its own reset() clears its string and
// blob members in place, so their buffers survive exactly as a top-level field's
// do. Only what that member itself loses -- its arrays' elements, its unions --
// travels up. A member holding nothing but plain strings therefore loses
// nothing, and must not be told that an element's storage is handed back.
func TestResetDocSilentForPlainNestedMembers(t *testing.T) {
	src := `
version: 1
$defs:
  struct:
    Inner:
      text: { id: 0, type: string, maxlen: 16 }
      data: { id: 1, type: blob, maxlen: 16 }
messages:
  M:
    payload:
      inner: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      name:  { id: 1, type: string, maxlen: 16 }
`
	h := headerFromYAML(t, src, "m.hpp")
	if strings.Contains(h, resetLostClaim) {
		t.Errorf("a nested member of plain strings keeps every buffer, so reset() must not warn about element storage:\n%s", h)
	}
}

// The same type reached BOTH ways in one message: as a member (reset in place,
// nothing lost) and as an array element (destroyed, its string handed back).
// The cycle guard must answer it once per rule, not once in total, or the
// member visit silences the element one.
func TestResetDocMemberDoesNotMaskElement(t *testing.T) {
	src := `
version: 1
$defs:
  struct:
    Code:
      text: { id: 0, type: string, maxlen: 16 }
messages:
  M:
    payload:
      one:  { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Code' } }
      many: { id: 1, type: array, items: { type: struct, count: 4, fields: { $ref: '#/$defs/struct/Code' } } }
`
	h := headerFromYAML(t, src, "m.hpp")
	if !strings.Contains(h, resetLostClaim) {
		t.Errorf("the array of Code loses its elements' strings, and the doc does not say so:\n%s", h)
	}
}
