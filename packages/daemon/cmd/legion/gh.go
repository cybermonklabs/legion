package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the repository the command names, or of its owner where the bundle holds the repository none, under LEGION_GH_OWNERS_DIR)"

// runGh is `legion gh`, the gh a pane or pod runs. It reads the repository the command acts on,
// and its owner, as gh resolves them (ghroute.TargetOf) and execs the real gh (LEGION_GH_PATH)
// as `gh` with its arguments untouched and GH_CONFIG_DIR set to a gh directory under
// LEGION_GH_OWNERS_DIR: the repository's own, `<owner>/<repo>`, whose token reaches that one
// repository, when every selector of the command named that one repository and the bundle holds
// its directory, and the owner's otherwise (repositoryGHConfigDir) — a command naming no
// repository, one naming two repositories of the one owner, one where a selector named the owner
// alone beside a repository, and one whose repository the bundle holds no directory for, which
// is every repository of an owner the role writes to one of. A command naming no owner at all,
// in a working directory whose git remotes name none, keeps GH_CONFIG_DIR, the issue's owner's.
// A command it cannot read for certain, one naming two owners, and one naming an owner the
// bundle holds no directory for are refused, exit 2, naming why, and the real gh never runs: no
// token but the target repository's or its owner's is ever sent.
//
// Before gh is handed anything, the leak guard reads the call (leakguard.GhCall): a call it
// refuses, and one whose checks could not be made to run, are refused here, exit 2, with the
// guard's own message, and gh never runs. Two calls pass unchecked: `legion gh auth
// git-credential`, which answers above before any of this, and the shared check's own read of a
// repository's visibility (ghVisibilityRead).
//
// The git gh runs itself (a clone, a fetch of a fork's parent, `pr checkout`) authenticates
// through the credential helper gh names after its own executable, GH_PATH: runGh sets it to the
// `gh` wrapper first on PATH, so that helper is `legion gh auth git-credential`, which answers as
// `legion git-credential` does, from the directory of the repository in git's path (useHttpPath set
// for github.com through GIT_CONFIG_COUNT), never with GH_CONFIG_DIR's token; when PATH's first gh
// is no such wrapper it refuses. The child also runs with gh's update check and prompts off, which a TTY
// would otherwise reach. Each of the three variables is runtime-set and required; an unset one is
// refused, exit 2, naming it. With no LEGION_GH_PATH, --help prints this command's usage, so
// `legion gh --help` outside a pane answers as every legion command does.
func runGh(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	env := processEnvironment()
	if env["LEGION_GH_PATH"] == "" && len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, ghUsage)
		return 0
	}
	for _, name := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		if env[name] == "" {
			fmt.Fprintf(stderr, "legion gh: %s is unset: a Legion pane or pod sets it, and nothing else runs this gh\n%s\n", name, ghUsage)
			return 2
		}
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "git-credential" {
		return runGitCredential(ctx, args[2:], stdout, stderr)
	}
	gh := env["LEGION_GH_PATH"]
	wrapper, err := ghWrapperPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	target, err := ghroute.TargetOf(args, env, func(path []string) (string, error) { return ghroute.ReadHelp(gh, path) }, ghroute.ReadGitRemotes)
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	// The guard, between the owner this command resolved and the gh it is about to run. The whole
	// argv is what it reads, rather than the options a parser calls text: the shared check's own
	// gh-guard reads full argv, and a private name is refusable wherever it stands. The call's
	// standard input goes with it, and whatever the guard read of it comes back to hand gh in
	// this process's place (runGhChild).
	//
	// Two calls reach gh unchecked, each for a reason of its own. `legion gh auth git-credential`
	// is one: it returned above, before any of this, because it is git's credential protocol on
	// standard input and standard output and reading that input would break it. The shared
	// check's own visibility read is the other (ghVisibilityRead).
	//
	// A *leakguard.Refusal and a *leakguard.SetupFailure each refuse the call: a check that did
	// not run has cleared nothing. Both exit 2, this command's status for a refusal made before
	// gh runs, which is what an unreadable command and an unset variable already exit with, and
	// both print the guard's own message, which names the record and the class and never the
	// text it refused.
	//
	// The files the call would send go with it, read out of the parse above, which knew each
	// option's arity from the leaf's own help (ghroute.Target.Files): the guard reads the text
	// of each, and needs no second reader of the argv for a call this command checked.
	var buffered []byte
	if !ghVisibilityRead(args, env) {
		repo := ""
		if target.Owner != "" && target.Repo != "" {
			repo = target.Owner + "/" + target.Repo
		}
		buffered, err = leakguard.New(leakguard.Options{}).GhCall(ctx, leakguard.GhRequest{
			Args:  args,
			Stdin: os.Stdin,
			Repo:  repo,
			Files: target.Files,
		})
		if err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	dir := env["GH_CONFIG_DIR"]
	if target.Owner != "" {
		if dir, err = repositoryGHConfigDir(env["LEGION_GH_OWNERS_DIR"], target.Owner, target.Repo); err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	set := map[string]string{"GH_CONFIG_DIR": dir, "GH_PATH": wrapper, "GH_NO_UPDATE_NOTIFIER": "1", "GH_PROMPT_DISABLED": "1"}
	childEnv := make([]string, 0, len(env)+len(set)+3)
	for _, pair := range os.Environ() {
		if name, _, _ := strings.Cut(pair, "="); set[name] == "" {
			childEnv = append(childEnv, pair)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		childEnv = append(childEnv, name+"="+set[name])
	}
	if childEnv, err = withGitHubUseHTTPPath(childEnv); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	return runGhChild(ctx, gh, args, childEnv, buffered, stderr)
}

// ghVisibilityRead is whether argv is the shared public leak check's own read of a repository's
// visibility, the one gh call the guard above never sees. A pod's gh is `legion gh`, so the
// check's read comes back through this command: checking it would run the check, whose own
// visibility read would come back here again, and the first write to a repository of another
// owner would recurse without end.
//
// The match is the check's own, as its gh guard makes it: exactly the argv
//
//	gh api --hostname github.com repos/<slug> --jq .visibility
//
// for exactly the slug the check's variable names, which must itself be a repository GitHub
// could have (ghrepo.Parse), so that a variable someone else set lets through one read of one
// repository and nothing else. Both halves come from leakguard, whose own visibility reader
// (Guard.ghVisibility) is built from the same two, so the exemption and the read it exempts
// cannot part.
func ghVisibilityRead(argv []string, env map[string]string) bool {
	slug := env[leakguard.VisibilityReadVar]
	if slug == "" {
		return false
	}
	if _, err := ghrepo.Parse(leakguard.VisibilityReadVar, slug); err != nil {
		return false
	}
	return slices.Equal(argv, leakguard.VisibilityReadArgs(slug))
}

// runGhChild runs the real gh as a child of this process and is the status to exit with: gh's
// own exit status, and 128+<signal> for a gh a signal killed, which is the status a shell reads
// such an end as.
//
// gh is a child rather than this process exec'd into, as it was before the guard: the guard may
// have read this process's standard input to check it, and exec cannot put those bytes back,
// while a child can be handed them (buffered, nil where the guard read none and gh gets this
// process's own standard input whole). Standard output and standard error are this process's
// own descriptors, so gh's reading of its terminal — its colour, its spinner, its prompts — is
// what an exec'd gh would have read.
//
// This child is given no process group of its own (no procgroup.Configure), unlike every other
// child of this module. gh is an interactive program — its pager and its editor read the
// terminal and change its mode — and a process group that is not the terminal's foreground
// group is stopped by SIGTTIN or SIGTTOU the moment it does either, which is a `gh pr view`
// that hangs in a pane with no way out. Sharing this process's group is what an exec'd gh had:
// a terminal's own SIGINT reaches gh with the rest of the group, so there is nothing to
// forward, and a `legion gh` run outside a terminal is in its parent's group exactly as the
// exec was.
//
// What the terminal does not send is forwarded instead: a SIGTERM or a SIGHUP sent to this
// process's own pid (forwardTo). An exec'd gh ended on either, and without this a SIGTERM would
// reach this process alone — main's signal.NotifyContext catches it and the child's context has
// the cancellation taken off — leaving gh running with nobody waiting for it, and a SIGHUP,
// which nothing here would catch, would kill this process where it stood and leave gh running.
// Each goes to child.Process, the pid and not the group: the group is the terminal's to signal,
// and a process-group signal from here would hit this process again and every sibling of it.
// SIGHUP is armed only where this process was not started with that signal ignored, because
// arming it would undo a `nohup gh …` (the comment on the signal.Notify below).
//
// The forwarding is armed before the child is started, so that a signal arriving in the moment
// between the fork and the wait is held in the channel rather than lost: until this process
// holds a pid there is nothing to forward to, and the two names would otherwise be the
// swallowed SIGTERM and the fatal SIGHUP again, in a window a loaded machine makes wide. A
// signal that came before that arming is in no channel at all, and is read from this context's
// cause instead (endedBySignal): it ends the command with 128+<signal> and gh is never
// started, where before it was swallowed whole and gh ran on as if nothing had been sent.
//
// Its context is this process's with the cancellation taken off (context.WithoutCancel): the
// cancellation is not wanted — a signal that ends this process's context reaches gh as the
// signal it was, forwarded here or from the terminal, and gh ends on it and answers with its
// own status, where the cancellation would kill gh outright, mid-clone, and answer with a
// status nobody sent.
func runGhChild(ctx context.Context, gh string, args, env []string, buffered []byte, stderr io.Writer) int {
	child := exec.CommandContext(context.WithoutCancel(ctx), gh, args...)
	child.Env = env
	child.Stdin = os.Stdin
	if buffered != nil {
		child.Stdin = bytes.NewReader(buffered)
	}
	// The forwarding is armed here, before the fork, and a signal that arrives before there is
	// a pid to send it to waits in this channel (forwardTo).
	//
	// SIGHUP is armed only where this process was not started with it ignored. signal.Notify
	// installs a handler for every name it is given — the signal "will no longer be ignored",
	// as os/signal's own doc puts it — and execve resets a caught signal to its default action
	// in the child, so under `nohup gh …`, which execs through the wrapper into this command
	// with SIGHUP ignored, arming it would hand gh a SIGHUP that kills it: the one thing nohup
	// was asked to prevent. Left unarmed, the ignoring is gh's as it is this process's, both
	// inherited across the exec, and there is nothing to forward because nothing reaches
	// either. SIGTERM is armed either way: main's own signal.NotifyContext (main.go) has
	// caught it since before this ran, whatever this process was started with.
	//
	// signal.Ignored answers for SIGHUP because the runtime records it: initsig leaves SIGHUP
	// and SIGINT alone where they were inherited as SIG_IGN and marks them ignored
	// (runtime's sigInitIgnored). agent-secrets asks the same question of the job-control
	// signals with a raw sigaction, for which that bookkeeping holds no answer
	// (packages/envoy/cmd/agent-secrets/secret_signal_linux.go's promptSignalIgnored).
	forwarded := []os.Signal{syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		forwarded = append(forwarded, syscall.SIGHUP)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, forwarded...)
	defer signal.Stop(signals)
	// A signal that came before the arming above left nothing in that channel: main's
	// signal.NotifyContext took it, cancelled this context with a cause naming it and raised it
	// no further, and the child's context drops the cancellation. gh is not started for it.
	if cause := context.Cause(ctx); cause != nil {
		if status, ok := endedBySignal(cause); ok {
			return status
		}
		// A cancellation this command cannot name a signal for is no death a shell reads as a
		// status: it is gh not started, and that is what it answers with.
		fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, cause)
		return 1
	}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, err)
		return 1
	}
	defer forwardTo(child.Process, signals)()
	err := child.Wait()
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, err)
		return 1
	}
	if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exited.ExitCode()
}

