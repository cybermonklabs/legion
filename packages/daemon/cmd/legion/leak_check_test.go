package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// noSHA is git's "this ref is not there", which a pre-push line carries for the remote side of a
// ref the push creates.
const noSHA = "0000000000000000000000000000000000000000"

// noSharedCheck leaves the command no check to resolve at all: nothing in PUBLIC_LEAK_CHECK, no
// public-leak-check.sh on any PATH entry, and no pointer file under a configuration home of its
// own. checker is the deployment's checker the command's environment names
// (leakguard.CheckerVar), as the runtime tells a role of a deployment that runs a shared check,
// and "" names none, a role of a deployment that runs no shared check. The PATH it leaves holds
// git alone, which the guard reads a push's commits with where no check refuses the push first.
// Every fixture a case needs is built before this, since it replaces the PATH a child of the test
// would be found on.
func noSharedCheck(t *testing.T, checker string) {
	t.Helper()
	fixtureenv.Unset(t, "PUBLIC_LEAK_CHECK")
	if checker == "" {
		fixtureenv.Unset(t, leakguard.CheckerVar)
	} else {
		t.Setenv(leakguard.CheckerVar, checker)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// theChecker is the deployment's checker these tests name where a case runs under a deployment
// that names a shared check: a stand-in sha256, since no case here runs the check whose program
// it would name.
var theChecker = strings.Repeat("5e", 32)

// aPushFixture is a repository holding one commit whose message is message, and the line git
// hands a pre-push hook for a push of it to a remote that holds the ref yet.
func aPushFixture(t *testing.T, message string) (dir, line string) {
	t.Helper()
	dir = t.TempDir()
	fixture := fixturegit.Isolated(t, t.TempDir())
	fixture.Run(dir, fixturegit.InitWorkTree("", "main")...)
	fixture.Run(dir, "git", "commit", "--allow-empty", "--quiet", "-m", message)
	sha := fixture.Run(dir, "git", "rev-parse", "HEAD")
	return dir, "refs/heads/main " + sha + " refs/heads/main " + noSHA + "\n"
}

// hostURL is the URL a row hands the hook where the guard has to run on the push. The hook clears
// a push that can only write to a local path (leakguard's Guard.StaysLocal), so a row that named
// one would read that clearing and never reach the guard. This URL's host names no GitHub
// repository, so the guard reads no visibility for it and starts no gh, and scans the push as
// public, as it scanned a push to a local path before the hook cleared one.
const hostURL = "https://example.invalid/o/n.git"

// leakCheck runs the command as git's hook and a pod's scripts run it: stdin is what they pipe
// in, and the status and what reached stderr are what they read. Standard output is the hook's
// own, and the command writes nothing to it. The command's context ends when what is left of the
// test binary's deadline less testBoundMargin has passed, and a command still running then fails
// the test by name: a check that a loaded machine makes slow passes. A fixed minute ended a slow
// check, and the row read the guard's setup failure that followed.
func leakCheck(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	limit := testwait.Bounded(t, 1<<60, testBoundMargin)
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	t.Cleanup(cancel)
	previous := leakCheckStdin
	t.Cleanup(func() { leakCheckStdin = previous })
	leakCheckStdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	code := run(ctx, append([]string{"legion", "leak-check"}, args...), &stdout, &stderr)
	if ctx.Err() != nil {
		t.Fatalf("legion leak-check %q still ran when its limit of %s, %s before the test binary's deadline, passed (exit %d): %s", args, limit.Round(time.Millisecond), testBoundMargin, code, stderr.String())
	}
	if stdout.Len() > 0 {
		t.Errorf("printed %q on standard output, want nothing", stdout.String())
	}
	return code, stderr.String()
}

// says fails the test unless printed holds every one of wants, and unless it is empty where
// there is nothing to want.
func says(t *testing.T, printed string, wants []string) {
	t.Helper()
	if len(wants) == 0 {
		if printed != "" {
			t.Errorf("printed %q, want nothing", printed)
		}
		return
	}
	for _, want := range wants {
		if !strings.Contains(printed, want) {
			t.Errorf("printed %q, want it to hold %q", printed, want)
		}
	}
}

// `legion leak-check pre-push` is what the image's hook runs: git's update lines on standard
// input, the repository it ran the hook in as the working directory, and the remote and its URL
// as its arguments. With the deployment's rule file named (here the fixture's, through
// LEGION_PRIVATE_NAME_RULES_FILE), either check refuses the push, and the statuses are the shared
// check's own — 0 clean, 3 refused, 2 a check that could not be made to run. Under a deployment
// that names no shared check (no leakguard.CheckerVar) a push with no check is not refused for its
// absence, and the deployment's rules still read it.
func TestLeakCheckPrePushRunsTheGuardOverGitsUpdateLines(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variable leakCheckStdin (leakCheck).
	for _, tc := range []struct {
		name    string
		message string
		// standIn is the stand-in shared check's body, and "" is a call with no check at all,
		// under checker, the deployment's checker the call is told ("" for none).
		standIn, checker string
		code             int
		says             []string
	}{
		{
			name:    "the shared check refuses the push",
			message: "a commit of nothing in particular",
			standIn: leakguardtest.Refuses,
			code:    3,
			says:    []string{"LEAK CHECK: refusing the push to origin: the shared public leak check refused it", "one of ours"},
		},
		{
			name:    "the deployment's rules refuse the push",
			message: "a commit that names " + leakguardtest.RulesHost,
			standIn: leakguardtest.Passes,
			code:    3,
			says:    []string{"LEAK CHECK: refusing the push to origin: the deployment's private-name rules match it", leakguardtest.RulesHostClass},
		},
		{
			name:    "a clean push",
			message: "a commit of nothing in particular",
			standIn: leakguardtest.Passes,
			code:    0,
		},
		{
			name:    "no shared check resolves under a deployment that names one",
			message: "a commit of nothing in particular",
			checker: theChecker,
			code:    2,
			says:    []string{"LEAK CHECK SETUP ERROR: the push to origin: the shared public leak check could not be found"},
		},
		{
			name:    "no shared check under a deployment that names none",
			message: "a commit of nothing in particular",
			code:    0,
		},
		{
			name:    "the deployment's rules refuse the push under a deployment that names no check",
			message: "a commit that names " + leakguardtest.RulesHost,
			code:    3,
			says:    []string{"LEAK CHECK: refusing the push to origin: the deployment's private-name rules match it", leakguardtest.RulesHostClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, line := aPushFixture(t, tc.message)
			// git runs its hook in the repository the push is made from, and that working
			// directory is where the command reads the push's commits from.
			t.Chdir(dir)
			if tc.standIn == "" {
				noSharedCheck(t, tc.checker)
			} else {
				t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), tc.standIn))
			}
			t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
			// The host URL, since the hook would clear a push to a local path (hostURL).
			code, printed := leakCheck(t, line, "pre-push", "origin", hostURL)
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, printed)
			}
			says(t, printed, tc.says)
		})
	}
}

