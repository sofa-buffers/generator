package matrix

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/pipeline"
)

// A directory of two message files and a $defs-only library, which one of the
// message files also imports by cross-file $ref (so the library's Vec reaches
// the schema twice: as the library's own entry and as an import).
var multiFileDir = map[string]string{
	"a_msg.yaml": `version: 1
messages:
  m_one:
    payload:
      pos: { id: 0, type: struct, fields: { $ref: 'z_lib.yaml#/$defs/struct/Vec' } }
      mode: { id: 1, type: enum, enum: { $ref: 'z_lib.yaml#/$defs/enum/Mode' }, default: 0 }
`,
	"b_msg.yaml": `version: 1
$defs:
  struct:
    Seg: { n: { id: 0, type: u16 } }
messages:
  m_two:
    payload:
      seg: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Seg' } }
      tag: { id: 1, type: string, maxlen: 8 }
`,
	"z_lib.yaml": `version: 1
$defs:
  struct:
    Vec: { x: { id: 0, type: fp32 }, y: { id: 1, type: fp32 } }
    Spare: { k: { id: 0, type: u8 } }
  enum:
    Mode: { OFF: 0, ON: 1 }
`,
}

// The same schema written as one file: what the directory must generate.
const multiFileOne = `version: 1
$defs:
  struct:
    Seg: { n: { id: 0, type: u16 } }
    Vec: { x: { id: 0, type: fp32 }, y: { id: 1, type: fp32 } }
    Spare: { k: { id: 0, type: u8 } }
  enum:
    Mode: { OFF: 0, ON: 1 }
messages:
  m_one:
    payload:
      pos: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Vec' } }
      mode: { id: 1, type: enum, enum: { $ref: '#/$defs/enum/Mode' }, default: 0 }
  m_two:
    payload:
      seg: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Seg' } }
      tag: { id: 1, type: string, maxlen: 8 }
`

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDirectoryIsOneSchema: a directory generates, in every backend, exactly
// what the same definitions written as one file generate — every message and
// every type once, from one backend run — rather than one run per file into
// the same output, where single-module targets kept only the last file.
func TestDirectoryIsOneSchema(t *testing.T) {
	dir := writeTree(t, multiFileDir)
	one := writeTree(t, map[string]string{"one.yaml": multiFileOne})
	cfg := config.Empty()
	for _, lang := range generator.Registered() {
		t.Run(lang, func(t *testing.T) {
			got, err := pipeline.Run(pipeline.Options{DefPath: dir, Lang: lang, Config: cfg})
			if err != nil {
				t.Fatalf("directory: %v", err)
			}
			want, err := pipeline.Run(pipeline.Options{DefPath: filepath.Join(one, "one.yaml"), Lang: lang, Config: cfg})
			if err != nil {
				t.Fatalf("single file: %v", err)
			}
			if len(got.Schema.Messages) != 2 {
				t.Errorf("want 2 messages in the IR, got %d", len(got.Schema.Messages))
			}
			if n := len(got.Schema.Named); n != len(want.Schema.Named) {
				t.Errorf("named types: directory %d, single file %d", n, len(want.Schema.Named))
			}
			wantFiles := map[string][]byte{}
			for _, f := range want.Files {
				wantFiles[f.Path] = f.Content
			}
			seen := map[string]bool{}
			var all []byte
			for _, f := range got.Files {
				if seen[f.Path] {
					t.Errorf("%s emitted twice", f.Path)
				}
				seen[f.Path] = true
				all = append(all, f.Content...)
				w, ok := wantFiles[f.Path]
				if !ok {
					t.Errorf("%s: not generated for the single-file schema", f.Path)
					continue
				}
				if !bytes.Equal(w, f.Content) {
					t.Errorf("%s differs from the single-file schema's", f.Path)
				}
			}
			for p := range wantFiles {
				if !seen[p] {
					t.Errorf("%s: missing from the directory's output", p)
				}
			}
			// Every message and every type a message uses. The unused library
			// struct Spare is emitted where a single file's unused $defs entry
			// is (the byte comparison above holds that).
			low := strings.ToLower(string(all))
			for _, name := range []string{"one", "two", "vec", "seg", "mode"} {
				if !strings.Contains(low, name) {
					t.Errorf("output never mentions %q", name)
				}
			}
		})
	}
}

