// Package pipeline orchestrates the generation stages (PLAN §8 diagram):
//
//	[1] Parser  -> [2] Model -> [3] Analysis -> [4] IR
//	    == Language Selection Point ==
//	[5] Backend (Visitor + Builder)
//
// Stages [1]–[4] are entirely language-independent; a backend is selected only
// after the IR is frozen. In M0 no backend is wired, so a run that omits/has no
// backend stops at the IR (the M0 "done when": validate + resolve + build IR).
package pipeline

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// Options controls one pipeline run.
type Options struct {
	DefPath string         // path to a definition file (YAML/JSON)
	Lang    string         // target language, or "" to stop at the IR
	Config  *config.Config // resolved config (may be config.Empty())
	OutDir  string         // output directory (when a backend runs)
}

// Result reports what a run produced.
type Result struct {
	Schema *ir.Schema
	Files  []generator.File // empty when no backend ran
}

// Run executes the pipeline for a single definition file. It fails closed: any
// validation or analysis error aborts with no output written (PLAN §1).
func Run(opts Options) (*Result, error) {
	// [1] Parser: load + hard-gate validation over the resolved document.
	doc, err := parser.Load(opts.DefPath)
	if err != nil {
		return nil, err
	}
	resolved, err := doc.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opts.DefPath, err)
	}
	if errs := parser.Validate(resolved); errs != nil {
		return nil, fmt.Errorf("%s: %w", opts.DefPath, errs)
	}

	// [2] Model: lower the validated, unresolved document into the IR
	// (composite fields carry unresolved TypeRefs).
	schema, err := model.Build(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opts.DefPath, err)
	}

	// [3] Analysis: resolve the shared-type graph + semantic checks; [4] freeze.
	if err := analysis.Analyze(schema); err != nil {
		return nil, fmt.Errorf("%s: %w", opts.DefPath, err)
	}

	res := &Result{Schema: schema}

	// == Language Selection Point == [5] Backend (if one is wired for --lang).
	if opts.Lang == "" {
		return res, nil
	}
	backend, ok := generator.Lookup(opts.Lang)
	if !ok {
		// M0: no backends are registered yet. This is not a failure of the
		// core pipeline — the IR is valid; there is simply no emitter.
		return res, &NoBackendError{Lang: opts.Lang}
	}
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Empty()
	}
	files, err := backend.Generate(schema, cfg.Effective(opts.Lang))
	if err != nil {
		return res, fmt.Errorf("backend %q: %w", opts.Lang, err)
	}
	res.Files = files
	return res, nil
}

// NoBackendError signals that the IR built successfully but no backend is
// registered for the requested language (expected in M0).
type NoBackendError struct{ Lang string }

func (e *NoBackendError) Error() string {
	return fmt.Sprintf("no backend registered for language %q (none are wired yet)", e.Lang)
}