func legionLeakCheckPrePush(t *testing.T, dir string, env map[string]string, line, url string) (int, string) {
	t.Helper()
	cmd := legionCommand(t, dir, env, "leak-check", "pre-push", "origin", url)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(line), &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("legion leak-check pre-push origin %s: %v", url, err)
		}
		code = exit.ExitCode()
	}
	if stdout.Len() > 0 {
		t.Errorf("printed %q on standard output, want nothing", stdout.String())
	}
	return code, stderr.String()
}

// A push a real gh makes — `gh repo create --source=. --push` — runs git's pre-push hook, `legion
// leak-check pre-push`, under the variable `legion gh` gives every real gh it runs
// (leakguard.RunByRealGhVar), since gh's git inherits it. With a rule file named, the guard reads
// the destination's visibility with `gh`, which in a pane or pod is `legion gh`, and it blanks the
// variable for that read, so the read reaches the real gh and the push is cleared. Under the
// variable the read would be refused as a real gh leading back to `legion gh`, and the push with
// it, leaving the repository gh created empty. The command runs as its own process, so the
// fixture's rule file is named in that process's environment.
func TestLeakCheckPrePushUnderTheRealGhsVariableReadsTheVisibility(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := execfile.Write(stub, []byte(ghStandInPrefix(help)+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "gh")
	dir, line := aPushFixture(t, "a commit of nothing in particular")
	code, printed := legionLeakCheckPrePush(t, dir, map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		"GH_STANDIN_RECORD": record, leakguard.RunByRealGhVar: stub,
		leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
	}, line, "https://github.com/acme/widgets.git")
	if code != leakguard.StatusClean || printed != "" {
		t.Errorf("legion leak-check pre-push under %s = exit %d, stderr %q; want exit 0 and nothing printed", leakguard.RunByRealGhVar, code, printed)
	}
	if want := "visibility " + strings.Join(leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), " ") + "\n"; readRecord(t, record) != want {
		t.Errorf("gh recorded %q, want the guard's REST read, %q", readRecord(t, record), want)
	}
}

// git runs its pre-push hook, `legion leak-check pre-push`, in the terminal's foreground process
// group with git, and the guard runs the shared check in a process group of its own, which none
// of the terminal's signals reach; `legion leak-check text` runs its check the same way. A SIGINT
// to the command's group cancels its context (main's signal.NotifyContext), and a SIGHUP, a pane
// that closes during the push, is caught while the command runs (hangupCancels) and cancels it the
// same way; either stops the check's group, and the command ends with 128+<signal>, which refuses
// the write, and prints nothing (leakCheckReport). The command is started here as the leader of a
// group of its own, and the stand-in check holds the write (signalWhileHeld).
//
// Before, a SIGHUP killed the command where it stood and left the check running, reparented and
// waited for by nobody, and a SIGINT stopped the check and was answered with the guard's setup
// error, exit 2.
//
// The command is this test binary run as `legion`, through main and its cobra command line, so
// the guard's context is the one main's signal.NotifyContext cancels: a leakCheckCommand that
// handed its shape another context leaves each SIGINT row's command running until the row gives up
// on it, by name, before the test binary's deadline (signalWhileHeld).
func TestASignalToLeakChecksGroupStopsTheSharedCheck(t *testing.T) {
	t.Parallel()
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether the stand-in is still running", err)
	}
	dir, line := aPushFixture(t, "a commit of nothing in particular")
	for _, shape := range []struct {
		name  string
		stdin string
		// args is the command after `legion`. The push names the host URL, since the hook clears
		// a push to a local path (hostURL), and the host URL names no GitHub repository, so the
		// push's guard reads no visibility and starts no gh, and the write is held in the check,
		// the guard's first child.
		args []string
		// seconds is the holding stand-in's sleep for each signal, one per row (holdsStandIn).
		seconds map[syscall.Signal]string
	}{
		{
			name: "leak-check pre-push", stdin: line, args: []string{"leak-check", "pre-push", "origin", hostURL},
			seconds: map[syscall.Signal]string{syscall.SIGINT: "86393", syscall.SIGHUP: "86394"},
		},
		{
			name: "leak-check text", stdin: "a title\na body of nothing in particular\n", args: []string{"leak-check", "text", "the pull request body"},
			seconds: map[syscall.Signal]string{syscall.SIGINT: "86391", syscall.SIGHUP: "86392"},
		},
	} {
		for _, tc := range []struct {
			name   string
			signal syscall.Signal
		}{
			{"SIGINT", syscall.SIGINT},
			{"SIGHUP", syscall.SIGHUP},
		} {
			t.Run(shape.name+" "+tc.name, func(t *testing.T) {
				seconds := shape.seconds[tc.signal]
				pidFile := filepath.Join(t.TempDir(), "stand-in")
				command := legionCommand(t, dir, map[string]string{
					"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), holdsStandIn(pidFile, seconds)),
				}, shape.args...)
				command.Stdin = strings.NewReader(shape.stdin)
				signalWhileHeld(t, command, "legion "+shape.name, pidFile, seconds, "the shared check", tc.signal)
			})
		}
	}
}

