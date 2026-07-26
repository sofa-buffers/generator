package matrix

import (
	"bufio"
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// devReference matches the traces of THIS project's development that must not
// reach a user's generated code: spec section citations, issue and PR numbers,
// Crucible finding ids, and corelib repository names.
//
// The rationale is not tidiness. Generated code is read by people who have the
// schema and nothing else — they cannot follow "generator#183" or look up
// "MESSAGE_SPEC §7.3", so such a comment explains the code to exactly the one
// audience that does not need it, while telling its actual readers nothing. What
// belongs in generated code is what documents the generated code: the schema's
// own descriptions, and enough prose to explain what a function does.
var devReference = regexp.MustCompile(
	`MESSAGE_SPEC|CORELIB_PLAN|Crucible|` +
		`\bF-\d{4}\b|` + // Crucible finding id
		`\b(?:generator|corelib-[a-z-]+)#\d+|` + // cross-repo issue reference
		`\bissue #\d+|` +
		`\((?:#\d+[,/ ]*)+\)`, // a bare "(#183)" / "(#183/#193)" parenthetical
)

// corelibImport allows the one legitimate occurrence of a corelib repo name: an
// import path or package coordinate, which is code, not commentary.
var corelibImport = regexp.MustCompile(`github\.com/sofa-buffers/|sofa-buffers/corelib|corelib-[a-z-]+"`)

// TestGeneratedCodeCarriesNoDevelopmentReferences sweeps every backend over the
// whole corpus and asserts that no generated line cites a spec section, an issue
// number, or a corelib repository.
//
// The schema's own text passes through untouched: a `summary:` or `description:`
// that mentions MESSAGE_SPEC is the author's content, so lines are only checked
// when the reference did not come from the definition itself.
func TestGeneratedCodeCarriesNoDevelopmentReferences(t *testing.T) {
	defs, _ := filepath.Glob("corpus/defs/*.yaml")
	defs = append(defs, filepath.Join("..", "..", "examples", "messages", "example.yaml"))
	if len(defs) < 2 {
		t.Fatal("no defs found")
	}
	modes := []map[string]any{
		{"emit": "sources"},
		{"emit": "project", "timestamp": false},
	}

	for _, def := range defs {
		s, err := buildIR(t, def)
		if err != nil {
			t.Fatalf("%s should validate: %v", def, err)
		}
		// Every string the definition itself contributes — descriptions,
		// summaries, units, names. A generated line that merely echoes one of
		// these is the author's text, not ours.
		schemaText := collectSchemaText(s)

		for _, lang := range generator.Registered() {
			if fixedOnlyTarget(lang) && hasUnboundedField(s) {
				continue // heapless target cannot size an unbounded field
			}
			b, _ := generator.Lookup(lang)
			for _, cfg := range modes {
				files, err := b.Generate(s, cfg)
				if err != nil {
					continue // generate errors are covered by the other sweeps
				}
				for _, f := range files {
					sc := bufio.NewScanner(bytes.NewReader(f.Content))
					sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
					for n := 1; sc.Scan(); n++ {
						line := sc.Text()
						hit := devReference.FindString(line)
						if hit == "" || corelibImport.MatchString(line) || fromSchema(line, schemaText) {
							continue
						}
						t.Errorf("[%s] %s (%s) line %d cites %q — generated code is read by "+
							"people who have the schema and nothing else:\n  %s",
							lang, f.Path, filepath.Base(def), n, hit, strings.TrimSpace(line))
					}
				}
			}
		}
	}
}

// fromSchema reports whether a generated line's content came from the definition
// (a description or summary echoed into a doc comment).
func fromSchema(line string, schemaText []string) bool {
	for _, txt := range schemaText {
		if txt != "" && strings.Contains(line, txt) {
			return true
		}
	}
	return false
}

// collectSchemaText returns the author-supplied strings of a schema, split into
// the word runs a backend may re-wrap a doc comment across.
func collectSchemaText(s *ir.Schema) []string {
	var out []string
	add := func(txt string) {
		for _, part := range strings.FieldsFunc(txt, func(r rune) bool { return r == '\n' || r == '\r' }) {
			words := strings.Fields(part)
			for i := 0; i+3 <= len(words); i++ {
				out = append(out, strings.Join(words[i:i+3], " "))
			}
			if n := len(words); n > 0 && n < 3 {
				out = append(out, strings.Join(words, " "))
			}
		}
	}
	var fields func([]*ir.Field)
	fields = func(fs []*ir.Field) {
		for _, f := range fs {
			add(f.Description)
			add(f.Unit)
		}
	}
	for _, m := range s.Messages {
		add(m.Summary)
		fields(m.Fields)
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		add(nt.Summary)
		fields(nt.Fields)
		for _, c := range nt.Consts {
			add(c.Description)
		}
		for _, fl := range nt.Flags {
			add(fl.Description)
		}
	}
	return out
}
