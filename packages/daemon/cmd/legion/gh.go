package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the owner the command names, under LEGION_GH_OWNERS_DIR)"

// runGh is `legion gh`, the gh a pane or pod runs. It reads the repositories the command acts on,
// and their owners, as gh resolves them (ghroute.TargetOf) and execs the real gh (LEGION_GH_PATH)
// as `gh` with its arguments untouched and GH_CONFIG_DIR set to a gh directory under
// LEGION_GH_OWNERS_DIR: the one directory that answers for every repository the command names
// (ghroute.DirFor), which is the owner's own — the one directory the bundle holds for that
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
// it cannot reach, but by one exception, R4 of dispatch://INFRA-3399 (ghroute.TargetOf): a call
// that writes nothing, names only owners the bundle holds no directory for and names no
// repository a configured rename moves, in a working directory whose remotes name no github.com
// repository for it, keeps GH_CONFIG_DIR, as a command naming no owner does. Under a GH_TOKEN or
// GITHUB_TOKEN, which gh sends in place of any directory's token, every command is refused the
// same way but these: the guard's own visibility reads, which run with that token
// (ghVisibilityRead), the shared check's visibility read (sharedCheckVisibilityRead), the exact
// `gh api rate_limit` (ghRateLimitRead) and `gh auth git-credential`.
//
// ghroute.TargetOf reads each command from gh's own help, which gh prints the same way until its
// binary, its configuration, its extensions or the variables it prints under change: runGh keeps
// each help it reads under the role's state directory and reads it from gh again only after such
// a change (ghHelpKeep). The visibility reads named above and the rate-limit read are exact argv
// and read no help at all.
//
// Before gh is handed anything, the leak guard reads the call (leakguard.GhCall): a call it
// refuses, one whose checks could not be made to run, and one whose destination GitHub answers
// 404 for, are refused here, exit 2, with the guard's own message, and gh never runs. A call that
// writes nothing, by the rule ghroute holds (ghroute.Target.WritesNothing) and under the shared
// check that rule was proven against (ghroute.ReadsProvenCheckSHA256), is read by that check alone
// and gets no read of a repository's visibility and no private-name pass. Three calls pass
// unchecked: `legion gh auth git-credential`, which answers above before any of this, the exact
// `gh api rate_limit` (ghRateLimitRead), and the guard's own visibility reads (ghVisibilityRead).
// The shared check's visibility read this command answers itself (sharedCheckVisibilityRead),
// before it reads anything else of the call. gh runs the rate-limit read, the guard's own
// visibility reads and each call the shared check alone read with no pager.
//
// gh runs with an environment built from an allowlist (ghChildEnviron), never the caller's whole:
// the proxy variables are kept, since a deployment that reaches GitHub through a proxy names it
// there, but no trust store, no editor, browser or pager program, no repository that is not the
// current directory's and no variable a later gh learns to read reaches it, so a proxy sees only
// a TLS tunnel it cannot read, and no proxy a CA vouches for is the caller's to name. A gh
// directory whose files route gh's requests by host, or name an alias of `help`, is refused
// (ghConfigShape). An editor gh starts is a program that fails (ghNoEditor), and gh's telemetry is
// off.
//
// The git gh runs itself (a clone, a fetch of a fork's parent, `pr checkout`) authenticates
// through the credential helper gh names after its own executable, GH_PATH: runGh sets it to the
// `gh` wrapper first on PATH, so that helper is `legion gh auth git-credential`, which answers as
// `legion git-credential` does, from the directory of the owner in git's path (useHttpPath set
// for github.com through GIT_CONFIG_COUNT), or, for a fork whose owner has no directory, from the
// directory of its parent's or else its source's owner (forkUpstreamDir), and never with
// GH_CONFIG_DIR's token merely because it is the default; when PATH's first gh
// is no such wrapper it refuses. That git reads no configuration file but the repository's own
// (ghChildForced), takes of the caller's GIT_CONFIG pairs only the runtime's own
// (ghChildGitConfig), and gets http.sslVerify on; a repository whose own configuration names a
// proxy, a certificate setting or another credential helper is refused (ghRepoGitConfigRefusal).
// Each of the three variables is runtime-set and required; an unset one is refused, exit 2,
// naming it. With no LEGION_GH_PATH, --help prints this command's usage, so `legion gh --help`
// outside a pane answers as every legion command does.
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
// Everything this command starts before gh — the help reads (ghroute.ReadHelp), and the leak guard's
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
	// gh reads the directory GH_CONFIG_DIR names for every help this reads, so it is judged before
	// the first; the directory the command routes to is judged again before gh runs.
	if err := ghConfigShape(env["GH_CONFIG_DIR"]); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	helpEnv := ghChildEnviron(os.Environ(), map[string]string{"GH_CONFIG_DIR": env["GH_CONFIG_DIR"]})
	rateLimitRead := ghRateLimitRead(args)
	visibilityRead, isVisibilityRead := ghVisibilityRead(args, env)
	// The repository the git gh runs works in is the current directory's, whatever the call: a
	// configuration of its that would hand that git's token to a proxy is refused before any read.
	// The two exact reads run `gh api` on an endpoint with no placeholder, which starts no git at
	// all, so they read none: each stays the one gh process the guard counts on.
	if !rateLimitRead && !isVisibilityRead {
		if err := ghRepoGitConfigRefusal(helpEnv); err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	// What the call targets, and the gh directory it runs with, chosen here once for every call.
	// The two exact reads go through no ghroute.TargetOf, and so read no gh help: the resolver has
	// nothing of their argv to read.
	var target ghroute.Target
	switch {
	case rateLimitRead:
		// It names no repository, and runs with GH_CONFIG_DIR.
		target.Command = []string{"api"}
	case isVisibilityRead:
		// The guard's own request. Its enclosing user call records the source of that request, so
		// logging it again would count one wrapped call twice.
		logCall = false
		target.Command = []string{"api"}
		// Its token is that of the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names, which
		// ghVisibilityRead matched to the read's exact argv, routed by the one rule
		// (ghroute.DirFor): a configured rename serves the repository from the `to` owner's
		// directory, and the `from` owner's token cannot read it. The GraphQL read names its
		// repository only in variables, so the variable is where its owner and name are read.
		// Its argv names the host, `--hostname github.com`, so no GH_HOST sends it elsewhere.
		//
		// Under the caller's GH_TOKEN or GITHUB_TOKEN it keeps GH_CONFIG_DIR and is routed by
		// nothing, because gh sends that token in place of any directory's: the bundle may hold
		// no directory for the read's repository at all, and this routing would then refuse a
		// read the caller's token can make, which is the second reason #4219 gives for running
		// these reads under it (ghVisibilityRead).
		//
		// With no caller token, a read of a repository whose owner the bundle holds no directory
		// for is refused below with leakguard.OwnerNotServedError's line (ghroute.DirFor), and
		// no gh runs. The guard reads exactly that refusal, and then reads the repository's
		// visibility with no credential (leakguard.Guard.Visibility).
		if ghroute.CallerToken(env) == "" {
			target.Owner, target.Repo = strings.ToLower(visibilityRead.Owner()), ghconfig.RepositoryName(visibilityRead.Name())
			target.Dir, err = ghroute.DirFor(env["LEGION_GH_OWNERS_DIR"], target.Owner, target.Repo)
		}
	default:
		// Every repository the command names is routed through the bundle's own pick, and the
		// command refused where they route to two directories (ghroute.TargetOf), before the
		// guard reads anything of it. gh's help of each command on the way is read once per gh
		// binary and kept for the calls after (ghHelpKeep).
		ownersDir := env["LEGION_GH_OWNERS_DIR"]
		// The git that reads the working directory's remotes runs with gh's own environment, so
		// it routes by the repository and the configuration gh's git reads (ghChildEnviron).
		remotes := func() ([]ghroute.Remote, error) { return ghroute.ReadGitRemotes(ghChildEnviron(os.Environ(), nil)) }
		target, err = ghroute.TargetOf(args, env, newGhHelpKeep(beforeGh, gh, env, helpEnv).read, remotes,
			func(owner, name string) (string, error) { return ghroute.DirFor(ownersDir, owner, name) })
		if status, ok := signalled(beforeGh); ok {
			return status
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	// The directory gh will run with must not send its requests anywhere but github.com: a pane
	// can write its own gh files between two of the daemon's refreshes (ghConfigShape).
	if err := ghConfigShape(ghCallDir(env, target)); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	commandPath = strings.Join(target.Command, " ")
	if target.Owner != "" && target.Repo != "" {
		repo = target.Owner + "/" + target.Repo
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
	// A call that writes nothing (ghroute.Target.WritesNothing, decided from the words it is
	// typed with) is handed to the guard as one (leakguard.GhRequest.WritesNothing), with the
	// sha256 of the shared check its rule was proven against, which this binary holds
	// (ghProvenCheckSHA256). Under that check the guard runs the check and nothing more, and says
	// so (leakguard.GhAnswer.CheckedOnly), and the log line says checked-only; under any other it
	// runs whole. The checker a role is told (leakguard.CheckerVar) plays no part in it. Every
	// other call is guarded whole, the visibility read and the private-name pass included.
	//
	// A *leakguard.Refusal, a *leakguard.SetupFailure and a *leakguard.NotFound each refuse the
	// call: a check that did not run has cleared nothing, and neither has a visibility read GitHub
	// answered 404. All three exit 2, this command's status for a refusal made before gh runs,
	// which is what an unreadable command and an unset variable already exit with, and all three
	// print the guard's own message, which never holds the text it refused.
	//
	// The files the call would send go with it, read out of the parse above, which knew each
	// option's arity from the leaf's own help (ghroute.Target.Files): the guard reads the text
	// of each, and needs no second reader of the argv for a call this command checked. So does
	// whether the call sends its standard input as content, which the guard reads itself where no
	// shared check runs to ask for it (ghroute.Target.StdinSent, leakguard.GhRequest.StdinSent).
	var buffered []byte
	checkedOnly := false
	mode := ghCallModeOf(rateLimitRead, isVisibilityRead, target.WritesNothing)
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
			Curl: leakguardCurl,
		})
		answer, err := guard.GhCall(beforeGh, leakguard.GhRequest{
			Args:          args,
			Stdin:         os.Stdin,
			Repo:          repo,
			Files:         target.Files,
			StdinSent:     target.StdinSent,
			WritesNothing: mode == ghReadCall,
			ProvenCheck:   ghProvenCheckSHA256,
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
	// Every mode runs with the directory chosen once above: by ghroute.TargetOf, through the one
	// rule (ghroute.DirFor), refusing a command whose repositories route to two, and for the
	// guard's visibility read by that rule for the repository its variable names. No mode picks a
	// directory any other way.
	if target.Dir != "" {
		dir = target.Dir
	}
	if err := ghConfigShape(dir); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	set := map[string]string{"GH_CONFIG_DIR": dir, "GH_PATH": wrapper, leakguard.RunByRealGhVar: gh}
	// A call no private-name pass read runs no pager program: such a program runs with this call's
	// token directory and could write what that pass never read. Its pager is cat, which gh reads
	// as no pager (GH_PAGER outranks gh's config `pager`; gh starts a pager only when its standard
	// output is a terminal). No call is given a browser, an editor or a pager from the caller's
	// environment (ghChildEnviron); gh's config `browser` is left, because gh reads it only to open
	// a page, which such a call never does: a leaf ghroute reads as a read opens one only under
	// --web, which no read grammar names, so a call giving it is guarded whole
	// (ghroute.Target.WritesNothing), and `gh api` opens none.
	if mode == ghRateLimitCall || mode == ghVisibilityCall || checkedOnly {
		set["GH_PAGER"] = "cat"
	}
	childEnv, err := withGitHubUseHTTPPath(ghChildEnviron(os.Environ(), set))
	if err != nil {
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
	// ghReadCall is a call that writes nothing (ghroute.Target.WritesNothing): the guard runs the
	// shared check alone where that check is the one the rule was proven against, and whole
	// otherwise.
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
// repository the variable names routes to (ghroute.DirFor), or GH_CONFIG_DIR under a caller's
// token.
//
// A caller's GH_TOKEN or GITHUB_TOKEN, which refuses every other call that reads the command
// (ghroute.TargetOf), does not refuse these reads. runGh runs them with that token, which gh
// sends in place of any directory's token. That token, and not the bundle's directory for the
// repository or its owner, is the credential of these reads on purpose:
//
//   - The guard reads a visibility under a caller's token for a write made with that token, such
//     as a git push whose credential helper answers with GH_TOKEN. That token writes the
//     repository the guard reads, so it can read that repository too.
//   - The bundle may hold no directory for the repository's owner. A read with the bundle's
//     directory would then be refused, and the guard could read the visibility only with no
//     credential, which answers for a public repository alone (leakguard.OwnerNotServedError):
//     a push to a private repository the token writes would be refused.
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
	// (packages/envoy/cmd/cipher/secret_signal_linux.go's promptSignalIgnored).
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

// ghRoutingKeys are the keys of a gh directory's files that send gh's requests, with the
// directory's token, somewhere other than github.com: gh 2.104.0's api_host, read from a host's
// block of hosts.yml, and http_unix_socket, read globally or per host. gh writes both with
// `gh config set`, which ghroute.TargetOf refuses, but a pane's user can write its own gh files
// between two of the daemon's refreshes, which put them back a minute later at most
// (ghconfig.WriteBundle, the runtimes' gitHubRefreshInterval).
var ghRoutingKeys = []string{"api_host", "http_unix_socket"}

// ghCallDir is the gh directory a call runs with: the one its repositories route to, or the
// pane's own GH_CONFIG_DIR for a call that names none.
func ghCallDir(env map[string]string, target ghroute.Target) string {
	if target.Dir != "" {
		return target.Dir
	}
	return env["GH_CONFIG_DIR"]
}

// ghConfigShape refuses the gh directory dir, naming the file and the key and never a value, when:
//
//   - its hosts.yml or config.yml is no regular file (a symbolic link, a named pipe, a directory)
//     or is larger than any the daemon writes: read only through ghconfig.ReadRegular, which
//     never waits on a pipe and never follows a link, so neither stalls the call; or does not
//     parse, since gh's reading of it is then not this one's;
//   - either holds a key of ghRoutingKeys where go-gh would read it as a key (ghRoutingKeyIn), so
//     gh never runs with a directory that would send its token elsewhere;
//   - its config.yml holds a `hosts` block, settings gh reads per host (an editor, a pager, a
//     browser) that the daemon never writes, or an alias named `help`. gh expands an alias
//     whose name is none of its own commands, and `help` is none (measured with gh 2.98.0 and
//     2.104.0: an alias `help` ran on `legion gh help` and sent what it named with the token,
//     which no guard read, since this command passes `help` to gh as a request for help). An
//     alias named after a command of gh's own, `pr` or `agent-task`, gh never expands (measured
//     for pr, issue and api), and any other name this command reads gh's own help of, which gh
//     answers for the expansion, so the command is routed and guarded as what it expands to.
//
// The daemon writes config.yml as the version line alone (ghconfig.Config) and hosts.yml from the
// installation token, so none of these is the daemon's. A file that is not there holds nothing.
//
// Its limit: it reads the files before gh does, so a pane that writes them between this check and
// gh's own read wins that race. Such a pane can already read the token in the same files, so the
// check guards against an induced `config set` or a stray edit, not against the pane itself.
func ghConfigShape(dir string) error {
	for _, name := range []string{ghconfig.HostsFile, ghconfig.ConfigFile} {
		path := filepath.Join(dir, name)
		data, err := ghconfig.ReadRegular(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case errors.Is(err, ghconfig.ErrNotRegular):
			return fmt.Errorf("%s is no regular file, so it is not read, and gh is not run with it; it sends nothing; the daemon rewrites it within a minute", path)
		case errors.Is(err, ghconfig.ErrTooLarge):
			return fmt.Errorf("%s is larger than any file the daemon writes, so it is not read, and gh is not run with it; it sends nothing; the daemon rewrites it within a minute", path)
		case err != nil:
			return fmt.Errorf("cannot read %s: %w; it sends nothing", path, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s does not parse as YAML, so it cannot be read for a key that sends gh's requests elsewhere; it sends nothing", path)
		}
		if key := ghRoutingKeyIn(&doc); key != "" {
			return fmt.Errorf("%s sets %s, which sends gh's requests with this directory's token somewhere other than github.com, so it sends nothing; the daemon rewrites this file within a minute", path, key)
		}
		if name != ghconfig.ConfigFile {
			continue
		}
		var config struct {
			Hosts   yaml.Node         `yaml:"hosts"`
			Aliases map[string]string `yaml:"aliases"`
		}
		if err := doc.Decode(&config); err != nil {
			return fmt.Errorf("%s does not parse as gh's configuration, so it cannot be read for what it changes of gh; it sends nothing", path)
		}
		if config.Hosts.Kind != 0 {
			return fmt.Errorf("%s holds a hosts block, settings gh reads per host that the daemon never writes, so it sends nothing; the daemon rewrites this file within a minute", path)
		}
		if _, ok := config.Aliases["help"]; ok {
			return fmt.Errorf("%s names an alias `help`, which gh would run on `legion gh help` in place of its help, so it sends nothing; the daemon rewrites this file within a minute", path)
		}
	}
	return nil
}

// ghRoutingKeyIn is the first key of ghRoutingKeys that node, or a node under it, holds where
// go-gh would read it as a key, and "" for none. go-gh looks a key up as the value of an
// even-indexed child of the node it reads, whatever that node's kind (go-gh v2.17.0
// internal/yamlmap/yaml_map.go:78-92, FindEntry), so a sequence `[api_host, <host>]` reads to it as
// a mapping from api_host to the host, and an alias node as the name of its anchor: the
// even-indexed children of every node with children count, a mapping's keys and a sequence's
// first, third and later items alike.
func ghRoutingKeyIn(node *yaml.Node) string {
	for i := 0; i < len(node.Content); i += 2 {
		if slices.Contains(ghRoutingKeys, node.Content[i].Value) {
			return node.Content[i].Value
		}
	}
	for _, child := range node.Content {
		if key := ghRoutingKeyIn(child); key != "" {
			return key
		}
	}
	return ""
}
