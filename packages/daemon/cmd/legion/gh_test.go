package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ghHelpDir holds the real gh's help of each command `legion gh` and internal/ghroute's tests
// read, saved from gh 2.98.0 (testdata/gh-help/VERSION), one file per path, its words joined by
// "_", and gh's own help (`gh --help`) as gh.txt.
const ghHelpDir = "testdata/gh-help"

// legionGh runs this test binary as `legion gh args…` in its own process, since a run execs the
// real gh in place of the process, with env (and nothing of this process's environment but PATH),
// in a working directory that is no git repository.
func legionGh(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return legionGhIn(t, t.TempDir(), env, "", args...)
}

// ghWrapperDir is a directory holding a pane's `gh` wrapper, a script running this test binary as
// `legion gh`, for the head of a test's PATH.
func ghWrapperDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec '"+os.Args[0]+"' gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legionGhCommand is the command legionGhIn runs and the one environment a `legion gh` of these
// tests is given: this test binary as `legion gh args…` in the working directory dir, with env
// and nothing of this process's own but PATH, whose head is a pane's `gh` wrapper
// (ghWrapperDir) unless env names a PATH of its own. A test that drives the run itself — one
// that signals it, or starts it under a shell of its own — takes the command and runs it, so
// that base environment is built in one place.
func legionGhCommand(t *testing.T, dir string, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"gh"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{testMainEnv + "=1", "PATH=" + ghWrapperDir(t) + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	return cmd
}

// legionGhIn runs `legion gh args…` as legionGh does, in the working directory dir with stdin,
// under the environment legionGhCommand builds, and answers what it exited with and printed.
func legionGhIn(t *testing.T, dir string, env map[string]string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := legionGhCommand(t, dir, env, args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatalf("legion gh %v: %v", args, err)
	}
	return 0, out.String(), errb.String()
}

// `legion gh` execs the real gh (LEGION_GH_PATH) as `gh`, with its arguments untouched and
// GH_CONFIG_DIR pointed at the directory of the repository the command names where the bundle
// holds one — `<owner>/<repo>`, whose token reaches that repository alone — at the owner's
// otherwise (a repository the bundle holds no directory for, two repositories of the one owner,
// a selector naming the owner alone), and at the issue's owner's (the GH_CONFIG_DIR it was
// given) when the command names no owner, reading --help=false as no help. It
// reads each command from the real gh's own help. The real gh runs with GH_PATH naming the `gh`
// wrapper first on PATH, so the git gh runs asks `legion gh auth git-credential`, which answers by
// the path's repository as `legion git-credential` does; with useHttpPath on for github.com, and gh's
// update check and prompts off. A command naming an owner the bundle holds no directory for is
// refused, exit 2, naming the owner, and so are one it cannot read, a gh alias, and any command
// when PATH's first gh is the real gh; the real gh never runs. In a clone of an owner's
// repository, a command naming none runs as that repository's. Each of the three variables it needs is
// required, and an unset one is named with exit 2; --help with no real gh to hand it to is
// legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub answers `[<command>] --help` with the saved help of that command, gh's own for none,
	// as the real gh would, and every other run with its argv and GH_CONFIG_DIR, then the rest of
	// the environment `legion gh` gives it.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"printf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\"\n" +
		"printf 'env %s|%s|%s=%s|%s|%s\\n' \"$GH_PATH\" \"$GIT_CONFIG_COUNT\" \"$GIT_CONFIG_KEY_0\" \"$GIT_CONFIG_VALUE_0\" \"$GH_NO_UPDATE_NOTIFIER\" \"$GH_PROMPT_DISABLED\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	// Every call passes the leak guard before gh runs, and a guard that can resolve no shared
	// check refuses it, so these runs carry a stand-in check that clears every call
	// (ghCheckPasses): what this test reads is which directory's token gh is run with, not what
	// the guard refuses, which TestLegionGhChecksTheCallWithTheLeakGuard reads.
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
	}
	wantEnv := "env " + filepath.Join(wrappers, "gh") + "|1|credential.https://github.com.useHttpPath=true|1|1\n"
	for _, tc := range []struct {
		args []string
		dir  string
	}{
		{[]string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, filepath.Join(root, "sjawhar")},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill"}, filepath.Join(root, "acme")},
		{[]string{"pr", "list"}, defaultDir},
		{[]string{"pr", "view", "1", "-R", "acme/widgets", "--help=false"}, filepath.Join(root, "acme")},
		{[]string{"api", "repos/sjawhar/legion-smoke", "--help=false", "-q", "."}, filepath.Join(root, "sjawhar")},
		// The repository the bundle holds a directory of its own for, named by -R and by `gh
		// api`'s endpoint; a repository it holds none for, two repositories of the one owner, and
		// a selector naming the owner alone are the owner's directory's.
		{[]string{"pr", "view", "1", "-R", "sjawhar/legion"}, filepath.Join(root, "sjawhar", "legion")},
		{[]string{"api", "repos/sjawhar/legion/pulls/1", "--jq", ".title"}, filepath.Join(root, "sjawhar", "legion")},
		{[]string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/legion-smoke"}, filepath.Join(root, "sjawhar")},
		{[]string{"repo", "list", "sjawhar"}, filepath.Join(root, "sjawhar")},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		want := stub + "|" + tc.dir + "|" + strings.Join(tc.args, " ") + "\n" + wantEnv
		if code != 0 || stdout != want {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.args, code, stdout, stderr, want)
		}
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "view", "https://github.com/other/thing/pull/1"}, `"other" is not an owner of this project`},
		{[]string{"api", "repos/Other/thing"}, `"other" is not an owner of this project`},
		// An absolute `gh api` endpoint that is not GitHub's own https://api.github.com/…: the
		// real gh would send the bundle's token over the scheme it names.
		{[]string{"api", "http://api.github.com/repos/sjawhar/legion"}, "is not https"},
		{[]string{"api", "https://ghe.example.com/api/v3/repos/sjawhar/legion"}, "another host than api.github.com"},
		{[]string{"pr", "view", "1", "--bogus"}, `"--bogus" is no option`},
		{[]string{"issue", "view", "1"}, "cannot read `gh issue view`"},
		{[]string{"co", "1"}, `"co" is a gh alias`},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 containing %q and no gh run", tc.args, code, stdout, stderr, tc.want)
		}
	}

	// gh's own git asks the wrapper, which answers with the token of the owner in git's path.
	for path, token := range map[string]string{"sjawhar/legion-smoke.git": "ghs_sjawhar_read", "acme/widgets.git": "ghs_acme_write"} {
		code, stdout, stderr := legionGhIn(t, t.TempDir(), env, "protocol=https\nhost=github.com\npath="+path+"\n\n", "auth", "git-credential", "get")
		if want := "username=x-access-token\npassword=" + token + "\n"; code != 0 || stdout != want {
			t.Errorf("legion gh auth git-credential get for %s = exit %d, stdout %q, stderr %q; want %q", path, code, stdout, stderr, want)
		}
	}
	if code, stdout, _ := legionGhIn(t, t.TempDir(), env, "protocol=https\nhost=github.com\n\n", "auth", "git-credential", "get"); code == 0 || stdout != "" {
		t.Errorf("legion gh auth git-credential get with no path = exit %d, stdout %q; want no answer", code, stdout)
	}
	// With the real gh first on PATH, gh's own git would answer with GH_CONFIG_DIR's token.
	unwrapped := map[string]string{"PATH": filepath.Dir(stub) + ":" + os.Getenv("PATH")}
	maps.Copy(unwrapped, map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root})
	if code, stdout, stderr := legionGh(t, unwrapped, "pr", "list"); code != 2 || stdout != "" || !strings.Contains(stderr, "is not the wrapper that runs `legion gh`") {
		t.Errorf("legion gh with the real gh first on PATH = exit %d, stdout %q, stderr %q; want exit 2 naming it", code, stdout, stderr)
	}
	// Nor may another wrapper of the machine's own lead PATH: gh would make it its git helper.
	shims := t.TempDir()
	if err := os.WriteFile(filepath.Join(shims, "gh"), []byte("#!/bin/sh\nGH_TOKEN=$(cat ~/.token) exec '"+stub+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shimmed := map[string]string{"PATH": shims + ":" + wrappers + ":" + os.Getenv("PATH")}
	maps.Copy(shimmed, map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root})
	if code, stdout, stderr := legionGh(t, shimmed, "pr", "list"); code != 2 || stdout != "" || !strings.Contains(stderr, "is not the wrapper that runs `legion gh`") {
		t.Errorf("legion gh with another gh wrapper first on PATH = exit %d, stdout %q, stderr %q; want exit 2 naming it", code, stdout, stderr)
	}

	// In a clone of sjawhar's repository, a command naming none acts on sjawhar's repository.
	clone := t.TempDir()
	fixture := fixturegit.Runner{TB: t, Env: []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}}
	for _, args := range [][]string{fixturegit.InitWorkTree("", "")[1:], {"remote", "add", "origin", "https://github.com/sjawhar/legion-smoke"}} {
		fixture.Run(clone, append([]string{"git"}, args...)...)
	}
	for _, args := range [][]string{{"pr", "list"}, {"pr", "list", "-R", "sjawhar/legion-smoke"}} {
		if code, stdout, stderr := legionGhIn(t, clone, env, "", args...); code != 0 || !strings.HasPrefix(stdout, stub+"|"+filepath.Join(root, "sjawhar")+"|") {
			t.Errorf("legion gh %q in that clone = exit %d, stdout %q, stderr %q; want sjawhar's directory", args, code, stdout, stderr)
		}
	}
	if code, stdout, stderr := legionGhIn(t, clone, env, "", "pr", "list", "-R", "acme/widgets"); code != 0 || !strings.HasPrefix(stdout, stub+"|"+filepath.Join(root, "acme")+"|") {
		t.Errorf("legion gh pr list -R acme/widgets in that clone = exit %d, stdout %q, stderr %q; want acme's directory", code, stdout, stderr)
	}

	for _, missing := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		partial := map[string]string{}
		for name, value := range env {
			if name != missing {
				partial[name] = value
			}
		}
		code, stdout, stderr := legionGh(t, partial, "pr", "list")
		if code != 2 || stdout != "" || !strings.Contains(stderr, missing+" is unset") {
			t.Errorf("legion gh without %s = exit %d, stdout %q, stderr %q; want exit 2 naming it", missing, code, stdout, stderr)
		}
	}

	code, _, stderr := legionGh(t, map[string]string{}, "--help")
	if code != 0 || !usageOf(stderr, "gh") {
		t.Errorf("legion gh --help with no LEGION_GH_PATH = exit %d, stderr %q; want exit 0 and its usage", code, stderr)
	}
}