// TestRealworldDirectoryMatchesItsMessageFile: the checked-in multi-file
// example, given as a directory, is the schema its message file already pulls
// together by cross-file $ref (every library definition is used), so every
// backend writes the same bytes for both inputs.
func TestRealworldDirectoryMatchesItsMessageFile(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "messages", "realworld")
	for _, lang := range generator.Registered() {
		t.Run(lang, func(t *testing.T) {
			got, err := pipeline.Run(pipeline.Options{DefPath: dir, Lang: lang, Config: config.Empty()})
			if err != nil {
				t.Fatalf("directory: %v", err)
			}
			want, err := pipeline.Run(pipeline.Options{DefPath: filepath.Join(dir, "vehicle_telemetry.yaml"), Lang: lang, Config: config.Empty()})
			if err != nil {
				t.Fatalf("message file: %v", err)
			}
			if len(got.Files) != len(want.Files) {
				t.Fatalf("directory wrote %d files, message file %d", len(got.Files), len(want.Files))
			}
			for i := range got.Files {
				if got.Files[i].Path != want.Files[i].Path || !bytes.Equal(got.Files[i].Content, want.Files[i].Content) {
					t.Errorf("%s differs", got.Files[i].Path)
				}
			}
		})
	}
}

// TestDirectoryDuplicateNameRefused: two files defining one name differently
// are an ambiguous schema — a located error naming both files, not one file's
// output overwriting the other's.
func TestDirectoryDuplicateNameRefused(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "message",
			files: map[string]string{
				"a.yaml": "version: 1\nmessages:\n  m:\n    payload:\n      x: { id: 0, type: u8 }\n",
				"b.yaml": "version: 1\nmessages:\n  M:\n    payload:\n      y: { id: 0, type: string, maxlen: 4 }\n",
			},
			want: []string{`b.yaml#/messages/M: "M" is already defined by a.yaml#/messages/m;`},
		},
		{
			name: "defs",
			files: map[string]string{
				"a.yaml": "version: 1\n$defs:\n  struct:\n    P: { x: { id: 0, type: u8 } }\n",
				"b.yaml": "version: 1\n$defs:\n  enum:\n    p: { A: 0 }\n",
			},
			want: []string{`b.yaml#/$defs/enum/p: "p" is already defined by a.yaml#/$defs/struct/P;`},
		},
		{
			name: "imported vs library",
			files: map[string]string{
				"a.yaml":     "version: 1\nmessages:\n  m:\n    payload:\n      p: { id: 0, type: struct, fields: { $ref: 'lib/p.yaml#/$defs/struct/P' } }\n",
				"b.yaml":     "version: 1\n$defs:\n  struct:\n    P: { y: { id: 0, type: u16 } }\n",
				"lib/p.yaml": "version: 1\n$defs:\n  struct:\n    P: { x: { id: 0, type: u8 } }\n",
			},
			want: []string{`lib/p.yaml#/$defs/struct/P (imported by a.yaml)`, `b.yaml#/$defs/struct/P`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, tc.files)
			_, err := pipeline.Run(pipeline.Options{DefPath: dir, Lang: "python", Config: config.Empty()})
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%v", w, err)
				}
			}
		})
	}
}

// TestDirectoryIdenticalDefinitionsMerge: two files carrying a deep-equal
// definition under one name are one type, as for cross-file imports.
func TestDirectoryIdenticalDefinitionsMerge(t *testing.T) {
	p := "version: 1\n$defs:\n  struct:\n    P: { x: { id: 0, type: u8 } }\n"
	dir := writeTree(t, map[string]string{"a.yaml": p, "b.yaml": p})
	res, err := pipeline.Run(pipeline.Options{DefPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Schema.Named) != 1 {
		t.Fatalf("want one type, got %v", res.Schema.NamedOrder)
	}
}

// TestDirectoryFileErrorsStayLocated: each file is validated on its own, and
// its errors name it.
func TestDirectoryFileErrorsStayLocated(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.yaml": "version: 1\nmessages:\n  m:\n    payload:\n      x: { id: 0, type: u8, default: 999 }\n",
		"b.yaml": "version: 1\nmessages:\n  n:\n    payload:\n      y: { id: 0, type: u8 }\n",
	})
	_, err := pipeline.Run(pipeline.Options{DefPath: dir})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "a.yaml")+": ") ||
		!strings.Contains(err.Error(), "#/messages/m/payload/x") {
		t.Fatalf("want a.yaml's located error, got %v", err)
	}
}