// `legion leak-check text` is the shape anything about to publish a piece of text runs: the text
// on standard input, and the kind it is called by in the refusal, which is the one thing the
// caller knows better than the guard does. The deployment's rule file is the fixture's, named
// through LEGION_PRIVATE_NAME_RULES_FILE. Under a deployment that names no shared check (no
// leakguard.CheckerVar) a text with no check is not refused for its absence, and the deployment's
// rules still read it.
func TestLeakCheckTextRunsTheGuardOverWhatItIsPiped(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable leakCheckStdin (leakCheck).
	const kind = "the pull request body"
	for _, tc := range []struct {
		name, text string
		// standIn is the stand-in shared check's body, and "" is a call with no check at all,
		// under checker, the deployment's checker the call is told ("" for none).
		standIn, checker string
		code             int
		says             []string
	}{
		{
			name:    "the shared check refuses the text",
			text:    "a title\na body of nothing in particular\n",
			standIn: leakguardtest.Refuses,
			code:    3,
			says:    []string{"LEAK CHECK: refusing " + kind + ": the shared public leak check refused it", "one of ours"},
		},
		{
			name:    "the deployment's rules refuse the text",
			text:    "a title\na body that names " + leakguardtest.RulesHost + "\n",
			standIn: leakguardtest.Passes,
			code:    3,
			says:    []string{"LEAK CHECK: refusing " + kind + ": the deployment's private-name rules match it", leakguardtest.RulesHostClass},
		},
		{
			name:    "a clean text",
			text:    "a title\na body of nothing in particular\n",
			standIn: leakguardtest.Passes,
			code:    0,
		},
		{
			name:    "no shared check resolves under a deployment that names one",
			text:    "a title\na body of nothing in particular\n",
			checker: theChecker,
			code:    2,
			says:    []string{"LEAK CHECK SETUP ERROR: " + kind + ": the shared public leak check could not be found"},
		},
		{
			name: "no shared check under a deployment that names none",
			text: "a title\na body of nothing in particular\n",
			code: 0,
		},
		{
			name: "the deployment's rules refuse the text under a deployment that names no check",
			text: "a title\na body that names " + leakguardtest.RulesHost + "\n",
			code: 3,
			says: []string{"LEAK CHECK: refusing " + kind + ": the deployment's private-name rules match it", leakguardtest.RulesHostClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.standIn == "" {
				noSharedCheck(t, tc.checker)
			} else {
				t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), tc.standIn))
			}
			t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
			code, printed := leakCheck(t, tc.text, "text", kind)
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, printed)
			}
			says(t, printed, tc.says)
		})
	}
}

// A push the deployment's rules refuse shows, on standard error, what matched, so the agent that
// wrote the commit sees what to change: the record, the commit's message; the description of the
// rule; the text the rule matched, the fixture's host and the space after it; and the line of the
// message it stands on, its third, whole. The report is the whole of what the command prints.
func TestLeakCheckPrePushRefusalShowsMatch(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variable leakCheckStdin (leakCheck).
	const line = "moved the run to " + leakguardtest.RulesHost + " this morning"
	dir, update := aPushFixture(t, "a subject of no interest\n\n"+line)
	t.Chdir(dir)
	t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes))
	t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
	code, printed := leakCheck(t, update, "pre-push", "origin", hostURL)
	commit := strings.Fields(update)[1][:7]
	want := "LEAK CHECK: refusing the push to origin: the deployment's private-name rules match it\n" +
		"  " + leakguardtest.ReportLine("commit "+commit+" message", leakguardtest.RulesHostClass, leakguardtest.RulesHost+" ", 3, line) + "\n"
	if code != 3 || printed != want {
		t.Errorf("legion leak-check pre-push = exit %d, stderr %q; want exit 3 and %q", code, printed, want)
	}
}

// A text the deployment's rules refuse shows what matched the same way: the record, the body;
// the rule's description; the fixture's host and the space after it; and the body's second
// line, on which it stands.
func TestLeakCheckTextRefusalShowsMatch(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it sets the package variable leakCheckStdin (leakCheck).
	const (
		kind = "the pull request body"
		line = "the runner answers at " + leakguardtest.RulesHost + " today"
	)
	t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes))
	t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
	code, printed := leakCheck(t, "a title of no interest\na first line of no interest\n"+line+"\n", "text", kind)
	want := "LEAK CHECK: refusing " + kind + ": the deployment's private-name rules match it\n" +
		"  " + leakguardtest.ReportLine("the body", leakguardtest.RulesHostClass, leakguardtest.RulesHost+" ", 2, line) + "\n"
	if code != 3 || printed != want {
		t.Errorf("legion leak-check text = exit %d, stderr %q; want exit 3 and %q", code, printed, want)
	}
}

