// Package parser implements stage [1] of the generation pipeline: it loads a
// SofaBuffers message-definition document (YAML or JSON) and validates it
// against the v1 schema as a HARD GATE (PLAN §1, §8.1).
//
// The validation contract is hand-ported from schema/README.md: structural
// rules from sofabuffers-schema-v1.json plus the parts a stock JSON-Schema
// validator cannot express — the $data cross-field rules and the six custom
// keywords (uniqueIds, uniquePositions, defaultMatchesEnum,
// defaultIdMatchesUnion, blobDefaultLength, int64Range) — and the
// "dereference-then-validate, generate-from-the-unresolved-document" contract.
//
// On any violation the caller receives a clear, located error list; nothing
// downstream ever sees unvalidated input.
package parser

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is the loaded definition, decoded into Go's generic value space
// (map[string]any / []any / scalars). It is the unresolved document — $ref
// nodes are preserved so the generator can emit a shared-type graph (§3.4).
type Document struct {
	Root any
	Path string // source file path, for error messages
}

// Load reads and decodes a YAML or JSON definition file. YAML is a superset of
// JSON, so a single YAML decode handles both. It does not validate; call
// Validate for the hard gate.
func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading definition %q: %w", path, err)
	}
	return Parse(data, path)
}

// Parse decodes an in-memory definition. path is used only for messages.
func Parse(data []byte, path string) (*Document, error) {
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", path, err)
	}
	root = normalize(root)
	return &Document{Root: root, Path: path}, nil
}

// normalize converts yaml.v3's map[string]any/[]any tree into a canonical form
// with string keys throughout, so the rest of the pipeline never has to care
// that YAML technically allows non-string keys. Non-string keys are coerced via
// fmt so they still surface in a located error rather than panicking.
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

// Resolve returns a deep copy of the document with every $ref dereferenced
// against the document's own $defs (JSON-pointer "#/..." form). This is the
// tree the validator checks: a dangling $ref therefore fails fast (README §1).
// The original Document is left untouched so generation still sees the shared
// $ref graph.
func (d *Document) Resolve() (any, error) {
	r := &resolver{root: d.Root}
	out, err := r.resolve(d.Root, "#", map[string]bool{})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type resolver struct{ root any }

func (r *resolver) resolve(v any, path string, seen map[string]bool) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		// A {"$ref": "#/..."} object is replaced by its (recursively resolved)
		// target. Per the schema, $ref objects are closed (only "$ref"), so a
		// lone $ref key is the dereference trigger.
		if ref, ok := t["$ref"]; ok && len(t) == 1 {
			refStr, _ := ref.(string)
			if seen[refStr] {
				return nil, fmt.Errorf("%s: circular $ref %q", path, refStr)
			}
			target, err := r.lookup(refStr)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			next := map[string]bool{refStr: true}
			for k := range seen {
				next[k] = true
			}
			return r.resolve(target, refStr, next)
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			res, err := r.resolve(val, path+"/"+k, seen)
			if err != nil {
				return nil, err
			}
			out[k] = res
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			res, err := r.resolve(val, fmt.Sprintf("%s/%d", path, i), seen)
			if err != nil {
				return nil, err
			}
			out[i] = res
		}
		return out, nil
	default:
		return v, nil
	}
}

// lookup resolves a "#/a/b/c" JSON pointer against the document root.
func (r *resolver) lookup(ref string) (any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("unsupported $ref %q (only local '#/...' pointers are supported)", ref)
	}
	cur := r.root
	for _, raw := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q: cannot descend into %q", ref, tok)
		}
		cur, ok = m[tok]
		if !ok {
			return nil, fmt.Errorf("$ref %q: no such definition %q", ref, tok)
		}
	}
	return cur, nil
}
