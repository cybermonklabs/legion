// Package fixturegit runs the git and jj commands a test's fixture is made of. A fixture builds a
// remote, a clone and a workspace out of the real tools, and every one of those commands needs the
// same three things: the directory it runs in, because jj searches upward from the directory it is
// started in for the closest .jj and a command with none would open the workspace the test process
// stands in, which in a Legion issue pod is a real one; an environment of its own, which is
// fixtureenv.Isolated's wherever the fixture has no reason for another, so no command reads the
// repository, configuration or credential of whoever runs the tests; and a failure that names what
// was run, where, and everything it printed. Runner holds the test and that environment, and its
// four ways of running one command are the four a fixture has: Run for the usual one, Raw where
// the output's own whitespace is part of what is read, Out where what is read must hold no stderr,
// and Try where the caller keeps the failure for itself.
//
// The repositories those fixtures are built out of have one argv apiece, which InitBare,
// InitWorkTree and JJInit return whole, command name and all, so no fixture spells the flags of an
// init itself and no two of them spell one flag differently.
//
// It is an ordinary package rather than a _test.go file of one of them because cmd/legion is
// package main, which can import no other package's test-only helpers.
package fixturegit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
)

// Runner runs a fixture's git or jj commands under one environment. Env is the whole environment
// of each child, as exec.Cmd takes it, so a fixture that hands its commands a handful of variables
// of its own passes them as they are. There is no binary: argv[0] names it, so a fixture that runs
// a pinned git or jj passes that path itself. Ctx, when set, bounds each command: the command is
// killed when Ctx ends, and its failure names Ctx's cause (context.Cause) before what was run.
type Runner struct {
	TB  testing.TB
	Env []string
	Ctx context.Context
}

// Isolated is a Runner whose commands run under fixtureenv.Isolated of home, which is the
// environment most of these fixtures want: one that reaches no workspace, repository, remote,
// credential or session store but the test's own.
func Isolated(tb testing.TB, home string) Runner {
	return Runner{TB: tb, Env: fixtureenv.Isolated(home)}
}

// CompiledGit is the first git on the test process's PATH that is a compiled program
// (lookpath.Kind), for a test that runs a real git under a git wrapper of its own: in a Legion pod
// the first git, /opt/legion/bin/git, is a script that wraps /usr/bin/git, and on CI the first git
// is the runner's own. The test fails when PATH holds none.
func CompiledGit(tb testing.TB) string {
	tb.Helper()
	return compiled(tb, "git")
}

// CompiledJJ is the first jj on the test process's PATH that is a compiled program
// (lookpath.Kind), for a test that runs the real jj, itself or under a jj wrapper of its own. A
// machine may put a script of its own in front of jj: a dev box's dotfiles shim reads a push's
// plan with the first git on PATH and refuses the push when that git answers nothing, as a test's
// logging stand-in git does, so the push never reaches jj. On CI the first jj is the binary the
// install-jj action puts at ~/.local/bin/jj. The test fails when PATH holds none.
func CompiledJJ(tb testing.TB) string {
	tb.Helper()
	return compiled(tb, "jj")
}

// compiled is the first program called name on the test process's PATH that is a compiled
// program, failing the test, with each one it passed over, where PATH holds none.
func compiled(tb testing.TB, name string) string {
	tb.Helper()
	found, _ := lookpath.Others(os.Getenv("PATH"), name, "")
	passed := make([]string, 0, len(found))
	for _, each := range found {
		if _, isCompiled, err := lookpath.Kind(each.Path); err == nil && isCompiled {
			return each.Path
		}
		passed = append(passed, each.Path)
	}
	if len(passed) == 0 {
		tb.Fatalf("PATH holds no %s at all: this test runs a real %s", name, name)
	}
	tb.Fatalf("no %s on PATH is a compiled program, so none is a real %s: %s", name, name, strings.Join(passed, ", "))
	return ""
}

// With is the runner with extra added to its environment, as a copy, so the one it was made from
// is unchanged and two runners of one fixture do not share an array. A key in extra wins over the
// same key in Env, as exec.Cmd takes the last value of a key.
func (r Runner) With(extra ...string) Runner {
	env := make([]string, 0, len(r.Env)+len(extra))
	env = append(append(env, r.Env...), extra...)
	return Runner{TB: r.TB, Env: env}
}