// The `gh` each runtime puts first on PATH, the image's packages/daemon/docker/gh and the script a
// tmux pane's launcher directory holds (workerbin), is one `legion gh` accepts as its wrapper
// (ghWrapperScript), so the git gh runs names it as its helper; a machine's own wrapper is not.
func TestTheRuntimesGhWrappersAreOnesLegionGhAccepts(t *testing.T) {
	image, err := os.ReadFile(filepath.Join("..", "..", "docker", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := workerbin.Install(root, "/opt/legion state/legion"); err != nil {
		t.Fatal(err)
	}
	pane, err := os.ReadFile(filepath.Join(workerbin.LauncherDir(root), "gh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string][]byte{"the image's": image, "a tmux pane's": pane} {
		if !ghWrapperScript.Match(script) {
			t.Errorf("%s gh wrapper %q is not one legion gh accepts", name, script)
		}
	}
	for _, script := range []string{"#!/bin/sh\nexec \"/opt/legion state/legion\" gh \"$@\"\n", "#!/bin/sh\nexec '/opt/legion'\\''s/legion' gh \"$@\"\n", "#!/bin/sh\nexec /opt/legion/bin/legion gh \"$@\"\n"} {
		if !ghWrapperScript.MatchString(script) {
			t.Errorf("legion gh refuses %q as its wrapper", script)
		}
	}
	for _, script := range []string{
		"#!/bin/sh\nGH_TOKEN=x exec /usr/bin/gh \"$@\"\n", "#!/bin/sh\nexec /usr/bin/gh \"$@\"\n", "\x7fELF",
		"#!/bin/sh\nexec \"$HOME/legion\" gh \"$@\"\n", "#!/bin/sh\nexec $(cat /x) gh \"$@\"\n", "#!/bin/sh\nexec `cat /x` gh \"$@\"\n",
		// Two commands, a redirect, a tilde and a glob: shell, not one literal path.
		"#!/bin/sh\nexec /x|/bin/sh gh \"$@\"\n", "#!/bin/sh\nexec /x&/bin/sh gh \"$@\"\n", "#!/bin/sh\nexec /x;/bin/sh gh \"$@\"\n",
		"#!/bin/sh\nexec /x>/dev/null gh \"$@\"\n", "#!/bin/sh\nexec ~/legion gh \"$@\"\n", "#!/bin/sh\nexec /opt/*/legion gh \"$@\"\n",
		// An option to the shell's exec, and a path relative to wherever gh runs.
		"#!/bin/sh\nexec -a gh \"$@\"\n", "#!/bin/sh\nexec '-a' gh \"$@\"\n", "#!/bin/sh\nexec \"-a\" gh \"$@\"\n",
		"#!/bin/sh\nexec legion gh \"$@\"\n", "#!/bin/sh\nexec 'bin/legion' gh \"$@\"\n",
	} {
		if ghWrapperScript.MatchString(script) {
			t.Errorf("legion gh accepts %q as its wrapper", script)
		}
	}

	// The whole script is read, to the bound: a wrapper at the bound passes, and one a byte past it
	// is refused by the bound alone, its first ghWrapperMaxBytes+1 bytes a whole wrapper, whatever
	// follows; the pattern's anchors refuse anything that goes on after a wrapper within it.
	wrapperOf := func(size int) string {
		head, tail := "#!/bin/sh\nexec '/", "' gh \"$@\"\n"
		return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
	}
	atBound, pastBound := wrapperOf(ghWrapperMaxBytes), wrapperOf(ghWrapperMaxBytes+1)
	if !ghWrapperScript.MatchString(pastBound) {
		t.Fatalf("the pattern refuses a wrapper of %d bytes, so the bound is not what refuses it", len(pastBound))
	}
	for name, tc := range map[string]struct {
		script string
		want   bool
	}{
		"a wrapper at the bound":              {atBound, true},
		"a wrapper a byte past it":            {pastBound, false},
		"a wrapper a byte past it, then more": {pastBound + "rm -rf ~\n", false},
		"a wrapper at the bound, then more":   {atBound + "rm -rf ~\n", false},
		"a short wrapper, then more":          {"#!/bin/sh\nexec '/opt/legion/bin/legion' gh \"$@\"\nrm -rf ~\n", false},
		"a wrapper of a short path":           {"#!/bin/sh\nexec '/opt/legion/bin/legion' gh \"$@\"\n", true},
	} {
		if got, err := isGhWrapper(strings.NewReader(tc.script)); err != nil || got != tc.want {
			t.Errorf("isGhWrapper(%s, %d bytes) = %t, %v; want %t", name, len(tc.script), got, err, tc.want)
		}
	}
}

// The git gh runs reads credential.useHttpPath from GIT_CONFIG_COUNT's entries, added after any
// the environment already holds and in place of a stale entry at the next index; a count that is
// no number is refused.
func TestWithGitHubUseHTTPPathAddsToGitsConfiguration(t *testing.T) {
	got, err := withGitHubUseHTTPPath([]string{"A=1", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.x", "GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_KEY_1=stale", "GIT_CONFIG_VALUE_1=stale"})
	want := []string{"A=1", "GIT_CONFIG_KEY_0=core.x", "GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_KEY_1=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_1=true", "GIT_CONFIG_COUNT=2"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("withGitHubUseHTTPPath = %q, %v; want %q", got, err, want)
	}
	if got, err := withGitHubUseHTTPPath([]string{"A=1"}); err != nil || !slices.Equal(got, []string{"A=1", "GIT_CONFIG_KEY_0=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_0=true", "GIT_CONFIG_COUNT=1"}) {
		t.Errorf("withGitHubUseHTTPPath with no count = %q, %v", got, err)
	}
	if _, err := withGitHubUseHTTPPath([]string{"GIT_CONFIG_COUNT=x"}); err == nil || !strings.Contains(err.Error(), "no count") {
		t.Errorf("withGitHubUseHTTPPath with GIT_CONFIG_COUNT=x = %v, want a refusal", err)
	}
}

// ghStandInPrefix is what every stand-in real gh of this package begins with. It answers
// `[<command>] --help` with the saved help of that command, gh's own for none, which is the help
// the command resolver reads; and it answers the shared public leak check's own visibility read
// — the one call `legion gh` hands gh without checking it — with a visibility, recording it in
// GH_STANDIN_RECORD where the test names one. Without that second answer the guard's read of a
// destination's visibility would get a stand-in's ordinary output and refuse every call it
// cleared, since a visibility it cannot read is a setup failure.
//
// A help answer holds where GH_STANDIN_HOLD names its command path: it records that it is
// holding and waits for the file GH_STANDIN_RELEASE names, up to ten seconds, which is inside
// ghroute.ReadHelp's own bound of thirty, and a tick more once that file is there. That is the one
// place a test can hold `legion gh` inside work of its own — the resolver reads the leaf's
// help before the guard and before the child — and it is how
// TestLegionGhStartsNoGhForASignalThatCameBeforeTheForwarding reaches the window it is about;
// the tick after the release is that test's margin, so a signal sent before the release is
// taken while the run is still held. A test that names neither variable holds nowhere.
func ghStandInPrefix(help string) string {
	return "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  if [ \"${path:-gh}\" = \"$GH_STANDIN_HOLD\" ]; then\n" +
		"    printf 'gh held %s\\n' \"${path:-gh}\" >>\"${GH_STANDIN_RECORD:-/dev/null}\"\n" +
		"    ticks=0\n" +
		"    while [ ! -f \"$GH_STANDIN_RELEASE\" ] && [ \"$ticks\" -lt 200 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
		"    sleep 0.05\n" +
		"  fi\n" +
		"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n" +
		"if [ -n \"$" + leakguard.VisibilityReadVar + "\" ]; then\n" +
		"  printf 'visibility %s\\n' \"$*\" >>\"${GH_STANDIN_RECORD:-/dev/null}\"\n" +
		"  echo public\n" +
		"  exit 0\n" +
		"fi\n"
}

// ghRefusal is what the stand-in checks of this package print when they refuse a `legion gh`
// call: the check's own report, which is the whole of what a refusal says about it.
const ghRefusal = "LEAK CHECK: refusing the gh call: one of ours"

// The stand-in shared public leak checks `legion gh`'s guard is driven against, which are
// leakguardtest's. Each records the call it was given in LEAK_CHECK_RECORD, so a test can read
// whether the guard ran at all, and each reads its standard input, as the real check does, so
// nothing is left in a pipe when it exits. ghCheckRefuses answers as the check answers a write
// it refuses: its report, and exit 3.
var (
	ghCheckPasses  = leakguardtest.Recording(leakguardtest.Passes)
	ghCheckRefuses = leakguardtest.Recording(leakguardtest.Refusing(ghRefusal))
)

// ghCheckReading is leakguardtest.Reading under this package's own report: the check's two-go
// shape for a call whose body is on standard input, where the first run exits 4 to say the call
// reads standard input and the second, handed the buffer as a file, records what it read and
// ends with status — 0 for a call it clears, 3 for one it refuses.
func ghCheckReading(status int) string {
	report := ""
	if status != 0 {
		report = ghRefusal
	}
	return leakguardtest.Reading(status, report)
}

// readRecord is what a stand-in wrote into its record file, and "" where it never wrote one.
func readRecord(t *testing.T, path string) string {
	t.Helper()
	recorded, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(recorded)
}

// `legion gh` hands every call to the leak guard before it runs gh, and runs gh not at all for a
// call the guard refuses: a title, a body and a body on standard input that the shared check
// refuses are each exit 2 with the check's own report, and gh is never started. A call the check
// clears reaches gh with its arguments untouched, and gh's own exit status comes back through
// `legion gh` — the stand-in exits 7 and `legion gh` exits 7 — with the standard input the guard
// buffered handed to gh in this process's place.
//
// Two calls reach gh unchecked. `legion gh auth git-credential` answers git as it always did,
// with no check run at all, since reading its standard input would break git's protocol. And the
// shared check's own visibility read passes when it is exactly that read for exactly the
// repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names — without it a pod's gh, which is this
// command, would call the check, whose own read would call it again, without end — while the
// same variable with any other argv, and a variable naming no repository, are checked like
// anything else.
func TestLegionGhChecksTheCallWithTheLeakGuard(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The gh of these cases records every run it is handed — its arguments and its standard
	// input — and exits 7, a status nothing in `legion gh` itself answers with, so a test can
	// read that gh's own status is what came back.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"printf 'gh %s\\n' \"$*\" >>\"$GH_STANDIN_RECORD\"\n" +
		"printf 'stdin %s\\n' \"$(cat)\" >>\"$GH_STANDIN_RECORD\"\n" +
		"exit 7\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	// call runs `legion gh args…` with stdin against a stand-in check whose body is check, and
	// answers its status, what it printed, what gh recorded and what the check recorded.
	call := func(t *testing.T, check, stdin string, extra map[string]string, args ...string) (code int, stdout, stderr, ghRan, checked string) {
		t.Helper()
		records := t.TempDir()
		ghRecord, checkRecord := filepath.Join(records, "gh"), filepath.Join(records, "check")
		env := map[string]string{
			"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
			"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), check),
			"GH_STANDIN_RECORD": ghRecord, "LEAK_CHECK_RECORD": checkRecord,
		}
		maps.Copy(env, extra)
		code, stdout, stderr = legionGhIn(t, t.TempDir(), env, stdin, args...)
		return code, stdout, stderr, readRecord(t, ghRecord), readRecord(t, checkRecord)
	}
	ranGh := func(recorded string) bool { return strings.Contains(recorded, "gh ") }

	for _, tc := range []struct {
		name  string
		check string
		stdin string
		args  []string
	}{
		{"a title", ghCheckRefuses, "", []string{"pr", "create", "-R", "acme/widgets", "--title", "a title of this call"}},
		{"a body", ghCheckRefuses, "", []string{"pr", "create", "-R", "acme/widgets", "--body", "a body of this call"}},
		{"a body on standard input", ghCheckReading(3), "a body on standard input\n", []string{"pr", "create", "-R", "acme/widgets", "--body-file", "-"}},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			code, stdout, stderr, ghRan, checked := call(t, tc.check, tc.stdin, nil, tc.args...)
			if code != 2 || !strings.Contains(stderr, "LEAK CHECK: refusing") {
				t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 and the guard's refusal", tc.args, code, stdout, stderr)
			}
			if ranGh(ghRan) {
				t.Errorf("gh ran: %q", ghRan)
			}
			if checked == "" {
				t.Errorf("the shared check was never run for %q", tc.args)
			}
		})
	}

	t.Run("cleared: a call the check passes reaches gh, and gh's status comes back", func(t *testing.T) {
		args := []string{"pr", "create", "-R", "acme/widgets", "--title", "a title of this call", "--body", "a body of this call"}
		code, stdout, stderr, ghRan, checked := call(t, ghCheckPasses, "", nil, args...)
		if code != 7 {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want gh's own 7", args, code, stdout, stderr)
		}
		if want := "gh " + strings.Join(args, " ") + "\n"; !strings.Contains(ghRan, want) {
			t.Errorf("gh recorded %q, wants %q", ghRan, want)
		}
		if checked == "" {
			t.Errorf("the shared check was never run for %q", args)
		}
	})

	t.Run("cleared: the standard input the guard read is handed to gh", func(t *testing.T) {
		body := "a body on standard input\n"
		args := []string{"pr", "create", "-R", "acme/widgets", "--body-file", "-"}
		code, stdout, stderr, ghRan, checked := call(t, ghCheckReading(0), body, nil, args...)
		if code != 7 {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want gh's own 7", args, code, stdout, stderr)
		}
		if want := "stdin " + strings.TrimSuffix(body, "\n") + "\n"; !strings.Contains(ghRan, want) {
			t.Errorf("gh read %q on its standard input, want %q", ghRan, want)
		}
		if want := "checked stdin " + strings.TrimSuffix(body, "\n") + "\n"; !strings.Contains(checked, want) {
			t.Errorf("the check read %q, want %q", checked, want)
		}
	})

	t.Run("unchecked: auth git-credential", func(t *testing.T) {
		code, stdout, _, ghRan, checked := call(t, ghCheckRefuses, "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", nil, "auth", "git-credential", "get")
		if want := "username=x-access-token\npassword=ghs_acme_write\n"; code != 0 || stdout != want {
			t.Errorf("legion gh auth git-credential get = exit %d, stdout %q; want %q", code, stdout, want)
		}
		if checked != "" || ranGh(ghRan) {
			t.Errorf("the check recorded %q and gh %q, want neither run", checked, ghRan)
		}
	})

	t.Run("unchecked: the shared check's own visibility read", func(t *testing.T) {
		visibility := []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".visibility"}
		code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion"}, visibility...)
		if code != 0 || stdout != "public\n" {
			t.Errorf("the check's own visibility read = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", code, stdout, stderr)
		}
		if checked != "" {
			t.Errorf("the shared check ran for its own visibility read: %q", checked)
		}
		if want := "visibility " + strings.Join(visibility, " ") + "\n"; ghRan != want {
			t.Errorf("gh recorded %q, want %q", ghRan, want)
		}
	})

	for _, tc := range []struct {
		name string
		slug string
		args []string
	}{
		{"another argv under the same variable", "sjawhar/legion", []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".name"}},
		{"a variable naming no repository", "sjawhar/legion/pulls", []string{"api", "--hostname", "github.com", "repos/sjawhar/legion/pulls", "--jq", ".visibility"}},
	} {
		t.Run("checked: "+tc.name, func(t *testing.T) {
			code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: tc.slug}, tc.args...)
			if code != 2 || !strings.Contains(stderr, "LEAK CHECK: refusing") {
				t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 and the guard's refusal", tc.args, code, stdout, stderr)
			}
			if checked == "" || ranGh(ghRan) {
				t.Errorf("the check recorded %q and gh %q; want the call checked and gh not run", checked, ghRan)
			}
		})
	}
}

