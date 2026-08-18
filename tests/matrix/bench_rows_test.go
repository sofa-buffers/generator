package matrix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
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
// row and something this test cannot and should not judge.
//
// Hermetic: generation only, no corelib and no toolchain.
func TestBenchRowsAreNotVacuous(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "bench", "rows.json"))
	if err != nil {
		t.Fatalf("read rows.json: %v", err)
	}
	var doc struct {
		Schema string `json:"schema"`
		Rows   []struct {
			ID     string `json:"id"`
			Lang   string `json:"lang"`
			Config string `json:"config"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rows.json: %v", err)
	}

	s, err := buildIR(t, filepath.Join("..", "..", filepath.FromSlash(doc.Schema)))
	if err != nil {
		t.Fatalf("bench schema %s must validate: %v", doc.Schema, err)
	}

	// gen renders one row's generated files as a single comparable image.
	gen := func(lang, cfgText string) (string, error) {
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
		files, err := b.Generate(s, cfg.Effective(lang))
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

	byLang := map[string][]int{}
	for i, r := range doc.Rows {
		if r.ID == "" || r.Lang == "" {
			continue // comment-only entries
		}
		byLang[r.Lang] = append(byLang[r.Lang], i)
	}

	for lang, idxs := range byLang {
		if _, ok := generator.Lookup(lang); !ok {
			continue
		}
		images := map[int]string{}
		for _, i := range idxs {
			img, err := gen(lang, doc.Rows[i].Config)
			if err != nil {
				t.Errorf("[%s] row %s: generate: %v", lang, doc.Rows[i].ID, err)
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
						"row and say why in rows.json.", ra.ID, rb.ID, doc.Schema)
				}
			}
		}
	}
}
