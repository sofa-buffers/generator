package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// InlineExternalRefs rewrites cross-file references — a `$ref` of the form
// "file.yaml#/$defs/<category>/<Name>" — into local ones by importing the
// referenced definition (and its same-file dependencies) into this document's
// own `$defs`, then pointing the `$ref` at "#/$defs/<category>/<Name>". After
// this, the document is self-contained and the rest of the pipeline (Resolve,
// validation, model building) treats every `$ref` as local.
//
// Paths are resolved relative to the document's own file. External files are
// themselves inlined first, so a chain of cross-file refs flattens transitively.
func (d *Document) InlineExternalRefs() error {
	root, ok := d.Root.(map[string]any)
	if !ok {
		return nil
	}
	if d.Path == "" {
		// No base path (in-memory parse); a cross-file ref cannot be resolved.
		return assertNoExternalRefs(d.Root)
	}
	r := &extResolver{
		baseDir:   filepath.Dir(d.Path),
		localDefs: ensureDefs(root),
		files:     map[string]map[string]any{},
		imported:  map[string]bool{},
	}
	return r.walk(d.Root)
}

type extResolver struct {
	baseDir   string
	localDefs map[string]any            // this document's root["$defs"]
	files     map[string]map[string]any // abs path -> inlined external root
	imported  map[string]bool           // "cat/name" already copied in
}

func ensureDefs(root map[string]any) map[string]any {
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		defs = map[string]any{}
		root["$defs"] = defs
	}
	return defs
}

func (r *extResolver) walk(node any) error {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && len(t) == 1 {
			if file, ptr, isExt := splitExtRef(ref); isExt {
				local, err := r.importRef(file, ptr)
				if err != nil {
					return err
				}
				t["$ref"] = local
				return nil
			}
		}
		for _, v := range t {
			if err := r.walk(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range t {
			if err := r.walk(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitExtRef splits "file#/pointer". A ref with no '#', or one beginning with
// '#', is local (isExt=false).
func splitExtRef(ref string) (file, ptr string, isExt bool) {
	i := strings.Index(ref, "#")
	if i <= 0 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

func (r *extResolver) importRef(file, ptr string) (string, error) {
	cat, name, err := parseDefsPtr(ptr)
	if err != nil {
		return "", err
	}
	extRoot, err := r.loadFile(file)
	if err != nil {
		return "", err
	}
	if err := r.importDef(extRoot, cat, name); err != nil {
		return "", err
	}
	return "#/$defs/" + cat + "/" + name, nil
}

func parseDefsPtr(ptr string) (cat, name string, err error) {
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	if len(parts) != 3 || parts[0] != "$defs" {
		return "", "", fmt.Errorf("external $ref pointer must be /$defs/<category>/<Name>, got %q", ptr)
	}
	return parts[1], parts[2], nil
}

func (r *extResolver) loadFile(file string) (map[string]any, error) {
	abs := filepath.Join(r.baseDir, file)
	if cached, ok := r.files[abs]; ok {
		return cached, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("external $ref: %w", err)
	}
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("external $ref %q: %w", file, err)
	}
	root = normalize(root)
	doc := &Document{Root: root, Path: abs}
	if err := doc.InlineExternalRefs(); err != nil { // flatten its own cross-file refs
		return nil, err
	}
	rm, _ := doc.Root.(map[string]any)
	r.files[abs] = rm
	return rm, nil
}

// importDef copies $defs/<cat>/<name> from extRoot into the local $defs,
// recursively pulling the same-file definitions it depends on.
func (r *extResolver) importDef(extRoot map[string]any, cat, name string) error {
	key := cat + "/" + name
	if r.imported[key] {
		return nil
	}
	extDefs, _ := extRoot["$defs"].(map[string]any)
	catMap, _ := extDefs[cat].(map[string]any)
	def, ok := catMap[name]
	if !ok {
		return fmt.Errorf("external $ref: no $defs/%s/%s in referenced file", cat, name)
	}
	r.imported[key] = true
	cp := deepCopy(def)
	if err := r.importLocalDeps(extRoot, cp); err != nil {
		return err
	}
	lc, _ := r.localDefs[cat].(map[string]any)
	if lc == nil {
		lc = map[string]any{}
		r.localDefs[cat] = lc
	}
	if _, exists := lc[name]; !exists {
		lc[name] = cp
	}
	return nil
}

// importLocalDeps pulls in every "#/$defs/.." dependency a copied external def
// refers to (from the same external file).
func (r *extResolver) importLocalDeps(extRoot map[string]any, node any) error {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && len(t) == 1 && strings.HasPrefix(ref, "#/$defs/") {
			if cat, name, err := parseDefsPtr(strings.TrimPrefix(ref, "#")); err == nil {
				return r.importDef(extRoot, cat, name)
			}
		}
		for _, v := range t {
			if err := r.importLocalDeps(extRoot, v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range t {
			if err := r.importLocalDeps(extRoot, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}

// assertNoExternalRefs returns a clear error if an in-memory document (no base
// path) contains a cross-file ref it cannot resolve.
func assertNoExternalRefs(node any) error {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && len(t) == 1 {
			if _, _, isExt := splitExtRef(ref); isExt {
				return fmt.Errorf("cross-file $ref %q requires a file path (load from disk, not in-memory)", ref)
			}
		}
		for _, v := range t {
			if err := assertNoExternalRefs(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range t {
			if err := assertNoExternalRefs(v); err != nil {
				return err
			}
		}
	}
	return nil
}
