package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/clitest"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// buildLegion builds this package's `legion` into a directory of the test's own and answers its
// path: the binary a caller runs, with no test code linked in, which is what clitest and the
// invocation table drive.
func buildLegion(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "legion")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binary
}

// legionCommands is every command a person runs, by its words, its groups included: the tree of
// the spec's "Command trees" for `legion`.
var legionCommands = [][]string{
	{"version"},
	{"config"}, {"config", "check"}, {"config", "reference"},
	{"start"}, {"stop"}, {"state"}, {"legions"}, {"status"},
	{"issue"}, {"issue", "status"},
	{"restart"},
	{"claims"}, {"claims", "spawn"}, {"claims", "deliver"}, {"claims", "suspend"}, {"claims", "resume"},
	{"claims", "stop"}, {"claims", "retry"}, {"claims", "compact"}, {"claims", "forget-session"},
	{"claims", "close"}, {"claims", "list"},
	{"sessions"}, {"sessions", "import"},
	{"probe-boot"},
	{"controller"}, {"controller", "start"},
}

// legionHidden is every command only a program runs — a pod's, a pane's, git's or an image
// build's. Each sets Hidden, so neither help nor completion offers it, its group's included.
var legionHidden = [][]string{
	{"worker-shim"},
	{"launcher"}, {"launcher", "role-init"},
	{"claude-bridge"}, {"model-token"}, {"probe-image"},
	{"workspace-init"}, {"workspace-init", "fetch"}, {"workspace-init", "skills"},
	{"workspace-init", "provision"}, {"workspace-init", "cache"},
	{"skills-check"}, {"git-credential"}, {"git"}, {"gh"},
	{"leak-check"}, {"leak-check", "pre-push"}, {"leak-check", "text"},
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
		for _, command := range legionHidden {
			if len(command) < 2 {
				continue
			}
			group, name := command[:len(command)-1], command[len(command)-1]
			for _, args := range [][]string{slices.Concat(group, []string{"--help"}), slices.Concat([]string{"__complete"}, group, []string{""})} {
				run := exec.Command(legion, args...)
				run.Dir, run.Env = t.TempDir(), append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
				out, err := run.Output()
				if err != nil {
					t.Errorf("legion %s: %v", strings.Join(args, " "), err)
					continue
				}
				for line := range strings.Lines(string(out)) {
					if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
						t.Errorf("legion %s lists %q, a command only a program runs:\n%s", strings.Join(args, " "), name, out)
					}
				}
			}
		}
		if requests := recorder.Requests(); len(requests) > 0 {
			t.Errorf("help and completion asked the daemon %q", requests)
		}
	})
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
			t.Fatalf("legion gh %q: %v, stderr %s", args, err, stderrOf(err))
		}
		if got, want := string(out), strings.Join(args, " ")+"\n"; got != want {
			t.Fatalf("the real gh received %q, want %q", got, want)
		}
	})
}

// stderrOf is what a run that failed printed on stderr, as exec.Cmd.Output keeps it.
func stderrOf(err error) string {
	if exit, ok := err.(*exec.ExitError); ok {
		return string(exit.Stderr)
	}
	return ""
}
