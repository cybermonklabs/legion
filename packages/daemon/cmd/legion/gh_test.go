package main

//ci:reads packages/daemon/docker/gh TestTheRuntimesGhWrappersAreOnesLegionGhAccepts ../../docker/gh

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/testfifo"
	"github.com/sjawhar/legion/daemon/internal/testkernel"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ghHelpDir holds the real gh's help of each command `legion gh` and internal/ghroute's tests
// read, saved from gh 2.98.0 (testdata/gh-help/VERSION), one file per path, its words joined by
// "_", and gh's own help (`gh --help`) as gh.txt.
const ghHelpDir = "testdata/gh-help"

// ghHelpWalkDir holds the real gh's help of every command of each release saved there, one
// directory per release, read by `legion gh`'s tests and internal/ghroute's alike (ghroute's
// TestCaptureGhHelpWalk saves a release's).
const ghHelpWalkDir = "testdata/gh-help-walk"

// savedGhHelpOf answers gh's help of a command path from the saved walk of the release version
// as the real gh does (ghroutetest.SavedHelp), as internal/ghroute's own savedGhHelpOf does. The
// directory is made absolute first, so a test that changes its working directory still reads it.
func savedGhHelpOf(t *testing.T, version string) func([]string) (string, error) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(ghHelpWalkDir, version))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "VERSION")); err != nil {
		t.Fatalf("no saved help of gh %s: %v", version, err)
	}
	return ghroutetest.SavedHelp(dir)
}

// savedGhHelp answers gh's help of a command path from ghHelpDir as the real gh does
// (ghroutetest.SavedHelp), for a test that reads a call as runGh does (ghroute.TargetOf).
var savedGhHelp = ghroutetest.SavedHelp(ghHelpDir)

// noGitRepository is the git remotes of a working directory that is no git repository: none, as
// legionGh's working directory has (ghroutetest.NoGitRepository).
var noGitRepository = ghroutetest.NoGitRepository[ghroute.Remote]

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

// legionCommand is the one environment a `legion` command of these tests is given: this test
// binary as `legion args…` in the working directory dir, with env and nothing of this process's
// own but PATH, whose head is a pane's `gh` wrapper (ghWrapperDir) unless env names a PATH of its
// own, and this binary's deadline (testDeadlineEnviron), which bounds what the run waits for. A test
// that drives the run itself — one that signals it, or starts it under a shell of its own — takes
// the command and runs it, so that base environment is built in one place.
func legionCommand(t *testing.T, dir string, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = append([]string{testMainEnv + "=1", "PATH=" + ghWrapperDir(t) + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, testDeadlineEnviron()...)
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	return cmd
}

// testGhChildEnviron is what this package's test binary, run as `legion`, hands gh beside the
// allowlist (ghChildTestEnviron): testMainEnv, so the `gh` wrapper gh's git runs as its credential
// helper runs this binary as `legion` again, with this binary's deadline (testDeadlineEnviron);
// every variable of environ the stand-in real gh of these tests reads (GH_STANDIN_*); and the
// pairs testGhChildEnv names. None of these is one ghChildEnviron keeps, which is the point of
// naming them here and nowhere in the binary. The stand-ins tell the guard's visibility read by
// its argv, as the real gh would have to (ghStandInPrefix), so no Legion variable naming that read
// is handed on.
func testGhChildEnviron(environ []string) []string {
	out := append([]string{testMainEnv + "=1"}, testDeadlineEnviron()...)
	for _, pair := range environ {
		if name, _, _ := strings.Cut(pair, "="); strings.HasPrefix(name, "GH_STANDIN_") {
			out = append(out, pair)
		}
	}
	for _, line := range strings.Split(os.Getenv(testGhChildEnv), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// legionGhCommand is the command legionGhIn runs: legionCommand's `legion gh args…`.
func legionGhCommand(t *testing.T, dir string, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()
	return legionCommand(t, dir, env, append([]string{"gh"}, args...)...)
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
// GH_CONFIG_DIR pointed at the directory of the owner the command names — one naming one of
// that owner's repositories, two of them, and one where a selector named the owner alone run
// with that one directory alike — and at the issue's owner's (the GH_CONFIG_DIR it was
// given) when the command names no owner, reading --help=false as no help. It
// reads each command from the real gh's own help. The real gh runs with GH_PATH naming the `gh`
// wrapper first on PATH, so the git gh runs asks `legion gh auth git-credential`, which answers by
// the path's owner as `legion git-credential` does; with useHttpPath on for github.com, and gh's
// update check and prompts off. A read naming only an owner the bundle holds no directory for
// runs with GH_CONFIG_DIR's directory (R4 of dispatch://INFRA-3399); a write naming one is
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
		"printf 'env %s|%s|%s=%s|%s=%s|%s=%s|%s=%s|%s=%s|%s|%s|%s|%s|%s|%s\\n' \"$GH_PATH\" \"$GIT_CONFIG_COUNT\" " +
		"\"$GIT_CONFIG_KEY_0\" \"$GIT_CONFIG_VALUE_0\" \"$GIT_CONFIG_KEY_1\" \"$GIT_CONFIG_VALUE_1\" \"$GIT_CONFIG_KEY_2\" \"$GIT_CONFIG_VALUE_2\" " +
		"\"$GIT_CONFIG_KEY_3\" \"$GIT_CONFIG_VALUE_3\" \"$GIT_CONFIG_KEY_4\" \"$GIT_CONFIG_VALUE_4\" " +
		"\"$GH_NO_UPDATE_NOTIFIER\" \"$GH_PROMPT_DISABLED\" \"$GH_EDITOR\" \"$GH_TELEMETRY\" \"$GIT_CONFIG_GLOBAL\" \"$GIT_CONFIG_NOSYSTEM\"\n"
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
	// gh's git gets http.sslVerify on, submodule recursion off and useHttpPath for github.com,
	// reads no global or system file, and gh gets an editor that fails and its telemetry off
	// (ghChildForced, ghForcedGitConfig).
	wantEnv := "env " + filepath.Join(wrappers, "gh") + "|5|http.sslVerify=true|submodule.recurse=false|fetch.recurseSubmodules=false|push.recurseSubmodules=no|credential.https://github.com.useHttpPath=true|1|1|" + ghNoEditor + "|0|/dev/null|1\n"
	for _, tc := range []struct {
		args []string
		dir  string
	}{
		{[]string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, filepath.Join(root, "sjawhar")},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill"}, filepath.Join(root, "acme")},
		{[]string{"pr", "list"}, defaultDir},
		{[]string{"pr", "view", "1", "-R", "acme/widgets", "--help=false"}, filepath.Join(root, "acme")},
		{[]string{"api", "repos/sjawhar/legion-smoke", "--help=false", "-q", "."}, filepath.Join(root, "sjawhar")},
		// A command naming one repository, by -R and by `gh api`'s endpoint, is its owner's
		// directory's too: the owner's lease reaches every repository its installation holds,
		// so the bundle holds no narrower token to run such a command with.
		{[]string{"pr", "view", "1", "-R", "sjawhar/legion"}, filepath.Join(root, "sjawhar")},
		{[]string{"api", "repos/sjawhar/legion/pulls/1", "--jq", ".title"}, filepath.Join(root, "sjawhar")},
		{[]string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/legion-smoke"}, filepath.Join(root, "sjawhar")},
		{[]string{"repo", "list", "sjawhar"}, filepath.Join(root, "sjawhar")},
		// A repository of an owner no record of the project names: the bundle holds that owner's
		// directory because the role's App is installed on the account, and the token in it
		// reaches every repository of that installation.
		{[]string{"pr", "create", "-R", "example-org/deployment", "--fill"}, filepath.Join(root, "example-org")},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		want := stub + "|" + tc.dir + "|" + strings.Join(tc.args, " ") + "\n" + wantEnv
		if code != 0 || stdout != want {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.args, code, stdout, stderr, want)
		}
	}

	// A read naming only an owner the bundle holds no directory for runs with GH_CONFIG_DIR's
	// directory, as a command naming no owner does (R4 of dispatch://INFRA-3399). It runs under
	// the stand-in the tests count as the shared check the read rule was proven against
	// (testProvenCheckEnv), which the guard runs alone for a call that writes nothing; the whole
	// guard would first read the repository's visibility, which `legion gh` refuses for an owner
	// it holds no directory for, and then ask GitHub itself, with no credential (git ls-remote).
	proven := maps.Clone(env)
	proven["PUBLIC_LEAK_CHECK"], proven[testProvenCheckEnv] = ghCheckAt(t, ghCheckPasses), ghProvenProgramSHA256
	for _, args := range [][]string{{"pr", "view", "https://github.com/other/thing/pull/1"}, {"api", "repos/Other/thing"}} {
		code, stdout, stderr := legionGh(t, proven, args...)
		if want := stub + "|" + defaultDir + "|" + strings.Join(args, " ") + "\n" + wantEnv; code != 0 || stdout != want {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", args, code, stdout, stderr, want)
		}
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "create", "-R", "other/thing", "--fill"}, `the role's GitHub App is not installed on "other"`},
		{[]string{"api", "-X", "POST", "repos/Other/thing/issues", "-f", "title=t"}, `the role's GitHub App is not installed on "other"`},
		// An absolute `gh api` endpoint that is not GitHub's own https://api.github.com/…: the
		// real gh would send the bundle's token over the scheme it names.
		{[]string{"api", "http://api.github.com/repos/sjawhar/legion"}, "is not https"},
		{[]string{"api", "https://ghe.example.com/api/v3/repos/sjawhar/legion"}, "another host than api.github.com"},
		{[]string{"pr", "view", "1", "--bogus"}, `"--bogus" is no option`},
		{[]string{"issue", "lock", "1"}, "cannot read `gh issue lock`"},
		{[]string{"co", "1"}, `"co" is a gh alias`},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 containing %q and no gh run", tc.args, code, stdout, stderr, tc.want)
		}
	}

	// gh's own git asks the wrapper, which answers with the token of the owner in git's path.
	for path, token := range map[string]string{"sjawhar/legion-smoke.git": "ghs_sjawhar_write", "acme/widgets.git": "ghs_acme_write"} {
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

// The git gh runs makes pushes of its own (`gh repo create --source=. --push`), and git runs the
// pre-push hook for each, `legion leak-check pre-push`, under the environment gh was handed. So
// gh keeps every variable of its caller's that the hook's guard reads, each with its value: the
// shared check (PUBLIC_LEAK_CHECK), the deployment's rule file (LEGION_PRIVATE_NAME_RULES_FILE),
// without which the hook would run the shared check alone and none of the role's private-name
// rules, and the git wrapper's loop guard (WRAPPED_GIT_AT). The stand-in gh prints each as it
// reads it, and "unset" for one it was not handed.
func TestLegionGhHandsGhThePrePushHooksVariables(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) + "printf '%s|%s|%s\\n' " +
		"\"${" + leakguard.CheckVar + "-unset}\" \"${" + leakguard.RulesFileVar + "-unset}\" \"${" + wrappedGitAt + "-unset}\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	check := leakguardtest.Write(t, t.TempDir(), ghCheckPasses)
	rules := leakguardtest.WriteRules(t, t.TempDir())
	const chain = "0:/usr/bin/git"
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root, "XDG_STATE_HOME": t.TempDir(),
		leakguard.CheckVar: check, leakguard.RulesFileVar: rules, wrappedGitAt: chain,
	}
	args := []string{"pr", "create", "-R", "acme/widgets", "--title", "a clean title", "--body", "a clean body"}
	code, stdout, stderr := legionGhIn(t, t.TempDir(), env, "", args...)
	if want := check + "|" + rules + "|" + chain + "\n"; code != 0 || stdout != want {
		t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and gh handed %q", args, code, stdout, stderr, want)
	}
}