// Two wrapped calls to one repository reuse its public visibility answer, and their audit entries
// identify the call without preserving the text that the command would send.
func TestLegionGhKeepsVisibilityAndLogsCalls(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	record := filepath.Join(t.TempDir(), "gh")
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GH_CONFIG_DIR":        defaultDir,
		"GH_STANDIN_RECORD":    record,
		"LEGION_GH_OWNERS_DIR": root,
		"LEGION_GH_PATH":       stub,
		"LEGION_ISSUE":         "issue-9",
		"LEGION_ROLE":          "tester",
		"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
		"XDG_STATE_HOME":       state,
	}
	args := []string{"pr", "create", "-R", "acme/widgets", "--title", "argument text must not be logged"}
	for range 2 {
		if code, _, stderr := legionGhIn(t, t.TempDir(), env, "", args...); code != 0 {
			t.Fatalf("legion gh %q = exit %d, stderr %q; want success", args, code, stderr)
		}
	}
	if got := strings.Count(readRecord(t, record), "visibility "); got != 1 {
		t.Errorf("the visibility read reached gh %d times, want once", got)
	}
	log, err := os.ReadFile(filepath.Join(state, "legion-gh", "calls.log"))
	if err != nil {
		t.Fatalf("read the call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 {
		t.Errorf("the call log has %d lines, want two: %q", len(lines), log)
	}
	for i, line := range lines {
		wants := []string{"tester", "issue-9", "acme/widgets", "pr create", "status=0"}
		if i == 0 {
			wants = append(wants, `visibility="github"`)
		} else {
			wants = append(wants, `visibility="kept"`)
		}
		for _, want := range wants {
			if !strings.Contains(line, want) {
				t.Errorf("the call log line %q does not name %q", line, want)
			}
		}
	}
	if strings.Contains(string(log), "argument text must not be logged") {
		t.Errorf("the call log holds command argument text: %q", log)
	}
}

