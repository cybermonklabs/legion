package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/envoy/clitest"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// legionBuildDir is where legionBuild puts the binary: a directory TestMain makes and removes.
var legionBuildDir string

// legionBuild builds this package's `legion` once for the whole test binary and answers its
// path: the binary a caller runs, with no test code linked in, which is what clitest and the
// invocation table drive. Every test that runs it only runs it, so one build serves them all. It
// links leakguardtest.NoCurl as its guards' curl (leakguardtest.NoCurlLinked), as TestMain gives
// this binary's own: the built binary reads no LEGION_CMD_TEST_CURL, and without the link its
// guards would run the system's /usr/bin/curl, with a token of the test's bundle, wherever their
// REST read through a stand-in gh fails on its connection or is stopped by its bound (INFRA-3904).
// It also links in this test binary's deadline (programbound's linkedDeadline, from
// testDeadlineEnv), so each wait the built binary bounds on a program it starts takes what is left
// of that deadline, as this binary's own runs as `legion` do; under -timeout 0 it links none, and
// the product's bounds hold. The two go in one -ldflags, since go build takes only the last.
var legionBuild = sync.OnceValues(func() (string, error) {
	binary := filepath.Join(legionBuildDir, "legion")
	ldflags := leakguardtest.NoCurlLinked
	if deadline, ok := os.LookupEnv(testDeadlineEnv); ok {
		ldflags += " -X github.com/sjawhar/legion/daemon/internal/programbound.linkedDeadline=" + deadline
	}
	if out, err := exec.Command("go", "build", "-ldflags", ldflags, "-o", binary, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, out)
	}
	return binary, nil
})

