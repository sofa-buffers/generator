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
	a.bindUnionDefaults()
	if len(a.errs) > 0 { // a split collision leaves sites pointing at the original
		return sortErrs(a.errs)
	}
	a.checkDepth()
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

// unionSite is one place a union type is used: a union field, an array's union
// element, or a nested array's union element.
type unionSite struct {
	ref *ir.TypeRef
	loc string
	id  int64 // effective default_id: the site's, else the lowest option id
}

// bindUnionDefaults gives every union NamedType its DefaultID (MESSAGE_SPEC
// §4.2: a fresh union holds default_id at that option's own default). The
// default is a property of the TYPE, not of the site, so every generated
// constructor, gap fill and omission test can bake it in. A $defs union used
// with different effective default_ids is therefore split into one NamedType
// per default_id, named <Name>_default_<option>, and every site is repointed at
// its variant. An omitted default_id means the option with the lowest id.
//
// Only a $defs union can split: an inline union has exactly one site.
func (a *analyzer) bindUnionDefaults() {
	order := append([]string(nil), a.schema.NamedOrder...)
	sites := map[string][]unionSite{}
	collect := func(r *ir.TypeRef, loc string) {
		if r == nil || r.Target == nil || r.Target.Category != ir.CatUnion {
			return
		}
		id, ok := lowestOptionID(r.Target)
		if !ok {
			return // an empty oneof is rejected by the validator
		}
		if r.DefaultID != nil {
			id = *r.DefaultID
		}
		sites[r.Target.Key] = append(sites[r.Target.Key], unionSite{ref: r, loc: loc, id: id})
	}
	walk := func(fields []*ir.Field, loc string) {
		for _, f := range fields {
			collect(f.Ref, loc+"/"+f.Name)
			collect(f.ElemRef, loc+"/"+f.Name+"[]")
			for e := f.ElemItems; e != nil; e = e.ElemItems {
				collect(e.ElemRef, loc+"/"+f.Name+"[]")
			}
		}
	}
	for _, m := range a.schema.Messages {
		walk(m.Fields, "messages/"+m.Name)
	}
	for _, key := range order {
		walk(a.schema.Named[key].Fields, key)
	}

	for _, key := range order {
		nt := a.schema.Named[key]
		if nt.Category != ir.CatUnion {
			continue
		}
		ids := distinctIDs(sites[key])
		switch len(ids) {
		case 0: // no site: the lowest option id
			if lo, ok := lowestOptionID(nt); ok {
				nt.DefaultID = &lo
			}
		case 1:
			id := ids[0]
			nt.DefaultID = &id
		default:
			a.splitUnion(nt, sites[key], ids)
		}
	}
}

// splitUnion replaces the union nt, used with several distinct default_ids, by
// one variant per id (ascending) at nt's position in NamedOrder, and repoints
// every site at its variant.
func (a *analyzer) splitUnion(nt *ir.NamedType, ss []unionSite, ids []int64) {
	variants := make([]*ir.NamedType, 0, len(ids))
	byID := make(map[int64]*ir.NamedType, len(ids))
	for _, d := range ids {
		opt := optionByID(nt, d)
		if opt == nil { // unreachable: the validator checks default_id membership
			a.add(siteLoc(ss, d), "default_id %d matches no option id in union %q", d, nt.Name)
			return
		}
		v := *nt // shallow: the Fields slice is shared, the IR is immutable after analysis
		id := d
		v.Name = nt.Name + "_default_" + opt.Name
		v.Key = nt.Key + "_default_" + opt.Name
		v.DefaultID = &id
		variants = append(variants, &v)
		byID[d] = &v
	}
	for i, v := range variants {
		if existing := a.foldedClash(v, nt, variants); existing != "" {
			other := ids[(i+1)%len(ids)]
			a.add(siteLoc(ss, ids[i]), "union %q is used with default_id %d and %d; the generated type %q for one of them collides with the existing type %q — rename one",
				nt.Name, ids[i], other, v.Key, existing)
			return
		}
	}
	for _, s := range ss {
		v := byID[s.id]
		s.ref.Key, s.ref.Target = v.Key, v
	}
	delete(a.schema.Named, nt.Key)
	order := make([]string, 0, len(a.schema.NamedOrder)+len(variants)-1)
	for _, k := range a.schema.NamedOrder {
		if k != nt.Key {
			order = append(order, k)
			continue
		}
		for _, v := range variants {
			a.schema.Named[v.Key] = v
			order = append(order, v.Key)
		}
	}
	a.schema.NamedOrder = order
}

// foldedClash returns the key of a type that v collides with, or "". The test
// is folded, not raw: every backend derives its type identifiers from the key
// (or the name) by case changes and separator removal, so two names equal once
// lowercased and stripped of every non-alphanumeric character clash in at least
// one target. orig, the union being split, is ignored.
func (a *analyzer) foldedClash(v, orig *ir.NamedType, variants []*ir.NamedType) string {
	fk, fn := fold(v.Key), fold(v.Name)
	for _, k := range a.schema.NamedOrder {
		if t := a.schema.Named[k]; t != orig && (fold(t.Key) == fk || fold(t.Name) == fn) {
			return t.Key
		}
	}
	for _, w := range variants {
		if w != v && (fold(w.Key) == fk || fold(w.Name) == fn) {
			return w.Key
		}
	}
	return ""
}

func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func lowestOptionID(nt *ir.NamedType) (int64, bool) {
	if len(nt.Fields) == 0 {
		return 0, false
	}
	lo := nt.Fields[0].ID
	for _, f := range nt.Fields[1:] {
		lo = min(lo, f.ID)
	}
	return lo, true
}

func optionByID(nt *ir.NamedType, id int64) *ir.Field {
	for _, f := range nt.Fields {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// distinctIDs returns the sites' effective default_ids, ascending, once each.
func distinctIDs(ss []unionSite) []int64 {
	seen := map[int64]bool{}
	var ids []int64
	for _, s := range ss {
		if !seen[s.id] {
			seen[s.id] = true
			ids = append(ids, s.id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// siteLoc is the location of the first site using default_id d.
func siteLoc(ss []unionSite, d int64) string {
	for _, s := range ss {
		if s.id == d {
			return s.loc
		}
	}
	return ss[0].loc
}

func sortErrs(es Errors) Errors {
	sort.SliceStable(es, func(i, j int) bool { return es[i].Loc < es[j].Loc })
	return es
}