// Concurrent processes share one log and rotate it as one operation, so no appended line that
// fits after the oversized prior content is lost.
func TestAppendGhCallLogKeepsConcurrentLinesThatFit(t *testing.T) {
	if repository := os.Getenv("LEGION_TEST_GH_CALL_LOG_REPOSITORY"); repository != "" {
		appendGhCallLog(map[string]string{"XDG_STATE_HOME": os.Getenv("LEGION_TEST_GH_CALL_LOG_STATE")}, repository, "api", "", 0)
		return
	}

	const calls = 128
	state := t.TempDir()
	dir := filepath.Join(state, "legion-gh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(path, append(bytes.Repeat([]byte{'x'}, ghCallLogBytes), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, calls)
	for i := range calls {
		repository := fmt.Sprintf("repo-%03d", i)
		go func() {
			command := exec.Command(os.Args[0], "-test.run=^TestAppendGhCallLogKeepsConcurrentLinesThatFit$")
			command.Env = append(os.Environ(),
				"LEGION_TEST_GH_CALL_LOG_REPOSITORY="+repository,
				"LEGION_TEST_GH_CALL_LOG_STATE="+state,
			)
			errs <- command.Run()
		}()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		want := fmt.Sprintf(`repository="repo-%03d"`, i)
		if got := strings.Count(string(content), want); got != 1 {
			t.Errorf("the call log records %q %d times, want once", want, got)
		}
	}
}

// The rate-limit endpoint is a read with no repository content, so it bypasses the guard; a
// method that writes or another endpoint still reaches the guard.
func TestLegionGhLetsOnlyTheRateLimitReadBypassTheGuard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	fixture := fixturegit.Isolated(t, t.TempDir())
	for _, args := range [][]string{
		fixturegit.InitWorkTree("", "")[1:],
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
	} {
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	script := strings.Replace(ghStandInPrefix(help), "  echo public\n  exit 0\n", "  echo unavailable >&2\n  exit 1\n", 1) + "exit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	root, defaultDir := gitCredentialBundle(t)
	for _, tc := range []struct {
		name      string
		args      []string
		check     string
		wantCode  int
		wantReads int
	}{
		{"the GET", []string{"api", "rate_limit"}, ghCheckPasses, 0, 0},
		{"a POST", []string{"api", "rate_limit", "--method", "POST"}, ghCheckRefuses, 2, 0},
		{"another endpoint", []string{"api", "user"}, ghCheckRefuses, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "gh")
			env := map[string]string{
				"GH_CONFIG_DIR":        defaultDir,
				"GH_STANDIN_RECORD":    record,
				"LEGION_GH_OWNERS_DIR": root,
				"LEGION_GH_PATH":       stub,
				"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), tc.check),
			}
			if code, _, stderr := legionGhIn(t, repository, env, "", tc.args...); code != tc.wantCode {
				t.Fatalf("legion gh %q = exit %d, stderr %q; want exit %d", tc.args, code, stderr, tc.wantCode)
			}
			if reads := strings.Count(readRecord(t, record), "visibility "); reads != tc.wantReads {
				t.Errorf("the visibility read reached gh %d times, want %d", reads, tc.wantReads)
			}
		})
	}
}

