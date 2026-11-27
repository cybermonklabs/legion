package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// stepSeen is the line a stand-in of TestLegionGhWritesTheStepItStarts appends to the record
// steps as it runs: who ran, the line the step file at file held then (none where there was no
// file), and whether that run's environment held LEGION_GH_STEP_FILE at all.
func stepSeen(who, file, steps string) string {
	return "printf '%s: %s%s\\n' " + who + " \"$(cat " + shellWord(file) + " 2>/dev/null || echo none)\" " +
		"\"${" + ghStepFileVar + "+, given " + ghStepFileVar + "}\" >>" + shellWord(steps) + "\n"
}

// stepLine is a line the step file holds: a step `legion gh` started and the milliseconds since
// the command started, as the stand-ins of TestLegionGhWritesTheStepItStarts record it after who
// ran.
var stepLine = regexp.MustCompile(`^([a-z-]+): (config|help|check|visibility|rules|gh) (0|[1-9][0-9]*)$`)

// lastStep is what the step file of a call that ran its real gh holds at the end: that step's
// line alone.
var lastStep = regexp.MustCompile(`^gh (0|[1-9][0-9]*)\n$`)

// `legion gh` writes the step a call is in to the file LEGION_GH_STEP_FILE names, at the start of
// each step, as the one line `<step> <milliseconds since the command started>`, by a file made
// beside it and renamed over it. The stand-ins of a guarded write read that file as they run: gh's
// help reads read help, the shared check reads check, the guard's visibility read, which a nested
// `legion gh` runs, reads visibility, and the call itself reads gh, each later than the one
// before it. The nested `legion gh` writes nothing over the call's own steps, and no gh is given
// the variable. A variable naming a path whose directory is of mode 0755, or is a link to a
// directory of mode 0700, or a relative path, writes nothing, and the call ends as it does with a
// step file.
func TestLegionGhWritesTheStepItStarts(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// file is the value LEGION_GH_STEP_FILE is given for a call in the working directory cwd,
		// the path a write of it would make, and the directory that path is in.
		file    func(t *testing.T, cwd string) (value, path, dir string)
		written bool
	}{
		{
			name: "an absolute path in a directory of mode 0700",
			file: func(t *testing.T, _ string) (string, string, string) {
				dir := privateDir(t, 0o700)
				return filepath.Join(dir, "step"), filepath.Join(dir, "step"), dir
			},
			written: true,
		},
		{
			name: "a path in a directory of mode 0755",
			file: func(t *testing.T, _ string) (string, string, string) {
				dir := privateDir(t, 0o755)
				return filepath.Join(dir, "step"), filepath.Join(dir, "step"), dir
			},
		},
		{
			name: "a path in a link to a directory of mode 0700",
			file: func(t *testing.T, _ string) (string, string, string) {
				dir := privateDir(t, 0o700)
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(link, "step"), filepath.Join(dir, "step"), dir
			},
		},
		{
			name: "a relative path",
			file: func(_ *testing.T, cwd string) (string, string, string) {
				return "step", filepath.Join(cwd, "step"), cwd
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd := privateDir(t, 0o700)
			value, path, dir := tc.file(t, cwd)
			steps := filepath.Join(t.TempDir(), "steps")
			bin := t.TempDir()
			stub := filepath.Join(bin, "real-gh")
			script := "#!/bin/sh\nfor last; do :; done\nwho=call\n" +
				"case \" $* \" in\n" + ghVisibilityReadArgv + ") who=visibility-read;;\nesac\n" +
				"if [ \"$last\" = --help ]; then who=help-read; fi\n" +
				stepSeen("\"$who\"", path, steps) +
				ghStandInPrefix(help) + "exit 0\n"
			if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{
				"GH_CONFIG_DIR":        defaultDir,
				"LEGION_GH_OWNERS_DIR": root,
				"LEGION_GH_PATH":       stub,
				"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), stepSeen("check", path, steps)+leakguardtest.Passes),
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
				ghStepFileVar:          value,
			}
			args := []string{"pr", "create", "-R", "acme/widgets", "--title", "a title the rules pass"}
			code, stdout, stderr := legionGhIn(t, cwd, env, "", args...)
			if code != 0 || stdout != "" {
				t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and nothing on standard output, as with no step file", args, code, stdout, stderr)
			}
			recorded := strings.Split(strings.TrimSuffix(readRecord(t, steps), "\n"), "\n")
			if !tc.written {
				for _, line := range recorded {
					if !strings.HasSuffix(line, ": none") {
						t.Errorf("a run read %q, want no step file and no %s", line, ghStepFileVar)
					}
				}
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
					t.Errorf("the directory the step file would be in holds %v, %v; want nothing", entries, err)
				}
				return
			}
			// Each run's step, the help reads counted once, and each step's milliseconds no fewer
			// than the one's before it.
			var got []string
			last := int64(-1)
			for _, line := range recorded {
				match := stepLine.FindStringSubmatch(line)
				if match == nil {
					t.Fatalf("a run read %q, want `<who>: <step> <ms>` and no %s given", line, ghStepFileVar)
				}
				ms, err := strconv.ParseInt(match[3], 10, 64)
				if err != nil || ms < last {
					t.Errorf("a run read %q after %d ms, want milliseconds no fewer than the step's before", line, last)
				}
				last = ms
				if seen := match[1] + ": " + match[2]; len(got) == 0 || got[len(got)-1] != seen {
					got = append(got, seen)
				}
			}
			if want := []string{"help-read: help", "check: check", "visibility-read: visibility", "call: gh"}; !slices.Equal(got, want) {
				t.Errorf("the runs read the steps %q, want %q", got, want)
			}
			// The last step is the real gh's, and the file is all that is left in its directory.
			if content, err := os.ReadFile(path); err != nil || !lastStep.Match(content) {
				t.Errorf("the step file holds %q, %v; want the one line `gh <ms>`", content, err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Errorf("the step file's directory holds %v, %v; want the step file alone", entries, err)
			}
		})
	}
}

// privateDir is a directory of the test's own of mode perm.
func privateDir(t *testing.T, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, perm); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The real gh is given neither of the two variables a caller hands `legion gh` itself: the step
// file it writes the call's steps to (LEGION_GH_STEP_FILE) and the deadline its guard runs under
// (LEGION_GH_DEADLINE). Both are this command's, and gh's environment is an allowlist
// (ghChildKeeps) that names neither.
func TestGhsChildEnvironmentGivesGhNeitherOfLegionGhsOwnVariables(t *testing.T) {
	t.Parallel()
	for _, name := range []string{ghStepFileVar, ghDeadlineVar} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			caller := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), name + "=/an/invented/value"}
			for _, entry := range ghChildEnviron(caller, nil) {
				if strings.HasPrefix(entry, name+"=") {
					t.Fatalf("gh's environment holds %q, want no %s", entry, name)
				}
			}
		})
	}
}
