package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInDirectoryEndToEnd runs the CLI over a directory of two message files
// and a $defs-only library: the output must hold both messages and the
// library's types. Before a directory was one schema, each file ran on its own
// into the same output, and single-module targets (python's message.py) kept
// only the last file's messages while Go's shared types file depended on the
// file order.
func TestInDirectoryEndToEnd(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	in := t.TempDir()
	files := map[string]string{
		// sorts first, so a per-file run would generate it before the library
		"a_msg.yaml": "version: 1\nmessages:\n  m_one:\n    payload:\n      pos: { id: 0, type: struct, fields: { $ref: 'z_lib.yaml#/$defs/struct/Vec' } }\n",
		"b_msg.yaml": "version: 1\nmessages:\n  m_two:\n    payload:\n      mode: { id: 0, type: enum, enum: { $ref: 'z_lib.yaml#/$defs/enum/Mode' }, default: 0 }\n",
		"z_lib.yaml": "version: 1\n$defs:\n  struct:\n    Vec: { x: { id: 0, type: fp32 } }\n  enum:\n    Mode: { OFF: 0, ON: 1 }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(in, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, lang := range []string{"python", "go", "typescript"} {
		t.Run(lang, func(t *testing.T) {
			out := t.TempDir()
			if code := run([]string{"--lang", lang, "--in", in, "--out", out}, devnull, devnull); code != 0 {
				t.Fatalf("exit %d", code)
			}
			var all strings.Builder
			filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					b, _ := os.ReadFile(p)
					all.Write(b)
				}
				return nil
			})
			for _, want := range []string{"MOne", "MTwo", "Vec", "Mode"} {
				if !strings.Contains(all.String(), want) {
					t.Errorf("generated %s never mentions %s", lang, want)
				}
			}
		})
	}

	// A second definition of m_one in another file is refused, naming both.
	if err := os.WriteFile(filepath.Join(in, "c_dup.yaml"),
		[]byte("version: 1\nmessages:\n  mOne:\n    payload:\n      z: { id: 0, type: u8 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errf, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer errf.Close()
	if code := run([]string{"--lang", "python", "--in", in, "--out", t.TempDir()}, devnull, errf); code != 1 {
		t.Fatalf("duplicate message: exit %d, want 1", code)
	}
	msg, _ := os.ReadFile(errf.Name())
	if !strings.Contains(string(msg), `c_dup.yaml#/messages/mOne: "mOne" is already defined by a_msg.yaml#/messages/m_one`) {
		t.Errorf("unexpected error output:\n%s", msg)
	}
}