// With no rule file named — LEGION_PRIVATE_NAME_RULES_FILE unset, as in a pod whose launch handed
// it none — the guard runs the shared check alone, so a write naming the fixture's host, which the
// fixture's rules refuse, is cleared: exit 0, and standard error holds nothing at all. The command
// says nothing of the rule it did not run: a script that reads what it prints reads the guard's
// answer alone, and the daemon's boot has said once, in its own log, that the deployment names no
// rule file.
func TestLeakCheckWithNoRuleFileRunsNoRuleSilently(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variable leakCheckStdin (leakCheck).
	t.Run("pre-push", func(t *testing.T) {
		dir, update := aPushFixture(t, "moved the run to "+leakguardtest.RulesHost+" this morning")
		t.Chdir(dir)
		t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes))
		fixtureenv.Unset(t, leakguard.RulesFileVar)
		// The host URL, since at a local path the hook would clear the push before any rule could
		// run, and this row would pass whatever the guard did with no rule file (hostURL).
		if code, printed := leakCheck(t, update, "pre-push", "origin", hostURL); code != 0 || printed != "" {
			t.Errorf("legion leak-check pre-push with no rule file = exit %d, stderr %q; want exit 0 and nothing printed", code, printed)
		}
	})
	t.Run("text", func(t *testing.T) {
		t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes))
		fixtureenv.Unset(t, leakguard.RulesFileVar)
		if code, printed := leakCheck(t, "a title\nthe runner answers at "+leakguardtest.RulesHost+" today\n", "text", "the pull request body"); code != 0 || printed != "" {
			t.Errorf("legion leak-check text with no rule file = exit %d, stderr %q; want exit 0 and nothing printed", code, printed)
		}
	})
}

// A call the command cannot read is a usage error, exit 2, and no write is checked by it: a hook
// or a script that called it wrongly has cleared nothing. A help request is the one call that
// prints the usage, on standard output, and exits 0.
func TestLeakCheckRefusesACallItCannotRead(t *testing.T) {
	// Not parallel: it sets the package variable leakCheckStdin (leakCheck).
	for _, tc := range []struct {
		name string
		args []string
		code int
		says string
	}{
		{name: "no shape at all", code: 2, says: "Usage:\n  legion leak-check [flags]\n"},
		{name: "a shape it does not know", args: []string{"pre-receive"}, code: 2, says: `legion leak-check: unknown command "pre-receive" for "legion leak-check"`},
		{name: "a pre-push with no url", args: []string{"pre-push", "origin"}, code: 2, says: leakCheckPrePushUsage},
		{name: "a pre-push with a third argument", args: []string{"pre-push", "origin", "url", "extra"}, code: 2, says: leakCheckPrePushUsage},
		{name: "a text with no kind", args: []string{"text"}, code: 2, says: leakCheckTextUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, printed := leakCheck(t, "", tc.args...)
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, printed)
			}
			says(t, printed, []string{tc.says})
		})
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "leak-check", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Usage:\n  legion leak-check [flags]\n") {
		t.Errorf("legion leak-check --help = %d, stdout %q, stderr %q; want exit 0 and its usage", code, out.String(), errb.String())
	}
}

