package main

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
)

// exampleCfg is the config this repository ships and its README points at. It
// is the first file a user copies, so it is also the one a new option must not
// break.
const exampleCfg = "../../examples/config/sofabgen.yaml"

// boundedDef is a definition EVERY target accepts. exampleDef is not: it has an
// unbounded field, which the fixed-storage C target refuses by design, and the
// subject here is the config plumbing, not the object models.
const boundedDef = "../../tests/matrix/corpus/defs/scalars.yaml"

// Every known target must generate from the shipped example config.
//
// This exists because a GENERIC config key silently shadows a per-target one of
// the same name: Config.Effective merges targets.<lang> over generic, so a
// generic `format` and the docs target's own `format: html` would be read as
// each other -- the docs target's `html` as a format mode at startup, and a
// generic `auto` as a documentation output format inside the docs backend.
// Nothing else catches that: tests/matrix drives Generate directly, and there
// is no docs conformance suite. Here the real CLI runs over the real config,
// for every target at once, so the next generic key that collides fails here.
func TestExampleConfigGeneratesForEveryTarget(t *testing.T) {
	for _, lang := range config.KnownTargets() {
		t.Run(lang, func(t *testing.T) {
			out := t.TempDir()
			code, errOut := runCLI(t, "--config", exampleCfg, "--lang", lang, "--in", boundedDef, "--out", out)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if files := generatedFiles(t, out); len(files) == 0 {
				t.Fatalf("no files written (stderr: %s)", errOut)
			}
		})
	}
}

// The docs target reads its OWN `format` option, and the generic format switch
// must not reach it under any name or any mode. Both halves are checked on the
// one target that has a `format` option of its own: the generic switch set to a
// mode the docs backend would refuse, and the docs option set to a value the
// format switch would refuse.
func TestTheFormatSwitchAndTheDocsFormatOptionAreIndependent(t *testing.T) {
	t.Run("generic_run_formatter_does_not_reach_the_docs_backend", func(t *testing.T) {
		cfg := writeConfig(t, "generic:\n  run_formatter: auto\ntargets:\n  docs:\n    format: html\n")
		out := t.TempDir()
		code, errOut := runCLI(t, "--config", cfg, "--lang", "docs", "--in", exampleDef, "--out", out)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		files := generatedFiles(t, out)
		if len(files) == 0 {
			t.Fatal("no documentation was written")
		}
		for _, f := range files {
			if strings.HasSuffix(f, ".html") {
				return
			}
		}
		t.Errorf("no .html page among %v", files)
	})

	t.Run("the_docs_format_option_is_not_read_as_a_format_mode", func(t *testing.T) {
		// `html` is not a format mode; if the switch read this key the run
		// would abort before generating anything.
		cfg := writeConfig(t, "targets:\n  docs:\n    format: html\n")
		out := t.TempDir()
		code, errOut := runCLI(t, "--config", cfg, "--lang", "docs", "--in", exampleDef, "--out", out)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if files := generatedFiles(t, out); len(files) == 0 {
			t.Fatalf("no files written (stderr: %s)", errOut)
		}
	})

	// A per-target `run_formatter` is not a thing: the switch is generic, and
	// the closed schema is what says so.
	t.Run("run_formatter_is_generic_only", func(t *testing.T) {
		cfg := writeConfig(t, "targets:\n  rust:\n    run_formatter: auto\n")
		code, errOut := runCLI(t, "--config", cfg, "--lang", "rust", "--in", exampleDef, "--out", t.TempDir())
		if code == 0 {
			t.Fatal("targets.rust.run_formatter was accepted")
		}
		if !strings.Contains(errOut, "run_formatter") {
			t.Errorf("the error does not name the key: %q", errOut)
		}
	})
}

// The example config must stay the one the docs describe: a reader who copies
// it gets the docs target's own option, not the format switch.
func TestExampleConfigDoesNotSetTheFormatSwitch(t *testing.T) {
	body, err := os.ReadFile(exampleCfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "run_formatter") {
		t.Error("the example config sets the format switch; the default (off) is what a copied config should inherit")
	}
}
