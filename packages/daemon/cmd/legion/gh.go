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
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the owner the command names, under LEGION_GH_OWNERS_DIR)"

// runGh is `legion gh`, the gh a pane or pod runs. It reads the repositories the command acts on,
// and their owners, as gh resolves them (ghTargetOf) and execs the real gh (LEGION_GH_PATH) as
// `gh` with its arguments untouched and GH_CONFIG_DIR set to a gh directory under
// LEGION_GH_OWNERS_DIR: the one directory that answers for every repository the command names
// (ghConfigDirFor), which is the owner's own — the one directory the bundle holds for that
// account, whose token reaches every repository its installation holds — except for a
// repository a configured rename moved to another owner, which is answered from the directory
// of the owner holding it now. Every repository the command names is routed so, each on its own,
// and every owner a selector names alone is routed to that owner's own directory, and they must
// all be one directory: a command naming one or two of an owner's repositories no rename moved,
// or the owner alone, runs with the owner's one directory, since the bundle holds no narrower
// token to run it with, and one naming a moved repository by both of its names runs with the
// directory of the owner holding it now. A command naming no owner at all, in a working
// directory whose git remotes name none, keeps GH_CONFIG_DIR: the directory the issue's
// repository routes to (ghconfig.RouteDir), or a controller's default owner's. A command it
// cannot read for certain, one whose repositories route to two directories (two owners'
// repositories, or a repository a rename moved beside another of the owner it moved from), one
// naming an owner the bundle holds no directory for — an owner the role's App is not installed
// on — and one naming a repository a pair moved to an owner the bundle holds no directory for are
// refused, exit 2, naming why, and the real gh never runs: no token is ever sent for a repository
// it cannot reach. Under a GH_TOKEN or GITHUB_TOKEN, which gh sends in place of any directory's
// token, every command is refused the same way but these: the guard's own visibility reads, which
// run with that token (ghVisibilityRead), the shared check's visibility read
// (sharedCheckVisibilityRead), the exact `gh api rate_limit` (ghRateLimitRead) and `gh auth
// git-credential`.
//
// ghTargetOf reads each command from gh's own help, which gh prints the same way until its
// binary, its configuration, its extensions or the variables it prints under change: runGh keeps
// each help it reads under the role's state directory and reads it from gh again only after such
// a change (ghHelpKeep). The visibility reads named above and the rate-limit read are exact argv
// and read no help at all.
//
// Before gh is handed anything, the leak guard reads the call (leakguard.GhCall): a call it
// refuses, and one whose checks could not be made to run, are refused here, exit 2, with the
// guard's own message, and gh never runs. A call that writes nothing, by the rule ghWritesNothing
// holds and under the shared check that rule was proven against (ghReadsProvenCheck), is read by
// that check alone and gets no read of a repository's visibility and no private-name pass. Three
// calls pass unchecked: `legion gh auth git-credential`, which answers above before any of this,
// the exact `gh api rate_limit` (ghRateLimitRead), and the guard's own visibility reads
// (ghVisibilityRead). The shared check's visibility read this command answers itself
// (sharedCheckVisibilityRead), before it reads anything else of the call. gh runs the rate-limit
// read, the guard's own visibility reads and each call the shared check alone read with no pager
// and no browser program from the environment.
//
// The git gh runs itself (a clone, a fetch of a fork's parent, `pr checkout`) authenticates
// through the credential helper gh names after its own executable, GH_PATH: runGh sets it to the
// `gh` wrapper first on PATH, so that helper is `legion gh auth git-credential`, which answers as
// `legion git-credential` does, from the directory of the owner in git's path (useHttpPath set
// for github.com through GIT_CONFIG_COUNT), never with GH_CONFIG_DIR's token; when PATH's first gh
// is no such wrapper it refuses. The child also runs with gh's update check and prompts off, which a TTY
// would otherwise reach. Each of the three variables is runtime-set and required; an unset one is
// refused, exit 2, naming it. With no LEGION_GH_PATH, --help prints this command's usage, so
// `legion gh --help` outside a pane answers as every legion command does.
//
// The real gh never leads back to this command. A LEGION_GH_PATH that is not an absolute path, and
// one that is a `legion gh` wrapper — the one first on PATH, or any script of that form — are
// refused, exit 2, before anything runs (refuseLegionGhAsRealGh). One that leads back only when it
// runs, such as a machine's own gh shim that hands every call to the next gh on PATH, which is the
// wrapper, is caught when it does: every real gh this command runs, for a help read or for the
// call itself, is given leakguard.RunByRealGhVar, and a `legion gh` started under it refuses, exit
// 2, so the help read that reached it fails and nothing runs again. The leak guard blanks the
// variable for every child it runs (leakguard's Guard.env says why): a push a real gh makes reaches
// the guard with it set, through git's pre-push hook, and the guard's visibility reads, which come
// back through this command, are the guard's and not that gh's. Each such read runs its own real
// gh under the variable again, so a real gh that leads back is still refused one start later, with
// or without a caller's GH_TOKEN.
//
// Everything this command starts before gh — the help reads (readGhHelp), and the leak guard's
// children, the shared check and the visibility reads, which the guard starts under the context
// it is given (leakguard's Guard.run) — runs in a process group of its own, which none of the
// terminal's signals reach. So this command stops them itself: on a SIGINT or a SIGTERM, which
// cancel its context (main's signal.NotifyContext), and on a SIGHUP, which it catches until it
// hands over to runGhChild's own forwarding (hangupCancels). Such a signal ends the command with
// 128+<signal> (endedBySignal), and gh is never started, as for a signal that came before
// runGhChild's forwarding.
func runGh(ctx context.Context, args []string, stdout, stderr io.Writer) (status int) {
	env := processEnvironment()
	repo, commandPath, visibilitySource := "", "", ""
	logCall := true
	defer func() {
		if logCall {
			appendGhCallLog(env, repo, commandPath, visibilitySource, status)
		}
	}()
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
		commandPath = "auth git-credential"
		return runGitCredential(ctx, args[2:], stdout, stderr)
	}
	// Everything this command starts from here until gh runs under beforeGh, which a SIGHUP
	// cancels as well (hangupCancels): each runs in a process group of its own, which a closing
	// pane's SIGHUP does not reach, and the cancel is what stops that group. A signal that stopped
	// one ends the command with its status rather than with the refusal its stopped work made.
	beforeGh, stopHangup := hangupCancels(ctx)
	defer stopHangup()
	if read, ok := sharedCheckVisibilityRead(args, env); ok {
		// The shared check's own read, which this command answers by the guard's rule. Like the
		// guard's own reads, it is not logged as a wrapped call.
		logCall = false
		return answerVisibilityRead(beforeGh, read, stdout, stderr)
	}
	if real := env[leakguard.RunByRealGhVar]; real != "" {
		fmt.Fprintf(stderr, "legion gh: the real gh %s ran `legion gh` again, so it is no real gh but a wrapper or shim that leads back to this one; set LEGION_GH_PATH to the real gh's install path\n", real)
		return 2
	}
	gh := env["LEGION_GH_PATH"]
	wrapper, err := ghWrapperPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	if err := refuseLegionGhAsRealGh(gh); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	rateLimitRead := ghRateLimitRead(args)
	visibilityRead, isVisibilityRead := ghVisibilityRead(args, env)
	// What the call targets, and the gh directory it runs with, chosen here once for every call.
	// The two exact reads go through no ghTargetOf, and so read no gh help: the resolver has
	// nothing of their argv to read.
	var target ghTarget
	switch {
	case rateLimitRead:
		// It names no repository, and runs with GH_CONFIG_DIR.
		target.command = "api"
	case isVisibilityRead:
		// The guard's own request. Its enclosing user call records the source of that request, so
		// logging it again would count one wrapped call twice.
		logCall = false
		target.command = "api"
		// Its token is that of the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names, which
		// ghVisibilityRead matched to the read's exact argv, routed by the one rule
		// (ghConfigDirFor): a configured rename serves the repository from the `to` owner's
		// directory, and the `from` owner's token cannot read it. The GraphQL read names its
		// repository only in variables, so the variable is where its owner and name are read.
		// Its argv names the host, `--hostname github.com`, so no GH_HOST sends it elsewhere.
		//
		// Under the caller's GH_TOKEN or GITHUB_TOKEN it keeps GH_CONFIG_DIR and is routed by
		// nothing, because gh sends that token in place of any directory's: the bundle may hold
		// no directory for the read's repository at all, and this routing would then refuse a
		// read the caller's token can make, which is the second reason #4219 gives for running
		// these reads under it (ghVisibilityRead).
		if ghCallerToken(env) == "" {
			target.owner, target.repo = strings.ToLower(visibilityRead.Owner()), visibilityRead.Name()
			target.dir, err = ghConfigDirFor(env["LEGION_GH_OWNERS_DIR"], target.owner, target.repo)
		}
	default:
		// Every repository the command names is routed through the bundle's own pick, and the
		// command refused where they route to two directories (selectors.dir), before the guard
		// reads anything of it. gh's help of each command on the way is read once per gh binary
		// and kept for the calls after (ghHelpKeep).
		ownersDir := env["LEGION_GH_OWNERS_DIR"]
		target, err = ghTargetOf(args, env, newGhHelpKeep(beforeGh, gh, env).read, readGitRemotes,
			func(owner, name string) (string, error) { return ghConfigDirFor(ownersDir, owner, name) })
		if status, ok := signalled(beforeGh); ok {
			return status
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	commandPath = target.command
	if target.owner != "" && target.repo != "" {
		repo = target.owner + "/" + target.repo
	}
	// The guard, between the directory this command resolved and the gh it is about to run. The
	// whole argv is what it reads, rather than the options a parser calls text: the shared check's
	// own gh-guard reads full argv, and a private name is refusable wherever it stands. The call's
	// standard input goes with it, and whatever the guard read of it comes back to hand gh in
	// this process's place (runGhChild).
	//
	// Three calls reach gh unchecked, each for a reason of its own. `legion gh auth
	// git-credential` is one: it returned above, before any of this, because it is git's
	// credential protocol on standard input and standard output and reading that input would
	// break it. The guard's own visibility reads (ghVisibilityRead) are another; the shared
	// check's read returned above too, answered by this command itself (answerVisibilityRead).
	// The exact `gh api rate_limit` GET reads no repository content and GitHub excludes it from
	// the limit, so it is the third, answered before the command is read at all; a method or
	// endpoint other than that exact argv stays guarded. Each is decided before a call that
	// writes nothing: `auth git-credential` and the shared check's read return above, and
	// ghCallModeOf takes the rate-limit read and the guard's own reads first, so none of them is
	// ever taken for one.
	//
	// A call that writes nothing (ghWritesNothing, decided from the words it is typed with) is
	// handed to the guard as one (leakguard.GhRequest.WritesNothing), with the sha256 of the
	// shared check its rule was proven against (ghReadsProvenCheck). Under that check the guard
	// runs the check and nothing more, and says so (leakguard.GhAnswer.CheckedOnly), and the log
	// line says checked-only; under any other it runs whole. Every other call is guarded whole,
	// the visibility read and the private-name pass included.
	//
	// A *leakguard.Refusal and a *leakguard.SetupFailure each refuse the call: a check that did
	// not run has cleared nothing. Both exit 2, this command's status for a refusal made before
	// gh runs, which is what an unreadable command and an unset variable already exit with, and
	// both print the guard's own message, which names the record and the class and never the
	// text it refused.
	//
	// The files the call would send go with it, read out of the parse above, which knew each
	// option's arity from the leaf's own help (ghSentFiles): the guard reads the text of each,
	// and needs no second reader of the argv for a call this command checked.
	var buffered []byte
	checkedOnly := false
	mode := ghCallModeOf(rateLimitRead, isVisibilityRead, target.writesNothing)
	switch mode {
	case ghRateLimitCall:
		// This read has no repository or body for the guard to inspect.
	case ghVisibilityCall:
		// This read is the guard's own, which the switch above routed: checking it would call
		// the guard again.
	case ghReadCall, ghGuardedCall:
		guard := leakguard.New(leakguard.Options{
			OnVisibility: func(source leakguard.VisibilitySource) {
				visibilitySource = string(source)
			},
		})
		answer, err := guard.GhCall(beforeGh, leakguard.GhRequest{
			Args:          args,
			Stdin:         os.Stdin,
			Repo:          repo,
			Files:         target.files,
			WritesNothing: mode == ghReadCall,
			ProvenCheck:   ghReadsProvenCheck.sha256,
		})
		if status, ok := signalled(beforeGh); ok {
			return status
		}
		if err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
		buffered, checkedOnly = answer.Stdin, answer.CheckedOnly
		if checkedOnly {
			visibilitySource = ghCheckedOnlyVisibility
		}
	}
	dir := env["GH_CONFIG_DIR"]
	// Every mode runs with the directory chosen once above: by ghTargetOf, through the one rule
	// (ghConfigDirFor), refusing a command whose repositories route to two, and for the guard's
	// visibility read by that rule for the repository its variable names. No mode picks a
	// directory any other way.
	if target.dir != "" {
		dir = target.dir
	}
	set := map[string]string{"GH_CONFIG_DIR": dir, "GH_PATH": wrapper, "GH_NO_UPDATE_NOTIFIER": "1", "GH_PROMPT_DISABLED": "1", leakguard.RunByRealGhVar: gh}
	// A call no private-name pass read runs no program the environment names: such a program
	// runs with this call's token directory and could write what that pass never read. Its pager
	// is cat, which gh reads as no pager (GH_PAGER outranks gh's config `pager` and PAGER; gh
	// starts a pager only when its standard output is a terminal). And it is given no browser:
	// GH_BROWSER and BROWSER are removed. gh's config `browser` is left, because gh reads it only
	// to open a page, which such a call never does: a leaf of ghReadLeaves opens one only under
	// --web, which no read grammar names, so a call giving it is guarded whole (ghWritesNothing),
	// and `gh api` opens none. A call guarded whole keeps all three as they were.
	var unset map[string]bool
	if mode == ghRateLimitCall || mode == ghVisibilityCall || checkedOnly {
		set["GH_PAGER"] = "cat"
		unset = ghUnguardedUnset
	}
	childEnv := make([]string, 0, len(env)+len(set)+3)
	for _, pair := range os.Environ() {
		if name, _, _ := strings.Cut(pair, "="); set[name] == "" && !unset[name] {
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
	// runGhChild arms a forwarding of its own, so the SIGHUP watch ends here, and a SIGHUP it took
	// in its last moment, which nothing else will end the command on, is read after it ends.
	stopHangup()
	if status, ok := signalled(beforeGh); ok {
		return status
	}
	return runGhChild(ctx, gh, args, childEnv, buffered, stderr)
}

// ghCallMode is what runGh does with a call before gh runs it, decided once per call
// (ghCallModeOf).
type ghCallMode int

const (
	// ghGuardedCall runs the whole guard: the shared check, the destination's visibility read
	// and the private-name pass.
	ghGuardedCall ghCallMode = iota
	// ghReadCall is a call that writes nothing (ghWritesNothing): the guard runs the shared
	// check alone where that check is the one the rule was proven against, and whole otherwise.
	ghReadCall
	// ghRateLimitCall is the exact `gh api rate_limit` (ghRateLimitRead), which runs no guard.
	ghRateLimitCall
	// ghVisibilityCall is one of the guard's own visibility reads, REST or GraphQL
	// (ghVisibilityRead), which runs no guard, writes no line of the call log and takes the token
	// of the repository it reads, or the caller's GH_TOKEN or GITHUB_TOKEN where one is set.
	ghVisibilityCall
)

// ghCallModeOf is the mode of a call from what runGh read of it: whether it is the exact
// rate-limit read, one of the guard's own visibility reads, and a call that writes nothing, in
// that order. The shared check's own visibility read never reaches this: runGh answers it first
// (sharedCheckVisibilityRead), as it answers `auth git-credential`.
func ghCallModeOf(rateLimitRead, visibilityRead, writesNothing bool) ghCallMode {
	switch {
	case rateLimitRead:
		return ghRateLimitCall
	case visibilityRead:
		return ghVisibilityCall
	case writesNothing:
		return ghReadCall
	}
	return ghGuardedCall
}

const ghCallLogBytes = 1 << 20

func appendGhCallLog(env map[string]string, repo, commandPath, visibility string, status int) {
	state := env["XDG_STATE_HOME"]
	if state == "" {
		return
	}
	dir := filepath.Join(state, "legion-gh")
	if !ghStateDir(dir, os.Getuid()) {
		return
	}
	lock, err := os.OpenFile(filepath.Join(dir, "calls.log.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if err := lock.Chmod(0o600); err != nil {
		return
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	path := filepath.Join(dir, "calls.log")
	log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if err := log.Chmod(0o600); err == nil {
		_, _ = fmt.Fprintf(log, "time=%s role=%s issue=%s repository=%s path=%s visibility=%s status=%d\n",
			time.Now().UTC().Format(time.RFC3339Nano),
			ghCallLogValue(env["LEGION_ROLE"]),
			ghCallLogValue(env["LEGION_ISSUE"]),
			ghCallLogValue(repo),
			ghCallLogValue(commandPath),
			ghCallLogValue(visibility),
			status,
		)
	}
	_ = log.Close()
	trimGhCallLog(path)
}

func ghCallLogValue(value string) string {
	const max = 256
	if len(value) > max {
		value = value[:max]
	}
	return strconv.Quote(value)
}

func trimGhCallLog(path string) {
	log, err := os.Open(path)
	if err != nil {
		return
	}
	defer log.Close()
	info, err := log.Stat()
	if err != nil || info.Size() <= ghCallLogBytes {
		return
	}
	if _, err := log.Seek(info.Size()-ghCallLogBytes, io.SeekStart); err != nil {
		return
	}
	kept, err := io.ReadAll(io.LimitReader(log, ghCallLogBytes))
	if err != nil {
		return
	}
	if cut := bytes.IndexByte(kept, '\n'); cut >= 0 {
		kept = kept[cut+1:]
	}
	ghReplaceFile(path, ".calls-", kept)
}

// ghVisibilityRead is the repository argv reads, and whether argv is one of the guard's own two
// visibility reads. A pod's gh is `legion gh`, so checking either would recurse: they are the
// guard's own visibility lookup. The REST read is:
//
//	gh api --hostname github.com repos/<slug> --include --jq .visibility
//
// and the GraphQL read, made when the REST read is refused for a rate limit, is
// leakguard.VisibilityGraphQLArgs. Each must name exactly the repository the variable names, and
// that name must parse as a repository GitHub could serve, so a variable someone else set lets
// through one read of one repository and nothing else. The shared check's own read, the same
// argv without `--include`, is not one of them (sharedCheckVisibilityRead).
//
// runGh reads no gh help for these reads: their argv is exact, and their directory is the one the
// repository the variable names routes to (ghConfigDirFor), or GH_CONFIG_DIR under a caller's
// token.
//
// A caller's GH_TOKEN or GITHUB_TOKEN, which refuses every other call that reads the command
// (ghTargetOf), does not refuse these reads. runGh runs them with that token, which gh sends in
// place of any directory's token. That token, and not the bundle's directory for the repository
// or its owner, is the credential of these reads on purpose:
//
//   - The guard reads a visibility under a caller's token for a write made with that token, such
//     as a git push whose credential helper answers with GH_TOKEN. That token writes the
//     repository the guard reads, so it can read that repository too.
//   - The bundle may hold no directory for the repository's owner. A read with the bundle's
//     directory would then be refused, and so would the push, for a repository the token writes.
//   - gh sends the caller's token first, so a read with the bundle's directory would also have to
//     remove that token from gh's environment.
//
// The visibility is the repository's own, so the answer is the same whichever token reads it. A
// token that cannot read the repository makes the read fail, and the guard refuses the call.
func ghVisibilityRead(argv []string, env map[string]string) (ghrepo.Repository, bool) {
	repo, ok := visibilityReadRepository(env)
	if !ok {
		return ghrepo.Repository{}, false
	}
	slug := env[leakguard.VisibilityReadVar]
	if slices.Equal(argv, leakguard.VisibilityReadWithHeadersArgs(slug)) || slices.Equal(argv, leakguard.VisibilityGraphQLArgs(slug)) {
		return repo, true
	}
	return ghrepo.Repository{}, false
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
	ok, err := ghWrapperFile(wrapper)
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

// ghWrapperFile says whether the file at path is, whole, a script ghWrapperScript accepts
// (isGhWrapper): the read both the wrapper first on PATH (ghWrapperPath) and LEGION_GH_PATH
// (refuseLegionGhAsRealGh) are judged by.
func ghWrapperFile(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	return isGhWrapper(file)
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

// ghTarget is what a gh command names: dir, the gh directory every selector of the command is
// routed to (selectors.dir), and "" for a command naming no owner, whose token stays the one
// GH_CONFIG_DIR already names; owner, the lowercased owner of the repository it acts on, "" where
// the selectors named no owner or two; and repo, that repository's name keyed as gh reads a name
// of it where every selector of the command named that one repository of that owner and "" where
// they named more than one, where one named the owner alone, and where none named a repository
// at all. command is the resolved command path with no arguments. files are the files the
// command would send to GitHub as content (ghSentFiles), which the leak guard reads the text of.
// writesNothing is whether the call is in a read grammar (ghWritesNothing), so that under the
// shared check that rule was proven against the guard runs that check alone for it (runGh's
// ghReadCall). Such a call sends no file: no grammar names an option of ghSentFileOptions
// (TestGhReadGrammarNamesEachOptionAsItsHelpDoes).
type ghTarget struct {
	dir, owner, repo string
	command          string
	files            []leakguard.GhFile
	writesNothing    bool
}

// ghSentFileOptions are the options of gh 2.98.0, by long name, whose value names a file gh
// sends to GitHub as content: true where the whole value is the path, and false for `gh api`'s
// --field, whose file is the `@<path>` half of a `<name>=@<path>` value. They are the same four
// plus the field that leakguard's own table holds (its ghFileArgs), read here by their long
// names because the leaf's own help has already said which option each argument is: a `-F` is
// --body-file in `gh pr create`, --notes-file in `gh release create` and --field in `gh api`,
// and a short cluster (`-dF <path>`) and an attached value (`-F=<path>`) are read as pflag
// reads them.
var ghSentFileOptions = map[string]bool{
	"body-file":  true,
	"notes-file": true,
	"input":      true,
	"from-file":  true,
	"field":      false,
}

// ghSentFiles are the files the parsed command would send to GitHub as content, each named once
// and each named by the option it came from, for the leak guard to read the text of
// (leakguard.GhRequest.Files). The answer is never nil, so a command that sends no file is a
// command the guard reads no file of, rather than one it reads the argv of again with a table
// of its own.
//
// A value of `-`, and an `@-`, is standard input, which the guard reads through the shared
// check's own handshake and never opens as a file. The options are read in the order their
// names sort, since a parse holds each option's values together and not the argv's order
// across options, and a report that named two files must name them the same way twice.
func ghSentFiles(parsed ghArgs) []leakguard.GhFile {
	files := []leakguard.GhFile{}
	for _, name := range slices.Sorted(maps.Keys(ghSentFileOptions)) {
		whole := ghSentFileOptions[name]
		for _, value := range parsed.values[name] {
			path := value
			if !whole {
				_, after, found := strings.Cut(value, "=")
				if !found || !strings.HasPrefix(after, "@") {
					// A field given a literal value, and `gh project field-list --field <name>`,
					// name no file.
					continue
				}
				path = strings.TrimPrefix(after, "@")
			}
			if path == "" || path == "-" {
				continue
			}
			if slices.ContainsFunc(files, func(file leakguard.GhFile) bool { return file.Path == path }) {
				continue
			}
			files = append(files, leakguard.GhFile{Option: "--" + name, Path: path})
		}
	}
	return files
}

// ghCallerToken is the first of GH_TOKEN and GITHUB_TOKEN that env sets, in gh's own order,
// and "" where env sets neither. gh sends that token in place of any gh directory's token.
func ghCallerToken(env map[string]string) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if env[name] != "" {
			return name
		}
	}
	return ""
}

// ghTargetOf is the ghTarget of the gh command argv (gh's arguments, without `gh`), the zero
// target when the command names no owner. It fails closed: whenever it cannot be certain which
// gh directory the command needs it is an error, which names the argument it could not place, and
// nothing is sent. `gh --version` alone, and a command whose last --help is true, name none,
// since gh only prints.
//
// It reads the command as gh does, from gh's own help of each command on the way (help): the
// path of subcommands down to the leaf the arguments run, every option's arity, and the arguments
// the leaf's USAGE line names. A first word gh's own help does not list, a command that runs
// another program (`gh copilot`, `gh extension exec`), and a word its parent's help lists as a gh
// alias, whose expansion may name a repository of its own, or as an extension, which reads gh's
// token itself, are errors before its help is read, since reading it could run them (ghWalk); so
// is such a word as the target of `gh help`, which for an extension gh runs (ghHelpTarget), and a
// word whose help is another command's that lists no such alias of its own. Before the leaf only
// the inherited -R/--repo and --help/-h may come, read with the leaf's own options; anything else
// there, a subcommand gh does not know, help that lists an option incompletely or not at all, an
// option the leaf's help does not list or names a selector of in a way this does not know, and an
// argument its USAGE has no place for or names in a way this does not know (ghSlotKinds) are
// errors. The leaf's options are read with pflag's rules, and after `--` the rest are its
// arguments, unless its USAGE passes them on (`gh repo clone <repository> [<directory>] [--
// <gitflags>...]`).
//
// Every selector the leaf's help names counts: a `<repository>` (or another repository) argument,
// before `--` or, for a leaf passing the rest on, the first after it; an `<owner>`; a URL argument
// that is a github.com URL (through gh's pr finder, which reads any other as a branch of the
// command's own repository, only a pull request's names its repository); OWNER/REPO#N where the
// leaf reads it (ghLeafQuirks); a search `<query>`'s repo:/owner:/user:/org: qualifiers; `gh api`'s
// endpoint (repos/<owner>/<repo>/…, repos/<owner>/, orgs/<owner>/, users/<owner>/, a search
// endpoint's q) and a GraphQL query's owner variable (`-F owner=<login>`); and the options naming
// a repository, an owner, a team or an issue by URL (ghOptionKinds). So does the command's own
// repository when gh resolves it, because no argument names it and the leaf reads one, or
// whatever names it for a leaf that reads every remote (fromOwnRepository): GH_REPO for a leaf
// that reads it, gh api's :owner and {repo} placeholders among them, and otherwise every
// github.com repository the working directory's git remotes name (remotes), read as gh reads
// them; a command naming no owner at all counts those too. Every repository they name, and every
// owner one names alone — `orgs/<owner>/`, an owner:/org:/user: qualifier, an `<owner>` argument,
// a team, a project, an owner option, since such a command reaches whatever else of that owner's
// gh resolves — is routed through dirFor, the bundle's own pick (ghConfigDirFor), and they must
// all route to one directory; two are an error (selectors.dir). The target's repository is the
// one repository they all name, which is none where a selector names an owner alone. A GraphQL
// query naming its repository otherwise, by a node id or in the query text, names none gh could
// resolve. GH_HOST naming another host than github.com, where gh would send no token of the
// bundle's, and a GH_TOKEN or GITHUB_TOKEN, which gh sends in place of any directory's token, are
// errors.
func ghTargetOf(argv []string, env map[string]string, help func(path []string) (string, error), remotes func() ([]ghRemote, error), dirFor func(owner, name string) (string, error)) (ghTarget, error) {
	if host := env["GH_HOST"]; host != "" && !strings.EqualFold(host, "github.com") {
		return ghTarget{}, fmt.Errorf("GH_HOST is %q, another host than github.com, so it sends no token; unset GH_HOST", host)
	}
	if name := ghCallerToken(env); name != "" {
		return ghTarget{}, fmt.Errorf("%s is set, and gh sends it in place of the token of the owner the command targets, so it sends no token; unset %s", name, name)
	}
	walk := ghWalk{help: help}
	var rest []string
	var cmd *ghCommand
	for i := 0; i < len(argv) && cmd == nil; i++ {
		arg := argv[i]
		switch {
		case len(walk.path) == 0 && arg == "--version":
			if len(argv) == 1 {
				return ghTarget{}, nil
			}
			// gh reads a word after it as a command, an alias's or an extension's among them.
			return ghTarget{}, cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may (gh's version is `gh --version` alone)", false)
		case len(walk.path) == 0 && arg == "help":
			return ghTarget{}, ghHelpTarget(argv[i+1:], help)
		case arg == "-R" || arg == "--repo":
			if i+1 == len(argv) {
				return ghTarget{}, cannotPlace(arg, "has no value", false)
			}
			rest = append(rest, arg, argv[i+1])
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2) ||
			arg == "--help" || strings.HasPrefix(arg, "--help=") || arg == "-h":
			// The leaf reads these as its own, in order with the rest of its arguments.
			rest = append(rest, arg)
		case strings.HasPrefix(arg, "-"):
			return ghTarget{}, cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may", false)
		default:
			if err := walk.check(arg); err != nil {
				return ghTarget{}, err
			}
			read, err := walk.read(arg)
			if err != nil {
				return ghTarget{}, err
			}
			if !read.group {
				cmd = &read
				rest = append(rest, argv[i+1:]...)
			}
		}
	}
	if cmd == nil {
		// No leaf: `gh`, or a group alone, which prints its help.
		return ghTarget{}, nil
	}
	parsed, err := cmd.parse(rest)
	if err != nil || parsed.help {
		return ghTarget{}, err
	}
	sel := selectors{api: cmd.isAPI()}
	if err := sel.fromCommand(cmd, parsed); err != nil {
		return ghTarget{}, err
	}
	if err := sel.fromOwnRepository(cmd, parsed, env, remotes); err != nil {
		return ghTarget{}, err
	}
	files := ghSentFiles(parsed)
	command := strings.Join(cmd.path, " ")
	writesNothing := ghWritesNothing(argv, cmd.path)
	dir, err := sel.dir(dirFor)
	if err != nil {
		return ghTarget{}, err
	}
	return ghTarget{dir: dir, owner: sel.onlyOwner(), repo: sel.onlyRepository(), command: command, files: files, writesNothing: writesNothing}, nil
}

// cannotPlace is ghTargetOf's refusal of an argument it cannot place: what it is and why, and how
// to name the repository instead, which for gh api is its endpoint (it has no -R).
func cannotPlace(arg, why string, api bool) error {
	advice := "pass -R <owner>/<repo>"
	if api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	return fmt.Errorf("cannot tell which repository the command targets: %q %s, so it sends no token; %s", arg, why, advice)
}

// ghArgs is a leaf's arguments as pflag reads them: its positional arguments in order, every
// value given each option, by long name, and whether gh only prints the leaf's help.
type ghArgs struct {
	positionals []string
	values      map[string][]string
	help        bool
}

// parse reads args, the arguments of the leaf with its path taken out, as pflag does, refusing an
// option the leaf's help does not list or does not say the arity of. --help is a boolean whose
// last value decides; a -h no option of the leaf takes is pflag's request for help, which ends
// the reading there. After `--` every argument is positional, but for a leaf whose usage passes
// them on (`gh repo clone <repository> [<directory>] [-- <gitflags>...]`), where gh, as cobra
// hands it every positional, takes the first one as the repository even after the dash and passes
// the rest on: so the first argument after `--` is positional when none came before it.
func (c *ghCommand) parse(args []string) (ghArgs, error) {
	api, leaf := c.isAPI(), c.quoted()
	parsed := ghArgs{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest := args[i+1:]
			if c.passthrough && len(parsed.positionals) > 0 {
				rest = nil
			} else if c.passthrough && len(rest) > 1 {
				rest = rest[:1]
			}
			parsed.positionals = append(parsed.positionals, rest...)
			return parsed, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			flag, ok := c.flags[name]
			if !ok {
				return ghArgs{}, cannotPlace(arg, "is no option "+leaf+"'s help lists", api)
			}
			if flag.uncertain {
				return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
			}
			if !hasValue {
				switch {
				case !flag.value:
					value, hasValue = "true", true
				case flag.optional:
					// Its default, which names nothing.
				case i+1 == len(args):
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				default:
					i++
					value, hasValue = args[i], true
				}
			}
			if hasValue {
				if err := parsed.set(name, flag, value, arg, api); err != nil {
					return ghArgs{}, err
				}
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			// pflag's shorthands, each in turn: `-f=value` gives any its value; a boolean or an
			// optional value takes its default and leaves the rest of the word to the next
			// shorthand; any other takes the rest of the word, or the next argument.
			for shorts := arg[1:]; shorts != ""; {
				short, rest := shorts[0], shorts[1:]
				name, ok := c.shorts[short]
				if !ok {
					if short == 'h' {
						parsed.help = true
						return parsed, nil
					}
					return ghArgs{}, cannotPlace(arg, fmt.Sprintf("names -%c, which %s's help does not list", short, leaf), api)
				}
				flag := c.flags[name]
				if flag.uncertain {
					return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
				}
				value, hasValue := "", true
				switch {
				case len(rest) > 1 && rest[0] == '=':
					value, rest = rest[1:], ""
				case !flag.value:
					value = "true"
				case flag.optional:
					hasValue = false
				case rest != "":
					value, rest = rest, ""
				case i+1 < len(args):
					i++
					value = args[i]
				default:
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				}
				if hasValue {
					if err := parsed.set(name, flag, value, arg, api); err != nil {
						return ghArgs{}, err
					}
				}
				shorts = rest
			}
		default:
			parsed.positionals = append(parsed.positionals, arg)
		}
	}
	return parsed, nil
}

// set records value given the option name: --help's as a boolean, any other's taking a value in
// the order given.
func (p *ghArgs) set(name string, flag ghFlag, value, arg string, api bool) error {
	switch {
	case name == "help":
		help, err := strconv.ParseBool(value)
		if err != nil {
			return cannotPlace(arg, "gives --help a value that is no boolean", api)
		}
		p.help = help
	case flag.value:
		p.values[name] = append(p.values[name], value)
	}
	return nil
}

// refuseLegionGhAsRealGh refuses a LEGION_GH_PATH that is not an absolute path, and one that is
// `legion gh` itself: a script of the wrapper's form (ghWrapperFile), the wrapper first on PATH
// among them, which ghWrapperPath has already found to be one. Run as the real gh, a wrapper would
// run this command again for every help read and every call. A name with no slash is run from PATH
// (exec.LookPath), whose first gh is that wrapper, and read here, if at all, from the working
// directory, so nothing this reads of it says what would run; the daemon takes no LEGION_GH_PATH
// that is not absolute either (internal/daemon's resolveTool). An absolute LEGION_GH_PATH it cannot
// read is left to the run that follows to fail on.
func refuseLegionGhAsRealGh(gh string) error {
	if !filepath.IsAbs(gh) {
		return fmt.Errorf("LEGION_GH_PATH is not an absolute path: %q; set LEGION_GH_PATH to the real gh's install path", gh)
	}
	if wrapped, err := ghWrapperFile(gh); err == nil && wrapped {
		return fmt.Errorf("LEGION_GH_PATH %s is a wrapper that runs `legion gh`, not the real gh; set LEGION_GH_PATH to the real gh's install path", gh)
	}
	return nil
}

var ghHelpTimeout = 30 * time.Second

// readGhHelp is the real gh's help of the command path, `gh <path> --help` (`gh --help` for
// gh's own), refused when gh answers it with an error. runGh reads it through ghHelpKeep, which
// keeps what gh printed for the calls after, and never a read that ctx ended. gh runs in a
// process group of its own, which is stopped whole when the read outlives ghHelpTimeout or ctx
// ends (procgroup.Configure): a stop that killed gh alone would leave whatever gh started running,
// reparented and unwaited for. That group is not the terminal's, so none of the terminal's signals
// reach it, and ctx is how they do: runGh's, which main cancels on a SIGINT or a SIGTERM and
// hangupCancels on a SIGHUP. It runs under leakguard.RunByRealGhVar, so a gh that leads back to
// `legion gh` is refused there.
func readGhHelp(ctx context.Context, gh string, path []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghHelpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, append(slices.Clone(path), "--help")...)
	killed := procgroup.Configure(cmd)
	cmd.Env = append(os.Environ(), leakguard.RunByRealGhVar+"="+gh)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		switch {
		case killed() && errors.Is(ctx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("gh did not answer its help within %s, and its process group was stopped", ghHelpTimeout)
		case killed():
			return "", fmt.Errorf("gh's help read was stopped, with its process group: %v", context.Cause(ctx))
		}
		return "", fmt.Errorf("gh answered its help with %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return "", errors.New("gh printed no help")
	}
	return stdout.String(), nil
}
