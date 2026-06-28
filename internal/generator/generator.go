// Package generator holds the backend CONTRACT only — interfaces, no language
// code (PLAN §8.6/§8.7). The core pipeline depends on this interface; concrete
// backends live under generators/<lang> and register themselves here. The core
// imports no concrete backend, so dependency arrows point inward.
//
// This package is the Language Selection Point: pipeline builds the IR, then
// looks up the Backend registered for --lang and hands it the frozen IR. No
// backend is wired in M0 (no targets yet); the registry is the seam they slot
// into.
package generator

import (
	"fmt"
	"sort"

	"github.com/sofa-buffers/generator/internal/ir"
)

// File is one generated source file produced by a backend.
type File struct {
	Path    string // path relative to the output dir
	Content []byte
}

// Backend is the contract every language generator implements. A backend
// traverses the frozen IR (Visitor) and emits files (Builder); it must treat
// the IR as read-only (§8.6).
type Backend interface {
	// Lang is the --lang key this backend answers to (e.g. "c", "go").
	Lang() string
	// Generate emits source files for the schema under the resolved config.
	Generate(s *ir.Schema, cfg map[string]any) ([]File, error)
}

var registry = map[string]Backend{}

// Register makes a backend available for selection. Backends call this from an
// init() in their own package; the CLI blank-imports the backend packages it
// ships. Panics on a duplicate registration (a build-time wiring bug).
func Register(b Backend) {
	lang := b.Lang()
	if _, dup := registry[lang]; dup {
		panic(fmt.Sprintf("generator: duplicate backend registration for %q", lang))
	}
	registry[lang] = b
}

// Lookup returns the backend for a language, or false if none is registered.
func Lookup(lang string) (Backend, bool) {
	b, ok := registry[lang]
	return b, ok
}

// Registered lists the languages with a wired backend (sorted).
func Registered() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
