package golang

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// deepSchema nests every sequence-opening shape the marshal emits -- struct
// field, union arm, array of struct, wrapper rows of wrapper rows, string and
// blob wrappers -- so the deepest path (rows -> row -> element -> leaves ->
// leaf -> tags) opens six frames at once.
const deepSchema = `
version: 1
$defs:
  struct:
    Leaf:
      tags: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }
      n: { id: 1, type: u32 }
    Mid:
      leaves: { id: 0, type: array, items: { type: struct, count: 2, fields: { $ref: '#/$defs/struct/Leaf' } } }
      cube:
        id: 1
        type: array
        items: { type: array, count: 2, items: { type: array, count: 2, items: { type: blob, count: 2, maxlen: 4 } } }
messages:
  Deep:
    payload:
      mid: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Mid' } }
      rows:
        id: 1
        type: array
        items: { type: array, count: 2, items: { type: struct, count: 2, fields: { $ref: '#/$defs/struct/Mid' } } }
      u:
        id: 2
        type: union
        default_id: 0
        oneof:
          x: { id: 0, type: u16 }
          m: { id: 1, type: struct, fields: { $ref: '#/$defs/struct/Mid' } }
`

// Every encode entry point passes the message's static nesting depth to the
// corelib (sofab.WithMaxDepth), so the Encoder sizes its lazy-sequence id stack
// to the schema instead of MaxDepth. The depth must count exactly the frames the
// marshal opens: one short and a valid value fails with ErrArgument, which the
// corelib-gated TestGoEncodeMaxDepthRoundTrip below proves on real encodes.
func TestGoEncodeMaxDepth(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		depth         int
	}{
		// No frame at all: the constant is 0, and the option still passes 1,
		// because WithMaxDepth(0) means "no bound" (MaxDepth).
		{"scalars only", `a: { id: 0, type: u32 }`, 0},
		{"native array", `v: { id: 0, type: array, items: { type: u32, count: 4 } }`, 0},
		// The outer wrapper opens a frame; each native row is one value inside it.
		{"native rows", `m: { id: 0, type: array, items: { type: array, count: 2, items: { type: u8, count: 2 } } }`, 1},
		{"string array", `s: { id: 0, type: array, items: { type: string, count: 2, maxlen: 4 } }`, 1},
		{"wrapper rows of strings", `
      c:
        id: 0
        type: array
        items: { type: array, count: 2, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }`, 3},
		// Unbounded: Encode takes the sink arm, which must pass the bound too.
		{"unbounded string array", `s: { id: 0, type: array, items: { type: string } }`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := genGo(t, schemaFromYAMLString(t, "version: 1\nmessages:\n  d:\n    payload:\n      "+strings.TrimSpace(tc.payload)+"\n"), map[string]any{})["d.go"]
			arg := "DMaxDepth"
			if tc.depth == 0 {
				arg = "1"
			}
			for _, want := range []string{
				"const DMaxDepth = " + strconv.Itoa(tc.depth) + "\n",
				"var _DEncOpts = []sofab.Option{sofab.WithMaxDepth(" + arg + ")}",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			// Every Encoder the file constructs takes the bound: Encode (buffer or
			// sink arm) and EncodeTo, and no constructor is left without it.
			if n, all := strings.Count(got, "_DEncOpts...)"), strings.Count(got, "sofab.NewEncoder"); n != all || n != 2 {
				t.Errorf("%d of %d encoder constructors pass the bound, want 2 of 2:\n%s", n, all, got)
			}
		})
	}

	deep := genGo(t, schemaFromYAMLString(t, deepSchema), map[string]any{})["deep.go"]
	if !strings.Contains(deep, "const DeepMaxDepth = 6\n") {
		t.Errorf("deepSchema opens six frames at once:\n%s", deep)
	}
}

// TestGoEncodeMaxDepthRoundTrip builds deepSchema against a real corelib-go and
// fills the deepest path of every branch, so every frame the depth counts is
// actually opened. The value must encode through both constructor families
// (Encode's buffer, EncodeTo's sink), decode, and re-encode byte-identically;
// an Encoder bounded ONE level tighter than DeepMaxDepth must refuse the same
// value with ErrArgument -- which pins the count from both sides. Gated on
// SOFAB_GO_CORELIB (a checkout that has sofab.WithMaxDepth).
func TestGoEncodeMaxDepthRoundTrip(t *testing.T) {
	corelib := os.Getenv("SOFAB_GO_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_GO_CORELIB to a corelib-go checkout to run the depth round trip")
	}
	files, err := (&Backend{}).Generate(schemaFromYAMLString(t, deepSchema), map[string]any{"package": "message"})
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
	if err := os.WriteFile(filepath.Join(dir, "rt_test.go"), []byte(goDepthDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
}

const goDepthDriver = `package rt

import (
	"bytes"
	"errors"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
	msg "rt/message"
)

func mid() msg.StructMid {
	return msg.StructMid{
		Leaves: []msg.StructLeaf{{N: 1}, {Tags: []string{"a", "bc"}, N: 2}},
		Cube:   [][][][]byte{{{{1}}, {{2, 3}, {4}}}, {{{5}}}},
	}
}

func deep() *msg.Deep {
	m := msg.NewDeep()
	m.Mid = mid()
	m.Rows = [][]msg.StructMid{{mid()}, {mid(), mid()}}
	m.U.M = mid()
	return m
}

func TestDeepRoundTrip(t *testing.T) {
	if msg.DeepMaxDepth != 6 {
		t.Fatalf("DeepMaxDepth = %d, want 6", msg.DeepMaxDepth)
	}
	m := deep()
	enc, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode at the schema's full depth: %v", err)
	}
	var sink bytes.Buffer
	if err := m.EncodeTo(&sink); err != nil {
		t.Fatalf("EncodeTo at the schema's full depth: %v", err)
	}
	if !bytes.Equal(enc, sink.Bytes()) {
		t.Fatalf("Encode and EncodeTo disagree:\n %x\n %x", enc, sink.Bytes())
	}
	got, err := msg.DecodeDeep(enc)
	if err != nil {
		t.Fatal(err)
	}
	again, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, again) {
		t.Fatalf("re-encode differs:\n want %x\n got  %x", enc, again)
	}
}

// One level tighter than the generated bound must refuse the same value: the
// bound is the exact depth the marshal reaches, not merely a safe one.
func TestDeepBoundIsTight(t *testing.T) {
	buf := make([]byte, msg.DeepMaxSize)
	e, err := sofab.NewEncoderBuffer(buf, 0, sofab.WithMaxDepth(msg.DeepMaxDepth-1))
	if err != nil {
		t.Fatal(err)
	}
	deep().Serialize(e)
	if err := e.Flush(); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("depth %d must refuse the value with ErrArgument, got %v", msg.DeepMaxDepth-1, err)
	}
}
`