// buildLegion is legionBuild's binary, or the test's failure when it could not be built.
func buildLegion(t *testing.T) string {
	t.Helper()
	binary, err := legionBuild()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// legionCommands is every command a person runs, by its words, its groups included: the commands
// of newRoot that do not set Hidden, which TestConformanceCaseIsTheTree holds it to.
var legionCommands = [][]string{
	{"version"},
	{"config"}, {"config", "check"}, {"config", "reference"},
	{"start"}, {"stop"}, {"state"}, {"legions"}, {"status"},
	{"issue"}, {"issue", "status"},
	{"restart"},
	{"claims"}, {"claims", "spawn"}, {"claims", "deliver"}, {"claims", "suspend"}, {"claims", "resume"},
	{"claims", "stop"}, {"claims", "retry"}, {"claims", "compact"}, {"claims", "forget-session"},
	{"claims", "release-names"}, {"claims", "close"}, {"claims", "list"},
	{"project"}, {"project", "pause"}, {"project", "resume"},
	{"sessions"}, {"sessions", "import"},
	{"replay"}, {"replay", "collect"}, {"replay", "collect-merges"}, {"replay", "collect-queue"}, {"replay", "neutralize"}, {"replay", "run"},
	{"replay", "scorecard"}, {"replay", "signals"}, {"replay", "models"}, {"replay", "validate"}, {"replay", "forecast"}, {"replay", "compare"},
	{"replay", "rules"},
	{"probe-boot"},
	{"controller"}, {"controller", "start"},
	{"intake"}, {"intake", "replay"}, {"intake", "discard"},
}

// legionHidden is every command only a program runs — a pod's, a pane's, git's or an image
// build's: the commands of newRoot that set Hidden, which TestConformanceCaseIsTheTree holds it
// to. Each sets Hidden, so neither help nor completion offers it, its group's included.
var legionHidden = [][]string{
	{"worker-shim"},
	{"launcher"}, {"launcher", "role-init"}, {"launcher", "volume-trim-delete"},
	{"claude-bridge"}, {"model-token"}, {"probe-image"},
	{"workspace-init"}, {"workspace-init", "fetch"},
	{"workspace-init", "provision"}, {"workspace-init", "cache"},
	{"git-credential"}, {"git"}, {"gh"},
	{"leak-check"}, {"leak-check", "pre-push"}, {"leak-check", "text"},
}

// The conformance case's two lists are newRoot's commands, split by Hidden, cobra's own help and
// completion left to clitest: a command added to the tree and to neither list fails here, so no
// command passes clitest by being left out of its case.
func TestConformanceCaseIsTheTree(t *testing.T) {
	var visible, hidden [][]string
	var walk func(cmd *cobra.Command, words []string)
	walk = func(cmd *cobra.Command, words []string) {
		for _, child := range cmd.Commands() {
			if len(words) == 0 && (child.Name() == "help" || child.Name() == "completion") {
				continue
			}
			path := append(slices.Clone(words), child.Name())
			if child.Hidden {
				hidden = append(hidden, path)
			} else {
				visible = append(visible, path)
			}
			walk(child, path)
		}
	}
	walk(newRoot(io.Discard, io.Discard), nil)
	for _, list := range []struct {
		name         string
		tree, listed [][]string
	}{{"legionCommands", visible, legionCommands}, {"legionHidden", hidden, legionHidden}} {
		tree, inCase := commandSet(list.tree), commandSet(list.listed)
		for command := range tree {
			if !inCase[command] {
				t.Errorf("legion %s is in the tree and not in %s, so clitest never runs it", command, list.name)
			}
		}
		for command := range inCase {
			if !tree[command] {
				t.Errorf("%s holds legion %s, which the tree does not have as such", list.name, command)
			}
		}
	}
}

// commandSet is commands, each by its words joined with spaces.
func commandSet(commands [][]string) map[string]bool {
	set := make(map[string]bool, len(commands))
	for _, words := range commands {
		set[strings.Join(words, " ")] = true
	}
	return set
}

// The built `legion` keeps the rules every command line on cobra keeps (clitest's package
// documentation): help on every command, a usage error that exits 2 and asks the daemon nothing,
// flags read after a positional, and completion scripts each shell reads. Every address legion
// calls points at the recorder, so a typo that reached the daemon would be counted. A command
// only a program runs is offered nowhere: neither its group's help nor its group's completion
// lists it, which clitest reads for visible groups alone.
func TestLegionConformance(t *testing.T) {
	legion := buildLegion(t)
	recorder := clitest.NewRecorder(t)
	env := []string{"LEGION_DAEMON_URL=" + recorder.URL()}
	clitest.Conformance(t, legion, clitest.Case{
		Commands: legionCommands,
		Hidden:   legionHidden,
		Env:      env,
		Server:   recorder,
	})
	t.Run("hidden", func(t *testing.T) {
		// Each group holding a hidden command, with the names of those commands.
		var groups [][]string
		names := map[string][]string{}
		for _, command := range legionHidden {
			if len(command) < 2 {
				continue
			}
			group, key := command[:len(command)-1], strings.Join(command[:len(command)-1], " ")
			if _, seen := names[key]; !seen {
				groups = append(groups, group)
			}
			names[key] = append(names[key], command[len(command)-1])
		}
		for _, group := range groups {
			hidden := names[strings.Join(group, " ")]
			for _, args := range [][]string{slices.Concat(group, []string{"--help"}), slices.Concat([]string{"__complete"}, group, []string{""})} {
				run := exec.Command(legion, args...)
				run.Dir, run.Env = t.TempDir(), append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
				out, err := run.Output()
				if err != nil {
					t.Errorf("legion %s: %v", strings.Join(args, " "), err)
					continue
				}
				for line := range strings.Lines(string(out)) {
					if fields := strings.Fields(line); len(fields) > 0 && slices.Contains(hidden, fields[0]) {
						t.Errorf("legion %s lists %q, a command only a program runs:\n%s", strings.Join(args, " "), fields[0], out)
					}
				}
			}
		}
		if requests := recorder.Requests(); len(requests) > 0 {
			t.Errorf("help and completion asked the daemon %q", requests)
		}
	})
}

// cobra's own two commands refuse a word they do not know as every other group does, exit 2:
// left as cobra makes them, `legion completion xyz` prints completion's help and exits 0 (the
// command is not runnable), and `legion help nope` prints the root's help and exits 0 (a root
// with an Args of its own resolves any word to itself). A word after a shell is a usage error
// too, where cobra's NoArgs would exit 1. The forms they serve still exit 0.
func TestCobraBuiltinsRefuseAWordTheyDoNotKnow(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"completion", "xyz"}, 2, `legion completion: unknown command "xyz" for "legion completion"`},
		{[]string{"completion"}, 2, "Usage:\n  legion completion"},
		{[]string{"completion", "bash", "extra"}, 2, `legion completion bash: unexpected argument "extra"`},
		{[]string{"completion", "zsh", "extra"}, 2, `legion completion zsh: unexpected argument "extra"`},
		{[]string{"completion", "fish", "extra"}, 2, `legion completion fish: unexpected argument "extra"`},
		{[]string{"completion", "powershell", "extra"}, 2, `legion completion powershell: unexpected argument "extra"`},
		{[]string{"help", "nope"}, 2, `legion help: unknown help topic "nope"`},
		{[]string{"help", "claims", "frob"}, 2, `legion help: unknown help topic "claims frob"`},
		{[]string{"help"}, 0, ""},
		{[]string{"help", "claims", "spawn"}, 0, ""},
		{[]string{"help", "launcher", "role-init"}, 0, ""},
		{[]string{"completion", "bash"}, 0, ""},
	} {
		code, stdout, stderr := legionRun(t, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.stderr) || (code == 0 && (stdout == "" || stderr != "")) {
			t.Errorf("legion %s = %d, stdout %.80q, stderr %q; want %d and stderr holding %q", strings.Join(tc.args, " "), code, stdout, stderr, tc.code, tc.stderr)
		}
	}
}

