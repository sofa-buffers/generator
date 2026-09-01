package python

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The reassembly buffer corelib-py joins a split construct in is the CALLER's
// storage, required and never defaulted (corelib-py#139, CORELIB_PLAN §6.2.1 /
// §6.6.2). These pin the number this backend derives for it, and the dimension
// it is derived in: BYTES that one construct can reach, not a message's size and
// not an element count.

var reSpan = regexp.MustCompile(`(?m)^MAX_FIELD_SPAN = (\d+)$`)

func spanOf(t *testing.T, src string, cfg map[string]any) int64 {
	t.Helper()
	mod := string(genPy(t, schema(t, src), cfg)["message.py"])
	m := reSpan.FindStringSubmatch(mod)
	if m == nil {
		t.Fatalf("no MAX_FIELD_SPAN emitted:\n%s", mod)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A schema whose every construct is small still has to clear the corelib's floor
// (sofab.MIN_REASSEMBLY): a buffer that cannot hold a construct's framing is not
// a smaller buffer, it is one the constructor refuses.
func TestPythonReassemblyClearsTheCorelibFloor(t *testing.T) {
	got := spanOf(t, `
version: 1
messages:
  tiny:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: u8 }
`, nil)
	if got != minReassembly {
		t.Errorf("MAX_FIELD_SPAN = %d, want the floor %d", got, minReassembly)
	}
}

// The dimension: a cap on an ARRAY is an element count, and the buffer holds
// bytes. An unbounded u64 array capped at 100 elements needs the 100 elements'
// worth of varints, each at its widest (§4.1 obliges a decoder to accept a
// non-minimal encoding), not 100 bytes.
func TestPythonReassemblyIsBytesNotACount(t *testing.T) {
	got := spanOf(t, `
version: 1
messages:
  m:
    payload:
      arr: { id: 0, type: array, items: { type: u64 } }
`, map[string]any{"max_dyn_array_count": 100})
	if got < 1000 {
		t.Errorf("MAX_FIELD_SPAN = %d; 100 u64 elements are at least 1000 bytes", got)
	}
}

// A nested message is not ONE construct. corelib-py reads a sequence field by
// field, so what has to be resident at once is its largest single field -- never
// the sum. Sizing this like the C++ refusal ceiling (which sums, because
// over-estimating a ceiling is free) would allocate the difference on every
// decoder.
func TestPythonReassemblyDoesNotSumASequencesFields(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      nested:
        id: 0
        type: struct
        fields:
          a: { id: 0, type: string, maxlen: 1000 }
          b: { id: 1, type: string, maxlen: 1000 }
          c: { id: 2, type: string, maxlen: 1000 }
`
	got := spanOf(t, src, nil)
	if got < 1000 {
		t.Fatalf("MAX_FIELD_SPAN = %d; one 1000-byte string must fit", got)
	}
	if got > 1100 {
		t.Errorf("MAX_FIELD_SPAN = %d; the three fields were summed, "+
			"but only one of them is ever resident", got)
	}
}

// The same for a wrapper array: its elements are separate constructs, one per
// sequence child, so a 500-element array of 1000-byte strings needs room for one
// element and not for five hundred.
func TestPythonReassemblyDoesNotSumAWrapperArraysElements(t *testing.T) {
	got := spanOf(t, `
version: 1
messages:
  m:
    payload:
      names: { id: 0, type: array, items: { type: string, maxlen: 1000, count: 500 } }
`, nil)
	if got < 1000 {
		t.Fatalf("MAX_FIELD_SPAN = %d; one 1000-byte element must fit", got)
	}
	if got > 1100 {
		t.Errorf("MAX_FIELD_SPAN = %d; the whole element run was charged, "+
			"but a wrapper array is read one element at a time", got)
	}
}

// A native array is the opposite case and must NOT be split: its elements are
// one count-prefixed payload the corelib reads in a single pass, so the whole
// run has to be resident.
func TestPythonReassemblyChargesANativeArrayWhole(t *testing.T) {
	got := spanOf(t, `
version: 1
messages:
  m:
    payload:
      samples: { id: 0, type: array, items: { type: u32, count: 500 } }
`, nil)
	if got < 500*5 {
		t.Errorf("MAX_FIELD_SPAN = %d; a native array's whole element run is "+
			"one payload and has to fit", got)
	}
}

// Where the caps come in: a field the schema leaves unbounded is sized by the
// receiver cap that governs it, because that cap is exactly what this peer will
// accept. Raising the cap raises the buffer.
func TestPythonReassemblyFollowsTheConfiguredCaps(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      b: { id: 0, type: blob }
`
	small := spanOf(t, src, map[string]any{"max_dyn_blob_len": 4096})
	large := spanOf(t, src, map[string]any{"max_dyn_blob_len": 1 << 20})
	if small >= large {
		t.Errorf("span did not follow the cap: %d vs %d", small, large)
	}
	if large < 1<<20 {
		t.Errorf("MAX_FIELD_SPAN = %d; a 1 MiB blob must fit", large)
	}
}

// The streaming reader holds the chunk it was just fed on top of the construct,
// and only the caller knows how large a chunk is -- so decoder() takes the size,
// and the default states the assumption instead of hiding it.
func TestPythonStreamDecoderTakesItsOwnSize(t *testing.T) {
	mod := string(genPy(t, schema(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
`), nil)["message.py"])
	for _, want := range []string{
		"REASSEMBLY = MAX_FIELD_SPAN + 65536",
		"def decoder(cls, reassembly: int = REASSEMBLY) -> _StreamDecoder:",
		"def __init__(self, msg_cls, vis_cls, reassembly=REASSEMBLY) -> None:",
		"                          reassembly=reassembly)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
}
