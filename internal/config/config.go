// Package config loads and validates sofabgen configuration (PLAN §7). The
// config file is the single source of truth; only --in/--out override it from
// the CLI. Every config is validated against schema/sofabgen-config-schema.json
// as a HARD GATE before use (§7.1) — same fail-closed semantics as definition
// validation.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	generator "github.com/sofa-buffers/generator"
	"gopkg.in/yaml.v3"
)

// Config is the parsed, schema-validated configuration. It is held generically
// (decoded maps) so adding a config key never requires a Go struct change — the
// schema is the source of truth and stays in lockstep with it (§7.1).
type Config struct {
	Generic map[string]any
	Targets map[string]map[string]any
	raw     map[string]any
}

// Default returns the built-in defaults applied beneath every config (§7.2).
func Default() map[string]any {
	return map[string]any{
		// `namespace` is intentionally NOT defaulted here: it is a per-language
		// concern, so each backend applies its own idiomatic default (C++
		// `message`, C# `Message`). A generic default would shadow those. Set
		// `generic.namespace` (or the per-target one) to override.
		//
		// Every other honored key (tool_banner, license, symbol_prefix, …) is
		// defaulted where it is consumed, so this map stays minimal — a key
		// belongs here only when several layers need to agree on its default.
		"emit": "sources",
	}
}

// Load reads, parses, and schema-validates a config file (YAML or JSON). On any
// schema violation it returns an error listing every problem (fail-closed).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	return Parse(data, path)
}

// Parse validates and decodes an in-memory config.
func Parse(data []byte, path string) (*Config, error) {
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}
	root = normalize(root)
	rootMap, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config %q: root must be a mapping", path)
	}

	// Hard gate: validate against the config schema before use.
	schema, err := loadConfigSchema()
	if err != nil {
		return nil, err
	}
	if errs := validateAgainstSchema(schema, rootMap); len(errs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "config %q failed validation (%d error(s)):", path, len(errs))
		for _, e := range errs {
			fmt.Fprintf(&b, "\n  - %s: %s", e.loc, e.msg)
		}
		return nil, fmt.Errorf("%s", b.String())
	}

	c := &Config{raw: rootMap, Targets: map[string]map[string]any{}}
	if g, ok := rootMap["generic"].(map[string]any); ok {
		c.Generic = g
	} else {
		c.Generic = map[string]any{}
	}
	if t, ok := rootMap["targets"].(map[string]any); ok {
		for lang, v := range t {
			if tm, ok := v.(map[string]any); ok {
				c.Targets[lang] = tm
			}
		}
	}
	return c, nil
}

// Empty returns a config with no file backing — used when --config is omitted
// for the M0 validate-and-build-IR flow (no backend selected yet).
func Empty() *Config {
	return &Config{Generic: map[string]any{}, Targets: map[string]map[string]any{}, raw: map[string]any{}}
}

// Effective resolves the configuration for one target language with the
// documented precedence: built-in default < generic < per-target (§7.1).
func (c *Config) Effective(lang string) map[string]any {
	out := map[string]any{}
	for k, v := range Default() {
		out[k] = v
	}
	for k, v := range c.Generic {
		out[k] = v
	}
	for k, v := range c.Targets[lang] {
		out[k] = v
	}
	return out
}

// normalize mirrors parser.normalize: string-keyed maps throughout.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = normalize(val)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = normalize(val)
		}
		return s
	default:
		return v
	}
}

func loadConfigSchema() (map[string]any, error) {
	data, err := generator.SchemaFS.ReadFile(generator.ConfigSchemaPath)
	if err != nil {
		return nil, fmt.Errorf("loading embedded config schema: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("parsing embedded config schema: %w", err)
	}
	return schema, nil
}

// KnownTargets lists the target keys the config schema accepts, for CLI help
// and validation of --lang.
func KnownTargets() []string {
	ts := []string{"c", "cpp", "rust", "go", "python", "java", "csharp", "typescript", "zig", "docs"}
	sort.Strings(ts)
	return ts
}
