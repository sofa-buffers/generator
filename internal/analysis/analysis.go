// Package analysis implements stage [3] of the pipeline: it resolves the IR's
// $ref/shared-type graph in place and runs the language-independent semantic
// checks, then freezes the IR (PLAN §8.2). After Analyze succeeds, every
// composite field's TypeRef.Target is non-nil and the tree is safe for any
// backend to traverse read-only (§8.6).
package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

// Error is a located semantic error (same located-error contract as §1).
type Error struct {
	Loc string
	Msg string
}

func (e Error) Error() string { return e.Loc + ": " + e.Msg }

// Errors aggregates all problems found in one pass.
type Errors []Error

func (es Errors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d analysis error(s):", len(es))
	for _, e := range es {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

// Analyze resolves and checks the schema. On success it returns nil and the
// schema is the frozen IR; on failure it returns a non-empty Errors and the
// schema must not be used.
func Analyze(s *ir.Schema) error {
	a := &analyzer{schema: s}
	a.resolveRefs()
	if len(a.errs) > 0 { // a dangling ref makes depth analysis unsafe
		return sortErrs(a.errs)
	}
	a.checkDepth()
	a.checkUnionDefaults()
	if len(a.errs) > 0 {
		return sortErrs(a.errs)
	}
	return nil
}

type analyzer struct {
	schema *ir.Schema
	errs   Errors
}

func (a *analyzer) add(loc, format string, args ...any) {
	a.errs = append(a.errs, Error{Loc: loc, Msg: fmt.Sprintf(format, args...)})
}

// resolveRefs wires every composite field to its single shared NamedType.
func (a *analyzer) resolveRefs() {
	for _, m := range a.schema.Messages {
		a.resolveFields(m.Fields, "messages/"+m.Name)
	}
	for _, key := range a.schema.NamedOrder {
		nt := a.schema.Named[key]
		a.resolveFields(nt.Fields, key)
	}
}

func (a *analyzer) resolveFields(fields []*ir.Field, loc string) {
	for _, f := range fields {
		a.resolveRef(f.Ref, loc+"/"+f.Name)
		// array element composite (enum/bitfield/struct/union), incl. nested.
		a.resolveRef(f.ElemRef, loc+"/"+f.Name+"[]")
		for e := f.ElemItems; e != nil; e = e.ElemItems {
			a.resolveRef(e.ElemRef, loc+"/"+f.Name+"[]")
		}
	}
}

func (a *analyzer) resolveRef(r *ir.TypeRef, loc string) {
	if r == nil {
		return
	}
	target, ok := a.schema.Named[r.Key]
	if !ok {
		a.add(loc, "unresolved type reference %q", r.Key)
		return
	}
	r.Target = target
}

// checkDepth enforces the shared MAX_NESTING_DEPTH = 256 cap (§4.2). Each
// struct/union opens one nesting level. Cycles (recursive structs) are broken
// at the back-edge: their runtime depth is data-dependent, not statically
// bounded, so a cycle is not itself an error here.
func (a *analyzer) checkDepth() {
	for _, m := range a.schema.Messages {
		a.walkDepth(m.Fields, 1, "messages/"+m.Name, map[string]bool{})
	}
}

func (a *analyzer) walkDepth(fields []*ir.Field, depth int, loc string, onPath map[string]bool) {
	if depth > ir.MaxNestingDepth {
		a.add(loc, "nesting depth %d exceeds MAX_NESTING_DEPTH (%d)", depth, ir.MaxNestingDepth)
		return
	}
	for _, f := range fields {
		// A composite field, or a composite array element (array-of-struct /
		// array-of-union), opens a nesting level; a nested array's element does
		// too. enum/bitfield/scalar/string/blob elements are leaves.
		a.descend(f.Ref, depth, loc+"/"+f.Name, onPath)
		a.descend(f.ElemRef, depth, loc+"/"+f.Name, onPath)
		for e := f.ElemItems; e != nil; e = e.ElemItems {
			a.descend(e.ElemRef, depth, loc+"/"+f.Name, onPath)
		}
	}
}

// descend recurses into a struct/union target one nesting level deeper, breaking
// recursive back-edges (their runtime depth is data-dependent, not static).
func (a *analyzer) descend(r *ir.TypeRef, depth int, loc string, onPath map[string]bool) {
	if r == nil || r.Target == nil {
		return
	}
	t := r.Target
	if t.Category != ir.CatStruct && t.Category != ir.CatUnion {
		return
	}
	if onPath[t.Key] {
		return
	}
	onPath[t.Key] = true
	a.walkDepth(t.Fields, depth+1, loc, onPath)
	delete(onPath, t.Key)
}

// checkUnionDefaults is a placeholder hook for cross-field semantic checks the
// validator already covers; kept so future model-level checks have a home.
func (a *analyzer) checkUnionDefaults() {}

func sortErrs(es Errors) Errors {
	sort.SliceStable(es, func(i, j int) bool { return es[i].Loc < es[j].Loc })
	return es
}
