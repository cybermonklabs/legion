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

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// theLeakCheckHost is the one refusable literal these tests need: a Tailscale MagicDNS host built
// out of invented words, which names no machine and no tailnet, and which this repository's own
// rules refuse (tailnet_host_rule, packages/daemon/internal/leakguard/private-names.toml). It is
// spelled in two pieces, the way that rule file and the guard's own tests spell every literal of
// their own, so that this file passes .github/scripts/check-private-names.sh: joined here, it
// would put a name the rules refuse into a tracked file of a public repository.
const theLeakCheckHost = "amber-kite.still-meadow.ts" + ".net"

// theLeakCheckClass is what a report calls that host, and the whole of what it says about the
// text: the description of the rule that matched.
const theLeakCheckClass = "names a Tailscale MagicDNS host"

// noSHA is git's "this ref is not there", which a pre-push line carries for the remote side of a
// ref the push creates.
const noSHA = "0000000000000000000000000000000000000000"

// noSharedCheck leaves the command no check to resolve at all: nothing in PUBLIC_LEAK_CHECK, no
// public-leak-check.sh on any PATH entry, and no pointer file under a configuration home of its
// own. Every fixture a case needs is built before this, since it empties the PATH a child of the
// test would be found on.
func noSharedCheck(t *testing.T) {
	t.Helper()
	fixtureenv.Unset(t, "PUBLIC_LEAK_CHECK")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

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

// leakCheck runs the command as git's hook and a pod's scripts run it: stdin is what they pipe
// in, and the status and what reached stderr are what they read. Standard output is the hook's
// own, and the command writes nothing to it.
func leakCheck(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	previous := leakCheckStdin
	t.Cleanup(func() { leakCheckStdin = previous })
	leakCheckStdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	code := runLeakCheck(ctx, args, &stdout, &stderr)
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
// as its arguments. Either check refuses the push, and the statuses are the shared check's own —
// 0 clean, 3 refused, 2 a check that could not be made to run.
func TestLeakCheckPrePushRunsTheGuardOverGitsUpdateLines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		// standIn is the stand-in shared check's body, and "" is a call with no check at all.
		standIn string
		code    int
		says    []string
	}{
		{
			name:    "the shared check refuses the push",
			message: "a commit of nothing in particular",
			standIn: leakguardtest.Refuses,
			code:    3,
			says:    []string{"LEAK CHECK: refusing the push to origin: the shared public leak check refused it", "one of ours"},
		},
		{
			name:    "this repository's own rules refuse the push",
			message: "a commit that names " + theLeakCheckHost,
			standIn: leakguardtest.Passes,
			code:    3,
			says:    []string{"LEAK CHECK: refusing the push to origin: this repository's own private-name rules match it", theLeakCheckClass},
		},
		{
			name:    "a clean push",
			message: "a commit of nothing in particular",
			standIn: leakguardtest.Passes,
			code:    0,
		},
		{
			name:    "no shared check resolves",
			message: "a commit of nothing in particular",
			standIn: "",
			code:    2,
			says:    []string{"LEAK CHECK SETUP ERROR: the push to origin: the shared public leak check could not be found"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, line := aPushFixture(t, tc.message)
			// git runs its hook in the repository the push is made from, and that working
			// directory is where the command reads the push's commits from.
			t.Chdir(dir)
			if tc.standIn == "" {
				noSharedCheck(t)
			} else {
				t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), tc.standIn))
			}
			// A local path names no github.com repository, so the guard reads no visibility and
			// this test starts no gh.
			code, printed := leakCheck(t, line, "pre-push", "origin", dir+".git")
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
// (leakguard.RunByRealGhVar), since gh's git inherits it. The guard reads the destination's
// visibility with `gh`, which in a pane or pod is `legion gh`, and it blanks the variable for that
// read, so the read reaches the real gh and the push is cleared. Under the variable the read would
// be refused as a real gh leading back to `legion gh`, and the push with it, leaving the
// repository gh created empty.
func TestLeakCheckPrePushUnderTheRealGhsVariableReadsTheVisibility(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "gh")
	dir, line := aPushFixture(t, "a commit of nothing in particular")
	code, printed := legionLeakCheckPrePush(t, dir, map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
		"GH_STANDIN_RECORD": record, leakguard.RunByRealGhVar: stub,
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
func TestASignalToLeakChecksGroupStopsTheSharedCheck(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether the stand-in is still running", err)
	}
	dir, line := aPushFixture(t, "a commit of nothing in particular")
	for _, shape := range []struct {
		name  string
		stdin string
		// args is the command after `legion`. A local path names no github.com repository, so
		// the push's guard reads no visibility and starts no gh, and the write is held in the
		// check, the guard's first child.
		args []string
		// seconds is the holding stand-in's sleep for each signal, one per row.
		seconds map[syscall.Signal]string
	}{
		{
			name: "leak-check pre-push", stdin: line, args: []string{"leak-check", "pre-push", "origin", dir + ".git"},
			seconds: map[syscall.Signal]string{syscall.SIGINT: "293", syscall.SIGHUP: "294"},
		},
		{
			name: "leak-check text", stdin: "a title\na body of nothing in particular\n", args: []string{"leak-check", "text", "the pull request body"},
			seconds: map[syscall.Signal]string{syscall.SIGINT: "291", syscall.SIGHUP: "292"},
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
// caller knows better than the guard does.
func TestLeakCheckTextRunsTheGuardOverWhatItIsPiped(t *testing.T) {
	const kind = "the pull request body"
	for _, tc := range []struct {
		name    string
		text    string
		standIn string
		code    int
		says    []string
	}{
		{
			name:    "the shared check refuses the text",
			text:    "a title\na body of nothing in particular\n",
			standIn: leakguardtest.Refuses,
			code:    3,
			says:    []string{"LEAK CHECK: refusing " + kind + ": the shared public leak check refused it", "one of ours"},
		},
		{
			name:    "this repository's own rules refuse the text",
			text:    "a title\na body that names " + theLeakCheckHost + "\n",
			standIn: leakguardtest.Passes,
			code:    3,
			says:    []string{"LEAK CHECK: refusing " + kind + ": this repository's own private-name rules match it", theLeakCheckClass},
		},
		{
			name:    "a clean text",
			text:    "a title\na body of nothing in particular\n",
			standIn: leakguardtest.Passes,
			code:    0,
		},
		{
			name:    "no shared check resolves",
			text:    "a title\na body of nothing in particular\n",
			standIn: "",
			code:    2,
			says:    []string{"LEAK CHECK SETUP ERROR: " + kind + ": the shared public leak check could not be found"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.standIn == "" {
				noSharedCheck(t)
			} else {
				t.Setenv("PUBLIC_LEAK_CHECK", leakguardtest.Write(t, t.TempDir(), tc.standIn))
			}
			code, printed := leakCheck(t, tc.text, "text", kind)
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, printed)
			}
			says(t, printed, tc.says)
		})
	}
}

// A call the command cannot read is a usage error, exit 2, and no write is checked by it: a hook
// or a script that called it wrongly has cleared nothing. A help request is the one call that
// prints the usage and exits 0.
func TestLeakCheckRefusesACallItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		says string
	}{
		{name: "no shape at all", code: 2, says: subcommandUsage("leak-check", leakCheckCommands)},
		{name: "a shape it does not know", args: []string{"pre-receive"}, code: 2, says: `legion leak-check: unknown subcommand "pre-receive"`},
		{name: "a pre-push with no url", args: []string{"pre-push", "origin"}, code: 2, says: leakCheckPrePushUsage},
		{name: "a pre-push with a third argument", args: []string{"pre-push", "origin", "url", "extra"}, code: 2, says: leakCheckPrePushUsage},
		{name: "a text with no kind", args: []string{"text"}, code: 2, says: leakCheckTextUsage},
		{name: "a help request", args: []string{"--help"}, code: 0, says: subcommandUsage("leak-check", leakCheckCommands)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, printed := leakCheck(t, "", tc.args...)
			if code != tc.code {
				t.Errorf("exited %d, want %d: %s", code, tc.code, printed)
			}
			says(t, printed, []string{tc.says})
		})
	}
}