// A SIGTERM or a SIGHUP sent to `legion gh`'s own pid reaches the gh it is waiting for, which is
// what an exec'd gh — the shape this replaced — ended on. Neither signal reaches gh by itself:
// the child is given no process group of its own on purpose, so a terminal's SIGINT reaches it
// with the rest of the group, but a signal sent to one pid reaches that pid alone. Without the
// forwarding a SIGTERM would end at main's own signal.NotifyContext, whose cancellation the
// child's context has taken off, and gh would run on with nobody waiting for it, while a SIGHUP
// would kill `legion gh` where it stood and leave gh running.
//
// The stand-in gh records the signal it caught and exits with a status of its own, so what this
// reads is both halves: gh ended on the signal, and its status came back through `legion gh`.
func TestLegionGhForwardsATerminatingSignalToGh(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{
		{"SIGTERM", syscall.SIGTERM, 42},
		{"SIGHUP", syscall.SIGHUP, 43},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := t.TempDir()
			ghRecord, running := filepath.Join(records, "gh"), filepath.Join(records, "running")
			// The stand-in traps both signals, says it is running once the traps are set, and
			// then waits in ticks short enough that a trap runs within one of them: a shell runs
			// no trap while a foreground command of its own is still going. The wait gives up
			// after thirty seconds and exits 9, so a forwarding that never arrives is a failure
			// naming it rather than a test binary that hangs until its deadline.
			stub := filepath.Join(t.TempDir(), "gh")
			script := ghStandInPrefix(help) +
				"trap 'printf \"gh caught TERM\\n\" >>\"$GH_STANDIN_RECORD\"; exit 42' TERM\n" +
				"trap 'printf \"gh caught HUP\\n\" >>\"$GH_STANDIN_RECORD\"; exit 43' HUP\n" +
				": >\"$GH_STANDIN_RUNNING\"\n" +
				"ticks=0\nwhile [ \"$ticks\" -lt 600 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
				"printf 'gh caught nothing\\n' >>\"$GH_STANDIN_RECORD\"\nexit 9\n"
			if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			command := legionGhCommand(t, t.TempDir(), map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
				"GH_STANDIN_RECORD": ghRecord, "GH_STANDIN_RUNNING": running,
			}, "pr", "list")
			// Standard output and standard error are a file of the test's own rather than a
			// pipe: gh's own `sleep` outlives the shell that trapped the signal by a tick, and
			// a pipe would hold Wait until that grandchild let go of it.
			printed, err := os.Create(filepath.Join(records, "printed"))
			if err != nil {
				t.Fatal(err)
			}
			defer printed.Close()
			command.Stdout, command.Stderr = printed, printed
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			// The signal is sent once gh itself is running: before that it would reach `legion
			// gh` while the guard still has the call, which is no forwarding this test can read.
			testwait.Eventually(t, "the stand-in gh to be running", func() bool {
				_, err := os.Stat(running)
				return err == nil
			})
			// The signal goes to this one pid, never to the group: gh shares this test binary's
			// own group, and a group signal would end the test run with it.
			if err := command.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			waited := command.Wait()
			exit, ok := waited.(*exec.ExitError)
			if !ok {
				t.Fatalf("legion gh under %s = %v, want gh's own status: %s", tc.name, waited, readRecord(t, printed.Name()))
			}
			if exit.ExitCode() != tc.code {
				t.Errorf("legion gh under %s exited %d, want gh's own %d: %s", tc.name, exit.ExitCode(), tc.code, readRecord(t, printed.Name()))
			}
			if want := "gh caught " + strings.TrimPrefix(tc.name, "SIG") + "\n"; !strings.Contains(readRecord(t, ghRecord), want) {
				t.Errorf("gh recorded %q, want %q: the signal never reached it", readRecord(t, ghRecord), want)
			}
		})
	}
}

