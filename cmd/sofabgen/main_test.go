package main

import (
	"os"
	"testing"
)

// TestRunExitCodes pins the CLI exit-code contract: an explicit help or version
// request succeeds (0), while a genuine flag misuse fails (2).
func TestRunExitCodes(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"help_long", []string{"--help"}, 0},
		{"help_short", []string{"-h"}, 0},
		{"version_long", []string{"--version"}, 0},
		{"version_short", []string{"-version"}, 0},
		{"bad_flag", []string{"--unknown"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args, devnull, devnull); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}