// git hands a pre-push hook the remote's URL as its name too where the push names a URL rather
// than a remote (`git push https://<token>@github.com/acme/no-such-repo.git`), and such a URL can
// carry a token as its userinfo, with no password. Nothing the command prints holds the token, on
// any of its answers: a call it cannot read, update lines it cannot read, a refusal by either
// check, and GitHub's 404 for the destination. Each row also holds its answer's own words, the
// push named by its URL with no credential, so a row that took another path, or printed the
// push's name some other way, fails too. Two rules stand between the token and the terminal, each
// answer's own Error() (leakguard's errors.go) and the URL rule on every command's stderr
// (newRoot), and these rows hold the two together; leakguard's
// TestNoAnswerPrintsACredentialThePushsNameCarries holds the first alone.
func TestLeakCheckPrePushPrintsNoCredentialItsRemoteCarries(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variables forkReadBound (pushVisibilityReadEnv), leakCheckStdin (leakCheck) and leakguardCurl (pushVisibilityReadEnv).
	const token = "FAKEtok3nZQ9x"
	remote := "https://" + token + "@github.com/" + noSuchRepo + ".git"
	shown := "the push to https://" + redact.URLWithCredentials + "/" + noSuchRepo + ".git"
	root, defaultDir := gitCredentialBundle(t)
	refusedPush := func(t *testing.T, check, message string) (int, string) {
		dir, line := aPushFixture(t, message)
		t.Chdir(dir)
		t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), check))
		t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
		// The host URL, since the hook would clear a push to a local path (hostURL).
		return leakCheck(t, line, "pre-push", remote, hostURL)
	}
	var ghNotFound visibilityReadCase
	for _, read := range visibilityReadCases(shown) {
		if read.name == "gh's 404" {
			ghNotFound = read
		}
	}
	if ghNotFound.gh == nil {
		t.Fatal(`visibilityReadCases holds no "gh's 404" row`)
	}
	for _, tc := range []struct {
		name string
		code int
		says string
		run  func(t *testing.T) (int, string)
	}{
		{
			name: "a call it cannot read", code: leakguard.StatusSetupFailure, says: leakCheckPrePushUsage,
			run: func(t *testing.T) (int, string) { return leakCheck(t, "", "pre-push", remote) },
		},
		{
			name: "update lines it cannot read", code: leakguard.StatusSetupFailure,
			says: "LEAK CHECK SETUP ERROR: " + shown + ": git's update lines could not be read",
			run:  func(t *testing.T) (int, string) { return leakCheck(t, "a b c\n", "pre-push", remote, remote) },
		},
		{
			name: "the shared check refuses", code: leakguard.StatusRefused,
			says: "LEAK CHECK: refusing " + shown + ": the shared public leak check refused it",
			run: func(t *testing.T) (int, string) {
				return refusedPush(t, leakguardtest.Refuses, "a commit of nothing in particular")
			},
		},
		{
			name: "the deployment's rules refuse", code: leakguard.StatusRefused,
			says: "LEAK CHECK: refusing " + shown + ": the deployment's private-name rules match it",
			run: func(t *testing.T) (int, string) {
				return refusedPush(t, leakguardtest.Passes, "a commit that names "+leakguardtest.RulesHost)
			},
		},
		{
			name: "GitHub answers 404", code: leakguard.StatusSetupFailure, says: notFoundSays(shown),
			run: func(t *testing.T) (int, string) {
				pushVisibilityReadEnv(t, root, defaultDir, ghNotFound)
				return leakCheck(t, refusedRefDeletion, "pre-push", remote, remote)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, printed := tc.run(t)
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, strings.ReplaceAll(printed, token, "<the token>"))
			}
			if strings.Contains(printed, token) {
				t.Errorf("printed the token the remote carries: %s", strings.ReplaceAll(printed, token, "<the token>"))
			}
			if !strings.Contains(printed, tc.says) {
				t.Errorf("printed %q, want it to hold %q", strings.ReplaceAll(printed, token, "<the token>"), tc.says)
			}
		})
	}
}

// `legion leak-check pre-push` to a repository whose owner the gh bundle holds no directory for:
// the guard's REST read goes through the pod's `gh`, `legion gh`, which refuses it, exit 2, naming
// the owner, and the real gh never runs. The guard then reads the visibility with git's
// ls-remote and no credential, once. Where ls-remote lists the refs, the repository is public and
// the push goes on to the scan, which refuses this push's ref name, exit 3. Where GitHub asks
// ls-remote for a credential, as it does for a private repository, the push is refused as a setup
// failure naming both reads, exit 2. A push to a repository of an owner the bundle holds is read
// by the real gh, as before, and git never runs. The fixture rule file is named
// (LEGION_PRIVATE_NAME_RULES_FILE): with no rule file the guard reads no destination's visibility.
func TestLeakCheckPrePushReadsAnUnservedOwnersVisibilityWithNoCredential(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variable leakCheckStdin (leakCheck).
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	lists := "printf '%s\\tHEAD\\n' " + strings.Repeat("1", 40) + "\nexit 0\n"
	refused := "echo 'fatal: could not read Username for '\\''https://github.com'\\'': terminal prompts disabled' >&2\nexit 128\n"
	for _, tc := range []struct {
		name, repo, git string
		// code and says are the command's exit status and what it prints; reads is what the
		// stand-ins recorded, in order: rest for the real gh's read, git for ls-remote.
		code  int
		says  []string
		reads string
	}{
		{
			name: "an unserved owner's repository ls-remote lists", repo: "other-owner/invented-repo", git: lists,
			code: 3, says: []string{"LEAK CHECK: refusing the push to origin: the deployment's private-name rules match it", leakguardtest.RulesHostClass},
			reads: "git\n",
		},
		{
			name: "an unserved owner's repository GitHub asks ls-remote a credential for", repo: "other-owner/invented-repo", git: refused,
			code: 2,
			says: []string{
				"LEAK CHECK SETUP ERROR: the push to origin: the visibility of other-owner/invented-repo could not be read: " +
					`gh api repos/other-owner/invented-repo exited 2: legion gh: the role's GitHub App is not installed on "other-owner": ` + root + " holds no gh directory for it, " +
					"and git ls-remote with no credential could not answer: git ls-remote of other-owner/invented-repo exited 128",
				"could not read Username",
			},
			reads: "git\n",
		},
		{name: "a served owner's private repository", repo: "acme/widgets", git: lists, code: 0, reads: "rest\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := t.TempDir(), filepath.Join(t.TempDir(), "reads")
			stub := filepath.Join(bin, "real-gh")
			script := ghStandInHelp(help) +
				"echo rest >> \"$GH_STANDIN_RECORD\"\n" +
				"printf '%s\\r\\n' 'HTTP/2.0 200 OK' 'X-Ratelimit-Remaining: 4999' ''\nprintf '%s\\n' private\nexit 0\n"
			if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := execfile.Write(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho git >> \"$GH_STANDIN_RECORD\"\n"+tc.git), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			for name, value := range map[string]string{
				testMainEnv: "1", "PATH": ghWrapperDir(t) + ":" + bin + ":" + os.Getenv("PATH"),
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"GH_TOKEN": "", "GITHUB_TOKEN": "", "GH_STANDIN_RECORD": record, "XDG_STATE_HOME": t.TempDir(),
				"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
			} {
				t.Setenv(name, value)
			}
			// A ref the push deletes, named for a host the rules refuse: its name is the push's one
			// record, so a scan refuses the push and no scan passes it, and no git reads a commit.
			line := "refs/heads/work " + noSHA + " refs/heads/legion/" + leakguardtest.RulesHost + " " + noSHA + "\n"
			code, printed := leakCheck(t, line, "pre-push", "origin", "https://github.com/"+tc.repo+".git")
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, printed)
			}
			says(t, printed, tc.says)
			if got := readRecord(t, record); got != tc.reads {
				t.Errorf("the stand-ins recorded the reads %q, want %q", got, tc.reads)
			}
		})
	}
}

