package golang

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGoReuseAgainstCorelib compiles <Msg>__TryDecode against a real corelib-go
// checkout and drives what the source-level tests (reuse_test.go) cannot: that a
// destination decoded into again and again reads as a fresh decode at every
// step -- a chain, not the single pair the conformance driver runs -- that an
// array which ARRIVES empty reads the same either way, that the storage really
// is reused, and that a message with no string decodes into a reused destination
// with no allocation at all. Gated on SOFAB_GO_CORELIB.
func TestGoReuseAgainstCorelib(t *testing.T) {
	corelib := os.Getenv("SOFAB_GO_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_GO_CORELIB to a corelib-go checkout to run the reuse driver")
	}
	files, err := (&Backend{}).Generate(schemaFromYAMLString(t, reuseCorelibDef), map[string]any{"package": "message"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, "message", f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(
		"module rt\n\ngo 1.24\n\nrequire github.com/sofa-buffers/corelib-go v0.0.0\n\nreplace github.com/sofa-buffers/corelib-go => "+corelib+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reuse_test.go"), []byte(goReuseDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"vet", "./..."}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
}

// Every storage-holding shape a struct keeps across Reset -- a blob with and
// without a default, a native array with a default, an fp32 array, a blob array
// (BlobSeq), a struct array (MessageSeq), a matrix, an array of blob arrays
// (NestedSeq) and a struct holding an array and a blob -- and no string, which
// is the one thing a Go decode must allocate.
const reuseCorelibDef = `version: 1
messages:
  probe:
    payload:
      n:  { id: 0, type: u32, default: 5 }
      b:  { id: 1, type: blob, maxlen: 16 }
      bd: { id: 2, type: blob, maxlen: 8, default: "AQI=" }
      a:  { id: 3, type: array, items: { type: u16, count: 8 }, default: [1, 2] }
      fa: { id: 4, type: array, items: { type: fp32, count: 4 } }
      ba: { id: 5, type: array, items: { type: blob, count: 4, maxlen: 8 } }
      ra: { id: 6, type: array, items: { type: struct, count: 4, fields: { z: { id: 0, type: u8, default: 2 } } } }
      mx: { id: 7, type: array, items: { type: array, count: 3, items: { type: u8, count: 4 } } }
      nb: { id: 8, type: array, items: { type: array, count: 2, items: { type: blob, count: 2, maxlen: 4 } } }
      st:
        id: 9
        type: struct
        fields:
          x:  { id: 0, type: i32, default: -3 }
          xs: { id: 1, type: array, items: { type: u8, count: 4 } }
          xb: { id: 2, type: blob, maxlen: 4 }
      u:
        id: 10
        type: union
        default_id: 0
        oneof:
          p: { id: 0, type: u8 }
          q: { id: 1, type: u16 }
          w: { id: 2, type: array, items: { type: blob, count: 2, maxlen: 4 } }
`

const goReuseDriver = `package rt

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
	msg "rt/message"
)

func full() *msg.Probe {
	m := msg.Probe__New()
	m.N = 7
	m.B = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}
	m.Bd = []byte{9, 9, 9}
	m.A = []uint16{4, 5, 6, 7}
	m.Fa = []float32{0.5, -1.5}
	m.Ba = [][]byte{{1}, nil, {2, 3, 4}}
	m.Ra = []msg.Probe_Ra{{Z: 1}, {Z: 2}, {Z: 9}}
	m.Mx = [][]uint8{{1, 2}, {3, 4, 5}}
	m.Nb = [][][]byte{{{1, 2}}, {{3}, {4, 5}}}
	m.St.X = 11
	m.St.Xs = []uint8{8, 9}
	m.St.Xb = []byte{7, 7}
	m.U.SetQ(300)
	return m
}

// sparse leaves most fields absent and the rest shorter, so a destination that
// held full must give every one of them back.
func sparse() *msg.Probe {
	m := msg.Probe__New()
	m.A = []uint16{}
	m.Ba = [][]byte{{5, 5}}
	m.Mx = [][]uint8{nil, {6}}
	m.St.Xs = []uint8{1}
	return m
}

func wire(t *testing.T, m *msg.Probe) []byte {
	t.Helper()
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func render(t *testing.T, m *msg.Probe) string {
	t.Helper()
	j, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(j)
}

// same holds the reused destination against a fresh decode of want: equal as
// JSON (which tells nil from empty) and re-encoding to the same bytes.
func same(t *testing.T, step string, got *msg.Probe, want []byte) {
	t.Helper()
	fresh, err := msg.Probe__Decode(want)
	if err != nil {
		t.Fatalf("%s: fresh decode: %v", step, err)
	}
	if g, f := render(t, got), render(t, fresh); g != f {
		t.Fatalf("%s: reused destination differs from a fresh decode\n reused: %s\n  fresh: %s", step, g, f)
	}
	if g := wire(t, got); !bytes.Equal(g, want) {
		t.Fatalf("%s: re-encode differs\n got %x\nwant %x", step, g, want)
	}
}

// A chain over one destination: every input after every other, the empty
// message and a refused one among them. The pair table of the conformance
// driver starts each pair from a plain Decode, so a spare that ALREADY holds
// storage when the next Reset runs -- the steady state -- is only reached here.
func TestChainReadsAsFreshAtEveryStep(t *testing.T) {
	f, s := wire(t, full()), wire(t, sparse())
	tail := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}
	steps := []struct {
		name string
		in   []byte
		want []byte // what the destination must read as; in itself unless refused
	}{
		{"full", f, f}, {"sparse", s, s}, {"full again", f, f}, {"empty", nil, nil},
		{"full after empty", f, f}, {"full", f, f}, {"sparse after full twice", s, s},
		{"refused full", append(append([]byte(nil), f...), tail...), f},
		{"sparse after refusal", s, s}, {"refused sparse", append(append([]byte(nil), s...), tail...), s},
		{"full last", f, f},
	}
	m := msg.Probe__New()
	for _, st := range steps {
		err := msg.Probe__TryDecode(st.in, m)
		refused := !bytes.Equal(st.in, st.want)
		if refused && !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Fatalf("%s: err = %v, want ErrInvalidMsg", st.name, err)
		}
		if !refused && err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		same(t, st.name, m, st.want)
	}
}

// A wrapper array that ARRIVES as an empty frame -- legal, though no encoder
// writes it for a plain field -- is the empty array, not an absent (nil) one,
// whether the destination is fresh or held storage before. With the reslice the
// arm used to emit, the two read differently: nil fresh, [] reused.
func TestEmptyWrapperFrameReadsAlike(t *testing.T) {
	buf := make([]byte, 64)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.WriteSequenceBeginLazy(5) // ba
	e.WriteSequenceEndKeep()
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	in := append([]byte(nil), e.Bytes()...)
	fresh, err := msg.Probe__Decode(in)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Ba == nil || len(fresh.Ba) != 0 {
		t.Fatalf("fresh decode of an empty frame: Ba = %#v, want empty and non-nil", fresh.Ba)
	}
	m, err := msg.Probe__Decode(wire(t, full()))
	if err != nil {
		t.Fatal(err)
	}
	if err := msg.Probe__TryDecode(in, m); err != nil {
		t.Fatal(err)
	}
	if g, f := render(t, m), render(t, fresh); g != f {
		t.Fatalf("empty frame: reused %s, fresh %s", g, f)
	}

	// The same for a ROW of an array of wrapper arrays that arrives empty: the
	// corelib's row collector opens it, never nil, over the storage a previous
	// decode left in that slot or over none.
	r := msg.Probe__New()
	r.Nb = [][][]byte{{{1}}, {}}
	in = wire(t, r)
	if fresh, err = msg.Probe__Decode(in); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Nb) != 2 || fresh.Nb[1] == nil {
		t.Fatalf("fresh decode of an empty row: Nb = %#v, want the last row empty and non-nil", fresh.Nb)
	}
	held0 := full()
	held0.Nb = [][][]byte{{{1}, {2}}, {{3}, {4}}}
	if m, err = msg.Probe__Decode(wire(t, held0)); err != nil {
		t.Fatal(err)
	}
	if err := msg.Probe__TryDecode(in, m); err != nil {
		t.Fatal(err)
	}
	if g, f := render(t, m), render(t, fresh); g != f {
		t.Fatalf("empty row: reused %s, fresh %s", g, f)
	}

	// The same for a union's wrapper-array option, which a union holding it
	// empty writes as exactly that frame (MESSAGE_SPEC §4.2).
	u := msg.Probe__New()
	u.U.SetW([][]byte{})
	in = wire(t, u)
	fresh, err = msg.Probe__Decode(in)
	if err != nil {
		t.Fatal(err)
	}
	if w := fresh.U.W(); !fresh.U.HasW() || w == nil || len(w) != 0 {
		t.Fatalf("fresh decode of a held empty option: W = %#v, want held, empty and non-nil", w)
	}
	held := full()
	held.U.SetW([][]byte{{1}, {2}})
	if m, err = msg.Probe__Decode(wire(t, held)); err != nil {
		t.Fatal(err)
	}
	if err := msg.Probe__TryDecode(in, m); err != nil {
		t.Fatal(err)
	}
	if g, f := render(t, m), render(t, fresh); g != f {
		t.Fatalf("held empty option: reused %s, fresh %s", g, f)
	}
}

// The storage is the destination's: a decode of the same shape writes into the
// very bytes the last one left, which is the documented half of the contract (a
// slice read before is overwritten).
func TestStorageIsReused(t *testing.T) {
	in := wire(t, full())
	m := msg.Probe__New()
	if err := msg.Probe__TryDecode(in, m); err != nil {
		t.Fatal(err)
	}
	b, a, ba, row, xs := &m.B[0], &m.A[0], &m.Ba[0][0], &m.Mx[1][0], &m.St.Xs[0]
	if err := msg.Probe__TryDecode(in, m); err != nil {
		t.Fatal(err)
	}
	for name, ok := range map[string]bool{
		"blob": b == &m.B[0], "native array": a == &m.A[0], "blob element": ba == &m.Ba[0][0],
		"matrix row": row == &m.Mx[1][0], "nested struct array": xs == &m.St.Xs[0],
	} {
		if !ok {
			t.Errorf("%s: the second decode allocated new storage instead of reusing it", name)
		}
	}
}

// allocs is the allocations per op of a fresh decode of in and of a decode of in
// into a destination that already holds it.
func allocs(t *testing.T, in []byte) (fresh, reused float64) {
	t.Helper()
	m := msg.Probe__New()
	if err := msg.Probe__TryDecode(in, m); err != nil {
		t.Fatal(err)
	}
	reused = testing.AllocsPerRun(100, func() {
		if err := msg.Probe__TryDecode(in, m); err != nil {
			t.Fatal(err)
		}
	})
	fresh = testing.AllocsPerRun(100, func() {
		if _, err := msg.Probe__Decode(in); err != nil {
			t.Fatal(err)
		}
	})
	return fresh, reused
}

// With no string and no wrapper array on the wire, a decode into a destination
// that already holds a message of the same shape allocates nothing at all; a
// fresh decode of the same bytes allocates every container.
func TestSteadyStateAllocatesNothing(t *testing.T) {
	m := full()
	m.Ba, m.Ra, m.Mx, m.Nb = nil, nil, nil, nil
	fresh, reused := allocs(t, wire(t, m))
	t.Logf("allocations per decode: fresh %v, reused %v", fresh, reused)
	if reused != 0 {
		t.Fatalf("a reused decode allocated %v times per op, want 0", reused)
	}
	if fresh == 0 {
		t.Fatal("a fresh decode allocated nothing: the reused figure proves nothing")
	}
}

// A wrapper array's elements are gathered by a corelib collector, and the
// collector itself is an allocation per sequence it gathers -- the corelib's,
// not the destination's storage. full() opens six: ba, ra, mx, nb and nb's two
// rows. Their elements and rows are reused, so that is all a reused decode
// pays.
func TestSteadyStateAllocatesOnlyCollectors(t *testing.T) {
	fresh, reused := allocs(t, wire(t, full()))
	t.Logf("allocations per decode: fresh %v, reused %v", fresh, reused)
	if reused > 6 {
		t.Fatalf("a reused decode allocated %v times per op, want at most the 6 collectors", reused)
	}
	if fresh <= reused {
		t.Fatalf("fresh %v <= reused %v: nothing is reused", fresh, reused)
	}
}
`