// `legion gh` never runs itself as the real gh. A LEGION_GH_PATH that is not an absolute path,
// which exec would look up on PATH, whose first gh is the wrapper, is refused, exit 2, before
// anything runs, and so is one that is the wrapper first on PATH, or another script of the
// wrapper's form. One that leads back to `legion gh` only when it runs — a machine's own gh shim
// that hands every call to the next gh on PATH, which is the wrapper, as the operator's knives
// shim did — reaches `legion gh` once more, under the variable every real gh this command runs is
// given (leakguard.RunByRealGhVar), and that run refuses, exit 2, so the help read fails and
// nothing runs again. gh's own git credential helper still answers under that variable, and the
// real gh a call runs is given it. The `legion` the wrappers here run counts its starts and stops
// a chain at five, so code that loops fails this test rather than running away; before the
// refusals, each case below started `legion gh` five times.
//
// The bound holds under a caller's GH_TOKEN, with which the guard's own visibility reads skip the
// help reads (ghVisibilityRead): the read's real gh, the shim, leads back once and that run
// refuses. It holds through the leak guard too, which blanks the variable for its children. The
// guard's own read for a push a real gh makes, whose `legion leak-check pre-push` runs under the
// variable, is two `legion gh` starts deep: the one the read runs through the wrapper, and the one
// the shim that read runs, for its help read or for the read itself under the token, leads back
// to, which refuses. The shared check's own read, which the check makes itself, is three deep: the
// `legion gh` that answers it (answerVisibilityRead), the one that answer's REST read runs through
// the wrapper, and the one that read's shim leads back to, which refuses. A refusal that let the
// guard's reads through under the variable loops under the token until the cap stops it.
func TestLegionGhRefusesARealGhThatRunsLegionGhAgain(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	starts := filepath.Join(t.TempDir(), "starts")
	legion := filepath.Join(t.TempDir(), "legion")
	counting := "#!/bin/sh\necho start >>'" + starts + "'\n[ \"$(wc -l <'" + starts + "')\" -le 4 ] || exit 97\n" +
		testMainEnv + "=1 exec '" + os.Args[0] + "' \"$@\"\n"
	wrappers, elsewhere := t.TempDir(), t.TempDir()
	shim := filepath.Join(t.TempDir(), "gh")
	for path, script := range map[string]string{
		legion:                         counting,
		filepath.Join(wrappers, "gh"):  "#!/bin/sh\nexec '" + legion + "' gh \"$@\"\n",
		filepath.Join(elsewhere, "gh"): "#!/bin/sh\nexec '" + legion + "' gh \"$@\"\n",
		shim:                           "#!/bin/sh\nexec '" + filepath.Join(wrappers, "gh") + "' \"$@\"\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	startsOf := func(t *testing.T) int {
		t.Helper()
		raw, err := os.ReadFile(starts)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "start\n")
	}
	// The push cases need the hook's guard to read the destination's visibility, which it does
	// only where the deployment names a rule file, so every case names the fixture's.
	rules := leakguardtest.WriteRules(t, t.TempDir())
	envFor := func(gh string) map[string]string {
		return map[string]string{
			"LEGION_GH_PATH": gh, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
			"PATH": wrappers + ":" + os.Getenv("PATH"), "PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
			leakguard.RulesFileVar: rules,
		}
	}
	for _, tc := range []struct {
		name, gh, want string
		starts         int
	}{
		{"a bare name", "gh", `LEGION_GH_PATH is not an absolute path: "gh"`, 0},
		{"the wrapper first on PATH", filepath.Join(wrappers, "gh"), "is a wrapper that runs `legion gh`", 0},
		{"a wrapper elsewhere", filepath.Join(elsewhere, "gh"), "is a wrapper that runs `legion gh`", 0},
		{"a shim that runs the wrapper", shim, "the real gh " + shim + " ran `legion gh` again", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(starts); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			code, stdout, stderr := legionGh(t, envFor(tc.gh), "pr", "list")
			if got := startsOf(t); code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) || got != tc.starts {
				t.Errorf("legion gh pr list with LEGION_GH_PATH %s = exit %d, stdout %q, stderr %q, and `legion gh` started %d more times; want exit 2 containing %q and %d more starts",
					tc.gh, code, stdout, stderr, got, tc.want, tc.starts)
			}
		})
	}

	pushDir, pushLine := aPushFixture(t, "a commit of nothing in particular")
	const slug = "acme/widgets"
	for _, tc := range []struct {
		name  string
		token bool
		// args is the `legion gh` call the case makes, and nil for the pre-push hook of a push
		// to slug that the shim, as the real gh, makes.
		args   []string
		starts int
	}{
		{"the guard's REST read under GH_TOKEN", true, leakguard.VisibilityReadWithHeadersArgs(slug), 1},
		{"the guard's GraphQL read under GH_TOKEN", true, leakguard.VisibilityGraphQLArgs(slug), 1},
		{"the pre-push hook of a push the real gh makes", false, nil, 2},
		{"the pre-push hook of a push the real gh makes under GH_TOKEN", true, nil, 2},
	} {
		t.Run("a shim that runs the wrapper: "+tc.name, func(t *testing.T) {
			if err := os.Remove(starts); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			env := envFor(shim)
			if tc.token {
				env["GH_TOKEN"] = "ghs_caller"
			}
			var code int
			var stdout, stderr string
			if tc.args != nil {
				env[leakguard.VisibilityReadVar] = slug
				code, stdout, stderr = legionGh(t, env, tc.args...)
			} else {
				env[leakguard.RunByRealGhVar] = shim
				code, stderr = legionLeakCheckPrePush(t, pushDir, env, pushLine, "https://github.com/"+slug+".git")
			}
			want := "the real gh " + shim + " ran `legion gh` again"
			if got := startsOf(t); code != 2 || stdout != "" || !strings.Contains(stderr, want) || got != tc.starts {
				t.Errorf("%s with LEGION_GH_PATH %s = exit %d, stdout %q, stderr %q, and `legion gh` started %d more times; want exit 2 containing %q and %d more starts",
					tc.name, shim, code, stdout, stderr, got, want, tc.starts)
			}
		})
	}

	// The shared check's own read of slug, made by a stand-in check of the hook of a push the real
	// gh makes. The push names a local path, so the hook's guard reads no visibility of its own and
	// the check's read is where every start comes from. The `legion gh` that answers the read finds
	// no visibility, since the real gh its read reaches leads back, and answers exit 1, which the
	// check records with what that `legion gh` said.
	for _, token := range []bool{false, true} {
		name := "a shim that runs the wrapper: the shared check's own read in the hook of a push the real gh makes"
		if token {
			name += " under GH_TOKEN"
		}
		t.Run(name, func(t *testing.T) {
			if err := os.Remove(starts); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			record := filepath.Join(t.TempDir(), "read")
			readsVisibility := "cat >/dev/null 2>&1\n" +
				leakguard.VisibilityReadVar + "=" + slug + " gh " + strings.Join(leakguard.VisibilityReadArgs(slug), " ") + " >/dev/null 2>'" + record + "'\n" +
				"echo \"exit $?\" >'" + record + ".status'\nexit 0\n"
			env := envFor(shim)
			env["PUBLIC_LEAK_CHECK"] = leakguardtest.Write(t, t.TempDir(), readsVisibility)
			env[leakguard.RunByRealGhVar] = shim
			if token {
				env["GH_TOKEN"] = "ghs_caller"
			}
			code, printed := legionLeakCheckPrePush(t, pushDir, env, pushLine, pushDir+".git")
			if code != leakguard.StatusClean || printed != "" {
				t.Errorf("legion leak-check pre-push = exit %d, stderr %q; want exit 0 and nothing printed", code, printed)
			}
			want := "the real gh " + shim + " ran `legion gh` again"
			if status, said, got := readRecord(t, record+".status"), readRecord(t, record), startsOf(t); status != "exit 1\n" || !strings.Contains(said, want) || got != 3 {
				t.Errorf("the shared check's own read with LEGION_GH_PATH %s = %q, stderr %q, and `legion gh` started %d more times; want exit 1 containing %q and 3 more starts",
					shim, status, said, got, want)
			}
		})
	}

	// gh's own git asks the wrapper under the variable, and the wrapper still answers.
	marked := envFor(shim)
	marked[leakguard.RunByRealGhVar] = shim
	if code, stdout, stderr := legionGhIn(t, t.TempDir(), marked, "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n", "auth", "git-credential", "get"); code != 0 || stdout != "username=x-access-token\npassword=ghs_acme_write\n" {
		t.Errorf("legion gh auth git-credential get under %s = exit %d, stdout %q, stderr %q; want acme's token", leakguard.RunByRealGhVar, code, stdout, stderr)
	}

	// The real gh a call runs is given the variable, naming itself.
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"printf 'run by %s\\n' \"$"+leakguard.RunByRealGhVar+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := legionGh(t, envFor(stub), "pr", "list"); code != 0 || stdout != "run by "+stub+"\n" {
		t.Errorf("legion gh pr list with a real gh = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, "run by "+stub+"\n")
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
	if err := workerbin.Install(root, "/opt/legion state/legion", "/usr/bin:/bin"); err != nil {
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

// ghStandInPrefix is what a stand-in real gh of this package answers with first, and what every
// one of them carries after any line of its own that records its run (loggingGh). It answers
// `[<command>] --help` with the saved help of that command, gh's own for none, which is the help
// the command resolver reads; and it answers the leak guard's own visibility reads — `gh api
// --hostname github.com repos/<slug> [--include] --jq .visibility`, and the GraphQL read `gh api
// --hostname github.com graphql --include -f query=… -f owner=… -f name=…` — with a visibility,
// the word GH_STANDIN_VISIBILITY names or public, recording it in GH_STANDIN_RECORD where the test
// names one. It tells those reads by their argv, as the real gh would have to: `legion gh` runs gh
// from an allowlist of its environment, which holds no Legion variable naming the read
// (ghChildEnviron). Without that answer the guard's read of a destination's visibility would get
// a stand-in's ordinary output and refuse every call it cleared, since a visibility it cannot read
// is a setup failure.
func ghStandInPrefix(help string) string {
	return ghStandInHelp(help) +
		"case \" $* \" in\n" +
		ghVisibilityReadArgv + ")\n" +
		"  printf 'visibility %s\\n' \"$*\" >>\"${GH_STANDIN_RECORD:-/dev/null}\"\n" +
		"  echo \"${GH_STANDIN_VISIBILITY:-public}\"\n" +
		"  exit 0;;\n" +
		"esac\n"
}

// ghVisibilityReadArgv is the shell `case` pattern that matches the leak guard's own visibility
// reads by their argv, as a stand-in gh must tell them: the REST read, `gh api --hostname
// github.com repos/<slug> [--include] --jq .visibility`, and the GraphQL read, `gh api --hostname
// github.com graphql --include -f query=… -f owner=… -f name=…`, each matched against " $* ".
const ghVisibilityReadArgv = "\\ api\\ --hostname\\ github.com\\ *\\ --jq\\ .visibility\\ |\\ api\\ --hostname\\ github.com\\ graphql\\ --include\\ -f\\ query=*\\ -f\\ owner=*\\ -f\\ name=*\\ "

// ghStandInHelp is the start of every stand-in gh of this package: a shell script that answers
// `[<command>] --help` with the saved help of that command, gh's own for none, and falls through
// to what the caller appends for every other run.
func ghStandInHelp(help string) string {
	return "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n"
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
// buffered handed to gh in this process's place. A call that writes nothing runs the check too,
// with no visibility read (TestLegionGhChecksAReadAndReadsNoVisibility).
//
// Two calls pass unchecked. `legion gh auth git-credential` answers git as it always did, with
// no check run at all, since reading its standard input would break git's protocol. And a
// visibility read of the shared check's or of the guard's own (the GraphQL read the guard makes
// when GitHub refuses its REST read for a spent rate limit) passes when it is exactly that read
// for exactly the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names — without it a pod's gh,
// which is this command, would call the check, whose own read would call it again, without end —
// and is not logged, since the call it reads for logs it. The variable makes no other call one
// of those reads: a write under it, another read under it, and the same read under a variable
// naming no repository are each checked like anything else. The shared check's own read this
// command answers itself, through the guard's REST read, which reaches gh.
//
// A push made with the caller's own token carries GH_TOKEN or GITHUB_TOKEN into the guard, and so
// into each of those reads. Each read runs under that token, for a repository of an owner the
// bundle holds no directory for too. The token widens nothing else. Under it, this command runs
// the guard's two visibility reads, the shared check's own read, the exact `gh api rate_limit` and
// `gh auth git-credential`, and refuses every other call before gh runs, naming the token. So
// another read under the variable, the guard's read under no variable, and the guard's read of
// another repository than the variable names are each refused.
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
		state := t.TempDir()
		code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion", "XDG_STATE_HOME": state}, visibility...)
		if code != 0 || stdout != "public\n" {
			t.Errorf("the check's own visibility read = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", code, stdout, stderr)
		}
		if checked != "" {
			t.Errorf("the shared check ran for its own visibility read: %q", checked)
		}
		if want := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion"), " ") + "\n"; ghRan != want {
			t.Errorf("gh recorded %q, want the guard's REST read, %q", ghRan, want)
		}
		if log := readRecord(t, filepath.Join(state, "legion-gh", "calls.log")); log != "" {
			t.Errorf("the check's own visibility read was logged as a call of its own: %q", log)
		}
	})

	t.Run("unchecked: the guard's own GraphQL visibility read", func(t *testing.T) {
		graphql := leakguard.VisibilityGraphQLArgs("sjawhar/legion")
		code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion"}, graphql...)
		if code != 0 || stdout != "public\n" {
			t.Errorf("the guard's own GraphQL read = exit %d, stdout %q, stderr %q; want exit 0 and gh's answer", code, stdout, stderr)
		}
		if checked != "" {
			t.Errorf("the shared check ran for the guard's own GraphQL read: %q", checked)
		}
		if want := "visibility " + strings.Join(graphql, " ") + "\n"; ghRan != want {
			t.Errorf("gh recorded %q, want %q", ghRan, want)
		}
	})

	// A push a real gh makes reaches the guard under the variable every real gh this command runs
	// is given (leakguard.RunByRealGhVar), through git's pre-push hook, and so does a shared check
	// that hook runs itself. The shared check's own read is answered under it, through the guard's
	// REST read, which reaches gh because the guard blanks the variable for its children, under a
	// caller's token as without one. The guard's own reads made under the variable are refused,
	// exit 2, and gh never runs: the guard runs none of them under it, so only a real gh that leads
	// back to this command makes one so.
	for _, token := range []string{"", "GH_TOKEN"} {
		under := "under " + leakguard.RunByRealGhVar
		marked := map[string]string{leakguard.RunByRealGhVar: stub, leakguard.VisibilityReadVar: "sjawhar/legion"}
		if token != "" {
			under += " and " + token
			marked[token] = "ghs_caller"
		}
		t.Run("unchecked "+under+": the shared check's own visibility read", func(t *testing.T) {
			args := leakguard.VisibilityReadArgs("sjawhar/legion")
			code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", marked, args...)
			if code != 0 || stdout != "public\n" {
				t.Errorf("legion gh %q %s = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", args, under, code, stdout, stderr)
			}
			if checked != "" {
				t.Errorf("the shared check ran for its own visibility read: %q", checked)
			}
			if want := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion"), " ") + "\n"; ghRan != want {
				t.Errorf("gh recorded %q, want the guard's REST read, %q", ghRan, want)
			}
		})
		for _, read := range []struct {
			name string
			args []string
		}{
			{"REST", leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion")},
			{"GraphQL", leakguard.VisibilityGraphQLArgs("sjawhar/legion")},
		} {
			t.Run("refused "+under+": the guard's own "+read.name+" visibility read", func(t *testing.T) {
				code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", marked, read.args...)
				if want := "the real gh " + stub + " ran `legion gh` again"; code != 2 || stdout != "" || !strings.Contains(stderr, want) {
					t.Errorf("legion gh %q %s = exit %d, stdout %q, stderr %q; want exit 2 containing %q", read.args, under, code, stdout, stderr, want)
				}
				if ghRan != "" || checked != "" {
					t.Errorf("gh recorded %q and the check %q, want neither run", ghRan, checked)
				}
			})
		}
	}

	for _, token := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		for _, slug := range []string{"sjawhar/legion", "octo/elsewhere"} {
			rest := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs(slug), " ") + "\n"
			for _, read := range []struct {
				name string
				args []string
				want string
			}{
				{"the shared check's own visibility read", leakguard.VisibilityReadArgs(slug), rest},
				{"the guard's own REST visibility read", leakguard.VisibilityReadWithHeadersArgs(slug), rest},
				{"the guard's own GraphQL visibility read", leakguard.VisibilityGraphQLArgs(slug), "visibility " + strings.Join(leakguard.VisibilityGraphQLArgs(slug), " ") + "\n"},
			} {
				t.Run("unchecked under "+token+": "+read.name+" of "+slug, func(t *testing.T) {
					code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", map[string]string{token: "ghs_caller", leakguard.VisibilityReadVar: slug}, read.args...)
					if code != 0 || stdout != "public\n" {
						t.Errorf("legion gh %q under %s = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", read.args, token, code, stdout, stderr)
					}
					if checked != "" {
						t.Errorf("the shared check ran for a visibility read: %q", checked)
					}
					if ghRan != read.want {
						t.Errorf("gh recorded %q, want %q", ghRan, read.want)
					}
				})
			}
		}
		for _, tc := range []struct {
			name string
			env  map[string]string
			args []string
		}{
			{"another read under the variable", map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion"}, []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".name"}},
			{"the guard's read under no variable", nil, leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion")},
			{"the guard's read of another repository than the variable's", map[string]string{leakguard.VisibilityReadVar: "sjawhar/other"}, leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion")},
		} {
			t.Run("refused under "+token+": "+tc.name, func(t *testing.T) {
				extra := map[string]string{token: "ghs_caller"}
				maps.Copy(extra, tc.env)
				code, stdout, stderr, ghRan, checked := call(t, ghCheckRefuses, "", extra, tc.args...)
				if want := token + " is set"; code != 2 || !strings.Contains(stderr, want) {
					t.Errorf("legion gh %q under %s = exit %d, stdout %q, stderr %q; want exit 2 and %q", tc.args, token, code, stdout, stderr, want)
				}
				if checked != "" || ghRan != "" {
					t.Errorf("the check recorded %q and gh %q; want neither run", checked, ghRan)
				}
			})
		}
	}

	for _, tc := range []struct {
		name string
		slug string
		args []string
	}{
		{"a write under the same variable", "sjawhar/legion", []string{"api", "--method", "PATCH", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".visibility"}},
		{"another argv under the same variable", "sjawhar/legion", []string{"api", "--hostname", "github.com", "repos/sjawhar/legion", "--jq", ".name"}},
		{"a variable naming no repository", "sjawhar/legion/pulls", []string{"api", "--hostname", "github.com", "repos/sjawhar/legion/pulls", "--jq", ".visibility"}},
		{"the GraphQL read of another repository under the variable", "sjawhar/legion", leakguard.VisibilityGraphQLArgs("sjawhar/other")},
		{"another GraphQL query under the variable", "sjawhar/legion", graphQLOf("query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { description } }")},
		{"a GraphQL mutation under the variable", "sjawhar/legion", graphQLOf(inventedMutation)},
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

// With the deployment's rule file named, `legion gh` refuses a call whose body file names the
// fixture's host, exit 2, and gh never runs the call: the guard reads the destination's
// visibility, which the stand-in gh answers public, then the file, and the fixture's host rule
// matches. Standard error shows what matched, as `legion leak-check` does: the record, which is
// the file as the call names it; the rule's description; the host and the space after it; and the
// line of the file it stands on, the second, whole. The refusal is the whole of what it prints.
func TestGhRefusalShowsMatch(t *testing.T) {
	run := ghGuardRig(t, leakguardtest.WriteRules(t, t.TempDir()))
	const line = "the runner answers at " + leakguardtest.RulesHost + " today"
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("a first line of no interest\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"pr", "create", "-R", "acme/widgets", "--title", "a title of no interest", "--body-file", body}
	got := run(t, ghCheckAt(t, ghCheckPasses), "", args...)
	want := "legion gh: LEAK CHECK: refusing the gh call: the deployment's private-name rules match it\n" +
		"  " + leakguardtest.ReportLine(body, leakguardtest.RulesHostClass, leakguardtest.RulesHost+" ", 2, line) + "\n"
	if got.code != 2 || got.stderr != want {
		t.Errorf("legion gh %q = exit %d, stderr %q; want exit 2 and %q", args, got.code, got.stderr, want)
	}
	if want := ghGuardRun(args); got.checked != want {
		t.Errorf("the shared check recorded %q, want its one run of the call, %q", got.checked, want)
	}
	// The guard's visibility read alone: gh never ran the call.
	if want := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), " ") + "\n"; got.gh != want {
		t.Errorf("gh recorded %q, want the guard's visibility read alone, %q", got.gh, want)
	}
}

// `legion gh` hands the guard the use the call makes of standard input, read out of its parse
// from the option that named that input (leakguard.GhRequest.Sent), so the guard reads that
// input as what it is. A JSON request body on `gh api --input -` whose one field holds the fixture
// pair's name and another field its word is read one string at a time and cleared, and gh runs it
// with the input the guard read; read raw as well, as it is where no use is known, the two would
// stand on one line and the pair rule would refuse it. The same text on `--body-file -` is a body
// GitHub stores as it stands, read whole, and refused.
func TestLegionGhHandsTheGuardTheOptionThatNamedStandardInput(t *testing.T) {
	run := ghGuardRig(t, leakguardtest.WriteRules(t, t.TempDir()))
	const body = `{"title":"` + leakguardtest.PairName + `","body":"` + leakguardtest.PairWord + `"}`
	args := []string{"api", "repos/acme/widgets/pulls", "--method", "POST", "--input", "-"}
	got := run(t, ghCheckAt(t, ghCheckReading(0)), body, args...)
	if got.code != 7 || got.stderr != "" {
		t.Errorf("legion gh %q with a JSON body = exit %d, stderr %q; want gh's own 7 and nothing printed", args, got.code, got.stderr)
	}
	if want := "gh " + strings.Join(args, " ") + "\n" + ghRigEnv + "stdin " + body + "\n"; !strings.HasSuffix(got.gh, want) {
		t.Errorf("gh recorded %q, want the call with the input the guard read, %q", got.gh, want)
	}
	args = []string{"pr", "create", "-R", "acme/widgets", "--title", "a title of no interest", "--body-file", "-"}
	got = run(t, ghCheckAt(t, ghCheckReading(0)), body, args...)
	if want := "the gh call's standard input: " + leakguardtest.PairClass + ": matched "; got.code != 2 || !strings.Contains(got.stderr, want) {
		t.Errorf("legion gh %q with the same text = exit %d, stderr %q; want exit 2 and a line holding %q", args, got.code, got.stderr, want)
	}
	if strings.Contains("\n"+got.gh, "\ngh ") {
		t.Errorf("gh ran a call the guard refused: %q", got.gh)
	}
}

// `gh workflow run <workflow> --json` reads the run's inputs from standard input as one JSON
// object (testdata/gh-help/workflow_run.txt: "--json  Read workflow inputs as JSON via STDIN", and
// its example `echo '{"name":"scully", "greeting":"hello"}' | gh workflow run triage.yml --json`),
// and GitHub keeps each input as a value of its own. So that input is read one string at a time,
// as a JSON request body is: inputs whose one value holds the fixture pair's name and another the
// word are cleared, and gh runs the call with the input the guard read; one value holding both is
// refused, the report naming the input by its JSON path. Read raw as well, as an input of no known
// use is, the two values would stand on one line and the pair rule would refuse the first call.
func TestLegionGhReadsAWorkflowRunsJSONInputsOneFieldAtATime(t *testing.T) {
	run := ghGuardRig(t, leakguardtest.WriteRules(t, t.TempDir()))
	args := []string{"workflow", "run", "triage.yml", "-R", "acme/widgets", "--json"}
	const apart = `{"name":"` + leakguardtest.PairName + `","greeting":"` + leakguardtest.PairWord + `"}`
	got := run(t, ghCheckAt(t, ghCheckReading(0)), apart, args...)
	if got.code != 7 || got.stderr != "" {
		t.Errorf("legion gh %q with two inputs = exit %d, stderr %q; want gh's own 7 and nothing printed", args, got.code, got.stderr)
	}
	if want := "gh " + strings.Join(args, " ") + "\n" + ghRigEnv + "stdin " + apart + "\n"; !strings.HasSuffix(got.gh, want) {
		t.Errorf("gh recorded %q, want the call with the input the guard read, %q", got.gh, want)
	}
	const together = `{"name":"the ` + leakguardtest.PairName + ` and the ` + leakguardtest.PairWord + `","greeting":"hello"}`
	got = run(t, ghCheckAt(t, ghCheckReading(0)), together, args...)
	if want := "the gh call's standard input, JSON .name: " + leakguardtest.PairClass + ": matched "; got.code != 2 || !strings.Contains(got.stderr, want) {
		t.Errorf("legion gh %q with one input holding both = exit %d, stderr %q; want exit 2 and a line holding %q", args, got.code, got.stderr, want)
	}
	if strings.Contains("\n"+got.gh, "\ngh ") {
		t.Errorf("gh ran a call the guard refused: %q", got.gh)
	}
}

// graphQLOf is the guard's GraphQL visibility read of sjawhar/legion with query in place of the
// guard's own, and inventedMutation a mutation to put there: `legion gh` must check either one
// like any other call, whatever PUBLIC_LEAK_CHECK_VISIBILITY_READ names.
func graphQLOf(query string) []string {
	return []string{"api", "--hostname", "github.com", "graphql", "--include", "-f", "query=" + query, "-f", "owner=sjawhar", "-f", "name=legion"}
}

const inventedMutation = "mutation($owner: String!, $name: String!) { createIssue(input: {repositoryId: $owner, title: $name}) { issue { id } } }"

// ghVisibilityRead lets through the guard's own REST and GraphQL visibility reads, and
// sharedCheckVisibilityRead the shared check's visibility callback, each for exactly the
// repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names, and nothing else: neither takes the other's
// read, and neither takes the GraphQL read of another repository, another query, a mutation, the
// read with a field added, the read with no variable, or a variable naming no repository.
func TestGhVisibilityReadLetsThroughOnlyTheExactReads(t *testing.T) {
	const slug = "sjawhar/legion"
	env := map[string]string{leakguard.VisibilityReadVar: slug}
	graphql := leakguard.VisibilityGraphQLArgs(slug)
	for _, tc := range []struct {
		name string
		argv []string
		env  map[string]string
		// guards is whether ghVisibilityRead takes argv, checks whether sharedCheckVisibilityRead does.
		guards, checks bool
	}{
		{"the shared check's callback", leakguard.VisibilityReadArgs(slug), env, false, true},
		{"the guard's REST read", leakguard.VisibilityReadWithHeadersArgs(slug), env, true, false},
		{"the guard's GraphQL read", graphql, env, true, false},
		{"the callback with no variable", leakguard.VisibilityReadArgs(slug), map[string]string{}, false, false},
		{"the callback of another repository", leakguard.VisibilityReadArgs("sjawhar/other"), env, false, false},
		{
			"the callback under a variable naming no repository",
			leakguard.VisibilityReadArgs("sjawhar/legion/pulls"),
			map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion/pulls"},
			false, false,
		},
		{"the GraphQL read of another repository", leakguard.VisibilityGraphQLArgs("sjawhar/other"), env, false, false},
		{"another GraphQL query", graphQLOf("{viewer{login}}"), env, false, false},
		{"a GraphQL mutation", graphQLOf(inventedMutation), env, false, false},
		{"the GraphQL read with a field added", append(slices.Clone(graphql), "-f", "extra=1"), env, false, false},
		{"the GraphQL read with no variable", graphql, map[string]string{}, false, false},
		{
			"the GraphQL read under a variable naming no repository",
			leakguard.VisibilityGraphQLArgs("sjawhar/legion/pulls"),
			map[string]string{leakguard.VisibilityReadVar: "sjawhar/legion/pulls"},
			false, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, read := range []struct {
				name string
				of   func([]string, map[string]string) (ghrepo.Repository, bool)
				want bool
			}{{"ghVisibilityRead", ghVisibilityRead, tc.guards}, {"sharedCheckVisibilityRead", sharedCheckVisibilityRead, tc.checks}} {
				repo, got := read.of(tc.argv, tc.env)
				if got != read.want {
					t.Errorf("%s(%q) = %v, want %v", read.name, tc.argv, got, read.want)
				}
				if got && repo.String() != slug {
					t.Errorf("%s(%q) read the repository %q, want %s", read.name, tc.argv, repo, slug)
				}
			}
		})
	}
}

// The guard's own visibility reads run with the token of the directory the repository they read
// ROUTES to, the REST read and the GraphQL read alike. The REST read names its repository in its
// endpoint; the GraphQL read names it only in variables, which the resolver reads as an owner
// alone, so its directory comes from the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names,
// which ghVisibilityRead has matched to the read's exact argv.
//
// A bundle holds one directory per owner and none per repository since INFRA-2543, so what tells
// the two halves apart is a configured rename rather than a repository directory: with the pair
// sjawhar/legion -> cybermonklabs/legion and the App installed on both owners, both reads of
// sjawhar/legion take cybermonklabs', the owner whose installation holds the repository GitHub
// serves under that name, and both reads of sjawhar/legion-smoke, which the pair does not move,
// take sjawhar's. Were the GraphQL read resolved by its owner alone, the sjawhar/legion half
// would take sjawhar's and this row would fail.
//
// Under the caller's own GH_TOKEN or GITHUB_TOKEN both reads run with that token, which gh sends
// in place of any directory's: they keep the GH_CONFIG_DIR they were given and are routed by
// nothing, for octo/elsewhere, whose owner the bundle holds no directory for, as much as for a
// repository it does hold one for (#4219).
func TestLegionGhRunsTheGuardsVisibilityReadsWithTheRepositorysOrTheCallersToken(t *testing.T) {
	root := pairRoutedBundle(t)
	defaultDir := ghconfig.Dir(root, "cybermonklabs")
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub records the GH_CONFIG_DIR, the GH_TOKEN, the GITHUB_TOKEN and the argv of every
	// run, then answers as the package's stand-in does: a help request with the saved help, a
	// visibility read with public.
	stub := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$GH_CONFIG_DIR\" \"$GH_TOKEN\" \"$GITHUB_TOKEN\" \"$*\" >>\"${GH_STANDIN_RECORD:-/dev/null}\"\n" +
		strings.TrimPrefix(ghStandInPrefix(help), "#!/bin/sh\n") + "exit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	for _, tc := range []struct {
		slug, token, dir string
	}{
		{"sjawhar/legion", "", ghconfig.Dir(root, "cybermonklabs")},
		{"sjawhar/legion-smoke", "", ghconfig.Dir(root, "sjawhar")},
		{"sjawhar/legion", "GH_TOKEN", defaultDir},
		{"octo/elsewhere", "GITHUB_TOKEN", defaultDir},
	} {
		for _, read := range []struct {
			name string
			args []string
		}{
			{"REST", leakguard.VisibilityReadWithHeadersArgs(tc.slug)},
			{"GraphQL", leakguard.VisibilityGraphQLArgs(tc.slug)},
		} {
			t.Run(read.name+" "+tc.slug+" "+tc.token, func(t *testing.T) {
				record := filepath.Join(t.TempDir(), "gh")
				env := map[string]string{
					"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
					"PATH": wrappers + ":" + os.Getenv("PATH"), "GH_STANDIN_RECORD": record,
					leakguard.VisibilityReadVar: tc.slug,
				}
				tokens := map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": ""}
				if tc.token != "" {
					env[tc.token], tokens[tc.token] = "ghs_caller", "ghs_caller"
				}
				code, stdout, stderr := legionGh(t, env, read.args...)
				if code != 0 || stdout != "public\n" {
					t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and gh's answer", read.args, code, stdout, stderr)
				}
				if want := tc.dir + "|" + tokens["GH_TOKEN"] + "|" + tokens["GITHUB_TOKEN"] + "|" + strings.Join(read.args, " ") + "\n"; !strings.Contains(readRecord(t, record), want) {
					t.Errorf("gh recorded\n%s\nwant the read run with %s:\n%s", readRecord(t, record), tc.dir, want)
				}
			})
		}
	}
}