// refusedFile is the file the commit of aPushOfARefusedLine adds, and refusedLine the one line it
// holds: a host the fixture rule file refuses (leakguardtest.RulesHost), so the guard refuses a
// push of that commit by its rules whatever the shared check answers.
const (
	refusedFile = "notes.txt"
	refusedLine = "the runner answers at " + leakguardtest.RulesHost
)

// aPushOfARefusedLine is a repository at root/repo made as aPushFixture makes one, but whose one
// commit adds refusedFile holding refusedLine; the runner that made it, for a row that adds a
// remote or a setting to it; and the line git hands a pre-push hook for a push of that commit to a
// remote that holds no ref yet. The repository has no remote.
func aPushOfARefusedLine(t *testing.T, root string) (fixture fixturegit.Runner, dir, line string) {
	t.Helper()
	dir = filepath.Join(root, "repo")
	fixture = fixturegit.Isolated(t, t.TempDir())
	fixture.Run(root, fixturegit.InitWorkTree(dir, "main")...)
	if err := os.WriteFile(filepath.Join(dir, refusedFile), []byte(refusedLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.Run(dir, "git", "add", "--", refusedFile)
	fixture.Run(dir, "git", "commit", "--quiet", "-m", "a commit of nothing in particular")
	sha := fixture.Run(dir, "git", "rev-parse", "HEAD")
	return fixture, dir, "refs/heads/main " + sha + " refs/heads/main " + noSHA + "\n"
}

// writeLegacyRemote writes the legacy remote file name under the repository dir's .git/<kind>
// (remotes or branches) holding content, as a person or an old tool writes one: git reads a push
// by that name from it, and `git remote` lists no such remote.
func writeLegacyRemote(t *testing.T, dir, kind, name, content string) {
	t.Helper()
	legacy := filepath.Join(dir, ".git", kind)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A push that can only write to a path on this machine is cleared with no check (R1 of
// dispatch://INFRA-3524's spec): exit 0, nothing printed, no shared check found or run, although
// its commit adds a line the fixture rule file refuses. Each row runs under a deployment that names
// a shared check none can be found for, where the guard answers 2, and under a stand-in check that
// records each run, which must record none. The rows are the four spellings of a local path the
// hook clears (an absolute path, one from ./ and from ../, and a file URL with no host), each from
// a repository with no remote and from one whose remotes origin and upstream are local paths; a
// push from a bare repository, where git runs the hook in the git directory; a push with GIT_DIR
// naming the repository from another directory, as `jj git push` runs git; and a push from a
// repository whose legacy remote files name local paths.
func TestLeakCheckPrePushClearsAPushThatCanOnlyWriteToALocalPath(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variable leakCheckStdin (leakCheck).
	type row struct {
		name string
		// push builds the repository under root, puts the process where git runs the hook, and
		// answers the line git hands the hook and the URL.
		push func(t *testing.T, root string) (line, url string)
	}
	var rows []row
	for _, local := range []struct {
		name string
		url  func(root string) string
	}{
		{"an absolute path", func(root string) string { return filepath.Join(root, "remote.git") }},
		{"a path from ./", func(string) string { return "./remote.git" }},
		{"a path from ../", func(string) string { return "../remote.git" }},
		{"a file URL with no host", func(root string) string { return "file://" + filepath.Join(root, "remote.git") }},
	} {
		rows = append(rows,
			row{local.name + " from a repository with no remote", func(t *testing.T, root string) (string, string) {
				_, dir, line := aPushOfARefusedLine(t, root)
				t.Chdir(dir)
				return line, local.url(root)
			}},
			row{local.name + " from a repository whose remotes origin and upstream are local paths", func(t *testing.T, root string) (string, string) {
				fixture, dir, line := aPushOfARefusedLine(t, root)
				fixture.Run(dir, "git", "remote", "add", "origin", filepath.Join(root, "origin.git"))
				fixture.Run(dir, "git", "remote", "add", "upstream", "../upstream.git")
				t.Chdir(dir)
				return line, local.url(root)
			}},
		)
	}
	rows = append(rows,
		row{"an absolute path from a bare repository, where the hook runs in the git directory", func(t *testing.T, root string) (string, string) {
			fixture, dir, line := aPushOfARefusedLine(t, root)
			bare := filepath.Join(root, "bare.git")
			fixture.Run(root, fixturegit.InitBare(bare, "main")...)
			fixture.Run(dir, "git", "push", "--quiet", bare, "main")
			t.Chdir(bare)
			return line, filepath.Join(root, "remote.git")
		}},
		row{"an absolute path with GIT_DIR naming the repository from another directory", func(t *testing.T, root string) (string, string) {
			_, dir, line := aPushOfARefusedLine(t, root)
			t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
			t.Chdir(t.TempDir())
			return line, filepath.Join(root, "remote.git")
		}},
		row{"an absolute path from a repository whose .git/remotes and .git/branches files name local paths", func(t *testing.T, root string) (string, string) {
			_, dir, line := aPushOfARefusedLine(t, root)
			writeLegacyRemote(t, dir, "remotes", "legacy", "URL: "+filepath.Join(root, "legacy-remote.git")+"\n")
			writeLegacyRemote(t, dir, "branches", "old", filepath.Join(root, "legacy-branch.git")+"#main\n")
			t.Chdir(dir)
			return line, filepath.Join(root, "remote.git")
		}},
	)
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			for _, check := range []struct {
				name string
				// set names the row's shared check, after the repository is built, and answers
				// the file a stand-in records each run in, "" for none.
				set func(t *testing.T) string
			}{
				{"with no shared check under a deployment that names one", func(t *testing.T) string {
					noSharedCheck(t, theChecker)
					return ""
				}},
				{"with a stand-in check that records each run", func(t *testing.T) string {
					record := filepath.Join(t.TempDir(), "runs")
					t.Setenv("LEAK_CHECK_RECORD", record)
					t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Recording(leakguardtest.Passes)))
					return record
				}},
			} {
				t.Run(check.name, func(t *testing.T) {
					line, url := tc.push(t, t.TempDir())
					record := check.set(t)
					t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
					if code, printed := leakCheck(t, line, "pre-push", "origin", url); code != 0 || printed != "" {
						t.Errorf("legion leak-check pre-push origin %s = exit %d, stderr %q; want exit 0 and nothing printed", url, code, printed)
					}
					if record == "" {
						return
					}
					if ran := readRecord(t, record); ran != "" {
						t.Errorf("the stand-in check ran: %q", ran)
					}
				})
			}
		})
	}
}

