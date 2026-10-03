package golang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildGoHarness generates a Go project for def, wires it to the corelib-go
// checkout at corelib and builds its harness, returning the binary's path.
func buildGoHarness(t *testing.T, corelib, def string) (string, error) {
	t.Helper()
	s := schemaFromYAMLString(t, def)
	files, err := (&Backend{}).Generate(s, map[string]any{
		"emit": "project", "package": "messages", "module_path": "example.com/vec", "go_version": "1.21",
	})
	if err != nil {
		return "", err
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		content := f.Content
		if f.Path == "go.mod" {
			content = []byte(strings.ReplaceAll(string(content), "${SOFAB_GO_CORELIB}", corelib))
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return "", err
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"vet", "./..."}, {"build", "-o", "harness_bin", "./harness"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go %v: %v\n%s", args, err, out)
		}
	}
	return filepath.Join(dir, "harness_bin"), nil
}