// The other half of the octo/elsewhere row above: a visibility read runs under the caller's token
// for an owner the bundle holds no directory for, and the SAME read with no caller token is
// refused, exit 2, naming that owner. That is what makes the caller-token arm worth having rather
// than a widening of what any read may reach: without a token of its own the read is still routed
// through the bundle (ghroute.DirFor), which holds nothing for octo, and #4219's second reason —
// that a read with the bundle's directory would be refused for a repository the caller's token
// writes — is this refusal.
//
// One gate holds it: the routing in the ghVisibilityCall arm, which refuses the unheld owner from
// the read's own repository. ghroute.TargetOf never reads these reads, which read no gh help
// (TestLegionGhReadsNoHelpForTheGuardsVisibilityReads).
func TestLegionGhRefusesTheGuardsVisibilityReadForAnUnheldOwnerWithoutACallerToken(t *testing.T) {
	root := pairRoutedBundle(t)
	defaultDir := ghconfig.Dir(root, "cybermonklabs")
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	const slug = "octo/elsewhere"
	for _, read := range []struct {
		name string
		args []string
	}{
		{"REST", leakguard.VisibilityReadWithHeadersArgs(slug)},
		{"GraphQL", leakguard.VisibilityGraphQLArgs(slug)},
	} {
		t.Run(read.name, func(t *testing.T) {
			env := map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH": wrappers + ":" + os.Getenv("PATH"), leakguard.VisibilityReadVar: slug,
			}
			code, stdout, stderr := legionGh(t, env, read.args...)
			const refusal = `the role's GitHub App is not installed on "octo"`
			if code != 2 || stdout != "" || !strings.Contains(stderr, refusal) {
				t.Errorf("legion gh %q with no caller token = exit %d, stdout %q, stderr %q; want exit 2 and %s", read.args, code, stdout, stderr, refusal)
			}
		})
	}
}