// A pass-through command hands every word after its name to the program it wraps, unread: a flag
// of git's or gh's is git's or gh's, and cobra parsing it would refuse it or take it away. Each
// case runs the built legion as its runtime's wrapper runs it and reads what the stand-in for the
// real program received.
func TestPassThroughCommandsParseNoFlags(t *testing.T) {
	legion := buildLegion(t)
	t.Run("git", func(t *testing.T) {
		// A tmux pane's git (trailerWrapperDir's shape, around the built binary) runs the next git
		// on PATH, here a script that records its arguments and then runs a real git.
		git := fixturegit.CompiledGit(t)
		bin := trailerWrapperDir(t, legion)
		host := t.TempDir()
		log := filepath.Join(host, "calls")
		writeTrailerScript(t, filepath.Join(host, "git"), trailerLogging(log)+"exec '"+git+"' \"$@\"\n")
		repo := trailerRepo(t, git)
		sh, err := exec.LookPath("sh")
		if err != nil {
			t.Fatal(err)
		}
		env := trailerEnv(t, bin+":"+host+":"+trailerPath(git))
		if code, _, stderr := trailerRun(t, repo, env, "", sh, "-c", "git commit --allow-empty -m x"); code != 0 {
			t.Fatalf("git commit -m x through the pane's git = %d: %s", code, stderr)
		}
		if calls := trailerCalls(t, log); len(calls) != 1 || !slices.Equal(calls[0], []string{"commit", "--allow-empty", "-m", "x"}) {
			t.Fatalf("the host's git received %q, want one run with [commit --allow-empty -m x]", calls)
		}
	})
	t.Run("gh", func(t *testing.T) {
		// A pod's gh (packages/daemon/docker/gh's shape) runs `legion gh`, which runs the real gh
		// (LEGION_GH_PATH), here a stand-in that prints its arguments.
		//
		// The guard's reads of acme/widgets stay on this machine. When the guard's REST read
		// through the stand-in gh fails on its connection, or its bound stops it, the guard reads
		// through curl, and then with ls-remote. The built legion has no curl (legionBuild), and a
		// stand-in git answers the ls-remote. A guard that reads through curl first asks `gh auth
		// git-credential get` for the token, and `legion gh` writes each such request to its call
		// log. So the row reads that the log holds none.
		root, defaultDir := gitCredentialBundle(t)
		help, err := filepath.Abs(ghHelpDir)
		if err != nil {
			t.Fatal(err)
		}
		stub := filepath.Join(t.TempDir(), "gh")
		if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"printf '%s\\n' \"$*\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		wrappers, state := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(wrappers, "gh"), []byte("#!/bin/sh\nexec '"+legion+"' gh \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		lsRemote := "#!/bin/sh\nfor a; do if [ \"$a\" = ls-remote ]; then printf '%s\\tHEAD\\n' " + strings.Repeat("1", 40) + "; exit 0; fi; done\n" +
			"exec " + shellprefix.Literal(fixturegit.CompiledGit(t)) + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(wrappers, "git"), []byte(lsRemote), 0o700); err != nil {
			t.Fatal(err)
		}
		args := []string{"pr", "view", "1", "--json", "state", "-R", "acme/widgets"}
		cmd := exec.Command(legion, append([]string{"gh"}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{
			"PATH=" + wrappers + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"LEGION_GH_PATH=" + stub, "GH_CONFIG_DIR=" + defaultDir, "LEGION_GH_OWNERS_DIR=" + root,
			"PUBLIC_LEAK_CHECK=" + leakguardtest.Write(t, t.TempDir(), ghCheckPasses), "XDG_STATE_HOME=" + state,
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("legion gh %q: %v, stderr %s", args, err, stderrOf(err))
		}
		if got, want := string(out), strings.Join(args, " ")+"\n"; got != want {
			t.Fatalf("the real gh received %q, want %q", got, want)
		}
		log := readRecord(t, filepath.Join(state, "legion-gh", "calls.log"))
		if !strings.Contains(log, ` repository="acme/widgets" `) {
			t.Fatalf("legion gh's call log holds no line of the call, so it cannot show a token request either: %q", log)
		}
		for line := range strings.Lines(log) {
			if strings.Contains(line, ` path="auth git-credential" `) {
				t.Fatalf("the built legion's guard asked `gh auth git-credential get` for a token (%q), which it asks for only to read through curl: the built legion has no curl (legionBuild)", strings.TrimSpace(line))
			}
		}
	})
}

// The legion these tests build links this test binary's deadline (legionBuild,
// programbound.linkedDeadline), so each bound it sets on a program it starts is what is left of
// that deadline, as it is in this binary's own runs as `legion`. The linker takes an -X of a name
// no package holds without a word, and a rename would give the built binary the product bounds
// back and every row that runs it a race with them again. So this row runs the built `legion gh`
// under a git whose configuration read answers past the product's 30 s for it (ghGitReadBound):
// it passes only where the built binary took the linked deadline, and a built binary with the
// product bound refuses the call, the read killed at 30 s.
func TestTheBuiltLegionTakesItsBoundsFromTheTestBinarysDeadline(t *testing.T) {
	if _, ok := os.LookupEnv(testDeadlineEnv); !ok {
		t.Skip("this test binary runs with -timeout 0, so the built legion links no deadline")
	}
	legion := buildLegion(t)
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte(ghStandInPrefix(help)+"printf '%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := t.TempDir()
	if err := os.WriteFile(filepath.Join(wrappers, "gh"), []byte("#!/bin/sh\nexec '"+legion+"' gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	slowGit := "#!/bin/sh\nfor a; do if [ \"$a\" = --show-origin ]; then sleep " + strconv.Itoa(int(ghGitReadBound/time.Second)+1) + "; break; fi; done\nexec '" + fixturegit.CompiledGit(t) + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrappers, "git"), []byte(slowGit), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"pr", "view", "1", "--json", "state", "-R", "acme/widgets"}
	cmd := exec.Command(legion, append([]string{"gh"}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + wrappers + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"LEGION_GH_PATH=" + stub, "GH_CONFIG_DIR=" + defaultDir, "LEGION_GH_OWNERS_DIR=" + root,
		"PUBLIC_LEAK_CHECK=" + leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the built legion gh %q under a git whose configuration read answers past %s: %v, stderr %s; want it to wait, its bound the linked deadline", args, ghGitReadBound, err, stderrOf(err))
	}
	if got, want := string(out), strings.Join(args, " ")+"\n"; got != want {
		t.Fatalf("the real gh received %q, want %q", got, want)
	}
}

// stderrOf is what a run that failed printed on stderr, as exec.Cmd.Output keeps it.
func stderrOf(err error) string {
	if exit, ok := err.(*exec.ExitError); ok {
		return string(exit.Stderr)
	}
	return ""
}
