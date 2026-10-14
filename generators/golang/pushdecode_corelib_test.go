package golang

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGoPushDecodeAgainstCorelib compiles the generated push decoder and
// EncodeInto against a real corelib-go checkout and drives them: a chunked
// decode that suspends INSIDE a field at every byte position, and an encode
// into a buffer one byte too short, which must refuse rather than truncate.
//
// The source-level test beside it pins the shape of what is emitted; only this
// one can tell whether the shape WORKS -- whether Feed really resumes where it
// suspended, and whether a short destination is reported instead of silently
// producing a prefix. Gated on SOFAB_GO_CORELIB.
func TestGoPushDecodeAgainstCorelib(t *testing.T) {
	corelib := os.Getenv("SOFAB_GO_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_GO_CORELIB to a corelib-go checkout to run the push-decode driver")
	}
	s := exampleSchema(t)
	files, err := (&Backend{}).Generate(s, map[string]any{"package": "message"})
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
	if err := os.WriteFile(filepath.Join(dir, "push_test.go"), []byte(goPushDriver), 0o644); err != nil {
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

const goPushDriver = `package rt

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
	msg "rt/message"
)

// sample is a message with a long string, a blob, arrays and a matrix, so a
// one-byte feed lands inside a varint, inside a payload, inside an array and
// inside a sequence -- every place the decoder has to suspend and resume.
func sample() *msg.Myfirstmessage {
	m := msg.Myfirstmessage__New()
	m.Someu32 = 123456
	m.Somei32 = -4242
	m.Someu64 = 18446744073709551000
	m.Somestring = "héllo wörld -- long enough to straddle"
	m.Someblob = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	m.Someuintarray = []uint32{9, 8, 7, 6}
	m.Somestringarray = []string{"a", "bb", "ccc"}
	m.Somematrix = [][]uint32{{1, 2, 3, 4}, {5, 6}}
	return m
}

// One byte per Feed. Every byte position in the message becomes a suspend
// point, so a decoder that lost its partial varint, its partial payload or its
// open sequence across a call cannot pass. The assertion is EQUIVALENCE with
// the in-memory path, not "it decoded": a chunked decode that dropped a field
// would still reach COMPLETE.
func TestPushDecoderResumesAtEveryByte(t *testing.T) {
	want, err := sample().Encode()
	if err != nil {
		t.Fatal(err)
	}
	d := msg.Myfirstmessage__NewDecoder()
	suspended := 0
	// One scratch byte, REUSED: a chunk is borrowed for the duration of the
	// call only, so a destination that kept a window into it decodes garbage.
	var chunk [1]byte
	for i := range want {
		chunk[0] = want[i]
		out, err := d.Feed(chunk[:])
		if err != nil {
			t.Fatalf("feed %d/%d: %v", i, len(want), err)
		}
		if out == sofab.Incomplete {
			suspended++
		}
		chunk[0] = 0xff
	}
	if suspended == 0 {
		t.Fatal("no feed ever suspended mid-field: the drip is not exercising resume")
	}
	got, err := d.Feed(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != sofab.Complete {
		t.Fatalf("final outcome = %v, want Complete", got)
	}
	again, err := d.Message().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, again) {
		t.Fatalf("chunked decode drifted:\n %x\n %x", want, again)
	}
	whole, err := msg.Myfirstmessage__Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	if d.Message().Somestring != whole.Somestring || len(d.Message().Somematrix) != len(whole.Somematrix) {
		t.Fatalf("chunked decode != whole decode:\n %#v\n %#v", d.Message(), whole)
	}
}

// Message is the destination, filled in place: taken once, before a byte is
// fed, it is still the object the decode lands in.
func TestPushDecoderMessageIsTheDestination(t *testing.T) {
	b, err := sample().Encode()
	if err != nil {
		t.Fatal(err)
	}
	d := msg.Myfirstmessage__NewDecoder()
	m := d.Message()
	if m.Somestring != msg.Myfirstmessage__New().Somestring {
		t.Fatal("a fresh decoder's message must carry the schema defaults")
	}
	if _, err := d.Feed(b); err != nil {
		t.Fatal(err)
	}
	if m.Somestring != sample().Somestring {
		t.Fatalf("the message taken up front was not filled: %q", m.Somestring)
	}
}

// An INVALID verdict is terminal: the decoder keeps answering it.
func TestPushDecoderRefusalIsTerminal(t *testing.T) {
	// someuintarray declares count: 4 at id 15 (header 0x7b); 5 elements is
	// over the schema bound and INVALID whatever follows.
	bad := []byte{0x7b, 0x05, 1, 2, 3, 4, 5}
	d := msg.Myfirstmessage__NewDecoder()
	out, err := d.Feed(bad)
	if err == nil || out != sofab.Invalid {
		t.Fatalf("over-count array must be Invalid, got %v / %v", out, err)
	}
	if out2, err2 := d.Feed(nil); out2 != sofab.Invalid || err2 == nil {
		t.Fatalf("a refusal must be terminal, got %v / %v", out2, err2)
	}
}

// EncodeInto writes into the CALLER's buffer, and refuses one that is too
// short instead of handing back a prefix.
func TestEncodeIntoCallerBuffer(t *testing.T) {
	m := sample()
	want, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, len(want))
	n, err := m.EncodeInto(dst)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) || !bytes.Equal(dst[:n], want) {
		t.Fatalf("EncodeInto wrote %d bytes, want %d:\n %x\n %x", n, len(want), dst[:n], want)
	}

	short := make([]byte, len(want)-1)
	n, err = m.EncodeInto(short)
	if !errors.Is(err, sofab.ErrBufferFull) {
		t.Fatalf("a short destination must be ErrBufferFull, got %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused encode must report 0 bytes, got %d", n)
	}

	// No storage is allocated FOR THE MESSAGE: dst is the whole of it, and the
	// corelib grows nothing (CORELIB_PLAN §5.1). What is left is the Encoder
	// value itself, one per call and the same one whatever the message holds --
	// so the count must not grow with the message, and it must be below what
	// Encode pays, which allocates the output as well.
	small := msg.Myfirstmessage__New()
	big := sample()
	aSmall := testing.AllocsPerRun(50, func() { small.EncodeInto(dst) })
	aBig := testing.AllocsPerRun(50, func() { big.EncodeInto(dst) })
	if aSmall != aBig {
		t.Errorf("EncodeInto allocations grow with the message: %v -> %v", aSmall, aBig)
	}
	aEncode := testing.AllocsPerRun(50, func() { big.Encode() })
	if aBig >= aEncode {
		t.Errorf("EncodeInto allocated %v per encode, Encode %v: the caller's buffer bought nothing", aBig, aEncode)
	}
}

// The generated options are what the generated encoders use: an encode driven
// by hand with them produces the same bytes, which is the drift the exported
// accessor exists to prevent.
func TestEncodeOptionsDriveTheSameEncoder(t *testing.T) {
	m := sample()
	want, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	scratch := make([]byte, 64)
	e, err := sofab.NewEncoderSink(scratch, 0, func(_ *sofab.Encoder, b []byte) error {
		out.Write(b)
		return nil
	}, msg.Myfirstmessage__EncodeOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	m.Serialize(e)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("hand-driven encode drifted:\n %x\n %x", out.Bytes(), want)
	}
}
`