// `legion gh` answers the shared check's own visibility read itself, by the guard's rule, and
// prints what `gh api ... --jq .visibility` prints: the word and a newline, exit 0; or, where no
// read answers, nothing on standard output and exit 1, which the check reads as an unknown
// visibility and refuses. The guard's reads come back through `legion gh` to the stand-in gh: the
// REST read, then, with the installation's REST limit spent, the GraphQL read, then a stand-in
// git's ls-remote, which can answer only public. Where gh's REST read fails on its connection to
// GitHub, the read through curl comes next, and it is a stand-in curl's (testCurlEnv) that records
// itself beside them: the system's curl would carry a token of the bundle to api.github.com, and
// the row's verdict would be the network's. Neither stream carries the shared check's own
// refusal, which only the check prints.
//
// The read is fresh: the check reads another remote of a push with it, and counts the commits of
// one it reads public as held, which it does not scan, so a public the guard kept for a
// repository made private since must not answer it. A kept public, which the guard's own
// Visibility does answer with, is passed over for the read, and no answer is kept. For a
// repository of an owner the bundle holds no directory for, `legion gh` refuses the guard's REST
// read before any gh runs, and ls-remote alone answers: public, or nothing.
func TestLegionGhAnswersTheSharedChecksVisibilityRead(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	const slug = "sjawhar/legion"
	rest := func(visibility string) string {
		return "printf '%s\\r\\n' 'HTTP/2.0 200 OK' 'X-Ratelimit-Remaining: 4999' ''\nprintf '%s\\n' " + visibility + "\nexit 0\n"
	}
	// A spent core limit whose reset is half an hour away, past the longest the guard waits for a
	// reset (Guard.waitFits, rateLimitWaitBound), so the guard does not wait for it whatever bound
	// the answer has.
	restSpent := fmt.Sprintf("printf '%%s\\r\\n' 'HTTP/2.0 403 Forbidden' 'X-Ratelimit-Remaining: 0' 'X-Ratelimit-Reset: %d' ''\n", time.Now().Add(30*time.Minute).Unix()) +
		`printf '%s\n' '{"message":"API rate limit exceeded for installation ID 1."}'` + "\n" +
		"echo 'gh: API rate limit exceeded for installation ID 1. (HTTP 403)' >&2\nexit 1\n"
	restFails := "printf '%s\\r\\n' 'HTTP/2.0 500 Internal Server Error' ''\nprintf '%s\\n' '{\"message\":\"invented\"}'\nexit 1\n"
	graphQL := func(body string, exit int) string {
		return "echo graphql >> \"$GH_STANDIN_RECORD\"\n" +
			"printf '%s\\r\\n' 'HTTP/2.0 200 OK' 'X-Ratelimit-Remaining: 12221' ''\n" +
			"printf '%s' '" + body + "'\n" + fmt.Sprintf("exit %d\n", exit)
	}
	answers := func(visibility string) string {
		return graphQL(`{"data":{"repository":{"nameWithOwner":"`+slug+`","visibility":"`+visibility+`"}}}`, 0)
	}
	graphQLLimited := graphQL(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded"}]}`, 1)
	graphQLNotFound := graphQL(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`, 1)
	lists := "printf '%s\\tHEAD\\n' " + strings.Repeat("1", 40) + "\nexit 0\n"
	refused := "echo 'fatal: could not read Username for '\\''https://github.com'\\'': terminal prompts disabled' >&2\nexit 128\n"
	// tlsTimeout is gh's REST read failing on its connection before GitHub answered anything, in the
	// one line gh 2.98.0 printed for a TLS handshake that never finished (leakguard's
	// ghTransportFailure), so the guard reads through curl next.
	tlsTimeout := "echo 'Get \"https://api.github.com/repos/" + slug + "\": net/http: TLS handshake timeout' >&2\nexit 1\n"
	// curlAnswers is the read through curl answered by GitHub with visibility; curlUnreached is a curl
	// that could not connect, exit 7, which the guard reads as no answer from GitHub and follows
	// with ls-remote. A case that names no curl answer gets curlUnreached.
	curlAnswers := func(visibility string) string {
		return "printf '%s\\r\\n' 'HTTP/2 200' ''\nprintf '%s\\n' '{\"visibility\":\"" + visibility + "\"}'\nexit 0\n"
	}
	curlUnreached := "echo 'curl: (7) Failed to connect to api.github.com port 443 after 0 ms: Could not connect to server' >&2\nexit 7\n"
	// keptFiles is each file of the guard's kept visibility answers under state, with its content.
	keptFiles := func(t *testing.T, state string) map[string]string {
		t.Helper()
		dir := filepath.Join(state, "legion-gh", "visibility")
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}
		}
		if err != nil {
			t.Fatal(err)
		}
		files := make(map[string]string, len(entries))
		for _, entry := range entries {
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[entry.Name()] = string(content)
		}
		return files
	}
	// keepPublic plants the guard's kept public answer for slug under state, as Guard.Visibility
	// keeps one, and fails the test unless the guard's own Visibility then answers public from it
	// without a read.
	keepPublic := func(t *testing.T, state string) {
		t.Helper()
		dir := filepath.Join(state, "legion-gh", "visibility")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		entry := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(slug))))
		if err := os.WriteFile(entry, []byte(`{"visibility":"public"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		visibility, err := leakguard.New(leakguard.Options{
			Env: []string{"XDG_STATE_HOME=" + state},
			Visibility: func(context.Context, string) (leakguard.Visibility, error) {
				return "", errors.New("the kept answer was not read")
			},
		}).Visibility(context.Background(), slug)
		if err != nil || visibility != leakguard.Public {
			t.Fatalf("the guard's own Visibility answered %q, %v from the kept entry; want the kept public", visibility, err)
		}
	}
	for _, tc := range []struct {
		name string
		// slug is the repository the check reads, sjawhar/legion where it is empty.
		slug          string
		rest, graphql string
		git           string
		// curl is the stand-in curl's answer, curlUnreached where it is empty.
		curl string
		// kept is whether the guard holds a kept public answer for the repository when the read
		// is made.
		kept bool
		// want is what legion gh prints, empty where it answers nothing and exits 1; reads is
		// the reads the stand-ins recorded, in order.
		want  string
		reads string
	}{
		{name: "REST answers public", rest: rest("public"), want: "public\n", reads: "rest\n"},
		{name: "REST answers private", rest: rest("private"), want: "private\n", reads: "rest\n"},
		{name: "a kept public, and REST answers private", kept: true, rest: rest("private"), want: "private\n", reads: "rest\n"},
		{
			name: "a kept public, and REST is limited and GraphQL answers private", kept: true,
			rest: restSpent, graphql: answers("PRIVATE"), want: "private\n", reads: "rest\ngraphql\n",
		},
		{name: "REST is limited and GraphQL answers public", rest: restSpent, graphql: answers("PUBLIC"), want: "public\n", reads: "rest\ngraphql\n"},
		{name: "REST is limited and GraphQL answers private", rest: restSpent, graphql: answers("PRIVATE"), want: "private\n", reads: "rest\ngraphql\n"},
		{name: "REST is limited and GraphQL answers internal", rest: restSpent, graphql: answers("INTERNAL"), want: "internal\n", reads: "rest\ngraphql\n"},
		{name: "REST and GraphQL are limited and ls-remote answers", rest: restSpent, graphql: graphQLLimited, git: lists, want: "public\n", reads: "rest\ngraphql\ngit\n"},
		{name: "REST and GraphQL are limited and ls-remote is refused", rest: restSpent, graphql: graphQLLimited, git: refused, reads: "rest\ngraphql\ngit\n"},
		{name: "REST is limited and GraphQL could not resolve the repository", rest: restSpent, graphql: graphQLNotFound, git: lists, reads: "rest\ngraphql\n"},
		{name: "REST fails another way", rest: restFails, graphql: answers("PRIVATE"), git: lists, reads: "rest\n"},
		{name: "gh cannot finish its TLS handshake and curl answers private", rest: tlsTimeout, curl: curlAnswers("private"), git: lists, want: "private\n", reads: "rest\ncurl\n"},
		{name: "gh and curl cannot reach GitHub and ls-remote answers", rest: tlsTimeout, git: lists, want: "public\n", reads: "rest\ncurl\ngit\n"},
		{name: "an unserved owner's repository ls-remote lists", slug: "other-owner/invented-repo", rest: rest("private"), git: lists, want: "public\n", reads: "git\n"},
		{name: "an unserved owner's repository GitHub asks ls-remote a credential for", slug: "other-owner/invented-repo", rest: rest("private"), git: refused, reads: "git\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := cmp.Or(tc.slug, slug)
			bin, record, state := t.TempDir(), filepath.Join(t.TempDir(), "reads"), t.TempDir()
			if tc.kept {
				keepPublic(t, state)
			}
			kept := keptFiles(t, state)
			stub := filepath.Join(bin, "real-gh")
			script := "#!/bin/sh\nfor last; do :; done\n" +
				"if [ \"$last\" = --help ]; then\n" +
				"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
				"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n" +
				"if [ \"$4\" = graphql ]; then\n  :\n" + tc.graphql + "fi\n" +
				"echo rest >> \"$GH_STANDIN_RECORD\"\n" + tc.rest
			if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho git >> \"$GH_STANDIN_RECORD\"\n"+tc.git), 0o700); err != nil {
				t.Fatal(err)
			}
			// The stand-in curl runs with only the names leakguard.TokenChildKeeps keeps, which hold
			// no GH_STANDIN_RECORD, so its record's path is written into it. It reads the token the
			// guard hands it on its standard input, as curl does, and keeps none of it.
			curl := filepath.Join(t.TempDir(), "curl")
			if err := os.WriteFile(curl, []byte("#!/bin/sh\ncat >/dev/null\necho curl >> '"+record+"'\n"+cmp.Or(tc.curl, curlUnreached)), 0o700); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := legionGh(t, map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH": wrappers + ":" + bin + ":" + os.Getenv("PATH"), "GH_STANDIN_RECORD": record,
				"XDG_STATE_HOME": state, leakguard.VisibilityReadVar: repo, testCurlEnv: curl,
			}, leakguard.VisibilityReadArgs(repo)...)
			switch {
			case tc.want != "" && (code != 0 || stdout != tc.want):
				t.Errorf("legion gh = exit %d, stdout %q, stderr %q; want exit 0 and %q", code, stdout, stderr, tc.want)
			case tc.want == "" && (code != 1 || stdout != ""):
				t.Errorf("legion gh = exit %d, stdout %q, stderr %q; want exit 1 and nothing on standard output", code, stdout, stderr)
			}
			if got := readRecord(t, record); got != tc.reads {
				t.Errorf("the stand-ins recorded the reads %q, want %q", got, tc.reads)
			}
			if got := keptFiles(t, state); !maps.Equal(got, kept) {
				t.Errorf("the guard's kept answers are %q after the read, want %q: the read keeps none", got, kept)
			}
			for _, refusal := range []string{"cannot read the visibility", "nothing is pushed"} {
				if strings.Contains(stdout+stderr, refusal) {
					t.Errorf("legion gh printed the shared check's own refusal %q: stdout %q, stderr %q", refusal, stdout, stderr)
				}
			}
		})
	}
}

