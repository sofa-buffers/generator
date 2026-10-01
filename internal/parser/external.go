package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sofa-buffers/generator/internal/naming"
)

// Source is where a definition is written: the absolute path of its file and
// the JSON pointer of the element inside that file ("#/$defs/struct/Vec3",
// "#/messages/m").
type Source struct {
	File string
	Ptr  string
}

// id is the definition's identity: the same file and pointer is the same
// definition, however many routes reach it.
func (s Source) id() string { return s.File + s.Ptr }

// InlineExternalRefs rewrites cross-file references — a `$ref` of the form
// "file.yaml#/$defs/<category>/<Name>" — into local ones by importing the
// referenced definition (and every definition it depends on, in its own file or
// further files) into this document's own `$defs`, then pointing the `$ref` at
// "#/$defs/<category>/<Name>". After this, the document is self-contained and
// the rest of the pipeline (Resolve, validation, model building) treats every
// `$ref` as local.
//
// Paths are resolved relative to the file the `$ref` is written in, so a chain
// of cross-file refs flattens transitively.
//
// An imported definition keeps its name, and messages and every `$defs`
// category are one namespace (ARCHITECTURE §8, naming rule 3). A definition is
// identified by its Source, so:
//
//   - the same definition reached twice (directly and transitively, or from two
//     files) is imported once and stays one type;
//   - a DIFFERENT definition whose name folds to one already taken — by a local
//     message or `$defs` entry, or by a definition imported from another file —
//     is a located error naming both sources: the document is ambiguous, and
//     keeping either one would silently give the other's fields its layout;
//   - two definitions in different files that are structurally identical (same
//     category, same name, deep-equal bodies) are one type: nothing is lost by
//     generating it once, and a schema split over files that copy a small
//     shared type stays accepted.
//
// Where each imported `$defs` entry came from is recorded in d.Origins, so a
// later error inside it can name its real file (Locate).
func (d *Document) InlineExternalRefs() error {
	root, ok := d.Root.(map[string]any)
	if !ok {
		return nil
	}
	if d.Path == "" {
		// No base path (in-memory parse); a cross-file ref cannot be resolved.
		return assertNoExternalRefs(d.Root)
	}
	abs, err := filepath.Abs(d.Path)
	if err != nil {
		return err
	}
	r := &importer{
		rootFile: abs,
		baseDir:  filepath.Dir(abs),
		defs:     ensureDefs(root),
		files:    map[string]map[string]any{abs: root},
		done:     map[string]bool{},
		byFold:   map[string]*nsEntry{},
		origins:  map[string]Source{},
	}
	// The document's own namespace comes first: a local name is never displaced
	// by an import.
	if msgs, ok := root["messages"].(map[string]any); ok {
		for _, name := range sortedNames(msgs) {
			r.claim(&nsEntry{src: Source{abs, "#/messages/" + name}, kind: "message", name: name, val: msgs[name]})
		}
	}
	for _, cat := range sortedNames(r.defs) {
		group, _ := r.defs[cat].(map[string]any)
		for _, name := range sortedNames(group) {
			src := Source{abs, "#/$defs/" + cat + "/" + name}
			r.done[src.id()] = true
			r.claim(&nsEntry{src: src, kind: cat, name: name, val: group[name]})
		}
	}
	// Walk the original elements only (imports land in r.defs while walking,
	// and are rewritten as they are imported).
	if msgs, ok := root["messages"].(map[string]any); ok {
		for _, name := range sortedNames(msgs) {
			r.walk(msgs[name], abs, "#/messages/"+name)
		}
	}
	type local struct{ cat, name string }
	var locals []local
	for _, cat := range sortedNames(r.defs) {
		group, _ := r.defs[cat].(map[string]any)
		for _, name := range sortedNames(group) {
			if _, imported := r.origins["#/$defs/"+cat+"/"+name]; !imported {
				locals = append(locals, local{cat, name})
			}
		}
	}
	for _, l := range locals {
		group, _ := r.defs[l.cat].(map[string]any)
		r.walk(group[l.name], abs, "#/$defs/"+l.cat+"/"+l.name)
	}
	if len(r.errs) > 0 {
		sort.SliceStable(r.errs, func(i, j int) bool { return r.errs[i].Loc < r.errs[j].Loc })
		return r.errs
	}
	if len(r.origins) > 0 {
		d.Origins = r.origins
	}
	d.base = r.baseDir
	d.self = abs
	return nil
}

// nsEntry is one name of the document's namespace and where it comes from.
type nsEntry struct {
	src  Source
	kind string // "message" or the $defs category
	name string
	val  any
	site string // the location of the $ref that imported it ("" when local)
}

type importer struct {
	rootFile string
	baseDir  string
	defs     map[string]any            // the document's root["$defs"]
	files    map[string]map[string]any // abs path -> decoded root (as written)
	done     map[string]bool           // Source.id() already imported (or local)
	byFold   map[string]*nsEntry       // naming.Fold(name) -> its owner
	origins  map[string]Source         // "#/$defs/<cat>/<name>" -> where it is written
	errs     Errors
}

func (r *importer) claim(e *nsEntry) {
	if _, taken := r.byFold[naming.Fold(e.name)]; !taken {
		// A fold clash between two LOCAL names is the validator's rule-3
		// report; the first one keeps the slot here.
		r.byFold[naming.Fold(e.name)] = e
	}
}

// display spells a file for a message: nothing for the document itself (the
// caller prefixes its path), otherwise the path relative to the document's
// directory.
func (r *importer) display(file string) string {
	return displayFile(r.baseDir, r.rootFile, file)
}

