// Command sofabgen is the SofaBuffers code generator CLI (PLAN §8.8). The
// surface is deliberately tiny — everything configurable lives in the config
// file; only --in/--out and --format override it (the paths that legitimately
// vary between machines, and whether this run may spawn the target's
// formatter). No per-option flags.
//
//	sofabgen --config <file> --lang <target> [--in <dir>] [--out <dir>]
//	         [--format off|auto|require]
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
	"runtime/debug"
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
	_ "github.com/sofa-buffers/generator/generators/dart"
	_ "github.com/sofa-buffers/generator/generators/docs"
	_ "github.com/sofa-buffers/generator/generators/golang"
	_ "github.com/sofa-buffers/generator/generators/java"
	_ "github.com/sofa-buffers/generator/generators/kotlin"
	_ "github.com/sofa-buffers/generator/generators/python"
	_ "github.com/sofa-buffers/generator/generators/rust"
	_ "github.com/sofa-buffers/generator/generators/typescript"
	_ "github.com/sofa-buffers/generator/generators/zig"
)

// version is the compiled-in fallback for builds where module version info is
// absent (local `go build`/`go run`). The release tag is the single source of
// truth: the release workflow injects it via -ldflags "-X main.version=<tag>",
// so release binaries report the exact tag and this placeholder never ships.
// See resolveVersion.
var version = "0.0.0-dev"

// resolveVersion prefers the module version the Go toolchain embeds when the
// binary is produced by `go install github.com/…/cmd/sofabgen@vX.Y.Z` (or any
// module-aware build of a tagged version), so an install-by-version reports that
// version. It falls back to the compiled-in constant when no module version is
// present ("(devel)" or empty), i.e. local builds and the release workflow.
func resolveVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return version
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	ver := resolveVersion()
	fs := flag.NewFlagSet("sofabgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfgPath      = fs.String("config", "", "path to the YAML/JSON config (§7); carries all options")
		lang         = fs.String("lang", "", "target backend: "+strings.Join(config.KnownTargets(), "|"))
		inDir        = fs.String("in", "", "input definition file or folder (overrides generic.input_dir)")
		outDir       = fs.String("out", "", "output folder (overrides generic.output_dir)")
		printDefault = fs.Bool("print-defaults", false, "print the effective resolved config for --lang and exit")
		dumpIR       = fs.Bool("dump-ir", false, "print the built IR as JSON for each input and exit (no codegen)")
		formatFlag   = fs.String("format", "", "run the target's canonical formatter over the generated files: off|auto|require (default off, or generic.format)")
		showVersion  = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "sofabgen %s — SofaBuffers code generator\n\n", ver)
		fmt.Fprintf(stderr, "usage: sofabgen --config <file> --lang <target> [--in <dir>] [--out <dir>] [--format off|auto|require]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// -h/--help is an explicit, valid request (flag prints usage); exit 0.
		// Any other parse error is a misuse; exit 2.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, ver)
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

	// Resolve the format switch: built-in default < generic.format < --format.
	mode, err := resolveFormatMode(fs, *formatFlag, cfg, *lang)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
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
	// A backend whose canonical formatter is an external program says so once
	// per run, not once per definition file.
	formatNoted := false
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
			files, err := formatFiles(mode, *lang, out, res.Files, stderr, &formatNoted)
			if err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				exit = 1
				continue
			}
			if err := writeFiles(out, files); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				exit = 1
				continue
			}
			fmt.Fprintf(stdout, "  wrote %d file(s) to %s\n", len(files), out)
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

// formatMode is the value of the --format switch (and of the generic.format
// config key): whether sofabgen may run the target's canonical formatter over
// what it generated.
type formatMode string

