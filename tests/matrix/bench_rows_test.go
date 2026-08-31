package matrix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// TestBenchRowsAreNotVacuous guards tests/bench against a row that measures its
// sibling. results.txt prints one line per row, and a reader takes each line to
// be a measurement of the axis the row's `config` names. Two rows of the same
// language whose configs differ must therefore GENERATE different code — if they
// do not, the second row measures the first, and the file reports coverage of an
// axis nothing ever touched.
//
// That is not hypothetical. `ts-bigint` and `ts-long` were byte-identical on the
// bench schema for as long as both rows existed (issue #336): `int64: long`
// backs 64-bit ARRAYS with corelib `Long[]` and deliberately leaves scalars
// alone, the schema had u64/i64 scalars but no 64-bit array, so the two configs
// collapsed onto each other. Their committed numbers differed by 3 and 1 Ir/op —
// exactly the JIT jitter `stabilize()` exists to absorb, presented as a result.
//
// Rows that share a config are exempt on purpose: they declare an axis that is
// not codegen at all. `python` and `python-native` run the identical generated
// code on corelib-py's two engines, which is a legitimate reason for a second
// row and something this test cannot and should not judge. So are rows measured
// on DIFFERENT schemas: `go` and `go-unbounded` share a config and separate on
// the message they measure, which TestBenchSchemasBoundAsDeclared guards instead.
//
// Hermetic: generation only, no corelib and no toolchain.
func TestBenchRowsAreNotVacuous(t *testing.T) {
	doc := readBenchRows(t)

	schemas := map[string]*ir.Schema{}
	for _, r := range doc.Rows {
		path := doc.schemaOf(r.Schema)
		if _, ok := schemas[path]; ok {
			continue
		}
		s, err := buildIR(t, filepath.Join("..", "..", filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("bench schema %s must validate: %v", path, err)
		}
		schemas[path] = s
	}

	// gen renders one row's generated files as a single comparable image.
	gen := func(lang, cfgText, schema string) (string, error) {
		cfg := config.Empty()
		if cfgText != "" {
			parsed, err := config.Parse([]byte(cfgText), "rows.json")
			if err != nil {
				return "", err
			}
			cfg = parsed
		}
		b, ok := generator.Lookup(lang)
		if !ok {
			return "", nil // language not registered in this build
		}
		files, err := b.Generate(schemas[schema], cfg.Effective(lang))
		if err != nil {
			return "", err
		}
		var image []byte
		for _, f := range files {
			image = append(image, f.Path...)
			image = append(image, 0)
			image = append(image, f.Content...)
			image = append(image, 0)
		}
		return string(image), nil
	}

	// Grouped by (lang, schema): two rows measuring different messages are not
	// each other's duplicate whatever their configs say.
	type group struct{ lang, schema string }
	byGroup := map[group][]int{}
	for i, r := range doc.Rows {
		if r.ID == "" || r.Lang == "" {
			continue // comment-only entries
		}
		g := group{r.Lang, doc.schemaOf(r.Schema)}
		byGroup[g] = append(byGroup[g], i)
	}

	for g, idxs := range byGroup {
		if _, ok := generator.Lookup(g.lang); !ok {
			continue
		}
		images := map[int]string{}
		for _, i := range idxs {
			img, err := gen(g.lang, doc.Rows[i].Config, g.schema)
			if err != nil {
				t.Errorf("[%s] row %s: generate: %v", g.lang, doc.Rows[i].ID, err)
				continue
			}
			images[i] = img
		}
		for a := 0; a < len(idxs); a++ {
			for b := a + 1; b < len(idxs); b++ {
				ra, rb := doc.Rows[idxs[a]], doc.Rows[idxs[b]]
				if ra.Config == rb.Config {
					continue // same codegen by declaration; a non-config axis separates them
				}
				ia, oka := images[idxs[a]]
				ib, okb := images[idxs[b]]
				if !oka || !okb {
					continue
				}
				if ia == ib {
					t.Errorf("bench rows %q and %q declare different configs but generate "+
						"byte-identical code for %s: one row is measuring the other, and "+
						"results.txt presents that as coverage of an axis this schema cannot "+
						"reach. Either give the schema a field the config acts on, or drop the "+
						"row and say why in rows.json.", ra.ID, rb.ID, g.schema)
				}
			}
		}
	}
}

// TestBenchSchemasBoundAsDeclared guards what each bench schema is FOR, which is
// the other half of a row not being vacuous: a row measured on the wrong kind of
// schema reports coverage of something it cannot see.
//
// The default schema must stay fully bounded. That is what lets every row —
// including the footprint ones, whose profiles reject an unbounded field at
// schema validation — measure the identical message, and `tests/bench/README.md`
// says so.
//
// A row that names its OWN schema exists because the default cannot show
// something, and today that something is the receiver caps (ARCHITECTURE §9.5,
// CORELIB_PLAN §6.2.1): they govern schema-UNBOUNDED fields only, so on the
// default schema not one `max_dyn_*` constant, cap argument or guard is emitted
// in any row, in any target. An extra schema whose fields are all bounded would
// therefore be a second row measuring the first, with the difference hidden in a
// second file rather than in a config string.
//
// The assertion is on the IR rather than on generated substrings on purpose: a
// per-target grep would have to be written eleven times and would still be
// checking spelling, whereas `ir.Bounds` is the very thing every backend keys its
// cap plumbing off (`HasDyn*` decides liveness).
func TestBenchSchemasBoundAsDeclared(t *testing.T) {
	doc := readBenchRows(t)

	load := func(path string) ir.BoundsInfo {
		s, err := buildIR(t, filepath.Join("..", "..", filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("bench schema %s must validate: %v", path, err)
		}
		var all []*ir.Field
		for _, m := range s.Messages {
			all = append(all, m.Fields...)
		}
		return ir.Bounds(all)
	}

	if b := load(doc.Schema); b.HasDyn() {
		t.Errorf("the default bench schema %s has an unbounded field (array=%v string=%v "+
			"blob=%v). Every row measures it, including the footprint rows whose profiles "+
			"reject an unbounded field outright — they would stop generating. Put the "+
			"unbounded field in a row-level schema instead.",
			doc.Schema, b.HasDynArray, b.HasDynString, b.HasDynBlob)
	}

	for _, r := range doc.Rows {
		if r.ID == "" || r.Schema == "" {
			continue
		}
		b := load(r.Schema)
		if !b.HasDynArray || !b.HasDynString || !b.HasDynBlob {
			t.Errorf("row %q measures %s, which is not unbounded in every kind "+
				"(array=%v string=%v blob=%v). The row exists to make the receiver caps "+
				"visible; a kind that is schema-bounded there emits no cap at all and the "+
				"row silently stops covering it.",
				r.ID, r.Schema, b.HasDynArray, b.HasDynString, b.HasDynBlob)
		}
		if b.MaxCount == 0 || b.MaxStringLen == 0 {
			t.Errorf("row %q measures %s, which has no schema-BOUNDED array/string left "+
				"(MaxCount=%d MaxStringLen=%d). The cost being measured is a decoder telling "+
				"a schema bound (INVALID) from a receiver cap (LimitExceeded) per field; "+
				"drop the bounded twins and the row measures caps in isolation instead.",
				r.ID, r.Schema, b.MaxCount, b.MaxStringLen)
		}
	}
}

// benchRows is rows.json as these two tests read it.
type benchRows struct {
	Schema string `json:"schema"`
	Rows   []struct {
		ID     string `json:"id"`
		Lang   string `json:"lang"`
		Config string `json:"config"`
		Schema string `json:"schema"` // empty: the row measures the default schema
	} `json:"rows"`
}

// schemaOf resolves a row's `schema` against the top-level default.
func (d benchRows) schemaOf(rowSchema string) string {
	if rowSchema == "" {
		return d.Schema
	}
	return rowSchema
}

func readBenchRows(t *testing.T) benchRows {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "bench", "rows.json"))
	if err != nil {
		t.Fatalf("read rows.json: %v", err)
	}
	var doc benchRows
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rows.json: %v", err)
	}
	return doc
}