// A `legion gh` started with SIGHUP ignored leaves it ignored for gh. That is how `nohup gh …`
// runs: nohup ignores the signal and execs the wrapper, which execs this command, and SIG_IGN
// is kept across both. Arming the forwarding for it would install a handler — the signal "will
// no longer be ignored", as os/signal's doc puts it — and execve resets a caught signal to its
// default action, so gh would start with the hangup set to kill and die of the first one, which
// is the whole of what nohup was asked to prevent.
//
// The run is under a shell that ignores the signal, as nohup does, and the stand-in gh sets a
// trap of its own: a shell keeps ignoring a signal that was ignored when it started, trap
// command or not, so a gh that records catching the hangup is a gh whose hangup the arming
// un-ignored. Nothing here is forwarded, and what the test reads is that `legion gh` sat
// through the hangup and came back with the status of a gh that ended on its own.
func TestLegionGhLeavesAnIgnoredHangupIgnoredForGh(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	ghRecord, running := filepath.Join(records, "gh"), filepath.Join(records, "running")
	release := filepath.Join(records, "release")
	// The stand-in traps the hangup, says it is running once the trap is set, and then waits
	// for the test's release in ticks short enough that a trap runs within one of them: a shell
	// runs no trap while a foreground command of its own is still going. The wait gives up
	// after ten seconds, so a release that never comes is a failure naming it rather than a
	// test binary that hangs until its deadline.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"trap 'printf \"gh caught the hangup\\n\" >>\"$GH_STANDIN_RECORD\"; exit 44' HUP\n" +
		": >\"$GH_STANDIN_RUNNING\"\n" +
		"ticks=0\nwhile [ ! -f \"$GH_STANDIN_RELEASE\" ] && [ \"$ticks\" -lt 200 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
		"printf 'gh survived the hangup\\n' >>\"$GH_STANDIN_RECORD\"\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := legionGhCommand(t, t.TempDir(), map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		"GH_STANDIN_RECORD": ghRecord, "GH_STANDIN_RUNNING": running, "GH_STANDIN_RELEASE": release,
	}, "pr", "list")
	// `sh -c 'trap "" HUP; exec …'` is what nohup does to the command it runs, and the exec is
	// the wrapper's: `legion gh` starts here with the hangup ignored and no handler of its own,
	// exactly as it does under `nohup gh …` on a pane's PATH.
	command.Args = append([]string{"/bin/sh", "-c", `trap "" HUP; exec "$@"`, "sh"}, command.Args...)
	command.Path = "/bin/sh"
	// Standard output and standard error are a file of the test's own rather than a pipe, for
	// the reason the forwarding test above gives: gh's own `sleep` outlives it by a tick.
	printed, err := os.Create(filepath.Join(records, "printed"))
	if err != nil {
		t.Fatal(err)
	}
	defer printed.Close()
	command.Stdout, command.Stderr = printed, printed
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "the stand-in gh to be running", func() bool {
		_, err := os.Stat(running)
		return err == nil
	})
	// The hangup goes to this one pid, never to the group: gh shares this test binary's own
	// group, and a group signal would end the test run with it.
	if err := command.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// The run is given a second to end on that hangup before gh is released. Where the hangup
	// is forwarded, gh catches it and ends the run inside that second, and the failure says so;
	// where it stays ignored, nothing happens in it, which is what the rest of this reads.
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case waited := <-done:
		t.Fatalf("legion gh ended on a hangup it was started ignoring: %v; gh recorded %q, printed %q",
			waited, readRecord(t, ghRecord), readRecord(t, printed.Name()))
	case <-time.After(time.Second):
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if waited := <-done; waited != nil {
		t.Fatalf("legion gh = %v, want gh's own 0: %s", waited, readRecord(t, printed.Name()))
	}
	if want := "gh survived the hangup\n"; readRecord(t, ghRecord) != want {
		t.Errorf("gh recorded %q, want %q", readRecord(t, ghRecord), want)
	}
}