const (
	// formatOff is the DEFAULT. No external program is spawned, ever. A run is
	// then a pure function of (IR, config): the same sofabgen version writes the
	// same bytes on every machine, whatever tools happen to be installed — which
	// is also what the golden snapshots in tests/matrix rest on.
	formatOff formatMode = "off"
	// formatAuto formats when the tool is there and writes unformatted output
	// with a one-line note on stderr when it is not.
	formatAuto formatMode = "auto"
	// formatRequire formats, and fails the run when the tool is missing or
	// refuses the generated code.
	formatRequire formatMode = "require"
)

// formatModes lists the accepted values, in the order the help text names them.
var formatModes = []formatMode{formatOff, formatAuto, formatRequire}

// parseFormatMode turns a user-supplied string into a mode, naming all three
// valid values on a miss rather than falling back to a default: a typo in
// `--format=requre` must not silently write unformatted code.
func parseFormatMode(s string) (formatMode, error) {
	for _, m := range formatModes {
		if s == string(m) {
			return m, nil
		}
	}
	names := make([]string, len(formatModes))
	for i, m := range formatModes {
		names[i] = string(m)
	}
	return "", fmt.Errorf("unknown format mode %q (valid: %s)", s, strings.Join(names, ", "))
}

// resolveFormatMode applies the documented precedence — built-in default
// (off) < generic.format < --format — and reports a bad value from either
// source, naming the source so the user knows which one to fix.
func resolveFormatMode(fs *flag.FlagSet, flagVal string, cfg *config.Config, lang string) (formatMode, error) {
	mode := formatOff
	if s, ok := cfg.Effective(lang)["format"].(string); ok {
		m, err := parseFormatMode(s)
		if err != nil {
			return "", fmt.Errorf("config generic.format: %w", err)
		}
		mode = m
	}
	// A flag only overrides when it was actually GIVEN; its zero value is not a
	// choice. flag has no "was set" accessor, so ask the FlagSet.
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "format" {
			given = true
		}
	})
	if given {
		m, err := parseFormatMode(flagVal)
		if err != nil {
			return "", fmt.Errorf("--format: %w", err)
		}
		mode = m
	}
	return mode, nil
}

// formatFiles hands the generated files to the backend's canonical formatter,
// when it has one (generator.Formatter) AND the run asked for it. This sits in
// the CLI rather than in Generate on purpose: Generate must stay a pure function
// of (IR, config) — the golden gate compares its bytes — while the tree a user
// actually receives is the one written here, and that is the tree their
// `cargo fmt --check` runs over.
//
// The switch decides what happens:
//
//	off      nothing is looked up and nothing is spawned; the files are the
//	         emitters' own output, byte for byte, on every machine.
//	auto     format when the tool is installed; when it is not, write
//	         unformatted output and print the reason once per run.
//	require  format, and fail when the tool is missing.
//
// A formatter that RUNS and refuses the code is an error under auto as well as
// under require: that means the backend emitted something that does not parse,
// which no switch should turn into a written file.
func formatFiles(mode formatMode, lang, outDir string, files []generator.File, stderr *os.File, noted *bool) ([]generator.File, error) {
	if mode == formatOff {
		return files, nil
	}
	b, ok := generator.Lookup(lang)
	if !ok {
		return files, nil
	}
	f, ok := b.(generator.Formatter)
	if !ok {
		return files, nil
	}
	// The formatter runs in the output dir so it sees the project's own
	// formatter config; it has to exist before the files land in it.
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, err
		}
	}
	out, note, err := f.Format(files, outDir)
	if err != nil {
		return nil, err
	}
	// A non-empty note means the tool is not installed and NOTHING was formatted
	// (generator.Formatter). Under require that is the failure the user asked
	// for; under auto it is a one-line report.
	if note != "" {
		if mode == formatRequire {
			return nil, fmt.Errorf("--format=require: %s (generated %s was not formatted; --format=auto writes it unformatted instead)", note, lang)
		}
		if !*noted {
			*noted = true
			fmt.Fprintf(stderr, "note: %s — generated %s is written unformatted (--format=require fails the run instead)\n", note, lang)
		}
	}
	return out, nil
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