// localPush is the repository of a row of TestLeakCheckPrePushGuardsEveryOtherPushAsItsBaseDid
// (aPushOfARefusedLine) and what the row adds to it with: root is the row's directory, dir the
// repository, local the absolute path root/remote.git, and fixture the runner that made it.
type localPush struct {
	root, dir, local string
	fixture          fixturegit.Runner
}

// Every push the hook does not clear gets the guard it got on the base this change merges onto,
// its exit status and its whole text (R2 of dispatch://INFRA-3524's spec). Each row's commit adds
// a line the fixture rule file refuses, and each row runs with no shared check under a deployment
// that names one, with a stand-in check that refuses, and, where its URL names no repository on
// GitHub, with a stand-in that passes, whose push the rules refuse. A URL that names one makes the
// guard read its visibility with gh after the shared check has answered, which the first two never
// let it reach. The rows are a local URL with each condition of R1 broken alone: another remote at
// a host, a pushurl at a host, a rewrite to a host by insteadOf and by pushInsteadOf,
// remote.<name>.vcs and remote.<name>.receivepack in the repository's configuration and on git's
// command line, the git wrapper's mark, GIT_EXEC_PATH at another directory, and a legacy remote
// file at a host; and every URL that is not one of the four spellings of a local path the hook
// clears. Each expected text is the one the same row printed with the base's code (f57cfa7e),
// which clears no push.
func TestLeakCheckPrePushGuardsEveryOtherPushAsItsBaseDid(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory; it sets the package variable leakCheckStdin (leakCheck).
	for _, tc := range []struct {
		name string
		// push adds to r what the row's repository holds, sets what the push's environment
		// holds, and answers the URL git hands the hook.
		push func(t *testing.T, r localPush) string
		// github is whether the URL names a repository on GitHub, whose push the passing
		// stand-in would hand to a visibility read.
		github bool
	}{
		{name: "a local URL in a repository that also has a remote at the host URL", push: func(t *testing.T, r localPush) string {
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			r.fixture.Run(r.dir, "git", "remote", "add", "upstream", hostURL)
			return r.local
		}},
		{name: "origin's url local and its pushurl the host URL", push: func(t *testing.T, r localPush) string {
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			r.fixture.Run(r.dir, "git", "remote", "set-url", "--push", "origin", hostURL)
			return r.local
		}},
		{name: "origin local, rewritten to the host URL by url.<host>.insteadOf", push: func(t *testing.T, r localPush) string {
			origin := filepath.Join(r.root, "origin.git")
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", origin)
			r.fixture.Run(r.dir, "git", "config", "url."+hostURL+".insteadOf", origin)
			return r.local
		}},
		{name: "another remote's url local, rewritten for its push to the host URL by url.<host>.pushInsteadOf", push: func(t *testing.T, r localPush) string {
			upstream := filepath.Join(r.root, "upstream.git")
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			r.fixture.Run(r.dir, "git", "remote", "add", "upstream", upstream)
			r.fixture.Run(r.dir, "git", "config", "url."+hostURL+".pushInsteadOf", upstream)
			return r.local
		}},
		{name: "origin sets remote.origin.vcs", push: func(t *testing.T, r localPush) string {
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			r.fixture.Run(r.dir, "git", "config", "remote.origin.vcs", "invented")
			return r.local
		}},
		{name: "origin sets remote.origin.receivepack", push: func(t *testing.T, r localPush) string {
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			r.fixture.Run(r.dir, "git", "config", "remote.origin.receivepack", "git-receive-pack")
			return r.local
		}},
		{name: "remote.origin.receivepack set through GIT_CONFIG_PARAMETERS", push: func(t *testing.T, r localPush) string {
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			// What `git -c remote.origin.receivepack=git-receive-pack push origin` hands its hook
			// (git 2.47.3).
			t.Setenv("GIT_CONFIG_PARAMETERS", "'remote.origin.receivepack'='git-receive-pack'")
			return r.local
		}},
		{name: "remote.origin.vcs set through a GIT_CONFIG_COUNT block", push: func(t *testing.T, r localPush) string {
			r.fixture.Run(r.dir, "git", "remote", "add", "origin", r.local)
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.vcs")
			t.Setenv("GIT_CONFIG_VALUE_0", "invented")
			return r.local
		}},
		{name: "the mark is set", push: func(t *testing.T, r localPush) string {
			t.Setenv(leakguard.PushNamesReceivePackVar, "1")
			return r.local
		}},
		{name: "GIT_EXEC_PATH names another directory", push: func(t *testing.T, r localPush) string {
			t.Setenv("GIT_EXEC_PATH", t.TempDir())
			return r.local
		}},
		{name: "that directory also holds a git, which logs each run and whose log must stay empty", push: func(t *testing.T, r localPush) string {
			execPath, log := t.TempDir(), filepath.Join(t.TempDir(), "runs")
			if err := execfile.Write(filepath.Join(execPath, "git"), []byte("#!/bin/sh\necho ran >> '"+log+"'\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			// The directory stays off PATH, so a git looked up there is the real one, which
			// the base's guard runs too.
			t.Setenv("GIT_EXEC_PATH", execPath)
			t.Cleanup(func() {
				if ran := readRecord(t, log); ran != "" {
					t.Errorf("the git in GIT_EXEC_PATH ran: %q", ran)
				}
			})
			return r.local
		}},
		{name: "a .git/remotes file whose URL is the host URL", push: func(t *testing.T, r localPush) string {
			writeLegacyRemote(t, r.dir, "remotes", "legacy", "URL: "+hostURL+"\n")
			return r.local
		}},
		{name: "a .git/branches file whose URL is the host URL", push: func(t *testing.T, r localPush) string {
			writeLegacyRemote(t, r.dir, "branches", "old", hostURL+"#main\n")
			return r.local
		}},
		{name: "remote.git", push: func(*testing.T, localPush) string { return "remote.git" }},
		{name: "file://localhost/abs/remote.git", push: func(_ *testing.T, r localPush) string { return "file://localhost" + r.local }},
		{name: "file://otherhost/abs/remote.git", push: func(_ *testing.T, r localPush) string { return "file://otherhost" + r.local }},
		{name: "host:remote.git", push: func(*testing.T, localPush) string { return "host:remote.git" }},
		{name: "git@github.com:invented-org/invented-repo.git", github: true, push: func(*testing.T, localPush) string {
			return "git@github.com:invented-org/invented-repo.git"
		}},
		{name: "ext::sh", push: func(*testing.T, localPush) string { return "ext::sh" }},
		{name: "foo::/abs/remote.git", push: func(_ *testing.T, r localPush) string { return "foo::" + r.local }},
		{name: hostURL, push: func(*testing.T, localPush) string { return hostURL }},
		{name: "git://127.0.0.1:9418/remote.git", push: func(*testing.T, localPush) string { return "git://127.0.0.1:9418/remote.git" }},
		{name: "https://github.com/invented-org/invented-repo.git", github: true, push: func(*testing.T, localPush) string {
			return "https://github.com/invented-org/invented-repo.git"
		}},
		{name: ".", push: func(*testing.T, localPush) string { return "." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, check := range []struct {
				name string
				// set names the row's shared check, after the repository is built, and answers
				// the status and the whole text the base's guard answered with it.
				set  func(t *testing.T) (int, string)
				skip bool
			}{
				{name: "with no shared check under a deployment that names one", set: func(t *testing.T) (int, string) {
					noSharedCheck(t, theChecker)
					// The pointer file is the last place the guard looks, under the home noSharedCheck
					// made, and the base's text names it.
					pointer := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "public-leak-check", "path")
					return 2, "LEAK CHECK SETUP ERROR: the push to origin: the shared public leak check could not be found: " +
						"PUBLIC_LEAK_CHECK names none, public-leak-check.sh is on no PATH entry, and the pointer file " + pointer +
						" could not be read: no such file or directory\n"
				}},
				{name: "with a stand-in check that refuses", set: func(t *testing.T) (int, string) {
					t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Refuses))
					return 3, "LEAK CHECK: refusing the push to origin: the shared public leak check refused it\n  LEAK CHECK: refusing: one of ours\n"
				}},
				{name: "with a stand-in check that passes", skip: tc.github, set: func(t *testing.T) (int, string) {
					t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes))
					return 3, "LEAK CHECK: refusing the push to origin: the deployment's private-name rules match it\n" +
						"  " + leakguardtest.ReportLine(refusedFile, leakguardtest.RulesHostClass, leakguardtest.RulesHost, 1, refusedLine) + "\n"
				}},
			} {
				if check.skip {
					continue
				}
				t.Run(check.name, func(t *testing.T) {
					root := t.TempDir()
					fixture, dir, line := aPushOfARefusedLine(t, root)
					url := tc.push(t, localPush{root: root, dir: dir, local: filepath.Join(root, "remote.git"), fixture: fixture})
					t.Chdir(dir)
					code, want := check.set(t)
					t.Setenv(leakguard.RulesFileVar, leakguardtest.WriteRules(t, t.TempDir()))
					if gotCode, printed := leakCheck(t, line, "pre-push", "origin", url); gotCode != code || printed != want {
						t.Errorf("legion leak-check pre-push origin %s = exit %d, stderr\n%s\nwant exit %d and\n%s", url, gotCode, printed, code, want)
					}
				})
			}
		})
	}
}