// mainNotified are the signals main's context is cancelled by (main.go's signal.NotifyContext),
// which are the only ones a cause of this process's context can name.
var mainNotified = []syscall.Signal{syscall.SIGINT, syscall.SIGTERM}

// endedBySignal is the status for a context whose cause is one of those signals, and whether it
// is one: 128+<signal>, the status a shell reads a death by that signal as, which is what
// runGhChild answers for a gh a signal killed and what this command answers for a signal that
// came before gh was started at all.
//
// The cause is where that signal is left: signal.NotifyContext's own goroutine takes the signal,
// cancels with a cause naming it and raises it no further. Each name is matched against the
// cause os/signal makes of that signal, the signal's own name and " signal received" (its
// signalCtx), built here from the signal rather than written out again: the error it makes
// carries no signal to read, so the words are the only way back to the name, and the two
// spellings of them are one edit of os/signal apart.
func endedBySignal(cause error) (status int, ok bool) {
	for _, sig := range mainNotified {
		if cause.Error() == sig.String()+" signal received" {
			return 128 + int(sig), true
		}
	}
	return 0, false
}

// forwardTo sends each signal it reads from signals on to process, until the returned stop is
// called. Its caller arms that channel (signal.Notify) before it starts the child and calls
// this once there is a pid: a signal that arrived in between is in the channel and goes on as
// soon as this runs, so neither name is lost in the moment between the fork and the wait.
//
// The signal goes to the one pid, never to the group: gh shares this process's group, and a
// signal sent to the group would come back to this process and reach every sibling in it. The
// two names are what an exec'd gh ended on and what this process is itself ended by — a pane's
// server sends SIGHUP to everything in it when the pane goes, and a stop sends SIGTERM. Where
// the sender reached gh as well, the forwarded copy is a second signal, which costs nothing: gh
// died on the first, and the second finds a pid that is gone, as a closing pane's SIGHUP does.
// SIGINT is left out for a reason of its own rather than that one: the terminal is its sender
// here, and it sends it to the whole foreground group every time, so there is no SIGINT this
// forwarding would be gh's only way of hearing.
//
// It is for a process its caller waits for: stop ends the forwarding once that wait is over,
// and a signal that arrives between the child's exit and stop is sent to a pid that is gone,
// which Signal answers with an error this drops. The caller's channel is buffered, so the
// runtime's own delivery never blocks on this.
func forwardTo(process *os.Process, signals <-chan os.Signal) (stop func()) {
	done := make(chan struct{})
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for {
			select {
			case sig := <-signals:
				_ = process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-forwarded
	}
}

// ghWrapperScript is the `gh` wrapper both runtimes install first on PATH: a script whose one
// command execs `legion gh` with its arguments (packages/daemon/docker/gh, workerbin.Install),
// the legion path absolute, so the shell's exec reads it as no option, and either a bare word of
// the characters a shell reads literally (shellprefix's safe set) or quoted, in single quotes
// (shellprefix.Literal) or in double quotes with no expansion.
var ghWrapperScript = regexp.MustCompile(`^#!/bin/sh\nexec ('/(?:[^']|'\\'')*'|"/[^"$` + "`" + `\\]*"|/[A-Za-z0-9_./:-]*) gh "\$@"\n?$`)

// ghWrapperMaxBytes bounds the wrapper read: a script longer is refused whole rather than matched
// by its start. A wrapper whose quoted legion executable path takes it past the bound (a path near
// PATH_MAX, or one whose single quotes, four bytes each once quoted, grow it that far) is refused
// too, which refuses every command `legion gh` would hand the real gh rather than letting one
// through; `legion gh auth git-credential`, answered before the check, answers by git's path alone.
const ghWrapperMaxBytes = 4096

// ghWrapperPath is the `gh` first on PATH, the wrapper that runs `legion gh`, as an absolute
// path: gh names its git credential helper after it (GH_PATH). It is refused when PATH's first gh
// is no such wrapper (isGhWrapper), or there is none: the real gh itself would answer every
// github.com request with GH_CONFIG_DIR's token, and a machine's own wrapper with whatever it
// chooses.
func ghWrapperPath() (string, error) {
	wrapper, err := exec.LookPath("gh")
	if err == nil {
		wrapper, err = filepath.Abs(wrapper)
	}
	if err != nil {
		return "", fmt.Errorf("no gh on PATH runs `legion gh` (%v), so the git gh runs would not authenticate through it; run it as a Legion pane's gh", err)
	}
	file, err := os.Open(wrapper)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wrapper, err)
	}
	defer file.Close()
	ok, err := isGhWrapper(file)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wrapper, err)
	}
	if !ok {
		return "", fmt.Errorf("the first gh on PATH, %s, is not the wrapper that runs `legion gh`, so the git gh runs would not authenticate through it; run it as a Legion pane's gh", wrapper)
	}
	return wrapper, nil
}

