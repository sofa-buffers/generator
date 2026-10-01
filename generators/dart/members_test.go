package dart

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/naming"
)

// memberProbes are names built to meet a derived member of another name: every
// derived shape, a prefix of one, names whose low-cased or Pascal-cased
// spelling equals another's derived member, reserved and capitalised names,
// a type-escaped one, and the library helper a slot could hide.
var memberProbes = []string{
	"a", "aId", "x", "hasX", "has", "id", "Id2", "mutable", "y", "mutableY", "f", "fFp32Bits",
	"hasFoo", "foo_id", "Point", "PointId", "which", "int", "String", "BigInt", "e", "bools01",
	"Inner", "ha", "mutabl", "mutable_x", "has_y", "hasId", "Fp32Bits", "gId", "hasG", "g",
	"reset", "toString", "M_A", "HasZ", "z", "userId", "user", "maxSize",
}

// optionNames is every member one option declares, under each kind that
// derives one: an fp32 option (bits) and a string option (mutable).
func optionNames(n string) []string {
	var out []string
	for _, k := range []ir.Kind{ir.KindFP32, ir.KindString} {
		o := optionMembers(&ir.Field{Name: n, Kind: k})
		for _, s := range []string{o.prop, o.idConst, o.has, o.mutable, o.slot, o.bits, o.bitSlot} {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return dedupe(out)
}

// structNames is every member one struct or message field declares, under the
// kinds that derive one (an fp32 field's bits).
func structNames(n string) []string {
	f := fieldMembers(&ir.Field{Name: n, Kind: ir.KindFP32})
	return []string{f.member, f.bits}
}

func dedupe(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// checkScope asserts that the members of one class -- the names' own and the
// class's fixed ones -- are pairwise distinct and that none hides what the
// class body names: a reserved name, a library helper, or a class (every
// class starts upper-case or `_`; a member starting upper-case must be
// escaped, and n + "_" is a class only for a type-escaped n).
func checkScope(t *testing.T, what string, names []string, of func(string) []string, fixed ...string) {
	t.Helper()
	owner := map[string]string{}
	isUnion := false
	for _, n := range fixed {
		isUnion = isUnion || n == "_which"
		owner[n] = "(fixed)"
	}
	for _, n := range names {
		for _, m := range of(n) {
			if prev, ok := owner[m]; ok {
				t.Errorf("%s: member %s of %q meets the one of %q", what, m, n, prev)
			}
			owner[m] = n
			if memberReserved(m) || (isUnion && unionFixed[m]) || dartPrivateHelpers[m] {
				t.Errorf("%s: member %s of %q is reserved", what, m, n)
			}
			if isUpperASCII(m[0]) {
				if !strings.HasSuffix(m, "_") {
					t.Errorf("%s: member %s of %q starts upper-case and is not escaped", what, m, n)
				} else if base := strings.TrimSuffix(m, "_"); isTypeReserved(base) {
					t.Errorf("%s: member %s of %q is the escaped class of %s", what, m, n, base)
				}
			}
		}
	}
}

// TestMemberNamesInjective proves the member spelling rule over the probes
// and over random sets of rule-abiding names: no two members of one class
// meet, and none hides a reserved or class name.
func TestMemberNamesInjective(t *testing.T) {
	unionFixedMembers := []string{"which", "_which", "serialize", "reset"}
	checkScope(t, "union probes", memberProbes, optionNames, unionFixedMembers...)
	checkScope(t, "struct probes", memberProbes, structNames, "_decodeInto")

	r := rand.New(rand.NewSource(624))
	parts := []string{"has", "Has", "mutable", "Mutable", "Id", "id", "Fp32Bits", "fp32", "Bits", "a", "A", "x", "X", "_", "1", "y", "Y"}
	for iter := 0; iter < 20000; iter++ {
		folds := map[string]bool{}
		var names []string
		for len(names) < 6 {
			var b strings.Builder
			for i := 0; i < 1+r.Intn(4); i++ {
				b.WriteString(parts[r.Intn(len(parts))])
			}
			n := b.String()
			if !naming.NameRe.MatchString(n) || folds[naming.Fold(n)] {
				continue
			}
			folds[naming.Fold(n)] = true
			names = append(names, n)
		}
		what := fmt.Sprintf("random %v", names)
		checkScope(t, what, names, optionNames, unionFixedMembers...)
		checkScope(t, what, names, structNames, "_decodeInto")
		if t.Failed() {
			return
		}
	}
}

// TestDerivedShapesMissTheLists: no fixed name ends in `_`, `Id` or
// `Fp32Bits` or starts with `has`/`mutable` + upper-case -- the spellings the
// derived members take -- so no derived member is ever a reserved name.
func TestDerivedShapesMissTheLists(t *testing.T) {
	for _, set := range []map[string]bool{dartKeywords, dartMembers, dartObjectMembers, dartOuterNames, unionFixed} {
		for n := range set {
			if optionShaped(n) || strings.HasSuffix(n, "_") || strings.HasSuffix(n, "Bits") {
				t.Errorf("%s has a derived member's shape", n)
			}
		}
	}
}

// TestMemberNamesPresenceIndependent generates one union, one struct, one
// message and one enum holding every probe at once, and each probe alone, and
// requires every member a probe gets to be spelled the same both ways: adding
// a field, an option or an unrelated type renames nothing.
func TestMemberNamesPresenceIndependent(t *testing.T) {
	schema := func(names []string, withTypes bool) string {
		var opts, fields, consts []string
		for i, n := range names {
			kind := "fp32"
			if i%2 == 1 {
				kind = "string, maxlen: 4"
			}
			opts = append(opts, fmt.Sprintf("%q: { id: %d, type: %s }", n, i, kind))
			fields = append(fields, fmt.Sprintf("%q: { id: %d, type: fp32 }", n, i))
			consts = append(consts, fmt.Sprintf("%q: %d", n, i))
		}
		var b strings.Builder
		b.WriteString("version: 1\n$defs:\n  struct:\n    s: { ")
		b.WriteString(strings.Join(fields, ", "))
		b.WriteString(" }\n")
		if withTypes {
			// Types spelled like every capitalised probe: none may rename a member.
			for _, n := range names {
				if isUpperASCII(n[0]) {
					fmt.Fprintf(&b, "    %q: { x: { id: 0, type: u8 } }\n", "t"+n)
				}
			}
			b.WriteString("    point: { x: { id: 0, type: u8 } }\n    inner: { x: { id: 0, type: u8 } }\n")
		}
		fmt.Fprintf(&b, "  enum:\n    col: { %s }\n", strings.Join(consts, ", "))
		fmt.Fprintf(&b, "messages:\n  m:\n    payload:\n      u: { id: 9000, type: union, oneof: { %s } }\n      st: { id: 9001, type: struct, fields: { $ref: '#/$defs/struct/s' } }\n      c: { id: 9002, type: enum, enum: { $ref: '#/$defs/enum/col' } }\n", strings.Join(opts, ", "))
		fmt.Fprintf(&b, "  n:\n    payload: { %s }\n", strings.Join(fields, ", "))
		return b.String()
	}
	// names of probe n in a generated schema: union option, struct field,
	// message field, enum constant.
	type spell struct{ opt, st, msg []string }
	collect := func(src string) (map[string]spell, map[string]string) {
		s := loadSchema(t, writeDef(t, src))
		g := &gen{schema: s}
		mt := g.members()
		out := map[string]spell{}
		consts := map[string]string{}
		for _, key := range s.NamedOrder {
			nt := s.Named[key]
			switch nt.Category {
			case ir.CatUnion:
				for _, o := range mt.unions[key].opts {
					sp := out[o.orig.Name]
					sp.opt = []string{o.prop, o.idConst, o.has, o.mutable, o.slot, o.bits, o.bitSlot}
					out[o.orig.Name] = sp
				}
			case ir.CatStruct:
				if nt.Path[0] != "s" {
					continue
				}
				for _, f := range nt.Fields {
					sp := out[f.Name]
					n := mt.fields[f]
					sp.st = []string{n.member, n.bits, n.def}
					out[f.Name] = sp
				}
			case ir.CatEnum:
				for _, c := range nt.Consts {
					consts[c.Name] = mt.consts[c]
				}
			}
		}
		for _, m := range s.Messages {
			if m.Name != "n" {
				continue
			}
			for _, f := range m.Fields {
				sp := out[f.Name]
				n := mt.fields[f]
				sp.msg = []string{n.member, n.bits, n.def}
				out[f.Name] = sp
			}
		}
		return out, consts
	}
	all, allConsts := collect(schema(memberProbes, true))
	for i, n := range memberProbes {
		// Alone, at the same position (so the same kind) as in the full set.
		names := make([]string, i+1)
		for j := range names[:i] {
			names[j] = fmt.Sprintf("pad%d", j)
		}
		names[i] = n
		one, oneConsts := collect(schema(names, false))
		if fmt.Sprint(one[n]) != fmt.Sprint(all[n]) {
			t.Errorf("%q: members %v alone, %v beside the other probes", n, one[n], all[n])
		}
		if oneConsts[n] != allConsts[n] {
			t.Errorf("%q: constant %s alone, %s beside the other probes", n, oneConsts[n], allConsts[n])
		}
	}
}