// Two wrapped calls to one repository reuse its public visibility answer, and their audit entries
// identify the call without preserving the text that the command would send. The deployment
// names a rule file (the fixture's), without which the guard reads no visibility at all
// (TestLegionGhRunsAWriteWithNoRuleFile).
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
		leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
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
//
// The 128 processes are copies of this test binary, each run on one P (GOMAXPROCS=1). A copy left
// to size GOMAXPROCS to the host starts a thread for each P its garbage collector's idle mark
// workers take: about 80 threads a copy on a 288-CPU host against 5 on one P, so on such a host
// the 128 together held up to 6,800 tasks and fork failed under a 4096-task cgroup limit. Each copy
// appends one line, which one P does as well as many. Every copy is waited for before the test
// reports, so a failure leaves none running into the next test.
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
				"GOMAXPROCS=1",
				"LEGION_TEST_GH_CALL_LOG_REPOSITORY="+repository,
				"LEGION_TEST_GH_CALL_LOG_STATE="+state,
			)
			errs <- command.Run()
		}()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		return
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

// The rate-limit endpoint is a read with no repository content, so it bypasses the guard: no
// check runs and no visibility is read. A GET of another endpoint writes nothing, so it runs the
// check, and a check that refuses it refuses it, but it reads no visibility: where the visibility
// read fails, as it does in an hour whose REST limit is spent, the GET still reaches gh. A write
// to the same repository still reads its visibility where the deployment names a rule file, as
// every case here does, and is refused when that read fails.
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
	script := strings.Replace(ghStandInPrefix(help), "  echo \"${GH_STANDIN_VISIBILITY:-public}\"\n  exit 0;;\n", "  echo unavailable >&2\n  exit 1;;\n", 1) + "exit 0\n"
	if !strings.Contains(script, "echo unavailable") {
		t.Fatal("the stand-in's visibility answer was not replaced, so this test would read a visibility")
	}
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
		{"the GET, under a check that would refuse it", []string{"api", "rate_limit"}, ghCheckRefuses, 0, 0},
		{"a POST", []string{"api", "rate_limit", "--method", "POST"}, ghCheckRefuses, 2, 0},
		{"another endpoint's GET under a check that refuses it", []string{"api", "user"}, ghCheckRefuses, 2, 0},
		{"another endpoint's GET while the visibility read fails", []string{"api", "repos/acme/widgets/pulls"}, ghCheckPasses, 0, 0},
		{"another endpoint's write while the visibility read fails", []string{"api", "repos/acme/widgets/issues", "--method", "POST"}, ghCheckPasses, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "gh")
			check := ghCheckAt(t, tc.check)
			env := map[string]string{
				"GH_CONFIG_DIR":        defaultDir,
				"GH_STANDIN_RECORD":    record,
				"LEGION_GH_OWNERS_DIR": root,
				"LEGION_GH_PATH":       stub,
				"PUBLIC_LEAK_CHECK":    check,
				leakguard.CheckerVar:   ghManifestOf(t, check),
				testProvenCheckEnv:     ghProvenProgramSHA256,
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
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
// The run is under a shell that ignores the signal, as nohup does, and no clock decides the row.
// gh's own disposition is read by behaviour, which every kernel shows: before it traps the hangup,
// the stand-in gh starts a child shell, which inherits gh's disposition of SIGHUP, and the child
// sends itself the hangup; it survives only if gh was started ignoring it, and dies of it
// otherwise (requireGhStartedIgnoringHangup). `legion gh`'s own disposition is read from
// /proc/<pid>/status, the only place a kernel reports another process's: a process that ignores a
// signal has it discarded as it is sent, so a hangup sent to such a `legion gh` can never end it,
// or reach gh, later; one with a handler for it (a signal.Notify) takes the hangup, and may end on
// it or forward it to gh at any later moment, which no wait of this row could outlast. So the row
// reads that gh was started ignoring SIGHUP, and that `legion gh` ignores it and has no handler
// for it, before the hangup and after it; then it releases gh and reads that gh ended on its own
// and `legion gh` came back with gh's status. A gVisor may report no signal masks in
// /proc/<pid>/status (release-20260803.0 reports none, release-20261116.0 does), and its
// /proc/<pid>/stat gives 0 for them even for a signal ignored, so there `legion gh`'s two reads
// have no source (requireHangupIgnored). The stand-in gh sets a trap of its own, which a shell
// started ignoring the signal cannot set, so a gh that records catching the hangup is one whose
// hangup the arming un-ignored.
func TestLegionGhLeavesAnIgnoredHangupIgnoredForGh(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	ghRecord, running := filepath.Join(records, "gh"), filepath.Join(records, "running")
	ghProbe, release := filepath.Join(records, "probe"), filepath.Join(records, "release")
	ghPid := filepath.Join(records, "pid")
	// The probe reads by behaviour, so the row first reads that it can fail: run with SIGHUP at
	// its default (env --default-signal sets that whatever this test binary inherited), it must
	// die of the hangup it sends itself.
	t.Run("the probe dies of a hangup at its default", func(t *testing.T) {
		requireProbeDiesAtDefault(t)
	})
	// The stand-in writes its pid, whole, and the status of its probe of its own disposition of
	// SIGHUP (ghHangupProbe, run in a child shell, which inherits that disposition). Then the
	// stand-in traps the hangup, says it is running, and waits for the test's release, or for this
	// test binary to be gone, in ticks short enough that a trap runs within one of them: a shell
	// runs no trap while a foreground command of its own is still going.
	stub := filepath.Join(t.TempDir(), "gh")
	script := ghStandInPrefix(help) +
		"echo $$ >\"$GH_STANDIN_PID.new\" && mv \"$GH_STANDIN_PID.new\" \"$GH_STANDIN_PID\"\n" +
		"sh -c '" + ghHangupProbe + "'; echo $? >\"$GH_STANDIN_PROBE\"\n" +
		"trap 'printf \"gh caught the hangup\\n\" >>\"$GH_STANDIN_RECORD\"; exit 44' HUP\n" +
		": >\"$GH_STANDIN_RUNNING\"\n" +
		"while [ ! -f \"$GH_STANDIN_RELEASE\" ] && kill -0 \"$GH_STANDIN_TEST_PID\" 2>/dev/null; do sleep 0.05; done\n" +
		"printf 'gh survived the hangup\\n' >>\"$GH_STANDIN_RECORD\"\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := legionGhCommand(t, t.TempDir(), map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		"GH_STANDIN_RECORD": ghRecord, "GH_STANDIN_RUNNING": running, "GH_STANDIN_RELEASE": release,
		"GH_STANDIN_PROBE": ghProbe, "GH_STANDIN_PID": ghPid, "GH_STANDIN_TEST_PID": strconv.Itoa(os.Getpid()),
	}, "pr", "list")
	// `sh -c 'trap "" HUP; exec …'` is what nohup does to the command it runs, and the exec is
	// the wrapper's: `legion gh` starts here with the hangup ignored and no handler of its own,
	// exactly as it does under `nohup gh …` on a pane's PATH, and keeps the shell's pid.
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
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	// A row that fails before it reads `legion gh`'s end ends both processes here, by the order of
	// their events. gh is released. A `legion gh` whose gh never started has nothing that release
	// reaches, so it is killed at once; one whose gh runs ends with gh, and is killed only if it
	// has not ended 10 s before the binary's deadline. Then the cleanup waits for the stand-in
	// itself to be gone, which a killed `legion gh` no longer waits for, so nothing writes into the
	// row's directories while they are removed.
	ended := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		if !ended {
			if _, err := os.Stat(ghPid); err != nil {
				_ = command.Process.Kill()
			}
			select {
			case <-done:
			case <-time.After(testwait.Bounded(t, 1<<60, 10*time.Second)):
				_ = command.Process.Kill()
				<-done
			}
		}
		if pid, err := os.ReadFile(ghPid); err == nil {
			stand := filepath.Join("/proc", strings.TrimSpace(string(pid)))
			testwait.Eventually(t, "the stand-in gh to end", func() bool {
				_, err := os.Stat(stand)
				return err != nil
			})
		}
	})
	testwait.Eventually(t, "the stand-in gh to be running", func() bool {
		_, err := os.Stat(running)
		return err == nil
	})
	t.Run("gh starts with the hangup ignored", func(t *testing.T) {
		requireGhStartedIgnoringHangup(t, ghProbe)
	})
	legion := command.Process.Pid
	t.Run("legion gh ignores the hangup and holds no handler for it before the hangup", func(t *testing.T) {
		requireHangupIgnored(t, legion, "before the hangup")
	})
	// The hangup goes to this one pid, never to the group: gh shares this test binary's own
	// group, and a group signal would end the test run with it.
	if err := command.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	t.Run("legion gh ignores the hangup and holds no handler for it after the hangup", func(t *testing.T) {
		requireHangupIgnored(t, legion, "after the hangup")
	})
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// gh ends within one tick of its release, and `legion gh` with it; the wait has no fixed span,
	// and a `legion gh` that never ends fails the row 30 s before the binary's deadline, which the
	// cleanup's kill then ends.
	var waited error
	select {
	case waited = <-done:
		ended = true
	case <-time.After(testwait.Bounded(t, 1<<60, 30*time.Second)):
		t.Fatalf("legion gh did not end after gh was released, to 30 s before the test binary's deadline: %s", readRecord(t, printed.Name()))
	}
	if waited != nil {
		t.Fatalf("legion gh = %v, want gh's own 0: %s", waited, readRecord(t, printed.Name()))
	}
	if want := "gh survived the hangup\n"; readRecord(t, ghRecord) != want {
		t.Errorf("gh recorded %q, want %q", readRecord(t, ghRecord), want)
	}
}