func displayFile(base, self, file string) string {
	if file == self {
		return ""
	}
	if rel, err := filepath.Rel(base, file); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(file)
}

func ensureDefs(root map[string]any) map[string]any {
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		defs = map[string]any{}
		root["$defs"] = defs
	}
	return defs
}

// walk rewrites every `$ref` under node, which is written in file at loc. In
// the document itself a local ref stays as it is; in an imported definition a
// local ref names a definition of that definition's file, which is imported
// in turn.
func (r *importer) walk(node any, file, loc string) {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && len(t) == 1 {
			target, ptr, isExt := splitExtRef(ref)
			switch {
			case isExt:
				target = filepath.Join(filepath.Dir(file), filepath.FromSlash(target))
			case file != r.rootFile && strings.HasPrefix(ref, "#"):
				target, ptr = file, strings.TrimPrefix(ref, "#")
			default:
				return // a local ref of the document itself
			}
			if local, ok := r.importRef(target, ptr, ref, loc); ok {
				t["$ref"] = local
			}
			return
		}
		for _, k := range sortedNames(t) {
			r.walk(t[k], file, loc+"/"+k)
		}
	case []any:
		for i, v := range t {
			r.walk(v, file, fmt.Sprintf("%s/%d", loc, i))
		}
	}
}

func (r *importer) fail(loc, ref, format string, args ...any) {
	r.errs = append(r.errs, Error{Loc: loc, Msg: "$ref " + ref + ": " + fmt.Sprintf(format, args...)})
}

// importRef imports file#ptr and returns the local ref that replaces it.
func (r *importer) importRef(file, ptr, ref, loc string) (string, bool) {
	cat, name, err := parseDefsPtr(ptr)
	if err != nil {
		r.fail(loc, ref, "%v", err)
		return "", false
	}
	local := "#/$defs/" + cat + "/" + name
	src := Source{file, local}
	if r.done[src.id()] {
		return local, true
	}
	extRoot, err := r.load(file)
	if err != nil {
		r.fail(loc, ref, "%v", err)
		return "", false
	}
	extDefs, _ := extRoot["$defs"].(map[string]any)
	group, _ := extDefs[cat].(map[string]any)
	def, ok := group[name]
	if !ok {
		r.fail(loc, ref, "no $defs/%s/%s in %s", cat, name, r.displayOrSelf(file))
		return "", false
	}
	// Mark before recursing, so a cycle through files ends here.
	r.done[src.id()] = true
	cp := deepCopy(def)
	r.walk(cp, file, r.display(file)+local)

	e := &nsEntry{src: src, kind: cat, name: name, val: cp, site: loc}
	if prev, taken := r.byFold[naming.Fold(name)]; taken {
		if prev.kind == cat && prev.name == name && equalDefs(prev.val, cp) {
			return local, true // the same type written twice: one type
		}
		r.fail(loc, ref, "%q is already defined by %s; one schema has one namespace for messages and $defs — rename one",
			name, r.describe(prev))
		return "", false
	}
	r.byFold[naming.Fold(name)] = e
	g, _ := r.defs[cat].(map[string]any)
	if g == nil {
		g = map[string]any{}
		r.defs[cat] = g
	}
	g[name] = cp
	r.origins[local] = src
	return local, true
}

func (r *importer) displayOrSelf(file string) string {
	if d := r.display(file); d != "" {
		return d
	}
	return "this file"
}

// describe names where an entry of the namespace comes from.
func (r *importer) describe(e *nsEntry) string {
	s := r.display(e.src.File) + e.src.Ptr
	if e.site != "" {
		s += " (imported for " + e.site + ")"
	}
	return s
}

// load returns a file's decoded root as written (its own refs untouched: they
// are rewritten per imported definition, relative to that file).
func (r *importer) load(file string) (map[string]any, error) {
	if cached, ok := r.files[file]; ok {
		return cached, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r.displayOrSelf(file), err)
	}
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", r.displayOrSelf(file), err)
	}
	rm, _ := normalize(root).(map[string]any)
	if rm == nil {
		rm = map[string]any{}
	}
	r.files[file] = rm
	return rm, nil
}

// splitExtRef splits "file#pointer". A ref with no '#', or one beginning with
// '#', is local (isExt=false).
func splitExtRef(ref string) (file, ptr string, isExt bool) {
	i := strings.Index(ref, "#")
	if i <= 0 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

func parseDefsPtr(ptr string) (cat, name string, err error) {
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	if len(parts) != 3 || parts[0] != "$defs" {
		return "", "", fmt.Errorf("a $ref into another file, or inside an imported definition, must point at /$defs/<category>/<Name>, got %q", ptr)
	}
	return parts[1], parts[2], nil
}

// equalDefs reports whether two definitions are the same type: deep-equal once
// every $ref is reduced to its pointer (names are one namespace, so the same
// pointer is the same type once the referencing file is gone — and a
// dependency that differs under one name is refused on its own import).
func equalDefs(a, b any) bool {
	return reflect.DeepEqual(stripRefFiles(a), stripRefFiles(b))
}

func stripRefFiles(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && len(t) == 1 {
			if i := strings.Index(ref, "#"); i > 0 {
				return map[string]any{"$ref": ref[i:]}
			}
			return t
		}
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = stripRefFiles(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = stripRefFiles(val)
		}
		return s
	default:
		return v
	}
}

func sortedNames(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
