package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigValidates(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "config", "sofabgen.yaml")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("example config should validate: %v", err)
	}
	eff := c.Effective("c")
	if eff["symbol_prefix"] != "myproj_" {
		t.Fatalf("expected per-target symbol_prefix to win, got %v", eff["symbol_prefix"])
	}
	if eff["license"] != "MIT" {
		t.Fatalf("expected generic license to apply, got %v", eff["license"])
	}
	if eff["emit"] != "sources" {
		t.Fatalf("expected built-in default emit=sources, got %v", eff["emit"])
	}
}

func TestConfigRejectsUnknownKey(t *testing.T) {
	_, err := Parse([]byte("generic:\n  bogus: 1\n"), "t.yaml")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected unknown-key rejection, got: %v", err)
	}
}

func TestConfigRejectsBadEnum(t *testing.T) {
	_, err := Parse([]byte("generic:\n  emit: nonsense\n"), "t.yaml")
	if err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("expected enum rejection, got: %v", err)
	}
}

func TestConfigRejectsUnknownTarget(t *testing.T) {
	_, err := Parse([]byte("targets:\n  cobol: {}\n"), "t.yaml")
	if err == nil || !strings.Contains(err.Error(), "cobol") {
		t.Fatalf("expected unknown-target rejection, got: %v", err)
	}
}
