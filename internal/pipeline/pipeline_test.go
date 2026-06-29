package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunExampleBuildsIR(t *testing.T) {
	def := filepath.Join("..", "..", "examples", "messages", "example.yaml")
	res, err := Run(Options{DefPath: def})
	if err != nil {
		t.Fatalf("example.yaml should run clean, got: %v", err)
	}
	if res.Schema == nil || len(res.Schema.Messages) != 1 {
		t.Fatalf("expected 1 message, got %+v", res.Schema)
	}
	if res.Schema.Messages[0].Name != "myfirstmessage" {
		t.Fatalf("unexpected message name %q", res.Schema.Messages[0].Name)
	}
	// 13 payload fields in the example.
	if got := len(res.Schema.Messages[0].Fields); got != 13 {
		t.Fatalf("expected 13 fields, got %d", got)
	}
}

func TestRunNoBackendSignalled(t *testing.T) {
	def := filepath.Join("..", "..", "examples", "messages", "example.yaml")
	res, err := Run(Options{DefPath: def, Lang: "c"})
	var nb *NoBackendError
	if !errors.As(err, &nb) {
		t.Fatalf("expected NoBackendError for unwired lang, got: %v", err)
	}
	if res == nil || res.Schema == nil {
		t.Fatalf("IR should still be built even without a backend")
	}
}

func TestRunInvalidFailsClosed(t *testing.T) {
	// Reuse a deliberately invalid definition written to a temp file.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\nmessages:\n  M:\n    payload:\n      a: {id: 0, type: u8, default: 999}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Options{DefPath: bad}); err == nil {
		t.Fatalf("expected validation failure")
	}
}
