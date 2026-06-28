// Command sbufgen is the SofaBuffers code generator CLI (PLAN §8.8). The
// surface is deliberately tiny — everything configurable lives in the config
// file; only --in/--out override it (the paths that legitimately vary between
// machines). No per-option flags.
//
//	sbufgen --config <file> --lang <target> [--in <dir>] [--out <dir>]
//
// In M0 no language backend is wired yet, so a run validates the definition(s),
// resolves $ref, and builds the IR, printing a summary. With --lang set but no
// backend registered, it reports that cleanly (exit 0 for the validate/IR
// gate; a future backend turns this into real output).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/pipeline"

	// Language backends self-register via init(). The core never imports these;
	// only the CLI binary does (dependency arrows point inward, PLAN §8.6).
	_ "github.com/sofa-buffers/generator/generators/c"
	_ "github.com/sofa-buffers/generator/generators/cpp"
	_ "github.com/sofa-buffers/generator/generators/csharp"
	_ "github.com/sofa-buffers/generator/generators/golang"
	_ "github.com/sofa-buffers/generator/generators/python"
	_ "github.com/sofa-buffers/generator/generators/rust"
	_ "github.com/sofa-buffers/generator/generators/typescript"
)

const version = "0.1.0-m0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("sbufgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfgPath      = fs.String("config", "", "path to the YAML/JSON config (§7); carries all options")
		lang         = fs.String("lang", "", "target backend: "+strings.Join(config.KnownTargets(), "|"))
		inDir        = fs.String("in", "", "input definition file or folder (overrides generic.input_dir)")
		outDir       = fs.String("out", "", "output folder (overrides generic.output_dir)")
		printDefault = fs.Bool("print-defaults", false, "print the effective resolved config for --lang and exit")
		dumpIR       = fs.Bool("dump-ir", false, "print the built IR as JSON for each input and exit (no codegen)")
		showVersion  = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "sbufgen %s — SofaBuffers code generator\n\n", version)
		fmt.Fprintf(stderr, "usage: sbufgen --config <file> --lang <target> [--in <dir>] [--out <dir>]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	// Load config (or an empty one for the bare validate/IR flow).
	var cfg *config.Config
	if *cfgPath != "" {
		c, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		cfg = c
	} else {
		cfg = config.Empty()
	}

	if *printDefault {
		eff := cfg.Effective(*lang)
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(eff)
		return 0
	}

	if *lang != "" && !knownTarget(*lang) {
		fmt.Fprintf(stderr, "error: unknown --lang %q (known: %s)\n", *lang, strings.Join(config.KnownTargets(), ", "))
		return 1
	}

	// Resolve input: --in overrides generic.input_dir.
	input := *inDir
	if input == "" {
		if s, ok := cfg.Effective(*lang)["input_dir"].(string); ok {
			input = s
		}
	}
	if input == "" {
		fmt.Fprintln(stderr, "error: no input given (set --in or generic.input_dir)")
		return 1
	}
	defs, err := collectDefs(input)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(defs) == 0 {
		fmt.Fprintf(stderr, "error: no definition files found under %q\n", input)
		return 1
	}

	// Resolve output dir (only needed once a backend writes files).
	out := *outDir
	if out == "" {
		if s, ok := cfg.Effective(*lang)["output_dir"].(string); ok {
			out = s
		}
	}

	exit := 0
	for _, def := range defs {
		// --dump-ir stops after the IR (stages [1]-[4]); no backend selected.
		runLang := *lang
		if *dumpIR {
			runLang = ""
		}
		res, err := pipeline.Run(pipeline.Options{DefPath: def, Lang: runLang, Config: cfg, OutDir: out})
		if err != nil {
			var nb *pipeline.NoBackendError
			if errors.As(err, &nb) {
				// IR built fine; just no emitter wired (M0).
				printSummary(stdout, def, res.Schema)
				fmt.Fprintf(stdout, "  (validated + IR built; %v)\n", nb)
				continue
			}
			fmt.Fprintf(stderr, "error: %v\n", err)
			exit = 1
			continue
		}
		if *dumpIR {
			stdout.Write(res.Schema.Dump())
			continue
		}
		printSummary(stdout, def, res.Schema)
		if len(res.Files) > 0 {
			if err := writeFiles(out, res.Files); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				exit = 1
				continue
			}
			fmt.Fprintf(stdout, "  wrote %d file(s) to %s\n", len(res.Files), out)
		}
	}
	return exit
}

func knownTarget(lang string) bool {
	for _, t := range config.KnownTargets() {
		if t == lang {
			return true
		}
	}
	return false
}

// collectDefs returns the definition files for a file-or-directory input.
func collectDefs(input string) ([]string, error) {
	info, err := os.Stat(input)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{input}, nil
	}
	var defs []string
	entries, err := os.ReadDir(input)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
			defs = append(defs, filepath.Join(input, e.Name()))
		}
	}
	sort.Strings(defs)
	return defs, nil
}

func writeFiles(outDir string, files []generator.File) error {
	for _, f := range files {
		full := filepath.Join(outDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func printSummary(stdout *os.File, def string, s *ir.Schema) {
	var structs, unions, enums, bitfields int
	for _, key := range s.NamedOrder {
		switch s.Named[key].Category {
		case ir.CatStruct:
			structs++
		case ir.CatUnion:
			unions++
		case ir.CatEnum:
			enums++
		case ir.CatBitfield:
			bitfields++
		}
	}
	fmt.Fprintf(stdout, "✓ %s — valid (schema v%d)\n", def, s.Version)
	fmt.Fprintf(stdout, "  %d message(s); named types: %d struct, %d union, %d enum, %d bitfield\n",
		len(s.Messages), structs, unions, enums, bitfields)
	for _, m := range s.Messages {
		fmt.Fprintf(stdout, "    message %s — %d field(s)\n", m.Name, len(m.Fields))
	}
}