// A signal that reaches `legion gh` before it has armed its own forwarding ends the command
// there, with the status a shell reads a death by that signal as, and gh is never started.
// Until that arming the signal's only reader is main's own signal.NotifyContext, which cancels
// the command's context, names the signal as the cause and raises it no further, and the
// context the child is given has that cancellation taken off: without the read of that cause gh
// would start and run to the end as if nothing had been sent, and `legion gh` would answer with
// gh's own status.
//
// The window is everything this command does before the child, and here it is held open in the
// resolver's read of the leaf's help (GH_STANDIN_HOLD), which runs before the guard and under a
// context of its own, so the signal lands inside it rather than in a race. The call is the
// shared check's own visibility read, the one call the guard never sees, so the whole of the
// held work is this command's own: the check of these runs refuses everything, and a run that
// reached it would exit 2 rather than on the signal.
func TestLegionGhStartsNoGhForASignalThatCameBeforeTheForwarding(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	visibility := leakguard.VisibilityReadArgs("sjawhar/legion")
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		code   int
	}{
		{"SIGINT", syscall.SIGINT, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := t.TempDir()
			ghRecord, release := filepath.Join(records, "gh"), filepath.Join(records, "release")
			// The stand-in is the prefix alone: it answers the help this run holds in, and it
			// records the visibility read it would answer if it were ever started as gh.
			stub := filepath.Join(t.TempDir(), "gh")
			if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)), 0o700); err != nil {
				t.Fatal(err)
			}
			command := legionGhCommand(t, t.TempDir(), map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PUBLIC_LEAK_CHECK":         leakguardtest.Write(t, t.TempDir(), ghCheckRefuses),
				leakguard.VisibilityReadVar: "sjawhar/legion",
				"GH_STANDIN_RECORD":         ghRecord,
				"GH_STANDIN_HOLD":           "api", "GH_STANDIN_RELEASE": release,
			}, visibility...)
			printed, err := os.Create(filepath.Join(records, "printed"))
			if err != nil {
				t.Fatal(err)
			}
			defer printed.Close()
			command.Stdout, command.Stderr = printed, printed
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			testwait.Eventually(t, "the resolver's read of gh api's help to be holding", func() bool {
				return strings.Contains(readRecord(t, ghRecord), "gh held api\n")
			})
			// The signal goes to this one pid, as the forwarding test's does, and the hold is
			// released after it: the hold waits a tick of its own once the file is there, so
			// the signal is taken while the run is still inside the window.
			if err := command.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			waited := command.Wait()
			exit, ok := waited.(*exec.ExitError)
			if !ok {
				t.Fatalf("legion gh under %s = %v, want 128+%d: %s", tc.name, waited, tc.signal, readRecord(t, printed.Name()))
			}
			if exit.ExitCode() != tc.code {
				t.Errorf("legion gh under %s exited %d, want %d: %s", tc.name, exit.ExitCode(), tc.code, readRecord(t, printed.Name()))
			}
			if want := "gh held api\n"; readRecord(t, ghRecord) != want {
				t.Errorf("gh recorded %q, want %q: gh was started for a signal that came before it", readRecord(t, ghRecord), want)
			}
		})
	}
}
