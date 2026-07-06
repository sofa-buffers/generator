// Package docs is the documentation backend: it renders the message
// definitions as human-readable reference documentation instead of source
// code. The one supported format is a single self-contained HTML page (inline
// CSS, no external assets), so the file can be mailed around, attached to CI
// artifacts, or dropped on a static server as-is.
package docs

import (
	"fmt"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for the docs target.
type Backend struct{}

func (*Backend) Lang() string { return "docs" }

// Generate emits one documentation file covering the whole definition
// (messages + the shared named-type graph). The `format` option selects the
// output format; html is the only one wired (and the default).
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	format := cfgString(cfg, "format", "html")
	if format != "html" {
		return nil, fmt.Errorf("docs: unsupported format %q (supported: html)", format)
	}
	g := &gen{
		schema:  s,
		banner:  cfgString(cfg, "tool_banner", "sofabgen"),
		license: generator.LicenseID(cfg),
	}
	page, err := g.htmlPage()
	if err != nil {
		return nil, err
	}
	return []generator.File{{Path: "message.html", Content: page}}, nil
}

type gen struct {
	schema  *ir.Schema
	banner  string
	license string // SPDX id, "" to omit the header comment
}

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}