// isGhWrapper says whether r holds, whole, a script ghWrapperScript accepts: one longer than
// ghWrapperMaxBytes is none.
func isGhWrapper(r io.Reader) (bool, error) {
	script, err := io.ReadAll(io.LimitReader(r, ghWrapperMaxBytes+1))
	if err != nil {
		return false, err
	}
	return len(script) <= ghWrapperMaxBytes && ghWrapperScript.Match(script), nil
}

// withGitHubUseHTTPPath adds credential.https://github.com.useHttpPath=true to env's GIT_CONFIG_COUNT
// entries, which git reads as it reads `-c`, so the git gh runs tells its credential helper the
// repository's path, whose owner `legion git-credential` answers for. A GIT_CONFIG_COUNT that is no
// count is refused.
func withGitHubUseHTTPPath(env []string) ([]string, error) {
	count := 0
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, "GIT_CONFIG_COUNT="); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("GIT_CONFIG_COUNT is %q, which is no count, so git's configuration cannot be extended; unset it", value)
			}
			count = n
		}
	}
	key, value := fmt.Sprintf("GIT_CONFIG_KEY_%d", count), fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)
	out := make([]string, 0, len(env)+3)
	for _, pair := range env {
		if name, _, _ := strings.Cut(pair, "="); name != "GIT_CONFIG_COUNT" && name != key && name != value {
			out = append(out, pair)
		}
	}
	return append(out, key+"=credential.https://github.com.useHttpPath", value+"=true", fmt.Sprintf("GIT_CONFIG_COUNT=%d", count+1)), nil
}
