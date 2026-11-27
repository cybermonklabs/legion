package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// regenerateGhHelpStandIn is the stand-in gh TestRegenerateGhHelpPrintsNoWordOfGhs runs the script
// on: it logs each call to GH_STANDIN_LOG, says GH_STANDIN_SAYS on its stderr where that is set,
// exits 4 on the call GH_STANDIN_FAILS names, answers --version with GH_STANDIN_VERSION and a
// second line as gh's does, and any other call with a help text. The version's two lines go out in
// one write (cat), as gh writes them: the script reads them through `| head -1`, and bash's printf
// writes each line on its own, so a head that had taken the first line and gone left the second
// write a SIGPIPE, which failed the script's run of gh --version (exit 141) about once in 120 runs.
const regenerateGhHelpStandIn = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GH_STANDIN_LOG"
[ -z "${GH_STANDIN_SAYS:-}" ] || printf '%s\n' "$GH_STANDIN_SAYS" >&2
[ "$*" != "${GH_STANDIN_FAILS:-}" ] || exit 4
if [ "$*" = --version ]; then
  cat <<EOF
$GH_STANDIN_VERSION
https://github.com/cli/cli/releases/tag/v2.98.0
EOF
else
  printf 'help of %s\n' "$*"
fi
`

// TestRegenerateGhHelpPrintsNoWordOfGhs runs testdata/regenerate-gh-help.sh, copied byte for byte
// into a directory of its own (it replaces the gh-help/ beside it), on a stand-in gh that
// LEGION_GH_PATH names. Whatever answers gh's calls in GitHub's place holds gh's token and writes
// what gh says and answers, so the script prints neither: the stand-in says the marker on its
// stderr, or answers it as its version, and no run of its characters (oauth2calltest.RequireNoPiece,
// capture legion.regenerate-gh-help/<case>) may be in what the script printed, stdout and stderr
// together. The twin is the file the script names, which does hold the marker; the controls are
// the stand-in's log of the calls it was given and, where the version is of gh's own shape, that
// version printed.
func TestRegenerateGhHelpPrintsNoWordOfGhs(t *testing.T) {
	const marker = "not-a-secret-token"
	const version = "gh version 2.98.0 (2026-08-20)"
	script, err := os.ReadFile(filepath.Join("testdata", "regenerate-gh-help.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// run runs the script in a directory of its own with the stand-in's settings, and answers that
	// directory, what the script printed and its exit status.
	run := func(t *testing.T, settings ...string) (string, string, int) {
		t.Helper()
		dir := t.TempDir()
		at := filepath.Join(dir, "testdata", "regenerate-gh-help.sh")
		gh := filepath.Join(dir, "gh")
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, script, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gh, []byte(regenerateGhHelpStandIn), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", at)
		cmd.Env = append(os.Environ(), append([]string{"LEGION_GH_PATH=" + gh, "TMPDIR=" + dir,
			"GH_STANDIN_LOG=" + filepath.Join(dir, "calls"), "GH_STANDIN_VERSION=" + version}, settings...)...)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			return dir, string(out), exit.ExitCode()
		case err != nil:
			t.Fatal(err)
		}
		return dir, string(out), 0
	}
	// named is the file the script names after words, read; the test fails when it names none.
	named := func(t *testing.T, printed, words string) string {
		t.Helper()
		m := regexp.MustCompile(regexp.QuoteMeta(words) + `(\S+)`).FindStringSubmatch(printed)
		if m == nil {
			t.Fatalf("the script does not say %q followed by a file: %q", words, printed)
		}
		kept, err := os.ReadFile(m[1])
		if err != nil {
			t.Fatalf("the file the script names, %s: %v", m[1], err)
		}
		return string(kept)
	}
	// noPiece is the case's designated assertion over what the script printed.
	noPiece := func(t *testing.T, printed string) {
		t.Helper()
		oauth2calltest.RequireNoPiece(t, "legion.regenerate-gh-help/"+filepath.Base(t.Name()), printed, marker)
	}
	t.Run("stderr", func(t *testing.T) {
		dir, printed, exit := run(t, "GH_STANDIN_SAYS=gh: "+marker+" (HTTP 401)")
		if exit != 0 {
			t.Fatalf("the script exited %d: %q", exit, printed)
		}
		noPiece(t, printed)
		if said := named(t, printed, "gh wrote on its stderr; its words are in "); !strings.Contains(said, marker) {
			t.Errorf("twin: the file of gh's stderr the script names does not hold the marker the stand-in said: %q", said)
		}
		if !strings.HasSuffix(printed, " help files and gh's reference from "+version+"\n") {
			t.Errorf("control: the script does not print the version line of gh's own shape: %q", printed)
		}
		calls, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []string{"--version", "help reference", "pr view --help", "agent-task view --help"} {
			if !strings.Contains(string(calls), call+"\n") {
				t.Errorf("control: the stand-in gh was not called as gh %s: %q", call, calls)
			}
		}
	})
	t.Run("failed-call", func(t *testing.T) {
		_, printed, exit := run(t, "GH_STANDIN_SAYS=gh: "+marker+" (HTTP 401)", "GH_STANDIN_FAILS=pr view --help")
		if exit != 4 {
			t.Errorf("the script exited %d where gh pr view --help exited 4: %q", exit, printed)
		}
		noPiece(t, printed)
		if said := named(t, printed, "gh pr view --help exited 4; gh's own words are in "); !strings.Contains(said, marker) {
			t.Errorf("twin: the file of gh's stderr the script names does not hold the marker the stand-in said: %q", said)
		}
	})
	t.Run("version", func(t *testing.T) {
		dir, printed, exit := run(t, "GH_STANDIN_VERSION=gh version "+marker)
		if exit != 0 {
			t.Fatalf("the script exited %d: %q", exit, printed)
		}
		noPiece(t, printed)
		if saved := named(t, printed, "help files and gh's reference from a version line not of gh's shape, in "); !strings.Contains(saved, marker) {
			t.Errorf("twin: the VERSION the script names does not hold the marker the stand-in answered: %q", saved)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "regenerate-gh-help.*")); len(left) != 0 {
			t.Errorf("the script kept %v, a file of gh's stderr, though gh said nothing there", left)
		}
	})
}