// Run runs argv in dir and returns its combined output, trimmed, failing the test when the command
// fails with what was run, the directory it ran in, and everything it printed.
func (r Runner) Run(dir string, argv ...string) string {
	r.TB.Helper()
	return strings.TrimSpace(r.Raw(dir, argv...))
}

// Raw is Run without the trim, for a caller that reads the output's own whitespace: an empty value
// a trim would take away, or a line count.
func (r Runner) Raw(dir string, argv ...string) string {
	r.TB.Helper()
	output, err := r.command(dir, argv).CombinedOutput()
	if err != nil {
		r.TB.Fatalf("%s%s in %s: %v\n%s", r.cause(), strings.Join(argv, " "), dir, err, output)
	}
	return string(output)
}

// Out runs argv in dir and returns its standard output alone, untrimmed, for a caller that reads a
// value git or jj printed rather than a transcript: a progress line on stderr would otherwise be
// part of the value. What the command printed on stderr appears in the failure instead.
func (r Runner) Out(dir string, argv ...string) string {
	r.TB.Helper()
	command := r.command(dir, argv)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		r.TB.Fatalf("%s%s in %s: %v\n%s", r.cause(), strings.Join(argv, " "), dir, err, stderr.String())
	}
	return string(output)
}

// Try runs argv in dir and returns its combined output, untrimmed, and the command's error,
// failing nothing itself: a caller whose failure text is its own, or one for which a command that
// fails is an answer rather than a failure, takes the error.
func (r Runner) Try(dir string, argv ...string) (string, error) {
	r.TB.Helper()
	output, err := r.command(dir, argv).CombinedOutput()
	return string(output), err
}

// command is argv in dir under the runner's environment. With Ctx set the command leads a process
// group of its own, and when Ctx ends the whole group is killed, so no child the command started
// (a git that a jj runs, say) outlives it; a child that still holds the output then is cut off a
// second later (WaitDelay), so the wait ends too.
func (r Runner) command(dir string, argv []string) *exec.Cmd {
	command := exec.Command(argv[0], argv[1:]...)
	if r.Ctx != nil {
		command = exec.CommandContext(r.Ctx, argv[0], argv[1:]...)
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
		command.WaitDelay = time.Second
	}
	command.Dir = dir
	command.Env = r.Env
	return command
}

// cause is Ctx's cause and a separator once Ctx has ended, and nothing before.
func (r Runner) cause() string {
	if r.Ctx == nil || r.Ctx.Err() == nil {
		return ""
	}
	return fmt.Sprintf("%v: ", context.Cause(r.Ctx))
}

// InitBare is the argv of a bare git repository in dir, which is what a fixture's remote is: a
// repository with no working tree of its own, whose HEAD names branch.
func InitBare(dir, branch string) []string {
	return gitInit(dir, branch, true)
}

// InitWorkTree is the argv of a git repository with a working tree in dir, which is what a
// fixture's seed or clone is. It reads dir and branch as InitBare does.
func InitWorkTree(dir, branch string) []string {
	return gitInit(dir, branch, false)
}

// gitInit is git init in dir, bare when the repository is to have no working tree. It is always
// quiet, because what init prints is a sentence about the directory it made and no fixture reads
// it. An empty branch leaves --initial-branch out, so the repository takes the default branch name
// git itself holds: under fixtureenv.Isolated, which shuts out both configuration files, that is
// git's compiled-in one, and naming a branch there would make a repository other than the one the
// fixture asks for. An empty dir leaves the directory out, for a fixture that gives its runner the
// directory to run in instead.
func gitInit(dir, branch string, bare bool) []string {
	argv := make([]string, 0, 6)
	argv = append(argv, "git", "init", "--quiet")
	if bare {
		argv = append(argv, "--bare")
	}
	if branch != "" {
		argv = append(argv, "--initial-branch="+branch)
	}
	if dir != "" {
		argv = append(argv, dir)
	}
	return argv
}

// JJInit is the argv of a jj repository in dir, colocated with a git repository beside it when
// colocate and holding its git repository under .jj alone when not. The two are different
// repositories rather than two spellings of one, so each call site keeps the one it asks for. An
// empty dir leaves the directory out, as InitBare does.
func JJInit(dir string, colocate bool) []string {
	colocation := "--no-colocate"
	if colocate {
		colocation = "--colocate"
	}
	argv := make([]string, 0, 5)
	argv = append(argv, "jj", "git", "init", colocation)
	if dir != "" {
		argv = append(argv, dir)
	}
	return argv
}
