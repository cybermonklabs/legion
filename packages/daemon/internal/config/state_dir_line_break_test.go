package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every phase task opens with its working copy line, and a send finds the end of that line at the
// task's first line break (workflow's taskOpening and workingCopyLine). Under tmux the line names
// a workspace directory under state_dir, so a state_dir that holds a line break would end the line
// inside its own directory. The load refuses such a state_dir, a carriage return as much as a line
// feed, and the refusal names the key and says why. The load stores state_dir absolute, resolved
// against the configuration file's directory and then made absolute against the working
// directory, and the refusal reads that same path, which the start then uses as it is (cmd/legion's
// start). So a relative state_dir under a directory that holds a line break is refused, and so is
// one that a file named by a relative path, from a working directory that holds a line break,
// leaves relative. A path that holds no line break loads as it resolves, one that holds a full stop
// and a space included, and one that a file named by a relative path leaves relative loads made
// absolute.
func TestAStateDirThatHoldsALineBreakIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stateDir is the value the file gives state_dir.
		stateDir string
		// under is the directory, inside the one the test writes the file in, that holds the file
		// instead, "" for none.
		under string
		// relative is whether the load names the file by its base name, from its own directory as
		// the working directory, as `legion start --config legion.yaml` does.
		relative bool
		// refused is whether the load refuses the file.
		refused bool
	}{
		{name: "a line feed", stateDir: "/srv/legion\nstate", refused: true},
		{name: "a carriage return", stateDir: "/srv/legion\rstate", refused: true},
		{name: "a relative path under a directory that holds a line feed", stateDir: "state", under: "legion\nconfig", refused: true},
		{name: "a relative path of a file named by a relative path, from a working directory that holds a line feed",
			stateDir: "state", under: "legion\nwork", relative: true, refused: true},
		{name: "a plain path", stateDir: "/srv/legion v2. state"},
		{name: "a relative path of a file named by a relative path", stateDir: "state", relative: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, strings.Replace(minimalFile, "state_dir: /var/lib/legion", "state_dir: "+strconv.Quote(tc.stateDir), 1))
			if tc.under != "" {
				moved := filepath.Join(filepath.Dir(path), tc.under, filepath.Base(path))
				if err := os.Mkdir(filepath.Dir(moved), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				path = moved
			}
			resolved := tc.stateDir
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), resolved)
			}
			named := path
			if tc.relative {
				t.Chdir(filepath.Dir(path))
				named = filepath.Base(path)
				// The load makes a relative state_dir absolute against the working directory as the
				// process reads it, which is what it stores and what the refusal names.
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				resolved = filepath.Join(wd, tc.stateDir)
			}
			cfg, err := Load(named, noEnv)
			if !tc.refused {
				if err != nil {
					t.Fatalf("Load: %v, want the file loaded: its state_dir holds no line break", err)
				}
				if cfg.StateDir != resolved {
					t.Fatalf("StateDir = %q, want %q", cfg.StateDir, resolved)
				}
				return
			}
			want := fmt.Sprintf("state_dir %q holds a line break, which a state directory may not hold: under tmux the working copy line every phase task opens with names a workspace directory under it and ends at its line break", resolved)
			if err == nil {
				t.Fatalf("Load succeeded with StateDir %q, want error %q", cfg.StateDir, want)
			}
			if err.Error() != want {
				t.Errorf("Load error = %q, want %q", err.Error(), want)
			}
		})
	}
}