// ghHangupProbe is the shell text that reads, by behaviour, a shell's disposition of SIGHUP as its
// parent left it: run as `sh -c`, it sends itself the hangup, and exits 0 if it survived it, which
// it does only if the hangup is ignored, or 3 if the kill could not send it, so a kill that fails
// never reads as ignored. A shell that dies of the hangup ends with status 129 as its parent shell
// reads it.
const ghHangupProbe = `kill -HUP $$ || exit 3; exit 0`

// requireProbeDiesAtDefault fails the row unless ghHangupProbe dies of the hangup it sends itself
// when its SIGHUP is at the default: env --default-signal sets that before it runs the probe,
// whatever this test binary inherited, so a probe that survives there could not tell an ignored
// hangup from one that kills.
func requireProbeDiesAtDefault(t *testing.T) {
	t.Helper()
	err := exec.Command("env", "--default-signal=HUP", "sh", "-c", ghHangupProbe).Run()
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("the probe with SIGHUP at its default answered %v, want a death by the hangup it sent itself: the probe cannot tell an ignored hangup from one that kills", err)
	}
	switch status := exited.Sys().(syscall.WaitStatus); {
	case status.Signaled() && status.Signal() == syscall.SIGHUP:
	case status.Exited() && status.ExitStatus() == 3:
		t.Fatal("the probe with SIGHUP at its default could not send itself the hangup, so it read nothing")
	default:
		t.Fatalf("the probe with SIGHUP at its default ended with %v, want a death by the hangup it sent itself", err)
	}
}

// requireGhStartedIgnoringHangup fails the row unless the stand-in gh's probe (ghHangupProbe),
// whose status it wrote at path, survived the hangup it sent itself: the probe is a child of gh,
// so it inherits gh's disposition of SIGHUP, which every kernel acts on, and it survives the
// signal only if gh was started ignoring it. A probe that wrote no status, or could not send the
// hangup, fails the row too: the row then has no answer, not a clean one.
func requireGhStartedIgnoringHangup(t *testing.T, path string) {
	t.Helper()
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the status the stand-in gh's probe wrote: %v", err)
	}
	switch status := strings.TrimSpace(string(written)); status {
	case "0":
	case "":
		t.Fatal("the stand-in gh's probe wrote no status")
	case "3":
		t.Fatal("the stand-in gh's probe could not send itself the hangup, so it read nothing of gh's disposition")
	default:
		t.Fatalf("gh was started with SIGHUP not ignored: a child of gh, which inherits its disposition, ended with status %s when it sent itself the hangup (129 is a death by it): `legion gh` handed a nohuped gh a hangup that ends it", status)
	}
}

