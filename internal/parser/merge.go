package parser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/naming"
)

// IsDefinitionFile reports whether a file name has a definition extension
// (.yaml, .yml, .json; any case).
func IsDefinitionFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// DefinitionFiles lists the definition files directly inside dir (not its
// subdirectories, which hold files reached by cross-file $ref), sorted.
func DefinitionFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var defs []string
	for _, e := range entries {
		if !e.IsDir() && IsDefinitionFile(e.Name()) {
			defs = append(defs, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(defs)
	return defs, nil
}

// LoadDir loads a directory of definition files as ONE schema. Each file is
// loaded and hard-gate validated on its own first, so its errors are located in
// it exactly as a single-file run would locate them; then every file's messages
// and $defs are merged into one document with one namespace (ARCHITECTURE §8,
// naming rule 3, applied across files):
//
//   - the same definition reached from two files (a library file that another
//     file also imports by cross-file $ref) is one type;
//   - two structurally identical definitions under one name and kind (deep-equal
//     bodies) are one type, as for cross-file imports;
//   - two different definitions whose names fold to one are a located error
//     naming both files: the directory is ambiguous.
//
// Every $defs entry of every file is kept, used by a message or not — exactly
// what a single-file run does with its own $defs. The returned document's
// Origins place every element in its file, so later errors name it.
func LoadDir(dir string) (*Document, error) {
	files, err := DefinitionFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no definition files (.yaml, .yml, .json) in %q", dir)
	}
	var docs []*Document
	var errs []error
	for _, f := range files {
		doc, err := Load(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		resolved, err := doc.Resolve()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		if verrs := Validate(resolved); verrs != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, doc.LocateErrors(verrs)))
			continue
		}
		docs = append(docs, doc)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	merged, err := Merge(dir, docs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return merged, nil
}

// Merge combines loaded documents (each already self-contained: cross-file
// refs inlined) into one, under the rules LoadDir states. dir is the directory
// error locations are given relative to.
func Merge(dir string, docs []*Document) (*Document, error) {
	base, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	defs := map[string]any{}
	msgs := map[string]any{}
	origins := map[string]Source{}
	byFold := map[string]*nsEntry{}
	var errs Errors

	show := func(src Source) string { return displayFile(base, "", src.File) + src.Ptr }
	describe := func(e *nsEntry) string {
		s := show(e.src)
		if e.site != "" {
			s += " (imported by " + e.site + ")"
		}
		return s
	}
	// add claims one top-level name for e; where is the element's own spelling
	// in its file (its import site for an imported $defs entry).
	add := func(e *nsEntry, local string, into map[string]any) {
		prev, taken := byFold[naming.Fold(e.name)]
		if !taken {
			byFold[naming.Fold(e.name)] = e
			into[e.name] = e.val
			origins[local] = e.src
			return
		}
		if prev.src.id() == e.src.id() {
			return // the same definition, reached from two files
		}
		if prev.kind == e.kind && prev.name == e.name && equalDefs(prev.val, e.val) {
			return // the same type written twice: one type
		}
		errs = append(errs, Error{Loc: describe(e), Msg: fmt.Sprintf(
			"%q is already defined by %s; one schema has one namespace for messages and $defs — rename one",
			e.name, describe(prev))})
	}

	for _, doc := range docs {
		root, _ := doc.Root.(map[string]any)
		self, err := filepath.Abs(doc.Path)
		if err != nil {
			return nil, err
		}
		if m, ok := root["messages"].(map[string]any); ok {
			for _, name := range sortedNames(m) {
				ptr := "#/messages/" + name
				add(&nsEntry{src: Source{self, ptr}, kind: "message", name: name, val: m[name]}, ptr, msgs)
			}
		}
		d, _ := root["$defs"].(map[string]any)
		for _, cat := range sortedNames(d) {
			group, _ := d[cat].(map[string]any)
			for _, name := range sortedNames(group) {
				ptr := "#/$defs/" + cat + "/" + name
				src, imported := doc.Origins[ptr]
				e := &nsEntry{kind: cat, name: name, val: group[name]}
				if imported {
					e.src = src
					e.site = displayFile(base, "", self)
				} else {
					e.src = Source{self, ptr}
				}
				g, _ := defs[cat].(map[string]any)
				if g == nil {
					g = map[string]any{}
					defs[cat] = g
				}
				add(e, ptr, g)
			}
		}
	}
	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Loc < errs[j].Loc })
		return nil, errs
	}
	root := map[string]any{"version": 1, "$defs": defs}
	if len(msgs) > 0 {
		root["messages"] = msgs
	}
	return &Document{Root: root, Path: dir, Origins: origins, base: base}, nil
}