// hangupBit is SIGHUP's bit in a /proc status's signal masks.
const hangupBit = uint64(1) << (syscall.SIGHUP - 1)

// requireHangupIgnored fails the row unless process pid ignores SIGHUP and has no handler for it,
// as /proc/<pid>/status reports; when names the moment. It first reads that this kernel's status
// records both masks, and records what a process does with a signal there
// (testkernel.RequireSignalMasks): gVisor's release-20260803.0 records neither, its
// /proc/<pid>/stat gives 0 for the masks of a process that ignores a signal, and no other interface
// reports another process's disposition, so where the kernel has no such record the row follows
// the daemon's rule for a test this machine cannot run, skipping by name outside CI and failing
// under GITHUB_ACTIONS. A process whose status cannot be read, lacks a line the probe found, or
// holds a mask that does not parse, fails the row too: the row then has no answer, not a clean one.
func requireHangupIgnored(t *testing.T, pid int, when string) {
	t.Helper()
	testkernel.RequireSignalMasks(t, "SigIgn", "SigCgt")
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		t.Fatalf("read the signal masks of `legion gh` (pid %d) %s: %v", pid, when, err)
	}
	ignored, ignoredOK := statusSignalMask(t, status, "SigIgn")
	caught, caughtOK := statusSignalMask(t, status, "SigCgt")
	switch {
	case !ignoredOK || !caughtOK:
		t.Fatalf("the status of `legion gh` (pid %d) %s lacks SigIgn or SigCgt, which this kernel's probe found in a shell's:\n%s", pid, when, status)
	case caught&hangupBit != 0:
		t.Fatalf("`legion gh` has a handler for SIGHUP %s (SigCgt %#x): the handler takes the hangup, and may end `legion gh` or forward it to gh at any later moment", when, caught)
	case ignored&hangupBit == 0:
		t.Fatalf("`legion gh` does not ignore SIGHUP %s (SigIgn %#x): the hangup ends it", when, ignored)
	}
}

// statusSignalMask is the mask a /proc status's line name holds, and whether the status holds
// that line; it fails the row where the line does not parse.
func statusSignalMask(t *testing.T, status []byte, name string) (uint64, bool) {
	t.Helper()
	for line := range strings.SplitSeq(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, name+":"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil {
				t.Fatalf("parse the %s mask %q: %v", name, value, err)
			}
			return mask, true
		}
	}
	return 0, false
}

// A signal that reaches `legion gh` before it has armed its own forwarding ends the command
// there, with the status a shell reads a death by that signal as, and gh is never started.
// Until that arming the signal's reader is main's own signal.NotifyContext for a SIGINT or a
// SIGTERM, and hangupCancels for a SIGHUP: each cancels the command's context with a cause naming
// the signal and raises it no further, and the context the child is given has that cancellation
// taken off. Without the read of that cause gh would start and run to the end as if nothing had
// been sent, and `legion gh` would answer with gh's own status.
//
// The window is everything this command does before the child, and here it is held open in the
// guard's read of the repository's visibility, which this command makes through PATH's gh before
// it answers: the `gh` first on PATH is a pane's wrapper whose legion is a script that records
// that it is holding and waits for GH_STANDIN_RELEASE, up to ten seconds, and a tick more once
// that file is there, before it runs `legion gh`. That read reads no gh help of its own
// (TestLegionGhReadsNoHelpForTheGuardsVisibilityReads), so the hold is in the wrapper rather than
// in a help the stand-in answers. The signal lands inside the hold rather than in a race. The
// call is the shared check's own visibility read, the one call the guard never sees, so the
// whole of the held work is this command's own: the check of these runs refuses everything, and
// a run that reached it would exit 2 rather than on the signal.
//
// Each `legion gh` here is this test binary run as `legion`, through main and its cobra command
// line (run, passThrough), so the context runGh is handed is the one main's signal.NotifyContext
// cancels, which endedBySignal reads the cause of. A command line that handed runGh another
// context fails the SIGINT and the SIGTERM rows: they answer the visibility, exit 0, with gh run
// for the read.
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
		{"SIGHUP", syscall.SIGHUP, 129},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := t.TempDir()
			ghRecord, release := filepath.Join(records, "gh"), filepath.Join(records, "release")
			// The stand-in is the prefix alone: it records the visibility read it would answer
			// if it were ever started as gh.
			stub := filepath.Join(t.TempDir(), "gh")
			if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)), 0o700); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			hold := filepath.Join(bin, "legion")
			holding := "#!/bin/sh\nprintf 'legion gh held\\n' >>\"$GH_STANDIN_RECORD\"\nticks=0\n" +
				"while [ ! -f \"$GH_STANDIN_RELEASE\" ] && [ \"$ticks\" -lt 200 ]; do sleep 0.05; ticks=$((ticks+1)); done\n" +
				"sleep 0.05\nexec '" + os.Args[0] + "' \"$@\"\n"
			if err := os.WriteFile(hold, []byte(holding), 0o700); err != nil {
				t.Fatal(err)
			}
			wrappers := t.TempDir()
			if err := os.WriteFile(filepath.Join(wrappers, "gh"), []byte("#!/bin/sh\nexec '"+hold+"' gh \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			command := legionGhCommand(t, t.TempDir(), map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH":                      wrappers + ":" + os.Getenv("PATH"),
				"PUBLIC_LEAK_CHECK":         leakguardtest.Write(t, t.TempDir(), ghCheckRefuses),
				leakguard.VisibilityReadVar: "sjawhar/legion",
				"GH_STANDIN_RECORD":         ghRecord, "GH_STANDIN_RELEASE": release,
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
			testwait.Eventually(t, "the guard's visibility read to be holding", func() bool {
				return strings.Contains(readRecord(t, ghRecord), "legion gh held\n")
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
			if want := "legion gh held\n"; readRecord(t, ghRecord) != want {
				t.Errorf("gh recorded %q, want %q: gh was started for a signal that came before it", readRecord(t, ghRecord), want)
			}
		})
	}
}

// ghHelpStandIn is a stand-in gh that answers a help read of the resolver's, `[<command>]
// --help`, with that command's help from gh 2.98.0's saved walk, and any other run, a help call
// whose words are more than a command's path among them, with a line of help, after recording
// the run in the file GH_STANDIN_RECORD names.
func ghHelpStandIn(t *testing.T) string {
	t.Helper()
	help, err := filepath.Abs(filepath.Join(ghHelpWalkDir, "2.98.0"))
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  if [ -f '" + help + "'/\"${path:-gh}\".txt ]; then exec cat '" + help + "'/\"${path:-gh}\".txt; fi\n" +
		"fi\n" +
		"printf 'gh %s\\n' \"$*\" >>\"$GH_STANDIN_RECORD\"\n" +
		"echo 'the help gh prints'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub
}

// A call that only prints gh's help of a command sends nothing, so `legion gh` hands the guard
// the zero GhSent of it (ghroute's printsOnly), and the guard opens no file its options name.
// With the fixture's rule file named, each help call below passes the whole guard, and gh runs it
// with its arguments untouched and prints its help: `gh release upload` and `gh release create`
// with a tag, and with an asset whose text names the fixture's word as well, and `gh pr comment`
// whose --body-file names a file that is not there, under `--help` and under a `-h` no option of
// the leaf takes. Handed no parse (a nil GhRequest.Sent), the guard reads the argv with tables of
// its own, which open the --body-file and refuse the `gh pr comment` rows as a file they could
// not open. At bd51cbc1 those tables refused the release rows too, as calls that may upload an
// asset no bare argv shows; they no longer refuse a release call, so for the four release rows
// only the check of ghroute.TargetOf's Sent holds a help call to the zero GhSent. The rows stay:
// they are the help rows of c-public-upload. Every command is read from gh 2.98.0's saved walk,
// whose help names `gh release upload`.
func TestLegionGhPassesAHelpCallAndOpensNothing(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	stub := ghHelpStandIn(t)
	asset := writeFile(t, "notes.txt", "the "+leakguardtest.Word+" notes\n")
	absent := filepath.Join(t.TempDir(), "absent.md")
	rules := leakguardtest.WriteRules(t, t.TempDir())
	savedHelp := savedGhHelpOf(t, "2.98.0")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"release upload's help", []string{"release", "upload", "v1", "--help"}},
		{"release create's help", []string{"release", "create", "v1", "--help"}},
		{"release upload's help with an asset", []string{"release", "upload", "v1", asset, "--help"}},
		{"release create's help with an asset", []string{"release", "create", "v1", asset, "--help"}},
		{"pr comment's help with a --body-file that is not there", []string{"pr", "comment", "1", "--body-file", absent, "--help"}},
		{"pr comment's -h with a --body-file that is not there", []string{"pr", "comment", "1", "--body-file", absent, "-h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ghTargetOfKept(t, tc.args, nil, savedHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
			if err != nil || !reflect.DeepEqual(target.Sent, &leakguard.GhSent{}) {
				t.Errorf("ghroute.TargetOf(%q) = %+v, %v; want the target of a call that sends nothing, whose Sent is the zero GhSent", tc.args, target, err)
			}
			records := t.TempDir()
			ghRecord, checkRecord := filepath.Join(records, "gh"), filepath.Join(records, "check")
			code, stdout, stderr := legionGh(t, map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), leakguard.RulesFileVar: rules,
				"GH_STANDIN_RECORD": ghRecord, "LEAK_CHECK_RECORD": checkRecord, "XDG_STATE_HOME": filepath.Join(records, "state"),
			}, tc.args...)
			if code != 0 || stdout != "the help gh prints\n" || stderr != "" {
				t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want gh's help, exit 0, and nothing of the guard's", tc.args, code, stdout, stderr)
			}
			if got, want := readRecord(t, ghRecord), "gh "+strings.Join(tc.args, " ")+"\n"; got != want {
				t.Errorf("gh recorded %q, want its one run of the call, %q", got, want)
			}
			if got, want := readRecord(t, checkRecord), ghGuardRun(tc.args); got != want {
				t.Errorf("the shared check recorded %q, want its one run of the call, %q", got, want)
			}
		})
	}
}

// A -h before the command's name with words after it is refused before gh runs, and the call
// sends nothing (the walk's -h case in ghroute.TargetOf says how gh reads it): `legion gh -h
// status config set -h github.com editor x`, which gh runs as `gh config set --host github.com
// editor x`, exits 2 naming the -h. The stand-in gh (ghHelpStandIn) records no run, so gh ran
// nothing but the resolver's help reads and never `config set`, and the shared check runs
// nothing.
func TestLegionGhRefusesAHelpFlagBeforeTheCommandAndRunsNothing(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	args := []string{"-h", "status", "config", "set", "-h", "github.com", "editor", "x"}
	records := t.TempDir()
	ghRecord, checkRecord := filepath.Join(records, "gh"), filepath.Join(records, "check")
	code, stdout, stderr := legionGh(t, map[string]string{
		"LEGION_GH_PATH": ghHelpStandIn(t), "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
		"GH_STANDIN_RECORD": ghRecord, "LEAK_CHECK_RECORD": checkRecord, "XDG_STATE_HOME": filepath.Join(records, "state"),
	}, args...)
	if want := `"-h" comes before the command's name`; code != 2 || stdout != "" || !strings.Contains(stderr, want) {
		t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 and a refusal containing %q", args, code, stdout, stderr, want)
	}
	if got := readRecord(t, ghRecord); got != "" {
		t.Errorf("gh recorded %q, want no run but the resolver's help reads", got)
	}
	if got := readRecord(t, checkRecord); got != "" {
		t.Errorf("the shared check recorded %q, want no run", got)
	}
}

// A release asset is handled as on main: the private-name rule pass opens no positional asset
// file of `gh release upload` or `gh release create`, and reads each argument as written. With
// the fixture's rule file named, the guard, handed the parse `legion gh` hands it of each call
// (ghroute.TargetOf, at gh 2.98.0, as leakguard.GhRequest.Sent), passes an upload and a create
// to a public repository of a file whose text names the fixture's word and whose argument holds
// no label. The same upload of a clean file with a `#label` naming the word is refused, the
// report naming the argument that holds the label.
func TestGhGuardHandlesAReleaseAssetAsMainDoes(t *testing.T) {
	help := savedGhHelpOf(t, "2.98.0")
	rules := leakguardtest.WriteRules(t, t.TempDir())
	asset := writeFile(t, "notes.txt", "the "+leakguardtest.Word+" notes\n")
	clean := writeFile(t, "clean.txt", "a clean asset\n")
	// call is the guard's answer to the call args, to a repository GitHub reports public, as
	// runGh hands it: the repository and the parse ghroute.TargetOf answers.
	call := func(t *testing.T, args ...string) error {
		t.Helper()
		target, err := ghTargetOfKept(t, args, nil, help, noGitRepository, ghroutetest.OwnersOwnDirectories)
		if err != nil {
			t.Fatalf("ghroute.TargetOf(%q) = %v", args, err)
		}
		guard := leakguard.New(leakguard.Options{
			Check:      leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
			Visibility: func(context.Context, string) (leakguard.Visibility, error) { return leakguard.Public, nil },
			Env:        append(fixtureenv.Isolated(t.TempDir()), leakguard.RulesFileVar+"="+rules),
		})
		_, err = guard.GhCall(context.Background(), leakguard.GhRequest{Args: args, Repo: target.Owner + "/" + target.Repo, Sent: target.Sent})
		return err
	}
	for _, args := range [][]string{
		{"release", "upload", "v1", asset, "-R", "acme/widgets"},
		{"release", "create", "v1", asset, "--notes", "the notes of a clean release", "-R", "acme/widgets"},
	} {
		t.Run(strings.Join(args[:2], " ")+" of an asset whose text names the word", func(t *testing.T) {
			if err := call(t, args...); err != nil {
				t.Errorf("the guard answered %q with %v, want it cleared", args, err)
			}
		})
	}
	t.Run("release upload of an asset whose label names the word", func(t *testing.T) {
		args := []string{"release", "upload", "v1", clean + "#the " + leakguardtest.Word + " notes", "-R", "acme/widgets"}
		err := call(t, args...)
		var refused *leakguard.Refusal
		want := "the gh call's argument 3: " + leakguardtest.WordClass + ": matched "
		if !errors.As(err, &refused) || !slices.ContainsFunc(refused.Lines, func(line string) bool { return strings.HasPrefix(line, want) }) {
			t.Errorf("the guard answered %q with %v, want a refusal with a line starting %q", args, err, want)
		}
	})
}

// A gh directory whose hosts.yml or config.yml sends gh's requests elsewhere is refused, naming
// the file and the key and never the value: api_host in a host's block, http_unix_socket at the
// top of config.yml or in a host's block, at any depth and in any shape gh might read it as a key,
// a list's even-indexed items among them (go-gh's FindEntry reads a list as pairs). So is a
// config.yml holding any other hosts block or an alias named `help`, and a file that is no regular
// file (a named pipe, a symbolic link, a directory) or is larger than any the daemon writes, which
// is refused without being read, so a named pipe stalls nothing. The files the daemon renders, a
// config.yml holding aliases, one naming an alias after a command `legion gh` refuses (which gh
// never expands), a routing key's name as a value, and a directory holding neither file pass; one
// that does not parse is refused.
func TestGhConfigShapeRefusesADirectoryThatSendsElsewhere(t *testing.T) {
	hosts := ghconfig.Hosts("ghs_acme_write")
	fifo := func(dir, name string) error { return syscall.Mkfifo(filepath.Join(dir, name), 0o600) }
	link := func(text string) func(dir, name string) error {
		return func(dir, name string) error {
			target := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(target, []byte(text), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dir, name))
		}
	}
	directory := func(dir, name string) error { return os.Mkdir(filepath.Join(dir, name), 0o700) }
	large := func(dir, name string) error {
		return os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("#", ghconfig.MaxFileBytes+1)), 0o600)
	}
	at := func(plant func(dir, name string) error, name string) func(dir string) error {
		return func(dir string) error { return plant(dir, name) }
	}
	for _, tc := range []struct {
		name          string
		hosts, config string // "" writes no such file
		plant         func(dir string) error
		want          string // "" for a directory that passes
	}{
		{name: "the files the daemon renders", hosts: hosts, config: ghconfig.Config},
		{name: "a config.yml holding aliases", hosts: hosts, config: ghconfig.Config + "aliases:\n    co: pr checkout\n"},
		{name: "an alias named after a refused command", hosts: hosts, config: ghconfig.Config + "aliases:\n    agent-task: pr view\n"},
		{name: "api_host as a list's value", hosts: "github.com: [user, api_host]\n", config: ghconfig.Config},
		{name: "neither file"},
		{name: "api_host in hosts.yml's github.com block", hosts: hosts + "    api_host: api.example.invalid\n", config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "api_host under the account", hosts: strings.Replace(hosts, "            oauth_token: ghs_acme_write\n", "            oauth_token: ghs_acme_write\n            api_host: api.example.invalid\n", 1), config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "http_unix_socket at the top of config.yml", hosts: hosts, config: ghconfig.Config + "http_unix_socket: /tmp/example.sock\n", want: "config.yml sets http_unix_socket"},
		{name: "api_host in a flow list of hosts.yml", hosts: "github.com: [oauth_token, ghs_acme_write, user, x-access-token, api_host, api.example.invalid]\n", config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "api_host in a block list of hosts.yml", hosts: "github.com:\n    - api_host\n    - api.example.invalid\n", config: ghconfig.Config, want: "hosts.yml sets api_host"},
		{name: "config.yml as a list", hosts: hosts, config: "[http_unix_socket, /tmp/example.sock]\n", want: "config.yml sets http_unix_socket"},
		{name: "http_unix_socket in a host's block of config.yml", hosts: hosts, config: ghconfig.Config + "hosts:\n    github.com:\n        http_unix_socket: /tmp/example.sock\n", want: "config.yml sets http_unix_socket"},
		{name: "any other hosts block in config.yml", hosts: hosts, config: ghconfig.Config + "hosts:\n    github.com:\n        editor: vi\n", want: "config.yml holds a hosts block"},
		{name: "an alias named help", hosts: hosts, config: ghconfig.Config + "aliases:\n    help: api repos/sjawhar/legion\n", want: "config.yml names an alias `help`"},
		{name: "a hosts.yml that does not parse", hosts: "github.com: [\n", config: ghconfig.Config, want: "hosts.yml does not parse as YAML"},
		{name: "a named pipe at hosts.yml", config: ghconfig.Config, plant: at(fifo, ghconfig.HostsFile), want: "hosts.yml is no regular file"},
		{name: "a named pipe at config.yml", hosts: hosts, plant: at(fifo, ghconfig.ConfigFile), want: "config.yml is no regular file"},
		{name: "a symbolic link at hosts.yml", config: ghconfig.Config, plant: at(link(hosts), ghconfig.HostsFile), want: "hosts.yml is no regular file"},
		{name: "a symbolic link at config.yml", hosts: hosts, plant: at(link(ghconfig.Config), ghconfig.ConfigFile), want: "config.yml is no regular file"},
		{name: "a directory at hosts.yml", config: ghconfig.Config, plant: at(directory, ghconfig.HostsFile), want: "hosts.yml is no regular file"},
		{name: "a directory at config.yml", hosts: hosts, plant: at(directory, ghconfig.ConfigFile), want: "config.yml is no regular file"},
		{name: "a hosts.yml larger than any the daemon writes", config: ghconfig.Config, plant: at(large, ghconfig.HostsFile), want: "hosts.yml is larger than any file the daemon writes"},
		{name: "a config.yml larger than any the daemon writes", hosts: hosts, plant: at(large, ghconfig.ConfigFile), want: "config.yml is larger than any file the daemon writes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, text := range map[string]string{ghconfig.HostsFile: tc.hosts, ghconfig.ConfigFile: tc.config} {
				if text != "" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.plant != nil {
				if err := tc.plant(dir); err != nil {
					t.Fatal(err)
				}
			}
			// A check that opened a planted named pipe plainly would wait there for a writer. Its
			// answer is waited for with testwait.Receive, as main waits for it, the only bound on the
			// wait. Beside that wait, the fifo watch reads from the kernel whether the check opens a
			// planted pipe plainly (testfifo.WatchBeside), and its own open for writing is the
			// writer that open needed, so the check then answers and the case fails naming the pipe
			// at once. The correct check opens each file with O_NONBLOCK and holds it for a moment,
			// which is no such wait. Only a case that planted a pipe needs the kernel's record, and
			// where the kernel keeps none, as gVisor keeps none, such a case fails under CI and skips
			// elsewhere (testfifo.RequireWatch).
			var pipes []string
			for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
				path := filepath.Join(dir, name)
				if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
					pipes = append(pipes, path)
				}
			}
			stopWatch := func() (string, error) { return "", nil }
			if len(pipes) > 0 {
				testfifo.RequireWatch(t)
				stopWatch = testfifo.WatchBeside(nil, pipes...)
			}
			defer func() { _, _ = stopWatch() }()
			done := make(chan error, 1)
			go func() { done <- ghConfigShape(dir) }()
			err := testwait.Receive(t, "ghConfigShape with "+tc.name, done)
			if opened, watchErr := stopWatch(); watchErr != nil {
				t.Fatalf("cannot tell whether ghConfigShape waits in a plain open of a planted pipe: %v", watchErr)
			} else if opened != "" {
				t.Fatalf("ghConfigShape opened the named pipe at %s plainly and waited there for a writer", filepath.Base(opened))
			}
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("ghConfigShape = %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("ghConfigShape = %v, want a refusal containing %q", err, tc.want)
			case err != nil && (strings.Contains(err.Error(), "example.invalid") || strings.Contains(err.Error(), "example.sock") || strings.Contains(err.Error(), "vi\n")):
				t.Fatalf("ghConfigShape's refusal %q quotes the value", err)
			}
		})
	}
}

// The git gh runs for a push makes the pre-push hook, whose guard resolves the check from
// PUBLIC_LEAK_CHECK and holds it to the deployment's checker (leakguard.CheckerVar), so gh's
// environment keeps both of its caller's: without the checker, that guard would run a check whose
// program hashes otherwise, which the role's own guard refuses.
func TestGhsChildEnvironmentKeepsTheLeakCheckAndItsChecker(t *testing.T) {
	checker := strings.Repeat("5e", 32)
	caller := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		leakguard.CheckVar + "=/opt/check/public-leak-check.sh", leakguard.CheckerVar + "=" + checker}
	kept := map[string]string{}
	for _, entry := range ghChildEnviron(caller, nil) {
		if name, value, ok := strings.Cut(entry, "="); ok && (name == leakguard.CheckVar || name == leakguard.CheckerVar) {
			kept[name] = value
		}
	}
	if kept[leakguard.CheckVar] != "/opt/check/public-leak-check.sh" || kept[leakguard.CheckerVar] != checker {
		t.Fatalf("gh's environment kept %v of the leak check, want both of its caller's", kept)
	}
}
